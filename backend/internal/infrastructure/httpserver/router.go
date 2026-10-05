// Package httpserver is the versioned REST gateway root for the FMS backend.
// It is the outermost Clean Architecture interface adapter (Blueprint §5.1,
// §7.1/§7.2): it mounts every /v1/* resource plus /health, applies the
// mandated middleware chain, and returns RFC 7807 error envelopes. It contains
// zero business logic — the use cases and domain own every rule.
//
// The file is intentionally self-contained. The sibling handler and middleware
// packages referenced by the original change description do not exist in this
// repository and are not in the dependency contract, so the gateway's middleware
// and handlers are defined inline here. No project file outside the dependency
// contract is imported; ports, use cases, config, and domain are the only
// internal dependencies.
//
// Reconciling the change description's Deps with the read routes the constructed
// use cases do not expose: GET /accounts/{id} is served from
// ports.AccountRepository.GetByID and GET /audit-logs from
// ports.AuditLogRepository.List. Mutations always flow through the use cases so
// business invariants are never bypassed by the gateway.
package httpserver

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/application/usecases"
	"fms/backend/internal/config"
	"fms/backend/internal/domain"
)

// Rate-limit budgets per route class (specification §7.2). Auth endpoints are
// capped hardest as the brute-force defense; exports are capped to bound the
// async worker queue; everything else is the general interactive budget.
const (
	authRateLimit   = 10
	exportRateLimit = 20
	generalRate     = 100

	rateWindow = 60 * time.Second

	// idempotencyTTL bounds how long a stored Idempotency-Key response may be
	// replayed, bounding Redis memory (§7.2).
	idempotencyTTL = 24 * time.Hour
	// idempotencyErrorTTL shortens the lifetime of a stored error response so a
	// retry after a transient failure gets a fresh attempt (IdempotencyStore doc).
	idempotencyErrorTTL = 30 * time.Second

	// defaultBodyMaxBytes caps request bodies on every route unless a route
	// raises it (security: memory-exhaustion DoS defense).
	defaultBodyMaxBytes = 1 << 20 // 1 MiB

	// healthProbeTimeout bounds the whole readiness probe batch so a hanging
	// dependency cannot block the readiness gate (§9.3).
	healthProbeTimeout = 500 * time.Millisecond

	// workerHeartbeatStaleThreshold is how old the scheduler heartbeat may be
	// before readiness reports the worker stale (§9.2).
	workerHeartbeatStaleThreshold = 60 * time.Second
	// replicaLagThreshold is the max tolerated read-model lag before readiness
	// fails (§9.4).
	replicaLagThreshold = 30 * time.Second
)

// Deps is the composition-root input to NewRouter. It aggregates the constructed
// application services (which hold the business logic), the output ports the
// gateway middleware and probes consume, and configuration. Only fields actually
// consumed by middleware, handlers, or probes are present; framework/driver
// handles are absent because use cases and their repository adapters are
// constructed by the composition root, not here.
type Deps struct {
	Config *config.Config
	Flags  config.FeatureFlags

	// Constructed application services (composition root supplies them).
	Accounts       *usecases.AccountService
	Transactions   *usecases.TransactionService
	Categories     *usecases.CategoryService
	Budgets        *usecases.BudgetService
	Reports        *usecases.ReportService
	Exports        *usecases.ExportService
	Auth           *usecases.AuthService
	Users          *usecases.UserService
	Certifications *usecases.CertificationService
	Dashboard      *usecases.DashboardService
	ClosePeriod    *usecases.ClosePeriodUseCase

	// Read-only repo ports for capabilities the use cases do not expose
	// (GET /accounts/{id}, GET /audit-logs). These are never used for writes.
	AccountRepo ports.AccountRepository
	AuditLogs   ports.AuditLogRepository

	// Tokens is consumed only to read the verified session/claims for the
	// server-side inactivity Touch; the AuthService remains the authoritative
	// authentication path.
	Tokens      ports.TokenManager
	Sessions    ports.SessionStore
	Idempotency ports.IdempotencyStore
	Cache       ports.Cache
	Events      ports.EventBus
	Clock       ports.Clock

	// DefaultTenant is the server-side tenant scope applied to unauthenticated
	// login. Resolved from configuration, never from client input (§7.8).
	DefaultTenant uuid.UUID

	// Permissions resolves the RBAC permission set for an authenticated
	// principal (data-not-code). A nil resolver fails closed (every protected
	// route is FORBIDDEN).
	Permissions PermissionResolver

	// HealthProbe drives the readiness gate; nil-safe no-op probes keep the
	// standalone router servable (pluggable-boundary invariant, §11 §9.1).
	Health HealthProbe
}

// HealthProbe abstracts the dependency probes run by the readiness endpoint
// (§9.2). It takes no framework/driver handles — the concrete adapter owns the
// *sql.DB/redis/AMQP handles behind the interface, and each probe uses pooled
// connections only and returns within a short bounded budget.
type HealthProbe interface {
	PingPostgres(ctx context.Context) error
	PingRedis(ctx context.Context) error
	PingAMQP(ctx context.Context) error
	// WorkerHeartbeat returns the last scheduler heartbeat marker time, or an
	// error when the marker is absent.
	WorkerHeartbeat(ctx context.Context) (time.Time, error)
	// ReplicaLag returns the current read-replica lag.
	ReplicaLag(ctx context.Context) (time.Duration, error)
}

// PermissionResolver maps an authenticated principal to its granted RBAC
// permissions (the permission matrix is data, not code).
type PermissionResolver interface {
	// PermissionsFor returns the granted permission set for userID, or a
	// non-nil error when the role cannot be resolved (fail closed).
	PermissionsFor(ctx context.Context, userID, tenantID uuid.UUID) ([]domain.Permission, error)
}

// NewRouter composes the FMS REST gateway. It registers the outermost
// error/security middleware, the unauthenticated health endpoints, and the /v1
// sub-router with its mandated middleware chain and route table. It returns an
// http.Handler (the interface, not a concrete chi.Router) so the seam stays
// replaceable and testable.
//
// Middleware order is fixed: recoverPanic (outermost) → correlation → auth →
// RBAC → tenant → rate-limit → idempotency → read-audit[flag] → handler.
func NewRouter(deps Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(securityHeadersMiddleware)
	r.Use(recoverPanicMiddleware)

	r.Get("/health", livenessHandler)
	r.Get("/health/ready", readinessHandler(deps))

	r.Mount("/v1", newV1Router(deps))

	return r
}

// ---------- Deps helpers ----------

func (d Deps) sessionTTL() time.Duration {
	if d.Config != nil && d.Config.Security.SessionTimeout > 0 {
		return d.Config.Security.SessionTimeout
	}
	return 30 * time.Minute
}

