---
title: Scheduled Sessions
description: 'Use cron jobs to spawn agent sessions on a schedule: every Monday morning, every night, every weekday at 09:00.'
---

# Scheduled Sessions

A **cron job** in Bossanova is a pre-configured session-creation
trigger that fires on a schedule. When the cron tick arrives, the
daemon spawns a session as if you had pressed `n` from the home view:
same prompt, same worktree machinery, same plugin pipeline, except
no human is sitting there to type the prompt. Useful for the kind of
work that wants to happen on a predictable cadence: weekly
dependency scans, nightly stale-issue triage, periodic docs sweeps.

## Open the cron list

From the home view, press `s` to open Settings, then `c` (or press `?` from any
screen for the keymap). The list shows every job the daemon knows about,
one row per job, sorted by next fire time. The columns are:

| Column     | Meaning                                                           |
| ---------- | ----------------------------------------------------------------- |
| `CRON`     | The schedule expression (e.g. `0 9 * * 1-5`).                     |
| `NAME`     | Human-readable name you set on creation.                          |
| `REPO`     | Which repo the spawned session targets.                           |
| `ENABLED`  | `yes` / `no`; disabled rows are dimmed and don't fire.            |
| `LAST RUN` | Relative time of the last fire (`5m ago`, `2h ago`, `never`).     |
| `NEXT RUN` | Relative time until the next scheduled fire.                      |
| `STATUS`   | `Running` (with spinner), `gating`, `gated`, `failed`, or `idle`. |

