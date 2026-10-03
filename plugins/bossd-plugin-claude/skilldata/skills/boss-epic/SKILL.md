---
name: boss-epic
description: Orchestrate an entire epic of planned Linear tickets to merged PRs, unattended. Assembles the epic's sub-issues (or an explicit ticket list), computes a dependency-ordered schedule, spawns parallel boss-build sessions, drives repair on failures, serializes merges, and reports progress on the parent issue. Use when asked to "implement an epic", "run this epic", "boss-epic", or given an epic parent ticket to ship end-to-end.
allowed-tools: Bash, Read, Glob, Grep, Skill
---

# boss-epic

Drive every eligible ticket of an epic — a parent's planned, agent-friendly sub-issues, or an
explicit list — from planned to a **merged PR**, with no human present. `boss-build` ships one
ticket; `boss-epic` schedules a fleet of them in dependency order, caps concurrency, repairs red PRs
and merges one at a time so the base never races itself. It schedules and merges; it never
re-implements boss-build.

Every scheduling decision is computed by tested helpers — `dag-scheduler.mjs` re-exported through
`bs-epic-lib.mjs`, the lifecycle in `epic-driver.mjs`, reporting in `progress-comment.mjs`. This
skill does the I/O.

## Contract

- **Unattended.** No `AskUserQuestion` after Phase 0 (none at all under `BOSS_CRON=true`); every
  decision has a coded default.
- **Never stop early.** The run is over only when `assertEpicCanTerminate(state)` stops throwing:
  no ready ticket, nothing in flight, no green waiting to merge, no unreconciled adopted session, no
  unrecorded cascade, no pending wake, external blockers evaluated this cycle. `transitionToDone` is
  the only way to `DONE`. A turn ending is never evidence the epic is done, and launching work is a
  checkpoint, not completion.
- **Status vocabulary** for every intermediate response: `RUNNING` (work remains, a wake is armed),
  `RUNNING_BUT_UNWATCHED` (work remains, a required wake is missing — retry or fail closed),
  `BLOCKED` (a capability is unavailable), `DONE`.
- **Resumable.** Re-running the same invocation rehydrates state and adopts in-flight sessions
  (Phase 2) — never duplicates them.
- **One merge at a time**, in `nextToMerge` order.
- **One coordinator per run.** Repeated `--epic <REF>` roots share one child universe, one graph, one
  concurrency budget and one merge queue, keyed on the **child id** (a child under several roots is
  handled once); only reporting fans out, one comment per root. Membership is not a dependency.
  [`references/multi-root.md`](references/multi-root.md).
- **Mutate only the enumerated children.** The parent is never closed or edited; a ticket outside
  the set is never moved, even when it is a blocker.
- **An empty eligible set is success.**
- **Implementation only.** Children run as `create_session` with `tmux_unattended: true`. Planning,
  recon and plan review never go through that path: run them as a subagent, or as a visible planning
  chat via `createPlanningChat` (`quick_chat: true`).

## Adapter seams

- **Tracker** — `resolveTrackerAdapter(env)` (`toolbox/tracker/adapter.mjs`): assembly
  (`selectPlanned`, `getIssue`), state writes (`moveState`), the progress comment
  (`readComments`/`writeComment`/`updateComment`). Workflow states resolve at runtime; Done/Canceled
  match by state type (`BLOCKER_CLEARED_STATE_TYPES`).
- **Session runner** — `resolveSessionRunnerAdapter(env)` (`toolbox/session/adapter.mjs`):
  `createSession`, `getSession`, `listSessions`, `listCheckSnapshots`, `mergeSession`,
  `resolveContext`, `listAgents`, `recordChat`, `sendChatMessage`, optional `getSessionStatuses`;
  `subSkills.implement` = `/boss-build`, `subSkills.repair` = `/boss-repair`. Each entry carries its
  `cli` equivalent (or `cli: null` with a reason); Phase 0 picks the transport and a CLI run uses
  those commands wherever this document names an MCP tool.
