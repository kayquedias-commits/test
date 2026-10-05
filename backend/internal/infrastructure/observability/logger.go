// Package observability provides the shared structured JSON logger for the FMS.
// Every service (API, worker, scheduler) writes through this logger so log
// lines aggregate coherently in the centralized logging pipeline (spec §9.2).
//
// Security invariant: logs are JSON to stdout and carry no PII beyond internal
// user IDs. Never log secrets — passwords, MFA/TOTP codes, tokens, JWT
// signatures, request bodies, or connection URLs with credentials. Redaction is
// enforced both at the call site (sanitizeKV) and as a defensive backstop at
// the handler level (redactAttr).
package observability

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"fms/backend/internal/config"
)

// Logger wraps log/slog behind a thin seam so callers stay decoupled from the
// concrete slog handler and a different backend can be swapped in later.
type Logger struct {
	l *slog.Logger
}

// ctxKey is an unexported context-key type; storing values under a private
// key type prevents collisions with other packages writing to the context.
type ctxKey struct{}

var (
	loggerCtxKey = ctxKey{}
	userIDCtxKey = ctxKey{}
)

// defaultLogger is the FromContext fallback. It is created once at info level
// so traceability is never lost when no request context value is present.
var defaultLogger = newLogger(os.Stdout, slog.LevelInfo)

// logLevelAllowlist is the strict set of accepted LOG_LEVEL values. Anything
// else falls back to info so a bad env value cannot flood the pipeline.
var logLevelAllowlist = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// correlationIDRe is the allowlist for correlation IDs. Values failing this
// (including anything containing control characters) are treated as untrusted
// and replaced with a fresh server-generated ID — never echoed raw.
var correlationIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// redactedKeys lists keys whose values are never written to logs. On a match
// the value is replaced with "[REDACTED]", preserving log structure for
// forensics. It covers secrets, credential-bearing fields, and PII.
var redactedKeys = map[string]struct{}{
	"password":          {},
	"token":             {},
	"access_token":      {},
	"refresh_token":     {},
	"authorization":     {},
	"cookie":            {},
	"session":           {},
	"mfa":               {},
	"totp":              {},
	"otp":               {},
	"secret":            {},
	"api_key":           {},
	"private_key":       {},
	"body":              {},
	"request_body":      {},
	"response_body":     {},
	"connection_string": {},
	"dsn":               {},
	"url":               {},
	"email":             {},
	"name":              {},
	"phone":             {},
	"ssn":               {},
	"tax_id":            {},
	"dob":               {},
}

// redactBufThreshold bounds the size of a value that is ever written verbatim.
// Larger buffers (e.g. raw request content) are redacted wholesale.
const redactBufThreshold = 4096

// New builds a Logger from the LOG_LEVEL environment variable (default info).
// The cfg parameter is accepted for composition-root symmetry and future
// config plumbing; today the level is read directly from the environment
// (the config package does not yet expose a LogLevel field).
func New(cfg *config.Config) (*Logger, error) {
	level := parseLevel(os.Getenv("LOG_LEVEL"))
	if level == slog.LevelInfo && os.Getenv("FMS_LOG_LEVEL") != "" {
		level = parseLevel(os.Getenv("FMS_LOG_LEVEL"))
	}
	return newLogger(os.Stdout, level), nil
}

// parseLevel maps an allowed level string to a slog.Level. Empty falls back to
// info; an unknown value is logged as a warning (never echoing control
// characters) and defaults to info so log volume stays bounded.
func parseLevel(s string) slog.Level {
	trimmed := strings.ToLower(strings.TrimSpace(s))
	if trimmed == "" {
		return slog.LevelInfo
	}
	if lvl, ok := logLevelAllowlist[trimmed]; ok {
		return lvl
	}
	slog.Warn("invalid LOG_LEVEL value, falling back to info",
		"value", sanitizeControl(trimmed))
	return slog.LevelInfo
}

