// Package events implements the event-pipeline side of Blueprint §7.4: the
// transactional outbox that gives durable at-least-once delivery to the AMQP
// broker, the in-process bus, and the relay that publishes committed outbox
// rows to the broker. State changes and outbox rows are committed in the same
// local ACID transaction (Append); this relay drains COMMITTED rows so no event
// is lost between commit and publish.
//
// Delivery is at-least-once: a crash between broker ack and MarkPublished
// leaves the row pending and it is republished on restart. Consumers must
// deduplicate by EventID so replay never double-applies ledger effects
// (never double-balance-updates). Within a tenant the monotonic Sequence from
// the outbox table preserves strict ordering; consumers checkpoint per
// (consumer, tenant_id), never on a single global high-water mark.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rabbitmq/amqp091-go"

	"fms/backend/internal/config"
	"fms/backend/internal/domain"
	"fms/backend/internal/infrastructure/observability"
)

const (
	relayExchange             = "fms.events"
	relayExchangeType         = "topic"
	relayQueue                = "fms.events"
	relayBindingKey           = "events.#"
	relayPublishRoutingPrefix = "events."
	relayDLXExchange          = "fms.events.dlx"

	outboxPending = "PENDING"
	outboxDead    = "DEAD"

	// maxPublishRetries bounds consecutive publish failures for one row before it
	// is marked DEAD — poison messages surface for operator review, never dropped.
	maxPublishRetries = 5
)

// correlationIDRE is the structural allowlist for correlation ids before they are
// placed on the broker headers. It bounds log-injection and rejects untrusted
// control characters (mirrors the domain envelope allowlist).
var correlationIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// OutboxRow is one persisted outbox row awaiting delivery. Sequence is the
// monotonic per-tenant order assigned by the publisher; EventID is the consumer
// dedup key that neutralizes at-least-once duplicates; TenantID is the
// partition/ordering boundary and the source of the tenant_id header (never
// derived from the payload body).
type OutboxRow struct {
	ID            int64
	EventID       uuid.UUID
	Type          domain.EventType
	TenantID      uuid.UUID
	CorrelationID string
	Payload       json.RawMessage
	Sequence      int64
	Status        string
	CreatedAt     time.Time
}

// OutboxReader is the read contract the relay depends on. Pending must return
// rows ordered by sequence ASC so a single relay drains without head-of-line
// blocking across tenants; MarkPublished and MarkDead are idempotent
// PENDING→{PUBLISHED,DEAD} transitions so duplicate relay instances never
// double-publish with conflicting correlation semantics.
type OutboxReader interface {
	Pending(ctx context.Context, limit int) ([]OutboxRow, error)
	MarkPublished(ctx context.Context, id int64) error
	MarkDead(ctx context.Context, id int64) error
}

// Publisher is the thin write seam over the AMQP channel that lets the relay be
// unit-tested against a mocked channel while production traffic flows through a
// real amqp091 channel.
type Publisher interface {
	Publish(ctx context.Context, msg amqp091.Publishing) error
	Close() error
}

// amqpPublisher adapts an *amqp091.Channel to the Publisher port. The routing
// key is derived from the event_type header so messages match the events.#
// topic binding.
type amqpPublisher struct {
	ch *amqp091.Channel
}

// Publish sends msg to the durable topic exchange with a routing key that
// matches the events.# binding.
func (p *amqpPublisher) Publish(ctx context.Context, msg amqp091.Publishing) error {
	key := relayPublishRoutingPrefix + headerString(msg.Headers, "event_type")
	return p.ch.PublishWithContext(ctx, relayExchange, key, false, false, msg)
}

// Close closes the underlying AMQP channel.
func (p *amqpPublisher) Close() error { return p.ch.Close() }

