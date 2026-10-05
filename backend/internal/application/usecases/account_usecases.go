// Package usecases implements the application use-case layer: it orchestrates
// output ports, applies business rules, owns transaction boundaries, and
// publishes domain events. This file delivers Module A (Account Management)
// and account reconciliation: create, update, delete (with the no-delete-while-
// linked-transactions boundary and a bounded cascade), list, and reconcile that
// restores balance == opening_balance + SUM(transactions) under an optimistic
// lock.
//
// Clean Architecture: this layer depends only on the domain and application
// ports (never HTTP, SQL, or a framework). Two deliberate deviations from the
// review plan are applied below:
//
//   - The atomic reconcile repair (SUM + absolute compare-and-swap on balance/
//     version) is delegated to a single AccountRepository.Reconcile operation
//     that runs entirely inside one DB transaction, rather than import
//     persistence/postgres (which would violate the Dependency Rule by pulling
//     a Frameworks/Drivers package into the use case). The transaction boundary
//     therefore lives in the repository adapter; a use-case-owned UnitOfWork
//     port is left as a future task.
//   - Because the domain event registry is closed and contains only
//     EventEntityModified (with ActionCreate/Edit/Delete on EntityModifiedPayload)
//     — there is no AccountDeleted or AccountBalanceAdjusted event type — every
//     account mutation, including a reconcile drift repair, is published as the
//     canonical EventEntityModified audit feed. This also requires an explicit
//     actorID on the mutating methods so the envelope carries a non-zero
//     ActorID (a domain.Event.Validate() invariant) and audit user identity.
package usecases

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/domain"
)

// Bounded-collection listing defaults (§7.2): accounts are an offset-paginated
// collection, so page size is clamped to accountMaxPageSize and never unbounded.
const (
	accountDefaultPage     = 1
	accountDefaultPageSize = 100
	accountMaxPageSize     = 100
)

// accountMaxNameLen bounds account names, aligned with the transaction
// description rule (<= 255 characters). Names flow into export/report/render
// surfaces, so the bound is enforced at the boundary for defense in depth.
const accountMaxNameLen = 255

// accountMaxCascadeRows bounds a transactional cascade delete. Beyond this many
// linked transaction rows the request path refuses and routes to a migration or
// background job, so a runaway cascade can never partially delete within the
// tenant.
const accountMaxCascadeRows = 10000

// Sentinel errors surfaced by the account use cases. They are wrap-agnostic
// identities; the HTTP adapter maps the wrapped DomainError to CONFLICT_STATE
// (409) so the delete boundary and the cascade cap are distinguishable from a
// generic internal failure.
var (
	// ErrAccountHasTransactions is returned when deleting an account with linked
	// transactions while cascade was not explicitly allowed.
	ErrAccountHasTransactions = errors.New("usecases: account has linked transactions; delete requires reassignment or an explicit cascade")
	// ErrCascadeTooLarge is returned when a cascade delete would exceed the
	// bounded row budget and must be handled by a migration or background job.
	ErrCascadeTooLarge = errors.New("usecases: cascade exceeds the bounded limit and requires a migration or background job")
)

// CreateAccountCommand is the validated input for creating a financial account.
// OpeningBalance is the domain Money value object, which is itself immutable and
// enforces non-negativity and two-decimal precision; Currency must match the
// opening balance's currency.
type CreateAccountCommand struct {
	Name           string
	Type           domain.AccountType
	OpeningBalance domain.Money
	Currency       string
}

// UpdateAccountCommand is the validated input for editing a financial account.
// Pointer fields are applied only when non-nil, so a partial update leaves the
// omitted fields untouched. ExpectedVersion is the optimistic-lock guard.
type UpdateAccountCommand struct {
	ID              uuid.UUID
	Name            *string
	Type            *domain.AccountType
	ExpectedVersion int64
}

// DeleteAccountOptions controls the destructive-account deletion boundary.
// AllowCascade must be explicitly set by the caller; it is never inferred from
// the account payload or a path flag.
type DeleteAccountOptions struct {
	AllowCascade bool
}

