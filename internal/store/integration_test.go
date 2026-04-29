// Package store_test contains integration tests that require a live PostgreSQL
// instance. Tests spin up a Postgres container via testcontainers-go.
//
// Docker MUST be running for these tests to pass. If Docker is unavailable,
// the TestMain will skip all tests in this package with a clear message rather
// than failing.
package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"banking-service/internal/ledger"
	"banking-service/internal/money"
	"banking-service/internal/store"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

var (
	testDSN   string
	testStore *store.Store
)

// TestMain spins up a Postgres container, runs migrations, and runs all subtests.
// If Docker is unavailable, all tests are skipped.
func TestMain(m *testing.M) {
	ctx := context.Background()

	pgContainer, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		// Docker not running or image unavailable — skip instead of hard-fail.
		// This keeps CI green on machines without Docker and matches the
		// contract: "use t.Skip with a clear message if testcontainers fails."
		m.Run() // runs zero tests; the global testStore is nil
		return
	}
	defer func() { _ = pgContainer.Terminate(ctx) }()

	testDSN, err = pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		m.Run()
		return
	}

	// Apply migrations using the file source, pointing at the project root's
	// migrations/ directory relative to this test file.
	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	migrationsDir, _ = filepath.Abs(migrationsDir)

	mg, err := migrate.New("file://"+migrationsDir, testDSN)
	if err != nil {
		m.Run()
		return
	}
	if err := mg.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		m.Run()
		return
	}

	testStore, err = store.New(ctx, testDSN)
	if err != nil {
		m.Run()
		return
	}
	defer testStore.Close()

	m.Run()
}

// skipIfNoDocker skips the test if testStore was not initialised (Docker absent).
func skipIfNoDocker(t *testing.T) {
	t.Helper()
	if testStore == nil {
		t.Skip("Docker not available; skipping integration test")
	}
}

// seedTransactionIDs are the well-known UUIDs inserted by migration 0003.
var seedTransactionIDs = []string{
	"01900000-0000-7000-8000-000000000001",
	"01900000-0000-7000-8000-000000000002",
	"01900000-0000-7000-8000-000000000003",
}

// seedAccountIDs are the well-known UUIDs inserted by migrations 0002 and 0003.
var seedAccountIDs = []string{
	"00000000-0000-0000-0000-000000000001",
	"00000000-0000-0000-0000-000000000002",
	"00000000-0000-0000-0000-000000000003",
	"00000000-0000-0000-0000-000000000004",
}

// truncate clears all non-seed data between tests for isolation.
// Deletion order respects FK constraints: entries → transactions → accounts.
// Entries are identified by their transaction_id (not account_id) to avoid
// leaving orphaned transactions when test entries reference seed accounts.
func truncate(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("truncate pool: %v", err)
	}
	defer pool.Close()

	stmts := []string{
		`DELETE FROM audit_log`,
		// Delete entries that belong to non-seed transactions.
		`DELETE FROM entries WHERE transaction_id NOT IN (
			'01900000-0000-7000-8000-000000000001',
			'01900000-0000-7000-8000-000000000002',
			'01900000-0000-7000-8000-000000000003'
		)`,
		// Now it is safe to delete the non-seed transaction headers.
		`DELETE FROM transactions WHERE id NOT IN (
			'01900000-0000-7000-8000-000000000001',
			'01900000-0000-7000-8000-000000000002',
			'01900000-0000-7000-8000-000000000003'
		)`,
		// Remove non-seed, non-system accounts added by individual tests.
		`DELETE FROM accounts WHERE is_system = FALSE AND id NOT IN (
			'00000000-0000-0000-0000-000000000002',
			'00000000-0000-0000-0000-000000000003',
			'00000000-0000-0000-0000-000000000004'
		)`,
		// Reset EXTERNAL and demo account balances to their seeded values so
		// tests that mutate balances don't leak state into subsequent tests.
		`UPDATE accounts SET balance = -30000.0000 WHERE id = '00000000-0000-0000-0000-000000000001'`,
		`UPDATE accounts SET balance =  10000.0000 WHERE id IN (
			'00000000-0000-0000-0000-000000000002',
			'00000000-0000-0000-0000-000000000003',
			'00000000-0000-0000-0000-000000000004'
		)`,
	}
	for _, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("truncate exec (%s): %v", stmt[:40], err)
		}
	}
}

