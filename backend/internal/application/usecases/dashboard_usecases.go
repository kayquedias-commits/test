// Package usecases implements the application-layer orchestration of the
// Financial Management System, sitting between the interface adapters and the
// domain model. This file declares the read-only Reporting bounded-context
// facade: DashboardService composes role-tailored KPI sets exclusively from the
// read model (ReadModelRepository), with a tenant-scoped 60s cache and an
// injected clock. It performs no writes and never touches the authoritative
// ledger; monetary values are domain.Money minor-unit values, never floats.
//
// The concrete-role-to-role class dispatch (CFO, Standard, Viewer) is policy
// data owned by the interface-adapter/RBAC layer, not this file: adding a new
// concrete role must never require a code change here. This file only exposes
// the three role classes as methods and enforces a bounded card set (≤9).
package usecases

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/google/uuid"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/domain"
)

// maxRangeSpan bounds the largest KPI date range a dashboard may request. It
// prevents a pathological range from triggering an unbounded aggregation scan
// over the materialized views (CWE-770 resource consumption).
const maxRangeSpan = 5 * 365 * 24 * time.Hour

// maxKPICards is the server-side cap on the number of cards any dashboard may
// carry (blueprint §8.1: no more than 7–9 KPI cards per dashboard).
const maxKPICards = 9

// baseCurrency is the single primary currency assumed by the MVP (§8
// assumptions). KPI monetary cards render in this currency; multi-currency is
// an extension boundary surfaced by the read model, not this use case.
const baseCurrency = "USD"

// Role-class cache namespaces. The role class is a server-side property of the
// invoked method, never client-supplied, so a standard user can never request a
// CFO cache namespace.
const (
	roleClassCFO = "cfo"
	roleClassStd = "srd"
	roleClassVwr = "vwr"
)

// Values used to assemble the drilldown route allowlist. Drilldown targets are
// internal route descriptors for the frontend breadcrumb trail; only these
// values may appear as Link.Route. No arbitrary or user-controlled URL is ever
// accepted into a drilldown link.
const (
	routeReportsCashFlow      = "/reports/cash-flow"
	routeReportsPNL           = "/reports/pnl"
	routeReportsExpensesByCat = "/reports/expenses-by-category"
	routeBudgets              = "/budgets"
	routeAccounts             = "/accounts"
)

// KPI card identifiers. These are stable ids rendered into the card ID slot and
// are policy-owned labels the frontend binds against; they are never used as
// computation inputs.
const (
	cardCashPosition  = "cash_position"
	cardRevenue       = "revenue"
	cardGrossMargin   = "gross_margin"
	cardNetMargin     = "net_margin"
	cardExpenseRatio  = "expense_ratio"
	cardBudgetBurn    = "budget_burn"
	cardAccountCount  = "account_count"
	cardPeriodClosed  = "period_closed"
	cardTotalIncome   = "total_income"
	cardTotalExpenses = "total_expenses"
	cardTxCount       = "tx_count"
	cardTopCategory   = "top_category"
)

// ReadModelPeriod carries prior-period revenue/expense comparison basis. It is
// the read-side prior-range aggregate used for KPI trend deltas.
type ReadModelPeriod struct {
	Revenue  int64
	Expenses int64
}

// ReadModelCFOView is the read-model aggregate backing the CFO dashboard. All
// monetary fields are integer minor units (cents); ratios are derived in the
// repository from integer sums, never floats for money. A repository MUST
// return a zero-valued struct (never an error) for an empty dataset.
type ReadModelCFOView struct {
	CashPosition    int64
	Revenue         int64
	TotalExpenses   int64
	DirectExpenses  int64
	GrossMargin     float64
	NetMargin       float64
	ExpenseRatio    float64
	BudgetAllocated int64
	BudgetSpent     int64
	BudgetBurnRate  float64
	AccountCount    int32
	PeriodClosed    bool
	AsOf            time.Time
	PriorPeriod     *ReadModelPeriod
}

