// Package rediscache provides the Redis-backed adapters for the approved output
// ports (Cache, SessionStore, IdempotencyStore) plus the distributed-lock and
// sliding-window rate-limit primitives consumed by the scheduler and the API
// gateway middleware.
//
// Key namespace:
//
//	ses:{id}                     session hash (server-side, 30-min sliding inactivity TTL)
//	sesuser:{user_id}            set of session IDs per user (RevokeAllForUser)
//	fms:v1:{tenant}:...          cache keys - callers compose via ports.Cache key
//	                             helpers (budget 60s, category/account lists 5min)
//	rl:{route}:{client}          sliding-window rate-limit sorted set
//	lock:{name}                  distributed lock (fencing token, SET NX PX)
//	idem:{tenant}:{user}:{hash}  idempotency keys - callers compose the full key
//	bl:{jti}                     JWT blacklist - reserved for security/jwt.go
//
// The authoritative ledger is never served from cache: this package is strictly
// read-through with a DB fallback and callers Invalidate on every mutation, so a
// Redis outage degrades rate limiting/session refresh but never yields stale or
// inconsistent financial state. Network failures are surfaced to callers as
// opaque dependency-unavailable errors (the HTTP layer maps them to 503 with a
// Retry-After header); the detailed cause is logged with the structured logger,
// never returned.
//
// All mutating primitives use Lua scripts executed via go-redis Script (which
// transparently falls back to EVAL on NOSCRIPT after a server script-cache
// flush) so each operation is atomic and bounded in O(window). The lock uses a
// randomly-generated token with compare-and-release. This gives mutual exclusion
// for a single scheduler job; it is not a Redis-issued monotonically increasing
// true fencing token, so the lock TTL should exceed the longest single batch
// runtime and RenewLock must be driven on a background goroutine.
//
// Because ports.Cache.Get and ports.IdempotencyStore.Get collide by name (they
// cannot share a single Go receiver), the package exposes three types over one
// connection pool: *Client (SessionStore + lock + rate limit) and the two
// lightweight adapters *CacheClient and *IdempotencyClient, all reachable from
// the single *Client returned by New.
package rediscache

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/config"
	"fms/backend/internal/domain"
	"fms/backend/internal/infrastructure/observability"
)

const (
	keySessionPrefix      = "ses:"
	keySessionIndexPrefix = "sesuser:"
	keyLockPrefix         = "lock:"

	optMinIdleConns = 1
	optDialTimeout  = 3 * time.Second
	optReadTimeout  = 2 * time.Second
	optWriteTimeout = 2 * time.Second

	pingRetries     = 3
	pingBaseBackoff = 100 * time.Millisecond
	idemPayloadMax  = 1 << 20  // 1 MiB cap on stored idempotency response bodies
	idemTTLMin      = 60 * time.Second
	idemTTLMax      = 24 * time.Hour
	defaultPoolSize = 10
)

// ErrLockLost is returned by RenewLock/ReleaseLock when the stored token no
// longer matches the caller's token, meaning the lock expired or was taken over.
// The scheduler must abort the current batch fail-closed rather than continue.
var ErrLockLost = errors.New("lock lost or expired")

// Client adapts Redis to the session store port and the lock/rate-limit
// primitives. It is one of three adapters sharing a single connection pool; use
// Cache() and Idempotency() to obtain the other two.
type Client struct {
	rdb *redis.Client
	log *observability.Logger

	cache *CacheClient
	idem  *IdempotencyClient
}

// Cache returns the Cache adapter sharing this client's connection pool.
func (c *Client) Cache() *CacheClient { return c.cache }

// Idempotency returns the IdempotencyStore adapter sharing this client's pool.
func (c *Client) Idempotency() *IdempotencyClient { return c.idem }

