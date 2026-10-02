# Headless dispatch contract (read by the orchestrator)

The Phase 2 headless path dispatches one awaited drafting subagent. `SKILL.md` carries the steps
that run on every run; this reference carries the in-turn hold every run uses (read it at step 3),
then the three rules that decide a run only when something is unusual — an EPIC triage, a transport
death, or a metadata object that does not match the file at its declared path. Read those at those
three points, not on the happy path.

## Premise drift is reconciled, not appended

Persist the Phase 2 sentinel array `$PREMISES` to `premises.json` before the first guard.
For empty premises, write `{}` to `premise-states.json`; both guard inputs must exist.
For a non-empty sentinel `premises` array, use fresh `getIssue` reads at both write boundaries.
The first pass runs after the secret gate and before image parity: `--annotate` updates the
description artifact and `$PLAN_FILE`, so the image guard copies annotated bytes and every later
gate checks the text that will be saved. The orchestrator never hand-appends drift lines.

Before composing step 5(f), read the stored description into `image-guard-final`, re-read the
premise ids and overwrite the same `premise-states` file. Reconcile that read-back, then save when
the dependency verdict requests recording or reconciliation changed its bytes. Keep the annotated
read-back as the save's intended bytes; fetching it again would discard the second pass. This
also catches a resolved premise, whose old annotations must be removed. Do not re-annotate the
attachment: step 5(f) already legitimately diverges from it. Step 6 verifies this final save.
The dependency scan needs no earlier premise verdict: it fetches its own candidate states fresh.

A Phase 1 selection payload, idempotence precheck or any state seen before the drafter returned
is not a reference for judging a returned premise. Disagreement is not a drafting error. Only
the fresh re-read decides drift; report and annotate it, never rewrite recon-time premise states.

`reconcilePremiseAnnotations` strips prior drift lines and inline markers for every declared id,
then applies the current drift set. Repeating a pass preserves identical bytes; changed states
replace annotations and resolved drift removes them. Whole-token matches exclude ticket prefixes.
The helper skips fenced code and `## Original notes` through EOF. Headings, table rows and
`## Planning` receive no inline markers. `## Premises` and `## Acceptance criteria` stay intact
because their checkbox bullets end in a guard-parsed `— check:` clause; their mentions appear in
the drift line's `also stated in` clause. Inline sections appear in `flagged inline in`. Drift
lines land directly after the last Planning bullet with no intervening blank line.

`premise-limit`, `premise-unresolved` and `unreadable-input` write no annotation files. On the
first pass these take the SAFE branch before tracker writeback. On the second pass the first save
already exists: stop further writes and retain scratch. Run mandatory step 6 against step 4's
intended bytes, report its verdict and the premise error, then exit non-zero. Never claim no write
occurred or verify the read-back as its own intended bytes. `ok` and `premise-drift` reconcile every named file; drift never aborts.

## Hold the dispatch in-turn with `wait`

The Agent tool backgrounds every dispatch, so "await" needs a mechanism, and ending the turn is not
one: a still-armed Stop hook finalizes the run mid-draft. The mechanism is the toolbox `wait` verb.
It settles on the `draft` run-file sentinel and the drafter's own heartbeat — never on the Agent task
output file (a symlink or a stub whose mtime and size never move), and never through a shell `sleep`.

1. **Record the dispatch clock** immediately before each dispatch attempt, the one transport-death
   retry included: `printf '%s000' "$(date +%s)" > "$RUN_DIR/draft.dispatched-at"`. The `draft`
   sentinel is written only when the drafter finishes, so without a caller-held clock every re-armed
   call would restart the seed age at zero and `abandoned` could never fire. Shell variables do not
   survive between Bash calls; the run dir does, its non-`.json` name keeps it out of the sentinel
   set, and `cleanup` removes it with the dir.
2. **Re-arm as a foreground Bash call.** One call blocks at most `--budget` (default 110000 ms,
   under the 120 s default Bash timeout), so the harness never backgrounds the hold itself:

   ```bash
   node "$BOSS_PLAN_TOOLBOX/bs-dispatch-await.mjs" wait "$RUN_DIR" "$RUN_ID" draft \
     --heartbeat .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.dispatch-heartbeat.json \
     --dispatched-at "$(cat "$RUN_DIR/draft.dispatched-at")" --while-live
   ```

