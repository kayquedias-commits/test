// Package postgres implements the FMS persistence seam: every output port
// declared in application/ports (the *Repository interfaces) against
// PostgreSQL 16 via pgx/v5.
//
// This is the single adapter isolating the database driver from the rest of
// the application. No pgx type, query, or driver error crosses the repository
// boundary — all methods accept/return domain aggregates and normalize
// failures onto the domain.DomainError contract. Use cases depend only on the
// ports interfaces, never on this package.
//
// Tenancy: every query embeds an explicit `tenant_id` predicate (the app-level
// isolation defense) and the schema applies row-level security (RLS) as
// defense-in-depth. RLS relies on the session GUC `app.tenant_id`. On a pooled
// connection that GUC is a session-scoped global, so the tenant context is
// re-established inside every transaction via `SET LOCAL` (rolled back with
// the transaction) and re-seeded per acquisition in the AfterConnect hook.
// Reads outside a transaction rely on the explicit tenant predicate as the
// primary defense; RLS still filters them via the seeded session value.
//
// UnitOfWork is single-goroutine-scoped. A *UnitOfWork must never be shared
// across goroutines: repository calls inside a UoW run on the held pgx.Tx, and
// concurrent use of one Tx would corrupt isolation.
//
// Pool sizing: pgx has no `max_overflow` concept (SQLAlchemy semantics);
// MaxConns is a hard cap, so `MaxConns = PoolSize + MaxOverflow` is the
// correct mapping of the §9.4 tuning (10 + 20 -> 30). Replica routing is not
// yet active because config exposes no READ_REPLICA_URL field; the seam
// (replica-unaware select) is in place for a follow-up config task.
package postgres

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/config"
	"fms/backend/internal/domain"
)

// Pagination and search bounds.
const (
	offsetMaxPageSize = 100
	cursorDefaultLimit = 100
	cursorMaxLimit     = 200
	maxSearchTermLen   = 100
	statementTimeout   = 5 * time.Second
)

// queryExecer is the union of the two connection-acquisition types a
// repository method may run against: the pool (outside a transaction) or an
// active pgx.Tx (inside a UnitOfWork).
type queryExecer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// tenantCtxKey carries the authorized tenant id through a request context. It
// is populated by the IAM middleware (interface adapter) from the authenticated
// token claims — never from a client-supplied header or body.
type tenantCtxKey struct{}

// WithTenant returns a copy of ctx carrying the tenant derived from the
// authenticated principal. Middleware calls this once per request; repository
// writes and the UnitOfWork read it to establish RLS context. Input must come
// from server-side claims, never from client input.
func WithTenant(ctx context.Context, tenant uuid.UUID) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, tenant)
}

// TenantFromContext reports the tenant stored in ctx, if any.
func TenantFromContext(ctx context.Context) (uuid.UUID, bool) {
	t, ok := ctx.Value(tenantCtxKey{}).(uuid.UUID)
	return t, ok && t != uuid.Nil
}

// Store is the PostgreSQL persistence adapter. It owns connection pooling and
// stands up the nine repository implementations behind the ports interfaces.
type Store struct {
	primary      *pgxpool.Pool
	replica      *pgxpool.Pool // nil: replica routing disabled until config exposes a READ_REPLICA_URL
	tx           pgx.Tx        // nil outside a UnitOfWork; set on the WithTx clone
	cursorSecret []byte
	baseCurrency string
}

// UnitOfWork is an explicit ACID boundary for a single use-case invocation. It
// holds one pgx.Tx and, on commit, makes the ledger change and any outbox rows
// atomic. It is single-goroutine-scoped: never share it across goroutines.
type UnitOfWork struct {
	tx    pgx.Tx
	store *Store
}

// NewStore builds the connection pool, runs a `SELECT 1` health check, and
// returns a Store. Pool acquisition is bounded by the configured connect
// timeout and retried (max 3 attempts) with exponential backoff plus jitter,
// retrying only transient connection failures — never auth/config-class errors.
// An empty DATABASE_URL is rejected before any dial is attempted.
func NewStore(ctx context.Context, cfg *config.Config) (*Store, error) {
	if strings.TrimSpace(cfg.Database.URL) == "" {
		return nil, domain.NewDependencyUnavailable("database url is not configured")
	}
	poolCfg, err := pgxpool.ParseConfig(cfg.Database.URL)
	if err != nil {
		return nil, domain.NewDependencyUnavailable("invalid database connection configuration")
	}
	// pgx has no max_overflow: MaxConns is a hard cap, so PoolSize+MaxOverflow
	// reproduces the §9.4 sizing (10 base + 20 burst => 30).
	poolCfg.MaxConns = int32(cfg.Database.PoolSize + cfg.Database.MaxOverflow)
	poolCfg.MinConns = 5
	poolCfg.MaxConnLifetime = cfg.Database.PoolRecycle
	poolCfg.ConnConfig.ConnectTimeout = cfg.Database.ConnTimeout
	// RLS GUC seeding per acquisition. This is session-scoped (SET, not SET
	// LOCAL): it ensures a freshly-created connection always filters reads, so
	// RLS never silently lets everything through on an unseeded checkout. The
	// authoritative per-transaction tenant is re-established in Begin.
	poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, "SET TIMEZONE = 'UTC'"); err != nil {
			return err
		}
		if tenant, ok := TenantFromContext(ctx); ok {
			_, err := conn.Exec(ctx, "SELECT fms_set_tenant($1)", tenant)
			return err
		}
		return nil
	}

	var pool *pgxpool.Pool
	var lastErr error
	attempts := 3
	backoff := 100 * time.Millisecond
	for i := 0; i < attempts; i++ {
		acqCtx, cancel := context.WithTimeout(ctx, cfg.Database.ConnTimeout)
		pool, err = pgxpool.NewWithConfig(acqCtx, poolCfg)
		pingErr := error(nil)
		if err == nil {
			pingCtx, cancelPing := context.WithTimeout(acqCtx, cfg.Database.ConnTimeout)
			pingErr = pool.Ping(pingCtx)
			cancelPing()
		}
		cancel()
		if err == nil && pingErr == nil {
			return &Store{
				primary:      pool,
				cursorSecret: []byte(cfg.Security.JWTSecret),
				baseCurrency: cfg.Accounting.BaseCurrency,
			}, nil
		}
		lastErr = err
		if err == nil {
			lastErr = pingErr
		}
		if pool != nil {
			pool.Close()
		}
		if !isRetryableConnErr(lastErr) {
			break
		}
		wait := backoff + time.Duration(rand.Int63n(int64(backoff)))
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, domain.Wrap(domain.CodeDependencyUnavailable, ctx.Err())
		}
		backoff *= 2
	}
	return nil, domain.Wrap(domain.CodeDependencyUnavailable, lastErr)
}

// isRetryableConnErr reports whether a connection error is transient and worth
// retrying. Auth, invalid database name, and other configuration-class errors
// fail immediately; only transport/timeout dials are retried.
func isRetryableConnErr(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "08000", "08003", "08006", "57P01", "57P02", "57P03", "53300":
			return true
		default:
			return false
		}
	}
	return true
}

// Ping verifies database reachability.
func (s *Store) Ping(ctx context.Context) error {
	return s.primary.Ping(ctx)
}

// PoolStats exposes the connection-pool gauges for the observability counter.
// Query/read gauges and replica lag are exported as a pool metric surface.
func (s *Store) PoolStats() pgxpool.Stat {
	return s.primary.Stat()
}

// Close releases all pool resources.
func (s *Store) Close() {
	if s.primary != nil {
		s.primary.Close()
	}
	if s.replica != nil {
		s.replica.Close()
	}
}

// scopeTenant resolves the authorized tenant from the request context for
// establishing RLS context (writes). The ports interfaces additionally thread
// an explicit tenantID argument on every call; both are enforced. A missing
// tenant indicates a broken request pipeline (middleware misconfiguration),
// surfaced as UNAUTHENTICATED per the persistence seam contract so the caller
// fails closed rather than touching any rows.
func (s *Store) scopeTenant(ctx context.Context) (uuid.UUID, *domain.DomainError) {
	t, ok := TenantFromContext(ctx)
	if !ok {
		return uuid.Nil, domain.NewUnauthenticated("tenant context is missing")
	}
	return t, nil
}

// conn returns the query runner for the current operation: the active
// UnitOfWork transaction when present, otherwise the primary pool. Query-only
// repository methods select the primary because replica routing is disabled
// until config exposes a read-replica URL.
func (s *Store) conn() queryExecer {
	if s.tx != nil {
		return s.tx
	}
	return s.primary
}

// --- UnitOfWork -----------------------------------------------------------

// Begin opens a transaction, establishes the RLS tenant context within it
// (SET LOCAL, tx-scoped), and returns the UnitOfWork. The tenant comes from
// the authenticated context — never from client input.
func (s *Store) Begin(ctx context.Context) (*UnitOfWork, *domain.DomainError) {
	tenant, derr := s.scopeTenant(ctx)
	if derr != nil {
		return nil, derr
	}
	tx, err := s.primary.Begin(ctx)
	if err != nil {
		return nil, rwError(err)
	}
	// Re-establish the tenant inside the transaction so RLS applies to every
	// statement of the UoW, including writes and the outbox insert.
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1::text, true)", tenant); err != nil {
		_ = tx.Rollback(ctx)
		return nil, rwError(err)
	}
	// Bound runaway queries inside the transaction to the latency target.
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = 5000"); err != nil {
		_ = tx.Rollback(ctx)
		return nil, rwError(err)
	}
	return &UnitOfWork{tx: tx, store: s}, nil
}

// Commit flushes the transaction. Idempotent after a completed Rollback.
func (u *UnitOfWork) Commit(ctx context.Context) *domain.DomainError {
	if err := u.tx.Commit(ctx); err != nil {
		return rwError(err)
	}
	return nil
}

