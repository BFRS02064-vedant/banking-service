# Active Plan — M9 (Two-panel history + Entry.AccountName)

> Status: **APPROVED** — dispatching `feature` agent.
> Execution mode: `full_feature`.
> Change Contract: [.claude/artifacts/artifact_design.json](../.claude/artifacts/artifact_design.json) — `implementation_ready: true`, `confidence_score: 0.93`, `risk: medium`.
> 7 files to modify, 0 to create. G-decisions locked: G1-a, G2-a.
> M1–M8 shipped (archives below).

---

## What this bundle ships

The submission-readiness layer. Everything needed for a reviewer to clone the repo, bring up the stack, exercise concurrency, and read the design without guessing.

### M6 — concurrency tooling
- A Go-based load test binary that hammers the HTTP API with concurrent transfers and asserts ledger invariants at the end. Reviewers run it as `go run ./cmd/loadtest` (or via README instructions).
- A documented manual recipe in the README for "how to test concurrency yourself" — exact curl commands or load-test invocations.

### M7 — packaging + observability + README
- `docker-compose.yml` to bring up Postgres (and optionally the app).
- `.env.example` with `DATABASE_URL` and any other env vars.
- `README.md` covering: setup, migrations, run, concurrency tests, design notes, assumptions, trade-offs (synthesised from `tasks/tradeoffs.md`).

---

## Decisions — **needs your call before spec-analyst**

### O1. Load test shape

| Option | Trade-off |
|---|---|
| **O1-a.** Standalone Go binary `cmd/loadtest/main.go`. Spawns N goroutines hitting `POST /api/transfers` against a running server. Configurable via flags: `-base-url`, `-n` workers, `-iters`, `-account-pair`. Asserts `Σ balances` invariant via `GET /api/accounts` after. | Matches stack; race-aware via `go run -race`; demoable in one command. ~120 LoC. |
| **O1-b.** Shell script + `xargs -P` + `curl`. | Tiny, no Go code. Less control over timing/assertions. Fragile across shells. |
| **O1-c.** `k6` or `wrk` script. | Industry standard; reviewer-familiar. But adds an out-of-repo tool dependency. |

*Lean:* **O1-a**. Stays in-repo, in-language, zero new tools, demos cleanly. The take-home is graded partly on "how to test concurrency" — a working tool that ships in the repo is stronger than "install k6 and run this script."

### O2. docker-compose scope

| Option | Trade-off |
|---|---|
| **O2-a.** Postgres only (`docker-compose up`); developer runs `go run ./cmd/server` separately. | Simplest. Reviewer needs Go installed. |
| **O2-b.** Postgres + app service (multi-stage Dockerfile builds the app, app waits for Postgres healthcheck). One command brings everything. | One-command demo (`docker-compose up`) — strongest first impression. Adds a Dockerfile (~20 lines). |
| **O2-c.** Postgres + app + a "loadtest" container that runs the load test on demand. | Full reproducible setup; over-engineered for the assignment. |

*Lean:* **O2-b**. Adds ~20-line Dockerfile, but the README payoff is "`docker-compose up` and visit localhost:8080" — that's the cleanest possible reviewer experience.

### O3. Makefile?

| Option | Trade-off |
|---|---|
| **O3-a.** Add `Makefile` with `migrate-up`, `test`, `test-race`, `load`, `up`, `down` targets. | Convenience; some reviewers expect it. ~25 lines. |
| **O3-b.** Skip `Makefile`; README documents each command directly. | One fewer file; commands are visible in README. |

*Lean:* **O3-b** — README is more honest documentation than `make up` hiding the actual command. Crisp-code preference. But if you'd rather have `make test-race` for live demo, that's defensible too.

### O4. README depth

The take-home prompt says: *"README must include how to run the app, how to run migrations/schema setup if any, how to test concurrency (scripts, load test, or documented manual steps), and enough detail that reviewers can follow your design and trade-offs without guessing."*

| Option | Trade-off |
|---|---|
| **O4-a.** Minimal: setup, run, test, brief design overview, trade-offs link to `tasks/tradeoffs.md`. | Fast; trade-offs file does the heavy lifting. |
| **O4-b.** Comprehensive: full design narrative inline, ALL trade-offs with reasoning, schema diagram (ASCII), API table, frontend walkthrough, known limitations, future work. | Strong submission artifact. Reviewer can read README alone. ~400-600 lines. |
| **O4-c.** Hybrid: README is a navigable index — short top-level summary + design diagrams (ASCII) + API table + setup/run inline; trade-offs synthesised from `tasks/tradeoffs.md` so reviewers don't need to chase another file. ~250 lines. | Best of both: skimmable top-down, complete details inline. |

*Lean:* **O4-c**. The reviewer in the live session opens README first; if it's a wall of text they skim, if it's a thin index pointing elsewhere they think it's incomplete. A navigable inline index respects their time and signals organisation.

---

## File list (if you go with all leans: O1-a, O2-b, O3-b, O4-c)

```
NEW
  cmd/loadtest/main.go             — load test binary
  docker-compose.yml               — Postgres + app services
  Dockerfile                       — multi-stage Go build
  .env.example                     — DATABASE_URL template
  README.md                        — overwrites the 77-byte stub

MODIFY
  cmd/server/main.go               — bind address from env (PORT) — only if not already, ~3 lines
  .gitignore                       — add .env, /server binary if missing
```

5 new files, up to 2 modified. Well within the 15-file rule.

---

## Out of scope

- Authentication / authorisation
- Rate limiting / circuit breakers
- Distributed tracing / OpenTelemetry
- CI/CD configuration

---

## Pipeline once O1–O4 are answered

