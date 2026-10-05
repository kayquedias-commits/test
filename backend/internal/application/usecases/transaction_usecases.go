package usecases

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/domain"
	"fms/backend/internal/infrastructure/persistence/postgres"
)

const (
	classicSourceService    = "ledger"
	transactionAggregateType = "transaction"
	maxCategoryCount         = 32
	transactionLimitDefault  = 100
	transactionLimitMax      = 200
)

// RecordTransactionCommand is the application-layer input contract for
// recording a ledger transaction. Validation of value invariants is delegated
// to domain.NewTransaction; existence and tenancy are checked by the service.
type RecordTransactionCommand struct {
	AccountID   uuid.UUID
	CategoryIDs []uuid.UUID
	Amount      domain.Money
	Direction   domain.TransactionType
	Date        time.Time
	Description string
	Scheduled   bool
	ReasonCode  *domain.ReasonCode
}

// TransactionPage is the concrete cursor-paginated transaction result returned
// to the HTTP adapter.
type TransactionPage struct {
	Items      []*domain.Transaction
	NextCursor *string
	HasMore    bool
}

// TransactionService orchestrates the module B ledger use cases: record, edit,
// delete, get, and list transactions. Every mutation keeps account balances
// reconciled inside a single ACID unit of work and publishes domain events
// through the outbox so audit and budget consumers never block the write path.
type TransactionService struct {
	store    *postgres.Store
	txns     ports.TransactionRepository
	accounts ports.AccountRepository
	cats     ports.CategoryRepository
	events   ports.EventBus
	outbox   ports.OutboxRepository
	now      ports.Clock
}

// NewTransactionService wires the transaction use cases to their output ports.
func NewTransactionService(store *postgres.Store, txns ports.TransactionRepository, accounts ports.AccountRepository, cats ports.CategoryRepository, events ports.EventBus, outbox ports.OutboxRepository, now ports.Clock) *TransactionService {
	if now == nil {
		now = ports.SystemClock{}
	}
	return &TransactionService{
		store:    store,
		txns:     txns,
		accounts: accounts,
		cats:     cats,
		events:   events,
		outbox:   outbox,
		now:      now,
	}
}

// signedDelta returns the net balance change for a transaction row: income
// adds, expense subtracts, with the sign carried by direction only.
func signedDelta(amount domain.Money, dir domain.TransactionType) domain.Money {
	if dir == domain.Expense {
		return amount.Negate()
	}
	return amount
}

// netDelta computes the combined balance change of reversing the old row and
// applying the new one in a single atomic statement.
func netDelta(oldAmount domain.Money, oldDir domain.TransactionType, newAmount domain.Money, newDir domain.TransactionType) (domain.Money, error) {
	reversed := signedDelta(oldAmount, oldDir).Negate()
	new := signedDelta(newAmount, newDir)
	delta, err := new.Add(reversed)
	if err != nil {
		return domain.Money{}, domain.NewInternal(err)
	}
	return delta, nil
}

// reverseDelta negates a row's signed contribution for delete operations.
func reverseDelta(amount domain.Money, dir domain.TransactionType) domain.Money {
	return signedDelta(amount, dir).Negate()
}

