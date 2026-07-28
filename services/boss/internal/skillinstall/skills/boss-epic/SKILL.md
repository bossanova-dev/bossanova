---
name: boss-epic
description: Orchestrate an entire epic of planned Linear tickets to merged PRs, unattended. Assembles the epic's sub-issues (or an explicit ticket list), computes a dependency-ordered schedule, spawns parallel boss-build sessions, drives repair on failures, serializes merges, and reports progress on the parent issue. Use when asked to "implement an epic", "run this epic", "boss-epic", or given an epic parent ticket to ship end-to-end.
allowed-tools: Bash, Read, Glob, Grep, Skill
---

# boss-epic

## Overview

Take a whole **epic** — a Linear parent issue whose sub-issues are already
planned, agent-friendly tickets, or an explicit list of such tickets — and drive
every eligible ticket from its planned state to a **merged PR**, with **no human present**.
This is the fan-out sibling of `boss-build`: where `boss-build` ships one
ticket, `boss-epic` schedules a fleet of them, respecting dependency order,
capping concurrency, running repair on red PRs, and serializing merges so the
base branch never races itself.

This skill is **prose driving tested primitives**. All scheduling, eligibility,
dependency-graph, cascade-skip, and merge-ordering decisions are computed by the
pure, unit-tested DAG scheduler in the installed `boss-epic/toolbox/dag-scheduler.mjs`
— re-exported through `bs-epic-lib.mjs`, which adds the tracker-coupled
classify/normalize/parse surface — this skill never re-derives them inline. The
skill's own job is the I/O, and it reaches every Bossanova coupling through the
**adapter seams** below: the tracker adapter (assembly, state, progress comment)
and the session-runner adapter (spawn/poll/merge sessions, per-ticket dispatch).

The unit of work per ticket is a `subSkills.implement` (`/boss-build <TICKET>`)
session. Do **not** re-implement boss-build's pipeline here; boss-epic only
schedules and merges.

## Operating Contract

- **Headless and unattended.** After Phase 0, make **zero** `AskUserQuestion`
  calls — a boss-epic run is fire-and-forget. Every decision has a coded default.
  Under `BOSS_CRON=true`, never ask at any phase. See Safety rails.
- **Idempotent start / resumable.** The exact same invocation can be re-run
  after a killed driver: Phase 2 reconstructs live state from Linear + open
  sessions and the progress comment marker, adopting in-flight work rather than
  duplicating it.
- **Serialized merges.** At most one merge in flight, ever — the base branch is
  advanced one PR at a time, in `nextToMerge` order.
- **Prefer a callback over blind polling.** Whenever you are about to wait on a
  child's PR / CI check / merge state, arm a one-shot callback group first rather
  than spinning on the poll — gated on the single `callbacksAvailable(env)` signal
  (`scripts/callback/adapter.mjs`, keyed on `BOSS_SESSION_ID`). Gate true ⇒
  `registerWatch` per in-flight child; gate false ⇒ skip arming and let
  `policy.fallbackPoll` alone drive Phase 3 — an explicit no-op, never a failed
  wait. Protocol: [`references/callback-watches.md`](references/callback-watches.md).
- **The parent issue is never mutated.** boss-epic moves only the explicitly
  enumerated child tickets to `Done`; it never closes or restates the parent.
- **Empty eligible set is success.** A parent whose children are all already
  done / not yet planned is a clean no-op, not an error.
- **Planning-only work is not implementation fan-out.** Route by intent; never send `/boss-plan`, plan-review, or recon through the implementation path:
  - Implementation work uses the session-runner `createSession` capability with `tmux_unattended: true` — the durable PR-backed run (Phase 3a).
  - Unattended planning, recon, or plan-review stays inside the driver as a subagent — no session at all.
  - Visible planning chat uses the session-runner `createPlanningChat` capability (`create_session` with `quick_chat: true`, no worktree/branch/PR).
  - It must not use `create_session` with `tmux_unattended`; that path is for PR-backed implementation runs only.

Workspace facts (resolve at runtime, do not hard-code):

- Tracker identity — the tracker MCP server, the backlog team and its key, and
  the workflow state names — is supplied by the **tracker adapter** (see Adapter
  seams), never hard-coded here: the adapter's `operationMap.selectPlanned` owns
  the eligible (planned-state) queue and knows the review state. Terminal `Done` /
  `Canceled` are matched by state _type_ (`BLOCKER_CLEARED_STATE_TYPES`), not by
  literal name.
- boss MCP tools used: `create_session` (with `tmux_unattended`+`model` — the
  durable tmux-hosted run path), `get_session`, `list_sessions`,
  `list_check_snapshots`, `get_chat_statuses`, `merge_session`,
  `resolve_context`, `list_agents`, and for repair rounds `record_chat` +
  `send_chat_message` (start a fresh chat in the ticket's own session — see
  Phase 3c). `get_session_statuses` is session-aggregate only.
- Session-title convention (the resume anchor): `boss-epic <TICKET>: <ticket title>`.

## Adapter seams (the pluggable boundary)

boss-epic's Bossanova coupling is reached through resolver seams, so a different
tracker or session runner can slot in without touching the phase logic. The
Bossanova reference impls resolve to today's exact tools and sub-skills —
**zero behaviour change**.

- **Pure DAG decisions** — `dag-scheduler.mjs`: `buildGraph`,
  `readyTickets`, `transitiveDependents`, `nextToMerge`,
  `mergeBlockedExternalBlockers`. Tracker-agnostic; re-exported through
  `bs-epic-lib.mjs`, so the runnable import shape below is unchanged.
