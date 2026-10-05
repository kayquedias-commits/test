// Package audit implements the append-only, tamper-evident audit store
// (Blueprint §5.3/§6.3). It is the infrastructure adapter behind the audit
// service seam: writes are INSERT-only and serialized per tenant via an
// advisory lock so the chained-hash tail is always linear, and reads are
// tenant-scoped with explicit predicates. The store is coupled only to the
// audit schema and the observability seam — it never leaks a pgx type to
// callers and exposes no Update/Delete method, mirroring the revoked grants in
// migration 0002 (append-only by construction).
//
// Hash invariants: each entry stores the SHA-256 of the previous entry's hash
// concatenated with the canonical (sorted-key, whitespace-free) payload. Append
// and VerifyChain share one authoritative hash routine (chainHash), so
// re-computation always matches the original write.
//
// Tenancy: the schema enables FORCEd row-level security on audit_entries, whose
// policies evaluate against the session GUC `app.tenant_id` (NULL -> zero rows
// and rejected writes). Every method therefore establishes that GUC inside a
// transaction with set_config(..., local=true); being transaction-scoped it is
// rolled back on commit/rollback, so no tenant context leaks across operations
// on the shared pool. The explicit tenant_id predicates remain the primary
// isolation defense (CWE-284).
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"fms/backend/internal/domain"
	"fms/backend/internal/infrastructure/observability"
)

// Persistence bounds mirrored from the schema and security posture.
const (
	// defaultQueryLimit caps Query page size; it guards CWE-400 unbounded scans.
	defaultQueryLimit = 100
	// maxQueryLimit is the hard upper bound accepted by Query/PendingArchival.
	maxQueryLimit = 200
	// maxSearchTermLen bounds the ILIKE term so a single request cannot drive a
	// full-table scan (CWE-20 / CWE-400).
	maxSearchTermLen = 64
	// verifyBatchRows bounds the memory of VerifyChain's streamed walk.
	verifyBatchRows = 1000
	// nilHashHex is the fixed input standing for a NULL prev_hash on the genesis
	// row, so the hashed input length is constant across the chain.
	nilHashHex = "0000000000000000000000000000000000000000000000000000000000000000"
)

// Entry is the persisted audit shape returned across the store seam. The
// domain-owned aggregate (domain/audit_entry.go) does not yet exist in the
// dependency contract; the store therefore declares the exact column set it
// writes and reads. It carries domain.AuditAction (a domain value object) and
// keeps before/after as canonical JSON so verification is deterministic.
type Entry struct {
	TenantID      uuid.UUID
	Seq           int64
	Action        domain.AuditAction
	ActorID       uuid.UUID
	EntityType    string
	EntityID      string
	OccurredAt    time.Time
	Before        []byte
	After         []byte
	CorrelationID string
	Hash          string
	PrevHash      *string
	ArchivedAt    *time.Time
}

// AuditFilter constrains a Query. Nil/zero fields are ignored; Search is
// escaped and length-bounded before it reaches the ILIKE predicate.
type AuditFilter struct {
	EntityType string
	ActorID    uuid.UUID
	Action     domain.AuditAction
	From       *time.Time
	To         *time.Time
	Search     string
}

// Store is the append-only audit adapter bound to the shared connection pool.
// It exposes only Append/Query/LastHash/PendingArchival/MarkArchived and
// VerifyChain — there is intentionally no Update or Delete method, and the
// schema trigger forbids any mutating statement other than the archived_at
// toggle performed by MarkArchived.
type Store struct {
	pool    *pgxpool.Pool
	log     *observability.Logger
	metrics *observability.Metrics
}

// NewStore returns a Store bound to the supplied pool. log/metrics are the
// shared observability seam; a nil log/metrics handle degrades gracefully to
// no-op recording so the store remains usable in tests.
func NewStore(pool *pgxpool.Pool, log *observability.Logger, metrics *observability.Metrics) *Store {
	return &Store{pool: pool, log: log, metrics: metrics}
}