// New builds the redis adapters from cfg. It requires TLS transport
// (rediss:// or unix://) unless the resolved host is loopback (for local and
// miniredis testing), verifies connectivity with a bounded retry, and returns a
// *Client whose pool size is bounded by cfg.Redis.
func New(cfg *config.Config) (*Client, error) {
	if cfg == nil || cfg.Redis.URL == "" {
		return nil, errors.New("rediscache: FMS_REDIS_URL is required")
	}
	opts, err := redis.ParseURL(cfg.Redis.URL)
	if err != nil {
		return nil, fmt.Errorf("rediscache: parse FMS_REDIS_URL: %w", err)
	}
	if err := secureTransportRequired(cfg.Redis.URL, opts); err != nil {
		return nil, err
	}

	poolSize := cfg.Redis.PoolSize
	if poolSize <= 0 {
		poolSize = defaultPoolSize
	}
	opts.PoolSize = poolSize
	opts.MinIdleConns = optMinIdleConns
	opts.DialTimeout = optDialTimeout
	opts.ReadTimeout = optReadTimeout
	opts.WriteTimeout = optWriteTimeout

	rdb := redis.NewClient(opts)
	if err := pingWithRetry(context.Background(), rdb); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("rediscache: connect/ping: %w", err)
	}

	log, _ := observability.New(cfg)
	c := &Client{rdb: rdb, log: log}
	c.cache = &CacheClient{rdb: rdb, log: log}
	c.idem = &IdempotencyClient{rdb: rdb, log: log}
	return c, nil
}

// secureTransportRequired rejects plaintext redis:// targets outside the local
// development host so session tokens, idempotency bodies, and rate-limit state
// never traverse the network unencrypted (OWASP API2). Unix sockets are always
// accepted because they are nested inside the OS credential boundary.
func secureTransportRequired(url string, opts *redis.Options) error {
	lower := strings.ToLower(url)
	switch {
	case strings.HasPrefix(lower, "rediss://"), strings.HasPrefix(lower, "unix://"):
		return nil
	case strings.HasPrefix(lower, "redis://"):
		host := opts.Addr
		if h, _, err := net.SplitHostPort(opts.Addr); err == nil {
			host = h
		}
		switch strings.Trim(host, "[]") {
		case "localhost", "127.0.0.1", "::1":
			return nil
		}
		return errors.New("rediscache: Redis transport must be TLS (rediss://) unless connecting to a loopback address for local development")
	default:
		return errors.New("rediscache: Redis URL scheme must be rediss://, unix://, or redis:// to a loopback address")
	}
}

func pingWithRetry(ctx context.Context, rdb *redis.Client) error {
	var err error
	backoff := pingBaseBackoff
	for i := 0; i < pingRetries; i++ {
		err = rdb.Ping(ctx).Err()
		if err == nil {
			return nil
		}
		if i < pingRetries-1 {
			// Exponential backoff with jitter to avoid a thundering herd on boot.
			time.Sleep(backoff + time.Duration(rand.Int63n(int64(backoff))))
			backoff *= 2
		}
	}
	return err
}

// Close drains the connection pool.
func (c *Client) Close() error {
	if c == nil || c.rdb == nil {
		return nil
	}
	return c.rdb.Close()
}

// ---- Cache adapter (implements ports.Cache) ----

// CacheClient is the Cache port adapter. Keys must be composed by callers via
// the ports.Cache helpers (budget 60s, category/account lists 5min); this type
// is intentionally policy-agnostic about TTL and key structure.
type CacheClient struct {
	rdb *redis.Client
	log *observability.Logger
}

// Get returns the bytes stored at key. A miss is (nil, nil); a Redis outage maps
// to an opaque ports.CacheUnavailable error so the caller falls back to the
// authoritative store rather than treating the outage as an absent key.
func (c *CacheClient) Get(ctx context.Context, key string) ([]byte, error) {
	if key == "" {
		return nil, errors.New("rediscache: cache key is required")
	}
	b, err := c.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		c.logDependency(ctx, err)
		return nil, ports.NewCacheUnavailable(err)
	}
	return b, nil
}

// Set stores val for ttl. A zero or negative TTL is rejected because Redis
// EXPIRE/SETEX treats it as an immediate delete.
func (c *CacheClient) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	if key == "" {
		return errors.New("rediscache: cache key is required")
	}
	if ttl <= 0 {
		return errors.New("rediscache: cache TTL must be positive")
	}
	if err := c.rdb.Set(ctx, key, val, ttl).Err(); err != nil {
		c.logDependency(ctx, err)
		return ports.NewCacheUnavailable(err)
	}
	return nil
}

