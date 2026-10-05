// Package usecases implements the application use-case layer (specification
// §5.1) for Module H — Data Export. ExportService orchestrates the three export
// ports (ports.ExportGateway, the ExportJobRepository persistence seam, and
// ports.Clock) to create asynchronous export jobs, poll status, list exports,
// and issue short-lived download links.
//
// LAYERING NOTE (stand-ins): per the persist-layer note in
// infrastructure/export/worker.go, the canonical domain.ExportJob entity and
// ports.ExportJobRepository interface do not yet exist in this repository. This
// file therefore owns the application-layer ExportJob entity stand-in and the
// ExportJobRepository port, exactly mirroring the Change Description's method
// set. Migrating to the canonical domain entity + ports interface is a
// follow-up that replaces these types without changing the use-case logic.
//
// EVENT NOTE: the worker claims PENDING jobs via its own ClaimNext on the
// bulkhead pool, so ExportRequested is not raised here (there is no
// EventExportRequested domain type to publish, and the worker polls rather than
// subscribes). Completion notification through ports.Notifier is likewise not
// dispatched: the Notifier port's NotificationKind is a closed set of budget
// alert kinds, so no valid export-completion Notification exists to build
// without violating that closed-set contract. A future bus/outbox is the
// intended channel for ExportRequested/ExportReady/ExportFailed.
package usecases

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/domain"
)

// Download-link and resource-governance policies. The presigned URL TTL of 15
// minutes is the Blueprint §7.4 policy and is enforced by the use case via the
// Clock port (never accepted from the client).
const (
	downloadURLTTL       = 15 * time.Minute
	maxConcurrentExports = 5
	maxExportSpan        = 366 * 24 * time.Hour
)

// ExportStatus enumerates the export_jobs status CHECK-constraint vocabulary.
type ExportStatus string

const (
	ExportStatusPending ExportStatus = "PENDING"
	ExportStatusRunning ExportStatus = "RUNNING"
	ExportStatusReady   ExportStatus = "READY"
	ExportStatusFailed  ExportStatus = "FAILED"
)

// ExportScope is the allowlisted dataset a job may target. It mirrors the
// domain permission catalog: a job's scope must be present in the actor's
// permitted scopes or the request is forbidden (least privilege, §9.7).
type ExportScope string

const (
	ExportScopeTransactions ExportScope = "transactions"
	ExportScopeReports      ExportScope = "reports"
)

// ExportOptions is an immutable value object capturing the parameterization of
// an export job. It is deliberately typed (never a free-form map) so the DB-side
// JSONB stays tractable and, by construction, carries no field that could
// influence an object-store key (no path, filename, bucket, or URL fields) —
// object keys are derived server-side from tenant + job id only (security §3).
type ExportOptions struct {
	ReportType     string // profit_loss | expense_by_category | income_trend | cash_flow (required iff Scope=="reports")
	CategoryID     string
	AccountID      string
	Locale         string
	Timezone       string
	IncludeHeaders bool
	Delimiter      string // CSV only; allowlist {",", ";", "\t"}; empty defaults to ","
}

// validateFor enforces the report-format and CSV-delimiter allowlists at the
// boundary, and rejects a report type requested for a transactions scope (or
// a missing report type for a reports scope). Returns a VALIDATION_FAILED error
// aggregating field detail.
func (o ExportOptions) validateFor(scope ExportScope) *domain.DomainError {
	var fields []domain.FieldError
	switch scope {
	case ExportScopeReports:
		switch o.ReportType {
		case "profit_loss", "expense_by_category", "income_trend", "cash_flow":
		default:
			fields = append(fields, domain.FieldError{
				Field:   "options.report_type",
				Message: "must be one of profit_loss, expense_by_category, income_trend, cash_flow",
			})
		}
	case ExportScopeTransactions:
		if o.ReportType != "" {
			fields = append(fields, domain.FieldError{
				Field:   "options.report_type",
				Message: "report_type is only valid for a reports scope",
			})
		}
	}
	if o.Delimiter != "" {
		switch o.Delimiter {
		case ",", ";", "\t":
		default:
			fields = append(fields, domain.FieldError{
				Field:   "options.delimiter",
				Message: "must be one of , ; \\t",
			})
		}
	}
	if len(fields) > 0 {
		return domain.NewValidationError(fields...)
	}
	return nil
}

