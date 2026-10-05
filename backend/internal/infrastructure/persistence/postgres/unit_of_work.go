package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"fms/backend/internal/domain"
)

// This file adds the declarative transaction-boundary seam (WithTx /
// WithReadTx) plus the context-based tx-propagation helpers on the existing
// Store/UnitOfWork types (defined in postgres.go). Each WithTx call gives a use
// case one bounded pgx.Tx scope in which repositories run; commit/rollback
// happens exactly once here, never in use-case code. A call is single-goroutine
// scoped: the Store.tx member is swapped in for the duration and the injected
// context handle gives repositories a tx-bound context to detect.

// txCtxKey carries an active pgx.Tx through a request context so repository
// implementations in this package can detect they are inside a transaction
// scope and run their statements on the tx handle instead of the pool.
type txCtxKey struct{}

// ctxWithTx returns a copy of ctx carrying tx.
func ctxWithTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txCtxKey{}, tx)
}

// TxFromContext reports the transaction stored in ctx, if any.
func TxFromContext(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txCtxKey{}).(pgx.Tx)
	return tx, ok
}

// errNestedTransaction guards against a second WithTx/WithReadTx acquisition
// inside an already-open transaction scope.
var errNestedTransaction = errors.New("nested transaction")

// WithTx runs fn inside a single read-write ACID transaction on the ledger
// pool. It begins the transaction with ReadCommitted isolation under a
// statementTimeout deadline, re-establishes the tenant RLS context via SET
// LOCAL, and injects the tx handle into the context so repository statements
// share this one scope. Commits when fn returns nil; rolls back and preserves
// fn's error otherwise. Serialization/deadlock/unique-violation failures map to
// domain.ErrConcurrentModification (409) so a stale optimistic-lock or a lost
// concurrent write is surfaced as CONFLICT_STATE, never as an internal error.
// A nested WithTx inside an existing transaction is rejected.
func (s *Store) WithTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	if _, ok := TxFromContext(ctx); ok {
		return fmt.Errorf("%w: WithTx inside existing transaction", errNestedTransaction)
	}
	return s.runTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite}, fn)
}

// WithReadTx runs fn inside a read-only transaction for reporting reads. Access
// mode ReadOnly prevents any write inside the scope; it still establishes RLS
// context and honors the same deadline. Reads bind to the ledger pool because
// no read-replica pool is configured yet (Store.replica is nil); when a replica
// pool exists the caller can bind there without changing this seam. A
// read transaction nested inside a write transaction (or vice-versa) is
// rejected.
func (s *Store) WithReadTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	if _, ok := TxFromContext(ctx); ok {
		return fmt.Errorf("%w: WithReadTx inside existing transaction", errNestedTransaction)
	}
	return s.runTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly}, fn)
}

// runTx holds the shared WithTx/WithReadTx mechanics. The tenant must be
// resolvable up front (fails closed as UNAUTHENTICATED otherwise) and the
// Store.tx member is swapped in for the duration so repository methods reached
// through s.conn() run on this transaction.
func (s *Store) runTx(ctx context.Context, opts pgx.TxOptions, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tenant, derr := s.scopeTenant(ctx)
	if derr != nil {
		return derr
	}
	dctx, cancel := context.WithTimeout(ctx, statementTimeout)
	defer cancel()

	tx, err := s.primary.BeginTx(dctx, opts)
	if err != nil {
		return rwError(err)
	}
	// Re-establish the tenant inside the transaction so RLS applies to every
	// statement in the scope, including writes and the outbox insert.
	if _, err := tx.Exec(dctx, "SELECT set_config('app.tenant_id', $1::text, true)", tenant); err != nil {
		_ = tx.Rollback(dctx)
		return mapTxError(err)
	}

	prevTx := s.tx
	s.tx = tx
	fnCtx := ctxWithTx(dctx, tx)
	defer func() { s.tx = prevTx }()

	if err := fn(fnCtx, tx); err != nil {
		_ = tx.Rollback(dctx)
		return mapTxError(err)
	}
	if err := tx.Commit(dctx); err != nil {
		return mapTxError(err)
	}
	return nil
}

// mapTxError normalizes a transaction-scoped failure onto the domain error
// contract. PostgreSQL serialization_failure (40001), deadlock_detected (40P01),
// and unique_violation (23505) map to a 409 conflict; they may surface either
// from a statement inside fn or from the commit itself (pgx.ErrTxCommitRollback
// wraps the underlying PgError). Any other error is left untouched when it is
// already a *domain.DomainError and otherwise normalized through rwError, so
// VALIDATION/NOT_FOUND/NOT_AUTHORIZED semantics from repositories are preserved
// and never reclassified as client-retryable.
func mapTxError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "40001", "40P01", "23505":
			return domain.NewConflict("record modified by another user")
		}
	}
	if errors.Is(err, pgx.ErrTxCommitRollback) {
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "40001", "40P01", "23505":
				return domain.NewConflict("record modified by another user")
			}
		}
		return rwError(err)
	}
	var derr *domain.DomainError
	if errors.As(err, &derr) {
		return err
	}
	return rwError(err)
}