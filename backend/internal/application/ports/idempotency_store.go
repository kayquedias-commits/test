// Package ports declares the driven/output ports of the application layer.
//
// IdempotencyStore is the deduplication seam for write APIs: calls that accept
// an optional Idempotency-Key header store the serialized response per key so a
// duplicate submission is replayed instead of re-executed (Blueprint §7.2). The
// interface is intentionally framework- and driver-free; the Redis-backed
// adapter lives in internal/cache/redis.go.
//
// The port takes caller-composed keys and stays tenancy-agnostic. Callers MUST
// derive the composite key server-side from the authenticated IAM context as
// `idem:{tenant_id}:{user_id}:{sha256(method|path|raw_key)}` — never from raw
// client input — otherwise a client-supplied value could collide with another
// user's slot (cross-user replay). Idempotency entries are non-authoritative
// replay state and must never back the ledger.
package ports

import (
	"context"
	"time"

	"fms/backend/internal/domain"
)

// NewStoreUnavailable wraps a cause in a DEPENDENCY_UNAVAILABLE domain error,
// giving the Redis adapter a single, consistent error contract that use cases
// map to HTTP 503 (§7.7). Driver-specific error types never leak through the port.
func NewStoreUnavailable(cause error) error {
	return &domain.DomainError{
		Code:    domain.CodeDependencyUnavailable,
		Message: "idempotency store unavailable",
		Cause:   cause,
	}
}

// IdempotencyStore persists and replays serialized write responses keyed by an
// Idempotency-Key. The stored value is an opaque serialized response (HTTP
// status + body) that is replayed byte-identically on a duplicate request.
//
// Correct use-case flow:
//  1. Get(key) — replay the returned value if found, otherwise proceed.
//  2. StoreIfAbsent(key, placeholder, ttl) — if false, a duplicate is in flight:
//     poll Get(key) until the winner's final response appears, then replay it.
//     If true, the caller wins the slot and proceeds with the write.
//  3. Store(key, finalResponse, ttl) — the winner overwrites its placeholder
//     with the final response, refreshing the TTL.
//
// Implementations must enforce SET-NX atomicity for StoreIfAbsent (a non-atomic
// Get-then-Set would permit a TOCTOU double-spend under concurrent first-writes),
// clamp TTL server-side (bounded: 60s..24h) and cap the payload size. The TTL
// parameter also lets callers store error responses (4xx/5xx) with a shorter
// lifetime so a retry after a transient failure gets a fresh attempt.
type IdempotencyStore interface {
	// Get returns the stored response to replay. Absence or TTL expiry yields
	// (nil, false, nil) — absence is normal, not an error, and allows retry.
	// The error return is reserved for infrastructure failure and must be a
	// DEPENDENCY_UNAVAILABLE domain error, not a driver type.
	Get(ctx context.Context, key string) ([]byte, bool, error)

	// Store atomically writes response for key with the given ttl, overwriting
	// any previous value (used by the winner to replace its placeholder with
	// the final response). It does not fail when the key already exists.
	Store(ctx context.Context, key string, response []byte, ttl time.Duration) error

	// StoreIfAbsent atomically stores response only if key is absent (SET NX),
	// claiming the idempotency slot. It returns true if stored (the caller owns
	// the slot) and false if the key already exists (a duplicate is in flight
	// and the caller must Get to replay the winner). The error return is
	// reserved for infrastructure failure only.
	StoreIfAbsent(ctx context.Context, key string, response []byte, ttl time.Duration) (bool, error)
}