// Package ports defines the application-layer output ports that decouple use
// cases from their transport, persistence, and notification implementations.
// Each port is an interface-only contract; adapters live in the infrastructure
// layer. Dependencies point strictly inward toward the domain layer — no
// framework, HTTP, ORM, or broker types appear here.
package ports

import (
	"context"

	"fms/backend/internal/domain"
)

// EventSink is the adapter contract for a consumer of domain events. Sink
// implementations (budget actuals, reporting read model, export status) are
// decoupled from producers — they only ever see a fully-validated
// domain.Event. A sink returns an error that the bus records, but a sink
// failure never fails the originating use case: per §7.4, consumer failures
// are recovered by the sink's retry/backoff policy or by the reconciliation
// job, never by rolling back the primary write.
type EventSink func(ctx context.Context, event domain.Event) error

// EventBus is the in-process/transport dispatch port for domain events.
//
// Contract:
//   - Publish is best-effort, POST-COMMIT dispatch. The primary state change
//     and the outbox row are committed first (same ACID transaction, via
//     OutboxRepository.Append); Publish then dispatches to in-memory sinks or
//     hands the relayed event to the transport. It performs no durable write
//     itself and never opens its own transaction.
//   - Durability is guaranteed by the OutboxRepository, so Publish must not be
//     called before Append returned nil. An append failure rolls back the whole
//     transaction and the event is never dispatched.
//   - The given ctx must carry the IAM-established tenant, actor, and
//     correlation IDs; an event without tenant context is rejected before
//     enqueue (UNAUTHENTICATED), never dispatched on a missing tenant.
//   - Callers publish once per mutation (no fan-out at the source).
//   - Idempotency and per-aggregate ordering are preserved by the
//     domain.Event ID/Sequence; the bus must never regenerate either.
type EventBus interface {
	Publish(ctx context.Context, event domain.Event) error
}

// OutboxRepository is the durability port for the transactional outbox pattern
// (§7.4). Append MUST be bound to the caller's unit of work transaction so the
// state change and the outbox row commit atomically — a crash between the two
// would lose the event. The caller owns the transaction and commits only after
// Append returns nil; the relay publishes only COMMITTED rows.
//
// Security and integrity obligations on implementations:
//   - Append MUST reject an event whose Validation fails or whose TenantID /
//     CorrelationID do not match the transaction context (across-tenant
//     leakage via replay is prevented at this boundary).
//   - The outbox row must be append-only (no UPDATE/DELETE by the relay user).
type OutboxRepository interface {
	Append(ctx context.Context, event domain.Event) error
}