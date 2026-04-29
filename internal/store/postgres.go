// Package store provides PostgreSQL-backed persistence for the banking ledger.
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store wraps a pgxpool connection pool and exposes ledger persistence methods.
// All balance reads within a transfer MUST occur AFTER LockAccountsForUpdate is
// called inside the transaction. Reads before lock acquisition are non-repeatable
// under ReadCommitted isolation.
type Store struct {
	pool *pgxpool.Pool
}

// New creates a Store backed by the given DSN, pings the DB, and returns an error
// on any connectivity or configuration problem.
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store.New pgxpool.New: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store.New ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases all connections in the pool.
func (s *Store) Close() {
	s.pool.Close()
}

// WithTx begins a ReadCommitted transaction, calls fn, and commits on success.
// If fn returns an error or panics, the transaction is rolled back.
// All balance reads within fn MUST happen after calling LockAccountsForUpdate.
func (s *Store) WithTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("store.WithTx begin: %w", err)
	}

	defer func() {
		// Rollback is a no-op after a successful commit.
		_ = tx.Rollback(ctx)
	}()

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store.WithTx commit: %w", err)
	}
	return nil
}
