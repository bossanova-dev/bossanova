# Core spine — the portable implement-one-ticket mechanism

This reference captures the project-agnostic **spine** of the implement-one-planned-ticket
workflow: the mechanism that stays the same no matter which tracker, which finalize policy, or
which daemon a project wires behind it. The skill body carries the project wiring (the tracker
capability the ticket is read from, the finalize commands, the daemon handshake); this reference
carries the reusable shape those wirings instantiate.

Read it once to orient. Six elements make up the spine.

## 1. Three terminal states — and nothing else

Every run ends in exactly one of three honest terminal states. There is no fourth, and success is
never "merged" or "done" — terminal success is a review-ready pull request handed to a human.

- **review-ready** — the pull request is open and green, the ticket has moved to its
  awaiting-human-merge state, and the pull request URL is recorded on the ticket. This is the only
  success state, and it is reachable only when no _required_ item was deferred.
- **blocked** — the ticket is left in its in-progress state with a comment naming, at
  file-and-line precision, what failed and what was tried; if work was pushed it stays a draft.
  A blocked run self-quarantines so a later run (or a human) can resume it.
- **no-change** — there was no eligible candidate, the claim was lost with no runner-up, a foreign
  branch already carried real work that is not this ticket's, a peer already held the workspace
  lock, or nothing committable remained after claiming (restore the ticket to its planned state).

The invariant that keeps the states honest: **a deferred _required_ item forces blocked, never
review-ready.** Required means a change the project's own gates treat as mandatory (an observable
API-surface change that needs a version bump, an open must-fix review finding). Optional items
(minor findings, best-effort proof capture) never flip the terminal state.

## 2. Subagent-driven, test-first implementation

The implementation phase is driven by a sequence of **fresh, single-purpose subagents**, one per
plan task, each awaited to completion before the next is dispatched — never run in the background.
This keeps each task's reasoning isolated and prevents one task's context from contaminating the
next.

Each task subagent returns a **fixed, short contract** — the task id, the files it touched, the
tests it added and their pass state, the interface signatures it produced, any residual risk, and the
commits it made (short SHA + subject, or an explicit _no commit — verification only_ note).
The orchestrator threads only that short contract into the next task's brief; it never pastes a
prior task's full transcript forward. Larger hand-offs (the task brief, a report file, a review
package) travel as files, not as inline text.

Every subagent writes the failing test first, watches it fail, then writes the minimal code to make
it pass — red, green, refactor. A test that never failed proves nothing. Model effort scales to the
task: the cheapest tier only for pure transcription where the plan already carries the complete
code, a stronger tier wherever judgment is involved.

When the ticket touches a user-facing surface, the means to prove the change belongs to the task
itself — the affordances a later capture step needs (a stable entry point, a fixture, a test
handle) ship _with_ the feature and pass through the same review, rather than being bolted on
afterward.

## 3. A bounded, whole-branch review stack

After the work is committed, the entire branch — not just the last task's diff — goes through a
review stack, dispatched to one fresh awaited subagent that owns the whole protocol and fixes every
must-fix finding locally before returning:

- a **bounded whole-branch review loop** with an explicit round cap and an oscillation guard, so
  review converges instead of looping forever;
- an **outside-voice, cross-model pass** — a second, independent model reviews the same diff, so a
  blind spot in one model is caught by another;
- a **project review pass** through the project's own review methodology.

The review baseline depends on the run shape: a fresh run reviews the work added since the run
began; a resumed run reviews the whole branch against its base, because a prior run's commits are
part of what ships.

## 4. Sentinel-file verdict routing

The review verdict is routed through a **file the review subagent writes**, never through the prose
it returns. A returned summary can be hallucinated or truncated; a subagent that dies mid-run
returns nothing at all. Routing on a sentinel file makes both failure modes safe:

- the subagent's byte-stable terminal line (clean, or capped-with-open-must-fix) is written to a
  per-run file as its last action;
- the orchestrator classifies **only** from that file;
- a missing or stale sentinel is a distinct dispatch-failure that routes to the safe non-clean
  branch — it is never treated as clean.

Clean proceeds to the pull-request gate; capped and dispatch-failure both route to blocked with the
open items recorded.

## 5. No bulk output in the orchestrator

The orchestrator's context is a scarce, re-charged resource: anything pasted into it is paid for
again on every later turn. So bulk material — full diffs, check logs, review threads — **never**
enters the orchestrator. It is read inside a subagent that returns a short summary, or filtered down
to the few relevant lines before it is read. Every review, repair, and finalize dispatch keeps its
bulk in its own context and returns only the verdict or a short summary. Avoiding re-charged bulk is
the fixed cost this whole design exists to eliminate.

## 6. Resume-or-adopt classification

An existing branch or pull request is not automatically a stop condition. The distinguishing
question is **real work**, not the branch name:

- a branch or pull request carrying no real work yet (only a bootstrap commit, or empty) holds
  nothing to clobber and is **always adoptable** — reuse it;
- a branch that already carries real work must prove it is _this ticket's own_ (by branch name,
  ticket id, or a recorded ticket link) before it is touched — build on top, never revert;
- a branch carrying real work that matches no ownership signal is **foreign**: never co-edit it;
  yield with no writes.

A resumed run assesses what the adopted work already satisfies and narrows the implementation scope
to only the remaining acceptance criteria, then reviews the whole branch against its base.

---

Each element above is instantiated by the skill body with the project's concrete wiring: which
tracker capability supplies and claims the ticket, which commands inject the tag and ready the pull
request, and whether a managing daemon is present. The spine is the same regardless; only the
wiring changes.
