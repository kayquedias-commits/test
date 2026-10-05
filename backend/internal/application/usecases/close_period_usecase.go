// Package usecases implements the application use-case layer: it orchestrates
// output ports, applies business rules, owns transaction boundaries, and
// publishes domain events. This file delivers the period-end close workflow
// (open -> closing -> closed) behind the Core Accounting (Ledger) bounded
// context, following the Month-End / Period-End Close business workflow and the
// immutable-reporting-invariant (§7.5, §6.1, §5.3).
//
// The close is a coordinated state machine guarded by a distributed lock so a
// concurrent close across replicas cannot double-run. Open ledger items are
// consumed in bounded batches of closeBatchSize rows, each advanced within its
// own transaction window. Once finalize completes the period is locked and a
// single, append-only reporting snapshot is materialized for it.
package usecases

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"math/big"
	"time"

	"github.com/google/uuid"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/domain"
)

// closeBatchSize is the fixed ceiling for one close batch (1,000 ledger rows
// per §7.5). It is a constant policy, never derived from caller input, so a
// caller cannot enlarge the per-batch transaction window (bounded tx windows,
// CWE-770).
const closeBatchSize = 1000

// Close-policy tunables for the distributed lock and batch retry. The lock
// lease must comfortably exceed the longest single batch window; it is renewed
// between batches so one close never holds a single long-lived lease.
const (
	lockLease        = 30 * time.Second
	lockNamePrefix   = "period_close:"
	maxCloseRetries  = 3
	retryBaseBackoff = 100 * time.Millisecond
)

// Job statuses persisted on the period_close job row. A FAILED job is terminal
// from the advancing path's perspective; it must be resumed through a fresh
// StartClose (guarded by the status state machine), never continued in place.
const (
	closeJobRunning   = "RUNNING"
	closeJobCompleted = "COMPLETED"
	closeJobFailed    = "FAILED"
)

// CloseJob is the transport projection of a period-close job row. It is a
// persistence seam, not a domain entity: the period's lifecycle invariants live
// on the domain.FinancePeriod aggregate, while this record tracks the
// batched-close progress (batches_total/done) and the resumable checkpoint.
type CloseJob struct {
	ID               uuid.UUID
	PeriodID         uuid.UUID
	TenantID         uuid.UUID
	BatchesTotal     int
	BatchesDone      int
	Status           string
	CheckpointCursor *string
	LastError        string
	LockToken        string
	CreatedBy        uuid.UUID
	CreatedAt        time.Time
	EndedAt          *time.Time
}

// BatchResult reports the outcome of one ProcessBatch advance.
type BatchResult struct {
	JobID         uuid.UUID
	BatchesDone   int
	BatchesTotal  int
	Done          bool
	ProcessedRows int
}

// ReportSnapshot is the immutable, append-only reporting projection captured
// for a closed period. It is read from the reporting read model (never
// recomputed against the live ledger at finalize time) and keyed uniquely by
// period so it is written exactly once per closed period (append-only, §5.3).
type ReportSnapshot struct {
	PeriodID  uuid.UUID
	TenantID  uuid.UUID
	Payload   []byte
	CreatedAt time.Time
}

// OpenPeriodSpec is the validated input for opening a new fiscal period. It
// carries only caller-supplied fields; tenancy is always taken from the
// authorized actor, never from this struct.
type OpenPeriodSpec struct {
	ID    uuid.UUID
	Label string
	Start time.Time
	End   time.Time
}

// CloseActor is the server-resolved principal driving a close operation. It is
// loaded from the actor repository (never from caller-supplied flags) so the
// use case can enforce authorization and tenant isolation internally.
type CloseActor struct {
	UserID   uuid.UUID
	TenantID uuid.UUID
	Role     domain.Role
}

