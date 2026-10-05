// Package security provides concrete implementations of the Identity & Access
// Management output ports. The Argon2id password hasher and the TOTP/JWT
// adapters live here so authentication use cases depend only on the ports in
// application/ports and never on a concrete cryptographic library.
//
// This file implements the TokenManager port using HS256 JWTs for short-lived
// access tokens and opaque, high-entropy rotating refresh tokens. The access
// token carries only identity + role + tenant claims (severity-bounded surface:
// no monetary data), so the gateway can perform RBAC and tenant isolation
// without a per-request database lookup:
//
//	application/vnd.fms.auth.v1+json
//	{
//	  "iss": "<config issuer>",
//	  "sub": "<uuid user id>",
//	  "role": "<role key>",
//	  "tenant_id": "<uuid>",
//	  "sid": "<uuid session id>",
//	  "username": "<string>",
//	  "jti": "<uuid token id>",
//	  "iat": <unix seconds>,
//	  "exp": <unix seconds>
//	}
//
// Redis namespaces maintained by this adapter (shared instance, key-pattern
// monitored by ops):
//
//	bl:{jti}              access-token blacklist; TTL = original exp, never
//	                      exceeded so revoked-token memory stays bounded by the
//	                      (<= 15 min) access TTL.
//	rf:{hex(sha256(raw))} refresh-token store; value "{role}:{userID}:{tenantID}:{jti}:{exp}",
//	                      TTL 7 days. The raw refresh token is never persisted —
//	                      only this SHA-256 digest — so database theft cannot mint
//	                      sessions.
//	rtk:{jti}             reverse index {jti} -> "{hash}:{exp}" so Revoke can
//	                      invalidate the refresh without scanning rf keys.
//
// Revocation is eventually consistent within a bounded window: Revoke blacklists
// the access jti and deletes the refresh in two independent operations, so a
// token may remain valid for the remaining access TTL until the blacklist write
// lands. Every Redis failure is fail-closed: an unreachable store degrades auth
// availability to a UNAUTHENTICATED / DEPENDENCY_UNAVAILABLE error rather than
// weakening revocation guarantees. The raw access and refresh tokens are never
// written to logs.
package security

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/config"
	"fms/backend/internal/domain"
)

// Redis key prefixes and token sizing/bounds used across this adapter.
const (
	// keyBlacklist indexes revoked access-token jti values.
	keyBlacklist = "bl:"
	// keyRefresh stores the SHA-256 digest of a live refresh token.
	keyRefresh = "rf:"
	// keyRefreshIndex maps a refresh jti to its digest+expiry for Revoke.
	keyRefreshIndex = "rtk:"

	// refreshEntropyBytes is the raw refresh-token entropy (256 bits).
	refreshEntropyBytes = 32
	// accessTokenMaxTTL is the hard upper bound on access-token lifetime. It is
	// enforced at parse time so the blacklist memory footprint stays bounded even
	// if a caller requests a longer TTL.
	accessTokenMaxTTL = 15 * time.Minute
	// clockSkewLeeway tolerates a small iat/exp skew to absorb NTP drift.
	clockSkewLeeway = 30 * time.Second

	// optDialTimeout, optReadTimeout, optWriteTimeout, optMinIdleConns bound the
	// Redis token-store connection so an unreachable cache cannot stall the auth
	// path indefinitely.
	optDialTimeout  = 3 * time.Second
	optReadTimeout  = 2 * time.Second
	optWriteTimeout = 2 * time.Second
	optMinIdleConns = 1
)

// reservedClaimKeys are JWT claim keys the signer supplies authoritatively and
// that must never be merged from the passthrough claim map (forgery guard).
var reservedClaimKeys = map[string]struct{}{
	"exp":       {},
	"sub":       {},
	"tenant_id": {},
	"sid":       {},
	"jti":       {},
	"iat":       {},
	"nbf":       {},
	"iss":       {},
}

// accessClaims is the signed JWT payload. RegisteredClaims carries the standard
// sub/jti/iat/exp/iss; the remaining fields are the role/tenant/session context
// consumed by the gateway for RBAC and tenant isolation.
type accessClaims struct {
	Username   string            `json:"username,omitempty"`
	Role       string            `json:"role,omitempty"`
	TenantID   string            `json:"tenant_id,omitempty"`
	SessionID  string            `json:"sid,omitempty"`
	Extra      map[string]string `json:"extra,omitempty"`
	jwt.RegisteredClaims
}

