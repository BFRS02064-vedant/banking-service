# Banking Service

A take-home ledger service written in Go. It implements account creation, INR
transfers, reversals, deposits, double-entry bookkeeping, and a full audit log.
Concurrency is handled via PostgreSQL row-level locking with a deterministic
lock-ordering rule, so the invariant "the sum of all account balances is
constant" is maintained under arbitrary concurrent load. There is no
authentication, no multi-currency support, and no horizontal scaling — all
documented limitations for a take-home scope.

---

## Quickstart

### Docker (recommended — one command)

Requires Docker and Docker Compose.

```
docker-compose up
```

The app container waits for the Postgres healthcheck, then runs migrations
automatically before starting the HTTP listener. Once you see
`banking-service listening addr=:8080`, open `http://localhost:8080` in a browser.

> If port 8080 is already bound on your machine, stop the conflicting process
> or change the `ports` mapping in `docker-compose.yml` before starting.

### Local dev

Requires Go 1.25+ and a running Postgres instance.

```
cp .env.example .env
# Edit .env if your Postgres credentials differ.
export $(cat .env | xargs)
go run ./cmd/server
```

Migrations run at startup via `migrate.Up()`. No separate migration step is needed.

---

## Architecture overview

```
Browser ──HTTP──► httpapi.Server ──► ledger.Service ──► store.Store ──► PostgreSQL
                    (handlers)          (domain)         (pgx/v5)
                        │                  │                 │
                      parse,            validate,         WithTx,
                      map errors        idempotency       FOR UPDATE,
                      to HTTP           replay, audit     deferred trigger
```

- `httpapi.Server` — thin HTTP layer: decode JSON, validate UUIDs/amounts,
  call domain, map sentinel errors to HTTP status codes, write JSON responses.
- `ledger.Service` — all business logic: idempotency replay, account locking,
  balance checks, double-entry entry creation, audit writes.
- `store.Store` — raw Postgres operations via `pgxpool`. Implements the
  `ledger.Storer` interface to avoid an import cycle.
- PostgreSQL — source of truth; the deferred constraint trigger enforces the
  double-entry invariant at the database level.

---

## Schema

```
accounts
  id           UUID PK
  name         TEXT NOT NULL
  balance      NUMERIC(20,4) DEFAULT 0
  is_system    BOOLEAN DEFAULT FALSE
  created_at   TIMESTAMPTZ
  updated_at   TIMESTAMPTZ
  CONSTRAINT non_negative_user_balance CHECK (is_system OR balance >= 0)

transactions
  id                      UUID PK
  kind                    TEXT  CHECK IN ('TRANSFER','REVERSAL','DEPOSIT','WITHDRAWAL')
  idempotency_key         TEXT  UNIQUE
  reverses_transaction_id UUID  UNIQUE REFERENCES transactions(id)
  created_at              TIMESTAMPTZ

entries
  id             BIGSERIAL PK
  transaction_id UUID REFERENCES transactions(id)
  account_id     UUID REFERENCES accounts(id)
  direction      TEXT CHECK IN ('DEBIT','CREDIT')
  amount         NUMERIC(20,4) CHECK (amount > 0)
  created_at     TIMESTAMPTZ

audit_log
  id             BIGSERIAL PK
  operation      TEXT
  from_account   UUID  (no FK — failed lookups still log)
  to_account     UUID
  amount         NUMERIC(20,4)
  outcome        TEXT CHECK IN ('SUCCESS','FAILURE')
  error_reason   TEXT
  transaction_id UUID
  request_id     TEXT
  created_at     TIMESTAMPTZ
```

The deferred constraint trigger `trig_check_double_entry` fires `AFTER INSERT OR
UPDATE ON entries DEFERRABLE INITIALLY DEFERRED`. It runs at `COMMIT` time, not
per-row, so both legs of a transfer can be inserted before the invariant is
checked. Any imbalance raises a Postgres exception and rolls back the transaction.

---

## API reference

