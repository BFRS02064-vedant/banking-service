package ledger

import "errors"

// Sentinel errors for predictable failure cases. Callers match with errors.Is.

var (
	// ErrAccountNotFound is returned when a requested account does not exist.
	ErrAccountNotFound = errors.New("account not found")

	// ErrTransactionNotFound is returned when a requested transaction does not exist.
	ErrTransactionNotFound = errors.New("transaction not found")

	// ErrInsufficientFunds is returned when a debit would bring a non-system
	// account below zero.
	ErrInsufficientFunds = errors.New("insufficient funds")

	// ErrSelfTransfer is returned when the source and destination accounts are
	// the same.
	ErrSelfTransfer = errors.New("self-transfer not allowed")

	// ErrAlreadyReversed is returned when a transfer has already been reversed.
	ErrAlreadyReversed = errors.New("transaction already reversed")

	// ErrNotReversible is returned when the transaction kind does not support
	// reversal (only TRANSFER rows can be reversed).
	ErrNotReversible = errors.New("transaction is not reversible")

	// ErrIdempotencyConflict is returned when an idempotency key is already used
	// by a different transaction.
	ErrIdempotencyConflict = errors.New("idempotency key conflict")

	// ErrInvalidAmount is returned when an amount is zero or negative where a
	// strictly positive value is required.
	ErrInvalidAmount = errors.New("amount must be positive")

	// ErrSystemAccount is returned when an operation that must not target a system
	// account does so.
	ErrSystemAccount = errors.New("operation not permitted on system account")
)
