package ledger

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"banking-service/internal/money"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// auditDeadline is the maximum time allowed for a WriteAudit call.
// Audit runs outside the business transaction; a tight deadline prevents pool exhaustion.
const auditDeadline = 3 * time.Second

// Storer is the persistence interface required by Service.
// *store.Store satisfies this interface; the interface exists only to break the
// import cycle (ledger ← store ← ledger would be circular).
type Storer interface {
	WithTx(ctx context.Context, fn func(pgx.Tx) error) error
	GetTransactionByIdempotencyKey(ctx context.Context, key string) (Transaction, []Entry, error)
	GetTransaction(ctx context.Context, id uuid.UUID) (Transaction, []Entry, error)
	WriteAudit(ctx context.Context, entry AuditEntry) error

	// Lock/mutate helpers — implemented as package-level functions in store;
	// wrapped here as interface methods so tests can substitute a fake.
	LockAccountsForUpdate(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) ([]Account, error)
	UpdateBalance(ctx context.Context, tx pgx.Tx, id uuid.UUID, bal money.Money) error
	InsertTransaction(ctx context.Context, tx pgx.Tx, t Transaction) error
	InsertReversalTransaction(ctx context.Context, tx pgx.Tx, t Transaction) error
	InsertEntries(ctx context.Context, tx pgx.Tx, entries []Entry) error
}

// Service orchestrates Transfer, Reverse, and Deposit over a Storer.
// Concurrency: all account-level locking is delegated to DB SELECT FOR UPDATE (N3-a).
type Service struct {
	store  Storer
	logger *slog.Logger
}

// NewService constructs a Service. Both arguments are required.
func NewService(s Storer, l *slog.Logger) *Service {
	return &Service{store: s, logger: l}
}

// Transfer debits src and credits dst by amount inside one atomic transaction.
// Pre-conditions validated before any DB call: amount > 0, src != dst, neither is EXTERNAL.
// Idempotency: if idemKey already exists the stored transaction is returned unchanged.
func (svc *Service) Transfer(ctx context.Context, srcID, dstID uuid.UUID, amount money.Money, idemKey, requestID string) (Transaction, []Entry, error) {
	if !amount.IsPositive() {
		svc.auditFailure(ctx, "TRANSFER", &srcID, &dstID, &amount, requestID, ErrInvalidAmount)
		return Transaction{}, nil, ErrInvalidAmount
	}
	if srcID == dstID {
		svc.auditFailure(ctx, "TRANSFER", &srcID, &dstID, &amount, requestID, ErrSelfTransfer)
		return Transaction{}, nil, ErrSelfTransfer
	}
	if srcID == ExternalAccountID || dstID == ExternalAccountID {
		svc.auditFailure(ctx, "TRANSFER", &srcID, &dstID, &amount, requestID, ErrSystemAccount)
		return Transaction{}, nil, ErrSystemAccount
	}

	// Idempotency replay: return existing transaction if key is already committed.
	if existing, entries, err := svc.store.GetTransactionByIdempotencyKey(ctx, idemKey); err == nil {
		return existing, entries, nil
	}

	txID, err := uuid.NewV7()
	if err != nil {
		return Transaction{}, nil, fmt.Errorf("service.Transfer uuid: %w", err)
	}
	txn := Transaction{ID: txID, Kind: KindTransfer, IdempotencyKey: idemKey}

	var committed Transaction
	var committedEntries []Entry

	txErr := svc.store.WithTx(ctx, func(tx pgx.Tx) error {
		locked, err := svc.store.LockAccountsForUpdate(ctx, tx, []uuid.UUID{srcID, dstID})
		if err != nil {
			return fmt.Errorf("service.Transfer lock: %w", err)
		}
		if len(locked) != 2 {
			return ErrAccountNotFound
		}
		var src, dst Account
		for _, a := range locked {
			switch a.ID {
			case srcID:
				src = a
			case dstID:
				dst = a
			}
		}
		if src.Balance.LessThan(amount) {
			return ErrInsufficientFunds
		}
		if err := svc.store.UpdateBalance(ctx, tx, srcID, src.Balance.Sub(amount)); err != nil {
			return err
		}
		if err := svc.store.UpdateBalance(ctx, tx, dstID, dst.Balance.Add(amount)); err != nil {
			return err
		}
		if err := svc.store.InsertTransaction(ctx, tx, txn); err != nil {
			return err
		}
		entries := []Entry{
			{TransactionID: txID, AccountID: srcID, Direction: DirectionDebit, Amount: amount},
			{TransactionID: txID, AccountID: dstID, Direction: DirectionCredit, Amount: amount},
		}
		if err := svc.store.InsertEntries(ctx, tx, entries); err != nil {
			return err
		}
		committed = txn
		committedEntries = entries
		return nil
	})

	if txErr != nil {
		// On idempotency race at commit: re-fetch and return existing — no audit.
		if txErr == ErrIdempotencyConflict {
			if existing, entries, fetchErr := svc.store.GetTransactionByIdempotencyKey(ctx, idemKey); fetchErr == nil {
				return existing, entries, nil
			}
		}
		svc.auditFailure(ctx, "TRANSFER", &srcID, &dstID, &amount, requestID, txErr)
		return Transaction{}, nil, txErr
	}

	svc.auditSuccess(ctx, "TRANSFER", &srcID, &dstID, &amount, requestID, &txID)
	return committed, committedEntries, nil
}

