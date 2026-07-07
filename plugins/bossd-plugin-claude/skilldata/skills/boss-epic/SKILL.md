---
name: boss-epic
description: Orchestrate an entire epic of planned Linear tickets to merged PRs, unattended. Assembles the epic's sub-issues (or an explicit ticket list), computes a dependency-ordered schedule, spawns parallel boss-implement sessions, drives repair on failures, serializes merges, and reports progress on the parent issue. Use when asked to "implement an epic", "run this epic", "boss-epic", or given an epic parent ticket like BOS-177 to ship end-to-end.
allowed-tools: Bash, Read, Glob, Grep, Skill
---

# boss-epic

## Overview

Take a whole **epic** — a Linear parent issue whose sub-issues are already
planned, agent-friendly tickets, or an explicit list of such tickets — and drive
every eligible ticket from `Todo` to a **merged PR**, with **no human present**.
This is the fan-out sibling of `boss-implement`: where `boss-implement` ships one
ticket, `boss-epic` schedules a fleet of them, respecting dependency order,
capping concurrency, running repair on red PRs, and serializing merges so the
base branch never races itself.

This skill is **prose driving tested primitives**. All scheduling, eligibility,
dependency-graph, cascade-skip, and merge-ordering decisions are computed by the
pure, unit-tested DAG scheduler in the installed `boss-epic/toolbox/dag-scheduler.mjs`
(BOS-197) — re-exported through `bs-epic-lib.mjs`, which adds the tracker-coupled
classify/normalize/parse surface — this skill never re-derives them inline. The
skill's own job is the I/O, and it reaches every Bossanova coupling through the
**adapter seams** below: the tracker adapter (assembly, state, progress comment)
and the session-runner adapter (spawn/poll/merge sessions, per-ticket dispatch).

The unit of work per ticket is a `subSkills.implement` (`/boss-implement BOS-NN`)
session. Do **not** re-implement boss-implement's pipeline here; boss-epic only
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
- **The parent issue is never mutated.** boss-epic moves only the explicitly
  enumerated child tickets to `Done`; it never closes or restates the parent.
- **Empty eligible set is success.** A parent whose children are all already
  done / not yet planned is a clean no-op, not an error.

Workspace facts (do not re-discover):

- Linear MCP: `bossanova-linear`. Team **Bossanova** (key `BOS`). Statuses by
  name: `Todo` (eligible queue), `In Review`, `Done`, `Canceled`.
