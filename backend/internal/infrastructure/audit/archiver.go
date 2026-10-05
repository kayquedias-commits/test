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
//
// archiver.go — the archival adapter attached to this package.
package audit

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"fms/backend/internal/domain"
	"fms/backend/internal/infrastructure/observability"
)

// ObjectStore is the narrow driven port the Archiver interacts with for
// S3-compatible/MinIO object storage. The concrete S3 adapter is wired in
// cmd/worker/main.go; keeping the archiver coupled to this interface (not the
// SDK) is what makes it unit-testable with an in-memory stub.
type ObjectStore interface {
	// PutObject writes a single object of size bytes. Implementations must be
	// idempotent by key so a retried batch overwrites the same key with
	// identical content.
	PutObject(ctx context.Context, key string, r io.Reader, size int64) error
	// GetObject returns a reader for the named object; callers own the reader
	// and must close it.
	GetObject(ctx context.Context, key string) (io.ReadCloser, error)
	// DeleteObject removes the named object. A delete of a non-existent key is
	// a no-op success (object stores are idempotent on delete).
	DeleteObject(ctx context.Context, key string) error
}

// Archival bounds. Batch size is the memory ceiling per archive call and the
// gzip-bomb caps place hard limits on what a restore may decompress so a
// malicious or corrupted object cannot exhaust worker memory (CWE-409/CWE-770).
const (
	// defaultBatchSize segments a retention window into archive objects of this
	// many entries, matching ApplyRetentionPolicy (1,000 entries/batch).
	defaultBatchSize = 1000
	// maxBatchSize is the hard upper bound accepted by WithBatchSize.
	maxBatchSize = 1000
	// formatVersion is the archive object format version carried in the trailer
	// so old objects stay deserializable after the domain model gains fields.
	formatVersion = 1
	// objectContentType is set on every archive object for lifecycle tooling.
	objectContentType = "application/x-jsonlines"
	// archiveRetries bounds PutObject/DeleteObject attempts under exponential
	// backoff before the whole batch is failed (consistency preserved).
	archiveRetries = 3
	// archivePrefix is the object-key root namespace.
	archivePrefix = "audit-archive/"
	// maxDecompressedBytes caps a single restore object's decompressed size
	// (a gzip bomb guard).
	maxDecompressedBytes = 64 << 20
	// maxCompressedBytes caps the compressed body read from object storage.
	maxCompressedBytes = 32 << 20
)

// Sentinel errors surfaced across the archiver seam. They are wrapped in a
// domain.DomainError before escaping so callers see a typed code + message.
var (
	// ErrEmptyBatch means no entries were supplied where at least one required.
	ErrEmptyBatch = errors.New("audit archive: empty batch")
	// ErrTenantMismatch means a batch spans more than one tenant (CWE-284).
	ErrTenantMismatch = errors.New("audit archive: batch spans multiple tenants")
	// ErrChainBroken means the chained-hash linkage no longer verifies.
	ErrChainBroken = errors.New("audit archive: chained hash verification failed")
	// ErrIntegrity means an archive object failed trailer/checksum validation.
	ErrIntegrity = errors.New("audit archive: object integrity check failed")
	// ErrOversizedObject means a restore object exceeded the decompression cap.
	ErrOversizedObject = errors.New("audit archive: object exceeds size bound")
)

// Archiver moves aging chain-hashed audit entries to S3-compatible object
// storage, verifying chain integrity before export and after restore. It is the
// egress side of the Audit & Compliance bounded context's retention lifecycle
// (Blueprint §5.3/§6.3/§9.3): entries are archived — never deleted — and the
// method that removes objects (GarbageCollect) is reachable only through the
// policy use case after the DB rows are marked archived. Retention policy
// itself (the cutoff) stays in the use-case layer; this adapter never consults
// config.
type Archiver struct {
	store       *Store
	objectStore ObjectStore
	log         *observability.Logger
	metrics     *observability.Metrics
	batchSize   int
}

// NewArchiver returns an Archiver bound to the source store and the object
// store port. A nil log/metrics handle degrades gracefully to no-op recording
// so the archiver remains usable in tests.
func NewArchiver(store *Store, objectStore ObjectStore, log *observability.Logger, metrics *observability.Metrics) *Archiver {
	return &Archiver{
		store:       store,
		objectStore: objectStore,
		log:         log,
		metrics:     metrics,
		batchSize:   defaultBatchSize,
	}
}