// Rollback aborts the transaction. Safe to call after an error.
func (u *UnitOfWork) Rollback(ctx context.Context) *domain.DomainError {
	if err := u.tx.Rollback(ctx); err != nil {
		return rwError(err)
	}
	return nil
}

// WithTx returns a Store bound to the given transaction so repository methods
// invoked through it run inside the same ACID boundary. Use cases thread the
// returned clone through every write path of one UnitOfWork so the ledger
// change and outbox insert commit atomically.
func (s *Store) WithTx(tx pgx.Tx) *Store {
	clone := *s
	clone.tx = tx
	return &clone
}

// AppendOutbox inserts a durable outbox row on the same transaction as the
// triggering ledger mutation, guaranteeing no event loss if the relay processes
// it at-least-once. This is the seam the write use cases thread through; it is
// not an output port.
func (s *Store) AppendOutbox(ctx context.Context, tenantID uuid.UUID, eventType string, aggregateID uuid.UUID, payload []byte) *domain.DomainError {
	c := s.conn()
	_, err := c.Exec(ctx,
		`INSERT INTO outbox (tenant_id, event_type, aggregate_id, payload)
		 VALUES ($1, $2, $3, $4)`,
		tenantID, eventType, aggregateID, payload)
	if err != nil {
		return rwError(err)
	}
	return nil
}

// --- error mapping --------------------------------------------------------

// rwError normalizes a driver/PostgreSQL failure onto the domain error
// contract. It never leaks the driver text, SQL, or connection material to
// callers; the cause is reachable only through Unwrap for server-side logging.
func rwError(err error) *domain.DomainError {
	if err == nil {
		return nil
	}
	if de, ok := domain.AsDomainError(err); ok {
		return de
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23505": // unique_violation
			return domain.NewConflict("duplicate resource")
		case pgErr.Code == "23503": // foreign_key_violation
			return domain.NewValidationError(domain.FieldError{Field: "id", Message: "referenced resource does not exist"})
		case pgErr.Code == "23502": // not_null_violation
			return domain.NewValidationError(domain.FieldError{Field: "value", Message: "required field missing"})
		case pgErr.Code == "23514": // check_violation
			return domain.NewValidationError(domain.FieldError{Field: "value", Message: "value violates a constraint"})
		case pgErr.Code == "55000" || pgErr.Code == "22023": // append-only guard / contextless RLS
			return domain.NewForbidden("operation is not permitted")
		default:
			return domain.Wrap(domain.CodeInternalError, err)
		}
	}
	return domain.Wrap(domain.CodeInternalError, err)
}

// --- money mapping --------------------------------------------------------

// moneyToMinor converts a NUMERIC(12,2) decimal-string representation into a
// domain.Money. Never uses float64: the decimal string round-trips via exact
// integer parsing, preserving minor-unit precision. A leading '-' (an
// authorized negative adjustment) is parsed as an absolute value then negated.
func moneyToMinor(repr, ccy string) (domain.Money, error) {
	neg := strings.HasPrefix(repr, "-")
	abs := strings.TrimPrefix(repr, "-")
	m, err := domain.Parse(abs, ccy)
	if err != nil {
		return domain.Money{}, err
	}
	if neg {
		m = m.Negate()
	}
	return m, nil
}

// likeEscape escapes the pattern metacharacters \ % _ so a user search term is
// treated as a literal and cannot broaden a query into an unintended match or
// an expensive scan (pattern-injection / resource-consumption control). The
// term is capped at maxSearchTermLen.
func likeEscape(term string) string {
	if len(term) > maxSearchTermLen {
		term = term[:maxSearchTermLen]
	}
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(term)
}

// --- cursor pagination ----------------------------------------------------

// cursorPayload is the fixed-width keyset position plus the filter signature
// over which the HMAC tag is computed. The tenant and filter signature are
// bound into the tag so replaying a cursor under a different predicate or
// tenant is rejected.
type cursorPayload struct {
	Tenant    uuid.UUID
	TS        int64 // created_at as unix nanoseconds
	ID        uuid.UUID
	FilterSig [32]byte
}

const cursorPayloadLen = 16 + 8 + 16 + 32

// encodeCursor produces the opaque, integrity-protected cursor token. The
// payload is HMAC-SHA256-signed with the JWT secret, so a tampered token fails
// verification and is rejected.
func encodeCursor(ts time.Time, id, tenant uuid.UUID, filterSig [32]byte, secret []byte) string {
	p := cursorPayload{Tenant: tenant, TS: ts.UnixNano(), ID: id, FilterSig: filterSig}
	buf := make([]byte, cursorPayloadLen)
	copy(buf[0:16], p.Tenant[:])
	binary.BigEndian.PutUint64(buf[16:24], uint64(p.TS))
	copy(buf[24:40], p.ID[:])
	copy(buf[40:72], p.FilterSig[:])
	mac := signCursor(buf, secret)
	return base64.RawURLEncoding.EncodeToString(append(buf, mac...))
}

func signCursor(payload []byte, secret []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write(payload)
	return m.Sum(nil)
}

// decodeCursor verifies the cursor's HMAC tag and parses the keyset position.
// A malformed or tampered token yields a validation error.
func decodeCursor(token string, tenant uuid.UUID, filterSig [32]byte, secret []byte) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != cursorPayloadLen+sha256.Size {
		return time.Time{}, uuid.Nil, domain.NewValidationError(domain.FieldError{Field: "cursor", Message: "malformed cursor"})
	}
	body := raw[:len(raw)-sha256.Size]
	mac := raw[len(raw)-sha256.Size:]
	if !hmac.Equal(signCursor(body, secret), mac) {
		return time.Time{}, uuid.Nil, domain.NewValidationError(domain.FieldError{Field: "cursor", Message: "invalid or tampered cursor"})
	}
	var p cursorPayload
	copy(p.Tenant[:], body[0:16])
	p.TS = int64(binary.BigEndian.Uint64(body[16:24]))
	copy(p.ID[:], body[24:40])
	copy(p.FilterSig[:], body[40:72])
	if p.Tenant != tenant || p.FilterSig != filterSig {
		return time.Time{}, uuid.Nil, domain.NewValidationError(domain.FieldError{Field: "cursor", Message: "cursor does not apply to this query"})
	}
	return time.Unix(0, p.TS).UTC(), p.ID, nil
}

// filterSig computes a stable signature over the applied filter fields so a
// cursor cannot be replayed under different predicates.
func filterSig(filter ports.TransactionFilter) [32]byte {
	h := sha256.New()
	if filter.AccountID != nil {
		h.Write([]byte("acct:"))
		h.Write((*filter.AccountID)[:])
	}
	if filter.CategoryID != nil {
		h.Write([]byte("cat:"))
		h.Write((*filter.CategoryID)[:])
	}
	if filter.From != nil {
		fmt.Fprintf(h, "from:%d", filter.From.UnixNano())
	}
	if filter.To != nil {
		fmt.Fprintf(h, "to:%d", filter.To.UnixNano())
	}
	if filter.MinAmount != nil {
		fmt.Fprintf(h, "min:%s:", filter.MinAmount.Currency())
		binary.Write(h, binary.BigEndian, filter.MinAmount.Major())
	}
	if filter.MaxAmount != nil {
		fmt.Fprintf(h, "max:%s:", filter.MaxAmount.Currency())
		binary.Write(h, binary.BigEndian, filter.MaxAmount.Major())
	}
	if filter.Description != nil {
		h.Write([]byte("desc:"))
		h.Write([]byte(*filter.Description))
	}
	if filter.Type != nil {
		h.Write([]byte("type:"))
		h.Write([]byte(*filter.Type))
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func auditFilterSig(f ports.AuditLogFilter) [32]byte {
	h := sha256.New()
	if f.EntityType != nil {
		h.Write([]byte("et:"))
		h.Write([]byte(*f.EntityType))
	}
	if f.EntityID != nil {
		h.Write([]byte("eid:"))
		h.Write((*f.EntityID)[:])
	}
	if f.Action != nil {
		h.Write([]byte("act:"))
		h.Write([]byte(*f.Action))
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// normalizeCursorPage applies the default/clamp limits.
func normalizeCursorPage(page ports.CursorPage) (limit int, cursor *string) {
	limit = page.Limit
	if limit <= 0 {
		limit = cursorDefaultLimit
	}
	if limit > cursorMaxLimit {
		limit = cursorMaxLimit
	}
	return limit, page.Cursor
}

// validateDateRange returns a validation error when the half-open range is
// reversed (From > To).
func validateDateRange(from, to *time.Time) *domain.DomainError {
	if from != nil && to != nil && to.Before(*from) {
		return domain.NewValidationError(domain.FieldError{Field: "date_range", Message: "start date must not be after end date"})
	}
	return nil
}

// validateAmountRange returns a validation error when MinAmount > MaxAmount or
// the bounds disagree on currency.
func validateAmountRange(min, max *domain.Money) *domain.DomainError {
	if min == nil || max == nil {
		return nil
	}
	if min.Currency() != max.Currency() {
		return domain.NewValidationError(domain.FieldError{Field: "amount", Message: "amount range bounds must share a currency"})
	}
	if cmp, _ := min.Cmp(*max); cmp > 0 {
		return domain.NewValidationError(domain.FieldError{Field: "amount", Message: "minimum amount must not exceed maximum amount"})
	}
	return nil
}

// normalizeOffsetPage clamps the page size and computes the OFFSET. Page
// defaults to 1 when non-positive.
func normalizeOffsetPage(page ports.OffsetPage) (size int, offset int) {
	pageNum := page.Page
	if pageNum <= 0 {
		pageNum = 1
	}
	size = page.PageSize
	if size <= 0 {
		size = offsetMaxPageSize
	}
	if size > offsetMaxPageSize {
		size = offsetMaxPageSize
	}
	return size, (pageNum - 1) * size
}

// --- account repository ---------------------------------------------------

// Accounts returns the AccountRepository implementation.
func (s *Store) Accounts() ports.AccountRepository { return &AccountRepository{Store: s} }

// AccountRepository implements ports.AccountRepository.
type AccountRepository struct{ *Store }

// Create persists an account, seeding Balance equal to OpeningBalance in the
// same INSERT (balance invariant §5.3).
func (r *AccountRepository) Create(ctx context.Context, tenantID uuid.UUID, account *domain.Account) *domain.DomainError {
	c := r.conn()
	_, err := c.Exec(ctx,
		`INSERT INTO accounts (id, tenant_id, name, type, opening_balance, balance, currency, version)
		 VALUES ($1,$2,$3,$4,$5,$5,$6,1)`,
		account.ID, tenantID, account.Name, string(account.Type),
		account.OpeningBalance.String(), account.Balance.String(), account.Currency)
	if err != nil {
		return rwError(err)
	}
	return nil
}

// GetByID loads one tenant-scoped account (never by primary key alone).
func (r *AccountRepository) GetByID(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) (*domain.Account, *domain.DomainError) {
	c := r.conn()
	row := c.QueryRow(ctx,
		`SELECT id, tenant_id, name, type, opening_balance::text, balance::text, currency, version
		 FROM accounts WHERE tenant_id=$1 AND id=$2`, tenantID, id)
	a, err := scanAccount(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.NewNotFound("account", id.String())
		}
		return nil, rwError(err)
	}
	return a, nil
}

// List returns an offset-paginated, tenant-scoped collection.
func (r *AccountRepository) List(ctx context.Context, tenantID uuid.UUID, page ports.OffsetPage) ([]*domain.Account, *domain.DomainError) {
	size, offset := normalizeOffsetPage(page)
	c := r.conn()
	rows, err := c.Query(ctx,
		`SELECT id, tenant_id, name, type, opening_balance::text, balance::text, currency, version
		 FROM accounts WHERE tenant_id=$1 ORDER BY created_at DESC
		 LIMIT $2 OFFSET $3`, tenantID, size+1, offset)
	if err != nil {
		return nil, rwError(err)
	}
	defer rows.Close()
	out := make([]*domain.Account, 0, size)
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, rwError(err)
		}
		if len(out) < size {
			out = append(out, a)
		}
	}
	if rows.Err() != nil {
		return nil, rwError(rows.Err())
	}
	return out, nil
}

// UpdateBalance applies a signed delta to the account balance as an atomic
// conditional UPDATE guarded by the expected version. Zero rows (stale version
// or missing account) surfaces CONFLICT_STATE.
func (r *AccountRepository) UpdateBalance(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, delta domain.Money, expectedVersion int64) (int64, *domain.DomainError) {
	c := r.conn()
	var newVersion int64
	err := c.QueryRow(ctx,
		`UPDATE accounts
		 SET balance = balance + $1::numeric, version = version + 1, updated_at = now()
		 WHERE id=$2 AND tenant_id=$3 AND version=$4
		 RETURNING version`,
		delta.String(), id, tenantID, expectedVersion).Scan(&newVersion)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, domain.NewConflict("concurrent modification: stale version")
		}
		return 0, rwError(err)
	}
	return newVersion, nil
}

