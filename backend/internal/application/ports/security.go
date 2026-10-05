// Package ports declares the driven/output ports of the application layer.
//
// This file defines the security seams for the Identity & Access Management
// bounded context: password hashing, token issuance/verification/rotation, and
// TOTP multi-factor authentication. It is interface-only — the concrete
// implementations (argon2id hashing, JWT signing, RFC 6238 TOTP, Redis-backed
// revocation) live in internal/security and depend on these ports rather than
// the reverse. The ports depend only on the standard library, uuid, and domain
// errors, preserving the inward-pointing Clean Architecture dependency rule and
// keeping authentication use cases framework-agnostic.
//
// Non-negotiables the implementations must honour:
//   - Passwords are hashed with Argon2id into a self-describing PHC string
//     ($argon2id$v=19$m=65536,t=3,p=2$salt$hash) so cost parameters can be
//     upgraded without invalidating stored hashes; salts are per-user random,
//     at least 16 bytes; verification is constant-time and never leaks whether
//     failure was a malformed hash versus a wrong password.
//   - Access tokens are short-TTL JWTs with a pinned signature algorithm (no
//     algorithm negotiation). Every VerifyAccess enforces signature, expiry
//     against the server clock, and the revocation blacklist.
//   - Refresh tokens are opaque high-entropy bearer secrets, never JWTs. The
//     server persists only their hash, rotates them single-use, and stores the
//     raw token only in the TokenPair handed to the client.
//   - MFA (TOTP) is enforced for all users; there is no "MFA off" path.
package ports

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"fms/backend/internal/domain"
)

// ErrInvalidHash is returned by a password-hasher implementation when the
// stored PHC string cannot be parsed or is structurally invalid. It lives in
// the application layer so use cases can detect a corrupt stored credential
// without importing the concrete infrastructure adapter. Callers surface it as
// a generic invalid-credentials boundary error and must never disclose its
// contents to a client (timing and secret-hygiene non-disclosure).
var ErrInvalidHash = errors.New("invalid password hash")

// PasswordHasher is the output port for adaptive password hashing. Use cases
// depend on this seam rather than on a concrete Argon2id library so hash
// algorithm and cost-parameter policy can evolve without touching application
// logic.
type PasswordHasher interface {
	// Hash derives an Argon2id PHC string for the given plaintext. It uses a
	// fresh per-user random salt (>= 16 bytes) on every call; the encoded form
	// is self-describing so cost parameters can be raised later without
	// re-encoding existing hashes. Passwords must never be logged or embedded
	// in returned error text.
	Hash(ctx context.Context, password string) (string, error)

	// Verify compares a plaintext against a stored PHC hash using constant-time
	// comparison. It returns (false, nil) on a plain mismatch, and (false, err)
	// only when the stored hash is malformed (ErrInvalidHash) or an
	// infrastructure error occurs. Callers must not use the error to
	// distinguish a wrong password from a corrupt record (timing
	// non-disclosure).
	Verify(ctx context.Context, hash, password string) (bool, error)

	// NeedsRehash reports whether the stored PHC hash was derived with cost
	// parameters below the implementer's current configured defaults, so the
	// caller can transparently upgrade legacy hashes on the next successful
	// login. A malformed or unparseable hash returns (false, ErrInvalidHash).
	NeedsRehash(ctx context.Context, hash string) (bool, error)
}

// TokenPair is the token artifact returned by issuance and rotation. RefreshToken
// is an opaque high-entropy bearer secret that the server stores only as a hash;
// TokenID is the server-issued handle used to revoke the pair. ExpiresAt bounds
// the access token; RefreshExpiresAt bounds the rotation lifecycle, which outlives
// the short access TTL.
type TokenPair struct {
	AccessToken      string
	RefreshToken     string
	TokenID          uuid.UUID
	ExpiresAt        time.Time
	RefreshExpiresAt time.Time
}

// Claims is the server-authoritative, verified identity carried by an access
// token. It is produced by the signer and reconstructed from the token signature
// on every VerifyAccess — Role and TenantID are never trusted from client input.
// TenantID injects the tenancy predicate into authorization; SessionID drives the
// inactivity timeout and bulk session revocation; TokenID is the standard jti
// claim binding the token to the Revoke seam.
type Claims struct {
	Subject   string
	Role      string
	TenantID  uuid.UUID
	SessionID uuid.UUID
	TokenID   uuid.UUID
	ExpiresAt time.Time
}

// TokenManager is the output port for JWT access tokens and rotating refresh
// tokens. Implementations pin a single signing algorithm, enforce signature +
// expiry + revocation on every verification, and treat refresh-token rotation
// and revocation as once-only, replicate-wide operations.
type TokenManager interface {
	// Issue mints an access token and an opaque refresh token for the subject.
	// Expiry is set server-side (never accepted from the request). claims is
	// optional extra-claim passthrough (e.g. scopes); reserved JWT claim keys
	// (exp, sub, tenant_id, sid, jti) must be rejected, not silently merged.
	Issue(ctx context.Context, userID, tenantID uuid.UUID, role string, claims map[string]string) (TokenPair, error)

	// VerifyAccess validates the presented access token and returns its claims.
	// Every call checks signature, expiry against the server clock, and the
	// revocation blacklist. It returns a UNAUTHENTICATED domain error for an
	// invalid, expired, or revoked token. It must fail closed (not silently
	// succeed) when the revocation store is unreachable.
	VerifyAccess(ctx context.Context, token string) (*Claims, error)

	// RotateRefresh swaps the presented refresh token for a fresh pair,
	// atomically shadowing/revoking the old token so a replayed token fails
	// rather than re-issuing. It returns a UNAUTHENTICATED domain error when the
	// refresh token is unknown, expired, or already rotated.
	RotateRefresh(ctx context.Context, refreshToken string) (TokenPair, error)

	// Revoke invalidates a token by its server-issued id. It is idempotent —
	// revoking an already-revoked or unknown id returns nil — and it propagates
	// to the shared blacklist so revocation is effective across all replicas.
	Revoke(ctx context.Context, tokenID uuid.UUID) error
}

// MfaVerifier is the output port for RFC 6238 TOTP multi-factor
// authentication. Implementations enforce constant-time comparison, tolerate
// ±1 time-step clock skew, and reject reused codes to prevent replay.
type MfaVerifier interface {
	// Verify checks the supplied six-digit TOTP code against the given secret.
	// It returns (false, nil) on a mismatch or malformed code and (false, err)
	// only on an infrastructure failure, so callers can distinguish an
	// authentication failure from a downstream 5xx without disclosing why a
	// code was rejected.
	Verify(ctx context.Context, secret, code string) (bool, error)

	// GenerateSecret returns a base32-encoded TOTP secret with at least 128 bits
	// of entropy (160-bit/20-byte recommended). The secret is exposed only for
	// enrollment and must never be logged or written to audit records.
	GenerateSecret() (string, error)
}