func (d Deps) nowUTC() time.Time {
	if d.Clock != nil {
		return d.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

// ---------- context keys & principal ----------

type ctxKey int

const (
	ctxKeyCorrelation ctxKey = iota
	ctxKeyPrincipal
	ctxKeyTenant
)

// principal is the IAM-resolved actor for one request, injected exactly once by
// the auth middleware and consumed (never re-derived) by downstream middleware
// and handlers.
type principal struct {
	UserID   uuid.UUID
	TenantID uuid.UUID
	Role     string
}

type tenantContext struct {
	TenantID uuid.UUID
	GroupIDs []uuid.UUID // reserved extension slot; groups are not modelled by the domain
}

func correlationFromCtx(ctx context.Context) string {
	if id, ok := ctx.Value(ctxKeyCorrelation).(string); ok {
		return id
	}
	return ""
}

func principalFromCtx(ctx context.Context) *principal {
	if p, ok := ctx.Value(ctxKeyPrincipal).(*principal); ok {
		return p
	}
	return nil
}

func tenantFromCtx(ctx context.Context) *tenantContext {
	if t, ok := ctx.Value(ctxKeyTenant).(*tenantContext); ok {
		return t
	}
	return nil
}

// ---------- correlation middleware ----------

// correlationIDRE validates an inbound correlation id to prevent log injection
// through a crafted header (CWE-117). Same allowlist as the domain event
// envelope; unknown input is regenerated rather than propagated.
var correlationIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func correlationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("correlation_id"))
		if id == "" || !correlationIDRE.MatchString(id) {
			id = newCorrelationID()
		}
		ctx := context.WithValue(r.Context(), ctxKeyCorrelation, id)
		w.Header().Set("X-Correlation-Id", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// newCorrelationID generates `req_` + 16 hex bytes, matching the tracing spine
// used by logs, spans, and outbox events.
func newCorrelationID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "req_" + uuid.NewString()
	}
	return "req_" + hex.EncodeToString(b)
}

// ---------- auth middleware ----------

func authMiddleware(deps Deps) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Fail-closed: every /v1 route behind this middleware must carry a
			// Bearer token; no route defaults to anonymous.
			token, ok := bearerToken(r)
			if !ok {
				WriteError(w, r, domain.NewUnauthenticated("authentication required"))
				return
			}

			// Authoritative authentication: signature, expiry, revocation, and a
			// re-check that the account still exists and is active in the tenant.
			user, err := deps.Auth.Authenticate(r.Context(), token)
			if err != nil {
				WriteError(w, r, err)
				return
			}

			// Enforce the server-side inactivity window by sliding the session TTL.
			// VerifyAccess also re-checks expiry/revocation as defense-in-depth.
			claims, verr := deps.Tokens.VerifyAccess(r.Context(), token)
			if verr != nil {
				WriteError(w, r, verr)
				return
			}
			if !claims.SessionID.IsZero() {
				if terr := deps.Sessions.Touch(r.Context(), claims.SessionID, deps.sessionTTL()); terr != nil {
					WriteError(w, r, terr)
					return
				}
			}

			p := &principal{
				UserID:   user.ID,
				TenantID: user.TenantID,
				Role:     user.RoleID.String(),
			}
			ctx := context.WithValue(r.Context(), ctxKeyPrincipal, p)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	t := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	if t == "" || strings.ContainsAny(t, " \t\r\n") {
		return "", false
	}
	return t, true
}

// ---------- RBAC middleware ----------

// routeRequiresPermission maps an HTTP verb + route pattern to the single
// required permission (data, not code). A route absent from this map, or a
// principal with zero resolved permissions, fails closed (FORBIDDEN).
var routeRequiresPermission = map[string]domain.Permission{
	"POST /accounts":                domain.PermissionAccountWrite,
	"GET /accounts":                 domain.PermissionAccountRead,
	"GET /accounts/{id}":            domain.PermissionAccountRead,
	"PATCH /accounts/{id}":          domain.PermissionAccountWrite,
	"DELETE /accounts/{id}":         domain.PermissionAccountWrite,
	"POST /accounts/{id}/reconcile": domain.PermissionAccountWrite,

	"POST /transactions":        domain.PermissionTransactionWrite,
	"GET /transactions":         domain.PermissionTransactionRead,
	"GET /transactions/{id}":    domain.PermissionTransactionRead,
	"PATCH /transactions/{id}":  domain.PermissionTransactionWrite,
	"DELETE /transactions/{id}": domain.PermissionTransactionWrite,

	"POST /categories":            domain.PermissionCategoryWrite,
	"GET /categories":             domain.PermissionCategoryRead,
	"POST /categories/{id}/merge": domain.PermissionCategoryWrite,

	"POST /budgets":               domain.PermissionBudgetWrite,
	"GET /budgets":                domain.PermissionBudgetRead,
	"GET /budgets/{id}/vs-actual": domain.PermissionBudgetRead,

	"GET /reports/{kind}": domain.PermissionReportRead,

	"POST /exports":              domain.PermissionExportCreate,
	"GET /exports/{id}":          domain.PermissionExportCreate,
	"GET /exports/{id}/download": domain.PermissionExportCreate,

	"POST /users":        domain.PermissionUserManage,
	"GET /users":         domain.PermissionUserManage,
	"GET /users/{id}":    domain.PermissionUserManage,
	"PATCH /users/{id}":  domain.PermissionUserManage,
	"DELETE /users/{id}": domain.PermissionUserManage,

	"GET /audit-logs": domain.PermissionAuditRead,

	"POST /certifications": domain.PermissionCertificationManage,
	"GET /certifications":  domain.PermissionCertificationManage,

	"GET /dashboard/kpis": domain.PermissionDashboardRead,

	// Logout is authenticated-only: any authenticated principal may terminate
	// their own session, so it requires no resource permission (empty value).
	"POST /auth/logout": "",
}

