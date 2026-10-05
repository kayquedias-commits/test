// Package export is the Frameworks & Drivers adapter for asynchronous data
// exports (Blueprint §7.4/§7.5/§9.4, Module H). It consumes export jobs via a
// bulkheaded connection pool, renders CSV/PDF through the ports.ExportGateway
// seam, streams to object storage, and persists final job states.
//
// NOTE ON A MISSING PORT: the Change Description types the worker against
// application/ports.ExportJobRepository, but that interface does not yet exist
// in ports/repositories.go (only Account/Transaction/Category/Budget/User/
// Audit/ReadModel/Certification/FinancePeriod repositories are declared there).
// The canonical contract and a domain.ExportJob entity are assumed to arrive
// with the export_jobs persistence layer. Until then, this package declares the
// export-specific job repository contract (ExportJobRepository) and the job
// record (ExportJob) package-locally, mirroring the exact signatures in the
// change description, so the export_usecases/cmd/worker that depend on this
// file see a stable seam. Migrating to the canonical ports.ExportJobRepository
// + domain.ExportJob is a follow-up: delete the local types and retype Worker.
package export

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/infrastructure/observability"
)

// Job-state vocabulary backing the export_jobs.status CHECK constraint. The
// canonical constants live with the persistence layer; these mirror them here.
const (
	statusPending   = "PENDING"
	statusRunning   = "RUNNING"
	statusRetryable = "RETRYABLE"
	statusReady     = "READY"
	statusFailed    = "FAILED"
)

// permissionScopeKey is the RBAC capability a job must retain at render time
// for the worker to process it. Absence is a permanent, forbidden outcome.
const permissionScopeKey = "export:create"

// Object-key extensions are allowlisted; never derived from input.
const (
	csvExtension = "csv"
	pdfExtension = "pdf"
)

// Defaults and bounds for worker fan-out, retry, and chunking (§7.7, §9.4).
const (
	defaultWorkers   = 4
	maxRetries       = 5
	chunkSizeDefault = 1000
	maxBackoff       = 60 * time.Second
	baseBackoff      = 100 * time.Millisecond
	chunkQueryTimout = 30 * time.Second
)

var (
	// errNoEligibleJob signals an empty claim scan; the loop backs off briefly.
	errNoEligibleJob = errors.New("no eligible export job")
	// errPermissionForbidden marks a job whose recorded permission snapshot no
	// longer grants export:create — permanent, never rendered.
	errPermissionForbidden = errors.New("export:create permission revoked")
	// errInvalidDateRange marks a corrupted/impossible job date range — caught
	// at render entry before any data is read or rendered.
	errInvalidDateRange = errors.New("export date-range validation failed")
)

// hundred is the cents-to-currency divisor (two-decimal precision, §7.3).
var hundred = decimal.NewFromInt(100)

// ExportPermissionScope is the creation-time snapshot of the export capability
// and tenant predicate. The worker enforces this captured scope and never
// performs a live permission lookup (TOCTOU widening guard, §7.8).
type ExportPermissionScope struct {
	// Permissions is a capability allowlist; export:create must be present.
	Permissions []string
	TenantID    uuid.UUID
}

// ExportJob is the entity the worker consumes. It mirrors domain.ExportJob (a
// follow-up once the domain entity exists).
type ExportJob struct {
	ID              uuid.UUID
	TenantID        uuid.UUID
	UserID          uuid.UUID
	Format          string // "csv" | "pdf"
	DateRangeStart  time.Time
	DateRangeEnd    time.Time
	PermissionScope ExportPermissionScope
	RetryCount      int
}

// ExportJobRepository is the persistence seam for the export-job state machine
// (PENDING → RUNNING → READY|FAILED; RETRYABLE → RUNNING), guarded by the
// repository. This is a package-local stand-in for the missing
// ports.ExportJobRepository (see package doc for the migration note).
type ExportJobRepository interface {
	// ClaimNext atomically claims the oldest eligible job:
	//   SELECT ... FROM export_jobs
	//   WHERE status IN ($1,$2) AND (next_retry_at IS NULL OR next_retry_at<=now())
	//   ORDER BY created_at ASC LIMIT 1 FOR UPDATE SKIP LOCKED;
	// followed by the guarded UPDATE ... SET status='RUNNING', claimed_by=$x,
	// version=version+1 WHERE id=$y AND status IN ('PENDING','RETRYABLE') in the
	// same transaction. Returns errNoEligibleJob when nothing is claimable.
	ClaimNext(ctx context.Context, statuses ...string) (*ExportJob, error)
	// MarkRunning persists the PENDING/RETRYABLE → RUNNING transition.
	MarkRunning(ctx context.Context, job *ExportJob) error
	// MarkReady persists RUNNING → READY with the object key, size, and checksum
	// (integrity verification, §9.4).
	MarkReady(ctx context.Context, jobID uuid.UUID, objectKey string, size int64, checksumSHA256 string) error
	// MarkRetryable persists RUNNING → RETRYABLE, incrementing retry_count and
	// gating the next claim on next_retry_at = now()+backoff.
	MarkRetryable(ctx context.Context, jobID uuid.UUID, errMsg string, backoff time.Duration, retryCount int) error
	// MarkFailed persists RUNNING → FAILED with a sanitized error code.
	MarkFailed(ctx context.Context, jobID uuid.UUID, errMsg string) error
}

