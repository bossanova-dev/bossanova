---
name: orchestrator
description: Operating guide for driving bossanova coding sessions from Hermes through the bossanova_* tools — finding repos and sessions, starting sessions and chats, sending messages, reading transcripts, watching statuses, scheduling cron jobs, broadcasting, and the confirm rule for destructive tools.
---

# Orchestrating bossanova from Hermes

bossanova runs coding agents (Claude Code, Codex, …) in isolated git worktrees, one **session** per
branch, each holding one or more **chats**. The `bossd` daemon owns those sessions. Every tool in the
`bossanova` toolset is a bossanova MCP tool, named `bossanova_<tool>`, proxied to the daemon on this
machine. Read a tool's own description before its first use: it is the authoritative contract.

You are the **orchestrator**, not the coder. The coding happens inside the sessions bossd starts.
The `boss-*` workflow skills (`boss-plan`, `boss-build`, `boss-repair`, `boss-review`, …) are
installed for the coding agents and run **inside** those sessions — Hermes does not execute them
itself. To have one run, create a session (or send a message to a chat) whose prompt invokes it, for
example `/boss-build <ticket id>`, and then watch the session.

## Find your bearings

- `bossanova_list_repos` lists registered repos and their ids; `bossanova_resolve_context` maps a
  working directory to its repo (and session, when it is a session worktree). Every repo-scoped tool
  takes that daemon-local repo id, never a git origin URL.
- `bossanova_list_sessions` (filter by `repo_id`, `states`) and `bossanova_get_session` show sessions,
  their branch, PR and chats; `bossanova_list_chats` lists a session's chats and their
  `agent_session_id`s.
- `bossanova_list_repo_prs` and `bossanova_list_tracker_issues` show work that could become a session.
- `/bossanova status` prints the resolved MCP binary, socket, settings file and a live status summary.

## Start work

- `bossanova_create_session` with `repo_id` and a `prompt` starts a session on a new worktree and
  branch. For unattended work set `detach: true` (a headless first pass) or `tmux_unattended: true`
  (a durable pane that survives a daemon restart). A create that names a branch or PR an active session
  already owns **attaches** to that session and does not run your prompt — read the response's
  `attached_existing` / `note` fields and deliver the prompt with `send_chat_message` instead.
- `bossanova_start_chat` opens another chat in an existing session.
- `bossanova_send_chat_message` delivers a message to a chat by `agent_session_id`; it wakes a sleeping
  chat by default. Check the response before assuming the agent started a turn.

## Watch and read

- `bossanova_get_session_statuses` gives each session's best chat status (aggregate only).
- `bossanova_get_chat_statuses` gives per-chat status with liveness fields. `last_output_at` is a
  floor, not liveness — decide "stalled" from the liveness fields the tool returns.
- `bossanova_get_chat_transcript` returns a chat's conversation and final assistant text — the way to
  learn what an agent did or why it stopped.
- `bossanova_list_check_snapshots` and `bossanova_list_github_callbacks` show CI state and pending
  PR-event callbacks; `bossanova_register_github_callback` asks bossd to message a chat when a PR
  reaches a state, instead of polling.

Poll sparingly: statuses change on the scale of minutes. Prefer callbacks and broadcast subscriptions
over tight loops.

## Schedule and fan out

- Cron: `bossanova_create_cron_job` (repo, name, prompt, cron `schedule`, optional `timezone`,
  `gate_command`), `bossanova_list_cron_jobs`, `bossanova_update_cron_job`, `bossanova_run_cron_job_now`.
  Each fire starts a session running the job's prompt.
- Broadcasts: `bossanova_send_broadcast` delivers one message to every chat an audience selector
  matches (`repo:<id>`, `session:<id>`, `agent:<name>`, …); `bossanova_register_broadcast_subscription`
  sends one when a session completes or errors. `bossanova_list_broadcasts` and
  `bossanova_list_broadcast_subscriptions` show what is pending.

## Destructive tools need `confirm: true`

Tools that discard work or data — `remove_repo`, `close_session`, `merge_session`, `remove_session`,
`archive_session`, `resurrect_session`, `delete_chat`, `empty_trash`, `delete_cron_job`,
`remove_account`, `delete_github_callback`, `delete_broadcast`, `delete_broadcast_subscription`,
`delete_note` — refuse unless the call passes `confirm: true`. Only pass it when the user asked for
that specific action, and say what will be lost before you do.

## When a call fails

Every tool returns JSON. `{"error": …}` with `"hint": "run boss hermes status"` usually means the call never
reached the daemon: the MCP binary is missing, bossd is not running, or the install config is stale.
Ask the user to run `boss hermes status` (or `boss hermes install` after upgrading `boss`). An
`{"error": …}` without that hint is the daemon's own answer — read it and adjust the request.