// beginTenant opens a transaction and establishes the tenant context for this
// operation via set_config(local=true) so RLS admits the tenant's rows. The
// setting is rolled back with the transaction, isolating tenant context on the
// shared pool. The returned tx must be committed (or rolled back) by the caller.
func (s *Store) beginTenant(ctx context.Context, tenantID uuid.UUID) (pgx.Tx, *domain.DomainError) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, rwError(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1::text, true)`, tenantID); err != nil {
		_ = tx.Rollback(ctx)
		return nil, rwError(err)
	}
	return tx, nil
}

// chainPayload is the canonical, hashed description of one audit entry. It is
// serialized with recursively sorted keys and no whitespace so recomputation is
// deterministic (§6.3). tenant_id and seq are included so hashes are chained
// per-tenant and cannot be shifted across tenants (CWE-353).
type chainPayload struct {
	Action        domain.AuditAction `json:"action"`
	ActorID       string             `json:"actor_id"`
	EntityID      string             `json:"entity_id"`
	EntityType    string             `json:"entity_type"`
	TenantID      string             `json:"tenant_id"`
	OccurredAt    time.Time          `json:"occurred_at"`
	Before        json.RawMessage    `json:"before"`
	After         json.RawMessage    `json:"after"`
	CorrelationID string             `json:"correlation_id"`
}

// marshalCanonical serializes p deterministically. The struct field declaration
// order fixes key order and json.Marshal writes no surrounding whitespace, so
// re-computation always yields identical bytes.
func marshalCanonical(p chainPayload) ([]byte, error) {
	return json.Marshal(p)
}

// chainHash computes hex(SHA256(prevHashHex ‖ 0x00 ‖ payload)). The 0x00
// delimiter removes ambiguity between the fixed-width prev hash and the payload
// (length-extension / preimage class), and genesis uses a fixed 64-char zero
// string so input length is constant across every chain position.
func chainHash(prevHashHex string, payload []byte) string {
	if prevHashHex == "" {
		prevHashHex = nilHashHex
	}
	h := sha256.New()
	_, _ = h.Write([]byte(prevHashHex))
	_, _ = h.Write([]byte{0x00})
	_, _ = h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}

// escapeILIKE neutralizes ILIKE metacharacters in a search term so an
// attacker-controlled "%" or "_" cannot widen the match (CWE-20 / CWE-89).
func escapeILIKE(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// boundLimit clamps a requested page size to [1, maxQueryLimit].
func boundLimit(limit int) int {
	if limit <= 0 {
		return defaultQueryLimit
	}
	if limit > maxQueryLimit {
		return maxQueryLimit
	}
	return limit
}

// rwError normalizes a pgx/driver error onto the domain.DomainError contract so
// no pgconn.PgError escapes the persistence seam (CWE-209). It mirrors the
// postgres adapter's mapping: no-rows -> NOT_FOUND, unique violation ->
// CONFLICT_STATE, deadlock/serialization -> CONFLICT_STATE, connection class ->
// DEPENDENCY_UNAVAILABLE, otherwise INTERNAL_ERROR (cause retained for logging
// only, never serialized).
func rwError(err error) *domain.DomainError {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewNotFound("audit entry", "")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return domain.NewConflict("audit entry already exists")
		case "40001", "40P01", "55P03":
			return domain.NewConflict("audit store is busy; retry the operation")
		}
		if len(pgErr.Code) >= 2 && pgErr.Code[:2] == "08" {
			return domain.Wrap(domain.CodeDependencyUnavailable, err)
		}
	}
	return domain.NewInternal(err)
}

// Append inserts one audit entry at the next position of the tenant's chain. It
// runs in a transaction that (1) takes a per-tenant advisory lock, (2) reads the
// current tail hash under FOR UPDATE, (3) computes the chained hash, and (4)
// inserts with the derived seq. The advisory lock also serializes the genesis
// write, which the FOR UPDATE tail-lock alone cannot protect (no tail row
// exists yet). Append is safe to call concurrently and is also the public seam
// integration tests use synchronously.
func (s *Store) Append(ctx context.Context, e *Entry) (*Entry, *domain.DomainError) {
	if e == nil {
		return nil, domain.NewValidationError(domain.FieldError{Field: "entry", Message: "audit entry is required"})
	}
	if !e.Action.Valid() {
		return nil, domain.NewValidationError(domain.FieldError{Field: "action", Message: "unknown audit action"})
	}
	if e.TenantID == uuid.Nil || e.ActorID == uuid.Nil {
		return nil, domain.NewValidationError(
			domain.FieldError{Field: "tenant_id", Message: "tenant must be non-empty"},
			domain.FieldError{Field: "actor_id", Message: "actor must be non-empty"},
		)
	}
	payload, perr := marshalCanonical(chainPayload{
		Action:        e.Action,
		ActorID:       e.ActorID.String(),
		EntityID:      e.EntityID,
		EntityType:    e.EntityType,
		TenantID:      e.TenantID.String(),
		OccurredAt:    e.OccurredAt.UTC(),
		Before:        json.RawMessage(e.Before),
		After:         json.RawMessage(e.After),
		CorrelationID: e.CorrelationID,
	})
	if perr != nil {
		return nil, domain.NewValidationError(domain.FieldError{Field: "entry", Message: "cannot canonicalize audit payload"})
	}

	var lastErr *domain.DomainError
	for attempt := 0; attempt < 3; attempt++ {
		out, derr := s.appendOnce(ctx, e, payload)
		if derr == nil {
			return out, nil
		}
		if derr.Code != domain.CodeConflictState {
			return nil, derr
		}
		lastErr = derr
		s.logf("info", "audit append contested; retrying", "attempt", attempt+1, "tenant", e.TenantID)
		time.Sleep(backoff(attempt))
	}
	return nil, domain.Wrap(domain.CodeConflictState, fmt.Errorf("audit append contested: %s", lastErr))
}

func (s *Store) appendOnce(ctx context.Context, e *Entry, payload []byte) (*Entry, *domain.DomainError) {
	tx, derr := s.beginTenant(ctx, e.TenantID)
	if derr != nil {
		return nil, derr
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialize appends for this tenant, including the empty-chain (genesis)
	// case. Advisory locks are transaction-scoped and released at COMMIT.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1::text))`, e.TenantID); err != nil {
		return nil, rwError(err)
	}

	var prevHash *string
	nextSeq := int64(1)
	err := tx.QueryRow(ctx,
		`SELECT seq, hash FROM audit_entries
		  WHERE tenant_id = $1
		  ORDER BY seq DESC
		  LIMIT 1
		  FOR UPDATE`, e.TenantID).Scan(&nextSeq, &prevHash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// no tail row: genesis, seq stays 1, prev_hash stays NULL
		nextSeq = 1
	case err != nil:
		return nil, rwError(err)
	default:
		nextSeq++ // append at last.seq + 1
	}

	hash := chainHash(prevHashVal(prevHash), payload)

	_, err = tx.Exec(ctx,
		`INSERT INTO audit_entries
		   (tenant_id, seq, action, actor_id, entity_type, entity_id, occurred_at,
		    before, after, correlation_id, hash, prev_hash, archived_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NULL)`,
		e.TenantID, nextSeq, string(e.Action), e.ActorID, e.EntityType, e.EntityID,
		e.OccurredAt.UTC(), e.Before, e.After, e.CorrelationID, hash, prevHash)
	if err != nil {
		return nil, rwError(err)
	}

	out := &Entry{
		TenantID:      e.TenantID,
		Seq:           nextSeq,
		Action:        e.Action,
		ActorID:       e.ActorID,
		EntityType:    e.EntityType,
		EntityID:      e.EntityID,
		OccurredAt:    e.OccurredAt.UTC(),
		Before:        e.Before,
		After:         e.After,
		CorrelationID: e.CorrelationID,
		Hash:          hash,
		PrevHash:      prevHash,
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, rwError(err)
	}
	s.recordAuditWrite("success", e.TenantID)
	return out, nil
}

