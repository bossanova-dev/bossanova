# Claim And Eligibility

## Probe-First Contract

Contention is pay-as-you-go: probe for it before paying for arbitration ceremony. Two probes have
already run by the time a claim is posted.

1. **Lock probe** — Step 1's `worktree-lock.sh acquire` returned a fresh `ACQUIRED`, not
   `TOOK_OVER_STALE` and not `HELD_BY_PEER`.
2. **Claim probe** — Step 3's pre-post `readComments` scan found zero peer claim comments on this
   ticket.

Route on the two probes **before** posting, because the contended path's first act — gathering
liveness evidence — has to happen before the claim goes up, not after. The claim probe also stops
being a probe once this run's own comment is in the set: posted first, "zero peer claims" and "only
my claim" are hard to tell apart without token filtering.

**Uncontended fast path** — both probes clear. Assert it with `UNCONTENDED=1`, exported in the same
shell that runs `claim-verdict`. The flag is stated affirmatively and only here, so the default —
including a separate shell that lost the variable — is the contended path, whose missing-evidence
guard then hard-errors. Do not invert this into a `CONTENDED` flag: that spelling fails open, and a
contended run that loses the variable would arbitrate with no liveness evidence at all. Post the
claim, **re-read the comments**, run
`claim-verdict` once on that post-post set, and skip the liveness-evidence snippet below along with
both timed waits (the 20s inline wait that otherwise sits between the post and the verdict, and the
post-`WON` ~10s re-confirm). The fast path drops the waits, never the re-read. `--liveness` is
optional on `claim-verdict`; there is nothing the fast path could have seen for liveness to forfeit,
and a peer claim that lands inside the window is by construction fresh, so first-writer-wins over
the post-post set is the safe answer either way: a stale claim that slipped past the probe on read
lag is yielded to, never overridden.

The re-read is not ceremony and is not skippable. `claim-verdict` over the pre-post set has no
claims in it at all and answers `NO_WINNER` (exit 4) — the fast path would never reach `WON`. Worse,
a run that "reads" its own claim into the pre-post set has each of two concurrent runs adjudicating
a set of exactly one comment, its own, and both conclude `WON`.

Why dropping the _waits_ is safe, stated as an ordering argument rather than a race-freedom claim.
Every run posts before it re-reads. So each run's post-post set contains every claim that was posted
before its own post, and arbitration is first-writer-wins by `createdAt`: the later poster sees the
earlier claim and loses; the earlier poster wins whether or not it sees the later claim. Two
concurrent first claims therefore agree on the winner unless **both** re-reads land inside the
tracker's read-after-write lag for the other's comment. The timed waits bought tolerance for exactly
that lag — that is the whole trade, and it is a bounded window, not a proof of absence. Reject the
fast path if you reject the trade.

Two things bound the residual. A re-read that misses even our own claim yields exit 4, whose
handling is delete-and-retry-once, then stop `NO_CHANGE` — the visibility-lag case that touches our
own comment fails closed. And the fast path is only ever entered from a clean `ACQUIRED` on the
worktree mutex, so a same-worktree peer is already excluded before the claim is written.

**Contended path** — any peer claim comment, or `TOOK_OVER_STALE`, or `HELD_BY_PEER`. Run the full
ceremony unchanged, in this order: the pre-post `readComments`, liveness evidence
gathered **before** the post, the pre-post `claim-verdict --liveness`, the post, the 20s inline wait
after the post, the re-read, `claim-verdict --liveness`, the post-`WON` ~10s timed confirm with a
fresh liveness snapshot, and stale-claim cleanup — with malformed evidence a hard error throughout.
The pre-post verdict is evidenced too: unevidenced it forfeits nothing, so a crashed prior run's
older claim wins it and the run stops `NO_CHANGE` — losing exactly the `TOOK_OVER_STALE` resume the
contended path exists to serve. Do not weaken the contended
path in any way. `TOOK_OVER_STALE` stays on it deliberately — a crashed prior run is exactly when
arbitration evidence matters most.

**Heartbeat cadence follows the same split.** Under contention, refresh the worktree lock at each
step boundary as before. On the uncontended fast path, refresh at long-phase boundaries only — Step
5 implement, Step 6 review, Step 8 repair. That cadence stays inside `worktree-lock.sh`'s staleness
tolerance, and the arithmetic is what makes it safe rather than lucky: a lock becomes
takeover-eligible once `now - eff_heartbeat` reaches `STALE_SECS` (18000 s — 5 h — compared strictly
with `-lt`), while the phase cap bounds any single long-phase interval at ~4 h, so the worst gap is
14400 s. 14400 < 18000 leaves a margin of 3600 s. Relaxing the cadence further, or raising the phase
cap past 5 h, breaks that inequality and has to raise `STALE_SECS` in the same change.

## Selection-Time Eligibility

Step 2's ranked walk is a side-effect-free eligibility pass. The adapter already reads each
candidate and the candidate's canonical native plan attachment before claim time, so the hard-ABORT
gate is checked there too: if the ticket or plan requires work on the exhaustive abort list, the
candidate is skipped and the walk continues.

