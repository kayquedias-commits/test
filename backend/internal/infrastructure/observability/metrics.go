// Package observability provides the Prometheus metrics registry and collectors used
// by both the API and worker processes (Blueprint §9.2 Observability).
//
// Design invariants (cross-checked against the security and data analyses):
//
//   - Every label value is drawn from a closed, documented vocabulary; unknown
//     values are coerced to "unknown" so no unbounded or business-identifying
//     series can be created. No label ever accepts a tenant ID, account ID,
//     category name, user email, or transaction ID.
//   - `/metrics` (see Handler) exposes only pre-aggregated, label-bounded
//     counter/histogram series — never request bodies, entity identifiers, or PII.
//     The endpoint is intended to be mounted by composition roots on an internal,
//     network-policy-restricted listener (k8s `networkpolicy.yaml`), never on the
//     public auth-gated router.
//   - NewMetrics must be called exactly once per process so the registered
//     collectors are actually exposed; a second call returns a disconnected
//     registry and is a silent observability gap.
//   - This package is infrastructure. It is reachable only from composition roots
//     (cmd/api, cmd/worker) and other infrastructure packages — never from the
//     domain or use-case layers.
//
// This package intentionally does not import internal/config: that package
// exposes no service/namespace scrape-label field for metric scoping yet, so a
// const-label partition would be dead weight. If such a field is added later,
// apply it uniformly to all collectors.
package observability

import (
	"net/http"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// httpBuckets are shaped so the bucket boundaries land on the p95 acceptance
// thresholds (write < 300 ms, query < 500 ms per §9.4), keeping quantile error
// within one bucket of those targets while the 10 s upper bound captures
// 99th-percentile outliers. The implicit +Inf bucket is appended by the client.
var httpBuckets = []float64{0.05, 0.1, 0.2, 0.3, 0.4, 0.5, 0.75, 1, 2.5, 5, 10}

var (
	// errorCodes is the canonical RFC 7807 error-code vocabulary from §7.7.
	errorCodes = map[string]struct{}{
		"VALIDATION_FAILED":      {},
		"UNAUTHENTICATED":        {},
		"FORBIDDEN":              {},
		"NOT_FOUND":              {},
		"CONFLICT_STATE":         {},
		"RATE_LIMITED":           {},
		"DEPENDENCY_UNAVAILABLE": {},
		"INTERNAL_ERROR":         {},
	}
	ownerVocab  = map[string]struct{}{"budget": {}, "category": {}, "account": {}, "session": {}, "default": {}}
	queueVocab  = map[string]struct{}{"export_jobs": {}, "event_bus": {}, "backup_jobs": {}}
	poolVocab   = map[string]struct{}{"db_read": {}, "db_write": {}, "export_worker": {}}
	jobVocab    = map[string]struct{}{"daily-full": {}, "wal-archive": {}}
	methodVocab = map[string]struct{}{"GET": {}, "POST": {}, "PATCH": {}, "PUT": {}, "DELETE": {}, "HEAD": {}, "OPTIONS": {}}
	resultVocab = map[string]struct{}{"success": {}, "failure": {}}
)

// Metrics is the single observability surface for a process. It is assembled by
// NewMetrics and passed (or its Handler) to the router/health endpoint by the
// composition root. All collectors carry only the bounded, pre-normalized labels
// documented in their declarations.
type Metrics struct {
	Reg           *prometheus.Registry
	HTTPReqDur    *prometheus.HistogramVec
	HTTPReqTotal  *prometheus.CounterVec
	ErrByCode     *prometheus.CounterVec
	QueueDepth    prometheus.Gauge
	PoolUsage     prometheus.Gauge
	CacheHitRatio *prometheus.CounterVec
	AuditWrite    *prometheus.CounterVec
	BackupResult  *prometheus.CounterVec

	// queueDepths and poolUsages back the QueueDepth/PoolUsage per-label gauges.
	// The exported QueueDepth/PoolUsage fields are single default-labelled gauges
	// (spec-required field shape); per-queue/per-pool sub-gauges are looked up or
	// created on demand here.
	queueDepths sync.Map // queue name -> prometheus.Gauge
	poolUsages  sync.Map // pool name   -> prometheus.Gauge
	queueVec    *prometheus.GaugeVec
	poolVec     *prometheus.GaugeVec
}

// NewMetrics constructs a fresh registry and registers every collector on it.
// Call exactly once per process (see package doc).
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()

	queueVec := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "fms_queue_depth",
			Help: "Current depth of each named queue (export_jobs, event_bus, backup_jobs).",
		},
		[]string{"queue"},
	)
	poolVec := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "fms_connection_pool_usage",
			Help: "Current number of in-use connections per named pool (db_read, db_write, export_worker).",
		},
		[]string{"pool"},
	)

	m := &Metrics{
		Reg: reg,
		HTTPReqDur: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "fms_http_request_duration_seconds",
				Help:    "Request duration in seconds per method, chi route pattern, and status class.",
				Buckets: httpBuckets,
			},
			[]string{"method", "route", "status"},
		),
		HTTPReqTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "fms_http_requests_total",
				Help: "Total HTTP requests per method, chi route pattern, and status class.",
			},
			[]string{"method", "route", "status"},
		),
		ErrByCode: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "fms_errors_total",
				Help: "Total errors per canonical API error code (RFC 7807).",
			},
			[]string{"code"},
		),
		QueueDepth: queueVec.WithLabelValues("unknown"),
		PoolUsage:  poolVec.WithLabelValues("unknown"),
		CacheHitRatio: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "fms_cache_hits_total",
				Help: "Cache hit/miss counts per owner; ratio is derived as hits/(hits+misses).",
			},
			[]string{"hit", "owner"},
		),
		AuditWrite: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "fms_audit_writes_total",
				Help: "Audit-log write throughput per result (success, failure).",
			},
			[]string{"result"},
		),
		BackupResult: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "fms_backup_result_total",
				Help: "Backup job outcome per job name and result (success, failure).",
			},
			[]string{"job", "result"},
		),
		queueVec: queueVec,
		poolVec:  poolVec,
	}

	// A single MustRegister call registers all collectors; duplicate registration
	// panics are impossible by construction.
	reg.MustRegister(
		m.HTTPReqDur,
		m.HTTPReqTotal,
		m.ErrByCode,
		m.queueVec,
		m.poolVec,
		m.CacheHitRatio,
		m.AuditWrite,
		m.BackupResult,
	)
	return m
}

