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
BASE_BRANCH="$BASE_BRANCH" node "$BOSS_BUILD_TOOLBOX/finalize/cli.mjs" inject-pr-tag "$PR_NUMBER"
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

Before blocking on this green gate, arm the one-shot callback **group** for the tagged head so the run
wakes the moment CI resolves or the PR merges/closes — `resolveCallbackAdapter(env)` `registerWatch`
(`boss callback add "$PR_NUMBER" <trigger> --group ...`) for each `policy.watchTriggers`. On every
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
# Re-inject only if boss-repair added tagless commits; else no rewrite, no push, no second CI wait.
if git log "origin/$BASE_BRANCH"..HEAD --oneline | grep -qv "\[#$PR_NUMBER\]"; then
  BASE_BRANCH="$BASE_BRANCH" node "$BOSS_BUILD_TOOLBOX/finalize/cli.mjs" inject-pr-tag "$PR_NUMBER"
  git push --force-with-lease origin "$SESSION_BRANCH"
test "$(git rev-parse HEAD)" = "$(git rev-parse @{u})" || exit 1  # HEAD == upstream (lease rejected → re-fetch, re-run)
  gh pr checks "$PR_NUMBER" --watch --fail-fast            # red → route back to Step 8 (boss-repair)
fi
# Ready the PR — the finalize adapter's readyPr capability (isDraft==true guard; command: gh pr ready).
if [ "$(gh pr view "$PR_NUMBER" --json isDraft -q .isDraft)" = "true" ]; then gh pr ready "$PR_NUMBER"; fi
test "$(gh pr view "$PR_NUMBER" --json isDraft -q .isDraft)" = "false" || exit 1
```

> If the re-inject branch's `gh pr checks --watch --fail-fast` goes red, route back to **Step 8
> (boss-repair)**; never move the ticket to **In Review** with non-green checks. This wait may also be
> driven by the one-shot callback group ([`callback-watches.md`](callback-watches.md)):
> re-arm after the force-push, reconcile real check state on wake, and treat `--watch --fail-fast` as
> the bounded fallback. Remove the group's live watches once the PR is readied.

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

Classify the surface (`node scripts/proof.mjs plan`), then run `node scripts/proof.mjs run`.
**`proof.mjs run`'s own PR comment — its structured deferred note — is the only proof channel.** Never
hand-write skip prose or a "proof skipped: …" one-line note. When proof cannot run (no UI surface,
missing prerequisite, pipeline bug), run `node scripts/proof.mjs run` anyway and let it post the honest
`env-unavailable`/`pipeline-error` note (doctor output is embedded so a human can fix the env). The
upload env is daemon-injected — do not source `.env`; run `node scripts/proof.mjs doctor` to see what
is missing. A TUI diff lacking the scenario authored in Step 5 earns a `scenario-missing`
note (exit 1 — proof is required for TUI). **Read [`proof-capture.md`](proof-capture.md)** for the full
surface/doctor gates, outcome classes, and non-fatal contract. Do not run the finalize sequence here
(it already ran in Steps 8–9).

## Step 12: Stop cleanly

<!-- delete claim if present; remove bossd stop-hooks; "$LOCK" release "$BLI_RUNID" -->

Every terminal state that acquired the worktree lock (Step 1) — including the Step 2.5 `foreign` yield
— must arrive here. If this run posted a claim comment and it still exists, delete `CLAIM_COMMENT_ID`.
Decide `OUTCOME` before the following optional post-terminal extension phase; it may not change that
outcome, the exit code, any tracker or PR write, or the final
`REVIEW_READY` / `PARTIAL` / `BLOCKED` / `NO_CHANGE` line. Keep the worktree lock until the phase completes.

### Post-terminal notes extensions (repo opt-in)

Resolve the extension helper, then discover the `notes` role:

```bash
BOSS_BUILD_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-build/toolbox"
if [ ! -d "$BOSS_BUILD_TOOLBOX" ]; then BOSS_BUILD_TOOLBOX="$HOME/.codex/skills/boss-build/toolbox"; fi
NOTES_JSON=$(node "$BOSS_BUILD_TOOLBOX/skill-extensions.mjs" discover --core boss-build --role notes --json)
```

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
