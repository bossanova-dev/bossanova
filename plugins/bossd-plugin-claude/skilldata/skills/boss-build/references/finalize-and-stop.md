# Finalize and stop — Steps 8-12

Read this when the run first routes to Steps 8–12: normally when Step 7 has opened (or reused) the
PR, or earlier when a terminal path such as Step 2.5 `foreign` must run Step 12. On a pre-PR terminal
route, run Step 12 only.

Initialize the terminal-route receipt before the first route obligation can stamp it:

```bash
BOSS_BUILD_ROUTE_RECEIPT="${BOSS_BUILD_ROUTE_RECEIPT:-$(mktemp -t boss-build-route.XXXXXX.json)}"
export BOSS_BUILD_ROUTE_RECEIPT
```

## Step 8: Tag commits, then repair to green (boss-repair, capped)

### Test gate cache contract

Before a cache-eligible test gate runs, consult a content-addressed gate cache keyed on the whole
working-tree content hash, the fully resolved command, the merge-base commit for the PR base, and the
gate-expansion fingerprint. The fingerprint includes toolchain versions plus the
repo-declared variables that change the selected runner or arguments. A hit skips the gate and
reports `cached` with the 12-character tree hash. A miss runs the gate and reports `fresh`; record a
stamp only after a zero-exit result.

The cache is opt-in per gate. A gate that the repo has not declared eligible, a gate that reads state
outside the hashed corpus, a nondeterministic gate, an unusable stamp directory, an unresolvable base
commit, or a failing git read all fail open to a fresh run and never yield a hit. Treat a `make`
target as an alias: the cache key uses the fully resolved command, while the eligibility lookup uses
the normalized gate site.

Order matters. Check the exact cache key first. Only on a miss, when the branch adds or renames a
file (including an uncommitted or untracked one), force the resolved gate through repo-declared
uncached behavior guarded by `commands.testUncached`; if the repo has no such command, keep caching
disabled for that gate and state why rather than guessing a runner flag.

**Inject the PR-number tag and force-push _before_ the green gate**, so CI runs once on the tagged
head. This is the finalize adapter's **inject-PR-tag** capability (`toolbox/finalize/cli.mjs
inject-pr-tag`, which delegates to the dependency-free `boss-finalize` helper at
`~/.claude/skills/boss-finalize/`, reachable in a cron worktree). **Tag-only, no squash** —
preserve the per-task commits. The PR was created in Step 7; this does **not** re-create it.

```bash
# PR_NUMBER was captured in Step 7; re-derive if unset (resume / fresh shell).
PR_NUMBER="${PR_NUMBER:-$(gh pr list --head "$SESSION_BRANCH" --state open --json number -q '.[0].number // empty')}"
test -n "$PR_NUMBER" || exit 1
BOSS_SKILLS_HOME="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}"
if [ ! -d "$BOSS_SKILLS_HOME/boss-build/toolbox" ]; then BOSS_SKILLS_HOME="$HOME/.codex/skills"; fi
BOSS_BUILD_TOOLBOX="$BOSS_SKILLS_HOME/boss-build/toolbox"
test -f "$BOSS_BUILD_TOOLBOX/finalize/cli.mjs" || exit 1
BASE_BRANCH="$(gh pr view "$PR_NUMBER" --json baseRefName -q .baseRefName)"
git fetch origin "$BASE_BRANCH"
# Rebase all commits since the PR base and inject [#PR_NUMBER] into any missing it.
# Run with a 600s tool timeout. Redirect output to a file, never to head/tail; a
# SIGPIPE during rebase can strand HEAD between commits. If the tool is killed or
# times out, check $(git rev-parse --git-path rebase-merge) and rebase-apply before
# retrying, then follow add-pr-numbers.sh cleanup_temp guidance to continue or abort.
TAG_LOG="$(mktemp -t boss-build-inject-pr-tag.XXXXXX.log)"
BASE_BRANCH="$BASE_BRANCH" node "$BOSS_BUILD_TOOLBOX/finalize/cli.mjs" inject-pr-tag "$PR_NUMBER" >"$TAG_LOG" 2>&1
git push --force-with-lease origin "$SESSION_BRANCH"
test "$(git rev-parse HEAD)" = "$(git rev-parse @{u})" || exit 1  # HEAD == upstream
```

> inject-PR-tag rewrites history (rebase). A daemon `pull --rebase` could race the force-push;
> `--force-with-lease` plus the `HEAD == @{u}` assertion guard against clobbering a concurrent advance.
> If the lease is rejected, re-fetch and re-run the block.
>
> **Linear history:** sync with the base by rebasing only — never by merging it in. Keep
> `git rev-list --merges --count "origin/$BASE_BRANCH"..HEAD` at `0`; boss-repair carries the
> full invariant and the linearize recovery.

Then run **boss-repair** (the finalize adapter's repair capability) to fix failing checks, rebase
conflicts, and review comments. Cap at `policy.repairCap` (**5**) passes. If still red after the cap:
keep the work as a **draft** PR, leave the ticket **In Progress**, post a blocker comment (failing
check name, `file:line`, what was attempted), then go to **Stop cleanly** with BLOCKED. This is
`BLOCKED` cause (1) — red quality gates.

Set `BOSS_NOTES_SUPPRESSED=1` in the environment each repair pass runs under, and state it in the
invocation; this run owns the single post-terminal notes dispatch for the whole top-level run (Step 12).

```bash
node "$BOSS_BUILD_TOOLBOX/finalize/route-contract.mjs" stamp --receipt "$BOSS_BUILD_ROUTE_RECEIPT" --token blocked-pr-left-draft --run-id "$BLI_RUNID"
```

Before blocking on this green gate, arm the one-shot callback watches for the tagged head —
`resolveCallbackAdapter(env)` `registerWatch` (`boss callback add "$PR_NUMBER" <trigger> --group ...`)
for each `policy.watchTriggers`, with each non-exclusive trigger under its own group. On every
wake **reconcile against real state before acting** (`gh pr checks`/`gh pr view`), re-arm while still
waiting, dedup by callback id, and back it with that reference's **bounded** fallback poll — its
Protocol step 5 loop, never a bare `gh pr checks --watch --fail-fast` (used directly when callbacks
are unavailable). Full protocol: [`callback-watches.md`](callback-watches.md).

**Never wait on CI with a fixed `sleep`.** Every wait on this PR's checks or merge state — this green
gate, Step 9's two re-push waits, Step 10's settle opener, and the two green readings
[`review-stack.md`](review-stack.md) takes on the routes that skip Step 9 — arms the watches when
`callbacksAvailable(env)` is true and runs the bounded Protocol step 5 poll either way. A fixed
`sleep` of **60 seconds or longer** spent waiting for CI is a defect, and so is any "wait N minutes,
then look" instruction standing in for the wait: a guessed duration is not a reading, and it returns
green-looking on a PR whose checks never reported. This leaves the short bounded backoffs inside a
retry loop untouched (`sleep 10` between mergeability reads, `sleep "$CI_WAIT_INTERVAL"` between the
poll's own reads): those pace a bounded read that is already running, they do not stand in for a wait.

## Step 9: Finalize (idempotent tag guard, ready), Linear writeback

Step 9 is an **idempotent** guard: re-inject **only** if `boss-repair` added untagged fix-commits,
then ready the PR. In the common path there is **no rewrite, no push, no second full CI wait**.

```bash
PR_NUMBER="${PR_NUMBER:-$(gh pr list --head "$SESSION_BRANCH" --state open --json number -q '.[0].number // empty')}"
test -n "$PR_NUMBER" || exit 1
BOSS_SKILLS_HOME="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}"
if [ ! -d "$BOSS_SKILLS_HOME/boss-build/toolbox" ]; then BOSS_SKILLS_HOME="$HOME/.codex/skills"; fi
BOSS_BUILD_TOOLBOX="$BOSS_SKILLS_HOME/boss-build/toolbox"
test -f "$BOSS_BUILD_TOOLBOX/finalize/cli.mjs" || exit 1
BASE_BRANCH="$(gh pr view "$PR_NUMBER" --json baseRefName -q .baseRefName)"
git fetch origin "$BASE_BRANCH"
# Re-inject only if boss-repair added tagless non-empty commits; else no rewrite,
# no push, no second CI wait. Empty bootstrap commits are exempt by tree equality,
# not subject text.
UNTAGGED_NONEMPTY="$(
  git log "origin/$BASE_BRANCH"..HEAD --format='%H%x09%s' |
    while IFS=$'\t' read -r sha subject; do
      if ! tree=$(git show -s --format=%T "$sha"); then exit 1; fi
      parent=$(git rev-parse --verify "$sha^" 2>/dev/null || true)
      if [ -n "$parent" ]; then
        if ! parent_tree=$(git show -s --format=%T "$parent"); then exit 1; fi
      else
        if ! parent_tree=$(git hash-object -t tree /dev/null); then exit 1; fi
      fi
      if [ "$tree" = "$parent_tree" ]; then continue; fi
      case "$subject" in *"[#$PR_NUMBER]"*) ;; *) printf '%s %s\n' "${sha:0:12}" "$subject";; esac
    done
)"
if [ -n "$UNTAGGED_NONEMPTY" ]; then
  BASE_BRANCH="$BASE_BRANCH" node "$BOSS_BUILD_TOOLBOX/finalize/cli.mjs" inject-pr-tag "$PR_NUMBER"
  git push --force-with-lease origin "$SESSION_BRANCH"