// WithBatchSize overrides the archival batch size, clamped to [1, maxBatchSize]
// so a caller-supplied value can never exceed the memory ceiling. It mutates
// and returns the receiver.
func (a *Archiver) WithBatchSize(n int) *Archiver {
	if n <= 0 {
		a.batchSize = 1
		return a
	}
	if n > maxBatchSize {
		a.batchSize = maxBatchSize
		return a
	}
	a.batchSize = n
	return a
}

// logf routes a structured message through the injected logger when present. It
// never logs before/after payloads or any secret material.
func (a *Archiver) logf(level, msg string, kv ...any) {
	if a.log == nil {
		return
	}
	switch level {
	case "warn":
		a.log.Warn(msg, kv...)
	case "error":
		a.log.Error(msg, kv...)
	default:
		a.log.Info(msg, kv...)
	}
}

// recordResult accounts for an archival/restore/GC outcome on the existing
// observability seam and logs the outcome. Dedicated audit_archive_* collectors
// are not registered in metrics.go, so this reuses the write-throughput counter
// and error counter that are available.
func (a *Archiver) recordResult(ok bool, tenant uuid.UUID, op string) {
	if a.metrics != nil {
		if ok {
			a.metrics.IncAuditWrite("success")
		} else {
			a.metrics.IncAuditWrite("failure")
		}
	}
	level := "info"
	if !ok {
		level = "error"
	}
	a.logf(level, "audit_archive "+op, "result", map[bool]string{true: "success", false: "failure"}[ok], "tenant", tenant)
}

// VerifyChain recomputes every entry's chained hash and asserts prev_hash
// linkage within the batch, using the authoritative in-package hash routine
// (chainHash/marshalCanonical from store.go) so re-computation always matches
// the original write. boundaryPrevHash is the expected prev_hash of the first
// entry: the store's last hash for pre-archive, or the first entry's own
// PrevHash when restoring into a fresh store (genesis accepted as validated at
// archive time). It runs identically pre-archive and post-restore so the
// tamper-evidence gate is a single, uncloned path.
//
// Empty entries are rejected with ErrEmptyBatch. Any hash or linkage mismatch
// records an ERROR "audit_chain_breach" and returns ErrChainBroken.
func (a *Archiver) VerifyChain(entries []Entry, boundaryPrevHash string) error {
	if len(entries) == 0 {
		return domain.Wrap(domain.CodeInternalError, ErrEmptyBatch)
	}
	tenant := entries[0].TenantID

	prevHash := boundaryPrevHash
	for i, e := range entries {
		gotPrev := prevHashVal(e.PrevHash)
		match := false
		switch {
		case i == 0:
			match = gotPrev == boundaryPrevHash
		default:
			match = gotPrev == prevHash
		}
		if !match {
			a.recordBreach(tenant, i, gotPrev, prevHash)
			return domain.Wrap(domain.CodeInternalError, ErrChainBroken)
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
			return domain.Wrap(domain.CodeInternalError, merr)
		}
		if computed := chainHash(prevHash, payload); computed != e.Hash {
			a.recordBreach(tenant, i, e.Hash, computed)
			return domain.Wrap(domain.CodeInternalError, ErrChainBroken)
		}
		prevHash = e.Hash
	}
	return nil
}

// recordBreach logs a chain-breach at ERROR level and increments the failure
// counter. Like Store.recordBreach it never logs before/after payloads.
func (a *Archiver) recordBreach(tenant uuid.UUID, index int, got, want string) {
	if a.metrics != nil {
		a.metrics.IncAuditWrite("failure")
	}
	a.logf("error", "audit_chain_breach: chained hash mismatch detected",
		"tenant", tenant, "entry_index", index, "detected_at", time.Now().UTC(),
		"got_hash_prefix", truncate(got), "want_hash_prefix", truncate(want))
}

