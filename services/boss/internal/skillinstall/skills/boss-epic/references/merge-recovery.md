# Merge recovery and infra-death resume

Situational deep-dives for the three failure shapes the driver hits around the
merge gate and around a chat that stopped for reasons the agent did not choose.
Read the one whose trigger fired; the SKILL.md body carries the decision rule.

## 1. Merge-eligibility gate (the five conditions)

The body's Safety rails state the gate; this is the rationale and the read for
each condition. A ticket is merge-eligible only when **all five** hold:

| #   | Condition                      | How to read it                                                                      |
| --- | ------------------------------ | ----------------------------------------------------------------------------------- |
| 1   | daemon merge gate clear        | `get_session` / the display status reports `gate 1` / `Passing` — **authoritative** |
| 2   | build chat SETTLED             | `get_chat_statuses` idle, no spinner, stable across two polls, real changed files   |
| 3   | PR is not a draft              | `gh pr view <n> --json isDraft -q .isDraft` → `false`                               |
| 4   | ticket in the **review** state | the adapter's `getIssue`, compared against the configured `states.inReview` role    |
| 5   | no do-not-merge marker         | `gh pr view <n> --json title,body` carries no partial-slice / `do not merge` marker |

Why each is load-bearing:

- **(1) is authoritative, not advisory.** The daemon computes the gate from the
  live check set, which varies by tier (a feature PR runs a different check set
  than a release PR). Never re-derive merge-readiness from a hard-coded list of
  check names.
- **(3) draft is the big one.** CI on a draft PR is _expected_ to be noisy or
  partial, so a green draft is not evidence of a finished slice. A prior
  production burn merged a partial-slice draft on green + idle alone. Green on a
  draft is expected CI noise, not merge-eligibility.
- **(4) the review state is the child run's own signal.** A child `/boss-build`
  moves its ticket into the review state only at `REVIEW_READY`; a ticket still
  sitting in the in-progress state means the child is mid-work (or ended
  BLOCKED) no matter what CI says. Resolve the state name from the configured
  state **roles** — never compare against a literal state name, which is
  workspace-specific.
- **(5) the marker is the author's explicit veto.** A child that deliberately
  shipped a partial slice says so in the PR title/body; honor it and skip the
  merge with a progress-comment note rather than merging past the author.

Failing any condition is a **hold**, not a failure: note it on the progress
comment and re-check next cycle. Only the recovery paths below convert a merge
attempt into a repair round or a fail-isolate.

## 2. Merge-commit deadlock (rebase-strategy repos)

**Symptom.** `merge_session` fails with a rebase refusal — typically a
`MERGE_STRATEGY_INCOMPATIBLE` terminal error — while the PR itself reads CLEAN
and the checks are green. Retrying verbatim reproduces it forever: the branch
shape, not the checks, is what the merge strategy rejects.

**Diagnosis.** On a repo whose merge strategy is rebase, count merge commits on
the branch:

```bash
git rev-list --merges --count "origin/<base>..<branch>"
```

Any count `> 0` is the deadlock: a rebase merge cannot replay a merge commit, so
the provider (or the daemon) refuses the strategy outright.

**Recovery.**

1. The daemon auto-squashes this shape when the repo allows squash merges, so
   **re-invoke `merge_session` once**. That single retry is the whole recovery on
   an up-to-date daemon.
2. If the retry still refuses, a driver-side squash of the branch via the VCS CLI
   is the last-resort **manual** path — in policy only when the repo allows squash
   merges. Otherwise hold the ticket and name the deadlock in the report; do not
   invent a merge strategy the repo has not enabled.

**Prevention (the linear-history invariant).** A `git merge` of the base ref into
a session branch is forbidden — refresh with `git pull --rebase` (or a rebase onto
the updated base) so the branch keeps a linear history. This is the same invariant the
`boss-repair` / `boss-finalize` contract enforces; a repair round that merges the
base back in is what manufactures the deadlock in the first place.

## 3. Stranded merge mid-run

`merge_session` can fail _after_ the merge has actually landed — the provider
merged the PR and the error is about the bookkeeping that followed. Treating that
error as a failed merge either re-merges (noise, or a second squash commit) or
fail-isolates a ticket whose work is already on the base branch.

**Rule.** On **any** `merge_session` error, re-read the PR's actual provider
state before deciding anything:

```bash
gh pr view <n> --json state,mergedAt -q '.state'
```

- `MERGED` → the merge landed. Complete the bookkeeping exactly as the success
  path would: move the ticket to its done state, fold the id into `merged` (plus
  `externallyCleared` for a non-node adopted child), release the merge slot, and
  refresh the base. **Never re-merge and never fail-isolate.**
- anything else → the merge did not land; route by the error (a not-passing /
  conflict error demotes to a repair round, a rebase refusal is §2 above).

`merge_session` is idempotent — invoking it against an already-merged PR returns
success rather than erroring — so the safe generic move for an unclassified merge
error is: **re-read the provider state, then retry once.**

## 4. Infra-death wake-to-resume

A chat can stop for a reason the agent never chose: the harness died mid-turn on a
transient API error. That is **not** BLOCKED, and the standing ban on nudging a
BLOCKED chat does not apply to it.

**Diagnosis cheat sheet** — all of these together, not any one alone:

- the session reads idle / the chat is not working;
- the chat's **last message is a transient API error** (a 5xx / overloaded /
  connection-reset style failure), not an agent conclusion;
- no spinner and activity is frozen — nothing has advanced since that message;
- the work is unfinished: no terminal state was reported, and the branch may
  carry committed work from before the death.

**Rule.** Deliver **one** wake-to-resume into that chat:

```text
send_chat_message {agent_session_id: <chat>, wake_if_asleep: true, submit: true,
  message: "continue from committed state; do not restart completed work"}
```

If the chat does not resume — or re-errors — within one poll cycle, **fail-isolate**
(leave the session open for a human). One wake, then the normal isolation path; do
not loop.

**Why this is not the forbidden BLOCKED-nudge.** BLOCKED is an _agent decision_ to
stop: the run reached a conclusion it could not act past, and nudging it just
overrides that judgement, which is why the body forbids it. An infra-death is the
_harness_ dying mid-work — there is no decision to override, only a turn that never
finished. The daemon may also auto-resume a transient API death on its own; the
driver's single wake is the belt-and-braces path when it has not.
