# Sibling split

A ticket that already has a parent is never decomposed: its children would be grandchildren that
`boss-epic` never schedules ([`epic.md`](epic.md)). When such a ticket is honestly ≥ 5 and not
atomic, the planner splits it into **siblings under the same parent** instead, which creates no
grandchild. `toolbox/plan/plan-sibling-split.mjs` owns every computable part: `normalizeSiblings`,
`siblingSplitPlan`, `siblingBody`, `siblingMarker`, `parseSiblingMarker`, `siblingEdges`,
`verifySiblings` and `SIBLING_MAX`.

## When

All of these hold, or the ticket keeps the old shape (estimate 5, `- Oversized-child:` with the
suggested split, `needs-human`):

- the ticket has a parent;
- its honest size is ≥ 5 and it is not atomic (an atomic 5 keeps `- Atomic-5:`);
- the orchestrator passed `allowSiblingSplit: true`. It passes it only for a parented issue when
  `node "$BOSS_PLAN_TOOLBOX/tracker/cli.mjs" operations --require createIssue,appendDependency`
  exits 0, on the single-ticket route and to each fan-out child planner. A child drafted inside
  [`epic.md`](epic.md) build step 2 never gets it: that decomposition is already being shaped;
- the other parts number 1 to `SIBLING_MAX` (5). More means the source was epic-sized: use
  `- Oversized-child:`.

## The drafter's contract

- Plan **this** ticket as the first part only: estimate ≤ 3, a plan attachment that covers only
  that part, and `agentBuild` by the usual judgement.
- Under `## Planning`, add `- Sibling-split: <why it is not atomic; this ticket is part a; the other
parts by key and title>`. Never together with `- Oversized-child:`.
- Return the other parts as Step 9's `siblings` ([`headless-drafting-brief.md`](headless-drafting-brief.md)).
  `blockedBy` names `source` (this ticket) or another part's key and must be acyclic; `blocks` lists
  existing issue ids that depend on that part (their edge on this ticket stays).

`plan-run-guards.mjs adopt-metadata` refuses the whole draft when the split disagrees with itself:
invalid `siblings`, a source estimate above 3, a missing `- Sibling-split:` bullet, both bullets, or
the bullet without `siblings`. Dropping a part would silently lose scope.

## Phase 4

Step 2 holds `needs-human` as the exposure label while siblings are pending, whatever `agentBuild`
says; the description already names every part, so the worst case is today's oversized-child state.
After write-back verification (step 5), file the siblings:

1. Write `.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.siblings-in.json` as
   `{sourceId, sourceTitle, parentId, siblings, existingChildren}`: `parentId` spelled as step 5's
   read-back reports the parent, `siblings` from the adopted metadata, `existingChildren` the
   parent's live children enumerated as Phase 1's route does (`listChildren`, or `list_issues` then
   `get_issue` each, since list descriptions truncate).
2. Plan the creates. Exit 1 names a truncated description (re-read that child) or two children
   claiming one key (a human keeps one); stop there with `needs-human` kept.

   ```bash
   BOSS_PLAN_ENV=; for d in "${BOSS_SKILLS_HOME:-}" "$HOME/.claude/skills" "$HOME/.codex/skills"; do if [ -f "$d/boss-plan/toolbox/plan/boss-plan-env.sh" ]; then BOSS_PLAN_ENV="$d/boss-plan/toolbox/plan/boss-plan-env.sh"; break; fi; done; [ -n "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
   SIBLINGS_IN=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.siblings-in.json"
   node "$BOSS_PLAN_TOOLBOX/plan/plan-sibling-split.mjs" plan --input "$SIBLINGS_IN" --out-dir .linear-plans/run-<RUN-SCRATCH-ID>
   ```

   It prints `{creates: [{key, title, estimate, bodyFile}], reused: {key: id}}`, `creates` in
   dependency order. A child already carrying the part's marker is reused, so a re-run never
   duplicates a sibling.

