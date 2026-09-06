# Interactive mode (read only on the interactive `boss-plan` path)

This reference holds the **interactive-exclusive** prose that the resident SKILL.md body points at:
the Phase 1 confirmation loop, the Phase 3 design-doc seed, and interactive draft resolution. A
headless (`BOSS_CRON=true`) run never reads this file — headless dispatches the drafting subagent instead
(`references/headless-drafting-brief.md`). The directives below are preserved **verbatim** from the
pre-split skill; a negative content assertion in the skill-contract test suite pins them so
interactive behaviour cannot silently drift.

## Phase 1 — Confirm the selection (interactive only)

The resident body ranks the unplanned queue. Interactively, before spending a long interview on the
wrong ticket, confirm the head of the ranked queue:

- Show the issue at the head of the ranked queue (`id`, `title`, `priority`, full `description`) and
  confirm via AskUserQuestion: **plan this one** / **skip this one (use the next ticket)** / **pick
  a different one** / **cancel**. This guards against spending a long interview on the wrong ticket.
  - **skip this one** → drop this issue from consideration and show the **next** issue in the ranked
    queue, then ask again. Repeat for each skip. Skipping does not modify the issue in Linear (it
    stays unplanned); it only advances your local selection. If the queue runs out, report that
    every unplanned ticket was skipped and stop.
  - **pick a different one** → ask which ticket (ID or "show me the list") and select that one
    instead.

When the user gave a ticket ID and it is already planned/in-progress/`Done`/`Canceled`, warn and
ask (AskUserQuestion) whether to re-plan before continuing.

## Phase 2 — Triage triviality (interactive)

Read the title + description and compute an honest estimate, which drives the classification — this
sets how deep the interview and plan go:

