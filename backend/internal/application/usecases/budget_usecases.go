// Package usecases implements the application-layer orchestration of the FMS
// core. Use cases depend only on the output ports and the domain layer; they
// contain no framework, HTTP, or persistence types.
//
// This file owns the Module-D (Budgeting) bounded context. It manages the budget
// lifecycle (create/update/delete/list) and the budget-vs-actual comparison,
// publishing threshold and audit events into the event bus.
package usecases

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/config"
	"fms/backend/internal/domain"
)

// Utilization percentages at which a budget is classified as "approaching" its
// allocation and "exceeded" by actuals. These are MVP policy constants; they
// are intentionally not migrated to configuration in this change.
const (
	approachThreshold = 80
	exceedThreshold   = 100
)

// Bounds for the bounded-collection budget list (spec default 20, max 100).
const (
	defaultBudgetPageSize = 20
	maxBudgetPageSize     = 100
)

// CacheOrigin reports where a budget-vs-actual result was served from. It is
// returned by the use case instead of an HTTP header so the transport layer
// remains an interface-adapter concern; the REST adapter maps it onto X-Cache.
type CacheOrigin string

const (
	// CacheOriginHit is returned when the result came from the cache.
	CacheOriginHit = "HIT"
	// CacheOriginMiss is returned when the result was computed from the ledger.
	CacheOriginMiss = "MISS"
	// CacheOriginStale is returned when the aggregation failed but a stale cached
	// result was served instead (graceful degradation).
	CacheOriginStale = "STALE"
)

// systemActorID is the reserved system principal under which the read path
// publishes threshold-monitoring events. It is never the zero UUID, which the
// domain Event validation requires for a valid actor.
var systemActorID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

// BudgetRepository is the output port for the Budget aggregate. Every method
// carries a tenant predicate so no query can cross a tenant boundary; Update
// and Delete perform version-guarded conditional writes for optimistic
// concurrency.
type BudgetRepository interface {
	// Create persists a new budget within the caller's transaction scope.
	Create(ctx context.Context, b *domain.Budget) error
	// Update persists field changes with an atomic conditional write guarded by
	// expectedVersion; a version mismatch surfaces as a conflict-state error.
	Update(ctx context.Context, b *domain.Budget, expectedVersion int64) error
	// Delete removes a budget only if it still carries expectedVersion.
	Delete(ctx context.Context, id, tenantID uuid.UUID, expectedVersion int64) error
	// Get loads a single tenant-scoped budget.
	Get(ctx context.Context, id, tenantID uuid.UUID) (*domain.Budget, error)
	// List returns a page of tenant-scoped budgets and the total row count.
	List(ctx context.Context, tenantID uuid.UUID, page, pageSize int) ([]*domain.Budget, int64, error)
}

// BudgetReadModel computes budget actuals from the ledger within a single
// aggregation statement, avoiding N+1 queries over the transaction dataset.
type BudgetReadModel interface {
	// ActualsForBudget returns the summed spending against a budget for its
	// exact period, scoped to the tenant.
	ActualsForBudget(ctx context.Context, budgetID, tenantID uuid.UUID, rng domain.DateRange) (domain.Money, error)
}

// CreateBudgetCommand carries the validated inputs for creating a budget.
type CreateBudgetCommand struct {
	CategoryID uuid.UUID
	PeriodType domain.BudgetPeriodType
	Start      time.Time
	End        time.Time
	Allocation domain.Money
}

// UpdateBudgetCommand carries the validated inputs for editing an existing
// budget.
type UpdateBudgetCommand struct {
	CategoryID uuid.UUID
	PeriodType domain.BudgetPeriodType
	Start      time.Time
	End        time.Time
	Allocation domain.Money
}

// BudgetComparison is the computed result of comparing a budget allocation
// against its actual spend. It is an application-layer DTO; there is no domain
// equivalent.
type BudgetComparison struct {
	BudgetID       uuid.UUID
	CategoryID     uuid.UUID
	PeriodType     domain.BudgetPeriodType
	Period         domain.DateRange
	Allocation     domain.Money
	Actual         domain.Money
	UtilizationPct int
	ThresholdState domain.BudgetThresholdState
}