| Method | Path                            | Body                                                                 | Idempotency-Key | Success |
|--------|---------------------------------|----------------------------------------------------------------------|-----------------|---------|
| GET    | /api/accounts                   | —                                                                    | —               | 200     |
| POST   | /api/accounts                   | `{"name":"Alice"}`                                                   | —               | 201     |
| GET    | /api/accounts/{id}              | —                                                                    | —               | 200     |
| POST   | /api/transfers                  | `{"src_account_id":"...","dst_account_id":"...","amount":"100.00"}` | required        | 201     |
| POST   | /api/transfers/{id}/reverse     | —                                                                    | required        | 201     |
| POST   | /api/deposits                   | `{"dst_account_id":"...","amount":"500.00"}`                         | required        | 201     |
| GET    | /api/accounts/{id}/transactions | — (query: `?limit=N`)                                                | —               | 200     |
| GET    | /api/accounts/{id}/audit        | — (query: `?limit=N`)                                                | —               | 200     |
| GET    | /api/transactions               | — (query: `?limit=N&offset=M&kind=K`)                                | —               | 200     |
| GET    | /healthz                        | —                                                                    | —               | 200     |

Every transaction response includes an `Entries` array. Each entry in that array
includes an `AccountName` field — the display name of the account on that leg,
populated via a `JOIN accounts` at query time (no stored denormalisation).
Example entry shape: `{"Direction":"DEBIT","Amount":"100.0000","AccountName":"Alice", ...}`.
The `EXTERNAL` system account appears as the literal name `EXTERNAL`.

The `GET /api/transactions` endpoint uses offset pagination (`?limit`, `?offset`).
Offset pagination is not stable under concurrent writes: if a new transaction is
inserted between two `Load more` clicks, row boundaries shift and a row may appear
twice or be skipped. Cursor-based pagination (e.g. `?before=<id>`) would be the
production-grade choice; offset is used here to keep the implementation simple.

`?kind` accepts case-sensitive uppercase values: `TRANSFER`, `REVERSAL`, `DEPOSIT`,
`WITHDRAWAL`. Any other value returns `400 {"error":"invalid_kind"}`.

Error responses have a stable shape: `{"error":"code","message":"..."}`.

| Condition                                         | Status |
|---------------------------------------------------|--------|
| Account / transaction not found                   | 404    |
| Already reversed / idempotency conflict           | 409    |
| Insufficient funds, self-transfer, invalid amount | 422    |
| Internal errors                                   | 500    |

JSON field names are PascalCase (Go struct defaults — no `json:""` tags on domain
types). The frontend reads `acc.ID`, `acc.Name`, `acc.Balance` to match.

---

## Concurrency model

`store.LockAccountsForUpdate` sorts account UUIDs by their canonical string form
before issuing `SELECT ... FOR UPDATE`. This deterministic ordering prevents the
classic A-locks-B-while-B-locks-A deadlock on any pair of accounts. Self-transfers
are rejected at the service layer before any lock is taken.

There are no in-process `sync.Mutex` maps. The only concurrency primitive is
PostgreSQL row-level locking. This means the model survives horizontal scaling
unchanged — a second server instance would acquire the same DB row locks and queue
correctly. Under hot-account burst with a small connection pool, unrelated requests
can wait for a connection; this is mitigated by short transactions and a
`lock_timeout` setting. At take-home TPS it is not a practical concern.

The DEFERRABLE INITIALLY DEFERRED constraint trigger backstops any code-level
mistake: even if a bug bypassed the service layer, the DB would reject a commit
that violates `Σ debits = Σ credits` per transaction.

---

## How to test concurrency

### a) Built-in load test

Run the server (or `docker-compose up`), then:

```
go run ./cmd/loadtest
```

Default: 8 workers, 50 iterations each = 400 bidirectional transfers on the Alice
and Bob seed accounts. Workers alternate direction (A→B on even iterations, B→A on
odd) so balances stay bounded. On success the tool prints:

```
load test summary
  target:                 http://localhost:8080
  src:                    00000000-0000-0000-0000-000000000002
  dst:                    00000000-0000-0000-0000-000000000003
  workers x iters:        8 x 50 = 400 ops
  elapsed:                3.21s
  throughput:             124.6 ops/s
  successes:              400
  failures by error_code: (none)
  start balance src+dst:  20000.0000
  end balance src+dst:    20000.0000
  invariant:              PASS
```