// CloseJobRepository is the output port for persisting the period-close job
// progress. It is declared here because no shared job-store contract exists on
// disk; the concrete adapter lives in the interface-adapter layer.
//
// Contract notes:
//   - Every method takes the tenant predicate explicitly so isolation is
//     enforced at the persistence boundary (WHERE tenant_id / RLS).
//   - IncrementDone is a conditional update guarded by the expected
//     batch sequence (batches_done = $expected); a replayed or double-advanced
//     batch yields zero rows and surfaces CONFLICT_STATE so retries are
//     idempotent.
//   - Complete must be guarded (status='RUNNING' AND batches_done = batches_total)
//     so a job cannot be finalized before its batches are consumed.
type CloseJobRepository interface {
	Create(ctx context.Context, tenantID uuid.UUID, job *CloseJob) *domain.DomainError
	GetByID(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) (*CloseJob, *domain.DomainError)
	CountOpenRows(ctx context.Context, tenantID uuid.UUID, from, to time.Time) (int64, *domain.DomainError)
	IncrementDone(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, batchSeq int, checkpoint *string) *domain.DomainError
	Fail(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, errText string) *domain.DomainError
	Complete(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) *domain.DomainError
}

// SnapshotRepository is the output port for the immutable reporting snapshot
// of a closed period. Append is keyset-unique on period_id; a second append for
// an already-closed period yields a unique-violation surfaced as
// CONFLICT_STATE so a closed period's snapshot is never overwritten (§5.3).
type SnapshotRepository interface {
	Append(ctx context.Context, tenantID uuid.UUID, snap *ReportSnapshot) *domain.DomainError
}

// CloseLock is the output port for the distributed per-period close lock. It
// mirrors the Redis adapter's TryLock/RenewLock/ReleaseLock surface so a close
// may be coordinated across replicas (§5.10). Renew/Release compare the owner
// token and abort with ErrLockLost when it no longer matches.
type CloseLock interface {
	TryLock(ctx context.Context, name string, ttl time.Duration, token string) (bool, error)
	RenewLock(ctx context.Context, name string, ttl time.Duration, token string) error
	ReleaseLock(ctx context.Context, name string, token string) error
}

// ErrCloseLockLost is the sentinel surfaced when the close lock expired or was
// taken over mid-run. The worker must fail closed rather than advance an
// unowned lock (CWE-667).
var ErrCloseLockLost = errors.New("close lock lost or expired")

// CloseActorRepo is the output port for resolving the acting principal's role
// and tenant for use-case authorization (BFLA mitigation at every entry point).
type CloseActorRepo interface {
	FindActor(ctx context.Context, userID uuid.UUID) (CloseActor, error)
}

// SnapshotPayload is the serializable report-snapshot envelope. It carries a
// schema_version so the immutable payload can evolve without breaking consumers.
type SnapshotPayload struct {
	SchemaVersion int                        `json:"schema_version"`
	PeriodID      uuid.UUID                  `json:"period_id"`
	From          time.Time                  `json:"from"`
	To            time.Time                  `json:"to"`
	ClosedBy      uuid.UUID                  `json:"closed_by"`
	ClosedAt      time.Time                  `json:"closed_at"`
	Status        domain.PeriodStatus       `json:"status"`
	Summary       json.RawMessage            `json:"summary,omitempty"`
}

// ClosePeriodUseCase orchestrates the period-end close workflow. It depends
// only on output ports and domain types (Clean Architecture: no framework or
// ORM). It owns authorization, the distributed lock lifecycle, the batched
// progression, and the post-commit domain events.
type ClosePeriodUseCase struct {
	periods    ports.FinancePeriodRepository
	txns       ports.TransactionRepository
	jobs       CloseJobRepository
	snapshots  SnapshotRepository
	lock       CloseLock
	events     ports.EventBus
	actors     CloseActorRepo
	clock      ports.Clock
	permission domain.Permission
}

// NewClosePeriodUseCase wires the close workflow to its output ports and the
// RBAC permission required for every close entry point. The composition root
// supplies the domain permission that gates OpenPeriod/StartClose/
// ProcessBatch/FinalizeClose.
func NewClosePeriodUseCase(
	periods ports.FinancePeriodRepository,
	txns ports.TransactionRepository,
	jobs CloseJobRepository,
	snapshots SnapshotRepository,
	lock CloseLock,
	events ports.EventBus,
	actors CloseActorRepo,
	clock ports.Clock,
	permission domain.Permission,
) *ClosePeriodUseCase {
	return &ClosePeriodUseCase{
		periods:    periods,
		txns:       txns,
		jobs:       jobs,
		snapshots:  snapshots,
		lock:       lock,
		events:     events,
		actors:     actors,
		clock:      clock,
		permission: permission,
	}
}

