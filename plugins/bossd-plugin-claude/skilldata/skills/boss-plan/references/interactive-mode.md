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
   decomposed). Run the secret + image-parity gates on each child plan before any write.
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

Run `node "$BOSS_PLAN_TOOLBOX/skill-extensions.mjs" discover --core boss-plan --role draft --json` and read
both `extensions` and `skipped`; record every skip in the autonomous decisions.
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
  Claude Code plan mode, delegate to it, then normalize the output to the planContract sections
  from `references/headless-drafting-brief.md` **Step 7**.
- **Tier 3:** if no extension succeeded under the draft success predicate and no host built-in exists, run the inline drafting prompt in
  Phase 5: work the review dimensions, follow the shared Step 5/Step 7 sections, and write a
  planContract-compliant plan with no external skill dependency.

If the ticket is TRIVIAL, keep your interview answers and follow-ups proportionate; do not
manufacture complexity.

## Phase 5 — Tier 3 inline drafting prompt

Use only when Phase 4 reaches Tier 3. Work these review dimensions yourself and carry each decision
into the plan: scope challenge, architecture, code quality, tests, performance, and outside-voice.
Follow the resident **## Phase 3 — Plan requirements** section in SKILL.md plus the shared drafting
details in `references/headless-drafting-brief.md` **Step 5** and **Step 7** (plan-body requirements
and the description summary template). Write to `.linear-plans/<ISSUE-ID>-<slug>.md` and stop after
saving the plan file. Do not continue into subagent-driven-development or executing-plans.

**Preserve every image reference VERBATIM** (all interactive tiers). When composing `## Original
notes`, copy every image reference the ticket carried — inline markdown `![alt](…)`, HTML `<img …>`
tags, and bare `uploads.linear.app`/attachment URLs — byte-for-byte, URLs intact. **Never** replace
an image with a `[screenshot: …]` text placeholder or any paraphrase: Linear does not expose
description history, so the rewritten description is the only surviving copy of those URLs (a prior
screenshot-dropping data-loss incident). You MAY additionally list them under a `## Screenshots` bullet list in the plan
body, but the URLs must stay intact in `## Original notes`. The orchestrator's mechanical guard
(`$BOSS_PLAN_TOOLBOX/plan-image-guard.mjs`, Phase 4) aborts the Linear write if any source image is dropped.

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