The skip semantics intentionally match the existing blocker and missing-plan skips. Auto-queue has a
runner-up list, so it keeps walking. An explicitly named ticket has no runner-up and keeps the
existing behavior: it stops `NO_CHANGE` when it does not clear the hard-ABORT list. In both paths,
the gate runs before workspace classification, claim comments, or tracker state moves.

Acceptance criteria that require production access, production credentials, deployed-client IDs, or
an audit of a live deployed environment are not dischargeable from an isolated worktree. They are
ineligible for unattended build selection unless the plan has already replaced that production
precondition with a worktree-local proof.

Epic parents are coordination containers, not buildable implementation targets. In the motivating
parent/child failure shape, a run resolved the parent epic, then claimed a child issue and starved
the child's own session. A run may select a buildable child, or stop when no eligible child exists,
but it must not claim the parent as if the parent's work were directly implementable.

A claim comment is an ownership signal for exactly one issue: the one Step 2 selected, or the one a
human explicitly named. Do not post it on a related child, parent, dependency, or umbrella issue.

## Claim Signals

Read this from Step 2.5 when classifying an existing workspace, and from Step 3 before deciding the
tracker claim. It narrows bootstrap and residue signals; it does not replace the Step 3
claim-verdict / liveness rules.

## Bootstrap Artifacts Carry No Claim Weight

In a bossd-managed session, the run's own bootstrap can produce both of these artifacts before
implementation begins:

- an in-progress tracker state;
- a draft PR on the session branch.

Those bootstrap artifacts carry no claim weight. The claim decision is based on the claim comment
set plus peer-liveness evidence. The worktree lock corroborates local ownership only; it is not the
cross-worktree arbiter. General rule: in-progress with no claim comment is a dead run's residue, not
a live peer.

Apply this rule before treating a PR number, an in-progress state, or a branch as evidence of a
claimed ticket. A bootstrap-only draft PR is adoptable under Step 2.5's fresh/reuse route, and
bootstrap artifacts by themselves are never a reason to declare another runner has the claim.

## Closed-PR Salvage Probe

When no open PR exists for the session branch, or when the tracker read shows multiple PR
attachments with zero claim comments, also probe for a recently closed PR on the same session
branch and list its commits. This salvage probe is diagnostic; neither finding constitutes a claim.

Use the closed PR's `closedAt` value plus its commit count as the discriminator:

- `closedAt` is recent and the commit set is bootstrap-only: there is nothing to resume.
- `closedAt` is recent and the commit set includes non-bootstrap commits: report the salvageable
  commits instead of silently discarding them.

Salvageable commits are evidence to report and assess, not claim ownership. The Step 3 claim
decision still depends on the claim comment set plus peer-liveness evidence.

## Lock Signal

A plain `ACQUIRED` rather than `TOOK_OVER_STALE` is consistent with a fresh worktree behind a dead
prior run. Treat that as corroborating a dead prior run, never as decisive. The decisive inputs stay
the claim comment set and peer-liveness evidence.

Keep the two readings apart. A fresh `ACQUIRED` is the first probe of the Probe-First Contract: it
establishes that no live peer holds _this_ worktree, which is all the fast path asks of it. It is
still not evidence about whether a prior run in this worktree died or finished — that question is
settled by the claim comment set and peer-liveness evidence, never by the lock outcome alone.

## Liveness Evidence Before Claim Verdict

This section states the contended path's requirement. Run it before `claim-verdict` whenever either probe in the Probe-First Contract shows contention; skip it in full on the uncontended fast path.

Before running `claim-verdict`, gather claim-owner liveness evidence from activity timestamps; this is an activity scan, not a lock check and not a `tracker_id` filter.

The worktree lock is per-worktree, so `ACQUIRED` says nothing about a peer in another worktree. A `tracker_id`-filtered session list also is not a detector: it misses a peer that linked itself to a different issue, such as the epic parent instead of this child ticket.

Scan recent sessions in this repository for claim owners with activity inside the staleness window. On the CLI transport, use `boss ls --json` and read `last_agent_activity_at` with `tracker_id` only as context for explaining mismatches, never as the filter that decides whether a peer exists. Where the MCP transport is available, also consult `get_chat_statuses` and its richer `last_output_at` signal.

Transport-preflight consequence: when the run is on the CLI transport, the richer chat-status signal is unavailable. Proceed on the session scan alone rather than skipping the check.

On the CLI transport, build the evidence before the verdict in the same shell as the `claim-verdict` block; a separate shell loses `BOSS_CLAIM_LIVENESS_JSON` and has not run the check. `BOSS_CLAIM_INACTIVE_AFTER_MS` is the staleness window; the default is 20 minutes. Resolve `BOSS` through `toolbox/boss-binary.mjs`, then resolve `REPO_ID` from `BOSS_REPO_ID`, falling back to `"$BOSS" env --json`'s `session.repo_id`, and block if neither is available:

```bash
BOSS="$(
node --input-type=module <<'NODE'
import { pathToFileURL as u } from 'node:url'
const { resolveBossBinary } = await import(u(process.env.BOSS_BUILD_TOOLBOX + '/boss-binary.mjs').href)
process.stdout.write(resolveBossBinary().path ?? '')
NODE
)"
test -n "$BOSS" || { echo "BLOCKED: boss binary unavailable"; exit 1; }
REPO_ID="${BOSS_REPO_ID:-$("$BOSS" env --json | node --input-type=module -e 'let s=""; process.stdin.on("data", c => s += c); process.stdin.on("end", () => { const env = JSON.parse(s); process.stdout.write(env?.session?.repo_id ?? env?.repo?.id ?? "") })')}"
test -n "$REPO_ID" || { echo "BLOCKED: claim liveness repo id"; exit 1; }
RECENT_SESSIONS_JSON="$("$BOSS" ls --repo "$REPO_ID" --json)" || { echo "BLOCKED: claim liveness session scan"; exit 1; }
BOSS_CLAIM_LIVENESS_JSON="$(node --input-type=module - "$RECENT_SESSIONS_JSON" "${BOSS_CLAIM_INACTIVE_AFTER_MS:-1200000}" <<'NODE'
const input = JSON.parse(process.argv[2])
const inactiveAfterMs = Number(process.argv[3])
if (!Number.isFinite(inactiveAfterMs) || inactiveAfterMs <= 0) throw new Error('inactiveAfterMs must be positive')
const sessions = {}
for (const session of input.sessions ?? []) {
  const id = session.id ?? session.session_id
  if (!id) continue
  const lastAgentActivityAt = session.last_agent_activity_at
  if (!lastAgentActivityAt) continue
  sessions[id] = {
    lastAgentActivityAt,
    tracker_id: session.tracker_id ?? null,
  }
}
process.stdout.write(JSON.stringify({ now: new Date().toISOString(), inactiveAfterMs, sessions }))
NODE
)" || { echo "BLOCKED: claim liveness evidence"; exit 1; }
```

On the MCP transport, build the same payload shape from `list_sessions`: each known owner row in `sessions` carries `lastAgentActivityAt`. When `get_chat_statuses` returns `last_output_at` for a known claim owner's chat, merge the richer chat timestamp into that owner's session row as `lastAgentActivityAt` when it is newer than `last_agent_activity_at`; keep unknown claim owners absent from `sessions` so they survive arbitration.

Feed the gathered evidence to `claim-verdict` in the tracker CLI's documented form:

```bash
node "$BOSS_BUILD_TOOLBOX/tracker/cli.mjs" claim-verdict --me "$TOKEN" --comments "$COMMENTS_JSON" --liveness "$BOSS_CLAIM_LIVENESS_JSON"
```

The liveness object is adapter-owned claim evidence, normally including `now`, `inactiveAfterMs`, and a `sessions` map keyed by claim owner session id with each known owner's `last_agent_activity_at` / `lastAgentActivityAt`. Malformed evidence is a hard error on the contended path: stop the run rather than falling back to first-writer-wins without liveness.

A claim whose owner is provably inactive beyond the window is forfeit. A claim whose owner the run could not identify is never forfeited on that basis; unknown owner means the claim survives liveness arbitration.

An explicitly named ticket may override a claim it can prove stale, matching the existing explicit-ID override for `needs-human` and `blockedBy`. It must delete the stale claim comment it overrides, then post its own claim before proceeding. An explicit ID may not override a claim whose owner is live or unknown; that path still stops `NO_CHANGE`.

After an explicit-ID run wins because liveness forfeited an older claim, delete each overridden stale claim comment before proceeding. Use the re-read `COMMENTS_JSON`: parse claim comments with their comment ids, match only comments whose `owner:<sessionId>` has a known `sessions[sessionId].lastAgentActivityAt` where `Date.parse(now) - Date.parse(lastAgentActivityAt) > inactiveAfterMs`, and delete only stale claims that would otherwise sort ahead of this run's token. If a stale override candidate lacks a comment id, stop `BLOCKED` rather than leaving a superseded claim behind. After deletion, re-read comments, rebuild `BOSS_CLAIM_LIVENESS_JSON`, and rerun `claim-verdict`. Never delete live or unknown-owner claims.

Keep the double-confirm pass and the existing `LOST` behavior intact on the contended path. This changes which claims survive arbitration, not what happens after the verdict.

## After WON: Link The Session

Once the claim is WON, link this session to the ticket so the TUI `[l]inear` shortcut opens it — **only when `BOSS_SESSION_ID` is set** (skip under `BOSSD_MANAGED=0`: there is no bossd session to link): call the boss MCP `update_session id=$BOSS_SESSION_ID tracker_url=<issue url> tracker_id=<ISSUE-ID>`, taking the issue url and id from Step 2's `getIssue` read. This is **best-effort and non-fatal** — log and continue on any error; never let it block the run.
