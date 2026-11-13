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
   bash <skill-dir>/toolbox/plan/boss-plan-run.sh node skill-extensions.mjs discover --core boss-plan --role plan-reviewer --json
   ```

   Pass `--role plan-reviewer` so a same-prefix extension that extends `boss-plan` but declares the
   wrong marker role (e.g. `role: lens`, or a typo) is rejected into `skipped` rather than returned
   for dispatch — dispatching a wrong-role add-on under the `plan-reviewer` envelope would run the
   wrong extension. A valid `role: draft` sibling is ignored here because Phase 2 owns draft
   discovery. For every `skipped` entry whose `deliberate` is `false`, record
   `extension <name>: skipped (<reason>)` — a same-prefix directory that fails discovery (a broken
   or half-written `x-boss-extension` marker, a wrong `role`, or a missing `SKILL.md`) is a
   misinstalled extension the degradation contract requires you to surface, not silently ignore.
   Key that on the entry's own `deliberate` field, never on the text of `reason`. A
   `deliberate: true` entry is a same-prefix skill that is not an extension of this core at all — a
   markerless helper, or one extending another core — and is never reported; warning about it would
   fire on every plan for as long as that skill exists. **Only when `extensions` is empty and no
   `skipped` entry is reportable** is this a true no-op: record nothing and proceed with the plan
   unchanged (the default path is untouched).

2. **Dispatch each extension in order** as a **fresh read-only subagent** (of type
   `general-purpose`), holding each dispatch in-turn as below, in headless mode too.
   The per-extension deadline is `BOSS_SKILL_EXTENSION_TIMEOUT_MS` (default `300000` ms).
   Phase 2 has already cleaned its sentinel context; interactive runs never created one.
   Before the first extension, create one context for this whole pass:

   ```bash
   bash <skill-dir>/toolbox/plan/boss-plan-run.sh sentinel-ctx
   ```

   Load the extension by **reading the descriptor's `skillPath` from disk** (`dir` is its directory),
   passing both `skillPath` and `dir` in the worker brief, and requiring relative extension resources
   to resolve from `dir`. Pass that `SKILL.md` content into the dispatch as the extension's instructions.
   Never load
   a discovered extension by its bare descriptor `name` through the Skill tool: extension skills are
   dispatched explicitly, never model-matched, so they SHOULD declare
   `disable-model-invocation: true`, and the Skill tool refuses such a skill.
   Before each dispatch, derive its envelope paths from the declared scratch families and create
   its scratch directory:

   ```bash
   bash <skill-dir>/toolbox/plan/boss-plan-run.sh ext-paths --run-scratch-id <RUN-SCRATCH-ID> --issue <ISSUE-ID> --name <extension-name>
   ```

   Immediately before each dispatch attempt, record its timestamp (retain it across wait re-arms):

   ```bash
   bash <skill-dir>/toolbox/plan/boss-plan-run.sh dispatch-mark --run-dir <RUN_DIR> --name ext-plan-reviewer-<name>
   ```

   Use the printed `runTmp` and `outPath` values in the `plan-reviewer` **invocation envelope**:

   ```json
   {
     "role": "plan-reviewer",
     "core": "boss-plan",
     "context": { "planPath": "<PLAN_PATH>", "ticket": "<id/title>" },
     "runTmp": "<printed runTmp>",
     "outPath": "<printed outPath>"
   }
   ```

   The declared forms are
   `.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.ext-plan-reviewer-<name>.tmp` (`runTmp`) and
   `.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.ext-plan-reviewer-<name>.json` (`outPath`).
   Never mint a separate temp directory for an extension; Phase 5's single removal of the run
   directory cleans up these paths.

   The heartbeat uses the declared family
   `.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.ext-plan-reviewer-<name>.heartbeat.json`.
   The orchestrator wraps the extension instructions in a worker brief that supplies the concrete
   `RUN_SENTINEL`, `RUN_DIR`, `RUN_ID`, `EXT_NAME`, `EXT_HEARTBEAT` and toolbox paths and requires:

   - Beat `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node bs-dispatch-await.mjs heartbeat "$EXT_HEARTBEAT"`
     at start and between long steps; only the worker writes this liveness signal.
   - After writing `outPath`, write the terminal sentinel:
     `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node bs-run-sentinel.mjs write "$RUN_DIR" "$RUN_ID" "ext-plan-reviewer-$EXT_NAME" ok`.
     If no envelope could be produced, write the same sentinel with kind `failed`.
   - Remain read-only: never edit the plan, index or HEAD. Only scratch and sentinel writes are
     permitted. The extension's own instructions and envelope contract are unchanged.

   **Hold the dispatch in-turn.** Re-arm this foreground call while the dispatch is open:

   ```bash
   bash <skill-dir>/toolbox/plan/boss-plan-run.sh dispatch-wait --run-dir <RUN_DIR> --run-id <RUN_ID> --name ext-plan-reviewer-<name> --heartbeat <EXT_HEARTBEAT> --leg-deadline --budget 590000
   ```

   A harness that cannot raise its tool timeout omits `--budget` (the default is `110000` ms).
   Follow the [headless dispatch exit table](headless-dispatch.md#hold-the-dispatch-in-turn-with-wait):

   | Exit                     | Next                                               |
   | ------------------------ | -------------------------------------------------- |
   | `98` or any unknown code | Re-arm; a tool interruption is not dispatch death. |
   | `2`                      | Fix the call, then re-arm.                         |
   | `0`, `96`, `97`          | Step 3, always validating `outPath`.               |

   A returned object arriving at any tool boundary also ends the hold and proceeds to step 3.
   Under Codex, `wait_agent` is the hold, followed by one `wait` call as in Phase 2.
   After the last extension has been validated and folded or skipped, clean this pass's context
   with `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node bs-run-sentinel.mjs cleanup "$RUN_DIR"`.

3. **Validate + fold or skip.** Validate `outPath` after every terminal exit, including `96`
   (timed out) and `97` (abandoned), or a returned object:

   ```bash
   bash <skill-dir>/toolbox/plan/boss-plan-run.sh node skill-extensions.mjs validate --role plan-reviewer --file <outPath>
   ```

   On a missing file or non-zero exit, record `extension <name>: skipped (<reason>)` and continue —
   never abort the run. Use `timed out after <ms>ms` on exit `96` (the configured leg timeout),
   `abandoned` on `97`, otherwise the validation failure. A valid envelope is folded even after
   `96` or `97`; sentinel kind and transport outcome never replace envelope validation.
   For a valid envelope, fold an item supplying a whole contract section by
   body file into both artifacts with `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node plan/plan-fold-section.mjs fold --heading <heading> --body-file <file> --plan <planPath> --description <descriptionSummary.path>`;
   it places the section by contract order and edits the description on disk, so no separate
   metadata refresh is needed. Fold other `plan-reviewer` `items`
   (`{ severity, section, title, detail }`) into the drafting agent's quality pass, and route
   genuinely-controversial forks into `## Open Questions` (which also drives the `agent-question`
   label per Phase 3). The orchestrator owns any edit to the plan file; extensions only produce
   findings.

   Correct every valid `refuted` finding in both artifacts before Phase 4; never attach a claim
   the reviewer disproved. When correction requires a genuine product fork, replace the refuted
   claim with an explicit `## Open Questions` decision; fold `gap` and `unverified` items into the
   quality pass as usual.

   **Recompute open questions after folding.** Phase 2's drafting subagent returns `openQuestions`
   before this phase runs. Append any new controversial fork to that list before Phase 4 so a
   now-non-empty list adds the `agent-question` label and the labels agree with the plan and saved
   description.

Extensions are read-only and non-fatal by contract: a missing, malformed, erroring, or timed-out
extension is a recorded skip, never an abort. See
[`docs/skills/extension-contract.md`](../../../../docs/skills/extension-contract.md) for the full
naming/discovery/ordering/envelope/degradation rules.
