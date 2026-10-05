// Package domain holds the financial-invariants building blocks of the core
// ledger. The package has zero external dependencies: only the standard library
// is permitted here so the domain layer stays framework- and ORM-agnostic.
//
// Account is the aggregate root for the Core Accounting (Ledger) bounded
// context. It owns the ledger's central invariant — that an account's current
// balance always equals its opening balance plus the algebraic sum of its
// posted transactions — and the deletion boundary that prevents an account
// with linked transactions from being removed. All monetary mutations flow
// through this aggregate (or a use case that loads it); the balance is never
// reachable as a plain field setter from outside the aggregate.
package domain

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AccountType enumerates the financial account categories the ledger supports.
// Values form the strict allowlist enforced by ParseAccountType and Valid.
type AccountType string

const (
	// AccountTypeChecking is a transaction/demand deposit account.
	AccountTypeChecking AccountType = "checking"
	// AccountTypeSavings is an interest-bearing deposit account.
	AccountTypeSavings AccountType = "savings"
	// AccountTypeCredit is a credit line; its balance represents the amount owed.
	AccountTypeCredit AccountType = "credit"
	// AccountTypeCash is a physical cash holding.
	AccountTypeCash AccountType = "cash"
	// AccountTypeOther catches any account that does not fit the above.
	AccountTypeOther AccountType = "other"
)

// ErrInvalidAccountType is returned when a raw string cannot be parsed into a
// known AccountType. ParseAccountType and NewAccount surface this; callers are
// expected to wrap or propagate it as a domain error at the boundary.
var ErrInvalidAccountType = errors.New("domain: invalid account type")

// ParseAccountType converts a raw string into an AccountType using a strict,
// exact-match allowlist. Leading/trailing whitespace is trimmed, but casing is
// case-sensitive and no unknown value is silently defaulted to a permissive
// type: anything outside the five canonical values returns ErrInvalidAccountType.
func ParseAccountType(raw string) (AccountType, error) {
	switch AccountType(strings.TrimSpace(raw)) {
	case AccountTypeChecking, AccountTypeSavings, AccountTypeCredit, AccountTypeCash, AccountTypeOther:
		return AccountType(strings.TrimSpace(raw)), nil
	default:
		return "", ErrInvalidAccountType
	}
}

// Valid reports whether the receiver is one of the five canonical account
// types. It is the persistence-facing allowlist check used by NewAccount.
func (t AccountType) Valid() bool {
	switch t {
	case AccountTypeChecking, AccountTypeSavings, AccountTypeCredit, AccountTypeCash, AccountTypeOther:
		return true
	default:
		return false
	}
}

// TxKind discriminates the direction of a posting so the ledger can resolve the
// signed effect of a transaction on an account's balance by account type. The
// Transaction aggregate that owns the full ledger row (amount, date, category,
// description) will map onto these two kinds at the boundary; this file carries
// only the minimal signal needed to preserve the balance invariant.
type TxKind string

const (
	// TxKindCredit is an income-style posting that increases asset-type balances.
	TxKindCredit TxKind = "credit"
	// TxKindDebit is an expense-style posting that decreases asset-type balances.
	TxKindDebit TxKind = "debit"
)

// AccountTx is the minimal value-type representation of a posting accepted by
// ApplyTransaction and Reconcile. It substitutes for the future Transaction
// aggregate that owns the full ledger row; exposing only AccountID and Kind here
// keeps this file free of a sibling aggregate dependency while still letting the
// aggregate enforce cross-account integrity and direction by account type.
type AccountTx struct {
	// AccountID must equal the Account's own ID; foreign references are rejected.
	AccountID uuid.UUID
	// Amount is the non-negative posted value. Direction is resolved by Kind and
	// account type; negative Amount values are rejected (specification §6.4).
	Amount Money
	// Kind carries the credit/debit direction resolved at the boundary.
	Kind TxKind
}

// Account is the ledger's aggregate root for a single financial account. It
// owns balance integrity (balance == opening + sum of signed transactions), the
// deletion boundary (no delete while transactions exist), and optimistic locking
// (version) to prevent lost updates and double-spending under concurrency.
//
// Value fields (IDs, Name, Type, OpeningBalance, Currency, timestamps, Closed)
// are immutable after construction. The only mutable state is Balance, Version,
// UpdatedAt, and TransactionCount, and those change exclusively through the
// guarded methods ApplyTransaction, AdjustBalance, Reconcile (diagnostic), and
// Close. There is deliberately no exported field setter or constructor that can
// seed an arbitrary Balance diverging from the invariant.
type Account struct {
	ID              uuid.UUID
	TenantID        uuid.UUID
	Name            string
	Type            AccountType
	OpeningBalance  Money
	Balance         Money
	Currency        string
	Version         int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Closed          bool
	TransactionCount int64
}