// newEvent builds a validated domain event carrying immutable payloads.
func (s *TransactionService) newEvent(ctx context.Context, typ domain.EventType, tenantID, actorID, aggregateID uuid.UUID, payload any) (domain.Event, error) {
	if tenantID == uuid.Nil || actorID == uuid.Nil || aggregateID == uuid.Nil {
		// A nil id would break audit traceability and tenant isolation downstream.
		return domain.Event{}, domain.NewInternal(errors.New("event requires non-zero tenant, actor and aggregate ids"))
	}
	version, err := typ.Version()
	if err != nil {
		return domain.Event{}, domain.NewInternal(err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return domain.Event{}, domain.NewInternal(err)
	}
	evtID := uuid.New()
	ev := domain.Event{
		ID:             evtID,
		Type:           typ,
		Version:        version,
		Timestamp:      s.now.Now().UTC(),
		TenantID:       tenantID,
		ActorID:        actorID,
		CorrelationID:  evtID.String(),
		SourceService:  classicSourceService,
		AggregateType:  transactionAggregateType,
		AggregateID:    aggregateID.String(),
		Payload:        raw,
		Sequence:       1,
	}
	if err := ev.Validate(); err != nil {
		return domain.Event{}, domain.NewInternal(err)
	}
	return ev, nil
}

// txSnapshot renders a transaction row as a minimal immutable JSON snapshot for
// audit before/after payloads.
func txSnapshot(t *domain.Transaction) json.RawMessage {
	if t == nil {
		return nil
	}
	raw, err := json.Marshal(map[string]any{
		"id":           t.ID.String(),
		"account_id":   t.AccountID.String(),
		"category_ids": t.CategoryIDs,
		"amount":       t.Amount.String(),
		"type":         string(t.Type),
		"date":         t.Date.Format(time.RFC3339),
		"description":  t.Description,
		"scheduled":    t.Scheduled,
		"reason_code":  reasonCodeString(t.ReasonCode),
		"version":      t.Version,
	})
	if err != nil {
		return nil
	}
	return raw
}

func reasonCodeString(rc *domain.ReasonCode) string {
	if rc == nil {
		return ""
	}
	return string(*rc)
}

// appendAudit enqueues an EntityModified audit event into the outbox bound to
// the open unit of work; the consumer persists asynchronously after commit.
func (s *TransactionService) appendAudit(ctx context.Context, action domain.AuditAction, tenantID, actorID, entityID uuid.UUID, before, after json.RawMessage) error {
	payload := domain.EntityModifiedPayload{
		EntityType: transactionAggregateType,
		EntityID:   entityID.String(),
		Action:     action,
		Before:     before,
		After:      after,
	}
	ev, err := s.newEvent(ctx, domain.EventEntityModified, tenantID, actorID, entityID, payload)
	if err != nil {
		return err
	}
	return s.outbox.Append(ctx, ev)
}

// RecordTransaction creates a ledger transaction, verifies tenant ownership of
// the referenced account and categories, applies the balance delta atomically,
// and publishes TransactionPosted plus a CREATE audit event.
func (s *TransactionService) RecordTransaction(ctx context.Context, cmd RecordTransactionCommand, tenantID, actorID uuid.UUID) (*domain.Transaction, error) {
	if tenantID == uuid.Nil {
		return nil, domain.NewForbidden("invalid tenant context")
	}
	if actorID == uuid.Nil {
		return nil, domain.NewForbidden("insufficient permissions")
	}
	if len(cmd.CategoryIDs) > maxCategoryCount {
		return nil, domain.NewValidationError(domain.FieldError{Field: "categories", Message: "too many categories"})
	}

	account, derr := s.accounts.GetByID(ctx, tenantID, cmd.AccountID)
	if derr != nil {
		return nil, derr
	}
	for _, cid := range cmd.CategoryIDs {
		if _, derr := s.cats.GetByID(ctx, tenantID, cid); derr != nil {
			return nil, derr
		}
	}

	t, err := domain.NewTransaction(uuid.New(), tenantID, cmd.AccountID, cmd.CategoryIDs, cmd.Amount, cmd.Date, cmd.Description, cmd.Direction, cmd.Scheduled, cmd.ReasonCode)
	if err != nil {
		return nil, mapTxValidationError(err)
	}
	t.TenantID = tenantID

	uow, derr := s.store.Begin(ctx)
	if derr != nil {
		return nil, derr
	}

	if derr := s.txns.Insert(ctx, tenantID, &t); derr != nil {
		_ = uow.Rollback()
		return nil, derr
	}
	delta := signedDelta(t.Amount, t.Direction)
	if _, derr := s.accounts.UpdateBalance(ctx, tenantID, cmd.AccountID, delta, account.Version); derr != nil {
		_ = uow.Rollback()
		return nil, derr
	}

	posted, err := s.newEvent(ctx, domain.EventTransactionPosted, tenantID, actorID, t.ID, domain.NewTransactionPostedPayload(t.ID, cmd.AccountID, t.Amount.String(), t.Direction))
	if err != nil {
		_ = uow.Rollback()
		return nil, err
	}
	audit, err := s.appendAudit(ctx, domain.ActionCreate, tenantID, actorID, t.ID, nil, txSnapshot(&t))
	if err != nil {
		_ = uow.Rollback()
		return nil, err
	}
	if derr := s.outbox.Append(ctx, posted); derr != nil {
		_ = uow.Rollback()
		return nil, derr
	}

	if derr := uow.Commit(); derr != nil {
		return nil, derr
	}
	// Post-commit best-effort dispatch; durability already rests on the outbox.
	_ = s.events.Publish(ctx, posted)
	_ = s.events.Publish(ctx, audit)
	return &t, nil
}

// EditTransaction reverses the prior balance delta and applies the new one
// within a single ACID scope, guarded by the expected transaction and account
// versions. Publishes TransactionEdited plus a before/after EDIT audit event.
func (s *TransactionService) EditTransaction(ctx context.Context, id uuid.UUID, expectedVersion int64, cmd RecordTransactionCommand, tenantID, actorID uuid.UUID) (*domain.Transaction, error) {
	if tenantID == uuid.Nil {
		return nil, domain.NewForbidden("invalid tenant context")
	}
	if actorID == uuid.Nil {
		return nil, domain.NewForbidden("insufficient permissions")
	}
	if len(cmd.CategoryIDs) > maxCategoryCount {
		return nil, domain.NewValidationError(domain.FieldError{Field: "categories", Message: "too many categories"})
	}

	before, derr := s.txns.GetByID(ctx, tenantID, id)
	if derr != nil {
		return nil, derr
	}
	if expectedVersion != before.Version {
		return nil, domain.NewConflict("transaction was modified by another user")
	}
	account, derr := s.accounts.GetByID(ctx, tenantID, cmd.AccountID)
	if derr != nil {
		return nil, derr
	}

	after, err := domain.NewTransaction(before.ID, tenantID, cmd.AccountID, cmd.CategoryIDs, cmd.Amount, cmd.Date, cmd.Description, cmd.Direction, cmd.Scheduled, cmd.ReasonCode)
	if err != nil {
		return nil, mapTxValidationError(err)
	}
	after.CreatedAt = before.CreatedAt
	after.TenantID = tenantID

	delta, err := netDelta(before.Amount, before.Type, after.Amount, after.Type)
	if err != nil {
		return nil, err
	}

	uow, derr := s.store.Begin(ctx)
	if derr != nil {
		return nil, derr
	}

	if _, derr := s.accounts.UpdateBalance(ctx, tenantID, cmd.AccountID, delta, account.Version); derr != nil {
		_ = uow.Rollback()
		return nil, derr
	}
	if derr := s.txns.Update(ctx, tenantID, &after, expectedVersion); derr != nil {
		_ = uow.Rollback()
		return nil, derr
	}

	edited, err := s.newEvent(ctx, domain.EventTransactionEdited, tenantID, actorID, after.ID, domain.TransactionEditedPayload{
		TransactionID: after.ID,
		Before:        txSnapshot(before),
		After:         txSnapshot(&after),
	})
	if err != nil {
		_ = uow.Rollback()
		return nil, err
	}
	audit, err := s.appendAudit(ctx, domain.ActionEdit, tenantID, actorID, after.ID, txSnapshot(before), txSnapshot(&after))
	if err != nil {
		_ = uow.Rollback()
		return nil, err
	}
	if derr := s.outbox.Append(ctx, edited); derr != nil {
		_ = uow.Rollback()
		return nil, derr
	}

	if derr := uow.Commit(); derr != nil {
		return nil, derr
	}
	_ = s.events.Publish(ctx, edited)
	_ = s.events.Publish(ctx, audit)
	return &after, nil
}

// DeleteTransaction reverses the row's balance delta and removes the row with a
// version guard inside one ACID scope. Publishes TransactionDeleted and a
// DELETE audit event. A second delete for the same id returns NOT_FOUND here;
// the HTTP layer maps it to 404.
func (s *TransactionService) DeleteTransaction(ctx context.Context, id uuid.UUID, expectedVersion int64, tenantID, actorID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return domain.NewForbidden("invalid tenant context")
	}
	if actorID == uuid.Nil {
		return domain.NewForbidden("insufficient permissions")
	}

	before, derr := s.txns.GetByID(ctx, tenantID, id)
	if derr != nil {
		return derr
	}
	if expectedVersion != before.Version {
		return domain.NewConflict("transaction was modified by another user")
	}
	account, derr := s.accounts.GetByID(ctx, tenantID, before.AccountID)
	if derr != nil {
		return derr
	}

	uow, derr := s.store.Begin(ctx)
	if derr != nil {
		return derr
	}

	reverse := reverseDelta(before.Amount, before.Type)
	if _, derr := s.accounts.UpdateBalance(ctx, tenantID, before.AccountID, reverse, account.Version); derr != nil {
		_ = uow.Rollback()
		return derr
	}
	if derr := s.txns.Delete(ctx, tenantID, id, expectedVersion); derr != nil {
		_ = uow.Rollback()
		return derr
	}

	deleted, err := s.newEvent(ctx, domain.EventTransactionDeleted, tenantID, actorID, before.ID, domain.TransactionEditedPayload{
		TransactionID: before.ID,
		Before:        txSnapshot(before),
		After:         nil,
	})
	if err != nil {
		_ = uow.Rollback()
		return err
	}
	audit, err := s.appendAudit(ctx, domain.ActionDelete, tenantID, actorID, before.ID, txSnapshot(before), nil)
	if err != nil {
		_ = uow.Rollback()
		return err
	}
	if derr := s.outbox.Append(ctx, deleted); derr != nil {
		_ = uow.Rollback()
		return derr
	}

	if derr := uow.Commit(); derr != nil {
		return derr
	}
	_ = s.events.Publish(ctx, deleted)
	_ = s.events.Publish(ctx, audit)
	return nil
}

