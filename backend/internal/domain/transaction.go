// Package domain contains the core financial invariants of the system with
// zero external dependencies beyond the standard library and google/uuid.
package domain

import (
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// maxCategoryCount caps the number of categories a single transaction may
// reference, bounding memory and preventing crafted slice growth (CWE-770).
const maxCategoryCount = 32

var (
	ErrInvalidTransactionID               = errors.New("domain: invalid transaction id")
	ErrInvalidTenantID                    = errors.New("domain: invalid tenant id")
	ErrInvalidAccountID                   = errors.New("domain: invalid account id")
	ErrAtLeastOneCategory                 = errors.New("domain: transaction must reference at least one category")
	ErrTooManyCategories                  = errors.New("domain: too many categories on a transaction")
	ErrDuplicateCategory                  = errors.New("domain: duplicate category references are not allowed")
	ErrInvalidDescription                 = errors.New("domain: invalid description")
	ErrInvalidTransactionDate             = errors.New("domain: invalid transaction date")
	ErrFutureDateRequiresScheduled        = errors.New("domain: future-dated transactions require the scheduled flag")
	ErrNegativeAmountRequiresReasonCode   = errors.New("domain: negative amount requires a reason code")
	ErrReasonCodeNotAllowed               = errors.New("domain: a reason code is only allowed for negative amounts")
)

// Transaction is the central ledgable fact: it references exactly one account
// and at least one category. It is a non-root entity inside the Account
// aggregate; orchestration and event emission belong to the use cases.
type Transaction struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	AccountID   uuid.UUID
	CategoryIDs []uuid.UUID
	Amount      Money
	Date        time.Time
	Description string
	Type        TransactionType
	Scheduled   bool
	ReasonCode  *ReasonCode
	Version     int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// validateCategoryIDs enforces that a transaction references at least one,
// at most maxCategoryCount, and no duplicate category IDs.
func validateCategoryIDs(ids []uuid.UUID) error {
	if len(ids) < 1 {
		return ErrAtLeastOneCategory
	}
	if len(ids) > maxCategoryCount {
		return ErrTooManyCategories
	}
	seen := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			return ErrDuplicateCategory
		}
		seen[id] = struct{}{}
	}
	return nil
}

// validateDescription requires a non-empty description of at most 255 runes
// containing no control characters. Length is measured in decoded runes, not
// bytes, so multi-byte characters are not truncated mid-sequence.
func validateDescription(d string) error {
	if n := utf8.RuneCountInString(d); n < 1 || n > 255 {
		return ErrInvalidDescription
	}
	return strings.IndexFunc(d, func(r rune) bool {
		return r < 0x20 || r == 0x7F || r == unicode.ReplacementChar
	}) >= 0
}

// normalizeDateUTC returns the date truncated to midnight UTC after verifying
// it is a valid calendar date. Go's time.Time silently normalizes impossible
// dates (e.g. Feb 30 to Mar 2), so the year/month/day round trip is checked
// explicitly before truncating.
func normalizeDateUTC(t time.Time) (time.Time, error) {
	if t.IsZero() {
		return time.Time{}, ErrInvalidTransactionDate
	}
	y, m, d := t.Date()
	// Reconstruct from the components; if the original was out-of-range the
	// reconstructed components no longer match the input, revealing the
	// normalized date.
	rebuilt := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	ry, rm, rd := rebuilt.Date()
	if ry != y || rm != m || rd != d {
		return time.Time{}, ErrInvalidTransactionDate
	}
	return rebuilt, nil
}

// validateAmountReason enforces the reason-code coupling on Amount: a negative
// amount requires a ReasonCode, and a non-negative amount must not carry one.
// Amount precision is owned by Money; only positivity/reason coupling is
// checked here.
func validateAmountReason(amount Money, reason *ReasonCode) error {
	if amount.IsNegative() && reason == nil {
		return ErrNegativeAmountRequiresReasonCode
	}
	if !amount.IsNegative() && reason != nil {
		return ErrReasonCodeNotAllowed
	}
	return nil
}

// NewTransaction builds a Transaction after validating every ledger invariant:
// non-nil identifiers, a known type, at least one category, a non-empty
// ≤255-char description free of control characters, a calendar-valid date
// (normalized to midnight UTC) that requires the scheduled flag when
// future-dated, and the negative-amount/reason-code coupling.
func NewTransaction(id, tenantID, accountID uuid.UUID, categoryIDs []uuid.UUID, amount Money, date time.Time, description string, typ TransactionType, scheduled bool, reasonCode *ReasonCode) (Transaction, error) {
	if id == uuid.Nil {
		return Transaction{}, ErrInvalidTransactionID
	}
	if tenantID == uuid.Nil {
		return Transaction{}, ErrInvalidTenantID
	}
	if accountID == uuid.Nil {
		return Transaction{}, ErrInvalidAccountID
	}
	if !typ.Valid() {
		return Transaction{}, ErrInvalidTransactionType
	}
	if err := validateCategoryIDs(categoryIDs); err != nil {
		return Transaction{}, err
	}
	if err := validateDescription(description); err != nil {
		return Transaction{}, err
	}
	normalized, err := normalizeDateUTC(date)
	if err != nil {
		return Transaction{}, err
	}
	if normalized.After(time.Now().UTC()) && !scheduled {
		return Transaction{}, ErrFutureDateRequiresScheduled
	}
	if err := validateAmountReason(amount, reasonCode); err != nil {
		return Transaction{}, err
	}

	now := time.Now().UTC()
	return Transaction{
		ID:          id,
		TenantID:    tenantID,
		AccountID:   accountID,
		CategoryIDs: categoryIDs,
		Amount:      amount,
		Date:        normalized,
		Description: description,
		Type:        typ,
		Scheduled:   scheduled,
		ReasonCode:  reasonCode,
		Version:     0,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

// bump records a mutation: it increments the optimistic-lock version and
// refreshes UpdatedAt. The conditional version-guard itself is applied at the
// repository boundary, not in the entity.
func (t *Transaction) bump() {
	t.Version++
	t.UpdatedAt = time.Now().UTC()
}

// ApplyDescription replaces the description after re-running the full
// description validation, and records a mutation.
func (t *Transaction) ApplyDescription(description string) error {
	if err := validateDescription(description); err != nil {
		return err
	}
	t.Description = description
	t.bump()
	return nil
}

// ApplyAmount replaces the amount after re-checking the reason-code coupling,
// and records a mutation.
func (t *Transaction) ApplyAmount(amount Money) error {
	if err := validateAmountReason(amount, t.ReasonCode); err != nil {
		return err
	}
	t.Amount = amount
	t.bump()
	return nil
}

// ApplyDate replaces the date after re-validating calendar validity and the
// future-date/scheduled rule against the current scheduled flag, and records a
// mutation.
func (t *Transaction) ApplyDate(date time.Time) error {
	normalized, err := normalizeDateUTC(date)
	if err != nil {
		return err
	}
	if normalized.After(time.Now().UTC()) && !t.Scheduled {
		return ErrFutureDateRequiresScheduled
	}
	t.Date = normalized
	t.bump()
	return nil
}