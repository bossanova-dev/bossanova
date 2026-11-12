---
name: boss-plan
description: Plan a tracker backlog ticket. Grabs the next unplanned issue by priority (or a ticket ID you provide), resolves drafting through boss-plan draft extensions or portable fallbacks, attaches the plan natively to the tracker issue, then writes a summary, labels, Fibonacci estimate, and priority before moving it from the unplanned to the planned state. Interactive by default; runs fully headless when BOSS_UNATTENDED=true (or legacy BOSS_CRON=true).
---

# boss-plan

Turn a vague tracker ticket into a **planned** ticket with an implementation-ready plan attached. Use
when asked to "plan a tracker ticket", "plan the next ticket", "boss-plan", or given a ticket ID. You
are a capable engineer: this document states what a planned ticket must look like, the helpers that
answer questions reliably, and the gates that protect the reporter's original notes and keep secrets
out of the tracker. How you research and write the plan is up to you.

**Modes.** Interactive by default (a draft extension may ask questions). With `BOSS_UNATTENDED=true`
or its legacy spelling `BOSS_CRON=true` — bossd sets both on every unattended run, including any
prompt-carrying `boss new`, so never fake the mode in prompt text — it is fully headless: **never**
call `AskUserQuestion`, dispatch one awaited drafting subagent, decide every fork yourself, and
record the controversial ones as open questions.

## What a planned ticket is

- **A plan attachment**, or the one marked plan comment on a tracker declaring `writePlan`, titled
  exactly `Implementation plan (<ISSUE-ID>)` — free-form Markdown, no
  required headings, ending in `## Original notes` (the ticket as it read before planning, verbatim).
  It is the plan; nothing is committed to the repo.
- **A short description** that points at the plan rather than copying it: `## Summary`,
  `## Key changes` (the paths the change touches — dependency linking reads them), `## Why this needs
a human` and `## Open Questions` only when they apply, `## Planning` (with a `- Contract: v1`
  stamp, dependency notes, any `- Atomic-5:` justification), and `## Original notes` verbatim. The
  drafting brief has the template. On a `writePlan` tracker this description is composed and gated
  locally; the stored issue description stays unchanged.
- **Labels**: the existing set, plus content labels that genuinely apply (`bug`, `feature`,
  `improvement`, `docs` — names via `optionalLabelName(config, '<role>')`, else the literal), plus
  exactly one of `agent-build` (the default) or `needs-human` (only when an agent genuinely could
  not implement it; size and a predicted verify park are never the reason, and the plan explains
  why), plus `agent-question`
  when there are open questions (headless only). The planning-queue label (`agent-plan`) is removed.
  Pipeline label roles resolve through `labelName(config, '<role>')`, whose keys are: `agentBuild`, `needsHuman`, `agentPlan`, `agentQuestion`, `epic`
  (it throws on an unknown role). Never create labels.
- **Estimate** (Fibonacci): 0 trivial; 1/2/3 one PR; 5/8 means it is an **epic**
  ([`references/epic.md`](references/epic.md)) unless a 5 is genuinely atomic.
- **Priority** 1–4 (`1=Urgent … 4=Low, 0=None`): keep a reporter-set priority, otherwise rank it
  against the planned backlog (security biases up). Not left at None.
- **State**: moved from the configured unplanned state to the planned state.
- **Dependencies**: `blockedBy` edges to tickets it genuinely conflicts with (Phase 4 step 5).

## Workspace facts

