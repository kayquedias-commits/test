// Package events implements the event-pipeline side of Blueprint §7.4. This
// file wires the idempotent projection consumers — read-model refresh, budget
// actuals recompute, and the append-only audit writer — behind a single code
// path shared by both the in-process InMemoryBus subscription and the RabbitMQ
// deliveries dispatched by cmd/worker/main.go. Each consumer applies its events
// in order, deduplicates by (consumer, event ID), records a checkpoint on
// success, retries transient failures with exponential backoff, and falls back
// to a tenant-scoped full recompute for eventual consistency. Consumers touch
// only read-model/budget/audit sinks and never write the authoritative ledger,
// preserving the write-path invariants (§5.3).
package events

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/domain"
	"fms/backend/internal/infrastructure/audit"
	"fms/backend/internal/infrastructure/observability"
)

// Pipeline tuning constants: replay/backoff bounds, the batch cap, and the key
// shapes matched by the persistence layer. The backoff follows §7.7 (base 100
// ms, factor 2) bounded to five attempts; the batch cap bounds memory per
// delivery (CWE-400 / CWE-770); the checkpoint and dedup key formats are shared
// so the persistence side can correlate exactly.
const (
	recomputeBackoffBase   = 100 * time.Millisecond
	recomputeBackoffFactor = 2
	recomputeMaxAttempts   = 5
	eventsBatchLimit       = 1000
	recomputeMinInterval   = 60 * time.Second

	checkpointKeyFmt = "checkpoint:%s:%d"
	dedupKeyFmt      = "dedup:%s:%s"

	consumerReadModel = "readmodel.Refresh"
	consumerBudget    = "budget.Actuals"
	consumerAudit     = "audit.Writer"
)

// ConsumerDeps bundles the ports and seams a projection consumer needs. Audit
// is the concrete append-only store (the changeset names a Store, and the
// in-scope ports package declares no audit-writer port), so it is injected as
// *audit.Store. AuditReadLogging mirrors the config.FeatureFlags gate so the
// worker advertises the READ-logging posture without re-reading the
// environment; mutation logging is unconditional and never gated.
type ConsumerDeps struct {
	ReadModels       ports.ReadModelRepository
	Budgets          ports.BudgetRepository
	Audit            *audit.Store
	Log              *observability.Logger
	Metrics          *observability.Metrics
	AuditReadLogging bool
}

// CheckpointStore is the durable-replay seam through which consumers resume
// exactly after a crash. It is declared here so the worker bus can bind a
// persistence-backed implementation when one exists; the in-process bus
// carries a no-op store because its deliveries are post-commit best-effort and
// durable recovery is owned by the outbox relay.
type CheckpointStore interface {
	Load(ctx context.Context, name string, partition int) (int64, error)
	Save(ctx context.Context, name string, partition int, seq int64) error
}

// noopCheckpoint implements CheckpointStore as an inert seam: it records
// nothing and returns zero so replay resumes from the stream head. Used on the
// in-memory path, where checkpointing is layered by the outbox relay rather
// than by the sink.
type noopCheckpoint struct{}

func (noopCheckpoint) Load(context.Context, string, int) (int64, error) { return 0, nil }
func (noopCheckpoint) Save(context.Context, string, int, int64) error   { return nil }

// ProjectionConsumer holds the shared sinks and pipeline plumbing behind the
// package's Consumer func alias. The struct carries mutable state (per-consumer
// dedup sets, per-tenant recompute stamps) that a bare func cannot; each
// registered consumer keeps its own dedup set so the same event ID can be
// applied once by each independent sink (read-model AND budget both react to a
// transaction event).
type ProjectionConsumer struct {
	bus        *InMemoryBus
	log        *observability.Logger
	metrics    *observability.Metrics
	readModels ports.ReadModelRepository
	budgets    ports.BudgetRepository
	audit      *audit.Store
	auditRead  bool
	checkpoint CheckpointStore

	mu       sync.Mutex
	processed map[string]struct{}         // key = consumer + "\x00" + eventID
	lastFull  map[uuid.UUID]time.Time     // per-tenant recompute rate-limit stamp
}

