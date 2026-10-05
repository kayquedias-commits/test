package domain

import (
	"errors"
	"time"
)

// ErrInvalidBudgetPeriod is returned when a string cannot be parsed as a
// supported budget period type or when window math is requested for an
// invalid type.
var ErrInvalidBudgetPeriod = errors.New("domain: invalid budget period type")

// BudgetPeriodType is the budget cycle a budget is measured over. It is a
// value object with no identity and is immutable by construction; the
// canonical lowercase strings are both the wire and persistence vocabulary.
type BudgetPeriodType string

const (
	// Monthly budgets cover a single calendar month.
	Monthly BudgetPeriodType = "monthly"
	// Quarterly budgets cover a single calendar quarter.
	Quarterly BudgetPeriodType = "quarterly"
	// Annual budgets cover a single calendar year.
	Annual BudgetPeriodType = "annual"
)

// budgetPeriodTypeValid reports whether t is one of the three supported
// period types. It is the single source of truth for allowlist membership so
// that ParseBudgetPeriodType and Valid never disagree.
func budgetPeriodTypeValid(t BudgetPeriodType) bool {
	switch t {
	case Monthly, Quarterly, Annual:
		return true
	default:
		// The zero value (empty string), case variants, and unknown values
		// are all rejected so an unrecognized input never silently maps to a
		// valid period.
		return false
	}
}

// ParseBudgetPeriodType parses an external string into a BudgetPeriodType.
// It is a strict allowlist: only the exact canonical lowercase values
// "monthly", "quarterly", and "annual" are accepted. Empty strings,
// whitespace-padded values, case variants, and unknown values all return
// ErrInvalidBudgetPeriod — there is no case folding, trimming, or prefix
// matching, because lenient parsing would let invalid period types reach
// budget creation and corrupt period aggregation.
func ParseBudgetPeriodType(s string) (BudgetPeriodType, error) {
	t := BudgetPeriodType(s)
	if !budgetPeriodTypeValid(t) {
		return "", ErrInvalidBudgetPeriod
	}
	return t, nil
}

// Valid reports whether t is a supported budget period type. Callers that
// receive a BudgetPeriodType from sources other than ParseBudgetPeriodType
// (for example direct construction or deserialization) must invoke this
// before using the value.
func (t BudgetPeriodType) Valid() bool {
	return budgetPeriodTypeValid(t)
}

// WindowBounds returns the half-open UTC interval [start, nextStart) that a
// transaction date falls within given a reference instant in. The reference
// location is discarded — the value is normalized to UTC so calendar math is
// locale-agnostic. The end is the first instant of the following period and
// is exclusive: a transaction dated exactly at the returned end belongs to
// the next window, so aggregation predicates must use
// date >= start AND date < end to avoid double-counting at boundaries.
//
// An invalid or zero-valued type returns ErrInvalidBudgetPeriod instead of a
// zero time pair.
func (t BudgetPeriodType) WindowBounds(in time.Time) (time.Time, time.Time, error) {
	if !t.Valid() {
		return time.Time{}, time.Time{}, ErrInvalidBudgetPeriod
	}

	ref := in.UTC()
	y, m := ref.Year(), ref.Month()

	var start, end time.Time
	switch t {
	case Monthly:
		start = time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
		// Month overflow past December normalizes into the next year.
		end = time.Date(y, m+1, 1, 0, 0, 0, 0, time.UTC)
	case Quarterly:
		// Quarter start months are Jan, Apr, Jul, Oct.
		startMonth := time.Month(((int(m)-1)/3)*3 + 1)
		start = time.Date(y, startMonth, 1, 0, 0, 0, 0, time.UTC)
		// Adding 3 to Oct normalizes Q4's end into January of next year.
		end = time.Date(y, startMonth+3, 1, 0, 0, 0, 0, time.UTC)
	case Annual:
		start = time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
		end = time.Date(y+1, 1, 1, 0, 0, 0, 0, time.UTC)
	}

	return start, end, nil
}