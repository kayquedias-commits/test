// Package domain holds the financial-management core with zero framework,
// HTTP, or persistence dependencies (Clean Architecture invariant).
package domain

import "errors"

// AuditAction is an immutable value object enumerating the operations an
// append-only audit entry may record, in the Audit & Compliance bounded context.
type AuditAction string

const (
	// ActionCreate marks an audit entry for the creation of an entity.
	ActionCreate AuditAction = "create"

	// ActionEdit marks an audit entry for the modification of an entity.
	ActionEdit AuditAction = "edit"

	// ActionDelete marks an audit entry for the deletion of an entity.
	ActionDelete AuditAction = "delete"

	// ActionRead marks an audit entry for a read operation. It is reserved and
	// dormant: emitted only when audit-read logging is enabled via the
	// AuditReadLoggingEnabled feature flag at the middleware boundary, and never
	// part of the always-on mutation audit set. Gating emission bounds audit
	// storage volume (§9.4 tradeoff).
	ActionRead AuditAction = "read"
)

// ErrInvalidAuditAction is returned when a value does not match a known action.
var ErrInvalidAuditAction = errors.New("domain: invalid audit action")

// auditActionValid reports whether the action is one of the canonical values.
func auditActionValid(a AuditAction) bool {
	switch a {
	case ActionCreate, ActionEdit, ActionDelete, ActionRead:
		return true
	default:
		return false
	}
}

// ParseAuditAction returns the action matching s, rejecting any string that is
// not exactly one of the canonical lowercase constants. The comparison is
// strict and case-sensitive; mismatched values yield ErrInvalidAuditAction.
func ParseAuditAction(s string) (AuditAction, error) {
	a := AuditAction(s)
	if !auditActionValid(a) {
		return "", ErrInvalidAuditAction
	}
	return a, nil
}

// Valid reports whether the action is one of the canonical values.
func (a AuditAction) Valid() bool {
	return auditActionValid(a)
}

// IsMutation reports whether the action mutates an entity. It is the single
// policy predicate consumers use to always persist create/edit/delete entries
// (with before/after state) and to treat read as non-mutating — read entries are
// emitted only conditionally and must not publish domain events.
func (a AuditAction) IsMutation() bool {
	switch a {
	case ActionCreate, ActionEdit, ActionDelete:
		return true
	default:
		return false
	}
}