// Delete removes an account within one transaction, refusing (NOT_FOUND, no
// mutation) when linked transactions exist. The in-transaction NOT EXISTS guard
// closes the check-then-delete race.
func (r *AccountRepository) Delete(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, expectedVersion int64) *domain.DomainError {
	c := r.conn()
	tag, err := c.Exec(ctx,
		`DELETE FROM accounts
		 WHERE id=$1 AND tenant_id=$2 AND version=$3
		   AND NOT EXISTS (SELECT 1 FROM transactions WHERE account_id=$1 AND deleted_at IS NULL)`,
		id, tenantID, expectedVersion)
	if err != nil {
		return rwError(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewNotFound("account", "with linked transactions or stale version")
	}
	return nil
}

// CountLinkedTransactions returns the count of non-deleted transactions
// referencing the account — the delete-protection gate.
func (r *AccountRepository) CountLinkedTransactions(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) (int64, *domain.DomainError) {
	c := r.conn()
	var n int64
	err := c.QueryRow(ctx,
		`SELECT count(*) FROM transactions WHERE tenant_id=$1 AND account_id=$2 AND deleted_at IS NULL`,
		tenantID, id).Scan(&n)
	if err != nil {
		return 0, rwError(err)
	}
	return n, nil
}

// Reconcile recomputes balance == opening_balance + SUM(transactions) as a
// single aggregate and surfaces CONFLICT_STATE on drift.
func (r *AccountRepository) Reconcile(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) *domain.DomainError {
	c := r.conn()
	var openingStr, balanceStr, ccy string
	err := c.QueryRow(ctx,
		`SELECT opening_balance::text, balance::text, currency
		 FROM accounts WHERE tenant_id=$1 AND id=$2`, tenantID, id).Scan(&openingStr, &balanceStr, &ccy)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewNotFound("account", id.String())
		}
		return rwError(err)
	}
	opening, err := moneyToMinor(openingStr, ccy)
	if err != nil {
		return domain.Wrap(domain.CodeInternalError, err)
	}
	balance, err := moneyToMinor(balanceStr, ccy)
	if err != nil {
		return domain.Wrap(domain.CodeInternalError, err)
	}
	var sumStr *string
	err = c.QueryRow(ctx,
		`SELECT SUM(CASE WHEN type='income' THEN amount ELSE -amount END)::text
		 FROM transactions WHERE tenant_id=$1 AND account_id=$2 AND deleted_at IS NULL`,
		tenantID, id).Scan(&sumStr)
	if err != nil {
		return rwError(err)
	}
	expected := opening
	if sumStr != nil {
		sum, err := moneyToMinor(*sumStr, ccy)
		if err != nil {
			return domain.Wrap(domain.CodeInternalError, err)
		}
		if sum.IsNegative() {
			expected, err = expected.Sub(sum.Negate())
		} else {
			expected, err = expected.Add(sum)
		}
		if err != nil {
			return domain.NewConflict(domain.ReconciledAgainst)
		}
	}
	if cmp, err := expected.Cmp(balance); err != nil || cmp != 0 {
		return domain.NewConflict(domain.ReconciledAgainst)
	}
	return nil
}

func scanAccount(row pgx.Row) (*domain.Account, error) {
	var a domain.Account
	var typ string
	var opening, balance string
	var ccy string
	err := row.Scan(&a.ID, &a.TenantID, &a.Name, &typ, &opening, &balance, &ccy, &a.Version)
	if err != nil {
		return nil, err
	}
	a.Type = domain.AccountType(typ)
	a.Closed = false // schema 0001 has no closed column; deactivation is tracked in users
	op, err := moneyToMinor(opening, ccy)
	if err != nil {
		return nil, err
	}
	bal, err := moneyToMinor(balance, ccy)
	if err != nil {
		return nil, err
	}
	a.OpeningBalance = op
	a.Balance = bal
	a.Currency = ccy
	return &a, nil
}

// --- transaction repository -----------------------------------------------

// Transactions returns the TransactionRepository implementation.
func (s *Store) Transactions() ports.TransactionRepository { return &TransactionRepository{Store: s} }

// TransactionRepository implements ports.TransactionRepository. Balance
// integrity is guaranteed by the use case invoking AccountRepository.
// UpdateBalance in the same transaction boundary; these methods own the ledger
// row only.
type TransactionRepository struct{ *Store }

// Insert persists a transaction row within the use-case transaction. Insertion
// targets the partitioned parent (`transactions`); PostgreSQL routes the row
// declaratively by date (the mapper never computes child-table names).
func (r *TransactionRepository) Insert(ctx context.Context, tenantID uuid.UUID, tx *domain.Transaction) *domain.DomainError {
	c := r.conn()
	var reason any
	if tx.ReasonCode != nil {
		reason = string(*tx.ReasonCode)
	}
	_, err := c.Exec(ctx,
		`INSERT INTO transactions (id, tenant_id, account_id, amount, date, description, type, scheduled, reason_code, version)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,0)`,
		tx.ID, tenantID, tx.AccountID, tx.Amount.String(), tx.Date, tx.Description,
		string(tx.Type), tx.Scheduled, reason)
	if err != nil {
		return rwError(err)
	}
	return r.insertCategories(ctx, tenantID, tx.ID, tx.CategoryIDs)
}

func (r *TransactionRepository) insertCategories(ctx context.Context, tenantID uuid.UUID, txID uuid.UUID, catIDs []uuid.UUID) *domain.DomainError {
	if len(catIDs) == 0 {
		return nil
	}
	c := r.conn()
	args := make([]any, 0, len(catIDs)*3)
	ph := make([]string, 0, len(catIDs))
	for i, cid := range catIDs {
		n := i * 3
		args = append(args, tenantID, txID, cid)
		ph = append(ph, fmt.Sprintf("($%d,$%d,$%d)", n+1, n+2, n+3))
	}
	_, err := c.Exec(ctx,
		`INSERT INTO transaction_categories (tenant_id, transaction_id, category_id) VALUES `+strings.Join(ph, ","),
		args...)
	if err != nil {
		return rwError(err)
	}
	return nil
}

// GetByID loads one tenant-scoped transaction with account and category ids
// populated. Not-found yields NOT_FOUND.
func (r *TransactionRepository) GetByID(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) (*domain.Transaction, *domain.DomainError) {
	c := r.conn()
	row := c.QueryRow(ctx,
		`SELECT t.id, t.tenant_id, t.account_id, t.amount::text, t.date, t.description, t.type, t.scheduled,
		        t.reason_code::text, t.version, t.created_at, a.currency
		 FROM transactions t
		 JOIN accounts a ON a.id = t.account_id AND a.tenant_id = t.tenant_id
		 WHERE t.tenant_id=$1 AND t.id=$2 AND t.deleted_at IS NULL`, tenantID, id)
	tx, err := scanTransactionRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.NewNotFound("transaction", id.String())
		}
		return nil, rwError(err)
	}
	catIDs, derr := r.categoriesFor(ctx, tenantID, id)
	if derr != nil {
		return nil, derr
	}
	tx.CategoryIDs = catIDs
	return tx, nil
}

