# Architecture

A walk-through of how the banking-service is built. Read this before the live session — every diagram below corresponds to code you can point at in the repo.

---

## 1. System layers

```
┌────────────────────────────────────────────────────────────────────┐
│                            BROWSER                                 │
│   web/index.html  —  static HTML                                   │
│   web/app.js      —  vanilla JS, fetch() to /api/*                 │
│   web/styles.css  —  plain CSS                                     │
└──────────────────────────────┬─────────────────────────────────────┘
                               │  HTTP (JSON)
                               ▼
┌────────────────────────────────────────────────────────────────────┐
│  HTTP LAYER  —  internal/httpapi/                                  │
│   • Server struct holds Service + Store + Logger                   │
│   • RegisterRoutes wires the 9 /api/* routes onto a *http.ServeMux │
│   • Each handler:                                                  │
│        parse JSON → validate at edge → call service                │
│        → mapError(sentinel → HTTP status) → writeJSON              │
│   • mapError table:                                                │
│        ErrAccountNotFound, ErrTransactionNotFound        → 404     │
│        ErrAlreadyReversed, ErrIdempotencyConflict        → 409     │
│        ErrInsufficientFunds, ErrNotReversible,                     │
│        ErrSelfTransfer, ErrSystemAccount, ErrInvalidAmount → 422   │
│        bad JSON / bad UUID / missing Idempotency-Key     → 400     │
│        anything else                                     → 500     │
└──────────────────────────────┬─────────────────────────────────────┘
                               │  Go function calls
                               ▼
┌────────────────────────────────────────────────────────────────────┐
│  SERVICE LAYER  —  internal/ledger/                                │
│   • Service struct holds a Storer interface + Logger               │
│   • Three public methods: Transfer, Reverse, Deposit               │
│   • Each method owns:                                              │
│        validation (positive amount, no self-transfer, etc.)        │
│        idempotency replay check                                    │
│        WithTx wrapping {Lock → re-read → check → update → insert}  │
│        audit-log write (outside the business tx)                   │
│   • Storer interface — declared HERE, satisfied by *store.Store    │
│     (dependency inversion: avoids the Go import cycle that would   │
│     happen if ledger imported store)                               │
└──────────────────────────────┬─────────────────────────────────────┘
                               │  Storer interface calls
                               ▼
┌────────────────────────────────────────────────────────────────────┐
│  PERSISTENCE LAYER  —  internal/store/                             │
│   • *Store wraps a *pgxpool.Pool                                   │
│   • WithTx(ctx, fn) — begins ReadCommitted tx, runs fn, commits    │
│   • LockAccountsForUpdate(tx, ids) — sorts ids by canonical UUID   │
│     string form, then SELECT … FOR UPDATE on those rows            │
│   • InsertTransaction, InsertEntries, InsertReversalTransaction    │
│     (the last one inspects pgErr.ConstraintName to distinguish     │
│     idempotency_key collision from reverses_transaction_id one)    │
│   • listEntries — JOINs accounts so each Entry carries AccountName │
│   • WriteAudit — runs OUTSIDE the business tx, on the pool         │
└──────────────────────────────┬─────────────────────────────────────┘
                               │  pgx (Postgres protocol)
                               ▼
┌────────────────────────────────────────────────────────────────────┐
│  DATABASE  —  PostgreSQL 16                                        │
│   tables:                                                          │
│      accounts        denormalised balance, is_system flag          │
│      transactions    header per double-entry event                 │
│      entries         debit/credit legs (one row each)              │
│      audit_log       outcome trail, no FK so failed lookups log    │
│   trigger:                                                         │
│      trig_check_double_entry — DEFERRABLE INITIALLY DEFERRED       │
│      asserts Σ(amount × sign(direction)) = 0 per transaction_id    │
│      at COMMIT time                                                │
└────────────────────────────────────────────────────────────────────┘
```

---

## 2. Code layout — where things live

