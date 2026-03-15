---
name: post-flight-checks
description: Runs spec-driven verification at the end of a flight leg. Plans tests from the spec, runs quality gates, executes tests in a fix-and-retry loop, and confirms confidence before handoff.
---

# Post-Flight Checks: Verify Before Handoff

"Post-flight checks" is the verification phase that happens at the end of each flight leg, BEFORE writing a handoff. Like a pilot's post-flight inspection, this ensures everything actually works before signing off.

---

## When to Use This Skill

Use post-flight-checks when:

- You've completed all implementation tasks in a flight leg
- You're about to reach a `[HANDOFF]` task
- You need to verify that the flight leg's work matches the spec
- Called from `/take-off`, `/handoff-task`, `/resume-handoff`, or `/pre-flight-checks`

This skill does **NOT** write the handoff document. After post-flight checks pass, the calling skill proceeds to `/handoff-task`.

---

## Workflow Overview

```
Implementation tasks complete
        │
        ▼
/post-flight-checks        ← YOU ARE HERE
  ├── 1. Read the spec/plan
  ├── 2. Run quality gates (format, lint, test)
  ├── 3. Plan verification tests from the spec
  ├── 4. Execute tests in a fix-and-retry loop
  ├── 5. Confirm confidence
  └── 6. Return control to caller
        │
        ▼
/handoff-task              ← Caller proceeds here
```

---

## Step 1: Read the Spec

Read the plan document for the current flight leg to understand what was supposed to be built.

### 1.1 Find the Plan

The plan path should be available from:

- The handoff document (if resuming via `/resume-handoff`)
- The plan file passed to `/take-off`
- The `docs/plans/` directory

```bash
cat docs/plans/<plan-name>.md
```

### 1.2 Identify the Current Flight Leg

Find the section of the plan corresponding to the current flight leg. Note:

- **What tasks were supposed to be completed** — the implementation goals
- **What the Post-Flight Checks section says** — planned verification steps
- **What behavior should be observable** — expected outcomes

---

## Step 2: Run Quality Gates

Run the mechanical checks first. These must pass before any further verification.

```bash
# 1. Auto-fix formatting
make format

# 2. Run the test suite
make test
```

### Quality Gate Rules

- Run `make format` first — it auto-fixes formatting issues
- Run `make test` to verify tests pass
- **If format changes files**: Stage them with `git add`
- **If tests fail**: Fix the issues, re-run, repeat until passing
- **Do NOT proceed** to Step 3 until quality gates pass

### Fix-and-Retry Loop

```
┌─────────────────────────┐
│  Run make format        │
│  Run make test          │
└──────────┬──────────────┘
           │
     ┌─────▼─────┐
     │  All pass? │──── Yes ──→ Proceed to Step 3
     └─────┬─────┘
           │ No
           ▼
     Fix the failures
           │
           └──→ Re-run from top
```

---

## Step 3: Plan Verification Tests

Now plan **spec-driven tests** — verification that the implementation actually does what the plan says it should do. This goes beyond `make test`.

### 3.1 Review What Changed

```bash
git diff --name-only
```

### 3.2 Read the Plan's Post-Flight Checks

Check the plan document for the current flight leg's `### Post-Flight Checks` section. It should describe:

- What behavior to verify
- Expected outcomes
- How to test (curl, Playwright, make test, manual inspection, etc.)

### 3.3 Plan Concrete Test Steps

Based on the spec and what changed, plan specific verification steps. Examples:

| What Changed | Verification Approach |
| --- | --- |
| API endpoint | `curl` the endpoint, check response shape and status |
| UI component | Playwright: navigate, snapshot, check elements, click |
| CLI command | Run the command, check output |
| Data model | Run query or test that exercises the model |
| Configuration | Verify the config loads and applies correctly |
| Refactoring | Run existing tests, verify no regressions |

### 3.4 Decide What to Test

**Test what the spec says should work.** Prioritize:

1. **Core functionality** — Does the main feature work?
2. **Edge cases mentioned in the spec** — Does it handle the specified scenarios?
3. **Integration points** — Does it connect correctly to existing code?
4. **Regressions** — Did existing functionality break?

