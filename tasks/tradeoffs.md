# Architecture Trade-offs

Running log of every locked decision. Source for the take-home README synthesised at M7. Each entry: **Decision** / **Alternatives considered** / **Why this one** / **Trade-offs accepted**.

---

## Persistence layer

### Database: PostgreSQL
- **Alternatives:** MySQL (InnoDB).
- **Why:** Stronger correctness primitives for ledger work — `pg_advisory_xact_lock`, SERIALIZABLE (SSI) that's actually usable, `NUMERIC` arbitrary precision, `INSERT ... ON CONFLICT ... RETURNING` for idempotency. Stronger CHECK / constraint culture.
- **Accepted:** Postgres is less common in Indian fintech stacks than MySQL, so reviewers may probe MySQL-shaped questions; we'd answer with the Postgres equivalent.

### Driver: pgx v5 (native, `pgxpool`)
- **Alternatives:** `lib/pq` + `database/sql`; `pgx` via the `database/sql` shim.
- **Why:** Native `NUMERIC` round-tripping, faster, modern. Better error-typing for `pgconn.PgError` (used to map `23505` → `ErrIdempotencyConflict`).
- **Accepted:** Non-stdlib API; couples us to the `pgx` package surface.

### Migrations: golang-migrate as a Go library
- **Alternatives:** `golang-migrate` CLI; `pressly/goose`; hand-rolled SQL files run on boot.
- **Why:** One binary does the whole thing — `./server` runs `migrate.Up()` then opens the listener. Cleaner live demo. Plain `*.sql` files keep the schema readable.
- **Accepted:** Adds a transitive dependency (and several indirects). Failing migrations abort startup, which is the right behaviour for a take-home.

---

## Domain model

### Ledger model: double-entry (`transactions` header + `entries` legs)
- **Alternatives:** Single-row movements (one row with `from_account`, `to_account`, `amount`).
- **Why:** Textbook for banking; generalises to fees, splits, multi-leg ops with no special cases. Invariant `Σ debits = Σ credits per transaction` is checkable in code and in SQL.
- **Accepted:** ~30–50% more code than single-row. Balance is denormalised on `accounts.balance` to keep reads fast.

### Money: `shopspring/decimal`, `NUMERIC(20, 4)` in DB
- **Alternatives:** `cockroachdb/apd` (per-context precision/rounding).
- **Why:** De-facto standard in Go fintech, well-known to reviewers, ergonomic API. 4dp scale is enforced at the `Money` constructor (`Round(4)`).
- **Accepted:** Global rounding mode rather than per-context. Not a problem for the assignment scope.

### Currency: INR-only (no `currency` column)
- **Alternatives:** Multi-currency with `currency` on every account/entry/transaction.
- **Why:** Assignment doesn't require FX. One less column, no currency-mismatch validation. Documented as an explicit assumption.
- **Accepted:** Cannot model a multi-currency account without a schema change.

### IDs: UUID v7 (accounts, transactions); BIGSERIAL (entries)
- **Alternatives:** `BIGSERIAL` everywhere; UUID v4 everywhere.
- **Why:** UUID v7 is time-ordered → better B-tree locality than v4 while still being URL-safe. `BIGSERIAL` for entries (internal-only) is more compact and reads cleanly in logs.
- **Accepted:** Slightly more complex than uniform `BIGSERIAL`; UUID v7 requires `google/uuid` v1.6+.

### Accounts: plain user accounts + one hidden `EXTERNAL` system account
- **Alternatives:** Plain user accounts only (no system account); full chart-of-accounts (asset/liability/equity).
- **Why:** Without a system counterparty, the double-entry invariant fundamentally can't hold for deposits/seeding — money has nowhere to come from. EXTERNAL preserves the invariant cleanly.
- **Accepted:** Slightly contradicts a "users only" purist reading, but `is_system` keeps it cleanly separated. EXTERNAL is allowed to carry a negative balance — that's the canonical liability semantic for a system source account.

