package store

import (
	"context"
	"errors"
	"fmt"

	"banking-service/internal/ledger"
	"banking-service/internal/money"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
)

const pgErrUniqueViolation = "23505"

// InsertTransaction inserts the transaction header row inside tx.
// Returns ledger.ErrIdempotencyConflict when the idempotency key is already in use.
func InsertTransaction(ctx context.Context, tx pgx.Tx, t ledger.Transaction) error {
	const q = `
		INSERT INTO transactions (id, kind, idempotency_key, reverses_transaction_id)
		VALUES ($1, $2, $3, $4)`

	_, err := tx.Exec(ctx, q, t.ID, string(t.Kind), t.IdempotencyKey, t.ReversesTransactionID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgErrUniqueViolation {
			return ledger.ErrIdempotencyConflict
		}
		return fmt.Errorf("store.InsertTransaction: %w", err)
	}
	return nil
}

// InsertReversalTransaction inserts a reversal transaction inside tx.
// It distinguishes two UNIQUE constraint violations:
//   - idempotency_key conflict    → ledger.ErrIdempotencyConflict
//   - reverses_transaction_id conflict → ledger.ErrAlreadyReversed
//
// The defensive fallback inspects pgErr.Detail when ConstraintName is empty,
// matching whichever column name Postgres surfaces.
func InsertReversalTransaction(ctx context.Context, tx pgx.Tx, t ledger.Transaction) error {
	const q = `
		INSERT INTO transactions (id, kind, idempotency_key, reverses_transaction_id)
		VALUES ($1, $2, $3, $4)`

	_, err := tx.Exec(ctx, q, t.ID, string(t.Kind), t.IdempotencyKey, t.ReversesTransactionID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgErrUniqueViolation {
			// Primary check: use constraint name when Postgres provides it.
			if pgErr.ConstraintName == "transactions_reverses_transaction_id_key" {
				return ledger.ErrAlreadyReversed
			}
			if pgErr.ConstraintName == "transactions_idempotency_key_key" {
				return ledger.ErrIdempotencyConflict
			}
			// Fallback: inspect Detail which contains the conflicting column.
			if containsSubstr(pgErr.Detail, "reverses_transaction_id") {
				return ledger.ErrAlreadyReversed
			}
			return ledger.ErrIdempotencyConflict
		}
		return fmt.Errorf("store.InsertReversalTransaction: %w", err)
	}
	return nil
}