// truncate shortens a hex hash for log output without leaking its full value.
func truncate(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

// objectKey derives the storage key for a batch:
// audit-archive/{tenant}/{YYYY}/{MM}/{batchStartSeq}-{batchEndSeq}.jsonl.gz.
// Only the server-derived date parts and validated tenant UUID and integer seqs
// enter the key — never raw user strings — so a tenant/ID cannot address an
// unintended prefix or another tenant's namespace (CWE-20/CWE-22/CWE-23).
func (a *Archiver) objectKey(entries []Entry) (string, error) {
	if len(entries) == 0 {
		return "", ErrEmptyBatch
	}
	first, last := entries[0], entries[len(entries)-1]
	if first.Seq < 0 || last.Seq < 0 || last.Seq < first.Seq {
		return "", domain.NewValidationError(domain.FieldError{Field: "seq", Message: "batch must be ordered by non-negative seq"})
	}
	tenant := first.TenantID
	if tenant == uuid.Nil || !uuidPattern.MatchString(tenant.String()) {
		return "", domain.NewValidationError(domain.FieldError{Field: "tenant_id", Message: "tenant must be a valid UUID"})
	}
	ts := first.OccurredAt.UTC()
	key := fmt.Sprintf("%s%s/%04d/%02d/%d-%d.jsonl.gz",
		archivePrefix, tenant.String(), ts.Year(), int(ts.Month()), first.Seq, last.Seq)
	return key, nil
}

// uuidPattern bounds the tenant segment that is interpolated into an object
// key, restricting it to the canonical 36-char UUID form so traversal separators
// cannot reach the key path.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// archiveLine is the JSONL serialization of one Entry on an archive object. It
// deliberately omits ArchivedAt (a lifecycle flag only) and carries the exact
// domain fields the chain hashes, in field-name form, so restore re-verifies
// against the authoritative hash routine. before/after stay as raw JSON bytes so
// the canonical bytes round-trip byte-for-byte.
type archiveLine struct {
	Seq           int64              `json:"seq"`
	TenantID      string             `json:"tenant_id"`
	Action        domain.AuditAction `json:"action"`
	ActorID       string             `json:"actor_id"`
	EntityType    string             `json:"entity_type"`
	EntityID      string             `json:"entity_id"`
	OccurredAt    time.Time          `json:"occurred_at"`
	Before        json.RawMessage    `json:"before"`
	After         json.RawMessage    `json:"after"`
	CorrelationID string             `json:"correlation_id"`
	Hash          string             `json:"hash"`
	PrevHash      *string            `json:"prev_hash,omitempty"`
}

// archiveTrailer is the final line of an archive object. It carries the entry
// count and a SHA-256 over all entry lines (decompressed, excluding itself) so
// early truncation or line corruption is detectable on restore, plus a format
// version for forward-compatible evolution.
type archiveTrailer struct {
	Count         int    `json:"count"`
	SHA256        string `json:"sha256"`
	FormatVersion int    `json:"format_version"`
}

// render serializes entries into a gzip-compressed JSONL object with the
// checksum trailer appended. The trailing newline ensures a truncated final
// entry still manifests as a malformed line when the trailer's count is read.
func (a *Archiver) render(entries []Entry) ([]byte, error) {
	var body bytes.Buffer
	for _, e := range entries {
		line, lerr := json.Marshal(archiveLine{
			Seq:           e.Seq,
			TenantID:      e.TenantID.String(),
			Action:        e.Action,
			ActorID:       e.ActorID.String(),
			EntityType:    e.EntityType,
			EntityID:      e.EntityID,
			OccurredAt:    e.OccurredAt.UTC(),
			Before:        json.RawMessage(e.Before),
			After:         json.RawMessage(e.After),
			CorrelationID: e.CorrelationID,
			Hash:          e.Hash,
			PrevHash:      e.PrevHash,
		})
		if lerr != nil {
			return nil, domain.Wrap(domain.CodeInternalError, lerr)
		}
		body.Write(line)
		body.WriteByte('\n')
	}

	sum := sha256.Sum256(body.Bytes())
	trailer, terr := json.Marshal(archiveTrailer{
		Count:         len(entries),
		SHA256:        hex.EncodeToString(sum[:]),
		FormatVersion: formatVersion,
	})
	if terr != nil {
		return nil, domain.Wrap(domain.CodeInternalError, terr)
	}
	body.Write(trailer)
	body.WriteByte('\n')

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, werr := zw.Write(body.Bytes()); werr != nil {
		return nil, domain.Wrap(domain.CodeInternalError, werr)
	}
	if cerr := zw.Close(); cerr != nil {
		return nil, domain.Wrap(domain.CodeInternalError, cerr)
	}
	return buf.Bytes(), nil
}

// parseObject decompresses and validates one archive object. It enforces a hard
// decompressed-size bound (gzip bomb guard), checks the trailer (format,
// count, SHA-256), and decodes the entries back into Entry values. Each action
// is re-validated against the canonical set so an archive object cannot smuggle
// an unknown action into a restored store (CWE-20). before/after round-trip as
// canonical JSON for downstream chain verification.
func (a *Archiver) parseObject(rc io.ReadCloser) ([]Entry, uuid.UUID, error) {
	defer func() { _ = rc.Close() }()

	gz, gerr := gzip.NewReader(rc)
	if gerr != nil {
		return nil, uuid.Nil, domain.Wrap(domain.CodeInternalError, gerr)
	}
	defer func() { _ = gz.Close() }()

	// Bounded read: anything exceeding the cap is rejected as oversized rather
	// than drained (CWE-409/CWE-770).
	body, rerr := io.ReadAll(io.LimitReader(gz, maxDecompressedBytes+1))
	if rerr != nil {
		return nil, uuid.Nil, domain.Wrap(domain.CodeInternalError, rerr)
	}
	if int64(len(body)) > maxDecompressedBytes {
		return nil, uuid.Nil, domain.Wrap(domain.CodeInternalError, ErrOversizedObject)
	}

	lines := strings.Split(string(body), "\n")
	// Drop the trailing empty fragment left by the final newline.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if len(lines) < 2 {
		return nil, uuid.Nil, domain.Wrap(domain.CodeInternalError, ErrIntegrity)
	}

	var trailer archiveTrailer
	if terr := json.Unmarshal([]byte(lines[len(lines)-1]), &trailer); terr != nil {
		return nil, uuid.Nil, domain.Wrap(domain.CodeInternalError, ErrIntegrity)
	}
	if trailer.FormatVersion != formatVersion {
		return nil, uuid.Nil, domain.Wrap(domain.CodeInternalError, ErrIntegrity)
	}

	entryLines := lines[:len(lines)-1]
	if trailer.Count != len(entryLines) {
		return nil, uuid.Nil, domain.Wrap(domain.CodeInternalError, ErrIntegrity)
	}

	// The trailer checksum covers every entry line (excluding only itself).
	rendered := strings.Join(entryLines, "\n") + "\n"
	sum := sha256.Sum256([]byte(rendered))
	if hex.EncodeToString(sum[:]) != trailer.SHA256 {
		return nil, uuid.Nil, domain.Wrap(domain.CodeInternalError, ErrIntegrity)
	}

	out := make([]Entry, 0, trailer.Count)
	var tenant uuid.UUID
	for _, l := range entryLines {
		var al archiveLine
		if derr := json.Unmarshal([]byte(l), &al); derr != nil {
			return nil, uuid.Nil, domain.Wrap(domain.CodeInternalError, ErrIntegrity)
		}
		tenantID, terr := uuid.Parse(al.TenantID)
		actorID, aerr := uuid.Parse(al.ActorID)
		if terr != nil || aerr != nil {
			return nil, uuid.Nil, domain.Wrap(domain.CodeInternalError, ErrIntegrity)
		}
		if al.Hash == "" || !al.Action.Valid() {
			return nil, uuid.Nil, domain.Wrap(domain.CodeInternalError, ErrIntegrity)
		}
		if tenant == uuid.Nil {
			tenant = tenantID
		} else if tenant != tenantID {
			return nil, uuid.Nil, domain.Wrap(domain.CodeInternalError, ErrTenantMismatch)
		}
		out = append(out, Entry{
			TenantID:      tenantID,
			Seq:           al.Seq,
			Action:        al.Action,
			ActorID:       actorID,
			EntityType:    al.EntityType,
			EntityID:      al.EntityID,
			OccurredAt:    al.OccurredAt.UTC(),
			Before:        []byte(al.Before),
			After:         []byte(al.After),
			CorrelationID: al.CorrelationID,
			Hash:          al.Hash,
			PrevHash:      al.PrevHash,
		})
	}
	return out, tenant, nil
}