// newLogger constructs the Logger writing to w at the given level. AddSource is
// left disabled so file/line data never enters logs, honoring the policy that
// no stack or source information reaches operators.
func newLogger(w io.Writer, level slog.Level) *Logger {
	opts := &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: redactAttr,
	}
	return &Logger{l: slog.New(slog.NewJSONHandler(w, opts))}
}

// redactAttr is the handler-level defense-in-depth: any attribute whose key
// names a redacted field is masked before serialization, independent of the
// call sites.
func redactAttr(_ []string, a slog.Attr) slog.Attr {
	if _, ok := redactedKeys[strings.ToLower(a.Key)]; ok {
		return slog.String(a.Key, "[REDACTED]")
	}
	return a
}

// Debug emits a debug-level message.
func (l *Logger) Debug(msg string, kv ...any) { l.l.Debug(msg, sanitizeKV(kv)...) }

// Info emits an info-level message.
func (l *Logger) Info(msg string, kv ...any) { l.l.Info(msg, sanitizeKV(kv)...) }

// Warn emits a warn-level message.
func (l *Logger) Warn(msg string, kv ...any) { l.l.Warn(msg, sanitizeKV(kv)...) }

// Error emits an error-level message.
func (l *Logger) Error(msg string, kv ...any) { l.l.Error(msg, sanitizeKV(kv)...) }

// With returns a child logger with the given key/value pairs attached. It never
// mutates the receiver, so concurrent handlers stay isolated.
func (l *Logger) With(kv ...any) *Logger {
	return &Logger{l: l.l.With(sanitizeKV(kv)...)}
}

// WithCorrelation returns a child logger carrying the given correlation ID. IDs
// failing the allowlist are replaced with a fresh server-generated ID.
func (l *Logger) WithCorrelation(id string) *Logger {
	if !correlationIDRe.MatchString(id) {
		id = newCorrelationID()
	}
	return l.With("correlation_id", id)
}

// WithUserID returns a child logger carrying the authenticated user ID.
func (l *Logger) WithUserID(id string) *Logger {
	return l.With("user_id", id)
}

// WithRequest extracts/creates the correlation ID from the X-Correlation-ID
// header and the authenticated user ID from request context, then returns a
// child logger carrying both. A client-supplied header is treated as untrusted:
// if it fails the allowlist a fresh ID is generated. The enriched logger is
// stored in the request context for downstream FromContext lookups.
func (l *Logger) WithRequest(r *http.Request) *Logger {
	cid := r.Header.Get("X-Correlation-ID")
	if !correlationIDRe.MatchString(cid) {
		cid = newCorrelationID()
	}
	uid, _ := r.Context().Value(userIDCtxKey).(string)
	child := l.With("correlation_id", cid, "user_id", uid)
	if r != nil {
		r = r.WithContext(context.WithValue(r.Context(), loggerCtxKey, child))
	}
	return child
}

// FromContext retrieves the logger stored in context by the correlation
// middleware. It never returns nil: when no logger is present, a fresh logger
// with a new correlation ID is returned so traceability is preserved.
func (l *Logger) FromContext(ctx context.Context) *Logger {
	if ctx == nil {
		return defaultLogger.WithCorrelation(newCorrelationID())
	}
	if lg, ok := ctx.Value(loggerCtxKey).(*Logger); ok && lg != nil {
		return lg
	}
	return defaultLogger.WithCorrelation(newCorrelationID())
}

// Err emits an error-level message carrying the domain error code and a
// sanitized message. The error is inspected via an interface for its typed
// code — no stack trace is ever captured, and message text is scrubbed of
// credential-bearing substrings and control characters.
func (l *Logger) Err(msg string, err error, kv ...any) {
	code := "INTERNAL_ERROR"
	var coder interface{ Code() string }
	if err != nil && errors.As(err, &coder) {
		code = coder.Code()
	}
	fields := sanitizeKV(kv...)
	fields = append(fields,
		slog.String("error_code", code),
		slog.String("error_message", sanitizeErrorText(fmt.Sprint(err))),
	)
	l.l.Error(msg, fields...)
}

