package usecases

import (
	"context"
	"time"

	"github.com/google/uuid"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/domain"
	"fms/backend/internal/infrastructure/audit"
)

// Retention policy and query bounds for the audit service. Batch size matches
// the archiver's memory ceiling (1,000 entries/batch) and the Query page size
// respects the store's clamp of [1,200] (Change Description §7.5/§7.2).
const (
	retentionBatchSize = 1000
	defaultQueryLimit  = 100
	// maxQueryWindow guards against a full-table scan driven by an unbounded
	// from/to range (CWE-20 / CWE-770).
	maxQueryWindow = 10 * 365 * 24 * time.Hour
	// maxGetByEntityPages bounds the lineage walk so a long-lived entity cannot
	// force an unbounded iteration over the store.
	maxGetByEntityPages = 10000
)

// auditEntityTypeAllowlist is the closed set of entity kinds an audit filter
// may select. Unknown values are rejected rather than treated as no-ops so a
// caller-supplied type can never silently broaden the query (CWE-20).
var auditEntityTypeAllowlist = map[string]struct{}{
	"account":     {},
	"transaction": {},
	"category":    {},
	"budget":      {},
	"user":        {},
	"role":        {},
}

// auditLogRepository is the output port for the append-only audit store. It is
// declared here (mirroring the CertificationRepository convention) and is
// satisfied structurally by *audit.Store. It exposes only Query, PendingArchival,
// and MarkArchived — no Update/Delete path — so the append-only invariant is
// enforced at the persistence boundary. Every method takes the tenant predicate
// explicitly so isolation is enforced in the query, never filtered in memory.
type auditLogRepository interface {
	// Query returns the tenant-scoped entries matching filter in ascending seq
	// order, with a keyset cursor (last returned seq, or nil when exhausted).
	Query(ctx context.Context, tenantID uuid.UUID, filter audit.AuditFilter, limit int, cursor *int64) ([]audit.Entry, *int64, *domain.DomainError)
	// PendingArchival returns the oldest un-archived entries at or before cutoff.
	PendingArchival(ctx context.Context, tenantID uuid.UUID, cutoff time.Time, limit int) ([]audit.Entry, *domain.DomainError)
	// MarkArchived stamps archived_at on un-archived rows whose seq is in ids,
	// within its own transaction, and returns the count affected.
	MarkArchived(ctx context.Context, tenantID uuid.UUID, ids []int64) (int64, *domain.DomainError)
}

// auditArchiver is the output port for the archival adapter. ArchiveBatch
// verifies the batch's chained-hash continuity and uploads object storage
// atomically (no object written on any mismatch/failure). It is implemented by
// *audit.Archiver.
type auditArchiver interface {
	// ArchiveBatch archives a single-tenant, seq-ordered batch and returns the
	// object keys written. Any hash mismatch or upload failure aborts the batch.
	ArchiveBatch(ctx context.Context, entries []audit.Entry) ([]string, error)
}

// AuditRetentionPolicy supplies the active-store lifetime after which entries
// are eligible for archival. The window is configuration, never hardcoded, and
// is consumed by ApplyRetentionPolicy.
type AuditRetentionPolicy interface {
	// RetentionWindow returns how long entries stay in the active store before
	// archival. A negative window is rejected by the use case (fail closed).
	RetentionWindow() time.Duration
}

// RetentionResult reports the outcome of a retention sweep. Partial success is
// not an error: FailedBatches > 0 returns a non-nil result so the caller can
// schedule a retry via NextRunHint.
type RetentionResult struct {
	// ArchivedCount is the number of entries marked archived in the run.
	ArchivedCount int
	// FailedBatches is the number of batches that aborted and must be re-picked.
	FailedBatches int
	// NextRunHint suggests when to retry: +1h when failures occurred, else next day.
	NextRunHint time.Time
}

// AuditService implements the Module G audit use cases: permission-scoped query
// and lineage reads, and configurable retention/archival policy. It depends only
// on output ports and domain types (no framework/ORM surface). It never deletes
// or edits an entry — retention only archives and marks archived — preserving
// the append-only audit invariant (§5.3). Audit write capture is a separate
// event-consumer path.
type AuditService struct {
	repo        auditLogRepository
	archiver    auditArchiver
	now         ports.Clock
	policy      AuditRetentionPolicy
	readLogging bool // injected AuditReadLoggingEnabled gate (§9.4)
}

// NewAuditService wires the audit use cases to their output ports. readLogging
// gates whether ActionRead filters are honored — it is injected as configuration
// (not read from a global) so the service stays testable and framework-isolated.
// It reflects the contested §9.4 decision at the capture boundary.
func NewAuditService(
	repo auditLogRepository,
	archiver auditArchiver,
	now ports.Clock,
	policy AuditRetentionPolicy,
	readLogging bool,
) *AuditService {
	return &AuditService{
		repo:        repo,
		archiver:    archiver,
		now:         now,
		policy:      policy,
		readLogging: readLogging,
	}
}

