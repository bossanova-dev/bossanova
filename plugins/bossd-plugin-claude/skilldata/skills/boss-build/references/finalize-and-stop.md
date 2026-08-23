# Finalize and stop — Steps 8-12

Read this when the run first routes to Steps 8–12: normally when Step 7 has opened (or reused) the
PR, or earlier when a terminal path such as Step 2.5 `foreign` must run Step 12. The normal PR tail
contains tag injection and the capped green gate (Step 8), idempotent finalize + Linear writeback
(Step 9), the capped settle loop (Step 10), mode-aware proof capture (Step 11), and the clean stop
that releases the worktree lock and picks the terminal state (Step 12). On a pre-PR terminal route,
run Step 12 only. The body (`SKILL.md`) carries the resident skeleton and each step's trigger; the
instructions themselves are here.

## Step 8: Tag commits, then repair to green (boss-repair, capped)

**Inject the PR-number tag and force-push _before_ the green gate**, so CI runs once on the tagged
head instead of a second time after a post-green rewrite. This is the finalize adapter's
**inject-PR-tag** capability (`toolbox/finalize/cli.mjs inject-pr-tag`, which delegates to the
dependency-free `boss-finalize` helper at `~/.claude/skills/boss-finalize/`, reachable in a
cron worktree) — the same self-owned finalize the cron siblings use. **Tag-only, no squash** —
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
# Run with a 600s tool timeout: this replays hooks once per commit, and multi-commit
# branches can exceed the default. Redirect output to a file, never to head/tail; a
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
conflicts, and review comments — the green gate now runs on the already-tagged head. Cap at
`policy.repairCap` (**5**) passes. If still red after the cap (or the wall-clock breaker trips): keep
the work as a **draft** PR, leave the ticket **In Progress**, post a blocker comment (failing check
name, `file:line`, what was attempted), then go to **Stop cleanly** with BLOCKED.

Before blocking on this green gate, arm the one-shot callback watches for the tagged head so the run
wakes the moment CI resolves or the PR merges/closes — `resolveCallbackAdapter(env)` `registerWatch`
(`boss callback add "$PR_NUMBER" <trigger> --group ...`) for each `policy.watchTriggers`, with each
non-exclusive trigger under its own group. On every
wake **reconcile against real state before acting** (`gh pr checks`/`gh pr view`), re-arm while still
waiting, dedup by callback id, and back it with the bounded `gh pr checks --watch --fail-fast`
fallback (used directly when callbacks are unavailable). Full protocol:
[`callback-watches.md`](callback-watches.md).

## Step 9: Finalize (idempotent tag guard, ready), Linear writeback

Tag injection + force-push already ran at the top of Step 8, so CI has been gated green on the tagged
head. Step 9 is an **idempotent** guard: re-inject **only** if `boss-repair` added untagged
fix-commits, then ready the PR. In the common path there is **no rewrite, no push, no second full CI
wait** — `gh pr ready` triggers no gating `test-*.yml` workflow (they fire `on: push`, not
`ready_for_review`).

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
# not subject text, so a changed placeholder string cannot break this guard.
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
  gh pr checks "$PR_NUMBER" --watch --fail-fast            # red → route back to Step 8 (boss-repair)
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
  gh pr checks "$PR_NUMBER" --watch --fail-fast
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