- **Tracker** — `resolveTrackerAdapter(env)` (`scripts/tracker/adapter.mjs`).
  Epic/children assembly (`operationMap.selectPlanned` / `getIssue`),
  ticket-state writeback (`moveState`), and the single progress comment
  (`readComments` / `writeComment`) route through its `operationMap`;
  `normalizeTicket` + `classifyTickets` (in `bs-epic-lib.mjs`) shape and bucket
  the tickets. Reference `createLinearAdapter` → the Linear MCP tools.
- **Session runner** — `resolveSessionRunnerAdapter(env)`
  (`scripts/session/adapter.mjs`). The boss MCP choreography (`createSession` /
  `getSession` / `listSessions` / `listCheckSnapshots` / `mergeSession` /
  `resolveContext` / `listAgents`, plus `recordChat` / `sendChatMessage` and the
  optional `getSessionStatuses`) and per-ticket dispatch route through its
  `operationMap` + `subSkills`. Reference `createBossSessionRunnerAdapter` → the
  boss MCP tools; `subSkills.implement` = `/boss-build`, `subSkills.repair`
  = `/boss-repair`. The tool/arg names named across Phases 3–4 are exactly this
  map's entries (`merge_session` carries the mandatory `confirm`).
- **Callback notifier** — `resolveCallbackAdapter(env)`
  (`scripts/callback/adapter.mjs`). One-shot GitHub PR-event watches
  (`registerWatch` / `listWatches` / `removeWatch` over `boss callback
add|list|remove`); `policy.watchTriggers` = `checks_passed` / `checks_failed`
  / `merged`, armed as a group per in-flight child **when
  `callbacksAvailable(env)`** (else `policy.fallbackPoll` alone drives the wait).
  A resolved/merged child PR then wakes the epic promptly; every wake
  **reconciles real session/PR state before acting** (Phase 3b). Full protocol:
  [`references/callback-watches.md`](references/callback-watches.md).

## The library: how to compute a decision

Every scheduling decision is a call into the installed `boss-epic/toolbox/bs-epic-lib.mjs`. Feed it
JSON on stdin and read JSON on stdout. The canonical invocation shape (reused at
every call-site — only the imported function and the piped payload change):

```bash
if [ -z "${BOSS_SKILLS_HOME:-}" ]; then
  for candidate in "$HOME/.claude/skills/bossanova" "$HOME/.codex/skills/bossanova"; do
    if [ -d "$candidate/boss-epic/toolbox" ]; then BOSS_SKILLS_HOME="$candidate"; break; fi
  done
fi
test -n "${BOSS_SKILLS_HOME:-}" || { echo "BLOCKED: installed bossanova skills not found"; exit 1; }
BOSS_EPIC_TOOLBOX="$BOSS_SKILLS_HOME/boss-epic/toolbox"
export BOSS_EPIC_TOOLBOX
# Eligibility (planned) and the merge gate (inReview) are gated on the tracker's state
# names, never a baked-in word. Resolve ADAPTER-FIRST via `resolveStateRole`: the tracker
# adapter's OPTIONAL `states` capability is the PRIMARY authority — a repo wired through a
# vendored adapter that knows its own states needs no trackerConfig at all — and the
# .boss-skills.json upward walk is the FALLBACK (the only source for an adapter without the
# capability). Both roles go through the one helper so they cannot diverge.
ADAPTER_STATES="$(node "$(git rev-parse --show-toplevel)/scripts/tracker/cli.mjs" states 2>/dev/null || true)"
resolve_state() { ADAPTER_STATES="$ADAPTER_STATES" ROLE="$1" node --input-type=module -e '
  import { readFileSync } from "node:fs"; import { dirname, join } from "node:path"
  const { resolveStateRole } = await import(`${process.env.BOSS_EPIC_TOOLBOX}/bs-epic-lib.mjs`)
  let adapterStates = null, trackerConfigStates = null
  try { adapterStates = JSON.parse(process.env.ADAPTER_STATES) } catch {}
  for (let d = process.cwd(); ; d = dirname(d)) {
    try { const c = JSON.parse(readFileSync(join(d, ".boss-skills.json")))
      const a = process.env.TRACKER || c.adapters?.tracker || "linear"
      trackerConfigStates = c.trackerConfig?.[a]?.states ?? null; break } catch {}
    if (dirname(d) === d) break
  }
  process.stdout.write(resolveStateRole({ role: process.env.ROLE, adapterStates, trackerConfigStates }) ?? "")
' 2>/dev/null; }
BOSS_EPIC_PLANNED_STATE="$(resolve_state planned)"; BOSS_EPIC_REVIEW_STATE="$(resolve_state inReview)"
export BOSS_EPIC_PLANNED_STATE BOSS_EPIC_REVIEW_STATE
# Fail closed with an actionable message naming BOTH probed sources (symmetric with the
# BOSS_SKILLS_HOME guard above) rather than a buried exception: BLOCK only when NEITHER the
# adapter nor the config yields a planned state, so a repo that is fully functional through
# its adapter never self-disables, and a repo with neither never spawns sessions for
# unplanned work. classifyTickets also throws on an empty state as a library-level backstop.
test -n "$BOSS_EPIC_PLANNED_STATE" || { echo "BLOCKED: no planned state resolved — tracker adapter states capability returned none and .boss-skills.json trackerConfig.<tracker>.states.planned is empty"; exit 1; }
echo "$TICKETS_JSON" | node --input-type=module -e '
  const { classifyTickets, normalizeTicket } = await import(`${process.env.BOSS_EPIC_TOOLBOX}/bs-epic-lib.mjs`)
  const raw = JSON.parse(await new Promise((r) => {
    let s = ""; process.stdin.on("data", (d) => (s += d)); process.stdin.on("end", () => r(s))
  }))
  const tickets = raw.map(normalizeTicket)
  process.stdout.write(JSON.stringify(classifyTickets(tickets, process.env.BOSS_EPIC_PLANNED_STATE)))
'
```

