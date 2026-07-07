# Phase 3.5 — Extension plan-reviewers (additive, non-fatal)

Read this when running the optional extension plan-reviewers pass (Phase 3.5 of `SKILL.md`), after
the plan is drafted (Phase 3) and before it is uploaded (Phase 4). This is the reference integration
of the shared **skill extension / add-on discovery system**
([`docs/skills/extension-contract.md`](../../../../docs/skills/extension-contract.md)).

It is **strictly additive**: when no `boss-plan-*` extension is installed (the default case today) it is
a documented no-op that leaves every existing drafting behaviour unchanged. It runs in **both** the
interactive and headless (`BOSS_CRON=true`) paths; in headless mode a discovered extension is still
dispatched as an awaited subagent (no human gating).

## Steps

1. **Discover.** Run the shared helper and read **both** `extensions` (ordered `(order, name)`) and
   `skipped`:

   ```bash
   node scripts/skill-extensions.mjs discover --core boss-plan --role plan-reviewer --json
   ```

   Pass `--role plan-reviewer` so a same-prefix extension that extends `boss-plan` but declares the
   wrong marker role (e.g. `role: lens`, or a typo) is rejected into `skipped` rather than returned
   for dispatch — dispatching a wrong-role add-on under the `plan-reviewer` envelope would run the
   wrong extension. The valid `boss-plan-draft` sibling is ignored here because Phase 2 owns draft
   discovery. For every entry in `skipped`, record `extension <name>: skipped (<reason>)` — a
   same-prefix directory that fails discovery (no `x-boss-extension` marker, wrong `extends`, wrong
   `role`, or missing `SKILL.md`) is a misinstalled extension the degradation contract requires you
   to surface, not silently ignore. **Only when _both_ `extensions` and `skipped` are empty** is this a true no-op:
   record nothing and proceed with the plan unchanged (the default path is untouched).

2. **Dispatch each extension in order** as a **fresh read-only subagent** (of type
   `general-purpose`; **await** it, **never** `run_in_background` — this holds in headless mode too).
   Bound each awaited dispatch with a per-extension timeout of
   `BOSS_SKILL_EXTENSION_TIMEOUT_MS` (default `300000` ms, mirroring `boss-review`'s
   `BOSS_CROSS_REVIEW_TIMEOUT_MS`) so one hung or never-returning extension cannot block the core
   run — in a headless `boss-plan` cron this is mandatory. On timeout expiry, record
   `extension <name>: skipped (timed out after <ms>ms)` and move to the next extension (the same
   skip path a missing/malformed/erroring envelope takes in Step 3); never abort the run.
   The subagent loads the extension's `SKILL.md` via the Skill tool and receives the `plan-reviewer`
   **invocation envelope** from the contract:

   ```json
   {
     "role": "plan-reviewer",
     "core": "boss-plan",
     "context": { "planPath": "<PLAN_PATH>", "ticket": "<id/title>" },
     "runTmp": "<scratch dir>",
     "outPath": "<file to write the result envelope>"
   }
   ```

   Extensions are **read-only** — they never edit the plan, index, or HEAD.

3. **Validate + fold or skip.** Validate each returned envelope:

   ```bash
   node scripts/skill-extensions.mjs validate --role plan-reviewer --file "<outPath>"
   ```

   On a missing file or non-zero exit, record `extension <name>: skipped (<reason>)` and continue —
   never abort the run. For a valid envelope, fold the `plan-reviewer` `items`
   (`{ severity, section, title, detail }`) into the drafting agent's quality pass, and route
   genuinely-controversial forks into `## Open Questions` (which also drives the `agent-question`
   label per Phase 3). The orchestrator owns any edit to the plan file; extensions only produce
   findings.

   **Recompute the Phase 4 metadata after folding.** Phase 2's drafting subagent returns
   `descriptionSummary`/`openQuestions` **before** this phase runs, and Phase 4 derives the Linear
   `description` and the `agent-question` label from that returned metadata — so a folded extension
   finding that changes the plan file would otherwise upload a plan whose `## Open Questions` (or
   other description-contract section) disagrees with the saved Linear description and labels. When a
   fold edits the plan file, update the returned metadata to match **before** Phase 4: append any new
   controversial fork to `openQuestions` (a now-non-empty list adds the `agent-question` label) and
   refresh the affected `## …` section(s) of `descriptionSummary` so the uploaded file, the Linear
   description, and the labels stay byte-consistent. A fold that only touches the drafting quality
   pass without changing a description-contract section needs no metadata change.

Extensions are read-only and non-fatal by contract: a missing, malformed, erroring, or timed-out
extension is a recorded skip, never an abort. See
[`docs/skills/extension-contract.md`](../../../../docs/skills/extension-contract.md) for the full
naming/discovery/ordering/envelope/degradation rules.
