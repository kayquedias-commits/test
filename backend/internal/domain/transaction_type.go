// Package domain holds pure financial-domain types with zero external
// dependencies, satisfying the Clean Architecture dependency rule. This file
// defines the income/expense taxonomy invariant (TransactionType) used by
// ledger math, reporting, and budget actuals.
package domain

import (
	"errors"
	"strings"
)

// TransactionType identifies whether a ledger entry increases (income) or
// decreases (expense) an account balance. It is a value object: immutable,
// identityless, defined solely by its string value.
type TransactionType string

const (
	// Income represents a balance-increasing entry.
	Income TransactionType = "income"
	// Expense represents a balance-decreasing entry.
	Expense TransactionType = "expense"
)

// ErrInvalidTransactionType is returned when a string that is not the
// canonical "income" or "expense" value is supplied. It is a sentinel so
// adapters can map it to the API VALIDATION_FAILED contract and tests can
// assert on error identity rather than message text.
var ErrInvalidTransactionType = errors.New("domain: invalid transaction type")

// ParseTransactionType is the single ingress boundary for the transaction
// type. It trims whitespace, lowercases, and then exact-matches only the two
// canonical values. Unknown, empty, whitespace-only, and type-juggled inputs
// are rejected with ErrInvalidTransactionType; there is no fallback to a
// default type. This allowlist is what stops arbitrary strings from flipping
// the aggregation direction of a ledger entry.
func ParseTransactionType(s string) (TransactionType, error) {
	normalized := strings.ToLower(strings.TrimSpace(s))
	switch normalized {
	case "income":
		return Income, nil
	case "expense":
		return Expense, nil
	default:
		return "", ErrInvalidTransactionType
	}
}

// Valid reports whether t is one of the two canonical constants. It shares
// the exact normalization vocabulary of ParseTransactionType so that no value
// can pass Valid() that ParseTransactionType would reject, and vice versa.
func (t TransactionType) Valid() bool {
	switch string(t) {
	case "income", "expense":
		return true
	default:
		return false
	}
}

// Direction returns the aggregation sign for t: +1 for income, -1 for
// expense. It is used only for aggregation math (balance reconciliation,
// profit/loss, budget actuals) and must never be persisted or multiplied into
// a stored Money amount. The ledger stores amounts non-negative; sign is a
// derived, read-side concern.
func (t TransactionType) Direction() int {
	if t == Expense {
		return -1
	}
	return 1
}