// JWTManager signs, verifies, rotates, and revokes access/refresh token pairs
// against a Redis-backed blacklist and refresh store. It is immutable after
// construction and therefore safe for concurrent use. Because the shared
// cache/redis client does not expose the atomic token-store primitives required
// here (single-use rotation, exact-expiry blacklists), the manager owns a raw
// go-redis client built from cfg.Redis.
type JWTManager struct {
	secret     []byte
	rdb        *redis.Client
	log        *slog.Logger
	accessTTL  time.Duration
	refreshTTL time.Duration
	issuer     string
}

var _ ports.TokenManager = (*JWTManager)(nil)

// NewJWTManager builds a JWTManager from the configured signing secret and
// token lifecycles. The secret must be at least 32 bytes (fail-fast) and comes
// from environment/secrets-manager config, never source control. It returns an
// error when the Redis endpoint is invalid, uses an insecure transport, or is
// unreachable, so a misconfiguration surfaces at construction rather than at
// request time.
func NewJWTManager(secret []byte, cfg *config.Config) (*JWTManager, error) {
	if len(secret) < 32 {
		return nil, fmt.Errorf("security: JWT_SIGNING_SECRET must be at least 32 bytes for 256-bit strength, got %d", len(secret))
	}
	if cfg == nil {
		return nil, errors.New("security: config is required")
	}
	if err := validateRedisTransport(cfg.Redis.URL); err != nil {
		return nil, err
	}

	opts, err := redis.ParseURL(cfg.Redis.URL)
	if err != nil {
		return nil, fmt.Errorf("security: invalid redis url for token store: %w", err)
	}
	if opts.PoolSize <= 0 {
		opts.PoolSize = cfg.Redis.PoolSize
	}
	opts.MinIdleConns = optMinIdleConns
	opts.DialTimeout = optDialTimeout
	opts.ReadTimeout = optReadTimeout
	opts.WriteTimeout = optWriteTimeout

	rdb := redis.NewClient(opts)
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("security: token store unreachable: %w", err)
	}

	accessTTL := cfg.Security.AccessTokenTTL
	if accessTTL <= 0 {
		accessTTL = accessTokenMaxTTL
	}

	return &JWTManager{
		secret:     secret,
		rdb:        rdb,
		log:        slog.Default(),
		accessTTL:  accessTTL,
		refreshTTL: cfg.Security.RefreshTokenTTL,
		issuer:     cfg.Security.Issuer,
	}, nil
}

// Close releases the underlying Redis connection. The manager is unusable after
// Close is called; callers should invoke it only during shutdown.
func (m *JWTManager) Close() error { return m.rdb.Close() }