| Path | Responsibility |
|---|---|
| `cmd/server/` | HTTP server entry. Reads `DATABASE_URL`/`PORT`. Runs `migrate.Up()`. Constructs Store → Service → Server. |
| `cmd/loadtest/` | Standalone concurrency tester. 8 workers × 50 iters of A↔B transfers; asserts balance conservation. |
| `internal/money/` | `Money` wrapper around `shopspring/decimal`. 4dp scale enforced at every constructor. |
| `internal/ledger/` | Domain types (Account, Transaction, Entry, AuditEntry, enums), sentinel errors, Service struct, `Storer` interface. |
| `internal/store/` | Postgres queries via `pgxpool`. `WithTx` helper. `LockAccountsForUpdate` (the only safe path for row locks). |
| `internal/httpapi/` | HTTP handlers. Request parsing. Sentinel-to-status mapping. JSON marshaling. |
| `migrations/` | `golang-migrate` SQL files. Schema, EXTERNAL seed, demo accounts. |
| `web/` | Plain HTML / JS / CSS. No bundler, no framework. |

---

## 3. Schema

```
                ┌──────────────────┐
                │   accounts       │
                │ ─────────────── │
                │  id    UUID PK   │
                │  name  TEXT      │
                │  balance NUMERIC │
                │  is_system BOOL  │
                │  created_at      │
                │  updated_at      │
                │                  │
                │  CHECK is_system │
                │   OR balance ≥ 0 │
                └────────┬─────────┘
                         │ 1
                         │
                         │ N
       ┌─────────────────┴──────────────────┐
       │                                    │
       ▼                                    ▼
┌──────────────┐                    ┌──────────────────┐
│   entries    │                    │   audit_log      │
│ ──────────── │                    │ ────────────────│
│  id BIGSERIAL│                    │  id BIGSERIAL    │
│  transaction_id ─────┐            │  operation TEXT  │
│  account_id (FK)     │            │  from_account UUID
│  direction enum      │            │  to_account   UUID
│  amount NUMERIC > 0  │            │  amount NUMERIC  │
│  created_at          │            │  outcome enum    │
└──────────────────────┼────────────│  error_reason    │
                       │            │  transaction_id  │
        ┌──────────────┘            │  request_id      │
        │ N                         │  created_at      │
        │                           │                  │
        │ 1                         │  no FK on        │
        ▼                           │  from/to_account │
┌──────────────────────────┐        │  (failed lookups │
│   transactions           │        │   must still log)│
│ ──────────────────────── │        └──────────────────┘
│  id UUIDv7 PK            │
│  kind enum (TRANSFER     │
│    | REVERSAL            │
│    | DEPOSIT             │
│    | WITHDRAWAL reserved)│
│  idempotency_key UNIQUE  │
│  reverses_transaction_id │
│    → transactions(id)    │
│    UNIQUE                │
│  created_at              │
└──────────────────────────┘

CONSTRAINT TRIGGER trig_check_double_entry
  ON entries
  AFTER INSERT OR UPDATE
  DEFERRABLE INITIALLY DEFERRED
  → asserts SUM(amount × sign(direction)) = 0 per transaction_id
    at COMMIT time
```

### Why each constraint is load-bearing

| Constraint | What it prevents | Where defined |
|---|---|---|
| `accounts.CHECK (is_system OR balance ≥ 0)` | A user account going negative | [migrations/0001_init.up.sql](../migrations/0001_init.up.sql) |
| `entries.CHECK (amount > 0)` | Zero/negative legs (sign is in `direction`, not amount) | same |
| `transactions.idempotency_key UNIQUE` | Duplicate operations under retries | same |
| `transactions.reverses_transaction_id UNIQUE` | Reversing the same transfer twice | same |
| `trig_check_double_entry` (deferred) | Σdebits ≠ Σcredits within a transaction | same, lines ~63–89 |

The deferred trigger is the headline guarantee. **Even if the application code is buggy, even if a DBA runs raw SQL, the database itself rejects unbalanced transactions at COMMIT.** That's the answer to the reviewer's "what stops the DB from going inconsistent?"

---

## 4. Request flows

### 4.1 Transfer

