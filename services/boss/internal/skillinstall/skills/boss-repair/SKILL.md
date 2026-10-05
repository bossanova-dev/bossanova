---
name: boss-repair
description: Automated PR repair — fixes conflicts, failing checks, and review feedback
---

# PR Repair

Get a PR back to green: resolve conflicts, fix failing checks, and answer review feedback. You are a
capable engineer; this document gives you the goal, the hazards that are genuinely dangerous, and the
helpers that answer questions reliably. How you investigate and fix is up to you.

## Modes

- **Default** (`/boss-repair`, how the repair plugin invokes it): one repair pass — fix, push, poll
  the PR once, report, **exit zero**. The plugin owns retries, backoff and attempt caps, and only
  resumes its in-session loop after a clean exit. Exit zero even when checks are still pending or red.
- **Watch** (`/boss-repair watch`, manual or dispatched by another skill): loop until the PR is green,
  bounded to 5 repair passes, then print the terminal line described in [Watch mode](#watch-mode).

## Helpers

Every helper ships in this skill's `toolbox/` (and the review probe in `scripts/`). Shell state does
not survive between tool calls, so resolve the paths in each block that uses them:

```bash
BOSS_REPAIR_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/toolbox"
if [ ! -d "$BOSS_REPAIR_TOOLBOX" ]; then BOSS_REPAIR_TOOLBOX="$HOME/.codex/skills/boss-repair/toolbox"; fi
BOSS_REPAIR_PROBE="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/scripts/review-feedback-probe.js"
if [ ! -f "$BOSS_REPAIR_PROBE" ]; then BOSS_REPAIR_PROBE="$HOME/.codex/skills/boss-repair/scripts/review-feedback-probe.js"; fi
```

| Question                                         | Ask                                                         |
| ------------------------------------------------ | ----------------------------------------------------------- |
| Is the worktree clean?                           | `node "$BOSS_REPAIR_TOOLBOX/worktree-state.mjs"`            |
| Are the checks green, failing, pending, unknown? | `node "$BOSS_REPAIR_TOOLBOX/pr-check-state.mjs" classify …` |
| What review feedback is open?                    | `node "$BOSS_REPAIR_PROBE"`                                 |
| Wait for CI without guessing a sleep             | `node "$BOSS_REPAIR_TOOLBOX/ci-wait.mjs" run --pr <n>`      |

`worktree-state.mjs` exists because a command-rewriting shell hook can make `git status` print a
fabricated clean result. Its `unknown` verdict is never clean; confirm with `/usr/bin/git status
--porcelain` before treating the tree as clean.

## Hard rules

These protect the branch, other people's work, and the truthfulness of the result. Everything else is
judgement.

1. **Rebase, never merge the base in.** Sync with `git fetch origin <base>` then
   `git rebase origin/<base>` — never `git merge` the base and never `git pull`. A merge commit on the
   branch makes GitHub's rebase-merge refuse the PR. Before any push that follows a base sync:

   ```bash
   MERGE_COUNT=$(git rev-list --merges --count "origin/$BASE_BRANCH"..HEAD) || exit 1
   test "$MERGE_COUNT" = 0 || { echo "merge commits on this branch; linearize before pushing"; exit 1; }
   ```

   If merge commits already exist, check whether any carries manual conflict resolution
   (`git show --remerge-diff <merge>` prints something); carry those edits into a normal commit
   first, then rebase to flatten. Never pass `--rebase-merges`.

2. **Never destroy work you did not author.** The worktree may be shared with another agent. Never
   `git stash`, `git reset --hard`, or `git checkout --` someone else's uncommitted changes. If the
   tree has uncommitted work when you arrive, read it: if it already fixes what you are repairing,
   validate and commit **that** (say so in the summary); otherwise leave it exactly where it is and
   report it.

3. **Force-push only with a lease, and only over your own rebase.** After a rebase, push with
   `git push --force-with-lease`. Never force-push over commits someone else pushed.

4. **Don't push a CI fix onto a head that moved.** Note the PR head SHA when you start
   (`gh pr view --json headRefOid -q .headRefOid`). Just before pushing a fix for a check failure,
   re-read it; if someone else moved the head, that CI result is stale — keep your commit, don't push
   it, and report it as built but unpushed. Your own pushes don't count: re-baseline after each one.
   Review-feedback fixes don't depend on CI, so they are not affected.

5. **Confirm your commits actually landed.** After pushing, `git fetch origin <branch>` and check
   that every commit you pushed is an ancestor of `origin/<branch>`
   (`git merge-base --is-ancestor <sha> origin/<branch>`). `Everything up-to-date` is not proof — it
   is also what you see when someone else moved your local HEAD. If local and remote have diverged,
   do not push and do not force-push; report it.

6. **Unobserved is never green.** Read checks only through `pr-check-state.mjs classify`: `green`
   is the only pass, and `pending` (including reason `absent-gate`, a gate the previous head had that
   this head lacks — waiting will not bring it back), `failing` and `unknown` are not. A mergeability
   of `UNKNOWN`/`null` is unobserved, not "no conflict". A review probe status other than
   `repair_status=clean` (or `parked`) is not clean.

7. **A failure that is already on the base is not this PR's to fix.** Before repairing a red check,
   read its output and confirm the cause is something this branch changed. Lock contention, a commit
   hook's signing or memory failure, and a failure in an untouched file are infrastructure; a cause
   reproducible on the base (prefer the base's own CI result; otherwise reproduce in a throwaway
   worktree, never by checking out the base here) is inherited. Report both as residuals.

## A repair pass

1. **Assess.** Record the PR head SHA. Read the PR, its checks (via the classifier), its open review
   feedback (via the probe), mergeability, and the worktree state. Decide what is wrong: conflict,
   failing checks, review feedback, a must-fix finding recorded only in the PR body or the
   dispatching core's review report, or nothing. A probe answer you could not read counts as "there
   may be something" — never report "nothing to repair" without having looked.
2. **Repair**, review feedback first (its content does not go stale), then conflicts, then failing
   checks (re-check the head per rule 4 before acting on CI). See the notes below for each.
3. **Verify locally.** Run the repo's formatter and the tests relevant to the change
   (`commands.testAffected` when the repo has one, otherwise the tests covering what you changed);
   CI re-runs the full suite after the push. Quote each gate's own final summary line in
   the report; a result you did not run is reported as not run, never as a pass. Take a verdict from
   the command's own exit status, not through a pipe (a pipeline reports its tail's status).
