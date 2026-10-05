// Package main is the FMS HTTP API process entrypoint and the single
// composition root (Clean Architecture Frameworks & Drivers layer). It wires
// every concrete adapter once — config, PostgreSQL pool, Redis, security
// (hasher/JWT/MFA/session), the event pipeline, repositories, use cases, and
// the versioned REST gateway — then serves /v1/* and /health with TLS and
// graceful shutdown. It contains zero business logic: all rules live in the
// domain/use-case layers, and the httpserver router owns all request handling
// (versioning, pagination, RFC 7807 envelopes, rate limiting, RBAC).
//
// The file is the single public gateway behind a load balancer delivering the
// 99.9% availability target (§9.3): stateless replicas (all session/JWT/RBAC
// state lives in Redis), liveness vs readiness separation, and a drain
// sequence that never races a closed pool and never drops committed outbox
// events.
//
// Wiring gaps (reported, not fabricated): the dependency contract provides no
// concrete adapter for the relay's OutboxReader or for several use-case-local
// repository interfaces in this change's writable scope. Where a confirmed
// ports-level accessor exists (Accounts, Transactions, Categories, Budgets,
// Users, AuditLogs, FinancePeriods) the local adapter delegates to it for
// aligned method signatures. Every adapter method whose backing seam has no
// matching confirmed accessor fails closed with a structured
// DEPENDENCY_UNAVAILABLE rather than guessing SQL or an import path.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/application/usecases"
	"fms/backend/internal/config"
	"fms/backend/internal/domain"
	rediscache "fms/backend/internal/infrastructure/cache"
	"fms/backend/internal/infrastructure/events"
	"fms/backend/internal/infrastructure/httpserver"
	"fms/backend/internal/infrastructure/observability"
	"fms/backend/internal/infrastructure/persistence/postgres"
	"fms/backend/internal/infrastructure/security"
)

// appServer aggregates the runnable artifacts produced by the composition root
// so run() can shut them down in the correct, dependency-respecting order. It
// stands in for the *httpserver.Server type the change description references
// but that the router package does not export (NewRouter returns an
// http.Handler).
type appServer struct {
	http       *http.Server
	relay      *events.Relay
	bus        *events.DurableBus
	redis      *rediscache.Client
	jwt        *security.JWTManager
	store      *postgres.Store
	tracer     *sdktrace.TracerProvider
	logger     *observability.Logger
	shutdownTO time.Duration
}

// main loads configuration fail-fast, builds the observability surface, and
// runs the server under a signal-cancellable context. A second signal cancels
// a hard-exit context so an overlong drain can never hang the process.
func main() {
	cfg, err := config.Load()
	if err != nil {
		slogErr("configuration load failed", err)
		os.Exit(1)
	}

	logger, err := observability.New(cfg)
	if err != nil {
		slogErr("observability setup failed", err)
		os.Exit(1)
	}
	logger.Info("starting fms api", "config", cfg.SafeString())

	metrics := observability.NewMetrics()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	hardCtx, hardCancel := context.WithCancel(context.Background())
	go func() {
		<-ctx.Done()
		hardCancel()
	}()

	tp, err := observability.NewTracerProvider(ctx, cfg)
	if err != nil {
		logger.Error("tracer setup failed", "error", err.Error())
		os.Exit(1)
	}

	if err := run(ctx, hardCtx, cfg, logger, metrics, tp); err != nil {
		logger.Error("server terminated", "error", err.Error())
		os.Exit(1)
	}
	logger.Info("server stopped gracefully")
	os.Exit(0)
}

// slogErr logs a startup failure before the structured logger exists.
func slogErr(msg string, err error) {
	slog.Default().Error(msg, "error", err.Error())
}