// BudgetService orchestrates the Module-D budgeting use cases. The Notifier
// port is held here per the port contract; delivery of threshold alerts is
// decided by the Notifier adapter/router (Noop until contested §9.6 resolves).
// The use case publishes threshold domain events on the event bus.
type BudgetService struct {
	budgets BudgetRepository
	read    BudgetReadModel
	cache   ports.Cache
	events  ports.EventBus
	notify  ports.Notifier
	flags   config.FeatureFlags
	clock   ports.Clock
}

// NewBudgetService wires the budget use cases to their output ports.
func NewBudgetService(budgets BudgetRepository, read BudgetReadModel, cache ports.Cache, events ports.EventBus, notify ports.Notifier, flags config.FeatureFlags, now ports.Clock) *BudgetService {
	if now == nil {
		now = ports.SystemClock{}
	}
	return &BudgetService{
		budgets: budgets,
		read:    read,
		cache:   cache,
		events:  events,
		notify:  notify,
		flags:   flags,
		clock:   now,
	}
}

// CreateBudget validates the command and persists a new budget for a tenant.
// The tenant and actor IDs must come from the authenticated context, never
// from client input.
func (s *BudgetService) CreateBudget(ctx context.Context, cmd CreateBudgetCommand, tenantID, actorID uuid.UUID) (*domain.Budget, error) {
	if tenantID == uuid.Nil {
		return nil, domain.NewForbidden("invalid tenant context")
	}
	if actorID == uuid.Nil {
		return nil, domain.NewForbidden("insufficient permissions")
	}

	rng, err := domain.NewDateRange(cmd.Start, cmd.End)
	if err != nil {
		return nil, domain.NewValidationError(domain.FieldError{Field: "range", Message: "start must not be after end"})
	}
	if !cmd.PeriodType.Valid() {
		return nil, domain.NewValidationError(domain.FieldError{Field: "period_type", Message: "must be monthly, quarterly, or annual"})
	}
	if cmd.CategoryID == uuid.Nil {
		return nil, domain.NewValidationError(domain.FieldError{Field: "category_id", Message: "category is required"})
	}

	// Dedicated BudgetCreated/Updated/Deleted event types are not registered in
	// the domain registry (adding them is outside this file's scope), so the
	// lifecycle mutation is published as the registered EventEntityModified with
	// EntityType "budget". The past-period guard and allocation rules live in
	// the aggregate constructor, driven by the server-side backdated flag.
	budget, err := domain.NewBudget(s.clock.Now(), uuid.New(), tenantID, cmd.CategoryID, cmd.PeriodType, rng, cmd.Allocation, s.flags.BackdatedBudgetsEnabled())
	if err != nil {
		return nil, s.mapBudgetValidationError(err)
	}

	if err := s.budgets.Create(ctx, budget); err != nil {
		return nil, err
	}
	_ = s.publishEntityModified(ctx, domain.ActionCreate, tenantID, actorID, budget.ID, nil, budgetSnapshot(budget))
	_ = s.cache.InvalidatePattern(ctx, ports.BudgetPattern(tenantID.String()))
	return budget, nil
}

