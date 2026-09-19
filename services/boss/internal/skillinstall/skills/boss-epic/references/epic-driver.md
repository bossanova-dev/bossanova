# The epic driver: persisted state, one reconciliation cycle, durable continuation

`toolbox/epic-driver.mjs` is the executable authority for the epic lifecycle. The resident SKILL
body states the invariant, the status vocabulary and the call shape; everything situational is here.

## Why the driver exists

The lifecycle used to be prose, and the most important rule was the one nothing checked: a driver
could launch a child, report that it started, and end its model turn with work still in flight. The
child later went green and merged, and no cycle ever reconciled it, unparked its dependents, or did
the merge bookkeeping. That run reached a final report **byte-identically** to one that drove the
whole epic. `assertEpicCanTerminate` is the read that makes the two distinguishable.

The end of a model turn is never evidence a run is complete. A turn that launches or adopts work is
an intermediate checkpoint. Do not implement termination by checking whether the current turn has no
more immediate tool calls.

## The persisted state

JSON-safe, keyed by `{epicId, runId}`, written after **every** transition — not once per turn.

```json
{
  "version": 1,
  "runId": "...",
  "epicId": "...",
  "repoId": "...",
  "agent": "...",
  "parallel": 1,
  "plannedState": "...",
  "reviewState": "...",
  "childWallClockMinutes": 360,
  "startedAt": "...",
  "lastReconciledAt": "...",
  "cycle": 0,
  "externalBlockersEvaluatedCycle": -1,
  "phase": "polling",
  "status": "RUNNING",
  "tickets": [],
  "ready": [],
  "inFlight": [],
  "greens": [],
  "merged": [],
  "failed": [],
  "cascadeSkipped": [],
  "externallyCleared": [],
  "pendingCascade": [],
  "needsHuman": [],
  "retainedEvidenceWatches": [],
  "sessions": {},
  "watches": {},
  "pendingWakes": [],
  "progressCommentId": "",
  "progressMarker": "boss-epic-progress",
  "finalProgressWrittenAt": "",
  "watchCleanupDoneAt": ""
}
```

**Never persist a `Map` or a `Set`.** `JSON.stringify(new Set(['A']))` is `{}`, so a state saved with
Set collections reloads with every id gone — the resume path then reconstructs an EMPTY in-flight
table and relaunches every child. `validateEpicState` rejects them at any depth, and
`saveEpicState` validates before writing, because an invalid state on disk is worse than a failed
save. Rebuild collections inside one cycle.

`saveEpicState` replaces atomically (temp file + `rename`), so a crash mid-write leaves the previous
state intact rather than a truncated file — and a file that fails to parse is indistinguishable from
a fresh run, which is the relaunch-everything failure. `loadEpicState` returns `null` only for a
genuinely absent file and **throws** on a corrupt one.

`sessions[ticket]` carries `sessionId`, the exact tracked `chatId`, `adopted`, `reconciled`,
`launchedAt`, `prNumber` / `prRepo` / `prUrl`, `liveness`, `repairRounds` and a `note`. A blank
`sessionId`/`chatId` fails validation: two records with `""` compare equal, so the driver would adopt
one child twice and remove another child's watch.

### The resume join

`buildEpicProgressState` emits the progress comment's `run` block (`runId`, `phase`, `status`,
`lastReconciledAt`, `nextWake`) through `renderProgressComment`. A fresh driver finds the comment by
its marker, reads `runId` back with `parseProgressRunMetadata`, and derives the state-file path with
`epicStatePath({epicId, runId, dir})`. That join is why the block is machine-readable rather than
decoration, and it is what makes resume independent of turn history.

## The terminal invariant

```text
The run is non-terminal while any of the following is true:

- the scheduler has at least one ready eligible ticket;
- the in-flight table contains any ticket;
- a green ticket is queued for serialized merge;
- an adopted session has not been reconciled to merged or fail-isolated;
- a failed ticket still has dependents whose cascade outcome has not been recorded;
- a callback/subscription wake is pending reconciliation;
- external blocker state has not been evaluated for the current cycle.
```

`epicTerminalBlockers(state)` returns these as `label:ids` strings; `assertEpicCanTerminate` throws
naming all of them. It is a throw and not a boolean deliberately: a caller who forgets to check a
boolean ships the premature final report, whereas a caller who forgets to catch a throw ships
nothing.

The external-blocker condition is **cycle-scoped**. A read from a previous cycle is not an
evaluation of this one — a ticket can be unparked or re-parked by a blocker that moved since, so a
stale read is exactly as uninformative as no read. `beginCycle` advances the counter; only
`recordReconciliation({externalBlockersEvaluated: true})` satisfies it again.