3. **Route on the `wait` exit code.** Each re-arm is also the tool-call boundary at which the
   dispatch's returned object can arrive; once it is in hand, stop re-arming and run step 4.

| `wait` exit | Meaning                                                                                   | Next                                                                  |
| ----------- | ----------------------------------------------------------------------------------------- | --------------------------------------------------------------------- |
| `98`        | budget spent: the draft is still open, or its sentinel landed and the drafter still beats | re-arm                                                                |
| `0`         | a non-provisional sentinel landed and the drafter stopped beating                         | step 4 with the returned object; without it, the transport-death rule |
| `96` / `97` | timed out / abandoned (a stale clock and no live beat)                                    | step 4 — its `disposition` classifies the dispatch failure            |
| `2`         | the call itself is wrong (a missing, empty or future clock; a bad flag)                   | fix the call; never read it as a death                                |
| any other   | the harness killed the call                                                               | unknown — re-arm; never read it as a death class                      |

Step 4's `disposition` passes the same `--dispatched-at`, so it cannot re-classify a draft `wait`
called abandoned as a resumable timeout. Under Codex, `wait_agent` is the hold; call `wait` once
after it returns (a landed sentinel with a non-live heartbeat exits 0 immediately).

## The subagent holds Phase 2.5 tracker-write authority

On an EPIC triage the dispatched subagent performs **every** epic tracker write itself
([`epic.md`](epic.md)), in this order:

1. upload the epic spec attachment to the parent;
2. create each child issue, fully planned, in stable topological order;
3. wire the intra-epic `blockedBy` DAG between them;
4. repurpose the parent — apply the epic label, flip it `unplanned → planned` last.

**An orchestrator may not narrow the brief to "draft only."** The narrowing looks harmless from the
orchestrator's side: it is the same ticket, and drafting is what a drafting dispatch does. But the
triage happens **inside** the dispatch, after the recon that decides it. A subagent that triages EPIC
under a draft-only brief has no authority to execute the one outcome its own recon selected, so it
returns an EPIC it was forbidden to perform and the run ends with nothing written. A full 231k-token
dispatch was spent exactly this way. The authority is part of the contract, not a permission the
orchestrator grants per run.

Step 4's epic arm still re-verifies every one of those writes against the tracker before accepting
the sentinel. Authority is not trust: the subagent may write, and the orchestrator must check.

### Hydrate the epic reverify before step 4's block runs

Step 4 accepts an epic only on `node "$BOSS_PLAN_TOOLBOX/plan-run-guards.mjs" epic-reverify`, and
that verb reads files, never the tracker: tracker reads are tool calls, which cannot run inside a
bash block and share no shell state with it. So once the sentinel reports `payload.epic == true`,
and before the step-4 block runs:

1. `get_issue` the parent; `list_issues parentId=<parent> limit=250`; `get_issue` each child it
   returns.
2. Write `.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.epic-reverify.json` as
   `{parentId, childIds, parent, children}` — `parentId` and `childIds` from the sentinel payload,
   `parent` and `children` carrying each issue's id, identifier, state, labels, attachments and
   links, and **no descriptions**. The verb ignores any description it finds there and derives every
   other path from the run directory itself, so nothing in the bundle can redirect a comparison.
3. Read every description through `node "$BOSS_PLAN_TOOLBOX/tracker/cli.mjs" read-description --id <UUID> --out-file <path>`
   on the run's one route (`references/plan-storage.md`; on the `getIssue` fallback route, copy
   the returned description byte-for-byte to the same path): the parent into
   `.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.image-guard-stored.md`, and **every live child
   `list_issues` returned** — not only the `childIds` — into
   `.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.child-<CHILD-ID>.image-guard-stored.md`, named by
   its `identifier`, the key the sentinel's `childIds` and `image-guard-new` basenames use. Stop on
   a read failure, or on a receipt `id` / `identifier` that is not the one requested.

The verb prints one JSON verdict on stdout and exits by class:

| Exit             | Class         | What it means for the sweep                                                                                                                                                                                              |
| ---------------- | ------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `0`              | `pass`        | accepted — skip Phase 3.5–4                                                                                                                                                                                              |
| `1`              | `resumable`   | the verdict on stdout says `resumable` — the parent was positively read unplanned: the next unplanned sweep re-picks it, so the run scratch is removed                                                                   |
| `3` or any other | `needs-human` | the parent already left the unplanned state, or its state or an input was unreadable, or node crashed (a bare `1` with no `resumable` verdict): no sweep resumes it, so the run scratch is retained as the only evidence |

A `needs-human` verdict writes nothing to the tracker — the Phase 4 step 6 precedent: the
descriptions are already stored and a corrective rewrite is forbidden. Report the retained run
scratch and the verdict's named blockers.

## A transport death is not a failed draft

Two different things end a dispatch without a sentinel, and they take opposite remedies:

| Death         | What it means                                  | Remedy                                  |
| ------------- | ---------------------------------------------- | --------------------------------------- |
| **work**      | the subagent ran and could not produce a plan  | safe branch — no tracker write, abort   |
| **transport** | the dispatch tool errored, or the turn was cut | retry once, then fall through to tier 3 |

Both leave the same absent sentinel, so the sentinel cannot tell them apart — the caller can, because
only one of them produced a tool error rather than a returned verdict. Route a transport death like a
failed draft and a sound plan is discarded for a reason that has nothing to do with the work.

**On a transport death:** print one stderr line, then retry **once** with the same `PLAN_PATH` and
the same `RUN_SENTINEL`/`RUN_DIR`/`RUN_ID`. Reusing the paths is what makes the retry cheap: the
brief's resume path normalizes a surviving `PLAN_PATH` rather than redrafting it, and
`toolbox/bs-dispatch-await.mjs`'s `disposition` verb says whether an artifact survived to resume
from. If the retry also dies on transport, fall through to the tier-3 inline draft. Make **no**
tracker write on either attempt.

## Validate the object that was returned, not a file at its path

`node "$BOSS_PLAN_TOOLBOX/plan-run-guards.mjs" adopt-metadata "$METADATA" "$RETURNED_METADATA"`
validates the object the dispatch **returned** (never a same-named file the worker may have written),
normalizes cosmetic fields — label spelling, estimate, priority, optional fields — and writes the
normalized object to `$METADATA`, printing a `warning:` line for each change. It fails only when the
plan path or the description is missing or unusable.

### Assign `$RETURNED_METADATA` with a quoted heredoc, never an inline quoted literal

The returned object travels as argv, which is the only form the orchestrator holds it in — writing
it to a file first would reintroduce the substitution this rule exists to refuse. That makes the
**assignment** the hazard, not the guard. Drafted JSON routinely carries an apostrophe (a ticket
title, a summary), and an apostrophe inside a single-quoted shell literal ends the literal: the
variable then holds truncated JSON and `adopt-metadata` cannot parse it.

Assign it with a **quoted** heredoc, whose delimiter suppresses every expansion and needs no
escaping for `'`, `"`, `$` or backticks:

```bash
RETURNED_METADATA="$(cat <<'RETURNED_JSON'
<the returned object, pasted verbatim>
RETURNED_JSON
)"
```

An unquoted `<<RETURNED_JSON` is not this rule: it would expand `$` and backticks inside the
drafted text. `adopt-metadata` also refuses a dispatch that returned **nothing**
(`metadata-not-returned`).

## Liveness is settled on the heartbeat, not on the seed clock

The drafting subagent beats `<ISSUE-ID>.dispatch-heartbeat.json` in the run scratch while it works
(`references/headless-drafting-brief.md`). Phase 2 step 4 passes that path to `disposition` as
`--heartbeat`, which is what makes the beat mean anything: without the flag the classifier falls
back to the sentinel seed's mtime, which never moves, so a live 34-minute draft ages into
`abandoned` and is indistinguishable from a dead one.

The signal only ever extends liveness. An absent heartbeat reads `absent`, never death, so a
drafter that has not beaten yet is not killed; a heartbeat that stops for the whole staleness
window is. The await side never writes the file it reads — touching the artifact you are measuring
manufactures the liveness you claim to observe.

Re-verification detail: zero-byte original guard sources (`.image-guard-orig.md` /
`.attachment-guard-orig.md`) are ok; every other consumed artifact must be non-empty.
