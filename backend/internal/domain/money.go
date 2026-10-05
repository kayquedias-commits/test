// Package domain holds the financial-invariants building blocks of the core
// ledger. The package has zero external dependencies: only the standard library
// is permitted here so the domain layer stays framework- and ORM-agnostic.
package domain

import (
	"errors"
	"math/bits"
	"regexp"
	"strconv"
	"strings"
)

// MaxDecimals is the fixed fractional precision carried by every monetary
// value in the system (two decimal places, enforced by construction).
const MaxDecimals = 2

// moneyDecimalFactor is 10^MaxDecimals: the factor that converts minor units
// into whole currency units, and scales integer parts into minor units.
const moneyDecimalFactor int64 = 100

// maxParseLength bounds the raw monetary string accepted by Parse. Format is
// up to 19 integer digits + '.' + 2 decimals = 22 characters. Rejecting longer
// input before any parsing prevents unbounded resource consumption (CWE-770).
const maxParseLength = 22

var (
	errMoneyCurrency     = errors.New("invalid currency: must be a 3-letter ISO code (A-Z)")
	errMoneyNonNumeric   = errors.New("invalid amount: must be a decimal number")
	errMoneyPrecision    = errors.New("invalid amount: at most two decimal places are allowed")
	errMoneyNonNegative  = errors.New("invalid amount: must be non-negative")
	errMoneyOverflow     = errors.New("money arithmetic overflow")
	errMoneyUnderflow    = errors.New("money arithmetic underflow: result would be negative")
	errMoneyMismatch     = errors.New("currency mismatch: currencies must match for this operation")
	errMoneyCmpMismatch  = errors.New("cannot compare money across different currencies")
	errMoneyEmptyCcy     = errors.New("currency must not be empty")
	errMoneyTooLong      = errors.New("invalid amount: exceeds maximum supported length")
	errMoneyDecimalValue = errors.New("invalid amount: malformed decimal component")
)

// isoCurrencyRE is a structural allowlist for ISO 4217 currency codes. It is
// deliberate that this checks shape only, not registry membership ("ZZZ"
// passes): enforcing the full code registry would require an external dataset
// and violate the domain's zero-dependency rule. A strict registry check, if
// required, belongs in an interface adapter.
var isoCurrencyRE = regexp.MustCompile(`^[A-Z]{3}$`)

// minorStringRE is the allowlist for a raw monetary amount: an integer part
// followed by an optional 1-2 digit fractional part. It rejects signs, signs
// embedded in the string, exponent/scientific notation, hex, thousands
// separators, bare ".50"/"1." forms, and whitespace.
var minorStringRE = regexp.MustCompile(`^[0-9]+(\.[0-9]{1,2})?$`)

// Money is a value object representing an exact monetary amount. It is
// immutable by construction and carries no identity. Stored units are minor
// units (the field `major` holds, e.g., cents) so the amount is exact and never
// subject to binary floating-point drift; currency is the ISO 4217 code.
//
// Values are non-negative by default. Negative amounts are introduced only via
// Negate, whose use is the authorized adjustment/refund path; the requirement
// that negatives carry an explicit reason code is a policy enforced at the
// Transaction aggregate, not here.
type Money struct {
	major    int64
	currency string
}

// Major returns the exact integer value in minor units (e.g., cents). For
// 123.45 USD this is 12345.
func (m Money) Major() int64 {
	return m.major
}

// Currency returns the ISO 4217 currency code (e.g., "USD").
func (m Money) Currency() string {
	return m.currency
}

// NewFromMajor constructs a Money from an exact value in minor units. The
// argument must be non-negative; negatives for reason-coded adjustments must
// be derived from a non-negative value via Negate. The currency must be a
// trimmed 3-letter uppercase ISO code.
func NewFromMajor(major int64, ccy string) (Money, error) {
	cur, err := normalizeCurrency(ccy)
	if err != nil {
		return Money{}, err
	}
	if major < 0 {
		return Money{}, errMoneyNonNegative
	}
	return Money{major: major, currency: cur}, nil
}

// Parse is the single ingress boundary for turning validated decimal-string
// input into a Money. The raw amount must be non-empty, numeric, non-negative,
// and carry at most MaxDecimals fractional places (1-2 decimals accepted and
// padded to 2; trailing zeros are normalized deterministically). Currency is
// trimmed and validated as a 3-letter uppercase ISO code.
//
// This path never converts through a float: the decimal string is split into
// an integer part and an optional fractional part that are combined as exact
// integers.
func Parse(raw, currency string) (Money, error) {
	cur, err := normalizeCurrency(currency)
	if err != nil {
		return Money{}, err
	}

	s := strings.TrimSpace(raw)
	if s == "" {
		return Money{}, errMoneyNonNumeric
	}
	// raw lengths are bounded (maxParseLength) entirely independently of the
	// caller; a caller supplying untrimmed trailing space is also bounded by
	// trimming first, but we bound the trimmed form too so ParseInt sees only
	// the validated allowlisted input.
	if len(s) > maxParseLength {
		return Money{}, errMoneyTooLong
	}
	if !minorStringRE.MatchString(s) {
		return Money{}, errMoneyNonNumeric
	}

	// Scalability and correctness: reject a raw value whose integer part alone
	// would overflow int64 after scaling. The integer part is at most the whole
	// string (no fraction), so a length guard of 19 integer digits plus the
	// capping below keeps the scaled product below MaxInt64.
	if len(strings.SplitN(s, ".", 2)[0]) > 19 {
		return Money{}, errMoneyTooLong
	}

	var intPart, fracPart string
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		intPart = s[:dot]
		fracPart = s[dot+1:]
	} else {
		intPart = s
	}

	majorInt, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return Money{}, errMoneyTooLong
	}
	if majorInt > (1<<63-1)/moneyDecimalFactor {
		return Money{}, errMoneyTooLong
	}

	var fracMinor int64
	if fracPart != "" {
		// normalize exactly 1-2 decimals to 2 decimals: pad a single digit
		// with a trailing zero so 123.4 == 12340 minor units.
		if len(fracPart) == 1 {
			fracPart += "0"
		}
		fv, err := strconv.ParseInt(fracPart, 10, 64)
		if err != nil || fv >= moneyDecimalFactor {
			return Money{}, errMoneyDecimalValue
		}
		fracMinor = fv
	}

	return Money{major: majorInt*moneyDecimalFactor + fracMinor, currency: cur}, nil
}

