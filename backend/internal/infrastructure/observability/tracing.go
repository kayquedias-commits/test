// Package observability provides the OpenTelemetry tracer adapter and the
// correlation-ID propagation helpers for distributed tracing across the
// API -> use case -> repository -> queue worker span chain (§9.2, §7.3). It is
// an infrastructure seam; application and domain layers must depend on a port
// interface rather than reaching into this package directly.
package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"fms/backend/internal/config"
)

const (
	// CorrelationHeaderKey is the HTTP header the gateway reads (and, when
	// absent, fills) to seed the request correlation ID.
	CorrelationHeaderKey = "X-Correlation-ID"
	// correlationIDRegex is the allowlist that inbound correlation IDs must
	// satisfy. Rejecting anything else (control chars, spaces, odd length)
	// prevents log/trace injection and keeps exporter payloads bounded.
	correlationIDRegex = "^[a-zA-Z0-9_-]{1,128}$"
	// generatedCorrelationIDPrefix distinguishes locally-generated IDs.
	generatedCorrelationIDPrefix = "req_"
	// spanScopeAttr carries the caller-provided layer scope on each span.
	spanScopeAttr = "fms.scope"
	// resourceSchemaURL is the W3C/OTel semantic-convention schema for the
	// resource attributes set below (service.name).
	resourceSchemaURL = "https://opentelemetry.io/schemas/1.26.0"
	// Attribute keys, kept as literals so no semconv module version has to be
	// pinned; the set is an explicit allowlist and is never extended with
	// request data.
	attrServiceName   = "service.name"
	attrTenantID      = "tenant.id"
	attrUserID        = "user.id"
	attrAggregateID   = "aggregate.id"
	attrHTTPRoute     = "http.route"
	attrHTTPStatus    = "http.status_code"
	attrOutboxMsgID   = "outbox.message_id"
)

var (
	corrRe = regexp.MustCompile(correlationIDRegex)

	// correlationCtxKey is the unexported context key under which the
	// correlation ID is stored. Its pointer identity keeps the value private.
	correlationCtxKey correlationContextKey
)

type correlationContextKey struct{}

// sensitiveAttrRe matches span attribute keys that must never be recorded,
// even if a caller misuses the meta helper (defensive redaction; the meta
// allowlist already excludes these).
var sensitiveAttrRe = regexp.MustCompile(`(?i)password|token|cookie|secret|authorization`)

// NewTracerProvider builds the OpenTelemetry TracerProvider. When the config
// has a non-empty OtelEndpoint the provider exports over OTLP/gRPC with TLS in
// transit; otherwise a no-op exporter keeps local/dev runs dependency-free with
// zero network egress. The delegated sampler is ParentBased over a
// TraceIDRatio, so child spans inherit their parent's sampling decision and the
// worker/outbox spans only follow when their API parent was sampled. The batch
// span processor is explicitly bounded so a full queue drops spans rather than
// blocking the request path.
func NewTracerProvider(ctx context.Context, cfg *config.Config) (*sdktrace.TracerProvider, error) {
	res := resource.NewWithAttributes(
		resourceSchemaURL,
		attribute.String(attrServiceName, cfg.Observability.ServiceName),
	)

	options := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(
			sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.Observability.TraceSampleRatio)),
		),
		sdktrace.WithResource(res),
	}

	if cfg.Observability.OtelEndpoint != "" {
		exp, err := otlptracegrpc.New(
			ctx,
			otlptracegrpc.WithEndpoint(cfg.Observability.OtelEndpoint),
			otlptracegrpc.WithTLSClientCredentials(),
		)
		if err != nil {
			return nil, errors.Join(errors.New("failed to create OTLP exporter"), err)
		}
		options = append(options, sdktrace.WithBatcher(
			exp,
			sdktrace.WithBatchTimeout(5*time.Second),
			sdktrace.WithMaxQueueSize(2048),
			sdktrace.WithMaxExportBatchSize(512),
			sdktrace.WithExportTimeout(10*time.Second),
		))
	}
	// Without an endpoint we intentionally omit the batcher option so the SDK
	// installs its no-op span processor: spans are recorded but not exported.

	tp := sdktrace.NewTracerProvider(options...)
	otel.SetTracerProvider(tp)
	return tp, nil
}