// UpdateBudget edits an existing budget under an optimistic version guard. The
// past-period guard is re-applied on update so an edit cannot silently move an
// active budget's window into the past.
func (s *BudgetService) UpdateBudget(ctx context.Context, id uuid.UUID, expectedVersion int64, cmd UpdateBudgetCommand, tenantID, actorID uuid.UUID) (*domain.Budget, error) {
	if tenantID == uuid.Nil {
		return nil, domain.NewForbidden("invalid tenant context")
	}
	if actorID == uuid.Nil {
		return nil, domain.NewForbidden("insufficient permissions")
	}

	budget, err := s.budgets.Get(ctx, id, tenantID)
	if err != nil {
		return nil, err
	}
	if budget == nil {
		return nil, domain.NewNotFound("budget", id.String())
	}
	if expectedVersion != budget.Version {
		return nil, domain.NewConflict("budget was modified by another user")
	}

	before := budgetSnapshot(budget)

	rng, err := domain.NewDateRange(cmd.Start, cmd.End)
	if err != nil {
		return nil, domain.NewValidationError(domain.FieldError{Field: "range", Message: "start must not be after end"})
	}
	if !cmd.PeriodType.Valid() {
		return nil, domain.NewValidationError(domain.FieldError{Field: "period_type", Message: "must be monthly, quarterly, or annual"})
	}
	if cmd.CategoryID == uuid.Nil {
		return nil, domain.NewValidationError(domain.FieldError{Field: "category_id", Message: "category is required"})
	}

	updated, err := domain.NewBudget(s.clock.Now(), id, tenantID, cmd.CategoryID, cmd.PeriodType, rng, cmd.Allocation, s.flags.BackdatedBudgetsEnabled())
	if err != nil {
		return nil, s.mapBudgetValidationError(err)
	}
	updated.Version = expectedVersion
	updated.ActualsSpent = budget.ActualsSpent

	if err := s.budgets.Update(ctx, updated, expectedVersion); err != nil {
		return conflictOrRepoError(err)
	}
	_ = s.publishEntityModified(ctx, domain.ActionEdit, tenantID, actorID, id, before, budgetSnapshot(updated))
	_ = s.cache.InvalidatePattern(ctx, ports.BudgetPattern(tenantID.String()))
	return updated, nil
}

// DeleteBudget removes a budget under an optimistic version guard. A repeat
// delete of the same id surfaces as NOT_FOUND in the API layer (idempotent).
func (s *BudgetService) DeleteBudget(ctx context.Context, id uuid.UUID, expectedVersion int64, tenantID, actorID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return domain.NewForbidden("invalid tenant context")
	}
	if actorID == uuid.Nil {
		return domain.NewForbidden("insufficient permissions")
	}

	budget, err := s.budgets.Get(ctx, id, tenantID)
	if err != nil {
		return err
	}
	if budget == nil {
		return domain.NewNotFound("budget", id.String())
	}
	if expectedVersion != budget.Version {
		return domain.NewConflict("budget was modified by another user")
	}

	before := budgetSnapshot(budget)

	if err := s.budgets.Delete(ctx, id, tenantID, expectedVersion); err != nil {
		return err
	}
	_ = s.publishEntityModified(ctx, domain.ActionDelete, tenantID, actorID, id, before, nil)
	_ = s.cache.InvalidatePattern(ctx, ports.BudgetPattern(tenantID.String()))
	return nil
}

// ListBudgets returns a tenant-scoped page of budgets with the total count.
// Pagination is bounded (page >= 1, 1 <= pageSize <= 100).
func (s *BudgetService) ListBudgets(ctx context.Context, tenantID uuid.UUID, page, pageSize int) ([]*domain.Budget, int64, error) {
	if tenantID == uuid.Nil {
		return nil, 0, domain.NewForbidden("invalid tenant context")
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = defaultBudgetPageSize
	}
	if pageSize > maxBudgetPageSize {
		pageSize = maxBudgetPageSize
	}
	return s.budgets.List(ctx, tenantID, page, pageSize)
}