```
USER fills form, clicks "Transfer"
   │
   │ POST /api/transfers
   │ Headers: Idempotency-Key: <fresh UUID>
   │ Body:    { "src": "...", "dst": "...", "amount": "100.00" }
   ▼
internal/httpapi/handlers.go : createTransfer()
   • parse JSON                    ─ on error: 400 invalid_request
   • read Idempotency-Key header   ─ if missing: 400 missing_idempotency_key
   • read X-Request-ID header      ─ if missing: generate UUID
   • call svc.Transfer(...)
   ▼
internal/ledger/service.go : Transfer(ctx, src, dst, amt, idemKey, reqID)
   ┌─ STEP 1 — VALIDATE (pre-tx)
   │    if !amt.IsPositive() → audit FAILURE; return ErrInvalidAmount
   │    if src == dst        → audit FAILURE; return ErrSelfTransfer
   │    if src or dst is EXTERNAL → audit FAILURE; return ErrSystemAccount
   │
   ├─ STEP 2 — IDEMPOTENCY REPLAY CHECK
   │    tx, entries, err := store.GetTransactionByIdempotencyKey(idemKey)
   │    if err == nil → return existing tx, entries (no audit, no DB write)
   │    if err == ErrTransactionNotFound → continue
   │    if err == anything else → audit FAILURE; return err
   │
   ├─ STEP 3 — ATOMIC UPDATE (inside store.WithTx)
   │    BEGIN ReadCommitted
   │    LockAccountsForUpdate(tx, [src, dst])      ← sorted by UUID-string
   │      → SELECT ... FOR UPDATE on both rows in deterministic order
   │      → other concurrent writers QUEUE here at the DB lock
   │    re-read both account balances              ← post-lock, fresh values
   │    if src.balance < amount → ErrInsufficientFunds (audit FAILURE on rollback)
   │    UpdateBalance(src, src.balance - amount)
   │    UpdateBalance(dst, dst.balance + amount)
   │    InsertTransaction({kind=TRANSFER, idemKey, ...})
   │      → may return ErrIdempotencyConflict if race-loser
   │    InsertEntries([
   │       {tx_id, src, DEBIT,  amount},
   │       {tx_id, dst, CREDIT, amount}
   │    ])
   │    COMMIT
   │      → trig_check_double_entry fires HERE (deferred)
   │      → assertion Σ = 0 must hold for this transaction_id
   │
   │    if ErrIdempotencyConflict (the race path):
   │       re-fetch GetTransactionByIdempotencyKey
   │       return that tx (someone else won the race; we replay)
   │
   ├─ STEP 4 — AUDIT (outside tx, success path)
   │    WriteAudit({operation=TRANSFER, from=src, to=dst, amount,
   │                outcome=SUCCESS, transaction_id=&txID, request_id})
   │    audit uses the pool, not the tx → not rolled back if commit failed
   │    (a failed commit → we never got here; that path audits FAILURE)
   │
   └─ return tx, entries, nil
   ▼
internal/httpapi/handlers.go (continued)
   • writeJSON(w, 201, transaction)   ← Entries inline; AccountName JOINed
   ▼
USER browser:
   • renders new balances in Account list
   • appends row to Account History (if account is open)
   • appends row to Ledger Activity
```

### 4.2 Reverse

Mirror image of Transfer, with two extra checks before the WithTx:

```
GET original transaction:                    ─ load tx, entries via GetTransaction
  if not found        → ErrTransactionNotFound (404)
  if kind != TRANSFER → ErrNotReversible      (422)
identify origSrc = entry with DIRECTION=DEBIT
identify origDst = entry with DIRECTION=CREDIT

WithTx:
  Lock [origSrc, origDst]
  re-read balances
  if origDst.balance < amount → ErrInsufficientFunds (N4-a: reject)
  UpdateBalance(origSrc, +amount)              ← restore source
  UpdateBalance(origDst, -amount)              ← debit destination
  InsertReversalTransaction({                  ← uses InsertReversalTransaction
    kind=REVERSAL,                              not InsertTransaction
    idempotencyKey,
    reversesTransactionID = original.ID         UNIQUE on this column means
  })                                            second reverse → 23505
                                                inspected by pgErr.ConstraintName:
                                                  if name = ...reverses_..._key
                                                    → ErrAlreadyReversed (409)
                                                  if name = ...idempotency_key_key
                                                    → ErrIdempotencyConflict (409)
  InsertEntries([
    {revTxID, origDst, DEBIT,  amount},        ← mirrored direction
    {revTxID, origSrc, CREDIT, amount}
  ])
  COMMIT (deferred trigger fires; Σ = 0 holds)

Audit + return.
```