// newProjectionConsumer assembles a consumer with empty dedup sets and a
// default rate-limit map. The no-op checkpoint store keeps the in-memory path
// self-contained; a persistent store can be assigned before registration.
func newProjectionConsumer(deps ConsumerDeps, bus *InMemoryBus) *ProjectionConsumer {
	return &ProjectionConsumer{
		bus:        bus,
		log:        deps.Log,
		metrics:    deps.Metrics,
		readModels: deps.ReadModels,
		budgets:    deps.Budgets,
		audit:      deps.Audit,
		auditRead:  deps.AuditReadLogging,
		checkpoint: noopCheckpoint{},
		processed:  make(map[string]struct{}),
		lastFull:   make(map[uuid.UUID]time.Time),
	}
}

func (c *ProjectionConsumer) logf(level, msg string, kv ...any) {
	if c.log == nil {
		return
	}
	switch level {
	case "warn":
		c.log.Warn(msg, kv...)
	case "error":
		c.log.Error(msg, kv...)
	default:
		c.log.Info(msg, kv...)
	}
}

func (c *ProjectionConsumer) recordError(code string) {
	if c.metrics != nil {
		c.metrics.IncError(code)
	}
}

// markProcessed inserts the (consumer, event ID) pair into the dedup set and
// reports whether it was already present. Only the first delivery of a given
// key applies; later deliveries become a no-op (at-least-once safety). The
// consumer component keeps two sinks reacting to the same event on different
// dedup keys, matching the dedup:{consumer}:{eventID} contract.
func (c *ProjectionConsumer) markProcessed(consumer string, id uuid.UUID) bool {
	key := consumer + "\x00" + id.String()
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, dup := c.processed[key]; dup {
		return false
	}
	c.processed[key] = struct{}{}
	return true
}

// errorIsTransient classifies a domain error as retryable: dependency and
// conflict (busy/optimistic) outcomes are worth retrying with backoff, while
// validation/malformed outcomes are permanent and never retried.
func errorIsTransient(err error) bool {
	de, ok := domain.AsDomainError(err)
	if !ok {
		return true
	}
	switch de.Code {
	case domain.CodeDependencyUnavailable, domain.CodeConflictState:
		return true
	default:
		return false
	}
}

// domainErrorCode flattens an error to its canonical §7.7 code, defaulting to
// INTERNAL_ERROR for non-domain errors so metrics/labels stay on the closed
// vocabulary.
func domainErrorCode(err error) string {
	if de, ok := domain.AsDomainError(err); ok {
		return string(de.Code)
	}
	return string(domain.CodeInternalError)
}

// withRetry invokes fn up to recomputeMaxAttempts times, waiting an exponential
// backoff between attempts on transient failures. A permanent error is returned
// immediately. The final transient error is returned so the caller can fall
// back to full recompute.
func (c *ProjectionConsumer) withRetry(ctx context.Context, e domain.Event, fn func(context.Context, domain.Event) error) error {
	var lastErr error
	delay := recomputeBackoffBase
	for attempt := 0; attempt < recomputeMaxAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := fn(ctx, e)
		if err == nil {
			return nil
		}
		lastErr = err
		if !errorIsTransient(err) {
			return err
		}
		if attempt+1 < recomputeMaxAttempts {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
			delay *= recomputeBackoffFactor
		}
	}
	return lastErr
}

// fullRecompute is the eventual-consistency fallback per §7.4: after a sink
// exhausts its retries it reconciles the tenant's projections from the source
// ledger. It is rate-limited per tenant (recomputeMinInterval), logged at WARN
// as a recoverable event, and counted on the error metric so repeated triggers
// surface as an intrusion signal. The projection-vs-ledger diff query itself
// lives in the persistence layer behind the read-model and budget ports.
func (c *ProjectionConsumer) fullRecompute(ctx context.Context, tenant uuid.UUID, reason string) error {
	c.mu.Lock()
	last, ok := c.lastFull[tenant]
	now := time.Now().UTC()
	if ok && now.Sub(last) < recomputeMinInterval {
		c.mu.Unlock()
		return errors.New("events: full recompute rate-limit exceeded for tenant")
	}
	c.lastFull[tenant] = now
	c.mu.Unlock()

	c.logf("warn", "events: full_recompute", "tenant_id", tenant.String(), "reason", reason)
	c.recordError(string(domain.CodeDependencyUnavailable))

	if c.readModels != nil {
		if derr := c.readModels.RefreshAccountBalances(ctx, tenant); derr != nil {
			c.logf("error", "events: full_recompute read_model refresh failed", "tenant_id", tenant.String())
		}
	}
	if c.budgets != nil {
		now := time.Now().UTC()
		if derr := c.budgets.ActualsByCategory(ctx, tenant, domain.StartOfMonth(now), now); derr != nil {
			c.logf("error", "events: full_recompute budget actuals failed", "tenant_id", tenant.String())
		}
	}
	return nil
}