// ArchiveBatch verifies the batch's chain integrity, serializes it to a single
// gzip-compressed JSONL object, and uploads it — returning the object key. It
// enforces the write-objects-first ordering: any hash mismatch or upload
// failure aborts the whole batch with no object written, so the DB (whose
// archival marking happens only after this succeeds, in the caller's
// transaction) and the object store never diverge at the batch level.
//
// The batch must be single-tenant and ordered by seq. objectContentType,
// retention, and origin tags are applied by the S3 adapter at upload; this
// adapter leaves that policy to the wiring layer.
func (a *Archiver) ArchiveBatch(ctx context.Context, entries []Entry) ([]string, error) {
	if len(entries) == 0 {
		return nil, domain.Wrap(domain.CodeInternalError, ErrEmptyBatch)
	}
	if len(entries) > a.batchSize {
		return nil, domain.NewValidationError(domain.FieldError{Field: "entries", Message: "batch exceeds configured size"})
	}
	tenant := entries[0].TenantID
	for _, e := range entries {
		if e.TenantID != tenant {
			a.recordResult(false, tenant, "archive")
			return nil, domain.Wrap(domain.CodeInternalError, ErrTenantMismatch)
		}
	}

	// Boundary: the batch must continue the chain from the store's last hash.
	lastHash, derr := a.store.LastHash(ctx, tenant)
	if derr != nil {
		a.recordResult(false, tenant, "archive")
		return nil, derr
	}
	if verr := a.VerifyChain(entries, lastHash); verr != nil {
		a.recordResult(false, tenant, "archive")
		return nil, verr
	}

	key, kerr := a.objectKey(entries)
	if kerr != nil {
		a.recordResult(false, tenant, "archive")
		return nil, kerr
	}
	data, rerr := a.render(entries)
	if rerr != nil {
		a.recordResult(false, tenant, "archive")
		return nil, rerr
	}
	if uerr := a.putRetry(ctx, key, data); uerr != nil {
		a.recordResult(false, tenant, "archive")
		return nil, uerr
	}

	a.recordResult(true, tenant, "archive")
	a.logf("info", "audit archive batch uploaded", "key", key, "tenant", tenant, "count", len(entries))
	return []string{key}, nil
}

