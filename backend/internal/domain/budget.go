// Package domain holds the financial-invariants building blocks of the core
// ledger. The package has zero external dependencies: only the standard library
// and github.com/google/uuid are permitted here so the domain layer stays
// framework- and ORM-agnostic.
package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Typed sentinel errors for Budget invariants. They are wrap-agnostic
// identities so use cases can map each deterministically onto the API error
// contract (all budget-creation rejections surface as VALIDATION_FAILED).
var (
	// ErrInvalidBudgetAllocation is returned when an allocation is zero,
	// negative, missing a currency, or when identity fields are nil.
	ErrInvalidBudgetAllocation = errors.New("domain: invalid budget allocation")
	// ErrPastPeriodNotAllowed is returned when creating a fully-elapsed budget
	// period while backdated budget creation is disabled.
	ErrPastPeriodNotAllowed = errors.New("domain: budget period is in the past")
	// ErrInvalidThresholdConfig is returned when threshold percentages violate
	// the allowed ordering 0 <= approachPct < exceedPct <= 100.
	ErrInvalidThresholdConfig = errors.New("domain: invalid threshold configuration")
	// ErrBudgetActualsCurrencyMismatch is returned when recomputed actuals
	// carry a different currency than the budget allocation.
	ErrBudgetActualsCurrencyMismatch = errors.New("domain: budget actuals currency mismatch")
)

// BudgetThresholdState classifies a budget's usage relative to its allocation.
// The classifier returns a typed state (never a raw string) so the use case can
// map it unambiguously onto EventBudgetThresholdApproached or
// EventBudgetThresholdExceeded.
type BudgetThresholdState string

const (
	// ThresholdStateNormal means usage is below the approaching threshold.
	ThresholdStateNormal BudgetThresholdState = "normal"
	// ThresholdStateApproaching means usage reached approachPct but not exceedPct.
	ThresholdStateApproaching BudgetThresholdState = "approaching"
	// ThresholdStateExceeded means usage reached or passed exceedPct.
	ThresholdStateExceeded BudgetThresholdState = "exceeded"
)

// String renders the classification as its canonical wire value.
func (s BudgetThresholdState) String() string {
	return string(s)
}

// maxDisplayedPercentage caps the percentage reported by PercentageUsed. Real
// overruns can exceed 100% by an arbitrary factor; without a clamp a malicious
// or pathological allocation/actual ratio could overflow the scaled integer
// math and wrap to a negative (misread as under-budget). The cap keeps the
// display safe while preserved as "exceeded" via the exceeded boolean.
const maxDisplayedPercentage = 100000

// Budget is the module-D aggregate root: a per-category allocation over a
// period (monthly/quarterly/annual) with materialized spend actuals. It is an
// entity owning identity (ID), tenancy (TenantID), and an optimistic-lock
// version. Invariants enforced here: positive allocation, valid period type,
// valid date range, and no backdated creation unless explicitly allowed.
//
// ActualsSpent is a materialized read-model projection, not a user-writable
// field. It is derived exclusively from the transaction read model and may be
// set only through RecomputeActuals (invoked by the use case when a
// TransactionPosted/Edited/Deleted event fires); no API/PATCH path may set it.
type Budget struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	CategoryID  uuid.UUID
	PeriodType  BudgetPeriodType
	Range       DateRange
	Allocation  Money
	Version     int64
	ActualsSpent Money
}

// NewBudget constructs a validated Budget aggregate. The clock is injected as
// now rather than read internally, keeping the domain pure and the past-period
// predicate testable; the use case passes the authoritative application time
// resolved at its boundary, never a client-supplied timestamp. backdatedAllowed
// is a server-side authorization/config decision, never a client field.
//
// The past-period guard compares the period end against today: a budget is
// rejected only when the whole window has already elapsed. Gating on end (the
// inclusive period boundary) rather than start permits creating a budget for
// a period that is currently in flight (e.g. a March budget created mid-March),
// while blocking retroactive budgets that could conceal overspend.
func NewBudget(
	now time.Time,
	id, tenantID, categoryID uuid.UUID,
	periodType BudgetPeriodType,
	rng DateRange,
	allocation Money,
	backdatedAllowed bool,
) (*Budget, error) {
	if id == uuid.Nil || tenantID == uuid.Nil || categoryID == uuid.Nil {
		return nil, ErrInvalidBudgetAllocation
	}
	if !periodType.Valid() {
		return nil, ErrInvalidBudgetPeriod
	}
	if !rng.Valid() {
		return nil, ErrInvalidDateRange
	}
	if allocation.IsZero() || allocation.IsNegative() || allocation.Currency() == "" {
		return nil, ErrInvalidBudgetAllocation
	}
	if !backdatedAllowed && rng.End.Before(businessDayStart(now)) {
		return nil, ErrPastPeriodNotAllowed
	}
	return &Budget{
		ID:          id,
		TenantID:    tenantID,
		CategoryID:  categoryID,
		PeriodType:  periodType,
		Range:       rng,
		Allocation:  allocation,
		Version:     1,
		ActualsSpent: Money{},
	}, nil
}