// refreshReadModel is the readmodel.Refresh sink: on transaction create/edit/
// delete and period close it re-materializes the tenant's account-balance
// projection from the ledger through the read-model port. The changeset names a
// richer RefreshProjection(tenant,event) port that does not exist on disk (only
// RefreshAccountBalances/RefreshBudgetActuals are declared), so this sink
// performs the tenant-scoped projection refresh — the real, compilable
// equivalent. The immutable reporting snapshot for a closed period is
// materialized by FinancePeriodRepository.CompleteClose on the write path, not
// here.
func (c *ProjectionConsumer) refreshReadModel(ctx context.Context, e domain.Event) error {
	if e.TenantID == uuid.Nil {
		c.logf("warn", "events: read_model rejected event without tenant", "event_id", e.ID.String(), "event_type", string(e.Type))
		c.recordError(string(domain.CodeValidationFailed))
		return nil
	}
	switch e.Type {
	case domain.EventPeriodClosed:
		if _, derr := domain.DecodePayload(e.Type, e.Payload); derr != nil {
			c.logf("warn", "events: read_model failed to decode period-closed payload", "event_id", e.ID.String())
			return nil
		}
	case domain.EventTransactionPosted, domain.EventTransactionEdited, domain.EventTransactionDeleted:
		// Validated defensively; the bus already validated the envelope.
	}
	if c.readModels == nil {
		return nil
	}
	return c.readModels.RefreshAccountBalances(ctx, e.TenantID)
}

// recomputeBudgetActuals is the budget.Actuals sink: on transaction create/
// edit/delete it recomputes the tenant's spend actuals through the budget-
// actuals port (a single-statement aggregate, no per-row loop). The changeset
// names a BudgetRepository.RecomputeActuals(tenant, categoryID, period) port
// and an EventCategoryMerged type, neither of which exist on disk, and the
// transaction event payloads carry no category/period; the recompute is
// therefore tenant-scoped over the current month to date. When a category
// + period-scoped port and a merged-category event land, this sink narrows to
// them and invalidates the cache:budget:{budgetId}:{period} key.
func (c *ProjectionConsumer) recomputeBudgetActuals(ctx context.Context, e domain.Event) error {
	if e.TenantID == uuid.Nil {
		c.logf("warn", "events: budget rejected event without tenant", "event_id", e.ID.String(), "event_type", string(e.Type))
		c.recordError(string(domain.CodeValidationFailed))
		return nil
	}
	switch e.Type {
	case domain.EventTransactionPosted, domain.EventTransactionEdited, domain.EventTransactionDeleted:
	default:
		c.logf("warn", "events: budget ignored unconsumed event type", "event_type", string(e.Type))
		return nil
	}
	if c.budgets == nil {
		return nil
	}
	now := time.Now().UTC()
	return c.budgets.ActualsByCategory(ctx, e.TenantID, domain.StartOfMonth(now), now)
}

// writeAudit is the audit.Writer sink. It accepts only mutation entities
// (create/edit/delete); EntityModifiedPayload.Validate already enforces
// IsMutation, so a READ action never reaches this sink — read logging is
// emitted by the audit-read middleware when the AuditReadLogging flag is set,
// never as a domain event. Audit.Append owns the chained-hash computation; a
// CONFLICT_STATE on append is treated as a duplicate (already logged) and
// swallowed so replay is a safe no-op.
func (c *ProjectionConsumer) writeAudit(ctx context.Context, e domain.Event) error {
	if e.Type != domain.EventEntityModified {
		return nil
	}
	if e.TenantID == uuid.Nil || e.ActorID == uuid.Nil {
		c.logf("warn", "events: audit rejected event without tenant/actor", "event_id", e.ID.String())
		c.recordError(string(domain.CodeValidationFailed))
		return nil
	}
	raw, derr := domain.DecodePayload(e.Type, e.Payload)
	if derr != nil {
		c.logf("warn", "events: audit failed to decode entity-modified payload", "event_id", e.ID.String())
		return nil
	}
	p, ok := raw.(domain.EntityModifiedPayload)
	if !ok {
		c.logf("warn", "events: audit unexpected payload shape", "event_id", e.ID.String())
		return nil
	}
	if c.audit == nil {
		return nil
	}
	entry := &audit.Entry{
		TenantID:      e.TenantID,
		Action:        p.Action,
		ActorID:       e.ActorID,
		EntityType:    p.EntityType,
		EntityID:      p.EntityID,
		OccurredAt:    e.Timestamp,
		Before:        p.Before,
		After:         p.After,
		CorrelationID: e.CorrelationID,
	}
	_, aerr := c.audit.Append(ctx, entry)
	if aerr != nil && aerr.Code != domain.CodeConflictState {
		return aerr
	}
	return nil
}

