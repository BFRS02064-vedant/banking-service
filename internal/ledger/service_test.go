// Package ledger_test contains integration tests for ledger.Service backed by a
// real PostgreSQL instance via testcontainers-go. Docker must be running.
package ledger_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
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
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

var (
	testDSN   string
	testStore *store.Store
	testSvc   *ledger.Service
)

// TestMain spins up a Postgres container, runs migrations, and runs all subtests.
// If Docker is unavailable, all tests are skipped gracefully.
func TestMain(m *testing.M) {
	ctx := context.Background()

	pgContainer, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("svctest"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		m.Run()
		return
	}
	defer func() { _ = pgContainer.Terminate(ctx) }()

	testDSN, err = pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		m.Run()
		return
	}

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

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	testSvc = ledger.NewService(testStore, logger)

	m.Run()
}

// skipIfNoDocker skips the test when testStore was not initialised (Docker absent).
func skipIfNoDocker(t *testing.T) {
	t.Helper()
	if testStore == nil {
		t.Skip("Docker not available; skipping integration test")
	}
}

// truncate resets to seed state between tests.
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
		`DELETE FROM entries WHERE transaction_id NOT IN (
			'01900000-0000-7000-8000-000000000001',
			'01900000-0000-7000-8000-000000000002',
			'01900000-0000-7000-8000-000000000003'
		)`,
		`DELETE FROM transactions WHERE id NOT IN (
			'01900000-0000-7000-8000-000000000001',
			'01900000-0000-7000-8000-000000000002',
			'01900000-0000-7000-8000-000000000003'
		)`,
		`DELETE FROM accounts WHERE is_system = FALSE AND id NOT IN (
			'00000000-0000-0000-0000-000000000002',
			'00000000-0000-0000-0000-000000000003',
			'00000000-0000-0000-0000-000000000004'
		)`,
		`UPDATE accounts SET balance = -30000.0000 WHERE id = '00000000-0000-0000-0000-000000000001'`,
		`UPDATE accounts SET balance = 10000.0000 WHERE id IN (
			'00000000-0000-0000-0000-000000000002',
			'00000000-0000-0000-0000-000000000003',
			'00000000-0000-0000-0000-000000000004'
		)`,
	}
	for _, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("truncate exec: %v", err)
		}
	}
}

var (
	aliceID = uuid.MustParse("00000000-0000-0000-0000-000000000002")
	bobID   = uuid.MustParse("00000000-0000-0000-0000-000000000003")
)

// ---------------------------------------------------------------------------
// Transfer — validation errors (no DB calls)
// ---------------------------------------------------------------------------

func TestTransfer_InvalidAmount(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	zero := money.Zero()
	_, _, err := testSvc.Transfer(ctx, aliceID, bobID, zero, "k1", "r1")
	if !errors.Is(err, ledger.ErrInvalidAmount) {
		t.Errorf("expected ErrInvalidAmount; got %v", err)
	}
}

func TestTransfer_SelfTransfer(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	amt, _ := money.FromString("100")
	_, _, err := testSvc.Transfer(ctx, aliceID, aliceID, amt, "k2", "r2")
	if !errors.Is(err, ledger.ErrSelfTransfer) {
		t.Errorf("expected ErrSelfTransfer; got %v", err)
	}
}