- **Callbacks** — `resolveCallbackAdapter(env)` (`toolbox/callback/adapter.mjs`): arm only when
  `callbacksAvailable(env)` **and** `selectEpicCallbackTarget` (`toolbox/callback/epic-target.mjs`)
  returns a verified target; otherwise the bounded fallback drives the wait.
  [`references/callback-watches.md`](references/callback-watches.md).
- Session titles are `[<TICKET>] <ticket title>` — the resume anchor.

## The library

Feed JSON on stdin, read JSON on stdout. Each Bash call is a fresh shell. This block resolves the
toolbox, the planned and review states, and the per-child wall clock, then classifies tickets:

```bash
if [ -z "${BOSS_SKILLS_HOME:-}" ]; then
  for candidate in "$HOME/.claude/skills" "$HOME/.codex/skills"; do
    if [ -d "$candidate/boss-epic/toolbox" ]; then BOSS_SKILLS_HOME="$candidate"; break; fi
  done
fi
test -n "${BOSS_SKILLS_HOME:-}" || { echo "BLOCKED: installed boss skills not found"; exit 1; }
BOSS_EPIC_TOOLBOX="$BOSS_SKILLS_HOME/boss-epic/toolbox"
export BOSS_EPIC_TOOLBOX
# Eligibility (planned) and the merge gate (inReview) are gated on the tracker's state
# names, never a baked-in word. Resolve ADAPTER-FIRST via `resolveStateRole`: the tracker
# adapter's OPTIONAL `states` capability is the PRIMARY authority — a repo wired through a
# vendored adapter that knows its own states needs no trackerConfig at all — and the
# .boss-skills.json upward walk is the FALLBACK (the only source for an adapter without the
# capability). Both roles go through the one helper so they cannot diverge.
ADAPTER_STATES="$(node "$BOSS_EPIC_TOOLBOX/tracker/cli.mjs" states 2>/dev/null || true)"
resolve_state() { ADAPTER_STATES="$ADAPTER_STATES" ROLE="$ROLE" node --input-type=module -e '
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
BOSS_EPIC_PLANNED_STATE="$(ROLE=planned resolve_state)"; BOSS_EPIC_REVIEW_STATE="$(ROLE=inReview resolve_state)"
export BOSS_EPIC_PLANNED_STATE BOSS_EPIC_REVIEW_STATE
# Fail closed with an actionable message naming BOTH probed sources (symmetric with the
# BOSS_SKILLS_HOME guard above) rather than a buried exception: BLOCK only when NEITHER the
# adapter nor the config yields a planned state, so a repo that is fully functional through
# its adapter never self-disables, and a repo with neither never spawns sessions for
# unplanned work. classifyTickets also throws on an empty state as a library-level backstop.
test -n "$BOSS_EPIC_PLANNED_STATE" || { echo "BLOCKED: no planned state resolved — tracker adapter states capability returned none and .boss-skills.json trackerConfig.<tracker>.states.planned is empty"; exit 1; }
# The per-child wall clock is a config knob, not a constant: resolve it through the
# skill-config seam, never a hardcoded number and never a raw .boss-skills.json read. Going
# through `epicChildWallClockMinutes` is what makes an absent `epicDefaults` block resolve
# the built-in default; 3c states what an unresolved budget does to expiry.
BOSS_EPIC_CHILD_WALL_CLOCK_MIN="$(node --input-type=module -e '
  import { pathToFileURL } from "node:url"
  const { loadSkillConfig, epicChildWallClockMinutes } = await import(
    pathToFileURL(process.env.BOSS_EPIC_TOOLBOX + "/skill-config.mjs").href)
  process.stdout.write(String(epicChildWallClockMinutes(loadSkillConfig({ cwd: process.cwd() }))))
')"
export BOSS_EPIC_CHILD_WALL_CLOCK_MIN
# A throwing load yields "" from the substitution above, and `elapsed > ` never fires — the
# same never-expires child the accessor exists to prevent. Fail closed instead.
if [ -z "$BOSS_EPIC_CHILD_WALL_CLOCK_MIN" ]; then BOSS_EPIC_CHILD_WALL_CLOCK_MIN=360; echo "warning: child wall clock unresolved from config; using 360 minutes" >&2; fi
echo "$TICKETS_JSON" | node --input-type=module -e '
  const { classifyTickets, normalizeTicket } = await import(`${process.env.BOSS_EPIC_TOOLBOX}/bs-epic-lib.mjs`)
  const { loadSkillConfig, optionalLabelName } = await import(`${process.env.BOSS_EPIC_TOOLBOX}/skill-config.mjs`)
  const config = loadSkillConfig({ cwd: process.cwd() })
  const raw = JSON.parse(await new Promise((r) => {
    let s = ""; process.stdin.on("data", (d) => (s += d)); process.stdin.on("end", () => r(s))
  }))
  const tickets = raw.map(normalizeTicket)
  process.stdout.write(JSON.stringify(classifyTickets(tickets, process.env.BOSS_EPIC_PLANNED_STATE, {
    agentFriendlyLabel: optionalLabelName(config, "agentFriendly") ?? "agent-friendly",
    needsHumanLabel: optionalLabelName(config, "needsHuman") ?? "needs-human",
  })))
'
```

