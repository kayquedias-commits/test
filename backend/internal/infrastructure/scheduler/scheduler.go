// Package scheduler provides the cron-driven batch worker (Blueprint §7.5):
// daily balance snapshots, next-month partition creation, audit archival,
// backup triggers, and the distributed-lock-guarded month-end close. It is a
// temporal driver at the interface-adapter layer; it performs no business
// logic itself and delegates every state transition to the approved use-case
// services (ClosePeriodUseCase, AuditService) and the rediscache distributed
// lock.
//
// Concurrency and availability semantics:
//   - Every job runs under a fencing-token lock (TryLock/ReleaseLock compare
//     the token atomically in Lua), so duplicate worker replicas never
//     double-run a job. Long jobs renew the lease on a background ticker and
//     abort fail-closed if the token is lost (rediscache.ErrLockLost).
//   - Every job body is idempotent: snapshot upserts keyed by (account, day),
//     partition creation is IF NOT EXISTS, archival only stamps archived_at,
//     the backup trigger uses an idempotent day marker key, and the month-end
//     close is bounded by the ClosePeriodUseCase state machine plus a
//     per-period fencing lock.
//   - Job Runnables are wrapped in a panic handler so a fault in one goroutine
//     can never take down the worker process (spec §9.3 99.9% availability).
package scheduler

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/robfig/cron/v3"

	"fms/backend/internal/application/usecases"
	"fms/backend/internal/config"
	"fms/backend/internal/infrastructure/audit"
	"fms/backend/internal/infrastructure/cache"
	"fms/backend/internal/infrastructure/observability"
	"fms/backend/internal/infrastructure/persistence/postgres"
)

// Scheduling and resilience constants.
const (
	// Job schedules (5-field standard cron; parser has no seconds field).
	specBalanceSnapshot  = "5 0 * * *" // daily 00:05
	specCreatePartitions = "0 1 * * *" // daily 01:00
	specAuditArchival    = "0 2 * * *" // daily 02:00
	specBackupTrigger    = "0 3 * * *" // daily 03:00
	defaultCloseSchedule = "0 4 1 * *" // day 1 of month at 04:00

	// lockLease is the distributed-lock TTL for short jobs; a background renew
	// ticker refreshes it at lease/3 so the holder outlives a single batch.
	lockLease = 30 * time.Second
	// closeLease is the per-period close lock TTL; it is renewed between
	// batches inside driveClose, mirroring the use case's own lease behaviour.
	closeLease = 90 * time.Second

	// Per-job deadlines bound runaway work off the request path (CWE-400).
	defaultJobTimeout = 30 * time.Minute
	closeJobTimeout   = 6 * time.Hour

	// Transient-failure retry budget (§7.7: 3 attempts, base 100 ms, factor 2).
	maxTransientRetries = 3
	retryBaseBackoff    = 100 * time.Millisecond
)

// backupHostAllowlist is the closed set of backup-trigger target hosts. The
// trigger URL is looked up from configuration only and validated against this
// allowlist plus a mandatory https scheme, so a compromised value cannot pivot
// into internal network targets (SSRF, OWASP A10).
var backupHostAllowlist = map[string]struct{}{
	"backup.internal.fms.local": {},
}

// Job is a single scheduled unit of work. Spec is a standard 5-field cron
// expression parsed at construction. Run executes the job body; the scheduler
// wraps it in lock acquisition, panic recovery, and a deadline context.
type Job struct {
	// Name is the stable job identifier used in logs, lock keys and metrics.
	Name string
	// Spec is the 5-field cron schedule (parsed eagerly, fail-fast).
	Spec string
	// Run is the job body. It must be safe to re-run; scheduling failures are
	// retried on the next cycle, never double-effected.
	Run func(context.Context) error
	// OncePerDay, when true, consumes a day-keyed marker so a restart or an
	// overlapping replica does not re-run the job within the same UTC day.
	OncePerDay bool
}