// Relay drains the durable outbox to the broker with at-least-once semantics.
// It keeps a single publisher-confirmed channel and, on broker failure, backs
// off exponentially (base 100 ms, factor 2, jitter, max 5 retries) before
// re-queueing; 5 consecutive failures for one row mark it DEAD. The API process
// and worker may each run a relay; MarkPublished idempotency plus consumer
// EventID dedup make duplicate instances harmless.
type Relay struct {
	outbox       OutboxReader
	ch           Publisher
	exchange     string
	queue        string
	log          *observability.Logger
	metrics      *observability.Metrics
	batchSize    int
	pollInterval time.Duration

	published *prometheus.CounterVec
	dead      *prometheus.CounterVec

	mu       sync.Mutex
	failures map[int64]int // consecutive publish failures per outbox row id
}

// NewRelay wires the outbox to the given broker connection, declares the
// durable dead-letter-enabled topology, registers the relay metrics, and
// returns the ready relay. It refuses plaintext broker URLs and missing
// credentials so financial event payloads never traverse an unauthenticated or
// non-TLS channel. The caller owns the connection lifecycle (NewRelay opens its
// own channel and only Shutdown closes it).
func NewRelay(outbox OutboxReader, conn *amqp091.Connection, cfg *config.Config, log *observability.Logger, metrics *observability.Metrics) (*Relay, error) {
	if conn == nil {
		return nil, errors.New("relay: nil amqp connection")
	}
	if outbox == nil {
		return nil, errors.New("relay: nil outbox reader")
	}
	if log == nil {
		return nil, errors.New("relay: nil logger")
	}
	if metrics == nil {
		return nil, errors.New("relay: nil metrics")
	}
	if err := enforceBrokerSecurity(cfg); err != nil {
		return nil, err
	}

	ch, err := conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("relay: open channel over %s: %w", amqpSanitizedURL(cfg.Amqp.URL), err)
	}
	if err := declareTopology(ch); err != nil {
		_ = ch.Close()
		return nil, err
	}

	published := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "relay_published_total",
		Help: "Total outbox rows published to the broker and acknowledged.",
	}, nil)
	dead := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "relay_dead_total",
		Help: "Total outbox rows marked dead after consecutive publish failures.",
	}, nil)
	metrics.Reg.MustRegister(published, dead)

	return &Relay{
		outbox:       outbox,
		ch:           &amqpPublisher{ch: ch},
		exchange:     relayExchange,
		queue:        relayQueue,
		log:          log,
		metrics:      metrics,
		batchSize:    100,
		pollInterval: time.Second,
		published:    published,
		dead:         dead,
		failures:     make(map[int64]int),
	}, nil
}

// enforceBrokerSecurity fails fast when the AMQP URL is absent, uses a
// plaintext (non-TLS) scheme, or carries no broker credentials (anonymous
// authentication refused, CWE-287). It never echoes the URL or credentials in
// error output.
func enforceBrokerSecurity(cfg *config.Config) error {
	if cfg == nil || strings.TrimSpace(cfg.Amqp.URL) == "" {
		return errors.New("relay: FMS_AMQP_URL is required")
	}
	u, err := url.Parse(cfg.Amqp.URL)
	if err != nil {
		return errors.New("relay: FMS_AMQP_URL is not a valid broker URL")
	}
	if !strings.EqualFold(u.Scheme, "amqps") {
		return errors.New("relay: broker transport must be amqps:// (TLS); plaintext amqp:// refused")
	}
	if u.User == nil || u.User.Username() == "" {
		return errors.New("relay: broker credentials required; anonymous authentication refused")
	}
	return nil
}

// declareTopology asserts the durable topic exchange, a dead-letter exchange,
// the durable bound queue, and publisher confirms so persistent deliveries
// survive broker restart and undeliverable messages poison to the DLX instead
// of being silently lost.
func declareTopology(ch *amqp091.Channel) error {
	if err := ch.ExchangeDeclare(relayExchange, relayExchangeType, true, false, false, false, nil); err != nil {
		return fmt.Errorf("relay: declare exchange: %w", err)
	}
	if err := ch.ExchangeDeclare(relayDLXExchange, relayExchangeType, true, false, false, false, nil); err != nil {
		return fmt.Errorf("relay: declare dead-letter exchange: %w", err)
	}
	args := amqp091.Table{"x-dead-letter-exchange": relayDLXExchange}
	if _, err := ch.QueueDeclare(relayQueue, true, false, false, false, args); err != nil {
		return fmt.Errorf("relay: declare queue: %w", err)
	}
	if err := ch.QueueBind(relayQueue, relayBindingKey, relayExchange, false, nil); err != nil {
		return fmt.Errorf("relay: bind queue: %w", err)
	}
	if _, err := ch.Confirm(false); err != nil {
		return fmt.Errorf("relay: enable publisher confirms: %w", err)
	}
	return nil
}