Exports: `normalizeTicket`, `classifyTickets`, `buildGraph`, `readyTickets`, `transitiveDependents`,
`transitiveDependentCounts`, `nextToMerge`, `parseEpicArgs`, `parseTicketRef`, `buildCombinedRun`,
`mergeBlockedExternalBlockers`, `resolveStateRole`, `resolvePlannedState`, `classifyChildLiveness`,
`classifyRepairLease`, `BLOCKER_CLEARED_STATE_TYPES`. `buildGraph`/`readyTickets`/`nextToMerge` return
`Map`/`Set`s, so run each poll cycle's scheduling in **one** node process and persist only ticket JSON
and id lists.

## Phase 0 — Preflight

1. **Arguments** — `parseEpicArgs` returns `{mode, parentId, parentIds, ids, parallel, agent,
assumeCleared, assumeClearedAndMerge}`: one positional is a parent, several are an explicit list,
   repeated `--epic <REF>` select several roots (never mixed with positionals). Refs may be ids or
   pasted Linear URLs. `--parallel` 1..8 (default 4), `--agent` (default `claude`).
   `--assume-cleared <ref>` unparks a blocked dependent for launch only;
   `--assume-cleared-and-merge <ref>` also lets the merge step pass that blocker. A throw stops
   `BLOCKED: <message>`.

   ```bash
   node --input-type=module -e '
     const { parseEpicArgs } = await import(`${process.env.BOSS_EPIC_TOOLBOX}/bs-epic-lib.mjs`)
     process.stdout.write(JSON.stringify(parseEpicArgs(process.argv.slice(1))))
   ' -- <TICKET> --parallel 4 --agent claude
   ```

   Children run on `MODEL="opus[1m]"` (quoted; bare `claude-opus-5` is the 200K window), passed as
   `create_session {model}`.

2. **`boss` binary** — resolve it once with `resolveBossBinary` (it stats each candidate:
   `$BOSS_BIN`, `boss` on `PATH`, `./bin/boss`) and use `"$BOSS"` everywhere:

   ```bash
   BOSS_SKILLS_HOME="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}"
   if [ ! -d "$BOSS_SKILLS_HOME/boss-epic/toolbox" ]; then BOSS_SKILLS_HOME="$HOME/.codex/skills"; fi
   BOSS_EPIC_TOOLBOX="$BOSS_SKILLS_HOME/boss-epic/toolbox"; export BOSS_EPIC_TOOLBOX
   test -f "$BOSS_EPIC_TOOLBOX/boss-binary.mjs" || { echo "BLOCKED: installed boss skills not found"; exit 1; }
   BOSS_WHY="$(mktemp)"  # stdout = resolved path (empty if none); stderr = the reason
   BOSS="$(node --input-type=module -e '
     import{pathToFileURL as u}from"node:url"
     const { resolveBossBinary } = await import(u(process.env.BOSS_EPIC_TOOLBOX+"/boss-binary.mjs").href)
     const r = resolveBossBinary()
     if (r.ok) process.stdout.write(r.path); else process.stderr.write(r.reason)
   ' 2>"$BOSS_WHY")"
   if [ -z "$BOSS" ]; then echo "BLOCKED: boss CLI unavailable — $(cat "$BOSS_WHY")"; rm -f "$BOSS_WHY"; exit 1; fi
   rm -f "$BOSS_WHY"; export BOSS
   ```