// containsSubstr reports whether s contains sub (simple scan, no allocations).
func containsSubstr(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// InsertEntries bulk-inserts entry rows inside tx.
// Returns ledger.ErrInvalidAmount if any entry has a non-positive amount.
// The DB deferred trigger will reject imbalanced sets at COMMIT time.
func InsertEntries(ctx context.Context, tx pgx.Tx, entries []ledger.Entry) error {
	for _, e := range entries {
		if !e.Amount.IsPositive() {
			return ledger.ErrInvalidAmount
		}
	}

	const q = `
		INSERT INTO entries (transaction_id, account_id, direction, amount)
		VALUES ($1, $2, $3, $4)`

	for _, e := range entries {
		if _, err := tx.Exec(ctx, q, e.TransactionID, e.AccountID, string(e.Direction), e.Amount.Decimal()); err != nil {
			return fmt.Errorf("store.InsertEntries: %w", err)
		}
	}
	return nil
}

// GetTransactionByIdempotencyKey fetches a transaction and its entries by idempotency key.
// Returns (zero, nil, ledger.ErrTransactionNotFound) when no matching row exists.
func (s *Store) GetTransactionByIdempotencyKey(ctx context.Context, key string) (ledger.Transaction, []ledger.Entry, error) {
	const q = `SELECT id, kind, idempotency_key, reverses_transaction_id, created_at
		FROM transactions WHERE idempotency_key = $1`

	row := s.pool.QueryRow(ctx, q, key)
	t, err := scanTransaction(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return ledger.Transaction{}, nil, ledger.ErrTransactionNotFound
	}
	if err != nil {
		return ledger.Transaction{}, nil, fmt.Errorf("store.GetTransactionByIdempotencyKey: %w", err)
	}

	entries, err := s.listEntries(ctx, t.ID)
	if err != nil {
		return ledger.Transaction{}, nil, err
	}
	t.Entries = entries
	return t, entries, nil
}

// GetTransaction fetches a transaction and its entries by transaction ID.
// Returns ledger.ErrTransactionNotFound when no matching row exists.
func (s *Store) GetTransaction(ctx context.Context, id uuid.UUID) (ledger.Transaction, []ledger.Entry, error) {
	const q = `SELECT id, kind, idempotency_key, reverses_transaction_id, created_at
		FROM transactions WHERE id = $1`

	row := s.pool.QueryRow(ctx, q, id)
	t, err := scanTransaction(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return ledger.Transaction{}, nil, ledger.ErrTransactionNotFound
	}
	if err != nil {
		return ledger.Transaction{}, nil, fmt.Errorf("store.GetTransaction: %w", err)
	}

	entries, err := s.listEntries(ctx, t.ID)
	if err != nil {
		return ledger.Transaction{}, nil, err
	}
	t.Entries = entries
	return t, entries, nil
}

// ListTransactionsForAccount returns up to limit transactions that include the
// given account, ordered by created_at DESC.
// Note: limit-only pagination; cursor-based pagination is a future improvement.
func (s *Store) ListTransactionsForAccount(ctx context.Context, accountID uuid.UUID, limit int) ([]ledger.Transaction, error) {
	const q = `
		SELECT DISTINCT t.id, t.kind, t.idempotency_key, t.reverses_transaction_id, t.created_at
		FROM transactions t
		JOIN entries e ON e.transaction_id = t.id
		WHERE e.account_id = $1
		ORDER BY t.created_at DESC
		LIMIT $2`

	rows, err := s.pool.Query(ctx, q, accountID, limit)
	if err != nil {
		return nil, fmt.Errorf("store.ListTransactionsForAccount: %w", err)
	}
	defer rows.Close()

	var txns []ledger.Transaction
	for rows.Next() {
		t, err := scanTransactionRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store.ListTransactionsForAccount scan: %w", err)
		}
		txns = append(txns, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListTransactionsForAccount rows: %w", err)
	}
	rows.Close() // close before issuing N+1 queries to avoid pool exhaustion

	// Populate Entries for each transaction (N+1 is acceptable given the limit cap).
	for i := range txns {
		entries, err := s.listEntries(ctx, txns[i].ID)
		if err != nil {
			return nil, err
		}
		txns[i].Entries = entries
	}
	return txns, nil
}

// listEntries fetches all entry rows for a given transaction ID, joining the
// account name so callers can display "DEBIT 100.0000 (Alice)" without a
// second query.
func (s *Store) listEntries(ctx context.Context, txID uuid.UUID) ([]ledger.Entry, error) {
	const q = `
		SELECT e.id, e.transaction_id, e.account_id, e.direction, e.amount, e.created_at, a.name
		FROM entries e
		JOIN accounts a ON a.id = e.account_id
		WHERE e.transaction_id = $1
		ORDER BY e.id`

	rows, err := s.pool.Query(ctx, q, txID)
	if err != nil {
		return nil, fmt.Errorf("store.listEntries: %w", err)
	}
	defer rows.Close()

	var entries []ledger.Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("store.listEntries scan: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// scanTransaction reads a single transaction from a pgx.Row.
func scanTransaction(row pgx.Row) (ledger.Transaction, error) {
	var t ledger.Transaction
	var kind string
	err := row.Scan(&t.ID, &kind, &t.IdempotencyKey, &t.ReversesTransactionID, &t.CreatedAt)
	if err != nil {
		return ledger.Transaction{}, err
	}
	t.Kind = ledger.TransactionKind(kind)
	return t, nil
}

// scanTransactionRow reads a single transaction from pgx.Rows (multi-row cursor).
func scanTransactionRow(rows pgx.Rows) (ledger.Transaction, error) {
	var t ledger.Transaction
	var kind string
	err := rows.Scan(&t.ID, &kind, &t.IdempotencyKey, &t.ReversesTransactionID, &t.CreatedAt)
	if err != nil {
		return ledger.Transaction{}, err
	}
	t.Kind = ledger.TransactionKind(kind)
	return t, nil
}

// scanEntry reads a single entry from pgx.Rows.
// The query must select columns in the order:
//
//	id, transaction_id, account_id, direction, amount, created_at, a.name
func scanEntry(rows pgx.Rows) (ledger.Entry, error) {
	var e ledger.Entry
	var dir string
	var amt decimal.Decimal
	err := rows.Scan(&e.ID, &e.TransactionID, &e.AccountID, &dir, &amt, &e.CreatedAt, &e.AccountName)
	if err != nil {
		return ledger.Entry{}, err
	}
	e.Direction = ledger.Direction(dir)
	e.Amount = money.NewFromDecimal(amt)
	return e, nil
}

// ListTransactionsOpts parameterises the global transaction listing query.
// Kind is a pointer — nil means no kind filter.
type ListTransactionsOpts struct {
	Limit  int
	Offset int
	Kind   *ledger.TransactionKind
}

// The following method wrappers allow *Store to satisfy ledger.Storer without
// removing the package-level functions that existing tests depend on.

// InsertTransaction is a method wrapper around the package-level InsertTransaction.
func (s *Store) InsertTransaction(ctx context.Context, tx pgx.Tx, t ledger.Transaction) error {
	return InsertTransaction(ctx, tx, t)
}

// InsertReversalTransaction is a method wrapper around the package-level InsertReversalTransaction.
func (s *Store) InsertReversalTransaction(ctx context.Context, tx pgx.Tx, t ledger.Transaction) error {
	return InsertReversalTransaction(ctx, tx, t)
}

// InsertEntries is a method wrapper around the package-level InsertEntries.
func (s *Store) InsertEntries(ctx context.Context, tx pgx.Tx, entries []ledger.Entry) error {
	return InsertEntries(ctx, tx, entries)
}

// ListTransactions returns all transactions ordered by created_at DESC, id DESC
// with optional kind filtering and offset/limit pagination. Each transaction has
// its Entries populated via listEntries (N+1 is acceptable given the limit cap of 200).
func (s *Store) ListTransactions(ctx context.Context, opts ListTransactionsOpts) ([]ledger.Transaction, error) {
	const q = `
		SELECT id, kind, idempotency_key, reverses_transaction_id, created_at
		FROM transactions
		WHERE ($1::text IS NULL OR kind = $1)
		ORDER BY created_at DESC, id DESC
		LIMIT $2 OFFSET $3`

	var kindArg *string
	if opts.Kind != nil {
		k := string(*opts.Kind)
		kindArg = &k
	}

	rows, err := s.pool.Query(ctx, q, kindArg, opts.Limit, opts.Offset)
	if err != nil {
		return nil, fmt.Errorf("store.ListTransactions: %w", err)
	}
	defer rows.Close()

	var txns []ledger.Transaction
	for rows.Next() {
		t, err := scanTransactionRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store.ListTransactions scan: %w", err)
		}
		txns = append(txns, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListTransactions rows: %w", err)
	}
	rows.Close() // close before issuing N+1 queries to avoid pool exhaustion

	// Populate Entries for each transaction (N+1 is acceptable given the limit cap).
	for i := range txns {
		entries, err := s.listEntries(ctx, txns[i].ID)
		if err != nil {
			return nil, err
		}
		txns[i].Entries = entries
	}
	return txns, nil
}