- boss MCP tools used: `create_session` (with `tmux_unattended`+`model` — the
  durable tmux-hosted run path), `get_session`, `list_sessions`,
  `list_check_snapshots`, `merge_session`, `resolve_context`, `list_agents`,
  and for repair rounds `record_chat` + `send_chat_message` (start a fresh
  chat in the ticket's own session — see Phase 3c). `get_session_statuses` is
  optional extra signal on `claude` sessions.
- Session-title convention (the resume anchor): `boss-epic BOS-NN: <ticket title>`.

## Adapter seams (the pluggable boundary)

boss-epic's Bossanova coupling is reached through resolver seams, so a different
tracker or session runner can slot in without touching the phase logic. The
Bossanova reference impls resolve to today's exact tools and sub-skills —
**zero behaviour change**.

- **Pure DAG decisions** — `dag-scheduler.mjs` (BOS-197): `buildGraph`,
  `readyTickets`, `transitiveDependents`, `nextToMerge`,
  `mergeBlockedExternalBlockers`. Tracker-agnostic; re-exported through
  `bs-epic-lib.mjs`, so the runnable import shape below is unchanged.
- **Tracker** — `resolveTrackerAdapter(env)` (`scripts/tracker/adapter.mjs`,
  BOS-190). Epic/children assembly (`operationMap.selectPlanned` / `getIssue`),
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
  boss MCP tools; `subSkills.implement` = `/boss-implement`, `subSkills.repair`
  = `/boss-repair`. The tool/arg names named across Phases 3–4 are exactly this
  map's entries (`merge_session` carries the mandatory `confirm`).

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
echo "$TICKETS_JSON" | node --input-type=module -e '
  const { classifyTickets, normalizeTicket } = await import(`${process.env.BOSS_EPIC_TOOLBOX}/bs-epic-lib.mjs`)
  const raw = JSON.parse(await new Promise((r) => {
    let s = ""; process.stdin.on("data", (d) => (s += d)); process.stdin.on("end", () => r(s))
  }))
  const tickets = raw.map(normalizeTicket)
  process.stdout.write(JSON.stringify(classifyTickets(tickets)))
'
```

Exports available: `normalizeTicket`, `classifyTickets`, `buildGraph`,
`readyTickets`, `transitiveDependents`, `nextToMerge`, `parseEpicArgs`,
`parseTicketRef`, `mergeBlockedExternalBlockers`, `BLOCKER_CLEARED_STATE_TYPES`.
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
   bare ticket id (`BOS-177`) **or a pasted Linear issue URL**
   (`https://linear.app/<workspace>/issue/BOS-177/<slug>`) — both resolve to the
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
   ' -- BOS-177 --parallel 4 --agent claude
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

2. **Verify Linear MCP** with a cheap read (`list_issue_statuses team=Bossanova`).
   Unreachable → stop `BLOCKED: Linear MCP unreachable`.

3. **Verify boss MCP** with `list_agents`. The chosen `--agent` runner **must**
   appear in the list; if the daemon is down or the runner is not loaded, stop
   `BLOCKED: boss daemon unreachable or agent '<name>' not loaded`.

4. **Resolve `repo_id`** from `$BOSS_REPO_ID` (set in boss-managed chats) if
   present, else `resolve_context {working_dir: <cwd>}`. No repo → stop BLOCKED.

5. **Agent choice.** Default `--agent claude`. In Phase 3 every ticket runs as a
   tmux-hosted `create_session` (`tmux_unattended: true` — the prompt is
   auto-injected and submitted into a live tmux pane, like a cron run, so the
   agent proceeds autonomously; the pane survives a `bossd` restart and is
   attach-safe); repair runs in a fresh chat inside the ticket's session
   (Phase 3c). QUESTION stalls largely disappear: an unattended
   `/boss-implement` self-decides under `BOSS_CRON` and, if truly stuck, ends
   BLOCKED (fail-isolated). Non-`claude` agents work the same way; chat
   introspection is thinner for codex-exec, so lean on `get_session` +
   `list_check_snapshots`.

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
   - `eligible`: `Todo` + `agent-friendly` label + has an Implementation-plan
     link + not `needs-human`.
   - `done`: state Done/Canceled — already merged for scheduling purposes.
   - `skipped`: everything else, each with a `{ticket, reason}` (Unplanned, In
     Progress, In Review, missing plan, `needs-human`, …).

   Immediately print the classification table in the driver chat **and** post the
   initial progress comment on the parent issue (see Reporting) — before any
   session spawns, so a human watching sees the plan up front.

3. **Empty eligible set → stop** with a success report (nothing to do).

4. **Build the dependency graph.** _Critical wiring contract:_ feed `buildGraph`
   the **`classifyTickets(...).eligible`** list **plus** fold every `done`
   ticket's id into the `externallyCleared` Set — **never** the raw full ticket
   list. `readyTickets` trusts every graph node to be eligible; handing it
   unclassified tickets would surface Unplanned / In Progress / In Review
   tickets as ready and spawn sessions for unplanned work. A `done` sibling is
   **not** a graph node, so its `blockedBy` edge is _external_ and clears
   against `externallyCleared` — never against `merged`. Seeding it with the
   `done` ids unblocks tickets behind already-done siblings (no step-5
   `get_issue` needed). `merged` starts empty: only tickets this run merges.

   ```bash
   # inside the single scheduling process:
   #   const { eligible, done } = classifyTickets(tickets)
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
   tickets by the title convention `boss-epic BOS-NN: …` or by `tracker_id`. A live
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
`session_id` / `chat_id`, wall-clock start, and repair-round counters.
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

For each ready id, up to the concurrency headroom (highest-priority first — the
array is already sorted), dispatch **one tmux-hosted unattended run** (the
session-runner `createSession` capability, prompt = `subSkills.implement`): the
prompt is auto-injected and submitted into a dedicated tmux pane (cron-style
delivery — BOS-179/BOS-208), so the agent runs with no TUI attach and the pane
survives a `bossd` restart:

```
create_session {
  repo_id,
  tmux_unattended: true,        // durable, restart-surviving, attach-safe
  model:  "claude-opus-4-8",   // MODEL from Phase 0 — no /model two-step
  prompt: "/boss-implement BOS-NN",   // BARE single-line command — see below
  title:  "boss-epic BOS-NN: <ticket title>",
  agent,                       // from Phase 0
  tracker_id:     "BOS-NN",
  tracker_source: "linear",
  tracker_url:    <ticket url>
}
```

**The prompt MUST be the bare single-line slash command — no preamble.** The
daemon auto-submits a pane's prompt only when it is one trimmed line starting
with `/` or `$` (`cronChatInputFromPrompt`); a multi-line prompt is pasted but
left **unsubmitted** and the run never starts. A preamble is unnecessary
anyway: `tmux_unattended` sessions run with `BOSS_CRON=true`, so
`/boss-implement` self-decides (no questions; ends BLOCKED if truly stuck).
Repair (3c) follows the same bare-command rule.

The `create_session` **response carries the primary `chat_id` (agent_session_id)
directly** — record `session_id` + `chat_id` from it (no sqlite read, no
`list_chats` round-trip needed). Add the id to `inFlight` and start its
per-ticket wall clock.

### 3b. Poll

Every 2–5 minutes, for each in-flight ticket read `get_session` (session state —
IMPLEMENTING / GREEN_DRAFT / READY_FOR_REVIEW / BLOCKED / MERGED; plus
`last_agent_activity_at` — fresh = a live turn, stale = a hang — and an
`AGENT_AUTH_FAILED` reason flagging login-death) and `list_check_snapshots`
(DisplayStatus: Passing / Failing / Conflict / Rejected / Pending). This is a
**poll-only** loop: the run finalizes its own PR, so the driver reads terminal
state, not a live chat. (`get_session_statuses` = extra `claude` signal.)

### 3c. Transitions

- **GREEN_DRAFT or READY_FOR_REVIEW + DisplayStatus Passing** → add to the
  **greens** (merge queue). BOS-179 makes the daemon Block an empty/no-op run
  (a bootstrap-only branch is **not** surfaced as green), so a green here
  genuinely means real work landed — safe to merge.
- **BLOCKED, or IDLE/STOPPED + Failing / Conflict / Rejected** → **repair
  round**. First read `get_session` `repair_active` (BOS-234): if a repairer
  holds the lease (the auto-repair plugin is engaged), do **not** dispatch a
  second chat — two repairers on one worktree collide; count it as a round and
  re-poll. Otherwise run `subSkills.repair` (`/boss-repair watch`) in a **new
  chat in the ticket's own session** (the session-runner `recordChat` +
  `sendChatMessage` capabilities) — clean context, session `model` inherited, same
  worktree/branch/PR. Never `create_session` for repair: on a PR whose session
  is live it attaches (`attached_existing: true`) and the supplied prompt is
  **not** run; only an orphan PR with no session (`attached_existing: false`)
  mints a fresh session that runs the prompt. Two calls:

  ```
  record_chat       {session_id, agent_session_id: <fresh uuidgen UUID>,
                     agent_name: agent, title: "boss-epic repair BOS-NN (round N)"}
  send_chat_message {agent_session_id: <same UUID>, wake_if_asleep: true,
                     submit: true, message: "/boss-repair watch"}  // submits
  ```

  Cap at **4 repair rounds per ticket** (a plugin-held round counts too; each
  capped at 5 passes). A conflicted green (Passing but `pr_mergeable=false`,
  BOS-234) needs a repair round, not a merge. Track the repair chat in place of
  the original in `inFlight`; still red after round 4 → fail-isolate.

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
`BOS-NN held: external blocker <ID> still open`) and re-check next cycle; a plain
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

Only one `merge_session` may be outstanding per cycle; compute `nextToMerge`,
re-check its external blockers, merge it, verify, and only then consider the next.

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
list and note that anchor in the driver chat. **Never** post per-event comments —
the single edited comment is the whole audit trail, and its marker is what makes
resume idempotent.

## Safety rails

State and honor these explicitly:

- **Never `merge_session` without `confirm: true`**, and never merge outside the
  `nextToMerge` order. At most one merge in flight, ever.
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

- Parent with zero planned children → empty eligible set → clean no-op (Phase 1).
- All children already Done → empty eligible set (`done` bucket →
  `externallyCleared`); clean stop, "already complete".
- External blocker never clears → its dependents stay parked and are skipped;
  the final report names the uncleared external ticket.
- Dependency cycle inside the epic → `readyTickets` never marks the cycle
  members ready (each keeps an unmerged in-epic blocker); reported as
  skipped/blocked. `transitiveDependents` is cycle-safe.
- Driver killed mid-run → relaunch the identical command; Phase 2 adopts
  in-flight sessions and the marker comment.
- Empty/no-op run → the daemon Blocks it (BOS-179), so the driver sees BLOCKED
  and fail-isolates — it never merges an empty PR.
- Non-`claude` agent → runs unattended the same way (tmux-hosted run + repair in
  a fresh chat); only live chat introspection is thinner, so lean on
  `get_session` state + `list_check_snapshots`. It still schedules/merges greens.

## Setup (one-time, outside this skill)

- Plan the epic's children first (each needs `Todo` + `agent-friendly` + an
  Implementation-plan link), e.g. via `boss-plan` / `bs-sweep-plan`.
- Invoke `/boss-epic BOS-NN` (parent, id or pasted Linear URL) or
  `/boss-epic BOS-1 BOS-2 …` (explicit list), optionally `--parallel N`,
  `--agent <name>`, and the `--assume-cleared` / `--assume-cleared-and-merge`
  operator overrides.