// Delete removes a single key; it is idempotent for keys that do not exist.
func (c *CacheClient) Delete(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("rediscache: cache key is required")
	}
	if err := c.rdb.Del(ctx, key).Err(); err != nil {
		c.logDependency(ctx, err)
		return ports.NewCacheUnavailable(err)
	}
	return nil
}

// InvalidatePattern deletes all keys matching pattern via SCAN+DEL. Callers pass
// only allowlisted patterns (e.g. ports.BudgetPattern scoped by tenant) - this
// adapter never accepts a free-form pattern from untrusted input.
func (c *CacheClient) InvalidatePattern(ctx context.Context, pattern string) error {
	if strings.TrimSpace(pattern) == "" {
		return errors.New("rediscache: cache pattern is required")
	}
	var cursor uint64
	const scanCount = 100
	for {
		keys, next, err := c.rdb.Scan(ctx, cursor, pattern, scanCount).Result()
		if err != nil {
			c.logDependency(ctx, err)
			return ports.NewCacheUnavailable(err)
		}
		if len(keys) > 0 {
			if err := c.rdb.Del(ctx, keys...).Err(); err != nil {
				c.logDependency(ctx, err)
				return ports.NewCacheUnavailable(err)
			}
		}
		cursor = next
		if cursor == 0 {
			return nil
		}
	}
}

func (c *CacheClient) logDependency(ctx context.Context, err error) {
	c.log.FromContext(ctx).Warn("redis cache dependency error", "cause", err.Error())
}

// ---- Idempotency adapter (implements ports.IdempotencyStore) ----

// IdempotencyClient is the IdempotencyStore port adapter. Keys must be composed
// by callers server-side as idem:{tenant}:{user}:{hash}; this type enforces the
// SET-NX atomicity, TTL clamp, and payload cap required by the port.
type IdempotencyClient struct {
	rdb *redis.Client
	log *observability.Logger
}

// Get returns the stored response for key. A miss is (nil, false, nil); an infra
// error is (nil, false, err) and is never mistaken for a miss.
func (c *IdempotencyClient) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if key == "" {
		return nil, false, errors.New("rediscache: idempotency key is required")
	}
	b, err := c.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		c.logDependency(ctx, err)
		return nil, false, ports.NewStoreUnavailable(err)
	}
	return b, true, nil
}

// Store atomically overwrites key with response for ttl, clamping the TTL to the
// port's 60s..24h bound and rejecting oversized payloads.
func (c *IdempotencyClient) Store(ctx context.Context, key string, response []byte, ttl time.Duration) error {
	if key == "" {
		return errors.New("rediscache: idempotency key is required")
	}
	if err := validateIdem(ttl, response); err != nil {
		return err
	}
	if err := c.rdb.Set(ctx, key, response, clampIdemTTL(ttl)).Err(); err != nil {
		c.logDependency(ctx, err)
		return ports.NewStoreUnavailable(err)
	}
	return nil
}

// StoreIfAbsent implements SET-NX single-flight: it stores response only if key
// is absent and reports whether the store happened. A concurrent or retried call
// with the same key gets (false, nil), prompting the caller to poll Get for the
// winner's final response. An infra error is never treated as "already stored".
func (c *IdempotencyClient) StoreIfAbsent(ctx context.Context, key string, response []byte, ttl time.Duration) (bool, error) {
	if key == "" {
		return false, errors.New("rediscache: idempotency key is required")
	}
	if err := validateIdem(ttl, response); err != nil {
		return false, err
	}
	ok, err := c.rdb.SetNX(ctx, key, response, clampIdemTTL(ttl)).Result()
	if err != nil {
		c.logDependency(ctx, err)
		return false, ports.NewStoreUnavailable(err)
	}
	return ok, nil
}

func (c *IdempotencyClient) logDependency(ctx context.Context, err error) {
	c.log.FromContext(ctx).Warn("redis idempotency dependency error", "cause", err.Error())
}

func validateIdem(ttl time.Duration, response []byte) error {
	if ttl <= 0 {
		return errors.New("rediscache: idempotency TTL must be positive")
	}
	if len(response) > idemPayloadMax {
		return errors.New("rediscache: idempotency response exceeds the payload cap")
	}
	return nil
}