Load the config once — `loadSkillConfig({cwd})` (synchronous, options object) → `config`;
`trackerConfigFor(config)` gives the tracker server, team (else the run's resolved team, Phase 0),
workspace (never a project filter) and the state roles `unplanned`, `planned`, `inProgress`, `inReview`. Reach the tracker only through the
resolved adapter. A blocker counts as cleared only when its state type is completed or canceled
(`plan/plan-deps-lib.mjs`); boss-build will not start a ticket with an uncleared blocker.

Each capability is an operation in the resolved adapter's map. `tracker/cli.mjs operations` lists
its transport. An `mcp` operation is the named tool, called through `resolvedServer`; a `command`
operation is run with `tracker/cli.mjs invoke-operation <op> --args-file <file>`, whose `result` is
the answer. A descriptor verb (`create-issue`, `update-comment`, `write-description`) that prints
`outcome: operation-executed` has already performed the write: do not make an MCP call for it.

## Helpers

Run helpers and named steps with the runner from the directory this `SKILL.md` was loaded from
(`<skill-dir>`). Each call is one line; never wrap it in `bash -c` or write it to a temporary script.
The runner locates its own toolbox, independent of the current directory or other installed trees.

```bash
bash <skill-dir>/toolbox/plan/boss-plan-run.sh node <helper.mjs> [args …]
bash <skill-dir>/toolbox/plan/boss-plan-run.sh <step> [--flag value …]
```

Blocks that still begin with the old preamble are being migrated; run them as written in a bash tool shell.

| Question                                     | Ask                                                                                   |
| -------------------------------------------- | ------------------------------------------------------------------------------------- |
| Which named steps are available?             | `bash <skill-dir>/toolbox/plan/boss-plan-run.sh steps`                                |
| Is this repo configured for planning?        | `resolveTrackerTeam` → `isConfiguredForPlanning(config, {team})` (`skill-config.mjs`) |
| Write the complete issue snapshot            | `node tracker/cli.mjs read-issue --id <id> --out-file <path>`                         |
| Is the ticket already planned?               | `node plan/plan-run-guards.mjs idempotence <payload> --selected-id <id>`              |
| Read / write the description exactly         | `node tracker/cli.mjs read-description \| write-description …`                        |
| Upload / read / delete the plan attachment   | [`references/plan-storage.md`](references/plan-storage.md)                            |
| Normalise the drafter's metadata             | `node plan/plan-run-guards.mjs adopt-metadata <file> <json>`                          |
| Did a secret slip in?                        | `node plan/plan-secret-scan.mjs <files>`                                              |
| Did the reporter's images and notes survive? | `node plan/plan-image-guard.mjs …`                                                    |
| Is the description a plan description?       | `node plan/plan-contract-guard.mjs --description … --plan …`                          |
| Have referenced tickets moved since recon?   | `node plan/plan-run-guards.mjs premises …`                                            |
| Which tickets does this conflict with?       | `planDependencyEdges` in `plan/plan-deps-lib.mjs` (Phase 4 step 5)                    |
| Did the description land as written?         | `node plan/plan-writeback-verify.mjs --intended … --stored … --read-receipt …`        |
| What scratch names may I use?                | `node plan/plan-scratch-paths.mjs families`                                           |
| Is a tracker failure safe to retry?          | `node tracker/cli.mjs classify-outcome --observed "<err>" --operation read\|write`    |
| Plan an epic's `agent-plan` children?        | `node plan/plan-child-fanout.mjs route …` (`references/child-fanout.md`)              |

## Rules

1. **Never lose the reporter's notes.** `## Original notes` is the stored description, verbatim, in
   both the plan and the description; the image guard proves it. The tracker keeps no description
   history, so never overwrite a description you cannot verify.
2. **Keep secrets out.** Plans are visible to everyone with access to the issue. Never write tokens,
   keys, passwords, connection strings, session cookies, internal hostnames/IPs or customer PII —
   reference where a value lives instead (`[REDACTED: repo-root .env]`). The secret gate is mandatory.
3. **Validate before writing.** Every gate runs before the first tracker write it protects; a failed
   gate writes nothing and exits non-zero.
4. **Scratch lives in this run's directory only.** `.linear-plans/` is shared by concurrent runs:
   write only inside `.linear-plans/run-<RUN-SCRATCH-ID>/`, under names `plan-scratch-paths.mjs`
   declares, and delete only that directory. Never clean up by issue-id pattern (two runs can plan
   the same ticket) or tidy a peer's files.
5. **Leave nothing behind**, on every terminal path — except after a write-back `drift` verdict,
   where the scratch is the only copy of the intended bytes.
6. **Labels merge.** `save_issue` `labels` replaces the whole set: read, then write existing ∪
   additions ∖ strips.

## Phase 0 — Preflight

```bash
bash <skill-dir>/toolbox/plan/boss-plan-run.sh preflight
```

`<RUN-SCRATCH-ID>` is the suffix of the printed `run scratch:` directory; substitute it literally
everywhere below (it is not the sentinel `RUN_ID`). An `unconfigured` repo is a clean no-op (exit 0);
`ready` and `needs-team` continue. A present but broken config aborts.

Measure checkout freshness before the drift report:

```bash
bash <skill-dir>/toolbox/plan/boss-plan-run.sh node plan/plan-checkout-freshness.mjs check
```

Carry the JSON `reconRef` as `<RECON-REF>`; substitute it literally like `<RUN-SCRATCH-ID>`.
On `stale` or `unevaluated`, report its warning and continue; `unevaluated` never means fresh.
Every inline or dispatched recon read uses `<RECON-REF>` when it is not `HEAD`.

Report installed-skill drift (a stale record warns; an absent capability stops):

```bash
bash <skill-dir>/toolbox/plan/boss-plan-run.sh drift
```

On `RELOAD:`, read the named file in full and restart Phase 0 under that body, once per run.
A second reload means a peer install is racing; continue under the body just read and report it.
A blocking drift verdict, including config-validator drift, is a hard stop.

Inspect `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node tracker/cli.mjs operations`. When it lists `writePlan`, require
`operations --require writePlan,readPlanAttachment`; otherwise require
`operations --require preparePlanAttachment,finalizePlanAttachment,readPlanAttachment,deletePlanAttachment`
through the same CLI. Exit 2 names what is missing: stop before any write. Confirm the tracker
answers a cheap read. A failed
read: classify it with `--operation read` and follow its action line (a retryable edge error is not
"tracker unreachable").

When `tracker/cli.mjs operations` lists a `preflight` operation, run it with
`tracker/cli.mjs invoke-operation preflight` instead of the MCP preflight and team listing. A
non-zero exit is the stop reason, verbatim.

**Team.** `--team <name>` is a skill argument, never a ticket id. When config names no team, the
cheap read is `listTeams` (`limit=50`, through `resolvedServer`; an adapter without it gives
`visibleTeams: null`), classified with `trackerMcpPreflight` (`tracker/preflight.mjs`, `probeOk` =
it succeeded). Then `resolveTrackerTeam(config, {preflight, visibleTeams: <raw result>, teamFlag})`
and gate on `isConfiguredForPlanning(config, {team})`. Unconfigured ⇒ print `message`, delete
`.linear-plans/run-<RUN-SCRATCH-ID>/`, exit 0 with no tracker writes. Configured ⇒ print a non-empty
`message` (an overridden `--team`), use `team` for every team-scoped read, and pass
`--team <team>` to `fetch-candidates`.

## Phase 1 — Select the issue

First report the phase: `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node stage-chain.mjs phase planning` (one JSON line, never fatal).

- **Arguments**: `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node plan/plan-child-fanout.mjs args -- <args>` parses the ticket
  ref, `--team`, the selection flags and `--parallel N` (child planners at once on the fan-out route;
  1..8, default 4) — non-zero ⇒ `BLOCKED: <its stderr>`.
- **A named ticket**: `get_issue` it, whatever its state. Interactive re-planning follows
  `references/interactive-mode.md`. Headless stops on Done/Canceled.
- **Selection flags** (`--label`, `--exclude-label`, `--assignee`, `--creator`, `--project` and their
  `--exclude-` forms) are filters, never ticket ids: classify arguments with
  `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node selection.mjs split-args -- <args>`; with a named ticket they are
  ignored with a one-line warning.
- **Otherwise**: candidates always come only from
  `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node tracker/cli.mjs list-unplanned --team <team> <selectionArgs>`,
  whether or not a selection is configured. Selection flags take precedence over the plan-stage
  entry, then shared keys; another stage's entry (for example `stages.build`) never applies. The
  verb requires `agent-plan` and excludes `needs-human`. It already excludes candidates holding a
  live peer plan claim and names each exclusion on stderr. The tracker claim is the only peer
  signal: never inspect peer run scratch or `boss ls` to choose a head. A non-zero exit stops the run quoting its
  stderr; never read the unplanned list through another path. The output is unordered: rank by
  priority (Urgent first, None last), then oldest. Interactive confirms with the user (plan / skip /
  pick another / cancel); headless takes the head. An empty queue reports and stops.

**Claim before routing.** Run
`node "$BOSS_PLAN_TOOLBOX/tracker/cli.mjs" read-issue --id <ISSUE-ID> --out-file .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.precheck.json`.
Exit 0 continues; exit 2 uses the [snapshot fallback](references/planning-claims.md#claim-before-drafting);
exit 64 means fix the call. Then run the planning claim protocol in
[planning claims](references/planning-claims.md#claim-before-drafting). It applies to named tickets,
single tickets, fan-out parents and epic resumes; acquire the issue lease before posting the comment claim. A planning claim must never use build scope.

**Route the selected issue** — named or sweep head — after claiming: when
`tracker/cli.mjs operations` lists `listChildren`, run
`tracker/cli.mjs invoke-operation listChildren --args-file <file>` with `{parentId}` and use its
hydrated `children` as the per-child payloads (no separate per-child read). Otherwise
`list_issues parentId=<id> limit=250`, then `get_issue includeRelations=true` each child (list
payloads truncate). Write the `{parent, children}` bundle (preserving any `relationWarnings`) to
`.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.fanout-route.json`, and run
`bash <skill-dir>/toolbox/plan/boss-plan-run.sh node plan/plan-child-fanout.mjs route <that file>`. Print any `relationWarnings`
in the route summary; they never stop the run. `single` ⇒ continue below;
`fan-out` ⇒ [`references/child-fanout.md`](references/child-fanout.md) instead of the precheck and Phases
2–5; `epic-resume` ⇒ [`references/epic.md`](references/epic.md#resume); `epic-noop` ⇒ report its
`reasons`, no writes, outcome `noop`; `abort` ⇒ report, no writes, outcome `blocked`.

Then, for a `single` route, run the idempotence precheck against the selection snapshot already written:

```bash
bash <skill-dir>/toolbox/plan/boss-plan-run.sh node plan/plan-run-guards.mjs idempotence .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.precheck.json --selected-id <ISSUE-ID>
```

`action: "noop"` (already planned with a valid description and plan attachment) ⇒ delete the scratch,
print one line, route through Phase 6/7 with outcome `noop` and no planning writes. `action: "plan"` ⇒ log its `reasons` and continue;
`fetched-issue-id-mismatch` means re-select. Exit 2 (`unreadable-input`) means re-run `read-issue`
or stop closed; never treat unreadable input as `plan`.

## Phase 2 — Draft

Drafting resolves through the first tier that succeeds: a repo-local `boss-plan-*` extension with
`role: draft`, a host built-in, or the inline prompt. A dispatch succeeds only when its result is
valid **and** it wrote a non-empty plan at the path it alone was given; record
`extension <name>: skipped (<reason>)` for every one that did not, and run `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node notes-record.mjs add --core boss-plan --trigger extension-failed --where "<extension name>"` (never fatal) for each.

**Interactive:** follow `references/interactive-mode.md`, then Phase 3.5 and Phase 4.

**Headless:** do not draft inline. Dispatch **one** awaited drafting subagent that keeps recon and the
plan in its own context and returns only a path and bounded metadata.

1. Create the sentinel context. Copy the printed `KEY=value` lines literally into later calls;
   no shell variables survive between calls:

   ```bash
   bash <skill-dir>/toolbox/plan/boss-plan-run.sh dispatch-ctx --precheck .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.precheck.json --run-scratch .linear-plans/run-<RUN-SCRATCH-ID>
   ```

2. Read the issue UUID from the selection snapshot's `uuid` field (MCP fallback: the adapter's UUID).
   Snapshot the stored description — the run's only description source, which the worker builds
   `## Original notes` from and Phase 4 gates against:
   `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node tracker/cli.mjs read-description --id <issue UUID> --out-file .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.image-guard-orig.md`
   (exit 2 ⇒ byte-copy the Phase 1 `get_issue` description; exit 64 or a receipt for another id
   stops). Add nothing to it.

3. Dispatch a `general-purpose` subagent on the orchestrator's model (planning is judgement). Pass
   the **path** `references/headless-drafting-brief.md`, the ticket id and title, the snapshot path,
   `PLAN_PATH`, `RUN_SCRATCH`, `<RECON-REF>` as `RECON_REF`, Phase 0's resolved tracker `team`,
   `RUN_SENTINEL`/`RUN_DIR`/`RUN_ID`, and `allowSiblingSplit: true` when
   the issue has a parent and `tracker/cli.mjs operations --require createIssue,appendDependency`
   exits 0. It returns only `planPath`, `labels`, `agentBuild`, `estimate`, `priority`,
   `openQuestions`, `descriptionSummary: {path}` and any `siblings` — never plan text. Before
   each attempt, record its clock:

   ```bash
   bash <skill-dir>/toolbox/plan/boss-plan-run.sh dispatch-mark --run-dir <RUN_DIR>
   ```

   Hold the dispatch; re-arm this call while it exits 98 or 95:

   ```bash
   bash <skill-dir>/toolbox/plan/boss-plan-run.sh dispatch-wait --run-dir <RUN_DIR> --run-id <RUN_ID> --heartbeat .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.dispatch-heartbeat.json --budget 590000
   ```

   Claude Code sets Bash tool `timeout: 600000` (budget below tool timeout); harnesses unable
   to raise it omit `--budget`.
   [`references/headless-dispatch.md`](references/headless-dispatch.md) owns the hold and failure rules.

4. Classify from the run file only, and re-verify every artifact the payload names by measuring it on
   disk:

   ```bash
   bash <skill-dir>/toolbox/plan/boss-plan-run.sh dispatch-classify --run-dir <RUN_DIR> --run-id <RUN_ID> --run-scratch .linear-plans/run-<RUN-SCRATCH-ID> --issue <ISSUE-ID> --plan-path <PLAN_PATH>
   ```

   On any headless drafting `dispatch-failure`, run `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node notes-record.mjs add --core boss-plan --trigger dispatch-failed --where "boss-plan headless drafting dispatch"` (never fatal).

   An epic outcome (`payload.epic`) already did every tracker write and is accepted only on
   `epic-reverify` exit 0 (`references/headless-dispatch.md` hydrates its inputs); skip Phase 3.5–4.
   A single-ticket outcome continues with the printed `PLAN_FILE`. The classifier writes
   `.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.premises.json` for Phase 4; an epic prints
   `OUTCOME=epic`, and a single ticket prints `OUTCOME=single`.

5. Before adopting any returned metadata or entering a Phase 4 gate, run the
   [fresh recheck](references/planning-claims.md#recheck-at-write-boundaries). Exit 3 releases the claim,
   discards the draft with `guard-discard`, removes this run's scratch, and ends `noop` with no
   planning writes. Exit 2 aborts closed and releases the claim; never treat unreadable input as proceed.

   Write the **returned** metadata object verbatim with the orchestrator’s file-writing tool to
   `.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.draft-returned.json` (the declared
   `draft-returned` family), overwriting any same-named file the drafter wrote. Then validate it:

   ```bash
   bash <skill-dir>/toolbox/plan/boss-plan-run.sh adopt-metadata --returned .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.draft-returned.json --metadata .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.draft-metadata.json --run-dir <RUN_DIR> --plan-path <PLAN_PATH> --run-scratch .linear-plans/run-<RUN-SCRATCH-ID>
   ```

   The guard normalises labels, estimate and priority itself (its `warning:` lines say what changed)
   and refuses a missing plan path or description, or estimate 5 without an `Atomic-5:` /
   `Oversized-child:` bullet under either artifact's current `## Planning` (`estimate-5-unjustified`).
   A plan-only bullet is carried into the description artifact with a `warning:` line.

## Phase 2.5 — Epic

When the honest triage is EPIC, build a parent plus planned children instead of one plan: follow
[`references/epic.md`](references/epic.md). Headless, the drafting subagent does this itself;
interactive, a human confirms the shape first.

## Phase 3 — What the plan contains

The drafting brief ([`references/headless-drafting-brief.md`](references/headless-drafting-brief.md)
Steps 5 and 7) is the single drafting spec for both modes — plan body and description template. The
plan lives at `PLAN_PATH`, the `plan-slug.mjs plan-path` output
(`.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>-<slug>.md`, where `<slug>` is the hyphenated title). Every query-bearing upload URL in it is query-stripped. Headless open
questions are only genuinely controversial forks; a non-empty list adds `agent-question` and an
`## Open Questions` section. Config-first helpers take `(config, description)`:
`validatePlanDescription`, `parseAcceptanceCriteria`, `parsePremises`, `validateVerifyOnlyEvidence`,
`extractKeyChangeAreas`.

## Phase 3.5 — Extension plan reviewers

Run any repo-local `boss-plan-*` extensions with `discover --core boss-plan --role plan-reviewer`
over the draft (additive, non-fatal, a no-op when none are installed), holding each dispatch in-turn
with `bs-dispatch-await.mjs wait --leg-deadline` per
[`references/extension-reviewers.md`](references/extension-reviewers.md).

## Phase 4 — Gate, attach, write back

**Premise re-check.** Tickets the plan cites may have moved since recon
([premise protocol](references/headless-dispatch.md#premise-drift-is-reconciled-not-appended)):

```bash
bash <skill-dir>/toolbox/plan/boss-plan-run.sh premises --run-scratch .linear-plans/run-<RUN-SCRATCH-ID> --issue <ISSUE-ID> --plan .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>-<slug>.md --description .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.description.md
```

When `premises.json` is absent, write the sentinel payload's `premises` array (or `[]`) there
with the file tool before calling `premises`. The step persists its exit status and report in
`<ISSUE-ID>.premise-report.json`; `writeback-verify` enforces that verdict across shell calls.
Non-zero ⇒ no writes. Drift is annotated (`- Premise drift: …`) and named in the report.

**Predict verify parks.** Before the secret, image and contract gates, run
`bash <skill-dir>/toolbox/plan/boss-plan-run.sh node plan/plan-park-prediction.mjs annotate --description .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.description.md --plan .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>-<slug>.md --json`.
It annotates both artifacts and prints `{predicted, codes, labels}`. A prediction never changes the
exposure label or build order: the marker is information for people, and Phase 6 names the
predicted codes and labels. All following gates see these final annotated bytes.

**Secret gate (mandatory).**

```bash
bash <skill-dir>/toolbox/plan/boss-plan-run.sh node plan/plan-secret-scan.mjs .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>-<slug>.md .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.description.md
```

A clean scan is a floor, not a pass: then read the whole plan (especially `## Original notes`)
yourself and redact anything credential- or PII-shaped in every persisted artifact. Credential query
values in external image URLs become `token=REDACTED`; a signed `uploads.linear.app` URL loses its
signature query and keeps its asset path.

**Image and notes gates.** Write `attachment-guard-orig.md` from the snapshot with only the mandatory redactions and signature
stripping; the step copies the description artifact to `image-guard-new.md` and runs all four gates:

```bash
bash <skill-dir>/toolbox/plan/boss-plan-run.sh image-gates --run-scratch .linear-plans/run-<RUN-SCRATCH-ID> --issue <ISSUE-ID> --plan .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>-<slug>.md --body .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.description.md
```

A failing gate deletes the source copies (they may be sensitive) — except on a usage error (exit
64), which keeps them — but keeps `$PLAN_FILE`, so the next attempt edits rather than redrafts. Pass `--allow-empty-original` only when the stored description
really was empty.

**Contract gate.**

```bash
bash <skill-dir>/toolbox/plan/boss-plan-run.sh contract-gate --run-scratch .linear-plans/run-<RUN-SCRATCH-ID> --issue <ISSUE-ID> --plan .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>-<slug>.md
```

It fails only on a missing `## Summary` / `## Original notes`, text that is not a plan description,
an unsubstituted placeholder, or an empty/truncated plan; everything else is a `warning:`.
For code in another repository, add `- Target repo: <owner>/<name>` under the description’s
`## Planning` and pass `--target-root <checkout>` to `plan-contract-guard.mjs` when a matching
checkout exists; otherwise it prints one `target-repo-not-resolved` advisory in place of per-path
warnings.

Then:

1. **Store** the plan per [`references/plan-storage.md`](references/plan-storage.md) and assert
   exactly one `Implementation plan (<ISSUE-ID>)` plan record remains (the adapter exposes it as an
   attachment; duplicates ⇒ no further writes).
2. **Labels**: read with `readLabels` and compute the merged set (see "What a planned ticket is").
   Interactive runs strip a stale `agent-question` (a human answered every fork). Adopted
   `siblings` hold `needs-human` until step 6 verifies them.
3. **Save** in one tracker write. On a tracker declaring `writePlan`, save only labels, estimate,
   priority and the planned state; never call `write-description` or use an inline description
   fallback. On other trackers, save the description from
   `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node tracker/cli.mjs write-description --id <ISSUE-ID> --body-file .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.image-guard-new.md --base-file .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.image-guard-orig.md`
   (execute the `{tool, args}` it emits for `descriptor-emitted`, regardless of `mode`; for `operation-executed`,
   use `result` without another write; fall back to an inline
   description only when stderr names a missing `writeDescription` op), plus labels, estimate,
   priority and the planned state. A rejected estimate: retry without it and warn. A failure
   `classify-outcome` calls `indeterminate`, or an anchored patch rejected as missing or non-unique:
   read the issue back before any retry; if it did not land, retry once without `--base-file`.
   If that retry fails the same way, split it: save everything except
   labels, then the merged labels in a labels-only write, and name the split in the run report.
   The save response can echo the pre-write issue; it is never evidence of what landed. Step 5's
   settled read-back supplies that evidence. After the description save, copy `image-guard-new.md`
   to `image-guard-final.md`: that file always holds the bytes of the run's last description save,
   including when step 4(f) is skipped.
4. **Link dependencies** — I/O only; `plan/plan-deps-lib.mjs` decides every edge:

   a. Fetch candidates (planned, in-progress, in-review, plus every related id):
   `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node tracker/cli.mjs fetch-candidates --out-file .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.candidates.json --team <team> --id <ISSUE-ID> --id <related id>…`.
   Read only the receipt and `jq -r '.[] | [.identifier, .title, .stateName] | @tsv'`; a
   non-zero exit means _could not evaluate_. Reuse the drafter's open-PR snapshot when present;
   otherwise run `plan-open-prs.mjs fetch` once:

   ```bash
   bash <skill-dir>/toolbox/plan/boss-plan-run.sh open-prs --run-scratch .linear-plans/run-<RUN-SCRATCH-ID> --issue <ISSUE-ID>
   ```

   A failed fetch records `- Open-PR overlap: could not evaluate (<reason>)` under `## Planning`;
   continue the independent ticket scan without `--open-prs`.
   b. Judge real logical dependencies yourself, with direction: `logicalDependencies[<id>] =
{direction: 'blockedBy' | 'blocks', note}`. Only these verdicts create blocking edges. File
   overlap is computed for you from each ticket's `## Key changes` (pass paths verbatim, never
   coarsened) and becomes a non-blocking `relatedTo` link — touching the same files is a rebase
   risk, not a prerequisite. Overlap only on regions shared with more than the fan-out cap
   (default three candidates) collapses into one `hot-overlap` planning note per region instead
   of relations.
   c. Write the scan input to `.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.deps-in.json`
   (`declaredRelatedIds`, `logicalDependencies`, optional `childrenByParentId`, plus any
   `moduleRoots`/`repoWideTokens`/`areaAliases` overrides — no `subject`) and classify:

   ```bash
   bash <skill-dir>/toolbox/plan/boss-plan-run.sh deps --run-scratch .linear-plans/run-<RUN-SCRATCH-ID> --issue <ISSUE-ID>
   ```

   Read the stderr line (`subjectAreas`, unresolved tokens, verdict) before writing anything.
   d. Act on the result: write each `edge.write` with `appendDependency` (append-only); `relatedTo`
   edges with `appendRelatedTo` when the adapter has it, else a `## Planning` note; `notes` under
   their declared destination (`planning` → `## Planning`, `risks` → `## Risks`); `questions` under `## Open Questions` plus `agent-question`. A `skipped` entry
   with `expandChildren` true — add its children as `--id` and re-run (at most twice, excluding
   parents already expanded). An `open-pr-ticket-unfetched` skip adds its id as `--id` and re-runs
   fetch-candidates plus deps once, reusing the snapshot. Record `open-pr-overlap` notes at their
   destination and `open-prs-truncated` warnings as-is. `no-candidates` is a clean result: record nothing.
   `could-not-evaluate` is reported as such, never as "no dependencies"; an unresolved area is
   code-marked or declared in `moduleRoots` and re-run.
   e. Before each blocking write, read both tickets' relations and skip a write that would form a
   cycle. A blocker whose PR is merged (`gh pr view <url> --json state,mergeCommit`) gets
   `landed: {evidence}` and a re-run instead. Pass those reads to `transitiveBlockWarnings`:
   upstream ⇒ a `- Transitive-block warning:` line; downstream ⇒ a note (escalated also as a
   question). After each relation write, re-read **both** sides; record a missing edge, never
   re-write it.
   f. Skip this entire stored-description second pass on a tracker declaring `writePlan`; retain
   dependency notes and questions in the run report and save any added `agent-question` label
   without a description. Otherwise, only when `verdict.recordToDescription` is true or the second
   premise pass changed the text:
   on the direct-read route, settle the stored description into `image-guard-base.md` first:

   ```bash
   bash <skill-dir>/toolbox/plan/boss-plan-run.sh read-base --run-scratch .linear-plans/run-<RUN-SCRATCH-ID> --issue <ISSUE-ID> --uuid <PHASE-1-ISSUE-UUID>
   ```

   Check the receipt UUID and `settled`. If false, never edit or save from this read: take the same
   labels-only branch as `writePlan` above, retain dependency notes and questions in the run report,
   and name the read lag there. Leave `image-guard-final.md` holding step 3's saved bytes. If true,
   copy `image-guard-base.md` to `image-guard-final.md`, re-read premise states and run the second
   pass below. On the run-wide `getIssue` fallback, byte-copy its stored description into the same
   base and final files without a receipt, as before:

   ```bash
   bash <skill-dir>/toolbox/plan/boss-plan-run.sh premise-repass --run-scratch .linear-plans/run-<RUN-SCRATCH-ID> --issue <ISSUE-ID>
   ```

   Append the notes, questions and dependencies to `$FINAL` with one helper invocation per heading;
   it matches the stored list marker and skips existing items:

   ```bash
   bash <skill-dir>/toolbox/plan/boss-plan-run.sh node plan/plan-section-append.mjs --file .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.image-guard-final.md --heading Planning --item "Dependencies: blocks <ID>; blocked by <ID>" --item "<note>"
   ```

   ```bash
   bash <skill-dir>/toolbox/plan/boss-plan-run.sh node plan/plan-section-append.mjs --file .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.image-guard-final.md --heading "Open Questions" --item "<question>"
   ```

   Save the edited `$FINAL` with `write-description --body-file "$FINAL" --base-file "$BASE"`
   (plus labels if `agent-question` was added), using step 3's read-before-retry rule on rejection. Record whether
   this second description save happened; only then is `image-guard-base.md` the stale input for step 5.
   The description and the attachment then legitimately differ at `## Planning`.

5. **Verify the write-back, once, after the final description save.** Skip this entire step on a
   tracker declaring `writePlan`: the plan-readback contract and image checks in
   `references/plan-storage.md` verify the stored plan instead. Otherwise, `image-guard-final.md` must
   hold the exact bytes of that save. On the direct-read route use the settled read below; add `--second-save`
   only when step 4(f) made a description save; that selects `image-guard-base.md` as the stale input
   instead of `image-guard-orig.md`. On the `getIssue` fallback byte-copy into `image-guard-stored.md` and run
   the step below with `--read-fallback`: it verifies those copied bytes without reusing an earlier
   receipt, and still enforces the persisted premise verdict:

   ```bash
   bash <skill-dir>/toolbox/plan/boss-plan-run.sh writeback-verify --run-scratch .linear-plans/run-<RUN-SCRATCH-ID> --issue <ISSUE-ID> --uuid <PHASE-1-ISSUE-UUID>
   ```

   `byte-exact` / `normalized-equivalent` pass. `unattributed` passes but keeps the scratch and names
   the line in the report. `drift` and `unsettled` fail: keep the scratch and **do not** rewrite the
   description. `unsettled` means the write is unconfirmed after the read budget, not content drift;
   record `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node notes-record.mjs add --core boss-plan --trigger evidence-unknown --where "boss-plan write-back read lag"` (never fatal).
   No verdict (a read failure) fails too.
   When write-back verification prints no verdict, run `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node notes-record.mjs add --core boss-plan --trigger evidence-unknown --where "boss-plan write-back verification"` (never fatal).

6. **File siblings** when the metadata carries `siblings`:
   [`references/sibling-split.md`](references/sibling-split.md#phase-4) creates, links and verifies
   each, then flips the exposure label; any failure keeps `needs-human`.

## Phase 5 — Clean up

Everything this run wrote — the plan text, rejected structure artifacts, epic child plans,
image and attachment guard scratch, signed upload headers and the epic spec — is inside this
run directory. Plan storage deletes each upload header after its PUT; cleanup also covers an
aborted upload that stranded one. The step checks that removal actually completed.

```bash
bash <skill-dir>/toolbox/plan/boss-plan-run.sh cleanup --run-scratch .linear-plans/run-<RUN-SCRATCH-ID>
```

Interactive runs also remove the seeded design doc (`references/interactive-mode.md`). Every terminal
path cleans up, including dispatch failures — except a write-back `drift`. Never select a sentinel
run directory by grepping a ticket id; use the `RUN_ID` you were handed.

## Phase 6 — Report

Issue id and title; the attachment id and title; final labels, estimate, priority; the state change;
the write-back verdict and read route; the dependency verdict (`<verdict> compared=N edges=M`) and any
transitive-block warning; premise drift; predicted verify park codes, labels and action; for an epic,
the parent and child ids; for a child fan-out, the per-child table and the parent-flip result ([`references/child-fanout.md`](references/child-fanout.md)).

On every terminal path, first run `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node plan/plan-claim.mjs release --receipt "$CLAIM_RECEIPT"`
per the planning claim protocol, then delete its printed `commentId`, when non-null, through
`deleteComment` when the adapter declares that optional operation. Remove scratch last. Release is best-effort and never
fatal: a stranded plan claim forfeits when its owner session is archived or closed, otherwise after two hours; build arbitration ignores it. Release also
before selecting the next candidate. See [planning claims](references/planning-claims.md).

On every terminal path, unless `BOSS_NOTES_SUPPRESSED=1`, run `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node notes-record.mjs flush --core boss-plan --outcome "<outcome>" --mode "<mode>"` once (unsampled, never fatal), then continue.

**Notes** (skip when `BOSS_NOTES_SUPPRESSED=1`): discover `--role notes`; none ⇒ nothing. Run
`bash <skill-dir>/toolbox/plan/boss-plan-run.sh node notes-record.mjs roll --core boss-plan --consume`.
`notesSampleRate(config)` in `skill-config.mjs` resolves `notesDefaults.sampleRate` (default `1`).
If JSON `sampled` is `false`, print `notes: sampled out (rate <rate>)` and skip this notes dispatch. Otherwise write at
most five secret-free observations (≤ 8 KiB) to a temp `observations.md` and dispatch each extension
(instructions from its `skillPath`, bounded by `BOSS_SKILL_EXTENSION_TIMEOUT_MS`) with
`{"role":"notes","core":"boss-plan","context":{"mode","core","outcome","repoId","observationPath"},"runTmp","outPath"}`;
validate with `--role notes`. Never fatal.
For a notes-extension skip or invalid result, run `bash <skill-dir>/toolbox/plan/boss-plan-run.sh node notes-record.mjs add --core boss-plan --trigger extension-failed --where "<extension name>"` (never fatal).

## Phase 7 — Self-archive

The very last action on every terminal path, after the report and any notes dispatch:
`bash <skill-dir>/toolbox/plan/boss-plan-run.sh node session/session-self-archive.mjs --outcome <planned|noop|epic|empty-queue|blocked|failed> --run-scratch .linear-plans/run-<RUN-SCRATCH-ID>`
— add `--suppressed` when a calling skill or your prompt owns the session. Act on its `verdict`:
`archive` — the archive is launched and will close this pane; end the turn with one line. `finish` —
the run is handed to bossd's finalize; end the turn with one line. `ask` —
ask whether to archive now; on yes re-run with `--confirmed`, on no stop. `skip` — print the
`reason` and stop; never an error.

## Cron gate

For an unattended planning cron, register
`node ~/.claude/skills/boss-plan/toolbox/cron-gates/boss-plan.mjs [selection flags]` (Codex:
`~/.codex/skills/...`) as the job's gate command, with the same flags in the job's prompt: it exits 0 only when an unplanned `agent-plan` issue
without `needs-human` matches the selection, exits `1` (`gated`) for no work and `69` (`gate_failed`) when it cannot evaluate (Linear uses bossd-injected `LINEAR_API_KEY`; GitHub uses the host’s own `gh` login with `gh` on the daemon’s `PATH`), and costs zero agent tokens otherwise. Interactive runs are
not gated.
