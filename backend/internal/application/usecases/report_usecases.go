// Package usecases implements the application-layer orchestration of the
// Financial Management System, sitting between the interface adapters and the
// domain model. This file declares the Reporting bounded-context facade (Module
// E): ReportService orchestrates profit/loss, expense breakdown, income trend,
// and cash-flow reports exclusively from the read model (ReportReadModelRepository),
// enforcing range validation and shaping response DTOs. It performs no writes,
// applies no currency formatting (rendering is presentation-layer), and returns
// typed empty summaries (never errors) for empty datasets.
package usecases

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/domain"
)

// maxReportSpan bounds the largest report date range a single request may
// cover. It prevents a pathological range from triggering an unbounded
// aggregation scan over the read model (CWE-770 resource consumption); spans
// beyond this require pagination or an asynchronous export route.
const maxReportSpan = 366 * 24 * time.Hour

// ErrReportRangeTooLarge is returned when a validated range exceeds
// maxReportSpan. It is distinct from domain.ErrInvalidDateRange so an adapter
// can map an over-wide but well-formed range to its own contract without being
// conflated with an inverted range.
var ErrReportRangeTooLarge = errors.New("usecases: report range exceeds maximum span")

// ProfitLossRow is an immutable row of the profit/loss read-model projection.
// Monetary fields are integer minor units (cents), never floats; the repository
// supplies them ordered by period.
type ProfitLossRow struct {
	Period       time.Time
	IncomeTotal  int64
	ExpenseTotal int64
}

// ExpenseBreakdownRow is an immutable row of the expense-breakdown read-model
// projection, supplied ordered by amount descending.
type ExpenseBreakdownRow struct {
	CategoryID   uuid.UUID
	CategoryName string
	Amount       int64
}

// IncomeTrendRow is an immutable row of the income-trend read-model projection.
type IncomeTrendRow struct {
	Period time.Time
	Amount int64
}

// CashFlowRow is an immutable row of the cash-flow read-model projection.
// NetMovement may be negative (expenses exceeding income in a period).
type CashFlowRow struct {
	Period      time.Time
	Inflow      int64
	Outflow     int64
	NetMovement int64
}

// ReportReadModelRepository is the sole data source for reports (Module E). It
// is the Reporting bounded-context output port: it exposes query methods only,
// so the use case structurally cannot write. Every method embeds a tenancy
// predicate derived from the server-side tenant context; all aggregate SQL lives
// in the Infrastructure adapter (single statement per report). Methods return
// empty slices (never an error) on empty datasets, reserving error for storage
// or connectivity failures.
type ReportReadModelRepository interface {
	ProfitLoss(ctx context.Context, rng domain.DateRange, tenant uuid.UUID) ([]ProfitLossRow, error)
	ExpenseBreakdown(ctx context.Context, rng domain.DateRange, tenant uuid.UUID) ([]ExpenseBreakdownRow, error)
	IncomeTrend(ctx context.Context, rng domain.DateRange, tenant uuid.UUID) ([]IncomeTrendRow, error)
	CashFlow(ctx context.Context, rng domain.DateRange, tenant uuid.UUID) ([]CashFlowRow, error)
}

// ProfitLossReport is the profit/loss DTO. Monetary fields are domain.Money
// minor-unit values in the base currency; Net is income minus expense and may
// be negative for a loss.
type ProfitLossReport struct {
	Start    time.Time
	End      time.Time
	Currency string
	Income   domain.Money
	Expense  domain.Money
	Net      domain.Money
}

// ExpenseBreakdownLine is a single category-amount row of an expense
// breakdown. Amount is non-negative in the base currency.
type ExpenseBreakdownLine struct {
	CategoryID uuid.UUID
	Category   string
	Amount     domain.Money
}

// ExpenseBreakdownReport is the expense-by-category DTO. Rows are ordered by
// amount descending; Total is the sum of all rows.
type ExpenseBreakdownReport struct {
	Start    time.Time
	End      time.Time
	Currency string
	Rows     []ExpenseBreakdownLine
	Total    domain.Money
}

// IncomeTrendPoint is a single period-amount point of an income trend.
type IncomeTrendPoint struct {
	Period time.Time
	Amount domain.Money
}

