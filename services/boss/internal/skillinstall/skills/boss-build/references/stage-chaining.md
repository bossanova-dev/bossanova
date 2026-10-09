# Stage chaining (Step 12)

A finished stage hands its work to the next stage instead of waiting for that stage's cron tick.
`toolbox/stage-chain.mjs` owns every argv; its tests are the specification. Every verb prints one
JSON line and exits 0 — chaining only accelerates a job that still fires on schedule — so a call
site prints the line and moves on. Exit 64 is a usage error.

## Consent and matching

A stage is chained only when the repo has a cron job for it: an **enabled** job whose prompt's first
`/<skill>` or `$<skill>` token is exactly that stage's skill (`/boss-build-ce` is not
`/boss-build`). One or more such jobs is consent (arming verify, the fast-path merge); `run-now`
needs exactly one, otherwise the verdict is `ambiguous` and nothing fires.

## Verbs

| Verb                    | Does                                                                           | `action`                                                              |
| ----------------------- | ------------------------------------------------------------------------------ | --------------------------------------------------------------------- |
| `next --stage <s>`      | reads `boss cron ls --repo $BOSS_REPO_ID --json`, names the next stage's job   | `run-now`, `none`, `ambiguous`, `skipped`, `error`                    |
| `run-next --stage <s>`  | `next`, then `boss cron run-now <job>` when exactly one job matches            | `started`, `skipped` (scheduler reason), `none`, `ambiguous`, `error` |
| `arm-verify --pr <n> …` | arms `checks_passed_ready` → `/boss-verify <n>` on the session's `verify` chat | `armed`, `already-armed`, `none`, `skipped`, `error`                  |
| `phase <name>`          | `boss session phase <name>`                                                    | `set`, `skipped`, `error`                                             |

Shared skips: `not-managed`, `no-boss-binary`, `no-repo-id`, `epic-child` (`arm-verify` only — the
epic orchestrator owns merge order) and `phase-unsupported` (an older CLI). `--dry-run` prints the
argv and runs nothing.

## Which call Step 12 makes

- Completion record `merged` ⇒ `run-next --stage verify`: the fast path _was_ the verify stage, run
  inline, so the hand-off goes to the stage after it.
- Otherwise on `REVIEW_READY` ⇒ `arm-verify`. It reuses the session's live chat titled `verify` (the
  same chat the verify router sends to) or creates one, and never sends `/boss-verify` now: the
  callback is the deferred send, so verify judges a settled head.

The callback is **state-matched** by default, because the verify receiver is idempotent per head —
a callback that fires on an already-green head costs one cheap judge. Only a `defect` the fast path
already posted on this head arms `--on-transition`, since only a new head can change that verdict.
A merged, `human` or `epic-child` record arms nothing. The callback expires after 7 days; the verify
cron gate covers the PR after that.

## Idempotency

A duplicate hand-off is harmless by construction: the verify head claim lets one run judge a head;
the build tracker claim lets one run take a ticket; the scheduler skips a `run-now` while the job's
previous run is active (`overlap_prev_active`) and still runs the job's gate; and `arm-verify` skips
when an active `checks_passed_ready` row for that chat and PR already exists.
