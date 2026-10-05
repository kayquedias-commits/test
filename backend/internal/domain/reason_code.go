package domain

import "errors"

// ErrInvalidReasonCode is returned when a string is not the canonical
// "adjustment" or "refund" value. It is a sentinel so adapters can map it to
// the API VALIDATION_FAILED contract and tests can assert on error identity.
var ErrInvalidReasonCode = errors.New("domain: invalid reason code")

// ReasonCode documents the business justification for a negative ledger
// amount. Per §5.2, amounts may be negative only for adjustments or refunds
// with an explicit reason code. It is a value object: immutable, identityless,
// defined solely by its string value.
type ReasonCode string

const (
	// ReasonAdjustment covers corrections and manual adjustments to ledger
	// balances.
	ReasonAdjustment ReasonCode = "adjustment"
	// ReasonRefund covers money returned to an offsetting party.
	ReasonRefund ReasonCode = "refund"
)

// reasonCodeValid is the single source of truth for allowlist membership so
// ParseReasonCode and Valid never disagree.
func reasonCodeValid(r ReasonCode) bool {
	switch r {
	case ReasonAdjustment, ReasonRefund:
		return true
	default:
		// The zero value (empty string), case variants, whitespace-padded
		// values, and unknowns are all rejected.
		return false
	}
}

// ParseReasonCode parses an external string into a ReasonCode. It is a strict
// allowlist: only the exact canonical lowercase values "adjustment" and
// "refund" are accepted. Empty strings, whitespace-padded values, case
// variants (e.g. "ADJUSTMENT"), and unknown values all return
// ErrInvalidReasonCode — there is no case folding or trimming, because lenient
// parsing would defeat the audit-visible justification guarantee for negative
// postings.
func ParseReasonCode(s string) (ReasonCode, error) {
	r := ReasonCode(s)
	if !reasonCodeValid(r) {
		return "", ErrInvalidReasonCode
	}
	return r, nil
}

// Valid reports whether r is a supported reason code. Callers that receive a
// ReasonCode from sources other than ParseReasonCode (for example direct
// construction or deserialization) must invoke this before using the value.
// The zero value (empty string) is always invalid.
func (r ReasonCode) Valid() bool {
	return reasonCodeValid(r)
}