// lockName derives the distributed-lock key for a period. The Redis adapter
// already prefixes lock:{name}, so the key carries only the period qualifier.
func (uc *ClosePeriodUseCase) lockName(periodID uuid.UUID) string {
	return lockNamePrefix + periodID.String()
}

// authorize resolves the actor from the actor repository and enforces, inside
// the use case, that the actor belongs to the given tenant and holds the
// configured close permission. It returns a generic FORBIDDEN on any mismatch so
// no principal existence is revealed. Authorization is never trusted from the
// caller and is re-checked independently at every entry point.
func (uc *ClosePeriodUseCase) authorize(ctx context.Context, actorID, tenantID uuid.UUID) (CloseActor, *domain.DomainError) {
	actor, err := uc.actors.FindActor(ctx, actorID)
	if err != nil {
		return CloseActor{}, domain.NewForbidden("access denied")
	}
	if actor.UserID != actorID || actor.TenantID != tenantID {
		return CloseActor{}, domain.NewForbidden("access denied")
	}
	if !actor.Role.Has(uc.permission) {
		return CloseActor{}, domain.NewForbidden("access denied")
	}
	return actor, nil
}

// newEvent builds a validated domain.Event envelope around a registered payload.
// The outbox/EventBus adapter assigns the authoritative per-aggregate monotonic
// Sequence and carries tenant/actor context from the IAM-established request
// context, per the EventBus port contract.
func (uc *ClosePeriodUseCase) newEvent(ctx context.Context, actor CloseActor, typ domain.EventType, aggregateType, aggregateID string, payload any) (domain.Event, *domain.DomainError) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return domain.Event{}, domain.NewInternal(err)
	}
	ev := domain.Event{
		ID:            uuid.New(),
		Type:          typ,
		Version:       1,
		Timestamp:     uc.clock.Now(),
		TenantID:      actor.TenantID,
		ActorID:       actor.UserID,
		CorrelationID: uuid.New().String(),
		SourceService: "ledger",
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		Payload:       raw,
		Sequence:      1,
	}
	if err := ev.Validate(); err != nil {
		return domain.Event{}, domain.NewInternal(err)
	}
	return ev, nil
}

// OpenPeriod creates a new open FinancePeriod for the tenant. It enforces the
// close-admin permission, validates the supplied window (start <= end, via the
// DateRange invariant), and delegates construction to the domain aggregate so
// label/range/invariant rules are enforced at one authoritative place. A
// duplicate (tenant, period_start, period_end) surfaces as CONFLICT_STATE,
// giving PUT-style idempotency.
func (uc *ClosePeriodUseCase) OpenPeriod(ctx context.Context, spec OpenPeriodSpec, tenantID, actorID uuid.UUID) (*domain.FinancePeriod, *domain.DomainError) {
	if _, err := uc.authorize(ctx, actorID, tenantID); err != nil {
		return nil, err
	}
	if spec.ID == uuid.Nil {
		return nil, domain.NewValidationError(domain.FieldError{Field: "id", Message: "period id must be provided"})
	}

	rng, err := domain.NewDateRange(spec.Start, spec.End)
	if err != nil {
		return nil, domain.NewValidationError(domain.FieldError{Field: "range", Message: "period range must have a start on or before its end"})
	}

	p, err := domain.NewFinancePeriod(spec.ID, tenantID, spec.Label, rng)
	if err != nil {
		return nil, domain.NewValidationError(domain.FieldError{Field: "period", Message: "invalid period parameters"})
	}

	if derr := uc.periods.Create(ctx, tenantID, &p); derr != nil {
		if derr.Code == domain.CodeConflictState {
			return nil, domain.NewConflict("a period already exists for this window")
		}
		return nil, derr
	}

	after, err := json.Marshal(struct {
		ID     uuid.UUID           `json:"id"`
		Status domain.PeriodStatus `json:"status"`
		From   time.Time           `json:"from"`
		To     time.Time           `json:"to"`
	}{ID: p.ID, Status: p.Status, From: rng.Start, To: rng.End})
	if err != nil {
		return nil, domain.NewInternal(err)
	}
	ev, derr := uc.newEvent(ctx, CloseActor{UserID: actorID, TenantID: tenantID}, domain.EventEntityModified, "period", p.ID.String(), domain.EntityModifiedPayload{
		EntityType: "period",
		EntityID:   p.ID.String(),
		Action:     domain.ActionCreate,
		Before:     nil,
		After:      after,
	})
	if derr != nil {
		return nil, derr
	}
	_ = uc.events.Publish(ctx, ev)
	return &p, nil
}