Exit code is 0 on PASS, 1 on any invariant failure or unexpected errors.

Flags: `-base-url`, `-workers`, `-iters`, `-amount`, `-src`, `-dst`, `-timeout`.

### b) Manual curl (race concurrent transfers)

```bash
for i in $(seq 1 100); do
  curl -s -o /dev/null -X POST http://localhost:8080/api/transfers \
    -H "Content-Type: application/json" \
    -H "Idempotency-Key: manual-$(uuidgen)" \
    -d '{"src_account_id":"00000000-0000-0000-0000-000000000002","dst_account_id":"00000000-0000-0000-0000-000000000003","amount":"100.00"}' &
done
wait
```

With a starting balance of 10000 and amount 100, roughly 100 transfers should
succeed. If you set an amount larger than the balance, later requests will return
`insufficient_funds` — that is the expected behaviour.

### c) SQL invariant query

Paste into `psql` against a live database:

```sql
SELECT transaction_id,
       SUM(amount * CASE direction WHEN 'CREDIT' THEN 1 ELSE -1 END) AS net
FROM entries
GROUP BY transaction_id
HAVING SUM(amount * CASE direction WHEN 'CREDIT' THEN 1 ELSE -1 END) <> 0;
```

Expected: zero rows. Any row returned means a transaction has a debit/credit
imbalance — the deferred trigger should have caught this, so a non-empty result
indicates the trigger was bypassed or disabled.

---

## Trade-offs

**PostgreSQL over MySQL** — Postgres provides `NUMERIC` arbitrary precision, a
usable SERIALIZABLE isolation level (SSI), `INSERT ... ON CONFLICT ... RETURNING`
for idempotency, and `CONSTRAINT TRIGGER` (the only kind that can be DEFERRABLE).
MySQL InnoDB does not support deferrable constraint triggers, which would force the
double-entry invariant to be application-only.

**Double-entry over single-row movements** — A single-row `(from, to, amount)`
table cannot model multi-leg operations (fees, splits) without special cases. The
`transactions` header + `entries` legs model generalises cleanly. The tradeoff is
30-50% more code and a denormalised `balance` column on `accounts` to keep reads
cheap.

**Denormalised balance with deferred-trigger backstop** — Recomputing balance from
entries on every read is correct but slow. `accounts.balance` is kept in sync by
an explicit `UPDATE` inside every business transaction. The `non_negative_user_balance`
CHECK and `trig_check_double_entry` mean the DB will reject any commit that leaves
the balance inconsistent, even if a future code change forgets the `UPDATE`.

**DEFERRABLE INITIALLY DEFERRED CONSTRAINT TRIGGER for the invariant** — A regular
trigger fires per-row, which means the first inserted entry (debit only, no credit
yet) would fail the invariant check. DEFERRABLE defers evaluation to `COMMIT`,
allowing both legs to be inserted before validation. Only `CONSTRAINT TRIGGER` can
be DEFERRABLE in Postgres; `CREATE TRIGGER` cannot.

**DB-only locking (SELECT FOR UPDATE) over in-process sync.Mutex** — A per-account
`sync.Mutex` map works for a single process but breaks the moment a second server
instance is deployed. Postgres row-level locks are the correct scope for a
shared-nothing database-backed service, and lock ordering is enforced inside
`LockAccountsForUpdate` so callers cannot bypass it.

**Idempotency replay returns the existing transaction without body validation** —
When the same `Idempotency-Key` is seen again, the service returns the stored
transaction immediately. It does not re-validate the request body against the
stored one (Stripe-style conflict detection). This is a documented trust contract:
clients must not reuse a key for a different request.

**Audit log writes outside the business transaction; caller decides retry policy**
— `WriteAudit` is called after `WithTx` commits. If the audit write fails, the
transfer has already succeeded. The service logs the failure loudly but returns the
transaction to the caller. Putting the audit write inside the business transaction
would roll back the transfer on an audit failure, which is a worse trade-off.