// GetTransaction returns a single tenant-scoped transaction.
func (s *TransactionService) GetTransaction(ctx context.Context, id, tenantID uuid.UUID) (*domain.Transaction, error) {
	if tenantID == uuid.Nil {
		return nil, domain.NewForbidden("invalid tenant context")
	}
	return s.txns.GetByID(ctx, tenantID, id)
}

// ListTransactions applies the tenant-scoped filter and cursor pagination,
// delegating the parameterized query (with eager account/category joins) to the
// transaction repository. This is an interactive ledger query, not a report
// aggregation, so it reads from the authoritative store.
func (s *TransactionService) ListTransactions(ctx context.Context, filter ports.TransactionFilter, cursor ports.CursorPage, tenantID uuid.UUID) (*TransactionPage, error) {
	if tenantID == uuid.Nil {
		return nil, domain.NewForbidden("invalid tenant context")
	}
	if filter.From != nil && filter.To != nil && filter.To.Before(*filter.From) {
		return nil, domain.NewValidationError(domain.FieldError{Field: "range", Message: "from must not be after to"})
	}
	if filter.MinAmount != nil && filter.MaxAmount != nil {
		cmp, err := filter.MinAmount.Cmp(*filter.MaxAmount)
		if err != nil {
			return nil, domain.NewValidationError(domain.FieldError{Field: "amount", Message: "invalid amount bounds"})
		}
		if cmp > 0 {
			return nil, domain.NewValidationError(domain.FieldError{Field: "amount", Message: "amount_min must not exceed amount_max"})
		}
	}
	if cursor.Limit <= 0 {
		cursor.Limit = transactionLimitDefault
	}
	if cursor.Limit > transactionLimitMax {
		cursor.Limit = transactionLimitMax
	}

	items, page, derr := s.txns.List(ctx, tenantID, filter, cursor)
	if derr != nil {
		return nil, derr
	}
	if items == nil {
		items = []*domain.Transaction{}
	}
	var next *string
	if page.NextCursor != nil {
		next = page.NextCursor
	}
	return &TransactionPage{Items: items, NextCursor: next, HasMore: page.NextCursor != nil}, nil
}

