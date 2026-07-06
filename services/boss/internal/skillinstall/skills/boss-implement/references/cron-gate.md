# Cron gate — full detail

Read this when scheduling `boss-implement` as an unattended implementation cron (a setup-time concern,
not part of a run). Register this **gate command** on the job (scheduler UI, `GateCommand` — see
PR #870) so the run only fires when there is a candidate, spending **zero** agent tokens otherwise:

```
node .claude/skills/boss-implement/gate/gate.mjs
```

It exits `0` (run) iff at least one Linear issue is in the **`Todo`** state, carries the
**`agent-friendly`** label, **and is not blocked by an uncleared blocker** (a blocker whose
state is not `Done`/`Canceled` — i.e. its PR is unmerged), and non-zero (skip) otherwise. This
keeps the cron from waking to a fully-blocked backlog and burning a run that only exits
`NO_CHANGE`. It is still a deliberately **loose superset** of Step 2's exact selection: the
gate does not check for the `Implementation plan (...)` link, so it can occasionally let a run
fire that then exits `NO_CHANGE` — a rare false-positive in exchange for one cheap query, never
a false-negative. Step 2's filter remains the source of truth. The gate is **fail-closed**: a
missing `LINEAR_API_KEY` (injected into the gate environment by bossd), network failure, or API
error exits non-zero with a one-line reason on stderr, captured in the scheduler's `gate_output`
log. The blocking-aware query + the single blocker-clearing rule (cleared iff `Done`/`Canceled`)
live in `scripts/linear-deps-lib.mjs` (unit-tested), layered on the shared `linearRequest` in
`scripts/linear-gate-lib.mjs`; this entry is a thin I/O wrapper.
