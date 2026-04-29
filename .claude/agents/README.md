# banking-service — Agent Pipeline

Five Claude Code agents handle spec review, implementation, testing, QA, and
PR review for this Go banking / ledger service. The full contract lives in
[`CLAUDE.md`](../../CLAUDE.md) at the repo root.

## Quick Start

```bash
# Install Claude Code CLI (if not already installed)
npm install -g @anthropic-ai/claude-code

# (Optional) authenticate gh for PR-related modes
gh auth login

# Run from project root
claude
```

Then tell Claude which mode to use — every session must start with one.

## Execution Modes

| Mode | Purpose | Pipeline |
|---|---|---|
| `full_feature` | Build a feature end-to-end | `spec-analyst` → (APPROVED) → `feature` → `tester` → `qa` → `reviewer` |
| `bug_fix` | Minimal fix for a known bug | `feature` → `tester` (regression) → `qa` |
| `design_review_only` | Spec review, no code | `spec-analyst` |
| `tests_only` | Add or improve tests | `tester` → `qa` |
| `qa` | Validate an existing PR / branch | `qa` |
| `pr_review` | Review existing PR + post inline comments | `reviewer` |
| `fix_pr_comments` | Pull review comments, fix code, resolve threads | `feature` → `qa` |

### Examples

```
Execution Mode: full_feature
Spec: Add a POST /transfer endpoint that moves funds between two accounts.
Must be safe under concurrent requests. Use Idempotency-Key for retries.
```

```
Execution Mode: bug_fix
Bug: Concurrent transfers on the same account pair occasionally produce
a balance off-by-one. `go test -race` reports a data race in ledger.go.
```

```
Execution Mode: pr_review
PR: #14
```

## Agents

| Agent | Role | File access |
|---|---|---|
| [`spec-analyst`](spec-analyst.md) | Reviews spec, generates Change Contract | read-only |
| [`feature`](feature.md) | Implements code, fixes PR comments | read + write |
| [`tester`](tester.md) | Writes unit / race / invariant / HTTP tests | write `*_test.go` only |
| [`qa`](qa.md) | Runs `go test -race`, validates against contract | read-only |
| [`reviewer`](reviewer.md) | Reviews PR, posts inline GitHub comments | read-only files + `gh` |

The built-in `Explore` agent is allowed for read-only codebase exploration.

## How It Works

### Approval Gates

- **Spec ambiguity** — If `spec-analyst` returns clarifying questions, the
  pipeline stops and asks you. Nothing else runs until they're answered.
- **Risk gate** — If the Change Contract estimates `medium` or `high` risk,
  the pipeline stops and waits for you to reply `APPROVED` before any code
  is written.

### Retry Loops

- `qa` fails → `feature` pushes a fix → `qa` re-runs (max 3 attempts)
- `reviewer` flags must-fix → `feature` pushes a fix → `qa` → `reviewer` re-runs (max 2 attempts)

### Artifacts

Each agent persists its output to `.claude/artifacts/` so a downstream agent
can resume from a stale state without re-running upstream work:

| File | Producer | Consumers |
|---|---|---|
| `artifact_design.json` | `spec-analyst` | `feature`, `tester`, `qa`, `reviewer` |
| `artifact_code.json` | `feature` | `tester`, `qa`, `reviewer` |
| `artifact_tests.json` | `tester` | `qa`, `reviewer` |
| `artifact_qa.json` | `qa` | `reviewer` |
| `artifact_review.json` | `reviewer` | (final) |

## Safety Rules

- **No direct edits by the orchestrator** — code, tests, and PR comments must
  go through the appropriate agent. See `CLAUDE.md` § "Direct-Edit Prohibition".
- **Max 15 files / 800 diff lines** per Change Contract.
- **`go test -race` must be clean** — a race report is a critical failure.
- **No `float64` for money** — decimal types only.
- **Deterministic lock ordering** on multi-account operations.
- **No dependency upgrades** unless explicitly listed in the Change Contract.

## Troubleshooting

**"Ask user to specify execution mode"**
You forgot to include `Execution Mode:` at the start. Always specify it.

**Pipeline stops after Change Contract**
Risk is `medium` or `high`. Review the contract and reply `APPROVED` to continue.

**Pipeline stops with clarifying questions**
The spec is ambiguous. Answer the questions, then it will proceed.

**Reviewer didn't post GitHub comments**
Make sure `gh auth login` is done and you have write access to the repo.

**Agent keeps prompting for permission on the same command**
Add the command pattern to `.claude/settings.json` under `permissions.allow`.
The default allowlist is intentionally tight — see that file for what's allowed.