Exports available: `normalizeTicket`, `classifyTickets`, `buildGraph`,
`readyTickets`, `transitiveDependents`, `nextToMerge`, `parseEpicArgs`,
`parseTicketRef`, `mergeBlockedExternalBlockers`, `resolveStateRole`,
`resolvePlannedState`, `BLOCKER_CLEARED_STATE_TYPES`.
Because
`buildGraph`/`readyTickets`/`nextToMerge` return `Map`/`Set` values that do not
round-trip through `JSON.stringify`, keep the multi-step scheduling logic
(Phase 3) inside a **single** node process per poll cycle that imports the lib,
rebuilds the graph from the ticket JSON, folds in the live state Sets, and emits
the plain-array answer (ready ids, next-to-merge id, newly-skipped ids). Never
persist a `Map`/`Set` across processes — persist the ticket JSON + the id lists.

## Phase 0 — Preflight

1. **Parse args** via `parseEpicArgs`. A single positional is the epic
   **parent**; two or more are an **explicit list**. Each positional may be a
   bare ticket id (`<TICKET>`) **or a pasted Linear issue URL**
   (`https://linear.app/<workspace>/issue/<TICKET>/<slug>`) — both resolve to the
   id. `--parallel N` is an integer 1..8 (default 4); `--agent <name>` defaults
   to `claude`. Two repeatable operator overrides clear a named external blocker:
   `--assume-cleared <ref>` unblocks a parked dependent for **launch only**, and
   `--assume-cleared-and-merge <ref>` additionally lets the serialized merge step
   (Phase 3d) merge past that blocker's own still-open gate. Both accept a bare
   id or a Linear URL. The parse returns `{parentId, ids, parallel, agent,
assumeCleared, assumeClearedAndMerge}`.

   ```bash
   node --input-type=module -e '
     const { parseEpicArgs } = await import(`${process.env.BOSS_EPIC_TOOLBOX}/bs-epic-lib.mjs`)
     process.stdout.write(JSON.stringify(parseEpicArgs(process.argv.slice(1))))
   ' -- <TICKET> --parallel 4 --agent claude
   ```

   `parseEpicArgs` throws on zero ids, a positional that is neither a ticket id
   nor a Linear URL (catches a typo'd flag), `--parallel` out of range, or an
   `--assume-cleared*` value that is not a ticket id/URL. On a throw, stop with
   `BLOCKED: <message>` — do not guess.

   **Model.** Fan-out runs on Opus by default — set `MODEL="claude-opus-4-8"`
   (or an operator-supplied id) and pass it as `create_session {model: …}` in
   Phase 3a. No `/model` two-step: the model is a first-class field on the run.

   **`boss` binary.** Resolve it once as `BOSS`: prefer `$BOSS_BIN` (exported in
   boss-managed chats), else `boss` on `PATH`, else the repo build `./bin/boss`
   (`make build` if missing). Use `"$BOSS"` at every call-site so a driver chat
   without `boss` on `PATH` still works.

2. **Verify the tracker MCP** with a cheap read — the adapter's `selectPlanned`
   capability (`operationMap.selectPlanned`), the same planned-queue list the epic
   assembly uses (an empty result still proves reachability).
   Unreachable → stop `BLOCKED: tracker MCP unreachable`.

3. **Verify boss MCP + discover every required tool (deterministic preflight).**
   Call `list_agents`: the chosen `--agent` runner **must** appear; if the daemon
   is down or the runner is not loaded, stop
   `BLOCKED: boss daemon unreachable or agent '<name>' not loaded`. Then prove
   **every** boss MCP tool this run can invoke is discoverable _before scheduling_
   — a prior preflight-gap failure was `list_check_snapshots` surfacing only after
   a targeted search mid-run. The required set is derived from the session-adapter
   source of truth, never hand-listed here:

   ```bash
   REPO_ROOT="$(git rev-parse --show-toplevel)"; export REPO_ROOT
   node --input-type=module -e '
     const { requiredBossToolsForEpic } = await import(`${process.env.REPO_ROOT}/scripts/session/boss.mjs`)
     process.stdout.write(requiredBossToolsForEpic().join("\n") + "\n")
   '  # → the authoritative checklist (create_session … list_check_snapshots … send_chat_message)
   ```

   Enumerate the boss MCP tools this runtime actually exposes (host-specific — do
   not overfit to one host) and diff them against the checklist via
   `bossEpicToolPreflight(availableTools)` → `{ ok, missing }`. If `missing` is
   non-empty, stop
   `BLOCKED: boss MCP missing required tools: <comma-separated missing>` naming
   the absent tools (e.g. `list_check_snapshots`). When all are present the
   preflight is a no-op — scheduling and merge behaviour are unchanged.

4. **Resolve `repo_id`** from `$BOSS_REPO_ID` (set in boss-managed chats) if
   present, else `resolve_context {working_dir: <cwd>}`. No repo → stop BLOCKED.

5. **Agent choice.** Default `--agent claude`. In Phase 3 every ticket runs as a
   tmux-hosted `create_session` (`tmux_unattended: true` — the prompt is
   auto-injected and submitted into a live tmux pane, like a cron run, so the
   agent proceeds autonomously; the pane survives a `bossd` restart and is
   attach-safe); repair runs in a fresh chat inside the ticket's session
   (Phase 3c). QUESTION stalls largely disappear: an unattended
   `/boss-build` self-decides under `BOSS_CRON` and, if truly stuck, ends
   BLOCKED (fail-isolated). Non-`claude` agents work the same way, but the
   settled-green gate still applies: a runner without readable chat status must
   hold or fail-isolate, never merge from `get_session` + `list_check_snapshots`
   alone.

`AskUserQuestion` is permitted **only** in Phase 0 and only when
`BOSS_CRON` is unset (a manual invocation) — e.g. to confirm an ambiguous
`working_dir`. Once Phase 1 begins it is never used again.

## Phase 1 — Assemble the epic set

1. **Gather tickets.**
   - Parent mode: `get_issue <parentId>`, then
     `list_issues parentId=<parentId> limit=250` for the children.
   - List mode: `get_issue` for each explicitly listed id (no parent).
   - For every child, `get_issue includeRelations=true` so `blockedBy` relations
     are present, then `normalizeTicket` each payload.

2. **Classify** via `classifyTickets` → `{eligible, done, skipped}`:
   - `eligible`: the planned state + `agent-friendly` label +
     has an Implementation-plan link + not `needs-human`.
   - `done`: state Done/Canceled — already merged for scheduling purposes.
   - `skipped`: everything else, each with a `{ticket, reason}` (not-yet-planned,
     In Progress, In Review, missing plan, `needs-human`, …).

   Immediately print the classification table in the driver chat. **When the
   eligible set is non-empty** (sessions will spawn), also post the initial
   progress comment on the parent issue (see Reporting) before any session spawns,
   so a human watching sees the plan up front. When it is empty, skip the initial
   post — the zero-launch branch (step 3) owns the single comment so the run never
   create-then-edits.

3. **Empty eligible set → zero-launch success (explicit branch).** No `eligible`
   tickets means no session will launch. Run Phase 2 reconstruction; if it adopts
   **no** in-flight session either, this is a **zero-launch** (no-ready /
   no-inflight) run that spawns **zero** sessions. Do not create an initial comment
   and then immediately edit a final one — **upsert exactly one** progress comment
   (edit the existing `<!-- boss-epic-progress -->` comment in place if the marker
   is present, else create it once) whose body **is** the final summary: the
   classification table plus a line stating **`no sessions spawned`**. Print the
   same in the driver chat and **stop success** (a clean no-op, not an error),
   skipping Phases 3–4. A resume that adopts an in-flight session is **not**
   zero-launch — it continues to Phase 3.

4. **Build the dependency graph.** _Critical wiring contract:_ feed `buildGraph`
   the **`classifyTickets(...).eligible`** list **plus** fold every `done`
   ticket's id into the `externallyCleared` Set — **never** the raw full ticket
   list. `readyTickets` trusts every graph node to be eligible; handing it
   unclassified tickets would surface not-yet-planned / In Progress / In Review
   tickets as ready and spawn sessions for unplanned work. A `done` sibling is
   **not** a graph node, so its `blockedBy` edge is _external_ and clears
   against `externallyCleared` — never against `merged`. Seeding it with the
   `done` ids unblocks tickets behind already-done siblings (no step-5
   `get_issue` needed). `merged` starts empty: only tickets this run merges.

   ```bash
   # inside the single scheduling process:
   #   const { eligible, done } = classifyTickets(tickets, plannedState)
   #   const graph = buildGraph(eligible)                       // eligible-only nodes
   #   // done siblings clear, PLUS both operator override sets clear for LAUNCH:
   #   const externallyCleared = new Set([
   #     ...done.map((t) => t.id), ...assumeCleared, ...assumeClearedAndMerge,
   #   ])
   #   const merged = new Set()                                 // this run's merges
   ```

5. **External blockers.** `buildGraph` partitions each ticket's `blockedBy` into
   in-epic edges vs. external (outside the node set). For each external blocker
   id, `get_issue` it and mark it `externallyCleared` **iff** its state type is
   completed/canceled (the same `BLOCKER_CLEARED_STATE_TYPES` rule the library
   uses). An uncleared external blocker parks its dependents; re-check external
   blockers **every poll cycle**, so an external ticket finishing mid-run unparks
   its dependents automatically. Both `--assume-cleared*` overrides are folded
   into the launch-clearing set above (unparking a dependent behind a gate the
   operator has decided to proceed past), but launch clearance is looser than
   **merge** clearance: only `--assume-cleared-and-merge` ids clear the Phase 3d
   merge-time re-check (`mergeBlockedExternalBlockers`); a plain `--assume-cleared`
   launches dependents yet the merge step still refuses to merge past that
   blocker's own still-open gate.

## Phase 2 — Resume reconstruction (idempotent start)

Rebuild live state so a killed driver relaunched with the identical command
adopts rather than duplicates:

1. **Already-terminal children.** A child now Done/Canceled was routed into the
   `done` bucket by Phase 1 classification, so its id is already in
   `externallyCleared` and its dependents are unblocked — verify nothing
   further is owed.

2. **Adopt open sessions.** `list_sessions {repo_id}` and match sessions to epic
   tickets by the title convention `boss-epic <TICKET>: …` or by `tracker_id`. A live
   session for a ticket is **adopted** into the in-flight table with its recorded
   `session_id` + `chat_id` — never a second session for the same ticket.

3. **Finish stranded merges.** A session already in `MERGED` state whose ticket
   is still `In Review` → complete the bookkeeping: `save_issue {id, state:
"Done"}`, fold its id into `externallyCleared` (an In-Review child is not a
   graph node, so dependents see it as external), and record it as merged in
   the progress table.

4. Reload the progress comment by its `<!-- boss-epic-progress -->` marker so
   Phase 3 edits it in place instead of creating a duplicate.

## Phase 3 — Scheduling loop

Repeat until the ready set is empty **and** the in-flight table is empty. State
carried across cycles as plain data: the ticket JSON, and the id lists `merged`,
`failed`, `inFlight`, `externallyCleared`, `greens`, plus per-ticket
`session_id` / `chat_id`, wall-clock start, repair-round counters, and — for the
3c frozen-lease escape — `prevLastRepairHeadSha` (the previous poll's
`last_repair_head_sha`) and `repairStallSince` (when the lease first classified
`stalled`).
A ticket enters `inFlight` at launch (3a) and leaves only when folded into
`merged` (3d) or `failed` (3e); `greens` is a **subset**
of `inFlight` — a green keeps its concurrency slot until merged, so
`readyTickets` never re-lists it and the exit condition holds.

### 3a. Launch

Compute the ready set via `readyTickets` and launch up to
`parallel - inFlight.size` new sessions:

```bash
# inside the scheduling process, after buildGraph + state Sets are assembled:
#   const ready = readyTickets(graph, { merged, failed, inFlight, externallyCleared })
#   // readyTickets internally cascade-skips dependents of `failed`; do not
#   // re-filter — it is the single authority.
#   process.stdout.write(JSON.stringify(ready.map((t) => t.id)))
```

The `readyTickets` set is drawn only from the eligible (planned-state)
implementation tickets classified in Phase 1, so this `createSession` block is for
implementation fan-out **only** — never for planning/recon/plan-review subtasks
(route those per the Operating Contract: subagent, or `createPlanningChat`).

For each ready id, up to the concurrency headroom (highest-priority first — the
array is already sorted), dispatch **one tmux-hosted unattended run** (the
session-runner `createSession` capability, prompt = `subSkills.implement`): the
prompt is auto-injected and submitted into a dedicated tmux pane (cron-style
delivery), so the agent runs with no TUI attach and the pane
survives a `bossd` restart:

```
create_session {
  repo_id,
  tmux_unattended: true,        // durable, restart-surviving, attach-safe
  model:  "claude-opus-4-8",   // MODEL from Phase 0 — no /model two-step
  prompt: "/boss-build <TICKET>",   // BARE single-line command — see below
  title:  "boss-epic <TICKET>: <ticket title>",
  agent,                       // from Phase 0
  tracker_id:     "<TICKET>",
  tracker_source: "linear",
  tracker_url:    <ticket url>
}
```

**The prompt MUST be the bare single-line slash command — no preamble.** The
daemon auto-submits a pane's prompt only when it is one trimmed line starting
with `/` or `$` (`cronChatInputFromPrompt`); a multi-line prompt is pasted but
left **unsubmitted** and the run never starts. A preamble is unnecessary
anyway: `tmux_unattended` sessions run with `BOSS_CRON=true`, so
`/boss-build` self-decides (no questions; ends BLOCKED if truly stuck).
Repair (3c) follows the same bare-command rule.

The `create_session` **response carries the primary `chat_id` (agent_session_id)
directly** — record `session_id` + `chat_id` from it (no sqlite read, no
`list_chats` round-trip needed). Add the id to `inFlight` and start its
per-ticket wall clock.

### 3b. Poll

A callback wake (armed per child that enters flight **only when
`callbacksAvailable(env)`**) trims the poll cadence but never replaces this
reconciliation — a wake means _re-read the state below_, never _act on the trigger
name_; dedup by callback id, and re-arm the child's group while it is still in flight.

**Never arm bare `checks_passed` on a child PR.** boss-build opens its PR as a **draft**
and CI runs on drafts, so bare `checks_passed` fires on the first green draft commit —
each premature fire consumes the one-shot watch at a moment that can never be
merge-eligible. Arm **`checks_passed_ready`** (green **and** not a draft — the
merge-eligibility moment) together with `checks_failed` and `merged`, plus optionally
**`ready_for_review`** for the un-draft flip itself; this draft-aware set replaces the
generic `policy.watchTriggers` default for boss-epic's in-flight watch. The daemon merge
gate stays authoritative — a wake is a signal, not proof. Re-arm any watch consumed
prematurely or expired, keeping one `group` per child so a superseded re-arm cancels its
siblings.

**How the driver waits.** Callbacks are primary; a **session cron** (a scheduled prompt
re-entering this poll cycle every 2–5 minutes) is the bounded fallback. **Never** rely on
backgrounded shell watchers or sleep loops to hold the wait — session hosts may kill them
within the turn, and a driver that assumes them stalls silently. When
`callbacksAvailable` is false, skip arming and let the cron/poll alone drive Phase 3 — a
clean no-op, never a failed wait. Every wake runs the same idempotent cycle below; the
driver never cares _why_ it woke. Trigger policy, cron cadence caveats, and the full
arm/reconcile/re-arm/cleanup protocol:
[`references/callback-watches.md`](references/callback-watches.md).

Every 2–5 minutes (or on a callback wake), for each in-flight ticket read
`get_session` (state, `last_agent_activity_at`, `AGENT_AUTH_FAILED`),
`list_check_snapshots` (DisplayStatus), and `get_chat_statuses {session_id}` for the entry whose
`agent_session_id` equals the ticket's recorded `chat_id`. A green is trustworthy
only once that tracked chat has **settled**: `IDLE` + stale
`last_agent_activity_at`, or `STOPPED` + stale/missing activity.
STOPPED + missing `last_agent_activity_at` = settled. `WORKING`/`QUESTION` =
still running, and `LIMITED` = not merge-settled. If unreadable, treat the child as **not settled** and re-poll; never assume settled on an unreadable status.

`get_session_statuses` is a session-level `get_session_statuses` aggregate across
all chats; display/diagnostic only. Never gate green/settled on it: an older
implementation chat can stay QUESTION/LIMITED while tracked repair chat is
IDLE/STOPPED + passing.

### 3c. Transitions

- **READY_FOR_REVIEW + DisplayStatus Passing + chat SETTLED** (tracked `chat_id`
  is `IDLE` stale, or `STOPPED` stale/missing) → check the remaining rail
  conditions before queueing: the PR is **not a draft**, the linked ticket sits
  in the tracker's **review** state (`$BOSS_EPIC_REVIEW_STATE`), and the PR title/body
  carries no partial-slice / `do not merge` marker. All five hold → add to the
  **greens** (merge queue). Do not add a ticket to `greens` while that chat is
  WORKING, QUESTION, or LIMITED. The daemon Blocks an empty/no-op
  run (a bootstrap-only branch is **not** surfaced as green), so a green here
  genuinely means real work landed. Reads for each condition:
  [`references/merge-recovery.md`](references/merge-recovery.md).
- **GREEN_DRAFT** (Passing + settled, but the PR is still a **draft**) → **hold**,
  never green. CI on a draft is expected noise, not merge-eligibility; the child
  has not declared the slice finished. Re-poll until it is marked ready, or route
  it as an unfinished child.
- **Passing but chat still WORKING/QUESTION/LIMITED** → **hold**: neither green
  nor repair; re-poll while the tracked `chat_id` finalizes review/comments.
- **BLOCKED, or tracked `chat_id` status IDLE/STOPPED + Failing / Conflict /
  Rejected** → **repair
  round**. First classify the repair lease with `classifyRepairLease`
  (bs-epic-lib), fed from `get_session` (`repair_active`, `repair_stalled_at`,
  `last_repair_head_sha`), the driver's `prevLastRepairHeadSha`, and the tracked
  repair chat's last output time:

  - `'active'` — a repairer holds the lease (the auto-repair plugin is engaged).
    Do **not** dispatch a second chat — two repairers on one worktree collide;
    count it as a round and re-poll.
  - `'stalled'` — a **frozen repair lease**. Diagnosis cheat: `repair_active:true`
    with `last_repair_head_sha` unchanged and repair-chat output stale across ≥2
    polls ⇒ dead lease (also reported directly by `repair_stalled_at` on daemons
    that expose it). The round is **exhausted**: count it against the cap
    immediately, then — rounds remaining → dispatch a fresh repair round now (the
    lease is stalled, so a second dispatch no longer collides); cap reached →
    fail-isolate the ticket with a `frozen repair lease` note. Never re-poll a
    lease already classified `stalled`: this is what makes the loop **terminate**,
    so "re-poll forever" is unreachable from a dead lease.
  - `'none'` — no lease held; dispatch below.

  Snapshot `last_repair_head_sha` into `prevLastRepairHeadSha` after every poll,
  and stamp `repairStallSince` on the first `stalled`; drop both when a fresh
  round is dispatched, so that round is judged on its own evidence.

  To dispatch, run `subSkills.repair` (`/boss-repair watch`) in a **new
  chat in the ticket's own session** (the session-runner `recordChat` +
  `sendChatMessage` capabilities) — clean context, session `model` inherited, same
  worktree/branch/PR. Never `create_session` for repair: on a PR whose session
  is live it attaches (`attached_existing: true`) and the supplied prompt is
  **not** run; only an orphan PR with no session (`attached_existing: false`)
  mints a fresh session that runs the prompt. Two calls:

  ```
  record_chat       {session_id, agent_session_id: <fresh uuidgen UUID>,
                     agent_name: agent, title: "boss-epic repair <TICKET> (round N)"}
  send_chat_message {agent_session_id: <same UUID>, wake_if_asleep: true,
                     submit: true, message: "/boss-repair watch"}  // submits
  ```

  Cap at **4 repair rounds per ticket** (a plugin-held or frozen-lease round
  counts too; each
  capped at 5 passes). A conflicted green (Passing but `pr_mergeable=false`)
  needs a repair round, not a merge. Track the repair chat in place of
  the original in `inFlight`; still red after round 4 → fail-isolate.

