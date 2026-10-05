package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidAuditEntry is returned when an audit entry fails construction-time
// invariant validation.
var ErrInvalidAuditEntry = errors.New("domain: invalid audit entry")

// hashAlgorithmVersion tags the canonical hash framing so the chained hash can
// evolve to a new framing (e.g. keyed HMAC or a length-prefixed schema change)
// without breaking verification of previously written entries.
const hashAlgorithmVersion byte = 0x01

// AuditEntry is an immutable, append-only record of an audited operation in the
// Audit & Compliance bounded context. Each entry stores the hash of the
// previous entry in the chain (PrevHash) so that altering, deleting, or
// reordering any entry breaks the chain. The type exposes no mutator methods:
// once constructed its fields are fixed, making append-only structural rather
// than convention-based.
type AuditEntry struct {
	// ID is the primary identity of the entry.
	ID uuid.UUID
	// Seq is the monotonic, per-tenant chain ordering key. It is the canonical
	// traversal key for chain verification; Timestamp is intentionally NOT the
	// chain key because clock skew and same-instant inserts make it ambiguous.
	Seq int64
	// TenantID binds the entry to its tenancy boundary and participates in the
	// hash, preventing cross-tenant hash splicing.
	TenantID uuid.UUID
	// Action is the audited operation (create/edit/delete; read only when the
	// audit-read-logging flag is enabled at the gateway boundary).
	Action AuditAction
	// ActorID is the immutable identity of the acting user.
	ActorID uuid.UUID
	// EntityType discriminates the modified aggregate (e.g. "transaction").
	EntityType string
	// EntityID references the modified aggregate root.
	EntityID uuid.UUID
	// Before and After hold the pre/post state snapshots as raw JSON bytes.
	// The stored bytes are hashed verbatim and must never be re-marshaled, or
	// the hash drifts from the persisted value.
	Before json.RawMessage
	After  json.RawMessage
	// PrevHash is the 64-char lowercase hex SHA-256 of the previous chain
	// entry; empty for the genesis entry.
	PrevHash string
	// Hash is the 64-char lowercase hex SHA-256 computed once at construction
	// over the canonical form of this entry's fields plus PrevHash.
	Hash string
	// Timestamp is the UTC wall clock of the operation, canonicalized to
	// RFC3339Nano inside ComputeHash. Set server-side at the write point.
	Timestamp time.Time
	// CorrelationID carries request tracing context. It is deliberately
	// excluded from the hash input as non-authoritative metadata.
	CorrelationID string
}

// NewAuditEntry validates the caller-supplied fields and constructs an entry
// whose Hash is fixed to the canonical hash of the given fields. This makes the
// chained hash accurate by construction rather than relying on the caller to
// keep Hash in sync. Validation rejects nil identities, a non-canonical action,
// a missing snapshot pair, a zero timestamp, and a malformed PrevHash.
func NewAuditEntry(
	id, tenantID uuid.UUID,
	seq int64,
	action AuditAction,
	actorID uuid.UUID,
	entityType string,
	entityID uuid.UUID,
	before, after json.RawMessage,
	prevHash string,
	timestamp time.Time,
	correlationID string,
) (*AuditEntry, error) {
	if id == uuid.Nil {
		return nil, ErrInvalidAuditEntry
	}
	if tenantID == uuid.Nil {
		return nil, ErrInvalidAuditEntry
	}
	if seq < 0 {
		return nil, ErrInvalidAuditEntry
	}
	if !action.Valid() {
		return nil, ErrInvalidAuditEntry
	}
	if actorID == uuid.Nil {
		return nil, ErrInvalidAuditEntry
	}
	if strings.TrimSpace(entityType) == "" {
		return nil, ErrInvalidAuditEntry
	}
	if entityID == uuid.Nil {
		return nil, ErrInvalidAuditEntry
	}
	// A mutation audit must never be a no-op: both snapshots nil means there is
	// nothing to log.
	if len(before) == 0 && len(after) == 0 {
		return nil, ErrInvalidAuditEntry
	}
	if timestamp.IsZero() {
		return nil, ErrInvalidAuditEntry
	}
	if !(prevHash == "" || isLowerHex64(prevHash)) {
		return nil, ErrInvalidAuditEntry
	}

	e := &AuditEntry{
		ID:            id,
		Seq:           seq,
		TenantID:      tenantID,
		Action:        action,
		ActorID:       actorID,
		EntityType:    entityType,
		EntityID:      entityID,
		Before:        before,
		After:         after,
		PrevHash:      prevHash,
		Timestamp:     timestamp,
		CorrelationID: correlationID,
	}
	e.Hash = e.ComputeHash()
	return e, nil
}

// ComputeHash returns the lowercase hex SHA-256 over this entry's canonical,
// order-fixed, length-prefixed byte sequence. The framing is
// [uint64 big-endian length][raw bytes] per field, so variable-length fields
// (entityType, before, after) cannot collide or be boundary-smuggled. Before
// and After are embedded as their exact raw bytes; a nil snapshot contributes a
// zero-length segment, which keeps CREATE (nil before) and DELETE (nil after)
// hashes stable and distinct.
func (e *AuditEntry) ComputeHash() string {
	f := make([]byte, 0, 256)
	f = appendField(f, []byte{hashAlgorithmVersion})
	f = appendField(f, []byte(e.PrevHash))
	f = appendField(f, []byte(e.Timestamp.UTC().Format(time.RFC3339Nano)))
	f = appendField(f, []byte(e.TenantID.String()))
	f = appendField(f, []byte(e.ActorID.String()))
	f = appendField(f, []byte(e.EntityType))
	f = appendField(f, []byte(e.EntityID.String()))
	f = appendField(f, []byte(string(e.Action)))
	f = appendField(f, e.Before)
	f = appendField(f, e.After)
	sum := sha256.Sum256(f)
	return hex.EncodeToString(sum[:])
}

// appendField appends a self-delimiting segment of the canonical hash input:
// [uint64 big-endian length][bytes]. A zero-length segment (nil JSON snapshot)
// is encoded as a zero length followed by no bytes.
func appendField(dst, b []byte) []byte {
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(b)))
	dst = append(dst, lenBuf[:]...)
	dst = append(dst, b...)
	return dst
}

// isLowerHex64 reports whether s is exactly 64 lowercase ASCII hex characters,
// the format produced by ComputeHash.
func isLowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// VerifyChain replays entries[0..n) in ascending Seq order, checking both that
// each entry's stored Hash equals a recomputation of its own fields and that
// each non-genesis entry's PrevHash equals the previous entry's Hash. It stops
// at the first mismatch and returns (index, false); the returned index is the
// first broken entry, and every entry after it is invalidated by the broken
// link. A fully verified chain returns (len(entries), true). The empty chain
// verifies vacuously.
func VerifyChain(entries []AuditEntry) (int, bool) {
	for i := 0; i < len(entries); i++ {
		if entries[i].ComputeHash() != entries[i].Hash {
			return i, false
		}
		if i > 0 && entries[i].PrevHash != entries[i-1].Hash {
			return i, false
		}
	}
	return len(entries), true
}