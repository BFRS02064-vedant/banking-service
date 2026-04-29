package store

import (
	"context"
	"fmt"

	"banking-service/internal/ledger"
	"banking-service/internal/money"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// ListAuditForAccount returns up to limit audit_log rows where from_account or
// to_account matches accountID, ordered by created_at DESC.
// Uses idx_audit_from and idx_audit_to indexes for performance.
func (s *Store) ListAuditForAccount(ctx context.Context, accountID uuid.UUID, limit int) ([]ledger.AuditEntry, error) {
	const q = `
		SELECT id, operation, from_account, to_account, amount, outcome,
		       error_reason, transaction_id, request_id, created_at
		FROM audit_log
		WHERE from_account = $1 OR to_account = $1
		ORDER BY created_at DESC
		LIMIT $2`

	rows, err := s.pool.Query(ctx, q, accountID, limit)
	if err != nil {
		return nil, fmt.Errorf("store.ListAuditForAccount: %w", err)
	}
	defer rows.Close()

	var entries []ledger.AuditEntry
	for rows.Next() {
		var e ledger.AuditEntry
		var amt *decimal.Decimal
		if err := rows.Scan(&e.ID, &e.Operation, &e.FromAccount, &e.ToAccount,
			&amt, &e.Outcome, &e.ErrorReason, &e.TransactionID, &e.RequestID, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("store.ListAuditForAccount scan: %w", err)
		}
		if amt != nil {
			m := money.NewFromDecimal(*amt)
			e.Amount = &m
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListAuditForAccount rows: %w", err)
	}
	return entries, nil
}

// WriteAudit inserts one row into audit_log using the pool directly (not a
// transaction) so that rolled-back business transactions are still auditable.
// It returns the error honestly.
// M3 service callers should log loudly on failure and still return success if
// the business transaction already committed. M1+M2 ships only this function —
// there is no caller yet.
// WriteAudit inherits the request context. M3 callers are expected to apply a
// tight additional deadline when calling WriteAudit to avoid pool exhaustion
// holding the HTTP handler goroutine.
func (s *Store) WriteAudit(ctx context.Context, entry ledger.AuditEntry) error {
	const q = `
		INSERT INTO audit_log
		    (operation, from_account, to_account, amount, outcome,
		     error_reason, transaction_id, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

	var amt interface{}
	if entry.Amount != nil {
		amt = entry.Amount.Decimal()
	}

	_, err := s.pool.Exec(ctx, q,
		entry.Operation,
		entry.FromAccount,
		entry.ToAccount,
		amt,
		string(entry.Outcome),
		entry.ErrorReason,
		entry.TransactionID,
		entry.RequestID,
	)
	if err != nil {
		return fmt.Errorf("store.WriteAudit: %w", err)
	}
	return nil
}
