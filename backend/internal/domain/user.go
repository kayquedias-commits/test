package domain

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// This file defines the User aggregate root of the Identity & Access bounded
// context. It carries the auth identity (username, email, opaque credential
// references) and the governance invariants: role assignment, deactivation,
// terminal deprovisioning, the "last admin cannot be removed" guard, and
// last-login tracking. The password hash and the encrypted MFA secret are
// treated as opaque references only — no crypto logic and no plaintext ever
// lives here (specification §5.1, §9.1).

// UserStatus is the lifecycle state of a User account. Deprovisioning is
// terminal and irreversible; suspension is a reversible administrative state.
// A single boolean could not distinguish these, collapsing the audit trail's
// semantic value, so an enum is used.
type UserStatus string

const (
	// UserStatusActive is the normal provisioning state; the account may auth.
	UserStatusActive UserStatus = "ACTIVE"
	// UserStatusSuspended is set by Deactivate; the account may not auth but can
	// be reactivated later by an administrator.
	UserStatusSuspended UserStatus = "SUSPENDED"
	// UserStatusDeprovisioned is the terminal state set by Deprovision.
	UserStatusDeprovisioned UserStatus = "DEPROVISIONED"
)

// Sentinel errors for state-transition guards. Wrapped by DomainError so the
// API contract maps them to the correct code (CONFLICT_STATE / FORBIDDEN).
var (
	errUserDeprovisioned        = errors.New("domain: user is deprovisioned")
	errUserAlreadySuspended     = errors.New("domain: user is already suspended")
	errUserAlreadyDeprovisioned = errors.New("domain: user is already deprovisioned")
)

// usernameRE is the structural allowlist for usernames. It rejects control
// characters, whitespace, and canonicalization-conflicting unicode, and bounds
// length. Applied after trimming and lowercasing.
var usernameRE = regexp.MustCompile(`^[a-zA-Z0-9._-]{3,64}$`)

// emailRE is a simplified RFC 5322 structural allowlist. It rejects CR/LF so a
// value can never be used for header/command injection should it ever reach an
// email or SFTP adapter, and requires a dot in the domain part.
var emailRE = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// User is the aggregate root for the Identity & Access bounded context. All
// fields except Version are immutable after construction; state changes flow
// exclusively through the mutation methods below, never via direct field
// writes (prevents mass assignment of Active/RoleID from untrusted input).
//
// Secret-handling contract: PasswordHash and MFASecret are opaque references
// (the hash and the AES-256-GCM ciphertext respectively) produced by the
// authentication/persistence adapters. The aggregate must never log them, must
// never include them in a domain event, and must never decrypt/interpret the
// MFA ciphertext.
type User struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	Username     string
	Email        string
	PasswordHash string
	MFASecret    string
	MFAActive    bool
	RoleID       uuid.UUID
	Status       UserStatus
	LastLoginAt  *time.Time
	Version      int64
}

// NewUser validates and constructs a User from untrusted input. ID, TenantID,
// and RoleID must be non-zero; Username and Email are allowlist-validated,
// trimmed, and lowercased. PasswordHash and MFASecret are accepted as opaque
// references. The account is created ACTIVE with Version 0.
func NewUser(id, tenantID uuid.UUID, username, email, passwordHash, mfaSecret string, mfaActive bool, roleID uuid.UUID) (User, error) {
	username = strings.TrimSpace(username)
	email = strings.TrimSpace(strings.ToLower(email))
	if id == uuid.Nil || tenantID == uuid.Nil {
		return User{}, NewValidationError(
			FieldError{Field: "id", Message: "user and tenant ids must be non-zero"},
		)
	}
	if !usernameRE.MatchString(username) {
		return User{}, NewValidationError(
			FieldError{Field: "username", Message: "must be 3-64 [a-zA-Z0-9._-] characters"},
		)
	}
	if len(email) == 0 || len(email) > 254 || strings.ContainsAny(email, "<>\r\n\x00") || !emailRE.MatchString(email) {
		return User{}, NewValidationError(
			FieldError{Field: "email", Message: "must be a valid email address"},
		)
	}
	if roleID == uuid.Nil {
		return User{}, NewValidationError(
			FieldError{Field: "role_id", Message: "must be a non-zero role id"},
		)
	}
	return User{
		ID:           id,
		TenantID:     tenantID,
		Username:     strings.ToLower(username),
		Email:        email,
		PasswordHash: passwordHash,
		MFASecret:    mfaSecret,
		MFAActive:    mfaActive,
		RoleID:       roleID,
		Status:       UserStatusActive,
		Version:      0,
	}, nil
}