test "$(git rev-parse HEAD)" = "$(git rev-parse @{u})" || exit 1  # HEAD == upstream (lease rejected → re-fetch, re-run)
  # CI wait: callback-watches.md Protocol step 5's bounded poll, never a bare `--watch`. Green
  # ONLY on CI_WAIT_STATE=settled; timeout/unknown are not green. Red → back to Step 8 (boss-repair)
fi
# Gate mergeability before readying. GitHub may report UNKNOWN briefly after a push, so poll with
# a bound; CONFLICTING or any dirty mergeStateStatus means rebase onto the base, run the
# configured commands.postRebase check, push, wait for checks, and re-read mergeability.
for attempt in 1 2 3 4 5 6; do
  PR_STATE="$(gh pr view "$PR_NUMBER" --json isDraft,mergeable,mergeStateStatus)"
  MERGEABLE="$(printf '%s' "$PR_STATE" | jq -r .mergeable)"
  MERGE_STATE="$(printf '%s' "$PR_STATE" | jq -r .mergeStateStatus)"
  if [ "$MERGEABLE" != "UNKNOWN" ]; then break; fi
  sleep 10
done
if [ "$MERGEABLE" != "MERGEABLE" ] || [ "$MERGE_STATE" = "DIRTY" ] || [ "$MERGE_STATE" = "BLOCKED" ]; then
  git rebase "origin/$BASE_BRANCH"
  POST_REBASE_CHECK="$(node --input-type=module -e 'import{pathToFileURL as u}from"node:url"; const m=await import(u(process.env.BOSS_BUILD_TOOLBOX+"/skill-config.mjs").href); process.stdout.write(m.command(m.loadSkillConfig({cwd:process.cwd()}),"postRebase")||"")')"
  test -n "$POST_REBASE_CHECK" || { echo "commands.postRebase is not configured"; exit 1; }
  sh -c "$POST_REBASE_CHECK"
  git push --force-with-lease origin "$SESSION_BRANCH"
  # CI wait: callback-watches.md Protocol step 5's bounded poll (settled only; never a bare --watch)
  PR_STATE="$(gh pr view "$PR_NUMBER" --json isDraft,mergeable,mergeStateStatus)"
  MERGEABLE="$(printf '%s' "$PR_STATE" | jq -r .mergeable)"
  MERGE_STATE="$(printf '%s' "$PR_STATE" | jq -r .mergeStateStatus)"
  test "$MERGEABLE" = "MERGEABLE" || exit 1
  test "$MERGE_STATE" != "DIRTY" || exit 1