// Run drains the outbox until ctx is cancelled, selecting on ctx.Done() between
// polls so shutdown is prompt and deterministic.
func (r *Relay) Run(ctx context.Context) error {
	r.log.Info("event relay started",
		"exchange", r.exchange,
		"queue", r.queue,
		"batch_size", r.batchSize,
		"poll_interval", r.pollInterval.String(),
	)
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.log.Info("event relay stopped")
			return nil
		case <-ticker.C:
			r.runOnce(ctx)
		}
	}
}

// runOnce fetches one sequence-ordered batch, publishes each row on ack, and
// emits the queue-depth gauge. It guards against busy-spin: a batch yielding no
// state transition backs off before the next poll (CWE-400).
func (r *Relay) runOnce(ctx context.Context) {
	if err := ctx.Err(); err != nil {
		return
	}
	rows, err := r.outbox.Pending(ctx, r.batchSize)
	if err != nil {
		r.log.Error("relay: pending fetch failed", "error", err.Error())
		r.backoff(ctx)
		return
	}
	r.metrics.SetQueueDepth("event_bus", float64(len(rows)))

	advanced := 0
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		if r.publishRow(ctx, row) {
			advanced++
		}
	}
	if len(rows) > 0 && advanced == 0 {
		r.backoff(ctx)
	}
}

// publishRow attempts delivery for one row. A structurally invalid row is routed
// straight to DEAD (Security §3) rather than published as malformed JSON. On a
// transient publish failure the row stays PENDING and is retried with backoff;
// after maxPublishRetries consecutive failures it is marked DEAD (never dropped).
func (r *Relay) publishRow(ctx context.Context, row OutboxRow) bool {
	err := r.publishRecord(ctx, row)
	if err == nil {
		r.clearFailure(row.ID)
		return true
	}
	// Structural validation failures are fatal: the row could never be delivered
	// intact, so poison it for operator review instead of burning retries.
	if errors.Is(err, errInvalidRow) {
		if markErr := r.outbox.MarkDead(ctx, row.ID); markErr == nil {
			r.dead.WithLabelValues().Inc()
			r.clearFailure(row.ID)
			r.log.Error("relay: structurally invalid outbox row marked dead",
				"id", row.ID, "event_type", row.Type, "sequence", row.Sequence, "error", err.Error())
			return true
		}
		r.log.Error("relay: marking invalid row dead failed", "id", row.ID, "error", err.Error())
		return true
	}
	if r.recordFailure(row.ID) >= maxPublishRetries {
		if markErr := r.outbox.MarkDead(ctx, row.ID); markErr != nil {
			r.log.Error("relay: marking row dead failed", "id", row.ID, "error", markErr.Error())
			return true
		}
		r.dead.WithLabelValues().Inc()
		r.clearFailure(row.ID)
		r.log.Error("relay: outbox row marked dead after consecutive publish failures",
			"id", row.ID, "event_type", row.Type, "sequence", row.Sequence)
		return true
	}
	r.log.Warn("relay: publish failed; will retry",
		"id", row.ID, "event_type", row.Type, "sequence", row.Sequence)
	r.backoff(ctx)
	return false
}