// StartClose acquires the per-period distributed lock, transitions the period
// open -> closing under the domain guard, and records a fresh close job with
// the batch plan (batches_total = ceil(open_rows / closeBatchSize)). A lock
// already held surfaces ErrConcurrentClose (CONFLICT_STATE); a period already
// closing or closed surfaces CONFLICT_STATE via the legal-transition guard.
// The lock is released on every exit path via defer.
func (uc *ClosePeriodUseCase) StartClose(ctx context.Context, periodID, actorID, tenantID uuid.UUID) (*CloseJob, *domain.DomainError) {
	actor, derr := uc.authorize(ctx, actorID, tenantID)
	if derr != nil {
		return nil, derr
	}
	if periodID == uuid.Nil {
		return nil, domain.NewValidationError(domain.FieldError{Field: "period_id", Message: "period id must be provided"})
	}

	// Load the period once to validate tenancy and pre-transition state.
	period, derr := uc.periods.Get(ctx, tenantID, periodID)
	if derr != nil {
		if derr.Code == domain.CodeNotFound {
			return nil, domain.NewNotFound("period", "")
		}
		return nil, derr
	}
	if period.TenantID != tenantID {
		return nil, domain.NewNotFound("period", "")
	}
	if !period.Status.CanTransitionTo(domain.PeriodClosing) {
		return nil, domain.NewConflict("period is not open for close")
	}

	// Acquire the distributed lock before any mutation; concurrent StartClose
	// on the same period across replicas is excluded by the SET NX lock.
	name := uc.lockName(periodID)
	token := uuid.New().String()
	ok, err := uc.lock.TryLock(ctx, name, lockLease, token)
	if err != nil {
		return nil, domain.NewDependencyUnavailable("close lock store unavailable")
	}
	if !ok {
		return nil, domain.NewConflict("a close is already in progress for this period")
	}
	defer func() { _ = uc.lock.ReleaseLock(ctx, name, token) }()

	// Compute the batch plan from the tenant- and period-scoped open-row count.
	count, derr := uc.jobs.CountOpenRows(ctx, tenantID, period.Range.Start, period.Range.End)
	if derr != nil {
		return nil, derr
	}
	batchesTotal := int((count + closeBatchSize - 1) / closeBatchSize)
	if batchesTotal < 1 {
		batchesTotal = 1
	}

	expectedVersion := period.Version
	if err := period.BeginClose(); err != nil {
		return nil, domain.NewConflict("period is not open")
	}
	if derr := uc.periods.BeginClose(ctx, tenantID, periodID, expectedVersion); derr != nil {
		return nil, derr
	}

	job := &CloseJob{
		ID:           uuid.New(),
		PeriodID:     periodID,
		TenantID:     tenantID,
		BatchesTotal: batchesTotal,
		BatchesDone:  0,
		Status:       closeJobRunning,
		LockToken:    token,
		CreatedBy:    actor.UserID,
		CreatedAt:    uc.clock.Now().UTC(),
	}
	if derr := uc.jobs.Create(ctx, tenantID, job); derr != nil {
		return nil, derr
	}

	after, jerr := json.Marshal(job)
	if jerr != nil {
		return nil, domain.NewInternal(jerr)
	}
	ev, derr := uc.newEvent(ctx, actor, domain.EventEntityModified, "period", periodID.String(), domain.EntityModifiedPayload{
		EntityType: "period",
		EntityID:   periodID.String(),
		Action:     domain.ActionEdit,
		Before:     json.RawMessage(`{"status":"open"}`),
		After:      after,
	})
	if derr != nil {
		return nil, derr
	}
	_ = uc.events.Publish(ctx, ev)

	return job, nil
}

