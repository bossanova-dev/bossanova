# Interactive mode (read only on the interactive `boss-plan` path)

This reference holds the **interactive-exclusive** prose that the resident SKILL.md body points at:
the Phase 1 confirmation loop, the Phase 3 design-doc seed, and interactive draft resolution. A
headless (`BOSS_CRON=true`) run never reads this file — headless dispatches the drafting subagent instead
(`references/headless-drafting-brief.md`). The directives below are preserved **verbatim** from the
pre-split skill; a negative content assertion in `scripts/boss-plan-skill.test.mjs` pins them so
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
2.5 owns the guards and ordering discipline; the deterministic core is `scripts/plan-epic-lib.mjs`).
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

## Phase 3 — Seed a design doc (auto-skip office-hours; interactive only)

`plan-eng-review` offers to run `office-hours` only when it finds **no** design doc. Seed one so
that offer never fires (headless never invokes `plan-eng-review`, so this whole phase is
interactive-only):

1. Compute the slug + branch exactly the way `plan-eng-review` does:
   ```bash
   SLUG=$(~/.claude/skills/gstack/browse/bin/remote-slug 2>/dev/null || basename "$(git rev-parse --show-toplevel 2>/dev/null || pwd)")
   BRANCH=$(git rev-parse --abbrev-ref HEAD 2>/dev/null | tr '/' '-' || echo 'no-branch')
   USER=$(whoami)
   DATETIME=$(date +%Y%m%d-%H%M%S)
   DESIGN_DIR="$HOME/.gstack/projects/$SLUG"
   mkdir -p "$DESIGN_DIR"
   echo "$DESIGN_DIR/$USER-$BRANCH-design-$DATETIME.md"
   ```
   **Record the exact path this prints** — call it the design-doc path. Cleanup (below) removes that
   literal path. Do **not** reconstruct it later (the `$DATETIME` stamp is non-deterministic, so a
   fresh shell would compute a different name) and do **not** stash it in a shared file (a fixed
   pointer path collides between two runs in the same worktree). Your own context carries it across
   the fresh Bash shells; only the shell is fresh, not your memory of this path.
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

Run `node scripts/skill-extensions.mjs discover --core boss-plan --role draft --json` and read
both `extensions` and `skipped`; record every skip in the autonomous decisions.
If the helper is missing in an installed public skill payload, treat discovery as
`{"extensions":[],"skipped":[]}` so the portable fallback tiers still run.

- **Tier 1:** if one or more extensions are returned, load each discovered extension by its returned
  descriptor `name` via the Skill tool on the main thread, passing
  `{ role: "draft", core: "boss-plan", context: { mode: "interactive", planPath, ticket, designDoc },
runTmp, outPath }`. The extension owns the interview and writes the plan. Tiers 2 and 3 do not run.
- **Tier 2:** if no extension exists and the host exposes a native drafting command, such as
  Claude Code plan mode, delegate to it, then normalize the output to the planContract sections
  from `references/headless-drafting-brief.md` **Step 7**.
- **Tier 3:** if no extension and no host built-in exists, run the inline drafting prompt in
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
(`scripts/plan-image-guard.mjs`, Phase 4) aborts the Linear write if any source image is dropped.

## Interactive cleanup

In addition to the plan file, remove the design doc you seeded in Phase 3 (headless has no design
doc to remove):

```bash
rm -f "<the exact design-doc path you recorded in Phase 3>"
```

Substitute the **literal** design-doc path you recorded in Phase 3 — do not recompute it (the
non-deterministic `$DATETIME` stamp would yield a different filename and leave the doc behind) and
do not glob the timestamp (that would over-delete a concurrent same-user/branch run's doc). Removal
is best-effort: a missing file is fine.
