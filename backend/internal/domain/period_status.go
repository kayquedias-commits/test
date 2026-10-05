// Package domain contains the FMS domain layer. It holds zero framework, HTTP,
// or persistence dependencies; all invariants are expressed as pure Go.
package domain

import "errors"

// ErrInvalidPeriodStatus is returned by ParsePeriodStatus when the input is not
// one of the three canonical period status strings.
var ErrInvalidPeriodStatus = errors.New("domain: invalid period status")

// PeriodStatus is an immutable value object representing the lifecycle state of
// a period-end close in the Core Accounting (Ledger) bounded context. It is
// string-backed so its serialized form ("open"/"closing"/"closed") equals the
// value stored in the period_close table.
type PeriodStatus string

const (
	// PeriodOpen accepts transaction writes and is eligible for close.
	PeriodOpen PeriodStatus = "open"
	// PeriodClosing marks a provisional lock while batch close work is in flight.
	PeriodClosing PeriodStatus = "closing"
	// PeriodClosed is terminal: writes are permanently locked and a reporting
	// snapshot becomes immutable.
	PeriodClosed PeriodStatus = "closed"
)

// periodStatusValid reports whether s is one of the canonical statuses. It is
// the single source of truth so that ParsePeriodStatus and Valid never disagree.
// The zero value (""), case variants, and unknown values are all rejected.
func periodStatusValid(s PeriodStatus) bool {
	switch s {
	case PeriodOpen, PeriodClosing, PeriodClosed:
		return true
	default:
		return false
	}
}

// Valid reports whether s is a canonical period status.
func (s PeriodStatus) Valid() bool {
	return periodStatusValid(s)
}

// ParsePeriodStatus parses a status string against a strict, case-sensitive
// allowlist. Empty input, mixed-case, padded, or unknown values fail closed by
// returning ErrInvalidPeriodStatus; it never defaults to a permissive status.
func ParsePeriodStatus(s string) (PeriodStatus, error) {
	status := PeriodStatus(s)
	if !periodStatusValid(status) {
		return "", ErrInvalidPeriodStatus
	}
	return status, nil
}

// CanTransitionTo reports whether s may legally advance to next. Only the
// open -> closing -> closed chain is allowed: open may go to closing, closing
// to closed, and closed is terminal with no outgoing edges. It also guards
// against an invalid (unparsed) receiver.
//
// This is a client-side predicate only. Authoritative double-close prevention
// (atomic conditional update with an optimistic-lock version guard, plus the
// no-post-close-write trigger) belongs to the persistence layer via the
// ClosePeriod use case.
func (s PeriodStatus) CanTransitionTo(next PeriodStatus) bool {
	if !s.Valid() {
		return false
	}
	switch s {
	case PeriodOpen:
		return next == PeriodClosing
	case PeriodClosing:
		return next == PeriodClosed
	case PeriodClosed:
		return false
	default:
		return false
	}
}