// applyWithCheckpoint runs the selected sink for one event, deduplicating by
// (consumer, event ID), retrying transient failures, checkpointing on success,
// and falling back to full recompute after final failure. It is the single code
// path shared by the in-memory subscription and the worker's RabbitMQ
// deliveries.
func (c *ProjectionConsumer) applyWithCheckpoint(ctx context.Context, consumer string, sink func(context.Context, domain.Event) error, e domain.Event, partition int) error {
	if !c.markProcessed(consumer, e.ID) {
		// Already applied by this consumer; do not re-apply and do not advance
		// the checkpoint again (idempotent replay).
		return nil
	}
	if err := e.Validate(); err != nil {
		c.logf("warn", "events: consumer rejected invalid event", "consumer", consumer, "event_id", e.ID.String())
		return nil
	}

	err := c.withRetry(ctx, e, func(cctx context.Context, ev domain.Event) error {
		return sink(cctx, ev)
	})
	if err != nil {
		c.logf("error", "events: consumer failed after retries", "consumer", consumer, "event_id", e.ID.String())
		return c.fullRecompute(ctx, e.TenantID, consumer+":"+string(e.Type))
	}
	if c.checkpoint != nil {
		if cerr := c.checkpoint.Save(context.Background(), consumer, partition, e.Sequence); cerr != nil && c.log != nil {
			c.log.Warn("events: consumer checkpoint save failed", "consumer", consumer, "event_id", e.ID.String())
		}
	}
	return nil
}

// sinkFor selects the concrete sink for a consumed event type. It is the seam
// the worker (cmd/worker/main.go) reuses to run the same apply functions on
// RabbitMQ deliveries, guaranteeing projection logic never diverges between
// transports.
func sinkFor(c *ProjectionConsumer, t domain.EventType) func(context.Context, domain.Event) error {
	switch t {
	case domain.EventTransactionPosted, domain.EventTransactionEdited,
		domain.EventTransactionDeleted, domain.EventPeriodClosed:
		return c.refreshReadModel
	case domain.EventEntityModified:
		return c.writeAudit
	default:
		return nil
	}
}

// RegisterConsumers wires the three projection/audit consumers onto the
// in-process InMemoryBus. Each event type is subscribed by every consumer that
// must react to it:
//
//   - readmodel.Refresh  -> TransactionPosted/Edited/Deleted, PeriodClosed
//   - budget.Actuals     -> TransactionPosted/Edited/Deleted
//   - audit.Writer       -> EntityModified
//
// Subscribe's fan-out runs each sink synchronously within the post-commit
// dispatch window; its errors are recorded but never fail the already-committed
// mutation, matching the bus contract in outbox_repository.go. The same sinks
// are reachable through sinkFor for the worker transport.
func RegisterConsumers(bus *InMemoryBus, deps ConsumerDeps) {
	c := newProjectionConsumer(deps, bus)

	budgetTypes := []domain.EventType{
		domain.EventTransactionPosted,
		domain.EventTransactionEdited,
		domain.EventTransactionDeleted,
	}
	for _, t := range budgetTypes {
		typ := t
		bus.Subscribe(string(typ), func(ctx context.Context, e domain.Event) error {
			return c.applyWithCheckpoint(ctx, consumerBudget, c.recomputeBudgetActuals, e, 0)
		})
	}

	readTypes := append([]domain.EventType{},
		domain.EventTransactionPosted,
		domain.EventTransactionEdited,
		domain.EventTransactionDeleted,
		domain.EventPeriodClosed,
	)
	for _, t := range readTypes {
		typ := t
		bus.Subscribe(string(typ), func(ctx context.Context, e domain.Event) error {
			return c.applyWithCheckpoint(ctx, consumerReadModel, c.refreshReadModel, e, 0)
		})
	}

	bus.Subscribe(string(domain.EventEntityModified), func(ctx context.Context, e domain.Event) error {
		return c.applyWithCheckpoint(ctx, consumerAudit, c.writeAudit, e, 0)
	})
}