// Query returns the tenant-scoped audit entries matching filter, paged via a
// keyset cursor. Authorization is enforced at the use-case boundary: actor must
// belong to tenant and hold audit:read, else an explicit FORBIDDEN is returned
// (never a silently empty result). actor/tenant derive from the IAM-resolved
// principal, never from caller input (CWE-639). Results come back in the
// store's ascending seq order; a newest-first ("rows desc by timestamp") page
// would require a descending seq-keyset cursor in the store, which is out of
// scope for this change.
func (s *AuditService) Query(ctx context.Context, filter audit.AuditFilter, cursor *int64, tenant uuid.UUID, actor Actor) ([]audit.Entry, *int64, error) {
	if actor.TenantID != tenant || !actor.Role.Has(domain.PermissionAuditRead) {
		return nil, nil, domain.NewForbidden("access denied")
	}
	if err := validateAuditFilter(filter); err != nil {
		return nil, nil, err
	}
	// READ capture is flag-gated (§9.4): when logging is disabled, a read filter
	// matches nothing — an empty result, not an error, mirroring graceful
	// degradation.
	if filter.Action == domain.ActionRead && !s.readLogging {
		return []audit.Entry{}, nil, nil
	}
	entries, next, derr := s.repo.Query(ctx, tenant, filter, defaultQueryLimit, cursor)
	if derr != nil {
		return nil, nil, derr
	}
	if entries == nil {
		entries = []audit.Entry{}
	}
	return entries, next, nil
}

// GetByEntity returns the full modification lineage for a single entity within
// the tenant, oldest first, for auditor before/after diff rendering. The
// caller (interface adapter) is responsible for the audit:read permission
// check; tenant scoping is guaranteed here by passing tenant into every page
// so cross-tenant lineage can never be read (CWE-639).
func (s *AuditService) GetByEntity(ctx context.Context, entityType string, entityID uuid.UUID, tenant uuid.UUID) ([]audit.Entry, error) {
	if _, ok := auditEntityTypeAllowlist[entityType]; !ok {
		return nil, domain.NewValidationError(domain.FieldError{Field: "entity_type", Message: "unknown entity type"})
	}
	if entityID == uuid.Nil {
		return nil, domain.NewValidationError(domain.FieldError{Field: "entity_id", Message: "entity id is required"})
	}
	// The store scopes entity_id through its ILIKE search clause; a full-length
	// UUID string is an exact match within the store's 64-char search bound.
	filter := audit.AuditFilter{EntityType: entityType, Search: entityID.String()}
	out := make([]audit.Entry, 0)
	var cursor *int64
	for pages := 0; ; pages++ {
		page, next, derr := s.repo.Query(ctx, tenant, filter, defaultQueryLimit, cursor)
		if derr != nil {
			return nil, derr
		}
		out = append(out, page...)
		if next == nil {
			return out, nil
		}
		if pages >= maxGetByEntityPages {
			return nil, domain.NewInternal(nil)
		}
		cursor = next
	}
}

// ApplyRetentionPolicy archives every entry older than the configured retention
// window, in batches, and only marks them archived once the archiver confirms
// chain-hash integrity and uploads succeed. It never deletes. A per-batch
// failure — archival error or chain mismatch — aborts the batch without marking
// it, so the same rows are re-picked on the next run; the sweep otherwise
// continues to the next batch and success is reported without error. It runs
// daily via the scheduler, off the request path.
func (s *AuditService) ApplyRetentionPolicy(ctx context.Context, tenant uuid.UUID) (*RetentionResult, error) {
	if s.policy == nil {
		return nil, domain.NewInternal(nil)
	}
	window := s.policy.RetentionWindow()
	if window < 0 {
		return nil, domain.NewValidationError(domain.FieldError{Field: "retention_window", Message: "retention window must not be negative"})
	}
	now := s.now.Now().UTC()
	cutoff := now.Add(-window)
	res := &RetentionResult{NextRunHint: now.Add(24 * time.Hour)}

	for {
		batch, derr := s.repo.PendingArchival(ctx, tenant, cutoff, retentionBatchSize)
		if derr != nil {
			if res.ArchivedCount == 0 && res.FailedBatches == 0 {
				return nil, derr
			}
			res.FailedBatches++
			res.NextRunHint = now.Add(time.Hour)
			break
		}
		if len(batch) == 0 {
			break
		}

		// ArchiveBatch internally verifies chained-hash continuity and abort the
		// batch atomically on any mismatch or upload failure — nothing is marked,
		// so the DB and object store never diverge at the batch level.
		if _, aerr := s.archiver.ArchiveBatch(ctx, batch); aerr != nil {
			res.FailedBatches++
			res.NextRunHint = now.Add(time.Hour)
			break
		}

		ids := make([]int64, len(batch))
		for i, e := range batch {
			ids[i] = e.Seq
		}
		n, merr := s.repo.MarkArchived(ctx, tenant, ids)
		if merr != nil {
			res.FailedBatches++
			res.NextRunHint = now.Add(time.Hour)
			break
		}
		res.ArchivedCount += int(n)
	}

	return res, nil
}

// validateAuditFilter enforces the input boundaries shared by Query: an
// allowlisted entity type, a non-inverted and width-bounded time range, and a
// known action. The store additionally bounds and escapes the search term in
// its ILIKE query (parameterized) so injection is rejected at the persistence
// seam (CWE-89).
func validateAuditFilter(f audit.AuditFilter) error {
	if f.EntityType != "" {
		if _, ok := auditEntityTypeAllowlist[f.EntityType]; !ok {
			return domain.NewValidationError(domain.FieldError{Field: "entity_type", Message: "unknown entity type"})
		}
	}
	if f.From != nil && f.To != nil {
		if f.To.Before(*f.From) {
			return domain.NewValidationError(domain.FieldError{Field: "to", Message: "to must not be before from"})
		}
		if f.To.Sub(*f.From) > maxQueryWindow {
			return domain.NewValidationError(domain.FieldError{Field: "to", Message: "time range exceeds the maximum width"})
		}
	}
	if f.Action != "" && !f.Action.Valid() {
		return domain.NewValidationError(domain.FieldError{Field: "action", Message: "unknown audit action"})
	}
	return nil
}