// Reverse undoes a TRANSFER by moving funds back from the original destination to source.
// Pre-condition: the original transaction must be kind=TRANSFER and not already reversed.
// Per N4-a: if the original destination lacks sufficient funds, ErrInsufficientFunds is returned.
func (svc *Service) Reverse(ctx context.Context, originalTxID uuid.UUID, idemKey, requestID string) (Transaction, []Entry, error) {
	// Idempotency replay check first.
	if existing, entries, err := svc.store.GetTransactionByIdempotencyKey(ctx, idemKey); err == nil {
		return existing, entries, nil
	}

	origTx, origEntries, err := svc.store.GetTransaction(ctx, originalTxID)
	if err != nil {
		svc.auditFailure(ctx, "REVERSAL", nil, nil, nil, requestID, err)
		return Transaction{}, nil, err
	}
	if origTx.Kind != KindTransfer {
		svc.auditFailure(ctx, "REVERSAL", nil, nil, nil, requestID, ErrNotReversible)
		return Transaction{}, nil, ErrNotReversible
	}

	// Identify original src and dst from entries.
	var origSrcID, origDstID uuid.UUID
	var amount money.Money
	for _, e := range origEntries {
		if e.Direction == DirectionDebit {
			origSrcID = e.AccountID
			amount = e.Amount
		} else {
			origDstID = e.AccountID
		}
	}

	txID, err := uuid.NewV7()
	if err != nil {
		return Transaction{}, nil, fmt.Errorf("service.Reverse uuid: %w", err)
	}
	revTx := Transaction{
		ID:                    txID,
		Kind:                  KindReversal,
		IdempotencyKey:        idemKey,
		ReversesTransactionID: &originalTxID,
	}

	var committed Transaction
	var committedEntries []Entry

	txErr := svc.store.WithTx(ctx, func(tx pgx.Tx) error {
		locked, err := svc.store.LockAccountsForUpdate(ctx, tx, []uuid.UUID{origSrcID, origDstID})
		if err != nil {
			return fmt.Errorf("service.Reverse lock: %w", err)
		}
		if len(locked) != 2 {
			return ErrAccountNotFound
		}
		var origSrc, origDst Account
		for _, a := range locked {
			switch a.ID {
			case origSrcID:
				origSrc = a
			case origDstID:
				origDst = a
			}
		}
		// N4-a: reject if the original destination can't cover the reversal.
		if origDst.Balance.LessThan(amount) {
			return ErrInsufficientFunds
		}
		if err := svc.store.UpdateBalance(ctx, tx, origDstID, origDst.Balance.Sub(amount)); err != nil {
			return err
		}
		if err := svc.store.UpdateBalance(ctx, tx, origSrcID, origSrc.Balance.Add(amount)); err != nil {
			return err
		}
		if err := svc.store.InsertReversalTransaction(ctx, tx, revTx); err != nil {
			return err
		}
		entries := []Entry{
			{TransactionID: txID, AccountID: origDstID, Direction: DirectionDebit, Amount: amount},
			{TransactionID: txID, AccountID: origSrcID, Direction: DirectionCredit, Amount: amount},
		}
		if err := svc.store.InsertEntries(ctx, tx, entries); err != nil {
			return err
		}
		committed = revTx
		committedEntries = entries
		return nil
	})

	if txErr != nil {
		if txErr == ErrIdempotencyConflict {
			if existing, entries, fetchErr := svc.store.GetTransactionByIdempotencyKey(ctx, idemKey); fetchErr == nil {
				return existing, entries, nil
			}
		}
		svc.auditFailure(ctx, "REVERSAL", &origDstID, &origSrcID, &amount, requestID, txErr)
		return Transaction{}, nil, txErr
	}

	svc.auditSuccess(ctx, "REVERSAL", &origDstID, &origSrcID, &amount, requestID, &txID)
	return committed, committedEntries, nil
}