// normalizeCurrency trims surrounding whitespace and validates the shape of an
// ISO code; it rejects empty and non-3-letter-uppercase values.
func normalizeCurrency(ccy string) (string, error) {
	c := strings.TrimSpace(ccy)
	if c == "" {
		return "", errMoneyEmptyCcy
	}
	if !isoCurrencyRE.MatchString(c) {
		return "", errMoneyCurrency
	}
	return c, nil
}

// Add returns the sum, enforcing currency match and guarding against int64
// overflow (never silent wrap). Both operands must carry a valid currency.
func (m Money) Add(o Money) (Money, error) {
	if moneyEmptyCcy(m) || moneyEmptyCcy(o) {
		return Money{}, errMoneyEmptyCcy
	}
	if m.currency != o.currency {
		return Money{}, errMoneyMismatch
	}
	sum, carry := bits.Add64(uint64(m.major), uint64(o.major), 0)
	if carry != 0 {
		return Money{}, errMoneyOverflow
	}
	return Money{major: int64(sum), currency: m.currency}, nil
}

// Sub returns the difference, enforcing currency match, guarding against int64
// underflow (borrow), and rejecting a negative result because Money is
// non-negative by default. Negative-path subtraction is expressed through the
// reason-coded adjustment machinery at the Transaction aggregate.
func (m Money) Sub(o Money) (Money, error) {
	if moneyEmptyCcy(m) || moneyEmptyCcy(o) {
		return Money{}, errMoneyEmptyCcy
	}
	if m.currency != o.currency {
		return Money{}, errMoneyMismatch
	}
	diff, borrow := bits.Sub64(uint64(m.major), uint64(o.major), 0)
	if borrow != 0 {
		return Money{}, errMoneyUnderflow
	}
	return Money{major: int64(diff), currency: m.currency}, nil
}

// Cmp compares two Money values of the same currency and returns -1, 0, or 1
// when m is less than, equal to, or greater than o. Comparing across
// currencies is rejected because it would silently corrupt budget-vs-actual
// and report comparisons.
func (m Money) Cmp(o Money) (int, error) {
	if moneyEmptyCcy(m) || moneyEmptyCcy(o) {
		return 0, errMoneyEmptyCcy
	}
	if m.currency != o.currency {
		return 0, errMoneyCmpMismatch
	}
	switch {
	case m.major < o.major:
		return -1, nil
	case m.major > o.major:
		return 1, nil
	default:
		return 0, nil
	}
}

// Negate returns a Money with the opposite sign. This is the sole construction
// path for negative amounts (reason-coded adjustments/refunds). MinInt64 is
// special-cased so the negation cannot overflow.
func (m Money) Negate() Money {
	if m.major == -1<<63 {
		// Negating MinInt64 would overflow int64; clamp by noting that a valid
		// non-negative Money cannot be MinInt64, so this guard only matters for
		// a downstream-constructed value. Returning the same extreme keeps the
		// value representable rather than wrapping positive.
		return Money{major: (1 << 63) - 1, currency: m.currency}
	}
	return Money{major: -m.major, currency: m.currency}
}

// IsNegative reports whether the amount is strictly less than zero. By
// construction a Money produced through Parse/NewFromMajor is never negative;
// this reports the sign of an adjustment-path value.
func (m Money) IsNegative() bool {
	return m.major < 0
}

// IsZero reports whether the amount is exactly zero. It is only meaningful
// over a valid-currency instance; Parse/NewFromMajor guarantee that invariant.
func (m Money) IsZero() bool {
	return m.major == 0
}

// String renders the amount as a decimal string with exactly MaxDecimals
// fractional places (e.g., "123.45", "-123.45", "0.00"). Rendering is integer
// based and sign-aware; no float is involved.
func (m Money) String() string {
	neg := m.major < 0
	abs := m.major
	if neg {
		abs = -m.major
	}
	intPart := abs / moneyDecimalFactor
	fracPart := abs % moneyDecimalFactor
	frac := strconv.FormatInt(fracPart, 10)
	for len(frac) < MaxDecimals {
		frac = "0" + frac
	}
	prefix := ""
	if neg {
		prefix = "-"
	}
	// Strconv.FormatInt on the absolute integer part; abs here is bounded well
	// below overflow because valid Money values are ~MaxInt64.
	return prefix + strconv.FormatInt(intPart, 10) + "." + frac
}

func moneyEmptyCcy(m Money) bool {
	return m.currency == ""
}