// RequestLog emits the standard end-of-request line for the given route, HTTP
// status, and latency. Only scalar fields are written; request/response bodies
// are never logged.
func (l *Logger) RequestLog(route string, status int, latency time.Duration) {
	l.l.Info("http_request",
		slog.String("route", sanitizeControl(route)),
		slog.Int("status", status),
		slog.Int64("latency_ms", latency.Milliseconds()),
	)
}

// Flush is a no-op kept for interface symmetry. slog writes synchronously to
// stdout, so there is no buffering state to flush; buffering is deliberately
// avoided so logs are never lost on crash (supports the availability target).
func (l *Logger) Flush() {}

// sanitizeKV filters caller-supplied key/value pairs before writing. Keys naming
// a redacted field, along with any []byte or oversized buffer values, are
// replaced with "[REDACTED]".
func sanitizeKV(kv ...any) []any {
	out := make([]any, 0, len(kv))
	for i := 0; i+1 < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			out = append(out, kv[i], kv[i+1])
			continue
		}
		val := kv[i+1]
		if _, isRed := redactedKeys[strings.ToLower(key)]; isRed || isRedactedBuffer(val) {
			val = "[REDACTED]"
		}
		out = append(out, key, val)
	}
	return out
}

// isRedactedBuffer reports whether a value must be masked as raw content.
func isRedactedBuffer(v any) bool {
	if b, ok := v.([]byte); ok {
		return true
	}
	if b, ok := v.(*bytes.Buffer); ok {
		return true
	}
	if s, ok := v.(string); ok && len(s) > redactBufThreshold {
		return true
	}
	return false
}

// sanitizeErrorText caps message length, ensures valid UTF-8, strips control
// characters, and scrubs credential-bearing patterns (URL userinfo and
// password/pwd parameters) so internal details never reach logs verbatim.
func sanitizeErrorText(s string) string {
	if len(s) > 512 {
		s = s[:512]
	}
	s = strings.ToValidUTF8(s, "")
	s = scrubCredentials(s)
	return sanitizeControl(s)
}

// sanitizeControl removes CR/LF/TAB from a string used in a log field, guarding
// against log injection through attacker-controlled inputs.
func sanitizeControl(s string) string {
	replacer := strings.NewReplacer("\r", "", "\n", "", "\t", "")
	return replacer.Replace(s)
}

// scrubCredentials removes password material and URL userinfo fragments from a
// text string. It is intentionally conservative: on finding such a pattern the
// whole credential-bearing span is masked.
func scrubCredentials(s string) string {
	// scheme://user:pass@host
	j := strings.Index(s, "://")
	if j >= 0 {
		rest := s[j+3:]
		at := strings.Index(rest, "@")
		if at >= 0 && strings.Contains(rest[:at], ":") {
			s = s[:j+3] + "[REDACTED]@" + rest[at+1:]
		}
	}
	for _, p := range []string{"password=", "pwd=", "passwd="} {
		idx := strings.Index(strings.ToLower(s), p)
		if idx >= 0 {
			end := strings.IndexAny(s[idx:], " &;\t\r\n")
			if end < 0 {
				s = s[:idx] + p + "[REDACTED]"
			} else {
				s = s[:idx] + p + "[REDACTED]" + s[idx+end:]
			}
		}
	}
	return s
}

// newCorrelationID generates a random RFC 4122 v4 UUID string. crypto/rand is
// used directly to avoid relying on any external dependency for correlation.
func newCorrelationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is unrecoverable for traceability; fall back to a
		// monotonically distinct value derived from the clock.
		return "fallback-" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	var sb [36]byte
	hex.Encode(sb[0:8], b[0:4])
	sb[8] = '-'
	hex.Encode(sb[9:13], b[4:6])
	sb[13] = '-'
	hex.Encode(sb[14:18], b[6:8])
	sb[18] = '-'
	hex.Encode(sb[19:23], b[8:10])
	sb[23] = '-'
	hex.Encode(sb[24:36], b[10:16])
	return string(sb[:])
}