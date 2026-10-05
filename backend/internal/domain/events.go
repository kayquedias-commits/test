// Package domain holds the financial-management core with zero framework,
// HTTP, or persistence dependencies (Clean Architecture invariant).
package domain

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// This file defines the canonical internal domain event contracts: an Event
// envelope plus a set of typed payloads. Producers (use cases) construct
// events through these types; consumers (budget actuals, reporting read model,
// audit store, export status) switch on EventType constants and decode payloads
// through the closed registry. Transport and the publish/subscribe backbone are
// deliberately absent here and live behind the events output port.

// EventType discriminates the concrete event kinds published on the internal
// event bus. Values are the canonical wire identifiers; consumers must match
// on these constants, never on hand-rolled string literals.
type EventType string

const (
	// EventTransactionPosted fires when a ledger transaction is recorded.
	EventTransactionPosted EventType = "transaction.posted"
	// EventTransactionEdited fires when an existing ledger transaction is modified.
	EventTransactionEdited EventType = "transaction.edited"
	// EventTransactionDeleted fires when an existing ledger transaction is removed.
	EventTransactionDeleted EventType = "transaction.deleted"
	// EventBudgetThresholdApproached fires when budget actuals reach an approaching threshold.
	EventBudgetThresholdApproached EventType = "budget.threshold.approached"
	// EventBudgetThresholdExceeded fires when budget actuals exceed the allocation.
	EventBudgetThresholdExceeded EventType = "budget.threshold.exceeded"
	// EventPeriodClosed fires when a fiscal period is closed and locked.
	EventPeriodClosed EventType = "period.closed"
	// EventUserDeprovisioned fires when a user account is deprovisioned.
	EventUserDeprovisioned EventType = "user.deprovisioned"
	// EventEntityModified fires for every create/edit/delete to feed the append-only audit store.
	EventEntityModified EventType = "entity.modified"
	// EventExportReady fires when an asynchronous export completes.
	EventExportReady EventType = "export.ready"
	// EventExportFailed fires when an asynchronous export terminates in failure.
	EventExportFailed EventType = "export.failed"
)

// Sentinel errors surfaced by the event registry and envelope validation.
// Wrap-agnostic identities let adapters map them to API error contracts and
// tests assert on identity rather than message text.
var (
	// ErrUnknownEventType is returned when an EventType is not in the closed registry.
	ErrUnknownEventType = errors.New("domain: unknown event type")
	// ErrInvalidEvent is returned when an Event envelope fails field validation.
	ErrInvalidEvent = errors.New("domain: invalid event")
	// ErrInvalidCorrelationID is returned when a correlation id violates the format allowlist.
	ErrInvalidCorrelationID = errors.New("domain: invalid correlation id")
	// ErrInvalidSequence is returned when a sequence number violates ordering semantics.
	ErrInvalidSequence = errors.New("domain: invalid sequence")
	// ErrInvalidPayload is returned when a payload fails decoding or field validation.
	ErrInvalidPayload = errors.New("domain: invalid payload")
)