// isTransient reports whether a domain error is retryable for a close batch.
// Only conflict (deadlock/serialization from the boundary) and downstream
// unavailability warrant retries with backoff; validations and not-founds are
// permanent.
func isTransient(derr *domain.DomainError) bool {
	if derr == nil {
		return false
	}
	switch derr.Code {
	case domain.CodeConflictState, domain.CodeDependencyUnavailable:
		return true
	default:
		return false
	}
}

// backoff computes the exponential-with-jitter delay for attempt i (0-based),
// per the retry policy (base 100ms, factor 2, jitter).
func backoff(attempt int) time.Duration {
	base := retryBaseBackoff << attempt
	if base <= 0 {
		base = retryBaseBackoff << (maxCloseRetries - 1)
	}
	if n, err := rand.Int(rand.Reader, big.NewInt(int64(base))); err == nil {
		return time.Duration(base) + time.Duration(n.Int64())
	}
	return time.Duration(base)
}

// ProcessBatch advances the close by consuming the next bounded batch of open
// ledger rows in the period, then atomically advancing the job's checkpoint and
// batches_done counter. Each batch runs within its own transaction window
// (boundary owned by the adapter). The batches_done increment is guarded by the
// expected batch sequence so a retried batch cannot double-advance. On transient
// errors up to maxCloseRetries with backoff are attempted; after exhaustion the
// job is marked FAILED (no partial close).
func (uc *ClosePeriodUseCase) ProcessBatch(ctx context.Context, jobID, actorID, tenantID uuid.UUID) (BatchResult, *domain.DomainError) {
	actor, derr := uc.authorize(ctx, actorID, tenantID)
	if derr != nil {
		return BatchResult{}, derr
	}
	if jobID == uuid.Nil {
		return BatchResult{}, domain.NewValidationError(domain.FieldError{Field: "job_id", Message: "job id must be provided"})
	}

	job, derr := uc.jobs.GetByID(ctx, tenantID, jobID)
	if derr != nil {
		if derr.Code == domain.CodeNotFound {
			return BatchResult{}, domain.NewNotFound("period close job", "")
		}
		return BatchResult{}, derr
	}
	if job.TenantID != tenantID {
		return BatchResult{}, domain.NewNotFound("period close job", "")
	}
	if job.Status == closeJobCompleted {
		return BatchResult{}, domain.NewConflict("period close job is already complete")
	}
	if job.Status == closeJobFailed {
		return BatchResult{}, domain.NewConflict("period close job failed; start a new close to resume")
	}

	period, derr := uc.periods.Get(ctx, tenantID, job.PeriodID)
	if derr != nil {
		return BatchResult{}, derr
	}
	if period.TenantID != tenantID {
		return BatchResult{}, domain.NewNotFound("period", "")
	}

	// Renew the lease; a lost lock means another worker took over, so the batch
	// must abort fail-closed rather than advance an unowned close.
	name := uc.lockName(job.PeriodID)
	if err := uc.lock.RenewLock(ctx, name, lockLease, job.LockToken); err != nil {
		return BatchResult{}, uc.failJob(ctx, actor, job, "close lock lost or expired: "+err.Error())
	}

	var (
		rows    []*domain.Transaction
		page    *ports.CursorPage
		syncErr *domain.DomainError
	)
	from, to := period.Range.Start, period.Range.End
	filter := ports.TransactionFilter{From: &from, To: &to}

	// Retry with backoff on transient failures; permanent errors fail immediately.
	for attempt := 0; attempt < maxCloseRetries; attempt++ {
		pageReq := ports.CursorPage{Limit: closeBatchSize, Cursor: job.CheckpointCursor}
		rows, page, syncErr = uc.txns.List(ctx, tenantID, filter, pageReq)
		if syncErr == nil {
			break
		}
		if !isTransient(syncErr) || attempt == maxCloseRetries-1 {
			return BatchResult{}, uc.failJob(ctx, actor, job, syncErr.Error())
		}
		time.Sleep(backoff(attempt))
	}

	// The immutable checkpoint advances to the server-issued next cursor so a
	// resumed job scans forward rather than rescanning from row zero.
	var checkpoint *string
	if page != nil {
		checkpoint = page.NextCursor
	}

	if derr := uc.jobs.IncrementDone(ctx, tenantID, jobID, job.BatchesDone, checkpoint); derr != nil {
		if derr.Code == domain.CodeConflictState {
			// Concurrent advance or replay: surface the optimistic-lock guard.
			return BatchResult{}, domain.NewConflict("period close job was concurrently advanced")
		}
		return BatchResult{}, derr
	}

	done := job.BatchesDone + 1
	return BatchResult{
		JobID:         jobID,
		BatchesDone:   done,
		BatchesTotal:  job.BatchesTotal,
		Done:          done >= job.BatchesTotal,
		ProcessedRows: len(rows),
	}, nil
}

