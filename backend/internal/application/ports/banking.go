// Package ports declares the application-layer contracts that the core FMS
// depends on to reach external systems. Because bank-feed connectivity is
// contested scope (Blueprint §9.1) and deliberately unconfigured, this package
// defines only the port surface and its data types; adapter implementations
// (including the default stub) live in infrastructure/banking.
//
// The port is read-only by construction: it exposes transaction ingress and
// sync-status observation, never a path that writes to the ledger aggregate.
// Manual entry is unaffected whenever this capability is unavailable (§7.7
// failure isolation).
package ports

import (
	"context"
	"time"

	"fms/backend/internal/domain"
)

// BankingSyncStatus describes the reconciliation state of a bank-account sync.
// Only NotConfigured is reachable with the stub; the remaining constants are
// forward-declared so an approved adapter can land without reshaping the
// contract.
type BankingSyncStatus string

const (
	// BankingSyncStatusNotConfigured reports that the banking integration is
	// unbound. It is the only state the stub can produce.
	BankingSyncStatusNotConfigured BankingSyncStatus = "not_configured"
	// BankingSyncStatusPending marks a sync run in flight.
	BankingSyncStatusPending BankingSyncStatus = "pending"
	// BankingSyncStatusSynced marks the last sync run completed.
	BankingSyncStatusSynced BankingSyncStatus = "synced"
	// BankingSyncStatusFailed marks the last sync run errored.
	BankingSyncStatusFailed BankingSyncStatus = "failed"
)

// BankTransaction is an immutable data-contract value at the port boundary. It
// quarantines the bank-native transaction model from the domain; a future
// adapter must map it to a domain Transaction/Money during ingest, never the
// reverse. All fields are data-contract definitions with no persistence implied.
type BankTransaction struct {
	// ExternalID is the canonical idempotency/dedup key for future ingest.
	ExternalID string
	// Amount reuses the domain Money value object so two-decimal precision is
	// enforced at the boundary; never a float64.
	Amount domain.Money
	// Date is the bank posting date in UTC; ledger-date assignment happens at
	// the use-case layer, not here.
	Date time.Time
	// Description is the raw bank-supplied text. Sanitization/truncation to the
	// transaction description constraint is the adapter's anti-corruption job.
	Description string
	// Metadata is the anti-corruption seam: unrecognized raw bank attributes are
	// held here, structurally isolated from the domain model. Allowed keys and
	// per-key value caps must be allowlisted by the adapter.
	Metadata map[string]string
}

// ExternalBankingIntegration is the output port for contested bank-feed
// connectivity (§9.1). Core FMS operates fully with only the stub bound. ctx
// carries the tenant group and correlation ID — tenancy is propagated through
// context, never through signature parameters. accountRef is an opaque external
// bank account reference, never an internal ledger ID, so external systems
// never observe domain identities (anti-corruption). since is the incremental
// sync high-water mark; the adapter owns watermark persistence and duplicate
// elimination via ExternalID.
//
// FetchTransactions returns an unbounded slice without pagination (deferred by
// spec); a future adapter must chunk internally to avoid materializing a large
// feed in memory.
//
// The stub in infrastructure/banking must satisfy: FetchTransactions returns a
// permanent, non-retryable DEPENDENCY_UNAVAILABLE error (503-style, with the
// message "banking integration not configured") — never nil and never a partial
// slice; SyncStatus returns BankingSyncStatusNotConfigured with a nil error.
type ExternalBankingIntegration interface {
	FetchTransactions(ctx context.Context, accountRef string, since time.Time) ([]BankTransaction, error)
	SyncStatus(ctx context.Context, accountRef string) (BankingSyncStatus, error)
}