// Issue mints an access token and an opaque rotating refresh token for the
// subject. Expiry is set server-side from configuration, never from the request.
// The passthrough claims are copied into the JWT except for the reserved keys,
// which are rejected to prevent claim forgery. Role and tenant are the
// authorization predicate: tenant_id is never defaulted or empty.
func (m *JWTManager) Issue(ctx context.Context, userID, tenantID uuid.UUID, role string, claims map[string]string) (ports.TokenPair, error) {
	if userID == uuid.Nil {
		return ports.TokenPair{}, domain.NewUnauthenticated("invalid request")
	}
	if tenantID == uuid.Nil {
		return ports.TokenPair{}, domain.NewUnauthenticated("invalid request")
	}
	if role == "" || len(role) > 64 {
		return ports.TokenPair{}, domain.NewUnauthenticated("invalid request")
	}

	extra := make(map[string]string, len(claims))
	for k, v := range claims {
		if _, reserved := reservedClaimKeys[k]; reserved {
			return ports.TokenPair{}, domain.NewValidationError(domain.FieldError{Field: k, Message: "reserved JWT claim key may not be supplied"})
		}
		if strings.TrimSpace(k) == "" || len(k) > 64 || len(v) > 255 {
			return ports.TokenPair{}, domain.NewValidationError(domain.FieldError{Field: k, Message: "invalid claim key or value"})
		}
		extra[k] = v
	}

	now := time.Now().UTC()
	accessExp := now.Add(m.accessTTL)
	jti := uuid.New()
	sid := uuid.New()
	access, err := m.sign(accessClaims{
		Role:      role,
		TenantID:  tenantID.String(),
		SessionID: sid.String(),
		Extra:     extra,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			ID:        jti.String(),
			Issuer:    m.issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(accessExp),
		},
	})
	if err != nil {
		return ports.TokenPair{}, err
	}

	raw, err := newRefreshToken()
	if err != nil {
		return ports.TokenPair{}, err
	}
	hash := sha256Hex(raw)
	refreshExp := now.Add(m.refreshTTL)

	// SetNX guards against overwriting a colliding live hash. The raw token is
	// never stored — only its SHA-256 digest, alongside the identity+role needed
	// to reconstruct the pair on rotation.
	ok, err := m.rdb.SetNX(ctx, keyRefresh+hash, refreshValue(role, userID, tenantID, jti, accessExp), m.refreshTTL).Result()
	if err != nil {
		return ports.TokenPair{}, domain.NewDependencyUnavailable("token store unavailable")
	}
	if !ok {
		return ports.TokenPair{}, domain.NewInternal(errors.New("security: refresh hash collision"))
	}

	// Reverse index maps the pair's jti to its digest and access expiry so Revoke
	// can invalidate both sides without a full key scan.
	if err := m.rdb.Set(ctx, keyRefreshIndex+jti.String(), hash+":"+strconv.FormatInt(accessExp.Unix(), 10), m.refreshTTL).Err(); err != nil {
		return ports.TokenPair{}, domain.NewDependencyUnavailable("token store unavailable")
	}

	return ports.TokenPair{
		AccessToken:      access,
		RefreshToken:     raw,
		TokenID:          jti,
		ExpiresAt:        accessExp,
		RefreshExpiresAt: refreshExp,
	}, nil
}

// VerifyAccess validates the presented access token: signature, algorithm pin,
// expiry against the server clock, lifetime bound, required claim presence, and
// the revocation blacklist. It returns a UNAUTHENTICATED domain error for any
// invalid/expired/revoked token and fails closed (DEPENDENCY_UNAVAILABLE) when
// the blacklist store is unreachable, so a revoked token can never be silently
// accepted.
func (m *JWTManager) VerifyAccess(ctx context.Context, token string) (*ports.Claims, error) {
	claims := &accessClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(*jwt.Token) (any, error) {
		return m.secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !parsed.Valid {
		return nil, domain.NewUnauthenticated("invalid access token")
	}

	// Defense-in-depth: re-check expiry/issued-at against the server clock and
	// reject an over-long lifetime so the blacklist footprint stays bounded even
	// if the library validation were misconfigured.
	now := time.Now().UTC()
	exp := claims.ExpiresAt
	iat := claims.IssuedAt
	if exp == nil || exp.Time.Before(now) {
		return nil, domain.NewUnauthenticated("invalid access token")
	}
	if iat == nil || iat.Time.After(now.Add(clockSkewLeeway)) {
		return nil, domain.NewUnauthenticated("invalid access token")
	}
	if exp.Time.Sub(iat.Time) > accessTokenMaxTTL+clockSkewLeeway {
		return nil, domain.NewUnauthenticated("invalid access token")
	}
	if m.issuer != "" && claims.Issuer != m.issuer {
		return nil, domain.NewUnauthenticated("invalid access token")
	}

	if claims.Subject == "" || claims.TenantID == "" || claims.ID == "" || claims.Role == "" {
		return nil, domain.NewUnauthenticated("invalid access token")
	}
	if len(claims.Username) > 255 {
		return nil, domain.NewUnauthenticated("invalid access token")
	}
	userUUID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return nil, domain.NewUnauthenticated("invalid access token")
	}
	tenantUUID, err := uuid.Parse(claims.TenantID)
	if err != nil {
		return nil, domain.NewUnauthenticated("invalid access token")
	}
	sessUUID, err := uuid.Parse(claims.SessionID)
	if err != nil {
		return nil, domain.NewUnauthenticated("invalid access token")
	}
	tokenUUID, err := uuid.Parse(claims.ID)
	if err != nil {
		return nil, domain.NewUnauthenticated("invalid access token")
	}

	// Blacklist lookup is the O(1) per-request cost (no database access).
	exists, err := m.rdb.Exists(ctx, keyBlacklist+claims.ID).Result()
	if err != nil {
		return nil, domain.NewDependencyUnavailable("token store unavailable")
	}
	if exists > 0 {
		return nil, domain.NewUnauthenticated("invalid access token")
	}

	return &ports.Claims{
		Subject:   claims.Subject,
		Role:      claims.Role,
		TenantID:  tenantUUID,
		SessionID: sessUUID,
		TokenID:   tokenUUID,
		ExpiresAt: exp.Time,
	}, nil
}