// Active reports whether the account is in the normal provisioning state and
// may authenticate. It is the boolean projection of Status kept for callers
// that filter or query on the legacy active flag; it never becomes the source
// of truth for lifecycle state.
func (u *User) Active() bool {
	return u.Status == UserStatusActive
}

// AssignRole assigns the given role to the user. It rejects the zero uuid and
// rejects assignment on a deprovisioned account (terminal: no state change may
// follow deprovisioning). Authorization (that the actor may change this role
// in this tenant) and role existence are enforced by the calling use case
// before this method is reached.
func (u *User) AssignRole(roleID uuid.UUID) error {
	if roleID == uuid.Nil {
		return NewValidationError(FieldError{Field: "role_id", Message: "must be a non-zero role id"})
	}
	if u.Status == UserStatusDeprovisioned {
		return NewConflict(errUserDeprovisioned.Error())
	}
	u.RoleID = roleID
	u.Version++
	return nil
}

// Deactivate suspends the account. It is idempotent-guarded: a second call on
// an already-suspended account returns a CONFLICT_STATE error so the audit
// path does not emit duplicate transition events. Deprovisioned accounts are
// terminal and refuse suspension. Re-activation is an administrative,
// use-case-level action that sets Status back to UserStatusActive.
func (u *User) Deactivate() error {
	if u.Status == UserStatusDeprovisioned {
		return NewConflict(errUserDeprovisioned.Error())
	}
	if u.Status == UserStatusSuspended {
		return NewConflict(errUserAlreadySuspended.Error())
	}
	u.Status = UserStatusSuspended
	u.Version++
	return nil
}

// Deprovision permanently revokes the account: it sets the terminal
// DEPROVISIONED status and returns the UserDeprovisioned payload for the
// application layer to wrap into an Event with type EventUserDeprovisioned and
// publish through the outbox in the same ACID transaction. The payload carries
// only UserID — it must never carry PasswordHash, MFASecret, or Email. A
// repeated call returns CONFLICT_STATE because deprovisioning is irreversible.
func (u *User) Deprovision() (UserDeprovisionedPayload, error) {
	if u.Status == UserStatusDeprovisioned {
		return UserDeprovisionedPayload{}, NewConflict(errUserAlreadyDeprovisioned.Error())
	}
	u.Status = UserStatusDeprovisioned
	u.Version++
	return UserDeprovisionedPayload{UserID: u.ID}, nil
}

// CanDelete enforces the "last admin cannot be removed" acceptance criterion:
// an ACTIVE admin user cannot be deprovisioned when the tenant's total active
// admin count (including this user) is at most one.
//
// activeAdminCount is supplied by the calling use case and MUST be counted
// within the same transaction boundary as the subsequent delete/deprovision,
// using a tenant-scoped predicate and row-level locks, to close the TOCTOU
// window (a concurrent deactivation could otherwise invalidate the count and
// remove the final active admin). Whether this user's RoleID denotes an admin
// role is decided at that use-case boundary — the aggregate holds only a role
// reference and cannot read the Role aggregate.
func (u *User) CanDelete(activeAdminCount int) error {
	if u.Status == UserStatusActive && activeAdminCount <= 1 {
		return NewForbidden("the last active administrator cannot be deprovisioned")
	}
	return nil
}

// TouchLogin records the last successful authentication time. It deliberately
// does not increment Version: last-login is not an optimistic-lock-guarded
// mutation, so bumping Version here would cause contention and spurious 409s
// on the hot authentication path. The use case persists it via a dedicated
// last_login_at update statement.
func (u *User) TouchLogin(t time.Time) {
	u.LastLoginAt = &t
}