3. **Tracker** — a cheap `selectPlanned` read, classified with `trackerMcpPreflight` so a failure says
   whether to fix the repo's declaration (`absent`) or credentials/network (`unreachable`). Log
   `tracker preflight: <status> (declared: <true|false|no report>)` either way and call tracker
   tools through `resolvedServer`. MCP servers can still be connecting when a session starts: if no
   tracker tools are in your tool list yet, wait about 20 seconds and look again, up to three times,
   before classifying:

   ```bash
   set -euo pipefail
   BOSS_SKILLS_HOME="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}"
   if [ ! -d "$BOSS_SKILLS_HOME/boss-epic/toolbox" ]; then BOSS_SKILLS_HOME="$HOME/.codex/skills"; fi
   BOSS_EPIC_TOOLBOX="$BOSS_SKILLS_HOME/boss-epic/toolbox"; export BOSS_EPIC_TOOLBOX
   BOSS="${BOSS:-$(node --input-type=module -e '
     import{pathToFileURL as u}from"node:url"
     const m = await import(u(process.env.BOSS_EPIC_TOOLBOX+"/boss-binary.mjs").href)
     process.stdout.write(m.resolveBossBinary().path ?? "")
   ' 2>/dev/null || true)}"
   # A server failing at CONNECT publishes no tools, so the tool list alone cannot
   # tell it from one never declared. Older binaries lack this read — hence the
   # explicit failure branch; no report is supported and degrades to today.
   DECLARED="$("$BOSS" session mcp "${BOSS_AGENT_SESSION_ID:-}" --json 2>/dev/null)" || DECLARED=''
   export DECLARED
   node --input-type=module -e '
     import{pathToFileURL as u}from"node:url"
     const { trackerMcpPreflight } = await import(u(process.env.BOSS_EPIC_TOOLBOX+"/tracker/preflight.mjs").href)
     let declaredServers = []
     try { declaredServers = JSON.parse(process.env.DECLARED || "{}").servers || [] } catch {}
     process.stdout.write(JSON.stringify(trackerMcpPreflight({
       operationMap: ADAPTER_OPERATION_MAP, mcpServer: TRACKER_MCP_SERVER,
       agent: process.env.BOSS_AGENT || "", availableTools: AVAILABLE_TOOLS,
       probeOk: PROBE_OK, declaredServers,
     })))
   '
   ```

4. **Transport** — the CLI is preferred; MCP when the CLI set is incomplete; `BLOCKED` only when
   neither is complete. Compare `boss env --json` (`.capabilities.cli`, `.capabilities.mcp`) against:

   ```bash
   BOSS_SKILLS_HOME="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}"
   if [ ! -d "$BOSS_SKILLS_HOME/boss-epic/toolbox" ]; then BOSS_SKILLS_HOME="$HOME/.codex/skills"; fi
   BOSS_EPIC_TOOLBOX="$BOSS_SKILLS_HOME/boss-epic/toolbox"; export BOSS_EPIC_TOOLBOX
   test -f "$BOSS_EPIC_TOOLBOX/session/boss.mjs" || { echo "BLOCKED: installed boss skills not found"; exit 1; }
   node --input-type=module -e '
     import{pathToFileURL as u}from"node:url"
     const m = await import(u(process.env.BOSS_EPIC_TOOLBOX+"/session/boss.mjs").href)
     process.stdout.write("tools:\n" + m.requiredBossToolsForEpic().join("\n") + "\n")
     process.stdout.write("cli:\n" + m.requiredBossCliCommandsForEpic().join("\n") + "\n")
   '  # → authoritative checklists, one per transport
   ```

   `bossEpicTransportPreflight({availableTools, availableCliCommands})` → `{ok, transport, missing,
degraded, partial, inventoryHint}`. Report `transport: <cli|mcp>` in the opening line, plus
   `cli-only mode (expected): resolveContext, getSessionStatuses, createPlanningChat` (no CLI
   equivalents; use their fallbacks) and any `partial:` fields (`boss show --json` lacks
   `repair_active`, `attention_status.reason`, `pr_mergeable`, `merge_block` — an unreadable signal
   is "not settled", never green). The chosen `--agent` must appear in `list_agents` /
   `boss agents --json`.