// CompareBudgetVsActual returns the allocation-vs-actual comparison for a
// budget, served from cache when fresh. Authorization is resolved before any
// cache read and the cache key is tenant-scoped, so the cache can never be the
// source of truth for access control (defense against cross-tenant leakage).
// Threshold events are published only on a threshold-state crossing.
func (s *BudgetService) CompareBudgetVsActual(ctx context.Context, id, tenantID uuid.UUID) (*BudgetComparison, CacheOrigin, error) {
	if tenantID == uuid.Nil {
		return nil, CacheOriginMiss, domain.NewForbidden("invalid tenant context")
	}

	// Authorize + load first: the tenant-scoped Get establishes ownership. The
	// cache is never consulted before this check.
	budget, err := s.budgets.Get(ctx, id, tenantID)
	if err != nil {
		return nil, CacheOriginMiss, err
	}
	if budget == nil {
		return nil, CacheOriginMiss, domain.NewNotFound("budget", id.String())
	}

	key := ports.BudgetKey(tenantID.String(), id.String(), periodKey(budget.Range))
	if key == "" {
		return nil, CacheOriginMiss, domain.NewInternal(errors.New("cannot compute budget cache key"))
	}
	// Crossing-state marker: records the last published threshold state so a
	// crossing event is fired once per state, not on every comparison.
	notifiedKey := key + ":notified"

	raw, _ := s.cache.Get(ctx, key)
	if raw != nil {
		var cached BudgetComparison
		if err := json.Unmarshal(raw, &cached); err == nil {
			return &cached, CacheOriginHit, nil
		}
	}

	actuals, err := s.read.ActualsForBudget(ctx, id, tenantID, budget.Range)
	if err != nil {
		// Graceful degradation: serve a stale cached result when present.
		if raw != nil {
			var stale BudgetComparison
			if uerr := json.Unmarshal(raw, &stale); uerr == nil {
				return &stale, CacheOriginStale, nil
			}
		}
		return nil, CacheOriginMiss, domain.NewDependencyUnavailable("budget actuals unavailable")
	}

	if err := budget.RecomputeActuals(actuals); err != nil {
		return nil, CacheOriginMiss, domain.NewInternal(err)
	}

	// Publish on crossing only. Failures are best-effort and must not fail a
	// read path; the Notifier adapter decides any delivery.
	_ = s.maybePublishThreshold(ctx, budget, notifiedKey)

	comp, err := s.buildComparison(budget)
	if err != nil {
		return nil, CacheOriginMiss, err
	}

	payload, _ := json.Marshal(comp)
	_ = s.cache.Set(ctx, key, payload, ports.BudgetAggregateTTL)
	return comp, CacheOriginMiss, nil
}

// periodKey returns a canonical ISO interval string for a budget's date range,
// used to build cache keys. The value is derived server-side from the loaded
// budget, never from client input.
func periodKey(r domain.DateRange) string {
	return r.Start.Format("2006-01-02") + "/" + r.End.Format("2006-01-02")
}

// mapBudgetValidationError translates a domain budget constructor error into a
// typed field-level validation error.
func (s *BudgetService) mapBudgetValidationError(err error) error {
	switch {
	case errors.Is(err, domain.ErrPastPeriodNotAllowed):
		return domain.NewValidationError(domain.FieldError{Field: "range", Message: "backdated budget periods are not allowed"})
	case errors.Is(err, domain.ErrInvalidBudgetAllocation):
		return domain.NewValidationError(domain.FieldError{Field: "allocation", Message: "allocation must be a positive amount with at most two decimal places"})
	case errors.Is(err, domain.ErrInvalidBudgetPeriod):
		return domain.NewValidationError(domain.FieldError{Field: "period_type", Message: "must be monthly, quarterly, or annual"})
	case errors.Is(err, domain.ErrInvalidDateRange):
		return domain.NewValidationError(domain.FieldError{Field: "range", Message: "start must not be after end"})
	default:
		return domain.NewInternal(err)
	}
}

// newEvent wraps a payload into a validated domain.Event envelope, mirroring
// the UserService helper. The event ID doubles as the correlation id; Sequence
// is a non-zero placeholder assigned by the publishing infrastructure.
func (s *BudgetService) newEvent(typ domain.EventType, tenantID, actorID, aggregateID uuid.UUID, payload any) (domain.Event, error) {
	if tenantID == uuid.Nil || actorID == uuid.Nil || aggregateID == uuid.Nil {
		return domain.Event{}, domain.NewInternal(errors.New("use case: event requires non-zero ids"))
	}
	version, ok := typ.Version()
	if !ok {
		return domain.Event{}, domain.ErrUnknownEventType
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return domain.Event{}, domain.NewInternal(err)
	}
	evtID := uuid.New()
	return domain.Event{
		ID:            evtID,
		Type:          typ,
		Version:       version,
		Timestamp:     s.clock.Now().UTC(),
		TenantID:      tenantID,
		ActorID:       actorID,
		CorrelationID: evtID.String(),
		SourceService: "budget",
		AggregateType: "budget",
		AggregateID:   aggregateID.String(),
		Payload:       raw,
		Sequence:      1,
	}, nil
}