// putRetry uploads a single archival object with up to archiveRetries attempts
// under exponential backoff plus jitter (Blueprint §7.7 base 100 ms, factor 2).
// Once attempts are exhausted the error is returned and the caller aborts the
// batch, preserving consistency between the DB and the object store.
func (a *Archiver) putRetry(ctx context.Context, key string, data []byte) error {
	var lastErr error
	for attempt := 0; attempt < archiveRetries; attempt++ {
		if attempt > 0 {
			delay := backoff(attempt-1) + time.Duration(rand.IntN(50))*time.Millisecond
			a.logf("warn", "audit archive object-store retry", "op", "put", "key", key, "attempt", attempt, "delay_ms", delay.Milliseconds())
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return domain.Wrap(domain.CodeDependencyUnavailable, ctx.Err())
			}
		}
		err := a.objectStore.PutObject(ctx, key, bytes.NewReader(data), int64(len(data)))
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return domain.Wrap(domain.CodeDependencyUnavailable, lastErr)
}

// Restore pulls the named archive objects, validates each (trailer, count,
// SHA-256, bounded decompression) and re-verifies chained-hash linkage across
// the whole restored set. On a fresh store the first entry's own PrevHash is
// accepted as the genesis boundary because it was validated against the source
// store's last hash at archive time. On any failure the result is an error —
// never a partial batch.
func (a *Archiver) Restore(ctx context.Context, keys []string) ([]Entry, error) {
	if len(keys) == 0 {
		return nil, domain.NewValidationError(domain.FieldError{Field: "keys", Message: "at least one object key required"})
	}

	var (
		all     []Entry
		tenant  uuid.UUID
		ordered = true
	)
	for _, key := range keys {
		rc, gerr := a.objectStore.GetObject(ctx, key)
		if gerr != nil {
			a.recordResult(false, tenant, "restore")
			return nil, domain.Wrap(domain.CodeDependencyUnavailable, gerr)
		}

		// Bound the compressed body before handing it to the gzip reader.
		limited := io.LimitReader(rc, maxCompressedBytes+1)
		compressed, rerr := io.ReadAll(limited)
		_ = rc.Close()
		if rerr != nil {
			a.recordResult(false, tenant, "restore")
			return nil, domain.Wrap(domain.CodeDependencyUnavailable, rerr)
		}
		if int64(len(compressed)) > maxCompressedBytes {
			a.recordResult(false, tenant, "restore")
			return nil, domain.Wrap(domain.CodeInternalError, ErrOversizedObject)
		}

		batch, batchTenant, perr := a.parseObject(io.NopCloser(bytes.NewReader(compressed)))
		if perr != nil {
			a.recordResult(false, tenant, "restore")
			return nil, perr
		}
		if tenant == uuid.Nil {
			tenant = batchTenant
		} else if tenant != batchTenant {
			a.recordResult(false, tenant, "restore")
			return nil, domain.Wrap(domain.CodeInternalError, ErrTenantMismatch)
		}
		if n := len(all); n > 0 && batch[0].Seq <= all[n-1].Seq {
			ordered = false
		}
		all = append(all, batch...)
	}

	genesisBoundary := prevHashVal(all[0].PrevHash)
	if !ordered {
		a.recordResult(false, tenant, "restore")
		return nil, domain.Wrap(domain.CodeInternalError, ErrIntegrity)
	}
	if verr := a.VerifyChain(all, genesisBoundary); verr != nil {
		a.recordResult(false, tenant, "restore")
		return nil, verr
	}

	a.recordResult(true, tenant, "restore")
	a.logf("info", "audit archive restored", "keys", len(keys), "entries", len(all), "tenant", tenant)
	return all, nil
}

