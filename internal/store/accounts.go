package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"banking-service/internal/ledger"
	"banking-service/internal/money"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// GetAccount fetches a single account by ID.
// Returns ledger.ErrAccountNotFound when the account does not exist.
func (s *Store) GetAccount(ctx context.Context, id uuid.UUID) (ledger.Account, error) {
	const q = `
		SELECT id, name, balance, is_system, created_at, updated_at
		FROM accounts WHERE id = $1`

	row := s.pool.QueryRow(ctx, q, id)
	a, err := scanAccount(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return ledger.Account{}, ledger.ErrAccountNotFound
	}
	if err != nil {
		return ledger.Account{}, fmt.Errorf("store.GetAccount: %w", err)
	}
	return a, nil
}

// ListAccounts returns all non-system accounts ordered by name.
func (s *Store) ListAccounts(ctx context.Context) ([]ledger.Account, error) {
	const q = `
		SELECT id, name, balance, is_system, created_at, updated_at
		FROM accounts WHERE is_system = FALSE ORDER BY name`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store.ListAccounts query: %w", err)
	}
	defer rows.Close()
	return collectAccounts(rows)
}

// LockAccountsForUpdate issues a SELECT FOR UPDATE on the given account IDs
// inside tx. IDs are sorted by canonical UUID string form inside this function
// to prevent deadlocks when two concurrent transactions lock the same pair in
// opposite orders.
// Callers MUST NOT issue SELECT FOR UPDATE on accounts outside this function.
func LockAccountsForUpdate(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) ([]ledger.Account, error) {
	// Sort by string form — this is the only safe lock-ordering path.
	sorted := make([]uuid.UUID, len(ids))
	copy(sorted, ids)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].String() < sorted[j].String()
	})

	const q = `
		SELECT id, name, balance, is_system, created_at, updated_at
		FROM accounts
		WHERE id = ANY($1)
		ORDER BY id::text
		FOR UPDATE`

	rows, err := tx.Query(ctx, q, sorted)
	if err != nil {
		return nil, fmt.Errorf("store.LockAccountsForUpdate: %w", err)
	}
	defer rows.Close()
	return collectAccounts(rows)
}

// UpdateBalance sets an account's balance and updated_at within tx.
func UpdateBalance(ctx context.Context, tx pgx.Tx, id uuid.UUID, newBalance money.Money) error {
	const q = `UPDATE accounts SET balance = $1, updated_at = $2 WHERE id = $3`
	_, err := tx.Exec(ctx, q, newBalance.Decimal(), time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("store.UpdateBalance: %w", err)
	}
	return nil
}

// CreateAccount inserts a new non-system account with balance 0 and returns it.
// Note: account names are not unique by design; see the assignment scope note.
func (s *Store) CreateAccount(ctx context.Context, name string) (ledger.Account, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return ledger.Account{}, fmt.Errorf("store.CreateAccount uuid: %w", err)
	}
	const q = `
		INSERT INTO accounts (id, name, balance, is_system)
		VALUES ($1, $2, 0, FALSE)
		RETURNING id, name, balance, is_system, created_at, updated_at`

	row := s.pool.QueryRow(ctx, q, id, name)
	a, err := scanAccount(row)
	if err != nil {
		return ledger.Account{}, fmt.Errorf("store.CreateAccount: %w", err)
	}
	return a, nil
}

// LockAccountsForUpdate is a method wrapper for the package-level function,
// required so that *Store satisfies the ledger.Storer interface.
func (s *Store) LockAccountsForUpdate(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) ([]ledger.Account, error) {
	return LockAccountsForUpdate(ctx, tx, ids)
}

// UpdateBalance is a method wrapper for the package-level function,
// required so that *Store satisfies the ledger.Storer interface.
func (s *Store) UpdateBalance(ctx context.Context, tx pgx.Tx, id uuid.UUID, bal money.Money) error {
	return UpdateBalance(ctx, tx, id, bal)
}

// scanAccount reads one account row from any pgx.Row.
func scanAccount(row pgx.Row) (ledger.Account, error) {
	var a ledger.Account
	var bal decimal.Decimal
	err := row.Scan(&a.ID, &a.Name, &bal, &a.IsSystem, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return ledger.Account{}, err
	}
	a.Balance = money.NewFromDecimal(bal)
	return a, nil
}

// collectAccounts drains pgx.Rows into a slice of Account.
func collectAccounts(rows pgx.Rows) ([]ledger.Account, error) {
	var accounts []ledger.Account
	for rows.Next() {
		var a ledger.Account
		var bal decimal.Decimal
		if err := rows.Scan(&a.ID, &a.Name, &bal, &a.IsSystem, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store.collectAccounts scan: %w", err)
		}
		a.Balance = money.NewFromDecimal(bal)
		accounts = append(accounts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.collectAccounts rows: %w", err)
	}
	return accounts, nil
}
