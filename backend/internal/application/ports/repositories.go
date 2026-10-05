// Package ports declares the driven/output ports of the application layer.
//
// This file is the persistence seam (Blueprint §5.2, §5.1 Interface Adapters):
// it declares what persistence must provide to the use cases — never how. The
// repository interfaces here are the single boundary isolating any ORM/database
// driver; implementations live in internal/persistence/postgres. Use cases see
// only these contracts and the domain types they return; no SQL, driver error,
// or ORM object ever crosses this boundary.
//
// Dependency rule: this package imports the domain layer only (plus stdlib and
// google/uuid). It stays framework- and ORM-agnostic by construction.
package ports

import (
	"context"
	"time"

	"github.com/google/uuid"

	"fms/backend/internal/domain"
)

// Tenancy contract — apply to every method below:
//
//   - The explicit tenantID parameter is the enforcement point for tenant data
//     isolation (no cross-tenant leakage). It MUST be derived server-side from
//     the authenticated IAM principal's tenant claim, never from a request body,
//     query, or path value, and must never be overridable by a caller.
//   - Every implementation MUST scope its WHERE clauses by tenant_id in addition
//     to any id/username predicate (never fetch by primary key alone), and MUST
//     additionally enforce row-level security (RLS) as defense-in-depth so a
//     compromised repository bypass leaks nothing.
//   - Use cases must not accept tenantID from client input; the IAM context is
//     the only legitimate source.
//
// Error contract: every method returns a *domain.DomainError with a canonical
// code (NOT_FOUND, CONFLICT_STATE, VALIDATION_FAILED, DEPENDENCY_UNAVAILABLE).
// Implementations must normalize driver/ORM failures onto this contract and
// must never leak driver exception details, stack traces, or connection strings
// to callers.

// CursorPage is the cursor-based pagination contract for high-volume
// collections (transactions, audit logs). Cursor is an opaque, server-generated
// token (a signed/base64 composite of the keyset position, scoped to the tenant
// and filter so replaying a cursor under a different predicate is rejected);
// it is never a raw offset. Limit defaults to 100 and is hard-capped at 200.
// NextCursor is set only when a limit+1 probe row was fetched and discarded,
// indicating more pages follow.
type CursorPage struct {
	Limit      int
	Cursor     *string
	NextCursor *string
}

// OffsetPage is the offset-based pagination contract for bounded collections
// (accounts, categories, budgets, users, certifications, finance periods).
// PageSize is clamped to a maximum of 100; Page is 1-based. NextPage is set
// only when more records follow the current page.
type OffsetPage struct {
	Page     int
	PageSize int
	NextPage *int
}

// TransactionFilter is the optional-predicate set for listing transactions.
// Pointer fields are omitted from the WHERE clause when nil, composing a
// tenant-scoped predicate AND each present field. Implementations MUST use
// parameterized queries for every value (never string concatenation — SQL
// injection prevention), and MUST map sort keys through an allowlist. Ranges
// are half-open: [From, To). The implementation rejects From > To with
// INVALID_DATE_RANGE and MinAmount > MaxAmount with VALIDATION_FAILED.
type TransactionFilter struct {
	AccountID   *uuid.UUID
	CategoryID  *uuid.UUID
	From, To    *time.Time
	MinAmount   *domain.Money
	MaxAmount   *domain.Money
	Description *string
	Type        *domain.TransactionType
}

// AuditEntry is the append-only audit record written through AuditLogRepository.
// It lives in the ports layer (not the domain) because the domain exposes only
// the AuditAction value object; the persisted entry shape is a persistence
// seam, so defining it here keeps the ORM driver out of the domain.
//
// Integrity contract: the audit log is immutable by construction — AuditLog
// exposes no Update/Delete method. Append MUST persist the chained hash as
// Hash = H(PrevHash || payload || Timestamp); PrevHash is the hash of the
// immediately preceding entry in the tenant's chain, supplied by the audit
// consumer so a gap or reorder is detectable. BeforeValue/AfterValue are the
// serialized prior/new entity states for mutation actions.
type AuditEntry struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	UserID      uuid.UUID
	Action      domain.AuditAction
	EntityType  string
	EntityID    uuid.UUID
	BeforeValue []byte
	AfterValue  []byte
	Timestamp   time.Time
	PrevHash    string
	Hash        string
}