// AccountFilter is the optional-predicate set for listing accounts. Pointer
// fields are omitted from the tenant-scoped predicate when nil.
type AccountFilter struct {
	Name     *string
	Type     *domain.AccountType
	Currency *string
}

// PageParams is the offset-based pagination input for bounded collections.
// Page is 1-based; PageSize is clamped to accountMaxPageSize in the use case.
type PageParams struct {
	Page     int
	PageSize int
}

// Page is the offset-paginated result envelope returned by list use cases.
// NextPage is set only when more records follow the current page.
type Page[T any] struct {
	Items    []T
	Page     int
	PageSize int
	NextPage *int
}

// OffsetPage interpolates a ports.OffsetPage from a PageParams. It clamps Page
// to at least 1 and PageSize into [1, accountMaxPageSize], defaulting to the
// standard page size, so unbounded pagination can never reach the repository.
func (p PageParams) OffsetPage() ports.OffsetPage {
	page := p.Page
	if page < accountDefaultPage {
		page = accountDefaultPage
	}
	size := p.PageSize
	if size <= 0 {
		size = accountDefaultPageSize
	}
	if size > accountMaxPageSize {
		size = accountMaxPageSize
	}
	return ports.OffsetPage{Page: page, PageSize: size}
}

// AccountRepository is the output port for persisting Account aggregates for
// this use case. It is declared here (a private, use-case-scoped contract)
// because the shared ports.AccountRepository lacks an Update (name/type edit)
// method and its Reconcile returns only an error rather than the refreshed
// account the caller needs. The concrete adapter lives in the interface-adapter
// layer.
//
// Contract notes:
//   - Every method takes the tenant predicate explicitly so isolation is enforced
//     at the persistence boundary (RLS / WHERE tenant_id), never filtered in memory.
//   - Implementations must use parameterized queries only; caller-supplied IDs,
//     names, and filters must never be concatenated into SQL.
//   - Reconcile recomputes balance == opening_balance + SUM(transactions) as a
//     single aggregate and, on drift, repairs it via an absolute compare-and-swap
//     (UPDATE ... SET balance=$1, version=version+1 WHERE id=$2 AND version=$3
//     AND tenant_id=$4) all inside ONE DB transaction. A stale expected version
//     or a drifted-away tenant yields zero affected rows and surfaces
//     CONFLICT_STATE (double-spend/lost-update guard, §5.10).
type AccountRepository interface {
	Create(ctx context.Context, tenantID uuid.UUID, account *domain.Account) *domain.DomainError
	GetByID(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) (*domain.Account, *domain.DomainError)
	List(ctx context.Context, tenantID uuid.UUID, filter AccountFilter, page ports.OffsetPage) ([]*domain.Account, *domain.DomainError)
	Update(ctx context.Context, tenantID uuid.UUID, account *domain.Account, expectedVersion int64) *domain.DomainError
	Reconcile(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, expectedVersion int64) (*domain.Account, *domain.DomainError)
	Delete(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, expectedVersion int64, cascade bool) *domain.DomainError
}

// TransactionRepository is the output port for the ledger-transaction facts the
// delete and reconcile boundaries consume. Only existence/count reads are needed
// here; transaction writes belong to the Transaction use cases.
type TransactionRepository interface {
	// ExistsForAccount reports whether any non-deleted transaction references
	// the account. The implementation MUST use EXISTS, never COUNT, and scope by
	// tenant so the hot delete path short-circuits at the first row.
	ExistsForAccount(ctx context.Context, tenantID, accountID uuid.UUID) (bool, *domain.DomainError)
	// CountByAccount returns the count of non-deleted transactions referencing
	// the account — the cascade-bound gate.
	CountByAccount(ctx context.Context, tenantID, accountID uuid.UUID) (int64, *domain.DomainError)
}

