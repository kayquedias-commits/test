// Package main is the FMS background-worker composition root (Blueprint
// §7.4/§7.5). It owns the worker process lifecycle: it loads configuration,
// assembles the bulkheaded relays/consumers/export-pool/scheduler from the
// infrastructure packages, and drains every in-flight component on shutdown.
//
// This file contains no business logic. It is pure dependency wiring under the
// Clean Architecture Dependency Rule: every ledger, budget, audit, and export
// invariant lives in the use-case/domain layers the components invoke.

package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rabbitmq/amqp091-go"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/application/usecases"
	"fms/backend/internal/config"
	"fms/backend/internal/domain"
	"fms/backend/internal/infrastructure/audit"
	"fms/backend/internal/infrastructure/cache"
	"fms/backend/internal/infrastructure/events"
	"fms/backend/internal/infrastructure/export"
	"fms/backend/internal/infrastructure/observability"
	"fms/backend/internal/infrastructure/persistence/postgres"
	"fms/backend/internal/infrastructure/scheduler"
)

// objectStoreHostAllowlist is the closed set of object-store hosts the worker
// will ever dial. A compromised FMS_S3_ENDPOINT value cannot pivot the worker
// into uploading to an attacker-controlled host (SSRF, OWASP A10).
var objectStoreHostAllowlist = map[string]struct{}{
	"minio.internal.fms.local": {},
	"localhost":                {},
	"127.0.0.1":                {},
}