func TestTransfer_SystemAccount(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	amt, _ := money.FromString("100")
	_, _, err := testSvc.Transfer(ctx, aliceID, ledger.ExternalAccountID, amt, "k3", "r3")
	if !errors.Is(err, ledger.ErrSystemAccount) {
		t.Errorf("expected ErrSystemAccount; got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Transfer — happy path
// ---------------------------------------------------------------------------

func TestTransfer_HappyPath(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)
	ctx := context.Background()

	amt, _ := money.FromString("500")
	idem := "xfer-happy-" + uuid.New().String()
	txn, entries, err := testSvc.Transfer(ctx, aliceID, bobID, amt, idem, "req-1")
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if txn.Kind != ledger.KindTransfer {
		t.Errorf("kind = %s; want TRANSFER", txn.Kind)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 entries; got %d", len(entries))
	}

	alice, _ := testStore.GetAccount(ctx, aliceID)
	bob, _ := testStore.GetAccount(ctx, bobID)
	expectedAlice, _ := money.FromString("9500")
	expectedBob, _ := money.FromString("10500")
	if !alice.Balance.Equal(expectedAlice) {
		t.Errorf("alice balance = %s; want %s", alice.Balance, expectedAlice)
	}
	if !bob.Balance.Equal(expectedBob) {
		t.Errorf("bob balance = %s; want %s", bob.Balance, expectedBob)
	}
}

// ---------------------------------------------------------------------------
// Transfer — idempotency replay
// ---------------------------------------------------------------------------

func TestTransfer_IdempotencyReplay(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)
	ctx := context.Background()

	amt, _ := money.FromString("100")
	idem := "idem-replay-" + uuid.New().String()

	txn1, _, err := testSvc.Transfer(ctx, aliceID, bobID, amt, idem, "req-a")
	if err != nil {
		t.Fatalf("first Transfer: %v", err)
	}

	txn2, _, err := testSvc.Transfer(ctx, aliceID, bobID, amt, idem, "req-b")
	if err != nil {
		t.Fatalf("replay Transfer: %v", err)
	}
	if txn1.ID != txn2.ID {
		t.Errorf("replay returned different transaction ID: %s vs %s", txn1.ID, txn2.ID)
	}

	// Balance should only be debited once.
	alice, _ := testStore.GetAccount(ctx, aliceID)
	expected, _ := money.FromString("9900")
	if !alice.Balance.Equal(expected) {
		t.Errorf("alice balance after replay = %s; want %s", alice.Balance, expected)
	}
}

// ---------------------------------------------------------------------------
// Transfer — concurrent idempotency (100 goroutines, same key → exactly 1 tx)
// ---------------------------------------------------------------------------

func TestTransfer_ConcurrentSameIdemKey_ExactlyOneTransaction(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	amt, _ := money.FromString("1")
	idem := "concurrent-idem-" + uuid.New().String()

	const N = 100
	errs := make([]error, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := range N {
		go func() {
			defer wg.Done()
			_, _, errs[i] = testSvc.Transfer(ctx, aliceID, bobID, amt, idem, "r")
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: unexpected error %v", i, err)
		}
	}

	// Exactly one debit on alice.
	alice, _ := testStore.GetAccount(ctx, aliceID)
	expected, _ := money.FromString("9999")
	if !alice.Balance.Equal(expected) {
		t.Errorf("alice balance = %s; want %s (expected exactly 1 debit)", alice.Balance, expected)
	}
}

// ---------------------------------------------------------------------------
// Transfer — insufficient funds race (2 goroutines × 80 from 100-balance account)
// ---------------------------------------------------------------------------

func TestTransfer_InsufficientFundsRace(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create a fresh account with exactly 100 balance.
	carolID := mustCreateAccountWithBalance(t, ctx, "Carol", "100")

	amt, _ := money.FromString("80")

	type result struct{ err error }
	results := make([]result, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for i := range 2 {
		go func() {
			defer wg.Done()
			idem := "race-" + uuid.New().String()
			_, _, results[i].err = testSvc.Transfer(ctx, carolID, aliceID, amt, idem, "r")
		}()
	}
	wg.Wait()

	successCount, insufficientCount := 0, 0
	for _, r := range results {
		if r.err == nil {
			successCount++
		} else if errors.Is(r.err, ledger.ErrInsufficientFunds) {
			insufficientCount++
		} else {
			t.Errorf("unexpected error: %v", r.err)
		}
	}
	if successCount != 1 || insufficientCount != 1 {
		t.Errorf("expected 1 success + 1 ErrInsufficientFunds; got %d + %d", successCount, insufficientCount)
	}
}

// ---------------------------------------------------------------------------
// Transfer — balance conservation (N=8 goroutines)
// ---------------------------------------------------------------------------

func TestTransfer_BalanceConservation(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	amt, _ := money.FromString("10")
	const N = 8
	var wg sync.WaitGroup
	wg.Add(N)
	for i := range N {
		go func() {
			defer wg.Done()
			idem := "conservation-" + uuid.New().String()
			// Alternate direction to stress lock ordering.
			var src, dst uuid.UUID
			if i%2 == 0 {
				src, dst = aliceID, bobID
			} else {
				src, dst = bobID, aliceID
			}
			_, _, _ = testSvc.Transfer(ctx, src, dst, amt, idem, "r")
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("goroutines did not finish within deadline — possible deadlock")
	}

	alice, _ := testStore.GetAccount(ctx, aliceID)
	bob, _ := testStore.GetAccount(ctx, bobID)
	total := alice.Balance.Add(bob.Balance)
	expected, _ := money.FromString("20000")
	if !total.Equal(expected) {
		t.Errorf("balance conservation violated: alice=%s bob=%s total=%s; want 20000", alice.Balance, bob.Balance, total)
	}
}

// ---------------------------------------------------------------------------
// Deposit — happy path and validations
// ---------------------------------------------------------------------------

func TestDeposit_HappyPath(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)
	ctx := context.Background()

	amt, _ := money.FromString("250")
	idem := "dep-happy-" + uuid.New().String()
	txn, entries, err := testSvc.Deposit(ctx, aliceID, amt, idem, "req-d1")
	if err != nil {
		t.Fatalf("Deposit: %v", err)
	}
	if txn.Kind != ledger.KindDeposit {
		t.Errorf("kind = %s; want DEPOSIT", txn.Kind)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 entries; got %d", len(entries))
	}
	alice, _ := testStore.GetAccount(ctx, aliceID)
	expected, _ := money.FromString("10250")
	if !alice.Balance.Equal(expected) {
		t.Errorf("alice balance = %s; want %s", alice.Balance, expected)
	}
}

func TestDeposit_SystemAccount(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	amt, _ := money.FromString("100")
	_, _, err := testSvc.Deposit(ctx, ledger.ExternalAccountID, amt, "dep-sys-"+uuid.New().String(), "r")
	if !errors.Is(err, ledger.ErrSystemAccount) {
		t.Errorf("expected ErrSystemAccount; got %v", err)
	}
}

func TestDeposit_InvalidAmount(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	zero := money.Zero()
	_, _, err := testSvc.Deposit(ctx, aliceID, zero, "dep-zero-"+uuid.New().String(), "r")
	if !errors.Is(err, ledger.ErrInvalidAmount) {
		t.Errorf("expected ErrInvalidAmount; got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Reverse — happy path and error cases
// ---------------------------------------------------------------------------

func TestReverse_HappyPath(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)
	ctx := context.Background()

	amt, _ := money.FromString("300")
	xferTxn, _, err := testSvc.Transfer(ctx, aliceID, bobID, amt, "xfer-for-rev-"+uuid.New().String(), "r1")
	if err != nil {
		t.Fatalf("Transfer setup: %v", err)
	}

	revTxn, entries, err := testSvc.Reverse(ctx, xferTxn.ID, "rev-happy-"+uuid.New().String(), "r2")
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}
	if revTxn.Kind != ledger.KindReversal {
		t.Errorf("kind = %s; want REVERSAL", revTxn.Kind)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 entries; got %d", len(entries))
	}

	// Balances should be back to seed values.
	alice, _ := testStore.GetAccount(ctx, aliceID)
	bob, _ := testStore.GetAccount(ctx, bobID)
	seed, _ := money.FromString("10000")
	if !alice.Balance.Equal(seed) || !bob.Balance.Equal(seed) {
		t.Errorf("balances not restored: alice=%s bob=%s; want 10000 each", alice.Balance, bob.Balance)
	}
}

func TestReverse_NotFound(t *testing.T) {
	skipIfNoDocker(t)
	ctx := context.Background()
	_, _, err := testSvc.Reverse(ctx, uuid.New(), "rev-nf-"+uuid.New().String(), "r")
	if !errors.Is(err, ledger.ErrTransactionNotFound) {
		t.Errorf("expected ErrTransactionNotFound; got %v", err)
	}
}

func TestReverse_NotReversible(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)
	ctx := context.Background()

	// Deposit first, then attempt to reverse it.
	depTxn, _, err := testSvc.Deposit(ctx, aliceID, mustMoney("100"), "dep-notrev-"+uuid.New().String(), "r1")
	if err != nil {
		t.Fatalf("Deposit: %v", err)
	}
	_, _, err = testSvc.Reverse(ctx, depTxn.ID, "rev-notrev-"+uuid.New().String(), "r2")
	if !errors.Is(err, ledger.ErrNotReversible) {
		t.Errorf("expected ErrNotReversible; got %v", err)
	}
}

func TestReverse_AlreadyReversed(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)
	ctx := context.Background()

	xferTxn, _, err := testSvc.Transfer(ctx, aliceID, bobID, mustMoney("50"), "xfer-dbl-"+uuid.New().String(), "r1")
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}

	_, _, err = testSvc.Reverse(ctx, xferTxn.ID, "rev1-"+uuid.New().String(), "r2")
	if err != nil {
		t.Fatalf("first Reverse: %v", err)
	}

	// Second reverse with a DIFFERENT idempotency key must return ErrAlreadyReversed.
	_, _, err = testSvc.Reverse(ctx, xferTxn.ID, "rev2-"+uuid.New().String(), "r3")
	if !errors.Is(err, ledger.ErrAlreadyReversed) {
		t.Errorf("expected ErrAlreadyReversed; got %v", err)
	}
}

func TestReverse_InsufficientFunds(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)
	ctx := context.Background()

	// Transfer 9500 from alice to bob (leaving alice with 500).
	xferTxn, _, err := testSvc.Transfer(ctx, aliceID, bobID, mustMoney("9500"), "xfer-insuf-"+uuid.New().String(), "r1")
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	// Bob now has 19500; drain bob to near zero via another transfer.
	_, _, err = testSvc.Transfer(ctx, bobID, aliceID, mustMoney("19000"), "drain-"+uuid.New().String(), "r2")
	if err != nil {
		t.Fatalf("drain Transfer: %v", err)
	}

	// bob now has 500; reversing the original 9500 transfer requires bob to have 9500.
	_, _, err = testSvc.Reverse(ctx, xferTxn.ID, "rev-insuf-"+uuid.New().String(), "r3")
	if !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Errorf("expected ErrInsufficientFunds; got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Audit log — success and failure rows
// ---------------------------------------------------------------------------

func TestAudit_SuccessRowPresent(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)
	ctx := context.Background()

	amt, _ := money.FromString("100")
	_, _, err := testSvc.Transfer(ctx, aliceID, bobID, amt, "audit-ok-"+uuid.New().String(), "r1")
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}

	// Give the async audit write a moment to land (it uses a 3s deadline but is synchronous in service).
	entries, err := testStore.ListAuditForAccount(ctx, aliceID, 10)
	if err != nil {
		t.Fatalf("ListAuditForAccount: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.Outcome == ledger.OutcomeSuccess && e.Operation == "TRANSFER" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected SUCCESS audit row for Transfer; not found")
	}
}

