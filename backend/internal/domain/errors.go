// Package domain defines the core business types and error vocabulary for the
// financial management system. It has zero external dependencies.
//
// The typed error contract documented here is the single authoritative source of
// error codes for the whole system and maps 1:1 onto the RFC 7807-style API
// error envelope (specification §7.7):
//
//	{
//	  "error": {
//	    "code": "VALIDATION_FAILED",
//	    "message": "...",
//	    "field_errors": [{"field": "amount", "message": "..."}],
//	    "correlation_id": "req_1234abc"
//	  }
//	}
//
// Running a financial system: this file aggregates multiple invalid fields in a
// single validation error, threads a correlation id for tracing, and never
// exposes stack traces or internal cause text to callers.
package domain

import (
	"errors"
	"fmt"
	"strings"
)

// ErrorCode identifies the category of failure a DomainError represents. The
// constants here are the only values that may appear in the "code" field of an
// API error envelope; they double as an allowlist against arbitrary strings
// entering the contract.
type ErrorCode string

const (
	// CodeValidationFailed signals an input schema violation; carries FieldErrors.
	CodeValidationFailed ErrorCode = "VALIDATION_FAILED"
	// CodeUnauthenticated signals a missing or expired credential.
	CodeUnauthenticated ErrorCode = "UNAUTHENTICATED"
	// CodeForbidden signals an RBAC denial at a resource boundary.
	CodeForbidden ErrorCode = "FORBIDDEN"
	// CodeNotFound signals that a requested resource is absent.
	CodeNotFound ErrorCode = "NOT_FOUND"
	// CodeConflictState signals an optimistic-lock version mismatch.
	CodeConflictState ErrorCode = "CONFLICT_STATE"
	// CodeRateLimited signals that the client exceeded a rate limit.
	CodeRateLimited ErrorCode = "RATE_LIMITED"
	// CodeDependencyUnavailable signals a downstream port failure.
	CodeDependencyUnavailable ErrorCode = "DEPENDENCY_UNAVAILABLE"
	// CodeInternalError signals an unclassified failure; Cause is log-only.
	CodeInternalError ErrorCode = "INTERNAL_ERROR"
)

// FieldError binds a single offending request field to its validation message.
// Field holds a JSON path into the request schema (e.g. "amount", "transactions[2].date")
// so clients can bind errors to inputs without a translation layer.
type FieldError struct {
	Field   string
	Message string
}

// DomainError is a structured domain error carrying an RFC 7807 error code, a
// user-safe message, aggregated field errors, and tracing context.
//
// Integrity rules: FieldErrors must be nil for every code except
// CodeValidationFailed; Cause holds the internal wrapped error and is reachable
// only via Unwrap/Cause for server-side logging — it must never be serialized
// into a response or interpolated into Message; CorrelationID is a trace carrier
// populated by the interface adapter, never by domain constructors; RetryAfter
// carries the Retry-After seconds emitted on 429/503 responses.
type DomainError struct {
	Code          ErrorCode
	Message       string
	FieldErrors   []FieldError
	Cause         error
	CorrelationID string
	RetryAfter    int
}

// Error returns a compact, sanitized representation containing only the code and
// message. It never includes the cause chain or stack information, so it is safe
// for client-facing output.
func (e *DomainError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Message
}

// Unwrap returns the wrapped cause so errors.Is/errors.As traverse the chain and
// the logging layer can reach the root cause for structured diagnosis.
func (e *DomainError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// HTTPStatus maps the error code to its canonical HTTP status per specification
// §7.7. This is the single place the code-to-status translation is defined; the
// mapping is centralized here so adapter layers never drift. The default of 500
// is defensive and unreachable for valid codes.
func (e *DomainError) HTTPStatus() int {
	if e == nil {
		return 500
	}
	switch e.Code {
	case CodeValidationFailed:
		return 400
	case CodeUnauthenticated:
		return 401
	case CodeForbidden:
		return 403
	case CodeNotFound:
		return 404
	case CodeConflictState:
		return 409
	case CodeRateLimited:
		return 429
	case CodeDependencyUnavailable:
		return 503
	case CodeInternalError:
		return 500
	default:
		return 500
	}
}

// sanitize strips control characters that could be used for log/response or
// header injection (CR, LF, NUL). All user-influenced text passed into error
// messages is treated as untrusted and neutralized here.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', 0:
			return -1
		default:
			return r
		}
	}, s)
}