// fmtLayout is the ISO date format used in CSV cells.
const fmtLayout = "2006-01-02"

// headers are the read-model CSV column headers (rendered once per export).
var headers = []string{
	"transaction_id", "transaction_date", "amount_cents", "description",
	"category_id", "category_name", "account_id", "account_name",
}

// exportRow is one denormalized read-model row fetched per chunk.
type exportRow struct {
	TransactionID uuid.UUID
	Date          time.Time
	AmountCents   int64
	Description   string
	CategoryID    uuid.UUID
	CategoryName  string
	AccountID     uuid.UUID
	AccountName   string
}

// periodAgg is a per-month accumulator for the PDF period series.
type periodAgg struct {
	Start   time.Time
	End     time.Time
	Income  int64
	Expense int64
}

// Worker consumes export jobs. It owns a dedicated bulkheaded pgxpool (hard
// cap wired at the composition root, entirely separate from postgres.go's
// interactive pool) so large exports can never exhaust interactive-query
// connections (§7.7). All writes to export-job state go through the repository.
type Worker struct {
	gateway    ports.ExportGateway
	jobs       ExportJobRepository
	log        *observability.Logger
	metrics    *observability.Metrics
	maxRetries int
	chunkSize  int
	pool       *pgxpool.Pool // bulkhead pool; nil in unit-only contexts

	wg   sync.WaitGroup
	stop context.CancelFunc
}

// NewWorker builds a worker with the bounded retry budget (5) and 1,000-row
// chunk size mandated by §7.7/§9.4.
func NewWorker(gateway ports.ExportGateway, jobs ExportJobRepository, pool *pgxpool.Pool, log *observability.Logger, metrics *observability.Metrics) *Worker {
	return &Worker{
		gateway:    gateway,
		jobs:       jobs,
		log:        log,
		metrics:    metrics,
		maxRetries: maxRetries,
		chunkSize:  chunkSizeDefault,
		pool:       pool,
	}
}

// Run fans out `workers` goroutines that repeatedly claim and process export
// jobs until ctx is cancelled. A workers value ≤ 0 falls back to EXPORT_WORKERS
// (default 4) and is clamped to the bulkhead pool cap so goroutines never
// contend past available pool connections. It blocks until Shutdown cancels ctx.
func (w *Worker) Run(ctx context.Context, workers int) error {
	if workers <= 0 {
		workers = exportWorkersFromEnv()
	}
	if w.pool != nil {
		if cap := int(w.pool.Stat().MaxConns()); cap > 0 && workers > cap {
			workers = cap
		}
	}
	if workers <= 0 {
		workers = defaultWorkers
	}

	wctx, cancel := context.WithCancel(ctx)
	w.stop = cancel
	defer cancel()

	w.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go w.workerLoop(wctx)
	}
	<-wctx.Done()
	w.wg.Wait()
	return nil
}

// workerLoop is one fan-out goroutine: claim the next eligible job and process
// it. Backs off briefly when the queue is empty and on transient claim failure
// so the loop does not spin. Pool utilization and queue depth are recorded
// after each processed job.
func (w *Worker) workerLoop(ctx context.Context) {
	defer w.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		job, err := w.jobs.ClaimNext(ctx, statusPending, statusRetryable)
		if err != nil {
			if errors.Is(err, errNoEligibleJob) {
				w.sleep(ctx, 500*time.Millisecond)
				continue
			}
			w.log.Warn("export claim failed", "error", sanitizeErrorText(err))
			w.metrics.IncError("DEPENDENCY_UNAVAILABLE")
			w.sleep(ctx, time.Second)
			continue
		}

		_ = w.ProcessExport(ctx, job)

		if w.pool != nil {
			w.metrics.SetPoolUsage("export_worker", float64(w.pool.Stat().AcquiredConns()))
			w.metrics.SetQueueDepth("export_jobs", float64(w.queueDepth(ctx)))
		}
	}
}