// correlationIDRE is the structural allowlist validated at the service
// boundary before a correlation id is propagated. It rejects CR/LF and all
// characters outside [A-Za-z0-9_-], bounding log-injection (CWE-117) and
// guaranteeing a stable trace key.
var correlationIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Event is the canonical envelope carried by every internal domain event. It
// is the exact shape of an outbox row (§7.4), so a state change and its
// message are written in the same ACID transaction with no transformation.
//
// Contract invariants that live outside this struct and MUST be honored by the
// event bus and every consumer:
//
//   - Tenant isolation: each consumer MUST verify that TenantID matches its own
//     tenant context and discard/ignore any event with a mismatched TenantID
//     before processing Payload. Presence of TenantID/ActorID in the envelope
//     never authorizes a consumer to process an event.
//   - Sequence is assigned exclusively by the publishing infrastructure,
//     monotonically increasing per (AggregateType, AggregateID) stream. It is
//     never read from request or client input; consumers checkpoint on it for
//     idempotent, in-order projection.
//   - CorrelationID carries only a sanitized, server-validated value; raw
//     client-supplied correlation input is rejected or regenerated upstream.
type Event struct {
	// ID uniquely identifies the event, enabling idempotent consumer handling
	// under at-least-once delivery.
	ID uuid.UUID
	// Type discriminates the event and selects its registered payload struct.
	Type EventType
	// Version is the payload schema version for Type (§7.3), incremented only
	// when the payload's JSON schema changes. It is not an aggregate version.
	Version int
	// Timestamp is the UTC time the event was emitted; never mutated downstream.
	Timestamp time.Time
	// TenantID is the tenancy partition key; every consumer filters on it.
	TenantID uuid.UUID
	// ActorID is the user identity that caused the event. A reserved system
	// principal is used for batch/period-close events, never the zero UUID.
	ActorID uuid.UUID
	// CorrelationID is the tracing spine spanning API, use case, and consumers.
	CorrelationID string
	// SourceService names the module that produced the event (Ledger, Budget,
	// Reporting, IAM, Export, Audit), per the source-service field of §7.4.
	SourceService string
	// AggregateType is the aggregate context for the event (account,
	// transaction, budget, category, period, user, export).
	AggregateType string
	// AggregateID is the stream key; sequence is monotonic within this value.
	// It is a string to allow heterogeneous aggregate identifiers while
	// remaining a stable partition key.
	AggregateID string
	// Payload is a lazily deserialized typed payload matching Type. It must be
	// non-nil valid JSON for the registered Type; consumers decode it only via
	// DecodePayload, never by direct unmarshaling.
	Payload json.RawMessage
	// Sequence is the per-stream monotonic order assigned by the publisher.
	Sequence int64
}

// eventTypeSpec binds an EventType to the schema version of its payload and the
// decoder that turns the raw JSON into the registered typed struct. This is the
// closed registry: every constant must be registered or it is rejected at the
// bus boundary before any consumer processing.
type eventTypeSpec struct {
	version int
	decode  func(json.RawMessage) (any, error)
}

// eventTypeSpecs is the closed event-type registry. All constants are required
// to be present; an unregistered Type fails Valid() and DecodePayload.
var eventTypeSpecs = map[EventType]eventTypeSpec{
	EventTransactionPosted:         {version: 1, decode: decodeTransactionPosted},
	EventTransactionEdited:         {version: 1, decode: decodeTransactionEdited},
	EventTransactionDeleted:        {version: 1, decode: decodeTransactionEdited},
	EventBudgetThresholdApproached: {version: 1, decode: decodeBudgetThreshold},
	EventBudgetThresholdExceeded:   {version: 1, decode: decodeBudgetThreshold},
	EventPeriodClosed:              {version: 1, decode: decodePeriodClosed},
	EventUserDeprovisioned:         {version: 1, decode: decodeUserDeprovisioned},
	EventEntityModified:            {version: 1, decode: decodeEntityModified},
	EventExportReady:               {version: 1, decode: decodeExport},
	EventExportFailed:              {version: 1, decode: decodeExport},
}

// Valid reports whether t is present in the closed event-type registry.
func (t EventType) Valid() bool {
	_, ok := eventTypeSpecs[t]
	return ok
}

// Version returns the payload schema version registered for t and whether t is
// registered. The two-value form lets callers distinguish an unregistered type
// from a registered one without an error sentinel.
func (t EventType) Version() (int, bool) {
	spec, ok := eventTypeSpecs[t]
	if !ok {
		return 0, false
	}
	return spec.version, true
}

// validateAmount reports whether s is a non-negative decimal string with at
// most MaxDecimals fractional places. It reuses the ledger's allowlist so event
// amounts respect the same precision, sign, and length constraints as Money
// (the payload carries its monetary value as a decimal string, staying clear of
// binary floats per §7.3).
func validateAmount(s string) error {
	if !minorStringRE.MatchString(s) {
		return ErrInvalidPayload
	}
	return nil
}

// validateUsedPct reports whether s is a decimal string in [0, 100] carrying at
// most MaxDecimals fractional places. Because minorStringRE already bound the
// fractional component to two digits, only the integer part needs a range check
// against 100. Threshold semantics are exact: both producer and consumer
// compare on the fixed-precision decimal string.
func validateUsedPct(s string) error {
	if !minorStringRE.MatchString(s) {
		return ErrInvalidPayload
	}
	intPart := s
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		intPart = s[:dot]
	}
	iv, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil || iv > 100 {
		return ErrInvalidPayload
	}
	return nil
}

