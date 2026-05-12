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

From the home view, press `c` (or press `?` from any screen for the
keymap). The list shows every job the daemon knows about,
one row per job, sorted by next fire time. The columns are:

| Column     | Meaning                                                       |
| ---------- | ------------------------------------------------------------- |
| `CRON`     | The schedule expression (e.g. `0 9 * * 1-5`).                 |
| `NAME`     | Human-readable name you set on creation.                      |
| `REPO`     | Which repo the spawned session targets.                       |
| `ENABLED`  | `yes` / `no`; disabled rows are dimmed and don't fire.        |
| `LAST RUN` | Relative time of the last fire (`5m ago`, `2h ago`, `never`). |
| `NEXT RUN` | Relative time until the next scheduled fire.                  |
| `STATUS`   | `Running` (with spinner), `failed`, or `idle`.                |

The action bar shows the available keys: `[n]ew`, `[e]dit`,
`[d]elete`, `[space] toggle`, `[r]un now`. `esc` returns to home.

## Add a cron job

Press `n` from the cron list. The form takes six fields, all from
`services/boss/internal/views/cron_form.go`:

| Field      | Required? | Notes                                                                             |
| ---------- | --------- | --------------------------------------------------------------------------------- |
| `Name`     | yes       | Letters, digits, spaces, hyphens, underscores. Up to 80 chars.                    |
| `Repo`     | yes       | Pick from a select of all configured repos.                                       |
| `Prompt`   | yes       | Single-turn prompt the agent runs. Must be self-contained; see the warning below. |
| `Schedule` | yes       | 5-field cron expression or one of `@daily` / `@hourly` / `@weekly` / `@monthly`.  |
| `Timezone` | no        | IANA name (e.g. `America/New_York`). Empty = the daemon's local zone.             |
| `Enabled`  | yes       | Defaults to on. Disabled rows persist but don't fire.                             |

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
4. **Spawn.** A worktree is created on a fresh branch, your repo's
   [setup script](setup-scripts.md) runs, and the agent runner starts
   the agent inside it with the cron job's prompt as the first turn.
5. **Persist.** `last_run_session_id`, `last_run_at`, and
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
  the agent is active, `failed` if the last fire's session ended in
  failure, `idle` otherwise.

There is no separate "history view". Every fire produces a normal
session, so to drill into a specific run, find its session in the
home view (it will have a `cron-…` branch name) or via `boss ls`.

## Run a job ad-hoc

Press `r` on the highlighted row in the cron list. This calls
`RunCronJobNow` and spawns a session immediately, regardless of
schedule. The same overlap and concurrency rules apply. If the
previous fire is still running, you'll see a `Skipped:
overlap_prev_active` toast at the bottom of the list.

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
- your repo's settings (open the Repos screen with `r` from home,
  then `enter` on the repo): per-repo automation flags that apply
  to cron-spawned PRs same as manual ones.