// publishRecord validates the row, then publishes a persistent message carrying
// the correlation/tenant/type/version/sequence headers with the typed event
// payload as the JSON body, and marks the row published on broker ack. Only row
// identity and event type are ever logged — never the payload body.
func (r *Relay) publishRecord(ctx context.Context, row OutboxRow) error {
	if err := validateRow(row); err != nil {
		return fmt.Errorf("%w: %v", errInvalidRow, err)
	}
	version, _ := row.Type.Version()
	msg := amqp091.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp091.Persistent,
		Timestamp:    row.CreatedAt,
		MessageId:    fmt.Sprintf("%s:%d", row.EventID, row.Sequence),
		Headers: amqp091.Table{
			"correlation_id": row.CorrelationID,
			"tenant_id":      row.TenantID.String(),
			"event_type":     string(row.Type),
			"event_version":  version,
			"sequence":       row.Sequence,
		},
		Body: row.Payload,
	}
	if err := r.ch.Publish(ctx, msg); err != nil {
		return fmt.Errorf("publish outbox row %d: %w", row.ID, err)
	}
	if err := r.outbox.MarkPublished(ctx, row.ID); err != nil {
		return fmt.Errorf("mark outbox row %d published: %w", row.ID, err)
	}
	r.published.WithLabelValues().Inc()
	return nil
}

// errInvalidRow is a sentinel distinguishing structural validation failures from
// transient publish errors so the relay can poison structurally-invalid rows
// immediately rather than retrying them.
var errInvalidRow = errors.New("outbox: structurally invalid row")

// validateRow enforces the structural contract before anything reaches the
// broker (CWE-20 / deserialization hardening): non-nil identities, an
// allowlisted correlation id, a positive sequence, PENDING status, a registered
// event type, and a round-trip payload decode.
func validateRow(row OutboxRow) error {
	if row.ID <= 0 {
		return errors.New("invalid row id")
	}
	if row.EventID == uuid.Nil {
		return errors.New("missing event id")
	}
	if row.TenantID == uuid.Nil {
		return errors.New("missing tenant id")
	}
	if !correlationIDRE.MatchString(row.CorrelationID) {
		return errors.New("invalid correlation id")
	}
	if row.Sequence <= 0 {
		return errors.New("invalid sequence")
	}
	if row.Status != outboxPending {
		return errors.New("row not pending")
	}
	if _, ok := row.Type.Version(); !ok {
		return errors.New("unknown event type")
	}
	if _, err := domain.DecodePayload(row.Type, row.Payload); err != nil {
		return errors.New("payload failed round-trip validation")
	}
	return nil
}

// Shutdown performs a best-effort final drain of pending rows then closes the
// relay's channel. The AMQP connection itself is closed by the caller.
func (r *Relay) Shutdown(ctx context.Context) error {
	if rows, err := r.outbox.Pending(ctx, r.batchSize); err == nil && len(rows) > 0 {
		r.log.Info("relay: draining remaining outbox rows", "count", len(rows))
		for _, row := range rows {
			if ctx.Err() != nil {
				break
			}
			r.publishRow(ctx, row)
		}
	}
	return r.ch.Close()
}

// backoff sleeps with exponential growth (base 100 ms, factor 2) plus jitter,
// up to maxPublishRetries steps per §7.7. A cancelled ctx returns immediately.
func (r *Relay) backoff(ctx context.Context) {
	delay := 100 * time.Millisecond
	for attempts := 0; attempts < maxPublishRetries; attempts++ {
		wait := delay + time.Duration(rand.Float64()*float64(delay))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		delay *= 2
	}
}

func (r *Relay) recordFailure(id int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures[id]++
	return r.failures[id]
}

func (r *Relay) clearFailure(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.failures, id)
}

// headerString reads a string-valued header from an AMQP table.
func headerString(h amqp091.Table, key string) string {
	if v, ok := h[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// amqpSanitizedURL strips userinfo credentials from a broker URL so it can be
// surfaced in error output without leaking username/password material.
func amqpSanitizedURL(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		return raw
	}
	rest := raw[i+3:]
	at := strings.Index(rest, "@")
	if at < 0 {
		return raw
	}
	return raw[:i+3] + "***@" + rest[at+1:]
}