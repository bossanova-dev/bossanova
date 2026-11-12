# Child fan-out (an epic's `agent-plan` children)

Phase 1 sends a selected issue here when `plan-child-fanout.mjs route` says `fan-out`: at least one
live child carries the planning-queue label (`labelName(config, 'agentPlan')`). The parent is not
planned as a ticket. Each eligible child is planned by its own awaited **child-planner** subagent, at
most `--parallel` (1..8, default 4) at a time, and the parent gets one label/state write at the end.
`toolbox/plan/plan-child-fanout.mjs` decides everything that can be computed — eligibility, order, which
child to launch or poll, legal outcomes, the parent write — and its ledger is the run's only fan-out
state. This file has two parts: the orchestrator, then the brief each child planner reads.

## Orchestrator

### 1. Confirm and initialise

Interactive: one `AskUserQuestion` — "plan these N children (M skipped) / plan the parent as one
ticket / cancel", with `init`'s order and skip reasons. "One ticket" returns to Phase 1's precheck as a
`single` route. Children are always planned headless (their forks become open questions plus
`agent-question`); headless runs skip the question.

```bash
bash <skill-dir>/toolbox/plan/boss-plan-run.sh fanout-init --run-scratch .linear-plans/run-<RUN-SCRATCH-ID> --issue <ISSUE-ID> --parallel <N>
```

`<N>` is the `parallel` the Phase 1 `args` verb printed. `init` prints the planning order, the skipped
children with reasons, and any warning (an existing `blockedBy` cycle falls back to priority order).
Substitute the printed `RUN_ID` / `RUN_DIR` literally below — every child shares this one sentinel
context, one sentinel name per child: `child-<CHILD-ID>`.

### 2. Launch, hold, settle — until `next` says `complete`

Run `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node plan/plan-child-fanout.mjs next .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.fanout-ledger.json`. It prints
`{launch, poll, complete, inFlight, queued}` and stamps the polled child, so polls round-robin.

**Launch** every id in `launch`, each in this order:

1. Its scratch: the ledger child's `scratch` when set (a transport retry reuses every path), else a
   fresh `mktemp -d .linear-plans/run-XXXXXXXX`. Its suffix is the child's `<CHILD-SCRATCH-ID>`.
2. Its description snapshot, exactly as Phase 2 step 2 does for one ticket:
   `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node tracker/cli.mjs read-description --id <CHILD-UUID> --out-file .linear-plans/run-<CHILD-SCRATCH-ID>/<CHILD-ID>.image-guard-orig.md`
   (`uuid` from the ledger; exit 2 ⇒ byte-copy the hydrated `get_issue` description; exit 64 or a
   receipt for another id ⇒ `record <CHILD-ID> failed --reason snapshot` and skip the dispatch).
