// Package ports declares the driven/output ports of the application layer.
//
// This file defines the session persistence seam for the Identity & Access
// Management bounded context. It is interface-only: the concrete Redis-backed
// implementation lives in internal/security/session.go and depends on this
// port rather than the reverse. The port depends only on the standard library
// and on domain errors, preserving the inward-pointing Clean Architecture
// dependency rule.
package ports

import (
	"context"
	"time"

	"github.com/google/uuid"

	"fms/backend/internal/domain"
)

// Session is a server-side authentication session record carried across the
// application/security-store boundary. It is a transport object, not a domain
// entity: it carries only identifiers, a role claim, and timestamps — no PII,
// financial data, or secrets. Role and TenantID are server-issued read-only
// state, never derived from client claims.
type Session struct {
	ID           uuid.UUID
	UserID       uuid.UUID
	TenantID     uuid.UUID
	Role         string
	CreatedAt    time.Time
	LastActiveAt time.Time
}

// newSessionUnauthenticated is the single statement of the session→UNAUTHENTICATED
// mapping. It is returned by the store when a session is missing or has naturally
// expired and is mapped by use cases to the 401 API contract via domain.AsDomainError.
// Adapter and use-case layers agree on this contract; no session ID or user ID is
// ever embedded in the message, keeping identifiers out of logs and error payloads.
func newSessionUnauthenticated() error {
	return domain.NewUnauthenticated("session is not found or has expired")
}

// SessionStore is the output port through which application layer code
// persists and revokes server-side sessions. The 30-minute inactivity window
// is passed as an explicit ttl on Create/Touch (a caller/use-case policy
// concern); the store only applies the sliding TTL, never a fixed timeout.
type SessionStore interface {
	// Create persists the session with an expiry of now+ttl. Writes are
	// atomic: the session is not visible to Get until fully written.
	Create(ctx context.Context, s Session, ttl time.Duration) error

	// Get returns the session, or nil with a UNAUTHENTICATED domain error
	// (newSessionUnauthenticated) when the session is missing or has naturally
	// expired — the store treats both identically. It never returns (nil, nil).
	// Reads never extend the TTL; only Touch does.
	Get(ctx context.Context, id uuid.UUID) (*Session, error)

	// Touch atomically resets LastActiveAt to now and resets the sliding TTL
	// to now+ttl (a true sliding window). It returns newSessionUnauthenticated
	// when the session is absent/expired so the caller re-authenticates. It
	// does not require fetching the full session object.
	Touch(ctx context.Context, id uuid.UUID, ttl time.Duration) error

	// Delete hard-revokes the session (logout, role change, deprovision). It is
	// idempotent: deleting an already-absent session returns nil.
	Delete(ctx context.Context, id uuid.UUID) error

	// RevokeAllForUser invalidates every session for a deprovisioned user. The
	// UserDeprovisioned event consumer calls this once to cut off all active
	// sessions atomically, rather than sweeping keys per session.
	RevokeAllForUser(ctx context.Context, userID uuid.UUID) error
}