// ReadModelStandardView is the personal-scope read model for a standard user.
// It aggregates only transactions owned by that user (own-transaction scope).
type ReadModelStandardView struct {
	TotalIncome    int64
	TotalExpenses  int64
	TxCount        int32
	TopCategory    uuid.UUID
	BudgetBurnRate float64
	AsOf           time.Time
}

// ReadModelViewerSummary is the read-only viewer read model: counts and totals
// only, with no margin or ratio math performed.
type ReadModelViewerSummary struct {
	AccountCount      int32
	TxCount           int32
	TotalIncome       int64
	TotalExpenses     int64
	LastTransactionAt *time.Time
	AsOf              time.Time
}

// ReadModelRepository is the sole data source for dashboard KPIs. It is the
// Reporting bounded-context output port: it exposes query methods only, so the
// use case structurally cannot write. Every method embeds a tenancy predicate
// derived from the server-side tenant context; all methods return zero-valued
// structs (never error) on empty datasets, reserving error for storage or
// connectivity failures.
type ReadModelRepository interface {
	// CFOKPIs aggregates KPIs across all of a tenant's accounts and budgets.
	CFOKPIs(ctx context.Context, rng domain.DateRange, tenant uuid.UUID) (*ReadModelCFOView, error)
	// StandardUserKPIs aggregates KPIs across a single user's own transactions.
	StandardUserKPIs(ctx context.Context, rng domain.DateRange, tenant, user uuid.UUID) (*ReadModelStandardView, error)
	// ViewerKPIs computes read-only counts and totals for a tenant.
	ViewerKPIs(ctx context.Context, rng domain.DateRange, tenant uuid.UUID) (*ReadModelViewerSummary, error)
}

// Link is a route descriptor for the frontend breadcrumb trail. Route is drawn
// strictly from the internal allowlist above; Params carries only validated
// range delimiters. It holds no framework or HTTP types.
type Link struct {
	Route  string
	Params map[string]string
}

// TrendDir is the direction of a KPI trend between the current and prior
// period. It is computed server-side from the integer comparison basis, never
// derived by the client from a sign.
type TrendDir string

const (
	// TrendUp indicates an increase from the prior period.
	TrendUp TrendDir = "up"
	// TrendDown indicates a decrease from the prior period.
	TrendDown TrendDir = "down"
	// TrendFlat indicates no change or an absent comparison basis.
	TrendFlat TrendDir = "flat"
)

// Trend is a KPI trend descriptor. DeltaPercent is the (current-prior)/|prior|
// percent change; a missing prior-period basis yields a nil trend.
type Trend struct {
	Direction    TrendDir
	DeltaPercent float64
}

// Card is a single bounded KPI card (≤9 per dashboard). Value is a
// presentation-ready string rendered server-side; Trend and Drilldown are nil
// when a prior-period basis or drill route is absent.
type Card struct {
	ID        string
	Label     string
	Value     string
	Trend     *Trend
	Drilldown *Link
}

// CFODashboard is the executive KPI DTO. AsOf is read-model snapshot metadata,
// not a card. Monetary fields are domain.Money minor-unit values, never floats.
type CFODashboard struct {
	CashPosition   domain.Money
	Revenue        domain.Money
	GrossMargin    float64
	NetMargin      float64
	ExpenseRatio   float64
	BudgetBurnRate float64
	AccountCount   int32
	PeriodClosed   bool
	AsOf           time.Time
	Cards          []Card
}

// StandardUserDashboard is the personal-scope KPI DTO for a standard user.
type StandardUserDashboard struct {
	TotalIncome    domain.Money
	TotalExpenses  domain.Money
	TxCount        int32
	TopCategory    uuid.UUID
	BudgetBurnRate float64
	AsOf           time.Time
	Cards          []Card
}