5. **Repo** — `$BOSS_REPO_ID`, else `resolve_context {working_dir}` (MCP) or `boss env --json` →
   `session.repo_id` (CLI). None ⇒ `BLOCKED`. Never infer it from a directory name.

`AskUserQuestion` is allowed only here, and only on a manual run (`BOSS_CRON` unset).

## Phase 1 — Assemble

1. **Gather.** Parent: `get_issue` it and `list_issues parentId=<id> limit=250`. List: `get_issue`
   each id. Several roots: read **every** root and its children first (fail closed on an unreadable
   root) and combine with `buildCombinedRun({parentIds, childrenByParent})`. Then `get_issue
includeRelations=true` each unique child once and `normalizeTicket` it.
2. **Classify** with `classifyTickets` → `eligible` (planned, `agent-friendly`, an
   `Implementation plan (<id>)` attachment, not `needs-human`), `done` (Done/Canceled), `skipped`
   (with reasons). Print the table. When there is work, post the initial progress comment on each
   root before anything launches.
3. **Nothing eligible and nothing to adopt** (after Phase 2) ⇒ upsert **one** progress comment that
   is the final summary (the table plus `no sessions spawned`), print it, stop success.
4. **Graph.** `buildGraph(eligible)` — eligible tickets only, never the raw list. Seed
   `externallyCleared` with every `done` id plus both `--assume-cleared*` sets; `merged` starts
   empty:

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

5. **External blockers.** For each blocker outside the graph, `get_issue` it: completed/canceled
   clears it; otherwise its dependents park. Re-check every cycle. A blocker owned by a session
   outside this epic is `cannot-evaluate-here` (neither cleared nor blocked, never counted as
   mergeable). Record the best-case merge count.

## Phase 2 — Resume

`loadEpicState({epicId, runId, dir})` returns `null` only for a fresh run and throws on a corrupt file
(starting fresh there would relaunch live children). The `runId` comes from the progress comment's
`run` block (`parseProgressRunMetadata`). `reconcileEpic` adopts each live session by `tracker_id` or
title — at most one per child. For several roots, persist root membership and per-root comment ids.
[`references/epic-driver.md`](references/epic-driver.md).

## Phase 3 — The loop

Every wake (callback, subscription, fallback, retry, manual resume, first run) runs one
`reconcileEpic` cycle; why it woke selects nothing:

```bash
# inside the scheduling process:
#   const { state, status, blockers } = await reconcileEpic({ state, wake, io, now, save })
#   // blockers = epicTerminalBlockers(state); non-empty ⇒ arm the next wake and yield RUNNING
```

The driver persists, after **every** transition: the ticket JSON; `merged`, `failed`, `inFlight`,
`greens` (a subset of `inFlight` — a green keeps its slot until merged), `cascadeSkipped`,
`externallyCleared` as arrays; the wall-clock budget; per-ticket session/chat/PR ids, start time,
repair rounds, `prevLastRepairHeadSha` and `repairStallSince`.

### 3a. Launch

```bash
# inside the scheduling process, after buildGraph + state Sets are assembled:
#   const ready = readyTickets(graph, { merged, failed, inFlight, externallyCleared })
#   // readyTickets internally cascade-skips dependents of `failed`; do not
#   // re-filter — it is the single authority.
#   process.stdout.write(JSON.stringify(ready.map((t) => t.id)))
```