// publishEntityModified publishes a registered EventEntityModified lifecycle
// event for a budget mutation; the before/after state carries minimal budget
// details for the append-only audit consumer.
func (s *BudgetService) publishEntityModified(ctx context.Context, action domain.AuditAction, tenantID, actorID, entityID uuid.UUID, before, after json.RawMessage) error {
	payload := domain.EntityModifiedPayload{
		EntityType: "budget",
		EntityID:   entityID.String(),
		Action:     action,
		Before:     before,
		After:      after,
	}
	event, err := s.newEvent(domain.EventEntityModified, tenantID, actorID, entityID, payload)
	if err != nil {
		return err
	}
	return s.events.Publish(ctx, event)
}

// budgetSnapshot renders a non-secret subset of a budget carried in audit
// events; it omits sensitive values not required by the event contract.
func budgetSnapshot(b *domain.Budget) json.RawMessage {
	if b == nil {
		return nil
	}
	raw, err := json.Marshal(struct {
		PeriodType domain.BudgetPeriodType `json:"period_type"`
		Allocation string                  `json:"allocation"`
		Start      string                  `json:"start"`
		End        string                  `json:"end"`
		Version    int64                   `json:"version"`
	}{
		PeriodType: b.PeriodType,
		Allocation: b.Allocation.String(),
		Start:      b.Range.Start.Format(time.RFC3339),
		End:        b.Range.End.Format(time.RFC3339),
		Version:    b.Version,
	})
	if err != nil {
		return nil
	}
	return raw
}

// buildComparison assembles the application-layer comparison DTO from the
// loaded budget, computing utilization and the threshold state.
func (s *BudgetService) buildComparison(b *domain.Budget) (*BudgetComparison, error) {
	pct, _ := b.PercentageUsed()
	state, _, err := b.ThresholdState(approachThreshold, exceedThreshold)
	if err != nil {
		return nil, domain.NewInternal(err)
	}
	return &BudgetComparison{
		BudgetID:       b.ID,
		CategoryID:     b.CategoryID,
		PeriodType:     b.PeriodType,
		Period:         b.Range,
		Allocation:     b.Allocation,
		Actual:         b.ActualsSpent,
		UtilizationPct: pct,
		ThresholdState: state,
	}, nil
}

// maybePublishThreshold publishes a registered threshold-domain event when the
// budget crosses into the approaching or exceeded state. The published state is
// recorded against notifiedKey so the event fires once per state, guarding
// against event storms across cache-expiry cycles.
func (s *BudgetService) maybePublishThreshold(ctx context.Context, b *domain.Budget, notifiedKey string) error {
	state, crossing, err := b.ThresholdState(approachThreshold, exceedThreshold)
	if err != nil {
		return err
	}
	if !crossing {
		return nil
	}

	prior := domain.ThresholdStateNormal
	if raw, _ := s.cache.Get(ctx, notifiedKey); raw != nil {
		if p := domain.BudgetThresholdState(string(raw)); p == domain.ThresholdStateApproaching || p == domain.ThresholdStateExceeded {
			prior = p
		}
	}
	if state == prior {
		return nil
	}

	var typ domain.EventType
	switch state {
	case domain.ThresholdStateApproaching:
		typ = domain.EventBudgetThresholdApproached
	case domain.ThresholdStateExceeded:
		typ = domain.EventBudgetThresholdExceeded
	default:
		return nil
	}

	payload, err := domain.NewBudgetThresholdPayload(b.ID, b.CategoryID, usedPctString(b))
	if err != nil {
		return err
	}
	// Threshold events are published by the system principal (not an end user)
	// because they originate from the read/comparison path.
	event, err := s.newEvent(typ, b.TenantID, systemActorID, b.ID, payload)
	if err != nil {
		return err
	}
	if err := s.events.Publish(ctx, event); err != nil {
		return err
	}
	_ = s.cache.Set(ctx, notifiedKey, []byte(string(state)), ports.BudgetAggregateTTL)
	return nil
}

// usedPctString returns the budget utilization as a two-decimal string clamped
// to the [0.00, 100.00] range required by the threshold payload validation; an
// overrun is capped at the payload maximum.
func usedPctString(b *domain.Budget) string {
	if b == nil {
		return "0.00"
	}
	pct, _ := b.PercentageUsed()
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return strconv.Itoa(pct) + ".00"
}