- **Infra-death** — a state distinct from BLOCKED: the session is idle, the
  chat's last message is a **transient API/5xx error** (not an agent
  conclusion), no spinner, activity frozen. Deliver **one** wake-to-resume —
  `send_chat_message` with wake + submit, telling it to continue from committed
  state and not restart completed work. No resume — or a re-error — within one
  poll cycle → fail-isolate. This is **not** the forbidden BLOCKED-nudge below:
  BLOCKED is the agent's own decision to stop, an infra-death is the harness
  dying mid-work. Diagnosis cheat sheet:
  [`references/merge-recovery.md`](references/merge-recovery.md).

- **Wall clock exceeded** (default **90 min**) → fail-isolate. Do **not**
  `stop_session` — leave the session open for a human.

A BLOCKED run gets a capped repair round or is fail-isolated — never a nudge
into the original chat.

### 3d. Merge (serialized — at most one merge in flight, ever)

Advance the base branch one PR at a time. Compute the merge target:

```bash
# inside the scheduling process:
#   const target = nextToMerge(greens, graph, merged)   // id or null
#   //   greens: [{id}, ...]   merged: Set of merged ids   (THREE args)
```

`nextToMerge` returns the single id mergeable right now (all **in-epic** blockers
already in `merged`, tie-broken by priority then oldest), or `null`. If non-null,
**re-check its own external blockers at merge time** before merging:

```bash
# inside the scheduling process, for the nextToMerge target:
#   // freshly fetch each external blocker's current Linear state type, then:
#   const open = mergeBlockedExternalBlockers(graph.nodes.get(target), graph, {
#     clearedForMerge: new Set(assumeClearedAndMerge),   // ONLY the --and-merge override
#     blockerStateTypes,                                 // id → fresh Linear state type
#   })
#   // open.length > 0 → skip-with-note this cycle; do NOT merge past the gate.
```

`mergeBlockedExternalBlockers` returns the still-open external blocker ids —
neither resolved (Done/Canceled) nor cleared by `--assume-cleared-and-merge`. If
non-empty, **skip the merge this cycle with a progress-comment note** (e.g.
`<TICKET> held: external blocker <ID> still open`) and re-check next cycle; a plain
`--assume-cleared` (launch-only) does **not** clear this gate. If empty:

1. `merge_session {id: <session_id>, confirm: true}`. The `confirm: true` is
   **mandatory** — `merge_session` refuses without it. Never merge outside the
   `nextToMerge` order.
2. **Success** → verify `get_session` shows `MERGED`, then `save_issue {id,
state: "Done"}`, fold the id into `merged` (plus `externallyCleared` for a
   non-node adopted child — its dependents gate externally and must unpark
   before the exit check), and refresh the base:
   `git pull --rebase` in the driver's repo checkout (skip with a logged note if
   the driver worktree is not on the base branch or is dirty).