1. `spec-analyst` produces M6+M7 Change Contract → user reviews.
2. APPROVED → `feature` implements within scope.
3. `tester` confirms `cmd/loadtest` runs cleanly + smoke-tests the documented README commands.
4. `qa` final submission-readiness check — README completeness, docker-compose brings up the stack, load test succeeds, all prior tests still pass, tradeoffs.md content reflected in README.

---

## Review — M9 COMPLETE (with 1 PARTIAL accepted)

**Pipeline ran clean:** spec-analyst (medium risk, conf 0.93) → APPROVED → feature → tester (all packages still pass, race-clean, count=2 stable) → qa (19/20 PASS, 1 PARTIAL).

**The PARTIAL** (H2 accepted):
- Acceptance criterion required an `account_name` assertion specifically on POST /api/transfers happy path. Feature put the assertion in `TestListAllTransactions_HappyPath` (GET path) instead. Functionally identical (both go through `listEntries → scanEntry`); only the test coverage missed pinning down one specific path. Accepted as `should_fix` per the QA tier — not material for submission.

**What shipped:**
- `Entry.AccountName` populated via SQL JOIN in `listEntries`. Single source of truth — every transaction-returning endpoint inherits the field.
- `scanEntry` reads the joined `a.name` column.
- UI: radio toggle removed entirely. Two distinct panels stacked: **Account History** (per-account, with audit) and **Ledger Activity** (global, paginated, no audit).
- Entries render as `DEBIT 100.0000 (Alice)` / `CREDIT 100.0000 (Bob)`. EXTERNAL appears as the literal name `EXTERNAL`.
- Mutations refresh both panels.

---

## Review — M8 COMPLETE (with fix loop)

**Pipeline ran:** spec-analyst (medium risk, conf 0.94) → APPROVED → feature → tester (8 new tests, 84 total pass, race-clean) → qa (20/21 PASS, 1 PARTIAL on `parseLimit` cap clamp) → **F1 fix loop** → feature (1-line fix + tightened test) → tester (TestListAllTransactions_LimitCap PASS) → qa (21/21 PASS, 0 PARTIAL).

**Total QA iterations:** 2.

**The bug** (now fixed): `parseLimit` was falling back to default (50) when input exceeded cap, instead of clamping to cap (200). Affected three list endpoints (the new `/api/transactions` plus `/api/accounts/{id}/transactions` and `/api/accounts/{id}/audit`). Test `TestListAllTransactions_LimitCap` originally asserted `len <= 200`, which passed at 50 — masked the bug. Fix: split the parseLimit return paths so `n > cap` clamps to `cap`. Test tightened to seed 210 deposits + assert `len == 200`.

**Lesson logged:** [tasks/lessons.md](lessons.md) — "Loose `<=` assertions hide clamp/cap bugs." The tester's job is to make bugs visible; for clamping/capping/defaulting logic, assertions must reflect the spec's specificity.

---

## Review — M6 + M7 COMPLETE — SUBMISSION READY

**Pipeline ran clean:** spec-analyst (low risk, conf 0.93) → APPROVED → feature → tester (102 tests still pass, race-clean) → qa (21/21 PASS, READY).

**Two nice-to-haves QA logged (non-blocking):**
1. Loadtest exits 1 on any non-2xx — including `insufficient_funds` (422). With default `-amount 1.00` against 10000 balances this never triggers, but a reviewer who cranks `-amount` would see exit 1 even with correct lock ordering. Live-session answer: "the loadtest is a strict invariant + zero-error test; if you want soft-failure semantics, add a `-allow-422` flag."
2. Dockerfile copies `cmd/` and `internal/` individually rather than `COPY . .` for layer-cache control. Documented choice.

Final test count: **102 passing, race-clean**. fmt/vet/build clean.

---

## Review — M3 + M4 COMPLETE (with 1 PARTIAL)

**Pipeline ran clean:** spec-analyst (high risk, conf 0.87) → APPROVED → feature → tester (102 tests pass, race-clean) → qa (21/22 PASS, 1 PARTIAL).

**The PARTIAL** (P1 accepted by user):
- Crisp-code line budget: `service.Transfer` (~87), `service.Reverse` (~101), `service.Deposit` (~77), `store.ListTransactionsForAccount` (~40), `store.ListAuditForAccount` (~44) all exceed the 30-line target. Inline section comments are present but no single dedicated "exceeds 30 lines because..." annotation. User accepted the PARTIAL — alternative was fragmenting one transfer into 5 helpers, which would make the live walkthrough harder.

Risk & confidence: `confidence_score: 0.87`, `race_clean: true`. Suggested label: **AI Generated – Safe Candidate** (with a footnote that walkthrough may surface the line-budget question).

**Two ACCEPTABLE deviations:**
1. `internal/store/accounts.go` modified to add `Storer` interface method wrappers (Go import-cycle constraint, 12 lines).
2. PascalCase JSON keys (intentional; frontend reads matching keys).

---

# Archive — M1 + M2 (COMPLETED)

Pipeline ran clean: spec-analyst (conf 0.91) → APPROVED → feature → tester (28 tests pass, race-clean) → qa (19/19 acceptance criteria PASS).

Risk & confidence: `confidence_score: 0.93`, `breaking_change_probability: 0.05`, `race_clean: true`. Suggested label: **AI Generated – Safe Candidate**.

Two ACCEPTABLE deviations:
1. `go.mod` directive `1.25.0` (toolchain auto-bump; no 1.25-only features).
2. `LockAccountsForUpdate` / `UpdateBalance` / `InsertTransaction` / `InsertEntries` are package-level `tx`-receiving functions, not `Store` methods.