// sleep honours ctx cancellation while pausing.
func (w *Worker) sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// ProcessExport drives one job end-to-end: re-validates the date range,
// re-checks the recorded permission snapshot (never rendering on a forbidden
// scope), marks RUNNING, streams the read-model dataset in bounded chunks,
// renders the object, and persists READY. Failures within the bounded retry
// budget become RETRYABLE; permanent or final failures become FAILED.
func (w *Worker) ProcessExport(ctx context.Context, job *ExportJob) error {
	log := w.log.WithCorrelation(job.ID.String()).With(
		"job_id", job.ID.String(), "tenant_id", job.TenantID.String(), "user_id", job.UserID.String())

	// Re-validate DateRange (start ≤ end, sane window). Corrupted job metadata
	// must never reach a query or renderer.
	if !validDateRange(job) {
		w.handleError(ctx, job, errInvalidDateRange)
		log.Error("export rejected: invalid date range", "error", "VALIDATION_FAILED")
		return errInvalidDateRange
	}

	// Permission snapshot re-check BEFORE any data read or render (TOCTOU guard).
	if !hasExportPermission(job) {
		w.handleError(ctx, job, errPermissionForbidden)
		log.Error("export rejected: permission scope revoked", "error", "FORBIDDEN")
		return errPermissionForbidden
	}

	ext, ok := extensionFor(job.Format)
	if !ok {
		w.handleError(ctx, job, fmt.Errorf("unsupported export format %q", job.Format))
		return fmt.Errorf("unsupported export format %q", job.Format)
	}

	if err := w.jobs.MarkRunning(ctx, job); err != nil {
		log.Error("export failed to mark running", "error", sanitizeErrorText(err))
		return err
	}
	start := time.Now()

	// Chunked read-model streaming over the bulkhead pool (keyset pagination;
	// one chunk ≤ chunkSize materialized at a time).
	var buf bytes.Buffer
	hash := sha256.New()
	mw := io.MultiWriter(&buf, hash)
	var (
		totalIncomeCents, totalExpenseCents int64
		periods                             = map[string]*periodAgg{}
		cursorDate                          = time.Time{}
		cursorID                            = ""
		done                                = false
		firstCSV                            = true
	)

	for !done {
		chunk, qerr := w.queryChunk(ctx, job, cursorDate, cursorID)
		if qerr != nil {
			w.handleError(ctx, job, qerr)
			log.Warn("export chunk query failed", "error", sanitizeErrorText(qerr))
			return qerr
		}

		if len(chunk) == 0 {
			done = true
			continue
		}

		rows := make([][]string, 0, len(chunk))
		for _, r := range chunk {
			rows = append(rows, []string{
				r.TransactionID.String(),
				r.Date.Format(fmtLayout),
				strconv.FormatInt(r.AmountCents, 10),
				r.Description,
				r.CategoryID.String(),
				r.CategoryName,
				r.AccountID.String(),
				r.AccountName,
			})
			cursorDate = r.Date
			cursorID = r.TransactionID.String()

			switch {
			case r.AmountCents < 0:
				totalExpenseCents += -r.AmountCents
			default:
				totalIncomeCents += r.AmountCents
			}
			key := r.Date.Format("2006-01")
			p, ok := periods[key]
			if !ok {
				ms := monthStart(r.Date)
				p = &periodAgg{Start: ms, End: monthEnd(ms)}
				periods[key] = p
			}
			if r.AmountCents < 0 {
				p.Expense += -r.AmountCents
			} else {
				p.Income += r.AmountCents
			}
		}

		if job.Format == string(ports.FormatCSV) {
			hdrs := headers
			if !firstCSV {
				hdrs = nil // subsequent chunks emit data lines only, no repeated header
			}
			b, rerr := w.gateway.RenderCSV(ctx, rows, hdrs)
			if rerr != nil {
				w.handleError(ctx, job, rerr)
				log.Warn("export CSV render failed", "error", sanitizeErrorText(rerr))
				return rerr
			}
			if _, werr := mw.Write(b); werr != nil {
				w.handleError(ctx, job, werr)
				return werr
			}
			firstCSV = false
		}

		if len(chunk) < w.chunkSize {
			done = true
		}
	}

	// PDF renders once from the fully aggregated model (the gateway's RenderPDF
	// returns a self-contained binary per call, so paged-chunk byte-append is
	// not valid; bounded chunk queries still bound per-batch memory).
	if job.Format == string(ports.FormatPDF) {
		report := buildReportModel(job, totalIncomeCents, totalExpenseCents, periods)
		pdf, rerr := w.gateway.RenderPDF(ctx, report)
		if rerr != nil {
			w.handleError(ctx, job, rerr)
			log.Warn("export PDF render failed", "error", sanitizeErrorText(rerr))
			return rerr
		}
		if _, werr := mw.Write(pdf); werr != nil {
			w.handleError(ctx, job, werr)
			return werr
		}
	}

	objectKey := "exports/" + job.TenantID.String() + "/" + job.ID.String() + "." + ext
	if uerr := w.gateway.Upload(ctx, objectKey, buf.Bytes()); uerr != nil {
		w.handleError(ctx, job, uerr)
		log.Warn("export upload failed", "error", sanitizeErrorText(uerr))
		return uerr
	}

	size := int64(buf.Len())
	checksum := hex.EncodeToString(hash.Sum(nil))
	if rerr := w.jobs.MarkReady(ctx, job.ID, objectKey, size, checksum); rerr != nil {
		log.Error("export failed to mark ready", "error", sanitizeErrorText(rerr))
		return rerr
	}

	log.Info("export complete",
		"format", job.Format,
		"object_key", objectKey,
		"bytes_uploaded", size,
		"checksum_sha256", checksum,
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return nil
}

// Shutdown drains in-flight jobs before worker termination. It cancels the run
// context, then waits for the fan-out goroutines, honouring the passed context
// deadline so a stuck render cannot hang termination.
func (w *Worker) Shutdown(ctx context.Context) error {
	if w.stop != nil {
		w.stop()
	}
	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// handleError translates a processing failure into the exact status transition.
// Permanent errors (forbidden scope, invalid range) are FAILED immediately;
// transient errors retry while within the bounded budget, then FAIL with the
// backoff window gated by the repository's next_retry_at.
func (w *Worker) handleError(ctx context.Context, job *ExportJob, cause error) {
	code := exportErrorCode(cause)
	if code != "INTERNAL_ERROR" {
		_ = w.jobs.MarkFailed(ctx, job.ID, code)
		w.metrics.IncError(code)
		return
	}
	if job.RetryCount < w.maxRetries {
		next := job.RetryCount + 1
		backoff := retryBackoff(next)
		_ = w.jobs.MarkRetryable(ctx, job.ID, code, backoff, next)
		w.log.Warn("export job scheduled for retry",
			"job_id", job.ID.String(), "retry_count", next, "backoff_ms", backoff.Milliseconds())
		w.metrics.IncError("DEPENDENCY_UNAVAILABLE")
		return
	}
	_ = w.jobs.MarkFailed(ctx, job.ID, code)
	w.metrics.IncError("INTERNAL_ERROR")
}

// queryChunk fetches one ≤chunkSize slice of the read-model dataset for the
// job's tenant and date range, keyset-paginated on (transaction_date, id) so no
// row is skipped or duplicated across chunk boundaries. Fully parameterized
// (SQL-injection safe). A nil pool (unit-only context) yields an empty dataset —
// the Empty-module boundary, never an error.
func (w *Worker) queryChunk(ctx context.Context, job *ExportJob, cursorDate time.Time, cursorID string) ([]exportRow, error) {
	if w.pool == nil {
		return nil, nil
	}
	qctx, cancel := context.WithTimeout(ctx, chunkQueryTimout)
	defer cancel()

	rows, err := w.pool.Query(qctx, chunkSelectSQL,
		job.TenantID, job.DateRangeStart, job.DateRangeEnd, cursorDate, cursorID, w.chunkSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]exportRow, 0, w.chunkSize)
	for rows.Next() {
		var r exportRow
		if serr := rows.Scan(&r.TransactionID, &r.Date, &r.AmountCents, &r.Description,
			&r.CategoryID, &r.CategoryName, &r.AccountID, &r.AccountName); serr != nil {
			return nil, serr
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// queueDepth reports the number of immediately claimable jobs (PENDING or
// RETRYABLE past their backoff gate) on the bulkhead pool.
func (w *Worker) queueDepth(ctx context.Context) int64 {
	if w.pool == nil {
		return 0
	}
	var n int64
	if err := w.pool.QueryRow(ctx, queueDepthSQL).Scan(&n); err != nil {
		return 0
	}
	return n
}

// validDateRange re-validates the job's span at render entry (start ≤ end and
// within a sane window). Corrupted metadata is rejected before any query.
func validDateRange(job *ExportJob) bool {
	return !job.DateRangeStart.IsZero() && !job.DateRangeEnd.IsZero() &&
		!job.DateRangeEnd.Before(job.DateRangeStart) &&
		job.DateRangeStart.Year() >= 1900 &&
		job.DateRangeEnd.Before(time.Now().Add(24*time.Hour))
}

// hasExportPermission reports whether the creation-time snapshot still grants
// export:create — the capability gate never widened by a live lookup.
func hasExportPermission(job *ExportJob) bool {
	for _, p := range job.PermissionScope.Permissions {
		if p == permissionScopeKey {
			return true
		}
	}
	return false
}

// extensionFor maps an allowlisted format to an object-key extension.
func extensionFor(format string) (string, bool) {
	switch format {
	case string(ports.FormatCSV):
		return csvExtension, true
	case string(ports.FormatPDF):
		return pdfExtension, true
	default:
		return "", false
	}
}

// buildReportModel assembles the PDF ReportModel from the aggregated chunk
// totals and per-month series. An empty dataset yields a zero Summary and empty
// series — the Empty-module boundary, never an error.
func buildReportModel(job *ExportJob, incomeCents, expenseCents int64, periods map[string]*periodAgg) ports.ReportModel {
	keys := make([]string, 0, len(periods))
	for k := range periods {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	series := make([]ports.ReportPeriodPoint, 0, len(keys))
	for _, k := range keys {
		p := periods[k]
		series = append(series, ports.ReportPeriodPoint{
			PeriodStart: p.Start,
			PeriodEnd:   p.End,
			Income:      centsToDecimal(p.Income),
			Expense:     centsToDecimal(p.Expense),
			Net:         centsToDecimal(p.Income - p.Expense),
		})
	}

	return ports.ReportModel{
		Summary: ports.ReportSummary{
			TotalIncome:    centsToDecimal(incomeCents),
			TotalExpense:   centsToDecimal(expenseCents),
			NetAmount:      centsToDecimal(incomeCents - expenseCents),
			DateRangeStart: job.DateRangeStart,
			DateRangeEnd:   job.DateRangeEnd,
		},
		PeriodSeries: series,
	}
}

func centsToDecimal(cents int64) decimal.Decimal {
	return decimal.NewFromInt(cents).Div(hundred)
}

func monthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
}

func monthEnd(start time.Time) time.Time {
	return start.AddDate(0, 1, 0).Add(-time.Nanosecond)
}

// retryBackoff returns the exponential backoff for the next attempt
// (base 100 ms, factor 2, ±25% jitter, capped at 60 s). Jitter prevents a
// thundering herd of workers retrying concurrently (§9.4, CWE-799).
func retryBackoff(next int) time.Duration {
	if next < 1 {
		next = 1
	}
	exp := next
	if exp > 9 {
		exp = 9
	}
	d := baseBackoff * time.Duration(1<<exp)
	if d > maxBackoff {
		d = maxBackoff
	}
	jitter := time.Duration(rand.Int63n(int64(d/4)))
	return d + jitter
}

// exportErrorCode maps a cause to a canonical §7.7 code (message stored on the
// job and surfaced to the status endpoint). Never a stack trace.
func exportErrorCode(err error) string {
	switch {
	case errors.Is(err, errPermissionForbidden):
		return "FORBIDDEN"
	case errors.Is(err, errInvalidDateRange):
		return "VALIDATION_FAILED"
	default:
		return "INTERNAL_ERROR"
	}
}

// exportWorkersFromEnv reads EXPORT_WORKERS (default 4), ignoring non-numeric
// values. A non-positive value still resolves to the default.
func exportWorkersFromEnv() int {
	if v := os.Getenv("EXPORT_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultWorkers
}

// sanitizeErrorText caps length and strips control characters so no internal or
// credential-bearing detail reaches logs or job records verbatim.
func sanitizeErrorText(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 512 {
		s = s[:512]
	}
	s = strings.ToValidUTF8(s, "")
	replacer := strings.NewReplacer("\r", "", "\n", "", "\t", "")
	return replacer.Replace(s)
}

// SQL statements executed against the bulkhead pool. Chunk and queue-depth
// reads run here; the claim SELECT ... FOR UPDATE SKIP LOCKED lives inside a
// future ExportJobRepository implementation (see package doc note) so this
// worker stays decoupled from persistence schema.
var (
	chunkSelectSQL = `
SELECT transaction_id, transaction_date, amount_cents, description,
       category_id, category_name, account_id, account_name
FROM transaction_read_model
WHERE tenant_id = $1
  AND transaction_date BETWEEN $2 AND $3
  AND (transaction_date, transaction_id) > ($4, $5)
ORDER BY transaction_date, transaction_id
LIMIT $6`

	queueDepthSQL = `
SELECT COUNT(*) FROM export_jobs
WHERE status IN ('PENDING','RETRYABLE')
  AND (next_retry_at IS NULL OR next_retry_at <= now())`
)