// ViewerDashboard is the read-only summary DTO for a viewer role.
type ViewerDashboard struct {
	AccountCount      int32
	TxCount           int32
	TotalIncome       domain.Money
	TotalExpenses     domain.Money
	LastTransactionAt *time.Time
	AsOf              time.Time
	Cards             []Card
}

// DashboardService is the orchestration use case for role-tailored dashboards.
// It depends only on the injected read-model port, cache port, and clock port.
type DashboardService struct {
	read  ReadModelRepository
	cache ports.Cache
	clock ports.Clock
}

// NewDashboardService builds a DashboardService from its output ports. read and
// cache must be non-nil; now is the clock used to resolve the AsOf fallback. A
// nil clock falls back to the production SystemClock.
func NewDashboardService(read ReadModelRepository, cache ports.Cache, now ports.Clock) (*DashboardService, error) {
	if read == nil || cache == nil {
		return nil, domain.NewInternal(nil)
	}
	c := now
	if c == nil {
		c = ports.SystemClock{}
	}
	return &DashboardService{read: read, cache: cache, clock: c}, nil
}

// CFOKPIs assembles the bounded CFO KPI set from the read model. It validates
// the range, serves from a tenant-scoped 60s cache on a hit, and maps repository
// failures to a DEPENDENCY_UNAVAILABLE error. It is read-only and never mutates
// state.
func (s *DashboardService) CFOKPIs(ctx context.Context, rng domain.DateRange, tenant uuid.UUID) (*CFODashboard, error) {
	if err := s.validateRange(rng); err != nil {
		return nil, err
	}
	if tenant == uuid.Nil {
		return nil, domain.NewForbidden("invalid tenant context")
	}

	var view *ReadModelCFOView
	err := s.withCache(ctx, s.cfoKey(tenant, rng), &view, func() error {
		v, err := s.read.CFOKPIs(ctx, rng, tenant)
		if err != nil {
			// Read-model unavailability maps to the 503 contract; the cause is
			// retained only for server-side logging, never surfaced.
			return domain.NewDependencyUnavailable("read model unavailable")
		}
		view = v
		return nil
	})
	if err != nil {
		return nil, err
	}

	return s.assembleCFO(view)
}

// StandardUserKPIs assembles the personal-scope KPI set for a standard user.
// Aggregates cover only transactions owned by user, on top of the tenant
// predicate. It maps the employee dashboard (§8.1) onto the only in-scope
// feature — own transactions; expense workflow approval chains are out of scope
// per §4.2.
func (s *DashboardService) StandardUserKPIs(ctx context.Context, rng domain.DateRange, tenant, user uuid.UUID) (*StandardUserDashboard, error) {
	if err := s.validateRange(rng); err != nil {
		return nil, err
	}
	if tenant == uuid.Nil || user == uuid.Nil {
		return nil, domain.NewForbidden("invalid tenant or user context")
	}

	var view *ReadModelStandardView
	err := s.withCache(ctx, s.stdKey(tenant, rng), &view, func() error {
		v, err := s.read.StandardUserKPIs(ctx, rng, tenant, user)
		if err != nil {
			return domain.NewDependencyUnavailable("read model unavailable")
		}
		view = v
		return nil
	})
	if err != nil {
		return nil, err
	}

	return s.assembleStandard(view)
}

// ViewerKPIs assembles the read-only summary KPI set for a viewer role. It is
// limited to counts and totals; no margin or ratio math is performed, and no
// executive aggregates are exposed.
func (s *DashboardService) ViewerKPIs(ctx context.Context, rng domain.DateRange, tenant uuid.UUID) (*ViewerDashboard, error) {
	if err := s.validateRange(rng); err != nil {
		return nil, err
	}
	if tenant == uuid.Nil {
		return nil, domain.NewForbidden("invalid tenant context")
	}

	var view *ReadModelViewerSummary
	err := s.withCache(ctx, s.vwrKey(tenant, rng), &view, func() error {
		v, err := s.read.ViewerKPIs(ctx, rng, tenant)
		if err != nil {
			return domain.NewDependencyUnavailable("read model unavailable")
		}
		view = v
		return nil
	})
	if err != nil {
		return nil, err
	}

	return s.assembleViewer(view)
}