4. **Commit and push** small, descriptive commits. Follow the repo's commit conventions.
5. **Poll once** (default mode) and report.

You may run a strategy in a dispatched subagent to keep diffs and CI logs out of your own context —
await it (this core's `toolbox/bs-dispatch-await.mjs` is the completion oracle), never fire and
forget. A dispatched worker sees only its brief, so the brief must state rule 7: a cause already on
the base is a residual, reported and not fixed. If dispatch is unavailable or fails, do the work
inline.

### Conflicts

Rebase onto the base (rule 1). For each conflicted file, understand both sides — during a rebase
`ours` is the base and `theirs` is the replayed PR commit. Regenerate generated files from their
sources rather than hand-editing them; keep both sides of independent additions to append-only
registries; for a disputed measurement or count, re-measure after the rebase rather than picking a
side. Continue with `GIT_EDITOR=true git rebase --continue`; skip a replayed commit that became
genuinely empty. After the rebase completes, re-run the relevant tests and grep for any call shape
this branch refactored — including in files the base added.
Push with `--force-with-lease`. If a conflict is too tangled to settle safely, leave a PR comment
naming the files and report it as a residual.

### Failing checks

Group failures by root cause from their output, not by check name, and report the
`cause → checks it explains` table. Fix the cause, not the symptom. Read logs selectively
(`gh run view <id> --log-failed`) rather than pasting whole logs into your context.

### Review feedback

`node "$BOSS_REPAIR_PROBE"` lists open review threads and inline comments (`gh pr view --comments`
misses inline review comments). Its contract:

- `probe_status=ok` ⇒ trust `repair_status`: `clean` (nothing open), `needs_repair` (handle every
  printed thread), `parked` (every open thread waits on a human), `unknown` (not clean).
- `probe_status=degraded` ⇒ GraphQL quota is exhausted and the probe fell back to REST, which cannot
  see resolution state; act on the printed comments, never read it as clean.
- `probe_status=failed`, `suspicious_zero`, an unknown contract version, or no `probe_status` line ⇒
  not evaluated. `probe_failure_class` decides what that means: `auth`, `not_found`, `environment`
  are true stops; `rate_limited` (retry after `probe_retry_after`) and `other` are residuals.

Handle **every** open thread. For each, check whether its factual claims hold against the current
tree (claim by claim — a finding can be partly right), then:

- **Fix** — mark it dispatched (`node "$BOSS_REPAIR_PROBE" mark --thread <id> --disposition
dispatched --repo <o/r> --pr <n> --host <host>`), make the change, reply saying what changed, and
  resolve the thread.
- **Decline** (the premise is false, stale, by design, or already satisfied) — reply with the reason;
  "already fixed" must cite the file and line that satisfies it, not a commit. If prose in the repo
  asserted the false premise, correct that prose too, or the same finding comes back. Resolve.
- **Agree, but don't apply the suggested change** (out of scope, wrong layer, or feature-sized) —
  record a follow-up first (`node "$BOSS_REPAIR_TOOLBOX/bs-repair-derivations.mjs" residual-sink`
  prints where), then reply affirming the defect and citing that record, and resolve. Never promise
  a follow-up you have not recorded.
- **Unclear** — ask a clarifying question, leave the thread open, and mark it
  `--disposition needs-human` so later passes park it until the reviewer replies.

Write reply bodies with your file tool and post them by file so Markdown is never shell-interpreted:

```bash
gh api "repos/OWNER/REPO/pulls/PR_NUM/comments/COMMENT_ID/replies" -F body=@"$REPLY_FILE" -q .html_url
gh api graphql -f query='mutation { resolveReviewThread(input: {threadId: "THREAD_ID"}) { thread { isResolved } } }'
```

If the resolve fails on GraphQL quota, do not re-post the reply: record it with
`node "$BOSS_REPAIR_PROBE" defer --thread <id> --reply <reply-url> …` and report it as a residual; a
later pass drains it with `node "$BOSS_REPAIR_PROBE" drain …`.

A parked verdict is keyed on the reviewer's last comment, not the branch, so re-check a parked
thread's premise against the current tree before reporting it; if the branch now satisfies it, reply
citing the file and line, resolve it, and clear the park (`--disposition open`).

**Bot reviews after a clean build review.** Identify an automated reviewer from the author (GraphQL
`isBot`, REST `"type": "Bot"`, or a `[bot]` login suffix). If the originating build run recorded
`REVIEW_VERDICT=clean` in `$(git rev-parse --git-dir)/boss-build-review-verdict`, bot findings are
advisory: answer each bot review once with a reason per finding, fix any that name a real defect,
resolve what the answer settles, and don't open a repair cycle over a diff that review already
passed. Human feedback and red CI are handled as usual.

## Reading PR state

Poll checks, review feedback and mergeability together — never wait on one while ignoring the
others. Check state goes through the classifier, keyed to the head SHA:

```bash
BOSS_REPAIR_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/toolbox"
if [ ! -d "$BOSS_REPAIR_TOOLBOX" ]; then BOSS_REPAIR_TOOLBOX="$HOME/.codex/skills/boss-repair/toolbox"; fi
CHECK_DIR="$(git rev-parse --git-dir)/boss-repair-checks"
mkdir -p "$CHECK_DIR"
HEAD_SHA="$(gh pr view --json headRefOid -q .headRefOid)" || exit 1
gh pr checks --json name,state,bucket,workflow > "$CHECK_DIR/checks.json"
gh api "repos/OWNER/REPO/commits/$HEAD_SHA/check-runs?per_page=100" --paginate --slurp > "$CHECK_DIR/runs.json"
gh run list --commit "$HEAD_SHA" --json name,workflowName,status,conclusion,headSha,event --limit 100 > "$CHECK_DIR/workflow-runs.json"
node "$BOSS_REPAIR_TOOLBOX/pr-check-state.mjs" classify \
  --head-sha "$HEAD_SHA" --observed-sha "$HEAD_SHA" \
  --checks "$CHECK_DIR/checks.json" --check-runs "$CHECK_DIR/runs.json" \
  --workflow-runs "$CHECK_DIR/workflow-runs.json" --prior "$CHECK_DIR/prior-contexts.json"
```

Before you push, copy the current head's `checks.json` payload to `$CHECK_DIR/prior-contexts.json`
(a bare JSON array of names is still accepted): a path-filtered push can shrink the check set, and
without the previous set the classifier cannot tell "all gates passed" from "fewer gates ran"
(`provesGreen` stays false). The payload's `workflow` and conclusions let the classifier drop a
context whose workflow did not run on the new head (`notTriggered`) and name the
`reported-on-prior-head` remedy. A head workflow run still queued or running holds the verdict at
`pending`.

**GraphQL quota.** GitHub's GraphQL quota runs out before REST. When a `gh pr …` read is
rate-limited, use REST and say so in the report (start the line with `DEGRADED_READ`):

| Question     | REST                                                                     |
| ------------ | ------------------------------------------------------------------------ |
| Head SHA     | `gh api repos/OWNER/REPO/pulls/PR_NUM -q .head.sha`                      |
| Base ref     | `gh api repos/OWNER/REPO/pulls/PR_NUM -q .base.ref`                      |
| Mergeability | `gh api repos/OWNER/REPO/pulls/PR_NUM -q '.mergeable, .mergeable_state'` |
| Check runs   | `gh api repos/OWNER/REPO/commits/HEAD_SHA/check-runs --paginate`         |

REST `mergeable: null` / `mergeable_state: unknown` and a check run with an empty `conclusion` are
unobserved. A degraded read can make a verdict worse, never better.

## Ending a pass

Every pass ends in one of these:

- **repaired** — a fix was pushed. Exit zero.
- **nothing to repair** — checks green, review feedback clean, no conflict. Report it and exit zero.
  Don't invent work.
- **parked** — green and conflict-free, but every open thread waits on a human. Exit zero.
- **residual** — something remains that this pass could not or should not resolve: pending CI, an
  inherited failure, a thread awaiting a human, a deferred resolve, a superseded CI view, a diverged
  branch, a conflict too tangled to settle, a question only a human can answer. Report each one and
  **exit zero**.
- **true stop** — the pass could not run at all: required tooling missing, auth failure, the repo or
  PR unreadable, an unexpected exception. Exit non-zero.

Never exit non-zero to signal "something remains": the plugin counts that as a failed attempt,
abandons its in-session retry, and backs off a branch that was never broken.

Report concisely:

- **Problem** — what was wrong (or "nothing").
- **Actions** and **commits** (hash and subject), noting any pre-existing work you adopted.
- **Gate results** — each gate's own quoted summary line, or "not run".
- **Root causes** — the cause → checks table, when checks failed.
- **Push state** — whether everything you committed is on `origin/<branch>`, or why not.
- **Status** — checks green / failing / pending / unknown; review feedback clean / parked / open.
- **Residuals** — each with why this pass stopped short, or "none".

## Watch mode

Run passes in a loop, at most **5 repair passes**:

1. At the start of each pass, re-read the PR head. If it is not a fast-forward of the head the
   previous pass started from (`git merge-base --is-ancestor <previous> <current>`), someone rewrote
   the branch: discard everything the earlier passes concluded and re-derive from the new head.
   Keep a note per pass of the SHA you pushed (if any) and the strategy and file it targeted.
2. Poll checks, review feedback and mergeability ([Reading PR state](#reading-pr-state)).
3. Open review threads → handle them (as above), push, next pass. Conflict → repair, push, next pass.
   Failing checks → repair, push, next pass.
4. Pending checks → wait on an event, not a clock. If callbacks are available
   (`callbacksAvailable` / `resolveCallbackAdapter` in `toolbox/callback/adapter.mjs`), register one
   watch per trigger in the adapter's `policy.watchTriggers`, each in its own group, with the
   adapter's `policy.defaultExpiresIn`:

   ```bash
   boss callback add "$PR_NUMBER" "$TRIGGER" --group "repairwait-$PR_NUMBER-$TRIGGER" \
     --independent-watch --message "$MSG" --expires-in "$WATCH_EXPIRY" --json
   ```

   On every wake, re-read the real state before acting. Without callbacks, wait with
   `node "$BOSS_REPAIR_TOOLBOX/ci-wait.mjs" run --pr <n>`, one tool call per chunk: re-issue it while
   it returns `state: continue`; `settled` is green, `failed` is red, a `timeout` with
   `trend: converging` earns one `run --extend`, and any other `timeout` or `unknown` is not green.
   Never `sleep` 60 seconds or more waiting for CI.

5. **No progress:** if a pass pushed nothing that is still on the branch and the failing signal is
   unchanged, stop — don't spin on something you can't fix. If two consecutive passes keep applying
   the same strategy to the same file, say so in the report (a branch that keeps re-conflicting).
6. **Done** when checks are green, mergeability is not `CONFLICTING`, and review feedback is `clean`
   or `parked` with every fixed or declined thread resolved. Re-poll review feedback after checks go
   green — that is when fresh threads appear. Before exiting, make sure something will still observe
   the PR (`$CHECK_VERDICT_JSON` is the classify output above, `$PR_VIEW_JSON` is
   `gh pr view --json state,isDraft,mergedAt,mergeStateStatus`, `$WATCH_LIST_JSON` is
   `boss callback list --chat "$BOSS_AGENT_SESSION_ID" --json`):

   ```bash
   node "$BOSS_REPAIR_TOOLBOX/callback/ci-watch.mjs" classify \
     --check-verdict "$CHECK_VERDICT_JSON" --pr-view "$PR_VIEW_JSON" --watches "$WATCH_LIST_JSON" \
     --target-chat "$BOSS_AGENT_SESSION_ID" --pr "$PR_NUMBER" \
     --triggers "$(
       node --input-type=module -e '
         import{pathToFileURL as u}from"node:url"
         const {resolveCallbackAdapter}=await import(u(process.env.BOSS_REPAIR_TOOLBOX+"/callback/adapter.mjs").href)
         process.stdout.write(resolveCallbackAdapter(process.env).policy.watchTriggers.join(","))
       '
     )" \
     ${CALLBACKS_AVAILABLE:+--callbacks-available} --arm-attempts "$ARM_ATTEMPTS"
   ```

   `unwatched` means arm the `missingTriggers` it names (once) before exiting; `settled`, `watched`
   and `polled` may exit. Remove the watches you registered.

End with the report plus a final line naming why the loop ended — exactly one of `green`, `parked`,
`no-progress`, `max-attempts`, `blocked` (a human must act) — with the number of passes used and the
pending-check state. Callers parse this token. Ending short of green is still a residual and exits
zero; only a true stop exits non-zero. When the 5-pass bound ends the loop, the residuals include
pending checks and any review feedback that arrived after the final push.

## Post-terminal notes extensions (repo opt-in)

Skip this phase when `BOSS_NOTES_SUPPRESSED=1` (a calling core owns the notes for its whole run).
Otherwise, after the terminal outcome is decided:

```bash
BOSS_REPAIR_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/toolbox"
if [ ! -d "$BOSS_REPAIR_TOOLBOX" ]; then BOSS_REPAIR_TOOLBOX="$HOME/.codex/skills/boss-repair/toolbox"; fi
node "$BOSS_REPAIR_TOOLBOX/skill-extensions.mjs" discover --core boss-repair --role notes --json
```

No extensions ⇒ do nothing and print nothing. Report each `skipped` entry whose `deliberate` is
`false` as `extension <name>: skipped (<reason>)`. Roll `notesDefaults.sampleRate` (`notesSampleRate`
in `toolbox/skill-config.mjs`, default `1.0`) once per run, reusing `NOTES_SAMPLED` if an earlier
phase already rolled; on a miss, stop. Otherwise write at most five short, secret-free observations
(problem plus a file/skill/command pointer — never transcripts, command output or credentials) to an
`observations.md` in a fresh temp dir, and dispatch each extension as an awaited subagent (bounded by
`BOSS_SKILL_EXTENSION_TIMEOUT_MS`, default 300000 ms) whose instructions are the `SKILL.md` read from
its descriptor's `skillPath` (resources resolve from `dir`), passing:

```json
{
  "role": "notes",
  "core": "boss-repair",
  "context": {
    "mode": "<interactive|headless>",
    "core": "boss-repair",
    "outcome": "<terminal outcome>",
    "repoId": "<BOSS_REPO_ID or null>",
    "observationPath": "<observations.md path>"
  },
  "runTmp": "<temp dir>",
  "outPath": "<temp dir>/notes-<extension-name>.json"
}
```

Validate each result with `node "$BOSS_REPAIR_TOOLBOX/skill-extensions.mjs" validate --role notes
--file <outPath>`, record the persisted-note count or the skip reason, and remove the temp dir. This
phase never changes the outcome, exit code, or any PR write.