// mapTxValidationError converts domain constructor errors into typed validation
// errors so the HTTP adapter can map them to VALIDATION_FAILED without losing
// the field context.
func mapTxValidationError(err error) error {
	switch {
	case errors.Is(err, domain.ErrFutureDateRequiresScheduled):
		return domain.NewValidationError(domain.FieldError{Field: "date", Message: "future-dated transactions require the scheduled flag"})
	case errors.Is(err, domain.ErrNegativeAmountRequiresReasonCode):
		return domain.NewValidationError(domain.FieldError{Field: "amount", Message: "negative amount requires a reason code"})
	case errors.Is(err, domain.ErrReasonCodeNotAllowed):
		return domain.NewValidationError(domain.FieldError{Field: "reason_code", Message: "a reason code is only allowed for negative amounts"})
	case errors.Is(err, domain.ErrAtLeastOneCategory):
		return domain.NewValidationError(domain.FieldError{Field: "categories", Message: "at least one category is required"})
	case errors.Is(err, domain.ErrInvalidDescription):
		return domain.NewValidationError(domain.FieldError{Field: "description", Message: "description must be non-empty and at most 255 characters"})
	case errors.Is(err, domain.ErrInvalidTransactionDate):
		return domain.NewValidationError(domain.FieldError{Field: "date", Message: "date must be a valid calendar date"})
	case errors.Is(err, domain.ErrInvalidTransactionType):
		return domain.NewValidationError(domain.FieldError{Field: "direction", Message: "kind must be income or expense"})
	case errors.Is(err, domain.ErrInvalidAccountID):
		return domain.NewValidationError(domain.FieldError{Field: "account_id", Message: "account is required"})
	case errors.Is(err, domain.ErrInvalidTenantID):
		return domain.NewValidationError(domain.FieldError{Field: "tenant", Message: "tenant is required"})
	case errors.Is(err, domain.ErrDuplicateCategory):
		return domain.NewValidationError(domain.FieldError{Field: "categories", Message: "duplicate categories are not allowed"})
	case errors.Is(err, domain.ErrTooManyCategories):
		return domain.NewValidationError(domain.FieldError{Field: "categories", Message: "too many categories"})
	default:
		return domain.NewInternal(err)
	}
}