// AuditLogFilter is the optional-predicate set for paging audit entries.
type AuditLogFilter struct {
	EntityType *string
	EntityID   *uuid.UUID
	Action     *domain.AuditAction
}

// AccountRepository persists Account aggregates (the ledger's root). Listed
// methods accept an explicit tenant; Delete enforces the "no account with
// linked transactions" boundary atomically (see Delete).
type AccountRepository interface {
	// Create persists an account, seeding Balance equal to OpeningBalance in
	// the same INSERT transaction (balance invariant §5.3).
	Create(ctx context.Context, tenantID uuid.UUID, account *domain.Account) *domain.DomainError

	// GetByID returns one tenant-scoped account, loading its Version for
	// optimistic locking. Not-found yields NOT_FOUND.
	GetByID(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) (*domain.Account, *domain.DomainError)

	// List returns an offset-paginated, tenant-scoped collection.
	List(ctx context.Context, tenantID uuid.UUID, page OffsetPage) ([]*domain.Account, *domain.DomainError)

	// UpdateBalance applies a signed delta to an account's balance as an atomic
	// conditional UPDATE guarded by the expected version:
	//   UPDATE accounts SET balance = balance + $delta, version = version + 1
	//   WHERE id = $id AND tenant_id = $tenant AND version = $expected
	// It returns the new version; a stale expected version yields zero rows and
	// surfaces CONFLICT_STATE (double-spend/lost-update guard). The balance is
	// never read-modify-write.
	UpdateBalance(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, delta domain.Money, expectedVersion int64) (int64, *domain.DomainError)

	// Delete removes an account within one transaction, refusing (NOT_FOUND with
	// no mutation) when linked transactions exist. The in-transaction NOT EXISTS
	// guard closes the check-then-delete race between CountLinkedTransactions
	// and this delete.
	Delete(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, expectedVersion int64) *domain.DomainError

	// CountLinkedTransactions returns the count of non-deleted transactions
	// referencing the account — the delete-protection gate.
	CountLinkedTransactions(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) (int64, *domain.DomainError)

	// Reconcile recomputes balance == opening_balance + SUM(transactions) as a
	// single aggregate and surfaces CONFLICT_STATE on drift (never a per-row loop).
	Reconcile(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) *domain.DomainError
}

// TransactionRepository persists the central ledger fact. Balance integrity is
// guaranteed by the use case invoking AccountRepository.UpdateBalance in the
// same transaction boundary; these methods do not own transactions.
type TransactionRepository interface {
	// Insert persists a transaction row within the use-case transaction.
	Insert(ctx context.Context, tenantID uuid.UUID, tx *domain.Transaction) *domain.DomainError

	// GetByID returns one tenant-scoped transaction with account/category
	// populated. Not-found yields NOT_FOUND.
	GetByID(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) (*domain.Transaction, *domain.DomainError)

	// Update persists an edited transaction under the optimistic-lock guard;
	// a stale expected version yields CONFLICT_STATE.
	Update(ctx context.Context, tenantID uuid.UUID, tx *domain.Transaction, expectedVersion int64) *domain.DomainError

	// Delete soft-deletes a transaction under the optimistic-lock guard;
	// a stale expected version yields CONFLICT_STATE.
	Delete(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, expectedVersion int64) *domain.DomainError

	// List returns a cursor-paginated, tenant/filter-scoped collection that
	// eager-loads account and category via a single JOIN (no N+1) and applies
	// the soft-delete predicate. Returns the page tokens for the next request.
	List(ctx context.Context, tenantID uuid.UUID, filter TransactionFilter, page CursorPage) ([]*domain.Transaction, *CursorPage, *domain.DomainError)

	// CountByCategoryInRange aggregates expense (and income) spend grouped by
	// category over [from, to) in a single statement — feeds budget-actuals
	// recomputation and expense-report breakdowns.
	CountByCategoryInRange(ctx context.Context, tenantID uuid.UUID, categoryID uuid.UUID, from, to time.Time) (map[uuid.UUID]domain.Money, *domain.DomainError)
}