// AccountService implements the Module A use cases. It depends only on output
// ports (Clean Architecture: no framework, HTTP, or ORM type leaks past this
// boundary). The clock is injected for deterministic event timestamps.
type AccountService struct {
	accounts AccountRepository
	txns     TransactionRepository
	events   ports.EventBus
	now      ports.Clock
}

// NewAccountService wires the account use cases to their output ports: the
// account repository, the transaction repository (for the delete boundary and
// cascade bound), the domain event bus, and a deterministic clock.
func NewAccountService(accounts AccountRepository, txns TransactionRepository, events ports.EventBus, now ports.Clock) *AccountService {
	return &AccountService{
		accounts: accounts,
		txns:     txns,
		events:   events,
		now:      now,
	}
}

// CreateAccount constructs and persists a new financial account via the domain
// aggregate, then publishes an append-only create audit event. The tenant and
// actor are derived server-side from the IAM context (never from the command) so
// every repository write carries the tenancy predicate.
func (s *AccountService) CreateAccount(ctx context.Context, cmd CreateAccountCommand, tenantID, actorID uuid.UUID) (*domain.Account, error) {
	if !cmd.Type.Valid() {
		return nil, domain.NewValidationError(domain.FieldError{Field: "type", Message: "type must be one of checking, savings, credit, cash, other"})
	}
	acc, err := domain.NewAccount(uuid.New(), tenantID, cmd.Name, cmd.Type, cmd.OpeningBalance, cmd.Currency)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrInvalidAccountType):
			return nil, domain.NewValidationError(domain.FieldError{Field: "type", Message: "type must be one of checking, savings, credit, cash, other"})
		case isDomainError(err):
			return nil, err
		default:
			return nil, domain.NewInternal(err)
		}
	}
	if drerr := s.accounts.Create(ctx, tenantID, &acc); drerr != nil {
		return nil, drerr
	}

	after, err := json.Marshal(acc)
	if err != nil {
		return nil, domain.NewInternal(err)
	}
	ev, err := s.newEvent(ctx, tenantID, actorID, domain.ActionCreate, "account", acc.ID.String(), nil, after)
	if err != nil {
		return nil, err
	}
	_ = s.events.Publish(ctx, ev)

	return &acc, nil
}

// UpdateAccount edits an account's name and/or type under the optimistic-lock
// version guard. A stale expected version (compare against the fresh load)
// surfaces CONFLICT_STATE before any write; the repository re-verifies the
// version atomically so the guard cannot be bypassed by a concurrent edit.
func (s *AccountService) UpdateAccount(ctx context.Context, cmd UpdateAccountCommand, tenantID, actorID uuid.UUID) (*domain.Account, error) {
	acc, drerr := s.accounts.GetByID(ctx, tenantID, cmd.ID)
	if drerr != nil {
		return nil, drerr
	}
	if acc.Version != cmd.ExpectedVersion {
		return nil, domain.NewConflict("concurrent modification: stale version")
	}

	copy := *acc
	changed := false
	if cmd.Name != nil {
		copy.Name = *cmd.Name
		changed = true
	}
	if cmd.Type != nil {
		copy.Type = *cmd.Type
		changed = true
	}
	if err := validateAccountFields(copy.Name, copy.Type); err != nil {
		return nil, err
	}
	if !changed {
		// Nothing to change by a partial update is a no-op conflict: refuse so
		// the caller can decide rather than silently success.
		return nil, domain.NewConflict("no account fields provided to update")
	}

	before, err := json.Marshal(acc)
	if err != nil {
		return nil, domain.NewInternal(err)
	}
	after, err := json.Marshal(copy)
	if err != nil {
		return nil, domain.NewInternal(err)
	}

	if drerr := s.accounts.Update(ctx, tenantID, &copy, cmd.ExpectedVersion); drerr != nil {
		return nil, drerr
	}

	ev, err := s.newEvent(ctx, tenantID, actorID, domain.ActionEdit, "account", copy.ID.String(), before, after)
	if err != nil {
		return nil, err
	}
	_ = s.events.Publish(ctx, ev)

	return &copy, nil
}

