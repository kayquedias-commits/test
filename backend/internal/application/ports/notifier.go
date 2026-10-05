// Package ports declares the application-layer output ports that invert the
// dependency rule: the application core owns the contracts, and concrete
// adapters in the infrastructure layer implement them. Ports carry no
// framework, HTTP, or ORM concern.
package ports

import (
	"context"

	"github.com/google/uuid"
)

// NotificationKind identifies the budget-alert category a Notification
// represents. It is a closed set of exactly two allowed values; anything else
// must be rejected at the use-case boundary (INVALID_NOTIFICATION_KIND) rather
// than reaching a delivery adapter as a free-form string.
type NotificationKind string

const (
	// NotificationKindBudgetApproached signals spend within the configured
	// approach threshold of the budget limit.
	NotificationKindBudgetApproached NotificationKind = "budget_approached"
	// NotificationKindBudgetExceeded signals spend past the budget limit.
	NotificationKindBudgetExceeded NotificationKind = "budget_exceeded"
)

// Notification is the delivery DTO produced when a budget threshold event is
// translated at the use-case boundary. It is distinct from the domain event
// (BudgetThresholdExceededEvent): the domain layer never references delivery
// concerns, and the port never leaks domain events.
//
// Contract: TenantID and UserID must be sourced from the IAM-resolved context
// at the call site and must share the generating tenant — the port must never
// be invoked with a recipient lacking read permission on the triggering budget
// (broken object level authorization). Metadata keys are a fixed allowlist and
// carry monetary values as decimal strings, never floats.
type Notification struct {
	TenantID uuid.UUID
	UserID   uuid.UUID
	Kind     NotificationKind
	Subject  string
	Body     string
	Metadata map[string]string
}

// Notifier is the output port for budget-alert delivery. The use case that
// publishes a budget threshold event translates it into a Notification and
// calls Notify.
//
// Non-blocking invariant: implementations MUST return within a short bounded
// budget or dispatch asynchronously (goroutine pool / message broker) and
// return nil immediately after enqueueing. Notify must never block the ledger
// mutation or budget recomputation request path.
//
// Error semantics: a non-nil error signals a delivery failure only. A use case
// must never roll back the underlying transaction because of it; the error is
// logged (Kind, TenantID, delivery outcome only — never Subject, Body, or
// Metadata content) and otherwise ignored.
//
// Idempotency: because the port is invoked from event consumers that may
// retry (at-least-once), delivery must be idempotent per the tuple
// (TenantID, UserID, Kind, Metadata.budget_id, period_start, period_end).
//
// The interface-only contract is self-sufficient: wiring selects a concrete
// adapter from a configuration flag, and the Noop adapter (infrastructure
// layer) returns nil without panicking when unbound.
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}