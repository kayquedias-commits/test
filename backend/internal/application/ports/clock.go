// Package ports defines the application-boundary seams that use cases depend
// on. This file declares the Clock port: deterministic time access that
// resolves the "today" business-date boundary according to a caller locale
// while always persisting in the single system timezone. It imports only the
// Go standard library, satisfying the Clean Architecture dependency rule.
package ports

import (
	"errors"
	"strings"
	"time"
)

// ErrInvalidLocale is returned when a locale string cannot be resolved to an
// IANA timezone. Callers use it to map to the API VALIDATION_FAILED contract.
var ErrInvalidLocale = errors.New("ports: invalid locale")

// Clock abstracts time so use cases, budget period logic, and transaction date
// validation can be deterministically tested. Production logic must depend on
// this port, never on SystemClock directly.
type Clock interface {
	// Now returns the current instant.
	Now() time.Time
	// Today returns the first instant of the caller's business day, resolved in
	// the given locale and converted to the system timezone for storage.
	Today(locale string) time.Time
}

// SystemClock is the stdlib-only production Clock. It is intentionally
// co-located with its port because it carries zero framework or storage
// dependency: it is a framework-free adapter, a deliberate exception to the
// otherwise infra-resident adapter rule. Loc is the single system timezone
// used for storage; a zero value behaves as UTC. SystemClock is stateless and
// safe for concurrent use.
type SystemClock struct {
	Loc time.Location
}

// Now returns the current instant in the system timezone.
func (c SystemClock) Now() time.Time {
	return time.Now().In(&c.Loc)
}

// Today returns the start of the caller's business day expressed in the system
// timezone (c.Loc). It resolves the locale to its IANA timezone to derive the
// calendar date, builds midnight on that date in the locale, then converts the
// absolute instant back to the system timezone. An empty or unresolvable
// locale falls back to the system timezone so this clock never panics or blocks
// the request path. The returned value is a wall-clock boundary already
// normalized for storage; callers must not re-apply locale conversion.
func (c SystemClock) Today(locale string) time.Time {
	loc := c.resolve(locale)
	now := c.Now().In(loc)
	y, m, d := now.Date()
	businessStart := time.Date(y, m, d, 0, 0, 0, 0, loc)
	return businessStart.In(&c.Loc)
}

// resolve returns the IANA timezone for locale, falling back to the system
// timezone when the locale is empty, not an IANA identifier, or not loadable.
// It trims whitespace and only hands values consisting of [A-Za-z0-9/_+-] to
// time.LoadLocation, which keeps invalid input from reaching the loader.
func (c SystemClock) resolve(locale string) *time.Location {
	normalized := strings.TrimSpace(locale)
	if !localeAllowlist(normalized) {
		return &c.Loc
	}
	tz, err := time.LoadLocation(normalized)
	if err != nil {
		return &c.Loc
	}
	return tz
}

// localeAllowlist permits only IANA-zone-safe characters so malformed or
// injected strings (path-like, whitespace-padded, control-character) never
// reach the tz loader.
func localeAllowlist(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '/' || r == '_' || r == '+' || r == '-':
		default:
			return false
		}
	}
	return true
}