// NewValidationError builds a VALIDATION_FAILED error aggregating one or more
// field errors in the order supplied. Calling it with no fields yields the code
// with nil FieldErrors rather than an empty slice.
func NewValidationError(fields ...FieldError) *DomainError {
	var clean []FieldError
	for _, f := range fields {
		clean = append(clean, FieldError{
			Field:   sanitize(f.Field),
			Message: sanitize(f.Message),
		})
	}
	return &DomainError{
		Code:        CodeValidationFailed,
		Message:     "one or more fields are invalid",
		FieldErrors: clean,
	}
}

// NewNotFound builds a NOT_FOUND error for a missing resource.
func NewNotFound(resource, id string) *DomainError {
	msg := fmt.Sprintf("%s not found", sanitize(resource))
	if id != "" {
		msg = fmt.Sprintf("%s %s not found", sanitize(resource), sanitize(id))
	}
	return &DomainError{
		Code:    CodeNotFound,
		Message: sanitize(msg),
	}
}

// NewConflict builds a CONFLICT_STATE error, typically an optimistic-lock
// version mismatch on a concurrent edit.
func NewConflict(msg string) *DomainError {
	return &DomainError{
		Code:    CodeConflictState,
		Message: sanitize(msg),
	}
}

// NewForbidden builds a FORBIDDEN error for an RBAC denial. The message stays
// generic and never explains why access was denied, preventing existence probing.
func NewForbidden(msg string) *DomainError {
	return &DomainError{
		Code:    CodeForbidden,
		Message: sanitize(msg),
	}
}

// NewUnauthenticated builds a UNAUTHENTICATED error for a missing or expired
// credential.
func NewUnauthenticated(msg string) *DomainError {
	return &DomainError{
		Code:    CodeUnauthenticated,
		Message: sanitize(msg),
	}
}

// NewRateLimited builds a RATE_LIMITED error carrying the Retry-After seconds
// for the HTTP adapter to emit. Negative durations are clamped to zero.
func NewRateLimited(retryAfter int) *DomainError {
	if retryAfter < 0 {
		retryAfter = 0
	}
	return &DomainError{
		Code:       CodeRateLimited,
		Message:    "too many requests; retry after the indicated duration",
		RetryAfter: retryAfter,
	}
}

// NewDependencyUnavailable builds a DEPENDENCY_UNAVAILABLE error signalling a
// failed downstream service; triggers Retry-After semantics at the boundary.
func NewDependencyUnavailable(msg string) *DomainError {
	return &DomainError{
		Code:    CodeDependencyUnavailable,
		Message: sanitize(msg),
	}
}

// NewInternal builds an INTERNAL_ERROR error. Message is a fixed generic string
// so internal state never leaks to callers; Cause retains the original error for
// server-side logging only and must never be serialized into a response.
func NewInternal(cause error) *DomainError {
	if cause == nil {
		cause = errors.New("unknown internal failure")
	}
	return &DomainError{
		Code:    CodeInternalError,
		Message: "internal error",
		Cause:   cause,
	}
}

// Wrap builds a DomainError of the given code wrapping the supplied error as its
// cause. The code is preserved losslessly (authorization codes are never
// collapsed into a generic 500) while Message stays sanitized.
func Wrap(code ErrorCode, err error) *DomainError {
	return &DomainError{
		Code:    code,
		Message: sanitize(string(code)),
		Cause:   err,
	}
}

// AsDomainError unwraps err and returns the innermost *DomainError in the chain,
// or false when none is present.
func AsDomainError(err error) (*DomainError, bool) {
	var de *DomainError
	if errors.As(err, &de) {
		return de, true
	}
	return nil, false
}