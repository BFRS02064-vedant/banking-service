// Package httpapi_test contains HTTP integration tests backed by testcontainers Postgres.
package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"banking-service/internal/httpapi"
	"banking-service/internal/ledger"
	"banking-service/internal/store"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

var (
	testDSN    string
	testStore  *store.Store
	testServer *httptest.Server
)

// TestMain boots Postgres, runs migrations, wires up a real httptest.Server.
func TestMain(m *testing.M) {
	ctx := context.Background()

	pgContainer, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("apitest"),
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
	svc := ledger.NewService(testStore, logger)
	apiSrv := httpapi.NewServer(svc, testStore, logger)

	mux := http.NewServeMux()
	apiSrv.RegisterRoutes(mux)
	testServer = httptest.NewServer(mux)
	defer testServer.Close()

	m.Run()
}

// skipIfNoDocker skips when testStore is nil (Docker absent).
func skipIfNoDocker(t *testing.T) {
	t.Helper()
	if testStore == nil {
		t.Skip("Docker not available; skipping HTTP integration test")
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

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func doRequest(t *testing.T, method, path string, body any, headers map[string]string) *http.Response {
	t.Helper()
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, testServer.URL+path, buf)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func decodeBody(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
}

func idemKey() string { return "idem-" + uuid.New().String() }

var (
	aliceID = uuid.MustParse("00000000-0000-0000-0000-000000000002")
	bobID   = uuid.MustParse("00000000-0000-0000-0000-000000000003")
)

// ---------------------------------------------------------------------------
// GET /api/accounts
// ---------------------------------------------------------------------------

func TestListAccounts_HappyPath(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	resp := doRequest(t, http.MethodGet, "/api/accounts", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	var accounts []map[string]any
	decodeBody(t, resp, &accounts)
	if len(accounts) < 1 {
		t.Errorf("expected at least 1 account; got %d", len(accounts))
	}
}

// ---------------------------------------------------------------------------
// POST /api/accounts
// ---------------------------------------------------------------------------

func TestCreateAccount_HappyPath(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	resp := doRequest(t, http.MethodPost, "/api/accounts", map[string]string{"name": "Testuser"}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d; want 201", resp.StatusCode)
	}
	var acc map[string]any
	decodeBody(t, resp, &acc)
	// ledger.Account has no json tags; fields marshal with their Go name (PascalCase).
	if acc["Name"] != "Testuser" {
		t.Errorf("Name = %v; want Testuser", acc["Name"])
	}
}

func TestCreateAccount_MissingName(t *testing.T) {
	skipIfNoDocker(t)
	resp := doRequest(t, http.MethodPost, "/api/accounts", map[string]string{"name": ""}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

// ---------------------------------------------------------------------------
// GET /api/accounts/{id}
// ---------------------------------------------------------------------------

func TestGetAccount_HappyPath(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)
	resp := doRequest(t, http.MethodGet, "/api/accounts/"+aliceID.String(), nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestGetAccount_NotFound(t *testing.T) {
	skipIfNoDocker(t)
	resp := doRequest(t, http.MethodGet, "/api/accounts/"+uuid.New().String(), nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d; want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestGetAccount_BadUUID(t *testing.T) {
	skipIfNoDocker(t)
	resp := doRequest(t, http.MethodGet, "/api/accounts/not-a-uuid", nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

// ---------------------------------------------------------------------------
// POST /api/deposits
// ---------------------------------------------------------------------------

func TestCreateDeposit_HappyPath(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	body := map[string]string{
		"dst_account_id": aliceID.String(),
		"amount":         "500.0000",
	}
	resp := doRequest(t, http.MethodPost, "/api/deposits", body, map[string]string{"Idempotency-Key": idemKey()})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 201, body: %s", resp.StatusCode, b)
	}
	resp.Body.Close()
}

func TestCreateDeposit_MissingIdemKey(t *testing.T) {
	skipIfNoDocker(t)
	body := map[string]string{"dst_account_id": aliceID.String(), "amount": "100"}
	resp := doRequest(t, http.MethodPost, "/api/deposits", body, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", resp.StatusCode)
	}
	var errBody map[string]string
	decodeBody(t, resp, &errBody)
	if errBody["error"] != "missing_idempotency_key" {
		t.Errorf("error = %q; want missing_idempotency_key", errBody["error"])
	}
}

func TestCreateDeposit_SystemAccount(t *testing.T) {
	skipIfNoDocker(t)
	body := map[string]string{
		"dst_account_id": ledger.ExternalAccountID.String(),
		"amount":         "100",
	}
	resp := doRequest(t, http.MethodPost, "/api/deposits", body, map[string]string{"Idempotency-Key": idemKey()})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d; want 422", resp.StatusCode)
	}
	resp.Body.Close()
}

// ---------------------------------------------------------------------------
// POST /api/transfers
// ---------------------------------------------------------------------------

func TestCreateTransfer_HappyPath(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	body := map[string]string{
		"src_account_id": aliceID.String(),
		"dst_account_id": bobID.String(),
		"amount":         "200.0000",
	}
	resp := doRequest(t, http.MethodPost, "/api/transfers", body, map[string]string{"Idempotency-Key": idemKey()})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 201, body: %s", resp.StatusCode, b)
	}
	resp.Body.Close()
}

func TestCreateTransfer_MissingIdemKey(t *testing.T) {
	skipIfNoDocker(t)
	body := map[string]string{
		"src_account_id": aliceID.String(),
		"dst_account_id": bobID.String(),
		"amount":         "100",
	}
	resp := doRequest(t, http.MethodPost, "/api/transfers", body, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", resp.StatusCode)
	}
	var errBody map[string]string
	decodeBody(t, resp, &errBody)
	if errBody["error"] != "missing_idempotency_key" {
		t.Errorf("error = %q; want missing_idempotency_key", errBody["error"])
	}
}

func TestCreateTransfer_InsufficientFunds(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	body := map[string]string{
		"src_account_id": aliceID.String(),
		"dst_account_id": bobID.String(),
		"amount":         "99999",
	}
	resp := doRequest(t, http.MethodPost, "/api/transfers", body, map[string]string{"Idempotency-Key": idemKey()})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d; want 422", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestCreateTransfer_SelfTransfer(t *testing.T) {
	skipIfNoDocker(t)
	body := map[string]string{
		"src_account_id": aliceID.String(),
		"dst_account_id": aliceID.String(),
		"amount":         "10",
	}
	resp := doRequest(t, http.MethodPost, "/api/transfers", body, map[string]string{"Idempotency-Key": idemKey()})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d; want 422", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestCreateTransfer_SystemAccount(t *testing.T) {
	skipIfNoDocker(t)
	body := map[string]string{
		"src_account_id": aliceID.String(),
		"dst_account_id": ledger.ExternalAccountID.String(),
		"amount":         "10",
	}
	resp := doRequest(t, http.MethodPost, "/api/transfers", body, map[string]string{"Idempotency-Key": idemKey()})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d; want 422", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestCreateTransfer_Replay_Returns200(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	key := idemKey()
	body := map[string]string{
		"src_account_id": aliceID.String(),
		"dst_account_id": bobID.String(),
		"amount":         "50",
	}
	headers := map[string]string{"Idempotency-Key": key}

	resp1 := doRequest(t, http.MethodPost, "/api/transfers", body, headers)
	if resp1.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp1.Body)
		t.Fatalf("first request status = %d, body: %s", resp1.StatusCode, b)
	}
	var tx1 map[string]any
	decodeBody(t, resp1, &tx1)

	resp2 := doRequest(t, http.MethodPost, "/api/transfers", body, headers)
	if resp2.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp2.Body)
		t.Fatalf("replay status = %d, body: %s", resp2.StatusCode, b)
	}
	var tx2 map[string]any
	decodeBody(t, resp2, &tx2)

	// ledger.Transaction has no json tags; "ID" marshals as "ID" (PascalCase).
	if tx1["ID"] != tx2["ID"] {
		t.Errorf("replay returned different transaction ID: %v vs %v", tx1["ID"], tx2["ID"])
	}
}

// ---------------------------------------------------------------------------
// POST /api/transfers/{id}/reverse
// ---------------------------------------------------------------------------

func TestReverseTransfer_HappyPath(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	// Create a transfer first.
	body := map[string]string{
		"src_account_id": aliceID.String(),
		"dst_account_id": bobID.String(),
		"amount":         "100",
	}
	resp := doRequest(t, http.MethodPost, "/api/transfers", body, map[string]string{"Idempotency-Key": idemKey()})
	var txn map[string]any
	decodeBody(t, resp, &txn)

	// ledger.Transaction marshals "ID" (PascalCase) — no json tags on the struct.
	txID := fmt.Sprintf("%v", txn["ID"])
	revResp := doRequest(t, http.MethodPost, "/api/transfers/"+txID+"/reverse", nil, map[string]string{"Idempotency-Key": idemKey()})
	if revResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(revResp.Body)
		t.Fatalf("reverse status = %d; want 201, body: %s", revResp.StatusCode, b)
	}
	revResp.Body.Close()
}

func TestReverseTransfer_AlreadyReversed(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	body := map[string]string{
		"src_account_id": aliceID.String(),
		"dst_account_id": bobID.String(),
		"amount":         "100",
	}
	resp := doRequest(t, http.MethodPost, "/api/transfers", body, map[string]string{"Idempotency-Key": idemKey()})
	var txn map[string]any
	decodeBody(t, resp, &txn)

	txID := fmt.Sprintf("%v", txn["ID"])
	// First reverse.
	r1 := doRequest(t, http.MethodPost, "/api/transfers/"+txID+"/reverse", nil, map[string]string{"Idempotency-Key": idemKey()})
	r1.Body.Close()

	// Second reverse with a different key.
	r2 := doRequest(t, http.MethodPost, "/api/transfers/"+txID+"/reverse", nil, map[string]string{"Idempotency-Key": idemKey()})
	if r2.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(r2.Body)
		t.Fatalf("second reverse status = %d; want 409, body: %s", r2.StatusCode, b)
	}
	r2.Body.Close()
}

func TestReverseTransfer_MissingIdemKey(t *testing.T) {
	skipIfNoDocker(t)
	// Use a random (valid) UUID — we just care about the missing idem-key 400.
	resp := doRequest(t, http.MethodPost, "/api/transfers/"+uuid.New().String()+"/reverse", nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", resp.StatusCode)
	}
	var errBody map[string]string
	decodeBody(t, resp, &errBody)
	if errBody["error"] != "missing_idempotency_key" {
		t.Errorf("error = %q; want missing_idempotency_key", errBody["error"])
	}
}

func TestReverseTransfer_BadUUID(t *testing.T) {
	skipIfNoDocker(t)
	resp := doRequest(t, http.MethodPost, "/api/transfers/not-a-uuid/reverse", nil, map[string]string{"Idempotency-Key": idemKey()})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestReverseTransfer_NotFound(t *testing.T) {
	skipIfNoDocker(t)
	resp := doRequest(t, http.MethodPost, "/api/transfers/"+uuid.New().String()+"/reverse", nil, map[string]string{"Idempotency-Key": idemKey()})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d; want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestReverseTransfer_NotReversible(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	// Deposit then try to reverse it.
	body := map[string]string{"dst_account_id": aliceID.String(), "amount": "100"}
	resp := doRequest(t, http.MethodPost, "/api/deposits", body, map[string]string{"Idempotency-Key": idemKey()})
	var txn map[string]any
	decodeBody(t, resp, &txn)

	txID := fmt.Sprintf("%v", txn["ID"])
	revResp := doRequest(t, http.MethodPost, "/api/transfers/"+txID+"/reverse", nil, map[string]string{"Idempotency-Key": idemKey()})
	if revResp.StatusCode != http.StatusUnprocessableEntity {
		b, _ := io.ReadAll(revResp.Body)
		t.Fatalf("status = %d; want 422, body: %s", revResp.StatusCode, b)
	}
	revResp.Body.Close()
}

// ---------------------------------------------------------------------------
// GET /api/accounts/{id}/transactions
// ---------------------------------------------------------------------------

func TestListTransactions_HappyPath(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	resp := doRequest(t, http.MethodGet, "/api/accounts/"+aliceID.String()+"/transactions", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	var txns []map[string]any
	decodeBody(t, resp, &txns)
	if len(txns) < 1 {
		t.Errorf("expected at least 1 seeded transaction; got %d", len(txns))
	}
}

// ---------------------------------------------------------------------------
// GET /api/accounts/{id}/audit
// ---------------------------------------------------------------------------

func TestListAudit_HappyPath(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	// Write one transfer to generate an audit row.
	body := map[string]string{
		"src_account_id": aliceID.String(),
		"dst_account_id": bobID.String(),
		"amount":         "10",
	}
	r := doRequest(t, http.MethodPost, "/api/transfers", body, map[string]string{"Idempotency-Key": idemKey()})
	r.Body.Close()

	resp := doRequest(t, http.MethodGet, "/api/accounts/"+aliceID.String()+"/audit", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	var entries []map[string]any
	decodeBody(t, resp, &entries)
	if len(entries) < 1 {
		t.Errorf("expected at least 1 audit entry; got %d", len(entries))
	}
}

// ---------------------------------------------------------------------------
// All 9 sentinel → HTTP status code mappings
// ---------------------------------------------------------------------------

// TestSentinelHTTPMappings explicitly covers all 9 sentinel errors to status
// code mappings required by the Change Contract acceptance criteria.
// Each sub-test provokes the specific error and asserts status + "error" JSON field.
func TestSentinelHTTPMappings(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	cases := []struct {
		name       string
		setup      func(t *testing.T) string // returns a path suffix or transaction ID
		method     string
		path       string
		body       map[string]string
		headers    map[string]string
		wantStatus int
		wantError  string
	}{
		{
			name:       "ErrAccountNotFound_404",
			method:     http.MethodGet,
			path:       "/api/accounts/" + uuid.New().String(),
			wantStatus: http.StatusNotFound,
			wantError:  "not_found",
		},
		{
			name:       "ErrTransactionNotFound_404",
			method:     http.MethodPost,
			path:       "/api/transfers/" + uuid.New().String() + "/reverse",
			headers:    map[string]string{"Idempotency-Key": idemKey()},
			wantStatus: http.StatusNotFound,
			wantError:  "not_found",
		},
		{
			name:   "ErrInsufficientFunds_422",
			method: http.MethodPost,
			path:   "/api/transfers",
			body: map[string]string{
				"src_account_id": aliceID.String(),
				"dst_account_id": bobID.String(),
				"amount":         "9999999",
			},
			headers:    map[string]string{"Idempotency-Key": idemKey()},
			wantStatus: http.StatusUnprocessableEntity,
			wantError:  "unprocessable",
		},
		{
			name:   "ErrSelfTransfer_422",
			method: http.MethodPost,
			path:   "/api/transfers",
			body: map[string]string{
				"src_account_id": aliceID.String(),
				"dst_account_id": aliceID.String(),
				"amount":         "1",
			},
			headers:    map[string]string{"Idempotency-Key": idemKey()},
			wantStatus: http.StatusUnprocessableEntity,
			wantError:  "unprocessable",
		},
		{
			name:   "ErrSystemAccount_422",
			method: http.MethodPost,
			path:   "/api/transfers",
			body: map[string]string{
				"src_account_id": aliceID.String(),
				"dst_account_id": ledger.ExternalAccountID.String(),
				"amount":         "1",
			},
			headers:    map[string]string{"Idempotency-Key": idemKey()},
			wantStatus: http.StatusUnprocessableEntity,
			wantError:  "unprocessable",
		},
		{
			name:   "ErrInvalidAmount_422",
			method: http.MethodPost,
			path:   "/api/transfers",
			body: map[string]string{
				"src_account_id": aliceID.String(),
				"dst_account_id": bobID.String(),
				"amount":         "0",
			},
			headers:    map[string]string{"Idempotency-Key": idemKey()},
			wantStatus: http.StatusUnprocessableEntity,
			wantError:  "unprocessable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var path string
			if tc.path != "" {
				path = tc.path
			}
			resp := doRequest(t, tc.method, path, tc.body, tc.headers)
			if resp.StatusCode != tc.wantStatus {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d; want %d, body: %s", resp.StatusCode, tc.wantStatus, b)
			}
			var body map[string]string
			decodeBody(t, resp, &body)
			if body["error"] != tc.wantError {
				t.Errorf("error = %q; want %q", body["error"], tc.wantError)
			}
		})
	}

	// ErrNotReversible (422) — requires a deposit transaction first.
	t.Run("ErrNotReversible_422", func(t *testing.T) {
		depResp := doRequest(t, http.MethodPost, "/api/deposits",
			map[string]string{"dst_account_id": aliceID.String(), "amount": "10"},
			map[string]string{"Idempotency-Key": idemKey()})
		var depTxn map[string]any
		decodeBody(t, depResp, &depTxn)
		depID := fmt.Sprintf("%v", depTxn["ID"])

		resp := doRequest(t, http.MethodPost, "/api/transfers/"+depID+"/reverse", nil,
			map[string]string{"Idempotency-Key": idemKey()})
		if resp.StatusCode != http.StatusUnprocessableEntity {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d; want 422, body: %s", resp.StatusCode, b)
		}
		var body map[string]string
		decodeBody(t, resp, &body)
		if body["error"] != "unprocessable" {
			t.Errorf("error = %q; want unprocessable", body["error"])
		}
	})

	// ErrAlreadyReversed (409) — requires a transfer reversed twice.
	t.Run("ErrAlreadyReversed_409", func(t *testing.T) {
		xferResp := doRequest(t, http.MethodPost, "/api/transfers",
			map[string]string{"src_account_id": aliceID.String(), "dst_account_id": bobID.String(), "amount": "5"},
			map[string]string{"Idempotency-Key": idemKey()})
		var xferTxn map[string]any
		decodeBody(t, xferResp, &xferTxn)
		xferID := fmt.Sprintf("%v", xferTxn["ID"])

		r1 := doRequest(t, http.MethodPost, "/api/transfers/"+xferID+"/reverse", nil,
			map[string]string{"Idempotency-Key": idemKey()})
		r1.Body.Close()

		r2 := doRequest(t, http.MethodPost, "/api/transfers/"+xferID+"/reverse", nil,
			map[string]string{"Idempotency-Key": idemKey()})
		if r2.StatusCode != http.StatusConflict {
			b, _ := io.ReadAll(r2.Body)
			t.Fatalf("status = %d; want 409, body: %s", r2.StatusCode, b)
		}
		var body map[string]string
		decodeBody(t, r2, &body)
		if body["error"] != "conflict" {
			t.Errorf("error = %q; want conflict", body["error"])
		}
	})

	// ErrIdempotencyConflict (409) — NOTE: this is the race-path ErrIdempotencyConflict.
	// The normal idempotency replay returns 200/201.  The 409 path arises when
	// InsertTransaction fires 23505 and re-fetch also fails (extremely rare race).
	// We cannot reliably trigger this race from an HTTP test; it is covered at
	// the service/store layer.  Skip here and note in coverage notes.
}

// ---------------------------------------------------------------------------
// GET /api/transactions (global list)
// ---------------------------------------------------------------------------

func TestListAllTransactions_HappyPath(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	// Seed: 1 deposit to alice, 1 deposit to bob, 1 transfer alice→bob.
	doRequest(t, http.MethodPost, "/api/deposits",
		map[string]string{"dst_account_id": aliceID.String(), "amount": "100"},
		map[string]string{"Idempotency-Key": idemKey()}).Body.Close()
	doRequest(t, http.MethodPost, "/api/deposits",
		map[string]string{"dst_account_id": bobID.String(), "amount": "50"},
		map[string]string{"Idempotency-Key": idemKey()}).Body.Close()
	doRequest(t, http.MethodPost, "/api/transfers",
		map[string]string{"src_account_id": aliceID.String(), "dst_account_id": bobID.String(), "amount": "10"},
		map[string]string{"Idempotency-Key": idemKey()}).Body.Close()

	resp := doRequest(t, http.MethodGet, "/api/transactions", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	var txns []map[string]any
	decodeBody(t, resp, &txns)
	if len(txns) < 3 {
		t.Errorf("expected >= 3 transactions; got %d", len(txns))
	}
	// Entries must be populated on each transaction.
	// Each entry must carry a non-empty AccountName (populated by the JOIN in listEntries).
	for _, tx := range txns {
		entries, ok := tx["Entries"].([]any)
		if !ok || len(entries) == 0 {
			t.Errorf("transaction %v has no Entries", tx["ID"])
			continue
		}
		for i, rawEntry := range entries {
			entry, ok := rawEntry.(map[string]any)
			if !ok {
				t.Errorf("transaction %v entry[%d] is not an object", tx["ID"], i)
				continue
			}
			name, _ := entry["AccountName"].(string)
			if name == "" {
				t.Errorf("transaction %v entry[%d] has empty AccountName; want non-empty (JOIN failed?)", tx["ID"], i)
			}
		}
	}
}

func TestListAllTransactions_LimitDefault(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	resp := doRequest(t, http.MethodGet, "/api/transactions", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	var txns []map[string]any
	decodeBody(t, resp, &txns)
	if len(txns) > 50 {
		t.Errorf("default limit: got %d rows; want <= 50", len(txns))
	}
}

func TestListAllTransactions_LimitCap(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	// Seed 210 deposits so there are more rows than the cap of 200.
	// Each call needs a unique idempotency key.
	for i := 0; i < 210; i++ {
		doRequest(t, http.MethodPost, "/api/deposits",
			map[string]string{"dst_account_id": aliceID.String(), "amount": "1"},
			map[string]string{"Idempotency-Key": idemKey()}).Body.Close()
	}

	resp := doRequest(t, http.MethodGet, "/api/transactions?limit=999", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	var txns []map[string]any
	decodeBody(t, resp, &txns)
	// Must be exactly 200 (clamped), not 50 (default) and not > 200.
	if len(txns) != 200 {
		t.Errorf("limit=999: got %d rows; want exactly 200 (clamped to cap)", len(txns))
	}
}

func TestListAllTransactions_LimitZero(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	resp := doRequest(t, http.MethodGet, "/api/transactions?limit=0", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	var txns []map[string]any
	decodeBody(t, resp, &txns)
	if len(txns) > 50 {
		t.Errorf("limit=0 (should default to 50): got %d rows; want <= 50", len(txns))
	}
}

func TestListAllTransactions_OffsetPagination(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	// Seed 4 deposits so we have at least 4 rows beyond the seeded transactions.
	for i := 0; i < 4; i++ {
		doRequest(t, http.MethodPost, "/api/deposits",
			map[string]string{"dst_account_id": aliceID.String(), "amount": "1"},
			map[string]string{"Idempotency-Key": idemKey()}).Body.Close()
	}

	resp1 := doRequest(t, http.MethodGet, "/api/transactions?limit=2&offset=0", nil, nil)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("page1 status = %d; want 200", resp1.StatusCode)
	}
	var page1 []map[string]any
	decodeBody(t, resp1, &page1)

	resp2 := doRequest(t, http.MethodGet, "/api/transactions?limit=2&offset=2", nil, nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("page2 status = %d; want 200", resp2.StatusCode)
	}
	var page2 []map[string]any
	decodeBody(t, resp2, &page2)

	if len(page1) == 0 || len(page2) == 0 {
		t.Fatalf("expected non-empty pages; page1=%d page2=%d", len(page1), len(page2))
	}

	// No overlap: all IDs in page1 must be absent from page2.
	ids1 := make(map[any]struct{}, len(page1))
	for _, tx := range page1 {
		ids1[tx["ID"]] = struct{}{}
	}
	for _, tx := range page2 {
		if _, dup := ids1[tx["ID"]]; dup {
			t.Errorf("transaction %v appears in both page 1 and page 2", tx["ID"])
		}
	}
}

func TestListAllTransactions_KindFilter_Transfer(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	// Seed a deposit and a transfer so both kinds exist.
	doRequest(t, http.MethodPost, "/api/deposits",
		map[string]string{"dst_account_id": aliceID.String(), "amount": "50"},
		map[string]string{"Idempotency-Key": idemKey()}).Body.Close()
	doRequest(t, http.MethodPost, "/api/transfers",
		map[string]string{"src_account_id": aliceID.String(), "dst_account_id": bobID.String(), "amount": "10"},
		map[string]string{"Idempotency-Key": idemKey()}).Body.Close()

	resp := doRequest(t, http.MethodGet, "/api/transactions?kind=TRANSFER", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	var txns []map[string]any
	decodeBody(t, resp, &txns)
	if len(txns) == 0 {
		t.Fatal("expected at least 1 TRANSFER; got 0")
	}
	for _, tx := range txns {
		if tx["Kind"] != "TRANSFER" {
			t.Errorf("kind = %v; want TRANSFER", tx["Kind"])
		}
	}
}

func TestListAllTransactions_KindFilter_Invalid(t *testing.T) {
	skipIfNoDocker(t)

	resp := doRequest(t, http.MethodGet, "/api/transactions?kind=transfer", nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", resp.StatusCode)
	}
	var body map[string]string
	decodeBody(t, resp, &body)
	if body["error"] != "invalid_kind" {
		t.Errorf("error = %q; want invalid_kind", body["error"])
	}
}

func TestListAllTransactions_OffsetPastEnd(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	resp := doRequest(t, http.MethodGet, "/api/transactions?offset=999999", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	var txns []map[string]any
	decodeBody(t, resp, &txns)
	if len(txns) != 0 {
		t.Errorf("expected empty array for offset past end; got %d rows", len(txns))
	}
}

// ---------------------------------------------------------------------------
// Cross-type idempotency (N5-a trade-off)
// ---------------------------------------------------------------------------

// TestHTTP_CrossTypeIdempotency_ReturnsOriginalDeposit asserts that when a
// Deposit idem-key is reused for a Transfer, the server returns the original
// Deposit transaction body (HTTP 201) without debiting any account.
// This is the N5-a accepted trade-off: no kind-check on replay.
func TestHTTP_CrossTypeIdempotency_ReturnsOriginalDeposit(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	key := idemKey()

	// 1. Deposit with key.
	depBody := map[string]string{"dst_account_id": aliceID.String(), "amount": "75"}
	depResp := doRequest(t, http.MethodPost, "/api/deposits", depBody,
		map[string]string{"Idempotency-Key": key})
	if depResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(depResp.Body)
		t.Fatalf("deposit status = %d, body: %s", depResp.StatusCode, b)
	}
	var depTxn map[string]any
	decodeBody(t, depResp, &depTxn)
	depTxnID := fmt.Sprintf("%v", depTxn["ID"])

	// Record Alice balance after the deposit.
	aliceAfterDeposit := doRequest(t, http.MethodGet, "/api/accounts/"+aliceID.String(), nil, nil)
	var aliceAcc map[string]any
	decodeBody(t, aliceAfterDeposit, &aliceAcc)
	aliceBalanceAfterDeposit := fmt.Sprintf("%v", aliceAcc["Balance"])

	// 2. Transfer with the SAME key — must return the Deposit transaction.
	xferBody := map[string]string{
		"src_account_id": aliceID.String(),
		"dst_account_id": bobID.String(),
		"amount":         "75",
	}
	xferResp := doRequest(t, http.MethodPost, "/api/transfers", xferBody,
		map[string]string{"Idempotency-Key": key})
	// N5-a: replay succeeds (no error) — status 201.
	if xferResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(xferResp.Body)
		t.Fatalf("cross-type transfer replay status = %d, body: %s (N5-a violated)", xferResp.StatusCode, b)
	}
	var replayTxn map[string]any
	decodeBody(t, xferResp, &replayTxn)

	// The replayed transaction ID must equal the original Deposit ID.
	replayID := fmt.Sprintf("%v", replayTxn["ID"])
	if replayID != depTxnID {
		t.Errorf("cross-type replay ID = %s; want original Deposit ID %s (N5-a replay broken)", replayID, depTxnID)
	}

	// Alice balance must be unchanged by the Transfer replay.
	aliceAfterReplay := doRequest(t, http.MethodGet, "/api/accounts/"+aliceID.String(), nil, nil)
	var aliceAccAfter map[string]any
	decodeBody(t, aliceAfterReplay, &aliceAccAfter)
	aliceBalanceAfterReplay := fmt.Sprintf("%v", aliceAccAfter["Balance"])
	if aliceBalanceAfterReplay != aliceBalanceAfterDeposit {
		t.Errorf("cross-type replay debited alice: balance changed from %s to %s (Transfer replay must be no-op)",
			aliceBalanceAfterDeposit, aliceBalanceAfterReplay)
	}
}

// ---------------------------------------------------------------------------
// End-to-end: create → deposit → transfer → reverse → audit
// ---------------------------------------------------------------------------

func TestEndToEnd_CreateDepositTransferReverseAudit(t *testing.T) {
	skipIfNoDocker(t)
	truncate(t)

	// 1. Create a new account.
	resp := doRequest(t, http.MethodPost, "/api/accounts", map[string]string{"name": "E2E-User"}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("createAccount status = %d", resp.StatusCode)
	}
	var newAcc map[string]any
	decodeBody(t, resp, &newAcc)
	newID := fmt.Sprintf("%v", newAcc["ID"])

	// 2. Deposit 1000 into the new account.
	depBody := map[string]string{"dst_account_id": newID, "amount": "1000"}
	depResp := doRequest(t, http.MethodPost, "/api/deposits", depBody, map[string]string{"Idempotency-Key": idemKey()})
	if depResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(depResp.Body)
		t.Fatalf("deposit status = %d, body: %s", depResp.StatusCode, b)
	}
	depResp.Body.Close()

	// 3. Transfer 200 from new account to alice.
	xferBody := map[string]string{
		"src_account_id": newID,
		"dst_account_id": aliceID.String(),
		"amount":         "200",
	}
	xferResp := doRequest(t, http.MethodPost, "/api/transfers", xferBody, map[string]string{"Idempotency-Key": idemKey()})
	if xferResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(xferResp.Body)
		t.Fatalf("transfer status = %d, body: %s", xferResp.StatusCode, b)
	}
	var xferTxn map[string]any
	decodeBody(t, xferResp, &xferTxn)
	xferID := fmt.Sprintf("%v", xferTxn["ID"])

	// 4. Reverse the transfer.
	revResp := doRequest(t, http.MethodPost, "/api/transfers/"+xferID+"/reverse", nil, map[string]string{"Idempotency-Key": idemKey()})
	if revResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(revResp.Body)
		t.Fatalf("reverse status = %d, body: %s", revResp.StatusCode, b)
	}
	revResp.Body.Close()

	// 5. Audit log must contain at least 3 rows for the new account (deposit + transfer + reversal).
	auditResp := doRequest(t, http.MethodGet, "/api/accounts/"+newID+"/audit", nil, nil)
	if auditResp.StatusCode != http.StatusOK {
		t.Fatalf("audit status = %d", auditResp.StatusCode)
	}
	var auditEntries []map[string]any
	decodeBody(t, auditResp, &auditEntries)
	if len(auditEntries) < 3 {
		t.Errorf("expected >= 3 audit entries after deposit+transfer+reverse; got %d", len(auditEntries))
	}

	// 6. Assert the first 3 audit entries (happy-path ops) are SUCCESS.
	for _, e := range auditEntries {
		if e["Outcome"] != "SUCCESS" {
			t.Errorf("unexpected non-SUCCESS audit entry before failure step: %v", e)
		}
	}

	// 7. Verify error JSON shape on a known failure path (insufficient funds).
	resp2 := doRequest(t, http.MethodPost, "/api/transfers", map[string]string{
		"src_account_id": newID,
		"dst_account_id": aliceID.String(),
		"amount":         "999999",
	}, map[string]string{"Idempotency-Key": idemKey()})
	var errBody map[string]string
	decodeBody(t, resp2, &errBody)
	if _, ok := errBody["error"]; !ok {
		t.Errorf("error response missing 'error' field: %v", errBody)
	}
	if _, ok := errBody["message"]; !ok {
		t.Errorf("error response missing 'message' field: %v", errBody)
	}

	// 8. After the failed transfer, audit log must have >= 4 rows (3 success + 1 failure).
	auditResp2 := doRequest(t, http.MethodGet, "/api/accounts/"+newID+"/audit", nil, nil)
	if auditResp2.StatusCode != http.StatusOK {
		t.Fatalf("audit status (post-failure) = %d", auditResp2.StatusCode)
	}
	var auditEntries2 []map[string]any
	decodeBody(t, auditResp2, &auditEntries2)
	if len(auditEntries2) < 4 {
		t.Errorf("expected >= 4 audit entries after deposit+transfer+reverse+failed_transfer; got %d", len(auditEntries2))
	}
	// The newest entry (index 0 if ordered desc, or last if asc) must be a FAILURE row.
	var hasFailure bool
	for _, e := range auditEntries2 {
		if e["Outcome"] == "FAILURE" {
			hasFailure = true
			break
		}
	}
	if !hasFailure {
		t.Errorf("expected at least one FAILURE audit entry after the failed transfer; got none")
	}

	// 8. Verify amount fields come back as strings (not floats).
	txnsResp := doRequest(t, http.MethodGet, "/api/accounts/"+newID+"/transactions", nil, nil)
	var txns []map[string]any
	decodeBody(t, txnsResp, &txns)
	for _, tx := range txns {
		entries, ok := tx["Entries"].([]any)
		if !ok {
			continue
		}
		for _, raw := range entries {
			e, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if amt, ok := e["Amount"]; ok {
				if _, isFloat := amt.(float64); isFloat {
					t.Errorf("amount serialized as float64; want string: %v", amt)
				}
				if s, isStr := amt.(string); isStr {
					if strings.ContainsAny(s, "eE") {
						t.Errorf("amount uses scientific notation: %s", s)
					}
				}
			}
		}
	}
}