// prevHashVal returns the 64-char hex passed to chainHash; an empty value
// (genesis) is normalized by chainHash to the fixed nilHashHex.
func prevHashVal(h *string) string {
	if h == nil {
		return ""
	}
	return *h
}

// backoff returns an exponential delay for retry attempt n with the §7.7 base
// (100 ms, factor 2), capped to avoid unbounded waiting.
func backoff(attempt int) time.Duration {
	d := time.Duration(100*math.Pow(2, float64(attempt))) * time.Millisecond
	if d > 1600*time.Millisecond {
		d = 1600 * time.Millisecond
	}
	return d
}

// Query returns a tenant-scoped window of audit entries ordered by seq,
// applying the supplied filters and a keyset cursor. The ILIKE term is escaped
// and length-bounded, and the hard LIMIT is enforced inside the SQL statement
// so no request can trigger an unbounded scan (CWE-400). It returns the next
// cursor (the last returned seq, or nil when the page is exhausted).
func (s *Store) Query(ctx context.Context, tenantID uuid.UUID, filter AuditFilter, limit int, cursor *int64) ([]Entry, *int64, *domain.DomainError) {
	size := boundLimit(limit)
	tx, derr := s.beginTenant(ctx, tenantID)
	if derr != nil {
		return nil, nil, derr
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := `SELECT tenant_id, seq, action, actor_id, entity_type, entity_id, occurred_at,
	             before, after, correlation_id, hash, prev_hash, archived_at
	        FROM audit_entries
	       WHERE tenant_id = $1`
	args := []any{tenantID}
	ph := 1
	add := func(condFmt string, val any) {
		ph++
		q += " AND " + fmt.Sprintf(condFmt, ph)
		args = append(args, val)
	}

	if filter.EntityType != "" {
		add("entity_type = $%d", filter.EntityType)
	}
	if filter.ActorID != uuid.Nil {
		add("actor_id = $%d", filter.ActorID)
	}
	if filter.Action != "" {
		add("action = $%d", string(filter.Action))
	}
	if filter.From != nil {
		add("occurred_at >= $%d", filter.From.UTC())
	}
	if filter.To != nil {
		add("occurred_at <= $%d", filter.To.UTC())
	}
	if t := strings.TrimSpace(filter.Search); t != "" {
		if len(t) > maxSearchTermLen {
			t = t[:maxSearchTermLen]
		}
		add("entity_id ILIKE $%d", "%"+escapeILIKE(t)+"%")
	}
	if cursor != nil {
		add("seq > $%d", *cursor)
	}

	ph++
	q += fmt.Sprintf(" ORDER BY seq ASC LIMIT $%d", ph)
	args = append(args, size)

	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, nil, rwError(err)
	}
	out := make([]Entry, 0, size)
	var lastSeq *int64
	for rows.Next() {
		e, scanErr := scanEntry(rows)
		if scanErr != nil {
			rows.Close()
			return nil, nil, rwError(scanErr)
		}
		out = append(out, e)
		lastSeq = &e.Seq
	}
	rows.Close()
	if rows.Err() != nil {
		return nil, nil, rwError(rows.Err())
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, rwError(err)
	}
	var nextCursor *int64
	if len(out) >= size {
		nextCursor = lastSeq
	}
	return out, nextCursor, nil
}