func clampIdemTTL(ttl time.Duration) time.Duration {
	if ttl < idemTTLMin {
		return idemTTLMin
	}
	if ttl > idemTTLMax {
		return idemTTLMax
	}
	return ttl
}

// ---- SessionStore port (via *Client) ----

// Create persists s at ses:{id} and registers it in the per-user index so
// RevokeAllForUser can revoke deterministically. The session hash and the index
// are written in one pipeline so they become visible atomically.
func (c *Client) Create(ctx context.Context, s ports.Session, ttl time.Duration) error {
	if s.ID == uuid.Nil || s.UserID == uuid.Nil || s.TenantID == uuid.Nil {
		return errors.New("rediscache: session requires id, user id and tenant id")
	}
	if ttl <= 0 {
		return errors.New("rediscache: session TTL must be positive")
	}
	now := time.Now().UTC()
	key := keySessionPrefix + s.ID.String()
	indexKey := keySessionIndexPrefix + s.UserID.String()

	_, err := c.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.HSet(ctx, key,
			"id", s.ID.String(),
			"user_id", s.UserID.String(),
			"tenant_id", s.TenantID.String(),
			"role", s.Role,
			"created_at", now.Format(time.RFC3339Nano),
			"last_active_at", now.Format(time.RFC3339Nano),
		)
		p.Expire(ctx, key, ttl)
		p.SAdd(ctx, indexKey, s.ID.String())
		// Keep the user index for as long as any of the user's sessions may live.
		p.Expire(ctx, indexKey, ttl+time.Minute)
		return nil
	})
	if err != nil {
		c.logDependency(ctx, err)
		return domain.NewDependencyUnavailable("session store unavailable")
	}
	return nil
}

// Get returns the session for id, or (nil, unauthenticated) when the session is
// missing or expired. It never returns (nil, nil), so auth middleware cannot
// mistake an unknown session for an authenticated empty one.
func (c *Client) Get(ctx context.Context, id uuid.UUID) (*ports.Session, error) {
	if id == uuid.Nil {
		return nil, domain.NewUnauthenticated("session is not found or has expired")
	}
	m, err := c.rdb.HGetAll(ctx, keySessionPrefix+id.String()).Result()
	if err != nil {
		c.logDependency(ctx, err)
		return nil, domain.NewDependencyUnavailable("session store unavailable")
	}
	if len(m) == 0 {
		return nil, domain.NewUnauthenticated("session is not found or has expired")
	}
	s, err := sessionFromHash(m)
	if err != nil {
		// Corrupted payloads must never become a partial authenticated session.
		c.log.FromContext(ctx).Debug("session payload rejected", "session_id", id.String())
		return nil, domain.NewUnauthenticated("session is not found or has expired")
	}
	return s, nil
}

// Touch implements the sliding inactivity window: it atomically resets
// last_active_at and extends the TTL, but returns unauthenticated (without
// extending) once the session has already expired.
func (c *Client) Touch(ctx context.Context, id uuid.UUID, ttl time.Duration) error {
	if id == uuid.Nil {
		return domain.NewUnauthenticated("session is not found or has expired")
	}
	if ttl <= 0 {
		return errors.New("rediscache: session TTL must be positive")
	}
	key := keySessionPrefix + id.String()
	v, err := sessionTouchScript.Run(ctx, c.rdb,
		[]string{key},
		ttl.Milliseconds(),
		time.Now().UTC().Format(time.RFC3339Nano),
	).Int()
	if err != nil {
		c.logDependency(ctx, err)
		return domain.NewDependencyUnavailable("session store unavailable")
	}
	if v == 0 {
		return domain.NewUnauthenticated("session is not found or has expired")
	}
	return nil
}