// Scheduler is the cron driver. It owns the ticker lifecycle, per-job
// distributed locks with fencing tokens, panic recovery, and graceful drain.
type Scheduler struct {
	lock        *cache.Client
	jobs        []Job
	log         *observability.Logger
	metrics     *observability.Metrics
	cfg         *config.Config
	closePeriod *usecases.ClosePeriodUseCase
	auditSvc    *usecases.AuditService
	archiver    *audit.Archiver
	db          *pgxpool.Pool
	closeActor  uuid.UUID

	parser cron.Parser
	mu     sync.Mutex
	crons  []*cron.Cron
	wg     sync.WaitGroup
}

// NewScheduler validates configuration and every cron spec (fail fast) and
// wires the five scheduled jobs. month-close is driven by closePeriod; audit
// archival by auditSvc; and the batch SQL (balance snapshot, partition
// creation, tenant enumeration) runs off db. A nil lock handle is rejected
// because distributed locking is required for the 99.9% availability target.
//
// It returns an error (not a bare *Scheduler) because malformed cron specs and
// invalid runtime config must abort the worker at startup rather than silently
// run a wrong schedule. The system actor that drives closes is resolved from
// FMS_CLOSE_ACTOR_ID and must satisfy the use case's close permission.
func NewScheduler(
	lock *cache.Client,
	closePeriod *usecases.ClosePeriodUseCase,
	auditSvc *usecases.AuditService,
	archiver *audit.Archiver,
	db *pgxpool.Pool,
	log *observability.Logger,
	metrics *observability.Metrics,
	cfg *config.Config,
) (*Scheduler, error) {
	if lock == nil {
		return nil, errors.New("scheduler: a distributed lock handle is required")
	}
	if db == nil {
		return nil, errors.New("scheduler: a database pool is required")
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	actor, err := parseActor(os.Getenv("FMS_CLOSE_ACTOR_ID"))
	if err != nil {
		return nil, err
	}

	closeSchedule := os.Getenv("FMS_CLOSE_SCHEDULE")
	if strings.TrimSpace(closeSchedule) == "" {
		closeSchedule = defaultCloseSchedule
	}

	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	specs := []struct{ name, spec string }{
		{"balance-snapshot", specBalanceSnapshot},
		{"create-partitions", specCreatePartitions},
		{"audit-archival", specAuditArchival},
		{"backup-trigger", specBackupTrigger},
		{"month-end-close", closeSchedule},
	}
	for _, sp := range specs {
		if _, perr := parser.Parse(sp.spec); perr != nil {
			return nil, fmt.Errorf("scheduler: invalid cron spec for %s (%q): %w", sp.name, sp.spec, perr)
		}
	}

	s := &Scheduler{
		lock:        lock,
		log:         log,
		metrics:     metrics,
		cfg:         cfg,
		closePeriod: closePeriod,
		auditSvc:    auditSvc,
		archiver:    archiver,
		db:          db,
		closeActor:  actor,
		parser:      parser,
	}
	s.jobs = []Job{
		{Name: "balance-snapshot", Spec: specBalanceSnapshot, Run: s.runSnapshotJob, OncePerDay: true},
		{Name: "create-partitions", Spec: specCreatePartitions, Run: s.runPartitionsJob, OncePerDay: true},
		{Name: "audit-archival", Spec: specAuditArchival, Run: s.runAuditJob, OncePerDay: true},
		{Name: "backup-trigger", Spec: specBackupTrigger, Run: s.runBackupJob, OncePerDay: true},
		{Name: "month-end-close", Spec: closeSchedule, Run: s.runCloseJob, OncePerDay: false},
	}
	return s, nil
}

// validateConfig fail-closes on schedule-relevant configuration. The retention
// window must be non-negative and not absurdly small (which would archive live
// entries).
func validateConfig(cfg *config.Config) error {
	if cfg == nil {
		return errors.New("scheduler: config is required")
	}
	if cfg.Audit.Retention < 0 {
		return errors.New("scheduler: FMS_AUDIT_RETENTION must not be negative")
	}
	if cfg.Audit.Retention > 0 && cfg.Audit.Retention < time.Hour {
		return errors.New("scheduler: FMS_AUDIT_RETENTION is too small to be safe")
	}
	return nil
}

// parseActor resolves the worker's service principal that drives closes. It is
// configuration, never caller input, and must be a valid UUID that satisfies
// the ClosePeriodUseCase close permission at runtime.
func parseActor(s string) (uuid.UUID, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return uuid.Nil, errors.New("scheduler: FMS_CLOSE_ACTOR_ID is required for the month-end-close job")
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, errors.New("scheduler: FMS_CLOSE_ACTOR_ID must be a valid UUID")
	}
	return id, nil
}

