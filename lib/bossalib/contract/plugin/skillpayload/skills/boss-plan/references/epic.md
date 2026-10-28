# Epic decomposition

The single source for turning one ticket into a tracker **parent + N fully-planned children** wired by
an intra-epic `blockedBy` DAG — the shape `boss-epic` consumes. Headless, the drafting subagent does
every tracker write here; interactive, a human approves the shape (`interactive-mode.md`) and the
orchestrator does the writes. The deterministic core is unit-tested — never re-derive it by hand:
`toolbox/plan/plan-epic-lib.mjs` (`validateDecomposition`, `normalizeDecomposition`, `validateLayering`,
`assertAcyclic`, `topoOrderChildren`, `epicWiringPlan`, `epicParentEstimate`, `stableChildKey`,
`epicChildMarker`, `serializeEpicSpec`, `parseEpicSpec`, `validateSpecIdentity`,
`specAttachmentFilename`, `specAttachmentTitle`, `reconcileEpicChildren`) and
`toolbox/plan/plan-epic-phase25.mjs` (`detectEpicParent`, `epicSpecRecoveryGate`,
`stalePlanAttachmentSweep`, `epicPhase25WritePlan`).

## When

Triage is **EPIC** when the honest estimate is ≥ 5 or the work spans several independently
shippable PRs, with at least `EPIC_MIN_CHILDREN` (2) genuinely separable pieces. A single ticket is
estimated only 0/1/2/3; an honest 5 that is truly atomic stays one ticket with a `- Atomic-5:`
justification under `## Planning`; an 8 is never a single ticket. A child of an epic is drafted with
`allowEpic: false` and is never decomposed again (depth 1); a non-atomic honest ≥ 5 child is planned
as estimate 5 with `- Oversized-child:` (why, and the sibling split) and `needs-human`.

Guards: each child ≤ `CHILD_MAX_ESTIMATE` (3); at most `EPIC_MAX_CHILDREN` (12). A cycle or dangling
`blockedByKeys` reference ⇒ plan it as one ticket and record why. Too big to split into ≤ 12 children
of ≤ 3 ⇒ `needs-human` ("too large to auto-plan"), never one oversized ticket.

## Preconditions

- The source must be **unplanned** and must not itself have a `parentId` (decomposing a child mints
  grandchildren `boss-epic` never schedules — plan it as one ticket, `- Oversized-child:` if non-atomic ≥ 5).