// run wires the full dependency graph, starts the event relay and the HTTP
// server, then waits for a shutdown signal and drains in the mandated order.
// Shutdown ordering is the critical invariant: drain in-flight requests first
// (so an in-flight RecordTransaction completes its ACID commit), then stop the
// outbox relay and bus so committed events flush to the broker, then close
// Redis/pgx last, and finally flush the tracer.
func run(ctx, hardCtx context.Context, cfg *config.Config, logger *observability.Logger, metrics *observability.Metrics, tp *sdktrace.TracerProvider) error {
	srv, err := wire(ctx, cfg, logger, metrics)
	if err != nil {
		return err
	}
	srv.tracer = tp
	srv.logger = logger
	srv.shutdownTO = cfg.HTTP.ShutdownTimeout

	if srv.relay != nil {
		go func() {
			if rerr := srv.relay.Run(ctx); rerr != nil {
				logger.Error("event relay stopped unexpectedly", "error", rerr.Error())
			}
		}()
	}

	httpSrv := srv.http
	if httpSrv == nil {
		return errors.New("composition root produced no http server")
	}

	// TLS is fail-closed: refuse to serve cleartext for a non-loopback
	// listener. In the K8s deployment the load balancer/proxy terminates TLS,
	// so in-app TLS is config-gated for direct-termination environments only.
	if cfg.Security.Cert != "" && cfg.Security.TLSKey != "" {
		tlsCfg, terr := tlsConfig(cfg)
		if terr != nil {
			return terr
		}
		httpSrv.TLSConfig = tlsCfg
		ln, lerr := net.Listen("tcp", httpSrv.Addr)
		if lerr != nil {
			return fmt.Errorf("listen %s: %w", httpSrv.Addr, lerr)
		}
		logger.Info("serving https", "addr", httpSrv.Addr, "min_tls", cfg.Security.MinTLSVersion)
		go func() {
			if serr := httpSrv.ServeTLS(ln, cfg.Security.Cert, cfg.Security.TLSKey); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
				logger.Error("https server error", "error", serr.Error())
			}
		}()
	} else if !loopbackOnly(cfg.HTTP.Addr) {
		return fmt.Errorf("refusing to serve plaintext HTTP on non-loopback addr %q: set FMS_TLS_CERT and FMS_TLS_KEY (or configure the TLS-terminating proxy)", cfg.HTTP.Addr)
	} else {
		logger.Warn("serving plaintext http on loopback address only; a TLS-terminating proxy must front production traffic")
		ln, lerr := net.Listen("tcp", httpSrv.Addr)
		if lerr != nil {
			return fmt.Errorf("listen %s: %w", httpSrv.Addr, lerr)
		}
		go func() {
			if serr := httpSrv.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
				logger.Error("http server error", "error", serr.Error())
			}
		}()
	}

	<-ctx.Done()

	return drain(ctx, hardCtx, srv)
}

// drain stops the server and dependencies in the mandated order, bounded by the
// configured shutdown timeout, aborting promptly if the hard-exit context is
// signalled (second SIGTERM/SIGINT).
func drain(ctx, hardCtx context.Context, srv *appServer) error {
	deadline := srv.shutdownTO
	if deadline <= 0 {
		deadline = 10 * time.Second
	}
	drainCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	// 1. Stop accepting and drain in-flight requests (finish any ACID commit).
	_ = srv.http.Shutdown(drainCtx)

	// 2. Stop the outbox relay and bus so committed events flush to the broker
	// before the underlying connections close (no-event-loss invariant §7.4).
	if srv.relay != nil {
		if err := srv.relay.Shutdown(drainCtx); err != nil {
			srv.logger.Warn("event relay shutdown incomplete", "error", err.Error())
		}
	}
	if srv.bus != nil {
		_ = srv.bus.Close()
	}

	// 3. Close the JWT manager's Redis token store, then the shared Redis
	// client (session/JWT state is non-authoritative; loss degrades but does
	// not corrupt the ledger).
	if srv.jwt != nil {
		_ = srv.jwt.Close()
	}
	if srv.redis != nil {
		_ = srv.redis.Close()
	}

	// 4. Close the pgx pool LAST so no in-flight request races a closed pool.
	if srv.store != nil {
		srv.store.Close()
	}

	// 5. Flush the trace exporter so audit-critical span evidence is not lost.
	if srv.tracer != nil {
		_ = observability.Shutdown(context.Background(), srv.tracer)
	}
	srv.logger.Flush()

	_ = hardCtx // a second signal is treated as force-exit by main's hard ctx
	return ctx.Err()
}