// CreateExportCommand is the application-layer input DTO for creating an async
// export job. Range==nil requests all permitted data; otherwise it must satisfy
// the domain.DateRange invariant (start <= end) and a bounded span. Options is
// validated against the requested Scope. Idempotency handling (duplicate 202
// submission mitigation via ports.IdempotencyStore, §7.2) is the HTTP adapter's
// concern and intentionally not modelled here.
type CreateExportCommand struct {
	Format  ports.ExportFormat
	Scope   ExportScope
	Range   *domain.DateRange
	Options ExportOptions
}

// PermissionSet is the pre-evaluated, server-side RBAC context applied by
// middleware. Permission policy is data, not code: the use case checks the
// capability set for export:create and intersects the requested/allowed scope,
// never branching on a hardcoded role name (§9.7). It must never be supplied by
// the client.
type PermissionSet struct {
	ActorID       uuid.UUID
	TenantID      uuid.UUID
	Permissions   []domain.Permission
	AllowedScopes []ExportScope
}

// Has reports whether the set grants exactly the given permission, matching by
// exact string equality and failing closed on an empty or unknown set.
func (p PermissionSet) Has(perm domain.Permission) bool {
	for _, held := range p.Permissions {
		if held == perm {
			return true
		}
	}
	return false
}

// AllowsScope reports whether the set permits the given dataset scope. An empty
// AllowedScopes denies everything (least privilege).
func (p PermissionSet) AllowsScope(s ExportScope) bool {
	for _, allowed := range p.AllowedScopes {
		if allowed == s {
			return true
		}
	}
	return false
}

// ExportJob is the export_jobs aggregate root (application-layer stand-in). It
// carries tenant and owner identity for row-level isolation, an immutable
// parameterization, and a validated lifecycle state. All state transitions flow
// through ExportService worker hooks; field writes never bypass the state
// guards here.
type ExportJob struct {
	ID           string // unguessable UUIDv4; never sequential (BOLA, security §1)
	TenantID     string
	OwnerID      string
	Format       ports.ExportFormat
	Scope        ExportScope
	DateRange    *domain.DateRange
	Options      ExportOptions
	Status       ExportStatus
	Progress     int // 0..100
	ObjectKey    string
	ErrorMessage string
	RetryCount   int
	Version      int
	CreatedAt    time.Time
	UpdatedAt    time.Time
	CompletedAt  *time.Time
}

// ExportJobFilter selects jobs in a tenant-scoped list by an optional status.
type ExportJobFilter struct {
	Status *ExportStatus
}

// ExportJobRepository is the persistence seam for the export-job state machine.
// Every method is tenancy-scoped — the tenant predicate is part of the WHERE
// clause, never a post-query filter — so no cross-tenant record can be observed
// or modified. GetByIDAndTenant returns a uniform NOT_FOUND for both a missing
// id and a valid id in another tenant (no existence probing). UpdateStatus uses
// an optimistic-lock/expected-version guard and returns CONFLICT_STATE on
// staleness, preventing two workers finalizing the same job (double completion).
// ListByTenant is a single LIMIT/OFFSET + COUNT(*) query (N+1 prevention).
// Create is idempotent per job ID with overwrite semantics so a retried create
// does not duplicate a job.
// The concrete adapter is the persist-layer follow-up (see package doc); all
// SQL is parameterized in that adapter — this interface never composes SQL.
type ExportJobRepository interface {
	Create(ctx context.Context, job *ExportJob) *domain.DomainError
	CountActive(ctx context.Context, tenantID string) (int, *domain.DomainError)
	GetByIDAndTenant(ctx context.Context, id, tenantID string) (*ExportJob, *domain.DomainError)
	UpdateStatus(ctx context.Context, job *ExportJob) *domain.DomainError
	ListByTenant(ctx context.Context, tenantID string, filter ExportJobFilter, page, pageSize int) ([]*ExportJob, int64, *domain.DomainError)
}

// ExportService orchestrates the Data-Export use cases. Constructor injection
// preserves hexagonal testability: gateway renders/uploads/signs object-store
// URLs, jobs is the persistence seam, notify is retained to match the spec's
// NewExportService shape (see package doc — it is not dispatched for export
// completion because the Notifier kind set is budget-only), and now is the
// Clock port that externalizes the 15-minute presigned-URL TTL.
type ExportService struct {
	gateway ports.ExportGateway
	jobs    ExportJobRepository
	notify  ports.Notifier
	clock   ports.Clock
}

// NewExportService builds the ExportService with its output ports and the Clock
// port that supplies the current instant.
func NewExportService(gateway ports.ExportGateway, jobs ExportJobRepository, notify ports.Notifier, now ports.Clock) *ExportService {
	return &ExportService{gateway: gateway, jobs: jobs, notify: notify, clock: now}
}