// Handler returns the Prometheus text exposition handler for this registry.
// Mount it on an internal, network-policy-restricted listener — never on the
// public auth-gated router.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Reg, promhttp.HandlerOpts{})
}

// Registry exposes the raw registry as the single seam for an optional
// OTel-meter bridge if configured. Only one bridge may attach.
func (m *Metrics) Registry() *prometheus.Registry {
	return m.Reg
}

// ObserveHTTP records request duration and total count. route must be the
// pre-normalized chi route pattern (e.g. "/v1/transactions/{id}"), never a raw
// entity ID — this is the cardinality guard. method and status are coerced to
// closed vocabularies (status to its 2xx/3xx/4xx/5xx class).
func (m *Metrics) ObserveHTTP(dur float64, method, route string, status int) {
	meth := normMethod(method)
	rte := strings.TrimSpace(route)
	if rte == "" {
		rte = "unknown"
	}
	cls := statusClass(status)
	m.HTTPReqDur.WithLabelValues(meth, rte, cls).Observe(dur)
	m.HTTPReqTotal.WithLabelValues(meth, rte, cls).Inc()
}

// IncError counts an error under a canonical §7.7 code. Any code outside the
// enumerated vocabulary is coerced to "unknown".
func (m *Metrics) IncError(code string) {
	m.ErrByCode.WithLabelValues(normLabel(code, errorCodes)).Inc()
}

// SetQueueDepth records the current depth of a named queue (level metric, set
// after each worker drain). The queue name must come from the closed vocabulary.
func (m *Metrics) SetQueueDepth(q string, n float64) {
	key := normLabel(q, queueVocab)
	g := m.queueDepths.LoadOrStore(key, m.queueVec.WithLabelValues(key))
	g.(prometheus.Gauge).Set(n)
}

// SetPoolUsage records the current number of in-use connections for a named
// pool. Alerting compares this against the pool_configured_max constant derived
// from connection-pool config (pool_size + max_overflow).
func (m *Metrics) SetPoolUsage(pool string, n float64) {
	key := normLabel(pool, poolVocab)
	g := m.poolUsages.LoadOrStore(key, m.poolVec.WithLabelValues(key))
	g.(prometheus.Gauge).Set(n)
}

// IncCacheHit records a cache hit (miss=false) or miss (miss=true) for the given
// owner. The owner must come from the closed vocabulary; no tenant/entity IDs.
// The hit ratio is derived at query time from the two counters.
func (m *Metrics) IncCacheHit(miss bool, owner string) {
	hit := "true"
	if miss {
		hit = "false"
	}
	m.CacheHitRatio.WithLabelValues(hit, normLabel(owner, ownerVocab)).Inc()
}

// IncAuditWrite records audit-log write throughput by result (success, failure).
func (m *Metrics) IncAuditWrite(result string) {
	m.AuditWrite.WithLabelValues(normLabel(result, resultVocab)).Inc()
}

// ObserveBackup records a backup job outcome and must be called after every job
// termination — on success and on failure — so the success denominator does not
// skew. The job name must come from the closed vocabulary.
func (m *Metrics) ObserveBackup(job, result string) {
	m.BackupResult.WithLabelValues(
		normLabel(job, jobVocab),
		normLabel(result, resultVocab),
	).Inc()
}

// normMethod upper-cases and validates an HTTP method against the closed set.
func normMethod(m string) string {
	m = strings.ToUpper(strings.TrimSpace(m))
	if _, ok := methodVocab[m]; ok {
		return m
	}
	return "unknown"
}

// statusClass reduces a 3-digit status to its 2xx/3xx/4xx/5xx class so series
// count stays bounded by endpoint count, not by the full status-code space.
func statusClass(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500 && status < 600:
		return "5xx"
	default:
		return "unknown"
	}
}

// normLabel returns v if it belongs to vocab, otherwise "unknown", so no
// unbounded or business-identifying value can become a series label.
func normLabel(v string, vocab map[string]struct{}) string {
	if _, ok := vocab[v]; ok {
		return v
	}
	return "unknown"
}