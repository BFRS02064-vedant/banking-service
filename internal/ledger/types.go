// Package ledger defines the domain types for the double-entry banking ledger.
package ledger

import (
	"time"

	"banking-service/internal/money"

	"github.com/google/uuid"
)

// ExternalAccountID is the single canonical UUID for the EXTERNAL system account.
// It acts as the counterparty for all Deposit operations (DEBIT EXTERNAL, CREDIT dst).
// Defined here so service.go and tests share exactly one constant.
var ExternalAccountID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

// TransactionKind classifies the nature of a ledger transaction.
type TransactionKind string

const (
	KindTransfer   TransactionKind = "TRANSFER"
	KindReversal   TransactionKind = "REVERSAL"
	KindDeposit    TransactionKind = "DEPOSIT"
	KindWithdrawal TransactionKind = "WITHDRAWAL" // reserved for M4
)

// Direction indicates whether an entry debits or credits an account.
type Direction string

const (
	DirectionDebit  Direction = "DEBIT"
	DirectionCredit Direction = "CREDIT"
)

// Outcome records whether an audited operation succeeded or failed.
type Outcome string

const (
	OutcomeSuccess Outcome = "SUCCESS"
	OutcomeFailure Outcome = "FAILURE"
)

// Account is a ledger account.
// IsSystem marks the EXTERNAL system account, which is permitted to carry a
// negative balance — this is the canonical liability semantic; see the
// accounts.is_system SQL comment in 0001_init.up.sql.
type Account struct {
	ID        uuid.UUID
	Name      string
	Balance   money.Money
	IsSystem  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Transaction is the header row for a double-entry ledger transaction.
// Entries holds the debit/credit legs (populated by store queries that join entries).
type Transaction struct {
	ID                    uuid.UUID
	Kind                  TransactionKind
	IdempotencyKey        string
	ReversesTransactionID *uuid.UUID
	CreatedAt             time.Time
	Entries               []Entry
}

// Entry is one leg of a double-entry transaction.
type Entry struct {
	ID            int64
	TransactionID uuid.UUID
	AccountID     uuid.UUID
	Direction     Direction
	AccountName   string
	Amount        money.Money
	CreatedAt     time.Time
}

// AuditEntry records a domain-level operation attempt in the audit_log table.
// Amount is a pointer so that operations with no monetary value marshal as null.
type AuditEntry struct {
	ID            int64
	Operation     string
	FromAccount   *uuid.UUID
	ToAccount     *uuid.UUID
	Amount        *money.Money
	Outcome       Outcome
	ErrorReason   *string
	TransactionID *uuid.UUID
	RequestID     *string
	CreatedAt     time.Time
}