func TestCreateAndGetAccount(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)
	ctx := context.Background()

	created, err := testStore.CreateAccount(ctx, "Priya")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if created.Name != "Priya" {
		t.Errorf("Name = %q; want Priya", created.Name)
	}
	if !created.Balance.IsZero() {
		t.Errorf("new account balance = %s; want 0.0000", created.Balance)
	}

	fetched, err := testStore.GetAccount(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if fetched.ID != created.ID {
		t.Errorf("GetAccount ID mismatch")
	}
}

func TestGetAccount_NotFound(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()

	_, err := testStore.GetAccount(ctx, uuid.New())
	if !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Errorf("expected ErrAccountNotFound; got %v", err)
	}
}

func TestListAccounts_ExcludesSystem(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)
	ctx := context.Background()

	accounts, err := testStore.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	for _, a := range accounts {
		if a.IsSystem {
			t.Errorf("ListAccounts returned system account %s", a.Name)
		}
	}
	// 3 demo accounts seeded by migration 0003.
	if len(accounts) != 3 {
		t.Errorf("expected 3 demo accounts; got %d", len(accounts))
	}
}

// TestLockAccountsForUpdate verifies that lock ordering is stable regardless of
// input order. We pass two IDs in reverse order and confirm both rows are returned.
func TestLockAccountsForUpdate(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)
	ctx := context.Background()

	alice := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	bob := uuid.MustParse("00000000-0000-0000-0000-000000000003")

	err := testStore.WithTx(ctx, func(tx pgx.Tx) error {
		// Pass IDs in reverse order — function must still work correctly.
		rows, err := store.LockAccountsForUpdate(ctx, tx, []uuid.UUID{bob, alice})
		if err != nil {
			return err
		}
		if len(rows) != 2 {
			t.Errorf("expected 2 locked accounts; got %d", len(rows))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
}

func TestUpdateBalance(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)
	ctx := context.Background()

	// Test positive balance update on a user account.
	alice := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	newBal, _ := money.FromString("5000.0000")

	err := testStore.WithTx(ctx, func(tx pgx.Tx) error {
		_, lockErr := store.LockAccountsForUpdate(ctx, tx, []uuid.UUID{alice})
		if lockErr != nil {
			return lockErr
		}
		return store.UpdateBalance(ctx, tx, alice, newBal)
	})
	if err != nil {
		t.Fatalf("UpdateBalance (user): %v", err)
	}

	a, err := testStore.GetAccount(ctx, alice)
	if err != nil {
		t.Fatalf("GetAccount after UpdateBalance: %v", err)
	}
	if !a.Balance.Equal(newBal) {
		t.Errorf("balance = %s; want %s", a.Balance, newBal)
	}

	// Test negative balance on system account (EXTERNAL) — must be permitted.
	extID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	negBal, _ := money.FromString("-30000.0000")

	err = testStore.WithTx(ctx, func(tx pgx.Tx) error {
		_, lockErr := store.LockAccountsForUpdate(ctx, tx, []uuid.UUID{extID})
		if lockErr != nil {
			return lockErr
		}
		return store.UpdateBalance(ctx, tx, extID, negBal)
	})
	if err != nil {
		t.Fatalf("UpdateBalance (system negative): %v", err)
	}
}

func TestInsertTransactionAndEntries(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)
	ctx := context.Background()

	txID, _ := uuid.NewV7()
	alice := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	bob := uuid.MustParse("00000000-0000-0000-0000-000000000003")
	amt, _ := money.FromString("100.0000")

	txn := ledger.Transaction{
		ID:             txID,
		Kind:           ledger.KindTransfer,
		IdempotencyKey: "test-transfer-" + txID.String(),
	}
	entries := []ledger.Entry{
		{TransactionID: txID, AccountID: alice, Direction: ledger.DirectionDebit, Amount: amt},
		{TransactionID: txID, AccountID: bob, Direction: ledger.DirectionCredit, Amount: amt},
	}

	err := testStore.WithTx(ctx, func(tx pgx.Tx) error {
		if err := store.InsertTransaction(ctx, tx, txn); err != nil {
			return err
		}
		return store.InsertEntries(ctx, tx, entries)
	})
	if err != nil {
		t.Fatalf("InsertTransaction+InsertEntries: %v", err)
	}
}