// Deposit credits dst by debiting the EXTERNAL system account.
// Pre-conditions: amount > 0, dst != EXTERNAL.
// Idempotency: if idemKey already exists the stored transaction is returned unchanged.
func (svc *Service) Deposit(ctx context.Context, dstID uuid.UUID, amount money.Money, idemKey, requestID string) (Transaction, []Entry, error) {
	if !amount.IsPositive() {
		svc.auditFailure(ctx, "DEPOSIT", &ExternalAccountID, &dstID, &amount, requestID, ErrInvalidAmount)
		return Transaction{}, nil, ErrInvalidAmount
	}
	if dstID == ExternalAccountID {
		svc.auditFailure(ctx, "DEPOSIT", &ExternalAccountID, &dstID, &amount, requestID, ErrSystemAccount)
		return Transaction{}, nil, ErrSystemAccount
	}

	if existing, entries, err := svc.store.GetTransactionByIdempotencyKey(ctx, idemKey); err == nil {
		return existing, entries, nil
	}

	txID, err := uuid.NewV7()
	if err != nil {
		return Transaction{}, nil, fmt.Errorf("service.Deposit uuid: %w", err)
	}
	txn := Transaction{ID: txID, Kind: KindDeposit, IdempotencyKey: idemKey}

	var committed Transaction
	var committedEntries []Entry

	txErr := svc.store.WithTx(ctx, func(tx pgx.Tx) error {
		locked, err := svc.store.LockAccountsForUpdate(ctx, tx, []uuid.UUID{ExternalAccountID, dstID})
		if err != nil {
			return fmt.Errorf("service.Deposit lock: %w", err)
		}
		if len(locked) != 2 {
			return ErrAccountNotFound
		}
		var ext, dst Account
		for _, a := range locked {
			switch a.ID {
			case ExternalAccountID:
				ext = a
			case dstID:
				dst = a
			}
		}
		if err := svc.store.UpdateBalance(ctx, tx, ExternalAccountID, ext.Balance.Sub(amount)); err != nil {
			return err
		}
		if err := svc.store.UpdateBalance(ctx, tx, dstID, dst.Balance.Add(amount)); err != nil {
			return err
		}
		if err := svc.store.InsertTransaction(ctx, tx, txn); err != nil {
			return err
		}
		entries := []Entry{
			{TransactionID: txID, AccountID: ExternalAccountID, Direction: DirectionDebit, Amount: amount},
			{TransactionID: txID, AccountID: dstID, Direction: DirectionCredit, Amount: amount},
		}
		if err := svc.store.InsertEntries(ctx, tx, entries); err != nil {
			return err
		}
		committed = txn
		committedEntries = entries
		return nil
	})

	if txErr != nil {
		if txErr == ErrIdempotencyConflict {
			if existing, entries, fetchErr := svc.store.GetTransactionByIdempotencyKey(ctx, idemKey); fetchErr == nil {
				return existing, entries, nil
			}
		}
		svc.auditFailure(ctx, "DEPOSIT", &ExternalAccountID, &dstID, &amount, requestID, txErr)
		return Transaction{}, nil, txErr
	}

	svc.auditSuccess(ctx, "DEPOSIT", &ExternalAccountID, &dstID, &amount, requestID, &txID)
	return committed, committedEntries, nil
}

// auditSuccess writes a SUCCESS audit row outside the business transaction.
// Audit failures are logged but do not affect the return value — money already moved.
func (svc *Service) auditSuccess(ctx context.Context, op string, from, to *uuid.UUID, amount *money.Money, requestID string, txID *uuid.UUID) {
	aCtx, cancel := context.WithTimeout(context.Background(), auditDeadline)
	defer cancel()
	entry := AuditEntry{
		Operation:     op,
		FromAccount:   from,
		ToAccount:     to,
		Amount:        amount,
		Outcome:       OutcomeSuccess,
		TransactionID: txID,
		RequestID:     &requestID,
	}
	if err := svc.store.WriteAudit(aCtx, entry); err != nil {
		svc.logger.Error("audit write failed (success path)", "op", op, "err", err)
	}
}

// auditFailure writes a FAILURE audit row outside any transaction.
func (svc *Service) auditFailure(ctx context.Context, op string, from, to *uuid.UUID, amount *money.Money, requestID string, reason error) {
	aCtx, cancel := context.WithTimeout(context.Background(), auditDeadline)
	defer cancel()
	errStr := reason.Error()
	entry := AuditEntry{
		Operation:   op,
		FromAccount: from,
		ToAccount:   to,
		Amount:      amount,
		Outcome:     OutcomeFailure,
		ErrorReason: &errStr,
		RequestID:   &requestID,
	}
	if err := svc.store.WriteAudit(aCtx, entry); err != nil {
		svc.logger.Error("audit write failed (failure path)", "op", op, "err", err)
	}
}
