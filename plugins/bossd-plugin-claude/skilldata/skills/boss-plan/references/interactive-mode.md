# Interactive mode (read only on the interactive `boss-plan` path)

This reference holds the **interactive-exclusive** prose that the resident SKILL.md body points at:
the Phase 1 confirmation loop, the Phase 3 design-doc seed, and interactive draft resolution. A
headless (`BOSS_CRON=true`) run never reads this file — headless dispatches the drafting subagent instead
(`references/headless-drafting-brief.md`). The directives below are preserved **verbatim** from the
pre-split skill; a negative content assertion in `scripts/boss-plan-skill.test.mjs` pins them so
interactive behaviour cannot silently drift.

## Phase 1 — Confirm the selection (interactive only)

The resident body ranks the Unplanned queue. Interactively, before spending a long interview on the
wrong ticket, confirm the head of the ranked queue:

- Show the issue at the head of the ranked queue (`id`, `title`, `priority`, full `description`) and
  confirm via AskUserQuestion: **plan this one** / **skip this one (use the next ticket)** / **pick
  a different one** / **cancel**. This guards against spending a long interview on the wrong ticket.
  - **skip this one** → drop this issue from consideration and show the **next** issue in the ranked
    queue, then ask again. Repeat for each skip. Skipping does not modify the issue in Linear (it
    stays Unplanned); it only advances your local selection. If the queue runs out, report that
    every Unplanned ticket was skipped and stop.
  - **pick a different one** → ask which ticket (ID or "show me the list") and select that one
    instead.

When the user gave a ticket ID and it is already `Todo`/`In Progress`/`Done`/`Canceled`, warn and
ask (AskUserQuestion) whether to re-plan before continuing.

## Phase 2 — Triage triviality (interactive)

Read the title + description and classify — this sets how deep the interview and plan go, and
informs the estimate:

- **TRIVIAL** — copy/doc tweak, a single obvious one-liner, no design decisions (e.g. "Mention setup
  scripts on the home page"). Use a short interview and a lightweight plan.
- **SUBSTANTIAL** — anything with design choices, multiple files, or unknowns.

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