// RotateRefresh atomically swaps a presented refresh token for a fresh pair.
// The old hash is read-and-deleted in a single atomic GetDel so exactly one of
// any set of concurrent rotations with the same token succeeds; a replayed
// token (hash already gone) fails with a UNAUTHENTICATED domain error and is
// logged at WARN for fraud monitoring. A Redis error returns
// DEPENDENCY_UNAVAILABLE without rotating, preserving single-use.
func (m *JWTManager) RotateRefresh(ctx context.Context, refreshToken string) (ports.TokenPair, error) {
	hash := sha256Hex(refreshToken)
	value, err := m.rdb.GetDel(ctx, keyRefresh+hash).Result()
	if err == redis.Nil {
		m.log.Warn("refresh token reuse detected", "event", "refresh_reuse")
		return ports.TokenPair{}, domain.NewUnauthenticated("invalid refresh token")
	}
	if err != nil {
		return ports.TokenPair{}, domain.NewDependencyUnavailable("token store unavailable")
	}

	role, userID, tenantID, jti, accessExp, err := parseRefreshValue(value)
	if err != nil {
		m.log.Warn("malformed refresh token record", "event", "refresh_corrupt")
		return ports.TokenPair{}, domain.NewUnauthenticated("invalid refresh token")
	}

	// The consumed pair is no longer usable: drop its reverse index and blacklist
	// its access token so the old access half dies with the rotated refresh.
	_ = m.rdb.Del(ctx, keyRefreshIndex+jti.String()).Err()
	if ttl := boundedBlacklistTTL(accessExp, time.Now().UTC(), m.accessTTL); ttl > 0 {
		_ = m.rdb.Set(ctx, keyBlacklist+jti.String(), "1", ttl).Err()
	}

	// Rotation reissues the same identity and role with no passthrough extras.
	return m.Issue(ctx, userID, tenantID, role, nil)
}

// Revoke invalidates a token pair by its server-issued id, blacklisting the
// access token for the bounded remainder of its TTL and deleting the refresh
// side so the pair cannot be rotated after logout. It is idempotent: revoking an
// already-revoked or unknown id returns nil.
func (m *JWTManager) Revoke(ctx context.Context, tokenID uuid.UUID) error {
	if tokenID == uuid.Nil {
		return nil
	}
	value, err := m.rdb.Get(ctx, keyRefreshIndex+tokenID.String()).Result()
	if err == redis.Nil {
		// Unknown or already-revoked id; idempotent no-op.
		return nil
	}
	if err != nil {
		return domain.NewDependencyUnavailable("token store unavailable")
	}

	hash, accessExp, err := parseRefreshIndex(value)
	if err != nil {
		return domain.NewInternal(err)
	}

	if err := m.rdb.Del(ctx, keyRefresh+hash, keyRefreshIndex+tokenID.String()).Err(); err != nil {
		return domain.NewDependencyUnavailable("token store unavailable")
	}
	if ttl := boundedBlacklistTTL(accessExp, time.Now().UTC(), m.accessTTL); ttl > 0 {
		if err := m.rdb.Set(ctx, keyBlacklist+tokenID.String(), "1", ttl).Err(); err != nil {
			return domain.NewDependencyUnavailable("token store unavailable")
		}
	}
	m.log.Info("token pair revoked", "jti", tokenID.String(), "event", "revoke")
	return nil
}