func rbacMiddleware(deps Deps) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := principalFromCtx(r.Context())
			if p == nil {
				WriteError(w, r, domain.NewForbidden("access denied"))
				return
			}
			routeKey := r.Method + " " + chi.RouteContext(r.Context()).RoutePattern()
			required, ok := routeRequiresPermission[routeKey]
			if !ok {
				// Unregistered method+route fails closed, never default-allows.
				WriteError(w, r, domain.NewForbidden("access denied"))
				return
			}
			if required == "" {
				// Authenticated-only route (e.g. logout).
				next.ServeHTTP(w, r)
				return
			}
			if deps.Permissions == nil {
				WriteError(w, r, domain.NewForbidden("access denied"))
				return
			}
			perms, err := deps.Permissions.PermissionsFor(r.Context(), p.UserID, p.TenantID)
			if err != nil || !hasPermission(perms, required) {
				WriteError(w, r, domain.NewForbidden("access denied"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func hasPermission(perms []domain.Permission, required domain.Permission) bool {
	for _, p := range perms {
		if p == required {
			return true
		}
	}
	return false
}

// ---------- tenant middleware ----------

// tenantMiddleware injects the server-derived tenant predicate into the request
// context exactly once. It never accepts a tenant from a header, query, or path.
// Handlers and repositories read this predicate; they never re-parse claims.
func tenantMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalFromCtx(r.Context())
		if p == nil || p.TenantID == uuid.Nil {
			WriteError(w, r, domain.NewForbidden("access denied"))
			return
		}
		tctx := &tenantContext{TenantID: p.TenantID}
		ctx := context.WithValue(r.Context(), ctxKeyTenant, tctx)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ---------- rate-limit middleware ----------

func rateLimitLimit(r *http.Request) int {
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/v1/auth/"):
		return authRateLimit
	case strings.HasPrefix(path, "/v1/exports"):
		return exportRateLimit
	default:
		return generalRate
	}
}

func ratelimitMiddleware(deps Deps) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			limit := rateLimitLimit(r)
			if deps.Cache == nil {
				// No cache ⇒ the limiter cannot enforce; the composition root must
				// supply a cache for the mandated brute-force and quota defense.
				next.ServeHTTP(w, r)
				return
			}
			key := "fms:v1:ratelimit:" + rateLimitClassKey(r) + ":" + clientKey(r)
			remaining, reset := slidingWindowAllow(deps.Cache, key, limit, rateWindow)
			setRateLimitHeaders(w, limit, remaining, reset)
			if remaining <= 0 {
				resetSec := int(time.Now().Unix()%60) + 1
				w.Header().Set("Retry-After", strconv.Itoa(resetSec))
				WriteError(w, r, domain.NewRateLimited(resetSec))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func rateLimitClassKey(r *http.Request) string {
	switch {
	case strings.HasPrefix(r.URL.Path, "/v1/auth/"):
		return "auth"
	case strings.HasPrefix(r.URL.Path, "/v1/exports"):
		return "export"
	default:
		return "general"
	}
}

func clientKey(r *http.Request) string {
	if p := principalFromCtx(r.Context()); p != nil && !p.UserID.IsZero() {
		return p.UserID.String()
	}
	if h := r.RemoteAddr; h != "" {
		if host, _, err := net.SplitHostPort(h); err == nil {
			return host
		}
		return h
	}
	return "unknown"
}

// slidingWindowAllow returns the remaining budget for key and the reset second.
// It is a Redis-backed sliding-window counter (Cache port); a cache error is
// treated as a permit (rate state is best-effort, never the ledger).
func slidingWindowAllow(cache ports.Cache, key string, limit int, window time.Duration) (int, int) {
	ctx := context.Background()
	raw, err := cache.Get(ctx, key)
	count := 0
	if err == nil && raw != nil {
		if v, perr := strconv.Atoi(string(raw)); perr == nil {
			count = v
		}
	}
	if count >= limit {
		return 0, int(time.Now().Unix()%60) + 1
	}
	if cerr := cache.Set(ctx, key, []byte(strconv.Itoa(count+1)), window); cerr != nil {
		return limit - count - 1, int(time.Now().Unix()%60) + 1
	}
	return limit - (count + 1), int(time.Now().Unix()%60) + 1
}

func setRateLimitHeaders(w http.ResponseWriter, limit, remaining, reset int) {
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	w.Header().Set("X-RateLimit-Reset", strconv.Itoa(reset))
}

// ---------- idempotency middleware ----------

var idemKeyRE = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,64}$`)

func idempotencyMiddleware(deps Deps) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isWriteMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			raw := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
			if raw == "" {
				next.ServeHTTP(w, r)
				return
			}
			if !idemKeyRE.MatchString(raw) {
				WriteError(w, r, domain.NewValidationError(domain.FieldError{
					Field:   "Idempotency-Key",
					Message: "must be 1-64 ASCII letters, digits, or . _ ~ -",
				}))
				return
			}

			p := principalFromCtx(r.Context())
			t := tenantFromCtx(r.Context())
			if p == nil || t == nil || deps.Idempotency == nil {
				WriteError(w, r, domain.NewForbidden("access denied"))
				return
			}
			key := idempotencyKey(t.TenantID, p.UserID, r.Method, r.URL.Path, raw)

			if stored, ok, err := deps.Idempotency.Get(r.Context(), key); err != nil {
				WriteError(w, r, err)
				return
			} else if ok {
				replayStored(w, stored, r)
				return
			}

			claimed, err := deps.Idempotency.StoreIfAbsent(r.Context(), key, []byte(`{}`), idempotencyTTL)
			if err != nil {
				WriteError(w, r, err)
				return
			}
			if !claimed {
				// A duplicate is in flight; poll briefly for the winner's response.
				for i := 0; i < 50; i++ {
					time.Sleep(20 * time.Millisecond)
					if stored, ok, err := deps.Idempotency.Get(r.Context(), key); err == nil && ok {
						replayStored(w, stored, r)
						return
					}
				}
				WriteError(w, r, domain.NewConflict("duplicate request in flight; retry"))
				return
			}

			// Winner: capture the response and persist it for duplicate replay.
			pw := &captureWriter{ResponseWriter: w}
			next.ServeHTTP(pw, r)

			payload, merr := json.Marshal(capturedResponse{
				Status: pw.status,
				Header: pw.header(),
				Body:   pw.body,
			})
			if merr != nil {
				return
			}
			ttl := idempotencyTTL
			if pw.status >= 400 {
				ttl = idempotencyErrorTTL
			}
			_ = deps.Idempotency.Store(r.Context(), key, payload, ttl)
		})
	}
}

// capturedResponse is the serialized HTTP response stored per Idempotency-Key.
type capturedResponse struct {
	Status int                 `json:"status"`
	Header map[string][]string `json:"header,omitempty"`
	Body   []byte              `json:"body"`
}

func replayStored(w http.ResponseWriter, raw []byte, r *http.Request) {
	var resp capturedResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		WriteError(w, r, domain.NewInternal(err))
		return
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.Status)
	if len(resp.Body) > 0 {
		_, _ = w.Write(resp.Body)
	}
}

// captureWriter buffers status, headers, and body so the idempotency middleware
// can persist and replay a handled response.
type captureWriter struct {
	http.ResponseWriter
	status    int
	header    http.Header
	body      []byte
	headerSet bool
}

func (c *captureWriter) WriteHeader(code int) {
	if !c.headerSet {
		c.status = code
		c.header = c.ResponseWriter.Header().Clone()
		c.headerSet = true
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if !c.headerSet {
		c.status = http.StatusOK
		c.header = c.ResponseWriter.Header().Clone()
		c.headerSet = true
	}
	c.body = append(c.body, b...)
	return c.ResponseWriter.Write(b)
}

func (c *captureWriter) header() map[string][]string {
	return c.header
}

// idempotencyKey derives a composite, tenant/user-scoped dedupe key from the
// authenticated context, never from raw client input (anti cross-user replay).
func idempotencyKey(tenantID, userID uuid.UUID, method, path, raw string) string {
	h := sha256.Sum256([]byte(method + "|" + path + "|" + raw))
	return "fms:v1:idem:" + tenantID.String() + ":" + userID.String() + ":" + hex.EncodeToString(h[:])
}

func isWriteMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPatch, http.MethodDelete, http.MethodPut:
		return true
	default:
		return false
	}
}

// ---------- read-audit middleware ----------

func readAuditMiddleware(deps Deps) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !deps.Flags.AuditReadLoggingEnabled() {
				next.ServeHTTP(w, r)
				return
			}
			p := principalFromCtx(r.Context())
			t := tenantFromCtx(r.Context())
			// Capture only resource + accessor identity (never response payloads),
			// published asynchronously off the request path (§11 §9.4).
			if deps.Events != nil && p != nil && t != nil {
				event := domain.Event{
					ID:            uuid.New(),
					Type:          domain.EventEntityModified,
					Version:       1,
					Timestamp:     deps.nowUTC(),
					TenantID:      t.TenantID,
					ActorID:       p.UserID,
					CorrelationID: correlationFromCtx(r.Context()),
					SourceService: "http-gateway",
					AggregateType: "audit.read",
					AggregateID:   r.Method + " " + r.URL.Path,
					Sequence:      1,
				}
				// Fire-and-forget; an audit failure never fails the read path.
				_ = deps.Events.Publish(context.WithoutCancel(r.Context()), event)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ---------- security headers, panic recovery, error envelope ----------

// corsAllowlist is the explicit origin allowlist. Credentials are allowed only
// for allowlisted origins, never reflected from an arbitrary inbound Origin.
var corsAllowlist = map[string]bool{
	"https://app.fms.example": true, // placeholder; production origins are configured in deployment
}

func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")

		if origin := r.Header.Get("Origin"); corsAllowlist[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Vary", "Origin")
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

func recoverPanicMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				WriteError(w, r, domain.NewInternal(fmt.Errorf("panic: %v", rec)))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// errorBody is the RFC 7807-style error envelope (§7.7). It emits only code,
// message, field errors, and correlation id — never stack traces, SQL strings,
// or internal package paths.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code          string              `json:"code"`
	Message       string              `json:"message"`
	FieldErrors   []domain.FieldError `json:"field_errors,omitempty"`
	CorrelationID string              `json:"correlation_id"`
}

// WriteError is the single error-dispatch path for every handler and middleware
// failure. It maps a domain.Error code to its HTTP status and always emits the
// correlation id. It never writes a stack trace or an unwrapped cause.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	body := errorBody{Error: errorDetail{
		Code:    string(domain.CodeInternalError),
		Message: "internal error",
	}}
	if de, ok := domain.AsDomainError(err); ok {
		status = de.HTTPStatus()
		body.Error.Code = string(de.Code)
		if de.Message != "" {
			body.Error.Message = de.Message
		}
		if de.FieldErrors != nil {
			body.Error.FieldErrors = de.FieldErrors
		}
		if de.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(de.RetryAfter))
		}
	}
	if r != nil {
		body.Error.CorrelationID = correlationFromCtx(r.Context())
	}
	w.Header().Set("Content-Type", "application/problem+json")
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="api", error="invalid_token"`)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeJSON is the success-path encoder used by handlers, keeping the response
// envelope consistent and routing JSON marshal errors to WriteError.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		WriteError(w, r, domain.NewInternal(err))
	}
}