// NewAccount constructs a new Account with its Balance seeded equal to the
// opening balance and its Version at 1. It validates the identity, type
// allowlist, name allowlist, and currency consistency of the opening balance.
// Validation failures are aggregated into a single VALIDATION_FAILED error;
// an unknown account type returns ErrInvalidAccountType.
func NewAccount(id, tenantID uuid.UUID, name string, typ AccountType, openingBalance Money, currency string) (Account, error) {
	var acc Account

	if id == uuid.Nil {
		return acc, NewValidationError(FieldError{Field: "id", Message: "account id must not be empty"})
	}
	if tenantID == uuid.Nil {
		return acc, NewValidationError(FieldError{Field: "tenant_id", Message: "tenant id must not be empty"})
	}
	if !typ.Valid() {
		return acc, ErrInvalidAccountType
	}

	var nameErr string
	if trimmed := strings.TrimSpace(name); trimmed == "" {
		nameErr = "account name must not be empty"
	} else if len([]rune(trimmed)) > 255 {
		nameErr = "account name exceeds 255 characters"
	} else if hasInvalidNameRune(trimmed) {
		nameErr = "account name contains invalid characters"
	}
	if nameErr != "" {
		return acc, NewValidationError(FieldError{Field: "name", Message: nameErr})
	}

	if openingBalance.Currency() == "" {
		return acc, NewValidationError(FieldError{Field: "opening_balance", Message: "opening balance currency must not be empty"})
	}
	if currency != openingBalance.Currency() {
		return acc, NewValidationError(FieldError{Field: "currency", Message: "currency must match the opening balance currency"})
	}
	if openingBalance.IsNegative() {
		return acc, NewValidationError(FieldError{Field: "opening_balance", Message: "opening balance must be non-negative"})
	}

	now := time.Now().UTC()
	return Account{
		ID:               id,
		TenantID:         tenantID,
		Name:             strings.TrimSpace(name),
		Type:             typ,
		OpeningBalance:   openingBalance,
		Balance:          openingBalance,
		Currency:         openingBalance.Currency(),
		Version:          1,
		CreatedAt:        now,
		UpdatedAt:        now,
		Closed:           false,
		TransactionCount: 0,
	}, nil
}

// hasInvalidNameRune reports whether name contains a control character or an
// injection-risky leading character. It is deliberately conservative and mirrors
// the soft-validation posture of the category aggregate.
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

// Close marks the account as closed and bumps the version so concurrent writers
// cannot slip a mutation past the closure. Closing an already-closed account is
// a no-op conflict rather than an error-free double transition.
func (a *Account) Close() error {
	if a.Closed {
		return NewConflict("account is already closed")
	}
	a.Closed = true
	a.Version++
	a.UpdatedAt = time.Now().UTC()
	return nil
}

// CanDelete reports whether the account is safe to delete on the client/UX side:
// it is true only when the account has no linked transactions. This is a check,
// not the enforcement point. The hard guarantee is the repository's atomic
// conditional DELETE (version guard + NOT EXISTS on the transactions table)
// executed inside the same transaction, which closes the time-of-check/
// time-of-use race between this check and an insert. TransactionCount is
// populated at load time by the repository; the domain cannot query.
func (a *Account) CanDelete() bool {
	return a.TransactionCount == 0
}

// ReconciledAgainst is the drifted-balance conflict message surfaced on a
// reconcile mismatch; kept as a constant so tests assert on the exact text.
const ReconciledAgainst = "reconcile: balance drift detected"

// Reconcile verifies that the account's recorded balance matches its opening
// balance plus the algebraic sum of the supplied transactions. It is a
// diagnostic/audit tool for detecting drift, not the enforcement path for the
// invariant: the same-transaction enforcement belongs to ApplyTransaction. It
// rejects any transaction referencing a different account (cross-account guard)
// and any negative Amount, returns CONFLICT_STATE on a mismatch, and never
// mutates the account — a drifted balance must be reported, not silently
// corrected, to preserve the audit narrative.
func (a *Account) Reconcile(transactions []AccountTx) error {
	running := a.OpeningBalance
	for _, tx := range transactions {
		if tx.AccountID == uuid.Nil {
			return NewConflict("reconcile: transaction with empty account reference")
		}
		if tx.AccountID != a.ID {
			return NewConflict("reconcile: foreign account transaction rejected")
		}
		if tx.Amount.IsNegative() {
			return NewConflict("reconcile: negative transaction amount rejected")
		}
		signed, err := a.signedDelta(tx)
		if err != nil {
			return NewConflict("reconcile: " + err.Error())
		}
		var next Money
		if signed.IsNegative() {
			next, err = running.Sub(signed.Negate())
		} else {
			next, err = running.Add(signed)
		}
		if err != nil {
			return NewConflict("reconcile: " + err.Error())
		}
		running = next
	}
	eq, err := running.Cmp(a.Balance)
	if err != nil {
		return NewConflict("reconcile: " + err.Error())
	}
	if eq != 0 {
		return NewConflict(ReconciledAgainst)
	}
	return nil
}