// CategoryRepository persists the Category taxonomy aggregate. Deletion of an
// in-use category is refused; merge is the lawful reassignment path.
type CategoryRepository interface {
	Create(ctx context.Context, tenantID uuid.UUID, c *domain.Category) *domain.DomainError

	GetByID(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) (*domain.Category, *domain.DomainError)

	// List returns the full tenant taxonomy (one level of subcategories) in a
	// single query, offset-paginated.
	List(ctx context.Context, tenantID uuid.UUID, page OffsetPage) ([]*domain.Category, *domain.DomainError)

	// Update persists a renamed/re-parented category under the optimistic-lock
	// guard; a stale expected version yields CONFLICT_STATE.
	Update(ctx context.Context, tenantID uuid.UUID, c *domain.Category, expectedVersion int64) *domain.DomainError

	// Merge atomically re-points a source category's transactions and child
	// categories onto a target and deletes the source in one transaction,
	// preserving reporting integrity. Returns the plan for the use case to emit
	// the propagation event.
	Merge(ctx context.Context, tenantID uuid.UUID, sourceID, targetID uuid.UUID) (*domain.CategoryMergePlan, *domain.DomainError)

	// SetInUse maintains the in-use deletion-protection flag, set true on first
	// transaction reference and released on merge.
	SetInUse(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, inUse bool) *domain.DomainError

	// Delete refuses deletion of an in-use category; the implementation
	// re-verifies InUse atomically with the delete (plus ON DELETE RESTRICT
	// backstop) to close the check-then-delete race.
	Delete(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, expectedVersion int64) *domain.DomainError
}

// BudgetRepository persists Budget aggregates (period + category allocation)
// and materialized spend actuals.
type BudgetRepository interface {
	Create(ctx context.Context, tenantID uuid.UUID, b *domain.Budget) *domain.DomainError

	GetByID(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) (*domain.Budget, *domain.DomainError)

	List(ctx context.Context, tenantID uuid.UUID, page OffsetPage) ([]*domain.Budget, *domain.DomainError)

	// Update persists a budget under the optimistic-lock guard.
	Update(ctx context.Context, tenantID uuid.UUID, b *domain.Budget, expectedVersion int64) *domain.DomainError

	Delete(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, expectedVersion int64) *domain.DomainError

	// ActualsByCategory aggregates expense spend grouped by category over
	// [from, to) in a single statement — the budget-vs-actual aggregation feed.
	ActualsByCategory(ctx context.Context, tenantID uuid.UUID, from, to time.Time) (map[uuid.UUID]domain.Money, *domain.DomainError)
}

// UserRepository persists the Identity & Access aggregate. Secret contract:
// returned domain.User objects carry PasswordHash and MFASecret as opaque
// references that MUST never be serialized into API responses or domain events.
type UserRepository interface {
	Create(ctx context.Context, tenantID uuid.UUID, u *domain.User) *domain.DomainError

	GetByID(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) (*domain.User, *domain.DomainError)

	// GetByUsername performs a tenant-scoped lookup for authentication. The
	// implementation should use a uniform/constant-time look-up shape so absent
	// vs. wrong-credential is not distinguishably timed (enumeration mitigation).
	GetByUsername(ctx context.Context, tenantID uuid.UUID, username string) (*domain.User, *domain.DomainError)

	List(ctx context.Context, tenantID uuid.UUID, page OffsetPage) ([]*domain.User, *domain.DomainError)

	// Update persists an edited user under the optimistic-lock guard.
	Update(ctx context.Context, tenantID uuid.UUID, u *domain.User, expectedVersion int64) *domain.DomainError

	// CountAdmins counts active admin users per tenant — feeds the last-admin
	// invariant; must be executed inside the same transaction boundary as the
	// subsequent delete so the count cannot go stale.
	CountAdmins(ctx context.Context, tenantID uuid.UUID) (int64, *domain.DomainError)

	Delete(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, expectedVersion int64) *domain.DomainError
}

