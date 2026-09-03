# Review stack (Step 6) — full protocol

Read this when running the whole-branch review (Step 6 of `SKILL.md`). It is the detailed protocol
the review subagent executes: **one** `boss-review` pass over the whole branch, and the routes the
verdict it produces travels out on. The orchestrator dispatches the **entire** protocol to one fresh
awaited `general-purpose` subagent (**await**, **never** `run_in_background`); if that dispatch
fails (a tool error), the orchestrator runs this protocol inline as an awaited, non-fatal fallback —
at the **full** tier or the [**quick** tier](#quick-tier-minimal), chosen by the same rule the
dispatched path uses, from the same branch diff, with the same answer.
Whichever **path**: same protocol, same lenses, same round caps, same reviewers.
Whichever **tier**: same must-fix categorization, same run-file sentinel, same non-clean routing — a
tier reduces coverage, never the gate. A **missing** or stale run-file sentinel is a different
failure and is untouched by the tier rule: it stays a `dispatch-failure`, published with its own
`none:` coverage token.

**One review system, not three.** This protocol runs `boss-review` and nothing else. There is no
second whole-branch loop stacked in front of it and no separate outside-voice chain behind it:
`boss-review` already carries the specialist lens passes, the repo-local review rounds, a cross-model
`second-voice` round, its own capped fix loop with an oscillation guard, and a round cap. Reviewing
the same diff again from this file would re-review work already reviewed and price a second budget
for it. **The per-run reviewer-dispatch bound is at most four reviewer dispatches (≤ 4) per run**,
counted over the awaited legs **this protocol itself starts**: the review subagent, the one
`boss-review` pass it runs, the conditional API-surface classification, and — only where that first
dispatch failed — the inline fallback pass that replaces it. It bounds this file's own fan-out and
deliberately **not** what happens inside `boss-review`: that pass's detection tiers, its
`second-voice` round, its round extensions and its fix→confirm rounds are its own, capped by its
`$MAX_ROUNDS` (default **3**) and its oscillation guard, and nothing here can halt that pass
mid-flight. **Read the two caps as nested, never summed.** Counting the pass's internal rounds
against this number is how a reader arrives at "the enumeration already exceeds four" and re-prices
a bound that was never wrong — those rounds are funded by `boss-review`'s own `$MAX_ROUNDS`, which is
why they are not counted here. The quick tier spends fewer still — one pass, its optional rounds
skipped. A protocol that wants a fifth dispatch of its own
does not get one: it caps and routes, which is the whole point of a bound. Anything that would
re-introduce a second complete review system here — a second loop, a second cross-model chain, a
second reviewer prompt of this file's own — is a regression, not an addition.

**Mark every reviewer dispatch.** Each of the awaited legs counted by that bound — the review
subagent, the `boss-review` pass, the conditional API-surface classification, and the inline
fallback — leads its worker prompt with exactly `[bs-reviewer-dispatch]` on a line of its own. The
marker is inert text, not an instruction to the worker: run-cost telemetry counts reviewer
subagents by matching it at the head of a dispatched prompt, so an unmarked dispatch is invisible to
`boss cost` rather than merely unreported. Read that count as fan-out made **observable**, never as
an audit of the bound above: `reviewer_dispatch_count` rolls every marked dispatch in the run's
descendants into one whole-tree total — `boss-review`'s own lens and round dispatches included —
so a compliant run routinely reports well above four. That is the same nested-never-summed reading
as above, seen from the telemetry side. The ≤ 4 bound is a protocol invariant this file asserts;
no `boss cost` arithmetic checks it.

The review subagent RETURNS a short structured result: the **rendered `boss-review` report** (the
markdown captured in the review pass, leading with the `<!-- bs-review -->` marker), the
`## Cross-model review` outcome token (the outcome of `boss-review`'s Phase D `second-voice` round),
the `## Review coverage` outcome token (below), the
**base-drift note** from **every** round boundary that read a hit — a refreshable or an
unrefreshable one (§Base-drift check below) — and, when no boundary did, the last boundary's note.
Return every hit rather than the latest note: the check keeps reading after it rebases, so a run
that hits at the check point and then reads `Base drift: none.` afterwards would otherwise return
the `none.` and lose the one boundary that mattered, which is the exact loss this check exists to
prevent. Last, the
finding ledger. The note is returned because the orchestrator — not this subagent — owns the PR
body's `## Autonomous decisions` section, so a note that stays in here reaches no reader on the
clean route at all. Bulk material — round-by-round review
transcripts, diffs, second-voice output, `boss-review` lens output — stays in the subagent's context
and is **NOT** pasted back.

**Write your terminal verdict to the run file (the run-file sentinel convention) — this, not your returned prose, is
what the orchestrator routes on.** The orchestrator provisioned a per-run sentinel context, passed
you `RUN_DIR` and `RUN_ID`, and **seeded** that file with a provisional pessimistic `capped 1`
before it dispatched you. Write the real line **the moment the blocking verdict is determined**, and
re-affirm the same line as your **last action**:

```bash
SENTINEL="$RUN_SENTINEL"
CAPS="$BOSS_BUILD_TOOLBOX/bs-review-caps.mjs"
node "$SENTINEL" write "$RUN_DIR" "$RUN_ID" review \
  "$(node "$CAPS" sentinel clean)" '{"provisional":false}'   # clean; or: sentinel capped <N>
```

**Write it when it is known — a last-action-only write is the defect, not the contract.** Deferring
the write to the end makes it one unguarded step at the close of a long, expensive dispatch, so
anything that ends you between _verdict determined_ and _verdict written_ destroys a verdict for
work that was really done, and the run is forced to publish `coverage unknown` over a review that
really settled something. Write at
each point below, then re-affirm at the end. Rewriting is always safe: the writer replaces the run
file wholesale rather than appending to it or refusing a second write, so re-stating a value costs
nothing and replacing one with a different value is equally well defined.

- **The review pass reported clean** — `boss-review`'s Phase 7 report carries zero open must-fix,
  and the conditional API-surface check has also run → write `sentinel clean` **there**, the moment
  that report is in hand, not after composing the return.
- **The review pass capped** — `boss-review`'s Phase 6 fix loop ended with open must-fix, its
  oscillation guard tripped, its round cap was reached, or a leg budget went non-positive → write
  `sentinel capped <N>` **there**, `<N>` = the rounds reached (a positive integer; the helper
  rejects `0`).
- **The pass did not report at all** — it errored, timed out, or returned nothing structured → write
  `sentinel capped 1`. An empty result is **not** a reviewer that found nothing.

Every one of those writes carries `'{"provisional":false}'`. The marker is always present and
explicit rather than inferred from absence, so the orchestrator can tell a cap a reviewer earned
from the seed nobody upgraded — see §REVIEW_READY-with-findings publication's per-arm token table
for what the latter publishes. Read `false` for exactly what it says: something **after** the dispatch authored this
line. It is not on its own evidence that a reviewer earned it — the pre-dispatch decline route and
the quick tier's did-not-report path write `false` too, and no lens ran on either. Whether a lens
ran is §PARTIAL-route publication's T1 question, not the marker's.

**`boss-review`'s own sentinel line IS this verdict now — do not demote it to advisory.** The pass
prints a `bs-review clean:` or `bs-review capped:` line of its own, generated by the same
`bs-review-caps.mjs` helper. When `boss-review` is dispatched with `RUN_DIR`/`RUN_ID` supplied it
writes that line into the run file itself (see `boss-review`'s Phase 7 caller sentinel contract), and
this protocol's job is to **confirm** the line landed rather than to re-derive a competing one. When
it was not — an inline fallback, or a pass that died before its own write — write the line here from
the report you hold, using the same helper. Never hand-write a sentinel literal:
`matchSentinel` classifies a capped line only when it carries the helper's full `after <N> rounds.`
tail, so an improvised line is **present but unmatchable** → `dispatch-failure`. And
`bs-review-caps.mjs` only prints to stdout, so generating the line without piping it into
`bs-run-sentinel.mjs write` leaves the run file **absent** → `dispatch-failure` again.

The orchestrator classifies this file with `matchSentinel` and never reads your reply — so if
you write nothing (a crash or watchdog kill), the orchestrator's provisional seed is what it reads,
which routes to the safe non-clean branch, never clean.

## Round Scope Contract

Step 6 delegates the actual review to `boss-review`, but this caller owns the durable review trace. A
delta-aware `boss-review` report carries a per-round scope object: `mode`, `base`,
`mergeBase`, `reviewedFiles`, `carriedClaims`, and `briefBytes`. Round 1 is always `mode=full` with
`base` equal to the true merge base. Rounds 2 and later may use `mode=delta` with `base` equal to the
previous round's recorded tip, but only when the round-state helper can prove the state is complete
and consistent.

The brief for a delta round is two-part:

- `git diff <base>..HEAD`, where `<base>` is the previous round's recorded tip;
- the cumulative carried claims list, each `{findingId, file, anchor}` with a greppable anchor.

The carried list includes fixed, verified, and still-open must-fix claims, accumulates across the
run, and drops a row only when a later round re-closes it. A missing or non-greppable anchor, base
movement, a recorded tip that is no longer an ancestor of `HEAD`, an unreviewed fix file, or
unreadable round state escalates the next round to `full`.

The PR-visible `boss-review` comment must show mode, diff base, and carried-claim count per round
and must include full-branch baseline bytes alongside the resolved-mode total.

## Step 6 entry — review tier selection

Pick the review **tier** here, at Step 6 **entry**, before the reviewer is dispatched. Step 6 entry
is a phase boundary that can still be abandoned cheaply; a tier discovered to be wrong mid-pass
cannot be re-chosen.

The tier is decided **from the diff** — what this branch changed — and never from a clock. Two runs
over the same branch diff pick the same tier whatever hour they start at, whatever host they run on,
and whatever the run has already spent. That determinism is the point. A tier that can be argued
either way is a tier that will always be argued cheap, and a tier keyed to a wall clock makes the
depth of a review a fact about the scheduler rather than about the code: the same diff earns a full
pass on a fast morning and a minimal one on a slow afternoon, and neither reading is checkable
afterwards.

**Do not re-introduce a wall-clock term into this rule** — not as an extra branch, not as a
tiebreaker, not as a "only if the clock allows" clause hung off the full tier. The per-step
allowances further down this file (`STEP_6C_DEADLINE`, the API-surface clamp, the quick tier's
reviewer clamp) are a different mechanism and stay: they cap what one dispatch may spend, they never
choose the tier.

Bind three inputs. All three come from the repo — the `.boss-skills.json` config read through
`toolbox/skill-config.mjs`, and git — and **none** from the dispatch brief, so the **dispatched**
path and the **inline fallback** compute the identical answer with no plumbing between them:

- **`reviewDefaults.forceFull`** — the operator override
  (`reviewDeltaDefaults(config).forceFull`, shipped default `false`).
- **The lens globs** — every `glob`/`globs` matcher in the config's `lensMap`, applied through
  `lensesForFile(config, path)`. A changed path "matches a lens" when that call returns a non-empty
  list.
- **`reviewDefaults.deltaFileThreshold`** — the changed-file count at and above which the diff is no
  longer small (`reviewDeltaDefaults(config).deltaFileThreshold`, shipped default **20**).

```bash
# Read the diff in its OWN command, never as an assignment prefix: a prefix's command
# substitution has its status discarded (errexit included), so a failed `git diff` would
# reach the matcher as an empty list and emit a well-formed `quick` — branch 2 unreachable.
# Re-derive `BOSS_BUILD_TOOLBOX` first, the way §Base-drift check does and for the same reason:
# SKILL.md exported it in the ORCHESTRATOR's shell, and shell state survives neither between Bash
# calls nor into this dispatched subagent. Left unguarded the helper below aborts on
# `undefined/skill-config.mjs`, `REVIEW_TIER_JSON` clears, and EVERY dispatched run takes branch 2
# — the full tier, recorded as an unreadable diff that was in fact perfectly readable, with the
# quick tier unreachable on its primary path.
if [ -z "${BOSS_BUILD_TOOLBOX:-}" ]; then
  for candidate in "$HOME/.claude/skills" "$HOME/.codex/skills"; do
    if [ -d "$candidate/boss-build/toolbox" ]; then BOSS_BUILD_TOOLBOX="$candidate/boss-build/toolbox"; break; fi
  done
fi
export BOSS_BUILD_TOOLBOX
CHANGED_OK=yes
if [ -z "${REVIEW_BASE:-}" ]; then
  CHANGED=''
  CHANGED_OK=no
else
  CHANGED="$(git diff --name-only "$REVIEW_BASE"...HEAD)" || CHANGED_OK=no
fi
export CHANGED

if [ "$CHANGED_OK" = no ]; then
  REVIEW_TIER_JSON=''                  # unreadable diff → branch 2 → full tier
else
  REVIEW_TIER_JSON="$(
    node --input-type=module -e '
      import{pathToFileURL as u}from"node:url"
      const m = await import(u(process.env.BOSS_BUILD_TOOLBOX + "/skill-config.mjs").href)
      const cfg = m.loadSkillConfig()
      const { deltaFileThreshold, forceFull } = m.reviewDeltaDefaults(cfg)
      const files = (process.env.CHANGED || "").split("\n").filter(Boolean)
      const lensHit = files.some((f) => m.lensesForFile(cfg, f).length > 0)
      const tier = forceFull || lensHit || files.length >= deltaFileThreshold ? "full" : "quick"
      process.stdout.write(JSON.stringify({ tier, forceFull, lensHit, changedFiles: files.length, deltaFileThreshold }))
    '
  )" || REVIEW_TIER_JSON=''
fi
```

`REVIEW_BASE` is the same base this protocol reviews against everywhere else — resolved once in
Preflight, re-checked by §Base-drift check. Use the three-dot form so the count is this branch's own
changes, not everything the base has landed since.

**Two shell values are the exception to "no plumbing", and the block guards both.** The rule's three
inputs are repo-local, but the diff it reads is not free: `REVIEW_BASE` arrives in the dispatch brief
and `BOSS_BUILD_TOOLBOX` was exported in the orchestrator's shell, and neither survives into this
dispatched subagent. That is why the block re-derives the toolbox and fails an unset `REVIEW_BASE`
closed to branch 2 instead of reading either as evidence about the diff. The two guards fail in
opposite directions, so neither is redundant. Drop the toolbox guard and the dispatched path takes
branch 2 on every run while the inline fallback still reaches branch 3 — the two paths then disagree
on exactly the diffs the quick tier exists for. Drop the `REVIEW_BASE` guard and it fails the other
way, toward the cheap tier: `git diff --name-only ...HEAD` with an empty left side is a valid,
exit-0 request for the **empty** diff, so `CHANGED_OK` stays `yes` and a run that never read its
change set lands in branch 3 and buys the quick tier.

Evaluate the branches below **in order** and take the **first** one that matches. The order is load
bearing: the override must be resolved before the diff is read, and an unreadable diff before either
matcher, or a run that could not read its own change set would be classified by a matcher fed an
empty list — which is exactly the shape that selects the cheap tier.

1. `reviewDefaults.forceFull` is **true** → **full tier**. The operator override wins over
   everything below it; no lens result and no file count can demote it.
2. The diff is **unreadable** — the `git diff` failed, the helper exited non-zero, or
   `REVIEW_TIER_JSON` is empty or does not parse → **full tier**. Ambiguity resolves toward **more**
   coverage, never less: an unreadable diff is not evidence of a small one, and treating it as one
   would make the cheap tier the default on every run whose base ref went missing. Record the reason
   in the `## Review coverage` line, but do not stop: an unreadable diff selects a tier, it is not
   itself a BLOCKED condition.
3. **No** changed path matches **any** configured lens glob **and** the changed-file count is
   **strictly below** `deltaFileThreshold` → **quick tier (minimal)**, defined below.
4. Otherwise → **full tier**. Run the rest of this reference unchanged.

The comparison in branch 3 is **strict**, and deliberately so: exactly `deltaFileThreshold` changed
files is the **full** tier, because the threshold names the count at which a diff stops being small
rather than the last count that still is. An **empty** diff — zero changed files, no lens hit — lands
in branch 3 and selects the quick tier; that is correct and not a special case, because there is
nothing for a full pass to find, and the quick tier still runs a real reviewer and still gates on
what it reports.

**A single lens hit is enough.** Branch 3 needs _every_ changed path to miss _every_ glob. One
changed file under one configured lens selects the full tier however small the diff is, because a
configured lens is the repo saying that area needs the specialist pass — a file count cannot
overrule that.

**An unusable glob matches.** If a `lensMap` entry's matcher cannot be compiled or applied, treat
that entry as matching every path, so the tier lands on **full**. Same fail-safe direction as branch
2: a matcher nobody can evaluate is not evidence that nothing matched it.

**Where each path evaluates it.** Because every input is repo-local, whoever runs this protocol
evaluates the rule themselves: the review subagent at the top of the dispatched protocol, the
orchestrator on the inline fallback. Neither needs the other to have measured anything first, and
there is no snapshot to go stale between them. That is a direct consequence of keying on the diff —
a clock-keyed rule needed the value carried in the brief and re-derived against an absolute deadline;
this one needs neither.

**Pre-dispatch decline route.** There is exactly one condition under which **no** reviewer is
dispatched at all, and it is not a tier: the `BOSS_BS_REVIEW=0` off switch below. A tier never
declines a review — the quick tier reduces coverage, it does not skip the pass — so if you find
yourself about to publish "no reviewer ran" for any reason other than that switch or a dispatch
failure, you have mis-taken a branch above. On the decline route, write the capped sentinel to the
run file, then route to **§REVIEW_READY-with-findings publication**, publishing `## Review coverage`
= `none: review stack did not run (<reason>)` — and **push the branch first**, per the push rule that
section shares with §BLOCKED-route publication. A stack that was never entered is a coverage fact,
not a defect; it reaches `BLOCKED` only if that push or the quality gates fail. **Generate the line
through the helper _and_ persist it through the run-file writer** — run the whole command, not either
half:

```bash
node "$RUN_SENTINEL" write "$RUN_DIR" "$RUN_ID" review \
  "$(node "$BOSS_BUILD_TOOLBOX/bs-review-caps.mjs" sentinel capped 1)" '{"provisional":false}'
```

Both halves are load bearing, and dropping either lands on the **same** wrong outcome from opposite
directions. `matchSentinel` classifies a capped line only when it carries the helper's full
`after <N> rounds.` tail, so an improvised `capped: <N>` line — with or without a round count — is
unmatchable: **present but unmatchable** → `dispatch-failure`. And `bs-review-caps.mjs` only prints
to stdout, so generating the line without piping it into `bs-run-sentinel.mjs write` leaves the run
file **absent**: missing sentinel → `dispatch-failure` again, by the other sub-case. Either way the
published token claims the coverage was _unreadable_ or _unknown_ when you know no reviewer ran at
all. Pass `1`, not `0`: the helper requires a **positive** round count and exits non-zero on `0`.

**Push before you write it.** The two steps above are stated in reading order, not execution order:
§BLOCKED-route publication's own pre-dispatch decline route governs the sequence, and it requires the
push **first** — only `PUSHED=yes` may then write this generated `sentinel capped 1` and publish its
tokens, while `PUSHED=rescue` or `PUSHED=no` takes that section's BLOCKED reporting instead. Where
this paragraph and that one appear to disagree on ordering, **that one governs**; a decline verdict
written over commits no reviewer can fetch records a decision about work nobody else can see.

So take **§REVIEW_READY-with-findings publication** below and run it to the end: **push the session
branch first**, through §BLOCKED-route publication's retry/rebase/rescue procedure until
`PUSHED=yes`, then write the sentinel above, publish both never-omit tokens and exit cleanly
`REVIEW_READY` on a green pushed branch. `PUSHED=rescue` or `PUSHED=no` reports BLOCKED cause (2)
instead — the push is the gate, not the review. This route stops **before** Step 7, and Step
7 is the only step that pushes — so the commits this route is standing on are stranded unless it
pushes them itself.

The choice is **recorded either way** — see the `## Review coverage` token below. A tier is never
chosen for any other reason: it is not an operator preference beyond `forceFull`, not something the
run's elapsed time can influence, and not something a reviewer's own findings can trigger. Anchoring
it to a rule a reader can re-evaluate from the diff, and publishing the result, is what stops the
cheap path from being the invisible default.

**Allowance-disclosure rule — a per-step allowance that declines work must name two numbers.**
Several gates in this reference are bounded by a **per-step allowance**: a deadline stamped for one
dispatch and spendable only by that dispatch — `STEP_6C_DEADLINE`, the API-surface clamp, and the
quick tier's reviewer clamp. Whenever one of those **declines work** — refuses a fix round, skips a
pass, stops a loop early — whatever it publishes must state, as **two separate numbers**, the
**allowance** that actually declined it and the **cost of the work it declined**. And it must never
phrase an inner box as the _run_ being out of time: this skill holds no run clock to be out of, so
"there was not enough time left" names a budget that does not exist and sends the next reader hunting
for it. Write "a 15-minute pass allowance had 412s left and a fix round costs 1200s" — allowance and
cost, both named — so a reader files a designed bound as a designed bound instead of as a budget bug,
and does not re-price a formula that was never wrong. This is the enclosing-ceiling failure — **the
clamp costs the diagnostic, not just the budget** — and the remedy is disclosure, not re-pricing:
locate every enclosing ceiling before tuning an inner deadline.

### Quick tier (minimal)

The quick tier is the documented shallow end of the review — a real pass at reduced scope, chosen for
a diff that touched no configured lens and stayed small. It runs:

- **Exactly one** awaited `boss-review` pass over `$REVIEW_BASE...HEAD`, at **reduced scope**: the
  pass is told to run its Phase 1 detection tier and its Phase 7 report, and to skip its optional
  rounds — the Phase D `second-voice` cross-model round and the configured round extensions. Supply
  it the plan/acceptance-criteria, as the full tier's pass does. Awaited,
  **never** `run_in_background`. "Exactly one" bounds the pass, and the pass's own capped fix loop is
  what may repair a finding; nothing here starts a second reviewer of its own.
- **Detection is a single pass.** No detection re-review of this protocol's own, no outside-voice
  round, and no extra rounds beyond the ones `boss-review`'s own cap already funds. What may follow
  detection is not a second pass: it is that pass's internal fix loop, running under the same clamp.
- The pass carries a **per-dispatch allowance** as well as a round count, derived from the same
  per-leg timeout every other dispatch in this file is bounded by. A round cap of one is not a time
  bound: one hung awaited pass otherwise blocks the run indefinitely, and a review nobody can
  interrupt is not cheaper than a full one, it is unbounded. Derive the allowance from
  `BOSS_SKILL_EXTENSION_TIMEOUT_MS` exactly as §Step 6 stamps `STEP_6C_DEADLINE`, at the quick
  tier's reduced leg count:

  ```bash
  leg_ms=${BOSS_SKILL_EXTENSION_TIMEOUT_MS:-300000}
  case "$leg_ms" in '' | *[!0-9]*) leg_ms=300000 ;; esac
  leg_ms=$(( 10#$leg_ms ))
  [ "$leg_ms" -gt 0 ] || leg_ms=300000
  DEADLINE_LEG_SECONDS=$(( (leg_ms + 999) / 1000 ))
  [ "$DEADLINE_LEG_SECONDS" -ge 300 ] || DEADLINE_LEG_SECONDS=300
  QUICK_REVIEWER_LEGS=2                                      # detection + report; no optional rounds
  QUICK_REVIEWER_SECONDS=$(( QUICK_REVIEWER_LEGS * DEADLINE_LEG_SECONDS ))
  ```

  Two legs, not the full tier's three, because this tier skips the optional rounds — the allowance
  shrinks with the scope it funds, and it is the **only** number that shrinks. Nothing here reads a
  run clock, and nothing here may borrow from one: the allowance is a property of the dispatch, so it
  is the same on the first minute of a run and the fourth hour.

  **State it in the brief** as a hard return-by — stamp the deadline from **this** number,
  `STEP_6C_DEADLINE=$(( $(date +%s) + QUICK_REVIEWER_SECONDS ))`, under that exact interface name so
  `boss-review` binds the same gate. Do **not** re-run §Step 6's stamping block here: it hardcodes
  `STEP_6C_INITIAL_LEGS=3` and would silently restore the full tier's allowance, leaving
  `QUICK_REVIEWER_SECONDS` computed and unread. A budget the holder never states bounds
  nothing, which is how a clamp ships inert. On expiry take the **same** route a non-reporting pass
  takes below — `bs-review capped:` → §REVIEW_READY-with-findings publication — never a clean exit.
  That route is already the documented outcome for a pass that produced nothing, and a pass stopped
  by its own allowance is one of those.

- **The pass's optional rounds are skipped by policy**, named here so a reader can tell a policy
  skip from an improvised one. Skipping the `second-voice` round costs the outside voice, and
  skipping the round extensions costs their lenses; both are real reductions in review depth, and
  that is exactly why the tier must be published rather than chosen quietly.
- The conditional **API-surface check** below **still runs**, before this
  tier's clean exit. It belongs to the gate, not coverage: a missing required version bump is a
  must-fix here exactly as in the full tier, never a Minor and never silently dropped. It carries its
  own per-dispatch allowance, stated as a cooperative hard return-by in its brief, on the same
  derivation §API-surface check gives. A required gate may not be skipped to save time, at either
  tier.

**Its findings still cap the pass.** The pass categorizes exactly as the full tier does: must-fix =
Critical + Important, deferred = Minor. Unless its own fix loop cleared a finding and its
confirming round verified the fix, **any** must-fix finding is recorded by `file:line` and routed
through the **same run-file sentinel** the full tier writes — `bs-review capped:`, which publishes
those findings through **§REVIEW_READY-with-findings publication** rather than swallowing them into
a blocker comment. Only a pass that **ran to
completion and found zero** must-fix writes `bs-review clean:`.

That routing is unchanged, but **what a capped sentinel is allowed to mean is not.** A pass writes it
only once every must-fix it holds open has a cause on the finding's own side — attempted and not
cleared, the round cap reached, or ineligible to attempt at all. A must-fix that was located and
never attempted does not qualify: the fix loop owes it one attempt first, funded by the
single overrun round `boss-review`'s §Caller deadline allows for exactly this case. So the capped
verdict this sentinel produces names the finding and why it is still open — never the clock as a
cause in its own right.

**A pass that did not report is not a pass that found nothing.** At this point the tier has
exactly one pass and no second opinion — the optional rounds are skipped — so unlike the full tier
nothing else would notice its absence. If that single dispatch errors, times out, or returns no
structured findings, do **not** read the empty result as zero must-fix — run the same
generate-and-persist command the pre-dispatch decline route uses, with `sentinel capped 1`:

```bash
node "$RUN_SENTINEL" write "$RUN_DIR" "$RUN_ID" review \
  "$(node "$BOSS_BUILD_TOOLBOX/bs-review-caps.mjs" sentinel capped 1)" '{"provisional":false}'
```

That routes to **§REVIEW_READY-with-findings publication** carrying a reduced coverage token; name
the failure in the `## Review coverage` reason. Both halves matter here too: a hand-written capped
line is unmatchable, and a generated line that never reaches the run file leaves it absent — each
downgrades this to a `dispatch-failure` instead of the earned capped verdict this tier owes. This is the run-file sentinel's
own "wrote clean" vs "wrote nothing" distinction applied one level down; collapsing them would let a
branch nobody reviewed reach REVIEW_READY through the cheapest path in the protocol.

**This tier may repair, but it may never self-certify.** Do not fix a must-fix here yourself and then
emit `clean` **on your own assertion**: on a repaired branch `clean` requires the pass's own
confirming round, and nothing else. The reasoning is exactly why that round — and not the change
gate — is the evidence: the change gate re-runs
`make` targets, which cannot confirm a _semantic_ finding was actually resolved, so "fixed it myself,
then declared myself clean" would be self-certification, precisely the unverified-fix path the
confirming round exists to prevent. Shipping `capped` with the finding published is recoverable (a
later run, or the human this PR is now waiting on, repairs it with a real budget); an unverified fix
shipped as `clean` is not.

The quick tier therefore reduces **coverage**, never the **gate**: it must not be able to carry an
open — or a silently self-resolved — must-fix into a REVIEW_READY, and the `SKILL.md`
required-deferred invariant applies to it unchanged.

**Record the tier (always).** Return a `## Review coverage` outcome token to the orchestrator
alongside the `## Cross-model review` token; the orchestrator writes it into the PR body in
Step 7, so a reader never mistakes silence for full coverage:

- `full` — the full tier was selected **and** every round the pass owns actually ran (its detection
  tiers, its fix loop, its `second-voice` round, its round extensions). If a round it owns did not
  run — skipped by its own budget gate, or never reached because the run capped early — name it here
  rather than emitting a bare token: `full (skipped: <round list>)`. The fix loop is the one
  exception, and it is not a skip: at the shipped defaults the pass's stamped allowance funds **0**
  ordinary fix rounds by design (§Step 6, hard-deadline bullet), so a run that ran none for that
  reason emits a bare `full` — never `full (skipped: fix loop)` on every full-tier run.
- `quick: <reason> (skipped: <round list>)` — e.g.
  `quick: no lens glob matched and 4 changed files is below the 20-file threshold (skipped: second-voice cross-model round, boss-review round extensions)`.
  The `<reason>` states the branch of the diff rule that selected the tier, so a reader can
  re-evaluate it against the same diff.
  When the pass's fix loop actually **repaired** findings and its confirming round cleared them,
  append that outcome as a **suffix** parenthetical:
  `quick: <reason> (skipped: second-voice cross-model round, boss-review round extensions) (repaired: <N> finding(s), verified by the pass's confirming round)`.
  That suffix asserts a **verified** repair, so only a run whose confirming round returned zero
  must-fix may publish it. A repair that ran and did **not** clear — the confirming round reported
  must-fix, errored, timed out, returned nothing structured, or its own clamp came back
  non-positive — takes the `capped` route, and the token published there (by
  §REVIEW_READY-with-findings publication, which publishes on exactly that route) must say so rather
  than borrow the verified form:
  `quick: <reason> (skipped: second-voice cross-model round, boss-review round extensions) (repair attempted: <N> finding(s), verification did not clear: <outcome>)`.
  A repair that was **gated out** — a must-fix `boss-review` classed **ineligible to attempt at
  all**, the third of the three lawful cap causes its own terminal-state rules allow — did not run,
  so its token keeps the original skipped list unchanged and its run takes the `capped` route
  rather than emitting a coverage token from a clean exit at all. That ineligibility is the only
  gate that reaches this arm: this protocol prices no repair leg of its own, so there is no
  affordability clamp here to gate one out, and the clock is never a cause in its own right.
- `none: review verdict unreadable (<reason>)` — the orchestrator's token for a `dispatch-failure`
  whose sentinel was **present but unmatchable** and whose subagent returned nothing usable. A tier
  may well have run here, so this says the verdict could not be read — never that no review happened.
- `none: review coverage unknown (<reason>)` — the orchestrator's token for a `dispatch-failure`
  whose sentinel is **missing or stale**. The stack was entered and then left no readable verdict;
  the orchestrator's pre-dispatch seed makes that unreachable from a subagent death alone, so
  arriving here means the seed itself never landed or the run dir was lost — and a kill, timeout, or
  crash anywhere in the stack lands here just the same, including one that struck after the pass had
  already reported. So neither `full`/`quick:` (no tier is known to have
  finished) nor `did not run` (no
  tier is known to have been skipped) is honest, and this token publishes the uncertainty itself. The
  subagent cannot emit it either — it wrote nothing, which is why you are here.
- `none: review stack did not run (<reason>)` — the orchestrator's token for the route where the
  review stack was **never entered**, so no reviewer can have run: no review subagent was ever
  dispatched **and** the inline fallback did not run either, **or** the `BOSS_BS_REVIEW=0` off switch
  declined the pass before any reviewer was started. Decide this from **your own
  record of what you dispatched**, never from the sentinel — a missing sentinel is equally
  consistent with a stack that ran and died, and that case takes `none: review coverage unknown`
  above; a decline is decidable from your own record because declining to dispatch _is_ that
  record. Pair it with `## Cross-model review` = `skipped: <reason>` — the cross-model round lives
  inside the pass, so a stack that was never entered never reached it, and that is a policy skip,
  not an `error`. The subagent
  cannot emit this one — it is not there to — so the orchestrator writes it when it fills the section
  itself. Do **not** reach for `quick:` here: a tier is only ever chosen by the diff rule above,
  and labelling a stack that never started as a tier claims a reviewer ran. That route ships
  `REVIEW_READY` on a pushed, green branch, and this token is what says out loud that **no review
  settled anything** — instead of leaving the section absent, which would read as full coverage.

**A verdict without a token is not `full`.** The run-file verdict and these returned tokens are two
different channels, and because the blocking verdict is now written the moment it is determined
rather than last, a `clean` on disk no longer implies the dispatch survived to report. When the
verdict is `clean` but the dispatch returned **no** `## Review coverage` token, do not improvise
`full`: an invented token claims a coverage nobody measured, on a run whose stack demonstrably did
not finish. Publish `none: review coverage unknown (<reason>)`, with `## Cross-model review` =
`error: <reason>`, exactly as the missing-sentinel route does. This governs only what is
**published**. It is **not** a routing rule and must never become one — routing reads the run file
and nothing else, so that `clean` still proceeds; it simply proceeds saying what it actually knows.

Emit a `full` token on a full-tier run too — the token is never omitted. On a resume, the orchestrator
**replaces** this section rather than appending a duplicate.

**The `## Cross-model review` token (always, same never-omit rule).** This is the **other** mandatory
section, and it has its own four-value vocabulary — `full` is **not** one of them. Derive it from the
ledger row `boss-review` records for its Phase D `second-voice` round, never from the pass's overall
verdict, which covers every other round too:

- `clean` — the round ran and raised no must-fix finding.
- `findings-fixed (per-finding dispositions)` — it raised must-fix findings that the pass's own fix
  loop closed and its confirming round verified; list each finding's disposition.
- `skipped: <reason>` — the round did not run: `quick tier`, `disabled`
  (`BOSS_REVIEW_DEFAULT_ROUNDS=0`), no second-voice agent available, or the pass's own deadline gate
  refused it. An unavailable capability is a skip, never an `error`.
- `error: <reason>` — the round ran and failed. A round whose findings are still open at the cap is
  neither `clean` nor `findings-fixed`: report it as `error: open must-fix at cap`.

This section is never omitted either, and on a resume the orchestrator **replaces** it rather than
appending a duplicate — the same rule the coverage token above carries, applied to both sections.

### The reserved merge-gate token (every route, no exceptions)

`do not merge` is **`boss-epic`'s merge-gate token**: it matches that substring against a PR's
`title,body` and holds the PR back. So on **any** route this file owns — Step 7's normal
publication, §PARTIAL-route publication, §BLOCKED-route publication, and Step 6's hand-off of the
review pass's report — no text sourced from `boss-review` may place that substring in a PR **title
or body**:
not a finding title, not a status phrase, not a summary line, not a quoted excerpt. Emitting it as
review prose wedges a merge nothing intended to wedge, on evidence that was never meant to gate a
merge at all. **Rephrase the finding rather than quoting the token**; open
items belong in the `<!-- bs-review -->` comment, which is where a human reads them.

The one deliberate use is this file's own PARTIAL marker
(`do not merge — partial: <satisfied>/<total> acceptance criteria`): that line is composed _here_, by
the route that means to hold the PR, and is not `boss-review`-sourced. The ban is on relaying the
token out of review text, never on the marker this protocol writes on purpose.

### PARTIAL-route publication

**Why this section exists.** `capped` never reaches Step 7, the sole place that writes the PR body.
A `PARTIAL` described in the state list but given no publication route of its own can therefore never
write a PR body at all: it ships **inert** — a state name with no artifact behind it, every gate
green. This section is that route. Step 7 is also the only step that **creates** a PR, the only one
that **readies** it and the only one that **pushes**, and Step 9 is the only one that measures the
branch green — so this route owns each of those writes and that reading itself. Assuming any of them
already happened is the same inertness defect one level down.

**The gate first — re-check all three conjuncts here.** Arriving at this section is not itself a
licence to publish `PARTIAL`. The T1/T2/T3 gate is specified in
[`finalize-and-stop.md`](finalize-and-stop.md) Step 9, and on the `capped` route **Step 9 never
ran** — so re-check it here, against this route's own artifact:

- **T1 — at least one in-scope acceptance criterion satisfied _and independently certified_**:
  certification is a **positive** record, never an absence. Require both halves — the
  acceptance-criteria certification **ran** over the full supplied criteria list and returned a
  verdict a **reviewer** authored; **and** its findings name no must-fix against the criterion you
  are counting. Ask of the first half only what that reviewer actually emits: it reports the
  criteria the branch does _not_ evidence, so there is no per-criterion "satisfied" line anywhere to
  point at, and requiring one would make `PARTIAL` unreachable and invite the agent to improvise a
  self-certification in its place. What the first half buys is the thing the second cannot check on
  its own — that a reviewer read this branch at all. Reading only the second half makes T1 vacuous
  exactly where it matters most: on a
  **generated** `sentinel capped 1` — the pre-dispatch decline route below, and the quick tier's
  did-not-report path — **no lens ever ran**, so "emitted no must-fix" is true of every criterion
  and a branch nobody reviewed would certify all of them. A `capped` verdict this run generated for
  itself, rather than one a reviewer earned, therefore **fails T1 by construction** and is not
  `PARTIAL`. A run that satisfied **zero** criteria is **never** `PARTIAL` either: `0/<total>` is
  the universal soft landing this state exists to refuse, and an agent's own assertion that a
  criterion is done is never certification. Failing T1 is not a blocker — it disqualifies this
  route, not the run: take §REVIEW_READY-with-findings publication above, which states the unmet
  criteria in the PR body under the coverage token this run actually earned. The orchestrator's **provisional seed** — a `capped`
  whose payload carries `provisional` = `true` — is named here explicitly so it cannot be reasoned
  around: it was authored **before the dispatch**, so no lens ran, no reviewer authored anything,
  and all three conjuncts are unestablishable at once. A provisional-survived verdict is therefore
  **never eligible for `PARTIAL`** and takes §REVIEW_READY-with-findings publication above, under
  its provisional-seed coverage token.
- **T2 — the branch is green.** Step 9's watch never ran on this route, so take the reading here,
  **after** the push and the ready below and against the PR this section publishes:
  run [`callback-watches.md`](callback-watches.md) Protocol step 5's bounded poll over
  `"$PR_NUMBER"` — never a bare `gh pr checks --watch --fail-fast`, which has no timeout of its own.
  Only `CI_WAIT_STATE=settled` is green. Red, `timeout`, `unknown`, or a rollup you cannot resolve, is
  `BLOCKED`. A draft PR cannot supply this reading — its CI is expected to be noisy or partial —
  which is why the ready step below is mandatory rather than cosmetic.
- **T3 — every deferred required item is _only_ an unsatisfied in-scope acceptance criterion.** The
  required set is **Step 9's**, not a shorter one restated here: read
  [`finalize-and-stop.md`](finalize-and-stop.md) Step 9's list whole and apply every member of it.
  Any one left undone — an open must-fix from another lens, a missing API-version bump or
  down-convert transform, a failed reviewed-tip confirmation, uncommitted residue, an untagged
  commit, a hard ABORT — means this is not `PARTIAL`; only the API-version bump and the hard ABORT
  are `BLOCKED` in their own right (causes 3 and 4), and the rest publish through
  §REVIEW_READY-with-findings publication above. Where this route's wording and Step 9's differ **on what that
  required set contains**, **Step 9 governs**: a locally shortened copy is a weaker gate wearing the
  same name, and the weakest copy is the one this live route would otherwise be read against. That
  deference is about the membership of the list and nothing else. Where the two copies differ on how
  **strong** a conjunct is, the **stronger** reading governs whichever file it appears in — a gate is
  never weakened by pointing at another copy of itself.

A conjunct that fails, or that you cannot establish from evidence, leaves this route. Where it lands
is **not** automatic: a red or unresolvable **T2** is `BLOCKED` cause (1), and a push that will not
land is cause (2) — those two take §BLOCKED-route publication below. A failing **T1** or **T3** on a
pushed, green branch is neither: it takes §REVIEW_READY-with-findings publication above, with the
deferred items published rather than swallowed. Never publish `PARTIAL` on two of three.

**T2 is read after publication, so a red reading must _unwind_ before it reports.** The ready step
and both writes below precede that reading — it is unavailable on a draft — so by the time T2 comes
back red the PR already carries the partial title suffix, the partial body and its do-not-merge
marker, and it is **ready** rather than the draft every other BLOCKED path leaves behind. Publishing
`BLOCKED` on top of that and stopping leaves an artifact whose title and body both still claim a
state this run did not earn, and boss-epic condition (5) goes on holding it on the strength of a
marker that is now false. So on a red T2 — and on any non-zero exit from the acquisition block
below, which on a fresh workspace has already created a PR carrying `$PR_BODY` — restore the
non-partial artifact **first**, then report. Compose the replacement body _before_ the write, for
the same reason `$PR_BODY` is composed before its writes: a `--body-file` reaching for a variable
nothing ever assigned fails on exactly the path that needs it, and an unwind that fails is an
artifact left claiming `PARTIAL`.

```bash
# ONLY on a red T2 reading, taken AFTER the writes further down — never run in sequence with them.
# Re-derive: the acquisition block below set $PR_NUMBER in an EARLIER Bash call, and the red T2
# reading that sends you here is a later one — shell state does not survive between them.
SESSION_BRANCH="${SESSION_BRANCH:-$(git branch --show-current)}"
PR_NUMBER="${PR_NUMBER:-$(gh pr list --head "$SESSION_BRANCH" --state open --json number -q '.[0].number // empty')}"
BLOCKED_BODY="$(mktemp)"   # now compose §BLOCKED-route publication's body into this file
```

Stop there and **write the body**, exactly as `$PR_BODY` is composed before its own writes. The two
fences are separate for that reason: run them as one and the `--body-file` below publishes an empty
file, stripping the very `## Cross-model review` and `## Review coverage` tokens whose absence reads
as full coverage. Only with `$BLOCKED_BODY` populated:

```bash
if [ -n "$PR_NUMBER" ]; then
  gh pr edit "$PR_NUMBER" --title "[<ISSUE-ID>] <issue title>" --body-file "$BLOCKED_BODY"
  gh pr ready --undo "$PR_NUMBER"   # this route readied it; BLOCKED never ships a ready PR
else
  echo "no open PR for $SESSION_BRANCH — nothing to unwind"
fi
rm -f "$BLOCKED_BODY"
```

`$BLOCKED_BODY` carries §BLOCKED-route publication's body — its real `## Cross-model review` and
`## Review coverage` tokens, no `## Partial` section and no do-not-merge marker — and the title
sheds the `(partial <satisfied>/<total>)` suffix. The `--undo` is not tidiness: readying was this
route's own write, [`finalize-and-stop.md`](finalize-and-stop.md) Step 9 says a BLOCKED run does
**not** ready, and a ready PR left behind drops boss-epic's double cover to condition (4) alone.
An empty `$PR_NUMBER` here means the create arm itself never landed a PR, so there is no partial
claim published anywhere and nothing to restore — that is the one case this guard passes over, not a
licence to skip an unwind whose lookup merely failed. Only once all three have returned take the
BLOCKED route below. A failed unwind is itself a blocker to name in the blocker comment — never a
reason to leave a partial claim standing on a branch that did not earn it.

**Push first, and only `PUSHED=yes` may take it.** Run the BLOCKED route's push procedure below (the
`PUSHED=yes|rescue|no` block) before publishing anything. Only `PUSHED=yes` may then take the
`PARTIAL` route; on `PUSHED=rescue` or `PUSHED=no` the terminal state becomes `BLOCKED` and the
reporting is §BLOCKED-route publication's, not this one's, because a slice a reviewer cannot fetch is
not a reviewable slice.

**Assemble the body first — both writes below consume it.** The create arm and the edit arm each
take `--body-file "$PR_BODY"`, so compose the whole body (every element listed further down, none
omitted) into a temp file _before_ either runs, exactly the way Step 7 does. Ordering is the defect,
not a style preference: a route that acquires the PR first and only then reaches for `$PR_BODY` is
passing an unset variable on the very fresh-workspace path this section exists to serve, and
`gh pr create --body-file ""` fails there before anything is published:

```bash
PR_BODY="$(mktemp)"   # compose the body elements listed below into this file, before either write
```

Remove it (`rm -f "$PR_BODY"`) once both writes have returned.

**Get a PR to write to, and ready it.** This route fires on fresh workspaces too, where no PR exists
because Step 7's `gh pr create` was never reached; a section that only knows how to _edit_ a PR
dead-ends there and ships the same inert state name it was written to prevent. Re-derive the three
identifiers first: Step 7 and Preflight assigned them in **earlier Bash calls**, and shell state does
not survive between calls, so an inherited-looking `$PR_NUMBER` may simply be empty here — which
would send a resume run down the create arm and fail it on "a pull request for branch … already
exists". Then branch on `PR_NUMBER` before writing anything:

```bash
SESSION_BRANCH="${SESSION_BRANCH:-$(git branch --show-current)}"
BASE_BRANCH="${BASE_BRANCH:-$(gh repo view --json defaultBranchRef -q .defaultBranchRef.name)}"
PR_NUMBER="${PR_NUMBER:-$(gh pr list --head "$SESSION_BRANCH" --state open --json number -q '.[0].number // empty')}"
if [ -z "$PR_NUMBER" ]; then                     # fresh — Step 7's create arm never ran
  gh pr create --base "$BASE_BRANCH" --head "$SESSION_BRANCH" \
    --title "[<ISSUE-ID>] <issue title>" --draft --label agent-made --body-file "$PR_BODY"
  PR_NUMBER="$(gh pr view "$SESSION_BRANCH" --json number -q .number)"
fi
# Ready it: T2 is unreadable on a draft, and the state list promises a ready PR.
if [ "$(gh pr view "$PR_NUMBER" --json isDraft -q .isDraft)" = "true" ]; then gh pr ready "$PR_NUMBER"; fi
test "$(gh pr view "$PR_NUMBER" --json isDraft -q .isDraft)" = "false" || exit 1
```

**Write the body and the title yourself**, with the same `gh pr edit --body-file` mechanism Step 7
uses — never by hand-editing in the browser and never by leaving the bootstrap body in place. Write
both fields in one call, from the `$PR_BODY` assembled above:

```bash
gh pr edit "$PR_NUMBER" --add-label agent-made \
  --title "[<ISSUE-ID>] <issue title> (partial <satisfied>/<total>)" --body-file "$PR_BODY"
```

**The body is Step 7's PR body plus a delta — never a fresh one.** Re-specifying it from scratch is
how the one route that owns the only body write silently drops what every other route guarantees. So
write Step 7's body **verbatim in shape**: the same mandatory first line linking the tracker issue
(downstream review keys off it), the same `Plan: docs/plans/<file>` line, and the same
`## Acceptance criteria`, `## Autonomous decisions`, `## Cross-model review` and `## Review coverage`
sections. On top of that body this route adds the following, and **none** may be omitted:

- A count line: `Partial: <satisfied>/<total> acceptance criteria`.
- The **full** criteria checklist — every in-scope criterion present, none elided. Tick `- [x]` only
  when the diff/tests demonstrate it **and** the acceptance-criteria lens did not flag it; every
  other criterion is `- [ ]`.
- **One line per `- [ ]`**, naming what is missing and why it was deferred, at `file:line` precision —
  the same evidentiary standard the BLOCKED blocker comment already carries. A bare unticked box
  hands the next reader nothing.
- **Real** tokens in the inherited `## Cross-model review` and `## Review coverage` sections — a
  `capped` run keeps whatever token its tier earned, never a cleaner one. An absent mandatory
  section reads as full coverage, which on a deliberately partial slice is the worst possible
  misreading.
- A `## Partial` section whose **first line** is exactly this literal:

```
do not merge — partial: <satisfied>/<total> acceptance criteria
```

**The PR title** gains a trailing ` (partial <satisfied>/<total>)`, written by the
`gh pr edit --title` above — never left to a later step, and never a body-only write. The title
already carries `[<ISSUE-ID>]`; this **appends**, never replaces it. Title and body are boss-epic
condition (5)'s two independent holds, so a body-only write halves the redundancy that is supposed to
survive a human editing one of them.

**The ticket comment** carries the same four things — the count line, the full checklist, the
`file:line` reason per open criterion, and the marker — **plus the PR URL**.

**boss-epic holds a `PARTIAL` PR deliberately, twice.** Its merge gate condition (4) reads the ticket
against the `.inReview` role, and a `PARTIAL` ticket is still in the `.inProgress` role; condition
(5) reads `title,body` for the do-not-merge marker above. The hold is therefore double-covered and
survives a human moving the ticket by hand. That redundancy is the reason the marker literal is
written out verbatim here rather than paraphrased: boss-epic matches this exact string.

### REVIEW_READY-with-findings publication

**Why this section exists.** A round-capped review is a review that **ran**. Its open findings are a
statement about the code's remaining risk, not about whether the branch may ship: the artifact this
pipeline produces is a PR that a human reviews next, so fail-closing a pushed, green branch to avoid
publishing an imperfect PR ships **nothing** and moves zero risk — it only hides the findings in a
blocker comment nobody reviews. This route is the honest alternative: publish the work, publish the
findings **loudly**, and let the downstream gate do the gating. `capped`, `capped` with a surviving
provisional seed, `dispatch-failure`, and the Step 6 pre-dispatch decline all land here.

Like §PARTIAL-route publication, this route fires **without** passing Step 7 — the only step that
creates a PR, readies it, pushes, and writes the PR body — and without Step 9, the only one that
measures the branch green. So it owns each of those writes and that reading itself. Assuming any of
them already happened publishes a state name with no artifact behind it.

**The two gates — and they are the only two.** This route is `BLOCKED` for exactly two of the four
causes, and both are decided here:

- **The branch must be pushed.** Run §BLOCKED-route publication's `PUSHED=yes|rescue|no` procedure
  below — the same block, not a restatement of it; a locally shortened copy is a weaker procedure
  wearing the same name. Only `PUSHED=yes` may take this route. `PUSHED=rescue` or `PUSHED=no` is
  cause (2), _the branch cannot be pushed_: report via §BLOCKED-route publication, because a slice a
  reviewer cannot fetch is not a reviewable slice.
- **The quality gates must be green.** Step 9's watch never ran on this route, so take the reading
  here, **after** the push and the ready below and against the PR this section publishes:
  run [`callback-watches.md`](callback-watches.md) Protocol step 5's bounded poll over
  `"$PR_NUMBER"` — never a bare `gh pr checks --watch --fail-fast`, which has no timeout of its own,
  and never a fixed `sleep` standing in for the reading. Only `CI_WAIT_STATE=settled` is green. Red,
  `timeout`, `unknown`, or a rollup you cannot resolve, is cause (1), _quality gates are red_. A draft PR cannot supply this reading — its CI is expected to be noisy or
  partial — which is why the ready step below is mandatory rather than cosmetic.

Nothing else on this route is a blocker. Open must-fix findings of **any** severity, from any lens,
in any number, are **published**, never fatal; an unread or unreadable verdict is a coverage fact
published as a coverage token, never fatal. Causes (3) and (4) — a missing required API-version bump
or down-convert transform per the configured API-compatibility lens role, and a plan that demands
something unsafe — are decided elsewhere and reach `BLOCKED` without ever entering this section.

**A red reading must _unwind_ before it reports**, exactly as §PARTIAL-route publication's does and
for the same reason: the ready step and both writes below precede the green reading, so by the time
it comes back red the PR is already **ready** and already carries this route's body and label.
Restore the non-ready artifact first — recompose §BLOCKED-route publication's body into a
`$BLOCKED_BODY` temp file, `gh pr edit --body-file` it, `gh pr ready --undo "$PR_NUMBER"`, and remove
`please-review` — then report `BLOCKED`. Reuse §PARTIAL-route publication's unwind fences verbatim;
only the label removal is additional. A failed unwind is itself a blocker to name in the blocker
comment, never a reason to leave a ready PR claiming a state this run did not earn.

**Assemble the body first — both writes below consume it.** The create arm and the edit arm each take
`--body-file "$PR_BODY"`, so compose the whole body into a temp file _before_ either runs, exactly the
way Step 7 and §PARTIAL-route publication do. A route that acquires the PR first and only then reaches
for `$PR_BODY` passes an unset variable on the very fresh-workspace path this section exists to serve,
and `gh pr create --body-file ""` fails there before anything is published:

```bash
PR_BODY="$(mktemp)"   # compose the body elements listed below into this file, before either write
```

Remove it (`rm -f "$PR_BODY"`) once both writes have returned.

**Get a PR to write to, and ready it.** This route fires on fresh workspaces too, where no PR exists
because Step 7's `gh pr create` was never reached. Re-derive the three identifiers first — Step 7 and
Preflight assigned them in **earlier Bash calls**, and shell state does not survive between calls, so
an inherited-looking `$PR_NUMBER` may simply be empty here — then branch on `PR_NUMBER` before
writing anything:

```bash
SESSION_BRANCH="${SESSION_BRANCH:-$(git branch --show-current)}"
BASE_BRANCH="${BASE_BRANCH:-$(gh repo view --json defaultBranchRef -q .defaultBranchRef.name)}"
PR_NUMBER="${PR_NUMBER:-$(gh pr list --head "$SESSION_BRANCH" --state open --json number -q '.[0].number // empty')}"
if [ -z "$PR_NUMBER" ]; then                     # fresh — Step 7's create arm never ran
  gh pr create --base "$BASE_BRANCH" --head "$SESSION_BRANCH" \
    --title "[<ISSUE-ID>] <issue title>" --draft --label agent-made --body-file "$PR_BODY"
  PR_NUMBER="$(gh pr view "$SESSION_BRANCH" --json number -q .number)"
fi
# Ready it: the green reading is unreadable on a draft, and the state list promises a ready PR.
if [ "$(gh pr view "$PR_NUMBER" --json isDraft -q .isDraft)" = "true" ]; then gh pr ready "$PR_NUMBER"; fi
test "$(gh pr view "$PR_NUMBER" --json isDraft -q .isDraft)" = "false" || exit 1
```

**Write the body and the title yourself**, with the same `gh pr edit --body-file` mechanism Step 7
uses. Write both fields in one call, from the `$PR_BODY` assembled above, and apply the
review-request label in the same call:

```bash
gh pr edit "$PR_NUMBER" --add-label agent-made --add-label please-review \
  --title "[<ISSUE-ID>] <issue title>" --body-file "$PR_BODY"
```

`please-review` is the **generic literal** this core applies on every review-ready route; it is never
a project-specific label name, and this route applies it for the same reason Step 9 does — the PR is
genuinely awaiting human review. The **title gains no suffix**: unlike `PARTIAL`, this state makes no
claim about unmet criteria.

**The body is Step 7's PR body — never a fresh one.** Write it **verbatim in shape**: the same
mandatory first line linking the tracker issue (downstream review keys off it), the same
`Plan: docs/plans/<file>` line, and the same `## Premise discharge`, `## Acceptance criteria`,
`## Autonomous decisions`, `## Cross-model review` and `## Review coverage` sections. On top of that:

- **Real** tokens in `## Cross-model review` and `## Review coverage`, never a cleaner one than the
  run earned. An absent mandatory section reads as full coverage, which on this route is the worst
  possible misreading. Which token depends on which arm brought you here, and they are **not**
  interchangeable — §BLOCKED-route publication's token vocabulary below is the authority for all of
  them, and it applies here unchanged:
  - `capped` (a reviewer really ran) — keep whatever token its tier earned, `full (skipped: …)` or
    `quick: …`. A capped review is a review that ran.
  - `capped` with the **provisional seed survived** — `none: review coverage unknown (review stack
entered; provisional verdict never upgraded — <reason>)`, with `## Cross-model review` =
    `error: <reason>`.
  - `dispatch-failure`, **missing/stale** sentinel — `none: review coverage unknown (<reason>)`, with
    `## Cross-model review` = `error: <reason>`.
  - `dispatch-failure`, sentinel **present but unmatchable** — keep whatever tokens the subagent
    returned, annotated with the unreadable verdict; only if it returned nothing usable,
    `none: review verdict unreadable (<reason>)`.
  - **Step 6 pre-dispatch decline** (the stack was never entered) — `none: review stack did not run
(<reason>)`, with `## Cross-model review` = `skipped: <reason>`. Decide this from your own record
    of what you dispatched, never from the sentinel.
- A `## Review findings` section stating the count and severity split of the open findings and
  pointing at the marker comment below. Keep it to a pointer and a count: the ledger itself lives in
  the comment, where a human reads it.
- The base-drift note, under `## Autonomous decisions`, if any round boundary's check produced one.

**Post the open-findings ledger as the `<!-- bs-review -->` PR comment.** This is where the findings
actually land, and its **first line** is the marker literal so every downstream reader finds it.
Upsert exactly **one** such comment per PR — edit the existing marker comment in place on a resume,
never stack duplicates — with Step 7's own block:

```bash
BS_REVIEW_BODY="$(mktemp)"   # the open-findings ledger; its FIRST line is <!-- bs-review -->
CID=$(gh pr view "$PR_NUMBER" --json comments \
  --jq '[.comments[] | select(.body | contains("<!-- bs-review -->")) | .url][-1] // ""')
if [ -n "$CID" ]; then
  gh api -X PATCH "repos/{owner}/{repo}/issues/comments/${CID##*-}" -F body=@"$BS_REVIEW_BODY"
else
  gh pr comment "$PR_NUMBER" --body-file "$BS_REVIEW_BODY"
fi
rm -f "$BS_REVIEW_BODY"
```

The ledger carries **every open must-fix finding**, one entry each, at `file:line` precision with the
lens that raised it, its severity, and its disposition at the cap (open, attempted-and-unverified,
ineligible) — the same evidentiary standard the BLOCKED blocker comment carries. A count with no
entries hands the next reader nothing. Where the pass returned no report at all (the provisional,
dispatch-failure and floor arms), post the honest **fallback note** under the same marker: what ran,
why no verdict is readable, and a pointer to the PR body's two coverage sections — so every run on
this route leaves a visible review trace.

**Post the same summary as a tracker comment**, via the tracker adapter's `writeComment` capability,
**plus the PR URL**: the count, the severity split, the per-finding `file:line` ledger, and the
coverage token. The ticket is the one place a reader who never opens the PR will see them.

**The merge-gate token ban applies in full.** This route publishes findings sourced from the review
pass, and no text it writes into the PR **title or body** may contain the reserved `do not merge`
substring — not a finding title, not a status phrase, not a quoted excerpt (see §The reserved
merge-gate token). This route means the PR to be **merged** after human review, so it writes no
merge-gate marker of its own either: that marker belongs to §PARTIAL-route publication alone.
Rephrase a finding rather than quoting the token.

**Then finish as a review-ready run.** Move the ticket to the **In Review** role via `moveState`,
comment the PR URL, and go to **Stop cleanly** with `REVIEW_READY`. The findings are published, the
label is on, the PR is ready, and the branch is green and pushed — that is the whole of what this
state claims, and it claims nothing about the findings being resolved.

### BLOCKED-route publication (the orchestrator's job, not the subagent's)

**`BLOCKED` has exactly four causes, and a capped review is not one of them.** This route is reached
only when (1) quality gates are red, (2) the branch cannot be pushed, (3) a required API-version bump
or down-convert transform is missing per the configured API-compatibility lens role, or (4) the plan
demands something unsafe. That list is exhaustive: open review findings, a capped verdict, a
surviving provisional seed, a `dispatch-failure`, and the Step 6 pre-dispatch decline all publish through
§REVIEW_READY-with-findings publication above instead, and reach this section **only** by failing
its push gate or its green gate — causes (2) and (1). Anything routed here for review procedure alone
is a fail-closed regression.

**Two of the three things below are shared, not BLOCKED-only.** The `PUSHED=yes|rescue|no` push
procedure and the `## Review coverage` / `## Cross-model review` token vocabulary in this section are
the single copies every non-Step-7 route reads: §REVIEW_READY-with-findings publication and
§PARTIAL-route publication both run this push procedure and both publish from this token vocabulary.
They live here because this is where they were written; nothing about them is a claim that the run
reading them is blocked.

Routes that do not pass Step 7 — the only place that writes the PR body — leave the two mandatory
sections guaranteed **absent** exactly where coverage was reduced or nil, which is the reading
(`absent` = full coverage) they exist to prevent. The orchestrator publishes them instead: when `PR_NUMBER` is known, upsert the body with
the same `gh pr edit --body-file` Step 7 uses; when no PR exists yet, put **both** tokens verbatim in
the BLOCKED blocker comment, each under its own `## Review coverage` and `## Cross-model review`
heading, exactly as the PR-body path writes both sections. **Both**, not just coverage: they are
mandatory for the same reason — an absent `## Cross-model review` reads as "the outside voice passed
clean", and the outside voice is now the review pass's own `second-voice` round — and a fresh run that caps or fails before Step 7 is precisely the case with no PR to fall
back on, so publishing one token there loses whether the cross-model pass ran, was skipped, or
errored. Every branch below therefore names a value for **both**.

**The base-drift note publishes here too.** If any round boundary's base-drift check produced a note
— a hit, or an `unevaluated` it could not resolve — carry it into whatever this route publishes,
under `## Autonomous decisions`: the PR body when `PR_NUMBER` is known, otherwise the BLOCKED blocker
comment beside the two coverage tokens. A run that routes to BLOCKED never reaches Step 7 and so
never writes a PR body, which makes this the only place the drift reaches a reader — and the next
run needs to know the base moved under this one before it re-derives anything at all.

**The push rule — Step 7 is also the only step that pushes.** Publishing the tokens is not the only
thing Step 7 owns. Every route in this section stops at **Stop cleanly**, and Step 12 deletes the
claim, drops the stop-hooks and releases the lock without ever running `git push`. By the time any of
these routes fires, Step 6 has already committed the implementation — and on the `capped` and
`dispatch-failure` routes the review pass's own fix commits as well — so taking one as written leaves
finished commits reachable only from this worktree. That breaks the repo's completion invariant
(_work is not complete until `git push` succeeds; never stop before pushing_), and it is strictly
worse than a red PR: a pushed branch is something a later run's `boss-repair` or a human can pick up,
while an unpushed one is invisible to both and dies with the worktree. So persist the branch **before**
publishing anything:

**Pre-dispatch decline route.** The orchestrator reaches this route before it can dispatch the review
stack. Run the same procedure below first. Only `PUSHED=yes` may then write the generated
`sentinel capped 1` decline verdict and publish its tokens; `PUSHED=rescue` or `PUSHED=no` uses this
section's BLOCKED reporting instead. Do not fall through to Step 7 in any case.

```bash
# Only commits not already on the remote; with no upstream yet, everything on this branch. If BOTH
# forms fail the substitution is non-zero, so the trailing `|| UNPUSHED=` is what keeps errexit from
# killing this block on its first line; an empty count then matches no arm of the guard below and
# falls through to the push, which is the fail-open direction for a count nobody could read.
UNPUSHED=$(git rev-list --count '@{upstream}..HEAD' 2>/dev/null || git rev-list --count HEAD) ||
  UNPUSHED=
# Nothing to send AND origin's copy of the branch CONTAINS this HEAD: the work is already stored
# there. Existence is not containment — `@{upstream}` is a remote-TRACKING ref, so it outlives both
# a server-side delete and a server-side force-push, and after a force-push the branch NAME is still
# advertised, just at a different commit. So read the advertised SHA and either match it against
# HEAD or fetch and prove HEAD is an ancestor of it. Starts `no` and only a positive proof moves it:
# an unreachable remote, a deleted branch, an empty or unparseable SHA, an unresolvable HEAD and a
# failed fetch all fall through to the push below rather than claiming a push that never happened.
REMOTE_HAS_HEAD=no
# Read the advertised SHA ONCE, OUTSIDE the zero-ahead guard: the confirmation below needs it, and so
# does the tag injection on the other arm, which is only reached when the guard did not fire. Keep
# the ASK's status apart from its answer. "Origin advertises nothing" licenses the injection below to
# rewrite freely; "could not ask origin" must not, and `2>/dev/null` renders the two identical. A
# pipeline's status is the LAST stage's, not git's, so capture the raw output first and parse it
# afterwards. The awk reads the record into a NAMED VARIABLE and splits that, instead of reading
# `$2`: this file belongs to a skill whose SKILL.md body is reachable as a slash command, and the
# harness rewrites `$0`-`$9` in that body before any shell runs it. A reference file is read rather
# than substituted, so the hazard is latent here — but this block exists to be lifted verbatim into
# a body, so it is written to survive the move. Substituted, `'$2 == ref { print $1 }'` arrives as
# `'== ref { print }'`. That does not fail loudly — it is an awk syntax error, the capture takes the
# `|| REMOTE_SHA=` tail, and REMOTE_SHA lands EMPTY, which this route reads as "origin advertises
# nothing" and treats as a licence to rewrite unguarded. `getline line` plus `f[1]`/`f[2]` say the
# same thing in bytes the harness does not touch. `$0` is NOT one of them: the index is zero-based,
# so `$0` is substituted like any other, which is the whole reason the record goes into `line`.
# Keep the tail's line ending in `|` above it: a capture whose
# `|| VAR=` sits past a line that does not end in an operator reads as bare to a continuation-joining
# reader, and under errexit a bare capture aborts this block before the push.
REMOTE_LS_OK=yes
REMOTE_LS=$(git ls-remote origin "refs/heads/$SESSION_BRANCH" 2>/dev/null) || REMOTE_LS_OK=no
REMOTE_SHA=$(printf '%s\n' "$REMOTE_LS" |
  awk -v ref="refs/heads/$SESSION_BRANCH" 'BEGIN { while ((getline line) > 0) { n = split(line, f, "\t"); if (n >= 2 && f[2] == ref) { print f[1]; exit } } }') || REMOTE_SHA=
if [ "$UNPUSHED" = "0" ]; then
  HEAD_SHA=$(git rev-parse HEAD 2>/dev/null) || HEAD_SHA=
  # Both non-empty first: an empty advertised SHA must never compare equal to an empty HEAD.
  if [ -n "$HEAD_SHA" ] && [ -n "$REMOTE_SHA" ]; then
    if [ "$REMOTE_SHA" = "$HEAD_SHA" ]; then
      REMOTE_HAS_HEAD=yes
    elif git fetch -q origin "$SESSION_BRANCH" 2>/dev/null &&
      git merge-base --is-ancestor "$HEAD_SHA" FETCH_HEAD 2>/dev/null; then
      # Ahead of HEAD but still containing it — someone pushed on top. The work IS stored.
      REMOTE_HAS_HEAD=yes
    fi
  fi
fi
if [ "$REMOTE_HAS_HEAD" = yes ]; then
  PUSHED=yes
  # Confirmed arms assign the tagging outcome too, for the same reason they assign PUSHED: the
  # report branches on it. This is the one arm that may not tag — origin already contains HEAD.
  TAGGED=skipped
  TAG_NOTE="origin already contains HEAD; tagging now would rewrite published history"
else
  PUSHED=no
  # Tag the commits BEFORE they are published. This route never reaches Step 7, so the finalize tag
  # injection that normally runs there never runs at all, and every commit this run made would leave
  # the worktree untagged. Here is the last moment the tag is free: once the branch is on origin,
  # adding one means rewriting published history. Nothing below may gate the push — no `exit`, no
  # `break`, no `if inject; then push; fi`. The injector reports non-zero for benign cases too, so
  # its status records an outcome and never a reason to withhold the commits.
  TAGGED=skipped
  TAG_NOTE="tag injection not attempted"
  # ONE author per value. `TAGGED` is written here as the not-attempted default, by whichever skip
  # arm below fires, and — for every attempted injection — by the re-derivation after the push loop,
  # never by the injector's own exit status: a value remembered from before the reconcile is exactly
  # what the re-derivation exists to replace. So the injector's result is recorded in two variables
  # that the re-derivation reads rather than in `TAGGED` itself: `TAG_INJECTED` gates the
  # re-derivation, and `TAG_INJECT_NOTE` carries the injector's own diagnostic through to the report.
  TAG_INJECTED=no
  TAG_INJECT_NOTE=
  # A bare `VAR=$(cmd)` takes the SUBSTITUTION's status, so under `set -e` an unauthenticated,
  # offline or rate-limited `gh` would abort this block BEFORE the push — the very gate the rule
  # above forbids, and one no non-errexit harness can observe. `2>/dev/null` hides the stderr, never
  # the status. EVERY capture on this route therefore carries a `|| VAR=` tail — no exceptions and
  # no per-command reachability argument, because the next editor cannot re-run that argument and a
  # rule with exceptions stops being read. A gate over this block's text enforces it literally.
  if [ -z "${PR_NUMBER:-}" ]; then
    PR_NUMBER=$(gh pr list --head "$SESSION_BRANCH" --state open --json number \
      -q '.[0].number // empty' 2>/dev/null) || PR_NUMBER=
  fi
  BOSS_SKILLS_HOME="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}"
  if [ ! -d "$BOSS_SKILLS_HOME/boss-build/toolbox" ]; then BOSS_SKILLS_HOME="$HOME/.codex/skills"; fi
  if [ -z "$PR_NUMBER" ]; then
    TAG_NOTE="no open PR maps to $SESSION_BRANCH"
  # `--untracked-files=no` deliberately. What the injector cannot survive is an uncommitted change
  # to a TRACKED path: its rebase refuses to start ("cannot rebase: You have unstaged changes"), and
  # a reset inside it would discard the edit. An untracked file is neither — a rebase runs straight
  # past it, and a message-only rewrite of commits already in this history checks out no path that
  # could clobber one. Counting `??` as dirty would let one leftover scratch artifact disable the
  # whole injection on the route most likely to have leftovers, and publish untagged commits anyway.
  elif [ -n "$(git status --porcelain --untracked-files=no 2>/dev/null)" ]; then
    TAG_NOTE="tracked files carry uncommitted changes; the injector's rebase would refuse to start"
  elif [ ! -f "$BOSS_SKILLS_HOME/boss-build/toolbox/finalize/cli.mjs" ]; then
    TAG_NOTE="finalize toolbox not resolvable"
  elif ! TAG_BASE=$(gh pr view "$PR_NUMBER" --json baseRefName -q .baseRefName 2>/dev/null) ||
    [ -z "$TAG_BASE" ] || ! git fetch -q origin "$TAG_BASE" 2>/dev/null; then
    TAG_NOTE="PR base branch unresolvable"
  else
    # The injector REWRITES the commits it tags, which is safe only for commits origin has never
    # seen. Do NOT gate that on `REMOTE_HAS_HEAD`: it is `no` on this arm by construction, so it
    # would answer every shape identically. Record the commit origin advertises AND already has in
    # this history, run the injector, then require that commit to STILL be an ancestor of `HEAD`.
    # That guard is the whole safety argument for rewriting here, so ESTABLISHING it fails closed
    # too: a remote this run could not read, and an advertised commit it could not fetch, both leave
    # ancestry undecidable, and an unguarded rewrite is not a licence this route grants itself.
    # Only an advertised-NOTHING remote — never pushed, or deleted server-side — rewrites freely.
    PUBLISHED_TIP=
    GUARD_READY=yes
    if [ "$REMOTE_LS_OK" != yes ]; then
      GUARD_READY=no
      TAG_NOTE="could not read what origin advertises; a rewrite here would be unguarded"
    elif [ -n "$REMOTE_SHA" ]; then
      if git fetch -q origin "$SESSION_BRANCH" 2>/dev/null &&
        git cat-file -e "$REMOTE_SHA^{commit}" 2>/dev/null; then
        # Decidable now. NOT an ancestor means origin has DIVERGED from this history, so the commit
        # it advertises is not in the range the injector walks and there is nothing here to protect;
        # the rewrite stays local and the reconcile in the loop below owns that shape.
        if git merge-base --is-ancestor "$REMOTE_SHA" HEAD 2>/dev/null; then
          PUBLISHED_TIP="$REMOTE_SHA"
        fi
      else
        GUARD_READY=no
        TAG_NOTE="origin's advertised commit is unfetchable; a rewrite here would be unguarded"
      fi
    fi
    # The rollback below rewinds to this commit, so capture it BEFORE the guard is consulted and
    # fail closed on an empty answer for the same reason the two arms above do: a rewrite whose
    # undo has no target is unguarded. `git reset --hard ""` is NOT the hazard — it exits 128 and
    # moves nothing — so what an empty answer really buys is an unsafe rewrite left standing with
    # nothing able to take it back, surfacing only as a rollback that did not complete. Refuse it.
    PRE_INJECT_SHA=$(git rev-parse HEAD 2>/dev/null) || PRE_INJECT_SHA=
    if [ -z "$PRE_INJECT_SHA" ]; then
      GUARD_READY=no
      TAG_NOTE="could not capture the pre-injection HEAD; a rewrite here would be unguarded"
    fi
    if [ "$GUARD_READY" = yes ]; then
      TAG_INJECTED=yes
      if ! BASE_BRANCH="$TAG_BASE" node "$BOSS_SKILLS_HOME/boss-build/toolbox/finalize/cli.mjs" inject-pr-tag "$PR_NUMBER"; then
        # Keep the injector's own diagnostic — it is the only record of WHY the injection stopped,
        # and the re-derivation below can observe that a commit is untagged but never why. The
        # CAUSE and nothing else: a non-zero injector exit does not imply an untagged commit, so a
        # second clause guessing at one would contradict a re-derived `TAGGED=all` in the same
        # report — the ambiguity the two-field split exists to remove, re-entering as a value.
        TAG_INJECT_NOTE="injector exited non-zero"
        # Never leave a half-finished rebase or a detached HEAD behind for the push loop to trip over.
        git rebase --abort 2>/dev/null || true
        git checkout -q "$SESSION_BRANCH" 2>/dev/null || true
      fi
      if [ -n "$PUBLISHED_TIP" ] && ! git merge-base --is-ancestor "$PUBLISHED_TIP" HEAD 2>/dev/null; then
        # Branch on the reset's OWN status. As a bare command it is not a condition, so under
        # errexit a lock left by the aborted rebase above would exit the arm whose entire purpose is
        # undoing an unsafe rewrite — before the push, and with the branch left rewritten. And even
        # without errexit a failed reset is indistinguishable from a successful one, so reporting
        # `skipped` unconditionally would assert a rewrite did not happen on a branch where it did.
        if git reset --hard "$PRE_INJECT_SHA"; then
          # Rolled back to the pre-injection commit, so there is nothing left for the re-derivation
          # to read and no injector diagnostic left that describes the branch as it now stands.
          TAG_INJECTED=no
          TAG_INJECT_NOTE=
          TAGGED=skipped
          TAG_NOTE="injection would have rewritten a commit origin already advertises"
        else
          # The rollback did not complete, so leave `TAG_INJECTED=yes` and let the re-derivation
          # below read what the branch stands at afterwards. The note records the ATTEMPT only.
          # It may NOT say the branch still carries the rewrite: the reconcile in the loop rebases,
          # and a rewritten copy of a commit origin still holds untagged is dropped there as
          # patch-identical — so that claim would publish a state this block never observed.
          TAG_INJECT_NOTE="rollback of an over-published rewrite did not complete"
        fi
      fi
    fi
  fi
  attempts=0
  delay=5
  # Retry over a real WINDOW, not a token count: a remote outage that clears in two minutes must
  # not cost this run its commits. Back off so the window is minutes, not milliseconds.
  while [ "$attempts" -lt 8 ]; do
    attempts=$((attempts + 1))
    if git push -u origin "$SESSION_BRANCH"; then PUSHED=yes; break; fi
    # Rejected means the remote moved under this run. Reconcile and retry; NEVER a merge.
    # NOT `git pull --rebase`: its fork-point heuristic reads the OLD origin/<branch> reflog entry,
    # so after a server-side force-push it concludes this run's commits are already upstream and
    # silently DROPS them — the push then "succeeds" with the work gone. Rebase onto what was just
    # fetched, fork-point off. Patch-identical commits are still skipped, so nothing duplicates.
    # A failed reconcile is usually transient too, so it must NOT end the loop — only make sure
    # the next attempt does not start from inside a half-finished rebase.
    git fetch -q origin "$SESSION_BRANCH" && git rebase --no-fork-point FETCH_HEAD ||
      git rebase --abort 2>/dev/null || true
    sleep "$delay"
    [ "$delay" -ge 60 ] || delay=$((delay * 2))
  done
  # Re-derive the tag outcome AFTER the loop, never from the pre-push scan. The reconcile above
  # rebases, and rebase skips commits by PATCH id, which ignores the message entirely: a tagged copy
  # of a commit origin still holds untagged is dropped as a duplicate and the tag goes with it. So
  # read the branch as it now stands, with the predicate the injector itself verifies — a SUBJECT
  # carrying the tag — over the same range the injector walked. Unverifiable is `partial`, never
  # `all`: this block may not publish a tag state it did not observe. Read what the push published,
  # so check FIRST that `HEAD` is still the branch: a failed re-attach above leaves a detached,
  # partly-tagged `HEAD` while the branch — the ref the loop pushes — still points at the untagged
  # original, and scanning `HEAD` would then report `all` for a branch carrying no tag at all. Keep
  # the read's status apart from its answer for the same reason the `ls-remote` above does: a
  # pipeline's status is `grep`'s, and an unreadable range prints nothing, which `grep -qv` reports
  # exactly as a fully tagged one.
  if [ "$TAG_INJECTED" = yes ]; then
    # `|| TAG_RANGE_BASE=` for the same reason as the PR lookup: a bare capture would abort under
    # `set -e` before reaching the `[ -z … ]` arm that exists precisely to handle it, turning the
    # intended `partial`/unverifiable outcome into an aborted publication.
    TAG_RANGE_BASE=$(git merge-base HEAD "origin/$TAG_BASE" 2>/dev/null) || TAG_RANGE_BASE=
    if [ "$(git rev-parse HEAD 2>/dev/null)" != "$(git rev-parse "refs/heads/$SESSION_BRANCH" 2>/dev/null)" ]; then
      TAGGED=partial
      TAG_NOTE="tag state unverifiable: HEAD is not on $SESSION_BRANCH"
    elif [ -z "$TAG_RANGE_BASE" ]; then
      TAGGED=partial
      TAG_NOTE="tag state unverifiable: the PR base no longer resolves"
    elif ! TAG_SUBJECTS=$(git log --format=%s "$TAG_RANGE_BASE..HEAD" 2>/dev/null); then
      TAGGED=partial
      TAG_NOTE="tag state unverifiable: the branch range would not resolve"
    elif [ -n "$TAG_SUBJECTS" ] && printf '%s\n' "$TAG_SUBJECTS" | grep -qvF "[#$PR_NUMBER]"; then
      TAGGED=partial
      TAG_NOTE="commits on this branch still carry no [#$PR_NUMBER]"
    elif [ -z "$TAG_SUBJECTS" ]; then
      # An empty range satisfies "every commit carries the tag" VACUOUSLY, and that is the outcome
      # the reconcile above produces when it drops this run's tagged copies as patch-identical —
      # the very case this re-derivation exists for. Left to fall into the arm below it would be
      # indistinguishable from a genuinely tagged branch, and the report would then assert a tag on
      # commits that are no longer there. Same rule as everywhere else on this route: say what was
      # observed, and an empty range is not an observation of a tag.
      TAGGED=all
      TAG_NOTE="the reconcile left no commits in the range; nothing carried the tag out"
    else
      TAGGED=all
      TAG_NOTE=
    fi
    # Two variables, never one joined string. The re-derivation owns WHAT the branch carries and the
    # injector owns WHY it stopped, and a report that drops the second sends the next reader to
    # re-derive it — but joined into one string neither clause is attributable any more: the reader
    # cannot tell which half was observed from which half is a cause, and the injector's text is
    # free form, so no separator a join might pick is reserved against it. `TAG_INJECT_NOTE` is
    # already a variable; report it beside `TAG_NOTE` and there is nothing to split.
  fi
  if [ "$PUSHED" != yes ]; then
    # The session branch ref is unreachable, or diverged beyond what an autonomous run may
    # reconcile. A UNIQUE rescue ref cannot be rejected as non-fast-forward, so the commits still
    # leave this worktree. Never accept "the SHAs are in the local log" as a terminal outcome.
    # The suffix is what makes the ref unique, so it is captured on its own and never left empty:
    # bare, a failing `rev-parse` would abort the last mechanism guaranteeing these commits leave
    # the worktree, and an empty one would build the invalid ref `refs/heads/<branch>-blocked-`.
    # `$$` is always set and cannot fail, so the fallback is unique enough to survive a push.
    RESCUE_SUFFIX=$(git rev-parse --short HEAD 2>/dev/null) || RESCUE_SUFFIX=
    [ -n "$RESCUE_SUFFIX" ] || RESCUE_SUFFIX="$$"
    RESCUE="$SESSION_BRANCH-blocked-$RESCUE_SUFFIX"
    if git push origin "HEAD:refs/heads/$RESCUE"; then
      PUSHED=rescue
      echo "session branch unpushable after $attempts attempt(s); commits persisted on $RESCUE — name that branch in the BLOCKED comment"
    else
      echo "push FAILED after $attempts attempt(s) and the rescue ref also failed; name the unpushed commits in the BLOCKED comment"
    fi
  fi
fi
```

**Every arm assigns `PUSHED` — never initialize it above the conditional.** The report below branches
on `yes`/`rescue`/`no`, so an arm that leaves the variable unset matches none of them and the BLOCKED
comment cannot say where the work ended up. The zero-ahead arm is a real route, not a corner: a
resumed or already-persisted branch reaches here with nothing to send. Give it its own explicit
`PUSHED=yes` inside the zero-ahead arm itself rather than seeding `PUSHED=yes` before the `if` —
seeding defaults the variable to the one value that claims the work is safe, so any future path that
forgets to assign it would report a push that never happened, which is the exact loss this whole rule
exists to prevent. Seeding `no` is not the answer either: the total-failure arm below promises that
every attempt **and** the rescue ref failed, which is a falsehood on a branch that never attempted a
push at all. Both arms assign explicitly, and `no` stays scoped to the arm that genuinely tried.

**Zero commits ahead is not by itself "on the remote".** `@{upstream}` is a remote-_tracking_ ref: it
still resolves after the branch it mirrors was deleted on the server, it survives a server-side
force-push, and it can name a different branch than `$SESSION_BRANCH`. So a bare zero-ahead test can
be true while nothing of this run is on origin — hence the confirmation against the remote before
claiming `yes`.

**The branch _existing_ on origin is not enough either — verify the remote contains `HEAD`.** A bare
`git ls-remote --exit-code --heads` answers "does this name still exist", which closes only the
delete case. A force-push leaves the name advertised at a **different** commit, so existence passes
while `HEAD` sits on no remote ref at all, `PUSHED=yes` is claimed, every push attempt is skipped,
and the BLOCKED route stops with the completed work still only local. The honest predicate is
**containment**: read the SHA `git ls-remote` already prints, and when it is not `HEAD`, fetch the
branch and prove `HEAD` is an ancestor of what was advertised.

**Containment, not equality.** A remote that moved _ahead_ of `HEAD` — someone pushed on top of this
branch — still holds every commit this run built, so the work is safe and there is nothing to send.
An equality test would call that state unconfirmed and force a push whose best outcome is a no-op and
whose likely outcome is a rejection followed by a rebase this run had no reason to perform.

**The confirmation fails closed.** `REMOTE_HAS_HEAD` starts at `no` and only a positive proof moves
it: an unreachable remote, a deleted branch, an empty or unparseable advertised SHA, an unresolvable
`HEAD`, and a failed `git fetch` each leave it `no`. Test both SHAs non-empty _before_ comparing them
— an empty string must never compare equal to another empty string on the way to `yes`. Every
unconfirmed reading falls through to the push loop, which is free to be wrong in that direction: a
branch that really was current costs one no-op `Everything up-to-date`, and one that was deleted or
force-pushed away is simply restored.

**Retry — do not swallow the failure.** A bare `git push || echo …` turns a failed push into a
successful command and walks on to Step 12 with the commits still stranded, which is the exact
outcome this rule exists to prevent, now wearing a reassuring log line. The loop above is the rule:
attempt, reconcile, re-attempt, and only report loss once the branch genuinely could not be
persisted. The `echo` is the last resort **after** the retries, never a substitute for them.

**Reconcile with `git rebase --no-fork-point FETCH_HEAD`, never `git pull --rebase`.** This is the
other half of the force-push case, and it is the more expensive half. `git pull --rebase` applies
git's fork-point heuristic, which reads the **old** `origin/$SESSION_BRANCH` reflog entry to decide
which commits are "already upstream". After the branch was force-pushed on the server that entry
still names the SHA this run pushed, so the heuristic concludes this run's commits are upstream
already and **drops them** — the rebase lands the worktree on the divergent remote tip, the next
push succeeds, and the block honestly reports `PUSHED=yes` for a branch the work is no longer on.
That turns a stranded-but-recoverable worktree into a deleted one. `rebase --no-fork-point` onto the
ref just fetched compares real ancestry instead, and still skips patch-identical commits, so nothing
is duplicated and nothing is lost. Setting `rebase.forkPoint=false` does **not** fix `git pull` here
— `pull` computes the fork point itself — so the `pull` form has to go, not be reconfigured.

**Never end the loop on a failed reconcile.** `git fetch … && git rebase … || break` is the same
defect one level down: a transient fetch error on the first reconcile ends the loop after a
**single** push attempt, while the message still claims all eight — so the run reports a thorough
effort it did not make and strands the commits anyway. A reconcile that fails is a reason to try
again, not to stop.
Abort any half-finished rebase so the next attempt starts from a clean worktree, and let the loop
run its full course. Report the attempt count from the counter rather than a literal, so the
number can never drift from the number of attempts actually made.

**Locally-recorded SHAs are a poor terminal outcome.** The invariant this rule serves is that the
work **leaves the worktree** as early as possible, while this run still knows what it built.

Be accurate about the stakes, though: **Stop cleanly does not delete the worktree** — it deletes the
claim comment, removes the stop-hooks and releases the lock — and the daemon that finalizes the
session independently protects committed work. A clean worktree whose branch is ahead of base is
pushed and given a PR rather than hard-deleted; if that push fails the worktree is **preserved**
rather than archived; and even on the hard-delete path an unmerged branch reads as not-safe and is
kept. A run that ends here unpushed is therefore not automatically lost. Do not lean on that: it is
another component's guarantee, it fires only on the shapes finalize recognises, and until it does the
work is invisible to the PR, to CI, and to the next repair round. Push here anyway. So the loop above
has two escapes and one bad-but-recoverable end:

- **Transient remote trouble** — an outage, a rate limit, a flaky fetch. The retry backs off (5 s,
  doubling to a 60 s ceiling, up to 8 attempts, so the window is minutes rather than a handful of
  back-to-back failures). Almost every real push failure clears inside it.
- **A diverged or unreconcilable branch ref** — the remote moved and the rebase will not replay
  cleanly, which is not something an autonomous run may resolve by guessing. Retrying the same ref
  cannot help however long it runs, so this is where the **rescue ref** earns its place: pushing
  `HEAD` to a fresh `<branch>-blocked-<sha>` ref is a create, not a fast-forward, so it cannot be
  rejected for divergence. The commits leave the worktree; the BLOCKED comment names the branch that
  now holds them.
- **The remote is entirely unreachable for the whole window** — nothing this run can do persists
  anything, so report it loudly and leave the commits in the tree. Do **not** refuse to reach Step 12
  over it: blocking terminal cleanup would hold the worktree lock open, never publish the outcome,
  and never release this run — and finalize, which is the component that can retry the push later and
  preserves the worktree when it cannot, only gets its turn once this run ends.

That is why the loop is bounded rather than literally "until it succeeds". An unbounded retry here
would not save the diverged case — the rescue ref is what saves it — while on a dead remote it would
hang this run forever: the worktree lock is never released, the terminal state never published, and
the caller's repair loop never gets the signal it is waiting for. An unreported hang that also
blocks later work is strictly worse than a reported, and now very unlikely, loss. Silently reporting
success remains the one outcome that must never happen.

Plain `git push` — never `--force`/`--force-with-lease`, and reconcile with a rebase,
never a merge (see the linear-history rule these skills share). The tag injection is the one step on
this route that rewrites commits, and where origin's advertised tip is contained in this history it
never rewrites a commit that tip is built on: it rolls itself back the moment that stops being true,
and it declines to run at all when it cannot read or fetch that tip. Bound the claim there and no
further — the containment is the premise, not a decoration on it. Where origin has **diverged**
from this history it advertises a commit this history does not contain, and the injection may then
rewrite commits origin also holds — but that rewrite stays **local**, because nothing on this route
may push over them; the reconcile below is what decides that shape. A rejected push therefore still
means the remote moved under this run: rebasing onto it keeps both sides, while a force would
overwrite a commit this run did not author.

**Report what `PUSHED` actually says, never a fixed sentence.** The block above has three terminal
outcomes and the BLOCKED comment owes a different statement for each. Read the variable; do not
assume the loop failed just because you reached this paragraph:

- `PUSHED=yes` — the session branch holds the work, either because a push in this block succeeded or
  because the branch had nothing to send and origin's copy of it was confirmed to **contain** `HEAD`.
  Name that branch; nothing further is owed here.
- `PUSHED=rescue` — the session branch was unpushable after all eight attempts, **but the commits
  did leave the worktree**. Name `$RESCUE` in the BLOCKED comment as the branch that now holds them,
  and do **not** describe the commits as unpushed or name bare SHAs as their only record. Both would
  be false, and the falsehood is expensive in the same direction twice: the next `boss-repair` run
  and the human both go looking for work that is already on the remote, while the one ref that has
  it goes unmentioned and is never picked up.
- `PUSHED=no` — the eight attempts **and** the rescue ref all failed, so nothing left the worktree.
  Only here name the unpushed commit SHAs in the BLOCKED comment; that comment is then the **only**
  record that the work existed.

A failed push does **not** change the terminal state: it is already `BLOCKED`.

**Report the tagging outcome too — it is an outcome, never a gate on the push.** The step that
injects the PR-number tag lives in Step 7, and every route in this section stops before it, so these
commits reach the remote through this block or not at all, and they reach it tagged only because the
injection above ran first. Read `TAGGED` **after** `PUSHED` is assigned — a tag on a commit that
never left the worktree is not something a reader can act on, and the block re-derives the value
there from the branch as it now stands rather than from what the injector said before the loop ran.
State one of:

- `TAGGED=all` — every commit in the branch's range carries the tag. Read it together with `PUSHED`:
  on `PUSHED=no` nothing left the worktree, so this describes the local branch only. A range with no
  commits left in it satisfies this **vacuously**, and the block records exactly that in `$TAG_NOTE`
  rather than leaving the two cases identical: where that note is non-empty, report that the
  reconcile dropped the commits — never "all commits are tagged", which asserts a tag on commits
  that are no longer there. Nothing further is owed beyond `$TAG_INJECT_NOTE` where it is non-empty.
- `TAGGED=partial` — some commit on that branch does not, **or** the state could not be verified at
  all. Those are different reports and `$TAG_NOTE` is what tells them apart, so name the branch and
  then name that reason: only the "still carry no `[#N]`" note licenses asserting an untagged
  commit, and the three `tag state unverifiable:` notes observed nothing, so asserting one there is
  an invention the next reader has to go and disprove. Quote the injector's own `these commits were
left untagged:` list where it printed one: it names each commit and the reason its amend was
  rejected, which a bare "unverified" throws away and the next reader then has to re-derive. Do
  **not** report it as a push failure: `PUSHED` owns that, and the two are independent.
- `TAGGED=skipped` — nothing was injected, for the reason in `$TAG_NOTE`. Name that reason rather
  than the bare word: "no open PR maps to the branch" and "the finalize toolbox was not resolvable"
  ask different things of the next run.

`$TAG_INJECT_NOTE` is a **second, separate field**: report it beside `$TAG_NOTE` whenever it is
non-empty, and never merge the two into one string. They answer different questions — `$TAG_NOTE`
says what the branch was **observed** to carry, `$TAG_INJECT_NOTE` says **why** the injection stopped
or which recovery step did not complete — and a merged note is one string in which neither clause is
attributable: the reader cannot tell the observation from the cause, and the injector's text is free
form, so no separator a join might pick is reserved against it. Being about the injection and never
about a commit, it licenses no assertion about the branch at all: state it as the **cause** alongside
the observation, never in place of one. It is **unset or empty on every ordinary outcome** — the
confirmed arm never injects, and an injection with nothing to report leaves it as it was — and an
empty field is **omitted from the report** rather than printed blank: a named field nobody filled in
is not a diagnostic that went missing. Only the non-empty case is owed a line.

**Where the line goes when `$TAG_NOTE` is empty.** "Beside `$TAG_NOTE`" has nothing to sit beside on
the `all` arm: the re-derivation clears `TAG_NOTE` there on every arm but the vacuous empty-range
one, and clears it nowhere else. So on `TAGGED=all` report
`$TAG_INJECT_NOTE` under the `TAGGED=all` statement itself, as the **cause** it always is. That
pairing is not a contradiction to be resolved away — a rollback that did not complete, or a benign
non-zero injector exit, can end with every _surviving_ commit tagged, because the reconcile drops the
rewritten copies as patch-identical. `all` and a non-empty injector diagnostic are therefore the one
combination most worth printing, and dropping the diagnostic because its companion field is empty
loses the only record that the recovery did not finish — exactly the loss the two-field split exists
to prevent.

Say what an untagged commit actually costs, and no more. The tag is a traceability link from commit
to PR, so when the project runs no commit-message check in CI, an untagged commit is a gap in that
link — not a red check. Where the project does run such a check, it is that too. Never assert a red
build this run has not observed: an invented consequence sends the next reader hunting a failure that
does not exist, which is the same class of error as an invented success, and it discredits the rest
of the report.

**Not a goal: retro-tagging commits origin already holds.** Those stay untagged, and the containment
check above exists to keep them that way. Tagging them means rewriting published history, whose only
delivery is a force-push over commits this run may not have authored — forbidden here in every form.
An already-published untagged commit is a closed loss to record, not an open task for this route.
The rollback that enforces this is **all-or-nothing** by choice: when the injector would rewrite a
published commit, the whole injection is reset, so this run's own unpublished commits lose their tag
too. Tagging just the unpublished suffix would keep them, and is the obvious refinement — but it is
not what this block does, so report `skipped` as the total outcome it is.

**Every reader of this block gets the injection, and the report re-checks what survived.** The
pre-dispatch decline route, `capped`, and `dispatch-failure` run the same block, so all three publish
tagged commits — the decline route included, even though it exits to a generated `capped 1` verdict
rather than to a review report — and §PARTIAL-route publication borrows this same push procedure, so
it is a fourth reader and its T3 conjunct reads the outcome as a gate. What a reconcile does to the
tag is exactly why the outcome is re-derived rather than remembered: `git rebase` skips commits by
**patch id**, which ignores the message entirely, so a tagged copy of a commit origin still holds
untagged is dropped as a duplicate and the tag goes with it. The re-derivation after the loop reads
the branch as it now stands, so `all` is an observation of that branch rather than a memory of what
the injector said it would do. That is not the same as a promise the tag survived: when the drop
takes **every** commit the range is left empty, and an empty range satisfies "every commit carries
the tag" vacuously — so the emptied-range arm reaches `all` with nothing carrying anything, and
records exactly that in `$TAG_NOTE`. Read the pair, never the token alone: `all` alongside that note
is a branch the reconcile left nothing on, and only `all` with `$TAG_NOTE` empty reports commits
observed carrying the tag. `PUSHED=rescue` pushes that same `HEAD` to `$RESCUE`, so the rescue ref
carries whatever the re-derivation found and is no lesser record. The zero-ahead arm is the one that
skips the injection **by construction**, and it must: it reached `yes` precisely because origin
already contains `HEAD`, the one state where tagging would rewrite published history. Every other
`skipped` is conditional and names its reason in `$TAG_NOTE`, so never read the bare word as proof
that the zero-ahead arm fired.

This binds **all three** routes: the Step 6 pre-dispatch decline, `capped`, and `dispatch-failure` alike.
They differ only in which tokens they publish; every one of them is reachable with commits in the
tree, so none may exit without this push.

Which token depends on **which** `dispatch-failure` sub-case fired, and they are not interchangeable.
**Neither** of them is `none: review stack did not run`: both fire only _after_ the stack was
entered, and a sentinel that never arrived is not evidence that no reviewer ran.

- **Missing/stale sentinel** — nothing readable is in the run file at all, not even the provisional
  seed, so the seed write itself failed or the run dir was lost. The subagent was still dispatched
  and returned no tokens, and a kill, timeout, or crash anywhere in the stack lands here — including
  one that struck after a reviewer, or several, had already reported. Nothing here proves coverage
  either way, so publish the uncertainty: write
  `## Review coverage` = `none: review coverage unknown (<reason>)` and `## Cross-model review` =
  `error: <reason>`. Do **not** write `did not run` — it asserts an absence the evidence does not
  support and hides partial coverage, the exact ambiguity this section exists to expose.
- **Present but unmatchable sentinel** (`matchSentinel` returned no status) — the subagent was alive
  and a tier really ran. `did not run` would be a **falsehood** in the one section whose purpose is
  honesty, and a reader who believes it will not go looking for the review that happened. Keep the
  tokens it returned and annotate the coverage one with the unreadable verdict; only if it returned
  nothing usable, write `none: review verdict unreadable (<reason>)`.
- **Provisional sentinel that survived** (`matchSentinel` → `capped`, **and** the payload's
  `provisional` is `true`) — the orchestrator's own pre-dispatch seed came back exactly as written,
  so the stack was entered and nothing inside it ever settled a verdict. Publish the same
  uncertainty the first bullet does, with the cause named: `## Review coverage` =
  `none: review coverage unknown (review stack entered; provisional verdict never upgraded — <reason>)`
  and `## Cross-model review` = `error: <reason>`. Deliberately the **same** head token rather than a
  sixth form — nothing about coverage is known here either, and the enumeration Step 7 copies is
  resident, where bytes are scarce. What changed is that the **routing survives**: the seed is why
  this run reaches a terminal state at all instead of an empty file. Decide this bullet from the
  **payload marker alone** — never from the kind string, the round count, or the returned prose, all
  of which a genuine reviewer-earned cap matches byte for byte. It is **not** eligible for `PARTIAL`
  either; see §PARTIAL-route publication's T1.

`none: review stack did not run (<reason>)` belongs to neither bullet — it is the **third** route,
the one where the stack was **never entered**: no review subagent was dispatched and the inline
fallback did not run either, **or** the `BOSS_BS_REVIEW=0` off switch declined the pass before any
reviewer was started. Either way there is no sentinel to classify and no `dispatch-failure` to sub-case. You
know you are on it from **your own record of what you dispatched**, never from the run file. It owes
the same two tokens as the bullets above: write `## Review coverage` =
`none: review stack did not run (<reason>)` and `## Cross-model review` = `skipped: <reason>` — the
cross-model round lives inside the pass, so a stack that was never entered never reached it, which
is a policy skip and not an `error: <reason>` (nothing failed; nothing ran). Do **not** leave `## Cross-model review`
out of this route's comment: it is the one route with no later step to supply it, and an omitted
mandatory section reads as "passed clean".

A `capped` run keeps whatever token its tier earned — `full (skipped: …)` or `quick: …` — since a
capped review is a review that ran.

A quick run still owes the **other** never-omit section a value. Step 7 makes
`## Cross-model review` mandatory precisely so silence cannot read as "passed clean", and the
quick tier skips the pass that would normally fill it — so emit
`skipped: quick tier` for `## Cross-model review` whenever the quick tier was selected. Do not
omit the section, and do not improvise a different token: an undocumented value in a mandatory
section is the same failure as an absent one.

### Reviewed-tip confirmation (Step 7 only — `PUSHED=yes` is not proof of coverage)

`PUSHED=yes` says the work is **stored**, never that the stored tree is the tree this run reviewed.
Two arms of the push procedure above reach it while moving the tip: the zero-ahead arm sets
`REMOTE_HAS_HEAD=yes` when origin is merely **ahead of `HEAD` but still containing it** — another
process pushed on top — and the retry loop reconciles a rejected push with
`git rebase --no-fork-point FETCH_HEAD`, which replays this run's commits onto commits it never saw.
Either way the branch backing the PR carries unreviewed commits while Step 7 writes a body reporting
full coverage. The BLOCKED routes above are unaffected — they never reach Step 7 and publish reduced
coverage by construction — so this confirmation binds **Step 7 alone**. Capture the reviewed tip
**before** invoking the procedure, and read back the tip it left on the remote:

```bash
REVIEWED_HEAD=$(git rev-parse HEAD)   # BEFORE the retry/rebase/rescue procedure
# ...run the procedure; continue only on PUSHED=yes...
git fetch -q origin "$SESSION_BRANCH" || true
PR_TIP=$(git rev-parse FETCH_HEAD 2>/dev/null || echo unknown)
```

Compare the two as **strings**. When `PR_TIP` equals `REVIEWED_HEAD`, the reviewed range covers what
ships and Step 7 continues. Anything else fails **closed**, including the `unknown` an unreachable
remote or an unresolvable `FETCH_HEAD` leaves — an unread tip is not a matching one. On a difference,
take one of exactly two routes: re-run the Step 5 gates and the Step 6 review against the new tip and
continue only once that pass is clean, or leave Step 7 for §REVIEW_READY-with-findings publication —
recording both SHAs and publishing `## Review coverage` =
`none: review coverage unknown (branch tip moved after review: <REVIEWED_HEAD> → <PR_TIP>)`. That
second route is not a blocker: it re-reads CI green against the tip that actually ships, so the
branch is still gated, and it reaches `BLOCKED` only if that reading is red (cause 1) or the branch
cannot be pushed (cause 2). What is never allowed is the third option — writing a PR body that
claims coverage of a tree that was never reviewed.

## Step 6: the review pass (`boss-review`)

This is **the** review. Run one **`boss-review`** pass — a consolidated, multi-lens review over the
implementation branch — and route the run on the verdict it writes. Invoke it via the `Skill` tool
(`boss-review`, stating the already-resolved `REVIEW_BASE` as the base to review from — never with no
args, which would let it resolve its own merge-base against the default base and review a different
range than the `## Review coverage` token below publishes).
`boss-review` resolves its passes at runtime rather than running a fixed roster. Conditional specialist
**lenses** come from the configured lens registry (the `lensMap` in `.boss-skills.json`, matched against
the changed paths, each carrying an inline fallback rubric for when the reviewer it names is absent).
Whole-branch **rounds** are then resolved by strict precedence: the repo-local round extensions this
repository provides, or — when it provides none — a single fallback round, either the host's native
whole-diff review or an inline whole-diff rubric. The **cross-model outside voice is one of those
rounds** — `boss-review`'s Phase D `second-voice` round — so it is inside this pass, not a separate
one. How many rounds run, and what each one looks at, is
therefore whatever the consuming repository configures. It fixes every must-fix finding
locally (committing tagless), and prints a rendered report (a one-line header, a ✅/❌ verdict block,
and collapsible `<details>` sections, produced by
`$BOSS_BUILD_TOOLBOX/bs-review-report.mjs`) followed by a `bs-review clean:` or `bs-review capped:` sentinel
line.

The orchestrator has already picked `REVIEW_BASE` (fresh/bootstrap-only → `$START_SHA`; resume →
`$BASE_REF`), run the change-detection gate, and committed this run's work tagless (including the
`docs/plans/<DATE>-<slug>.md` deliverable). The pass reviews the diff `$REVIEW_BASE...HEAD`.

**Two gates bracket the dispatch.** Run [§Base-drift check](#base-drift-check-at-the-review-check-point)
**before** dispatching the pass — a rebase after the stamp invalidates it — and
[§API-surface check](#api-surface-check-conditional-required--do-this-before-the-clean-exit)
**before** the clean exit. Both are subsections of this step.

**Supply the plan's acceptance criteria.** State them in the invocation so the pass runs its
acceptance-criteria certification against them rather than inferring intent from the diff. A pass
that cannot certify a criterion reports it as an open must-fix, which is exactly the signal the
capped route exists to carry.

**Own the notes phase, and suppress the nested one.** This run takes the single post-terminal
notes dispatch for the whole top-level run (Step 12), so set `BOSS_NOTES_SUPPRESSED=1` in the
environment the nested pass runs under and state it in the invocation. Without it the nested pass
takes its own notes on top of this run's, and one outcome is reported twice. Every other boss core
this run dispatches gets the same marker for the same reason. The marker defaults to **not
suppressed**, so it never leaks into a standalone `boss-review` invocation — only a run whose
caller actually set it skips its own notes.

This step is **await-only** (never `run_in_background`) and **blocking**: its verdict _is_ the
run-file verdict. There is no advisory demotion any more and no second review pass to fall back on —
`clean` proceeds to Step 7, `capped` publishes its findings through
§REVIEW_READY-with-findings publication, and a
pass that produced no readable verdict is a `dispatch-failure`, not a clean run.

**Capture the rendered report** — everything `boss-review` printed _before_ the sentinel line — and
hold it for Step 7, which posts it as the single `<!-- bs-review -->` PR comment (the PR does not
exist yet at this step). Then confirm the sentinel:

- `bs-review clean:` → the pass certified the branch. Confirm the run file holds that line, record
  `boss-review: clean` in the run log, and proceed to Step 7.
- `bs-review capped:` (open must-fix remain) → record `boss-review: capped`, confirm the run file
  holds the capped line, and take §REVIEW_READY-with-findings publication above — a capped verdict
  is a **shippable** outcome, not a blocking one. Surface the open items to the human
  reviewer **in the posted comment**; the finding text itself must not reach the **PR title or
  body** in any form. Put it in the `<!-- bs-review -->` comment, which is where a human reads it,
  and nowhere else. In particular this step is bound by
  [§The reserved merge-gate token](#the-reserved-merge-gate-token-every-route-no-exceptions), which
  applies on **any** route and not only here: no text sourced from `boss-review` may place the
  substring `do not merge` in a PR title or body except through the one sanctioned PARTIAL marker,
  because that is `boss-epic`'s reserved merge-gate token, so quoting it as advisory prose blocks a
  merge nothing intended to block. Rephrase the finding rather than quoting the token.
- any `boss-review` error/timeout, or a pass that returns without a matchable sentinel → do **not**
  read the empty result as zero must-fix. Write `sentinel capped 1` through the generate-and-persist
  command at the top of this reference, record `boss-review: error (<reason>)`, and take
  §REVIEW_READY-with-findings publication with the matching `none:` coverage token. An unreviewed
  branch never reaches REVIEW_READY **silently** — it ships only with the PR body saying, in the
  mandatory sections, that no review settled anything.

**The pass writes the sentinel itself.** When it is handed `RUN_DIR` and `RUN_ID` it writes the
terminal line the moment its verdict is determined and re-affirms it as its last action (see
`boss-review`'s §Phase 7 caller sentinel contract). Your job here is to **confirm** that write, and
to perform it yourself only when the pass did not — never to overwrite a line it already wrote with
one of your own reading.

**Hard deadline — `STEP_6C_MINUTES` (default 15).** This pass's initial legs get an allowance of
`STEP_6C_MINUTES`, and it is a **cap this step enforces**, not an estimate of its worst path:
`boss-review`
runs its own fix loop of up to `$MAX_ROUNDS` fix→confirm rounds, so left unbounded this pass
never returns and the run never reaches a terminal state. Enforce it arithmetically: a felt sense
that some margin remains is a judgement call, and it bounds nothing. The allowance is derived from
the per-leg timeout, not from anything the run has already spent — the same number on the first
minute as on the fourth hour.

**Full tier only — the quick tier has already stamped.** `STEP_6C_INITIAL_LEGS=3` below is the
full tier's three legs. The quick-tier route stamped `STEP_6C_DEADLINE` from its own
`QUICK_REVIEWER_SECONDS` back at §Step 6 entry, and shell state does not survive between Bash calls,
so nothing here can detect that and skip itself: re-running this block on that route silently
restores the full allowance and leaves the shrunken one computed and unread. On the quick tier, skip
this block and carry the deadline that route already stamped.

```bash
leg_ms=${BOSS_SKILL_EXTENSION_TIMEOUT_MS:-300000}
case "$leg_ms" in '' | *[!0-9]*) leg_ms=300000 ;; esac
leg_ms=$(( 10#$leg_ms ))
[ "$leg_ms" -gt 0 ] || leg_ms=300000
DEADLINE_LEG_SECONDS=$(( (leg_ms + 999) / 1000 ))
[ "$DEADLINE_LEG_SECONDS" -ge 300 ] || DEADLINE_LEG_SECONDS=300
STEP_6C_INITIAL_LEGS=3
STEP_6C_MINUTES=$(( (STEP_6C_INITIAL_LEGS * DEADLINE_LEG_SECONDS + 59) / 60 ))
NOW=$(date +%s)
STEP_6C_DEADLINE=$(( NOW + STEP_6C_MINUTES * 60 ))          # stamp BEFORE invoking boss-review
```

That assignment block is not decoration. `$(( ))` resolves an **unset** bare name to `0` rather than
failing, so leaving `STEP_6C_MINUTES` to the formula block above would stamp `STEP_6C_DEADLINE =
NOW`, and `boss-review` would refuse its very first leg on a budget that had not been spent — a step
that reports nothing on every run, with no error to notice. The block uses the same
`BOSS_SKILL_EXTENSION_TIMEOUT_MS` normalization as `boss-review`'s own leg gate, so raising the
timeout raises this allowance in lockstep. Both variable names are deliberately unchanged:
`boss-review` binds `deadline="${STEP_6C_DEADLINE:-}"` by that exact name, so the name is an
**interface** with that skill rather than a label for the step it was first written for.

**Check the stamped allowance can fund a leg.** `STEP_6C_MINUTES` is this step's allowance, and an
allowance that cannot fund even one dispatch leg buys nothing but a guaranteed refusal. Say so
arithmetically rather than discovering it inside the pass — a misconfigured
`BOSS_SKILL_EXTENSION_TIMEOUT_MS` would otherwise leave the stamp handing `boss-review` a budget its
own first gate rejects:

```bash
STEP_6C_ALLOWANCE_SECONDS=$(( STEP_6C_DEADLINE - NOW ))
if [ "$STEP_6C_ALLOWANCE_SECONDS" -lt "$DEADLINE_LEG_SECONDS" ]; then
  BOSS_REVIEW_OUTCOME="boss-review: skipped (allowance ${STEP_6C_ALLOWANCE_SECONDS}s cannot fund one ${DEADLINE_LEG_SECONDS}s leg)"
fi
```

- **Entry gate.** Enter only when the stamped allowance can fund at least one initial dispatch leg
  — `STEP_6C_ALLOWANCE_SECONDS` at or above `DEADLINE_LEG_SECONDS`, compared in **seconds**. If it
  cannot, there is no other review pass to fall back on, so this never reaches a clean exit: write
  `sentinel capped 1`, record the stop under the sanctioned `skipped (budget)` token rather than an
  improvised one, record `boss-review: skipped (allowance <A>s cannot fund one <L>s leg)` in the
  `## Review coverage` reason, and take §REVIEW_READY-with-findings publication rather than awaiting
  a pass whose first gate is guaranteed to refuse. Both numbers, per §Allowance-disclosure rule: the
  allowance and the cost of the leg it could not fund.
- **Hand the deadline to `boss-review` itself, and require it to bound the _whole_ pass.** State
  `STEP_6C_DEADLINE` in the invocation and require the pass to check it before **every** expensive
  awaited leg it runs — its initial specialist and whole-branch passes, and the post-terminal notes
  workers a repository may have opted into, as much as its own fix loop —
  stopping at the first leg the remaining clock cannot fund and reporting whatever it has. State it
  under **exactly** that name: `boss-review` binds its gate variable with
  `deadline="${STEP_6C_DEADLINE:-}"`, so a value handed over under any other name leaves every gate
  there reading a name nothing assigned, taking the no-deadline branch, and the cap inert with both
  halves still reading as satisfied. You **await** this Skill call and cannot preempt it, so a
  deadline you keep to yourself bounds nothing — the holder of the budget is not the component that
  spends it, and an unhanded-off deadline is inert while every other instruction here still reads as
  satisfied. A deadline handed over but consulted
  **only in the fix loop** is inert the same way for everything before it: the initial
  passes run first, are awaited, and on `boss-review`'s Tier-2/Tier-3 fallback paths carry no
  extension timeout of their own, so they can spend this step's whole allowance — and overrun it —
  before the first fix-loop check is ever reached. `boss-review` gates each
  leg on its **whole allowance** (`FIX_ROUND_MINUTES` for a fix→confirm round; one dispatch batch for
  an initial pass), compared in **seconds** against
  a `date +%s` clock, and not on the deadline's mere arrival — a leg it has already started cannot be
  preempted either, so `now < deadline` would admit one that overruns by the rest of its cost.
- **What that arithmetic means at the default, computed rather than narrated.**
  At the shipped default, `DEADLINE_LEG_SECONDS=300`, `STEP_6C_INITIAL_LEGS=3`, and
  `STEP_6C_MINUTES=15`. That allowance funds those initial legs and **0 ordinary fix rounds** because
  `FIX_ROUND_SECONDS=1200` does not fit after them — the fix loop is bounded instead by
  `boss-review`'s own `$MAX_ROUNDS` round cap rather than by this stamp, which is why the two are
  bounded apart. There is one further exception, and it is bounded:
  where the pass found a must-fix **it has not yet attempted**, `boss-review` funds a single overrun
  round from its own `MUSTFIX_OVERRUN_ROUNDS` allowance rather than deferring a finding nobody tried
  to fix. That round is drawn from that allowance, once per run, and cannot repeat.
  So a report reading "the caller's deadline left 412s, and a fix round costs 1200s" is
  **this bound working as designed**, not a mis-derived deadline: 900s of allowance less the ~10
  minutes the initial pass spends leaves roughly that, and 1200s is `FIX_ROUND_MINUTES`. Recognise
  it, disclose it through the suffix below, and do not re-file it as a budget bug or re-price the
  formula to make it go away. Read that sentence with its precondition attached, though: it is the
  right reading for a pass carrying **no open, unattempted must-fix**. The same 412s reported
  alongside a located must-fix nothing has tried to fix is not this bound working — it is the
  overrun round having been skipped, and it is a bug in the pass, not in the price.
- **Exit gate.** When the call returns, compare `date +%s` against `$STEP_6C_DEADLINE`. At or past
  it, do **nothing further** in this step — no additional fix round, no re-invocation — and route on
  the verdict the pass already wrote.
- **Publish the stop.** A deadline-truncated pass is a pass that did not fully run,
  so name the rounds it did not reach in the `## Review coverage` token
  (`full (skipped: second-voice cross-model round)`) rather than letting silence read as full
  coverage. A pass that **ran and returned a capped report** is not one
  of these — it ran. Record `boss-review: capped` and leave the coverage token as `full`: the token
  describes the coverage, not the verdict, and the capped verdict routes to
  §REVIEW_READY-with-findings publication on its own merits. The zero-fix-round default above is that case, so do **not** stamp a skip on every
  full-tier run.
  A capped pass does still owe the **allowance-disclosure rule** above, so append a **suffix** to
  that `full` token — a suffix, never a new head form, so the resident enumeration Step 7 copies
  stays untouched:
  `full (boss-review capped — <N> open must-fix reported; its <M>-minute allowance funds 0 fix rounds, each costing <C> minutes)`.
  `<M>` is `STEP_6C_MINUTES` and `<C>` is `FIX_ROUND_MINUTES` — the allowance and the cost of the
  work it declined, the two separate numbers that rule requires, so a reader sees an inner box
  decline work it could not afford rather than reading it as the run running out of time. Publish the suffix on the `## Review coverage` token the
  orchestrator writes in Step 7. That step's `|`-separated list enumerates **head** forms, not the
  whole string space, so a `full` carrying this suffix **is** the `full` head it already admits:
  copy the suffix through verbatim rather than normalising it back to a bare `full`, which would
  delete the only disclosure this rule produces.

**Off switch — `BOSS_BS_REVIEW=0` means _no review_, not _no gate_.** When it is set, do not run the
pass; but this is now the only review pass there is, so a run with it set has not been reviewed.
Write `sentinel capped 1`, record `boss-review: skipped (disabled)`, publish
`## Review coverage` = `none: review stack did not run (disabled by BOSS_BS_REVIEW=0)`, and take
§REVIEW_READY-with-findings publication. Under the old three-system stack this switch dropped one
advisory pass; under a single pass it drops the gate, so it must route to a state a human looks at —
a **ready** PR carrying `please-review` and a coverage token that says no review ran — rather than to
a silent REVIEW_READY that leaves the section absent and reads as full coverage.

### Base-drift check (at the review check point)

`REVIEW_BASE`, and the base ref behind it, were resolved **once** in Preflight — and a run can
take hours to reach here. Anything that lands on the base branch meanwhile is invisible here: the reviewer
reads `$REVIEW_BASE...HEAD`, so every finding, disposition and clean verdict is computed against a
base that no longer exists upstream. The benign case surfaces at the end as a dirty merge state. The
case this check exists for does not: a semantically **overlapping** change that still merges
textually clean goes entirely unnoticed until a human reads the merged result. Re-check **here**,
immediately before the review pass is dispatched, and act on a hit rather than recording one and
reviewing on regardless.

**The dispatch supplies the base identity; never re-derive it.** `BASE_REF`, `BASE_REMOTE` and
`BASE_BRANCH` arrive from the orchestrator's Step 6 dispatch under exactly those names. If any of
the three is missing, or the detector is not installed, **skip the check and record which one was
missing**. Re-deriving is a guess about which branch this work is even for, and a wrong guess
rebases onto the wrong history; a recorded skip is a degradation the reader can see, which a silent
pass is not.

**Bounded, not budgeted.** Like the API-surface check, this is one single-ref fetch and a
couple of local git reads, so it draws nothing from the review pass's leg clamps. Run it **before**
`STEP_6C_DEADLINE` is stamped: that allowance is measured against the current
tree, and a rebase after the stamp leaves it describing a tree that no longer exists.

```bash
DRIFT_JSON=''
# Pin `REVIEW_BASE` to an OID BEFORE the fetch below, and note that this runs whether
# or not the check goes on to fire. On a resume the orchestrator binds `REVIEW_BASE` to the ref NAME
# `refs/remotes/<remote>/<branch>` — and the fetch three lines down force-updates exactly that ref.
# Unpinned, `REVIEW_BASE` therefore follows the base tip on a round where nothing rebased, silently
# breaking this section's own rule that only a successful rebase re-binds it. The damage lands on
# the review pass: it resolves its range from this binding, so a base tip that is not an ancestor of
# HEAD moves that range's left endpoint — and any consumer that reads the binding as a two-dot
# `BASE..HEAD` range then renders the base branch's own commits as deletions this branch never
# made, a corrupted review artifact produced by the check meant to protect it. Pinning also removes
# the window between reading the ref and using it.
REVIEW_BASE_OID="$(git rev-parse --verify --quiet "${REVIEW_BASE:-}^{commit}")" || REVIEW_BASE_OID=''
if [ -n "$REVIEW_BASE_OID" ]; then REVIEW_BASE="$REVIEW_BASE_OID"; fi
# PRINT it. The assignment above dies with this Bash call for exactly the reason the next comment
# gives for re-deriving `BOSS_BUILD_TOOLBOX`, and YOU — not the shell — are what carries
# `REVIEW_BASE` from here to the review pass's own `$REVIEW_BASE...HEAD` range. A pin nobody reads
# back is an inert line that every gate still reads as satisfied.
printf 'base-drift: REVIEW_BASE pinned to %s\n' "${REVIEW_BASE_OID:-<unresolved>}" >&2
# Re-derive `BOSS_BUILD_TOOLBOX` the way SKILL.md's own preambles do. It was exported in an EARLIER
# Bash call, in the ORCHESTRATOR's shell, and shell state survives neither between calls nor into
# this dispatched subagent. Left unguarded it is the worst kind of failure: `[ ! -f
# "/base-drift.mjs" ]` is true, so the block reports "not installed in this toolbox" on every round
# forever — a recorded skip whose stated reason is false, which reads as a host problem nobody can
# reproduce. Under `set -u` it aborts the round instead.
if [ -z "${BOSS_BUILD_TOOLBOX:-}" ]; then
  for candidate in "$HOME/.claude/skills" "$HOME/.codex/skills"; do
    if [ -d "$candidate/boss-build/toolbox" ]; then BOSS_BUILD_TOOLBOX="$candidate/boss-build/toolbox"; break; fi
  done
fi
if [ -z "${BASE_REF:-}" ] || [ -z "${BASE_REMOTE:-}" ] || [ -z "${BASE_BRANCH:-}" ]; then
  echo "base-drift SKIPPED: dispatch supplied no BASE_REF/BASE_REMOTE/BASE_BRANCH" >&2
elif [ -z "${BOSS_BUILD_TOOLBOX:-}" ]; then
  echo "base-drift SKIPPED: no boss-build toolbox on this host to resolve base-drift.mjs from" >&2
elif [ ! -f "$BOSS_BUILD_TOOLBOX/base-drift.mjs" ]; then
  echo "base-drift SKIPPED: base-drift.mjs is not installed in this toolbox" >&2
else
  # A failed fetch is an INPUT to the report, never only a log line. FETCH_FAILED is deliberately
  # unquoted below so that empty expands to no argument at all.
  FETCH_FAILED=''
  git fetch --no-tags "$BASE_REMOTE" "+refs/heads/$BASE_BRANCH:$BASE_REF" \
    || FETCH_FAILED='--fetch-failed'
  DRIFT_JSON="$(node "$BOSS_BUILD_TOOLBOX/base-drift.mjs" check \
    --repo "$(git rev-parse --show-toplevel)" \
    --base "$BASE_REF" --head "$(git rev-parse HEAD)" $FETCH_FAILED)" || DRIFT_JSON=''
  if [ -z "$DRIFT_JSON" ]; then
    echo "base-drift UNEVALUATED: the detector returned no report" >&2
  else
    printf '%s\n' "$DRIFT_JSON"
  fi
fi
```

**The fetch moves the ref `REVIEW_BASE` may be named after, so pin it first.** This is the one way
the check can damage the round it opens rather than inform it. A resume binds `REVIEW_BASE` to the
base ref's **name**, and this step force-updates that name every round; the reviewer's own diff
range is resolved from it, so an unpinned binding hands round N a diff in which the base branch's
commits appear as deletions. The pin is the first two lines of the block above, ahead of the fetch
and ahead of the toolbox guard, so it holds on the skip paths too. **Read the printed OID back and
use it as `REVIEW_BASE` for the rest of this round** — it is what the review pass's own range
expands from, and a shell assignment cannot reach that dispatch on its own; when the line prints
`<unresolved>` the ref named
nothing this repo holds, so record that and keep the existing binding rather than inventing one. It
is not a substitute for the
re-bind below: a rebase still re-binds `REVIEW_BASE` deliberately, to the new tip. What the pin
forbids is the ref moving underneath the variable when nothing rebased.

**A failed fetch is an input to the report, not a log line.** `--fetch-failed` is what carries it:
the detector then returns `behind: unevaluated` with a note saying the ref could not be refreshed.
Without it the check reads a stale `BASE_REF`, plausibly counts `behind: 0`, and publishes the flat
string `Base drift: none.` — a proven-unmoved verdict nobody obtained, and exactly the substitution
the reviewer brief below forbids for this note.

The report carries `behind` (commits the base holds that HEAD does not), `intersection` (paths both
sides changed since the merge base), `mergeTree` (`clean` / `conflicts` / `skipped` / `unevaluated`),
and a one-line `note`. **`unevaluated` is not `clean`** — it is the detector saying it could not
tell, on either field, and it is never to be read as "no drift" or as "no overlap". Treat an
`unevaluated` `behind` as a base that may have moved, and an `unevaluated` `mergeTree` as an overlap
nobody checked. **`skipped` is neither**: it is the probe the detector did not need to run, because
nothing overlapped for it to answer.

**Act on a hit — rebase, never merge.** `boss-finalize`'s gate and the straight-line-history
invariant both forbid merging the base into the branch, so the only legal way to re-seat this branch
on the moved base is a rebase, and this check point is the only safe moment for one: the review pass
has not been dispatched yet, so no reviewer or fixer leg is in flight.

**Two readings count as a hit, and only one of them rebases.** Classify the report against its own
fields, in this order:

- **Refreshable drift** — `behind` is a positive integer **and** `intersection` is non-empty **and**
  `mergeTree` is `clean`. This is the only reading that rebases.
- **Unrefreshable drift** — `behind` is `unevaluated`; **or** `mergeTree` is `unevaluated`; **or**
  `mergeTree` is `conflicts`. Do **not** rebase: record the drift, publish
  the note, brief the reviewer, and review on.

A report matching neither is not a hit and the review pass proceeds untouched.

**Every test above reads one field's own value — never reconstruct one from a pair.** `mergeTree`
carries three non-`clean` values and they do not mean the same thing. `conflicts` is a probe that
ran and did not reconcile. `unevaluated` is a probe that was **needed and could not run** — no merge
base, a failed changed-path diff, a git without `merge-tree --write-tree`. `skipped` is a probe that
was **not needed**, because nothing overlapped for it to answer: the base has not moved, or the base
moved on paths this branch never touched. Only `unevaluated` is a hit; `skipped` is the ordinary
healthy round and matches neither reading, exactly as the closing line above says.
A trigger that cannot tell those two apart fires on a branch with
**no drift at all** — on the unmoved base, and on every run where the base merely moved somewhere
else, which is most of them — and a drift note published on most runs is how a real hit stops being
visible. An earlier revision tried to recover the distinction from a second field (`stage2` is `true`
**and** `mergeTree` is `unevaluated`); that inference is what shipped the misfire on the
moved-but-disjoint case, where `stage2` is `true` and the probe was simply unnecessary. The
detector now draws the distinction on the field itself, so read it there: a rule a model executes
unattended must be a lookup, not a derivation. `intersection` is an array
and can never hold the string `unevaluated`, so never test it for one.

**A clean `mergeTree` is the rebase's precondition, not a nicety.** It is the only reading in which
this branch and the moved base are known to reconcile. On `conflicts` or on `unevaluated` an
unattended rebase attempts a reconciliation nobody has evidence for, at a point whose only
recovery is an abort — so those readings are recorded and briefed instead, and `boss-finalize` still
owns the reconciliation at the end. This is deliberately narrower than "rebase whenever the paths
overlap".

On a **refreshable** reading do all of the following in one go. On an **unrefreshable** one do
only **Brief the reviewer** and then publish the note as below — no rebase happened, so the re-bind,
the force-full-pass rule and the cap all have nothing to act on:

- **Rebase onto the moved base** (refreshable reading only). Check the tree **first**, the way the
  tag injector already does: `git status --porcelain --untracked-files=no` must be empty. `git
rebase` refuses outright on unstaged changes and `git rebase --abort` then exits 128 with "no
  rebase in progress", so the two post-conditions below would report a worktree left mid-rebase by a
  rebase that never started — a BLOCKED whose stated cause is the opposite of what happened. Step 6
  has already committed the implementation, so a dirty tree here is an ordinary
  state, not an edge case. A dirty tree is a recorded skip with its own reason: keep the old base,
  publish the note, and review on. That pre-condition
  belongs here, ahead of the command, because a top-down executor acts at the first mention — a
  prohibition stated three bullets later is read after the rebase it forbids. Only on a clean tree
  run `git rebase "$BASE_REF"`. On failure run `git rebase --abort`,
  then confirm the abort actually landed before continuing: two post-conditions, both required —
  `git rev-parse --verify --quiet REBASE_HEAD` prints nothing, and `git status --porcelain` is
  empty. An abort that reports success while either still holds has left the worktree mid-rebase,
  where no further review is meaningful; route to **BLOCKED** with the drift note attached — a
  worktree stuck mid-rebase can neither be committed nor pushed, so that is cause (2), not a review
  finding. A clean
  abort is a recorded skip instead: keep the old base, publish the note, and review on.
- **Re-bind `REVIEW_BASE`** to `$(git rev-parse "$BASE_REF")` immediately after a successful rebase.
  It is the binding every later consumer expands — the review pass's own `$REVIEW_BASE...HEAD`
  range in both tiers, and, on the full tier only, the range its `second-voice` round reads —
  so a rebase that moves the branch without moving this variable leaves all of them reviewing a
  range whose left endpoint is no longer an ancestor of HEAD.
- **Force the review pass's round 1 to `mode=full`** — as the Round Scope Contract already requires
  — and only after a rebase actually happened. The rebase lands _before_ the pass is dispatched, so
  the pass is the first to read the moved base and the first whose findings carry post-rebase line
  numbers. Brief the pass that its own oscillation guard must compare at **file** level, not
  `file:line`, for that first round only, and return to `file:line` the round after: a rebase
  rewrites every commit, so a shifted line number makes `file:line` equality stop matching a
  surviving finding and a genuine oscillation read as two unrelated ones — the guard failing open. A
  check
  that detected drift but did **not** rebase changes nothing about the diff, so it raises nothing.
- **Cap the rebases.** At most **one** rebase per run, `BS_BASE_DRIFT_MAX_REBASES` clamped
  **lower-only** to that default of 1 exactly as `BS_REVIEW_MAX_ROUNDS` is (invalid / absent / too
  high → 1; the env may only lower it, to 0). A base branch under active traffic can move while a
  run is in flight, and an uncapped rule turns a bounded review into a rebase treadmill that never
  converges. Once the cap is spent, later boundaries still **check** and still publish the note —
  they simply stop rebasing.
- **Brief the reviewer** (both readings). Carry the report's `note` into the `boss-review`
  invocation **verbatim**, as its base-drift note — the pass has no other channel to it. A textually
  clean overlap tells the reviewer exactly what git cannot: these paths were changed by someone else
  too, and the merge hid it. When the check was skipped, or either field came back `unevaluated`,
  the note says which — never substitute
  `Base drift: none.` for an answer nobody obtained, because that flat string asserts a base proven
  not to have moved, which is the one thing an unrun check cannot establish.

**Publish the note on every route out — the clean one first, then the reduced-coverage, `BLOCKED`
and `PARTIAL` ones.** The clean
route is the one this check exists for: the incident behind it was a run that went green and found
the overlap only after the final push, so a rule that named only the failure routes would leave the
primary one silent. On the **clean** route the orchestrator writes each returned drift note verbatim
under `## Autonomous decisions` in the Step 7 PR body — it is not a decision the run made, but it is
the section a reader looks in for what the run did about its own environment, and Step 7 is the only
step that writes a body. The note travels there through the returned-result contract at the top of
this file, which is why it is on that list at all.

The non-Step-7 routes then need their own rule for the opposite reason: a run that publishes through
§REVIEW_READY-with-findings, §PARTIAL-route or §BLOCKED-route publication never reaches Step 7 and so
never writes a Step 7 body, yet its
drift is precisely what the next run needs to know before it re-derives anything. Carry the note into
each of those routes' `## Autonomous decisions` section alongside the rest.

### API-surface check (conditional, required — do this before the clean exit)

When the branch diff touches `proto/bossanova/v1/**`, `services/bosso/internal/server/**`, or
`lib/bossalib/apiversion/**` — or presents a **hidden behavioral change** (a handler's response
values, defaults, or enum set changed in business logic without a proto edit) — run the `api-review`
classification (that skill's Phase 1 file buckets + Phase 3 observable-change decision tree). If the
change is **observable** on the `bossanova.v1` surface and **no** matching `lib/bossalib/apiversion`
date-based version bump + down-convert transform + test is present, that missing bump is a
**required** must-fix finding — never a Minor/deferrable one.

Handle it like any must-fix: hand it to the review pass's own fix loop (add the version + transform +
test). If the round cap forces it to be deferred, it is a **required-deferred** item —
record it by name in the PR body and route the run to **BLOCKED** (never REVIEW_READY) — this is the
one review finding that is a blocking cause in its own right, `BLOCKED` cause (3) in the `SKILL.md`
Hard rules. A missing required version bump must never fall silently into the optional/deferred
(Minor) bucket, and it is the sole exception to the rule that open findings publish rather than
block.

**The bound — apply it in BOTH tiers.** The check is conditional, so neither tier sets an allowance
for it ahead of time: like the review pass's own stamped allowance it is **bounded instead of
budgeted**, and the block below is that bound. The bound is the same number in either tier — it is
derived from the same per-dispatch leg every other allowance in this step is derived from — so a
quick-tier run and a full-tier run give this classification exactly the same room. The bound is
cooperative because the classification is awaited.

**Assign the constant here before the arithmetic.** This block is the only place the name is set in
any shell that reaches this snippet — and an unset name is **zero** inside `$(( ))`. Without the
assignment `API_CHECK_SECONDS` computes to `0`, and the rule below then routes every triggering run
to the capped route without ever running the check its tier owes:

```bash
leg_ms=${BOSS_SKILL_EXTENSION_TIMEOUT_MS:-300000}
case "$leg_ms" in '' | *[!0-9]*) leg_ms=300000 ;; esac
leg_ms=$(( 10#$leg_ms ))
[ "$leg_ms" -gt 0 ] || leg_ms=300000
API_CHECK_SECONDS=$(( (leg_ms + 999) / 1000 ))
[ "$API_CHECK_SECONDS" -ge 300 ] || API_CHECK_SECONDS=300
```

One leg, because the classification is a single awaited dispatch. Nothing here reads a run clock: the
number is the same on the first minute of a run and on its fourth hour.

Dispatch the API classification with `API_CHECK_SECONDS` as its hard return-by. If it cannot be
dispatched at all, or does not return within that allowance, write the normal generated capped
sentinel and take the capped route: an unrun required API gate cannot produce a clean verdict in
**either** tier. A _reported_ missing API-version bump is different — that is BLOCKED cause (3),
decided by the configured API-compatibility lens role and not by this clamp.