3. Create each entry of `creates`, in order, as an unplanned shell under the same parent that
   carries the planning-queue label, so the normal sweep plans it later with its own recon:

   ```bash
   BOSS_PLAN_ENV=; for d in "${BOSS_SKILLS_HOME:-}" "$HOME/.claude/skills" "$HOME/.codex/skills"; do if [ -f "$d/boss-plan/toolbox/plan/boss-plan-env.sh" ]; then BOSS_PLAN_ENV="$d/boss-plan/toolbox/plan/boss-plan-env.sh"; break; fi; done; [ -n "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
   node "$BOSS_PLAN_TOOLBOX/tracker/cli.mjs" create-issue --title "<title>" \
     --body-file .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.sibling-<key>.md \
     --state-role unplanned --label-role agentPlan --label "<content label>" --parent "<PARENT-ID>"
   ```

   Repeat `--label` for each of the source's content labels. Execute an emitted `{tool, args}`
   descriptor; for `operation-executed` use its `result` and make no MCP call. Then
   `setPriorityEstimate` the new issue with the part's `estimate` and the source's priority. Write
   every key's id, created and reused, to
   `.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.siblings-created.json` as `{key: id}`.

4. Wire the edges with `appendDependency`, each `{issue, blockedBy}` meaning `issue` is blocked by
   `blockedBy`. They are additive; never delete a relation. As in step 4e, read both sides before
   each write and skip one that would form a cycle, then re-read both sides and record a missing edge
   rather than re-writing it.

   ```bash
   BOSS_PLAN_ENV=; for d in "${BOSS_SKILLS_HOME:-}" "$HOME/.claude/skills" "$HOME/.codex/skills"; do if [ -f "$d/boss-plan/toolbox/plan/boss-plan-env.sh" ]; then BOSS_PLAN_ENV="$d/boss-plan/toolbox/plan/boss-plan-env.sh"; break; fi; done; [ -n "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
   SIBLINGS_IN=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.siblings-in.json"
   SIBLINGS_CREATED=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.siblings-created.json"
   node "$BOSS_PLAN_TOOLBOX/plan/plan-sibling-split.mjs" edges --input "$SIBLINGS_IN" --created "$SIBLINGS_CREATED"
   ```

5. Read every sibling back (`get_issue`, or `listChildren` on the parent), write
   `.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.siblings-readback.json` as `{key: issue}` (`null`
   for a sibling the read did not find), and verify that each exists, sits under the parent and
   carries its marker:

   ```bash
   BOSS_PLAN_ENV=; for d in "${BOSS_SKILLS_HOME:-}" "$HOME/.claude/skills" "$HOME/.codex/skills"; do if [ -f "$d/boss-plan/toolbox/plan/boss-plan-env.sh" ]; then BOSS_PLAN_ENV="$d/boss-plan/toolbox/plan/boss-plan-env.sh"; break; fi; done; [ -n "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
   SIBLINGS_IN=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.siblings-in.json"
   SIBLINGS_READBACK=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.siblings-readback.json"
   node "$BOSS_PLAN_TOOLBOX/plan/plan-sibling-split.mjs" verify --input "$SIBLINGS_IN" --read-back "$SIBLINGS_READBACK"
   ```

6. Only on exit 0, and only when the drafter's `agentBuild` was true, flip the source's exposure
   label `needs-human` to `agent-build` in a labels-only write (read, merge, write), then read it
   back.

Any failure, at any point, leaves `needs-human` on the source. Never loop, retry a create blind, or
delete a sibling. The run report (Phase 6) names every sibling id that exists, created or reused,
the `verify` problems, and whether the exposure flipped.

## Fan-out child planners

A child planner ([`child-fanout.md`](child-fanout.md)) may create new sibling shells under the
parent and write edges to and from them, each behind step 4e's cycle check: nobody else knows the
new shells yet, so no concurrent planner can write an opposing edge. Every other rule in its brief
stands.