// ---------- health handlers ----------

func livenessHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

// readinessHandler runs the dependency probes under a single global timeout and
// reports structured, topology-safe status; on any critical failure it returns
// 503 (§9.2/§9.3).
func readinessHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		probes := map[string]string{}

		if deps.Health == nil {
			for _, name := range []string{"postgres", "redis", "amqp", "worker_heartbeat", "replica_lag"} {
				probes[name] = "ok"
			}
			writeJSON(w, r, http.StatusOK, map[string]any{
				"status": "ready", "checks": probes, "correlation_id": correlationFromCtx(r.Context()),
			})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), healthProbeTimeout)
		defer cancel()

		postgresErr := deps.Health.PingPostgres(ctx)
		redisErr := deps.Health.PingRedis(ctx)
		amqpErr := deps.Health.PingAMQP(ctx)
		probes["postgres"] = probeResult(postgresErr)
		probes["redis"] = probeResult(redisErr)
		probes["amqp"] = probeResult(amqpErr)

		if hb, err := deps.Health.WorkerHeartbeat(ctx); err != nil {
			probes["worker_heartbeat"] = "stale: heartbeat unavailable"
		} else if age := deps.nowUTC().Sub(hb); age > workerHeartbeatStaleThreshold {
			probes["worker_heartbeat"] = fmt.Sprintf("stale: %s ago", age.Round(time.Second))
		} else {
			probes["worker_heartbeat"] = "ok"
		}

		if lag, err := deps.Health.ReplicaLag(ctx); err != nil {
			probes["replica_lag"] = "unknown"
		} else if lag > replicaLagThreshold {
			probes["replica_lag"] = fmt.Sprintf("lagging: %s", lag.Round(time.Second))
		} else {
			probes["replica_lag"] = "ok"
		}

		if ctx.Err() != nil || postgresErr != nil {
			w.Header().Set("Retry-After", "5")
			writeJSON(w, r, http.StatusServiceUnavailable, map[string]any{
				"status": "unavailable", "checks": probes, "correlation_id": correlationFromCtx(r.Context()),
			})
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{
			"status": "ready", "checks": probes, "correlation_id": correlationFromCtx(r.Context()),
		})
	}
}

func probeResult(err error) string {
	if err == nil {
		return "ok"
	}
	// Generic dependency status only; never the underlying error text (which may
	// embed connection strings or addresses).
	return "unavailable"
}

// ---------- input helpers ----------

// parseCursorPage extracts cursor pagination params for high-volume collections
// (transactions). limit defaults to 100 and is rejected (VALIDATION_FAILED) if
// outside [1,100] — an explicit contract, never silently clamped.
func parseCursorPage(r *http.Request) (ports.CursorPage, error) {
	q := r.URL.Query()
	page := ports.CursorPage{Limit: 100}
	if raw := q.Get("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > 100 {
			return page, domain.NewValidationError(domain.FieldError{Field: "limit", Message: "must be an integer in 1..100"})
		}
		page.Limit = v
	}
	if c := q.Get("cursor"); c != "" {
		page.Cursor = &c
	}
	return page, nil
}

// parseOffsetPage extracts offset pagination params for bounded collections.
// page defaults to 1; page_size defaults to 100 and is validated to [1,100].
func parseOffsetPage(r *http.Request) (int, int, error) {
	q := r.URL.Query()
	page, size := 1, 100
	if raw := q.Get("page"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 {
			return page, size, domain.NewValidationError(domain.FieldError{Field: "page", Message: "must be an integer >= 1"})
		}
		page = v
	}
	if raw := q.Get("page_size"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > 100 {
			return page, size, domain.NewValidationError(domain.FieldError{Field: "page_size", Message: "must be an integer in 1..100"})
		}
		size = v
	}
	return page, size, nil
}

func parseUUIDParam(r *http.Request, name string) (uuid.UUID, error) {
	raw := chi.URLParam(r, name)
	if raw == "" {
		return uuid.Nil, domain.NewValidationError(domain.FieldError{Field: name, Message: "is required"})
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, domain.NewValidationError(domain.FieldError{Field: name, Message: "must be a valid UUID"})
	}
	return id, nil
}

func parseBody(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, defaultBodyMaxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return domain.NewValidationError(domain.FieldError{Field: "body", Message: "malformed JSON body"})
	}
	return nil
}

func parseRangeQuery(r *http.Request) (domain.DateRange, error) {
	from := parseQueryTime(r, "from")
	to := parseQueryTime(r, "to")
	if from == nil || to == nil {
		return domain.DateRange{}, domain.NewValidationError(domain.FieldError{Field: "range", Message: "from and to (RFC3339) are required"})
	}
	return domain.NewDateRange(*from, *to)
}

func parseQueryTime(r *http.Request, name string) *time.Time {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return &t
	}
	return nil
}

func requireInt64Query(r *http.Request, name string, def int64) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, domain.NewValidationError(domain.FieldError{Field: name, Message: "must be an integer"})
	}
	return v, nil
}

func nextPage(page, pageSize int, total int64) *int {
	if int64(page*pageSize) < total {
		np := page + 1
		return &np
	}
	return nil
}

// roleClassFor returns the role-class dispatch bucket for dashboard KPIs. It is
// derived from the resolved permission set (data, not code): a principal with
// account+report read gets the executive CFO set, one with transaction read the
// personal standard set, and otherwise the read-only viewer set. dashboard:read
// was already enforced by RBAC before this handler runs.
func roleClassFor(perms []domain.Permission) string {
	if hasPermission(perms, domain.PermissionAccountRead) && hasPermission(perms, domain.PermissionReportRead) {
		return "cfo"
	}
	if hasPermission(perms, domain.PermissionTransactionRead) {
		return "standard"
	}
	return "viewer"
}

// ---------- auth handlers ----------

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func loginHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var req loginRequest
		if err := parseBody(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
		// Stage-1 login returns a 200 result carrying mfa_challenge_required and a
		// pending id; any failure (including an absent/invalid account) surfaces
		// the same generic UNAUTHENTICATED to defeat enumeration.
		res, err := deps.Auth.Login(r.Context(), usecases.LoginCommand{
			Username: req.Username,
			Password: req.Password,
		}, deps.DefaultTenant)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, res)
	}
}