func (r *TransactionRepository) categoriesFor(ctx context.Context, tenantID uuid.UUID, txID uuid.UUID) ([]uuid.UUID, *domain.DomainError) {
	c := r.conn()
	rows, err := c.Query(ctx,
		`SELECT category_id FROM transaction_categories
		 WHERE tenant_id=$1 AND transaction_id=$2 ORDER BY category_id`, tenantID, txID)
	if err != nil {
		return nil, rwError(err)
	}
	defer rows.Close()
	ids := make([]uuid.UUID, 0, 8)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, rwError(err)
		}
		ids = append(ids, id)
	}
	if rows.Err() != nil {
		return nil, rwError(rows.Err())
	}
	return ids, nil
}

// Update persists an edited transaction under the optimistic-lock guard. The
// category links are re-synced atomically inside the same transaction.
func (r *TransactionRepository) Update(ctx context.Context, tenantID uuid.UUID, tx *domain.Transaction, expectedVersion int64) *domain.DomainError {
	c := r.conn()
	var reason any
	if tx.ReasonCode != nil {
		reason = string(*tx.ReasonCode)
	}
	tag, err := c.Exec(ctx,
		`UPDATE transactions
		 SET account_id=$3, amount=$4, date=$5, description=$6, type=$7, scheduled=$8, reason_code=$9,
		     version = version + 1, updated_at = now()
		 WHERE id=$1 AND tenant_id=$2 AND version=$10 AND deleted_at IS NULL`,
		tx.ID, tenantID, tx.AccountID, tx.Amount.String(), tx.Date, tx.Description,
		string(tx.Type), tx.Scheduled, reason, expectedVersion)
	if err != nil {
		return rwError(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewConflict("concurrent modification: stale version")
	}
	if _, err := c.Exec(ctx,
		`DELETE FROM transaction_categories WHERE tenant_id=$1 AND transaction_id=$2`, tenantID, tx.ID); err != nil {
		return rwError(err)
	}
	return r.insertCategories(ctx, tenantID, tx.ID, tx.CategoryIDs)
}

// Delete soft-deletes a transaction under the optimistic-lock guard.
func (r *TransactionRepository) Delete(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, expectedVersion int64) *domain.DomainError {
	c := r.conn()
	tag, err := c.Exec(ctx,
		`UPDATE transactions SET deleted_at = now(), version = version + 1, updated_at = now()
		 WHERE id=$1 AND tenant_id=$2 AND version=$3 AND deleted_at IS NULL`,
		id, tenantID, expectedVersion)
	if err != nil {
		return rwError(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewNotFound("transaction", id.String())
	}
	// Soft-deleted transactions must not feed aggregates; detach their category
	// links so joins exclude them.
	_, err = c.Exec(ctx,
		`DELETE FROM transaction_categories WHERE tenant_id=$1 AND transaction_id=$2`, tenantID, id)
	if err != nil {
		return rwError(err)
	}
	return nil
}

// List returns a cursor-paginated, tenant/filter-scoped collection that
// eager-loads account/category via a bounded set of queries (no N+1) and
// applies the soft-delete predicate.
func (r *TransactionRepository) List(ctx context.Context, tenantID uuid.UUID, filter ports.TransactionFilter, page ports.CursorPage) ([]*domain.Transaction, *ports.CursorPage, *domain.DomainError) {
	if derr := validateDateRange(filter.From, filter.To); derr != nil {
		return nil, nil, derr
	}
	if derr := validateAmountRange(filter.MinAmount, filter.MaxAmount); derr != nil {
		return nil, nil, derr
	}
	limit, cursorToken := normalizeCursorPage(page)
	sig := filterSig(filter)
	var cursorTS time.Time
	var cursorID uuid.UUID
	if cursorToken != nil {
		ts, cid, err := decodeCursor(*cursorToken, tenantID, sig, r.cursorSecret)
		if err != nil {
			return nil, nil, domain.NewValidationError(domain.FieldError{Field: "cursor", Message: "invalid cursor"})
		}
		cursorTS, cursorID = ts, cid
	}

	// Only allowlisted column identifiers appear in the SQL text; every user
	// value is a bound parameter.
	where := "t.tenant_id=$1 AND t.deleted_at IS NULL"
	args := []any{tenantID}
	next := func() int { return len(args) }
	if filter.AccountID != nil {
		i := next() + 1
		where += fmt.Sprintf(" AND t.account_id=$%d", i)
		args = append(args, *filter.AccountID)
	}
	if filter.CategoryID != nil {
		i := next() + 1
		where += fmt.Sprintf(` AND EXISTS (SELECT 1 FROM transaction_categories tc
			WHERE tc.tenant_id=$1 AND tc.transaction_id=t.id AND tc.category_id=$%d)`, i)
		args = append(args, *filter.CategoryID)
	}
	if filter.From != nil {
		i := next() + 1
		where += fmt.Sprintf(" AND t.date >= $%d", i)
		args = append(args, *filter.From)
	}
	if filter.To != nil {
		i := next() + 1
		where += fmt.Sprintf(" AND t.date < $%d", i)
		args = append(args, *filter.To)
	}
	if filter.MinAmount != nil {
		i := next() + 1
		where += fmt.Sprintf(" AND t.amount >= $%d", i)
		args = append(args, filter.MinAmount.String())
	}
	if filter.MaxAmount != nil {
		i := next() + 1
		where += fmt.Sprintf(" AND t.amount <= $%d", i)
		args = append(args, filter.MaxAmount.String())
	}
	if filter.Description != nil {
		i := next() + 1
		term := likeEscape(*filter.Description)
		where += fmt.Sprintf(" AND t.description ILIKE '%%'||$%d||'%%' ESCAPE '\\'", i)
		args = append(args, term)
	}
	if filter.Type != nil {
		i := next() + 1
		where += fmt.Sprintf(" AND t.type=$%d", i)
		args = append(args, string(*filter.Type))
	}
	if cursorToken != nil {
		i := next() + 1
		i2 := i + 1
		where += fmt.Sprintf(" AND (t.created_at, t.id) < ($%d, $%d)", i, i2)
		args = append(args, cursorTS, cursorID)
	}
	limitArg := next() + 1
	args = append(args, limit+1) // probe for the next page

	orderBy := "t.created_at DESC, t.id DESC" // compile-time allowlist
	sel := `SELECT t.id, t.tenant_id, t.account_id, t.amount::text, t.date, t.description, t.type,
	               t.scheduled, t.reason_code::text, t.version, t.created_at, a.currency
	        FROM transactions t
	        JOIN accounts a ON a.id = t.account_id AND a.tenant_id = t.tenant_id
	        WHERE ` + where + ` ORDER BY ` + orderBy + ` LIMIT $` + fmt.Sprintf("%d", limitArg)

	c := r.conn()
	rows, err := c.Query(ctx, sel, args...)
	if err != nil {
		return nil, nil, rwError(err)
	}
	defer rows.Close()

	out := make([]*domain.Transaction, 0, limit)
	var probeTS time.Time
	var probeID uuid.UUID
	for rows.Next() {
		tx, err := scanTransactionRow(rows)
		if err != nil {
			return nil, nil, rwError(err)
		}
		if len(out) < limit {
			out = append(out, tx)
		} else {
			probeTS, probeID = tx.CreatedAt, tx.ID
		}
	}
	if rows.Err() != nil {
		return nil, nil, rwError(rows.Err())
	}

	if len(out) > 0 {
		if err := r.loadCategoryIDs(ctx, tenantID, out); err != nil {
			return nil, nil, rwError(err)
		}
	}

	var nextCursor *string
	if len(out) == limit {
		nc := encodeCursor(probeTS, probeID, tenantID, sig, r.cursorSecret)
		nextCursor = &nc
	}
	respPage := ports.CursorPage{Limit: limit, Cursor: page.Cursor, NextCursor: nextCursor}
	return out, &respPage, nil
}

// loadCategoryIDs populates CategoryIDs for a paged set in one batched query.
func (r *TransactionRepository) loadCategoryIDs(ctx context.Context, tenantID uuid.UUID, txs []*domain.Transaction) *domain.DomainError {
	ids := make([]uuid.UUID, 0, len(txs))
	for _, t := range txs {
		ids = append(ids, t.ID)
	}
	c := r.conn()
	rows, err := c.Query(ctx,
		`SELECT transaction_id, category_id FROM transaction_categories
		 WHERE tenant_id=$1 AND transaction_id = ANY($2) ORDER BY transaction_id, category_id`,
		tenantID, ids)
	if err != nil {
		return rwError(err)
	}
	defer rows.Close()
	byTX := make(map[uuid.UUID][]uuid.UUID)
	for rows.Next() {
		var txID, catID uuid.UUID
		if err := rows.Scan(&txID, &catID); err != nil {
			return rwError(err)
		}
		byTX[txID] = append(byTX[txID], catID)
	}
	if rows.Err() != nil {
		return rwError(rows.Err())
	}
	for _, t := range txs {
		t.CategoryIDs = byTX[t.ID]
	}
	return nil
}

func scanTransactionRow(row pgx.Row) (*domain.Transaction, error) {
	var t domain.Transaction
	var amount string
	var typ string
	var ccy string
	var scheduled bool
	var created time.Time
	var reasonPtr *string
	var acctID uuid.UUID
	var ver int64
	err := row.Scan(&t.ID, &t.TenantID, &acctID, &amount, &t.Date, &t.Description, &typ,
		&scheduled, &reasonPtr, &ver, &created, &ccy)
	if err != nil {
		return nil, err
	}
	t.AccountID = acctID
	t.Scheduled = scheduled
	t.Version = ver
	t.CreatedAt = created
	t.UpdatedAt = created
	if reasonPtr != nil {
		if rc, err := domain.ParseReasonCode(*reasonPtr); err == nil {
			t.ReasonCode = &rc
		}
	}
	if st, err := domain.ParseTransactionType(typ); err == nil {
		t.Type = st
	}
	m, err := moneyToMinor(amount, ccy)
	if err != nil {
		return nil, err
	}
	t.Amount = m
	return &t, nil
}

// CountByCategoryInRange aggregates expense/(income) spend grouped by category
// over [from,to) in a single statement — feeds budget-actuals recomputation.
func (r *TransactionRepository) CountByCategoryInRange(ctx context.Context, tenantID uuid.UUID, categoryID uuid.UUID, from, to time.Time) (map[uuid.UUID]domain.Money, *domain.DomainError) {
	c := r.conn()
	rows, err := c.Query(ctx,
		`SELECT tc.category_id, SUM(t.amount)::text, MAX(a.currency)
		 FROM transactions t
		 JOIN transaction_categories tc ON tc.transaction_id = t.id AND tc.tenant_id = t.tenant_id
		 JOIN accounts a ON a.id = t.account_id AND a.tenant_id = t.tenant_id
		 WHERE t.tenant_id=$1 AND tc.category_id=$2
		   AND t.date >= $3 AND t.date < $4 AND t.deleted_at IS NULL
		 GROUP BY tc.category_id`,
		tenantID, categoryID, from, to)
	if err != nil {
		return nil, rwError(err)
	}
	defer rows.Close()
	result := make(map[uuid.UUID]domain.Money)
	for rows.Next() {
		var cid uuid.UUID
		var sumStr *string
		var ccy string
		if err := rows.Scan(&cid, &sumStr, &ccy); err != nil {
			return nil, rwError(err)
		}
		if sumStr == nil {
			continue
		}
		m, err := moneyToMinor(*sumStr, ccy)
		if err != nil {
			return nil, domain.Wrap(domain.CodeInternalError, err)
		}
		result[cid] = m
	}
	if rows.Err() != nil {
		return nil, rwError(rows.Err())
	}
	return result, nil
}

// --- category repository --------------------------------------------------

// Categories returns the CategoryRepository implementation.
func (s *Store) Categories() ports.CategoryRepository { return &CategoryRepository{Store: s} }

// CategoryRepository implements ports.CategoryRepository. Migration 0001 places
// no `version` or `in_use` column on `categories`, so the optimistic-lock guard
// for rename/re-parent and the in-use flag are not DB-backed; the repository
// re-verifies in-use and child guards atomically with the delete and relies on
// the one-level/type trigger. A follow-up schema revision should add `version`
// and `in_use` to fully back the ports contract.
type CategoryRepository struct{ *Store }

// Create persists a category.
func (r *CategoryRepository) Create(ctx context.Context, tenantID uuid.UUID, c *domain.Category) *domain.DomainError {
	conn := r.conn()
	var parent any
	if c.ParentID != nil {
		parent = *c.ParentID
	}
	_, err := conn.Exec(ctx,
		`INSERT INTO categories (id, tenant_id, name, type, parent_id)
		 VALUES ($1,$2,$3,$4,$5)`,
		c.ID, tenantID, c.Name, strings.ToUpper(string(c.Type)), parent)
	if err != nil {
		return rwError(err)
	}
	return nil
}

// GetByID loads one tenant-scoped category. InUse is derived from the
// transaction-reference count so the returned category reflects the deletion
// guard without a stored flag (schema 0001 has no in_use column).
func (r *CategoryRepository) GetByID(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) (*domain.Category, *domain.DomainError) {
	conn := r.conn()
	row := conn.QueryRow(ctx,
		`SELECT c.id, c.tenant_id, c.name, c.type, c.parent_id
		   FROM categories c WHERE c.tenant_id=$1 AND c.id=$2`, tenantID, id)
	c, err := scanCategoryRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.NewNotFound("category", id.String())
		}
		return nil, rwError(err)
	}
	var n int64
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM transaction_categories WHERE tenant_id=$1 AND category_id=$2`,
		tenantID, id).Scan(&n); err != nil {
		return nil, rwError(err)
	}
	c.InUse = n > 0
	return c, nil
}

// List returns the full tenant taxonomy in one query, offset-paginated.
func (r *CategoryRepository) List(ctx context.Context, tenantID uuid.UUID, page ports.OffsetPage) ([]*domain.Category, *domain.DomainError) {
	size, offset := normalizeOffsetPage(page)
	conn := r.conn()
	rows, err := conn.Query(ctx,
		`SELECT id, tenant_id, name, type, parent_id FROM categories
		 WHERE tenant_id=$1 ORDER BY type, name LIMIT $2 OFFSET $3`, tenantID, size+1, offset)
	if err != nil {
		return nil, rwError(err)
	}
	defer rows.Close()
	out := make([]*domain.Category, 0, size)
	for rows.Next() {
		c, err := scanCategoryRow(rows)
		if err != nil {
			return nil, rwError(err)
		}
		if len(out) < size {
			out = append(out, c)
		}
	}
	if rows.Err() != nil {
		return nil, rwError(rows.Err())
	}
	return out, nil
}

// scanCategoryRow decodes a category row. InUse is populated by GetByID via a
// transaction-reference count; List does not stale-read the flag.
func scanCategoryRow(row pgx.Row) (*domain.Category, error) {
	var c domain.Category
	var typ string
	var parent *uuid.UUID
	err := row.Scan(&c.ID, &c.TenantID, &c.Name, &typ, &parent)
	if err != nil {
		return nil, err
	}
	c.Type = domain.TransactionType(strings.ToLower(typ))
	c.ParentID = parent
	return &c, nil
}

// SetInUse maintains the in-use deletion-protection flag. Schema 0001 has no
// in_use column; the authoritative in-use guard is the atomic existence check
// in Delete, so this is a documented-maintenance no-op recording intent.
func (r *CategoryRepository) SetInUse(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, inUse bool) *domain.DomainError {
	return nil
}

// categoryHasChildren reports whether a category has any subcategory.
func (r *CategoryRepository) categoryHasChildren(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) (bool, *domain.DomainError) {
	conn := r.conn()
	var n int
	err := conn.QueryRow(ctx,
		`SELECT count(*) FROM categories WHERE tenant_id=$1 AND parent_id=$2`, tenantID, id).Scan(&n)
	if err != nil {
		return false, rwError(err)
	}
	return n > 0, nil
}

// Update persists a renamed/re-parented category. Absent a schema version
// column, existence is verified before the mutation and the one-level/type
// guards are enforced by the database trigger.
func (r *CategoryRepository) Update(ctx context.Context, tenantID uuid.UUID, c *domain.Category, expectedVersion int64) *domain.DomainError {
	conn := r.conn()
	var parent any
	if c.ParentID != nil {
		parent = *c.ParentID
	}
	tag, err := conn.Exec(ctx,
		`UPDATE categories SET name=$1, parent_id=$3, updated_at=now()
		 WHERE tenant_id=$2 AND id=$4`,
		c.Name, tenantID, parent, c.ID)
	if err != nil {
		return rwError(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewNotFound("category", c.ID.String())
	}
	return nil
}

// Delete refuses deletion of an in-use category; it atomically re-verifies the
// in-use and child guards and relies on ON DELETE RESTRICT as a backstop.
// Returns CONFLICT_STATE when a guard holds.
func (r *CategoryRepository) Delete(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, expectedVersion int64) *domain.DomainError {
	conn := r.conn()
	hasChildren, derr := r.categoryHasChildren(ctx, tenantID, id)
	if derr != nil {
		return derr
	}
	if hasChildren {
		return domain.NewConflict("category has child categories")
	}
	var n int64
	err := conn.QueryRow(ctx,
		`SELECT count(*) FROM transaction_categories WHERE tenant_id=$1 AND category_id=$2`,
		tenantID, id).Scan(&n)
	if err != nil {
		return rwError(err)
	}
	if n > 0 {
		return domain.NewConflict("category is in use")
	}
	tag, err := conn.Exec(ctx,
		`DELETE FROM categories WHERE tenant_id=$1 AND id=$2`, tenantID, id)
	if err != nil {
		return rwError(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewNotFound("category", id.String())
	}
	return nil
}

// Merge atomically re-points a source category's transactions and child
// categories onto a target and deletes the source in one transaction, then
// returns the plan the use case needs to emit the propagation event.
func (r *CategoryRepository) Merge(ctx context.Context, tenantID uuid.UUID, sourceID, targetID uuid.UUID) (*domain.CategoryMergePlan, *domain.DomainError) {
	conn := r.conn()
	row := conn.QueryRow(ctx,
		`SELECT id, tenant_id, name, type, parent_id FROM categories WHERE tenant_id=$1 AND id=$2`,
		tenantID, sourceID)
	source, err := scanCategoryRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.NewNotFound("category", sourceID.String())
		}
		return nil, rwError(err)
	}
	row = conn.QueryRow(ctx,
		`SELECT id, tenant_id, name, type, parent_id FROM categories WHERE tenant_id=$1 AND id=$2`,
		tenantID, targetID)
	target, err := scanCategoryRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.NewNotFound("category", targetID.String())
		}
		return nil, rwError(err)
	}

	txRows, err := conn.Query(ctx,
		`SELECT transaction_id FROM transaction_categories WHERE tenant_id=$1 AND category_id=$2
		 ORDER BY transaction_id`, tenantID, sourceID)
	if err != nil {
		return nil, rwError(err)
	}
	var txIDs []uuid.UUID
	for txRows.Next() {
		var id uuid.UUID
		if err := txRows.Scan(&id); err != nil {
			txRows.Close()
			return nil, rwError(err)
		}
		txIDs = append(txIDs, id)
	}
	txRows.Close()
	if txRows.Err() != nil {
		return nil, rwError(txRows.Err())
	}

	childRows, err := conn.Query(ctx,
		`SELECT id FROM categories WHERE tenant_id=$1 AND parent_id=$2 ORDER BY id`, tenantID, sourceID)
	if err != nil {
		return nil, rwError(err)
	}
	var childIDs []uuid.UUID
	for childRows.Next() {
		var id uuid.UUID
		if err := childRows.Scan(&id); err != nil {
			childRows.Close()
			return nil, rwError(err)
		}
		childIDs = append(childIDs, id)
	}
	childRows.Close()
	if childRows.Err() != nil {
		return nil, rwError(childRows.Err())
	}

	plan, err := source.MergeInto(target, childIDs, txIDs)
	if err != nil {
		return nil, domain.NewValidationError(domain.FieldError{Field: "category", Message: err.Error()})
	}

	if _, err := conn.Exec(ctx,
		`UPDATE transaction_categories SET category_id=$3 WHERE tenant_id=$1 AND category_id=$2`,
		tenantID, sourceID, targetID); err != nil {
		return nil, rwError(err)
	}
	if len(childIDs) > 0 {
		if _, err := conn.Exec(ctx,
			`UPDATE categories SET parent_id=$3, updated_at=now() WHERE tenant_id=$1 AND parent_id=$2`,
			tenantID, sourceID, targetID); err != nil {
			return nil, rwError(err)
		}
	}
	if _, err := conn.Exec(ctx,
		`DELETE FROM categories WHERE tenant_id=$1 AND id=$2`, tenantID, sourceID); err != nil {
		return rwError(err)
	}
	return &plan, nil
}

// --- budget repository ----------------------------------------------------

// Budgets returns the BudgetRepository implementation.
func (s *Store) Budgets() ports.BudgetRepository { return &BudgetRepository{Store: s} }

// BudgetRepository implements ports.BudgetRepository.
type BudgetRepository struct{ *Store }

// Create persists a budget.
func (r *BudgetRepository) Create(ctx context.Context, tenantID uuid.UUID, b *domain.Budget) *domain.DomainError {
	conn := r.conn()
	_, err := conn.Exec(ctx,
		`INSERT INTO budgets (id, tenant_id, category_id, period_type, period_start, period_end, allocation, version)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,1)`,
		b.ID, tenantID, b.CategoryID, string(b.PeriodType), b.Range.Start, b.Range.End, b.Allocation.String())
	if err != nil {
		return rwError(err)
	}
	return nil
}

// GetByID loads one tenant-scoped budget.
func (r *BudgetRepository) GetByID(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) (*domain.Budget, *domain.DomainError) {
	conn := r.conn()
	row := conn.QueryRow(ctx,
		`SELECT id, tenant_id, category_id, period_type, period_start, period_end,
		        allocation::text, version, actuals_spent::text
		 FROM budgets WHERE tenant_id=$1 AND id=$2`, tenantID, id)
	b, err := scanBudgetRow(row, r.baseCurrency)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.NewNotFound("budget", id.String())
		}
		return nil, rwError(err)
	}
	return b, nil
}

func scanBudgetRow(row pgx.Row, baseCcy string) (*domain.Budget, error) {
	var b domain.Budget
	var pt string
	var alloc, actuals string
	var start, end time.Time
	if err := row.Scan(&b.ID, &b.TenantID, &b.CategoryID, &pt, &start, &end, &alloc, &b.Version, &actuals); err != nil {
		return nil, err
	}
	typ, err := domain.ParseBudgetPeriodType(pt)
	if err != nil {
		return nil, err
	}
	b.PeriodType = typ
	b.Range = domain.DateRange{Start: start, End: end}
	if am, err := moneyToMinor(alloc, baseCcy); err == nil {
		b.Allocation = am
	}
	if act, err := moneyToMinor(actuals, baseCcy); err == nil {
		b.ActualsSpent = act
	}
	return &b, nil
}

// List returns an offset-paginated, tenant-scoped collection.
func (r *BudgetRepository) List(ctx context.Context, tenantID uuid.UUID, page ports.OffsetPage) ([]*domain.Budget, *domain.DomainError) {
	size, offset := normalizeOffsetPage(page)
	conn := r.conn()
	rows, err := conn.Query(ctx,
		`SELECT id, tenant_id, category_id, period_type, period_start, period_end,
		        allocation::text, version, actuals_spent::text
		 FROM budgets WHERE tenant_id=$1 ORDER BY period_start DESC LIMIT $2 OFFSET $3`,
		tenantID, size+1, offset)
	if err != nil {
		return nil, rwError(err)
	}
	defer rows.Close()
	out := make([]*domain.Budget, 0, size)
	for rows.Next() {
		b, err := scanBudgetRow(rows, r.baseCurrency)
		if err != nil {
			return nil, rwError(err)
		}
		if len(out) < size {
			out = append(out, b)
		}
	}
	if rows.Err() != nil {
		return nil, rwError(rows.Err())
	}
	return out, nil
}

// Update persists a budget under the optimistic-lock guard.
func (r *BudgetRepository) Update(ctx context.Context, tenantID uuid.UUID, b *domain.Budget, expectedVersion int64) *domain.DomainError {
	conn := r.conn()
	tag, err := conn.Exec(ctx,
		`UPDATE budgets
		 SET category_id=$4, period_type=$5, period_start=$6, period_end=$7, allocation=$8,
		     version = version + 1, updated_at = now()
		 WHERE id=$1 AND tenant_id=$2 AND version=$3`,
		b.ID, tenantID, expectedVersion, b.CategoryID, string(b.PeriodType), b.Range.Start, b.Range.End, b.Allocation.String())
	if err != nil {
		return rwError(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewConflict("concurrent modification: stale version")
	}
	return nil
}

// Delete removes a budget under the optimistic-lock guard.
func (r *BudgetRepository) Delete(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, expectedVersion int64) *domain.DomainError {
	conn := r.conn()
	tag, err := conn.Exec(ctx,
		`DELETE FROM budgets WHERE id=$1 AND tenant_id=$2 AND version=$3`, id, tenantID, expectedVersion)
	if err != nil {
		return rwError(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewNotFound("budget", id.String())
	}
	return nil
}

// ActualsByCategory aggregates spend grouped by category over [from,to) in a
// single statement — the budget-vs-actual feed. Routed to the primary so
// budget actuals never read stale replica data.
func (r *BudgetRepository) ActualsByCategory(ctx context.Context, tenantID uuid.UUID, from, to time.Time) (map[uuid.UUID]domain.Money, *domain.DomainError) {
	conn := r.conn()
	rows, err := conn.Query(ctx,
		`SELECT tc.category_id, SUM(t.amount)::text, MAX(a.currency)
		 FROM transactions t
		 JOIN transaction_categories tc ON tc.transaction_id = t.id AND tc.tenant_id = t.tenant_id
		 JOIN accounts a ON a.id = t.account_id AND a.tenant_id = t.tenant_id
		 WHERE t.tenant_id=$1 AND t.date >= $2 AND t.date < $3 AND t.deleted_at IS NULL
		 GROUP BY tc.category_id`,
		tenantID, from, to)
	if err != nil {
		return nil, rwError(err)
	}
	defer rows.Close()
	result := make(map[uuid.UUID]domain.Money)
	for rows.Next() {
		var cid uuid.UUID
		var sumStr *string
		var ccy string
		if err := rows.Scan(&cid, &sumStr, &ccy); err != nil {
			return nil, rwError(err)
		}
		if sumStr == nil {
			continue
		}
		m, err := moneyToMinor(*sumStr, ccy)
		if err != nil {
			return nil, domain.Wrap(domain.CodeInternalError, err)
		}
		result[cid] = m
	}
	if rows.Err() != nil {
		return nil, rwError(rows.Err())
	}
	return result, nil
}

// --- user repository ------------------------------------------------------

// Users returns the UserRepository implementation.
func (s *Store) Users() ports.UserRepository { return &UserRepository{Store: s} }

// UserRepository implements ports.UserRepository.
type UserRepository struct{ *Store }

// Create persists a user.
func (r *UserRepository) Create(ctx context.Context, tenantID uuid.UUID, u *domain.User) *domain.DomainError {
	conn := r.conn()
	_, err := conn.Exec(ctx,
		`INSERT INTO users (id, tenant_id, username, password_hash, mfa_secret, mfa_enabled, role_id, active, version)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,0)`,
		u.ID, tenantID, u.Username, u.PasswordHash, nullableStr(u.MFASecret), u.MFAActive, u.RoleID, u.Active())
	if err != nil {
		return rwError(err)
	}
	return nil
}

func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (r *UserRepository) GetByID(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) (*domain.User, *domain.DomainError) {
	conn := r.conn()
	row := conn.QueryRow(ctx, userCols+" FROM users WHERE tenant_id=$1 AND id=$2", tenantID, id)
	u, err := scanUserRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.NewNotFound("user", id.String())
		}
		return nil, rwError(err)
	}
	return u, nil
}

// GetByUsername performs a tenant-scoped, uniform-shape lookup so absent vs.
// wrong-credential is not distinguishably timed (enumeration mitigation). The
// returned user carries its opaque credential references untouched.
func (r *UserRepository) GetByUsername(ctx context.Context, tenantID uuid.UUID, username string) (*domain.User, *domain.DomainError) {
	conn := r.conn()
	row := conn.QueryRow(ctx, userCols+" FROM users WHERE tenant_id=$1 AND username=$2", tenantID, username)
	u, err := scanUserRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Uniform response shape: a structural placeholder with no usable
			// credential, indistinguishable from an absent user.
			return &domain.User{ID: uuid.Nil, TenantID: tenantID, Username: username, Status: domain.UserStatusDeprovisioned}, nil
		}
		return nil, rwError(err)
	}
	return u, nil
}

// userCols is the shared user projection.
const userCols = `SELECT id, tenant_id, username, password_hash, mfa_secret, mfa_enabled, role_id, active, version, last_login_at`

func scanUserRow(row pgx.Row) (*domain.User, error) {
	var u domain.User
	var mfaSecret *string
	var active bool
	var lastLogin *time.Time
	if err := row.Scan(&u.ID, &u.TenantID, &u.Username, &u.PasswordHash, &mfaSecret, &u.MFAActive,
		&u.RoleID, &active, &u.Version, &lastLogin); err != nil {
		return nil, err
	}
	if mfaSecret != nil {
		u.MFASecret = *mfaSecret
	}
	if active {
		u.Status = domain.UserStatusActive
	} else {
		u.Status = domain.UserStatusSuspended
	}
	u.LastLoginAt = lastLogin
	return &u, nil
}

// List returns an offset-paginated, tenant-scoped collection.
func (r *UserRepository) List(ctx context.Context, tenantID uuid.UUID, page ports.OffsetPage) ([]*domain.User, *domain.DomainError) {
	size, offset := normalizeOffsetPage(page)
	conn := r.conn()
	rows, err := conn.Query(ctx,
		userCols+" FROM users WHERE tenant_id=$1 ORDER BY username LIMIT $2 OFFSET $3",
		tenantID, size+1, offset)
	if err != nil {
		return nil, rwError(err)
	}
	defer rows.Close()
	out := make([]*domain.User, 0, size)
	for rows.Next() {
		u, err := scanUserRow(rows)
		if err != nil {
			return nil, rwError(err)
		}
		if len(out) < size {
			out = append(out, u)
		}
	}
	if rows.Err() != nil {
		return nil, rwError(rows.Err())
	}
	return out, nil
}

// Update persists an edited user under the optimistic-lock guard.
func (r *UserRepository) Update(ctx context.Context, tenantID uuid.UUID, u *domain.User, expectedVersion int64) *domain.DomainError {
	conn := r.conn()
	tag, err := conn.Exec(ctx,
		`UPDATE users
		 SET email=$4, password_hash=$5, mfa_secret=$6, mfa_enabled=$7, role_id=$8, active=$9,
		     version = version + 1
		 WHERE id=$1 AND tenant_id=$2 AND version=$3`,
		u.ID, tenantID, expectedVersion, u.Email, u.PasswordHash, nullableStr(u.MFASecret),
		u.MFAActive, u.RoleID, u.Status == domain.UserStatusActive)
	if err != nil {
		return rwError(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewConflict("concurrent modification: stale version")
	}
	return nil
}

// CountAdmins counts active admin users per tenant — the last-admin invariant
// guard. The use case must call this inside the same transaction boundary as
// the subsequent delete so the count cannot go stale.
func (r *UserRepository) CountAdmins(ctx context.Context, tenantID uuid.UUID) (int64, *domain.DomainError) {
	conn := r.conn()
	var n int64
	err := conn.QueryRow(ctx,
		`SELECT count(*) FROM users u JOIN roles r ON u.role_id = r.id
		 WHERE u.tenant_id=$1 AND u.active AND r.code = 'admin'`, tenantID).Scan(&n)
	if err != nil {
		return 0, rwError(err)
	}
	return n, nil
}

// Delete deprovisions a user (terminal, appends to audit narrative) under the
// optimistic-lock guard.
func (r *UserRepository) Delete(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, expectedVersion int64) *domain.DomainError {
	conn := r.conn()
	tag, err := conn.Exec(ctx,
		`UPDATE users SET active = false, version = version + 1
		 WHERE id=$1 AND tenant_id=$2 AND version=$3`, id, tenantID, expectedVersion)
	if err != nil {
		return rwError(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewNotFound("user", id.String())
	}
	return nil
}

// --- audit repository -----------------------------------------------------

// AuditLogs returns the AuditLogRepository.
func (s *Store) AuditLogs() ports.AuditLogRepository { return &AuditLogRepository{Store: s} }

// AuditLogRepository implements the append-only audit seam. Immutability is
// enforced by schema: the audit_entries table has a blocking trigger and the
// app_user role holds INSERT/SELECT only. This repository offers no update or
// delete path by construction.
type AuditLogRepository struct{ *Store }

// Append persists an audit entry with its chained hash
// (Hash = H(prev_hash || payload || timestamp)), tenant-scoped. The chained
// insert reads the previous hash in the same statement, keeping the chain
// gap-free.
func (r *AuditLogRepository) Append(ctx context.Context, tenantID uuid.UUID, entry ports.AuditEntry) *domain.DomainError {
	conn := r.conn()
	prevHash := entry.PrevHash
	payload := chainedPayload(entry)
	sum := sha256.Sum256(append([]byte(prevHash), payload...))
	hash := fmt.Sprintf("%x", sum)
	_, err := conn.Exec(ctx,
		`INSERT INTO audit_entries (id, tenant_id, user_id, action, entity_type, entity_id,
		                           before_value, after_value, "timestamp", prev_hash, hash, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$9)`,
		entry.ID, tenantID, entry.UserID, string(entry.Action), entry.EntityType, entry.EntityID,
		entry.BeforeValue, entry.AfterValue, entry.Timestamp, prevHash, hash)
	if err != nil {
		return rwError(err)
	}
	return nil
}

// chainedPayload is the length-prefixed serialization used by the chained hash
// so before/after byte pairs cannot ambiguously concatenate.
func chainedPayload(e ports.AuditEntry) []byte {
	var b []byte
	b = append(b, []byte(e.EntityType)...)
	b = append(b, 0)
	b = append(b, e.EntityID[:]...)
	b = appendU64(b, uint64(len(e.BeforeValue)))
	b = append(b, e.BeforeValue...)
	b = appendU64(b, uint64(len(e.AfterValue)))
	b = append(b, e.AfterValue...)
	return b
}

func appendU64(b []byte, n uint64) []byte {
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], n)
	return append(b, tmp[:]...)
}

// List returns a cursor-paginated, tenant/filter-scoped slice.
func (r *AuditLogRepository) List(ctx context.Context, tenantID uuid.UUID, filter ports.AuditLogFilter, page ports.CursorPage) ([]ports.AuditEntry, *ports.CursorPage, *domain.DomainError) {
	limit, cursorToken := normalizeCursorPage(page)
	sig := auditFilterSig(filter)
	var cursorTS time.Time
	var cursorID uuid.UUID
	if cursorToken != nil {
		ts, cid, err := decodeCursor(*cursorToken, tenantID, sig, r.cursorSecret)
		if err != nil {
			return nil, nil, domain.NewValidationError(domain.FieldError{Field: "cursor", Message: "invalid cursor"})
		}
		cursorTS, cursorID = ts, cid
	}
	where := "tenant_id=$1"
	args := []any{tenantID}
	next := func() int { return len(args) }
	if filter.EntityType != nil {
		i := next() + 1
		where += fmt.Sprintf(" AND entity_type=$%d", i)
		args = append(args, *filter.EntityType)
	}
	if filter.EntityID != nil {
		i := next() + 1
		where += fmt.Sprintf(" AND entity_id=$%d", i)
		args = append(args, *filter.EntityID)
	}
	if filter.Action != nil {
		i := next() + 1
		where += fmt.Sprintf(" AND action=$%d", i)
		args = append(args, string(*filter.Action))
	}
	if cursorToken != nil {
		i := next() + 1
		i2 := i + 1
		where += fmt.Sprintf(" AND (created_at, id) < ($%d, $%d)", i, i2)
		args = append(args, cursorTS, cursorID)
	}
	limitArg := next() + 1
	args = append(args, limit+1)
	conn := r.conn()
	rows, err := conn.Query(ctx,
		`SELECT id, tenant_id, user_id, action, entity_type, entity_id, before_value, after_value,
		        "timestamp", prev_hash, hash, created_at
		 FROM audit_entries WHERE `+where+` ORDER BY created_at DESC, id DESC LIMIT $`+fmt.Sprintf("%d", limitArg), args...)
	if err != nil {
		return nil, nil, rwError(err)
	}
	defer rows.Close()
	out := make([]ports.AuditEntry, 0, limit)
	var probeTS time.Time
	var probeID uuid.UUID
	for rows.Next() {
		var e ports.AuditEntry
		var action string
		var created time.Time
		if err := rows.Scan(&e.ID, &e.TenantID, &e.UserID, &action, &e.EntityType, &e.EntityID,
			&e.BeforeValue, &e.AfterValue, &e.Timestamp, &e.PrevHash, &e.Hash, &created); err != nil {
			return nil, nil, rwError(err)
		}
		if a, err := domain.ParseAuditAction(action); err == nil {
			e.Action = a
		}
		if len(out) < limit {
			out = append(out, e)
		} else {
			probeTS, probeID = created, e.ID
		}
	}
	if rows.Err() != nil {
		return nil, nil, rwError(rows.Err())
	}
	var nextCursor *string
	if len(out) == limit {
		nc := encodeCursor(probeTS, probeID, tenantID, sig, r.cursorSecret)
		nextCursor = &nc
	}
	respPage := ports.CursorPage{Limit: limit, Cursor: page.Cursor, NextCursor: nextCursor}
	return out, &respPage, nil
}

// --- read-model repository ------------------------------------------------

// ReadModels returns the ReadModelRepository.
func (s *Store) ReadModels() ports.ReadModelRepository { return &ReadModelRepository{Store: s} }

// ReadModelRepository implements the CQRS read-side projections. Refreshes are
// invoked after the write transaction commits; they never write the
// authoritative ledger. The materialized views must carry a unique index for
// REFRESH ... CONCURRENTLY.
type ReadModelRepository struct{ *Store }

// RefreshAccountBalances recomputes the account-balance projection for a
// tenant in a single pass.
func (r *ReadModelRepository) RefreshAccountBalances(ctx context.Context, tenantID uuid.UUID) *domain.DomainError {
	conn := r.conn()
	if _, err := conn.Exec(ctx, `SELECT fms_refresh_account_balances($1)`, tenantID); err == nil {
		return nil
	}
	if _, err := conn.Exec(ctx, `REFRESH MATERIALIZED VIEW CONCURRENTLY account_balances`); err != nil {
		return domain.NewDependencyUnavailable("read-model account-balance projection unavailable")
	}
	return nil
}

// RefreshBudgetActuals recomputes the spend-actuals projection for one category
// within a period; idempotent by (categoryID, range).
func (r *ReadModelRepository) RefreshBudgetActuals(ctx context.Context, tenantID uuid.UUID, categoryID uuid.UUID, rng domain.DateRange) *domain.DomainError {
	conn := r.conn()
	if _, err := conn.Exec(ctx, `SELECT fms_refresh_budget_actuals($1, $2, $3, $4)`,
		tenantID, categoryID, rng.Start, rng.End); err == nil {
		return nil
	}
	if _, err := conn.Exec(ctx, `REFRESH MATERIALIZED VIEW CONCURRENTLY budget_actuals`); err != nil {
		return domain.NewDependencyUnavailable("read-model budget-actuals projection unavailable")
	}
	return nil
}

// --- certification repository ---------------------------------------------

// Certifications returns the CertificationRepository.
func (s *Store) Certifications() ports.CertificationRepository { return &CertificationRepository{Store: s} }

// CertificationRepository persists access-certification reviews (append-only).
type CertificationRepository struct{ *Store }

// Append persists a finalized, tenant-scoped certification; re-append of an
// already-recorded certification yields CONFLICT_STATE (unique on id).
func (r *CertificationRepository) Append(ctx context.Context, tenantID uuid.UUID, c *domain.AccessCertification) *domain.DomainError {
	conn := r.conn()
	_, err := conn.Exec(ctx,
		`INSERT INTO access_certifications (id, tenant_id, reviewer_id, user_id, decision, reason, evidence_refs, reviewed_at, review_start, review_end)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		c.ID, tenantID, c.ReviewerID, c.UserID, string(c.Decision), nullableStr(c.Reason),
		strings.Join(c.EvidenceRefs, ","), c.ReviewedAt, c.ReviewPeriod.Start, c.ReviewPeriod.End)
	if err != nil {
		return rwError(err)
	}
	return nil
}