`readyTickets` returns launch order (most transitive unlocks first, then priority, age, id) and
already cascade-skips dependents of failed tickets. Launch up to `parallel - inFlight.size`. Before
each, re-check that the child has no live session (shared children, adopted sessions). One
tmux-hosted unattended run per ticket:

```
create_session {
  repo_id,
  tmux_unattended: true,
  model:  "opus[1m]",
  prompt: "/boss-build <TICKET>",
  title:  "[<TICKET>] <ticket title>",
  agent,
  tracker_id:     "<TICKET>",
  tracker_source: "linear",
  tracker_url:    <ticket url>
}
```

The prompt must be the bare one-line slash command: the daemon submits only a single trimmed line
starting with `/` or `$`, and a multi-line prompt is pasted but never submitted. The response carries
`session_id` and the primary `chat_id`; pass both to `recordLaunch` and start the child's wall clock.

### 3b. Watch and poll

Arm callbacks per child entering flight (when available and targeted), **list-verify** them, and add
a child-session `settled` subscription (`sessionOutcomeMap`, with the child's `--session`). Never arm
bare `checks_passed` on a child PR — boss-build's PR is a draft and that fires on the first green
draft commit; arm `policy.draftAwareTriggers` / `policy.epicChildTriggers` (`checks_passed_ready`,
`ready_for_review`, `merged`, `closed`) and never `policy.forbiddenDraftTriggers`. A `settled`
subscription can fire mid-flight, so `needsRearm` treats `fired`/`expired`/`canceled` as a hole. The
fallback wake is an in-session scheduled wake-up — never `boss cron`, and never a backgrounded shell
loop. A failed arm or verification is a recorded transition (mechanism, next time, reason, retries),
reported as `RUNNING_BUT_UNWATCHED` or `BLOCKED` when no wake can be armed.

Each cycle, per in-flight ticket, read `get_session` (state, `last_agent_activity_at`,
`AGENT_AUTH_FAILED`), `list_check_snapshots`, the real PR state, and the tracked chat's entry in
`get_chat_statuses {session_id}`; classify with `classifyChildLiveness` and route on its `action`.

- **Session state says nothing about pushes.** It moves when the daemon re-polls existing checks.
  The push oracle is the remote (`git rev-list --count origin/<base>..origin/<branch>`); see
  [`references/epic-driver.md`](references/epic-driver.md).
- **Settled** = the tracked chat is `IDLE` or `STOPPED` on two consecutive polls with no spinner.
  `WORKING`/`QUESTION`/`WAITING` are alive; `LIMITED` is the resume lane; `UNSPECIFIED` or
  unreadable is unknown. Never gate on timestamps or on the session-wide `get_session_statuses`
  aggregate. [`references/merge-recovery.md`](references/merge-recovery.md).

### 3c. Transitions

- **Green** — `READY_FOR_REVIEW`, DisplayStatus `Passing`, chat settled, PR **not a draft**, and no
  `do not merge` / partial marker in its title or body (a child that ended `PARTIAL` carries
  `do not merge — partial: <n>/<total> acceptance criteria` and a `(partial n/total)` title suffix) ⇒
  add to `greens`. If the ticket is not yet in the review state, move it there.
- **Green but draft**, or **passing while the chat is still working** ⇒ hold and re-poll.
- **Liveness first** — `classifyChildLiveness`: `alive/hold` re-poll; `environmental-death/resume`
  one resume; `agent-blocked/repair` a repair round; `wall-clock-expired/fail-isolate` fail-isolate
  (never repair); `unknown/investigate` read the chat's last message and `waiting_reason`, reclassify,
  else re-poll. Unknown never repairs.
- **Repair** (only on `agent-blocked/repair`; a `BLOCKED` session needs the chat's own concluding
  message to count). Check the lease with `classifyRepairLease`: `active` — someone is repairing,
  count the round and re-poll; `stalled` (repair active, head SHA and output frozen across two polls)
  — count it as exhausted and dispatch a fresh round, or fail-isolate at the cap with
  `frozen repair lease`; `none` — dispatch. Dispatch `/boss-repair watch` in a **new chat in the
  ticket's own session** (never `create_session`, which attaches to a live session without running
  the prompt):

  ```
  record_chat       {session_id, agent_session_id: <fresh uuidgen UUID>,
                     agent_name: agent, title: "[<TICKET>] Repair (round N)"}
  send_chat_message {agent_session_id: <same UUID>, wake_if_asleep: true,
                     submit: true, message: "/boss-repair watch"}
  ```

  Read the repair chat's terminal token (strip backticks, emphasis and labels):
  `inner-cap-exhausted` counts a round; `no-progress` means nothing changed; anything unreadable is
  `repair-unclassifiable` and counts. **Four rounds per ticket**, then fail-isolate. A conflicted
  green (`pr_mergeable=false`) needs a repair round, not a merge. Track the repair chat in place of
  the original.