`transitionToDone` is the **sole** DONE path and adds, on top of the invariant: final authoritative
reconciliation recorded; every eligible ticket merged / fail-isolated / cascade-skipped; the final
progress upsert written; settled-child watch cleanup done (retained evidence watches excepted).
`epicDoneBlockers` reports the unmet ones. The zero-launch branch is DONE only when reconstruction
adopts no live session and the same assertion passes.

## The launch / adoption handoff

One transaction, in this order, before yielding:

1. record the child ticket id;
2. record the session id;
3. record the exact tracked chat id;
4. record the launch/adoption timestamp;
5. record the PR URL/number when available;
6. add the ticket to `inFlight`;
7. upsert the single progress comment;
8. resolve the verified callback target (`selectEpicCallbackTarget`);
9. arm the draft-aware PR watches;
10. register the child-session `settled` subscription;
11. **list-read each registration to verify it is active and ours**;
12. persist;
13. only then return a non-terminal `RUNNING` checkpoint.

Failure before durable coverage cannot be reported as success. `recordLaunch` forces `status` back
to `RUNNING`, so the step that adds work can never be the step that declares completion.

A `needsHuman` ticket is **refused** by `recordLaunch` rather than normalized away — a caller that
tried to launch one has a bug that normalization would hide. `normalizeEpicState` additionally
strips needs-human ids out of `ready`/`inFlight` at the one choke point every transition passes
through, so a new transition cannot forget the rule.

## Durable wakes

### PR callbacks

`policy.epicChildTriggers` (callback adapter): `checks_passed_ready`, `ready_for_review`,
`checks_failed`, `merged`, `closed`. `policy.forbiddenDraftTriggers` names bare `checks_passed` —
a child opens its PR as a **draft** and CI runs on drafts, so bare `checks_passed` fires on the
first green draft commit and burns the one-shot watch at a moment that can never be merge-eligible.
`closed` is a required failure path: a child PR closed without merging leaves a ticket that never
goes green, and without the trigger the run waits on it until the wall clock expires.

Keep one `group` per trigger, so a re-arm cancels only same-trigger siblings. These waits are not
mutually exclusive, so a shared group would let one state cancel the other still-needed watches.

### The session-outcome subscription

`sessionOutcomeMap` (session adapter) declares subscribe / list / remove over
`boss broadcast subscribe|subscriptions|unsubscribe`, with `--on settled` and the child's
`--session` passed **explicitly** — the flag defaults to the ambient session, which for a child
watch is the orchestrator's own and therefore the wrong session entirely.

Verify with `verifySubscriptionRow`: ownership is `owner_session_id` + `origin_chat_id`, liveness is
`state` plus the absence of `fired_at`. A row that is not ours is not coverage, however active it
looks. `sessionOutcomeCapability(adapter)` and `sessionOutcomeTransportPreflight` detect a missing
durable transport **before** a child is promised as watched.

A session becoming idle between turns is not terminal; the subscription is based on session outcome
semantics, never on chat idleness.

### Re-arming

A one-shot registration is **consumed when it fires**, and a `settled` subscription can fire
mid-flight — burning the very wake the driver was relying on. `needsRearm` therefore treats
`fired` / `expired` / `canceled` / absent as a hole to re-arm while the child stays in flight, and
only `active` as coverage. Remove a settled child's watches only after durable terminal
bookkeeping; retain the watches of a live fail-isolated child as evidence
(`retainedEvidenceWatches`).

### Verification is the list read

An arm that returns without an error but does not appear in the list read **did not take**, and is
indistinguishable from one that did unless the driver looks. `ensureDurableCoverage` therefore
list-reads after every arm and treats a still-missing trigger as a registration failure.

### The bounded fallback

Registration or verification failure is a **transition**, never a silent continue:

- a supported bounded in-session wake recorded in `watches[ticket].fallback` (`mechanism`,
  `nextWakeAt`, `reason`, `retryCount`, `lastReconciledAt`) ⇒ status stays `RUNNING`, because the
  child is still genuinely observed;
- no fallback available ⇒ `RUNNING_BUT_UNWATCHED`, which requires immediate retry/repair of the
  registration;
- a required capability missing with no safe fallback ⇒ `BLOCKED`.

Never a background shell loop, a blind `sleep`, or a new recurring cron job (each fire is a NEW
session that overlaps its siblings and outlives the epic). And never `DONE`.

## One reconciliation cycle

Every wake — callback, subscription, scheduled fallback, retry, manual resume, initial invocation —
runs the same `reconcileEpic` cycle. **A callback trigger name and a subscription outcome select no
mutation path.** A callback is only a prompt delivery; a subscription is only an outcome delivery;
neither proves a merge, a green, a failure, or finality. At-least-once delivery means a duplicate
wake must be a no-op, which it is because every step reads authoritative state first.

1. Load persisted state and the progress-comment marker.
2. Re-list sessions for the repository.
3. Adopt matching live sessions by `tracker_id`, else by the `[<TICKET>] <title>` convention — at
   most **one** session per child, never a second.