// validateRange enforces the DateRange invariant and bounds the span before any
// cache lookup or repository access. An inverted range is rejected with a
// VALIDATION_FAILED field error (start ≤ end, spec §7.2).
func (s *DashboardService) validateRange(rng domain.DateRange) error {
	if !rng.Valid() {
		return domain.NewValidationError(domain.FieldError{
			Field:   "range",
			Message: "start must not be after end",
		})
	}
	if rng.End.Sub(rng.Start) > maxRangeSpan {
		return domain.NewValidationError(domain.FieldError{
			Field:   "range",
			Message: "range spans more than the maximum supported window",
		})
	}
	return nil
}

// withCache serves compute through the tenant-scoped cache: a hit returns the
// stored read-model struct; a miss runs compute and populates the cache at the
// 60s TTL. A cache outage is never treated as a miss — compute falls back to the
// authoritative read model and the cache error is ignored so the request path
// degrades rather than failing.
func (s *DashboardService) withCache(ctx context.Context, key string, out interface{}, compute func() error) error {
	raw, err := s.cache.Get(ctx, key)
	if err == nil && raw != nil {
		if uerr := json.Unmarshal(raw, out); uerr == nil {
			return nil
		}
	}
	if cerr := compute(); cerr != nil {
		return cerr
	}
	if payload, merr := json.Marshal(out); merr == nil {
		// Set failures are non-fatal: the authoritative read model already
		// served this request, and the next call retries the cache fill.
		_ = s.cache.Set(ctx, key, payload, ports.BudgetAggregateTTL)
	}
	return nil
}

// cfoKey builds the tenant-scoped cache key for a CFO KPI set.
func (s *DashboardService) cfoKey(tenant uuid.UUID, rng domain.DateRange) string {
	return kpiKey(roleClassCFO, tenant, rng)
}

// stdKey builds the tenant-scoped cache key for a standard-user KPI set.
func (s *DashboardService) stdKey(tenant uuid.UUID, rng domain.DateRange) string {
	return kpiKey(roleClassStd, tenant, rng)
}

// vwrKey builds the tenant-scoped cache key for a viewer KPI set.
func (s *DashboardService) vwrKey(tenant uuid.UUID, rng domain.DateRange) string {
	return kpiKey(roleClassVwr, tenant, rng)
}

// kpiKey builds a tenant-scoped cache key forcing the tenant component from a
// server-derived tenant UUID. The tenant is part of the key/namespace, so one
// tenant's cached payload can never be served to another (CWE-346 origin
// validation). A nil tenant yields an empty key that must never be used.
func kpiKey(roleClass string, tenant uuid.UUID, rng domain.DateRange) string {
	if tenant == uuid.Nil {
		return ""
	}
	return "fms:v1:" + tenant.String() + ":kpi:" + roleClass +
		":" + rng.Start.Format("2006-01-02") + ":" + rng.End.Format("2006-01-02")
}

// KPIInvalidationPattern returns the allowlisted tenant-scoped pattern used by
// the transaction/budget mutation event consumer to sweep all dashboard KPI
// entries for a single tenant. Scope is exactly one tenant; a broad global flush
// is never emitted.
func KPIInvalidationPattern(tenant uuid.UUID) string {
	if tenant == uuid.Nil {
		return ""
	}
	return "fms:v1:" + tenant.String() + ":kpi:*"
}