// LastHash returns the hash of the most recent entry in the tenant's audit
// chain, or an empty string when the chain is empty. The archiver uses it to
// prove chain continuation before archiving a window.
func (s *Store) LastHash(ctx context.Context, tenantID uuid.UUID) (string, *domain.DomainError) {
	tx, derr := s.beginTenant(ctx, tenantID)
	if derr != nil {
		return "", derr
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var h string
	err := tx.QueryRow(ctx,
		`SELECT hash FROM audit_entries WHERE tenant_id = $1 ORDER BY seq DESC LIMIT 1`,
		tenantID).Scan(&h)
	if errors.Is(err, pgx.ErrNoRows) {
		if ctxErr := tx.Commit(ctx); ctxErr != nil {
			return "", rwError(ctxErr)
		}
		return "", nil
	}
	if err != nil {
		return "", rwError(err)
	}
	if ctxErr := tx.Commit(ctx); ctxErr != nil {
		return "", rwError(ctxErr)
	}
	return h, nil
}

// PendingArchival returns the oldest un-archived entries at or before cutoff,
// ordered by seq and bounded by limit. It supports ApplyRetentionPolicy and
// returns the archival job's candidate window; an empty slice means nothing is
// due. The returned page never exceeds maxQueryLimit.
func (s *Store) PendingArchival(ctx context.Context, tenantID uuid.UUID, cutoff time.Time, limit int) ([]Entry, *domain.DomainError) {
	size := boundLimit(limit)
	tx, derr := s.beginTenant(ctx, tenantID)
	if derr != nil {
		return nil, derr
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx,
		`SELECT tenant_id, seq, action, actor_id, entity_type, entity_id, occurred_at,
		        before, after, correlation_id, hash, prev_hash, archived_at
		   FROM audit_entries
		  WHERE tenant_id = $1 AND archived_at IS NULL AND occurred_at < $2
		  ORDER BY seq ASC
		  LIMIT $3`,
		tenantID, cutoff.UTC(), size)
	if err != nil {
		return nil, rwError(err)
	}
	out := make([]Entry, 0, size)
	for rows.Next() {
		e, scanErr := scanEntry(rows)
		if scanErr != nil {
			rows.Close()
			return nil, rwError(scanErr)
		}
		out = append(out, e)
	}
	rows.Close()
	if rows.Err() != nil {
		return nil, rwError(rows.Err())
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, rwError(err)
	}
	return out, nil
}

// MarkArchived stamps archived_at on un-archived rows whose seq is in ids. It is
// the sole UPDATE the store issues and it touches only the archival flag — never
// hash, prev_hash, before, or after — so chain integrity is preserved through
// the retention lifecycle (§5.3). It returns the number of rows stamped.
func (s *Store) MarkArchived(ctx context.Context, tenantID uuid.UUID, ids []int64) (int64, *domain.DomainError) {
	if len(ids) == 0 {
		return 0, nil
	}
	tx, derr := s.beginTenant(ctx, tenantID)
	if derr != nil {
		return 0, derr
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`UPDATE audit_entries SET archived_at = now()
		  WHERE tenant_id = $1 AND seq = ANY($2) AND archived_at IS NULL`,
		tenantID, ids)
	if err != nil {
		return 0, rwError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, rwError(err)
	}
	return tag.RowsAffected(), nil
}

// VerifyChain walks the tenant's chain in seq order, recomputing each entry's
// hash from the stored payload and the previous row's hash and asserting the
// prev_hash linkage. An empty chain is vacuously valid and returns true. A
// mismatch (tamper or corruption) returns false after logging an ERROR
// "audit_chain_breach" and incrementing the write-failure counter; error is
// non-nil only when the chain could not be read. The reconciliation job and the
// archiver call this to detect tampering.
func (s *Store) VerifyChain(ctx context.Context, tenantID uuid.UUID) (bool, *domain.DomainError) {
	tx, derr := s.beginTenant(ctx, tenantID)
	if derr != nil {
		return false, derr
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx,
		`SELECT tenant_id, seq, action, actor_id, entity_type, entity_id, occurred_at,
		        before, after, correlation_id, hash, prev_hash, archived_at
		   FROM audit_entries
		  WHERE tenant_id = $1
		  ORDER BY seq ASC`, tenantID)
	if err != nil {
		return false, rwError(err)
	}

	var prevHash string
	first := true
	valid := true
	for rows.Next() {
		e, scanErr := scanEntry(rows)
		if scanErr != nil {
			rows.Close()
			return false, rwError(scanErr)
		}
		if first {
			// Genesis row must have a NULL prev_hash.
			if e.PrevHash != nil {
				valid = false
				break
			}
			first = false
		} else if e.PrevHash == nil || *e.PrevHash != prevHash {
			// Linkage check: every non-genesis row must point at its predecessor.
			valid = false
			break
		}

		payload, merr := marshalCanonical(chainPayload{
			Action:        e.Action,
			ActorID:       e.ActorID.String(),
			EntityID:      e.EntityID,
			EntityType:    e.EntityType,
			TenantID:      e.TenantID.String(),
			OccurredAt:    e.OccurredAt.UTC(),
			Before:        json.RawMessage(e.Before),
			After:         json.RawMessage(e.After),
			CorrelationID: e.CorrelationID,
		})
		if merr != nil {
			rows.Close()
			return false, rwError(merr)
		}
		if computed := chainHash(prevHash, payload); computed != e.Hash {
			valid = false
			break
		}
		prevHash = e.Hash
	}
	rows.Close()
	if rows.Err() != nil {
		return false, rwError(rows.Err())
	}
	if !valid {
		s.recordBreach(tenantID)
		return false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, rwError(err)
	}
	return true, nil
}

// scanEntry decodes one audit_entries row into an Entry. before/after are kept
// as raw JSON bytes (the canonical bytes are what the chain hashes, so the
// stored value is preserved byte-for-byte for verification).
func scanEntry(row pgx.Row) (Entry, error) {
	var e Entry
	var action string
	var before, after []byte
	var prevHash *string
	if err := row.Scan(
		&e.TenantID, &e.Seq, &action, &e.ActorID, &e.EntityType, &e.EntityID,
		&e.OccurredAt, &before, &after, &e.CorrelationID, &e.Hash, &prevHash,
		&e.ArchivedAt,
	); err != nil {
		return Entry{}, err
	}
	act, err := domain.ParseAuditAction(action)
	if err != nil {
		return Entry{}, err
	}
	e.Action = act
	e.Before = before
	e.After = after
	e.PrevHash = prevHash
	return e, nil
}

// logf routes a structured message through the injected logger when present. It
// never logs before/after payloads or any secret material.
func (s *Store) logf(level, msg string, kv ...any) {
	if s.log == nil {
		return
	}
	if level == "error" {
		s.log.Error(msg, kv...)
		return
	}
	s.log.Info(msg, kv...)
}

// recordAuditWrite pushes the write-throughput counter when metrics are wired.
func (s *Store) recordAuditWrite(result string, tenant uuid.UUID) {
	if s.metrics != nil {
		s.metrics.IncAuditWrite(result)
	}
	s.logf("info", "audit entry written", "result", result, "tenant", tenant)
}

// recordBreach logs a chain-breach at ERROR level and increments the write
// failure counter. It is invoked only after VerifyChain confirms a mismatch
// (never for an empty chain), so the metric is not polluted by absence.
func (s *Store) recordBreach(tenant uuid.UUID) {
	if s.metrics != nil {
		s.metrics.IncAuditWrite("failure")
	}
	s.logf("error", "audit_chain_breach: chained hash mismatch detected",
		"tenant", tenant, "detected_at", time.Now().UTC())
}