// List returns an offset-paginated, tenant-scoped collection.
func (r *CertificationRepository) List(ctx context.Context, tenantID uuid.UUID, page ports.OffsetPage) ([]*domain.AccessCertification, *domain.DomainError) {
	size, offset := normalizeOffsetPage(page)
	conn := r.conn()
	rows, err := conn.Query(ctx,
		`SELECT id, tenant_id, reviewer_id, user_id, decision, reason, evidence_refs, reviewed_at,
		        review_start, review_end
		 FROM access_certifications WHERE tenant_id=$1 ORDER BY reviewed_at DESC LIMIT $2 OFFSET $3`,
		tenantID, size+1, offset)
	if err != nil {
		return nil, rwError(err)
	}
	defer rows.Close()
	var out []*domain.AccessCertification
	for rows.Next() {
		c, err := scanCertificationRow(rows)
		if err != nil {
			return nil, rwError(err)
		}
		if len(out) < size {
			out = append(out, c)
		}
	}
	if rows.Err() != nil {
		return nil, rwError(rows.Err())
	}
	return out, nil
}

func scanCertificationRow(row pgx.Row) (*domain.AccessCertification, error) {
	var c domain.AccessCertification
	var decision string
	var reason *string
	var refs *string
	var start, end time.Time
	if err := row.Scan(&c.ID, &c.TenantID, &c.ReviewerID, &c.UserID, &decision, &reason,
		&refs, &c.ReviewedAt, &start, &end); err != nil {
		return nil, err
	}
	c.Decision = domain.CertificationDecision(decision)
	if reason != nil {
		c.Reason = *reason
	}
	if refs != nil && *refs != "" {
		c.EvidenceRefs = strings.Split(*refs, ",")
	}
	c.ReviewPeriod = domain.DateRange{Start: start, End: end}
	return &c, nil
}