- **Environmental death** (chat `LIMITED`, a usage-limit banner, or a transient 5xx as the last
  message; no spinner; frozen activity) ⇒ one `send_chat_message` (wake + submit) telling it to
  continue from committed state; no resume or a re-error within a cycle ⇒ fail-isolate.
- **Wall clock** — compare elapsed time against the budget from
  `epicChildWallClockMinutes(config)` (`epicDefaults.childWallClockMinutes`, default 360 minutes —
  children routinely run 2–4 h). Carry it in state, or re-resolve it with its own guard:

  ```bash
  BOSS_SKILLS_HOME="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}"
  if [ ! -d "$BOSS_SKILLS_HOME/boss-epic/toolbox" ]; then BOSS_SKILLS_HOME="$HOME/.codex/skills"; fi
  BOSS_EPIC_TOOLBOX="$BOSS_SKILLS_HOME/boss-epic/toolbox"; export BOSS_EPIC_TOOLBOX
  BOSS_EPIC_CHILD_WALL_CLOCK_MIN="$(node --input-type=module -e '
    import { pathToFileURL } from "node:url"
    const { loadSkillConfig, epicChildWallClockMinutes } = await import(
      pathToFileURL(process.env.BOSS_EPIC_TOOLBOX + "/skill-config.mjs").href)
    process.stdout.write(String(epicChildWallClockMinutes(loadSkillConfig({ cwd: process.cwd() }))))
  ')"
  test -n "$BOSS_EPIC_CHILD_WALL_CLOCK_MIN" || { echo "BLOCKED: child wall clock unresolved at 3c — BOSS_EPIC_TOOLBOX unresolved or skill-config.mjs missing from it (stale payload: run boss skills install), .boss-skills.json failed to load, or epicDefaults is malformed"; exit 1; }
  ```

  Expiry fail-isolates; it is a budget fact, never death evidence, and never repairs.

Never nudge a BLOCKED child in its original chat.

### 3d. Merge — one at a time

```bash
# inside the scheduling process:
#   const target = nextToMerge(greens, graph, merged)   // id or null
#   //   greens: [{id}, ...]   merged: Set of merged ids   (THREE args)
```

Re-check the target's external blockers at merge time:

```bash
# inside the scheduling process, for the nextToMerge target:
#   // freshly fetch each external blocker's current Linear state type, then:
#   const open = mergeBlockedExternalBlockers(graph.nodes.get(target), graph, {
#     clearedForMerge: new Set(assumeClearedAndMerge),   // ONLY the --and-merge override
#     blockerStateTypes,                                 // id → fresh Linear state type
#   })
#   // open.length > 0 → skip-with-note this cycle; do NOT merge past the gate.
```

Still-open blockers ⇒ skip this cycle with a progress note (`<TICKET> held: external blocker <ID>
still open`); only `--assume-cleared-and-merge` clears this gate. Otherwise:

1. `merge_session {id: <session_id>, confirm: true}` — `confirm` is mandatory.
2. Success ⇒ confirm `MERGED`, move the ticket to Done, fold it into `merged` (and
   `externallyCleared` for an adopted non-node child), then refresh the driver's checkout
   (`git fetch origin <base>` and `git rebase --no-fork-point FETCH_HEAD`; skip with a note if it is
   not on the base or is dirty).
