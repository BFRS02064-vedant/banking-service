// Package store_test — concurrency, idempotency, migration-round-trip, and
// JSON nil-pointer tests that complement integration_test.go.
//
// All DB-touching tests in this file require Docker (testcontainers).  They
// call skipIfNoDocker and are therefore safe to run on machines without Docker.
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
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

// ---------------------------------------------------------------------------
// 1. Race / concurrency test: LockAccountsForUpdate prevents deadlock
//
// Spawn N goroutines each running a WithTx block that calls
// LockAccountsForUpdate with the SAME pair of accounts in alternating input
// order (odd goroutines: [alice,bob], even: [bob,alice]), then performs a
// balanced balance swap: alice +1, bob -1.
//
// After all goroutines finish:
//   - assert no deadlock (test completes before context deadline)
//   - assert alice.balance + bob.balance == original seeded total (conservation)
// ---------------------------------------------------------------------------

func TestLockAccountsForUpdate_ConcurrentNGoroutines_NoDeadlock(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	alice := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	bob := uuid.MustParse("00000000-0000-0000-0000-000000000003")

	const N = 50 // 50 concurrent transfer pairs
	inc, _ := money.FromString("1.0000")

	type result struct{ err error }
	results := make([]result, N)

	var wg sync.WaitGroup
	wg.Add(N)
	for i := range N {
		go func() {
			defer wg.Done()
			// Alternate the input order on every goroutine to stress the sort.
			var ids []uuid.UUID
			if i%2 == 0 {
				ids = []uuid.UUID{alice, bob}
			} else {
				ids = []uuid.UUID{bob, alice}
			}
			results[i].err = testStore.WithTx(ctx, func(tx pgx.Tx) error {
				locked, err := store.LockAccountsForUpdate(ctx, tx, ids)
				if err != nil {
					return fmt.Errorf("lock: %w", err)
				}
				if len(locked) != 2 {
					return fmt.Errorf("expected 2 locked rows; got %d", len(locked))
				}
				var aliceBal, bobBal money.Money
				for _, a := range locked {
					switch a.ID {
					case alice:
						aliceBal = a.Balance
					case bob:
						bobBal = a.Balance
					}
				}
				if err := store.UpdateBalance(ctx, tx, alice, aliceBal.Add(inc)); err != nil {
					return fmt.Errorf("update alice: %w", err)
				}
				if err := store.UpdateBalance(ctx, tx, bob, bobBal.Sub(inc)); err != nil {
					return fmt.Errorf("update bob: %w", err)
				}
				return nil
			})
		}()
	}

	// Wait with the context deadline; if we deadlock, the context fires first.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		// all goroutines completed
	case <-ctx.Done():
		t.Fatal("goroutines did not finish within deadline — possible deadlock")
	}

	for i, r := range results {
		if r.err != nil {
			t.Errorf("goroutine %d returned error: %v", i, r.err)
		}
	}

	// Conservation invariant: alice + bob == 20000 (seed 10000 each).
	aliceAcc, err := testStore.GetAccount(ctx, alice)
	if err != nil {
		t.Fatalf("GetAccount alice: %v", err)
	}
	bobAcc, err := testStore.GetAccount(ctx, bob)
	if err != nil {
		t.Fatalf("GetAccount bob: %v", err)
	}
	total := aliceAcc.Balance.Add(bobAcc.Balance)
	expected, _ := money.FromString("20000.0000")
	if !total.Equal(expected) {
		t.Errorf("balance conservation violated: alice=%s bob=%s total=%s; want 20000.0000",
			aliceAcc.Balance, bobAcc.Balance, total)
	}
}

// ---------------------------------------------------------------------------
// 2. Idempotency-key conflict under concurrency
//
// Two goroutines race to InsertTransaction with the SAME idempotency key.
// Exactly one must succeed; the other must return ErrIdempotencyConflict.
// Total committed transaction rows for that key must be exactly 1.
// ---------------------------------------------------------------------------