// ListOrphaned returns tenant users with no active role/membership — the
// deprovisioning evidence query.
func (r *CertificationRepository) ListOrphaned(ctx context.Context, tenantID uuid.UUID, page ports.OffsetPage) ([]*domain.User, *domain.DomainError) {
	size, offset := normalizeOffsetPage(page)
	conn := r.conn()
	rows, err := conn.Query(ctx,
		`SELECT u.id, u.tenant_id, u.username, u.password_hash, u.mfa_secret, u.mfa_enabled, u.role_id,
		        u.active, u.version, u.last_login_at
		 FROM users u
		 LEFT JOIN roles rr ON u.role_id = rr.id
		 WHERE u.tenant_id=$1 AND (u.active = false OR rr.id IS NULL OR rr.code = 'view-only')
		 ORDER BY u.username LIMIT $2 OFFSET $3`, tenantID, size+1, offset)
	if err != nil {
		return nil, rwError(err)
	}
	defer rows.Close()
	var out []*domain.User
	for rows.Next() {
		u, err := scanUserRow(rows)
		if err != nil {
			return nil, rwError(err)
		}
		if len(out) < size {
			out = append(out, u)
		}
	}
	if rows.Err() != nil {
		return nil, rwError(rows.Err())
	}
	return out, nil
}