**Skip:**

- Exhaustive testing of unchanged code
- Tests that duplicate what `make test` already covers
- Manual-only checks that the agent can't perform

---

## Step 4: Execute Tests

Run each planned test. Fix issues and re-run until all pass.

### 4.1 Execute Each Test

For each planned verification step:

1. Run the test
2. Check the result against the expected outcome
3. If it passes, move to the next test
4. If it fails, fix the issue and re-run

### 4.2 Fix-and-Retry Loop

```
For each test:
  ┌──────────────────┐
  │  Run the test    │
  └────────┬─────────┘
           │
     ┌─────▼─────┐
     │  Passed?   │──── Yes ──→ Next test
     └─────┬─────┘
           │ No
           ▼
     Diagnose failure
     Fix the code
     Re-run quality gates (make format && make test)
           │
           └──→ Re-run this test
```

### 4.3 When to Stop Iterating

- **Pass**: Test produces the expected outcome
- **Known limitation**: The spec explicitly excludes this case — note it and move on
- **Infrastructure issue**: The test can't run (e.g., no server available) — note it and move on
- **After 3 failed attempts on the same test**: Note the issue, document what was tried, and move on. Do not loop indefinitely.

---

## Step 5: Confirm Confidence

Before returning control to the caller, explicitly state what was verified and your confidence level.

### Confidence Declaration

```
## Post-Flight Checks: PASSED

### Quality Gates
- make format: PASSED
- make test: PASSED

### Verification Tests
- [Test 1 description]: PASSED — [brief result]
- [Test 2 description]: PASSED — [brief result]
- [Test 3 description]: SKIPPED — [reason]

### Confidence
I am confident this flight leg matches the spec because:
- [Reason 1: e.g., "API endpoint returns correct response shape"]
- [Reason 2: e.g., "UI renders the expected elements"]
- [Reason 3: e.g., "All existing tests still pass"]

### Known Limitations
- [Any caveats, e.g., "Could not test WebSocket connection without running server"]
```

### If NOT Confident

If you cannot reach confidence:

1. Document what's failing and why
2. Note what you tried
3. Present the findings to the caller — the handoff should include these issues
4. Do NOT silently proceed

---

## Step 6: Return Control

Post-flight checks are complete. Return control to the calling skill:

- `/take-off` → proceeds to write the handoff via `/handoff-task`
- `/handoff-task` → proceeds to write the handoff document
- `/resume-handoff` → proceeds to write the handoff via `/handoff-task`
- `/pre-flight-checks` → proceeds to write the handoff via `/handoff-task`

---

## Checklist

- [ ] Plan document read for current flight leg
- [ ] `make format` passed (files staged if changed)
- [ ] `make test` passed
- [ ] Verification tests planned from the spec
- [ ] Each verification test executed
- [ ] Failures fixed and re-verified
- [ ] Confidence declaration made
- [ ] Known limitations documented

---

## Anti-Patterns

| Anti-Pattern | Problem | Fix |
| --- | --- | --- |
| Only running `make test` | Misses spec-level verification | Plan tests from the spec, not just the test suite |
| Single-pass testing | Leaves failures unfixed | Fix-and-retry loop until passing |
| Testing everything | Wastes time on unchanged code | Focus on what the flight leg built |
| Skipping the spec | Tests don't match requirements | Always read the plan first |
| Infinite retry loop | Gets stuck on one failure | Cap at 3 attempts, document and move on |
| Silent failures | Issues hidden from handoff | Always declare confidence and note limitations |
| Writing the handoff | Not this skill's job | Return control — the caller handles handoff |

---

## Related Skills

| Skill | Relationship |
| --- | --- |
| `/file-a-flight-plan` | Creates the plan with Post-Flight Checks sections |
| `/pre-flight-checks` | Creates bd tasks; invokes post-flight-checks before handoff |
| `/take-off` | Invokes post-flight-checks before handoff |
| `/handoff-task` | Invokes post-flight-checks; then writes the handoff |
| `/resume-handoff` | Invokes post-flight-checks before handoff |
| `/land-the-plane` | End-of-session checks (separate from flight-leg checks) |