> If the re-inject branch's `gh pr checks --watch --fail-fast` goes red, route back to **Step 8
> (boss-repair)**; never move the ticket to **In Review** with non-green checks. This wait may also be
> driven by the one-shot callback watches ([`callback-watches.md`](callback-watches.md)):
> re-arm after the force-push, reconcile real check state on wake, and treat `--watch --fail-fast` as
> the bounded fallback. Remove the live watches once the PR is readied.
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
is not complete.** If any required item — an
unsatisfied in-scope criterion, an open must-fix, or a missing API-version transform — was deferred,
finalize BLOCKED (Step 12) naming it; do not ready. After the PR is ready, add `please-review` if missing. Then move
the ticket from in-progress to in-review (`.inProgress → .inReview`) via the adapter's `moveState` capability, and comment the PR URL
(the adapter's `writeComment` capability).

### The verify-only evidence gate

A criterion whose correct outcome is _"this file needed no change"_ produces no diff, so the rule
above cannot be applied to it at all: the honest builder is left with ticking it on faith (silence)
or leaving it open, and leaving it open is itself a required-deferred ⇒ BLOCKED. Both failure modes
come out of the same sentence. A plan marks such a criterion with the literal `(verify-only)` prefix
and a `— check:` command; the run discharges it by **recording the check it actually ran**:

```
- [x] (verify-only) <the invariant claimed> — checked: `<command>` → <result>
```

**Backtick the command.** The `— checked:` clause must wrap its command in backticks, exactly as the
template above shows. This is not style: without a delimiter an arrow _inside_ the command
(`rg "a→b"`) is indistinguishable from the `→` that introduces the result, so the parser cannot
tell a command-with-no-result from a command-and-result. It refuses to guess — an undelimited
command reads as **no command at all** and the gate blocks naming the criterion.

Before readying, run `validateVerifyOnlyEvidence(config, body)` from `toolbox/skill-config.mjs` over
the **PR body** — an executed check, not prose faith, the same shape as the `validatePlanDescription`
call at Step 4. It returns `{ ok, verifyOnly, missingEvidence }`, and `ok` is true only when every
criterion that is both marked and ticked carries a **non-empty** command **and** a **non-empty**
result. An `ok:false` result makes each criterion it names a **deferred required item**: finalize
BLOCKED (Step 12) naming each one, and do not ready. An **unticked** marked criterion is not a
failure of this gate — it is already an open in-scope criterion under the rule above, and this gate
does not double-report what that rule owns.

The gate checks **structure and non-emptiness, never truth**. It cannot tell whether the recorded
command was really run. What it buys is that the check becomes named and re-runnable by a human
reviewer, so a verify-only criterion can no longer be discharged in silence.

It is also **not** a completeness check, and must not be read as one: it inspects only the criteria
the body actually carries, so a criterion **omitted** from the body entirely is invisible to it and
would pass. That case is owned by the rule above — every in-scope criterion must be present and
ticked — and the two compose: the completeness rule establishes that each criterion is _there_, and
this gate establishes that each marked-and-ticked one is _evidenced_. Run both; neither substitutes
for the other.

### The premise discharge gate

Before reporting `REVIEW_READY`, restate the plan's central premise from `## Premises` and the
evidence that it still holds on this branch. If the central premise no longer holds, terminate
BLOCKED with a written refutation even when every acceptance criterion is satisfied; a green
checklist is not enough when the ticket's stated reason for the change is false. When merged work
inverted the premise and the run took the documented-departure route, the PR body and tracker
comment must name the criterion verbatim and the merged change that inverted it before the PR is
readied.

**Reclassification — the route out of an automatic BLOCK.** A plan written before this contract, or
one whose drafter missed a verify-only criterion, still lands an in-scope criterion the diff cannot
demonstrate. When the correct outcome of such a criterion genuinely is "no change was needed", you
may discharge it as verify-only **by writing the discharge form yourself**: run a real check, record
its command — **in backticks**, per the rule above — and its result in the PR body's
`## Acceptance criteria` block, and note the reclassification under `## Autonomous decisions`. It is then subject to this same gate. This is
deliberately **not** a way to skip work: it demands more than a silent tick (a named command, a
recorded result, and a recorded decision), and it does nothing for a criterion that genuinely needs
a code change — an unmarked criterion with no evidence line stays required-deferred exactly as today.

**The one scoped exception — the `PARTIAL` gate.** Before routing a deferral to BLOCKED, classify the
deferred required items. Take the `PARTIAL` route instead of BLOCKED when **all three** hold:

- **T1** — at least one in-scope acceptance criterion is satisfied **and independently certified**:
  certification is a **positive** record, never an absence. Both halves are required — boss-review's
  always-on acceptance-criteria certification **ran** over the full supplied criteria list and
  returned a verdict a **reviewer** authored, **and** it emitted no `lens: acceptance-criteria`
  must-fix naming that criterion. The second half alone certifies vacuously on a branch no lens ever
  read, so a `capped` verdict a run generated for itself fails T1 by construction. An agent's own
  assertion that a criterion is done is never certification, and a run that satisfied **zero**
  criteria is BLOCKED, never `PARTIAL`.
- **T2** — the branch is **green** at this step's green gate, after boss-repair capped at
  `policy.repairCap`. A run that arrives at `PARTIAL` from the review loop's `capped` arm never
  reached this step, so its greenness is not inherited from here: that route takes the same reading
  itself, against the PR it publishes, per [`review-stack.md`](review-stack.md) §PARTIAL-route
  publication. Unmeasured is not green.
- **T3** — every deferred required item is **only** an unsatisfied in-scope acceptance criterion:
  zero open must-fix findings from any lens other than `acceptance-criteria`; no missing
  `bossanova.v1` API-version bump or down-convert transform; no unattributed residue or untagged
  commits; the reviewed-tip confirmation held; no hard-ABORT condition fired.

Anything else is BLOCKED, unchanged. On the `PARTIAL` route do **not** run the **review-move** half of
the block above: **never** move the ticket to `.inReview`, **never** add `please-review`, and leave
the ticket in the `.inProgress` role. The **ready** half still runs — the PR **is** made non-draft,
because a draft PR's CI is expected to be noisy or partial, so leaving it a draft would make T2's
"branch green" unverifiable on the very artifact it gates — but it carries the do-not-merge marker
and no `please-review` label. Comment the
PR URL on the ticket alongside the partial enumeration (the adapter's `writeComment` capability). The
PR-body and ticket-comment authoring belongs to
[`review-stack.md`](review-stack.md) §PARTIAL-route publication; do not improvise either here.

## Step 10: Settle loop (capped)

Late reviews sometimes land minutes after ready. Wait 5 minutes; if new review feedback appears, go
back to Step 8 (boss-repair), then re-verify finalize. If late feedback cannot be repaired after the
PR was marked ready, re-quarantine: convert the PR back to draft if supported, remove `please-review`,
leave the ticket **In Progress**, post the blocker summary, then stop with BLOCKED. Bounded to
`policy.settleCap` (**3**) settle cycles (or until the breaker trips), after which go to **Stop
cleanly** — the repair plugin owns anything later in a fresh session.

## Step 11: Proof (capture-only, mode-aware, non-fatal, REVIEW_READY only)

<!-- Compact. Full gate detail (surface/env/time gates, headless fallbacks) is in
     proof-capture.md. -->

Only on the `REVIEW_READY` path — green, ready PR with the ticket already moved to **In Review**. Skip
entirely for `BLOCKED`, `PARTIAL`, draft, and `NO_CHANGE` — proof of an incomplete slice is
misleading evidence. This step may **never** change the terminal state
(BLOCKED is not reachable from here) and every failure is recorded and ignored.

Classify the surface (`node scripts/proof.mjs plan`), read `recipes`, `surfaces`, and `order`, then
run browser recipes with explicit selection:

```bash
node scripts/proof.mjs run --recipe <id> --recipe <id>
```

Always select this change's browser recipes explicitly: a bare `run` executes the default preset, and
an unrelated recipe failure still fails the aggregate process. **`proof.mjs run`'s own PR comment —
its structured deferred note — is the only proof channel.** Never hand-write skip prose or a
"proof skipped: …" one-line note. When proof cannot run (no UI surface, missing prerequisite,
pipeline bug), run the proof pipeline anyway and let it post the honest
`env-unavailable`/`pipeline-error` note (doctor output is embedded so a human can fix the env). The
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

Every `NO_CHANGE` exit for a resolved ticket also leaves exactly one durable breadcrumb tracker
comment naming the `NO_CHANGE` branch that fired and the single fact that made it fire. This
breadcrumb is not deleted with the claim comment; deleting the transient claim and preserving the
diagnostic breadcrumb are separate acts with opposite intent. The breadcrumb is idempotent: if this
run already posted one, update it rather than adding a duplicate, and make repeated `NO_CHANGE` exits
on the same ticket visible as repeats. Keep the breadcrumb secret-hygienic: a short reason plus a
file, skill, or command pointer only — never a transcript, command output, user-provided content,
credentials, or tokens. Write the breadcrumb and perform the guarded state restore here in Step 12,
before the optional post-terminal notes extensions phase, whose contract forbids tracker writes.

### Post-terminal notes extensions (repo opt-in)

Resolve the extension helper, then discover the `notes` role:

```bash
BOSS_BUILD_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-build/toolbox"
if [ ! -d "$BOSS_BUILD_TOOLBOX" ]; then BOSS_BUILD_TOOLBOX="$HOME/.codex/skills/boss-build/toolbox"; fi
NOTES_JSON=$(node "$BOSS_BUILD_TOOLBOX/skill-extensions.mjs" discover --core boss-build --role notes --json)
```

Record every `NOTES_JSON.skipped` entry whose `deliberate` is `false` as
`extension <name>: skipped (<reason>)` in the ledger, before dispatching. Key that on the entry's
own `deliberate` field, never on the text of `reason`. A `deliberate: true` entry is a same-prefix
skill that is not an extension of this core — a markerless helper, or one extending another core —
and is never reported. Recording is all that is due: a discovery skip is never fatal and never
changes control flow; the phase still degrades exactly as documented below.

If `NOTES_JSON.extensions` is empty, do nothing and print nothing: a repo without a local notes
extension has not opted in. Create no scratch in that case. Otherwise create a terminal-only handoff:

```bash
NOTES_RUN_TMP=$(mktemp -d "${TMPDIR:-/tmp}/boss-build-notes.XXXXXX")
NOTES_OBSERVATIONS="$NOTES_RUN_TMP/observations.md"
```

Before dispatch, the orchestrator that still owns the completed run writes at most five
secret-scrubbed candidate observations to `NOTES_OBSERVATIONS`, with a maximum 8 KiB file size.
Keep each candidate to a short problem statement plus a file/skill/command pointer. Never copy a
transcript, command output, user-provided content, credentials, tokens, or other secrets; an empty
file is valid. This artifact is the only run-history source sent across the fresh-subagent boundary.

Dispatch descriptors in ascending `(order, name)` order as fresh, **awaited** subagents. Bound each by
`BOSS_SKILL_EXTENSION_TIMEOUT_MS` (default `300000` ms). Load each extension by **reading the
descriptor's `skillPath` from disk** (`dir` is its directory), passing both `skillPath` and `dir` in
the worker brief, and requiring relative extension resources to resolve from `dir`. Pass that `SKILL.md`
content into the dispatch as the extension's instructions — never by its bare descriptor `name` via the
Skill tool, which refuses a skill declaring `disable-model-invocation: true`.
Each receives:

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
append `extension <name>: skipped (<reason>)` and continue. Remove `NOTES_RUN_TMP` on every
post-opt-in terminal path. This phase is non-fatal in every case.
Then remove bossd Stop-hook entries so bossd does not double-finalize:

```bash
BOSS_BUILD_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-build/toolbox"
if [ ! -d "$BOSS_BUILD_TOOLBOX" ]; then BOSS_BUILD_TOOLBOX="$HOME/.codex/skills/boss-build/toolbox"; fi
node "$BOSS_BUILD_TOOLBOX/remove-bossd-stop-hooks.mjs"
```

(A no-op under `BOSSD_MANAGED=0` — bossd installed no Stop-hooks.) Finally, release the worktree lock:

```bash
"$BOSS_BUILD_TOOLBOX/worktree-lock.sh" release "$BLI_RUNID"
```

(The startup `HELD_BY_PEER` yield is the one exit that does **not** reach Step 12 — it never owned the
lock.)

Pick the terminal state honestly — **REVIEW_READY only with no deferred required item** (Hard rules);
else BLOCKED. `PARTIAL` is the single scoped exception to that rule: pick it, never BLOCKED, when the
Step 9 gate's T1/T2/T3 all held — every deferred required item an unsatisfied in-scope acceptance
criterion, the branch green, at least one criterion certified. On a run that reached `PARTIAL` from
the review loop's `capped` arm, Step 9 never executed, so read the same three conjuncts from
[`review-stack.md`](review-stack.md) §PARTIAL-route publication, which re-checks them on that route:
unevaluated is not held, and a conjunct you cannot establish is BLOCKED. Any other deferred required
class is BLOCKED. Output the terminal state (`REVIEW_READY` / `PARTIAL` / `BLOCKED` / `NO_CHANGE`) with the
ticket id, PR URL, (for BLOCKED) the blocker summary naming the item, and (for `PARTIAL`) the
`<satisfied>/<total>` acceptance-criteria count plus every open criterion by name.