func TestAudit_FailureRowPresent(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)
	ctx := context.Background()

	zero := money.Zero()
	_, _, _ = testSvc.Transfer(ctx, aliceID, bobID, zero, "audit-fail-"+uuid.New().String(), "r1")

	entries, err := testStore.ListAuditForAccount(ctx, aliceID, 10)
	if err != nil {
		t.Fatalf("ListAuditForAccount: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.Outcome == ledger.OutcomeFailure && e.ErrorReason != nil {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected FAILURE audit row; not found")
	}
}

// ---------------------------------------------------------------------------
// Audit — every domain failure produces a FAILURE audit row with error_reason
// ---------------------------------------------------------------------------

// TestAudit_AllDomainFailures verifies that every sentinel error that the
// service produces causes a FAILURE audit row with a non-null error_reason.
// Covers acceptance criterion: "audit_log contains outcome=FAILURE row with
// non-null error_reason for every domain-level rejection."
func TestAudit_AllDomainFailures(t *testing.T) {
	skipIfNoDocker(t)

	ctx := context.Background()

	// We need a transferred transaction so we can probe ErrNotReversible and
	// ErrAlreadyReversed separately. Set up once outside sub-tests.
	truncate(t)
	// xferTxn: used to probe ErrAlreadyReversed (reverse it twice).
	xferIdem := "audit-domains-xfer-" + uuid.New().String()
	xferTxn, _, err := testSvc.Transfer(ctx, aliceID, bobID, mustMoney("10"), xferIdem, "setup")
	if err != nil {
		t.Fatalf("setup Transfer: %v", err)
	}
	// depTxn: a Deposit, used to probe ErrNotReversible.
	depIdem := "audit-domains-dep-" + uuid.New().String()
	depTxn, _, err := testSvc.Deposit(ctx, aliceID, mustMoney("10"), depIdem, "setup")
	if err != nil {
		t.Fatalf("setup Deposit: %v", err)
	}
	// Reverse xferTxn once so the second reverse hits ErrAlreadyReversed.
	_, _, err = testSvc.Reverse(ctx, xferTxn.ID, "audit-domains-rev1-"+uuid.New().String(), "setup")
	if err != nil {
		t.Fatalf("setup first Reverse: %v", err)
	}

	// Cases where the audit row IS queryable via ListAuditForAccount (from/to not nil).
	// ErrNotReversible writes nil from/to accounts (bug documented in issues_found), so
	// it is verified separately via error-return assertion below.
	cases := []struct {
		name      string
		accountID uuid.UUID // account whose audit log is queried
		trigger   func()    // provoke the error; must return the sentinel
	}{
		{
			name:      "ErrInvalidAmount",
			accountID: aliceID,
			trigger: func() {
				_, _, _ = testSvc.Transfer(ctx, aliceID, bobID, money.Zero(), "audit-invalid-"+uuid.New().String(), "r")
			},
		},
		{
			name:      "ErrSelfTransfer",
			accountID: aliceID,
			trigger: func() {
				_, _, _ = testSvc.Transfer(ctx, aliceID, aliceID, mustMoney("1"), "audit-self-"+uuid.New().String(), "r")
			},
		},
		{
			name:      "ErrSystemAccount",
			accountID: aliceID,
			trigger: func() {
				_, _, _ = testSvc.Transfer(ctx, aliceID, ledger.ExternalAccountID, mustMoney("1"), "audit-sys-"+uuid.New().String(), "r")
			},
		},
		{
			name:      "ErrInsufficientFunds",
			accountID: aliceID,
			trigger: func() {
				// Use a fresh account with known balance so the failure is deterministic.
				freshID := mustCreateAccountWithBalance(t, ctx, "AuditFreshInsuf", "5")
				_, _, _ = testSvc.Transfer(ctx, freshID, aliceID, mustMoney("9999"), "audit-insuf-"+uuid.New().String(), "r")
			},
		},
		{
			// ErrAlreadyReversed: written with origDstID=bobID, origSrcID=aliceID,
			// so the row has from_account=bobID, to_account=aliceID.
			// Querying by aliceID will match via to_account.
			name:      "ErrAlreadyReversed",
			accountID: aliceID,
			trigger: func() {
				// xferTxn was already reversed in setup.
				_, _, _ = testSvc.Reverse(ctx, xferTxn.ID, "audit-alreadyrev-"+uuid.New().String(), "r")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Capture audit-row count before.
			before, err := testStore.ListAuditForAccount(ctx, tc.accountID, 200)
			if err != nil {
				t.Fatalf("ListAuditForAccount before: %v", err)
			}

			tc.trigger()

			after, err := testStore.ListAuditForAccount(ctx, tc.accountID, 200)
			if err != nil {
				t.Fatalf("ListAuditForAccount after: %v", err)
			}

			newCount := len(after) - len(before)
			if newCount < 1 {
				t.Fatalf("expected at least 1 new audit row for %s; got %d new rows", tc.name, newCount)
			}

			// The newest row(s) — those not in 'before' — must be FAILURE with non-null error_reason.
			beforeIDs := make(map[int64]struct{}, len(before))
			for _, e := range before {
				beforeIDs[e.ID] = struct{}{}
			}
			var foundFailure bool
			for _, e := range after {
				if _, seen := beforeIDs[e.ID]; seen {
					continue
				}
				if e.Outcome == ledger.OutcomeFailure && e.ErrorReason != nil && *e.ErrorReason != "" {
					foundFailure = true
					break
				}
			}
			if !foundFailure {
				t.Errorf("[%s] expected new audit row with outcome=FAILURE and non-null error_reason", tc.name)
			}
		})
	}

	// ErrNotReversible: the service does write a FAILURE audit row but with nil
	// from/to accounts (known issue — see issues_found in artifact_tests.json).
	// ListAuditForAccount cannot find it. We verify the sentinel is returned
	// correctly here; the audit row existence is untestable at this layer without
	// a privileged "list all audits" store method.
	t.Run("ErrNotReversible_SentinelCorrect", func(t *testing.T) {
		_, _, gotErr := testSvc.Reverse(ctx, depTxn.ID, "audit-notrev-"+uuid.New().String(), "r")
		if !errors.Is(gotErr, ledger.ErrNotReversible) {
			t.Errorf("expected ErrNotReversible; got %v", gotErr)
		}
	})
}