// GarbageCollect removes archive objects. It is the only path that deletes
// objects and it deletes only object-store keys — there is no DB-row deletion,
// preserving the append-only invariant (Module G). Callers (ApplyRetentionPolicy)
// must have already marked the DB rows archived and verified the object listing
// before invoking this; the method additionally enforces tenant scoping on every
// key so no object outside the calling tenant's audit-archive prefix can be
// removed (CWE-22/CWE-863). Deleting a missing object is a no-op success.
func (a *Archiver) GarbageCollect(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return domain.NewValidationError(domain.FieldError{Field: "keys", Message: "at least one object key required"})
	}

	var tenant uuid.UUID
	for _, key := range keys {
		seg, ok := strings.CutPrefix(key, archivePrefix)
		if !ok {
			a.recordResult(false, tenant, "garbage_collect")
			return domain.Wrap(domain.CodeInternalError, ErrTenantMismatch)
		}
		tenantPart, _, _ := strings.Cut(seg, "/")
		if !uuidPattern.MatchString(tenantPart) {
			a.recordResult(false, tenant, "garbage_collect")
			return domain.Wrap(domain.CodeInternalError, ErrTenantMismatch)
		}
		if tenant == uuid.Nil {
			candidate, perr := uuid.Parse(tenantPart)
			if perr != nil {
				a.recordResult(false, tenant, "garbage_collect")
				return domain.Wrap(domain.CodeInternalError, ErrTenantMismatch)
			}
			tenant = candidate
		}
	}

	var lastErr error
	for _, key := range keys {
		if err := a.deleteRetry(ctx, key); err != nil {
			lastErr = err
			break
		}
	}
	if lastErr != nil {
		a.recordResult(false, tenant, "garbage_collect")
		return lastErr
	}
	a.recordResult(true, tenant, "garbage_collect")
	a.logf("info", "audit archive objects collected", "keys", len(keys), "tenant", tenant)
	return nil
}

// deleteRetry removes one object with the same bounded exponential-backoff
// policy as putRetry.
func (a *Archiver) deleteRetry(ctx context.Context, key string) error {
	var lastErr error
	for attempt := 0; attempt < archiveRetries; attempt++ {
		if attempt > 0 {
			delay := backoff(attempt-1) + time.Duration(rand.IntN(50))*time.Millisecond
			a.logf("warn", "audit archive object-store retry", "op", "delete", "key", key, "attempt", attempt, "delay_ms", delay.Milliseconds())
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return domain.Wrap(domain.CodeDependencyUnavailable, ctx.Err())
			}
		}
		err := a.objectStore.DeleteObject(ctx, key)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return domain.Wrap(domain.CodeDependencyUnavailable, lastErr)
}