// Start validates and starts each job ticker. Starred crons run on their own
// goroutines; per-run work is marshalled through the distributed lock.
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.crons) > 0 {
		return nil
	}
	for _, j := range s.jobs {
		c := cron.New(cron.WithParser(s.parser), cron.WithChain())
		if _, err := c.AddFunc(j.Spec, func(j Job) func() {
			return func() { s.handle(ctx, j) }
		}(j)); err != nil {
			return err
		}
		c.Start()
		s.crons = append(s.crons, c)
	}
	return nil
}

// Shutdown cancels in-flight work and drains the ticker goroutines within the
// supplied context deadline. It never returns before a bounded drain completes.
func (s *Scheduler) Shutdown(ctx context.Context) {
	s.mu.Lock()
	crons := s.crons
	s.crons = nil
	s.mu.Unlock()

	for _, c := range crons {
		c.Stop()
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	if s.log != nil {
		s.log.Flush()
	}
}

// handle is the per-tick wrapper: panic recovery, per-day fire-once gating, a
// per-job deadline, distributed-lock acquisition, and failure accounting. A
// fault in the job body is logged and counted but never panics the worker.
func (s *Scheduler) handle(ctx context.Context, j Job) {
	s.wg.Add(1)
	defer s.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			s.recordFailure(j.Name, fmt.Errorf("scheduler panic: %v", r))
		}
	}()

	log := s.logFrom(ctx)
	if log != nil {
		log = log.With("job", j.Name)
	}
	if j.OncePerDay && !s.mayRunOnce(ctx, j.Name) {
		if log != nil {
			log.Debug("job already ran for the UTC day; skipping")
		}
		return
	}

	runCtx, cancel := context.WithTimeout(ctx, s.jobTimeout(j.Name))
	defer cancel()

	// The job-level lock guarantees a single leader runs the job even when
	// multiple worker replicas tick simultaneously.
	err := s.runLocked(runCtx, "job:"+j.Name, lockLease, func(rctx context.Context, _ string) error {
		return j.Run(rctx)
	})
	if err != nil {
		s.recordFailure(j.Name, err)
		if log != nil {
			log.Error("scheduler job failed", "error", err.Error())
		}
		return
	}
	if log != nil {
		log.Info("scheduler job completed")
	}
}

// logFrom returns a context-bearing logger, defaulting to the package logger
// when ctx carries none, or nil when the scheduler was built without a logger.
func (s *Scheduler) logFrom(ctx context.Context) *observability.Logger {
	if s.log == nil {
		return nil
	}
	if ctx == nil {
		return s.log.FromContext(context.Background())
	}
	return s.log.FromContext(ctx)
}

// jobTimeout returns the per-run deadline: long for the close (many batches),
// bounded for the short daily jobs.
func (s *Scheduler) jobTimeout(name string) time.Duration {
	if name == "month-end-close" {
		return closeJobTimeout
	}
	return defaultJobTimeout
}