### 4.3 Deposit

```
Validate: amount > 0, dst != EXTERNAL
Idempotency replay check
WithTx:
  Lock [EXTERNAL, dst]                         ← EXTERNAL UUID sorts first
  UpdateBalance(EXTERNAL, ext.balance - amount)  ← may go negative
  UpdateBalance(dst, dst.balance + amount)
  InsertTransaction({kind=DEPOSIT, ...})
  InsertEntries([
    {txID, EXTERNAL, DEBIT,  amount},
    {txID, dst,      CREDIT, amount}
  ])
  COMMIT
Audit + return.
```

### 4.4 Idempotency replay — sequential vs concurrent

**Sequential replay** (the easy case):

```
Time 1:  POST /api/transfers idem=X  → service does full work; tx-X stored
Time 2:  POST /api/transfers idem=X  →
   ┌─ Step 2 in service: GetTransactionByIdempotencyKey(X) → returns tx-X
   ├─ return tx-X, entries, nil
   └─ no DB writes, no audit row
   handler returns 201 with the SAME transaction body
```

**Concurrent replay** (the race-loser case):

```
Time T:    Goroutine A: POST /api/transfers idem=X
Time T+1µs Goroutine B: POST /api/transfers idem=X
   Both reach Step 2.
   Both call GetTransactionByIdempotencyKey(X).
   Both receive ErrTransactionNotFound (no row exists yet).
   Both proceed to Step 3 (WithTx).

   Inside WithTx, LockAccountsForUpdate serialises them on the account row
   locks. Suppose A wins the race for the lock.

   Goroutine A:
     • locks accounts
     • InsertTransaction({idemKey: X}) → success
     • InsertEntries        → success
     • COMMIT               → success
     • trigger fires        → Σ = 0; passes
   Goroutine A returns the new tx.

   Goroutine B (was queued):
     • acquires the lock after A commits
     • re-reads balances (now post-A)
     • InsertTransaction({idemKey: X}) → 23505 on idempotency_key UNIQUE
       → store wraps to ErrIdempotencyConflict
     • service catches ErrIdempotencyConflict
     • re-calls GetTransactionByIdempotencyKey(X) → returns A's tx
     • returns A's tx to handler
   Goroutine B's handler returns 201 with A's transaction body.
```

Net effect: exactly one transaction row, exactly one audit row, both clients see the same response. The race-loser does NOT write a duplicate audit row.

---

## 5. Concurrency model

### The single locking primitive

```
internal/store/accounts.go : LockAccountsForUpdate(tx, ids)
   • sorts ids ascending by uuid.UUID.String() canonical form
   • runs:
        SELECT id, name, balance, is_system, created_at, updated_at
        FROM accounts
        WHERE id = ANY($1)
        ORDER BY id::text
        FOR UPDATE
   • returns the locked rows
```

**Sorting happens INSIDE this function so callers cannot bypass.** The doc comment says "calling SELECT FOR UPDATE on accounts directly is forbidden." Every Service method that touches multiple accounts goes through this single function.

### Timing diagram — two concurrent transfers between same A↔B

```
                    T0           T1                      T2
Goroutine X  │ BEGIN  LockAccountsForUpdate([A,B])   ── work ──   COMMIT
                                ▲                                 │
                                │ holds row locks on A and B      │ releases
                                │                                 │ locks
Goroutine Y                     QUEUE on FIRST(A)                 │
             │                   blocks at the SQL level          ▼
                                                                  WAKE
                                                                  re-read A & B
                                                                  (sees X's update)
                                                                  ── work ── COMMIT
```

No deadlock can occur because both goroutines lock A first, then B (deterministic UUID-string sort). The second arriver waits, reads the post-commit state, and proceeds.

### Why no in-process mutex

