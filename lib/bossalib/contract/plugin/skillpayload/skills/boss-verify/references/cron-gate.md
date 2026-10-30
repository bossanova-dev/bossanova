# Cron gate — the verify job

Read this when scheduling the verify stage (a setup-time concern, not part of a run). One cron job
covers every open PR whose ticket waits in the review state, plus every open PR a cron run opened
that no ticket names. The repo's admission policy, `verifyDefaults.admission` in `.boss-skills.json`
(`docs/skills/skill-config.md`), can also admit human and bot PRs that name no ticket:

- **Schedule:** for example `*/15 * * * *`.
- **Prompt:** `/boss-verify`.
- **Gate command** (cwd = the repo root):

```
BOSS_VERIFY_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-verify/toolbox"
if [ ! -d "$BOSS_VERIFY_TOOLBOX" ]; then BOSS_VERIFY_TOOLBOX="$HOME/.codex/skills/boss-verify/toolbox"; fi
node "$BOSS_VERIFY_TOOLBOX/cron-gates/boss-verify.mjs"
```

Append `--batch <n>` (default 3 judged PRs plus up to 3 verified-head merge retries per run), `--budget-ms <ms>` (default 45000, inside
the scheduler's 60 s gate timeout) or any shared selection flag (`--label`, `--exclude-label`,
`--assignee`, `--creator`, `--project` and their `--exclude-` forms) to narrow the scan. Any other
argument makes the gate exit `1` with a reason.

When a repo's `.boss-project.json` lists several daemons, the plain gate reads the live sessions
across those daemons through the orchestrator. It skips PRs with a live session only on another
daemon (`#N skipped: session on another machine`) and still covers true orphans. An unreadable
cross-daemon view blocks only new sessions and prints why; local sessions and mechanical verdicts
continue. `--local-sessions-only` remains an option for a machine that should act only on its own
sessions, rather than a requirement for sharing the repo. It exits `1` when its local session list
is unreadable, and with no live PR session it reads nothing from GitHub and prints
`idle: no session on this machine has a PR`.

The GitHub token is the user's and shared with every other tool, so each run first reads `gh api
rate_limit` (which costs nothing). While the core or GraphQL quota has under a quarter left, the run
reads nothing more, prints `idle: GitHub <core|graphql> quota low (…)` and exits `1`.

Discovery reads every open PR's head, `boss/verify` status and CI in one GraphQL query per 100 PRs.
After a quiet tick (nothing to judge, only standing parks, live claims, red or unsettled CI,
verified heads and merge retries that waited without writing), the gate stores that tick's
fingerprint in `<git common dir>/boss-verify/sweep-state.json`. While the next tick's PRs, review
tickets and sessions still match it, the tick ends after that one query, prints `unchanged since
the last quiet tick` and exits `1`. Any other tick clears the entry; a hand-run `/boss-verify`
sweep always scans in full.

bossd sets `BOSS_VERIFY_EVENT_PR_URL` to run the same gate for exactly one GitHub PR URL;
malformed URLs are refused, and event summaries start with `boss-verify gate (event #N)`.
An event run exits `1` once the PR head is judged or deliberately skipped, and `69` when it still needs judging.
It sets `BOSS_WEBHOOKS_HEALTHY=1` while webhooks deliver: a recent quiet tick then skips all
subprocess reads until the safety-net window expires, printing
`idle: webhooks deliver; next safety-net sweep after <ISO time>` and exiting `1`.
`--safety-net-minutes N` sets that gate-only window (default `60`; `0` disables it).
An unchanged tick refreshes the quiet entry's last-check time; a non-quiet event clears its
matching cron entry so the next tick scans again. With neither variable set, cadence is unchanged.

## This gate dispatches

Unlike the other cron gates, this one does the work itself, because a cron fire always creates a new
session and verify work belongs in each PR's **existing** session. It judges each candidate with the
zero-token `judge`, posts and merges mechanically when no `verify` extension is installed, and
otherwise routes `/boss-verify <pr> --claim <token>` into the PR's session (its `verify` chat, a new
`verify` chat, or a detached `boss new --pr <n>` session for a PR with none). It never waits for a
dispatched verify. Merges are handed off to a detached process and settled within the budget; a
merge still in flight at the deadline prints `#N dispatched: merge in flight (log …)` and finishes
on its own. Each PR gets one line in the gate output, then a summary:

```
#41 merged <sha>
#42 parked: needs human (partial-build)
#43 dispatched: existing-verify-chat
#46 dispatched: merge in flight (log /tmp/boss-verify-merge-46-….log)
#44 skipped: wait ci-not-settled
#45 deferred: budget
boss-verify gate: merged=1 parked=1 dispatched=2 skipped=1 deferred=1
```

## Exit codes

| Exit | Meaning                                                                                                                                                                                                   |
| ---- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `1`  | Nothing to do, or every candidate handled. The run is recorded `gated` and **no session starts** — the normal outcome.                                                                                    |
| `1`  | Fail closed: a missing configured tracker credential (`LINEAR_API_KEY` for Linear; the host's `gh` login for GitHub), an unreadable config, a failed tracker read or `gh pr list`, or a refused argument. |
| `1`  | `--dry-run`, always: it writes and dispatches nothing and prints the routes it would take.                                                                                                                |
| `0`  | `undispatchable:` — a candidate needed the `boss` CLI and it was unreachable from the gate. Cron starts the `/boss-verify` sweep.                                                                         |

The gate fires a session only on that last row: the session's own environment carries the `boss`
binary and daemon socket, and its sweep routes exactly as the gate would have. A host whose `boss`
is off the service `PATH` therefore pays one fallback session per tick while candidates exist, and
the `undispatchable:` line in the gate output says so; putting `boss` on that `PATH` (or setting
`BOSS_BIN` in the daemon's environment) removes it.