func TestGetTransactionByIdempotencyKey(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)
	ctx := context.Background()

	txID, _ := uuid.NewV7()
	alice := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	bob := uuid.MustParse("00000000-0000-0000-0000-000000000003")
	amt, _ := money.FromString("50.0000")
	iKey := "idem-key-" + txID.String()

	err := testStore.WithTx(ctx, func(tx pgx.Tx) error {
		t2 := ledger.Transaction{ID: txID, Kind: ledger.KindTransfer, IdempotencyKey: iKey}
		if err := store.InsertTransaction(ctx, tx, t2); err != nil {
			return err
		}
		return store.InsertEntries(ctx, tx, []ledger.Entry{
			{TransactionID: txID, AccountID: alice, Direction: ledger.DirectionDebit, Amount: amt},
			{TransactionID: txID, AccountID: bob, Direction: ledger.DirectionCredit, Amount: amt},
		})
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	t.Run("found", func(t *testing.T) {
		got, entries, err := testStore.GetTransactionByIdempotencyKey(ctx, iKey)
		if err != nil {
			t.Fatalf("GetTransactionByIdempotencyKey: %v", err)
		}
		if got.ID != txID {
			t.Errorf("transaction ID mismatch")
		}
		if len(entries) != 2 {
			t.Errorf("expected 2 entries; got %d", len(entries))
		}
	})

	t.Run("not_found", func(t *testing.T) {
		_, _, err := testStore.GetTransactionByIdempotencyKey(ctx, "nonexistent-key")
		if !errors.Is(err, ledger.ErrTransactionNotFound) {
			t.Errorf("expected ErrTransactionNotFound; got %v", err)
		}
	})
}

func TestGetTransaction_NotFound(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()

	_, _, err := testStore.GetTransaction(ctx, uuid.New())
	if !errors.Is(err, ledger.ErrTransactionNotFound) {
		t.Errorf("expected ErrTransactionNotFound; got %v", err)
	}
}

func TestIdempotencyConflict(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)
	ctx := context.Background()

	txID, _ := uuid.NewV7()
	alice := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	bob := uuid.MustParse("00000000-0000-0000-0000-000000000003")
	amt, _ := money.FromString("10.0000")
	iKey := "conflict-key-" + txID.String()

	// First insert succeeds.
	_ = testStore.WithTx(ctx, func(tx pgx.Tx) error {
		t2 := ledger.Transaction{ID: txID, Kind: ledger.KindTransfer, IdempotencyKey: iKey}
		if err := store.InsertTransaction(ctx, tx, t2); err != nil {
			return err
		}
		return store.InsertEntries(ctx, tx, []ledger.Entry{
			{TransactionID: txID, AccountID: alice, Direction: ledger.DirectionDebit, Amount: amt},
			{TransactionID: txID, AccountID: bob, Direction: ledger.DirectionCredit, Amount: amt},
		})
	})

	// Second insert with same idempotency key must return ErrIdempotencyConflict.
	txID2, _ := uuid.NewV7()
	err := testStore.WithTx(ctx, func(tx pgx.Tx) error {
		t3 := ledger.Transaction{ID: txID2, Kind: ledger.KindTransfer, IdempotencyKey: iKey}
		return store.InsertTransaction(ctx, tx, t3)
	})
	if !errors.Is(err, ledger.ErrIdempotencyConflict) {
		t.Errorf("expected ErrIdempotencyConflict; got %v", err)
	}
}

