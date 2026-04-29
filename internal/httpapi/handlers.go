// Package httpapi exposes the banking service over HTTP using net/http method-prefixed routes.
// Each handler is a plain method on Server; there is no middleware chain.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"banking-service/internal/ledger"
	"banking-service/internal/money"
	"banking-service/internal/store"

	"github.com/google/uuid"
)

// queryer is the subset of *store.Store used directly by handlers (not via Service).
// Defined as an interface so handlers_test.go can substitute a fake.
type queryer interface {
	GetAccount(ctx context.Context, id uuid.UUID) (ledger.Account, error)
	ListAccounts(ctx context.Context) ([]ledger.Account, error)
	CreateAccount(ctx context.Context, name string) (ledger.Account, error)
	ListTransactionsForAccount(ctx context.Context, accountID uuid.UUID, limit int) ([]ledger.Transaction, error)
	ListAuditForAccount(ctx context.Context, accountID uuid.UUID, limit int) ([]ledger.AuditEntry, error)
	ListTransactions(ctx context.Context, opts store.ListTransactionsOpts) ([]ledger.Transaction, error)
}

// Server holds the dependencies for all HTTP handlers.
type Server struct {
	svc   *ledger.Service
	store queryer
	log   *slog.Logger
}

// NewServer constructs a Server.
func NewServer(svc *ledger.Service, store queryer, log *slog.Logger) *Server {
	return &Server{svc: svc, store: store, log: log}
}

// RegisterRoutes registers all API routes on mux.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/accounts", s.listAccounts)
	mux.HandleFunc("POST /api/accounts", s.createAccount)
	mux.HandleFunc("GET /api/accounts/{id}", s.getAccount)
	mux.HandleFunc("POST /api/transfers", s.createTransfer)
	mux.HandleFunc("POST /api/transfers/{id}/reverse", s.reverseTransfer)
	mux.HandleFunc("POST /api/deposits", s.createDeposit)
	mux.HandleFunc("GET /api/accounts/{id}/transactions", s.listTransactions)
	mux.HandleFunc("GET /api/accounts/{id}/audit", s.listAudit)
	mux.HandleFunc("GET /api/transactions", s.listAllTransactions)
}

// listAccounts handles GET /api/accounts.
func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.store.ListAccounts(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if accounts == nil {
		accounts = []ledger.Account{}
	}
	writeJSON(w, http.StatusOK, accounts)
}

// createAccount handles POST /api/accounts.
func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "name is required")
		return
	}
	account, err := s.store.CreateAccount(r.Context(), body.Name)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, account)
}

// getAccount handles GET /api/accounts/{id}.
func (s *Server) getAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUID(w, r.PathValue("id"))
	if !ok {
		return
	}
	account, err := s.store.GetAccount(r.Context(), id)
	if err != nil {
		s.mapError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, account)
}