We deliberately do NOT have a `map[uuid.UUID]*sync.Mutex`. The DB locks alone are correct and:
- Survive horizontal scaling (an in-process mutex would not).
- Have no memory growth (the map would).
- Can be cancelled by `ctx.Done()` (a `sync.Mutex` cannot).
- Are the single source of truth a reviewer can probe.

Trade-off: under same-process burst contention, every queued goroutine holds a DB connection while waiting. With the default `pgxpool` size (~12) and our take-home TPS, this is not a real problem.

---

## 6. The deferred trigger — a closer look

```sql
CREATE OR REPLACE FUNCTION check_double_entry_balance()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    v_sum NUMERIC;
BEGIN
    SELECT COALESCE(SUM(amount * CASE direction
                                  WHEN 'CREDIT' THEN 1
                                  ELSE -1 END), 0)
    INTO v_sum
    FROM entries
    WHERE transaction_id = NEW.transaction_id;

    IF v_sum <> 0 THEN
        RAISE EXCEPTION 'double-entry invariant violated for transaction %: net sum is %',
            NEW.transaction_id, v_sum;
    END IF;

    RETURN NEW;
END;
$$;

CREATE CONSTRAINT TRIGGER trig_check_double_entry
    AFTER INSERT OR UPDATE ON entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION check_double_entry_balance();
```

### Two non-obvious things about this

1. **`CONSTRAINT TRIGGER`, not `CREATE TRIGGER`.** Only `CREATE CONSTRAINT TRIGGER` supports `DEFERRABLE`. A regular `CREATE TRIGGER ... AFTER INSERT` would fire after every single INSERT, which fails after the first leg ("Σ = 100, not 0") even though the COMMIT would balance.
2. **`INITIALLY DEFERRED`** means the check runs at COMMIT time by default, not per-row. So we can insert the DEBIT leg, then the CREDIT leg, and the check runs once at the end. Both legs visible, sum is zero, transaction commits.

### What happens if you bypass it?

You can't. The trigger fires:
- When `feature` agent's `InsertEntries` runs both legs and commits — passes.
- If a buggy code path inserts only one leg and commits — FAILS at COMMIT. Whole tx rolls back. The application sees a Postgres error on `tx.Commit(ctx)`.
- If a DBA opens psql and runs `INSERT INTO entries ...; COMMIT;` — same failure.
- If someone updates an entry's amount (the ON UPDATE clause) — re-checks, FAILS if it breaks balance.

The only way to silently break the invariant would be to disable the trigger first, which a reviewer would notice in the schema migration history.

---

## 7. Audit log

### What gets logged

Every domain-level attempt:

| Outcome | When | Captured |
|---|---|---|
| `SUCCESS` | After the business tx commits | operation, from, to, amount, transaction_id, request_id, timestamp |
| `FAILURE` | When the service returns a typed sentinel error | operation, from, to, amount, **error_reason**, request_id (no transaction_id) |

What DOESN'T get logged:
- HTTP-layer errors (bad JSON, missing Idempotency-Key) — those go to `slog`.
- Idempotency replay (sequential or race-loser path) — original row already exists.

### The "outside the tx" pattern

`WriteAudit` runs against the pool, not the tx. If the business tx rolls back, the audit row is still committed. That's intentional: you want to see "this attempt happened and failed because of insufficient funds" even when the failure caused a rollback.

Conversely, if the business tx commits but `WriteAudit` fails, the service logs loudly and still returns success. You can't undo a transfer because the audit log writer is down.

### Failure auditing — every path

| Domain error | Where caught | Audit row |
|---|---|---|
| `ErrInvalidAmount` | service Step 1 (pre-tx) | yes, FAILURE |
| `ErrSelfTransfer` | service Step 1 | yes, FAILURE |
| `ErrSystemAccount` | service Step 1 | yes, FAILURE |
| `ErrAccountNotFound` (mid-lock) | service Step 3 (mid-tx) | yes, FAILURE |
| `ErrInsufficientFunds` | service Step 3 | yes, FAILURE |
| `ErrTransactionNotFound` (Reverse) | service Step 2 of Reverse | yes, FAILURE |
| `ErrNotReversible` | service Step 2 of Reverse | yes, FAILURE |
| `ErrAlreadyReversed` | service Step 3 of Reverse | yes, FAILURE |
| `ErrIdempotencyConflict` (race) | replay path | NO (original already audited) |