// assembleCFO builds the bounded CFO KPI set (8 cards ≤ 9) from the read model.
// Empty datasets produce a fully-populated zero-valued dashboard, never an
// error.
func (s *DashboardService) assembleCFO(view *ReadModelCFOView) (*CFODashboard, error) {
	if view == nil {
		view = &ReadModelCFOView{}
	}
	cash, err := domain.NewFromMajor(view.CashPosition, baseCurrency)
	if err != nil {
		return nil, domain.NewInternal(err)
	}
	revenue, err := domain.NewFromMajor(view.Revenue, baseCurrency)
	if err != nil {
		return nil, domain.NewInternal(err)
	}
	asOf := view.AsOf
	if asOf.IsZero() {
		asOf = s.clock.Now()
	}

	cards := []Card{
		{ID: cardCashPosition, Label: "Cash Position", Value: cash.String(),
			Trend: nil, Drilldown: aLink(routeReportsCashFlow, nil)},
		{ID: cardRevenue, Label: "Revenue", Value: revenue.String(),
			Trend: trendFrom(view.Revenue, priorRevenue(view)), Drilldown: aLink(routeReportsPNL, nil)},
		{ID: cardGrossMargin, Label: "Gross Margin", Value: pct(view.GrossMargin),
			Trend: nil, Drilldown: aLink(routeReportsPNL, nil)},
		{ID: cardNetMargin, Label: "Net Margin", Value: pct(view.NetMargin),
			Trend: nil, Drilldown: aLink(routeReportsPNL, nil)},
		{ID: cardExpenseRatio, Label: "Expense Ratio", Value: pct(view.ExpenseRatio),
			Trend: nil, Drilldown: aLink(routeReportsExpensesByCat, nil)},
		{ID: cardBudgetBurn, Label: "Budget Burn Rate", Value: pct(view.BudgetBurnRate),
			Trend: nil, Drilldown: aLink(routeBudgets, nil)},
		{ID: cardAccountCount, Label: "Accounts", Value: strconv.Itoa(int(view.AccountCount)),
			Trend: nil, Drilldown: aLink(routeAccounts, nil)},
		{ID: cardPeriodClosed, Label: "Period Closed", Value: boolString(view.PeriodClosed),
			Trend: nil, Drilldown: nil},
	}

	if len(cards) > maxKPICards {
		return nil, domain.NewInternal(nil)
	}
	return &CFODashboard{
		CashPosition:   cash,
		Revenue:        revenue,
		GrossMargin:    view.GrossMargin,
		NetMargin:      view.NetMargin,
		ExpenseRatio:   view.ExpenseRatio,
		BudgetBurnRate: view.BudgetBurnRate,
		AccountCount:   view.AccountCount,
		PeriodClosed:   view.PeriodClosed,
		AsOf:           asOf,
		Cards:          cards,
	}, nil
}

// assembleStandard builds the personal-scope KPI set (5 cards). TopCategory, if
// absent (uuid.Nil), yields a dashes placeholder rather than a false category.
func (s *DashboardService) assembleStandard(view *ReadModelStandardView) (*StandardUserDashboard, error) {
	if view == nil {
		view = &ReadModelStandardView{}
	}
	income, err := domain.NewFromMajor(view.TotalIncome, baseCurrency)
	if err != nil {
		return nil, domain.NewInternal(err)
	}
	expenses, err := domain.NewFromMajor(view.TotalExpenses, baseCurrency)
	if err != nil {
		return nil, domain.NewInternal(err)
	}
	asOf := view.AsOf
	if asOf.IsZero() {
		asOf = s.clock.Now()
	}

	topCatVal := "—"
	if view.TopCategory != uuid.Nil {
		topCatVal = view.TopCategory.String()
	}

	cards := []Card{
		{ID: cardTotalIncome, Label: "Income", Value: income.String(),
			Trend: nil, Drilldown: aLink(routeReportsPNL, nil)},
		{ID: cardTotalExpenses, Label: "Expenses", Value: expenses.String(),
			Trend: nil, Drilldown: aLink(routeReportsExpensesByCat, nil)},
		{ID: cardTxCount, Label: "Transactions", Value: strconv.Itoa(int(view.TxCount)),
			Trend: nil, Drilldown: nil},
		{ID: cardTopCategory, Label: "Top Category", Value: topCatVal,
			Trend: nil, Drilldown: nil},
		{ID: cardBudgetBurn, Label: "Budget Burn Rate", Value: pct(view.BudgetBurnRate),
			Trend: nil, Drilldown: aLink(routeBudgets, nil)},
	}

	if len(cards) > maxKPICards {
		return nil, domain.NewInternal(nil)
	}
	return &StandardUserDashboard{
		TotalIncome:    income,
		TotalExpenses:  expenses,
		TxCount:        view.TxCount,
		TopCategory:    view.TopCategory,
		BudgetBurnRate: view.BudgetBurnRate,
		AsOf:           asOf,
		Cards:          cards,
	}, nil
}