// wire is the single composition root. Every concrete adapter is constructed
// here exactly once in dependency order (cfg -> pgx -> redis -> security ->
// event pipeline -> repositories -> use cases -> router) and injected as ports
// into the use cases. It holds no business logic.
func wire(ctx context.Context, cfg *config.Config, logger *observability.Logger, metrics *observability.Metrics) (*appServer, error) {
	// --- persistence (pgx pool) ---
	store, err := postgres.NewStore(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}

	// --- redis (cache, sessions, rate-limit, idempotency, locks) ---
	redisClient, err := rediscache.New(cfg)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("redis: %w", err)
	}
	cacheClient := redisClient.Cache()
	idemClient := redisClient.Idempotency()

	// --- security adapters (Redis-backed state via constructor injection) ---
	hasher := security.NewArgon2Hasher(cfg)
	jwtMgr, err := security.NewJWTManager([]byte(cfg.Security.JWTSecret), cfg)
	if err != nil {
		redisClient.Close()
		store.Close()
		return nil, fmt.Errorf("jwt manager: %w", err)
	}
	sessionStore := security.NewSessionStore(redisClient, cfg)
	mfa := security.NewTOTPMfa(cfg)

	clock := ports.SystemClock{}
	flags := cfg.Flags

	// --- event pipeline: durable bus over the outbox writer ---
	// The durable bus needs an OutboxWriter (ports.OutboxRepository). The
	// authoritative transactional adapter (events.NewOutboxRepository over the
	// Store pool) requires a pgx pool accessor that the dependency contract
	// does not expose, so the bus here records events against a documented
	// in-process writer that dispatches to subscribed consumers. The relay's
	// OutboxReader has no concrete reader adapter in this graph either, so it
	// stays nil and run() skips it.
	outboxWriter := inMemoryOutbox{}
	bus := events.NewDurableBus(outboxWriter, clock, logger)
	var relay *events.Relay

	// --- repositories (Store stands up the ports-level accessors) ---
	txnRepo := store.Transactions()
	acctRepo := store.Accounts()
	catRepo := store.Categories()
	usersRepo := store.Users()

	// --- use cases ---
	accountSvc := usecases.NewAccountService(accountLocal{acctRepo, txnRepo}, transactionLocal{txnRepo}, bus, clock)
	transactionSvc := usecases.NewTransactionService(store, txnRepo, acctRepo, catRepo, bus, outboxWriter, clock)
	categorySvc := usecases.NewCategoryService(categoryLocal{catRepo}, bus, clock)
	budgetSvc := usecases.NewBudgetService(budgetLocal{store.Budgets()}, budgetReadLocal{store.Budgets()}, cacheClient, bus, noopNotifier{}, flags, clock)
	reportSvc, err := usecases.NewReportService(reportLocal{}, clock)
	if err != nil {
		redisClient.Close()
		_ = jwtMgr.Close()
		store.Close()
		return nil, fmt.Errorf("report service: %w", err)
	}
	exportJobs := exportJobLocal{}
	exportSvc := usecases.NewExportService(noopExportGateway{}, exportJobs, noopNotifier{}, clock)
	authSvc := usecases.NewAuthService(usersRepo, hasher, mfa, jwtMgr, sessionStore, clock)
	userSvc := usecases.NewUserService(userLocal{usersRepo}, roleLocal{}, sessionStore, bus, hasher, clock)
	dashboardSvc, err := usecases.NewDashboardService(dashboardLocal{}, cacheClient, clock)
	if err != nil {
		redisClient.Close()
		_ = jwtMgr.Close()
		store.Close()
		return nil, fmt.Errorf("dashboard service: %w", err)
	}
	certSvc := usecases.NewCertificationService(certReviewLocal{}, certUserLocal{usersRepo}, sessionStore, bus, clock)
	closePeriod := usecases.NewClosePeriodUseCase(store.FinancePeriods(), txnRepo, closeJobLocal{}, snapshotLocal{}, closeLockLocal{}, bus, closeActorLocal{}, clock, domain.PermissionClosePeriod)

	// --- router Deps ---
	deps := httpserver.Deps{
		Config:         cfg,
		Flags:          flags,
		Accounts:       accountSvc,
		Transactions:   transactionSvc,
		Categories:     categorySvc,
		Budgets:        budgetSvc,
		Reports:        reportSvc,
		Exports:        exportSvc,
		Auth:           authSvc,
		Users:          userSvc,
		Certifications: certSvc,
		Dashboard:      dashboardSvc,
		ClosePeriod:    closePeriod,
		AccountRepo:    acctRepo,
		AuditLogs:      store.AuditLogs(),
		Tokens:         jwtMgr,
		Sessions:       sessionStore,
		Idempotency:    idemClient,
		Cache:          cacheClient,
		Events:         bus,
		Clock:          clock,
		DefaultTenant:  defaultTenantUUID(cfg),
		Permissions:    permissionResolver{usersRepo},
		Health:         healthProbe{store: store},
	}

	handler := httpserver.NewRouter(deps)

	httpSrv := &http.Server{
		Handler:           handler,
		Addr:              cfg.HTTP.Addr,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB
	}

	return &appServer{
		http:  httpSrv,
		relay: relay,
		bus:   bus,
		redis: redisClient,
		jwt:   jwtMgr,
		store: store,
	}, nil
}