// IncomeTrendReport is the income-trend DTO: a monthly series over the range.
type IncomeTrendReport struct {
	Start    time.Time
	End      time.Time
	Currency string
	Series   []IncomeTrendPoint
}

// CashFlowPoint is a single period's inflow/outflow/net-movement point. Net
// Movement may be negative in a period where expenses exceed income.
type CashFlowPoint struct {
	Period      time.Time
	Inflow      domain.Money
	Outflow     domain.Money
	NetMovement domain.Money
}

// CashFlowReport is the cash-flow DTO: inflow and outflow per period with the
// resulting net movement.
type CashFlowReport struct {
	Start    time.Time
	End      time.Time
	Currency string
	Series   []CashFlowPoint
}

// ReportService is the orchestration use case for Module E reports. It depends
// only on the injected read-model port and clock port, and performs no writes
// to any transactional store.
type ReportService struct {
	read  ReportReadModelRepository
	clock ports.Clock
}

// NewReportService builds a ReportService from its output ports. read must be
// non-nil; now is the injected clock (nil falls back to the production
// SystemClock). The service is read-only by construction: no write- or
// transaction-capable repository is injected.
func NewReportService(read ReportReadModelRepository, now ports.Clock) (*ReportService, error) {
	if read == nil {
		return nil, domain.NewInternal(nil)
	}
	c := now
	if c == nil {
		c = ports.SystemClock{}
	}
	return &ReportService{read: read, clock: c}, nil
}

// ValidateRange enforces the report range invariant: start <= end and a
// bounded span. It returns domain.ErrInvalidDateRange for an inverted or
// missing-bounds range and ErrReportRangeTooLarge for an over-wide span. It
// returns no HTTP vocabulary; the interface adapter maps the errors to status
// codes.
func (s *ReportService) ValidateRange(rng domain.DateRange) error {
	if !rng.Valid() {
		return domain.ErrInvalidDateRange
	}
	if rng.End.Sub(rng.Start) > maxReportSpan {
		return ErrReportRangeTooLarge
	}
	return nil
}

// ProfitLoss aggregates income, expense, and net movement over the range from
// the read model. Empty datasets yield zero Money totals, never an error. The
// tenant is enforced: a nil tenant is rejected as FORBIDDEN before any query,
// so a cross-tenant read can never surface as a masked empty report.
func (s *ReportService) ProfitLoss(ctx context.Context, rng domain.DateRange, tenant uuid.UUID) (*ProfitLossReport, error) {
	if err := s.ValidateRange(rng); err != nil {
		return nil, err
	}
	if tenant == uuid.Nil {
		return nil, domain.NewForbidden("invalid tenant context")
	}

	rows, err := s.read.ProfitLoss(ctx, rng, tenant)
	if err != nil {
		return nil, domain.NewDependencyUnavailable("read model unavailable")
	}

	var incomeTotal, expenseTotal int64
	for _, r := range rows {
		incomeTotal += r.IncomeTotal
		expenseTotal += r.ExpenseTotal
	}
	income, err := domain.NewFromMajor(incomeTotal, baseCurrency)
	if err != nil {
		return nil, domain.NewInternal(err)
	}
	expense, err := domain.NewFromMajor(expenseTotal, baseCurrency)
	if err != nil {
		return nil, domain.NewInternal(err)
	}
	net, err := netMoney(income, expense)
	if err != nil {
		return nil, err
	}
	return &ProfitLossReport{
		Start:    rng.Start,
		End:      rng.End,
		Currency: baseCurrency,
		Income:   income,
		Expense:  expense,
		Net:      net,
	}, nil
}