// AuditLogRepository is the append-only audit seam. It exposes Append and List
// only — there is deliberately no Update or Delete method, making audit
// immutability a compile-time invariant rather than a runtime convention.
type AuditLogRepository interface {
	// Append persists an audit entry with its chained hash
	// (Hash = H(PrevHash || payload || Timestamp)), tenant-scoped.
	Append(ctx context.Context, tenantID uuid.UUID, entry AuditEntry) *domain.DomainError

	// List returns a cursor-paginated, tenant/filter-scoped slice from the
	// append-only store.
	List(ctx context.Context, tenantID uuid.UUID, filter AuditLogFilter, page CursorPage) ([]AuditEntry, *CursorPage, *domain.DomainError)
}

// ReadModelRepository is the CQRS read-side port (§3.5). Consumers refresh
// projections after the write transaction commits; they never write the
// authoritative ledger. Refreshes are idempotent by aggregate identity and
// checkpointed so the event relay can resume in order after a failure.
type ReadModelRepository interface {
	// RefreshAccountBalances recomputes the account-balance projection for a
	// tenant from the ledger in a single pass.
	RefreshAccountBalances(ctx context.Context, tenantID uuid.UUID) *domain.DomainError

	// RefreshBudgetActuals recomputes the spend-actuals projection for one
	// category within a period; idempotent by (categoryID, range).
	RefreshBudgetActuals(ctx context.Context, tenantID uuid.UUID, categoryID uuid.UUID, rng domain.DateRange) *domain.DomainError
}

// CertificationRepository persists access-certification reviews (append-only)
// and orphans, powering the deprovisioning workflow.
type CertificationRepository interface {
	// Append persists a finalized, tenant-scoped certification; re-append of an
	// already-recorded certification yields CONFLICT_STATE.
	Append(ctx context.Context, tenantID uuid.UUID, c *domain.AccessCertification) *domain.DomainError

	List(ctx context.Context, tenantID uuid.UUID, page OffsetPage) ([]*domain.AccessCertification, *domain.DomainError)

	// ListOrphaned returns tenant users with no active role/membership —
	// deprovisioning evidence.
	ListOrphaned(ctx context.Context, tenantID uuid.UUID, page OffsetPage) ([]*domain.User, *domain.DomainError)
}

// FinancePeriodRepository persists the period-end close state machine
// (open -> closing -> closed). Transition methods are atomic conditional
// updates guarded by expected version so a concurrent close cannot slip through.
type FinancePeriodRepository interface {
	Get(ctx context.Context, tenantID uuid.UUID, id uuid.UUID) (*domain.FinancePeriod, *domain.DomainError)

	List(ctx context.Context, tenantID uuid.UUID, page OffsetPage) ([]*domain.FinancePeriod, *domain.DomainError)

	Create(ctx context.Context, tenantID uuid.UUID, p *domain.FinancePeriod) *domain.DomainError

	// BeginClose atomically transitions open -> closing
	// (WHERE status='open' AND version=$expected); a period already closing or
	// closed yields CONFLICT_STATE.
	BeginClose(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, expectedVersion int64) *domain.DomainError

	// CompleteClose atomically transitions closing -> closed, records the
	// closing actor, and materializes the immutable reporting snapshot.
	CompleteClose(ctx context.Context, tenantID uuid.UUID, id uuid.UUID, actorID uuid.UUID, expectedVersion int64) *domain.DomainError

	// IsClosed reports whether the period containing date is locked; ledger
	// writes are rejected with PERIOD_CLOSED when true.
	IsClosed(ctx context.Context, tenantID uuid.UUID, date time.Time) (bool, *domain.DomainError)
}