// createTransfer handles POST /api/transfers.
func (s *Server) createTransfer(w http.ResponseWriter, r *http.Request) {
	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey == "" {
		writeError(w, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key header is required")
		return
	}
	requestID := resolveRequestID(r)

	var body struct {
		SrcID  string      `json:"src_account_id"`
		DstID  string      `json:"dst_account_id"`
		Amount money.Money `json:"amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	srcID, ok := parseUUIDField(w, body.SrcID, "src_account_id")
	if !ok {
		return
	}
	dstID, ok := parseUUIDField(w, body.DstID, "dst_account_id")
	if !ok {
		return
	}

	txn, _, err := s.svc.Transfer(r.Context(), srcID, dstID, body.Amount, idemKey, requestID)
	if err != nil {
		s.mapError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, txn)
}

// reverseTransfer handles POST /api/transfers/{id}/reverse.
func (s *Server) reverseTransfer(w http.ResponseWriter, r *http.Request) {
	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey == "" {
		writeError(w, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key header is required")
		return
	}
	requestID := resolveRequestID(r)

	originalID, ok := parseUUID(w, r.PathValue("id"))
	if !ok {
		return
	}

	txn, _, err := s.svc.Reverse(r.Context(), originalID, idemKey, requestID)
	if err != nil {
		s.mapError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, txn)
}

// createDeposit handles POST /api/deposits.
func (s *Server) createDeposit(w http.ResponseWriter, r *http.Request) {
	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey == "" {
		writeError(w, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key header is required")
		return
	}
	requestID := resolveRequestID(r)

	var body struct {
		DstID  string      `json:"dst_account_id"`
		Amount money.Money `json:"amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	dstID, ok := parseUUIDField(w, body.DstID, "dst_account_id")
	if !ok {
		return
	}

	txn, _, err := s.svc.Deposit(r.Context(), dstID, body.Amount, idemKey, requestID)
	if err != nil {
		s.mapError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, txn)
}

// listTransactions handles GET /api/accounts/{id}/transactions?limit=N.
// Default limit: 50. Cap: 200. Bad values fall back to default.
func (s *Server) listTransactions(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUID(w, r.PathValue("id"))
	if !ok {
		return
	}
	limit := parseLimit(r, 50, 200)
	txns, err := s.store.ListTransactionsForAccount(r.Context(), id, limit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if txns == nil {
		txns = []ledger.Transaction{}
	}
	writeJSON(w, http.StatusOK, txns)
}

// validKinds is the set of accepted ?kind= values for GET /api/transactions.
var validKinds = map[string]struct{}{
	"TRANSFER":   {},
	"REVERSAL":   {},
	"DEPOSIT":    {},
	"WITHDRAWAL": {},
}

// listAllTransactions handles GET /api/transactions?limit=N&offset=M&kind=K.
// Default limit: 50. Cap: 200. Bad/zero/negative values fall back to defaults.
// ?kind is case-sensitive uppercase; unknown values return 400 {"error":"invalid_kind"}.
func (s *Server) listAllTransactions(w http.ResponseWriter, r *http.Request) {
	limit := parseLimit(r, 50, 200)

	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			offset = n
		}
	}

	var kind *ledger.TransactionKind
	if k := r.URL.Query().Get("kind"); k != "" {
		if _, ok := validKinds[k]; !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_kind"})
			return
		}
		tk := ledger.TransactionKind(k)
		kind = &tk
	}

	txns, err := s.store.ListTransactions(r.Context(), store.ListTransactionsOpts{
		Limit:  limit,
		Offset: offset,
		Kind:   kind,
	})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if txns == nil {
		txns = []ledger.Transaction{}
	}
	writeJSON(w, http.StatusOK, txns)
}

// listAudit handles GET /api/accounts/{id}/audit?limit=N.
// Default limit: 50. Cap: 200. Bad values fall back to default.
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUID(w, r.PathValue("id"))
	if !ok {
		return
	}
	limit := parseLimit(r, 50, 200)
	entries, err := s.store.ListAuditForAccount(r.Context(), id, limit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if entries == nil {
		entries = []ledger.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// mapError translates a domain sentinel error to an HTTP status and writes the response.
func (s *Server) mapError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ledger.ErrAccountNotFound), errors.Is(err, ledger.ErrTransactionNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, ledger.ErrAlreadyReversed), errors.Is(err, ledger.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, ledger.ErrInsufficientFunds),
		errors.Is(err, ledger.ErrNotReversible),
		errors.Is(err, ledger.ErrSelfTransfer),
		errors.Is(err, ledger.ErrSystemAccount),
		errors.Is(err, ledger.ErrInvalidAmount):
		writeError(w, http.StatusUnprocessableEntity, "unprocessable", err.Error())
	default:
		s.internalError(w, r, err)
	}
}

// internalError logs the full error and writes a generic 500 response.
func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("internal error", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal_error", "an internal error occurred")
}

// writeJSON encodes v as JSON with status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a stable JSON error body: {"error":"code","message":"text"}.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "message": message})
}

// parseUUID parses a UUID from a path segment and writes 400 on failure.
func parseUUID(w http.ResponseWriter, s string) (uuid.UUID, bool) {
	id, err := uuid.Parse(s)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid UUID in path")
		return uuid.UUID{}, false
	}
	return id, true
}

// parseUUIDField parses a UUID from a JSON field value and writes 400 on failure.
func parseUUIDField(w http.ResponseWriter, s, field string) (uuid.UUID, bool) {
	if s == "" {
		writeError(w, http.StatusBadRequest, "bad_request", field+" is required")
		return uuid.UUID{}, false
	}
	id, err := uuid.Parse(s)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", field+" is not a valid UUID")
		return uuid.UUID{}, false
	}
	return id, true
}

// parseLimit reads ?limit from the query string, applies defaultVal and cap.
func parseLimit(r *http.Request, defaultVal, cap int) int {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultVal
	}
	if n > cap {
		return cap
	}
	return n
}

// resolveRequestID returns the X-Request-ID header value, or generates a new UUID v7 string.
func resolveRequestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-ID"); id != "" {
		return id
	}
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.New().String()
	}
	return id.String()
}