4. Re-read each in-flight session.
5. Re-read the tracked chat status for the **exact** tracked chat id.
6. Re-read CI/check snapshots.
7. Re-read the actual provider PR state.
8. Re-read each external blocker's current tracker state.
9. Reclassify liveness with `classifyChildLiveness` — the only liveness authority. Route on
   `action`, never on a proxy.
10. Recompute ready tickets with `readyTickets` over eligible-only graph nodes.
11. Apply resume / repair / fail-isolate transitions.
12. Admit to `greens` only children that pass every `greenAdmissionBlockers` condition.
13. Compute exactly one `nextToMerge` target.
14. Re-check that target's external blockers (`mergeBlockedExternalBlockers`).
15. Merge at most one target.
16. Verify provider/session merge state.
17. Move the child ticket to its done state **only after** a verified merge.
18. Update the single progress comment after every transition.
19. Re-arm consumed or expired watches while the child remains in flight.
20. Persist.
21. Evaluate the terminal invariant.
22. Non-terminal ⇒ arm/verify the next wake and yield as `RUNNING` (or the explicit degradation).
23. Terminal ⇒ clean up, write the final summary, then return `DONE`.

### Green admission

`greenAdmissionBlockers(snapshot)` must be empty. All five conditions, each on an authoritative
re-read: checks `passing`; the PR is **not** a draft; the ticket sits in the configured review
state; no partial-slice / `do not merge` marker; the tracked chat has **settled**. Missing evidence
is never admission — an empty snapshot blocks on four counts.

### Merge verification

`recordMerge` requires an explicit `verified: true` and throws otherwise. "The merge call returned"
and "the provider says merged" are different facts, and only the second may write the ticket to
Done. A merge error is never a merge failure until the provider is re-read: an error can follow a
merge that landed. An unverified target is demoted from `greens` with a note, not recorded.

### Session state carries no push information

A `state` transition, and a `last_check_state` appearing where there was none, both fire when the
daemon re-polls checks that already exist — they move while the remote branch is unchanged. Reading
one as a push puts the driver on merge rails against a branch still holding only its bootstrap
commit. The push oracle is the remote itself:

```bash
git fetch --quiet origin && git rev-list --count origin/<base>..origin/<branch> 2>/dev/null || echo 0
```

Zero means nothing was pushed, whatever the session state says. Keep the guard: before the first
push `origin/<branch>` does not exist and `rev-list` errors with **empty output** instead of
printing `0`. An unmoving remote head means either the child produced no commit or it has local
commits that never pushed; establish whether a push happened before reading the head, and record
which cause was observed. An unmoving remote head is never admissible as evidence of child death —
it is only a `classifyChildLiveness` reason annotation.

## Continuation prompts

Every callback / subscription / fallback payload is a continuation **command**, not a notification.
An informational wording is what let a delivered callback be read as "the child is done, report it"
instead of "run a cycle". `buildContinuationPrompt({epicId, kind, runId})` builds it and
`validateContinuationPrompt(text, {epicId})` checks a payload the driver did not build itself.

The epic id is **interpolated**, never a literal, so the published core carries no example ticket
from any backlog. `CONTINUATION_DIRECTIVES` names the six required rules:

| Rule                   | What the payload must say                                                 |
| ---------------------- | ------------------------------------------------------------------------- |
| `resume`               | resume the epic run rather than answering about it                        |
| `noSummarize`          | do not summarize and stop                                                 |
| `rehydrate`            | rehydrate the persisted run state and the progress marker                 |
| `authoritativeReread`  | treat this wake as a prompt and re-read authoritative state before acting |
| `fullCycle`            | execute one full reconciliation and scheduling cycle                      |
| `noFinalUntilTerminal` | do not return a final answer until the terminal invariant is false        |

For a user request such as "monitor this session", interpret it as "continue the epic run with
durable monitoring" unless the user explicitly asks to pause the epic. Never read it as permission
to end the scheduler, and never as licence to perform one read and stop.

## The driver CLI

The decision surface only — `reconcileEpic` needs live I/O and is driven from the scheduling
process, not from a shell.

```bash
node "$BOSS_EPIC_TOOLBOX/epic-driver.mjs" validate --state <file|->   # {ok, errors}
node "$BOSS_EPIC_TOOLBOX/epic-driver.mjs" blockers --state <file|->   # {blockers, canTerminate}
node "$BOSS_EPIC_TOOLBOX/epic-driver.mjs" status   --state <file|->   # {status, line}
node "$BOSS_EPIC_TOOLBOX/epic-driver.mjs" prompt   --epic <id> [--kind callback|subscription|fallback]
```

`blockers` is the gate to read before any final report: a non-empty `blockers` with
`canTerminate: false` means continue, whatever the last wake appeared to say.