// Delete removes the session and its membership in the user index. It is
// idempotent for an already-absent session.
func (c *Client) Delete(ctx context.Context, id uuid.UUID) error {
	if id == uuid.Nil {
		return nil
	}
	userID := uuid.Nil
	if m, err := c.rdb.HGetAll(ctx, keySessionPrefix+id.String()).Result(); err == nil && len(m) > 0 {
		if uid, perr := uuid.Parse(m["user_id"]); perr == nil {
			userID = uid
		}
	}
	if err := c.rdb.Del(ctx, keySessionPrefix+id.String()).Err(); err != nil {
		c.logDependency(ctx, err)
		return domain.NewDependencyUnavailable("session store unavailable")
	}
	if userID != uuid.Nil {
		if err := c.rdb.SRem(ctx, keySessionIndexPrefix+userID.String(), id.String()).Err(); err != nil {
			c.logDependency(ctx, err)
			return domain.NewDependencyUnavailable("session store unavailable")
		}
	}
	return nil
}

// RevokeAllForUser invalidates every active session for a user and clears the
// user index in one atomic Lua step (UserDeprovisioned event consumer).
func (c *Client) RevokeAllForUser(ctx context.Context, userID uuid.UUID) error {
	if userID == uuid.Nil {
		return nil
	}
	indexKey := keySessionIndexPrefix + userID.String()
	if _, err := revokeAllScript.Run(ctx, c.rdb, []string{indexKey}).Result(); err != nil {
		c.logDependency(ctx, err)
		return domain.NewDependencyUnavailable("session store unavailable")
	}
	return nil
}

func sessionFromHash(m map[string]string) (*ports.Session, error) {
	id, err := uuid.Parse(m["id"])
	if err != nil {
		return nil, err
	}
	userID, err := uuid.Parse(m["user_id"])
	if err != nil {
		return nil, err
	}
	tenantID, err := uuid.Parse(m["tenant_id"])
	if err != nil {
		return nil, err
	}
	created, err := time.Parse(time.RFC3339Nano, m["created_at"])
	if err != nil {
		return nil, err
	}
	lastActive, err := time.Parse(time.RFC3339Nano, m["last_active_at"])
	if err != nil {
		return nil, err
	}
	return &ports.Session{
		ID:           id,
		UserID:       userID,
		TenantID:     tenantID,
		Role:         m["role"],
		CreatedAt:    created,
		LastActiveAt: lastActive,
	}, nil
}

// ---- Distributed lock primitives (scheduler) ----

// TryLock attempts to acquire lock {name} with token for ttl (SET NX PX). It
// returns (true, nil) on acquisition and (false, nil) while another replica
// holds it.
func (c *Client) TryLock(ctx context.Context, name string, ttl time.Duration, token string) (bool, error) {
	if err := validateLockArgs(name, ttl, token); err != nil {
		return false, err
	}
	ok, err := c.rdb.SetNX(ctx, keyLockPrefix+name, token, ttl).Result()
	if err != nil {
		c.logDependency(ctx, err)
		return false, domain.NewDependencyUnavailable("lock store unavailable")
	}
	return ok, nil
}

// RenewLock extends the lock only while the caller still owns the token; it
// returns ErrLockLost otherwise so the worker aborts before a lost lock double-runs.
func (c *Client) RenewLock(ctx context.Context, name string, ttl time.Duration, token string) error {
	if err := validateLockArgs(name, ttl, token); err != nil {
		return err
	}
	v, err := renewLockScript.Run(ctx, c.rdb, []string{keyLockPrefix + name}, token, ttl.Milliseconds()).Int()
	if err != nil {
		c.logDependency(ctx, err)
		return domain.NewDependencyUnavailable("lock store unavailable")
	}
	if v == 0 {
		return ErrLockLost
	}
	return nil
}

// ReleaseLock deletes the lock only if the stored token matches (compare-and-del).
func (c *Client) ReleaseLock(ctx context.Context, name string, token string) error {
	if name == "" || token == "" {
		return errors.New("rediscache: lock name and token are required")
	}
	v, err := releaseLockScript.Run(ctx, c.rdb, []string{keyLockPrefix + name}, token).Int()
	if err != nil {
		c.logDependency(ctx, err)
		return domain.NewDependencyUnavailable("lock store unavailable")
	}
	if v == 0 {
		return ErrLockLost
	}
	return nil
}

func validateLockArgs(name string, ttl time.Duration, token string) error {
	if name == "" || token == "" {
		return errors.New("rediscache: lock name and token are required")
	}
	if ttl <= 0 {
		return errors.New("rediscache: lock TTL must be positive")
	}
	return nil
}