3. `bash <skill-dir>/toolbox/plan/boss-plan-run.sh dispatch-mark --run-dir <RUN_DIR> --name child-<CHILD-ID>`, then
   `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node plan/plan-child-fanout.mjs record .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.fanout-ledger.json <CHILD-ID> dispatched --scratch .linear-plans/run-<CHILD-SCRATCH-ID>`
   **before** dispatching a `general-purpose` subagent on the orchestrator's model with the
   [brief](#child-planner-brief) and Phase 0's `<RECON-REF>` as `RECON_REF`, so a dispatch-tool error finds the child `dispatched` and earns its
   transport retry.

**Hold** the one id `next` returned as `poll` with a foreground re-arm (never end the turn while a
child is in flight — the Stop-hook hazard `headless-dispatch.md` documents):

```bash
bash <skill-dir>/toolbox/plan/boss-plan-run.sh dispatch-wait --run-dir <RUN_DIR> --run-id <RUN_ID> --name child-<CHILD-ID> --heartbeat .linear-plans/run-<CHILD-SCRATCH-ID>/<CHILD-ID>.dispatch-heartbeat.json --budget 25000
```

Route on its exit code with the `headless-dispatch.md` table: `98`, `95` or a harness kill ⇒ back to `next`;
`2` ⇒ fix the call; `0` / `96` / `97` ⇒ settle. A child's returned object can also arrive at any tool
boundary — settle that child then. A dispatch tool error with no sentinel is a **transport** death: run the **Settle** check below
first (the child's Phase 4 may have landed before the transport died) and record `planned` on `noop`;
otherwise `record <CHILD-ID> failed --reason transport` — the helper re-queues it once, and a second
death fails it. There is no inline fallback here: a failed child stays `agent-plan` and a re-run picks it up.

**Settle** on the tracker, not on the child's word. Run
`bash <skill-dir>/toolbox/plan/boss-plan-run.sh node tracker/cli.mjs read-issue --id <CHILD-UUID> --out-file .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.child-<CHILD-ID>.precheck.json`.
Exit 0 continues; exit 2 uses the [snapshot fallback](planning-claims.md#claim-before-drafting);
exit 64 means fix the call. Then
`bash <skill-dir>/toolbox/plan/boss-plan-run.sh node plan/plan-run-guards.mjs idempotence <that file> --selected-id <CHILD-ID>`.
Only `action: "noop"` is `record <CHILD-ID> planned`; anything else is
`record <CHILD-ID> failed --reason <the guard's reasons, comma-joined>` (prefix the `disposition`
reason when the sentinel never became publishable). Keep the child's returned `outcome` for cleanup.

### 3. Parent flip, report, clean up

When `next` says `complete`: `get_issue` the parent fresh (parent-flip needs labels) into
`.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.precheck.json` and run
`bash <skill-dir>/toolbox/plan/boss-plan-run.sh node plan/plan-child-fanout.mjs parent-flip .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.fanout-ledger.json <that file>`. `flip: true` with
`write: true` ⇒ one `save_issue` carrying exactly `labels` (the whole merged set) and, when non-null,
`state` — never the description, estimate or attachments — then read it back; a mismatch is a
failed run (report it, exit non-zero, Phase 7 outcome `failed`) even when every child planned.
`flip: false` ⇒ no parent write; print its `reasons`.

Report (Phase 6) the `summary` verb's rows as a table — id, title, `planned` / `failed: <reason>` /
`skipped: <reason>` — plus the parent-flip result. Then clean up: `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node bs-run-sentinel.mjs cleanup
"<RUN_DIR>"`, remove `.linear-plans/run-<RUN-SCRATCH-ID>`, and remove each failed child's scratch
unless that child returned `outcome: "drift"` (its scratch is then the only copy of the intended
bytes; name it in the report). A planned child's own Phase 5 already removed its scratch.

Exit non-zero when `summary` says `ok: false` or the parent read-back mismatched. Phase 7's outcome
is `planned` when `ok` and the parent write (if any) verified, else `failed`.

## Child-planner brief

You plan **one** child of an epic, unattended, inside an awaited subagent the boss-plan orchestrator
dispatched. Never call `AskUserQuestion`.

**Inputs:** `RECON_REF` (the orchestrator's Phase 0 ref); `CHILD-ID`, its title and UUID; your scratch directory
`.linear-plans/run-<CHILD-SCRATCH-ID>/` — use `<CHILD-SCRATCH-ID>` wherever the brief or SKILL.md says
`<RUN-SCRATCH-ID>` and `CHILD-ID` wherever they say `<ISSUE-ID>`; the snapshot path; `PLAN_PATH`, computed with `plan-slug.mjs plan-path` from the child run id
and precheck payload (use exactly as given);
`RUN_SENTINEL` / `RUN_DIR` / `RUN_ID` and your sentinel name `child-<CHILD-ID>`; `parentId`;
`siblingIds`; `earlierSiblings`; `plannedSiblings`.

0. **Select and claim the child** — run
   `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node tracker/cli.mjs read-issue --id <CHILD-UUID> --out-file .linear-plans/run-<CHILD-SCRATCH-ID>/<CHILD-ID>.precheck.json` before drafting.
   Exit 0 continues; exit 2 uses the [snapshot fallback](planning-claims.md#claim-before-drafting);
   exit 64 means fix the call. Keep that
   selection snapshot unchanged through both rechecks; the description snapshot is a separate
   artifact. Set `PRECHECK` to this path and follow
   [`planning-claims.md`](planning-claims.md#claim-before-drafting) for this child, using its own claim receipt under the child scratch
   (never the parent's). On `LOST`, release and return
   `{"issueId":"<CHILD-ID>","outcome":"failed","reason":"peer-claimed"}` without drafting;
   the orchestrator's Settle check decides whether the peer finished planning. A repeated
   `NO_WINNER` also returns failed after release. Unsupported plan scope continues unclaimed as
   that protocol specifies. Release this child's claim on **every** return or abort, before
   scratch removal, including metadata-validation failure and write-back drift: children skip
   Phase 6, so its terminal release will not run for them.
1. **Draft** — [`headless-drafting-brief.md`](headless-drafting-brief.md) Steps 1–8 with
   `allowEpic: false` and `allowSiblingSplit` as SKILL.md Phase 2 step 3 sets it (beat `.linear-plans/run-<CHILD-SCRATCH-ID>/<CHILD-ID>.dispatch-heartbeat.json`;
   run draft extensions inline, never as a nested dispatch), **except** run Step 8’s `draft-verify` with `--verify-only`: it runs all guards and omits the
   `draft` sentinel, whose name is shared by every sibling.
2. **Recheck and validate** — run the fresh planning recheck against the child selection snapshot
   before adopting metadata. A stop releases the child claim and returns failed with the guard's
   reasons and no planning write; the orchestrator still settles on tracker state. Validate the Step 9 object you would have returned with SKILL.md Phase 2 step 5
   (`adopt-metadata`); a failure ends here with no tracker write.
3. **Write back** — SKILL.md Phase 3.5 inline, then Phase 4 for your child only. You hold Phase 4
   tracker-write authority for exactly this one issue; never write the parent or a sibling except the
   edges below, or new sibling shells of a split ([`sibling-split.md`](sibling-split.md)). In step 4 pass every `siblingIds` entry to `fetch-candidates` as `--id`, and write a
   blocking edge to a sibling **only** if it is in `earlierSiblings` or `plannedSiblings` (already
   planned before this run, so no concurrent planner can write back); a dependency on a later sibling
   becomes a `## Planning` note plus an open question (and `agent-question`), because two concurrent
   siblings writing edges toward each other could form a cycle.
4. **Clean up** — SKILL.md Phase 5 for your scratch only (kept after a write-back `drift`). Skip
   Phases 6–7: no report, notes flush or self-archive — the orchestrator owns those.
5. **Sentinel, last** — only when Phase 4 completed:

   ```bash
   bash <skill-dir>/toolbox/plan/boss-plan-run.sh node bs-run-sentinel.mjs write <RUN_DIR> <RUN_ID> child-<CHILD-ID> ok '{"issueId":"<CHILD-ID>","outcome":"planned"}'
   ```

   Write nothing on failure — an absent sentinel is the safe outcome.

Return only `{"issueId": "<CHILD-ID>", "outcome": "planned" | "failed" | "drift", "reason": "<one
line>"}`.
