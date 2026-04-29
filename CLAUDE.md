# Claude Instructions — banking-service

> Autonomous contract for spec review, implementation, testing, QA, and PR review.
> Read this at session start. Update when corrections recur.

---

## EXECUTION MODE (MANDATORY AT SESSION START)

**Every task MUST begin with an explicit execution mode.** If the user does not
state one, STOP and ask. Do NOT analyze code or modify files until a mode is set.

| Mode | Purpose |
|---|---|
| `full_feature` | Spec → Design Review → Code → Tests → QA → PR Review |
| `bug_fix` | Root cause → Fix → Regression test → QA |
| `design_review_only` | Spec review only, no code |
| `tests_only` | Add / improve tests; no production-code changes |
| `qa` | Validate an existing PR / branch (read-only) |
| `pr_review` | Review existing PR, post inline GitHub comments |
| `fix_pr_comments` | Pull GitHub comments, fix code, resolve threads |

---

## AGENT USAGE (MANDATORY — NON-NEGOTIABLE)

**You MUST delegate to a custom agent in `.claude/agents/` for any work that
falls into one of the modes above. You may NOT do design review,
implementation, test authoring, QA, or code review directly in the main
session — always invoke the right agent via the `Agent` tool with
`subagent_type` matching the agent filename.**

Available agents:

| Agent | Role | Writes to repo? |
|---|---|---|
| `spec-analyst` | Design review, ambiguity detection, Change Contract | No (artifact only) |
| `feature` | Implements code, fixes PR comments, resolves GitHub threads | Yes |
| `tester` | Writes unit / race / invariant / HTTP tests | Yes (`*_test.go` only) |
| `qa` | Read-only validation; runs `go test -race`; reports issues | No (artifact only) |
| `reviewer` | Read-only review; posts inline GitHub PR comments | No (artifact only) |

The built-in `Explore` agent is allowed for read-only codebase exploration
alongside the custom agents.

### Mode → Required Agent Pipeline

| Execution Mode | Required Pipeline |
|---|---|
| `full_feature` | `spec-analyst` → (APPROVED) → `feature` → `tester` → `qa` → `reviewer` |
| `bug_fix` | `feature` (root cause + minimal fix) → `tester` (regression test) → `qa` |
| `design_review_only` | `spec-analyst` |
| `tests_only` | `tester` → `qa` |
| `qa` | `qa` |
| `pr_review` | `reviewer` |
| `fix_pr_comments` | `feature` → `qa` |

### Direct-Edit Prohibition

The orchestrating Claude session MUST NOT:
- Edit any `.go`, `.html`, `.css`, `.js`, `go.mod`, or `go.sum` file directly.
- Run `go test`, `go build`, `gofmt`, or `go vet` outside an agent — those
  belong to `feature`, `tester`, or `qa`.
- Post GitHub PR comments — that belongs to `reviewer`.
- Author tests directly — that belongs to `tester`.

The only files the orchestrator may write directly are:
- `.claude/artifacts/*.json` (when re-driving a phase from a stale artifact)
- `tasks/todo.md`, `tasks/lessons.md` (planning + retrospective)
- `CLAUDE.md` itself (only when the user asks for instruction changes)