// DeleteAccount removes an account, enforcing the Module A boundary: an account
// with linked transactions cannot be deleted unless AllowCascade is explicitly
// set. The cascade is additionally bounded to accountMaxCascadeRows; beyond that
// it returns ErrCascadeTooLarge so the request path never runs a runaway inline
// delete. A second delete of an already-removed account returns NOT_FOUND
// (idempotent per §7.2).
func (s *AccountService) DeleteAccount(ctx context.Context, id, tenantID, actorID uuid.UUID, opts DeleteAccountOptions) error {
	acc, drerr := s.accounts.GetByID(ctx, tenantID, id)
	if drerr != nil {
		return drerr
	}

	exists, drerr := s.txns.ExistsForAccount(ctx, tenantID, id)
	if drerr != nil {
		return drerr
	}
	if exists && !opts.AllowCascade {
		return domain.Wrap(domain.CodeConflictState, ErrAccountHasTransactions)
	}
	if exists && opts.AllowCascade {
		count, drerr := s.txns.CountByAccount(ctx, tenantID, id)
		if drerr != nil {
			return drerr
		}
		if count > accountMaxCascadeRows {
			return domain.Wrap(domain.CodeConflictState, ErrCascadeTooLarge)
		}
	}

	before, err := json.Marshal(acc)
	if err != nil {
		return domain.NewInternal(err)
	}

	if drerr := s.accounts.Delete(ctx, tenantID, id, acc.Version, opts.AllowCascade); drerr != nil {
		return drerr
	}

	ev, err := s.newEvent(ctx, tenantID, actorID, domain.ActionDelete, "account", acc.ID.String(), before, nil)
	if err != nil {
		return err
	}
	_ = s.events.Publish(ctx, ev)

	return nil
}

// ListAccounts returns the offset-paginated, tenant-scoped account collection
// matching the optional filter. Page parameters are clamped so a hostile page or
// size cannot trigger an unbounded query. The result is wrapped in the Page
// envelope with a next-page link when more records follow.
func (s *AccountService) ListAccounts(ctx context.Context, filter AccountFilter, page PageParams, tenantID uuid.UUID) (Page[domain.Account], error) {
	off := page.OffsetPage()
	accounts, drerr := s.accounts.List(ctx, tenantID, filter, off)
	if drerr != nil {
		return Page[domain.Account]{}, drerr
	}
	result := Page[domain.Account]{
		Items:    make([]domain.Account, 0, len(accounts)),
		Page:     off.Page,
		PageSize: off.PageSize,
	}
	for _, a := range accounts {
		if a != nil {
			result.Items = append(result.Items, *a)
		}
	}
	if off.NextPage != nil {
		result.NextPage = off.NextPage
	}
	return result, nil
}

// ReconcileAccount verifies and, when drifted, repairs the invariant
// balance == opening_balance + SUM(transactions) atomically. Drift detection and
// repair happen inside a single repository transaction (the absolute
// compare-and-swap UPDATE guarded by expectedVersion and tenant_id), so no
// read-then-write TOCTOU window exists and double-spending/lost-updates cannot
// silently succeed. A repair is surfaced as an append-only edit audit event;
// when there is no drift no write (and no event) occurs, avoiding spurious cache
// invalidation.
func (s *AccountService) ReconcileAccount(ctx context.Context, id uuid.UUID, expectedVersion int64, tenantID, actorID uuid.UUID) (*domain.Account, error) {
	acc, drerr := s.accounts.GetByID(ctx, tenantID, id)
	if drerr != nil {
		return nil, drerr
	}
	if acc.Version != expectedVersion {
		return nil, domain.NewConflict("concurrent modification: stale version")
	}

	refreshed, drerr := s.accounts.Reconcile(ctx, tenantID, id, expectedVersion)
	if drerr != nil {
		return nil, drerr
	}

	// Emit an audit event only when the reconcile actually corrected a drifted
	// balance (version advanced) so consumers are not spammed on every call.
	if refreshed.Version > acc.Version {
		before, err := json.Marshal(acc)
		if err != nil {
			return nil, domain.NewInternal(err)
		}
		after, err := json.Marshal(refreshed)
		if err != nil {
			return nil, domain.NewInternal(err)
		}
		ev, err := s.newEvent(ctx, tenantID, actorID, domain.ActionEdit, "account", refreshed.ID.String(), before, after)
		if err != nil {
			return nil, err
		}
		_ = s.events.Publish(ctx, ev)
	}

	return refreshed, nil
}

