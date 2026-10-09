# Child fan-out (an epic's `agent-plan` children)

Phase 1 sends a selected issue here when `plan-child-fanout.mjs route` says `fan-out`: at least one
live child carries the planning-queue label (`labelName(config, 'agentPlan')`). The parent is not
planned as a ticket. Each eligible child is planned by its own awaited **child-planner** subagent, at
most `--parallel` (1..8, default 4) at a time, and the parent gets one label/state write at the end.
`toolbox/plan-child-fanout.mjs` decides everything that can be computed — eligibility, order, which
child to launch or poll, legal outcomes, the parent write — and its ledger is the run's only fan-out
state. This file has two parts: the orchestrator, then the brief each child planner reads.

## Orchestrator

### 1. Confirm and initialise

Interactive: one `AskUserQuestion` — "plan these N children (M skipped) / plan the parent as one
ticket / cancel", with `init`'s order and skip reasons. "One ticket" returns to Phase 1's precheck as a
`single` route. Children are always planned headless (their forks become open questions plus
`agent-question`); headless runs skip the question.

```bash
BOSS_PLAN_ENV=; for d in "${BOSS_SKILLS_HOME:-}" "$HOME/.claude/skills" "$HOME/.codex/skills"; do if [ -f "$d/boss-plan/toolbox/boss-plan-env.sh" ]; then BOSS_PLAN_ENV="$d/boss-plan/toolbox/boss-plan-env.sh"; break; fi; done; [ -n "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
ROUTE=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.fanout-route.json"
LEDGER=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.fanout-ledger.json"
node "$BOSS_PLAN_TOOLBOX/plan-child-fanout.mjs" init "$ROUTE" "$LEDGER" --parallel <N> || exit 1
RUN_SENTINEL="$BOSS_PLAN_TOOLBOX/bs-run-sentinel.mjs"
RUN="$(node "$RUN_SENTINEL" make-ctx boss-plan)" || exit 1
printf 'RUN_ID=%s\nRUN_DIR=%s\n' "${RUN%%$'\t'*}" "${RUN#*$'\t'}"
```

`<N>` is the `parallel` the Phase 1 `args` verb printed. `init` prints the planning order, the skipped
children with reasons, and any warning (an existing `blockedBy` cycle falls back to priority order).
Substitute the printed `RUN_ID` / `RUN_DIR` literally below — every child shares this one sentinel
context, one sentinel name per child: `child-<CHILD-ID>`.

### 2. Launch, hold, settle — until `next` says `complete`

Run `node "$BOSS_PLAN_TOOLBOX/plan-child-fanout.mjs" next "$LEDGER"`. It prints
`{launch, poll, complete, inFlight, queued}` and stamps the polled child, so polls round-robin.

**Launch** every id in `launch`, each in this order:

1. Its scratch: the ledger child's `scratch` when set (a transport retry reuses every path), else a
   fresh `mktemp -d .linear-plans/run-XXXXXXXX`. Its suffix is the child's `<CHILD-SCRATCH-ID>`.
2. Its description snapshot, exactly as Phase 2 step 2 does for one ticket:
   `node "$BOSS_PLAN_TOOLBOX/tracker/cli.mjs" read-description --id <CHILD-UUID> --out-file .linear-plans/run-<CHILD-SCRATCH-ID>/<CHILD-ID>.image-guard-orig.md`
   (`uuid` from the ledger; exit 2 ⇒ byte-copy the hydrated `get_issue` description; exit 64 or a
   receipt for another id ⇒ `record <CHILD-ID> failed --reason snapshot` and skip the dispatch).