// ExpenseBreakdown reports per-category expense amounts ordered by amount
// descending, with a running total. Empty datasets yield an empty non-nil Rows
// slice and a zero Total, never an error.
func (s *ReportService) ExpenseBreakdown(ctx context.Context, rng domain.DateRange, tenant uuid.UUID) (*ExpenseBreakdownReport, error) {
	if err := s.ValidateRange(rng); err != nil {
		return nil, err
	}
	if tenant == uuid.Nil {
		return nil, domain.NewForbidden("invalid tenant context")
	}

	rows, err := s.read.ExpenseBreakdown(ctx, rng, tenant)
	if err != nil {
		return nil, domain.NewDependencyUnavailable("read model unavailable")
	}

	total, err := domain.NewFromMajor(0, baseCurrency)
	if err != nil {
		return nil, domain.NewInternal(err)
	}
	lines := make([]ExpenseBreakdownLine, 0, len(rows))
	for _, r := range rows {
		amt, err := domain.NewFromMajor(r.Amount, baseCurrency)
		if err != nil {
			return nil, domain.NewInternal(err)
		}
		total, err = total.Add(amt)
		if err != nil {
			return nil, domain.NewInternal(err)
		}
		lines = append(lines, ExpenseBreakdownLine{
			CategoryID: r.CategoryID,
			Category:   r.CategoryName,
			Amount:     amt,
		})
	}
	return &ExpenseBreakdownReport{
		Start:    rng.Start,
		End:      rng.End,
		Currency: baseCurrency,
		Rows:     lines,
		Total:    total,
	}, nil
}

// IncomeTrend reports income per month as a periodic series. Empty datasets
// yield an empty non-nil Series slice, never an error.
func (s *ReportService) IncomeTrend(ctx context.Context, rng domain.DateRange, tenant uuid.UUID) (*IncomeTrendReport, error) {
	if err := s.ValidateRange(rng); err != nil {
		return nil, err
	}
	if tenant == uuid.Nil {
		return nil, domain.NewForbidden("invalid tenant context")
	}

	rows, err := s.read.IncomeTrend(ctx, rng, tenant)
	if err != nil {
		return nil, domain.NewDependencyUnavailable("read model unavailable")
	}

	series := make([]IncomeTrendPoint, 0, len(rows))
	for _, r := range rows {
		amt, err := domain.NewFromMajor(r.Amount, baseCurrency)
		if err != nil {
			return nil, domain.NewInternal(err)
		}
		series = append(series, IncomeTrendPoint{Period: r.Period, Amount: amt})
	}
	return &IncomeTrendReport{
		Start:    rng.Start,
		End:      rng.End,
		Currency: baseCurrency,
		Series:   series,
	}, nil
}

// CashFlow reports inflow, outflow, and net movement per month. Empty datasets
// yield an empty non-nil Series slice, never an error.
func (s *ReportService) CashFlow(ctx context.Context, rng domain.DateRange, tenant uuid.UUID) (*CashFlowReport, error) {
	if err := s.ValidateRange(rng); err != nil {
		return nil, err
	}
	if tenant == uuid.Nil {
		return nil, domain.NewForbidden("invalid tenant context")
	}

	rows, err := s.read.CashFlow(ctx, rng, tenant)
	if err != nil {
		return nil, domain.NewDependencyUnavailable("read model unavailable")
	}

	series := make([]CashFlowPoint, 0, len(rows))
	for _, r := range rows {
		inflow, err := domain.NewFromMajor(r.Inflow, baseCurrency)
		if err != nil {
			return nil, domain.NewInternal(err)
		}
		outflow, err := domain.NewFromMajor(r.Outflow, baseCurrency)
		if err != nil {
			return nil, domain.NewInternal(err)
		}
		net, err := netMoney(inflow, outflow)
		if err != nil {
			return nil, err
		}
		series = append(series, CashFlowPoint{
			Period:      r.Period,
			Inflow:      inflow,
			Outflow:     outflow,
			NetMovement: net,
		})
	}
	return &CashFlowReport{
		Start:    rng.Start,
		End:      rng.End,
		Currency: baseCurrency,
		Series:   series,
	}, nil
}

// netMoney returns a - b using domain.Money arithmetic. Because Money is
// non-negative by default, a difference that would underflow is computed in the
// reverse direction and negated, so a loss (negative net) is represented
// exactly without binary floating-point.
func netMoney(a, b domain.Money) (domain.Money, error) {
	cmp, err := a.Cmp(b)
	if err != nil {
		return domain.Money{}, domain.NewInternal(err)
	}
	if cmp >= 0 {
		return a.Sub(b)
	}
	diff, err := b.Sub(a)
	if err != nil {
		return domain.Money{}, domain.NewInternal(err)
	}
	return diff.Negate(), nil
}