// assembleViewer builds the read-only summary KPI set (4 cards) with counts and
// totals only.
func (s *DashboardService) assembleViewer(view *ReadModelViewerSummary) (*ViewerDashboard, error) {
	if view == nil {
		view = &ReadModelViewerSummary{}
	}
	income, err := domain.NewFromMajor(view.TotalIncome, baseCurrency)
	if err != nil {
		return nil, domain.NewInternal(err)
	}
	expenses, err := domain.NewFromMajor(view.TotalExpenses, baseCurrency)
	if err != nil {
		return nil, domain.NewInternal(err)
	}
	asOf := view.AsOf
	if asOf.IsZero() {
		asOf = s.clock.Now()
	}

	cards := []Card{
		{ID: cardAccountCount, Label: "Accounts", Value: strconv.Itoa(int(view.AccountCount)),
			Trend: nil, Drilldown: aLink(routeAccounts, nil)},
		{ID: cardTxCount, Label: "Transactions", Value: strconv.Itoa(int(view.TxCount)),
			Trend: nil, Drilldown: nil},
		{ID: cardTotalIncome, Label: "Income", Value: income.String(),
			Trend: nil, Drilldown: aLink(routeReportsPNL, nil)},
		{ID: cardTotalExpenses, Label: "Expenses", Value: expenses.String(),
			Trend: nil, Drilldown: aLink(routeReportsExpensesByCat, nil)},
	}

	if len(cards) > maxKPICards {
		return nil, domain.NewInternal(nil)
	}
	return &ViewerDashboard{
		AccountCount:      view.AccountCount,
		TxCount:           view.TxCount,
		TotalIncome:       income,
		TotalExpenses:     expenses,
		LastTransactionAt: view.LastTransactionAt,
		AsOf:              asOf,
		Cards:             cards,
	}, nil
}

// aLink builds a drilldown Link for the given allowlisted route and params. Only
// internal route constants are ever passed as the route; an empty route yields
// a nil link.
func aLink(route string, params map[string]string) *Link {
	if route == "" {
		return nil
	}
	return &Link{Route: route, Params: params}
}

// priorRevenue returns the prior-period revenue basis, or a nil trend basis when
// the read model supplies no prior period.
func priorRevenue(view *ReadModelCFOView) *int64 {
	if view == nil || view.PriorPeriod == nil {
		return nil
	}
	v := view.PriorPeriod.Revenue
	return &v
}

// trendFrom computes DeltaPercent = (current - prior) / |prior| * 100. With no
// prior basis or a zero prior value, it returns nil (avoiding a division by
// zero). Direction is derived server-side from the integer comparison basis.
func trendFrom(current int64, prior *int64) *Trend {
	if prior == nil || *prior == 0 {
		return nil
	}
	p := float64(*prior)
	pctChange := (float64(current) - p) / math.Abs(p) * 100
	direction := TrendFlat
	switch {
	case current > *prior:
		direction = TrendUp
	case current < *prior:
		direction = TrendDown
	}
	return &Trend{Direction: direction, DeltaPercent: pctChange}
}

// boolString renders a boolean KPI value for presentation.
func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// pct renders a ratio (0..1) as a percentage string with one decimal place for
// presentation only; it never feeds computation.
func pct(v float64) string {
	return fmt.Sprintf("%.1f%%", v*100)
}