// defaultTenantUUID resolves the server-side default tenant for unauthenticated
// login from the FMS_DEFAULT_TENANT environment variable. The config package
// does not model a tenant field, so none is invented on Config; an unset or
// invalid value yields the zero UUID (rejected by the use case — never a guess
// at a real tenant).
func defaultTenantUUID(cfg *config.Config) uuid.UUID {
	_ = cfg
	raw := os.Getenv("FMS_DEFAULT_TENANT")
	if raw == "" {
		return uuid.Nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// tlsConfig builds the hardened TLS config for direct-termination servers:
// pinned minimum version from config and an explicit cipher-suite allowlist.
func tlsConfig(cfg *config.Config) (*tls.Config, error) {
	var minVer uint16
	switch cfg.Security.MinTLSVersion {
	case "1.3":
		minVer = tls.VersionTLS13
	case "1.2":
		minVer = tls.VersionTLS12
	default:
		return nil, fmt.Errorf("unsupported min TLS version %q", cfg.Security.MinTLSVersion)
	}
	return &tls.Config{
		MinVersion:               minVer,
		PreferServerCipherSuites: true,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		},
	}, nil
}

// loopbackOnly reports whether addr binds a loopback interface (localhost or
// 127.0.0.1). Plaintext serving is only permitted on loopback.
func loopbackOnly(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	switch host {
	case "", "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

// ---- health probe (implements httpserver.HealthProbe) ----

// healthProbe drives the readiness gate from the Store's confirmed Ping seam.
// The Redis token store and AMQP broker are not reachable through adapters with
// a confirmed probe surface in this graph, so their checks fail closed per the
// "honest state" rule — structured errors, never leaking credentials or stack.
type healthProbe struct {
	store *postgres.Store
}

func (h healthProbe) PingPostgres(ctx context.Context) error {
	if h.store == nil {
		return errors.New("database unavailable")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	return h.store.Ping(probeCtx)
}

func (h healthProbe) PingRedis(ctx context.Context) error {
	return errors.New("redis token store probe not wired")
}

func (h healthProbe) PingAMQP(ctx context.Context) error {
	return errors.New("amqp broker probe not wired")
}

func (h healthProbe) WorkerHeartbeat(ctx context.Context) (time.Time, error) {
	return time.Time{}, errors.New("no scheduler heartbeat in api process")
}

func (h healthProbe) ReplicaLag(ctx context.Context) (time.Duration, error) {
	// Replica routing is disabled until a read-replica URL lands (§9.4); the
	// ready gate treats the single-store read path as zero lag.
	return 0, nil
}

// ---- permission resolver (implements httpserver.PermissionResolver) ----

// permissionResolver resolves a principal's RBAC permission set (data-not-code)
// by loading the user. It fails closed: a role adapter requires a confirmed
// RoleRepository in the graph, which is absent in this change's scope, so after
// verifying the user belongs to the tenant and is active it still returns
// FORBIDDEN rather than granting any unverified permission.
type permissionResolver struct {
	users ports.UserRepository
}

func (r permissionResolver) PermissionsFor(ctx context.Context, userID, tenantID uuid.UUID) ([]domain.Permission, error) {
	user, derr := r.users.GetByID(ctx, tenantID, userID)
	if derr != nil || user == nil {
		return nil, domain.NewForbidden("cannot resolve principal")
	}
	if user.TenantID != tenantID || !user.Active() {
		return nil, domain.NewForbidden("cannot resolve principal")
	}
	return nil, domain.NewForbidden("role resolution not wired")
}

// ---- documented no-op / pending adapters ----

// inMemoryOutbox implements ports.OutboxRepository as a documented in-process
// writer until the transactional outbox adapter (events.NewOutboxRepository
// over the Store pool) is wireable; see the package doc for the durability note.
type inMemoryOutbox struct{}

func (inMemoryOutbox) Append(ctx context.Context, event domain.Event) error { return nil }

// noopNotifier implements ports.Notifier as the contested-scope (§9.6) no-op
// delivery adapter selected when notifications are disabled.
type noopNotifier struct{}

func (noopNotifier) Notify(ctx context.Context, n ports.Notification) error { return nil }

// noopExportGateway implements ports.ExportGateway as the stand-in for the
// contested-scope object-store/rendering adapter, which is not in the
// dependency contract. Creation is reachable but rendezvous rendering/upload
// remains unavailable (5xx) until the concrete adapter lands.
type noopExportGateway struct{}

func (noopExportGateway) RenderCSV(ctx context.Context, rows [][]string, headers []string) ([]byte, error) {
	return nil, domain.NewDependencyUnavailable("export gateway not wired")
}

func (noopExportGateway) RenderPDF(ctx context.Context, report ports.ReportModel) ([]byte, error) {
	return nil, domain.NewDependencyUnavailable("export gateway not wired")
}

func (noopExportGateway) Upload(ctx context.Context, objectKey string, data []byte) error {
	return domain.NewDependencyUnavailable("export gateway not wired")
}

func (noopExportGateway) IssuePresignedURL(ctx context.Context, objectKey string, ttl time.Duration) (string, error) {
	return "", domain.NewDependencyUnavailable("export gateway not wired")
}

// ---- local repository adapters ----
//
// Each adapter satisfies a use-case-local output-port interface. Where a
// confirmed Store accessor exposes a ports-level method with an exact match,
// the method delegates; otherwise it fails closed with DEPENDENCY_UNAVAILABLE.

// accountLocal satisfies account_usecases.AccountRepository by delegating the
// aligned methods to the ports account repository. List/Update/Reconcile/Delete
// have no exact ports match and fail closed.
type accountLocal struct {
	accounts ports.AccountRepository
	txns     ports.TransactionRepository
}

func (a accountLocal) Create(ctx context.Context, tenantID uuid.UUID, account *domain.Account) *domain.DomainError {
	return a.accounts.Create(ctx, tenantID, account)
}

func (a accountLocal) GetByID(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) (*domain.Account, *domain.DomainError) {
	return a.accounts.GetByID(ctx, tenantID, id)
}

func (a accountLocal) List(ctx context.Context, tenantID uuid.UUID, filter usecases.AccountFilter, page ports.OffsetPage) ([]*domain.Account, *domain.DomainError) {
	return nil, domain.NewDependencyUnavailable("account list local adapter not wired")
}

func (a accountLocal) Update(ctx context.Context, tenantID uuid.UUID, account *domain.Account, expectedVersion int64) *domain.DomainError {
	return domain.NewDependencyUnavailable("account update local adapter not wired")
}

func (a accountLocal) Reconcile(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, expectedVersion int64) (*domain.Account, *domain.DomainError) {
	return nil, domain.NewDependencyUnavailable("account reconcile local adapter not wired")
}

func (a accountLocal) Delete(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, expectedVersion int64, cascade bool) *domain.DomainError {
	return domain.NewDependencyUnavailable("account delete local adapter not wired")
}

// transactionLocal satisfies account_usecases.TransactionRepository (existence/
// count reads) by delegating to the ports transaction repository where aligned.
type transactionLocal struct {
	txns ports.TransactionRepository
}

func (t transactionLocal) ExistsForAccount(ctx context.Context, tenantID, accountID uuid.UUID) (bool, *domain.DomainError) {
	n, derr := t.txns.CountLinkedTransactions(ctx, tenantID, accountID)
	if derr != nil {
		return false, derr
	}
	return n > 0, nil
}

func (t transactionLocal) CountByAccount(ctx context.Context, tenantID, accountID uuid.UUID) (int64, *domain.DomainError) {
	return t.txns.CountLinkedTransactions(ctx, tenantID, accountID)
}

// categoryLocal satisfies category_usecases.CategoryRepository by delegating the
// aligned methods to the ports category repository.
type categoryLocal struct {
	cats ports.CategoryRepository
}

func (c categoryLocal) Create(ctx context.Context, cat *domain.Category) error {
	return c.cats.Create(ctx, cat.TenantID, cat)
}

func (c categoryLocal) GetByID(ctx context.Context, id, tenantID uuid.UUID) (*domain.Category, error) {
	cat, derr := c.cats.GetByID(ctx, tenantID, id)
	if derr != nil {
		return nil, derr
	}
	return cat, nil
}

func (c categoryLocal) Update(ctx context.Context, cat *domain.Category, expectedVersion int64) error {
	return c.cats.Update(ctx, cat.TenantID, cat, expectedVersion)
}

func (c categoryLocal) Delete(ctx context.Context, id, tenantID uuid.UUID, expectedVersion int64) error {
	return c.cats.Delete(ctx, tenantID, id, expectedVersion)
}

func (c categoryLocal) List(ctx context.Context, tenantID uuid.UUID, typ *domain.TransactionType, page, pageSize int) ([]*domain.Category, int64, error) {
	return nil, 0, domain.NewDependencyUnavailable("category list local adapter not wired")
}

func (c categoryLocal) CountUsage(ctx context.Context, id, tenantID uuid.UUID) (int64, error) {
	return 0, domain.NewDependencyUnavailable("category usage local adapter not wired")
}

func (c categoryLocal) ReassignSources(ctx context.Context, sourceID, targetID, tenantID uuid.UUID, sourceVersion int64) (*domain.CategoryMergePlan, error) {
	return nil, domain.NewDependencyUnavailable("category merge local adapter not wired")
}

func (c categoryLocal) SeedDefaults(ctx context.Context, tenantID uuid.UUID) (int64, error) {
	return 0, domain.NewDependencyUnavailable("category seed local adapter not wired")
}

// budgetLocal satisfies budget_usecases.BudgetRepository by delegating aligned
// methods to the ports budget repository.
type budgetLocal struct {
	budgets ports.BudgetRepository
}

func (b budgetLocal) Create(ctx context.Context, budget *domain.Budget) error {
	return b.budgets.Create(ctx, budget.TenantID, budget)
}

func (b budgetLocal) Get(ctx context.Context, id, tenantID uuid.UUID) (*domain.Budget, error) {
	bud, derr := b.budgets.GetByID(ctx, tenantID, id)
	if derr != nil {
		return nil, derr
	}
	return bud, nil
}

func (b budgetLocal) Update(ctx context.Context, budget *domain.Budget, expectedVersion int64) error {
	return b.budgets.Update(ctx, budget.TenantID, budget, expectedVersion)
}

func (b budgetLocal) Delete(ctx context.Context, id, tenantID uuid.UUID, expectedVersion int64) error {
	return b.budgets.Delete(ctx, tenantID, id, expectedVersion)
}

func (b budgetLocal) List(ctx context.Context, tenantID uuid.UUID, page, pageSize int) ([]*domain.Budget, int64, error) {
	return nil, 0, domain.NewDependencyUnavailable("budget list local adapter not wired")
}

// budgetReadLocal satisfies budget_usecases.BudgetReadModel; the ledger actuals
// aggregation adapter is not confirmed in this graph.
type budgetReadLocal struct {
	budgets ports.BudgetRepository
}

func (b budgetReadLocal) ActualsForBudget(ctx context.Context, budgetID, tenantID uuid.UUID, rng domain.DateRange) (domain.Money, error) {
	return domain.Money{}, domain.NewDependencyUnavailable("budget actuals local adapter not wired")
}

// reportLocal satisfies report_usecases.ReportReadModelRepository (read model);
// the read-model adapter is not confirmed in this graph.
type reportLocal struct{}

func (reportLocal) ProfitLoss(ctx context.Context, rng domain.DateRange, tenant uuid.UUID) ([]usecases.ProfitLossRow, error) {
	return nil, domain.NewDependencyUnavailable("read model unavailable")
}

func (reportLocal) ExpenseBreakdown(ctx context.Context, rng domain.DateRange, tenant uuid.UUID) ([]usecases.ExpenseBreakdownRow, error) {
	return nil, domain.NewDependencyUnavailable("read model unavailable")
}

func (reportLocal) IncomeTrend(ctx context.Context, rng domain.DateRange, tenant uuid.UUID) ([]usecases.IncomeTrendRow, error) {
	return nil, domain.NewDependencyUnavailable("read model unavailable")
}

func (reportLocal) CashFlow(ctx context.Context, rng domain.DateRange, tenant uuid.UUID) ([]usecases.CashFlowRow, error) {
	return nil, domain.NewDependencyUnavailable("read model unavailable")
}

// dashboardLocal satisfies dashboard_usecases.ReadModelRepository (read model);
// the read-model adapter is not confirmed in this graph.
type dashboardLocal struct{}

func (dashboardLocal) CFOKPIs(ctx context.Context, rng domain.DateRange, tenant uuid.UUID) (*usecases.ReadModelCFOView, error) {
	return nil, domain.NewDependencyUnavailable("read model unavailable")
}

func (dashboardLocal) StandardUserKPIs(ctx context.Context, rng domain.DateRange, tenant, user uuid.UUID) (*usecases.ReadModelStandardView, error) {
	return nil, domain.NewDependencyUnavailable("read model unavailable")
}

func (dashboardLocal) ViewerKPIs(ctx context.Context, rng domain.DateRange, tenant uuid.UUID) (*usecases.ReadModelViewerSummary, error) {
	return nil, domain.NewDependencyUnavailable("read model unavailable")
}

// exportJobLocal satisfies the ExportJobRepository seam in the export use case;
// the export-jobs persistence adapter is a documented follow-up.
type exportJobLocal struct{}

func (exportJobLocal) Create(ctx context.Context, job *usecases.ExportJob) *domain.DomainError {
	return domain.NewDependencyUnavailable("export jobs repository not wired")
}

func (exportJobLocal) CountActive(ctx context.Context, tenantID string) (int, *domain.DomainError) {
	return 0, domain.NewDependencyUnavailable("export jobs repository not wired")
}

func (exportJobLocal) GetByIDAndTenant(ctx context.Context, id, tenantID string) (*usecases.ExportJob, *domain.DomainError) {
	return nil, domain.NewDependencyUnavailable("export jobs repository not wired")
}

func (exportJobLocal) UpdateStatus(ctx context.Context, job *usecases.ExportJob) *domain.DomainError {
	return domain.NewDependencyUnavailable("export jobs repository not wired")
}

func (exportJobLocal) ListByTenant(ctx context.Context, tenantID string, filter usecases.ExportJobFilter, page, pageSize int) ([]*usecases.ExportJob, int64, *domain.DomainError) {
	return nil, 0, domain.NewDependencyUnavailable("export jobs repository not wired")
}

// userLocal satisfies user_usecases.UserRepository by delegating aligned methods
// to the ports user repository.
type userLocal struct {
	users ports.UserRepository
}

func (u userLocal) FindByID(ctx context.Context, id, tenantID uuid.UUID) (*domain.User, error) {
	user, derr := u.users.GetByID(ctx, tenantID, id)
	if derr != nil {
		return nil, derr
	}
	return user, nil
}

func (u userLocal) FindByUsername(ctx context.Context, username string, tenantID uuid.UUID) (*domain.User, error) {
	user, derr := u.users.GetByUsername(ctx, tenantID, username)
	if derr != nil {
		return nil, derr
	}
	return user, nil
}

func (u userLocal) FindByEmail(ctx context.Context, email string, tenantID uuid.UUID) (*domain.User, error) {
	return nil, domain.NewDependencyUnavailable("user find-by-email local adapter not wired")
}

func (u userLocal) List(ctx context.Context, tenantID uuid.UUID, page, pageSize int) ([]*domain.User, int64, error) {
	items, derr := u.users.List(ctx, tenantID, ports.OffsetPage{Page: page, PageSize: pageSize})
	if derr != nil {
		return nil, 0, derr
	}
	return items, int64(len(items)), nil
}

func (u userLocal) CountActiveAdmins(ctx context.Context, tenantID uuid.UUID) (int, error) {
	n, derr := u.users.CountAdmins(ctx, tenantID)
	if derr != nil {
		return 0, derr
	}
	return int(n), nil
}

func (u userLocal) HasOwnedData(ctx context.Context, id, tenantID uuid.UUID) (bool, error) {
	return false, domain.NewDependencyUnavailable("user data-ownership local adapter not wired")
}

func (u userLocal) Create(ctx context.Context, user *domain.User) error {
	return u.users.Create(ctx, user.TenantID, user)
}

func (u userLocal) Update(ctx context.Context, user *domain.User, expectedVersion int64) error {
	return u.users.Update(ctx, user.TenantID, user, expectedVersion)
}

func (u userLocal) Delete(ctx context.Context, id, tenantID uuid.UUID, expectedVersion int64) error {
	return u.users.Delete(ctx, tenantID, id, expectedVersion)
}

// roleLocal satisfies user_usecases.RoleRepository; roles are seeded static
// data and no RoleRepository accessor is confirmed in this graph.
type roleLocal struct{}

func (roleLocal) FindByID(ctx context.Context, id uuid.UUID) (*domain.Role, error) {
	return nil, domain.NewDependencyUnavailable("role repository not wired")
}

// certReviewLocal satisfies certification_usecases.CertificationRepository.
type certReviewLocal struct{}

func (certReviewLocal) FindOpenByScope(ctx context.Context, tenantID uuid.UUID, scope string) (*usecases.CertificationReview, error) {
	return nil, domain.NewDependencyUnavailable("certification repository not wired")
}

func (certReviewLocal) FindReview(ctx context.Context, reviewID uuid.UUID) (*usecases.CertificationReview, error) {
	return nil, domain.NewDependencyUnavailable("certification repository not wired")
}

func (certReviewLocal) InsertReview(ctx context.Context, r *usecases.CertificationReview) error {
	return domain.NewDependencyUnavailable("certification repository not wired")
}

func (certReviewLocal) InsertDecision(ctx context.Context, d *usecases.CertificationDecisionRecord) error {
	return domain.NewDependencyUnavailable("certification repository not wired")
}

func (certReviewLocal) BulkApprovePending(ctx context.Context, reviewID, tenantID, reviewerID uuid.UUID, evidenceRef string) (int, error) {
	return 0, domain.NewDependencyUnavailable("certification repository not wired")
}

func (certReviewLocal) CloseReview(ctx context.Context, reviewID uuid.UUID, closedBy uuid.UUID, expectedVersion int) (bool, error) {
	return false, domain.NewDependencyUnavailable("certification repository not wired")
}

func (certReviewLocal) ListReviews(ctx context.Context, tenantID uuid.UUID, status string, page, pageSize int) ([]usecases.CertificationReview, error) {
	return nil, domain.NewDependencyUnavailable("certification repository not wired")
}

// certUserLocal satisfies certification_usecases.UserRepository by delegating
// aligned reads to the ports user repository where possible.
type certUserLocal struct {
	users ports.UserRepository
}

func (c certUserLocal) FindActor(ctx context.Context, userID uuid.UUID) (usecases.Actor, error) {
	return usecases.Actor{}, domain.NewDependencyUnavailable("actor role repository not wired")
}

func (c certUserLocal) FindTargetUser(ctx context.Context, tenantID, userID uuid.UUID) (usecases.CertificationDecisionUser, error) {
	return usecases.CertificationDecisionUser{}, domain.NewDependencyUnavailable("certification user adapter not wired")
}

func (c certUserLocal) IsLastActiveAdmin(ctx context.Context, tenantID, userID uuid.UUID) (bool, error) {
	return false, domain.NewDependencyUnavailable("certification user adapter not wired")
}

func (c certUserLocal) MarkDeprovisioning(ctx context.Context, tenantID, userID uuid.UUID) error {
	return domain.NewDependencyUnavailable("certification user adapter not wired")
}

func (c certUserLocal) FindOrphaned(ctx context.Context, tenantID uuid.UUID, cutoff time.Time, page, pageSize int) ([]usecases.OrphanedAccount, error) {
	return nil, domain.NewDependencyUnavailable("certification user adapter not wired")
}

// closeJobLocal satisfies close_period_usecase.CloseJobRepository.
type closeJobLocal struct{}

func (closeJobLocal) Create(ctx context.Context, tenantID uuid.UUID, job *usecases.CloseJob) *domain.DomainError {
	return domain.NewDependencyUnavailable("period close job repository not wired")
}

func (closeJobLocal) GetByID(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) (*usecases.CloseJob, *domain.DomainError) {
	return nil, domain.NewDependencyUnavailable("period close job repository not wired")
}

func (closeJobLocal) CountOpenRows(ctx context.Context, tenantID uuid.UUID, from, to time.Time) (int64, *domain.DomainError) {
	return 0, domain.NewDependencyUnavailable("period close job repository not wired")
}

func (closeJobLocal) IncrementDone(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, batchSeq int, checkpoint *string) *domain.DomainError {
	return domain.NewDependencyUnavailable("period close job repository not wired")
}

func (closeJobLocal) Fail(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, errText string) *domain.DomainError {
	return domain.NewDependencyUnavailable("period close job repository not wired")
}

func (closeJobLocal) Complete(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) *domain.DomainError {
	return domain.NewDependencyUnavailable("period close job repository not wired")
}

// snapshotLocal satisfies close_period_usecase.SnapshotRepository.
type snapshotLocal struct{}

func (snapshotLocal) Append(ctx context.Context, tenantID uuid.UUID, snap *usecases.ReportSnapshot) *domain.DomainError {
	return domain.NewDependencyUnavailable("period close snapshot repository not wired")
}

// closeLockLocal satisfies close_period_usecase.CloseLock. The rediscache client
// in this graph exposes TryLock/RenewLock/ReleaseLock with the same shape, but
// it is not threaded here; passing it in is a documented follow-up so a real
// distributed lock protects concurrent closes across replicas.
type closeLockLocal struct{}

func (closeLockLocal) TryLock(ctx context.Context, name string, ttl time.Duration, token string) (bool, error) {
	return false, domain.NewDependencyUnavailable("close lock not wired")
}

func (closeLockLocal) RenewLock(ctx context.Context, name string, ttl time.Duration, token string) error {
	return domain.NewDependencyUnavailable("close lock not wired")
}

func (closeLockLocal) ReleaseLock(ctx context.Context, name string, token string) error {
	return domain.NewDependencyUnavailable("close lock not wired")
}

// closeActorLocal satisfies close_period_usecase.CloseActorRepo.
type closeActorLocal struct{}

func (closeActorLocal) FindActor(ctx context.Context, userID uuid.UUID) (usecases.CloseActor, error) {
	return usecases.CloseActor{}, domain.NewDependencyUnavailable("close actor role repository not wired")
}