3. `printf '%s000' "$(date +%s)" > "<RUN_DIR>/child-<CHILD-ID>.dispatched-at"`, then
   `node "$BOSS_PLAN_TOOLBOX/plan-child-fanout.mjs" record "$LEDGER" <CHILD-ID> dispatched --scratch .linear-plans/run-<CHILD-SCRATCH-ID>`
   **before** dispatching a `general-purpose` subagent on the orchestrator's model with the
   [brief](#child-planner-brief), so a dispatch-tool error finds the child `dispatched` and earns its
   transport retry.

**Hold** the one id `next` returned as `poll` with a foreground re-arm (never end the turn while a
child is in flight — the Stop-hook hazard `headless-dispatch.md` documents):

```bash
BOSS_PLAN_ENV=; for d in "${BOSS_SKILLS_HOME:-}" "$HOME/.claude/skills" "$HOME/.codex/skills"; do if [ -f "$d/boss-plan/toolbox/boss-plan-env.sh" ]; then BOSS_PLAN_ENV="$d/boss-plan/toolbox/boss-plan-env.sh"; break; fi; done; [ -n "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
node "$BOSS_PLAN_TOOLBOX/bs-dispatch-await.mjs" wait "<RUN_DIR>" "<RUN_ID>" child-<CHILD-ID> \
  --heartbeat .linear-plans/run-<CHILD-SCRATCH-ID>/<CHILD-ID>.dispatch-heartbeat.json \
  --dispatched-at "$(cat "<RUN_DIR>/child-<CHILD-ID>.dispatched-at")" --budget 25000 --while-live
```

Route on its exit code with the `headless-dispatch.md` table: `98` or a harness kill ⇒ back to `next`;
`2` ⇒ fix the call; `0` / `96` / `97` ⇒ settle. A child's returned object can also arrive at any tool
boundary — settle that child then. A dispatch tool error with no sentinel is a **transport** death: run the **Settle** check below
first (the child's Phase 4 may have landed before the transport died) and record `planned` on `noop`;
otherwise `record <CHILD-ID> failed --reason transport` — the helper re-queues it once, and a second
death fails it. There is no inline fallback here: a failed child stays `agent-plan` and a re-run picks it up.

**Settle** on the tracker, not on the child's word. `get_issue` the child, write the payload to
`.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.child-<CHILD-ID>.precheck.json`, then
`node "$BOSS_PLAN_TOOLBOX/plan-run-guards.mjs" idempotence <that file> --selected-id <CHILD-ID>`.
Only `action: "noop"` is `record <CHILD-ID> planned`; anything else is
`record <CHILD-ID> failed --reason <the guard's reasons, comma-joined>` (prefix the `disposition`
reason when the sentinel never became publishable). Keep the child's returned `outcome` for cleanup.

### 3. Parent flip, report, clean up

When `next` says `complete`: `get_issue` the parent fresh into
`.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.precheck.json` and run
`node "$BOSS_PLAN_TOOLBOX/plan-child-fanout.mjs" parent-flip "$LEDGER" <that file>`. `flip: true` with
`write: true` ⇒ one `save_issue` carrying exactly `labels` (the whole merged set) and, when non-null,
`state` — never the description, estimate or attachments — then read it back; a mismatch is a
failed run (report it, exit non-zero, Phase 7 outcome `failed`) even when every child planned.
`flip: false` ⇒ no parent write; print its `reasons`.

Report (Phase 6) the `summary` verb's rows as a table — id, title, `planned` / `failed: <reason>` /
`skipped: <reason>` — plus the parent-flip result. Then clean up: `node "$RUN_SENTINEL" cleanup
"<RUN_DIR>"`, remove `.linear-plans/run-<RUN-SCRATCH-ID>`, and remove each failed child's scratch
unless that child returned `outcome: "drift"` (its scratch is then the only copy of the intended
bytes; name it in the report). A planned child's own Phase 5 already removed its scratch.

Exit non-zero when `summary` says `ok: false` or the parent read-back mismatched. Phase 6's hand-off
runs when any child was planned with `agent-build`; Phase 7's outcome is `planned` when `ok` and the
parent write (if any) verified, else `failed`.

## Child-planner brief

You plan **one** child of an epic, unattended, inside an awaited subagent the boss-plan orchestrator
dispatched. Never call `AskUserQuestion`.

**Inputs:** `CHILD-ID`, its title and UUID; your scratch directory
`.linear-plans/run-<CHILD-SCRATCH-ID>/` — use `<CHILD-SCRATCH-ID>` wherever the brief or SKILL.md says
`<RUN-SCRATCH-ID>` and `CHILD-ID` wherever they say `<ISSUE-ID>`; the snapshot path; `PLAN_PATH`;
`RUN_SENTINEL` / `RUN_DIR` / `RUN_ID` and your sentinel name `child-<CHILD-ID>`; `parentId`;
`siblingIds`; `earlierSiblings`; `plannedSiblings`.

1. **Draft** — [`headless-drafting-brief.md`](headless-drafting-brief.md) Steps 1–8 with
   `allowEpic: false` (beat `.linear-plans/run-<CHILD-SCRATCH-ID>/<CHILD-ID>.dispatch-heartbeat.json`;
   run draft extensions inline, never as a nested dispatch), **except** Step 8's `draft` sentinel
   write — that name is shared by every sibling.
2. **Validate** the Step 9 object you would have returned with SKILL.md Phase 2 step 5
   (`adopt-metadata`); a failure ends here with no tracker write.
3. **Write back** — SKILL.md Phase 3.5 inline, then Phase 4 for your child only. You hold Phase 4
   tracker-write authority for exactly this one issue; never write the parent or a sibling except the
   edges below. In step 4 pass every `siblingIds` entry to `fetch-candidates` as `--id`, and write a
   blocking edge to a sibling **only** if it is in `earlierSiblings` or `plannedSiblings` (already
   planned before this run, so no concurrent planner can write back); a dependency on a later sibling
   becomes a `## Planning` note plus an open question (and `agent-question`), because two concurrent
   siblings writing edges toward each other could form a cycle.
4. **Clean up** — SKILL.md Phase 5 for your scratch only (kept after a write-back `drift`). Skip
   Phases 6–7: no report, notes flush, hand-off or self-archive — the orchestrator owns those.
5. **Sentinel, last** — only when Phase 4 completed:

   ```bash
   node "$RUN_SENTINEL" write "$RUN_DIR" "$RUN_ID" child-<CHILD-ID> ok \
     "$(jq -nc --arg id "<CHILD-ID>" '{issueId:$id,outcome:"planned"}')"
   ```

   Write nothing on failure — an absent sentinel is the safe outcome.

Return only `{"issueId": "<CHILD-ID>", "outcome": "planned" | "failed" | "drift", "reason": "<one
line>"}`.