// CreateExport persists a PENDING export job after enforcing the export:create
// capability, the dataset-scope permission, and the allowlist validations for
// format, scope, range, and options. It enforces a per-tenant concurrent-job cap
// (RATE_LIMITED) to bound worker-queue flooding. The returned PENDING job lets
// the HTTP adapter respond 202 Accepted with a Location header; the worker
// claims and processes it asynchronously. No event is raised here (see package
// doc for the ExportRequested disposition).
func (s *ExportService) CreateExport(ctx context.Context, cmd CreateExportCommand, tenant string, perms PermissionSet) (*ExportJob, *domain.DomainError) {
	if !perms.Has(domain.PermissionExportCreate) || !perms.AllowsScope(cmd.Scope) {
		return nil, domain.NewForbidden("export permission denied")
	}

	switch cmd.Format {
	case ports.FormatCSV, ports.FormatPDF:
	default:
		return nil, domain.NewValidationError(domain.FieldError{
			Field:   "format",
			Message: "must be one of csv, pdf",
		})
	}
	switch cmd.Scope {
	case ExportScopeTransactions, ExportScopeReports:
	default:
		return nil, domain.NewValidationError(domain.FieldError{
			Field:   "scope",
			Message: "must be one of transactions, reports",
		})
	}

	if cmd.Range != nil {
		if !cmd.Range.Valid() {
			return nil, domain.NewValidationError(domain.FieldError{
				Field:   "range",
				Message: "start must not be after end",
			})
		}
		if cmd.Range.End.Sub(cmd.Range.Start) > maxExportSpan {
			return nil, domain.NewValidationError(domain.FieldError{
				Field:   "range",
				Message: "range must not exceed one fiscal year",
			})
		}
	}

	if derr := cmd.Options.validateFor(cmd.Scope); derr != nil {
		return nil, derr
	}

	active, derr := s.jobs.CountActive(ctx, tenant)
	if derr != nil {
		return nil, derr
	}
	if active >= maxConcurrentExports {
		return nil, domain.NewRateLimited(60)
	}

	now := s.clock.Now()
	job := &ExportJob{
		ID:        uuid.NewString(),
		TenantID:  tenant,
		OwnerID:   perms.ActorID.String(),
		Format:    cmd.Format,
		Scope:     cmd.Scope,
		DateRange: cmd.Range,
		Options:   cmd.Options,
		Status:    ExportStatusPending,
		Progress:  0,
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if derr := s.jobs.Create(ctx, job); derr != nil {
		return nil, derr
	}
	return job, nil
}

// GetExport loads a job by id + tenant for a status poll, re-verifying that the
// actor's permitted scopes cover the job's scope (function-level auth on the
// read path; see package doc — the catalog lacks a distinct export:read token,
// so the scope gate is the available read-path control). Returns uniform
// NOT_FOUND for missing or other-tenant jobs.
func (s *ExportService) GetExport(ctx context.Context, id, tenant string, perms PermissionSet) (*ExportJob, *domain.DomainError) {
	return s.loadForActor(ctx, id, tenant, perms)
}

// ListExports returns the tenant-scoped jobs for offset pagination (correct for
// the bounded export collection, §7.2) with the total count for the next-page
// link.
func (s *ExportService) ListExports(ctx context.Context, filter ExportJobFilter, tenant string, page, pageSize int) ([]*ExportJob, int64, *domain.DomainError) {
	items, total, derr := s.jobs.ListByTenant(ctx, tenant, filter, page, pageSize)
	if derr != nil {
		return nil, 0, derr
	}
	return items, total, nil
}

// GetDownloadURL issues the per-request 15-minute presigned download URL for a
// READY job, after re-verifying tenant ownership and the actor's scope. The URL
// is derived each request and never stored, cached, or logged; only object_key
// is persisted. Non-READY jobs are rejected (CONFLICT_STATE for in-flight,
// NOT_FOUND for failed), and an empty object_key on a READY job is a corruption
// state surfaced as INTERNAL_ERROR. The returned URL is the 302 redirect target.
func (s *ExportService) GetDownloadURL(ctx context.Context, id, tenant string, perms PermissionSet) (string, *domain.DomainError) {
	job, derr := s.loadForActor(ctx, id, tenant, perms)
	if derr != nil {
		return "", derr
	}

	switch job.Status {
	case ExportStatusReady:
		if job.ObjectKey == "" {
			return "", domain.NewInternal(nil)
		}
	case ExportStatusFailed:
		return "", domain.NewNotFound("export", id)
	case ExportStatusPending, ExportStatusRunning:
		return "", domain.NewConflict("export not ready")
	default:
		return "", domain.NewConflict("export not ready")
	}

	url, err := s.gateway.IssuePresignedURL(ctx, job.ObjectKey, downloadURLTTL)
	if err != nil {
		if de, ok := domain.AsDomainError(err); ok {
			return "", de
		}
		return "", domain.NewDependencyUnavailable("object store unavailable")
	}

	// Redirect-safety guard: never emit an arbitrary URL from the gateway. The
	// target must be https and carry no embedded credentials; a domain allowlist
	// for the object store is future hardening once configuration lands.
	if !safeRedirectURL(url) {
		return "", domain.NewForbidden("unsafe download target")
	}
	return url, nil
}

// MarkReady is a worker hook that persists a PENDING/RUNNING job as READY with
// its object key. It is internal and must never be exposed on a public router
// (a forged READY would enable unauthorized download). Final-state transition is
// validated here; the worker owns the retry budget and render-time permission
// resnapshot (both out of this file's scope). The optimistic version guard
// surfaces double-completion as CONFLICT_STATE.
func (s *ExportService) MarkReady(ctx context.Context, id, tenant, objectKey string) *domain.DomainError {
	job, derr := s.jobs.GetByIDAndTenant(ctx, id, tenant)
	if derr != nil {
		return derr
	}
	if !job.canTransitionToReady() {
		return domain.NewConflict("export cannot transition to ready")
	}
	now := s.clock.Now()
	job.Status = ExportStatusReady
	job.ObjectKey = objectKey
	job.Progress = 100
	job.CompletedAt = &now
	job.UpdatedAt = now
	return s.jobs.UpdateStatus(ctx, job)
}

// MarkFailed is a worker hook that persists a job as FAILED with a sanitized
// §7.7 error code. It is internal and never publicly reachable. The retry budget
// (≤5) is owned by the worker; this hook records the terminal state only.
func (s *ExportService) MarkFailed(ctx context.Context, id, tenant, code string) *domain.DomainError {
	job, derr := s.jobs.GetByIDAndTenant(ctx, id, tenant)
	if derr != nil {
		return derr
	}
	if !job.canTransitionToFailed() {
		return domain.NewConflict("export cannot transition to failed")
	}
	now := s.clock.Now()
	job.Status = ExportStatusFailed
	job.ErrorMessage = sanitizeCode(code)
	job.CompletedAt = &now
	job.UpdatedAt = now
	return s.jobs.UpdateStatus(ctx, job)
}

// canTransitionToReady permits PENDING and RUNNING (the worker may finalize a
// job it just claimed) and rejects any already-terminal job.
func (j *ExportJob) canTransitionToReady() bool {
	return j.Status == ExportStatusPending || j.Status == ExportStatusRunning
}

// canTransitionToFailed mirrors canTransitionToReady: a terminal job must not be
// re-failed, and PENDING/RUNNING jobs may fail.
func (j *ExportJob) canTransitionToFailed() bool {
	return j.Status == ExportStatusPending || j.Status == ExportStatusRunning
}

// loadForActor loads a job by id + tenant and re-verifies the actor's scope
// covers the job's scope. Tenant-scoped load yields a uniform NOT_FOUND for both
// missing and other-tenant ids (BOLA, security §1).
func (s *ExportService) loadForActor(ctx context.Context, id, tenant string, perms PermissionSet) (*ExportJob, *domain.DomainError) {
	job, derr := s.jobs.GetByIDAndTenant(ctx, id, tenant)
	if derr != nil {
		return nil, derr
	}
	if !perms.AllowsScope(job.Scope) {
		return nil, domain.NewForbidden("export permission denied")
	}
	return job, nil
}

// safeRedirectURL permits only an https redirect target with a host and no URL
// userinfo, so a misconfigured/compromised gateway cannot emit SSRF or
// open-redirect targets through the download endpoint.
func safeRedirectURL(raw string) bool {
	if len(raw) == 0 || len(raw) > 4096 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Scheme == "https" && u.Host != "" && u.User == nil
}

// sanitizeCode caps length and strips control characters so a worker-supplied
// error code cannot carry injection or credential-bearing detail into the job
// record or status response.
func sanitizeCode(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 64 {
		s = s[:64]
	}
	replacer := strings.NewReplacer("\r", "", "\n", "", "\t", "", "\x00", "")
	return replacer.Replace(s)
}