fi
# Ready the PR — the finalize adapter's readyPr capability (isDraft==true guard; command: gh pr ready).
if [ "$(printf '%s' "$PR_STATE" | jq -r .isDraft)" = "true" ]; then gh pr ready "$PR_NUMBER"; fi
test "$(gh pr view "$PR_NUMBER" --json isDraft -q .isDraft)" = "false" || exit 1
```

> If the re-inject branch's bounded CI wait goes red, route back to **Step 8
> (boss-repair)**; never move the ticket to **In Review** with non-green checks. Both waits above are
> **driven by the one-shot callback watches whenever `callbacksAvailable(env)` is true**
> ([`callback-watches.md`](callback-watches.md)) — armed before the run blocks on anything, re-armed
> after the force-push, and reconciled against real check state on wake. That reference's Protocol
> step 5 bounded poll backs the wait on **both** branches: safety net under a true gate, sole wait
> mechanism under a false one. Remove the live watches once the PR is readied.
> The mergeability gate runs strictly before `gh pr ready`: readying itself can make GitHub report an
> unstable merge state while review automation starts, and that post-ready state is not a conflict
> signal. `UNKNOWN` is unsettled, never a pass; after the bounded poll it takes the same
> rebase-then-re-verify path as a dirty state.

Before readying, confirm **no required item was deferred** (Hard rules) — this now includes **every
in-scope acceptance criterion being satisfied**: each `- [ ]` this ticket was scoped to close must be
ticked `- [x]` and demonstrated by the diff/tests (a criterion whose evidence is a captured-proof
artifact counts as satisfied once the diff/tests demonstrate it — proof _capture_ is the non-fatal
Step 11, not a ready gate), **with one exception**: a `(verify-only)` criterion is demonstrated by
recorded evidence rather than by a diff, under the gate immediately below. **Partial implementation
is not complete.** A deferred required item takes this run off the plain `REVIEW_READY` route, but
**which** route it takes depends on the item, and only one of the three is a blocker:

- an **unsatisfied in-scope criterion** → the `PARTIAL` gate below, on a green pushed branch;
- an **open must-fix** review finding → `REVIEW_READY` with the findings **published**, per
  [`review-stack.md`](review-stack.md) §REVIEW_READY-with-findings publication. Ready the PR and
  apply `please-review`;
- a **missing required API-version bump or down-convert transform**, per the configured
  API-compatibility lens role → finalize BLOCKED (Step 12) naming it, and do not ready. This is
  `BLOCKED` cause (3), the one review finding that blocks in its own right.

Name whichever item deferred in the PR body either way. After the PR is ready, add `please-review` if missing. Then move
the ticket from in-progress to in-review (`.inProgress → .inReview`) via the adapter's `moveState` capability, and comment the PR URL
(the adapter's `writeComment` capability).

### The verify-only evidence gate

A plan marks a criterion whose correct outcome is _"this file needed no change"_ with the literal
`(verify-only)` prefix and a `— check:` command; the run discharges it by **recording the check it
actually ran**:

```
- [x] (verify-only) <the invariant claimed> — checked: `<command>` → <result>
```

**Backtick the command.** The `— checked:` clause must wrap its command in backticks, exactly as the
template above shows. An undelimited command reads as **no command at all** and the gate blocks
naming the criterion.

Before readying, run `validateVerifyOnlyEvidence(config, body)` from `toolbox/skill-config.mjs` over
the **PR body**. It returns `{ ok, verifyOnly, missingEvidence, malformedMarker, advisory }`, and
`ok` is true only when every criterion that is both marked and ticked carries a **non-empty** command,
a **non-empty** result, and a statically resolvable command head. Every `missingEvidence` item carries
a closed-set `reason` plus a one-line `remedy`: `no-clause`, `undelimited-command`,
`planned-tense-on-ticked`, `empty-command`, `empty-result`, or `command-unresolvable`. An `ok:false`
result makes each criterion it names a **deferred required item**, of the unsatisfied-in-scope-criterion
kind: name each reason/remedy in the PR body and route through the `PARTIAL` gate below, not through
`BLOCKED`. An **unticked** marked criterion is not a failure of this gate — it is already an open
in-scope criterion under the rule above.

`malformedMarker` reports a criterion whose text contains `(verify-only)` but does not begin with it
after markdown emphasis is stripped. It is a warning bucket, not reclassification: the literal marker
is prefix-only by contract. `advisory` reports static proof-quality risks that do **not** set
`ok:false` and do **not** block readying: working-tree-scoped git evidence with no committed anchor,
zero-selection filters with no count assertion, unquoted option globs, pipelines with no pipefail,
`git grep -E` word-boundary usage, and cached Bazel tests without `--nocache_test_results`.

The gate checks **structure, non-emptiness and decidable command resolvability, never truth**; it
does not execute the recorded command.

It is also **not** a completeness check: a criterion omitted from the body entirely is invisible to
it. Completeness is owned by the rule above; run both.

```bash
node "$BOSS_BUILD_TOOLBOX/finalize/route-contract.mjs" stamp --receipt "$BOSS_BUILD_ROUTE_RECEIPT" --token verify-only-evidence-validated --run-id "$BLI_RUNID"
```

### The premise discharge gate

Before reporting `REVIEW_READY`, restate the plan's central premise from `## Premises` and the
evidence that it still holds on this branch. If the central premise no longer holds, terminate
BLOCKED with a written refutation even when every acceptance criterion is satisfied. This is
`BLOCKED` cause (4). When merged work inverted the premise and the run took the documented-departure
route, the PR body and tracker comment must name the criterion verbatim and the merged change that
inverted it before the PR is readied.