---

## 8. Idempotency mechanism

### Storage-level guarantees

`transactions.idempotency_key TEXT NOT NULL UNIQUE` — one row max per key, ever, across all operation types.

This means: a `Deposit` with `idem=X` followed by a `Transfer` with `idem=X` (cross-type collision) returns the Deposit's transaction in response to the Transfer call. Per N5-a, we explicitly do not validate that the request body matches the stored transaction — clients are trusted to use unique keys per logical request.

### Replay vs conflict — how the service distinguishes

```
Service.Transfer (or Reverse, or Deposit)
   ┌── Step 2: GetTransactionByIdempotencyKey
   │     ├── HIT  → return existing (sequential replay)
   │     └── MISS → continue to Step 3
   │
   └── Step 3: WithTx → InsertTransaction
         ├── 23505 on transactions_idempotency_key_key
         │      → ErrIdempotencyConflict (race-loser path)
         │      → service re-fetches and returns the winner's tx
         │
         └── 23505 on transactions_reverses_transaction_id_key (Reverse only)
                → ErrAlreadyReversed (different Reverse beat us to it)
                → handler returns 409
```

The constraint-name dispatch in `InsertReversalTransaction` is the load-bearing trick that makes "double-reverse" return the right error code instead of the misleading `ErrIdempotencyConflict`.

---

## 9. Error contract

### Sentinel errors → HTTP status

```
internal/ledger/errors.go declares 9 sentinels.
internal/httpapi/handlers.go : mapError() does the dispatch.

  ErrAccountNotFound        404   ─ resource not found (read or write)
  ErrTransactionNotFound    404
  ErrAlreadyReversed        409   ─ conflict with existing state
  ErrIdempotencyConflict    409
  ErrInsufficientFunds      422   ─ semantic violation (request makes
  ErrNotReversible          422     sense but can't be applied)
  ErrSelfTransfer           422
  ErrSystemAccount          422
  ErrInvalidAmount          422
  (json/uuid parse error)   400   ─ malformed request
  (missing Idempotency-Key) 400
  anything else             500   ─ logged via slog; client gets generic msg
```

### Response body shape

All error responses share one shape:
```json
{ "error": "machine_code", "message": "human-readable text" }
```

Not RFC 7807. Deliberately simple — easy to render in the UI, easy to grep in logs.

---

## 10. Where the layers couple

The only places one layer reaches into another:

```
Browser   →   HTTP handler          via fetch(), JSON
HTTP      →   Service               via concrete method calls
Service   →   Storer interface       declared in ledger; *store.Store satisfies it
Service   →   Money                 value type, no coupling concerns
Store     →   pgxpool               concrete dependency
Migrations →  Postgres              SQL strings
```

The `Storer` interface in `internal/ledger/service.go` is the only "abstraction that pays rent" — it exists because `internal/store` already imports `internal/ledger` (for `Account`, `Entry`, etc.), and Go forbids the cycle. The interface lives in the *consumer* (service), not the *provider* (store) — classic dependency inversion. Method wrappers in `internal/store/accounts.go` and `transactions.go` make `*Store` satisfy the interface.

If you ever need to mock the persistence layer for a service-level unit test, you can satisfy `Storer` with a fake. Today's tests don't bother — they hit a real testcontainers Postgres and assert against actual DB state.

---

## 11. Non-trivial files to know cold

If you have 5 minutes before the live session, skim these:

1. [internal/ledger/service.go](../internal/ledger/service.go) — the three operations end-to-end, ~250 lines.
2. [internal/store/postgres.go](../internal/store/postgres.go) — Store struct, `WithTx`, ReadCommitted note.
3. [internal/store/accounts.go](../internal/store/accounts.go) — `LockAccountsForUpdate` with the canonical-string sort.
4. [migrations/0001_init.up.sql](../migrations/0001_init.up.sql) — the schema and the deferred trigger (lines ~63–89 are the headline).
5. [internal/httpapi/handlers.go](../internal/httpapi/handlers.go) — the `mapError` dispatch table.

The rest is variations on those patterns.