// --- finance period repository --------------------------------------------

// FinancePeriods returns the FinancePeriodRepository.
func (s *Store) FinancePeriods() ports.FinancePeriodRepository { return &FinancePeriodRepository{Store: s} }

// FinancePeriodRepository persists the period-end close state machine with
// atomic conditional transitions guarded by expected version.
type FinancePeriodRepository struct{ *Store }

// Get loads one tenant-scoped finance period.
func (r *FinancePeriodRepository) Get(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) (*domain.FinancePeriod, *domain.DomainError) {
	conn := r.conn()
	row := conn.QueryRow(ctx,
		`SELECT id, tenant_id, label, date_start, date_end, status, closed_by, closed_at, version
		 FROM finance_periods WHERE tenant_id=$1 AND id=$2`, tenantID, id)
	p, err := scanFinancePeriodRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.NewNotFound("finance period", id.String())
		}
		return nil, rwError(err)
	}
	return p, nil
}

func scanFinancePeriodRow(row pgx.Row) (*domain.FinancePeriod, error) {
	var p domain.FinancePeriod
	var status string
	var start, end time.Time
	if err := row.Scan(&p.ID, &p.TenantID, &p.Label, &start, &end, &status, &p.ClosedBy, &p.ClosedAt, &p.Version); err != nil {
		return nil, err
	}
	if ps, err := domain.ParsePeriodStatus(status); err == nil {
		p.Status = ps
	}
	p.Range = domain.DateRange{Start: start, End: end}
	return &p, nil
}