// ---- Sliding-window rate limiting ----

// RateLimit applies a sliding-window limit (limit calls per window) keyed by
// rl:{route}:{client}, returning whether the call is allowed, the remaining
// allowance, and the reset time. The whole window is maintained atomically in
// Lua and bounded in O(window). The key is the full rl:{route}:{client} string
// supplied by the caller with a trusted discriminator.
func (c *Client) RateLimit(ctx context.Context, key string, limit int, window time.Duration) (allowed bool, remaining int, resetAt time.Time, err error) {
	if key == "" {
		return false, 0, time.Time{}, errors.New("rediscache: rate-limit key is required")
	}
	if limit <= 0 || window <= 0 {
		return false, 0, time.Time{}, errors.New("rediscache: rate-limit limit and window must be positive")
	}
	now := time.Now().UTC().UnixMilli()
	windowMs := window.Milliseconds()
	member := strconv.FormatInt(now, 10) + ":" + randString(6)

	res, rerr := rateLimitScript.Run(ctx, c.rdb,
		[]string{key},
		now, windowMs, int64(limit), member,
	).Int64Slice()
	if rerr != nil {
		c.logDependency(ctx, rerr)
		return false, 0, time.Time{}, domain.NewDependencyUnavailable("rate limit store unavailable")
	}
	if len(res) != 3 {
		return false, 0, time.Time{}, errors.New("rediscache: rate-limit script returned an unexpected result")
	}
	allowed = res[0] == 1
	remaining = int(res[1])
	resetAt = time.UnixMilli(res[2]).UTC()
	return allowed, remaining, resetAt, nil
}

func randString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

func (c *Client) logDependency(ctx context.Context, err error) {
	// The detailed cause is logged with the structured logger; callers only see
	// the opaque dependency-unavailable error returned to the HTTP layer.
	c.log.FromContext(ctx).Warn("redis dependency error", "cause", err.Error())
}

// ---- Lua scripts ----

// sessionTouchScript resets last_active_at and extends TTL when the session
// still exists (sliding window). Returns 0 when the session is already gone.
var sessionTouchScript = redis.NewScript(`
if redis.call("EXISTS", KEYS[1]) == 1 then
  redis.call("HSET", KEYS[1], "last_active_at", ARGV[2])
  redis.call("PEXPIRE", KEYS[1], ARGV[1])
  return 1
end
return 0
`)

// revokeAllScript deletes every session in the user index and drops the index,
// returning the number revoked.
var revokeAllScript = redis.NewScript(`
local ids = redis.call("SMEMBERS", KEYS[1])
for _, id in ipairs(ids) do
  redis.call("DEL", "ses:" .. id)
end
redis.call("DEL", KEYS[1])
return table.getn(ids)
`)

// renewLockScript extends TTL only while the caller still owns the token.
var renewLockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  redis.call("PEXPIRE", KEYS[1], ARGV[2])
  return 1
end
return 0
`)

// releaseLockScript compares the stored token before deleting (compare-and-del).
var releaseLockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  redis.call("DEL", KEYS[1])
  return 1
end
return 0
`)

// rateLimitScript evicts expired entries, counts the window, and - when the
// limit allows - records the call and returns {1, remaining, resetMs}; otherwise
// it returns {0, 0, resetMs} where resetMs is the oldest entry plus the window.
var rateLimitScript = redis.NewScript(`
local cutoff = ARGV[1] - ARGV[2]
redis.call("ZREMRANGEBYSCORE", KEYS[1], 0, cutoff)
local current = redis.call("ZCARD", KEYS[1])
if current < tonumber(ARGV[3]) then
  redis.call("ZADD", KEYS[1], ARGV[1], ARGV[4])
  redis.call("PEXPIRE", KEYS[1], ARGV[2])
  return {1, ARGV[3] - current - 1, ARGV[1] + ARGV[2]}
end
local first = redis.call("ZRANGE", KEYS[1], 0, 0, "WITHSCORES")
local reset = ARGV[1] + ARGV[2]
if first and first[2] then
  reset = tonumber(first[2]) + tonumber(ARGV[2])
end
return {0, 0, reset}
`)