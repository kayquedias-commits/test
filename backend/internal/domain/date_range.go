// Package domain contains the pure domain model for the financial management
// system. It has zero external dependencies; its value objects and entities
// encode business invariants without reference to frameworks, ORMs, or HTTP.
package domain

import (
	"errors"
	"fmt"
	"time"
)

// periodType enumerates the supported decomposition granularities offered by
// DateRange.Periods. It is deliberately a typed value rather than a bare string
// so call sites cannot pass arbitrary values.
type periodType string

const (
	monthly    periodType = "monthly"
	quarterly  periodType = "quarterly"
	annual     periodType = "annual"
)

// maxPeriods caps the number of periods DateRange.Periods may materialize.
// Without it, a caller-supplied range spanning centuries would allocate an
// unbounded number of slices.
const maxPeriods = 5000

// ErrInvalidDateRange is returned whenever a DateRange fails to satisfy its
// invariant (missing bounds, reversed bounds, or an over-long decomposition).
// Adapters map it to a VALIDATION_FAILED response without leaking internals.
var ErrInvalidDateRange = errors.New("domain: invalid date range")

// DateRange is an immutable value object describing a window from Start to End,
// inclusive of both endpoints. Instances are constructed with NewDateRange,
// which normalizes both bounds to UTC (stripping any monotonic clock reading)
// and enforces Start <= End. The Start and End fields are read-only by
// convention; do not mutate them.
type DateRange struct {
	Start time.Time
	End   time.Time
}

// NewDateRange constructs a DateRange, rejecting missing bounds and reversed
// bounds, and normalizing both bounds to UTC before storage. Equal bounds are
// allowed: a single-instant window is a legitimate budget/report period.
func NewDateRange(start, end time.Time) (DateRange, error) {
	if start.IsZero() || end.IsZero() {
		return DateRange{}, ErrInvalidDateRange
	}
	if end.Before(start) {
		return DateRange{}, ErrInvalidDateRange
	}
	return DateRange{Start: start.UTC(), End: end.UTC()}, nil
}

// Valid reports whether the range satisfies the invariant. It mirrors
// NewDateRange exactly, so an unpopulated zero-value DateRange (both bounds
// zero) is reported invalid and downstream consumers fail safely.
func (r DateRange) Valid() bool {
	return !r.Start.IsZero() && !r.End.IsZero() && !r.End.Before(r.Start)
}

// Contains reports whether t falls inside the range, inclusive of both
// endpoints. A transaction posted exactly on the period-end date belongs to the
// window, preserving budget/report reconciliation.
func (r DateRange) Contains(t time.Time) bool {
	return !t.Before(r.Start) && !t.After(r.End)
}

// ContainsRange reports whether o is fully contained within r (strict
// superset). Disjoint or partially overlapping ranges return false.
func (r DateRange) ContainsRange(o DateRange) bool {
	return !o.Start.Before(r.Start) && !o.End.After(r.End)
}

// Equal reports whether o covers the same instants as r. It compares
// normalized UTC instants via time.Time.Equal rather than the struct ==
// operator, which yields false negatives across differing locations or
// monotonic readings.
func (r DateRange) Equal(o DateRange) bool {
	return r.Start.Equal(o.Start) && r.End.Equal(o.End)
}

// String renders the range as two ISO 8601 (RFC3339) instants, e.g.
// "2024-01-01T00:00:00Z". It never uses time.Time.String, whose non-standard
// format and monotonic suffix would produce inconsistent serializations.
func (r DateRange) String() string {
	return r.Start.Format(time.RFC3339) + " → " + r.End.Format(time.RFC3339)
}

// StartOfMonth returns the first instant of the calendar month containing t,
// aligned to UTC.
func StartOfMonth(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// EndOfMonth returns the last instant of the calendar month containing t,
// aligned to UTC.
func EndOfMonth(t time.Time) time.Time {
	return StartOfMonth(t).AddDate(0, 1, 0).Add(-time.Nanosecond)
}

// StartOfQuarter returns the first instant of the calendar quarter
// (Jan/Apr/Jul/Oct) containing t, aligned to UTC.
func StartOfQuarter(t time.Time) time.Time {
	quarterMonth := time.Month(((int(t.Month())-1)/3)*3 + 1)
	return time.Date(t.Year(), quarterMonth, 1, 0, 0, 0, 0, time.UTC)
}

// EndOfQuarter returns the last instant of the calendar quarter containing t,
// aligned to UTC.
func EndOfQuarter(t time.Time) time.Time {
	return StartOfQuarter(t).AddDate(0, 3, 0).Add(-time.Nanosecond)
}

// Periods decomposes the range into contiguous calendar periods of the given
// granularity. Sub-periods exactly re-cover the original range with no gaps,
// no overlap, and no out-of-range dates: the first and last periods are
// truncated to the range bounds, and interior periods are full calendar
// periods. A zero-length range (Start == End) yields a single period. Any
// decomposition exceeding maxPeriods periods returns ErrInvalidDateRange
// rather than allocating.
func (r DateRange) Periods(pt periodType) ([]DateRange, error) {
	if !r.Valid() {
		return nil, ErrInvalidDateRange
	}

	var start, end func(time.Time) time.Time
	var stepMonths int
	switch pt {
	case monthly:
		start, end = StartOfMonth, EndOfMonth
		stepMonths = 1
	case quarterly:
		start, end = StartOfQuarter, EndOfQuarter
		stepMonths = 3
	case annual:
		start, end = func(t time.Time) time.Time {
			return time.Date(t.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
		}, func(t time.Time) time.Time {
			return time.Date(t.Year()+1, 1, 1, 0, 0, 0, 0, time.UTC).Add(-time.Nanosecond)
		}
		stepMonths = 12
	default:
		return nil, fmt.Errorf("%w: unknown period type %q", ErrInvalidDateRange, pt)
	}

	periods := make([]DateRange, 0, 8)
	cursor := start(r.Start)
	for !cursor.After(r.End) {
		if len(periods) >= maxPeriods {
			return nil, fmt.Errorf("%w: decomposition exceeds %d periods", ErrInvalidDateRange, maxPeriods)
		}
		pStart, pEnd := cursor, end(cursor)
		if pStart.Before(r.Start) {
			pStart = r.Start
		}
		if pEnd.After(r.End) {
			pEnd = r.End
		}
		periods = append(periods, DateRange{Start: pStart, End: pEnd})
		cursor = cursor.AddDate(0, stepMonths, 0)
	}
	return periods, nil
}