The `gating` and `gated` statuses come from the optional **gate command**
(see [Gate command](#gate-command) below): `gating` shows while the gate is
running, and `gated` is remembered when the last tick was blocked by a
non-zero gate exit.

The action bar shows the available keys: `[n]ew`, `[e/enter]dit`,
`[d]elete`, `[space] toggle`, `[r]un now`. `esc` returns to Settings.
Both `e` and `Enter` open the highlighted job in the edit form.

## Add a cron job

Press `n` from the cron list. The form's fields all come from
`services/boss/internal/views/cron_form.go`:

| Field               | Required? | Notes                                                                                                                                       |
| ------------------- | --------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `Name`              | yes       | Letters, digits, spaces, hyphens, underscores. Up to 80 chars.                                                                              |
| `Repo`              | yes       | Pick from a select of all configured repos.                                                                                                 |
| `Prompt`            | yes       | Single-turn prompt the agent runs. Must be self-contained; see the warning below.                                                           |
| `Schedule`          | yes       | 5-field cron expression or one of `@daily` / `@hourly` / `@weekly` / `@monthly`.                                                            |
| `Timezone`          | no        | IANA name (e.g. `America/New_York`). Empty = the daemon's local zone.                                                                       |
| `Gate command`      | no        | Optional command run before each fire; a non-zero exit skips the run. See [Gate command](#gate-command).                                    |
| `Run setup command` | yes       | Defaults to **on**. Turn off to skip the repo [setup script](setup-scripts.md) for light jobs. See [Run setup command](#run-setup-command). |
| `Zero output`       | yes       | Defaults to **off**. Turn on for jobs that change nothing in the repo — no worktree, branch, or PR. See [Zero output](#zero-output).        |
| `Enabled`           | yes       | Defaults to on. Disabled rows persist but don't fire.                                                                                       |

The form renders a live **next-fire preview** under the schedule
field. As you type a valid expression, it shows the literal next
wall-clock time the job would fire in the chosen timezone. Invalid
expressions show a red error inline.

:::warning Single-turn prompts

Cron sessions only listen for the main agent's `Stop` hook. Subagents
are ignored, and there is no follow-up loop. Whatever the prompt
does in one shot is the run. Keep the prompt self-contained: don't
write it as "ask me first" or "if X, ping me." If you need
interaction, start a regular session instead.

:::

### Schedule format

The schedule field accepts either of:

- **Standard 5-field cron.** Minute, hour, day-of-month, month,
  day-of-week. Examples:
  - `0 9 * * 1-5`: every weekday at 09:00.
  - `*/15 * * * *`: every 15 minutes.
  - `0 3 1 * *`: 03:00 on the first of every month.
- **Predefined macros.** `@hourly`, `@daily`, `@weekly`, `@monthly`,
  `@yearly`. (Source: `bossalib/cronutil` parser used by both the form
  validator and the scheduler.)

Cron's smallest granularity is one minute, so `*/30 * * * * *` and
similar second-level expressions are rejected.

## Gate command

Some cron jobs run often and carry heavy, token-expensive prompts, but
only actually need to run when a cheap mechanical condition holds — for
example, "there is an open PR with label `needs-triage`." The **gate
command** is that cheap check. It runs _before_ the job fires; a zero
exit lets the run proceed, and a non-zero exit blocks it. Because the
gate runs before any worktree is created, a blocked tick costs nothing
beyond the gate command itself — no worktree, no setup script, no agent
session, no tokens.

Leave the field empty for no gate (the default — every tick fires).

### Command vs. path

The command is interpreted one of two ways:

- **Path to an executable** when it starts with `/`, `./`, or `../`. The
  command is split on whitespace and run **directly, without a shell**.
  Relative paths resolve against the repo clone (the working directory).
- **Shell command** otherwise: run as `sh -c "<command>"`, so pipes,
  `&&`, environment expansion, and other shell features work.

### Outcome

| Gate result                           | What happens                                                                                            |
| ------------------------------------- | ------------------------------------------------------------------------------------------------------- |
| Exit `0`                              | The job fires normally (worktree, setup, agent session).                                                |
| Non-zero exit                         | The job is **gated**: no session, `next_run_at` advances.                                               |
| Timeout (default **60s**)             | Treated as a failure → **gated**. The scheduler slot is released so a hung gate can't wedge the runner. |
| Launch error (binary not found, etc.) | Treated as a failure → **gated**.                                                                       |

The `STATUS` column shows `gating` (with a spinner) while the gate runs
and `gated` once a tick is blocked. `gated` is **remembered** — it is
written to `last_run_outcome`, so it survives a daemon restart and stays
visible in the list until a later successful fire supersedes it.

The gate runs inside the shared cron concurrency slot (see [Overlap and
concurrency](#overlap-and-concurrency)), so keep gate commands fast: a gate
that runs near the 60s timeout holds one of the (default 3) slots for that
whole time. This is why the gate should be a cheap mechanical check, not
heavy work.

### Environment

The gate runs on the **daemon host** (not in a worktree — none exists
yet), with its working directory set to the repo's main clone. The
following environment variables are provided so a gate script can talk to
the repo's services without hard-coding anything:

| Variable            | Value                                | Why it's provided                                                                                                                         |
| ------------------- | ------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------- |
| `REPO_DIR`          | The repo's main clone path           | Lets the gate inspect the checkout (run `git`, read files, etc.).                                                                         |
| `WORKTREE_DIR`      | Same as `REPO_DIR`                   | The standard variable [setup scripts](setup-scripts.md) receive. No per-run worktree exists at gate time, so it points at the repo clone. |
| `BOSS_WORKTREE_DIR` | Same as `REPO_DIR`                   | Alias of `WORKTREE_DIR` for gate scripts that expect the `BOSS_`-prefixed name.                                                           |
| `LINEAR_API_KEY`    | The repo's Linear API key (or blank) | So a gate can query Linear (e.g. "is there an issue labelled X?") without storing the key itself. Blank when the repo has none.           |
| `SENTRY_API_KEY`    | The repo's Sentry API key (or blank) | So a gate can query Sentry. Blank when unset.                                                                                             |
| `SENTRY_ORG`        | The repo's Sentry org (or blank)     | Companion to `SENTRY_API_KEY` for Sentry API calls. Blank when unset.                                                                     |

These keys come from the repo's settings (the same trust boundary as the
setup script). The gate's combined stdout+stderr is captured and logged
by the daemon — **don't print secret values** from your gate script.

:::note Worktree variables point at the repo clone

The gate deliberately runs before a per-run worktree exists, so
`WORKTREE_DIR` / `BOSS_WORKTREE_DIR` both point at the repo's main clone
rather than an isolated worktree. Write gate scripts that only read from
the checkout; don't expect a clean per-run worktree.

:::

### Example

A gate that only lets a triage job run when there's at least one open PR
labelled `needs-triage` (exits non-zero — gating the job — when there is
nothing to do):

```sh
test -n "$(gh pr list --label needs-triage --state open --json number -q '.[].number')"
```

## Run setup command

Before the agent starts, a cron fire normally runs the repo's
[setup script](setup-scripts.md) in the fresh worktree to install
dependencies, generate code, etc. Some jobs failed in the past because
setup hadn't run; others are light enough that paying the full setup cost
on every tick is wasteful.

**Run setup command** is the per-job toggle for this. It defaults to
**on** (so the safe default is preserved — setup always runs unless you
opt out). Turn it **off** to keep a light job fast: the fire skips the
setup script entirely and starts the agent in the bare worktree.

Internally this maps to the session's `SkipSetupScript` option
(`run_setup_command` off ⇒ `SkipSetupScript` true).

## Zero output

Some jobs only ever report somewhere else — they post to Slack, probe a
health endpoint, or file a ticket — and are never expected to change a
line of this repository. Provisioning a worktree, a branch and a draft PR
for those is pure overhead.

**Zero output** is the per-job toggle for that shape. It defaults to
**off** — the only toggle on this form that does, because running with no
worktree is never a safe default. Turn it **on** and the fire runs with
no worktree, no branch and no PR: the agent runs in the repository
checkout itself, the setup script is skipped regardless of
[Run setup command](#run-setup-command), and the run finalizes as a
benign no-op recorded with the `zero_output` outcome.

Because the agent runs in your real checkout rather than an isolated
worktree, a zero-output job that does write files leaves them there —
nothing cleans them up and no PR is opened. Keep the prompt to jobs that
genuinely produce no repository changes.

It is settable from every surface: the TUI and web cron forms, `boss cron
add --zero-output` / `boss cron update <id> --zero-output=false`, and the
`create_cron_job` / `update_cron_job` MCP tools' `zero_output` argument.

## What happens at fire time

When the schedule's next tick arrives, the daemon's scheduler
([`services/bossd/internal/cron/scheduler.go`](https://github.com/bossanova-dev/bossanova/blob/main/services/bossd/internal/cron/scheduler.go))
runs `fire()`:

1. **Re-fetch the job.** A job that was disabled or deleted between
   the tick scheduling and the actual fire is skipped (skip reasons
   `disabled` and `db_fetch_error` are logged but not surfaced).
2. **Overlap check.** If the job's last spawned session is still
   active and not archived, the fire is skipped with
   `overlap_prev_active`. See the next section.
3. **Concurrency cap.** A counting semaphore limits simultaneous
   fires across **all** cron jobs to 3 by default
   (`DefaultMaxConcurrent` in `scheduler.go`). Extra fires block
   until a slot frees up.
4. **Gate command.** If a [gate command](#gate-command) is set, it runs
   now — _before_ any worktree exists. A zero exit lets the fire
   proceed; a non-zero exit, timeout, or launch error blocks it
   (`STATUS` becomes `gated`, `next_run_at` still advances, no session
   is created). The gate runs on **every** fire — scheduled and manual
   [Run now](#run-a-job-ad-hoc) alike — so a manual run is also blocked
   when the gate fails.
5. **Spawn.** A worktree is created on a fresh branch; your repo's
   [setup script](setup-scripts.md) runs **unless [Run setup
   command](#run-setup-command) is off**; and the agent plugin starts
   the agent inside it with the cron job's prompt as the first turn.
   When [Zero output](#zero-output) is on, none of that provisioning
   happens: the agent starts in the repository checkout with no
   worktree, branch or PR.
6. **Persist.** `last_run_session_id`, `last_run_at`, and
   `next_run_at` are written to the cron job row, so the list view's
   `LAST RUN` and `NEXT RUN` columns update on the next poll.

### Branch naming

Every fire produces a unique branch named:

```
cron-<name-slug>-<unix-timestamp>
```

The slug is the job's name lower-cased with non-`[a-z0-9]` characters
replaced by hyphens, truncated to 40 chars. The unix timestamp suffix
guarantees consecutive fires (which are at least one minute apart by
cron's minimum granularity) don't collide on a previously-merged or
SIGTERM'd branch. (Source: `cronBranchName` in
[`scheduler.go:510`](https://github.com/bossanova-dev/bossanova/blob/main/services/bossd/internal/cron/scheduler.go#L510).)

## Overlap and concurrency

Cron fires interact with parallel sessions in two places:

- **Per-job overlap.** If the job's previous fire is still running
  (the spawned session is in a non-terminal, non-archived state),
  the next fire is skipped with `overlap_prev_active`. This is what
  prevents a slow weekly job from stacking up on itself across runs.
  Once the previous session reaches `Merged`, `Closed`, or is
  archived, the next fire goes through.
- **Cross-job concurrency.** All cron fires share a global semaphore
  capped at 3. If you have a dozen jobs that all happen to fire at
  09:00, three start immediately and the rest queue until a slot
  frees up. Tune by setting `MaxConcurrent` on the scheduler (today
  this is wired internally, not exposed in `settings.json`).

For broader patterns on running many sessions at once, see
[Worktrees → Multiple sessions](../concepts/worktrees.md#multiple-sessions).

## Inspecting cron history

The cron list is the dashboard. It refreshes every 2 seconds while
open, so `LAST RUN` and `STATUS` stay live without manual refresh.
Specifically:

- **`LAST RUN`** shows when the most recent fire actually spawned a
  session. A skipped fire (overlap, disabled-between-tick-and-fire)
  does not update this column.
- **`NEXT RUN`** is computed from the parsed schedule and the
  job's timezone, so it reflects the runner's current decision:
  not a snapshot from when you last edited the row.
- **`STATUS`** reflects the spawned session's state: `Running` while
  the agent is active, `gating` while a [gate command](#gate-command) is
  running, `gated` if the last tick was blocked by the gate, `failed` if
  the last fire's session ended in failure, `idle` otherwise. `gated` is
  remembered across restarts; a later successful fire supersedes it.

There is no separate "history view". Every fire produces a normal
session, so to drill into a specific run, find its session in the
home view (it will have a `cron-…` branch name) or via `boss ls`.

## Run a job ad-hoc

Press `r` on the highlighted row in the cron list. This calls
`RunCronJobNow` and spawns a session immediately, regardless of
schedule. The same overlap, concurrency, **and gate** rules apply. If
the previous fire is still running, you'll see a `Skipped: previous run
still active` toast at the bottom of the list.

**Run now honors the [gate command](#gate-command).** A manual run is
gated exactly like a scheduled fire: if the gate exits non-zero the run
is blocked and you'll see a `Skipped: blocked by gate command` toast —
so pressing `r` is the way to test a gated job on demand.

Use this when you want to manually re-trigger a cron job without
waiting for the next scheduled fire (handy when you've just edited
the prompt and want to see how the new version behaves).

## Failure handling

If `CreateSession` itself returns an error (out of disk, repo gone,
worktree dir not writable), the job's `last_run_outcome` is set to
`fire_failed` and `next_run_at` is cleared. The cron runner still
ticks on its own schedule for the next fire. A single failed spawn
does not disable the job.

If the spawned session itself fails (the agent crashes, the prompt
errors out), the cron job row's `STATUS` cell shows `failed` until
the next successful fire. Repair plugin behaviour applies to cron
sessions exactly as it does to manual sessions. See
[PR Lifecycle](./pr-lifecycle.md).

## See also

- [Worktrees: Multiple sessions](../concepts/worktrees.md#multiple-sessions): the
  cross-job concurrency cap and how cron fits into the bigger
  parallelism story.
- [Setup scripts](setup-scripts.md): what runs in the worktree
  before the cron prompt does.
- your repo's settings (open Settings with `s` from home, then `r`
  for the Repos screen, then `enter` on the repo): per-repo
  automation flags that apply
  to cron-spawned PRs same as manual ones.