// mayRunOnce consumes a day-keyed marker so a daily job runs once per UTC day
// even across a restart. Idempotent jobs make an error here safe to fail open.
func (s *Scheduler) mayRunOnce(ctx context.Context, name string) bool {
	day := time.Now().UTC().Format("2006-01-02")
	ok, err := s.lock.TryLock(ctx, "once:"+name+":"+day, 24*time.Hour, s.newToken())
	if err != nil {
		return true
	}
	return ok
}

// runLocked acquires the distributed lock {name} with a fresh crypto-random
// fencing token, runs the body under a renewing lease, and compare-and-
// releases the lock. When another replica holds the lock the run is skipped
// (no duplicate execution). A lost lease cancels the running body so the stale
// holder never writes after ownership transfers (CWE-667).
func (s *Scheduler) runLocked(ctx context.Context, name string, ttl time.Duration, run func(context.Context, string) error) error {
	token := s.newToken()
	acquired, err := s.lock.TryLock(ctx, name, ttl, token)
	if err != nil {
		return err
	}
	if !acquired {
		if lg := s.logFrom(ctx); lg != nil {
			lg.Info("scheduler job skipped; lock held by another replica", "lock", name)
		}
		return nil
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	renewDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(ttl / 3)
		defer ticker.Stop()
		for {
			select {
			case <-renewDone:
				return
			case <-ticker.C:
				if rerr := s.lock.RenewLock(ctx, name, ttl, token); rerr != nil {
					// Lock lost: cancel the body so a stale holder aborts.
					if lg := s.logFrom(ctx); lg != nil {
						lg.Error("scheduler lock lost; aborting job", "lock", name)
					}
					cancel()
					return
				}
			}
		}
	}()

	runErr := run(runCtx, token)
	close(renewDone)

	// Compare-and-delete: only the current token holder releases. A lost lock
	// on release is expected and not an error worth surfacing.
	if relErr := s.lock.ReleaseLock(context.Background(), name, token); relErr != nil && !errors.Is(relErr, cache.ErrLockLost) {
		if lg := s.logFrom(ctx); lg != nil {
			lg.Warn("scheduler lock release failed", "lock", name)
		}
	}
	return runErr
}

// newToken returns a high-entropy v4 UUID fencing token from crypto/rand (see
// security analysis: tokens must never be math/rand generated). It is never
// placed in logs, metrics, or audit fields.
func (s *Scheduler) newToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	u, err := uuid.FromBytes(b[:])
	if err != nil {
		return ""
	}
	return u.String()
}

// recordFailure accounts for a job fault on the closed vocabulary (metrics
// reject unknown labels) and logs the error class without echoing the cause's
// internals.
func (s *Scheduler) recordFailure(name string, err error) {
	if s.metrics != nil {
		s.metrics.ObserveBackup("daily-full", "failure")
		s.metrics.IncError("INTERNAL_ERROR")
	}
	if s.log != nil {
		s.log.Error("scheduler job failure", "job", name)
	}
}

// ---------------------------------------------------------------------------
// Job bodies
// ---------------------------------------------------------------------------

// runSnapshotJob drives the daily balance-snapshot materialization with
// transient-failure retry.
func (s *Scheduler) runSnapshotJob(ctx context.Context) error {
	return s.retry(ctx, func() error { return s.balanceSnapshot(ctx) })
}

// runPartitionsJob creates the next-month partition; the DDL is idempotent, so
// a transient retry is safe.
func (s *Scheduler) runPartitionsJob(ctx context.Context) error {
	return s.retry(ctx, func() error { return s.createPartitions(ctx) })
}

// runAuditJob applies the retention policy per tenant. The archival adapter
// owns its own bounded retry and batching, so no scheduler-level retry is added.
func (s *Scheduler) runAuditJob(ctx context.Context) error {
	if s.auditSvc == nil {
		if lg := s.logFrom(ctx); lg != nil {
			lg.Warn("audit service not wired; audit archival skipped", "job", "audit-archival")
		}
		return nil
	}
	return s.auditArchival(ctx)
}