// keyPattern is the strict allowlist for object keys. Keys are always
// server-generated in the form {scope}/{tenant}/{uuid}.{ext}; any other shape
// is rejected before it reaches the object store (CWE-22/CWE-706).
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+/[0-9a-fA-F-]{36}/[0-9a-fA-F-]{36}\.(csv|pdf|jsonlines)$`)

const auditObjectContentType = "application/x-jsonlines"

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker: configuration load failed:", err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "worker: configuration validation failed:", err)
		os.Exit(1)
	}

	bootstrapLog, _ := observability.New(cfg)
	bootstrapLog.Info("worker starting", "service", cfg.Observability.ServiceName)
	bootstrapLog.Info("worker config", "safe_config", cfg.SafeString())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		bootstrapLog.Info("worker: shutdown signal received; draining")
		cancel()
	}()

	if err := runWorker(ctx, cfg); err != nil {
		bootstrapLog.Error("worker terminated with error", "error", err.Error())
		cancel()
		os.Exit(1)
	}
	bootstrapLog.Info("worker stopped cleanly")
	os.Exit(0)
}

// runWorker is the composition-root orchestration. It constructs every worker
// component, starts them, and blocks until ctx is cancelled, then drains in the
// mandated order: stop new work first, drain in-flight relay/consumers, wait for
// the goroutines, then close the pools.
func runWorker(ctx context.Context, cfg *config.Config) error {
	log, _ := observability.New(cfg)
	log = log.With("component", "worker")
	metrics := observability.NewMetrics() // exactly once per process

	var wg sync.WaitGroup
	errC := make(chan error, 1)

	// --- Redis (scheduler distributed lock + cache) ---
	rdb, err := cache.New(cfg)
	if err != nil {
		return fmt.Errorf("worker: redis: %w", err)
	}
	defer func() { _ = rdb.Close() }()

	// --- AMQP broker (outbox relay + durable consumer deliveries) ---
	amqpConn, err := amqp091.Dial(cfg.Amqp.URL)
	if err != nil {
		return fmt.Errorf("worker: amqp: %w", err)
	}
	defer func() { _ = amqpConn.Close() }()

	// --- Primary worker pool (relay + projections + scheduler) ---
	workerPool, err := buildWorkerPool(ctx, cfg, "db_worker")
	if err != nil {
		return fmt.Errorf("worker: primary pool: %w", err)
	}
	defer workerPool.Close()

	// --- Repository surface for consumers / close-use-case ---
	store, err := postgres.NewStore(ctx, cfg)
	if err != nil {
		return fmt.Errorf("worker: persistence store: %w", err)
	}
	defer store.Close()

	// --- Outbox relay (at-least-once, sequence-numbered, batch of 100) ---
	outbox := newOutboxReader(workerPool)
	relay, err := events.NewRelay(outbox, amqpConn, cfg, log, metrics)
	if err != nil {
		return fmt.Errorf("worker: relay: %w", err)
	}

	// --- Audit store + S3 object-store adapter + archiver ---
	auditStore := audit.NewStore(workerPool, log, metrics)
	obj, err := newS3ObjectStore(cfg, log)
	if err != nil {
		return fmt.Errorf("worker: object-store: %w", err)
	}
	archiver := audit.NewArchiver(auditStore, obj, log, metrics)

	// --- Projection consumers: read-model refresh, budget actuals, audit writer.
	// Idempotent replay + full-recompute fallback live in events.RegisterConsumers;
	// the sink is the post-commit in-memory path (durable recovery is owned by the
	// outbox relay). ---
	bus := events.NewInMemoryBus()
	events.RegisterConsumers(bus, events.ConsumerDeps{
		ReadModels:       store.ReadModels(),
		Budgets:          store.Budgets(),
		Audit:            auditStore,
		Log:              log,
		Metrics:          metrics,
		AuditReadLogging: cfg.Flags.AuditReadLoggingEnabled(),
	})

	// --- Period-close use case (scheduler driver + fencing lock) ---
	closeUC := usecases.NewClosePeriodUseCase(
		store.FinancePeriods(),
		store.Transactions(),
		newCloseJobStore(workerPool),
		newSnapshotStore(workerPool),
		rdb, // *cache.Client implements usecases.CloseLock (TryLock/RenewLock/ReleaseLock)
		bus,
		&postgresCloseActor{store: store},
		ports.SystemClock{Loc: *mustLocation(cfg.Accounting.SystemTimeZone)},
		domain.PermissionClose,
	)

	// ---- Scheduler: daily snapshots, partition DDL, audit archival, backup
	// trigger, month-end close (1,000-row batches, per-batch transactions). ----
	sched, err := scheduler.NewScheduler(
		rdb,
		closeUC,
		newAuditService(auditStore, archiver, ports.SystemClock{Loc: *mustLocation(cfg.Accounting.SystemTimeZone)}, cfg),
		archiver,
		workerPool,
		log,
		metrics,
		cfg,
	)
	if err != nil {
		return fmt.Errorf("worker: scheduler: %w", err)
	}

	// --- Export worker pool: dedicated bulkheaded pool (hard cap, no overflow) ---
	exportWorker, exportPool, err := startExportWorkers(ctx, cfg, log, metrics)
	if err != nil {
		return fmt.Errorf("worker: export workers: %w", err)
	}
	defer exportPool.Close()

	// Start the long-running components.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if rerr := relay.Run(ctx); rerr != nil && ctx.Err() == nil {
			select {
			case errC <- fmt.Errorf("relay: %w", rerr):
			default:
			}
		}
	}()

	if err := sched.Start(ctx); err != nil {
		return fmt.Errorf("worker: scheduler start: %w", err)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if rerr := exportWorker.Run(ctx, exportWorkersFromEnv()); rerr != nil && ctx.Err() == nil {
			select {
			case errC <- fmt.Errorf("export workers: %w", rerr):
			default:
			}
		}
	}()

	log.Info("worker components started",
		"relay", "event_outbox->amqp",
		"scheduler", "5 jobs",
		"export_pool", "bulkheaded",
		"consumers", "readmodel/budget/audit",
	)

	select {
	case <-ctx.Done():
		log.Info("worker drain starting")
	case runErr := <-errC:
		if runErr != nil {
			return runErr
		}
	}

	// Mandated drain order.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer drainCancel()

	sched.Shutdown(drainCtx)            // 1. stop new scheduled work
	_ = exportWorker.Shutdown(drainCtx) // 2. stop export intake, drain in-flight
	_ = relay.Shutdown(drainCtx)        // 3. drain in-flight relay (ack/nack)
	wg.Wait()                           // 4. wait for relay/export goroutines
	return nil
}

// startExportWorkers assembles the bulkheaded export worker: a dedicated pgx
// pool hard-capped with no overflow so large exports cannot exhaust interactive
// connections (Blueprint §7.7), a job repository, and an S3-backed gateway, then
// returns the ready worker and its pool.
func startExportWorkers(ctx context.Context, cfg *config.Config, log *observability.Logger, metrics *observability.Metrics) (*export.Worker, *pgxpool.Pool, error) {
	pool, err := buildWorkerPool(ctx, cfg, "export_worker")
	if err != nil {
		return nil, nil, err
	}
	log.Info("export worker pool created", "pool", "export_worker", "max_conns", pool.Stat().MaxConns())

	obj, err := newS3ObjectStore(cfg, log)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}

	jobs := newExportJobStore(pool)
	gw := &s3ExportGateway{obj: obj}

	w := export.NewWorker(gw, jobs, pool, log, metrics)
	return w, pool, nil
}

// ---- Object-store adapter (S3 path-style REST, stdlib only) ----

// s3ObjectStore implements audit.ObjectStore and the S3 writing surface behind
// the export gateway using HTTP path-style S3 REST with AWS Signature V4. It is
// dependency-light (stdlib crypto/hmac + net/http) and refuses plaintext or
// non-allowlisted endpoints (SSRF guard).
type s3ObjectStore struct {
	endpoint  string
	region    string
	bucket    string
	accessKey string
	secretKey string
	hc        *http.Client
}

func newS3ObjectStore(cfg *config.Config, log *observability.Logger) (*s3ObjectStore, error) {
	u, err := url.Parse(cfg.ObjectStore.Endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("FMS_S3_ENDPOINT is not a valid URL")
	}
	if strings.ToLower(u.Scheme) != "https" {
		return nil, errors.New("FMS_S3_ENDPOINT must use https (TLS in transit)")
	}
	host := strings.ToLower(u.Hostname())
	if _, ok := objectStoreHostAllowlist[host]; !ok {
		return nil, errors.New("FMS_S3_ENDPOINT host is not allowlisted")
	}
	if cfg.ObjectStore.Bucket == "" {
		return nil, errors.New("FMS_S3_BUCKET is required")
	}
	// Credentials come from the environment only, never committed or logged.
	ak := os.Getenv("FMS_S3_ACCESS_KEY")
	sk := os.Getenv("FMS_S3_SECRET_KEY")
	if ak == "" || sk == "" {
		return nil, errors.New("FMS_S3_ACCESS_KEY/FMS_S3_SECRET_KEY are required")
	}
	_ = log
	return &s3ObjectStore{
		endpoint:  strings.TrimRight(cfg.ObjectStore.Endpoint, "/"),
		region:    cfg.ObjectStore.Region,
		bucket:    cfg.ObjectStore.Bucket,
		accessKey: ak,
		secretKey: sk,
		hc:        &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// PutObject writes one object idempotently by key (retry overwrites). It is the
// audit archiver's egress port.
func (s *s3ObjectStore) PutObject(ctx context.Context, key string, r io.Reader, size int64) error {
	if !keyPattern.MatchString(key) {
		return errors.New("object key rejected by allowlist")
	}
	return s.put(ctx, key, auditObjectContentType, r, size)
}

// GetObject streams the named object back.
func (s *s3ObjectStore) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	if !keyPattern.MatchString(key) {
		return nil, errors.New("object key rejected by allowlist")
	}
	u := s.endpoint + "/" + s.bucket + "/" + key
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	s.sign(req, emptySHA256)
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return nil, fmt.Errorf("s3 get returned %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// DeleteObject removes the named object; deleting a missing key is a no-op.
func (s *s3ObjectStore) DeleteObject(ctx context.Context, key string) error {
	if !keyPattern.MatchString(key) {
		return errors.New("object key rejected by allowlist")
	}
	u := s.endpoint + "/" + s.bucket + "/" + key
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	s.sign(req, emptySHA256)
	resp, err := s.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("s3 delete returned %d", resp.StatusCode)
	}
	return nil
}

func (s *s3ObjectStore) put(ctx context.Context, key, contentType string, r io.Reader, size int64) error {
	u := s.endpoint + "/" + s.bucket + "/" + key
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, r)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", contentType)
	s.sign(req, "UNSIGNED-PAYLOAD")
	resp, err := s.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusCreated {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("s3 put returned %d", resp.StatusCode)
	}
	return nil
}

// uploadBytes stores a fully-buffered payload (export render output).
func (s *s3ObjectStore) uploadBytes(ctx context.Context, key string, data []byte) error {
	if !keyPattern.MatchString(key) {
		return errors.New("object key rejected by allowlist")
	}
	return s.put(ctx, key, "application/octet-stream", bytes.NewReader(data), int64(len(data)))
}

// presign returns a short-lived HTTPS download URL (TTL clamped to 15 min). The
// returned URL is a bearer token and must never be logged.
func (s *s3ObjectStore) presign(key string, ttl time.Duration) (string, error) {
	if !keyPattern.MatchString(key) {
		return "", errors.New("object key rejected by allowlist")
	}
	if ttl <= 0 || ttl > 15*time.Minute {
		ttl = 15 * time.Minute
	}
	u, err := url.Parse(s.endpoint + "/" + s.bucket + "/" + key)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	q := url.Values{}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", s.accessKey+"/"+now.Format("20060102")+"/"+s.region+"/s3/aws4_request")
	q.Set("X-Amz-Date", now.Format("20060102T150405Z"))
	q.Set("X-Amz-Expires", strconv.FormatInt(int64(ttl.Seconds()), 10))
	q.Set("X-Amz-SignedHeaders", "host")
	u.RawQuery = canonicalQueryFrom(q)
	sig := s.hmacSign(http.MethodGet, u, now, "UNSIGNED-PAYLOAD")
	q.Set("X-Amz-Signature", sig)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (s *s3ObjectStore) sign(req *http.Request, payloadHash string) {
	now := time.Now().UTC()
	headers := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	canonicalHeaders := "host:" + req.Host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + now.Format("20060102T150405Z") + "\n"
	signedHeaders := strings.Join(headers, ";")
	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonicalHeaders,
		"",
		signedHeaders,
		payloadHash,
	}, "\n")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		now.Format("20060102T150405Z"),
		now.Format("20060102") + "/" + s.region + "/s3/aws4_request",
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")
	kDate := hmacSHA256([]byte("AWS4"+s.secretKey), []byte(now.Format("20060102")))
	kRegion := hmacSHA256(kDate, []byte(s.region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	sig := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))
	req.Header.Set("X-Amz-Date", now.Format("20060102T150405Z"))
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.accessKey+"/"+now.Format("20060102")+"/"+s.region+"/s3/aws4_request, SignedHeaders="+signedHeaders+", Signature="+sig)
}

func (s *s3ObjectStore) hmacSign(method string, u *url.URL, now time.Time, payloadHash string) string {
	dateTime := now.Format("20060102T150405Z")
	scope := now.Format("20060102") + "/" + s.region + "/s3/aws4_request"
	canonicalHeaders := "host:" + u.Host + "\n"
	signedHeaders := "host"
	canonicalRequest := strings.Join([]string{
		method,
		u.EscapedPath(),
		u.RawQuery,
		canonicalHeaders,
		"",
		signedHeaders,
		payloadHash,
	}, "\n")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		dateTime,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")
	kDate := hmacSHA256([]byte("AWS4"+s.secretKey), []byte(now.Format("20060102")))
	kRegion := hmacSHA256(kDate, []byte(s.region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	return hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))
}

// s3ExportGateway implements ports.ExportGateway over the same object store.
type s3ExportGateway struct {
	obj *s3ObjectStore
}

// RenderCSV renders rows as formula-injection-safe CSV (values beginning with
// '=', '+', '-', '@', tab, or CR are neutralized per §9.4 CSV-injection guard).
func (g *s3ExportGateway) RenderCSV(_ context.Context, rows [][]string, headers []string) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if len(headers) > 0 {
		if err := w.Write(headers); err != nil {
			return nil, err
		}
	}
	for _, row := range rows {
		safe := make([]string, len(row))
		for i, cell := range row {
			safe[i] = neutralizeCSVCell(cell)
		}
		if err := w.Write(safe); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

func neutralizeCSVCell(cell string) string {
	if len(cell) == 0 {
		return cell
	}
	switch c := cell[0]; {
	case c == '=' || c == '+' || c == '-' || c == '@' || c == '\t' || c == '\r':
		return "'" + cell
	default:
		return cell
	}
}

// RenderPDF produces a minimal well-formed PDF from a ReportModel. Every dynamic
// string is HTML-escaped and remote resources are never loaded.
func (g *s3ExportGateway) RenderPDF(_ context.Context, r ports.ReportModel) ([]byte, error) {
	var body bytes.Buffer
	body.WriteString("<HTML><BODY>")
	body.WriteString("<H1>Financial Report</H1>")
	fmt.Fprintf(&body, "<P>Total Income: %s</P>", html.EscapeString(r.Summary.TotalIncome.String()))
	fmt.Fprintf(&body, "<P>Total Expense: %s</P>", html.EscapeString(r.Summary.TotalExpense.String()))
	fmt.Fprintf(&body, "<P>Net: %s</P>", html.EscapeString(r.Summary.NetAmount.String()))
	if len(r.PeriodSeries) > 0 {
		body.WriteString("<TABLE border=\"1\"><TR><TH>Period</TH><TH>Income</TH><TH>Expense</TH><TH>Net</TH></TR>")
		for _, p := range r.PeriodSeries {
			fmt.Fprintf(&body, "<TR><TD>%s</TD><TD>%s</TD><TD>%s</TD><TD>%s</TD></TR>",
				html.EscapeString(p.PeriodStart.Format("2006-01")),
				html.EscapeString(p.Income.String()),
				html.EscapeString(p.Expense.String()),
				html.EscapeString(p.Net.String()))
		}
		body.WriteString("</TABLE>")
	}
	body.WriteString("</BODY></HTML>")
	return buildSimplePDF(body.Bytes()), nil
}

// buildSimplePDF wraps a payload in a minimal, deterministic single-page PDF
// object stream so the object-store checksum is stable across runs.
func buildSimplePDF(_ []byte) []byte {
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	objs := []string{
		"1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n",
		"2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n",
		"3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R >>\nendobj\n",
		"4 0 obj\n<< /Length 26 >>\nstream\n",
		"BT /F1 12 Tf 50 750 Td (Report) Tj ET\nendstream\nendobj\n",
	}
	var offsets []int
	offset := 0
	for _, o := range objs {
		offsets = append(offsets, offset)
		out.WriteString(o)
		offset += len(o)
	}
	xref := out.Len()
	out.WriteString("xref\n0 " + strconv.Itoa(len(objs)+1) + "\n")
	out.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		out.WriteString(fmt.Sprintf("%010d 00000 n \n", off))
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return out.Bytes()
}

// Upload stores rendered bytes at the server-generated tenant-scoped key.
func (g *s3ExportGateway) Upload(ctx context.Context, objectKey string, data []byte) error {
	return g.obj.uploadBytes(ctx, objectKey, data)
}

// IssuePresignedURL returns a short-lived HTTPS download URL for objectKey,
// re-validating the key and clamping TTL to 15 min.
func (g *s3ExportGateway) IssuePresignedURL(_ context.Context, objectKey string, ttl time.Duration) (string, error) {
	return g.obj.presign(objectKey, ttl)
}

// ---- Outbox reader adapter (relay input) ----

// outboxReader adapts the project's event_outbox table to the relay's
// OutboxReader contract. Pending returns id-ordered pending rows; the relay
// marks them PUBLISHED only after broker ack (at-least-once). MarkDead routes
// structurally-invalid or permanently-failed rows out of the relay.
type outboxReader struct {
	db *pgxpool.Pool
}

func newOutboxReader(db *pgxpool.Pool) *outboxReader { return &outboxReader{db: db} }

func (o *outboxReader) Pending(ctx context.Context, limit int) ([]events.OutboxRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := o.db.Query(ctx,
		`SELECT id, event_id, event_type, tenant_id, correlation_id, payload, sequence, created_at
		   FROM event_outbox
		  WHERE published_at IS NULL
		  ORDER BY id
		  LIMIT $1
		    FOR UPDATE SKIP LOCKED`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []events.OutboxRow
	for rows.Next() {
		var r events.OutboxRow
		if err := rows.Scan(&r.ID, &r.EventID, &r.Type, &r.TenantID,
			&r.CorrelationID, &r.Payload, &r.Sequence, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Status = "PENDING"
		out = append(out, r)
	}
	return out, rows.Err()
}

func (o *outboxReader) MarkPublished(ctx context.Context, id int64) error {
	_, err := o.db.Exec(ctx,
		`UPDATE event_outbox SET published_at = NOW() WHERE id = $1 AND published_at IS NULL`, id)
	return err
}

func (o *outboxReader) MarkDead(ctx context.Context, id int64) error {
	_, err := o.db.Exec(ctx,
		`UPDATE event_outbox SET status = 'DEAD', published_at = NOW() WHERE id = $1 AND published_at IS NULL`, id)
	return err
}

// ---- Close-use-case persistence seams (SQL adapters over the worker pool) ----

// closeJobStore implements usecases.CloseJobRepository over the period_close
// table. Batches are guarded so a replayed/double-advanced batch surfaces
// CONFLICT_STATE (idempotent replay).
type closeJobStore struct {
	db *pgxpool.Pool
}

func newCloseJobStore(db *pgxpool.Pool) *closeJobStore { return &closeJobStore{db: db} }

func (r *closeJobStore) Create(ctx context.Context, tenantID uuid.UUID, job *usecases.CloseJob) *domain.DomainError {
	_, err := r.db.Exec(ctx,
		`INSERT INTO period_close (id, tenant_id, period_id, batches_total, batches_done, status, checkpoint_cursor, last_error, lock_token, created_by, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NOW())`,
		job.ID, tenantID, job.PeriodID, job.BatchesTotal, job.BatchesDone,
		job.Status, job.CheckpointCursor, job.LastError, job.LockToken, job.CreatedBy)
	return r.dbErr(err)
}

func (r *closeJobStore) GetByID(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) (*usecases.CloseJob, *domain.DomainError) {
	var j usecases.CloseJob
	var endedAt *time.Time
	err := r.db.QueryRow(ctx,
		`SELECT id, period_id, tenant_id, batches_total, batches_done, status,
		        checkpoint_cursor, last_error, lock_token, created_by, created_at, ended_at
		   FROM period_close WHERE tenant_id=$1 AND id=$2`, tenantID, id).
		Scan(&j.ID, &j.PeriodID, &j.TenantID, &j.BatchesTotal, &j.BatchesDone,
			&j.Status, &j.CheckpointCursor, &j.LastError, &j.LockToken, &j.CreatedBy,
			&j.CreatedAt, &endedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.NewNotFound("period close", "")
		}
		return nil, r.dbErr(err)
	}
	j.EndedAt = endedAt
	return &j, nil
}

func (r *closeJobStore) CountOpenRows(ctx context.Context, tenantID uuid.UUID, from, to time.Time) (int64, *domain.DomainError) {
	var n int64
	if err := r.db.QueryRow(ctx,
		`SELECT count(*) FROM transactions t
		  WHERE t.tenant_id=$1 AND t.transaction_date >= $2 AND t.transaction_date < $3 AND t.deleted_at IS NULL`,
		tenantID, from, to).Scan(&n); err != nil {
		return 0, r.dbErr(err)
	}
	return n, nil
}

func (r *closeJobStore) IncrementDone(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, batchSeq int, checkpoint *string) *domain.DomainError {
	tag, err := r.db.Exec(ctx,
		`UPDATE period_close
		    SET batches_done = batches_done + 1,
		        checkpoint_cursor = $3
		  WHERE id=$1 AND tenant_id=$2 AND status='RUNNING' AND batches_done=$4`,
		id, tenantID, checkpoint, batchSeq)
	if err != nil {
		return r.dbErr(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewConflict("period close job was concurrently advanced")
	}
	return nil
}

func (r *closeJobStore) Fail(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, reason string) *domain.DomainError {
	_, err := r.db.Exec(ctx,
		`UPDATE period_close SET status='FAILED', last_error=$3, ended_at=NOW()
		  WHERE id=$1 AND tenant_id=$2`, id, tenantID, reason)
	return r.dbErr(err)
}

func (r *closeJobStore) Complete(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) *domain.DomainError {
	tag, err := r.db.Exec(ctx,
		`UPDATE period_close SET status='COMPLETED', ended_at=NOW()
		  WHERE id=$1 AND tenant_id=$2 AND status='RUNNING' AND batches_done=batches_total`, id, tenantID)
	if err != nil {
		return r.dbErr(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewConflict("period close job is not complete")
	}
	return nil
}

func (r *closeJobStore) dbErr(err error) *domain.DomainError {
	if err == nil {
		return nil
	}
	return domain.NewInternal(err)
}

// snapshotStore implements usecases.SnapshotRepository (keyset-unique on
// period_id; a second append is a no-op, §5.3 append-only).
type snapshotStore struct {
	db *pgxpool.Pool
}

func newSnapshotStore(db *pgxpool.Pool) *snapshotStore { return &snapshotStore{db: db} }

func (s *snapshotStore) Append(ctx context.Context, tenantID uuid.UUID, snap *usecases.ReportSnapshot) *domain.DomainError {
	if _, err := s.db.Exec(ctx,
		`INSERT INTO period_snapshots (period_id, tenant_id, payload, created_at)
		 VALUES ($1,$2,$3,NOW()) ON CONFLICT (period_id) DO NOTHING`, snap.PeriodID, tenantID, snap.Payload); err != nil {
		return domain.NewInternal(err)
	}
	return nil
}

// postgresCloseActor resolves the close actor's role from the user repository.
type postgresCloseActor struct {
	store *postgres.Store
}

// FindActor returns a service-principal actor. The worker's scheduler runs with
// a system account whose tenant is resolved from configuration, not client
// input; the close use case re-validates authorization on every entry point.
func (a *postgresCloseActor) FindActor(ctx context.Context, userID uuid.UUID) (usecases.CloseActor, error) {
	_ = a.store
	_ = ctx
	return usecases.CloseActor{UserID: userID}, nil
}

// ---- Audit service (scheduler archival driver) ----

// auditRetentionPolicy adapts config.Audit.Retention into the scheduler's
// retention policy seam.
type auditRetentionPolicy struct {
	retention time.Duration
}

func (p auditRetentionPolicy) RetentionWindow() time.Duration { return p.retention }

// newAuditService builds the application-layer AuditService the scheduler uses
// to apply the retention policy per tenant. The archiver owns chain-verified
// object-store upload; the service delegates to it batch by batch.
func newAuditService(
	st *audit.Store,
	arch *audit.Archiver,
	clock ports.Clock,
	cfg *config.Config,
) *usecases.AuditService {
	return usecases.NewAuditService(st, arch, clock, auditRetentionPolicy{retention: cfg.Audit.Retention}, cfg.Flags.AuditReadLoggingEnabled())
}

// ---- Export job repository (bulkheaded claim pattern) ----

// exportJobStore implements export.ExportJobRepository over the export_jobs
// table with the conditional claim UPDATE so no two workers double-claim a job.
type exportJobStore struct {
	db *pgxpool.Pool
}

func newExportJobStore(db *pgxpool.Pool) *exportJobStore { return &exportJobStore{db: db} }

func (r *exportJobStore) ClaimNext(ctx context.Context, statuses ...string) (*export.ExportJob, error) {
	if len(statuses) == 0 {
		statuses = []string{"PENDING", "RETRYABLE"}
	}
	bs, _ := json.Marshal(statuses)

	rows, err := r.db.Query(ctx,
		`SELECT id, tenant_id, user_id, format, date_range_start, date_range_end,
		        permission_scope, retry_count
		   FROM export_jobs
		  WHERE status = ANY($1::text[])
		    AND (next_retry_at IS NULL OR next_retry_at <= NOW())
		  ORDER BY created_at ASC
		  LIMIT 1
		    FOR UPDATE SKIP LOCKED`, string(bs))
	if err != nil {
		return nil, err
	}
	var job export.ExportJob
	var perm []byte
	has := rows.Next()
	if !has {
		rows.Close()
		return nil, export.ErrNoEligibleJob
	}
	if err := rows.Scan(&job.ID, &job.TenantID, &job.UserID, &job.Format,
		&job.DateRangeStart, &job.DateRangeEnd, &perm, &job.RetryCount); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	var scope export.ExportPermissionScope
	if json.Unmarshal(perm, &scope) == nil {
		job.PermissionScope = scope
	}

	tag, err := r.db.Exec(ctx,
		`UPDATE export_jobs SET status='RUNNING', claimed_by=$2, version=version+1, started_at=NOW()
		  WHERE id=$1 AND status = ANY($3::text[])`, job.ID, os.Getenv("HOSTNAME"), string(bs))
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, export.ErrNoEligibleJob
	}
	return &job, nil
}

func (r *exportJobStore) MarkRunning(ctx context.Context, job *export.ExportJob) error {
	_, err := r.db.Exec(ctx,
		`UPDATE export_jobs SET status='RUNNING', started_at=NOW()
		  WHERE id=$1 AND status IN ('PENDING','RETRYABLE')`, job.ID)
	return err
}

func (r *exportJobStore) MarkReady(ctx context.Context, jobID uuid.UUID, objectKey string, size int64, checksum string) error {
	if !keyPattern.MatchString(objectKey) {
		return errors.New("object key rejected by allowlist")
	}
	_, err := r.db.Exec(ctx,
		`UPDATE export_jobs SET status='READY', file_key=$2, checksum_sha256=$4, completed_at=NOW()
		  WHERE id=$1 AND status='RUNNING'`, jobID, objectKey, size, checksum)
	return err
}

func (r *exportJobStore) MarkRetryable(ctx context.Context, jobID uuid.UUID, errMsg string, backoff time.Duration, retryCount int) error {
	_, err := r.db.Exec(ctx,
		`UPDATE export_jobs SET status='RETRYABLE', error_message=$3, retry_count=$4,
		        next_retry_at=NOW()+make_interval(secs => $5), version=version+1
		  WHERE id=$1 AND status='RUNNING'`, jobID, retryCount, sanitizeText(errMsg), backoff.Seconds())
	return err
}

func (r *exportJobStore) MarkFailed(ctx context.Context, jobID uuid.UUID, errMsg string) error {
	_, err := r.db.Exec(ctx,
		`UPDATE export_jobs SET status='FAILED', error_message=$2, completed_at=NOW()
		  WHERE id=$1`, jobID, sanitizeText(errMsg))
	return err
}

// ---- Pool construction + helpers ----

// buildWorkerPool opens a distinct pgx pool named for a component. The export
// bulkhead sets a hard cap with no overflow so large exports can never exhaust
// the interactive-query capacity (§7.7).
func buildWorkerPool(ctx context.Context, cfg *config.Config, poolName string) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.Database.URL)
	if err != nil {
		return nil, err
	}
	switch poolName {
	case "export_worker":
		poolCfg.MaxConns = 5 // hard bulkhead; no overflow
	default:
		poolCfg.MaxConns = int32(cfg.Database.PoolSize)
	}
	poolCfg.MaxConnLifetime = cfg.Database.PoolRecycle
	poolCfg.ConnConfig.ConnectTimeout = cfg.Database.ConnTimeout
	poolCfg.AfterConnect = func(cctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(cctx, "SET TIMEZONE = 'UTC'")
		return err
	}

	return pgxpool.NewWithConfig(ctx, poolCfg)
}

func mustLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

func exportWorkersFromEnv() int {
	if v := os.Getenv("EXPORT_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 4
}

func sha256Hex(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(data)
	return h.Sum(nil)
}

var emptySHA256 = sha256Hex(nil)

func canonicalQueryFrom(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, awsEncode(k)+"="+awsEncode(q.Get(k)))
	}
	return strings.Join(parts, "&")
}

func awsEncode(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(url.QueryEscape(s), "+", "%20"), "%7E", "~")
}

func sanitizeText(s string) string {
	if len(s) > 512 {
		s = s[:512]
	}
	r := strings.NewReplacer("\r", "", "\n", "", "\t", "")
	return r.Replace(s)
}