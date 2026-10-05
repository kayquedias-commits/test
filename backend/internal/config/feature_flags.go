// Package config is the composition-root seam for the FMS. It holds the typed
// feature flags (Technical Specification §3.7/§4) that govern contested-scope
// behavior so clarified requirements can be absorbed without restructuring. The
// feature-flags value object here is parsed once at process startup from a
// trusted, immutable source (deployment environment) and injected into the use
// cases and interface adapters that consume it; it is never settable at runtime.
package config

import (
	"errors"
	"os"
	"strings"
)

// FeatureFlags is an immutable value object capturing which contested behaviors
// are enabled for this process. Every field defaults to false so the core ledger
// runs fully with conservative least-privilege behavior; no capability becomes
// active unless an operator deliberately flips the matching environment flag.
//
// AuditReadLoggingEnabled defaults to false pending contested-scope resolution
// (§9.4). It is a compliance-relevant default, not a pure feature toggle: while
// false, read access to financial data is unlogged. Modification logging
// (create/edit/delete) is unconditionally required and is never governed by this
// flag. The value is read once at startup and must be injected via constructor
// argument; consumers must never re-parse the environment per request.
type FeatureFlags struct {
	multiCurrencyEnabled    bool // accepts non-base currencies at the validation boundary (§9.5)
	auditReadLoggingEnabled bool // emits AuditAction READ entries from the read middleware (§9.4)
	notificationsEnabled    bool // binds the Notifier port adapter (§9.6)
	backdatedBudgetsEnabled bool // allows past-period budget creation (§5.3)
}

// MultiCurrencyEnabled reports whether non-base currencies are accepted at the
// service/validation boundary. When false, money validators reject any currency
// other than the configured base currency.
func (f FeatureFlags) MultiCurrencyEnabled() bool {
	return f.multiCurrencyEnabled
}

// AuditReadLoggingEnabled reports whether read access to financial data is
// audited. When false, the audit-read middleware is pass-through and emits no
// READ entries. It never affects create/edit/delete logging.
func (f FeatureFlags) AuditReadLoggingEnabled() bool {
	return f.auditReadLoggingEnabled
}

// NotificationsEnabled reports whether the Notifier port is bound to a delivery
// channel. When false, budget threshold events are still published but the port
// is a no-op adapter.
func (f FeatureFlags) NotificationsEnabled() bool {
	return f.notificationsEnabled
}

// BackdatedBudgetsEnabled reports whether budgets may be created for past
// periods. When false, the budget use case rejects any period whose end is in
// the past.
func (f FeatureFlags) BackdatedBudgetsEnabled() bool {
	return f.backdatedBudgetsEnabled
}

// Defaults returns a FeatureFlags with every behavior disabled — the conservative
// baseline under which the core ledger runs fully without any contested feature.
func Defaults() FeatureFlags {
	return FeatureFlags{}
}

// InvalidFlagError describes an environment flag whose value was not an accepted
// token. It carries the offending variable name and value so operators can fix
// exactly which flag is malformed.
type InvalidFlagError struct {
	Name  string
	Value string
}

// Error returns a human-readable description of the invalid env flag.
func (e InvalidFlagError) Error() string {
	return "invalid value for " + e.Name + ": " + e.Value + " (expected true|false)"
}

// FromEnv reads the FMS feature flags from the process environment once at
// startup. An unset variable yields its conservative default (false) without
// error; a set variable must be exactly "true" or "false" (case-insensitive,
// surrounding whitespace trimmed) and is parsed against a strict allowlist.
// Every invalid variable is aggregated into the single returned error so all
// misconfiguration is surfaced in one boot.
//
// strconv.ParseBool is deliberately not used: it permissively coerces values
// like "1", "t", and "TRUE". Silently enabling an audit- or money-critical
// behavior off a typo is exactly the failure mode this file prevents, so any
// token outside the allowlist fails loudly instead.
func FromEnv() (FeatureFlags, error) {
	var (
		flags FeatureFlags
		errs  []error
	)

	flags.multiCurrencyEnabled = parseFlag("FMS_MULTI_CURRENCY", &errs)
	flags.auditReadLoggingEnabled = parseFlag("FMS_AUDIT_READ_LOGGING", &errs)
	flags.notificationsEnabled = parseFlag("FMS_NOTIFICATIONS", &errs)
	flags.backdatedBudgetsEnabled = parseFlag("FMS_BACKDATED_BUDGETS", &errs)

	if len(errs) > 0 {
		return FeatureFlags{}, errors.Join(errs...)
	}
	return flags, nil
}

// parseFlag reads one boolean flag from the environment. An unset variable
// returns false; a set variable is allowlist-validated against true|false and
// appended to errs as an InvalidFlagError when it does not match.
func parseFlag(name string, errs *[]error) bool {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return false
	}

	token := strings.TrimSpace(raw)
	if strings.EqualFold(token, "true") {
		return true
	}
	if strings.EqualFold(token, "false") {
		return false
	}

	*errs = append(*errs, InvalidFlagError{Name: name, Value: raw})
	return false
}