**JSON wire format is PascalCase (Go default; no json:"..." tags)** — Domain types
in `internal/ledger/types.go` are tag-free. One struct definition serves both
internal Go code and the JSON wire format. Adding camelCase tags later is a
10-line change that does not touch logic.

**`Storer` interface in the `ledger` package due to Go import cycle constraint** —
`internal/store` imports `internal/ledger` for the `Account`/`Transaction` types.
A direct import of `internal/store` from `internal/ledger` would create a cycle.
The idiomatic Go fix is dependency inversion: `ledger` declares the `Storer`
interface describing what it needs; `*store.Store` satisfies it with thin wrappers.

**Service methods exceed the 30-line crisp-code budget** — `Transfer`, `Reverse`,
and `Deposit` each implement one linear flow: validate → idempotency replay →
`WithTx{lock → re-read → check → update → insert entries}` → audit → return.
Decomposing into private helpers fragments the algorithm and makes the live
walkthrough harder. The methods are commented with inline section markers.

---

## Assumptions

- INR only. No currency column; no FX support.
- Reversal of a transfer is rejected if the original destination account would go
  negative after the reversal (the `non_negative_user_balance` CHECK enforces this).
- Idempotency-key trust: the service does not validate that a replayed key carries
  the same request body. Clients are responsible.
- No authentication or authorisation.
- Single-instance deployment only.
- The `EXTERNAL` system account (UUID `00000000-0000-0000-0000-000000000001`) is
  the counterparty for all deposits. It is allowed to carry a negative balance,
  which is the canonical double-entry liability semantic.

---

## Known limitations / future work

- No authentication. Any caller can transfer funds between any accounts.
- No rate limiting.
- Transaction and audit pagination is limit-only (no cursor or offset).
- `EXTERNAL` carrying a negative balance is intentional and not an error.
- `WITHDRAWAL` is reserved in the schema `kind` CHECK but no handler exists yet.

---

## Project layout

```
cmd/server/       — main binary: migrations, DB pool, HTTP server, graceful shutdown
cmd/loadtest/     — load test binary; exercises concurrent transfers + invariant check
internal/httpapi/ — HTTP handlers, routing, error mapping, JSON helpers
internal/ledger/  — domain types, service (Transfer/Reverse/Deposit), Storer interface
internal/money/   — Money type backed by shopspring/decimal with 4dp rounding
internal/store/   — pgxpool-backed Store; implements ledger.Storer
migrations/       — plain SQL migration files (golang-migrate source)
web/              — static HTML/CSS/JS frontend served from /
vendor/           — vendored dependencies (go mod vendor)
tasks/            — design notes, trade-offs log, execution plan (not shipped)
```

---

## Test commands

Run the full test suite:

```
go test ./...
```

Run with the race detector (required — the service is concurrency-critical):

```
go test -race ./...
```

Integration tests use `testcontainers-go` and require Docker. They spin up a real
Postgres instance, run migrations, and exercise the full stack.

---

## UI overview

The frontend (`web/`) is a single-page HTML/JS app with no bundler or framework:

- **Accounts** — lists all accounts with live balances; create new accounts inline.
- **Operations** — Transfer, Deposit, and Reverse-by-ID forms.
- **Account History** — select an account from the dropdown, click Load to view
  that account's transaction list and full audit log side by side.
- **Ledger Activity** — auto-loads the first 50 global transactions on page open.
  A "Load more" button appends the next page. Each entry row displays
  `DIRECTION amount (AccountName)` — e.g. `DEBIT 100.0000 (Alice)`.
  After any successful transfer, deposit, or reversal, this panel resets to
  page 0 so the newest transaction appears at the top.

---

## Submission note

This repository is a take-home assignment for Shiprocket Quick. The implementation
covers all required milestones: schema + store (M1+M2), domain service + HTTP API
(M3+M4), frontend (M5), load test (M6), packaging + README (M7), and two-panel
history UI + Entry.AccountName JOIN (M9).