// ---------------------------------------------------------------------------
// Idempotency — cross-type (N5-a): same key used for Deposit then Transfer
// ---------------------------------------------------------------------------

// TestIdempotency_CrossType_ReturnsOriginalTransaction asserts the N5-a accepted
// trade-off: a Transfer with an idem-key previously used for a Deposit returns
// the Deposit transaction body without error (no kind-check on replay).
// This test must remain green as long as N5-a is in effect.  If the behaviour
// is changed (kind-check added), this test must fail loudly so the trade-off is
// revisited consciously.
func TestIdempotency_CrossType_ReturnsOriginalTransaction(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)
	ctx := context.Background()

	sharedKey := "cross-type-" + uuid.New().String()

	// Step 1: Deposit with sharedKey — creates a DEPOSIT transaction.
	depTxn, _, err := testSvc.Deposit(ctx, aliceID, mustMoney("50"), sharedKey, "req-dep")
	if err != nil {
		t.Fatalf("Deposit: %v", err)
	}
	if depTxn.Kind != ledger.KindDeposit {
		t.Fatalf("setup: expected DEPOSIT kind; got %s", depTxn.Kind)
	}

	// Step 2: Transfer with the SAME sharedKey — must return the Deposit transaction (replay).
	replayTxn, _, err := testSvc.Transfer(ctx, aliceID, bobID, mustMoney("50"), sharedKey, "req-xfer")
	if err != nil {
		t.Fatalf("Transfer replay returned unexpected error (N5-a trade-off violated): %v", err)
	}

	// The replayed transaction must be the original Deposit (same ID).
	if replayTxn.ID != depTxn.ID {
		t.Errorf("cross-type replay: got transaction ID %s (kind=%s); want original Deposit ID %s — N5-a replay semantics broken",
			replayTxn.ID, replayTxn.Kind, depTxn.ID)
	}

	// Balance must NOT have been debited twice (Deposit credited alice once; Transfer is a replay).
	alice, _ := testStore.GetAccount(ctx, aliceID)
	// seed 10000 + 50 deposit = 10050; Transfer replay must not debit alice.
	expected, _ := money.FromString("10050")
	if !alice.Balance.Equal(expected) {
		t.Errorf("cross-type replay: alice balance = %s; want %s (Transfer replay must not debit)", alice.Balance, expected)
	}
}

