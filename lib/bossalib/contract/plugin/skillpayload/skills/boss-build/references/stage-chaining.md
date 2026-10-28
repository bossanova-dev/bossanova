# Stage chaining (Step 12)

Build can defer verification of the same PR through a callback. No verb in
`toolbox/stage-chain.mjs` starts another cron job; each stage runs on the operator's schedule.
The helper owns every argv; its tests are the specification. Every verb prints one JSON line and
exits 0, so a call site prints the line and moves on. Exit 64 is a usage error.

## Verify callback and matching

Arming verify requires an **enabled** verify cron job whose prompt's first `/<skill>` or `$<skill>`
token is exactly `boss-verify` (`boss-verify-custom` is a different skill). One or more matching jobs
allows the callback; the callback continues work on the same PR without firing a cron job.

## Verbs

| Verb                    | Does                                                                           | `action`                                             |
| ----------------------- | ------------------------------------------------------------------------------ | ---------------------------------------------------- |
| `arm-verify --pr <n> …` | arms `checks_passed_ready` → `/boss-verify <n>` on the session's `verify` chat | `armed`, `already-armed`, `none`, `skipped`, `error` |
| `phase <name>`          | `boss session phase <name>`                                                    | `set`, `skipped`, `error`                            |

Shared skips: `not-managed`, `no-boss-binary`, `no-repo-id`, `epic-child` (`arm-verify` only — the
epic orchestrator owns merge order) and `phase-unsupported` (an older CLI). `--dry-run` prints the
argv and runs nothing.

## Which call Step 12 makes

- By default every authorized `REVIEW_READY` build hands off: `fast-path-off` records arm a
  state-matched callback. Inline verification requires `buildDefaults.verifyFastPath: true`.
- On `REVIEW_READY` with no `merged` completion record ⇒ `arm-verify`. It reuses the session's live
  verify chat (role `verify`; the same chat the verify router sends to) or creates one if absent, and never
  sends `/boss-verify` now: the callback is the deferred send, so verify judges a settled head.
- `armed` / `already-armed` ends the build's own CI observation: Step 12 classifies with
  `--handed-off` and re-arms nothing.

The callback is **state-matched** by default, because the verify receiver is idempotent per head —
a callback that fires on an already-green head costs one cheap judge. Only a `defect` the fast path
already posted on this head arms `--on-transition`, since only a new head can change that verdict.
A merged, `human` or `epic-child` record arms nothing. The callback expires after 7 days; the verify
cron gate covers the PR after that.

## Idempotency

A duplicate hand-off is harmless by construction: the verify head claim lets one run judge a head,
and `arm-verify` skips when an active `checks_passed_ready` row for that chat and PR already exists.