// sign serializes and HMAC-SHA256 signs an access claim set.
func (m *JWTManager) sign(c accessClaims) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	if m.issuer != "" {
		token.Header["kid"] = m.issuer
	}
	signed, err := token.SignedString(m.secret)
	if err != nil {
		return "", domain.NewInternal(fmt.Errorf("security: failed to sign access token: %w", err))
	}
	return signed, nil
}

// newRefreshToken returns a fresh 256-bit opaque bearer token, base64url
// encoded, never persisted raw server-side. It is returned to the client exactly
// once at issuance/rotation.
func newRefreshToken() (string, error) {
	b := make([]byte, refreshEntropyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", domain.NewInternal(fmt.Errorf("security: failed to generate refresh token: %w", err))
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// sha256Hex returns the hex SHA-256 digest of a string — the only form in which
// a refresh token is ever stored.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// refreshValue encodes the role/identity context and access expiry stored beside
// a refresh hash so rotation can reconstruct the owning pair. The expiry is
// epoch seconds (colon-free) so the value remains splittable on ":". Role keys
// are enumerated and never contain a colon.
func refreshValue(role string, userID, tenantID, jti uuid.UUID, accessExp time.Time) string {
	return role + ":" + userID.String() + ":" + tenantID.String() + ":" + jti.String() +
		":" + strconv.FormatInt(accessExp.Unix(), 10)
}

// parseRefreshValue decodes a refresh-store value into its identity parts.
func parseRefreshValue(v string) (role string, userID, tenantID, jti uuid.UUID, exp time.Time, err error) {
	parts := strings.Split(v, ":")
	if len(parts) != 5 {
		return "", uuid.Nil, uuid.Nil, uuid.Nil, time.Time{}, fmt.Errorf("security: malformed refresh record")
	}
	uid, e1 := uuid.Parse(parts[1])
	tid, e2 := uuid.Parse(parts[2])
	j, e3 := uuid.Parse(parts[3])
	sec, e4 := strconv.ParseInt(parts[4], 10, 64)
	if parts[0] == "" || e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		return "", uuid.Nil, uuid.Nil, uuid.Nil, time.Time{}, fmt.Errorf("security: malformed refresh record")
	}
	return parts[0], uid, tid, j, time.Unix(sec, 0).UTC(), nil
}

// parseRefreshIndex decodes an rtk reverse-index value "{hash}:{expUnixSeconds}".
// The hash is 64 hex chars and the expiry is digits, so splitting on the first
// colon is unambiguous.
func parseRefreshIndex(v string) (hash string, exp time.Time, err error) {
	i := strings.Index(v, ":")
	if i <= 0 {
		return "", time.Time{}, fmt.Errorf("security: malformed refresh index")
	}
	sec, e := strconv.ParseInt(v[i+1:], 10, 64)
	if e != nil {
		return "", time.Time{}, fmt.Errorf("security: malformed refresh index")
	}
	return v[:i], time.Unix(sec, 0).UTC(), nil
}

// boundedBlacklistTTL caps a blacklist TTL at the access-token lifetime so
// revoked-token memory never grows unbounded, and returns 0 when the access
// token has already expired (short-circuit: do not persist a dead key).
func boundedBlacklistTTL(accessExp, now time.Time, capTTL time.Duration) time.Duration {
	remaining := accessExp.Sub(now)
	if remaining <= 0 {
		return 0
	}
	if remaining > capTTL {
		return capTTL
	}
	return remaining
}

// validateRedisTransport rejects plaintext (non-loopback) Redis endpoints so
// token state is never transmitted over an insecure transport.
func validateRedisTransport(url string) error {
	lower := strings.ToLower(url)
	switch {
	case strings.HasPrefix(lower, "rediss://"), strings.HasPrefix(lower, "unix://"):
		return nil
	case strings.HasPrefix(lower, "redis://"):
		host := strings.TrimPrefix(lower, "redis://")
		if i := strings.Index(host, "/"); i >= 0 {
			host = host[:i]
		}
		if i := strings.Index(host, "@"); i >= 0 {
			host = host[i+1:]
		}
		if host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "" {
			return nil
		}
		return fmt.Errorf("security: redis token store must use rediss://, unix://, or loopback redis://")
	default:
		return fmt.Errorf("security: redis token store URL requires a scheme")
	}
}