3. **`FailedPrecondition "PR is not passing"` or a conflict** (a sibling merge
   just landed and invalidated this green) → demote the ticket from `greens` back
   to a repair round. This conflict-after-green demotion gets its **own single
   extra** repair round and does **not** consume the ticket's normal 4-round cap
   the first time.
4. **Any other merge error** → never treat the merge as failed until you have
   re-read the PR's real provider state (an error can follow a merge that
   landed), and read a rebase refusal as a merge commit on the branch, not as a
   red PR. Both recipes:
   [`references/merge-recovery.md`](references/merge-recovery.md).

Only one `merge_session` may be outstanding per cycle; compute `nextToMerge`,
re-check its external blockers, merge it, verify, and only then consider the next.

**Expect drift, and budget for it.** Because merges are serialized and builds are long, a
late-finishing child WILL sit many merges behind the base by the time its turn comes.
Two mechanisms absorb that, neither of them this skill's to enable: the daemon's **opt-in
proactive rebase** (a per-repo setting) keeps in-flight branches current without driver
action, and the **linear-history invariant** — every child rebases onto the base and never
merges the base back in — keeps whatever conflict does surface a single-branch replay
rather than a tangled merge graph. Treat a late drift conflict as a normal repair round
(step 3 above), not as a child failure.