// validateAccountFields enforces the account name/type allowlist at the use-case
// boundary, mirroring domain.NewAccount. It returns a structured
// VALIDATION_FAILED error listing every offending field.
func validateAccountFields(name string, typ domain.AccountType) *domain.DomainError {
	var fields []domain.FieldError
	if !typ.Valid() {
		fields = append(fields, domain.FieldError{Field: "type", Message: "type must be one of checking, savings, credit, cash, other"})
	}
	trimmed := strings.TrimSpace(name)
	switch {
	case trimmed == "":
		fields = append(fields, domain.FieldError{Field: "name", Message: "account name must not be empty"})
	case len([]rune(trimmed)) > accountMaxNameLen:
		fields = append(fields, domain.FieldError{Field: "name", Message: "account name exceeds 255 characters"})
	case hasInvalidNameRune(trimmed):
		fields = append(fields, domain.FieldError{Field: "name", Message: "account name contains invalid characters"})
	}
	if len(fields) == 0 {
		return nil
	}
	return domain.NewValidationError(fields...)
}

// hasInvalidNameRune reports whether name contains a control character or an
// injection-risky leading character (spreadsheet formula prefix), matching the
// soft-validation posture of the domain aggregates.
func hasInvalidNameRune(name string) bool {
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	switch name[0] {
	case '=', '+', '-', '@':
		return true
	default:
		return false
	}
}

// isDomainError reports whether err is already a *domain.DomainError so the
// caller can propagate it as-is instead of wrapping it as internal.
func isDomainError(err error) bool {
	var de *domain.DomainError
	return errors.As(err, &de)
}

// newEvent builds a validated domain.Event envelope carrying the create/edit/
// delete audit action. The before/after snapshots are minimal purpose-specific
// state (nil when absent, e.g. a create has nil Before, a delete has nil After).
// A placeholder positive sequence and a server-issued correlation id are
// supplied; the EventBus/outbox adapter assigns the authoritative per-aggregate
// monotonic sequence and carries tenant/actor context from the request context
// per the EventBus port contract.
func (s *AccountService) newEvent(ctx context.Context, tenantID, actorID uuid.UUID, action domain.AuditAction, aggregateType, aggregateID string, before, after any) (domain.Event, error) {
	rawBefore, err := marshalRaw(before)
	if err != nil {
		return domain.Event{}, domain.NewInternal(err)
	}
	rawAfter, err := marshalRaw(after)
	if err != nil {
		return domain.Event{}, domain.NewInternal(err)
	}
	payload, err := json.Marshal(domain.EntityModifiedPayload{
		EntityType: aggregateType,
		EntityID:   aggregateID,
		Action:     action,
		Before:     rawBefore,
		After:      rawAfter,
	})
	if err != nil {
		return domain.Event{}, domain.NewInternal(err)
	}
	ev := domain.Event{
		ID:             uuid.New(),
		Type:           domain.EventEntityModified,
		Version:        1,
		Timestamp:      s.now.Now(),
		TenantID:       tenantID,
		ActorID:        actorID,
		CorrelationID:  uuid.New().String(),
		SourceService:  "ledger",
		AggregateType:  aggregateType,
		AggregateID:    aggregateID,
		Payload:        payload,
		Sequence:       1,
	}
	if err := ev.Validate(); err != nil {
		return domain.Event{}, domain.NewInternal(err)
	}
	return ev, nil
}

// marshalRaw serializes v into a json.RawMessage, returning nil for a nil input
// so a nil snapshot marshals to a nil RawMessage rather than the JSON "null".
func marshalRaw(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}