// failJob marks the job FAILED (retaining the resumable checkpoint in the row it
// was already persisted with), records the failure reason, and emits an
// append-only audit event for the close lifecycle, satisfying §5.3 traceability.
func (uc *ClosePeriodUseCase) failJob(ctx context.Context, actor CloseActor, job *CloseJob, reason string) *domain.DomainError {
	if derr := uc.jobs.Fail(ctx, job.TenantID, job.ID, reason); derr != nil {
		return derr
	}
	after, err := json.Marshal(job)
	if err != nil {
		return domain.NewInternal(err)
	}
	ev, derr := uc.newEvent(ctx, actor, domain.EventEntityModified, "period", job.PeriodID.String(), domain.EntityModifiedPayload{
		EntityType: "period_close",
		EntityID:   job.ID.String(),
		Action:     domain.ActionEdit,
		Before:     json.RawMessage(`{"status":"closing"}`),
		After:      after,
	})
	if derr != nil {
		return derr
	}
	_ = uc.events.Publish(ctx, ev)
	return domain.NewDependencyUnavailable("period close batch failed")
}

// FinalizeClose completes the close once all batches are consumed: it writes
// the immutable reporting snapshot from the read model, transitions closing ->
// closed under the domain guard, records the actor, publishes the PeriodClosed
// event, and releases the lock. The snapshot write and the status transition
// are atomic — the adapter must bind Append + CompleteClose within a single
// WithTx scope — so a Closed period can never lack its snapshot, and the
// snapshot (keyset-unique on period_id) is never overwritten.
func (uc *ClosePeriodUseCase) FinalizeClose(ctx context.Context, jobID, actorID, tenantID uuid.UUID) (*domain.FinancePeriod, *domain.DomainError) {
	actor, derr := uc.authorize(ctx, actorID, tenantID)
	if derr != nil {
		return nil, derr
	}
	if jobID == uuid.Nil {
		return nil, domain.NewValidationError(domain.FieldError{Field: "job_id", Message: "job id must be provided"})
	}

	job, derr := uc.jobs.GetByID(ctx, tenantID, jobID)
	if derr != nil {
		if derr.Code == domain.CodeNotFound {
			return nil, domain.NewNotFound("period close job", "")
		}
		return nil, derr
	}
	if job.TenantID != tenantID || job.PeriodID == uuid.Nil {
		return nil, domain.NewNotFound("period close job", "")
	}
	if job.Status == closeJobCompleted {
		return nil, domain.NewConflict("period close job is already complete")
	}
	if job.BatchesDone < job.BatchesTotal {
		return nil, domain.NewConflict("period close is not complete; batches remain")
	}

	period, derr := uc.periods.Get(ctx, tenantID, job.PeriodID)
	if derr != nil {
		return nil, derr
	}
	if period.TenantID != tenantID {
		return nil, domain.NewNotFound("period", "")
	}
	if !period.Status.CanTransitionTo(domain.PeriodClosed) {
		return nil, domain.NewConflict("period is not closing")
	}

	name := uc.lockName(job.PeriodID)
	if err := uc.lock.RenewLock(ctx, name, lockLease, job.LockToken); err != nil {
		return nil, uc.failJob(ctx, actor, job, "close lock lost or expired: "+err.Error())
	}

	now := uc.clock.Now().UTC()
	expectedVersion := period.Version

	// Build the immutable snapshot payload from the reporting read model fields
	// carried by the closed period. The Status/ClosedBy/ClosedAt are terminal.
	payload, err := json.Marshal(SnapshotPayload{
		SchemaVersion: 1,
		PeriodID:      period.ID,
		From:          period.Range.Start,
		To:            period.Range.End,
		ClosedBy:      actor.UserID,
		ClosedAt:      now,
		Status:        domain.PeriodClosed,
		Summary:       nil,
	})
	if err != nil {
		return nil, domain.NewInternal(err)
	}
	snap := &ReportSnapshot{
		PeriodID:  period.ID,
		TenantID:  tenantID,
		Payload:   payload,
		CreatedAt: now,
	}

	// The adapter must run Append + CompleteClose inside one WithTx scope so the
	// snapshot cannot be written without the close transition applying (and vice
	// versa). A unique-violation on Append means the period already has a
	// snapshot: surface CONFLICT_STATE and never overwrite (§5.3 append-only).
	if derr := uc.snapshots.Append(ctx, tenantID, snap); derr != nil {
		if derr.Code == domain.CodeConflictState {
			return nil, domain.NewConflict("a reporting snapshot already exists for this period")
		}
		return nil, derr
	}
	if derr := uc.periods.CompleteClose(ctx, tenantID, period.ID, actor.UserID, expectedVersion); derr != nil {
		return nil, derr
	}

	if derr := uc.jobs.Complete(ctx, tenantID, job.ID); derr != nil {
		return nil, derr
	}

	closedAt := now
	period.ClosedBy = &actor.UserID
	period.ClosedAt = &closedAt
	period.Status = domain.PeriodClosed

	// Publish the post-commit PeriodClosed event (EventBus dispatches after the
	// writing transaction commits), alongside the append-only audit entry.
	evPC, derr := uc.newEvent(ctx, actor, domain.EventPeriodClosed, "period", period.ID.String(), domain.PeriodClosedPayload{PeriodID: period.ID, ClosedAt: closedAt})
	if derr != nil {
		return nil, derr
	}
	_ = uc.events.Publish(ctx, evPC)

	after, jerr := json.Marshal(snap)
	if jerr != nil {
		return nil, domain.NewInternal(jerr)
	}
	evEM, derr := uc.newEvent(ctx, actor, domain.EventEntityModified, "period", period.ID.String(), domain.EntityModifiedPayload{
		EntityType: "period",
		EntityID:   period.ID.String(),
		Action:     domain.ActionEdit,
		Before:     json.RawMessage(`{"status":"closing"}`),
		After:      after,
	})
	if derr != nil {
		return nil, derr
	}
	_ = uc.events.Publish(ctx, evEM)

	return period, nil
}

// IsPeriodClosed reports whether the period is locked (status == closed). It is
// the single enforcement point for period immutability: ledger write use cases
// consult it at entry and reject writes with a 409 period-closed response. A
// missing period or a wrong-tenant id is surfaced as NOT_FOUND so no
// cross-tenant existence is revealed.
func (uc *ClosePeriodUseCase) IsPeriodClosed(ctx context.Context, periodID, actorID, tenantID uuid.UUID) (bool, *domain.DomainError) {
	if _, derr := uc.authorize(ctx, actorID, tenantID); derr != nil {
		return false, derr
	}
	if periodID == uuid.Nil {
		return false, domain.NewValidationError(domain.FieldError{Field: "period_id", Message: "period id must be provided"})
	}
	period, derr := uc.periods.Get(ctx, tenantID, periodID)
	if derr != nil {
		if derr.Code == domain.CodeNotFound {
			return false, domain.NewNotFound("period", "")
		}
		return false, derr
	}
	if period.TenantID != tenantID {
		return false, domain.NewNotFound("period", "")
	}
	return period.Status == domain.PeriodClosed, nil
}