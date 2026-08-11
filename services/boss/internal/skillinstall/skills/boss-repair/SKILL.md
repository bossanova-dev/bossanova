---
name: boss-repair
description: Automated PR repair — fixes conflicts, failing checks, and review feedback
---

# PR Repair: Automated Fix Workflow

This skill is invoked automatically by the repair plugin when a PR enters a failing state (red status). It assesses the current PR state, identifies the root cause, and systematically repairs the issue.

---

## When This Skill is Invoked

The repair plugin automatically invokes this skill when:

- **Failing status (3)**: CI checks are failing
- **Conflict status (4)**: Merge conflicts with base branch
- **Rejected status (5)**: Review feedback requires changes

This skill normally runs **automatically** via the repair plugin and performs a focused repair pass per invocation: fix, push, wait for the resulting PR checks to finish, then report green/red/pending status. The repair plugin owns retrying additional attempts when checks remain red.

It may also be run **by hand** as `/boss-repair watch` to keep the whole wait-and-repair loop inside one manual invocation until the PR is green. See [Watch Mode](#watch-mode) below. The repair plugin always invokes this skill with no arguments, so automated runs never enter watch mode.

---

## Linear-History Invariant

**Always sync a branch with its base by rebasing. Never merge the base branch into the session branch.**

```bash
git fetch origin "$BASE_BRANCH"
git rebase "origin/$BASE_BRANCH"
```

A `git merge` of the base ref — and any `git pull` that records a merge — is **FORBIDDEN**. On a repo
whose merge strategy is rebase, a merge commit on the PR branch structurally breaks GitHub's
rebase-merge: GitHub refuses the merge no matter how green the checks are, and every later repair
round that merges again re-poisons the branch, deadlocking the PR. When a pull is unavoidable, use
`git pull --rebase`.

**Preflight — before any push that follows a base sync, assert zero merge commits:**

```bash
MERGE_COUNT=$(git rev-list --merges --count "origin/$BASE_BRANCH"..HEAD) || exit 1
test "$MERGE_COUNT" = 0 ||
  { echo "Merge commit(s) on this branch; linearize before pushing"; exit 1; }
```

Capture the count and compare it as a **string**. `test "$(…)" -eq 0` fails **open**: when
`$BASE_BRANCH` is unset or `origin/$BASE_BRANCH` has not been fetched, `git rev-list` errors and the
substitution is empty, and an empty operand compares equal to `0` under zsh — so the guard would pass
and the poisoned branch would be pushed. The form above fails closed in every shell.

**Diagnosis.** A count greater than `0` means one or more merge commits are on the branch — most
often a base merge. That is the one-line signal that GitHub's rebase-merge will refuse this PR. List
what you are about to flatten with `git rev-list --merges --oneline "origin/$BASE_BRANCH..HEAD"`.

**Linearize recovery.** Flattening **discards anything recorded only in a merge commit** — manual
conflict-resolution edits and files added directly in the merge. Run the Strategy A amendment guard
first (it lists those merges via `git show --remerge-diff`) and convert any amendment it reports into
a normal commit before continuing; otherwise this rewrite loses it silently. Then flatten, re-verify
the count is `0`, and force-push:

```bash
git fetch origin "$BASE_BRANCH"
git rebase --onto "origin/$BASE_BRANCH" "$(git merge-base "origin/$BASE_BRANCH" HEAD)"
# Resolve any conflicts in-rebase (Strategy A steps 2-4) until the rebase COMPLETES, then:
if [ -d "$(git rev-parse --git-path rebase-merge)" ] || [ -d "$(git rev-parse --git-path rebase-apply)" ]; then
  echo "Rebase still in progress; finish it or 'git rebase --abort' before pushing"; exit 1
fi
MERGE_COUNT=$(git rev-list --merges --count "origin/$BASE_BRANCH"..HEAD) || exit 1
test "$MERGE_COUNT" = 0 ||
  { echo "Branch still has merge commits after linearizing; resolve by hand"; exit 1; }
git push --force-with-lease
```

Each assertion must be `||`-guarded like the ones above — a bare `test` line prints nothing and gates
nothing, so the force-push on the next line would run regardless.

A plain `git rebase "origin/$BASE_BRANCH"` flattens merge commits too; the `--onto` form above is the
explicit equivalent for the linear-base case this recovery targets. Do **not** pass `--rebase-merges`
when linearizing — it recreates the very merge commits you are removing. Strategy A names it only to
explain what its amendment guard protects.

---

## Boss transport preflight

Before Phase 1, decide **which carrier** this run will use for boss session operations — reading the
session's state, its check snapshots, its chats. There are two: the boss MCP tools, and the `boss`
CLI. Validate whichever the runtime actually exposes and BLOCK only when **neither** is complete; a
runtime with no MCP server but a working `boss` binary repairs perfectly well, and stopping it would
be a self-inflicted outage.

Enumerate the available boss MCP tool names and `boss` subcommands, then diff both at once with
`bossEpicTransportPreflight({availableTools, availableCliCommands})` from
`$BOSS_REPAIR_TOOLBOX/session/boss.mjs`, which returns `{ ok, transport, missing, degraded,
partial }`:

The CLI is **preferred, not a fallback** — a complete CLI set wins even when the MCP set is also
complete. That preference is what made it safe to stop wiring the boss MCP server by default, so on
a managed spawn expect `cli`.

- `transport: 'cli'` — every `cli`-mapped capability is reachable, whether or not MCP is also
  complete. Proceed; substitute each capability's `cli` invocation for its MCP spelling.
- `transport: 'mcp'` — the CLI set is incomplete and the tool set is complete. Proceed on the
  richer carrier.
- `ok: false` — neither is complete. Stop `BLOCKED: no complete boss transport: <comma-separated
missing>`, naming everything absent from both carriers in one line.

**Report the chosen transport in the repair run's opening line** — `transport: <mcp|cli>`, plus
`degraded: <comma-separated capabilities>` when `degraded` is non-empty, plus
`partial: <capability>(<missing fields>)` for each entry in `partial`. The degraded names are
capabilities with no CLI equivalent at all — `resolveContext`, `getSessionStatuses` and
`createPlanningChat` (three, not two: `boss new` has no `--quick-chat` flag) — so a CLI-mode run
must use their documented fallbacks rather than guessing; saying so out loud is what stops a later
reader believing the run consulted them.

A capability the CLI covers only _partially_ still has a transport and is **not** degraded, which is
why `partial` is a separate list rather than an extension of that one. Today it holds `getSession`:
`boss show --json` carries the lifecycle state but none of `last_agent_activity_at`,
`repair_active`, `attention_status.reason`, `pr_mergeable` or `merge_block`. Where a `partial` entry
names a routing signal the CLI cannot supply, treat that signal as **not settled**, never as a pass
— `pr_mergeable` and `merge_block` in particular are how a repair run would otherwise mistake a
conflicting PR for a mergeable one.

Repair reaches for boss session state rarely — `gh` carries most of this workflow — so an incomplete
boss transport is usually a nuisance rather than a stop. Run the preflight anyway: discovering the
gap at the moment you need the value is how a repair run stalls with the PR half-fixed.

---

## Repair Workflow

### Phase 1: Assess Current State

**1.1 Check PR Status**

Record the PR head SHA **first**, ahead of every other command here, per the round-freshness rule in the [Phase 2](#phase-2-execute-repair-strategy) lead-in:

```bash
gh pr view --json headRefOid -q .headRefOid   # record this value as ROUND_HEAD for the rest of the round
git status                    # Check for conflicts and uncommitted changes
git log --oneline -5          # Recent commits
gh pr view                    # PR details, checks, and review status
```

**1.2 Identify Problem Type**

Based on the output, categorize the issue:

- **Merge Conflict**: Git reports conflicts in files
- **Failing Checks**: PR checks show failures (tests, lint, build)
- **Review Feedback**: PR has requested changes or comments

**1.3 Identify Project Gate Commands**

Before running local checks, discover this repo's commands from project instructions, CI, and command files (`Makefile`, `justfile`, `Taskfile.yml`, `package.json`, `go.mod`, etc.). Use the smallest non-duplicative command set that covers the failing area.

### Phase 2: Execute Repair Strategy

For each triggered strategy below, the orchestrator runs that strategy's investigate-and-fix steps and ends with a short summary — files changed, what was fixed, and residual risk. **Where** those steps run is one decision, taken per strategy:

```
subagent tool permitted AND available  -> dispatch a fresh awaited subagent (the expected path)
tool absent, or a higher-priority      -> run the strategy inline (sanctioned)
  instruction prohibits calling it
dispatch attempted and failed          -> run the strategy inline (sanctioned)
```

Dispatch is the normal branch — a session that provisions the subagent tool should use it (`subagent_type: general-purpose`), because the subagent keeps the bulk material out of the orchestrator's context and gives the fix a second voice. Inline is the **sanctioned** path for a session where the tool genuinely is not usable, not a lesser mode to choose by preference: read the permission actually in force rather than assuming. A blanket "do not call the subagent tool unless the user asked for it" instruction is one common instance of the second branch; `BOSS_CRON=true` by itself is **not** one — an unattended run may dispatch. Either way the strategy's steps and its reporting are identical; only the context they run in differs, so running inline is never a reason to skip a step or shorten the summary.

<!-- tier: opus (no override) because this dispatch runs whichever strategy triggered — A (merge-conflict resolution), B (fixing failing tests/build), or C (implementing review feedback) — all of which author or evaluate code, i.e. judgment. Not tiered down. -->

A dispatch stays on the orchestrator's model (Opus): conflict resolution, failing-check code fixes, and review-feedback reasoning are all judgment, so no cheaper `model:` override is applied. The subagent keeps the bulk material (diffs, CI logs, `gh run view` output, review threads) inside its own context; only the summary returns to the orchestrator, which stays thin. This is orchestration framing only: Strategy A/B/C below are unchanged and are exactly what runs, dispatched or inline. A dispatch is awaited (**never** `run_in_background`) and its failure is a tool error, not a repair failure — it routes to the inline branch above and must never turn a would-be clean exit into a nonzero one.

**Round freshness — capture the head SHA before reading PR state.** The first thing a round does,
before reading review threads, check runs, or mergeability, is record the commit the **PR head**
points at — the commit every check run is attached to. That capture is
`gh pr view --json headRefOid -q .headRefOid`, and it is already the first command of
[Phase 1](#phase-1-assess-current-state) step 1.1, ahead of `gh pr view`.
**Do not run it again here** as the round's routine baseline: re-capturing in Phase 2 would
re-baseline onto a head that has already absorbed any mid-round push, and the comparison below could
never fire. The missing-baseline recovery below is the one exception, and it is safe only because it
re-reads the check runs alongside the re-capture.

The local worktree tip is **not** the value to compare. `git rev-parse HEAD` only moves when this
round itself commits, so it can never see the push by another actor that this rule exists to catch —
and it moves on a local commit that was never pushed and superseded no check run at all.

Carry the printed SHA in your own notes, not only in a shell variable: shell state does not survive
between tool calls, and an empty baseline must never be mistaken for a moved head. If you reach the
freshness test without a recorded baseline, do not cancel and do not proceed on what you already
read: re-capture the PR head and re-read the check runs against it, so the view you act on is
coherent with a baseline you hold. A missing baseline is never on its own a reason to cancel.

**Handle review feedback before CI failures.** Thread content is stable: a reviewer's words mean the
same thing whether or not another commit has landed since they were written, so feedback can be
acted on exactly as read. Check runs are volatile — each describes one specific commit, and a push
that lands mid-round supersedes every check result read before it. Taking the stable half first
leaves only the volatile half needing a freshness test.

**Re-read the head SHA before acting on a CI failure, and skip the failure when it has moved.**
Re-read the PR head (`gh pr view --json headRefOid -q .headRefOid`) and compare it against
`ROUND_HEAD` at the moment you are about to act on a check
failure. If they differ, this round's CI view describes a superseded commit:
do not fix it and do not push a commit for it.
Check the head once more immediately before you push a CI fix, too: the window between deciding to
act and pushing is small but not empty, and a head that moved inside it supersedes the fix you are
about to send. Cancel the CI half then as well rather than force-pushing over the newer commit.
Keep the commit you already made — do not reset it — and name it in the residual as built but unpushed;
the branch being ahead of origin is expected in this one case, so Phase 3's clean-tree check and
Watch Mode's no-progress comparison both read it as the cancellation it is, not as progress.
Skip only the CI half of the round — any conflict repair or review work this round still owes is
unaffected and is still due first; end the pass only once that work is done.
Report the superseded CI view as a residual and **exit zero**, per
[Residuals vs true stops](#residuals-vs-true-stops).

That skip is **cancellation, not error**. A superseded round is a normal outcome, not a failed one:
the push that superseded it raises its own signal and gets its own round, which reads a coherent
view of the new head. Nothing is broken and nothing is lost, so a cancelled round
must never exit non-zero. In Watch Mode the cancellation is not an exit at all: skip the superseded
CI view, return to the poll loop, and read the new head's state on the next pass, per
[Watch Mode](#watch-mode) — the exit-zero form applies to default mode only.

**A round's own push never marks that round stale.** The freshness test is applied only _before_ this
round acts on a check failure, and only against commits this round did not author. Once the round has
fixed a failure and pushed, the head is _expected_ to differ from `ROUND_HEAD` — that is the round
succeeding by its own hand, not going stale. After **every** push this round makes — including the
review-feedback push that the ordering above puts first —
re-baseline `ROUND_HEAD` to the commit you just pushed
(`git rev-parse HEAD` immediately after your own push is exactly that commit)
**and re-read the check runs**: a CI view read before any push, your own included, describes a
superseded commit and must be re-read rather than acted on. You never have to inspect authorship — because you
re-baseline after every push you make, any remaining difference from `ROUND_HEAD` is by construction
a commit this round did not author. Only a head that moved before this round acted, by a commit this
round did not author, cancels it.

Check runs for a commit you just pushed usually do not exist yet, so that re-read is a freshness
check inside the pass, not a second repair pass and not the Phase 3 poll. If it returns nothing, or
only pending runs, there is no fresh CI view to act on: leave it to Phase 3's post-push poll (Watch
Mode: to the next poll) rather than falling back on the pre-push results. `$BEFORE` in
[Watch Mode](#watch-mode) step 1 is a different baseline for a different question — it is the local
tip and detects whether _this_ pass made progress, while `ROUND_HEAD` is the PR head and detects a
push this round did not make. They are not interchangeable.

#### Strategy A: Merge Conflicts

**Symptoms**: Git reports conflicts, PR status shows conflict

**Resolution**:

1. Fetch and rebase onto the PR base branch — never merge it in, per the
   [Linear-History Invariant](#linear-history-invariant):

   ```bash
   BASE_BRANCH=$(gh pr view --json baseRefName -q .baseRefName 2>/dev/null || true)
   if [ -z "$BASE_BRANCH" ]; then
     CURRENT_BRANCH=$(git branch --show-current)
     BASE_BRANCH=$(
       git for-each-ref --format='%(refname:short)' refs/remotes/origin |
         sed 's#^origin/##' |
         grep -Fvx HEAD |
         grep -Fvx "$CURRENT_BRANCH" |
         while read -r branch; do
           git merge-base --is-ancestor HEAD "origin/$branch" && continue
           base=$(git merge-base HEAD "origin/$branch" 2>/dev/null) || continue
           printf '%s %s\n' "$(git show -s --format=%ct "$base")" "$branch"
         done |
         sort -nr |
         awk 'NR == 1 {print $2}'
     )
     if [ -n "$BASE_BRANCH" ]; then echo "Using inferred git base branch: $BASE_BRANCH"; fi
   fi
   test -n "$BASE_BRANCH" || { echo "Could not determine PR base branch"; exit 1; }
   git fetch origin "$BASE_BRANCH"
   MERGE_AMENDMENTS=$(
     git rev-list --merges "origin/$BASE_BRANCH"..HEAD |
       while read -r merge_commit; do
         if [ -n "$(git show --remerge-diff --format= "$merge_commit")" ]; then
           echo "$merge_commit"
         fi
       done
   )
   if [ -n "$MERGE_AMENDMENTS" ]; then
     echo "Merge commit amendments detected; automatic rebase could drop manual conflict-resolution edits:"
     echo "$MERGE_AMENDMENTS"
     echo "Resolve manually or convert those amendments into normal commits before rebasing."
     exit 1
   fi
   git rebase "origin/$BASE_BRANCH"
   ```

   Use the **amendment guard** (the `MERGE_AMENDMENTS` probe above — distinct
   from the merge-count preflight in the
   [Linear-History Invariant](#linear-history-invariant)) because a plain rebase
   flattens merge commits but does not preserve manual conflict-resolution edits
   or files added directly in merge commits. Only rebase automatically when the
   guard finds no merge-commit amendments. `--rebase-merges` would preserve that
   shape, but it recreates the merge commits the invariant forbids — so when the
   guard trips, lift those amendments out of the merge and re-run the plain
   rebase; never resolve it with a new merge:

   ```bash
   for merge_commit in $MERGE_AMENDMENTS; do
     git show --remerge-diff --format= "$merge_commit" > "/tmp/amend-$merge_commit.patch"
   done
   git rebase "origin/$BASE_BRANCH"          # flattens; the amendments are now saved off-branch
   git apply "/tmp/amend-<sha>.patch"        # re-apply each captured amendment, oldest first
   git add -A && git commit -m "fix: re-apply conflict resolution from flattened merge"
   ```

   If an amendment does not re-apply cleanly, stop and escalate per
   [Complex Conflicts](#complex-conflicts) rather than merging the base in.

2. Identify conflicting files:

   ```bash
   git diff --name-only --diff-filter=U
   ```

3. For each conflicting file:
   - Read the file to see conflict markers (`<<<<<<<`, `=======`, `>>>>>>>`)
   - Understand both versions. During rebase conflicts, Git labels can feel
     reversed from a merge: `ours` is the upstream/base branch being rebased
     onto, and `theirs` is the replayed PR commit.
   - Resolve by:
     - Keeping both changes if they're independent
     - Choosing the correct version if they conflict
     - Merging logic intelligently if needed
   - Use Edit tool to remove conflict markers and apply resolution

4. Continue the rebase:

   ```bash
   git add <resolved-files>
   GIT_EDITOR=true git rebase --continue
   ```

   Set `GIT_EDITOR=true` so automated repair accepts the existing replayed
   commit message instead of blocking or failing when no interactive editor is
   available.

   If `git rebase --continue` reports that the replayed commit became empty,
   inspect the resolution and confirm the base branch already contains the
   commit's intended change. When the patch is genuinely redundant, skip that
   replayed commit:

   ```bash
   git rebase --skip
   ```

   Do not preserve empty commits with `git commit --allow-empty` unless the PR
   intentionally needs an empty marker commit.

   Repeat steps 2-4 until the rebase completes. Do not create a merge commit.

5. Test the rebased branch with the repo's formatting and test gates:

   ```bash
   # Examples only; use the commands discovered for this repo
   pnpm lint && pnpm test
   go test ./...
   cargo test
   ```

6. Run the merge-commit preflight, then push the rebased branch:
   ```bash
   MERGE_COUNT=$(git rev-list --merges --count "origin/$BASE_BRANCH"..HEAD) || exit 1
   test "$MERGE_COUNT" = 0 ||
     { echo "Merge commit(s) on this branch; linearize before pushing"; exit 1; }
   git push --force-with-lease
   ```
   A nonzero count means the branch is poisoned; run the linearize recovery in the
   [Linear-History Invariant](#linear-history-invariant) before pushing.

#### Strategy B: Failing Checks

**Symptoms**: PR checks show failures (tests, lint, build errors)

The A/B/C ordering here is presentational, not an execution order. If review feedback is also present, run [Strategy C](#strategy-c-review-feedback) first and apply the round-freshness rule from the Phase 2 lead-in before acting on anything below.

**Resolution**:

1. Identify which checks are failing:

   ```bash
   gh pr checks --json name,bucket,state,link
   ```

2. Get failure details:

   ```bash
   gh run view <run-id>     # View specific check run details
   ```

   If checks are still pending, do not block only on CI. First probe review threads and mergeability, then poll again as described in Phase 3 or Watch Mode.

3. For **test failures**:
   - Read test output to identify failing tests
   - Read the test file and implementation
   - Fix the root cause (not just the symptom)
   - Run the relevant test command locally to verify:
     ```bash
     # Examples only; use the command discovered for this repo
     pnpm test
     go test ./...
     cargo test
     ```
   - Commit the fix:
     ```bash
     git add <fixed-files>
     git commit -m "fix(tests): resolve failing test in <component>"
     git push
     ```

4. For **lint/format failures**:
   - Run the repo's formatter or lint fixer:
     ```bash
     # Examples only; use the command discovered for this repo
     pnpm lint --fix
     gofmt -w <files>
     cargo fmt
     ```
   - Commit if changes were made:
     ```bash
     git add .
     git commit -m "style: apply formatting fixes"
     git push
     ```

5. For **build failures**:
   - Read build output to identify error
   - Fix compilation/build issues
   - Verify locally with the repo's build command:
     ```bash
     # Examples only; use the command discovered for this repo
     pnpm build
     go test ./...
     cargo build
     ```
   - Commit and push the fix

#### Strategy C: Review Feedback

**Symptoms**: PR has requested changes or review comments

**Resolution**:

1. List all unresolved review threads and inline review comments.

   Do not rely on `gh pr view --comments` or `gh pr view --json comments` for this step. Those only cover PR conversation comments and can miss inline review comments with URLs like `#discussion_r...`.

   First, run the review feedback probe from this skill directory. This script uses both required GitHub APIs and prints a compact summary, so a blank result cannot be mistaken for "no comments."

   ```bash
   node scripts/review-feedback-probe.js
   ```

   Probe interpretation rules:
   - `probe_status=failed`: the probe failed. Fix the command or auth issue; do not report "no review feedback."
   - `probe_status=suspicious_zero`: `latestReviews` contains `COMMENTED`, but both probes found zero comments. Retry with the explicit repo and PR number before concluding there is no review feedback.
   - Empty stdout from any wrapped/batched command is a probe failure, not a zero-comment result.
   - Trust `repair_status` as the normalized result when `probe_status=ok`.
   - `repair_status=clean`: there are no unresolved review threads. Historical REST `inline_comments`, resolved GraphQL `review_threads`, and `COMMENTED` latest reviews do not require action by themselves.
   - `repair_status=needs_repair`: handle every printed unresolved thread.
   - `repair_status=parked`: every unresolved thread is waiting on a human. Do not re-dispatch repair work unless a later probe reports `needs_repair`.
   - `repair_status=unknown`: REST and GraphQL disagree or thread state is unavailable. Retry with explicit repo/pr before concluding there is no review feedback.

2. For each unresolved thread, triage into one of three categories:

   **a) Actionable — fix it:**
   - Read the relevant code/files
   - Implement the requested change
   - Mark the thread as dispatched before acting on it:
     ```bash
     node scripts/review-feedback-probe.js mark --thread THREAD_ID --disposition dispatched
     ```
   - Add a reply comment on the thread explaining what was fixed:
     Reply bodies are Markdown and must never be shell-interpolated. `-F` is selected here because
     the required local stub check proves its `@file` form passes the file's bytes through literally.
     First run the path-creation block by itself. It prints a concrete temporary path for the
     file-editing tool. Use the agent's file-editing tool to write the complete reply text to that
     path. Then put the same printed path in the submission block. Do not use shell input, redirection, `printf`, or a
     heredoc for the reply text: automatic repair has no interactive stdin, and shell source can
     reinterpret Markdown. The file must contain only the reply text.
     ```bash
     REPLY_BODY="$(mktemp)"
     printf '%s\n' "$REPLY_BODY"
     ```
     Use the agent's file-editing tool to write the exact reply text to the printed path, then:
     ```bash
     REPLY_BODY="/the/path/printed/above"
     if gh api repos/OWNER/REPO/pulls/PR_NUM/comments/COMMENT_ID/replies -F body=@"$REPLY_BODY"; then
       rm -f "$REPLY_BODY"
     else
       GH_STATUS=$?
       rm -f "$REPLY_BODY"
       exit "$GH_STATUS"
     fi
     ```
   - Resolve the parent review thread:
     ```bash
     gh api graphql -f query='
       mutation {
         resolveReviewThread(input: {threadId: "THREAD_ID"}) {
           thread { isResolved }
         }
       }'
     ```

   **b) Not actionable — decline and resolve:**
   Some review comments are by design, already fixed, stale (reference old code), or low-priority style suggestions. For these:
   - Add a reply comment explaining why it won't be fixed:
     First create and print a temporary path, then write the reply to that exact printed path with
     the agent's file-editing tool:
     ```bash
     REPLY_BODY="$(mktemp)"
     printf '%s\n' "$REPLY_BODY"
     ```
     Submit that same path after the file-editing tool has written the reply:
     ```bash
     REPLY_BODY="/the/path/printed/above"
     if gh api repos/OWNER/REPO/pulls/PR_NUM/comments/COMMENT_ID/replies -F body=@"$REPLY_BODY"; then
       rm -f "$REPLY_BODY"
     else
       GH_STATUS=$?
       rm -f "$REPLY_BODY"
       exit "$GH_STATUS"
     fi
     ```
   - Then resolve the thread:
     ```bash
     gh api graphql -f query='
       mutation {
         resolveReviewThread(input: {threadId: "THREAD_ID"}) {
           thread { isResolved }
         }
       }'
     ```

   **c) Unclear — ask for clarification:**
   - Add a reply comment asking for clarification:
     First create and print a temporary path, then write the reply to that exact printed path with
     the agent's file-editing tool:
     ```bash
     REPLY_BODY="$(mktemp)"
     printf '%s\n' "$REPLY_BODY"
     ```
     Submit that same path after the file-editing tool has written the reply:
     ```bash
     REPLY_BODY="/the/path/printed/above"
     if gh api repos/OWNER/REPO/pulls/PR_NUM/comments/COMMENT_ID/replies -F body=@"$REPLY_BODY"; then
       rm -f "$REPLY_BODY"
     else
       GH_STATUS=$?
       rm -f "$REPLY_BODY"
       exit "$GH_STATUS"
     fi
     ```
   - Do NOT resolve the thread — leave it open for the reviewer.
   - Mark it `needs-human` after posting the clarification, so later repair rounds park it until the
     reviewer's last-comment identity changes:
     ```bash
     node scripts/review-feedback-probe.js mark --thread THREAD_ID --disposition needs-human
     ```
   - A thread left open this way is a **residual**: record it in the Repair Summary alongside the
     rest, per [Residuals vs true stops](#residuals-vs-true-stops). It does not fail the pass.

   **IMPORTANT**: Every unresolved thread must be handled. Do not silently skip threads. Either fix and resolve, decline and resolve with an explanation, or ask for clarification. Fixed or declined threads must be resolved before the PR is considered clean. Only true clarification requests may remain unresolved.

3. After implementing changes:
   - Because this checklist runs before commit, use only a zero-write scratch copy. Do not mutate the
     checkout. An in-place probe belongs after a commit, with its commit-first backup and exact-restore
     safeguards.
   - Materialize every scratch fixture through a read-only, `noatime` view of the checkout source. A
     plain host read, copy, or directory traversal can update checkout access metadata before the
     sandboxed command begins. If that atime-safe source view is unavailable, reject the probe.
   - A shell, interpreted-source, or test-gate invocation is zero-write only inside a filesystem sandbox
     whose only writable path is `"$PROBE_DIR"`, with an explicit read-only allowlist for the scratch
     fixture, required tool binaries and libraries, and any dependency cache. Start with a cleared
     environment (put `HOME`, `TMPDIR`, `PWD`, and XDG paths under `"$PROBE_DIR"`), do not expose host
     credentials or home paths, and deny writes outside the sandbox root. Network access is disabled,
     including loopback, link-local, and metadata endpoints. If filesystem or network confinement is
     unavailable, reject the probe; never execute the copied command on the host.
   - If a fix adds or changes a guard, gate, or assertion, prove it non-vacuous with this ordered
     checklist before committing the review-feedback result:
     1. **Name the property** the gate claims to forbid, including the unbounded direction for a
        one-sided bound.
     2. **Mutate the production feed, never the assertion**, using the zero-write scratch copy.
     3. **Prove the mutation landed** before reading the result; a no-op mutation proves nothing.
     4. **Require red for the right reason** and require the failure to name the property. A compile
        or harness error is not evidence that the gate detected the mutation.
     5. **Restore exactly, then prove the restore** and re-run the gate green.
   - Run the repo's formatting and test gates:
     ```bash
     # Examples only; use the commands discovered for this repo
     pnpm lint && pnpm test
     go test ./...
     cargo test
     ```
   - Commit with reference to review feedback:

     ```bash
     git add <changed-files>
     git commit -m "$(cat <<'EOF'
     fix(review): address feedback on <component>

     - [Change 1 from review]
     - [Change 2 from review]

     Co-Authored-By: <the Co-Authored-By trailer your harness specifies>
     EOF
     )"
     ```

   - Push changes:
     ```bash
     git push
     ```

### Phase 3: Verify and Monitor

**3.1 Verify Fix**

After applying the repair:

1. Check that local state is clean:

   ```bash
   git status     # Should show clean working tree
   ```

2. Verify the committed fix is pushed to origin:

   ```bash
   git status -sb  # Branch should not be ahead of origin
   ```

3. Poll the remote PR state, then report the final PR state (default mode performs one post-push poll; in Watch Mode you loop per the [Watch Mode](#watch-mode) section):

   ```bash
   gh pr checks --json bucket
   node scripts/review-feedback-probe.js
   gh pr view --json mergeable -q .mergeable
   ```

4. If `repair_status=needs_repair` or `repair_status=unknown`, handle or retry the review feedback before exiting; do not treat unknown review status as clean. If checks are still pending, failed, or timed out after known review feedback is handled, note that in output. In default mode, still **exit cleanly (zero)** after the push even when checks are pending or failed — report the status but do not exit nonzero. The repair plugin only enters its in-session resume/retry loop after a clean exit; a nonzero exit makes it abandon that loop and fall back to a slower fresh sweep. (Watch Mode is the exception: it owns the loop and re-runs the matching repair strategy on failures itself, per the [Watch Mode](#watch-mode) section.)

   Dispatching Strategy A/B/C investigation into an awaited subagent (see the Phase 2 lead-in and the [Watch Mode](#watch-mode) section) is internal orchestration bookkeeping for a single repair pass only. It MUST NOT change this default-mode contract — one pass, push, a single poll, and a clean zero exit even when checks are pending, so the repair plugin keeps owning retries — nor the default-vs-watch distinction described above.

   ```
   ✓ Repair applied and pushed
   Checks finished: [passing | failing | pending | timed out]
   ```

**3.2 Report Results**

Provide a concise summary:

```
## Repair Summary

**Problem Identified**: [Merge conflict | Failing tests | Review feedback]

**Actions Taken**:
- [Action 1]
- [Action 2]
- [Action 3]

**Commits Created**:
- <commit-hash>: <commit-message>

**Status**:
- Changes pushed to origin
- [Checks are now passing | Checks are pending | Awaiting review]

**Residuals**:
- [What this pass could not resolve and why the round stopped short | none]
```

---

## Residuals vs true stops

Not every unfinished repair is a failure. Decide which of these two outcomes you are in **before**
choosing an exit code.

- **Residual** — a condition this pass legitimately could not resolve: conflicts too tangled to
  settle safely, a fix that needs a decision only a human can make, a red signal whose cause lives
  outside the branch. A residual is **not an error**. Report it explicitly — as a residual line in
  the Repair Summary, and in a PR comment when a human has to act — and then **exit zero**. A
  residual is always a _reported_ outcome, never a silent one; leaving it out of the report is the
  real failure.
- **True stop** — the worker could not run at all: required tooling is missing, the repository or PR
  is unreadable, or an unexpected exception aborted the pass. There is no repair outcome to report,
  so **exit non-zero** and let the breakage surface loudly.

Both outcomes describe how the **pass itself** ends. The `exit 1` guards inside the command snippets
above are narrower: they abort that step — refusing to push a branch that would poison the PR, or a
rebase that would drop manual edits — rather than settling the pass's exit status. When a guard trips,
follow whatever the surrounding step says to do next — the merge-count preflights route to the
linearize recovery above, the amendment guard escalates to
[Complex Conflicts](#complex-conflicts) when its recovery will not re-apply, and a guard that leaves
nothing to work with (no resolvable base branch) is a true stop — then end the pass by the same rule:
a residual when there is an outcome to report, a true stop only when the pass could not run.

Retry, cooldown, and attempt caps belong to the **daemon-owned repair loop** (the repair plugin
described in [Integration with Repair Plugin](#integration-with-repair-plugin)), not to this worker.
The loop decides whether and when another pass runs, and a pass that exits zero with its residuals
reported gives it exactly what it needs to decide. Never simulate backoff by failing on purpose: a
non-zero exit that only means "something remains" is recorded as a **failed attempt**, so it does not
just disguise a normal outcome as a crash — it abandons the loop's in-session retry for a slower
fresh sweep, lengthens the backoff before the next pass, and leaves a consecutive-failure count
standing against a branch that was never broken. Failing does not "apply the cooldown" the branch
needs; it charges the branch for one it did not earn. Watch Mode is the exception the rest of this
document already names: a
manual `/boss-repair watch` run owns its own bounded loop and is not driven by the daemon loop's
retry schedule, per [Watch Mode](#watch-mode). The residual-versus-true-stop rule still decides how
that loop exits.

---

## Edge Cases and Error Handling

### Complex Conflicts

If conflicts are too complex to resolve automatically:

1. Add a PR comment explaining the situation:

   ```bash
   gh pr comment --body "Automatic repair detected complex merge conflicts that require manual review. Files affected: <list>. Please resolve manually."
   ```

2. Report the unresolved conflict as a **residual** in the Repair Summary — name the files still
   conflicting and why this round stopped short — and **exit zero**. This is not a true stop, so do
   not fail the pass to force a cooldown; whichever loop is driving this pass — the daemon-owned one,
   or watch mode's own — owns backoff and any further attempt.

### Cascading Failures

If fixing one issue causes another (e.g., fixing a conflict causes tests to fail):

1. Continue with the next repair strategy
2. Don't give up after the first fix
3. Iterate through strategies until stable
4. If the repair began by rebasing the PR branch, keep using
   `git push --force-with-lease` after any follow-up fixes. The branch history
   was already rewritten, so a plain `git push` will fail or leave the repair
   unpushed.

### Missing Context

If the repair requires information not available (e.g., design decisions, external dependencies):

1. Add a PR comment requesting clarification
2. Do NOT make assumptions
3. Report the open question as a **residual** in the Repair Summary — state what information is
   missing and who has to supply it — and **exit zero**. Waiting on a human is not a true stop, and
   failing the pass would neither get the question answered sooner nor tell the loop driving it
   anything it does not already track.

---

## Guidelines

1. **Root Cause Over Symptoms**: Fix the underlying issue, not just the visible error
2. **Minimal Changes**: Only change what's necessary to resolve the issue
3. **Test Locally**: Always run the repo's formatting and test gates before pushing
4. **Clear Commits**: Write descriptive commit messages that explain the fix
5. **Atomic Repairs**: Each repair attempt should be self-contained
6. **Stop Early, Not Loudly**: If this pass cannot fix the issue, report the residual and exit zero
   rather than grinding — see [Residuals vs true stops](#residuals-vs-true-stops)
7. **No raw bulk output in the main thread**: Never paste full diffs, CI logs, `gh run view` output, or
   review threads into the orchestrator's context — that bulk is re-charged on every later turn. The
   Phase 2 strategy subagent reads them in its own context and returns only a summary; when working
   inline, filter to the few relevant lines (`gh pr checks --json name,state,bucket`,
   `gh run view <run-id> --log-failed | tail`, `node scripts/review-feedback-probe.js`'s compact
   summary) instead of dumping.

---

## Anti-Patterns

| Anti-Pattern                                                      | Problem                             | Fix                                                               |
| ----------------------------------------------------------------- | ----------------------------------- | ----------------------------------------------------------------- |
| Accepting all "ours" or "theirs" blindly                          | Loses important changes             | Review each conflict individually                                 |
| Skipping tests after conflict resolution                          | Introduces bugs                     | Always run full test suite                                        |
| Commenting out failing tests                                      | Hides problems                      | Fix the root cause                                                |
| Merging the base branch in to resolve drift                       | Merge commit deadlocks rebase-merge | Rebase onto the base; keep `git rev-list --merges --count` at `0` |
| Plain force pushing                                               | Loses history, breaks collaboration | Use `--force-with-lease` after rebase                             |
| Making unrelated "improvements"                                   | Scope creep                         | Fix only the reported issue                                       |
| Retrying immediately after failure                                | Triggers cooldown loops             | Fix the root cause first                                          |
| Pasting raw diffs / CI logs / review threads into the main thread | Re-charged on every later turn      | Summarize in the strategy subagent; filter with `--json` / `grep` |

---

## Example Scenarios

### Scenario 1: Simple Rebase Conflict

```
Problem: PR shows conflict status, git reports conflicts in server.go

Resolution:
1. Fetch and rebase onto the PR base branch (never merge it in)
2. Read server.go, see conflict in import statements
3. Keep both imports (they're independent)
4. git add server.go && GIT_EDITOR=true git rebase --continue
5. Run repo formatting and test gates (passes)
6. Preflight: git rev-list --merges --count "origin/$BASE_BRANCH"..HEAD → 0
7. git push --force-with-lease

Result: ✓ Conflict resolved, checks passing
```

### Scenario 2: Failing Test

```
Problem: PR checks show test failure in user_handler_test.go

Resolution:
1. gh pr checks → see "TestUserCreate" failing
2. gh run view <run-id> → read error: "expected status 201, got 400"
3. Read user_handler_test.go and user_handler.go
4. Found bug: missing validation check for email field
5. Add validation in user_handler.go
6. Repo test command passes
7. git add . && git commit -m "fix(user): add email validation in create handler"
8. git push

Result: ✓ Tests now passing
```

### Scenario 3: Review Feedback

```
Problem: PR has requested changes - "Extract this logic into a helper function"

Resolution:
1. gh pr view --comments → read reviewer's comment
2. Read the file mentioned in comment
3. Extract logic into new helper function
4. Update calling code to use helper
5. Repo formatting and test gates pass
6. git add . && git commit -m "refactor(handlers): extract validation logic to helper"
7. git push
8. gh pr comment --body "Extracted as requested. PTAL!"

Result: ✓ Review feedback addressed
```

---

## Integration with Repair Plugin

This skill is invoked by the repair plugin via:

```go
StartChatRun(ctx, &StartChatRunHostRequest{
    SessionId: sessionID,
    Command:   "boss-repair",
    Title:     "Repair: " + sessionName,
})
```

The plugin:

- Detects red status changes (Failing/Conflict/Rejected)
- Applies exponential backoff between attempts, persisted so a daemon restart
  cannot reset the schedule
- Prevents concurrent repairs for the same session (the guard is held across the
  whole wait-and-loop, not just a single run)
- After each run, waits for the PR checks to settle and loops — up to a bounded
  number of attempts — until the PR is clean, the failing signal stops changing,
  or the wait times out
- Calls `FireSessionEvent(FixComplete)` on success

This skill should:

- Focus on fixing the immediate problem
- Complete within a reasonable time (< 5 minutes typical)
- Exit zero once a pass completes, with any residual reported in the Repair Summary, and non-zero
  only on a true stop — see [Residuals vs true stops](#residuals-vs-true-stops)
- Provide actionable feedback via PR comments if unable to fix

---

## Watch Mode

**Manual `/boss-repair watch` only.** This mode is active **only** when the skill is invoked with the `watch` argument. The repair plugin always invokes `/boss-repair` with no arguments, so it never enters watch mode; default mode still waits for checks after its pushed fix, but the plugin owns additional repair attempts.

In default mode (no `watch` argument) you MUST behave exactly as Phases 1–3 describe: apply one repair pass, push, poll the resulting PR state once, report the result, and exit so the repair plugin can decide whether to retry. **Do not** start an additional repair loop in default mode. Default mode may exit while checks are pending, but it must not exit while known unresolved review feedback still needs repair.

In watch mode, after completing one normal repair pass (Phases 1–2) and pushing, do **not** exit. Instead poll the PR state directly, bounded to **5 repair passes total** (matching the plugin's own limit):

Each of these repair passes dispatches its own fresh awaited subagent (per the Phase 2 lead-in) to run the matching strategy's investigate-and-fix steps; the thin orchestrator running this loop only tracks the pass counter, the `$BEFORE` baseline commit, and poll state between passes — it does not carry a prior pass's diffs/logs/threads forward. This is internal bookkeeping only: it does not alter the default-mode contract described in Phase 3 §4, and the 5-pass bound plus the final reason line below stay byte-identical.

1. Record the current commit before each repair pass, and refresh this baseline after every pushed repair before returning to the poll loop:

   ```bash
   BEFORE=$(git rev-parse HEAD)
   ```

2. Poll all repair signals before every sleep:

   ```bash
   gh pr checks --json bucket
   node scripts/review-feedback-probe.js
   gh pr view --json mergeable -q .mergeable
   ```

3. Interpret the full PR state:

   - **Checks:** `gh pr checks --json bucket` — all checks pass only when every bucket is passing/successful and none are pending, skipped-required, cancelled, timed out, or failed.
   - **Review threads:** `node scripts/review-feedback-probe.js` — trust `repair_status` (`clean`, `parked`, `needs_repair`, `unknown`) using the same interpretation rules as Strategy C above.
   - **Conflicts:** `gh pr view --json mergeable -q .mergeable` — `CONFLICTING` means a merge conflict appeared.

4. **Review work first:** if `repair_status=needs_repair`, repair every printed unresolved thread immediately. For each valid comment, fix it, reply with what changed, resolve the parent review thread, push, then start the next poll. For each invalid, stale, or already-handled comment, reply explaining why it is declined, resolve the parent review thread, push if the reply changed local state, then start the next poll. Only true clarification requests may remain unresolved. If `repair_status=parked`, no action is due until a reviewer replies. If `repair_status=unknown`, retry with explicit repo/pr and do not treat reviews as clean.

5. **Conflicts next:** if mergeable is `CONFLICTING`, repair the conflict, commit, push, then start the next poll.

6. **Pending checks:** if checks are pending, sleep 30–60 seconds, then poll checks, review threads, and mergeability again. Do not wait on checks without probing reviews and mergeability first.

7. **Failed checks:** if checks failed, run the matching repair strategy from Phase 2 for the new failure, push, then start the next poll.

8. **Done — green or parked:** the loop has reached a non-repair terminal state only when checks pass AND (`repair_status=clean` **or** `repair_status=parked`) AND mergeable is not `CONFLICTING` AND all fixed or declined review threads are resolved. Once all four hold, stop and exit zero.

9. **No-progress stop:** after a repair pass, if `git rev-parse HEAD` equals the `$BEFORE` value captured immediately before that pass (no new commit was pushed) and the failing signal is unchanged, stop and report — do not spin on an unfixable failure. This mirrors the plugin's duplicate-input guard.

10. **Bound:** never exceed 5 repair passes. After the 5th, report the remaining failures and exit.

11. **Ending short of green is a residual, not a true stop:** the no-progress stop, the 5-pass bound, and any escalation out of [Edge Cases and Error Handling](#edge-cases-and-error-handling) all end a loop that ran fine and left something behind. Record what is still red in the Repair Summary's `**Residuals**` line and **exit zero**; watch mode exits non-zero only on a true stop, per [Residuals vs true stops](#residuals-vs-true-stops).

When watch mode exits (green, parked, no-progress, 5 attempts reached, or escalated to a human per an edge case), print the standard Repair Summary plus a final line stating why the loop ended (`green` / `parked` / `no-progress` / `max-attempts` / `blocked`). Use `blocked` for an edge-case escalation: it is the established token for "this needs a human", and callers that classify this line already understand it.

---

## Checklist

Before completing the repair:

- [ ] Problem identified and categorized
- [ ] Appropriate repair strategy executed
- [ ] Local formatting and test gates passed
- [ ] Changes committed with descriptive message
- [ ] Base sync was a rebase; `git rev-list --merges --count "origin/$BASE_BRANCH"..HEAD` is `0`
- [ ] Changes pushed to origin; post-push waiting is delegated to the repair plugin
- [ ] Summary provided with actions taken
- [ ] Residuals reported in the summary, or explicitly recorded as `none`

A pass that ends on a residual without changing the branch skips the commit/push/gate boxes above —
there is nothing to commit — and is complete once the residual is reported.

---

## Success Criteria

"Successful" here means the **run** did its job, not that the PR came out green. A pass that ends on
a reported residual is still a successful run — see [Residuals vs true stops](#residuals-vs-true-stops).
Criteria 2–4 apply only when the pass produced a change; a pass that ends on a residual without
touching the branch has nothing to gate, commit, or push and is judged on 1, 5, and 6 alone. One
repair run is successful when:

1. ✅ The immediate issue was identified and fixed, or the reason it could not be is recorded as a residual
2. ✅ Local formatting and test gates passed
3. ✅ Changes were committed with a descriptive message
4. ✅ Changes were pushed to the PR branch
5. ✅ Review threads touched by this repair were resolved, declined with explanation, or replied to with a clarification question
6. ✅ The run exits cleanly so the repair plugin can wait for GitHub checks/reviews/conflicts to settle

The entire repair workflow is successful only when the repair plugin observes:

1. ✅ Checks have finished and are passing
2. ✅ No merge conflict remains
3. ✅ No unresolved actionable review feedback remains

Do not treat pending checks as final success. In **default mode**, pending checks mean the agent run should exit cleanly after pushing, and the repair plugin will wait; if checks fail, new review feedback appears, or a conflict appears, the plugin will start a fresh `boss-repair` run. In **Watch Mode** (`/boss-repair watch`), the skill itself waits and loops as described in [Watch Mode](#watch-mode) instead of exiting on pending checks.

### Post-terminal notes extensions (repo opt-in)

After the terminal outcome is decided, resolve the extension helper and run:

```bash
BOSS_REPAIR_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/toolbox"
if [ ! -d "$BOSS_REPAIR_TOOLBOX" ]; then BOSS_REPAIR_TOOLBOX="$HOME/.codex/skills/boss-repair/toolbox"; fi
NOTES_JSON=$(node "$BOSS_REPAIR_TOOLBOX/skill-extensions.mjs" discover --core boss-repair --role notes --json)
```

If `NOTES_JSON.extensions` is empty, do nothing and print nothing: a repo without a local notes
extension has not opted in. Create no scratch in that case. Otherwise create a terminal-only handoff:

```bash
NOTES_RUN_TMP=$(mktemp -d "${TMPDIR:-/tmp}/boss-repair-notes.XXXXXX")
NOTES_OBSERVATIONS="$NOTES_RUN_TMP/observations.md"
```

Before dispatch, the orchestrator that still owns the completed run writes at most five
secret-scrubbed candidate observations to `NOTES_OBSERVATIONS`, with a maximum 8 KiB file size.
Keep each candidate to a short problem statement plus a file/skill/command pointer. Never copy a
transcript, command output, user-provided content, credentials, tokens, or other secrets; an empty
file is valid. This artifact is the only run-history source sent across the fresh-subagent boundary.

Dispatch descriptors in ascending `(order, name)` order as fresh, **awaited** subagents, each bounded
by `BOSS_SKILL_EXTENSION_TIMEOUT_MS` (default `300000` ms). Load each extension by **reading the
descriptor's `skillPath` from disk** (`dir` is its directory), passing both `skillPath` and `dir` in
the worker brief, and requiring relative extension resources to resolve from `dir`. Pass that `SKILL.md`
content into the dispatch as the extension's instructions — never by its bare descriptor `name` via the
Skill tool, which refuses a skill declaring `disable-model-invocation: true`.
Each receives:

```json
{
  "role": "notes",
  "core": "boss-repair",
  "context": {
    "mode": "<interactive if this run involved operator interaction; otherwise headless>",
    "core": "boss-repair",
    "outcome": "<resolved terminal outcome>",
    "repoId": "<BOSS_REPO_ID when present; otherwise null>",
    "observationPath": "<NOTES_OBSERVATIONS>"
  },
  "runTmp": "<NOTES_RUN_TMP>",
  "outPath": "<NOTES_RUN_TMP>/notes-<extension-name>.json"
}
```

Validate each result with `node "$BOSS_REPAIR_TOOLBOX/skill-extensions.mjs" validate --role notes --file
"<outPath>"`. On success append one terminal-ledger line with the total persisted-note count. On a
discovery skip, timeout, missing output, malformed envelope, validation failure, or subagent failure,
append `extension <name>: skipped (<reason>)` and continue. Remove `NOTES_RUN_TMP` on every
post-opt-in terminal path. The phase cannot change the outcome, exit code, tracker or PR writes, and
is non-fatal in every case.
