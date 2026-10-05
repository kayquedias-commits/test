// Package security implements the authentication-side infrastructure adapters
// (password hashing, MFA, and the server-side session store).
//
// This file provides the SessionStore port adapter. Session persistence and the
// 30-minute sliding inactivity window are owned by rediscache.Client (sessions
// stored as Redis hashes `ses:{id}` with the `sesuser:{user_id}` index used by
// RevokeAllForUser, all manipulated by atomic Lua scripts); this type wraps that
// canonical store and supplies the validated default inactivity timeout so the
// composition root can wire the configured SESSION_TIMEOUT once.
package security

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/config"
	rediscache "fms/backend/internal/infrastructure/cache"
)

const (
	sessionTimeoutMin = 1 * time.Minute
	sessionTimeoutMax = 24 * time.Hour
)

// SessionStore adapts the rediscache session store to the SessionStore port.
// It does not reimplement storage: serialization, atomic Touch/revocation, and
// the sliding-window TTL are delegated to rediscache.Client. The configured
// inactivity timeout is applied as the default TTL when a caller passes a
// non-positive ttl.
type SessionStore struct {
	rc      *rediscache.Client
	timeout time.Duration
}

var _ ports.SessionStore = (*SessionStore)(nil)

// NewSessionStore wraps the Redis session store and validates SESSION_TIMEOUT
// at startup, failing fast (panic) on a value outside the 1-minute..24-hour
// bounds so an invalid knob never reaches the request path.
func NewSessionStore(rc *rediscache.Client, cfg *config.Config) *SessionStore {
	t := cfg.Security.SessionTimeout
	if t < sessionTimeoutMin || t > sessionTimeoutMax {
		panic(fmt.Sprintf("security: SESSION_TIMEOUT must be between %s and %s, got %s", sessionTimeoutMin, sessionTimeoutMax, t))
	}
	return &SessionStore{rc: rc, timeout: t}
}

// Timeout returns the validated default inactivity window.
func (s *SessionStore) Timeout() time.Duration { return s.timeout }

// Create persists the session atomically with an expiry of now+ttl; a
// non-positive ttl falls back to the configured SESSION_TIMEOUT.
func (s *SessionStore) Create(ctx context.Context, sess ports.Session, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = s.timeout
	}
	return s.rc.Create(ctx, sess, ttl)
}

// Get returns the session, or the port's UNAUTHENTICATED error when missing or
// expired (handled by rediscache). Reads never extend the TTL.
func (s *SessionStore) Get(ctx context.Context, id uuid.UUID) (*ports.Session, error) {
	return s.rc.Get(ctx, id)
}

// Touch atomically resets LastActiveAt and the sliding TTL; a non-positive ttl
// falls back to the configured SESSION_TIMEOUT.
func (s *SessionStore) Touch(ctx context.Context, id uuid.UUID, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = s.timeout
	}
	return s.rc.Touch(ctx, id, ttl)
}

// Delete hard-revokes the session and its user-index membership (idempotent).
func (s *SessionStore) Delete(ctx context.Context, id uuid.UUID) error {
	return s.rc.Delete(ctx, id)
}

// RevokeAllForUser invalidates every session for a deprovisioned user
// atomically via the user-index (UserDeprovisioned event consumer).
func (s *SessionStore) RevokeAllForUser(ctx context.Context, userID uuid.UUID) error {
	return s.rc.RevokeAllForUser(ctx, userID)
}