func TestInsertTransaction_ConcurrentSameIdempotencyKey_ExactlyOneSucceeds(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	ctx := context.Background()
	iKey := "concurrent-idem-" + uuid.New().String()
	alice := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	bob := uuid.MustParse("00000000-0000-0000-0000-000000000003")
	amt, _ := money.FromString("1.0000")

	type result struct{ err error }
	results := make([]result, 2)

	var wg sync.WaitGroup
	wg.Add(2)
	for i := range 2 {
		go func() {
			defer wg.Done()
			txID, _ := uuid.NewV7()
			results[i].err = testStore.WithTx(ctx, func(tx pgx.Tx) error {
				t2 := ledger.Transaction{
					ID:             txID,
					Kind:           ledger.KindTransfer,
					IdempotencyKey: iKey,
				}
				if err := store.InsertTransaction(ctx, tx, t2); err != nil {
					return err
				}
				return store.InsertEntries(ctx, tx, []ledger.Entry{
					{TransactionID: txID, AccountID: alice, Direction: ledger.DirectionDebit, Amount: amt},
					{TransactionID: txID, AccountID: bob, Direction: ledger.DirectionCredit, Amount: amt},
				})
			})
		}()
	}
	wg.Wait()

	successCount := 0
	conflictCount := 0
	for _, r := range results {
		if r.err == nil {
			successCount++
		} else if errors.Is(r.err, ledger.ErrIdempotencyConflict) {
			conflictCount++
		} else {
			t.Errorf("unexpected error (not nil, not ErrIdempotencyConflict): %v", r.err)
		}
	}
	if successCount != 1 || conflictCount != 1 {
		t.Errorf("expected exactly 1 success + 1 ErrIdempotencyConflict; got %d success + %d conflict",
			successCount, conflictCount)
	}

	// Exactly one transaction row committed.
	_, _, err := testStore.GetTransactionByIdempotencyKey(ctx, iKey)
	if err != nil {
		t.Fatalf("GetTransactionByIdempotencyKey after concurrent inserts: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 3. Deferred trigger semantics: error surfaces at COMMIT, not at INSERT
//
// InsertEntries with imbalanced amounts MUST NOT return an error.
// The error must surface from WithTx at Commit time.
// ---------------------------------------------------------------------------

func TestDeferredTrigger_ErrorAtCommitNotAtInsert(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	ctx := context.Background()
	txID, _ := uuid.NewV7()
	alice := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	bob := uuid.MustParse("00000000-0000-0000-0000-000000000003")
	debit, _ := money.FromString("100.0000")
	credit, _ := money.FromString("50.0000") // deliberately imbalanced

	var insertEntriesErr error
	withTxErr := testStore.WithTx(ctx, func(tx pgx.Tx) error {
		t2 := ledger.Transaction{
			ID:             txID,
			Kind:           ledger.KindTransfer,
			IdempotencyKey: "deferred-commit-test-" + txID.String(),
		}
		if err := store.InsertTransaction(ctx, tx, t2); err != nil {
			return err
		}
		insertEntriesErr = store.InsertEntries(ctx, tx, []ledger.Entry{
			{TransactionID: txID, AccountID: alice, Direction: ledger.DirectionDebit, Amount: debit},
			{TransactionID: txID, AccountID: bob, Direction: ledger.DirectionCredit, Amount: credit},
		})
		// Explicitly return nil to force WithTx to reach the Commit call.
		// The deferred trigger fires at Commit, not at InsertEntries.
		return nil
	})

	if insertEntriesErr != nil {
		t.Errorf("InsertEntries returned error before COMMIT — trigger is NOT deferred: %v", insertEntriesErr)
	}
	if withTxErr == nil {
		t.Fatal("WithTx succeeded with imbalanced entries — deferred trigger did not fire at COMMIT")
	}
}

// ---------------------------------------------------------------------------
// 4. Money JSON nil-pointer marshaling
//
// Confirm json.Marshal on a struct with a nil *Money field produces
// {"A":null} and not {"A":"0.0000"} or an error.
// This is a pure unit test that requires no Docker.
// ---------------------------------------------------------------------------

func TestMoney_NilPointerJSONMarshal(t *testing.T) {
	t.Parallel()
	type wrapper struct {
		A *money.Money `json:"A"`
	}
	got, err := json.Marshal(wrapper{A: nil})
	if err != nil {
		t.Fatalf("json.Marshal nil *Money: %v", err)
	}
	want := `{"A":null}`
	if string(got) != want {
		t.Errorf("nil *Money marshal = %s; want %s", string(got), want)
	}
}

// ---------------------------------------------------------------------------
// 5. Migration round-trip: Up → Down → Up
//
// Spins up a dedicated Postgres container isolated from the shared testStore,
// then runs migrate.Up(), migrate.Down(), migrate.Up() and verifies:
//   - Down migrations complete without FK violations.
//   - A second Up run is clean.
//   - The accounts table is populated after the second Up.
// ---------------------------------------------------------------------------

func TestMigration_UpDownUp_RoundTrip(t *testing.T) {
	skipIfNoDocker(t)

	ctx := context.Background()

	pgContainer, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("roundtripdb"),
		tcpostgres.WithUsername("rtuser"),
		tcpostgres.WithPassword("rtpass"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Skipf("failed to start dedicated Postgres container for migration test: %v", err)
	}
	defer func() { _ = pgContainer.Terminate(ctx) }()

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("ConnectionString: %v", err)
	}

	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	migrationsDir, _ = filepath.Abs(migrationsDir)
	migrationURL := "file://" + migrationsDir

	// --- First Up ---
	mg1, err := migrate.New(migrationURL, dsn)
	if err != nil {
		t.Fatalf("migrate.New (first Up): %v", err)
	}
	if err := mg1.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate.Up (first): %v", err)
	}
	mg1.Close()

	// Spot-check: accounts table must have at least 4 rows (1 EXTERNAL + 3 demo).
	pool1, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New after first Up: %v", err)
	}
	var count int
	if err := pool1.QueryRow(ctx, "SELECT COUNT(*) FROM accounts").Scan(&count); err != nil {
		t.Fatalf("SELECT COUNT(*) FROM accounts (after first Up): %v", err)
	}
	if count < 4 {
		t.Errorf("expected >= 4 account rows after first Up; got %d", count)
	}
	pool1.Close()

	// --- Down (all migrations) ---
	mg2, err := migrate.New(migrationURL, dsn)
	if err != nil {
		t.Fatalf("migrate.New (Down): %v", err)
	}
	if err := mg2.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate.Down: %v", err)
	}
	mg2.Close()

	// Spot-check: accounts table must no longer exist.
	pool2, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New after Down: %v", err)
	}
	var dummy int
	downErr := pool2.QueryRow(ctx, "SELECT COUNT(*) FROM accounts").Scan(&dummy)
	if downErr == nil {
		t.Error("expected error querying accounts after migrate.Down; table should not exist")
	}
	pool2.Close()

	// --- Second Up ---
	mg3, err := migrate.New(migrationURL, dsn)
	if err != nil {
		t.Fatalf("migrate.New (second Up): %v", err)
	}
	if err := mg3.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate.Up (second): %v", err)
	}
	mg3.Close()

	// Spot-check: accounts table repopulated.
	pool3, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New after second Up: %v", err)
	}
	defer pool3.Close()
	if err := pool3.QueryRow(ctx, "SELECT COUNT(*) FROM accounts").Scan(&count); err != nil {
		t.Fatalf("SELECT COUNT(*) FROM accounts (after second Up): %v", err)
	}
	if count < 4 {
		t.Errorf("expected >= 4 account rows after second Up; got %d", count)
	}
}