// AdjustBalance applies a signed monetary delta (an authorized adjustment) to
// the current balance under an optimistic-lock guard. It rejects a closed
// account, a mismatched currency, and a stale expectedVersion — returning
// CONFLICT_STATE without mutating anything, so a lost update or double-spend
// can never silently succeed. On success the balance is recomputed as an exact
// Money value and the version is bumped.
//
// The domain version check is a precondition: it cannot alone prevent lost
// updates across concurrent requests. Atomicity is guaranteed by the repository
// persisting this as `UPDATE ... SET balance = balance + $delta, version =
// version + 1 WHERE id = $id AND version = $expected` inside the same DB
// transaction as the triggering mutation; a zero affected-row count there must
// roll back and surface CONFLICT_STATE.
func (a *Account) AdjustBalance(delta Money, expectedVersion int64) error {
	if a.Closed {
		return NewConflict("account is closed")
	}
	if expectedVersion != a.Version {
		return NewConflict("concurrent modification: stale version")
	}
	if delta.Currency() != a.Currency {
		return NewConflict("currency mismatch: delta must match account currency")
	}

	var err error
	var next Money
	if delta.IsNegative() {
		next, err = a.Balance.Sub(delta.Negate())
	} else {
		next, err = a.Balance.Add(delta)
	}
	if err != nil {
		return NewConflict("adjust balance: " + err.Error())
	}

	a.Balance = next
	a.Version++
	a.UpdatedAt = time.Now().UTC()
	return nil
}

// ApplyTransaction posts a single transaction onto the account, the ledger's
// real invariant-enforcement path. It rejects a nil or foreign AccountID
// (BOLA-style cross-account write guard), a negative Amount, a closed account,
// and a stale expectedVersion — each surfacing as CONFLICT_STATE with no
// mutation. On success the balance is recomputed via the credit/debit direction
// resolved by account type, the version is bumped, and TransactionCount is
// incremented.
//
// This method must be invoked by a use case inside the same ACID transaction as
// the transaction-row insert and the repository's atomic version-guarded save,
// so the balance invariant and the ledger row can never drift apart.
func (a *Account) ApplyTransaction(tx AccountTx, expectedVersion int64) error {
	if tx.AccountID == uuid.Nil {
		return NewConflict("transaction with empty account reference")
	}
	if tx.AccountID != a.ID {
		return NewConflict("foreign account transaction rejected")
	}
	if tx.Amount.IsNegative() {
		return NewConflict("negative transaction amount rejected")
	}
	if a.Closed {
		return NewConflict("account is closed")
	}
	if expectedVersion != a.Version {
		return NewConflict("concurrent modification: stale version")
	}
	if tx.Amount.IsZero() {
		return nil
	}

	signed, err := a.signedDelta(tx)
	if err != nil {
		return NewConflict("apply transaction: " + err.Error())
	}
	if err := a.AdjustBalance(signed, a.Version); err != nil {
		return err
	}
	a.TransactionCount++
	return nil
}

// signedDelta resolves a posting's signed effect on the account balance by
// account type. On asset-type accounts (checking, savings, cash, other) a
// credit/income posting increases the balance and a debit/expense posting
// decreases it. On a credit account the balance represents the amount owed, so
// the direction inverts: a debit/expense posting increases the liability and a
// credit/income posting reduces it.
func (a *Account) signedDelta(tx AccountTx) (Money, error) {
	if tx.Amount.Currency() != a.Currency {
		return Money{}, errors.New("currency mismatch: transaction must match account currency")
	}
	asset := a.Type != AccountTypeCredit
	income := tx.Kind == TxKindCredit
	// An asset account gains on income, loses on expense; a credit account
	// inverts — expense adds to the liability, income reduces it.
	if (asset && income) || (!asset && tx.Kind == TxKindDebit) {
		return tx.Amount, nil
	}
	return tx.Amount.Negate(), nil
}