- **TRIVIAL** — copy/doc tweak, a single obvious one-liner, no design decisions (e.g. "Mention setup
  scripts on the home page"). Use a short interview and a lightweight plan.
- **SUBSTANTIAL** — anything with design choices, multiple files, or unknowns.
- **EPIC** — the honest Fibonacci estimate is **≥ 5**, or the work otherwise spans **multiple
  independently-shippable PRs**, each a coherent deliverable reviewable and mergeable on its own.
  **Estimate is the forcing function:** a single ticket may be estimated only `0/1/2/3`; an honest `5`
  is EPIC unless genuinely atomic & un-splittable (then it stays one ticket with a recorded
  `- Atomic-5:` justification); an `8` is **never** a single-ticket estimate. An epic still requires
  **≥ 2** genuinely separable children — if the honest estimate is `≤ 3` and you cannot articulate ≥ 2
  independent PR-sized pieces it is `SUBSTANTIAL`, not EPIC. When EPIC, run the decomposition flow
  below instead of a single plan.

## Phase 2.5 — Epic decomposition (interactive: propose → confirm → create)

When triage is EPIC, decompose the ticket into a parent + N fully-planned children (SKILL.md Phase
2.5 owns the guards and ordering discipline; the deterministic core is `$BOSS_PLAN_TOOLBOX/plan-epic-lib.mjs`).
Interactively:

1. **Draft the decomposition spec** — decompose along architectural seams, producer-before-consumer
   (`contract → persistence → producer → read → ui`), tagging each child's `layer`, keeping every
   child estimate **≤ `CHILD_MAX_ESTIMATE`=3**, and setting each `read`/`ui` child `blockedBy` its
   `producer` (or an external upstream when the producer already exists in the merged tree). Validate
   locally (`validateDecomposition` + `assertAcyclic`), then run `validateLayering` (advisory
   producer-before-consumer warnings — confirm or fix each, they never block). A **drafting-bug**
   failure (a `blockedByKeys` cycle, dangling refs) → re-ask the user to reshape it or fall back to a
   single `SUBSTANTIAL` plan. A **size** failure (more than `EPIC_MAX_CHILDREN`=12 children, a child
   stuck above the estimate ceiling, or an honest ≥ 5 that will not separate into ≥ 2 PR-sized
   children) → surface it as too-large (mark `needs-human` / split by hand), **never** a single
   oversized ticket.
2. **Fully plan every child locally** through the normal draft path with the child as a synthetic
   ticket, passing `allowEpic: false` in the context (the recursion guard — a child is never itself
   decomposed). Two dispatch shapes produce those plans, and both end at the same per-child gates:

   - **Per-child dispatch (the default).** One draft dispatch per child, each resolving its own tier
     through Phase 4 exactly as a single ticket does.
   - **Batch dispatch.** ONE drafting subagent drafts every child, the way the headless epic path
     already does. Select it when the approved spec has **≥ `BATCH_DRAFT_MIN_CHILDREN`=3** children,
     or whenever the operator asks for it at any child count. Below that threshold the default
     stands: a two-child epic saves one dispatch and gives up per-child tier fallback for it. See
     Phase 4's **Batch child drafting** for the dispatch and its output contract.

   Whichever shape drafted them, the per-child gates are unchanged and stay orchestrator-side: for
   every child run the plan-contract guard, the image-parity guard, and the secret gate before any
   write. Batching changes who writes the drafts, not what is validated.

3. **Confirm before any Linear write** via `AskUserQuestion`, presenting the parent goal, the N
   child titles/goals, and the DAG edges. Offer exactly: **create this epic** / **plan as one ticket
   instead** / **cancel**. `AskUserQuestion` is approve/adjust/cancel only — there is no per-child
   editing; cancel + re-run to iterate.
4. On **create this epic**, publish + create children in `topoOrderChildren` order, wire the DAG via
   `epicWiringPlan`, add external conflict links, and repurpose the original ticket as the epic
   parent (SKILL.md Phase 2.5 steps 4–7). The repurpose is **last** — SKILL.md step 7's
   unplanned → planned flip under the parent-label exception (**neither** `agent-friendly` **nor**
   `needs-human`, **stripping** any pre-existing build label + stale single-ticket `Implementation
plan (…)` link a previously-planned ticket carried, so the epic parent isn't `boss-build`-
   selectable); do not stop after wiring/exposure and leave the parent unplanned, or the next
   boss-plan sweep will keep re-selecting the already-created epic container. Re-running on a
   partially-built parent **adopts** existing children rather than duplicating them.

## Phase 3 — Seed a design doc in this run's private scratch (interactive only)

A drafting layer that finds no design doc typically opens its own discovery interview from scratch.
Seed one so the Phase 4 draft extension starts from the ticket's own starting material instead
(headless never reads it — the drafting brief carries the ticket itself — so this whole phase is
interactive-only).

The seed lives in **this run's own private scratch**, never in a third-party tool's directory and
never at a user-global or otherwise shared path: a published core runs in arbitrary projects on
arbitrary machines, so it may only write where it owns the ground.

1. Create the run scratch and compute the design-doc path inside it:
   ```bash
   ISSUE_ID="${ISSUE_ID:?set to the id of the ticket you are planning}"
   RUN_TMP=$(mktemp -d "${TMPDIR:-/tmp}/boss-plan-run.XXXXXX")
   DESIGN_DIR="$RUN_TMP/design"
   mkdir -p "$DESIGN_DIR"
   echo "$RUN_TMP"
   echo "$DESIGN_DIR/$ISSUE_ID-design.md"
   ```
   Substitute the ticket's own id for `${ISSUE_ID:?…}` (or export `ISSUE_ID` first) — a fresh Bash
   shell carries no value for it, so a verbatim copy aborts on that guard rather than seeding.
   `RUN_TMP` is the `runTmp` you pass to every Phase 4 dispatch; `mktemp -d` makes it unique per run,
   so two concurrent runs in the same worktree never collide. **Record the exact paths this prints**
   — call them the run scratch and the design-doc path. Cleanup (below) removes that literal scratch
   directory. Do **not** reconstruct either path later (the `mktemp` suffix is non-deterministic, so
   a fresh shell would compute a different name) and do **not** stash it in a shared file (a fixed
   pointer path collides between two runs in the same worktree). Your own context carries it across
   the fresh Bash shells; only the shell is fresh, not your memory of these paths.
2. Write a design doc to that path using the Write tool. Body:

   ```markdown
   # Design: <ISSUE-ID> <title>

   Source: Linear issue <ISSUE-ID> (<url>)
   Triage: <TRIVIAL|SUBSTANTIAL>

   ## Starting material

   The ONLY starting material is this Linear issue title and the description below.
   Interview the user to flesh out the problem, approach, edge cases, and tests.
   Scale interview + plan depth to the triage classification above.

   ## Title

   <title>

   ## Description (verbatim, may be empty)

   <description or "(none provided)">
   ```

## Phase 4 — Resolve the draft/review step (interactive only)

Run `node "$BOSS_PLAN_TOOLBOX/skill-extensions.mjs" discover --core boss-plan --role draft --json`
after running the toolbox preamble first, and read both `extensions` and `skipped`; record every
`skipped` entry whose `deliberate` is `false` as
`extension <name>: skipped (<reason>)` in the autonomous decisions. Key that on the entry's own
`deliberate` field, never on the text of `reason`. A `deliberate: true` entry is a same-prefix skill
that is not an extension of this core — a markerless helper, or one extending another core — and is
never reported. Recording is all that is due: a discovery skip is never fatal and never changes
control flow; the tiers below still resolve exactly as documented.
If the helper is missing in an installed public skill payload, treat discovery as
`{"extensions":[],"skipped":[]}` so the portable fallback tiers still run.

- **Tier 1:** if one or more extensions are returned, dispatch each on the main thread, passing
  `{ role: "draft", core: "boss-plan", context: { mode: "interactive", planPath, ticket, designDoc },
runTmp, outPath }`. Load the extension by **reading the descriptor's `skillPath` from disk**
  (`dir` is its directory), passing both `skillPath` and `dir` in the worker brief, and requiring
  relative extension resources to resolve from `dir`. Pass that `SKILL.md` content into the dispatch
  as the extension's instructions. Never load a discovered extension by its bare descriptor `name` through the Skill tool:
  extension skills are dispatched explicitly, never model-matched, so they SHOULD declare
  `disable-model-invocation: true`, and the Skill tool refuses such a skill.
  The extension owns the interview and writes the plan.

  **Per-dispatch plan target.** The `planPath` you pass is **not** the shared
  `.linear-plans/<ISSUE-ID>-<slug>.md` plan target: give each dispatch its own path under `runTmp`
  (`<runTmp>/draft-<extension-name>/<ISSUE-ID>-<slug>.md`), unique to the dispatch you are about to
  classify, and create its parent directory before dispatching. You promote the winner yourself:
  copy the file produced by the **first** dispatch that succeeded under the predicate below to
  `.linear-plans/<ISSUE-ID>-<slug>.md`, which is the plan target every later phase reads and the one
  tiers 2 and 3 write directly. A later sibling never overwrites a promoted plan.

  **Draft success predicate** — one definition, used by every tier gate below. A dispatched draft
  extension **succeeded** only when both of these hold: it returned a result envelope valid for the
  requested dispatch, **AND** the requested plan now exists and is non-empty at the `planPath` you
  passed **this** dispatch, **written by this dispatch**. Verify
  the second conjunct yourself by reading that `planPath` after the dispatch returns; a valid envelope
  that wrote no plan did **not** succeed. The per-dispatch target is what makes that read an
  attribution: hand every sibling the **same** `planPath` and "a plan is there now"
  is a test of shared state, not of this extension, so the first extension to
  write a plan silently credits every sibling dispatched after it, which is the false success this
  predicate exists to prevent. Do not try to rescue a shared path by comparing it before and after
  instead — neither half of that comparison holds across arbitrary projects and filesystems, which is
  where these skills run. Identical bytes are the ordinary output of a deterministic redraft, so a
  byte comparison records a real dispatch as a skip and drops the run to a lower tier that overwrites
  its plan; and the modification time need not advance either, because a filesystem whose timestamp
  resolution is coarser than the rewrite stamps both writes the same. Attribution has to come from
  the target you chose. Anything else is a failed dispatch: record
  `extension <name>: skipped (<reason>)` for that extension as you classify it — every failed
  dispatch is recorded, including when a sibling succeeded, so the ledger shows the whole Tier-1
  outcome and not just the winner.
  If at least one extension succeeded under the draft success predicate, tiers 2 and 3 do not run.
  If **no** extension succeeded under the draft success predicate — whether it failed to load,
  returned no valid envelope, or returned a valid envelope without producing the plan —
  fall through to Tier 2, then
  Tier 3 — the drafting layer is never silently dropped.

- **Tier 2:** if no extension succeeded under the draft success predicate and the host exposes a native drafting command, such as
  Claude Code plan mode, delegate to it, preserve the resulting plan structure while adding the
  shared Step 5 plan-body requirements, and compose a separate `descriptionSummary` from
  `references/headless-drafting-brief.md` **Step 7**.
- **Tier 3:** if no extension succeeded under the draft success predicate and no host built-in exists, run the inline drafting prompt in
  Phase 5: work the review dimensions, follow the shared Step 5 plan-body requirements, and write a
  plan plus its separate Step 7 description projection with no external skill dependency.

If the ticket is TRIVIAL, keep your interview answers and follow-ups proportionate; do not
manufacture complexity.

### Batch child drafting (epic runs only)

Phase 2.5 step 2 selects this shape when the approved spec has **≥ `BATCH_DRAFT_MIN_CHILDREN`=3**
children, or whenever the operator asks. Dispatch **one** drafting subagent for the whole epic and
await it, resolving the tier for that single dispatch exactly as above — Tier 1 with a discovered
draft extension, else Tier 2, else Tier 3.

Brief the worker with the **same** shared drafting spec the headless epic path uses: point it at
`references/headless-drafting-brief.md` **Steps 5–7** for the plan body, and at that file's "Epic
decompose-and-auto-create" **step 2** for the per-child drafting rules (`allowEpic: false`, and the
`agentFriendly` + `openQuestions` copy-back onto each spec entry). Do not restate the plan-body spec
here. That brief is the single normative source for it, and this path already links rather than
duplicates it; a second copy is a second thing to keep in sync, and the copy that drifts is the one
nobody is reading when the contract changes.