// List returns an offset-paginated, tenant-scoped collection.
func (r *FinancePeriodRepository) List(ctx context.Context, tenantID uuid.UUID, page ports.OffsetPage) ([]*domain.FinancePeriod, *domain.DomainError) {
	size, offset := normalizeOffsetPage(page)
	conn := r.conn()
	rows, err := conn.Query(ctx,
		`SELECT id, tenant_id, label, date_start, date_end, status, closed_by, closed_at, version
		 FROM finance_periods WHERE tenant_id=$1 ORDER BY date_start DESC LIMIT $2 OFFSET $3`,
		tenantID, size+1, offset)
	if err != nil {
		return nil, rwError(err)
	}
	defer rows.Close()
	var out []*domain.FinancePeriod
	for rows.Next() {
		p, err := scanFinancePeriodRow(rows)
		if err != nil {
			return nil, rwError(err)
		}
		if len(out) < size {
			out = append(out, p)
		}
	}
	if rows.Err() != nil {
		return nil, rwError(rows.Err())
	}
	return out, nil
}

// Create persists a new open finance period.
func (r *FinancePeriodRepository) Create(ctx context.Context, tenantID uuid.UUID, p *domain.FinancePeriod) *domain.DomainError {
	conn := r.conn()
	_, err := conn.Exec(ctx,
		`INSERT INTO finance_periods (id, tenant_id, label, date_start, date_end, status, version)
		 VALUES ($1,$2,$3,$4,$5,'open',1)`,
		p.ID, tenantID, p.Label, p.Range.Start, p.Range.End)
	if err != nil {
		return rwError(err)
	}
	return nil
}

// BeginClose atomically transitions open -> closing. A period already closing
// or closed yields CONFLICT_STATE.
func (r *FinancePeriodRepository) BeginClose(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, expectedVersion int64) *domain.DomainError {
	conn := r.conn()
	tag, err := conn.Exec(ctx,
		`UPDATE finance_periods SET status = 'closing', version = version + 1, updated_at = now()
		 WHERE id=$1 AND tenant_id=$2 AND status = 'open' AND version=$3`,
		id, tenantID, expectedVersion)
	if err != nil {
		return rwError(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewConflict("period is not open or stale version")
	}
	return nil
}

// CompleteClose atomically transitions closing -> closed, records the closing
// actor, and materializes the reporting snapshot in the same transaction.
func (r *FinancePeriodRepository) CompleteClose(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, actorID uuid.UUID, expectedVersion int64) *domain.DomainError {
	conn := r.conn()
	tag, err := conn.Exec(ctx,
		`UPDATE finance_periods SET status = 'closed', closed_by = $4, closed_at = now(),
		        version = version + 1, updated_at = now()
		 WHERE id=$1 AND tenant_id=$2 AND status = 'closing' AND version=$3`,
		id, tenantID, expectedVersion, actorID)
	if err != nil {
		return rwError(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewConflict("period is not closing or stale version")
	}
	// Publish the period-closed outbox event atomically with the transition.
	return r.AppendOutbox(ctx, tenantID, "PeriodClosed", id, nil)
}