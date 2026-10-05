// Package events implements the event-pipeline side of Blueprint §7.4. This
// file provides the durable in-process bus: an alternative EventBus adapter
// that appends each published event to the transactional outbox before
// delivering it synchronously to registered consumers. Durability is owned by
// the outbox (the same ACID transaction as the state change), so a crash before
// commit drops nothing the committed mutation did not also drop.
//
// It is deliberately named DurableBus and uses a distinct constructor
// (NewDurableBus) because the package already exports an in-memory-only
// InMemoryBus in outbox_repository.go; both implement the same ports.EventBus
// contract. Consumers registered here must be side-effect safe and idempotent,
// because the outbox relay re-delivers committed events to the async pipeline.
package events

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"fms/backend/internal/application/ports"
	"fms/backend/internal/domain"
	"fms/backend/internal/infrastructure/observability"
)

// Delivery and capacity bounds. Delivery is synchronous and confined to
// projection/audit work, so a per-consumer deadline plus a hard consumer cap
// keep a stuck or runaway sink from stalling the request transaction (CWE-400 /
// CWE-770). The latency gate below targets the §9.4 p95 write budget.
const (
	defaultWarnThreshold   = 100 * time.Millisecond
	defaultConsumerTimeout = 2 * time.Second
	maxConsumers           = 64
)

// Sentinel errors returned by the bus. ErrBusClosed is returned by Publish and
// Subscribe once Close has run; ErrDuplicateConsumer enforces exactly-once
// registration; ErrTooManyConsumers caps subscription growth.
var (
	ErrBusClosed         = errors.New("events: bus closed")
	ErrDuplicateConsumer = errors.New("events: consumer already registered")
	ErrTooManyConsumers  = errors.New("events: consumer limit exceeded")
)

// OutboxWriter is the durability seam behind which the bus records events. It
// aliases the authoritative application-layer outbox port; the concrete
// implementation writes the outbox row on the caller's unit-of-work transaction.
type OutboxWriter = ports.OutboxRepository

// DurableBus dispatches published events to subscribed consumers after durably
// appending them to the outbox. Append is the hard precondition for delivery: a
// failed append returns an error and no consumer sees the event. Consumer
// failures are collected and surfaced as a post-commit, best-effort signal,
// never as a reason to roll back an already-committed mutation.
type DurableBus struct {
	mu       sync.RWMutex
	consumer []Consumer
	identity map[uintptr]struct{}
	outbox   OutboxWriter
	clock    ports.Clock
	log      *observability.Logger
	closed   bool
}

var _ ports.EventBus = (*DurableBus)(nil)

// NewDurableBus returns an empty durable bus that records events through outbox
// and reads the clock for latency instrumentation. A nil clock falls back to
// the wall clock; a nil logger disables bus logging.
func NewDurableBus(outbox OutboxWriter, clock ports.Clock, logger *observability.Logger) *DurableBus {
	return &DurableBus{
		consumer: make([]Consumer, 0, 8),
		identity: make(map[uintptr]struct{}),
		outbox:   outbox,
		clock:    clock,
		log:      logger,
	}
}

// Subscribe registers handler for exactly-once delivery of future events. It
// rejects duplicate handlers (by function identity) and respects the consumer
// cap (maxConsumers). It returns ErrBusClosed after Close.
func (b *DurableBus) Subscribe(handler Consumer) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrBusClosed
	}
	id := reflect.ValueOf(handler).Pointer()
	if _, ok := b.identity[id]; ok {
		return ErrDuplicateConsumer
	}
	if len(b.consumer) >= maxConsumers {
		return ErrTooManyConsumers
	}
	b.consumer = append(b.consumer, handler)
	b.identity[id] = struct{}{}
	return nil
}

// Publish validates the event, appends it durably to the outbox, then delivers
// it synchronously to every registered consumer in subscription order. An
// invalid event or a failed outbox append returns an error with zero delivery
// (durability boundary). Consumer errors are aggregated but do not stop
// delivery to the remaining sinks and are returned after append has committed.
func (b *DurableBus) Publish(ctx context.Context, event domain.Event) error {
	// Fail closed: envelope validation enforces a non-zero TenantID/ActorID, a
	// registered type, a valid correlation id, and a non-nil payload, so an
	// event lacking tenant context is never delivered.
	if err := event.Validate(); err != nil {
		return err
	}

	b.mu.RLock()
	snapshot := append([]Consumer(nil), b.consumer...)
	closed := b.closed
	b.mu.RUnlock()
	if closed {
		return ErrBusClosed
	}

	start := b.now()
	// Durability boundary: the outbox row must be appended before any consumer
	// runs, so a failed append leaves the event unaccepted and undelivered.
	if err := b.outbox.Append(ctx, event); err != nil {
		return err
	}

	var errs []error
	for _, c := range snapshot {
		if err := b.deliver(ctx, c, event); err != nil {
			errs = append(errs, err)
		}
	}

	elapsed := b.now().Sub(start)
	if b.log != nil {
		if elapsed >= defaultWarnThreshold {
			b.log.Warn("event published",
				"event_id", event.ID.String(),
				"event_type", string(event.Type),
				"correlation_id", event.CorrelationID,
				"tenant_id", event.TenantID.String(),
				"latency_ms", elapsed.Milliseconds())
		} else {
			b.log.Info("event published",
				"event_id", event.ID.String(),
				"event_type", string(event.Type),
				"correlation_id", event.CorrelationID,
				"tenant_id", event.TenantID.String(),
				"latency_ms", elapsed.Milliseconds())
		}
	}
	return errors.Join(errs...)
}

// Close idempotently stops the bus: subsequent Publish and Subscribe calls
// return ErrBusClosed and any registered consumers are dropped.
func (b *DurableBus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	b.consumer = nil
	b.identity = nil
	return nil
}

// deliver invokes one consumer under a bounded deadline and a recover guard so
// a panicking or hung sink can never stall the request or crash the process
// (CWE-248 / CWE-391). A deadline expiry or panic is returned as an error.
func (b *DurableBus) deliver(ctx context.Context, c Consumer, event domain.Event) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if b.log != nil {
				b.log.Error("events: consumer panicked",
					"event_id", event.ID.String(),
					"event_type", string(event.Type),
					"correlation_id", event.CorrelationID,
					"tenant_id", event.TenantID.String(),
					"panic", fmt.Sprintf("%v", r))
			}
			err = fmt.Errorf("events: consumer panicked: %v", r)
		}
	}()
	dctx, cancel := context.WithTimeout(ctx, defaultConsumerTimeout)
	defer cancel()
	return c(dctx, event)
}

// now returns the current instant from the injected clock, falling back to the
// wall clock when none was provided.
func (b *DurableBus) now() time.Time {
	if b.clock != nil {
		return b.clock.Now()
	}
	return time.Now()
}