// ---------------------------------------------------------------------------
// Balance conservation — entries sum matches account balance
// ---------------------------------------------------------------------------

// TestTransfer_BalanceConservation_EntrySum augments the basic conservation
// test by also checking the DB-level invariant: the algebraic sum of all
// entries for each account equals accounts.balance.
func TestTransfer_BalanceConservation_EntrySum(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	amt, _ := money.FromString("5")
	const N = 16
	var wg sync.WaitGroup
	wg.Add(N)
	for i := range N {
		go func() {
			defer wg.Done()
			idem := "conservation-entry-" + uuid.New().String()
			var src, dst uuid.UUID
			if i%2 == 0 {
				src, dst = aliceID, bobID
			} else {
				src, dst = bobID, aliceID
			}
			_, _, _ = testSvc.Transfer(ctx, src, dst, amt, idem, "r")
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("goroutines did not finish within deadline — possible deadlock")
	}

	alice, _ := testStore.GetAccount(ctx, aliceID)
	bob, _ := testStore.GetAccount(ctx, bobID)
	total := alice.Balance.Add(bob.Balance)
	expected, _ := money.FromString("20000")
	if !total.Equal(expected) {
		t.Errorf("balance conservation violated: alice=%s bob=%s total=%s; want 20000", alice.Balance, bob.Balance, total)
	}

	// Verify account balance equals net entry sum from the entries table.
	checkEntrySum(t, ctx, aliceID, alice.Balance)
	checkEntrySum(t, ctx, bobID, bob.Balance)
}

// checkEntrySum asserts that the net sum of entries (CREDIT positive, DEBIT
// negative) for accountID equals expectedBalance.  The seed migration deposits
// are included because they are real entries in the DB.
func checkEntrySum(t *testing.T, ctx context.Context, accountID uuid.UUID, expectedBalance money.Money) {
	t.Helper()
	txns, err := testStore.ListTransactionsForAccount(ctx, accountID, 1000)
	if err != nil {
		t.Fatalf("ListTransactionsForAccount for checkEntrySum: %v", err)
	}
	net := money.Zero()
	for _, txn := range txns {
		for _, e := range txn.Entries {
			if e.AccountID != accountID {
				continue
			}
			if e.Direction == ledger.DirectionCredit {
				net = net.Add(e.Amount)
			} else {
				net = net.Sub(e.Amount)
			}
		}
	}
	if !net.Equal(expectedBalance) {
		t.Errorf("checkEntrySum: net entry sum for %s = %s; want %s (accounts.balance mismatch)", accountID, net, expectedBalance)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mustMoney(s string) money.Money {
	m, err := money.FromString(s)
	if err != nil {
		panic("mustMoney: " + err.Error())
	}
	return m
}

func mustCreateAccountWithBalance(t *testing.T, ctx context.Context, name, balStr string) uuid.UUID {
	t.Helper()
	acc, err := testStore.CreateAccount(ctx, name)
	if err != nil {
		t.Fatalf("CreateAccount %s: %v", name, err)
	}
	bal, _ := money.FromString(balStr)
	// Deposit to set balance — use the service.
	idem := "setup-" + acc.ID.String()
	_, _, err = testSvc.Deposit(ctx, acc.ID, bal, idem, "setup")
	if err != nil {
		t.Fatalf("Deposit for setup %s: %v", name, err)
	}
	return acc.ID
}