// businessDayStart truncates t to the start (UTC midnight) of its calendar day.
// The business-date "today" boundary follows UTC per the single-system-time
// assumption; locale presentation is resolved at the presentation layer.
func businessDayStart(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// PercentageUsed reports the floor percentage of the allocation consumed by
// ActualsSpent and whether the allocation has been exceeded. Percentages above
// 100 are preserved (overrun) and clamped at maxDisplayedPercentage so scaled
// integer math cannot overflow; the exceeded boolean is authoritative for
// overruns. A cross-currency or otherwise unresolvable comparison degrades
// fail-safe to (0, false) rather than panicking.
func (b *Budget) PercentageUsed() (percentage int, exceeded bool) {
	if b == nil || b.Allocation.IsZero() || b.Allocation.Currency() == "" || b.ActualsSpent.Currency() == "" {
		return 0, false
	}
	cmp, err := b.ActualsSpent.Cmp(b.Allocation)
	if err != nil {
		return 0, false
	}
	return percentageUsed(b.ActualsSpent.Major(), b.Allocation.Major()), cmp == 1
}

// percentageUsed computes floor(actuals/allocation*100) without overflow and
// clamped at maxDisplayedPercentage. The scaled product actuals*100 is never
// materialized: the whole part scales first and the remainder scales second,
// so no intermediate product exceeds int64 for valid Money magnitudes. A
// non-positive allocation degrades to 0 rather than dividing by zero.
func percentageUsed(actuals, allocation int64) int {
	if allocation <= 0 {
		return 0
	}
	whole := actuals / allocation
	rem := actuals % allocation
	// whole*100 would exceed the clamp once whole >= maxDisplayedPercentage/100.
	if whole > maxDisplayedPercentage/100 {
		return maxDisplayedPercentage
	}
	wholePct := whole * 100 // whole <= 1000, so wholePct <= 100000, within int64.
	remPct, ok := mulSafe(rem, 100)
	if !ok {
		return maxDisplayedPercentage
	}
	remPct /= allocation
	pct := wholePct + remPct
	if pct > maxDisplayedPercentage {
		return maxDisplayedPercentage
	}
	return int(pct)
}

// mulSafe returns a*b and whether the product fits in int64, guarding the
// percentage scale-up against overflow (CWE-190) for pathological inputs.
func mulSafe(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a > (1<<63-1)/b || a < (-1<<63)/b {
		return 0, false
	}
	return a * b, true
}

// ThresholdState classifies usage relative to the given thresholds and returns
// the typed state, whether it should publish a threshold event, and an error.
// Classification is mutually exclusive and deterministic: usage >= exceedPct is
// EXCEEDED, usage >= approachPct is APPROACHING, otherwise NORMAL. Only a
// non-NORMAL state signals an event to publish.
//
// The thresholds are configurable policy passed in from the use case (never
// client input) and are validated: 0 <= approachPct < exceedPct <= 100. An
// invalid configuration returns ErrInvalidThresholdConfig instead of silently
// normalizing, which would suppress or duplicate notifications.
func (b *Budget) ThresholdState(approachPct, exceedPct int) (BudgetThresholdState, bool, error) {
	if approachPct < 0 || exceedPct <= approachPct || exceedPct > 100 {
		return "", false, ErrInvalidThresholdConfig
	}
	pct, _ := b.PercentageUsed()
	switch {
	case pct >= exceedPct:
		return ThresholdStateExceeded, true, nil
	case pct >= approachPct:
		return ThresholdStateApproaching, true, nil
	default:
		return ThresholdStateNormal, false, nil
	}
}

// RecomputeActuals is the single controlled write path for the ActualsSpent
// projection. It is invoked by the use case after the read model recomputes
// spend for the category+period (typically on a TransactionPosted/Edited/Deleted
// event). The actuals must share the allocation's currency; a mismatch is
// rejected because comparing across currencies would corrupt the percentage.
func (b *Budget) RecomputeActuals(actuals Money) error {
	if b == nil || b.Allocation.Currency() == "" {
		return ErrInvalidBudgetAllocation
	}
	if b.Allocation.Currency() != actuals.Currency() {
		return ErrBudgetActualsCurrencyMismatch
	}
	b.ActualsSpent = actuals
	return nil
}