### Account fields: `id, name, balance, is_system, created_at, updated_at`
- **Alternatives:** Add `email`; add a separate `users` table.
- **Why:** Assignment is about ledger correctness, not user management.
- **Accepted:** No name uniqueness constraint; two accounts with the same name are legal. Documented.

### Reversal scope: only `kind = TRANSFER` rows can be reversed
- **Alternatives:** Anything reversible (a reversal is just another transfer).
- **Why:** Simpler invariant. UNIQUE on `reverses_transaction_id` enforces "at most one reversal per original" in the schema.
- **Accepted:** Cannot reverse a reversal directly. To "redo" you'd issue a fresh transfer.

### Reversal that would push original-destination negative: reject
- **Alternatives:** Allow B to go negative on a REVERSAL; configurable flag.
- **Why:** Preserves the `non_negative_user_balance` CHECK, simpler invariant to defend.
- **Accepted:** Real banks force-debit on chargebacks; we don't. Documented.

### Idempotency replay: return existing transaction when key matches
- **Alternatives:** Hash request body + key, return 409 on mismatch (Stripe-style).
- **Why:** Crisp. One DB read on replay. Documented expectation: clients must not reuse an idempotency-key for a different request.
- **Accepted:** A misbehaving client that sends `idem-X` for two different transfers will get a "success" response for the second that doesn't match the body it sent. This is a documented trust contract.

### Audit log: single table; logs domain-level attempts only
- **Alternatives:** Audit only failures; split tables for success/failure; full HTTP-level audit including malformed-JSON.
- **Why:** One table to point at when a reviewer asks "where's the audit log?". Domain-level scope keeps the table meaningful — structured request logs cover HTTP-level noise.
- **Accepted:** Validation failures at the HTTP boundary (bad JSON) are NOT in `audit_log` — they're in `slog`.

---

## Error contract

### Sentinel errors via `errors.Is`
- **Alternatives:** `(value, bool, error)` tuple for exists checks; error-codes-as-strings.
- **Why:** Uniform: `GetAccount`, `GetTransaction`, `GetTransactionByIdempotencyKey` all return `(value, error)` with a typed sentinel on miss. One contract, easy to map to HTTP status codes in M4.
- **Accepted:** Caller must `errors.Is`; not as cheap as a bool.

### `WriteAudit` returns errors honestly (no fire-and-forget)
- **Alternatives:** Always return nil; log internally on failure.
- **Why:** Caller (M3 service) chooses policy. Standard policy will be "log loudly + still return success when business tx already committed."
- **Accepted:** Slight extra burden on callers; honest semantics worth it.

---

## Concurrency