**What differs from headless: the worker writes files, never the tracker.** The human approved a
_shape_, not the drafts, so Phase 2.5 step 3's confirmation and step 4's create-and-wire stay
orchestrator-owned. Give the worker the run scratch and require it to write, for each child `key` in
the approved spec:

- `<runTmp>/batch-draft/<key>.md` — that child's full plan document.
- `<runTmp>/batch-draft/<key>.description.md` — that child's ticket description.
- `<runTmp>/batch-draft/<PARENT-ISSUE-ID>.batch-metadata.json` — ONE bounded file for the whole
  batch: `{ "parentId": "<ISSUE-ID>", "children": { "<key>": { … } } }`, keyed by spec key. Each
  child entry is the ordinary bounded draft-metadata object minus the two fields that are files
  above — so `agentFriendly`, `estimate`, `priority`, `openQuestions`, `labels`. The descriptions
  stay out of the JSON on purpose: this file is read whole, and inlining N descriptions is what makes
  a bounded artifact unbounded in exactly the runs that have the most children.

**Per-child validation reuses the existing gate — do not write a second one.** Every command below
dereferences `$BOSS_PLAN_TOOLBOX`, so run each in a block that begins with the toolbox preamble —
each Bash call is a fresh shell, and an unset `$BOSS_PLAN_TOOLBOX` turns these gates into
module-not-found errors at the one step whose whole point is that they run. For each child, compose
its draft-metadata object from its `children[<key>]` entry plus `planPath` (its plan file) and
`descriptionSummary` (its description file's contents), write that to a scratch file, and run
`node "$BOSS_PLAN_TOOLBOX/plan-run-guards.mjs" metadata <file>` — the same bounded-metadata guard the
single-ticket path runs, so an unknown key, a non-boolean `agentFriendly` or a non-single-ticket
estimate fails identically here. Then run the plan-contract guard
(`node "$BOSS_PLAN_TOOLBOX/plan-contract-guard.mjs" --description <desc> --plan <plan>`), the image
guard (`node "$BOSS_PLAN_TOOLBOX/plan-image-guard.mjs" --require-verbatim …`), and the secret gate,
per child, **before the first tracker write**. One dispatch drafting ten plans is precisely where a
late plan degrades, so these are the gates that must not be batched.

**Batch success predicate.** The dispatch succeeded only when all three hold: it returned a result
envelope valid for the requested dispatch, **AND** every child key in the approved spec has a
non-empty plan and description at its path above, **AND** the metadata file parses with an entry for
every one of those keys. A partial batch is a **failed** dispatch, not a partial success: discard it,
fall back to per-child dispatch for the whole epic, and record
`batch draft: fell back to per-child (<reason>)` in the autonomous decisions. Never create children
from a partial batch — a child with no plan becomes a ticket with no plan, and the DAG makes every
dependent of that child unbuildable behind it.

## Phase 5 — Tier 3 inline drafting prompt

Use only when Phase 4 reaches Tier 3. Work these review dimensions yourself and carry each decision
into the plan: scope challenge, architecture, code quality, tests, performance, and outside-voice.
Follow the resident **## Phase 3 — Plan requirements** section in SKILL.md plus the shared drafting
details in `references/headless-drafting-brief.md` **Step 5** and **Step 7** (plan-body requirements
and the description summary template). Write to `.linear-plans/<ISSUE-ID>-<slug>.md` and stop after
saving the plan file. Do not continue into subagent-driven-development or executing-plans.
The single-ticket plan file must retain the shared plan-file floor from that brief: required
description-contract headings, `## Problem Frame`, `## Requirements`, `## Implementation Units`, and
at least one heading outside `planContract.sections`. Epic-parent overviews and adopted-child
redrafts use explicit exemption reasons; consumers do not require this structure.

**Preserve `## Original notes` VERBATIM** (all interactive tiers). When composing
`## Original notes`, copy the ticket's prior description byte-for-byte from
`DESCRIPTION_SNAPSHOT_PATH`; do not retype, summarize, or reconstruct it. Every image reference the
ticket carried — inline markdown `![alt](…)`, HTML `<img …>` tags, and bare
`uploads.linear.app`/attachment URLs — must survive byte-for-byte, URLs intact except for required
upload-signature stripping. **Never** replace an image with a `[screenshot: …]` text placeholder or
any paraphrase: Linear does not expose description history, so the rewritten description is the only
surviving copy of those URLs (a prior screenshot-dropping data-loss incident). You MAY additionally
list them under a `## Screenshots` bullet list in the plan body, but the URLs must stay intact in
`## Original notes`. The orchestrator's mechanical guard
(`$BOSS_PLAN_TOOLBOX/plan-image-guard.mjs`, Phase 4) aborts the Linear write if any source image is
dropped or the block is not verbatim.

## Interactive cleanup

In addition to the plan file, remove the run scratch you created in Phase 3 — it holds the seeded
design doc and every per-dispatch draft target (headless has no design doc to remove):

```bash
rm -rf "<the exact run-scratch path you recorded in Phase 3>"
```

Substitute the **literal** run-scratch path you recorded in Phase 3 — do not recompute it (the
non-deterministic `mktemp` suffix would yield a different directory and leave the seed behind) and
do not glob the suffix (that would over-delete a concurrent run's scratch). Removal is best-effort:
a missing directory is fine.

A dispatched draft extension may also produce artifacts of its own **outside** the scratch you
handed it. That is the extension's own cleanup obligation, stated in the extension contract; the
core neither guesses at those paths nor leaves them standing — if such an artifact survives a
dispatch you can see, record it in the autonomous decisions.
