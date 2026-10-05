// FinancePeriod encodes the period-end close state machine (open -> closing ->
// closed) for the Core Accounting (Ledger) bounded context. It is the aggregate
// root for a fiscal-calendar period: its transition methods are the only lawful
// way to advance a period through close, and Locked/AcceptsWrites are the
// guards the ClosePeriod use case and the ledger write path consult to enforce
// the no-post-close-mutation invariant ($7.5).
package domain

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// labelMaxRunes caps the length of FinancePeriod.Label, aligned with the
// transaction-description rule (<= 255 characters). Labels are user-supplied
// text that later flows into reporting snapshots, so the bound is enforced at
// construction rather than left to the persistence layer.
const labelMaxRunes = 255

// FinancePeriod is a fiscal calendar window owned by a tenant. It tracks the
// lifecycle of the period-end close and, once Closed, becomes an immutable
// reporting snapshot: writes to the period are forbidden and ClosedBy/ClosedAt
// record the closing actor for audit/compliance traceability.
//
// Integrity rules: Status is the sole source of truth for the lock state —
// Locked and AcceptsWrites derive from it alone, never from ClosedBy/ClosedAt.
// The Closed state is terminal; Version is the optimistic-lock counter used by
// the persistence layer to make each transition atomic under concurrency.
type FinancePeriod struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	Label    string
	Range    DateRange
	Status   PeriodStatus
	ClosedBy *uuid.UUID
	ClosedAt *time.Time
	Version  int64
}

// validLabel reports whether s is non-empty (after trimming), fits within
// labelMaxRunes, and contains no control characters. It fails closed on the
// zero value, so a zero-value Label is rejected at construction.
func validLabel(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return len([]rune(s)) <= labelMaxRunes
}

// NewFinancePeriod builds an Open FinancePeriod bound to the given tenant and
// fiscal window. It rejects a missing id/tenant, a non-empty-invalid label, and
// any range that does not satisfy the DateRange invariant (missing or reversed
// bounds), mirroring the CHECK constraints enforced at the persistence layer.
//
// On success Status is PeriodOpen and Version is seeded at 1 so optimistic-lock
// comparisons have a non-zero baseline.
func NewFinancePeriod(id, tenantID uuid.UUID, label string, rng DateRange) (FinancePeriod, error) {
	if id == uuid.Nil {
		return FinancePeriod{}, NewValidationError(FieldError{Field: "id", Message: "period id must be provided"})
	}
	if tenantID == uuid.Nil {
		return FinancePeriod{}, NewValidationError(FieldError{Field: "tenant_id", Message: "tenant must be provided"})
	}
	label = strings.TrimSpace(label)
	if !validLabel(label) {
		return FinancePeriod{}, NewValidationError(FieldError{Field: "label", Message: "label must be non-empty and at most 255 characters, without control characters"})
	}
	if !rng.Valid() {
		return FinancePeriod{}, NewValidationError(FieldError{Field: "range", Message: "period range must have a start on or before its end"})
	}
	return FinancePeriod{
		ID:       id,
		TenantID: tenantID,
		Label:    label,
		Range:    rng,
		Status:   PeriodOpen,
		Version:  1,
	}, nil
}

// BeginClose advances the period from open to closing, provisional lock that
// halts further writes while batch close work is in flight. It returns a
// CONFLICT_STATE error for any source state other than open, which rejects
// calling it twice and calling it on an already-closed period.
func (p *FinancePeriod) BeginClose() error {
	if p.Status != PeriodOpen {
		return NewConflict("period is not open")
	}
	p.Status = PeriodClosing
	p.Version++
	return nil
}

// CompleteClose finalizes the close: it sets the closing state to closed and
// records who closed the period and when, producing the immutable reporting
// snapshot. It rejects a zero-value actor (so the close is always attributable)
// and any source state other than closing; the latter makes a second close call
// on an already-closed period return CONFLICT_STATE. Once set, ClosedBy and
// ClosedAt are terminal and never cleared.
func (p *FinancePeriod) CompleteClose(actorID uuid.UUID) error {
	if actorID == uuid.Nil {
		return NewValidationError(FieldError{Field: "closed_by", Message: "closing actor must be provided"})
	}
	if p.Status != PeriodClosing {
		return NewConflict("period is not closing")
	}
	now := time.Now().UTC()
	p.Status = PeriodClosed
	p.ClosedBy = &actorID
	p.ClosedAt = &now
	p.Version++
	return nil
}

// Locked reports whether the period is terminal and immutable. It derives
// exclusively from Status, so a provisional closing period is not yet locked
// even though it refuses writes via AcceptsWrites.
func (p *FinancePeriod) Locked() bool {
	return p.Status == PeriodClosed
}

// AcceptsWrites reports whether the ledger may post transactions to this
// period. Only an open period admits writes; entering closing immediately
// freezes the period even before the close completes, preserving the
// no-post-close-mutation invariant.
func (p *FinancePeriod) AcceptsWrites() bool {
	return p.Status == PeriodOpen
}