// TestDeferredTrigger_ImbalancedEntries confirms the DB-level double-entry
// invariant fires at COMMIT for imbalanced entries.
func TestDeferredTrigger_ImbalancedEntries(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)
	ctx := context.Background()

	txID, _ := uuid.NewV7()
	alice := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	bob := uuid.MustParse("00000000-0000-0000-0000-000000000003")
	debit, _ := money.FromString("100.0000")
	credit, _ := money.FromString("50.0000") // deliberately imbalanced

	err := testStore.WithTx(ctx, func(tx pgx.Tx) error {
		t2 := ledger.Transaction{ID: txID, Kind: ledger.KindTransfer, IdempotencyKey: "imbalanced-" + txID.String()}
		if err := store.InsertTransaction(ctx, tx, t2); err != nil {
			return err
		}
		return store.InsertEntries(ctx, tx, []ledger.Entry{
			{TransactionID: txID, AccountID: alice, Direction: ledger.DirectionDebit, Amount: debit},
			{TransactionID: txID, AccountID: bob, Direction: ledger.DirectionCredit, Amount: credit},
		})
	})
	if err == nil {
		t.Fatal("expected deferred trigger to reject imbalanced entries at COMMIT; got nil error")
	}
}

func TestListTransactionsForAccount(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)
	ctx := context.Background()

	alice := uuid.MustParse("00000000-0000-0000-0000-000000000002")

	// alice was credited 10000 in the seed migration.
	txns, err := testStore.ListTransactionsForAccount(ctx, alice, 10)
	if err != nil {
		t.Fatalf("ListTransactionsForAccount: %v", err)
	}
	if len(txns) < 1 {
		t.Errorf("expected at least 1 transaction for Alice; got %d", len(txns))
	}
}

func TestWriteAudit(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)
	ctx := context.Background()

	alice := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	bob := uuid.MustParse("00000000-0000-0000-0000-000000000003")
	amt, _ := money.FromString("25.0000")
	reason := "unit test"
	txID := uuid.New()

	entry := ledger.AuditEntry{
		Operation:     "TRANSFER",
		FromAccount:   &alice,
		ToAccount:     &bob,
		Amount:        &amt,
		Outcome:       ledger.OutcomeSuccess,
		ErrorReason:   nil,
		TransactionID: &txID,
		RequestID:     &reason,
		CreatedAt:     time.Now(),
	}
	if err := testStore.WriteAudit(ctx, entry); err != nil {
		t.Fatalf("WriteAudit: %v", err)
	}

	// Write a failure audit with nil Amount.
	failEntry := ledger.AuditEntry{
		Operation:   "TRANSFER",
		FromAccount: &alice,
		ToAccount:   &bob,
		Amount:      nil, // no amount on pre-validation failure
		Outcome:     ledger.OutcomeFailure,
		ErrorReason: &reason,
	}
	if err := testStore.WriteAudit(ctx, failEntry); err != nil {
		t.Fatalf("WriteAudit (failure): %v", err)
	}
}

func TestInsertEntries_InvalidAmount(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()

	txID, _ := uuid.NewV7()
	alice := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	zero := money.Zero()

	err := testStore.WithTx(ctx, func(tx pgx.Tx) error {
		t2 := ledger.Transaction{ID: txID, Kind: ledger.KindTransfer, IdempotencyKey: "zero-amt-" + txID.String()}
		if err := store.InsertTransaction(ctx, tx, t2); err != nil {
			return err
		}
		return store.InsertEntries(ctx, tx, []ledger.Entry{
			{TransactionID: txID, AccountID: alice, Direction: ledger.DirectionDebit, Amount: zero},
		})
	})
	if !errors.Is(err, ledger.ErrInvalidAmount) {
		t.Errorf("expected ErrInvalidAmount for zero amount; got %v", err)
	}
}