type mfaRequest struct {
	Username  string `json:"username"`
	TOTPCode  string `json:"totp_code"`
	PendingID string `json:"pending_id"`
}

func mfaHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var req mfaRequest
		if err := parseBody(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
		res, err := deps.Auth.VerifyMfa(r.Context(), usecases.MfaCommand{
			Username:  req.Username,
			TOTPCode:  req.TOTPCode,
			PendingID: req.PendingID,
		})
		if err != nil {
			WriteError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, res)
	}
}

type refreshRequest struct {
	RefreshToken string    `json:"refresh_token"`
	SessionID    uuid.UUID `json:"session_id"`
}

func refreshHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var req refreshRequest
		if err := parseBody(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
		res, err := deps.Auth.RotateRefreshSingleUse(r.Context(), req.RefreshToken, req.SessionID)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, res)
	}
}

func logoutHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalFromCtx(r.Context())
		if p == nil {
			WriteError(w, r, domain.NewUnauthenticated("authentication required"))
			return
		}
		token, _ := bearerToken(r)
		var sessionID, tokenID uuid.UUID
		if token != "" {
			if claims, err := deps.Tokens.VerifyAccess(r.Context(), token); err == nil {
				sessionID = claims.SessionID
				tokenID = claims.TokenID
			}
		}
		if err := deps.Auth.Logout(r.Context(), sessionID, tokenID, p.UserID); err != nil {
			WriteError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---------- account handlers ----------

type accountCreateRequest struct {
	Name           string `json:"name"`
	Type           string `json:"type"`
	OpeningBalance string `json:"opening_balance"`
	Currency       string `json:"currency"`
}

func accountsCreateHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var req accountCreateRequest
		if err := parseBody(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
		typ, err := domain.ParseAccountType(req.Type)
		if err != nil {
			WriteError(w, r, domain.NewValidationError(domain.FieldError{Field: "type", Message: "must be one of checking, savings, credit, cash, other"}))
			return
		}
		ob, err := domain.Parse(req.OpeningBalance, req.Currency)
		if err != nil {
			WriteError(w, r, domain.NewValidationError(domain.FieldError{Field: "opening_balance", Message: "must be a non-negative decimal with at most two places"}))
			return
		}
		p := principalFromCtx(r.Context())
		acc, err := deps.Accounts.CreateAccount(r.Context(), usecases.CreateAccountCommand{
			Name:           req.Name,
			Type:           typ,
			OpeningBalance: ob,
			Currency:       req.Currency,
		}, p.TenantID, p.UserID)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusCreated, acc)
	}
}

func accountsListHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		page, size, err := parseOffsetPage(r)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		p := principalFromCtx(r.Context())
		res, err := deps.Accounts.ListAccounts(r.Context(), usecases.AccountFilter{}, usecases.PageParams{Page: page, PageSize: size}, p.TenantID)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, res)
	}
}

func accountGetHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseUUIDParam(r, "id")
		if err != nil {
			WriteError(w, r, err)
			return
		}
		p := principalFromCtx(r.Context())
		acc, derr := deps.AccountRepo.GetByID(r.Context(), p.TenantID, id)
		if derr != nil {
			WriteError(w, r, derr)
			return
		}
		writeJSON(w, r, http.StatusOK, acc)
	}
}

func accountUpdateHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseUUIDParam(r, "id")
		if err != nil {
			WriteError(w, r, err)
			return
		}
		var req struct {
			Name            *string `json:"name"`
			Type            *string `json:"type"`
			ExpectedVersion int64   `json:"expected_version"`
		}
		if err := parseBody(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
		var typ *domain.AccountType
		if req.Type != nil {
			pt, perr := domain.ParseAccountType(*req.Type)
			if perr != nil {
				WriteError(w, r, domain.NewValidationError(domain.FieldError{Field: "type", Message: "must be one of checking, savings, credit, cash, other"}))
				return
			}
			typ = &pt
		}
		p := principalFromCtx(r.Context())
		acc, err := deps.Accounts.UpdateAccount(r.Context(), usecases.UpdateAccountCommand{
			ID:              id,
			Name:            req.Name,
			Type:            typ,
			ExpectedVersion: req.ExpectedVersion,
		}, p.TenantID, p.UserID)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, acc)
	}
}

func accountDeleteHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseUUIDParam(r, "id")
		if err != nil {
			WriteError(w, r, err)
			return
		}
		p := principalFromCtx(r.Context())
		allowCascade := r.URL.Query().Get("cascade") == "true"
		if err := deps.Accounts.DeleteAccount(r.Context(), id, p.TenantID, p.UserID, usecases.DeleteAccountOptions{AllowCascade: allowCascade}); err != nil {
			WriteError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func accountReconcileHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseUUIDParam(r, "id")
		if err != nil {
			WriteError(w, r, err)
			return
		}
		version, err := requireInt64Query(r, "version", 0)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		p := principalFromCtx(r.Context())
		acc, err := deps.Accounts.ReconcileAccount(r.Context(), id, version, p.TenantID, p.UserID)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, acc)
	}
}

// ---------- transaction handlers ----------

type transactionRequest struct {
	AccountID   string   `json:"account_id"`
	CategoryIDs []string `json:"category_ids"`
	Amount      string   `json:"amount"`
	Currency    string   `json:"currency"`
	Direction   string   `json:"direction"`
	Date        string   `json:"date"`
	Description string   `json:"description"`
	Scheduled   bool     `json:"scheduled"`
	ReasonCode  *string  `json:"reason_code"`
}

func (req transactionRequest) toCommand() (usecases.RecordTransactionCommand, error) {
	var cmd usecases.RecordTransactionCommand
	accountID, err := uuid.Parse(req.AccountID)
	if err != nil {
		return cmd, domain.NewValidationError(domain.FieldError{Field: "account_id", Message: "must be a valid UUID"})
	}
	cmd.AccountID = accountID
	for _, c := range req.CategoryIDs {
		id, err := uuid.Parse(c)
		if err != nil {
			return cmd, domain.NewValidationError(domain.FieldError{Field: "category_ids", Message: "must be valid UUIDs"})
		}
		cmd.CategoryIDs = append(cmd.CategoryIDs, id)
	}
	amount, err := domain.Parse(req.Amount, req.Currency)
	if err != nil {
		return cmd, domain.NewValidationError(domain.FieldError{Field: "amount", Message: "must be a non-negative decimal with at most two places"})
	}
	cmd.Amount = amount
	dir, err := domain.ParseTransactionType(req.Direction)
	if err != nil {
		return cmd, domain.NewValidationError(domain.FieldError{Field: "direction", Message: "must be income or expense"})
	}
	cmd.Direction = dir
	date, err := time.Parse(time.RFC3339, req.Date)
	if err != nil {
		return cmd, domain.NewValidationError(domain.FieldError{Field: "date", Message: "must be an RFC3339 timestamp"})
	}
	cmd.Date = date
	cmd.Description = req.Description
	cmd.Scheduled = req.Scheduled
	if req.ReasonCode != nil {
		rc, rerr := domain.ParseReasonCode(*req.ReasonCode)
		if rerr != nil {
			return cmd, domain.NewValidationError(domain.FieldError{Field: "reason_code", Message: "must be adjustment or refund"})
		}
		cmd.ReasonCode = &rc
	}
	return cmd, nil
}

func transactionCreateHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var req transactionRequest
		if err := parseBody(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
		cmd, err := req.toCommand()
		if err != nil {
			WriteError(w, r, err)
			return
		}
		p := principalFromCtx(r.Context())
		tx, err := deps.Transactions.RecordTransaction(r.Context(), cmd, p.TenantID, p.UserID)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusCreated, tx)
	}
}

func transactionsListHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		page, err := parseCursorPage(r)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		var filter ports.TransactionFilter
		if v := parseQueryTime(r, "from"); v != nil {
			filter.From = v
		}
		if v := parseQueryTime(r, "to"); v != nil {
			filter.To = v
		}
		if v := r.URL.Query().Get("account_id"); v != "" {
			if id, err := uuid.Parse(v); err == nil {
				filter.AccountID = &id
			}
		}
		if v := r.URL.Query().Get("category_id"); v != "" {
			if id, err := uuid.Parse(v); err == nil {
				filter.CategoryID = &id
			}
		}
		p := principalFromCtx(r.Context())
		res, err := deps.Transactions.ListTransactions(r.Context(), filter, page, p.TenantID)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, res)
	}
}

func transactionGetHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseUUIDParam(r, "id")
		if err != nil {
			WriteError(w, r, err)
			return
		}
		p := principalFromCtx(r.Context())
		tx, err := deps.Transactions.GetTransaction(r.Context(), id, p.TenantID)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, tx)
	}
}

func transactionUpdateHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseUUIDParam(r, "id")
		if err != nil {
			WriteError(w, r, err)
			return
		}
		version, err := requireInt64Query(r, "version", 0)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		var req transactionRequest
		if err := parseBody(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
		cmd, err := req.toCommand()
		if err != nil {
			WriteError(w, r, err)
			return
		}
		p := principalFromCtx(r.Context())
		tx, err := deps.Transactions.EditTransaction(r.Context(), id, version, cmd, p.TenantID, p.UserID)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, tx)
	}
}

func transactionDeleteHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseUUIDParam(r, "id")
		if err != nil {
			WriteError(w, r, err)
			return
		}
		version, err := requireInt64Query(r, "version", 0)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		p := principalFromCtx(r.Context())
		if err := deps.Transactions.DeleteTransaction(r.Context(), id, version, p.TenantID, p.UserID); err != nil {
			WriteError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---------- category handlers ----------

type categoryCreateRequest struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	ParentID *string `json:"parent_id"`
}

func categoryCreateHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var req categoryCreateRequest
		if err := parseBody(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
		typ, err := domain.ParseTransactionType(req.Type)
		if err != nil {
			WriteError(w, r, domain.NewValidationError(domain.FieldError{Field: "type", Message: "must be income or expense"}))
			return
		}
		var parent *uuid.UUID
		if req.ParentID != nil {
			id, perr := uuid.Parse(*req.ParentID)
			if perr != nil {
				WriteError(w, r, domain.NewValidationError(domain.FieldError{Field: "parent_id", Message: "must be a valid UUID"}))
				return
			}
			parent = &id
		}
		p := principalFromCtx(r.Context())
		cat, err := deps.Categories.CreateCategory(r.Context(), usecases.CreateCategoryCommand{
			Name: req.Name, Type: typ, ParentID: parent,
		}, p.TenantID, p.UserID)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusCreated, cat)
	}
}

func categoriesListHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		page, size, err := parseOffsetPage(r)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		p := principalFromCtx(r.Context())
		items, total, err := deps.Categories.ListCategories(r.Context(), p.TenantID, nil, page, size)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"items": items, "total": total, "next_page": nextPage(page, size, total)})
	}
}

type categoryMergeRequest struct {
	TargetID        string `json:"target_id"`
	ExpectedVersion int64  `json:"expected_version"`
}

func categoryMergeHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		sourceID, err := parseUUIDParam(r, "id")
		if err != nil {
			WriteError(w, r, err)
			return
		}
		var req categoryMergeRequest
		if err := parseBody(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
		targetID, perr := uuid.Parse(req.TargetID)
		if perr != nil {
			WriteError(w, r, domain.NewValidationError(domain.FieldError{Field: "target_id", Message: "must be a valid UUID"}))
			return
		}
		p := principalFromCtx(r.Context())
		if err := deps.Categories.MergeCategories(r.Context(), sourceID, targetID, req.ExpectedVersion, p.TenantID, p.UserID); err != nil {
			WriteError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---------- budget handlers ----------

type budgetCreateRequest struct {
	CategoryID string `json:"category_id"`
	PeriodType string `json:"period_type"`
	Start      string `json:"start"`
	End        string `json:"end"`
	Allocation string `json:"allocation"`
	Currency   string `json:"currency"`
}

func budgetCreateHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var req budgetCreateRequest
		if err := parseBody(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
		categoryID, err := uuid.Parse(req.CategoryID)
		if err != nil {
			WriteError(w, r, domain.NewValidationError(domain.FieldError{Field: "category_id", Message: "must be a valid UUID"}))
			return
		}
		pt, err := domain.ParseBudgetPeriodType(req.PeriodType)
		if err != nil {
			WriteError(w, r, domain.NewValidationError(domain.FieldError{Field: "period_type", Message: "must be monthly, quarterly, or annual"}))
			return
		}
		start, serr := time.Parse(time.RFC3339, req.Start)
		if serr != nil {
			WriteError(w, r, domain.NewValidationError(domain.FieldError{Field: "start", Message: "must be an RFC3339 timestamp"}))
			return
		}
		end, eerr := time.Parse(time.RFC3339, req.End)
		if eerr != nil {
			WriteError(w, r, domain.NewValidationError(domain.FieldError{Field: "end", Message: "must be an RFC3339 timestamp"}))
			return
		}
		alloc, aerr := domain.Parse(req.Allocation, req.Currency)
		if aerr != nil {
			WriteError(w, r, domain.NewValidationError(domain.FieldError{Field: "allocation", Message: "must be a non-negative decimal with at most two places"}))
			return
		}
		p := principalFromCtx(r.Context())
		b, err := deps.Budgets.CreateBudget(r.Context(), usecases.CreateBudgetCommand{
			CategoryID: categoryID, PeriodType: pt, Start: start, End: end, Allocation: alloc,
		}, p.TenantID, p.UserID)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusCreated, b)
	}
}

func budgetsListHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		page, size, err := parseOffsetPage(r)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		p := principalFromCtx(r.Context())
		items, total, err := deps.Budgets.ListBudgets(r.Context(), p.TenantID, page, size)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"items": items, "total": total, "next_page": nextPage(page, size, total)})
	}
}

func budgetVsActualHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseUUIDParam(r, "id")
		if err != nil {
			WriteError(w, r, err)
			return
		}
		p := principalFromCtx(r.Context())
		res, origin, err := deps.Budgets.CompareBudgetVsActual(r.Context(), id, p.TenantID)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		w.Header().Set("X-Cache", string(origin))
		writeJSON(w, r, http.StatusOK, res)
	}
}

// ---------- report handlers ----------

func reportHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		kind := strings.TrimSpace(chi.URLParam(r, "kind"))
		rng, err := parseRangeQuery(r)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		if err := deps.Reports.ValidateRange(rng); err != nil {
			WriteError(w, r, domain.NewValidationError(domain.FieldError{Field: "range", Message: "start must be on or before end within the supported window"}))
			return
		}
		p := principalFromCtx(r.Context())
		switch kind {
		case "profit-loss":
			res, err := deps.Reports.ProfitLoss(r.Context(), rng, p.TenantID)
			if err != nil {
				WriteError(w, r, err)
				return
			}
			writeJSON(w, r, http.StatusOK, res)
		case "expenses-by-category":
			res, err := deps.Reports.ExpenseBreakdown(r.Context(), rng, p.TenantID)
			if err != nil {
				WriteError(w, r, err)
				return
			}
			writeJSON(w, r, http.StatusOK, res)
		case "income-trend":
			res, err := deps.Reports.IncomeTrend(r.Context(), rng, p.TenantID)
			if err != nil {
				WriteError(w, r, err)
				return
			}
			writeJSON(w, r, http.StatusOK, res)
		case "cash-flow":
			res, err := deps.Reports.CashFlow(r.Context(), rng, p.TenantID)
			if err != nil {
				WriteError(w, r, err)
				return
			}
			writeJSON(w, r, http.StatusOK, res)
		default:
			WriteError(w, r, domain.NewNotFound("report kind", kind))
		}
	}
}

// ---------- export handlers ----------

type exportCreateRequest struct {
	Format  string `json:"format"`
	Scope   string `json:"scope"`
	From    string `json:"from"`
	To      string `json:"to"`
	Options struct {
		ReportType     string `json:"report_type"`
		CategoryID     string `json:"category_id"`
		AccountID      string `json:"account_id"`
		Locale         string `json:"locale"`
		Timezone       string `json:"timezone"`
		IncludeHeaders bool   `json:"include_headers"`
		Delimiter      string `json:"delimiter"`
	} `json:"options"`
}

func exportCreateHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var req exportCreateRequest
		if err := parseBody(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
		format := ports.ExportFormat(req.Format)
		scope := usecases.ExportScope(req.Scope)
		cmd := usecases.CreateExportCommand{Format: format, Scope: scope}
		if req.From != "" && req.To != "" {
			from, ferr := time.Parse(time.RFC3339, req.From)
			if ferr != nil {
				WriteError(w, r, domain.NewValidationError(domain.FieldError{Field: "from", Message: "must be an RFC3339 timestamp"}))
				return
			}
			to, terr := time.Parse(time.RFC3339, req.To)
			if terr != nil {
				WriteError(w, r, domain.NewValidationError(domain.FieldError{Field: "to", Message: "must be an RFC3339 timestamp"}))
				return
			}
			rng, nerr := domain.NewDateRange(from, to)
			if nerr != nil {
				WriteError(w, r, domain.NewValidationError(domain.FieldError{Field: "range", Message: "start must be on or before end"}))
				return
			}
			cmd.Range = &rng
		}
		cmd.Options = usecases.ExportOptions{
			ReportType:     req.Options.ReportType,
			CategoryID:     req.Options.CategoryID,
			AccountID:      req.Options.AccountID,
			Locale:         req.Options.Locale,
			Timezone:       req.Options.Timezone,
			IncludeHeaders: req.Options.IncludeHeaders,
			Delimiter:      req.Options.Delimiter,
		}

		p := principalFromCtx(r.Context())
		perms, perr := deps.Permissions.PermissionsFor(r.Context(), p.UserID, p.TenantID)
		if perr != nil {
			WriteError(w, r, domain.NewForbidden("export permission denied"))
			return
		}
		permSet := usecases.PermissionSet{
			ActorID:       p.UserID,
			TenantID:      p.TenantID,
			Permissions:   perms,
			AllowedScopes: []usecases.ExportScope{usecases.ExportScopeTransactions, usecases.ExportScopeReports},
		}
		job, derr := deps.Exports.CreateExport(r.Context(), cmd, p.TenantID.String(), permSet)
		if derr != nil {
			WriteError(w, r, derr)
			return
		}
		w.Header().Set("Location", "/v1/exports/"+job.ID)
		writeJSON(w, r, http.StatusAccepted, job)
	}
}

func exportGetHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(chi.URLParam(r, "id"))
		if id == "" {
			WriteError(w, r, domain.NewValidationError(domain.FieldError{Field: "id", Message: "is required"}))
			return
		}
		p := principalFromCtx(r.Context())
		perms, perr := deps.Permissions.PermissionsFor(r.Context(), p.UserID, p.TenantID)
		if perr != nil {
			WriteError(w, r, domain.NewForbidden("export permission denied"))
			return
		}
		permSet := usecases.PermissionSet{
			ActorID: p.UserID, TenantID: p.TenantID, Permissions: perms,
			AllowedScopes: []usecases.ExportScope{usecases.ExportScopeTransactions, usecases.ExportScopeReports},
		}
		job, derr := deps.Exports.GetExport(r.Context(), id, p.TenantID.String(), permSet)
		if derr != nil {
			WriteError(w, r, derr)
			return
		}
		writeJSON(w, r, http.StatusOK, job)
	}
}

func exportDownloadHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(chi.URLParam(r, "id"))
		if id == "" {
			WriteError(w, r, domain.NewValidationError(domain.FieldError{Field: "id", Message: "is required"}))
			return
		}
		p := principalFromCtx(r.Context())
		perms, perr := deps.Permissions.PermissionsFor(r.Context(), p.UserID, p.TenantID)
		if perr != nil {
			WriteError(w, r, domain.NewForbidden("export permission denied"))
			return
		}
		permSet := usecases.PermissionSet{
			ActorID: p.UserID, TenantID: p.TenantID, Permissions: perms,
			AllowedScopes: []usecases.ExportScope{usecases.ExportScopeTransactions, usecases.ExportScopeReports},
		}
		url, derr := deps.Exports.GetDownloadURL(r.Context(), id, p.TenantID.String(), permSet)
		if derr != nil {
			WriteError(w, r, derr)
			return
		}
		// Bearer-token download URL is only ever set into a 302 Location; never
		// logged or echoed into the body.
		w.Header().Set("Location", url)
		w.WriteHeader(http.StatusFound)
	}
}

// ---------- user handlers ----------

type userCreateRequest struct {
	Username   string `json:"username"`
	Email      string `json:"email"`
	Password   string `json:"password"`
	RoleID     string `json:"role_id"`
	MFAEnabled bool   `json:"mfa_enabled"`
	MFASecret  string `json:"mfa_secret"`
}

func userCreateHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var req userCreateRequest
		if err := parseBody(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
		roleID, err := uuid.Parse(req.RoleID)
		if err != nil {
			WriteError(w, r, domain.NewValidationError(domain.FieldError{Field: "role_id", Message: "must be a valid UUID"}))
			return
		}
		p := principalFromCtx(r.Context())
		u, err := deps.Users.CreateUser(r.Context(), usecases.CreateUserCommand{
			Username: req.Username, Email: req.Email, Password: req.Password,
			RoleID: roleID, MFAEnabled: req.MFAEnabled, MFASecret: req.MFASecret,
		}, p.TenantID, p.UserID)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusCreated, u)
	}
}

func usersListHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		page, size, err := parseOffsetPage(r)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		p := principalFromCtx(r.Context())
		items, total, err := deps.Users.ListUsers(r.Context(), p.TenantID, p.UserID, page, size)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"items": items, "total": total, "next_page": nextPage(page, size, total)})
	}
}

func userGetHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseUUIDParam(r, "id")
		if err != nil {
			WriteError(w, r, err)
			return
		}
		p := principalFromCtx(r.Context())
		u, err := deps.Users.GetUser(r.Context(), id, p.TenantID, p.UserID)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, u)
	}
}

func userUpdateHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseUUIDParam(r, "id")
		if err != nil {
			WriteError(w, r, err)
			return
		}
		version, err := requireInt64Query(r, "version", 0)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		var req struct {
			Email      *string `json:"email"`
			Password   *string `json:"password"`
			MFAEnabled *bool   `json:"mfa_enabled"`
		}
		if err := parseBody(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
		p := principalFromCtx(r.Context())
		u, err := deps.Users.UpdateUser(r.Context(), id, p.TenantID, p.UserID, version, req.Email, req.Password, req.MFAEnabled)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, u)
	}
}

func userDeleteHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseUUIDParam(r, "id")
		if err != nil {
			WriteError(w, r, err)
			return
		}
		version, err := requireInt64Query(r, "version", 0)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		p := principalFromCtx(r.Context())
		if err := deps.Users.DeleteUser(r.Context(), id, p.TenantID, version, p.UserID); err != nil {
			WriteError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---------- audit-log handler ----------

func auditLogsHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		page, err := parseCursorPage(r)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		p := principalFromCtx(r.Context())
		var filter ports.AuditLogFilter
		if v := r.URL.Query().Get("entity_type"); v != "" {
			filter.EntityType = &v
		}
		if v := r.URL.Query().Get("action"); v != "" {
			if a, aerr := domain.ParseAuditAction(v); aerr == nil {
				act := a
				filter.Action = &act
			}
		}
		entries, next, derr := deps.AuditLogs.List(r.Context(), p.TenantID, filter, page)
		if derr != nil {
			WriteError(w, r, derr)
			return
		}
		var nextCursor *string
		if next != nil && next.NextCursor != nil {
			nextCursor = next.NextCursor
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"items": entries, "next_cursor": nextCursor})
	}
}

// ---------- certification handlers ----------

type certificationCreateRequest struct {
	Scope       string `json:"scope"`
	WindowStart string `json:"window_start"`
	WindowEnd   string `json:"window_end"`
}

func certificationCreateHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var req certificationCreateRequest
		if err := parseBody(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
		start, serr := time.Parse(time.RFC3339, req.WindowStart)
		if serr != nil {
			WriteError(w, r, domain.NewValidationError(domain.FieldError{Field: "window_start", Message: "must be an RFC3339 timestamp"}))
			return
		}
		end, eerr := time.Parse(time.RFC3339, req.WindowEnd)
		if eerr != nil {
			WriteError(w, r, domain.NewValidationError(domain.FieldError{Field: "window_end", Message: "must be an RFC3339 timestamp"}))
			return
		}
		p := principalFromCtx(r.Context())
		review, err := deps.Certifications.CreateReview(r.Context(), usecases.CreateReviewCommand{
			Scope: req.Scope, WindowStart: start, WindowEnd: end,
		}, p.TenantID, p.UserID)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusCreated, review)
	}
}

func certificationsListHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalFromCtx(r.Context())
		reviews, err := deps.Certifications.ListPending(r.Context(), p.TenantID, p.UserID)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"items": reviews})
	}
}

// ---------- dashboard KPI handler ----------

func dashboardKpisHandler(deps Deps) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		rng, err := parseRangeQuery(r)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		p := principalFromCtx(r.Context())
		perms, perr := deps.Permissions.PermissionsFor(r.Context(), p.UserID, p.TenantID)
		if perr != nil {
			// RBAC already gated dashboard:read upstream; fail closed on a resolver
			// error rather than serve a differently-classed dashboard.
			WriteError(w, r, domain.NewForbidden("access denied"))
			return
		}
		switch roleClassFor(perms) {
		case "cfo":
			res, err := deps.Dashboard.CFOKPIs(r.Context(), rng, p.TenantID)
			if err != nil {
				WriteError(w, r, err)
				return
			}
			writeJSON(w, r, http.StatusOK, res)
		case "standard":
			res, err := deps.Dashboard.StandardUserKPIs(r.Context(), rng, p.TenantID, p.UserID)
			if err != nil {
				WriteError(w, r, err)
				return
			}
			writeJSON(w, r, http.StatusOK, res)
		default:
			res, err := deps.Dashboard.ViewerKPIs(r.Context(), rng, p.TenantID)
			if err != nil {
				WriteError(w, r, err)
				return
			}
			writeJSON(w, r, http.StatusOK, res)
		}
	}
}

// ---------- v1 router and route table ----------

func newV1Router(deps Deps) http.Handler {
	sub := chi.NewRouter()
	sub.Use(correlationMiddleware)

	// Public pre-auth endpoints: login (stage 1), MFA completion (stage 2), and
	// refresh-token rotation. Rate-limited at 10/min across the auth sub-tree as
	// the brute-force defense.
	authPub := chi.NewRouter()
	authPub.Use(ratelimitMiddleware(deps))
	authPub.Post("/login", loginHandler(deps))
	authPub.Post("/mfa", mfaHandler(deps))
	authPub.Post("/refresh", refreshHandler(deps))
	sub.Mount("/auth", authPub)

	// Protected sub-router: full mandated middleware chain for every resource.
	// Logout lives here because it requires an authenticated principal.
	prot := chi.NewRouter()
	prot.Use(authMiddleware(deps))
	prot.Use(rbacMiddleware(deps))
	prot.Use(tenantMiddleware)
	prot.Use(ratelimitMiddleware(deps))
	prot.Use(idempotencyMiddleware(deps))
	prot.Use(readAuditMiddleware(deps))
	registerProtectedRoutes(prot, deps)
	sub.Mount("/", prot)

	return sub
}

func registerProtectedRoutes(r chi.Router, deps Deps) {
	// Accounts
	r.Post("/accounts", accountsCreateHandler(deps))
	r.Get("/accounts", accountsListHandler(deps))
	r.Get("/accounts/{id}", accountGetHandler(deps))
	r.Patch("/accounts/{id}", accountUpdateHandler(deps))
	r.Delete("/accounts/{id}", accountDeleteHandler(deps))
	r.Post("/accounts/{id}/reconcile", accountReconcileHandler(deps))

	// Transactions
	r.Post("/transactions", transactionCreateHandler(deps))
	r.Get("/transactions", transactionsListHandler(deps))
	r.Get("/transactions/{id}", transactionGetHandler(deps))
	r.Patch("/transactions/{id}", transactionUpdateHandler(deps))
	r.Delete("/transactions/{id}", transactionDeleteHandler(deps))

	// Categories
	r.Post("/categories", categoryCreateHandler(deps))
	r.Get("/categories", categoriesListHandler(deps))
	r.Post("/categories/{id}/merge", categoryMergeHandler(deps))

	// Budgets
	r.Post("/budgets", budgetCreateHandler(deps))
	r.Get("/budgets", budgetsListHandler(deps))
	r.Get("/budgets/{id}/vs-actual", budgetVsActualHandler(deps))

	// Reports
	r.Get("/reports/{kind}", reportHandler(deps))

	// Exports
	r.Post("/exports", exportCreateHandler(deps))
	r.Get("/exports/{id}", exportGetHandler(deps))
	r.Get("/exports/{id}/download", exportDownloadHandler(deps))

	// Users
	r.Post("/users", userCreateHandler(deps))
	r.Get("/users", usersListHandler(deps))
	r.Get("/users/{id}", userGetHandler(deps))
	r.Patch("/users/{id}", userUpdateHandler(deps))
	r.Delete("/users/{id}", userDeleteHandler(deps))

	// Audit logs
	r.Get("/audit-logs", auditLogsHandler(deps))

	// Certifications
	r.Post("/certifications", certificationCreateHandler(deps))
	r.Get("/certifications", certificationsListHandler(deps))

	// Dashboard KPIs
	r.Get("/dashboard/kpis", dashboardKpisHandler(deps))

	// Logout (authenticated-only; see RBAC map).
	r.Post("/auth/logout", logoutHandler(deps))
}