### Double-entry invariant: enforced at the DB via DEFERRABLE INITIALLY DEFERRED CONSTRAINT TRIGGER
- **Alternatives:** Application-level only (relying on `WithTx` + `InsertEntries` always being paired).
- **Why:** "What stops the DB from going inconsistent?" — answer is "the DB does," not "our code." Bulletproof against future bugs, debugging SQL, third-party tools.
- **Accepted:** ~30 lines of PL/pgSQL; reviewers must understand `CONSTRAINT TRIGGER` semantics (they're rarer than regular triggers).

### Account-level locking: per-row `SELECT ... FOR UPDATE` in deterministic UUID-string order
- **Alternatives:** `pg_advisory_xact_lock`; SERIALIZABLE + retry; per-account `sync.Mutex` (in-process) layered on top of DB locks; pure in-process mutex with no DB locks.
- **Why:** Simplest model that's actually correct. One lock primitive. Sort happens *inside* `LockAccountsForUpdate` so callers can't bypass it. Survives horizontal scaling unchanged.
- **Accepted:** Each waiter holds a Postgres connection during the queue; under hot-account burst with a small pool, unrelated requests can starve. Mitigated by lock_timeout, short transactions, sufficient pool size — not a problem at take-home TPS.

### Service struct: single `ledger.Service` with `Transfer / Reverse / Deposit` methods
- **Alternatives:** Three separate structs (`TransferService`, `ReversalService`, `DepositService`).
- **Why:** One file, one constructor, one logger, one Store reference. Crisp top-to-bottom read.
- **Accepted:** Slightly less "single responsibility" purist; pragmatic for a small surface.

---

## API surface

### JSON field names: PascalCase (Go default, no `json:"..."` tags)
- **Alternatives:** camelCase via `json:"id"`/`json:"name"` tags; snake_case via `json:"created_at"`.
- **Why:** Domain types in `internal/ledger/types.go` are kept tag-free for crispness — one struct definition serves both internal Go code and the JSON wire format. Frontend reads `acc.ID`, `acc.Name`, `acc.Balance` to match.
- **Accepted:** Uppercase JSON keys are unconventional for a public API. A reviewer may ask "why not camelCase?" — answer: "to keep the type definitions minimal; if the API ever ships externally we'd add tags then." If the reviewer pushes back on this in the live session, adding `json:"camelCase"` tags is a 10-line change that doesn't touch logic.

## Cross-package wiring

### Service depends on a `ledger.Storer` interface, not on `*store.Store`
- **Alternatives:** Have `service.go` import `internal/store` directly; merge `ledger` and `store` into one package; introduce a third package (`domain`) that both depend on.
- **Why:** `internal/store` already imports `internal/ledger` (for the Account/Transaction types). A direct import the other way would create a Go import cycle. The standard fix in idiomatic Go is dependency inversion: `ledger` declares an interface describing what it needs from persistence; `*store.Store` satisfies the interface via thin method wrappers.
- **Implementation:** `Storer` interface in `internal/ledger/service.go` (~10 method signatures); 12 lines of method wrappers in `internal/store/accounts.go` and `internal/store/transactions.go` that call the existing package-level functions. Existing tests are unchanged.
- **Accepted:** One small deviation from the original M3+M4 Change Contract (`internal/store/accounts.go` was modified, not just `transactions.go` and `audit.go`). The QA agent confirmed this as ACCEPTABLE: it's a Go-language constraint, not a design choice. Defensible answer in the live session: "I can't have a circular import, so the persistence layer is behind an interface owned by the domain layer."

## Function length budget

### Service methods exceed the 30-line crisp-code budget; comments document section flow
- **Alternatives:** Decompose each method into ~5 private helpers (`executeTransferTx`, `validateTransferInputs`, `auditAndReturn`, etc.) so each method reads in <30 lines.
- **Why:** `Transfer`, `Reverse`, and `Deposit` each have an essential linear flow: validate → idempotency replay → `WithTx{lock → re-read → check → update → 2 inserts}` → audit → return. Decomposing fragments the algorithm across multiple files/functions and makes the live walkthrough harder, not easier — the reviewer would jump between `executeTransferTx` and its private helpers to follow one transfer.
- **Accepted:** QA reported a PARTIAL on the explicit crisp-code line-budget criterion. Methods are well-commented with inline section markers but lack a single dedicated "exceeds 30 lines because..." annotation. We accepted the PARTIAL to keep the algorithm readable end-to-end. If a reviewer challenges length in the live session, the answer is: "the alternative is fragmenting one transfer across 5 functions."

## Bundle / pipeline choices

### M1+M2 shipped together (G1)
- **Alternatives:** Split into M1 then M2.
- **Why:** Splitting forces M1 to ship without anything actually using it (no schema, no Store) — wasted ceremony. Foundation modules go together.
- **Accepted:** Bundle is 18 files (3 modify + 15 create), exceeding the 15-file default in CLAUDE.md by 3.

### M3 + M4 will ship together (N1-b)
- **Alternatives:** M3 alone first, then M4.
- **Why:** Bundling lets us land a curl-able, browser-demoable app in one shot — closes the loop between domain logic and observable behaviour.
- **Accepted:** Bigger contract; M3's concurrency story shares the spotlight with M4's HTTP plumbing.

---

> Update this file every time a new decision is locked. Keep it the source of truth for the eventual README at M7.