```bash
node "$BOSS_BUILD_TOOLBOX/finalize/route-contract.mjs" stamp --receipt "$BOSS_BUILD_ROUTE_RECEIPT" --token premise-discharged --run-id "$BLI_RUNID"
node "$BOSS_BUILD_TOOLBOX/finalize/route-contract.mjs" stamp --receipt "$BOSS_BUILD_ROUTE_RECEIPT" --token required-deferred-asserted --run-id "$BLI_RUNID"
node "$BOSS_BUILD_TOOLBOX/finalize/route-contract.mjs" stamp --receipt "$BOSS_BUILD_ROUTE_RECEIPT" --token pr-ready --run-id "$BLI_RUNID"
node "$BOSS_BUILD_TOOLBOX/finalize/route-contract.mjs" stamp --receipt "$BOSS_BUILD_ROUTE_RECEIPT" --token please-review-added --run-id "$BLI_RUNID"
```

**Reclassification — the route out of an automatic BLOCK.** When a plan lands an in-scope criterion
the diff cannot demonstrate and the correct outcome genuinely is "no change was needed", you may
discharge it as verify-only **by writing the discharge form yourself**: run a real check, record its
command — **in backticks**, per the rule above — and its result in the PR body's
`## Acceptance criteria` block, and note the reclassification under `## Autonomous decisions`. It is
then subject to this same gate. An unmarked criterion with no evidence line stays required-deferred.

**The `PARTIAL` gate — one meaning, and only one.** `PARTIAL` says exactly one thing: _the branch is
green and pushed, a reviewer certified real progress, and what is left undone is **only** in-scope
acceptance criteria this run did not meet._ Classify the deferred required items first, then take the
`PARTIAL` route when **all three** hold:

- **T1** — at least one in-scope acceptance criterion is satisfied **and independently certified**:
  certification is a **positive** record, never an absence. Both halves are required — boss-review's
  always-on acceptance-criteria certification **ran** over the full supplied criteria list and
  returned a verdict a **reviewer** authored, **and** it emitted no `lens: acceptance-criteria`
  must-fix naming that criterion. The second half alone certifies vacuously on a branch no lens ever
  read, so a `capped` verdict a run generated for itself fails T1 by construction. An agent's own
  assertion that a criterion is done is never certification, and a run that satisfied **zero**
  criteria is never `PARTIAL`.
- **T2** — the branch is **green** at this step's green gate, after boss-repair capped at
  `policy.repairCap`. A run that arrives at `PARTIAL` from the review loop's `capped` arm never
  reached this step, so its greenness is not inherited from here: that route takes the same reading
  itself, against the PR it publishes, per [`review-stack.md`](review-stack.md) §PARTIAL-route
  publication. Unmeasured is not green.
- **T3** — every deferred required item is **only** an unsatisfied in-scope acceptance criterion:
  zero open must-fix findings from any lens other than `acceptance-criteria`; no missing API-version
  bump or down-convert transform reported by the configured API-compatibility lens role; no
  unattributed residue or untagged commits; the reviewed-tip confirmation held; no hard-ABORT
  condition fired.