// TransactionPostedPayload is the payload for EventTransactionPosted. Amount is
// a non-negative decimal string with at most two decimals, consistent with the
// ledger's monetary representation.
type TransactionPostedPayload struct {
	TransactionID uuid.UUID
	AccountID     uuid.UUID
	Amount        string
	Type          TransactionType
}

func decodeTransactionPosted(raw json.RawMessage) (any, error) {
	var p TransactionPostedPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, ErrInvalidPayload
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// Validate checks the required fields of a transaction-posted payload.
func (p TransactionPostedPayload) Validate() error {
	if p.TransactionID == uuid.Nil || p.AccountID == uuid.Nil {
		return ErrInvalidPayload
	}
	if err := validateAmount(p.Amount); err != nil {
		return err
	}
	if !p.Type.Valid() {
		return ErrInvalidTransactionType
	}
	return nil
}

// NewTransactionPostedPayload constructs a validated transaction-posted
// payload. It enforces the monetary and type invariants at construction so a
// malformed payload can never be published.
func NewTransactionPostedPayload(transactionID, accountID uuid.UUID, amount string, typ TransactionType) (TransactionPostedPayload, error) {
	p := TransactionPostedPayload{
		TransactionID: transactionID,
		AccountID:     accountID,
		Amount:        amount,
		Type:          typ,
	}
	if err := p.Validate(); err != nil {
		return TransactionPostedPayload{}, err
	}
	return p, nil
}

// TransactionEditedPayload is the payload for both EventTransactionEdited and
// EventTransactionDeleted. Before/After carry minimal, purpose-specific
// snapshots of the aggregate at each state, never full entity copies (so a
// consumer receives only the fields its function requires — over-exposure of
// sensitive financial data is prevented by keeping snapshots minimal). Nil
// semantics: a delete event carries a populated Before and a nil After; a
// create carries a nil Before and a populated After; an edit carries both.
type TransactionEditedPayload struct {
	TransactionID uuid.UUID
	Before        json.RawMessage
	After         json.RawMessage
}

func decodeTransactionEdited(raw json.RawMessage) (any, error) {
	var p TransactionEditedPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, ErrInvalidPayload
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// Validate requires a non-zero transaction id and at least one of Before/After.
func (p TransactionEditedPayload) Validate() error {
	if p.TransactionID == uuid.Nil {
		return ErrInvalidPayload
	}
	if p.Before == nil && p.After == nil {
		return ErrInvalidPayload
	}
	return nil
}

// BudgetThresholdPayload is the payload for EventBudgetThresholdApproached and
// EventBudgetThresholdExceeded. UsedPct is a decimal string in [0, 100]; the
// routing to the Notifier port decides the delivery channel.
type BudgetThresholdPayload struct {
	BudgetID   uuid.UUID
	CategoryID uuid.UUID
	UsedPct    string
}

func decodeBudgetThreshold(raw json.RawMessage) (any, error) {
	var p BudgetThresholdPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, ErrInvalidPayload
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// Validate checks the required identifiers and the [0,100] usage percentage.
func (p BudgetThresholdPayload) Validate() error {
	if p.BudgetID == uuid.Nil || p.CategoryID == uuid.Nil {
		return ErrInvalidPayload
	}
	if err := validateUsedPct(p.UsedPct); err != nil {
		return err
	}
	return nil
}

// NewBudgetThresholdPayload constructs a validated budget-threshold payload.
func NewBudgetThresholdPayload(budgetID, categoryID uuid.UUID, usedPct string) (BudgetThresholdPayload, error) {
	p := BudgetThresholdPayload{BudgetID: budgetID, CategoryID: categoryID, UsedPct: usedPct}
	if err := p.Validate(); err != nil {
		return BudgetThresholdPayload{}, err
	}
	return p, nil
}

// PeriodClosedPayload is the payload for EventPeriodClosed; it locks the period
// and triggers the reporting snapshot.
type PeriodClosedPayload struct {
	PeriodID uuid.UUID
	ClosedAt time.Time
}

func decodePeriodClosed(raw json.RawMessage) (any, error) {
	var p PeriodClosedPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, ErrInvalidPayload
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// Validate requires a non-zero period id and a non-zero close time.
func (p PeriodClosedPayload) Validate() error {
	if p.PeriodID == uuid.Nil || p.ClosedAt.IsZero() {
		return ErrInvalidPayload
	}
	return nil
}

// UserDeprovisionedPayload is the payload for EventUserDeprovisioned, driving
// session revocation and access-certification logging.
type UserDeprovisionedPayload struct {
	UserID uuid.UUID
}

func decodeUserDeprovisioned(raw json.RawMessage) (any, error) {
	var p UserDeprovisionedPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, ErrInvalidPayload
	}
	if p.UserID == uuid.Nil {
		return ErrInvalidPayload
	}
	return p, nil
}

// EntityModifiedPayload is the payload for EventEntityModified, which feeds the
// append-only audit store asynchronously. Action reuses the domain AuditAction
// enum (single source of truth); EntityType follows the AggregateType
// vocabulary. Before/After are minimal, purpose-specific snapshots with the
// same nil semantics as TransactionEditedPayload.
type EntityModifiedPayload struct {
	EntityType string
	EntityID   string
	Action     AuditAction
	Before     json.RawMessage
	After      json.RawMessage
}

func decodeEntityModified(raw json.RawMessage) (any, error) {
	var p EntityModifiedPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, ErrInvalidPayload
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// Validate requires a non-empty entity reference, a known mutation action, and
// at least one of Before/After.
func (p EntityModifiedPayload) Validate() error {
	if p.EntityType == "" || p.EntityID == "" {
		return ErrInvalidPayload
	}
	if !p.Action.IsMutation() {
		return ErrInvalidAuditAction
	}
	if p.Before == nil && p.After == nil {
		return ErrInvalidPayload
	}
	return nil
}

// ExportPayload is the payload for both EventExportReady and EventExportFailed;
// it reports an asynchronous export job's terminal state to the status
// endpoint.
type ExportPayload struct {
	ExportID    uuid.UUID
	DownloadURL string
	Error       string
}

func decodeExport(raw json.RawMessage) (any, error) {
	var p ExportPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, ErrInvalidPayload
	}
	if p.ExportID == uuid.Nil {
		return ErrInvalidPayload
	}
	return p, nil
}

// Validate checks the required export identifier.
func (p ExportPayload) Validate() error {
	if p.ExportID == uuid.Nil {
		return ErrInvalidPayload
	}
	return nil
}

// Validate checks the Event envelope's required fields and type/schema
// consistency against the registry. It does not decode the payload; callers
// validate the payload separately via DecodePayload.
func (e Event) Validate() error {
	if e.ID == uuid.Nil || e.TenantID == uuid.Nil || e.ActorID == uuid.Nil {
		return ErrInvalidEvent
	}
	if !e.Type.Valid() {
		return ErrUnknownEventType
	}
	specVersion, ok := e.Type.Version()
	if !ok || e.Version <= 0 || e.Version != specVersion {
		return ErrInvalidEvent
	}
	if e.Timestamp.IsZero() {
		return ErrInvalidEvent
	}
	if !correlationIDRE.MatchString(e.CorrelationID) {
		return ErrInvalidCorrelationID
	}
	if e.SourceService == "" || e.AggregateType == "" || e.AggregateID == "" {
		return ErrInvalidEvent
	}
	if e.Sequence <= 0 {
		return ErrInvalidSequence
	}
	if e.Payload == nil {
		return ErrInvalidEvent
	}
	return nil
}

// DecodePayload is the single unmarshal dispatch point: it routes a raw
// payload to the registered struct for t and field-validates the result. An
// unknown type is rejected with ErrUnknownEventType; a schema mismatch or a
// failed field check yields ErrInvalidPayload. Consumers must decode strictly
// through this function and never unmarshal Payload directly.
func DecodePayload(t EventType, raw json.RawMessage) (any, error) {
	spec, ok := eventTypeSpecs[t]
	if !ok {
		return nil, ErrUnknownEventType
	}
	return spec.decode(raw)
}