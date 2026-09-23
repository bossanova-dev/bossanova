# Headless dispatch contract (read by the orchestrator)

The Phase 2 headless path dispatches one awaited drafting subagent. `SKILL.md` carries the steps
that run on every run; this reference carries the three rules that decide a run only when something
is unusual — an EPIC triage, a transport death, or a metadata object that does not match the file at
its declared path. Read it at those three points, not on the happy path.

## The subagent holds Phase 2.5 tracker-write authority

On an EPIC triage the dispatched subagent performs **every** Phase 2.5 tracker write itself, in this
order:

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

`<ISSUE-ID>.draft-metadata.json` is a **declared** scratch family, and the brief tells the worker to
write its local files under declared basenames. So the worker can write the very path the
orchestrator is specified to write the returned object to — and an orchestrator that validates
without writing first validates the worker's file instead of the worker's message. Every guarantee
the metadata guard gives is then a guarantee about a file the worker chose.

`node "$BOSS_PLAN_TOOLBOX/plan-run-guards.mjs" adopt-metadata "$METADATA" "$RETURNED_METADATA"` is
the whole rule. It adopts the returned object when the path is absent or already identical, and
refuses with `metadata-not-from-message` when the path holds something else — **without**
overwriting it, because that file is the evidence that the dispatch broke its contract and an
orchestrator that silently repaired it would keep dispatching workers that do the same. Adoption is
not a way around validation: what it adopts is then held to the ordinary metadata contract.

### Assign `$RETURNED_METADATA` with a quoted heredoc, never an inline quoted literal

The returned object travels as argv, which is the only form the orchestrator holds it in — writing
it to a file first would reintroduce the substitution this rule exists to refuse. That makes the
**assignment** the hazard, not the guard. Drafted JSON routinely carries an apostrophe (a ticket
title, a summary), and an apostrophe inside a single-quoted shell literal ends the literal: the
variable then holds truncated JSON, `adopt-metadata` refuses it as malformed, and the SAFE branch
`rm -rf`s a run scratch whose plan had already passed re-verification. The refusal is correct; the
input was wrong before the guard ever saw it.

Assign it with a **quoted** heredoc, whose delimiter suppresses every expansion and needs no
escaping for `'`, `"`, `$` or backticks:

```bash
RETURNED_METADATA="$(cat <<'RETURNED_JSON'
<the returned object, pasted verbatim>
RETURNED_JSON
)"
```

An unquoted `<<RETURNED_JSON` is not this rule: it would expand `$` and backticks inside the
drafted text. A false refusal is the harm to design against here — refusing cannot deadlock a run,
but discarding a good plan costs the whole dispatch.

`adopt-metadata` also refuses a dispatch that returned **nothing** (`metadata-not-returned`). That is
the same separation `bs-dispatch-await.mjs` draws between artifact readiness and message readiness: a
landed sentinel says the artifact is there, never that the message arrived, and with no message in
hand the file at the path is the only thing left to validate — which is exactly the substitution this
rule exists to refuse.

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