### 3e. Fail-isolate bookkeeping

To fail-isolate a ticket: add its id to `failed`, **leave the session open**
(isolate = preserve evidence for a human; never `stop_session`), recompute the
cascade of now-unreachable dependents via `transitiveDependents(graph,
failed)`, mark each as skipped with the failed ancestor named in the reason, and
update the progress comment.

### 3f. Report every transition

After **every** state change (launch, green, merged, repair kickoff, failed,
skipped, external-unpark) update the single parent-issue progress comment (see
Reporting). Never post a per-event comment.

## Phase 4 — Final report

When the loop exits, post the **final summary** as the last edit of the progress
comment and print the same in the driver chat:

- **merged** — ticket → PR link, in merge order.
- **failed-isolated** — ticket → session id + last DisplayStatus + a one-line
  reason.
- **skipped** — ticket → reason (ineligible, or cascade-skipped under a named
  failed ancestor).
- **duration** — wall-clock of the whole run.

If every eligible ticket merged, note that the epic is complete — but **the
parent issue is left for the human to close**; boss-epic never mutates the
parent's state.

## Reporting contract (single-comment protocol)

Exactly **one** comment on the parent issue, created on the first report and
**edited in place** thereafter. Find it on resume by the marker line, which is
the literal first line of the comment body:

```
<!-- boss-epic-progress -->
```

Body below the marker: a `| ticket | state | session | note |` table (one row
per epic ticket) plus a one-line legend explaining the state vocabulary
(queued / in-flight / green / merged / repairing / failed / skipped). In list
mode (no parent issue), post the progress comment on the first ticket in the
list and note that anchor in the driver chat.

**Do not hand-roll the renderer or the create-vs-update decision** —
`toolbox/progress-comment.mjs` is the reference implementation: `validateProgressState`
pins the state-file schema (`epicId`, the **bare** `marker` token, a caller-supplied
`updatedAt`, and an ordered `tickets[]` whose `status` is one of the six
`PROGRESS_STATUSES` — map the driver vocabulary above onto them, queued → `pending` and
in-flight or repairing → `building`, carrying any round count in `rounds` / `note`),
`renderProgressComment(state)` builds the body, and
`planProgressCommentUpsert({comments, marker, body})` returns the `create` or `update` op
the driver executes verbatim through the tracker adapter's `writeComment` /
`updateComment`. The helpers are pure — they never call a tracker themselves.

**Never** post per-event comments —
the single edited comment is the whole audit trail, and its marker is what makes
resume idempotent. On a **zero-launch** run (no-ready / no-inflight — Phase 1
step 3) this same single comment is upserted exactly once as the final summary
carrying `no sessions spawned`; there is no separate initial-then-final edit.