3. `FailedPrecondition "PR is not passing"` or a conflict (a sibling merge invalidated it) ⇒ back to
   a repair round; the first such demotion gets one extra round outside the cap of four.
4. Any other error ⇒ re-read the PR's real state before calling it failed (`MERGED` means it landed:
   finish the bookkeeping, never re-merge). A rebase refusal (`MERGE_STRATEGY_INCOMPATIBLE`) means a
   merge commit on the branch. [`references/merge-recovery.md`](references/merge-recovery.md).

Late children will sit behind many merges; the daemon's opt-in proactive rebase and the
rebase-never-merge rule keep that a normal repair round, not a failure.

### 3e. Fail-isolate

Add the id to `failed`, **leave its session open** for a human (never `stop_session`), skip its
`transitiveDependents(graph, failed)` naming the failed ancestor, update the comment. One failure
never stops the run, stops a sibling, or tears down shared watches.

### 3f. Report every transition

After every launch, green, merge, repair, failure, skip or unpark, update the progress comment on
every root containing that ticket. Never post a per-event comment. A failed upsert is retried on the
next transition.

## Phase 4 — Final report

Call `assertEpicCanTerminate(state)` and let it throw while anything remains; then `transitionToDone`
(a final reconciliation, every eligible ticket merged / fail-isolated / skipped, a successful final
upsert, settled children's watches removed — a live fail-isolated child keeps its watches). Post the
final summary on each root and in the chat: **merged** (with PRs, in order), **failed-isolated**
(session, last status, reason), **skipped** (reason or failed ancestor), and **duration**. The parent
is left for a human to close.

**Notes** (skip when `BOSS_NOTES_SUPPRESSED=1`): discover `--role notes`; none ⇒ nothing. Roll
`notesSampleRate` once per run (reuse `NOTES_SAMPLED`); on a miss, stop. Otherwise write at most five
secret-free observations (≤ 8 KiB) to a temp `observations.md` and dispatch each extension
(instructions from `skillPath`, bounded by `BOSS_SKILL_EXTENSION_TIMEOUT_MS`) with
`{"role":"notes","core":"boss-epic","context":{"mode","core","outcome","repoId","observationPath"},"runTmp","outPath"}`;
validate with `--role notes`. Never fatal.

## The progress comment

Exactly one comment per root, first line `<!-- boss-epic-progress -->`, edited in place (list mode:
on the first ticket). Never hand-roll it: `buildEpicProgressState(state)` → (for several roots)
`projectProgressByParent({state, parentIds, childrenByParent})` → `renderProgressComment` →
`planProgressCommentUpsert({comments, marker, body})`, executed verbatim through the tracker adapter
(`toolbox/progress-comment.mjs`). Statuses map onto the six `PROGRESS_STATUSES` (queued → `pending`,
in flight or repairing → `building`). Its `run` block (`runId`, phase, status, `lastReconciledAt`,
next wake) and per-row session, chat, PR, liveness and watch state are what let a fresh driver
resume without the previous turn.

## Safety rails

- `merge_session` only with `confirm: true`, only in `nextToMerge` order, one at a time.
- **A PR is merge-eligible only when all four hold**: the daemon gate is `Passing`; the build chat
  settled with real changes; the PR is not a draft; no `do not merge` / partial marker. A PR number
  alone — especially an empty bootstrap draft — is never completion (boss-build adopts that branch
  and continues).
- Never `stop_session` a failed ticket.
- The scheduler's graph is eligible-only; done ids clear externally; tickets outside the set are
  never mutated or merged.
- A runner without readable chat status (non-`claude` agents) must hold or fail-isolate — never merge
  from session state and check snapshots alone.

## Setup

Plan the children first (planned, `agent-friendly`, a plan attachment — `boss-plan`). Then
`/boss-epic <TICKET>` (a parent id or URL), `/boss-epic <A> <B> …` (a list), or `--epic <A> --epic
<B>` (several roots), with optional `--parallel N`, `--agent <name>` and the `--assume-cleared*`
overrides.