// runBackupJob fires the daily backup trigger; the HTTP call retries internally.
func (s *Scheduler) runBackupJob(ctx context.Context) error {
	return s.backupTrigger(ctx)
}

// runCloseJob drives the monthly month-end close state machine.
func (s *Scheduler) runCloseJob(ctx context.Context) error {
	if s.closePeriod == nil {
		if lg := s.logFrom(ctx); lg != nil {
			lg.Warn("close use case not wired; month-end-close skipped", "job", "month-end-close")
		}
		return nil
	}
	return s.retry(ctx, func() error { return s.monthEndClose(ctx) })
}

// balanceSnapshot upserts, per tenant, the ledger balance of every account as
// of today into account_balance_snapshots. The read is a single aggregate
// GROUP BY (no N+1), the write is a natural-key upsert keyed by
// (tenant_id, account_id, snapshot_date) so re-runs never drift, and each
// tenant's transaction commits independently (bounded WAL growth).
func (s *Scheduler) balanceSnapshot(ctx context.Context) error {
	return s.forEachTenant(ctx, func(ctx context.Context, tenant uuid.UUID) error {
		day := time.Now().UTC().Format("2006-01-02")
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()

		// Re-establish the tenant inside the transaction so RLS applies.
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1::text, true)", tenant); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO account_balance_snapshots (tenant_id, account_id, balance, snapshot_date)
			SELECT a.tenant_id, a.id,
			       a.opening_balance + COALESCE(SUM(t.amount), 0),
			       $2::date
			  FROM accounts a
			  LEFT JOIN transactions t
			         ON t.account_id = a.id
			        AND t.tenant_id = a.tenant_id
			        AND t.date <= $2::date
			 WHERE a.tenant_id = $1
			 GROUP BY a.tenant_id, a.id, a.opening_balance
			ON CONFLICT (tenant_id, account_id, snapshot_date)
			DO UPDATE SET balance = EXCLUDED.balance, created_at = now()`,
			tenant, day); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
}

// createPartitions creates the next calendar month's range partition on the
// transactions table, one month ahead, using a deterministic name and IF NOT
// EXISTS so it is a pure no-op on re-run. Partition naming must mirror the
// baseline migration's transactions_YYYY_MM convention.
func (s *Scheduler) createPartitions(ctx context.Context) error {
	const ddl = `
		DO $$
		DECLARE nm_start date; nm_end date;
		BEGIN
		  nm_start := date_trunc('month', now() + interval '1 month')::date;
		  nm_end   := (date_trunc('month', now() + interval '2 months'))::date;
		  EXECUTE format('CREATE TABLE IF NOT EXISTS transactions_%s PARTITION OF transactions
		                 FOR VALUES FROM (%L) TO (%L)',
		                 to_char(nm_start, 'YYYY_MM'), nm_start, nm_end);
		END $$;`
	_, err := s.db.Exec(ctx, ddl)
	return err
}

// auditArchival applies the retention policy per tenant. The use case archives
// (never deletes) aging entries and marks them archived once object-store
// upload and chain verification succeed; cross-tenant isolation is enforced by
// passing the loop's tenant into every call.
func (s *Scheduler) auditArchival(ctx context.Context) error {
	return s.forEachTenant(ctx, func(ctx context.Context, tenant uuid.UUID) error {
		res, err := s.auditSvc.ApplyRetentionPolicy(ctx, tenant)
		if err != nil {
			return err
		}
		if lg := s.logFrom(ctx); lg != nil {
			lg.Info("audit retention sweep completed",
				"tenant", tenant, "archived", res.ArchivedCount, "failed_batches", res.FailedBatches)
		}
		return nil
	})
}

// backupTrigger fires the external backup contract. The target URL is read from
// configuration (FMS_BACKUP_TRIGGER_URL) and validated against an https+host
// allowlist; on validation failure the job fails closed and never proceeds. The
// POST carries an idempotent day marker so two replicas cannot double-fire.
// When no URL is configured the job no-ops (external adapter disabled).
func (s *Scheduler) backupTrigger(ctx context.Context) error {
	raw := os.Getenv("FMS_BACKUP_TRIGGER_URL")
	if strings.TrimSpace(raw) == "" {
		if lg := s.logFrom(ctx); lg != nil {
			lg.Info("backup trigger not configured; skipping", "job", "backup-trigger")
		}
		return nil
	}
	target, err := validateBackupURL(raw)
	if err != nil {
		s.recordFailure("backup-trigger", err)
		return err
	}
	idemKey := "backup/trigger/" + time.Now().UTC().Format("2006-01-02")
	status, err := s.postBackup(ctx, target, idemKey)
	if err != nil {
		s.recordFailure("backup-trigger", err)
		return err
	}
	if s.metrics != nil {
		s.metrics.ObserveBackup("daily-full", "success")
	}
	if lg := s.logFrom(ctx); lg != nil {
		lg.Info("backup trigger dispatched", "job", "backup-trigger", "status", status)
	}
	return nil
}

// validateBackupURL enforces the SSRF guard: https scheme only, no IP literal,
// host from the closed allowlist. Any violation fails closed. The raw value is
// never logged.
func validateBackupURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("backup trigger URL is invalid")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, errors.New("backup trigger URL must use https")
	}
	host := strings.ToLower(u.Hostname())
	if net.ParseIP(host) != nil {
		return nil, errors.New("backup trigger URL must not be an IP literal")
	}
	if _, ok := backupHostAllowlist[host]; !ok {
		return nil, errors.New("backup trigger URL host is not allowlisted")
	}
	return u, nil
}

// postBackup POSTs the trigger with an idempotency header, retrying 5xx/network
// errors on the §7.7 budget. The request shares the caller's deadline context.
func (s *Scheduler) postBackup(ctx context.Context, target *url.URL, idemKey string) (int, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	var lastStatus int
	for attempt := 0; attempt < maxTransientRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(backoff(attempt - 1)):
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Idempotency-Key", idemKey)
		resp, doErr := client.Do(req)
		if doErr != nil {
			lastStatus = 0
			continue
		}
		lastStatus = resp.StatusCode
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return lastStatus, nil
		case resp.StatusCode < 500:
			return lastStatus, fmt.Errorf("backup trigger returned %d", resp.StatusCode)
		default:
			// 5xx: fall through to retry under the §7.7 budget.
		}
	}
	return lastStatus, fmt.Errorf("backup trigger failed after %d attempts", maxTransientRetries)
}

// monthEndClose drives the month-end close per open period, per tenant, each
// under a tenant-scoped distributed lock. State-transition authority lives
// entirely in ClosePeriodUseCase (StartClose/ProcessBatch/FinalizeClose/
// IsPeriodClosed); the scheduler renews its own outer lease and the use case
// renews its period lock between batches, both token-guarded.
func (s *Scheduler) monthEndClose(parent context.Context) error {
	now := time.Now().UTC()
	return s.forEachTenant(parent, func(ctx context.Context, tenant uuid.UUID) error {
		rows, err := s.db.Query(ctx, `
			SELECT id FROM periods
			 WHERE tenant_id = $1
			   AND status = 'OPEN'
			   AND end_date < $2
			 ORDER BY end_date`, tenant, now)
		if err != nil {
			return err
		}
		var periodIDs []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			periodIDs = append(periodIDs, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		for _, periodID := range periodIDs {
			name := s.periodLockKey(tenant, periodID)
			if err := s.runLocked(ctx, name, closeLease, func(rctx context.Context, token string) error {
				return s.driveClose(rctx, tenant, periodID, token)
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// periodLockKey scopes the close lock by tenant and period so cross-tenant
// operations never share a lock domain (data-isolation invariant).
func (s *Scheduler) periodLockKey(tenant, periodID uuid.UUID) string {
	return "period_close:" + tenant.String() + ":" + periodID.String()
}

// driveClose advances one period through the close state machine: it verifies
// the period is not yet closed, opens the close, consumes batches (renewing the
// fencing lock before each batch and re-checking IsPeriodClosed), then
// finalizes. A lost lock surfaces as an error that aborts the run fail-closed.
func (s *Scheduler) driveClose(ctx context.Context, tenant, periodID uuid.UUID, token string) error {
	closed, derr := s.closePeriod.IsPeriodClosed(ctx, periodID, s.closeActor, tenant)
	if derr != nil {
		return derr
	}
	if closed {
		return nil
	}
	if _, derr := s.closePeriod.StartClose(ctx, periodID, s.closeActor, tenant); derr != nil {
		return derr
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		closed, derr := s.closePeriod.IsPeriodClosed(ctx, periodID, s.closeActor, tenant)
		if derr != nil {
			return derr
		}
		if closed {
			break
		}
		// Renew the scheduler's outer lease between batches; the use case renews
		// its own period lock inside ProcessBatch. A lost token aborts.
		if rerr := s.lock.RenewLock(ctx, s.periodLockKey(tenant, periodID), closeLease, token); rerr != nil {
			return rerr
		}
		job, gerr := s.currentJobID(ctx, tenant, periodID)
		if gerr != nil {
			return gerr
		}
		res, derr := s.closePeriod.ProcessBatch(ctx, job, s.closeActor, tenant)
		if derr != nil {
			return derr
		}
		if res.Done {
			if _, derr := s.closePeriod.FinalizeClose(ctx, job, s.closeActor, tenant); derr != nil {
				return derr
			}
			break
		}
	}
	return nil
}

// currentJobID resolves the active close job id for a period. StartClose above
// created it; reading it back keeps the scheduler resilient to an intermediate
// restart while staying within the confirmed use-case surface.
func (s *Scheduler) currentJobID(ctx context.Context, tenant, periodID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.db.QueryRow(ctx, `
		SELECT id FROM period_close
		 WHERE tenant_id = $1 AND period_id = $2 AND status = 'RUNNING'
		 ORDER BY created_at DESC LIMIT 1`, tenant, periodID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, errors.New("scheduler: no running close job for period")
	}
	return id, err
}

// forEachTenant enumerates every tenant and invokes fn with a tenant-scoped
// context, so every job's repository call carries the explicit tenant predicate
// derived from the loop (isolation is never read from shared context).
func (s *Scheduler) forEachTenant(ctx context.Context, fn func(context.Context, uuid.UUID) error) error {
	rows, err := s.db.Query(ctx, "SELECT id FROM tenants ORDER BY id")
	if err != nil {
		return err
	}
	var tenants []uuid.UUID
	for rows.Next() {
		var t uuid.UUID
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return err
		}
		tenants = append(tenants, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range tenants {
		if err := fn(postgres.WithTenant(ctx, t), t); err != nil {
			return err
		}
	}
	return nil
}

// retry runs fn under the §7.7 transient-failure budget, backing off
// exponentially with jitter and abstaining on permanent errors.
func (s *Scheduler) retry(ctx context.Context, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt < maxTransientRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(backoff(attempt - 1)):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if !isTransient(lastErr) {
			return lastErr
		}
	}
	return lastErr
}

// backoff returns base << n plus jitter, both derived so transient retries do
// not thundering-herd across replicas.
func backoff(n int) time.Duration {
	base := retryBaseBackoff << n
	j, err := rand.Int(rand.Reader, big.NewInt(int64(base)))
	if err != nil || base <= 0 {
		return base
	}
	return base + time.Duration(j.Int64())
}

// isTransient reports whether an error is worth retrying under the §7.7 budget.
// Only transient PostgreSQL connection/session codes are retried; all other
// classes fail immediately to avoid masking permanent errors.
func isTransient(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "08000", "08003", "08006", "57P01", "57P02", "57P03", "53300":
			return true
		default:
			return false
		}
	}
	return false
}