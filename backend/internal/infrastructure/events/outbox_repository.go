// Package events implements the event-pipeline side of Blueprint §7.4: the
// transactional outbox that gives durable at-least-once delivery to the AMQP
// broker, the in-process bus that synchronously dispatches committed events to
// subscribed sinks, and the relay that drains the outbox (see relay.go). Append
// commits the outbox row in the same ACID transaction as the ledger mutation;
// the relay publishes COMMITTED rows so no event is lost between commit and
// publish.
//
// The three adapters live in the Interface Adapters / Frameworks layer and
// depend only on the inward ports and the domain event contracts.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/domain"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sqlUniqueViolation is the PostgreSQL error code for a UNIQUE constraint
// violation, mapped to a conflict error at the Append boundary.
const sqlUniqueViolation = "23505"

// OutboxRecord is one row awaiting (or having received) delivery. Sequence is
// the per-aggregate monotonic order assigned by the publisher; id is the
// globally monotonic claim order used by ClaimBatch.
type OutboxRecord struct {
	ID            int64
	AggregateID   string
	Sequence      int64
	EventType     domain.EventType
	Payload       json.RawMessage
	CorrelationID string
	CreatedAt     time.Time
}

// OutboxRepository is the persistence adapter behind the ports.OutboxRepository
// contract. Append opens and commits its own transaction (the current port
// signature carries no caller-owned tx), writing the outbox row atomically with
// the surrounding mutation; the sibling claim/publish methods run in short
// autocommit transactions so lock windows stay bounded.
type OutboxRepository struct {
	db *pgxpool.Pool
}

var _ ports.OutboxRepository = (*OutboxRepository)(nil)

// NewOutboxRepository returns an outbox adapter bound to the given pool.
func NewOutboxRepository(db *pgxpool.Pool) *OutboxRepository {
	return &OutboxRepository{db: db}
}