- An explicitly named source in another state: classify with `detectEpicParent(issue)` over one
  `get_issue` payload (attachments **and** description). `isEpicParent` ⇒ it is an existing epic: go to
  [Resume](#resume), never a single-ticket plan. `ambiguous` (two or more `Epic spec (…)` attachments)
  ⇒ abort loudly; a human deletes all but one. Neither store ⇒ plan it as one ticket.
- Resolve the storage route from [`plan-storage.md`](plan-storage.md) **before the first epic write**.
  The attachment route requires `deletePlanAttachment`, as before. The single-record route requires
  `tracker/cli.mjs operations --require writePlan,readPlanAttachment,writeEpicSpec,createIssue,listChildren,appendDependency`
  and a successful `tracker/cli.mjs invoke-operation preflight` (including native sub-issue and
  dependency support). A missing operation, missing preflight or failed preflight means
  `unsupported tracker for epic decomposition`: make no epic write.

## The spec

`{ parentId, parent: {title, goal, keyChanges[], priority}, children: [{key, title, goal,
keyChanges[], blockedByKeys[], estimate, priority, layer, agentBuild, openQuestions[]}] }`

- Decompose along seams, producer before consumer (`contract → persistence → producer → read → ui`);
  a read/ui child is `blockedBy` the producer that writes its rows. `validateLayering` warnings are
  advisory.
- `parentId` is the source ticket's id — required; an unbound spec can never pass
  `validateSpecIdentity`. Keys come from `stableChildKey` so a retry re-derives them identically.
- On the attachment route it is stored as a native attachment: filename `epic-spec.json`, MIME `application/json`, title
  `Epic spec (<ISSUE-ID>)` (never starting with `Implementation plan`, which `boss-epic` reads as the
  plan), body `serializeEpicSpec(spec)`. It carries metadata only, never plan bodies. Identity is
  `validateSpecIdentity(spec, <ISSUE-ID>)`, never the title alone. A legacy parent may instead carry
  the spec as an inline `<!-- boss-plan-epic-spec:… -->` description marker; `parseEpicSpec` reads
  both, and that marker must be carried verbatim through any later description save.
- On the single-record route, `writeEpicSpec` stores the serialized spec once in a separate trusted
  comment beginning `<!-- boss-epic-spec v1 -->`, exposed as `Epic spec (<ISSUE-ID>)` with
  `application/json` content. Identical bytes are unchanged; different bytes or duplicate trusted
  records are refused. Read it through `readPlanAttachment` (`read-plan`) and validate its identity.
  Never produce an inline description marker on this route, and never rewrite an existing issue body.

## Build it — validate everything before the first write

1. `validateDecomposition` + `assertAcyclic` on the spec.
2. Fully plan every child locally (`allowEpic: false`) — a plan file plus a description per child —
   and copy each child plan's `agentBuild` verdict and `openQuestions` back onto its spec entry
   (`serializeEpicSpec` defaults a missing `agentBuild` to `true` and derives `agent-question`
   from `openQuestions`). Re-run `validateDecomposition` on the completed spec. Run the secret and
   image-parity gates on every child.
3. Interactive only: confirm (create this epic / plan as one ticket / cancel).
4. Before the **first tracker write** (including the label strip), run the
   [fresh planning recheck](planning-claims.md#recheck-at-write-boundaries) on the parent against
   Phase 1's immutable selection payload and parent claim. Exit 3 takes its stop branch without
   any epic write. The parent claim remains held throughout decomposition. Execute `epicPhase25WritePlan({parentId, spec, unplannedState, staleAttachmentIds, labelsToStrip, storage})`
   with `storage: 'attachment'` (default) or `'single-record'`; omit `staleAttachmentIds` for
   single-record storage (supplying ids is an error). Execute its ops **in emitted order**, minus skipped stages (and, on a resume, minus every child
   `reconcileEpicChildren` does not report `missing`):
   - **Stage 1, label strip:** read the parent's labels and save them minus `agent-build` /
     `needs-human` (`save_issue` `labels` replaces the whole set). From this first write on, the
     parent is not `boss-build`-selectable.
   - **Stage 2, spec store, exactly once:** if either store already holds a spec, skip stages 2 and
     3 and resume against the stored spec. Otherwise write `serializeEpicSpec(spec)` to
     `.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.epic-spec.json`, check
     `validateSpecIdentity(parseEpicSpec(<file>), <ISSUE-ID>)`. On the attachment route, upload it the way
     [`plan-storage.md`](plan-storage.md) uploads a plan, and **read it back** before any child is
     created. Retain the exact finalized spec attachment id as this run's own parent upload;
     pass it as `--own-attachment-id` on later parent rechecks, including the overview prepare.
     Keep Phase 1's original snapshot unchanged so a peer's different attachment id still stops
     the run. A failed attachment read-back here may delete the orphan row (zero children exist yet).
     On the single-record route execute the emitted `writeEpicSpec` op through
     `invoke-operation writeEpicSpec` with `{issue, file, title}`. Read its record back through
     `readPlanAttachment` before creating any child; retain its attachment-shaped id for later parent
     rechecks. The spec comment is append-only: do not PATCH or delete it on a retry.
   - **Stage 3, stale strip:** delete the ids `stalePlanAttachmentSweep(attachments)` returns
     (prefix-scoped to `Implementation plan (…)`, so the spec survives). Single-record mode emits
     no stale-delete stage; skip attachment deletion entirely.
   - **Children:** in `topoOrderChildren` order, create each as an **unplanned, unexposed shell** —
     `parentId`, the config-resolved unplanned state, the child's `estimate` and `priority`, content
     labels (plus `agent-question` when it has open questions), and its description with
     `epicChildMarker(key)` placed **before** `## Original notes`; never `agent-build` /
     `needs-human` yet. On the single-record route map each emitted `createChild` op to
     `tracker/cli.mjs create-issue --parent <parent>` with the shell's title, description, state and
     content labels, then apply `setPriorityEstimate`. Validate all supplied labels before creation.
     The GitHub adapter creates the body once, links the native sub-issue, verifies that link, and
     only then applies its state label. A failed or unverified link leaves an inert issue with no
     workflow-state label; report the orphan number and stop without exposure. Rename its local plan to
     `.linear-plans/run-<RUN-SCRATCH-ID>/<PARENT>-child-<key>-<slug>.md` (`<slug>` is
     `issueSlug(child-id, child-title)`). Before plan storage, save the shell's bare
     `getIssue` result in its own child scratch as `<CHILD-ID>.precheck.json` and claim the child
     through `planning-claims.md`; recheck against that snapshot, never the parent's, and release
     the child claim on every success or abort. Store it titled exactly `Implementation plan (<child id>)`
     through the selected route (attachment upload or `writePlan`), read it back through
     `readPlanAttachment`, then move the shell to
     planned.
5. Wire the intra-epic DAG from `epicWiringPlan(spec, createdIdByKey)` (include the reserved `parent`
   entry). Intra-epic edges come only from there.
6. **Commit the parent overview before exposing anything.** Re-assert the parent's unplanned state
   (the tracker's sub-issue rollup can move it), compose the overview — `## Summary`,
   `## Child tickets`, `## Planning`, verbatim `## Original notes` — in
   `.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.epic-overview.md`, run the secret, image-parity and
   `plan-contract-guard.mjs --mode epic-parent` gates. On the attachment route attach it and read it
   back, re-assert unplanned, and save it as the parent description (still unplanned). On the
   single-record route write it with `writePlan` on the parent, titled
   `Implementation plan (<ISSUE-ID>)`, read it back with `readPlanAttachment`, and re-assert unplanned;
   skip the description save. An overview exceeding the comment limit fails closed. Then link each child against the
   **non-epic** active backlog (SKILL.md Phase 4 step 5, excluding this epic's own ids), and only then
   expose each child with its own call: `agent-build`, or `needs-human` when its plan said so.
7. **Flip the parent last:** union the `epic` label (`labelName(config, 'epic')`), drop
   `agent-build` / `needs-human` and the planning-queue label, delete the ids
   `stalePlanAttachmentSweep(attachments, {keepAttachmentId: <overview id>})` returns on the attachment
   route only, and move it to
   planned with `estimate = epicParentEstimate(spec)` and the parent's priority. Retry without the
   estimate if it is rejected; re-read and warn if the stored estimate differs (Linear clamps silently).

Any failure takes the safe branch at the point it happens: abort without exposure and without the
planned flip. Because the parent stays unplanned until step 7, the next unplanned sweep re-picks a
partial epic and resumes it.

## Resume

A re-picked parent recovers the **original** spec — never a fresh decomposition. Read the
`Epic spec (<ISSUE-ID>)` attachment body (decode with `plan-attachment.mjs decode`), or on the
single-record route read its record through `readPlanAttachment` / `read-plan` into a file.
Run `parseEpicSpec` on those bytes and check `validateSpecIdentity` (stored-record sources only; a legacy inline spec is accepted as is).
An unreadable or unbindable spec goes to `epicSpecRecoveryGate({parent, children, plannedState,
epicLabel})`, which only ever answers `noop` or `abort` — never fall through to a single-ticket plan.

When `tracker/cli.mjs operations` lists `listChildren`, enumerate children with
`tracker/cli.mjs invoke-operation listChildren --args-file <file>` using `{parentId}` and use its
hydrated `children` as the per-child payloads (no separate per-child read). Otherwise enumerate with
`list_issues parentId=<parent> limit=250`, then `get_issue includeRelations=true` each child (list
descriptions truncate the marker). Print any `relationWarnings` in the route summary; they never
stop the run. Join with `reconcileEpicChildren(spec, children)`:

- **aligned** — create exactly what `missing` names, drafted from the persisted metadata;
- **one rename** (`repairs`) — on single-record storage, write nothing, report the required marker
  repair and stop with the parent unplanned for a human: marker-rename repair is unsupported because
  it rewrites an existing child body. On the attachment route, rewrite that child's marker to `epicChildMarker(specKey)`, preserving the
  rest of its description byte for byte; never re-point the spec key;
- **ambiguous** (`ok: false`) — write nothing, report `errors`. A refusal is never "no children".
  An unmarked hand-filed sub-issue is ignored while `missing` is empty.

An adopted shell without its canonical `Implementation plan (<child id>)` attachment is always
redrafted and attached before it is exposed. An adopted unexposed child takes its exposure label from
the spec's `agentBuild`. On single-record storage recover the saved parent overview through
`readPlanAttachment` on its `Implementation plan (…)` record; reuse those exact bytes. On attachment
storage, if the parent description is already the saved overview, reuse it verbatim
(never recompose `## Original notes` from it), re-assert unplanned, run the external links, then
finish exposure and the flip. On a fully built epic this is a clean no-op.

## What the orchestrator re-verifies (headless)

The terminal sentinel's payload for an epic carries `epic: true`, `epicParentId`, `childIds`,
`childPlanPaths` (id → plan path), `epicSpecPaths`, `guardScratchPaths` (each child's
`image-guard-orig`, `attachment-guard-orig` and `image-guard-new` files) and `premises`. For the
orchestrator's write-back check, leave the exact bytes of each description's **last** save on disk:
the parent overview in `<ISSUE-ID>.epic-overview.md`, each child's in
`<ISSUE-ID>.child-<CHILD-ID>.image-guard-new.md` (re-written after any later save; for an adopted
child this run did not rewrite, its stored description via `tracker/cli.mjs read-description`, or
byte-for-byte from `getIssue` when `readDescription` is absent). On single-record storage the parent
artifact is the overview record's bytes, while child artifacts are their creation-time descriptions.
