# Lessons

> Re-read at session start. Append a new entry whenever the user corrects a
> mistake — write a rule for yourself that prevents the same mistake next time.
>
> Format:
>
> ## YYYY-MM-DD — short title
> **Mistake:** what went wrong
> **Rule:** what to do (or not do) from now on
> **Why:** the reasoning, so the rule isn't followed blindly

## 2026-04-29 — Loose `<=` assertions hide clamp/cap bugs

**Mistake:** The M8 test `TestListAllTransactions_LimitCap` asserted `len(txns) <= 200` for `?limit=999`. The actual return was 50 (the default fallback) because `parseLimit` falls back to default when value exceeds cap, instead of clamping. The assertion passed at 50 ≤ 200, masking the spec violation. QA caught it by reading the helper, not by running the test.

**Rule:** When the spec specifies an exact clamp/cap value (e.g. "clamps to 200"), the test MUST assert the exact expected length, not a `<=` bound. Use `len(txns) == 200` (with enough seed rows to fill the cap) for clamp-up tests. Reserve `<=` for pure "no more than" guarantees.

**Why:** The `tester` agent's job is to make bugs visible. A test that passes both with the bug and without it is a false-positive trap. For clamping/capping/defaulting logic, the assertion shape should reflect the spec's specificity.