**What a failed conjunct means — and it is usually not `BLOCKED`.** Falling out of this gate says
only that `PARTIAL` is the wrong label. Route by what actually failed: a red or unmeasurable **T2**
is `BLOCKED` cause (1) and an unpushable branch is cause (2); a reported missing API-version bump is
cause (3) and a hard ABORT is cause (4). Everything else that fails **T1** or **T3** on a green,
pushed branch — an uncertified criterion, a capped or unreadable verdict, an open must-fix from
another lens — ships `REVIEW_READY` with those items published, per
[`review-stack.md`](review-stack.md) §REVIEW_READY-with-findings publication. On the `PARTIAL` route do **not** run the **review-move** half of
the block above: **never** move the ticket to `.inReview`, **never** add `please-review`, and leave
the ticket in the `.inProgress` role. The **ready** half still runs — the PR **is** made non-draft —
but it carries the do-not-merge marker and no `please-review` label. Comment the PR URL on the ticket
alongside the partial enumeration (the adapter's `writeComment` capability). The PR-body and
ticket-comment authoring belongs to [`review-stack.md`](review-stack.md) §PARTIAL-route publication;
do not improvise either here.

```bash
node "$BOSS_BUILD_TOOLBOX/finalize/route-contract.mjs" stamp --receipt "$BOSS_BUILD_ROUTE_RECEIPT" --token partial-gate-satisfied --run-id "$BLI_RUNID"
node "$BOSS_BUILD_TOOLBOX/finalize/route-contract.mjs" stamp --receipt "$BOSS_BUILD_ROUTE_RECEIPT" --token do-not-merge-marked --run-id "$BLI_RUNID"
```

## Step 10: Settle loop (capped)

Late reviews sometimes land minutes after ready, so this step **waits on state, never on a clock**:
run [`callback-watches.md`](callback-watches.md) Protocol step 5's bounded poll over `"$PR_NUMBER"`,
capped by `policy.settleCap`, and route `timeout`/`unknown` the way that step routes them. Never a
fixed `sleep` and never a "wait N minutes" — a duration guessed here returns before the late review
lands or spends settle allowance after it already did, and either way it is a clock standing in for a
reading. Arming is deliberately **not** what this step opens with: Step 9 gated the branch green
before Step 10 runs, so `checks_passed` already reads satisfied and re-arming a satisfied trigger
fires it immediately and burns the watch (Protocol step 4), and no `policy.watchTriggers` trigger
reports review activity at all — arm only on a later cycle whose reconcile reads that trigger false.
Then **partition late feedback by
source before acting** — the source decides whether a settle cycle is spent at all. Read
`REVIEW_VERDICT` from the run note Step 6 wrote at
`$(git rev-parse --git-dir)/boss-build-review-verdict`.

- **Bot reviews, after a clean verdict.** Identify an automated reviewer generically from the review
  author, never from a list of product names: the GraphQL author's `isBot` field, the REST author's
  `"type": "Bot"` value, or a login ending in the `[bot]` suffix. Any one signal is sufficient. With
  `REVIEW_VERDICT=clean`, such a review is advisory. It gets exactly one grouped response comment
  per bot review, posted within the bot's own threads, carrying a per-finding reason for every
  finding it raised — never a blanket dismissal, and never silence. The contract is in
  [receiving-code-review.md](receiving-code-review.md) §Source-Specific Handling. It consumes no
  `settleCap` cycle, and never re-enters Step 8. Advisory is not ignored: a bot finding that names a
  real defect is still fixed — advisory means it does not mechanically open a fix cycle, not that
  the finding is dropped. Fix it here. A fix taken on the advisory path gets the same finalize
  re-verification a Step 8 repair would: run the gates, commit, push, and re-verify finalize on the
  new head before the grouped response is posted. Spending no `settleCap` cycle is not licence to
  skip the verification — an advisory fix that is never committed and pushed merges as if it had
  never been made. The advisory path carries its own bound, so removing it from `settleCap` cannot
  make it unbounded: at most one grouped response round per PR head SHA (a round answers every bot review pending on that head, one
  grouped comment each), and at most three advisory rounds per run. Past either bound, stop treating
  further bot feedback as actionable in this run and go to **Stop cleanly**.
- **Human changes-requested reviews, and red CI.** These are unchanged: they still go back to Step 8
  (boss-repair), still re-verify finalize, and still consume a settle cycle. Read CI from
  `gh pr checks` — a PR that flips to `UNSTABLE` after being readied is not red CI. Readying the PR
  never opens a cycle by itself.
- **Every other verdict.** The shortcut is verdict-gated: only a verdict positively recorded as
  `clean` unlocks it; `capped`, `none`, or an absent record means bot feedback is triaged exactly as
  today. It routes to Step 8 and spends a settle cycle like any other late review.

If late feedback cannot be repaired after the PR was marked ready, **respond and stay
`REVIEW_READY`** — do not re-quarantine. Post a per-finding response comment saying what was
attempted and why each item is still open, keep `please-review` applied, and leave the PR **ready**
with the ticket **In Review**, published per [`review-stack.md`](review-stack.md)
§REVIEW_READY-with-findings publication.

Re-quarantine **only** when one of the four `BLOCKED` causes (Step 12) actually holds — the repair
left the branch red, or left it unpushable, or the feedback surfaced a missing required API-version
bump, or the fix it demands is one the Decide-vs-ABORT list forbids. Then, and only then: convert the
PR back to draft if supported, remove `please-review`, leave the ticket **In Progress**, post the
blocker summary, then stop with BLOCKED. Bounded either way to
`policy.settleCap` (**3**) settle cycles, after which go to **Stop
cleanly** — the repair plugin owns anything later in a fresh session.

## Step 11: Proof (capture-only, mode-aware, non-fatal, REVIEW_READY only)

<!-- Compact. Full gate detail (surface/env/time gates, headless fallbacks) is in
     proof-capture.md. -->

Only on the `REVIEW_READY` path — green, ready PR with the ticket already moved to **In Review**. Skip
entirely for `BLOCKED`, `PARTIAL`, draft, and `NO_CHANGE`. This step may **never** change the terminal
state (BLOCKED is not reachable from here) and every failure is recorded and ignored.

Classify the surface (`node scripts/proof.mjs plan`), read `recipes`, `surfaces`, and `order`, then
run browser recipes with explicit selection:

```bash
node scripts/proof.mjs run --recipe <id> --recipe <id>
```

Always select this change's browser recipes explicitly. **`proof.mjs run`'s own PR comment — its
structured deferred note — is the only proof channel.** Never hand-write skip prose or a
"proof skipped: …" one-line note. When proof cannot run (no UI surface, missing prerequisite,
pipeline bug), run the proof pipeline anyway and let it post the honest
`env-unavailable`/`pipeline-error` note. The
upload env is daemon-injected — do not source `.env`; run `node scripts/proof.mjs doctor` to see what
is missing. If it is a TUI diff, this step expects the Step 5 scenario to exist; TUI proof is
scenario-driven, not `--recipe`-selectable. The intended missing-scenario policy contributes exit 1,
but a green exit can still carry a downgraded stub; check the manifest's `clamped` array before
treating the gallery as evidence. **Read [`proof-capture.md`](proof-capture.md)** for the full
surface/doctor gates, outcome classes, and non-fatal contract. Do not run the finalize sequence here
(it already ran in Steps 8–9).

## Step 12: Stop cleanly

<!-- delete claim if present; remove bossd stop-hooks; "$LOCK" release "$BLI_RUNID" -->

Every terminal state that acquired the worktree lock (Step 1) — including the Step 2.5 `foreign` yield
— must arrive here. If this run posted a claim comment and it still exists, delete `CLAIM_COMMENT_ID`.
Decide `OUTCOME` before the following optional post-terminal extension phase; it may not change that
outcome, the exit code, any tracker or PR write, or the final
`REVIEW_READY` / `PARTIAL` / `BLOCKED` / `NO_CHANGE` line. Keep the worktree lock until the phase completes.

```bash
node "$BOSS_BUILD_TOOLBOX/finalize/route-contract.mjs" stamp --receipt "$BOSS_BUILD_ROUTE_RECEIPT" --token claim-deleted --run-id "$BLI_RUNID"
```

When a ticket has been resolved, the run must carry the tracker state captured at session entry into
Step 12. In bossd-managed runs, that value comes from the bootstrap/session payload captured before
bossd's session-start sync moves the ticket; Step 12 must never recapture the current post-sync state
and treat it as entry. In standalone/manual runs, capture the entry state before the first tracker move
this run performs. On a `NO_CHANGE` exit — including a lost claim and the Step 2.5 `foreign` yield —
restore that entry state with the tracker adapter's `moveState` capability when the ticket is not
already there. Guard the restore: skip it when the current state is not one this run or its bootstrap
produced, or when claim arbitration shows another runner still owns the ticket (for example, a `LOST`
claim with the winning claim comment still present), because a third-party owner or state change is
authoritative and must not be clobbered.
A failed restore is reported as a warning and does not change the terminal outcome.

```bash
node "$BOSS_BUILD_TOOLBOX/finalize/route-contract.mjs" stamp --receipt "$BOSS_BUILD_ROUTE_RECEIPT" --token entry-state-restored --run-id "$BLI_RUNID"
```

Every `NO_CHANGE` exit for a resolved ticket also leaves exactly one durable breadcrumb tracker
comment naming the `NO_CHANGE` branch that fired and the single fact that made it fire. This
breadcrumb is not deleted with the claim comment. The breadcrumb is idempotent: if this
run already posted one, update it rather than adding a duplicate, and make repeated `NO_CHANGE` exits
on the same ticket visible as repeats. Keep the breadcrumb secret-hygienic: a short reason plus a
file, skill, or command pointer only — never a transcript, command output, user-provided content,
credentials, or tokens. Write the breadcrumb and perform the guarded state restore here in Step 12,
before the optional post-terminal notes extensions phase, whose contract forbids tracker writes.

```bash
node "$BOSS_BUILD_TOOLBOX/finalize/route-contract.mjs" stamp --receipt "$BOSS_BUILD_ROUTE_RECEIPT" --token no-change-breadcrumb-written --run-id "$BLI_RUNID"
```

### Post-terminal notes extensions (repo opt-in)

**Caller suppression — check this before anything else.** A run another boss core dispatched as part
of its own larger run must not take its own notes: that caller already owns the single post-terminal notes dispatch for
the whole top-level run, so a nested phase here is exactly the duplicate this gate exists to remove.
A caller signals that ownership by setting
`BOSS_NOTES_SUPPRESSED=1` in the dispatched worker's environment. The marker **defaults to not
suppressed** — unset, empty, or any other value means this run owns its own notes.

A dispatched worker does not inherit that environment, so the caller also states the marker **in the
invocation**: bind it into the shell that runs the gate below (`BOSS_NOTES_SUPPRESSED=1`) before
reading it.

```bash
if [ "${BOSS_NOTES_SUPPRESSED:-}" = "1" ]; then
  echo "notes: suppressed (caller owns notes)"   # end the phase here: no discovery, no scratch, no dispatch
fi
```

Skipping is all that is due: suppression is never fatal and never changes the terminal outcome.

Resolve the extension helper, then discover the `notes` role:

```bash
BOSS_BUILD_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-build/toolbox"
if [ ! -d "$BOSS_BUILD_TOOLBOX" ]; then BOSS_BUILD_TOOLBOX="$HOME/.codex/skills/boss-build/toolbox"; fi
NOTES_JSON=$(node "$BOSS_BUILD_TOOLBOX/skill-extensions.mjs" discover --core boss-build --role notes --json)
```

Record every `NOTES_JSON.skipped` entry whose `deliberate` is `false` as
`extension <name>: skipped (<reason>)` in the ledger, before dispatching. Key that on the entry's
own `deliberate` field, never on the text of `reason`. A `deliberate: true` entry is never reported.
Recording is all that is due: a discovery skip is never fatal and never changes control flow.

If `NOTES_JSON.extensions` is empty, do nothing and print nothing: a repo without a local notes
extension has not opted in. Create no scratch in that case.

**Sampling roll — one per run, shared by every reporting phase.** `notesDefaults.sampleRate` (a
number in `[0,1]`, default `1.0`; `0.33` is the recommended production setting) is the probability
that a run reports at all. Roll it **once per run** and carry the pair forward. The Step 6.5
knowledge phase runs first and left its roll in a file inside the git directory; read that file back
here rather than rolling again. Reading it is a **consume**: the line is removed once used, and a
line older than twelve hours is ignored as a leftover rather than reused. Only when no usable line is
there — Step 6.5 never ran, or this run reached the terminal state by a route that skipped it — does
this phase take the roll itself:

```bash
NOTES_ROLL_FILE="$(git rev-parse --git-dir 2>/dev/null || echo .)/boss-build-notes-roll"
if [ -z "${NOTES_SAMPLED:-}" ] && [ -n "$(find "$NOTES_ROLL_FILE" -mmin -720 2>/dev/null)" ]; then
  read -r NOTES_SAMPLE_RATE NOTES_SAMPLED < "$NOTES_ROLL_FILE"
  rm -f "$NOTES_ROLL_FILE"
  export NOTES_SAMPLE_RATE NOTES_SAMPLED
fi
if [ -z "${NOTES_SAMPLED:-}" ]; then
  NOTES_SAMPLE_RATE=$(export BOSS_BUILD_TOOLBOX; node --input-type=module -e 'import { pathToFileURL } from "node:url"; const { loadSkillConfig, notesSampleRate } = await import(pathToFileURL(process.env.BOSS_BUILD_TOOLBOX + "/skill-config.mjs").href); process.stdout.write(String(notesSampleRate(loadSkillConfig())))')
  NOTES_SAMPLED=$(awk -v r="${NOTES_SAMPLE_RATE:-1}" -v s="$$" 'BEGIN{srand(s);print (rand()<r)?"yes":"no"}')
  export NOTES_SAMPLE_RATE NOTES_SAMPLED
fi
if [ "$NOTES_SAMPLED" != "yes" ]; then
  echo "notes: sampled out (rate ${NOTES_SAMPLE_RATE:-1})"   # end the phase here: no scratch, no dispatch
fi
```

An unreadable or malformed rate resolves to `1.0` at both ends. This gate sits **after** the
empty-`extensions` check, so a repo that never opted in still prints nothing at all.

Once both gates pass, create the terminal-only handoff:

```bash
NOTES_RUN_TMP=$(mktemp -d "${TMPDIR:-/tmp}/boss-build-notes.XXXXXX")
NOTES_OBSERVATIONS="$NOTES_RUN_TMP/observations.md"
```

Before dispatch, the orchestrator that still owns the completed run writes at most five
secret-scrubbed candidate observations to `NOTES_OBSERVATIONS`, with a maximum 8 KiB file size.
Keep each candidate to a short problem statement plus a file/skill/command pointer. Never copy a
transcript, command output, user-provided content, credentials, tokens, or other secrets; an empty
file is valid. This artifact is the only run-history source sent across the fresh-subagent boundary.

Dispatch **one combined subagent for the whole phase**, fresh and **awaited** — not one per
descriptor. A top-level run performs at most one post-terminal notes dispatch, and this is it. That
single worker walks the descriptors in ascending `(order, name)` order, loading each extension by
**reading the descriptor's `skillPath` from disk** (`dir` is its directory), passing both `skillPath`
and `dir` in the worker brief, and requiring relative extension resources to resolve from `dir`. Pass
that `SKILL.md` content into the dispatch as the extension's instructions — never by its bare
descriptor `name` via the Skill tool, which refuses a skill declaring `disable-model-invocation: true`.
The worker finishes one extension and writes its `outPath` before it loads the next one.

Bound the combined worker by `BOSS_SKILL_EXTENSION_TIMEOUT_MS` (default `300000` ms) **per
extension**: the worker's whole allowance is that value multiplied by the descriptor count, and
inside it each extension gets exactly one such share. State both numbers in the worker brief. An
extension that overruns its own share is abandoned and recorded as a skip; its siblings still run.

The worker brief carries one invocation envelope per descriptor, each unchanged:

```json
{
  "role": "notes",
  "core": "boss-build",
  "context": {
    "mode": "<interactive if this run involved operator interaction; otherwise headless>",
    "core": "boss-build",
    "outcome": "<OUTCOME>",
    "repoId": "<BOSS_REPO_ID when present; otherwise null>",
    "observationPath": "<NOTES_OBSERVATIONS>"
  },
  "runTmp": "<NOTES_RUN_TMP>",
  "outPath": "<NOTES_RUN_TMP>/notes-<extension-name>.json"
}
```

Validate each result with `node "$BOSS_BUILD_TOOLBOX/skill-extensions.mjs" validate --role notes --file
"<outPath>"`. On success append one terminal-ledger line with the total persisted-note count. On a
discovery skip, timeout, missing output, malformed envelope, validation failure, or subagent failure,
append `extension <name>: skipped (<reason>)` and continue; if the combined worker itself fails or
overruns its whole allowance, record every descriptor that produced no `outPath` as its own skip.
Remove `NOTES_RUN_TMP` on every post-opt-in terminal path. This phase is non-fatal in every case.

A **ledger write that itself fails** — this terminal-ledger line, or the findings ledger published at
Step 9 — prints `warning: ledger write failed (<reason>) — bookkeeping only, work state unaffected`
and the run continues to its terminal state. A ledger is history, never a terminal-state input.

```bash
node "$BOSS_BUILD_TOOLBOX/finalize/route-contract.mjs" stamp --receipt "$BOSS_BUILD_ROUTE_RECEIPT" --token notes-before-lock-release --run-id "$BLI_RUNID"
```

Then remove bossd Stop-hook entries so bossd does not double-finalize:

```bash
BOSS_BUILD_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-build/toolbox"
if [ ! -d "$BOSS_BUILD_TOOLBOX" ]; then BOSS_BUILD_TOOLBOX="$HOME/.codex/skills/boss-build/toolbox"; fi
node "$BOSS_BUILD_TOOLBOX/remove-bossd-stop-hooks.mjs"
node "$BOSS_BUILD_TOOLBOX/finalize/route-contract.mjs" stamp --receipt "$BOSS_BUILD_ROUTE_RECEIPT" --token stop-hooks-removed --run-id "$BLI_RUNID"
```

(A no-op under `BOSSD_MANAGED=0` — bossd installed no Stop-hooks.) Finally, release the worktree lock:

```bash
"$BOSS_BUILD_TOOLBOX/worktree-lock.sh" release "$BLI_RUNID"
node "$BOSS_BUILD_TOOLBOX/finalize/route-contract.mjs" stamp --receipt "$BOSS_BUILD_ROUTE_RECEIPT" --token lock-released --run-id "$BLI_RUNID"
```

(The startup `HELD_BY_PEER` yield is the one exit that does **not** reach Step 12 — it never owned the
lock.)

**Pick the terminal state honestly, and start from `BLOCKED`'s four causes.** `BLOCKED` is reachable
for exactly four reasons:

1. **quality gates are red**;
2. **the branch cannot be pushed**;
3. **a required API-version bump or down-convert transform is missing**, per the configured
   API-compatibility lens role;
4. **the plan demands something unsafe** (the Decide-vs-ABORT list).

**That list is exhaustive.** Nothing else finalizes `BLOCKED` — and **open review findings are not on
it**. A round-capped review, a verdict nobody could read, a review that never ran, a surviving
provisional seed, an uncertified criterion, unrepaired late feedback: none of them is a cause here.
Each one ships `REVIEW_READY` with what it left open **published** — the findings ledger on the PR
and the ticket, `please-review` applied, the PR readied, and the real `## Review coverage` /
`## Cross-model review` tokens in the body — per [`review-stack.md`](review-stack.md)
§REVIEW_READY-with-findings publication. If you are reaching for `BLOCKED` and cannot name which of
the four numbered causes above you are in, you are in none of them.

`PARTIAL` is the one scoped narrowing of `REVIEW_READY`, not a softer `BLOCKED`: pick it when the
Step 9 gate's T1/T2/T3 all held — every deferred required item an unsatisfied in-scope acceptance
criterion, the branch green, at least one criterion certified. On a run that reached `PARTIAL` from
the review loop's `capped` arm, Step 9 never executed, so read the same three conjuncts from
[`review-stack.md`](review-stack.md) §PARTIAL-route publication, which re-checks them on that route:
unevaluated is not held. A conjunct you cannot establish means `PARTIAL` is the wrong label — it
falls back to `REVIEW_READY` with the items published, and to `BLOCKED` only through cause (1) or
(2), a red or unpushable branch.

### Bookkeeping is advisory

Bookkeeping is every surface that **records what a run did**: the route receipt and its stamps, the
run-note and findings ledgers, and the skills-drift gate. None of it is work state, so **none of it
is one of the four causes above**, and a failure in any of it emits a `warning:` line and the run
continues to the terminal state its work earned. The wording is uniform so it is greppable in a
transcript — `warning: <what failed> — bookkeeping only, work state unaffected` — which is how an
advisory failure stays auditable without becoming terminal.

The split that decides which side a preflight check falls on is **recording versus capability**:

- **Recording → warn.** Installed skills that _drift_ from the checkout source still run; the drift
  report only says which payload this run read. Same for a receipt that is missing tokens and a
  ledger write that fails: history, not work state.
- **Absent capability → block.** `BLOCKED: installed boss skills not found`, a missing toolbox
  directory, and a missing helper script are not accounting discrepancies — with no toolbox there is
  nothing downstream to execute at all. Those stay hard stops.

The same rule governs the transport preflight's capability report, which is why it prints
`cli-only mode (expected):` for the three capabilities that have no CLI equivalent and never will,
and reserves `degraded:` for a capability missing from **both** transports. A permanent condition
reported as a fault on every single run is noise, and noise is what taught readers to skim the one
line that should have meant something.

Record the route contract immediately before printing. **The receipt is bookkeeping — it records
the route, it never picks the terminal state.** A satisfied receipt may still downgrade a
non-`BLOCKED` outcome to `BLOCKED`, because that downgrade is the receipt agreeing with obligations
the run actually stamped. An unsatisfied one may not do the reverse: when the helper exits non-zero
it returns the non-terminal `ROUTE_UNSATISFIED` on stdout and a `{status, missing, unknown, error}`
detail on stderr, and the run **keeps the outcome it derived from the work** and prints one
`warning:` line:

```bash
# Keep the two streams APART. stdout carries only the honest outcome line; stderr carries the JSON
# detail plus node's own load-time noise, written BEFORE the outcome. Never merge them with 2>&1.
RC_ERR="$(mktemp -t boss-build-route-err.XXXXXX)"
RC_OUT="$(node "$BOSS_BUILD_TOOLBOX/finalize/route-contract.mjs" assert --outcome "$OUTCOME" --receipt "$BOSS_BUILD_ROUTE_RECEIPT" --run-id "$BLI_RUNID" 2>"$RC_ERR")" && RC_OK=yes || RC_OK=no
RC_VERDICT="$(printf '%s\n' "$RC_OUT" | head -n 1)"
case "$RC_VERDICT" in
  REVIEW_READY | PARTIAL | BLOCKED | NO_CHANGE | ROUTE_UNSATISFIED) ;;
  # No verdict line at all: the helper is absent, or was called wrong (its usage() exit 2 writes
  # nothing to stdout). That is an ABSENT CAPABILITY, not an accounting gap — it stays a hard stop.
  *)
    cat "$RC_ERR" >&2
    rm -f "$RC_ERR"
    echo "BLOCKED: route-contract helper unusable; no verdict on stdout" >&2
    exit 1
    ;;
esac
if [ "$RC_OK" = yes ]; then
  OUTCOME="$RC_VERDICT"   # satisfied: the legitimate BLOCKED downgrade
else
  RC_DETAIL="$(tr '\n' ' ' <"$RC_ERR")"
  echo "warning: route receipt incomplete (${RC_DETAIL:-no detail}) — bookkeeping only, work state unaffected" >&2
fi
rm -f "$RC_ERR"
```

**Never finalize `BLOCKED` because a receipt was incomplete.** Pick the terminal state from the
**work state** — is the branch pushed, are the quality gates green, which acceptance criteria are met
— exactly as if the receipt had been perfect. The same rule governs every other bookkeeping surface
this run touched: a failed run-notes or findings **ledger** write, and installed-skills **drift**,
each warn and continue.

That advisory rule covers the receipt **as a record**, not the side effects its stamps attest to.
`TERMINAL_ROUTES` owes `claim-deleted`, `stop-hooks-removed` and `lock-released` on every route, each
stamped straight after a real mutation of _shared_ state. A missing cleanup stamp still obliges this
run to perform (or re-attempt) the cleanup it names before printing; what the missing stamp alone may
not do is change the terminal state.

Output the terminal state (`REVIEW_READY` / `PARTIAL` / `BLOCKED` / `NO_CHANGE`) as the first token
on its own line, then the ticket id, PR URL, blocker or partial summary. Run-cost extraction matches
a **line-leading** token, so a state printed mid-sentence (`Terminal state: REVIEW_READY`) is read as
unrecorded and the run reports no terminal state at all.