// Append validates the event and inserts one immutable outbox row for it in a
// fresh transaction. The event already carries a publisher-assigned positive
// Sequence (the domain contract forbids regeneration), and the
// UNIQUE(aggregate_id, sequence) constraint is the backstop against duplicate
// appends to a stream. A hostile event type or mismatched sequence is rejected
// before any write (CWE-89 / deserialization-poisoning hardening).
func (r *OutboxRepository) Append(ctx context.Context, event domain.Event) error {
	if err := event.Validate(); err != nil {
		return err
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("outbox: begin append tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	// Parameterized only — event_type, payload, correlation_id and sequence are
	// never interpolated into SQL text.
	_, err = tx.Exec(ctx,
		`INSERT INTO event_outbox (aggregate_id, sequence, event_type, payload, correlation_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, NOW())`,
		event.AggregateID, event.Sequence, string(event.Type), event.Payload, event.CorrelationID,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == sqlUniqueViolation {
			return fmt.Errorf("outbox: duplicate event for aggregate %s at sequence %d", event.AggregateID, event.Sequence)
		}
		return fmt.Errorf("outbox: append %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("outbox: commit append tx: %w", err)
	}
	return nil
}

// ClaimBatch claims up to batchSize pending rows in id order using
// FOR UPDATE SKIP LOCKED, so worker replicas contend-free and never double-
// claim. The limit bounds the claim query via a child context so a stuck worker
// cannot hold row locks indefinitely. batchSize is clamped to an upper bound to
// prevent a misconfiguration from flooding the relay (CWE-770).
func (r *OutboxRepository) ClaimBatch(ctx context.Context, batchSize int, limit time.Duration) ([]OutboxRecord, error) {
	if batchSize <= 0 {
		batchSize = 100
	}
	if batchSize > 500 {
		batchSize = 500
	}
	claimCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	rows, err := r.db.Query(claimCtx,
		`SELECT id, aggregate_id, sequence, event_type, payload, correlation_id, created_at
		   FROM event_outbox
		  WHERE published_at IS NULL
		  ORDER BY id
		  LIMIT $1
		    FOR UPDATE SKIP LOCKED`,
		batchSize,
	)
	if err != nil {
		return nil, fmt.Errorf("outbox: claim batch: %w", err)
	}
	defer rows.Close()

	var records []OutboxRecord
	for rows.Next() {
		var rec OutboxRecord
		if err := rows.Scan(&rec.ID, &rec.AggregateID, &rec.Sequence, &rec.EventType,
			&rec.Payload, &rec.CorrelationID, &rec.CreatedAt); err != nil {
			return nil, fmt.Errorf("outbox: scan claim: %w", err)
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("outbox: iterate claim: %w", err)
	}
	return records, nil
}

// MarkPublished stamps the given rows as published in their own autocommit
// transaction, never inside a claim or mutation transaction. The
// published_at IS NULL guard makes remarking an idempotent no-op, so a crash
// between publish and mark leaves the row pending and it is re-claimed
// (at-least-once), while a successful mark prevents re-delivery.
func (r *OutboxRepository) MarkPublished(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := r.db.Exec(ctx,
		`UPDATE event_outbox SET published_at = NOW()
		  WHERE id = ANY($1::bigint[]) AND published_at IS NULL`,
		ids,
	)
	if err != nil {
		return fmt.Errorf("outbox: mark published: %w", err)
	}
	return nil
}

// PendingCount reports the current queue depth (rows awaiting delivery), served
// by the partial pending index. It is the queue-depth metric for worker scaling.
func (r *OutboxRepository) PendingCount(ctx context.Context) (int64, error) {
	var n int64
	if err := r.db.QueryRow(ctx,
		`SELECT count(*) FROM event_outbox WHERE published_at IS NULL`,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("outbox: pending count: %w", err)
	}
	return n, nil
}

// Consumer is a sink registered on the in-memory bus; it must handle a fully
// validated domain.Event and return an error the bus records without failing
// the caller.
type Consumer func(ctx context.Context, e domain.Event) error

// InMemoryBus is the in-process dispatch adapter implementing ports.EventBus.
// Publish is best-effort post-commit dispatch: Append (which committed the
// outbox row) must have returned nil first. A panicking consumer is contained
// by recover and logged with the event correlation id (CWE-248 / CWE-755); a
// consumer error is recorded but never fails the publish or rolls back the
// already-committed mutation.
type InMemoryBus struct {
	mu   sync.RWMutex
	subs map[string][]Consumer
	log  *slog.Logger
}

var _ ports.EventBus = (*InMemoryBus)(nil)

// NewInMemoryBus returns an empty bus that logs dispatch failures to logger.
func NewInMemoryBus() *InMemoryBus {
	return &InMemoryBus{
		subs: make(map[string][]Consumer),
		log:  slog.Default(),
	}
}

// Subscribe registers c to receive events of the given type. Subscriptions are
// read (fanned out) by Publish.
func (b *InMemoryBus) Subscribe(eventType string, c Consumer) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[eventType] = append(b.subs[eventType], c)
}

// Publish dispatches the event synchronously to every consumer registered for
// its type. Each consumer runs inside a recover guard so a panicking sink can
// never crash the process; the bus records the panic and any returned error and
// still returns nil to the caller (durable delivery is the outbox's job).
func (b *InMemoryBus) Publish(ctx context.Context, event domain.Event) error {
	b.mu.RLock()
	consumers := append([]Consumer(nil), b.subs[string(event.Type)]...)
	b.mu.RUnlock()

	for _, c := range consumers {
		err := b.dispatch(ctx, c, event)
		if err != nil {
			b.log.Error("in-memory event sink failed",
				"correlation_id", event.CorrelationID,
				"event_type", event.Type,
				"aggregate_id", event.AggregateID,
				"sequence", event.Sequence,
				"error", err.Error(),
			)
		}
	}
	return nil
}

func (b *InMemoryBus) dispatch(ctx context.Context, c Consumer, event domain.Event) (err error) {
	defer func() {
		if r := recover(); r != nil {
			b.log.Error("in-memory event sink panicked",
				"correlation_id", event.CorrelationID,
				"event_type", event.Type,
				"sequence", event.Sequence,
				"panic", fmt.Sprintf("%v", r),
			)
			err = fmt.Errorf("in-memory event sink panicked: %v", r)
		}
	}()
	return c(ctx, event)
}