If the user asks "just change X for me" and the change touches code, refuse
the shortcut: invoke `feature` (or `tester` if it's only a test). The cost
of one extra hop is far less than skipping the gates.

---

## CORE EXECUTION RULES

### 1. Write Protection

You are NOT allowed to dispatch the `feature` agent until:

1. A spec is provided (for `full_feature` mode).
2. `spec-analyst` has produced a Design Review Gate + Change Contract.
3. The user has explicitly replied **APPROVED** when risk is `medium` or `high`.

No implicit approval. `low` risk + zero clarifying questions may proceed without
APPROVED — but the Change Contract still has to exist first.

### 2. Plan First
- Enter plan mode for any non-trivial task (3+ steps or architectural decisions).
- Write the plan to `tasks/todo.md` with checkable items before dispatching `feature`.
- If something goes sideways, STOP and re-plan — don't keep pushing.

### 3. Scope Discipline (HARD GUARDRAIL)
- Only modify files declared in the Change Contract.
- No "while I'm here" improvements.
- No silent refactoring or formatting-only diffs.
- No dependency upgrades unless explicitly listed in the contract.
- Max files changed: **15**. Max diff size: **800 lines**.

### 4. Verification Before Done
Never mark a task complete without proof:
- `gofmt -s -w .` is clean
- `go vet ./...` passes
- `go test ./...` passes
- `go test -race ./...` passes (REQUIRED — banking is concurrency-critical)
- All acceptance criteria from the Change Contract are met

### 5. Self-Improvement Loop
After ANY user correction: append the pattern to `tasks/lessons.md` so the same
mistake doesn't recur. Re-read `tasks/lessons.md` at session start.

### 6. Demand Correctness Over Cleverness
- For non-trivial changes, ask: "is there a simpler, less clever way?"
- Banking code is read more than it's written. Prefer obvious over compact.
- If a fix feels hacky, replace it with the elegant version BEFORE asking for review.

---

## DESIGN REVIEW GATE

Applied when mode is `design_review_only` or `full_feature`. Produced by
`spec-analyst`. Output JSON shape:

```json
{
  "feature_summary": "",
  "critical_issues": [],
  "major_concerns": [],
  "minor_suggestions": [],
  "clarifying_questions": [],
  "concurrency_risks": [],
  "money_correctness_risks": [],
  "api_contract_gaps": [],
  "edge_cases_identified": [],
  "implementation_ready": false,
  "confidence_score": 0.0
}
```

Rules:
- If `clarifying_questions` is non-empty → `implementation_ready = false`.
- If `implementation_ready = false` → STOP and ask the user.
- Never assume unspecified behavior.

---

## CHANGE CONTRACT GATE

Produced by `spec-analyst` once `implementation_ready = true`. Output JSON:

```json
{
  "files_to_modify": [],
  "files_to_create": [],
  "files_to_delete": [],
  "public_api_changes": false,
  "storage_schema_changes": false,
  "concurrency_strategy": "per-account mutex | global lock | actor-per-account | optimistic CAS",
  "estimated_risk": "low|medium|high",
  "test_strategy": "",
  "rollback_strategy": "",
  "acceptance_criteria": [],
  "implementation_plan": []
}
```

Risk Escalation:
- More than 5 directories touched → risk ≥ `medium`
- `public_api_changes = true` → risk cannot be `low`
- `storage_schema_changes = true` → risk ≥ `medium`

If `estimated_risk ∈ {medium, high}` → STOP and wait for **APPROVED** before
dispatching `feature`.

---

## VERIFICATION LOOP (DETERMINISTIC)

Run by `qa` after `feature` finishes. Must pass:
- `gofmt -s -l .` (no diff)
- `go vet ./...`
- `go test ./...`
- `go test -race ./...` (race report ⇒ critical fail)
- All Change Contract `acceptance_criteria` met

Retry constraints:
- Max retries per stage: **5**
- Each retry must analyze actual logs — no blind regeneration
- After 5 attempts: emit a failure report and STOP

---

## RETRY LOOPS

```
feature ──► CREATE PR ──► qa ──FAIL──► feature (push fix) ──► qa
                                              (max 3 loops)

reviewer ──MUST-FIX──► feature (push fix) ──► qa ──► reviewer
                                              (max 2 loops)
```

---

## ARTIFACT PASSING BETWEEN AGENTS

Artifacts live in `.claude/artifacts/` as JSON. Each agent writes its own and
reads upstream ones:

| File | Producer | Consumers |
|---|---|---|
| `artifact_design.json` | `spec-analyst` | `feature`, `tester`, `qa`, `reviewer` |
| `artifact_code.json` | `feature` | `tester`, `qa`, `reviewer` |
| `artifact_tests.json` | `tester` | `qa`, `reviewer` |
| `artifact_qa.json` | `qa` | `reviewer` |
| `artifact_review.json` | `reviewer` | (final) |

Lifecycle:
- Pipeline start: stale artifacts may be cleaned (only when starting a fresh feature).
- During retries: the relevant artifact is overwritten.
- A downstream agent reads only the artifacts it needs to keep its context tight.

---

## RISK & CONFIDENCE REPORT

Before declaring a `full_feature` or `bug_fix` run complete, the orchestrator
emits:

```json
{
  "confidence_score": 0.0,
  "breaking_change_probability": 0.0,
  "race_clean": true,
  "money_correctness_verified": true,
  "atomicity_verified": true,
  "edge_cases_considered": [],
  "total_qa_iterations": 0,
  "total_review_iterations": 0
}
```

Auto PR labelling guidance:
- `confidence_score < 0.75` OR `breaking_change_probability > 0.2` OR
  `race_clean = false` → **AI Assisted – Mandatory Review**
- Else → **AI Generated – Safe Candidate**

---

## TASK MANAGEMENT

1. **Plan first** in `tasks/todo.md` with checkable items.
2. **Confirm the plan** with the user before dispatching `feature`.
3. **Track progress** — mark items complete as agents return.
4. **Document results** — append a brief review section to `tasks/todo.md`.
5. **Capture lessons** in `tasks/lessons.md` after any correction.

---

## PROJECT CONTEXT

- **Stack**: Go 1.22+ · standard library `net/http` (method-prefixed routes) ·
  plain HTML/JS frontend served from `web/`.
- **Domain**: banking / ledger with concurrency. Core operations: account
  creation, balance read, transfer (debit + credit), transaction history.
- **Layout**: `cmd/server/` for the binary; `internal/` for domain packages
  (suggested: `internal/account`, `internal/ledger`, `internal/http`); `web/`
  for the HTML/JS frontend; `*_test.go` colocated with code.
- **Frontend**: minimal — plain HTML, vanilla JS, plain CSS. No bundler, no
  framework. Served as static files by the Go server.

---

## PROJECT-SPECIFIC RULES

- **Money never in float64.** Use a decimal type (`shopspring/decimal` or
  equivalent) end-to-end for balances, amounts, and rates.
- **Transfers are atomic.** A two-account transfer must be observable as
  all-or-nothing — no intermediate state where one side is debited but the
  other isn't credited.
- **Lock ordering is deterministic.** When taking two account locks, lock by
  a stable order (e.g. `min(accountID), max(accountID)`) to prevent deadlock.
- **Self-transfer is a defined case.** Either reject it explicitly or handle
  it without taking the same lock twice — never let it deadlock.
- **`go test -race` must be clean.** It runs in CI and as part of `qa`. A race
  report is treated as a critical bug, not a warning.
- **Context propagation.** Every I/O call and long-running operation accepts
  and respects `context.Context`.
- **HTTP layer is thin.** Validate input → call domain → translate errors →
  write JSON. No business logic in handlers.
- **Stable error contract.** Sentinel errors (`var ErrInsufficientFunds = ...`)
  for predictable cases; map them to specific HTTP status codes
  (404 / 409 / 422) — not 500.
- **Idempotency.** If the spec mentions write retries, honour an
  `Idempotency-Key` header on transfer endpoints.
- **No global state.** Pass dependencies through constructors.
- **Frontend is dumb.** Keep `web/app.js` thin — fetch, render, that's it. No
  business rules in the browser.

---

## CODE STYLE & CONVENTIONS

- **Errors**: wrap with `fmt.Errorf("...: %w", err)`. Define sentinel errors
  for predictable cases. Never swallow errors silently.
- **Logging**: use `log/slog` with structured fields. No secrets or full PII
  in logs. Include request / trace IDs where available.
- **Validation**: validate at the HTTP boundary (positive amounts, known
  accounts, currency rules). Domain constructors enforce invariants.
- **Decimal arithmetic**: `shopspring/decimal` (or equivalent) for all money.
  Compare via `.Equal()`, not `==`. Set rounding mode explicitly when it matters.
- **Concurrency primitives**: prefer `sync.Mutex` / `sync.RWMutex` for shared
  state, channels for ownership transfer. Avoid `sync.Map` unless profiling
  shows it's needed.
- **Goroutines**: every spawned goroutine has a clear exit path tied to a
  `context.Context`. No fire-and-forget without tracking.
- **Tests**: table-driven with `t.Run`; helpers call `t.Helper()`; use
  `httptest` for HTTP; never `time.Sleep` for synchronization.
- **Comments**: only when the *why* isn't obvious. Don't restate the code.

---

## QUICK CHECKLIST FOR THE ORCHESTRATOR

Before dispatching anything:
1. Did the user state an execution mode? If no → ask.
2. Does this require code or test changes? If yes → an agent must do it.
3. For `full_feature`: has `spec-analyst` produced design + contract?
4. Is risk `medium` or `high`? If yes → wait for **APPROVED**.
5. After dispatch: did `qa` report `race_clean = true`?
6. After review: were all `must-fix` items addressed?