// Shutdown flushes any in-flight spans and then shuts the provider down so
// audit-critical span evidence is not lost during deploys. Safe to call with a
// nil provider.
func Shutdown(ctx context.Context, tp *sdktrace.TracerProvider) error {
	if tp == nil {
		return nil
	}
	var errs []error
	if err := tp.ForceFlush(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := tp.Shutdown(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// StartSpan starts a span scoped to the synthetic OTel tracer for scope and
// attaches the fms.scope attribute so layer latency can be bucketed. It returns
// the span-scoped context and the open span; the caller must defer span.End().
// Long-running jobs should pass context.Background() so their root span is not
// derived from an expired HTTP request context.
func StartSpan(ctx context.Context, scope, name string) (context.Context, trace.Span) {
	ctx, span := otel.Tracer(scope).Start(ctx, name, trace.WithSpanKind(spanKind(scope)))
	span.SetAttributes(attribute.String(spanScopeAttr, scope))
	return ctx, span
}

// spanKind maps an FMS layer scope to a semantic OTel span kind.
func spanKind(scope string) trace.SpanKind {
	switch strings.ToLower(scope) {
	case "handler", "httphandler", "api":
		return trace.SpanKindServer
	case "worker", "outbox", "consumer":
		return trace.SpanKindConsumer
	case "producer", "publisher":
		return trace.SpanKindProducer
	default:
		return trace.SpanKindInternal
	}
}

// AddSpanMeta attaches only the allowlisted tenant/user/aggregate identifiers
// to the current span. Empty values are skipped so read-heavy spans do not
// explode the trace-store cardinality or carry PII. A defensive redaction pass
// drops any key matching the sensitive-attribute denylist. No request body,
// credential, or account number is ever accepted here by construction.
func AddSpanMeta(ctx context.Context, tenantID, userID, aggregateID string) {
	attrs := make([]attribute.KeyValue, 0, 3)
	appendIf := func(k string, v string) {
		if v == "" || sensitiveAttrRe.MatchString(k) {
			return
		}
		attrs = append(attrs, attribute.String(k, v))
	}
	appendIf(attrTenantID, tenantID)
	appendIf(attrUserID, userID)
	appendIf(attrAggregateID, aggregateID)
	if len(attrs) > 0 {
		trace.SpanFromContext(ctx).SetAttributes(attrs...)
	}
}

// SpanFromContext returns the active span from ctx so middleware can attach
// HTTP route/status attributes after route resolution.
func SpanFromContext(ctx context.Context) trace.Span {
	return trace.SpanFromContext(ctx)
}

// AddHTTPMeta attaches route and status-code attributes to the current span.
// route must be the framework route pattern (e.g. /v1/transactions/{id}), never
// a raw URI carrying concrete values, so no PII leaks into traces.
func AddHTTPMeta(ctx context.Context, route string, status int) {
	span := trace.SpanFromContext(ctx)
	if route != "" {
		span.SetAttributes(attribute.String(attrHTTPRoute, route))
	}
	if status != 0 {
		span.SetAttributes(attribute.Int(attrHTTPStatus, status))
	}
}

// AddOutboxMeta attaches the outbox message ID so the worker span links the
// queue handoff back to the ledger write.
func AddOutboxMeta(ctx context.Context, messageID string) {
	if messageID != "" {
		trace.SpanFromContext(ctx).SetAttributes(attribute.String(attrOutboxMsgID, messageID))
	}
}

// ExtractCorrelation returns the correlation ID bound to ctx, or "" if none is
// present. It never generates a value (generation happens once at the gateway
// via InjectCorrelation), keeping downstream extraction idempotent.
func ExtractCorrelation(ctx context.Context) string {
	if v, ok := ctx.Value(correlationCtxKey).(string); ok {
		return v
	}
	return ""
}

// InjectCorrelation binds a valid correlation ID to ctx. If one is already
// present it is returned unchanged, preserving a single source of truth. When
// absent it generates a cryptographically random ID. This value is tracing
// metadata only: consumers re-enriching outbox events must resolve the
// authoritative tenant/user from the IAM context and must never authorize on
// this header.
func InjectCorrelation(ctx context.Context) context.Context {
	if v := ExtractCorrelation(ctx); v != "" {
		return ctx
	}
	return context.WithValue(ctx, correlationCtxKey, generateCorrelation())
}

// InjectHTTPHeaders writes the bound correlation ID into the given header map
// (used when pushing a message to the outbox/queue). A missing value is not
// written, so an empty header can never seed a downstream trace.
func InjectHTTPHeaders(ctx context.Context, headers map[string]string) {
	if v := ExtractCorrelation(ctx); v != "" {
		headers[CorrelationHeaderKey] = v
	}
}

// ExtractHTTPHeaders reads X-Correlation-ID from the given header map,
// sanitizing it against the allowlist, and binds it to ctx. A malformed value
// is discarded and replaced by a freshly generated ID so a hostile header can
// never inject log or trace entries. If no header is present one is generated.
func ExtractHTTPHeaders(ctx context.Context, headers map[string]string) context.Context {
	raw := headers[CorrelationHeaderKey]
	if v := sanitizeCorrelation(raw); v != "" {
		return context.WithValue(ctx, correlationCtxKey, v)
	}
	return InjectCorrelation(ctx)
}

// sanitizeCorrelation validates and returns a trimmed inbound correlation ID,
// or "" when it fails the allowlist (characters or length).
func sanitizeCorrelation(v string) string {
	v = strings.TrimSpace(v)
	if !corrRe.MatchString(v) {
		return ""
	}
	return v
}

// generateCorrelation returns a fresh "req_<32 hex>" ID sourced from crypto/
// rand so unguessable values seed outbox joins and trace aggregation.
func generateCorrelation() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand cannot normally fail; fall back to a time-seeded value
		// rather than returning an empty ID that would break trace correlation.
		return generatedCorrelationIDPrefix + strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	}
	return generatedCorrelationIDPrefix + hex.EncodeToString(b[:])
}