## Safety rails

State and honor these explicitly:

- **Never `merge_session` without `confirm: true`**, and never merge outside the
  `nextToMerge` order. At most one merge in flight, ever.
- **A PR number alone is never completion or merge-readiness.** An open PR —
  especially an empty draft placeholder — proves only that a branch exists. A
  ticket is **merge-eligible only when all five hold**: (1) the daemon merge
  gate is clear (`gate 1` / DisplayStatus `Passing` — the daemon gate is
  authoritative); (2) the build chat has SETTLED (idle, no spinner, stable
  across two polls) with real changed files; (3) the PR is **not a draft**;
  (4) the linked ticket sits in the tracker's **review** state (resolved from
  the `inReview` role adapter-first by the same `resolveStateRole` as the
  planned state — the adapter's `states` capability, else the configured
  `states.inReview` — and exported by Phase 0 as `$BOSS_EPIC_REVIEW_STATE`;
  never a baked-in state name. **Empty ⇒ condition 4 cannot hold: never
  merge, never compare against `""`.** Phase 0 does not BLOCK on an empty
  review state — only `planned` is preflight-critical — so this gate is where
  it fails closed);
  (5) the PR title/body carries **no** partial-slice or `do not merge` marker.
  Green on a _draft_ PR is expected CI noise, not merge-eligibility. Rationale
  and the read for each condition:
  [`references/merge-recovery.md`](references/merge-recovery.md).
- **Never `stop_session` a failed ticket.** Fail-isolate means leave the session
  open so a human inherits the evidence.
- **Never call AskUserQuestion after Phase 0** — the run is fire-and-forget.
  Under `BOSS_CRON=true`, never call it at all.
- **Tickets outside the epic set are NEVER mutated.** Only the children
  explicitly enumerated in Phase 1 are ever moved to `Done`. This is the guard
  against a foreign `blocks` relation dragging an unrelated ticket into the
  merge set: `buildGraph` classifies such a relation as an _external_ blocker
  (a node outside the graph), so it can gate readiness but can never be merged
  or restated by this skill.
- **The graph fed to the scheduler is eligible-only** (Phase 1 wiring): nodes
  are `classifyTickets(...).eligible`; `done` ids are folded into
  `externallyCleared` (a done sibling is not a node — its edge is external).
  Never feed `readyTickets` the raw ticket list.

## Edge cases

- Parent with zero planned children → empty eligible set → **zero-launch branch**
  (Phase 1 step 3): one progress comment upserted with `no sessions spawned`,
  stop success.
- All children already Done → empty eligible set (`done` bucket →
  `externallyCleared`) → same **zero-launch branch**: one comment upserted with
  `no sessions spawned` (noting "already complete"), stop success.
- External blocker never clears → its dependents stay parked and are skipped;
  the final report names the uncleared external ticket.
- Dependency cycle inside the epic → `readyTickets` never marks the cycle
  members ready (each keeps an unmerged in-epic blocker); reported as
  skipped/blocked. `transitiveDependents` is cycle-safe.
- Driver killed mid-run → relaunch the identical command; Phase 2 adopts
  in-flight sessions and the marker comment.
- Merge-commit deadlock → `merge_session` fails with a rebase refusal
  (`MERGE_STRATEGY_INCOMPATIBLE`) while the PR reads CLEAN. Diagnose with
  `git rev-list --merges --count origin/<base>..<branch>` (any count `> 0` on a
  rebase-strategy repo); the daemon auto-squashes when the repo allows squash, so
  re-invoke `merge_session` **once**. Recovery paths and the linear-history
  prevention invariant (a `git merge` of the base ref is forbidden — always
  rebase): [`references/merge-recovery.md`](references/merge-recovery.md).
- Stranded merge mid-run → on **any** `merge_session` error, re-read the PR's
  actual provider state before treating the merge as failed. `MERGED` means it
  landed: complete the bookkeeping (ticket → its done state, fold into `merged`,
  release the merge slot) and **never re-merge or fail-isolate**. `merge_session`
  is idempotent, so the safe generic move is re-read, then retry once.
- Empty draft PR placeholder — a draft PR with no real file changes and no check
  evidence, present _before/without_ a settled run (e.g. bossd's bootstrap
  draft). This is **not** completion and **not** a settled no-op. Such an
  empty draft PR placeholder routes the child `/boss-build` to
  **adopt the existing branch/session** and continue implementation:
  never restart selection/planning, and never fail-isolate merely because a PR
  exists — a PR number alone is not completion or merge-readiness. Contrast the
  settled no-op below.
- Empty/no-op run → a run that genuinely _settled_ with no changes: the daemon
  Blocks it, so the driver sees BLOCKED and fail-isolates — it never
  merges an empty PR.
- Non-`claude` agent → runs unattended the same way (tmux-hosted run + repair in
  a fresh chat), but the settled-green gate is mandatory. A runner without
  readable chat status must hold or fail-isolate; it must not merge from
  `get_session` state + `list_check_snapshots` alone.

## Setup (one-time, outside this skill)

- Plan the epic's children first (each needs the planned state +
  `agent-friendly` + an Implementation-plan link), e.g. via
  `boss-plan` / `bs-sweep-plan`.
- Invoke `/boss-epic <TICKET>` (parent, id or pasted Linear URL) or
  `/boss-epic <TICKET-A> <TICKET-B> …` (explicit list), optionally `--parallel N`,
  `--agent <name>`, and the `--assume-cleared` / `--assume-cleared-and-merge`
  operator overrides.
