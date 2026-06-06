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

## Repair Workflow

### Phase 1: Assess Current State

**1.1 Check PR Status**

```bash
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

#### Strategy A: Merge Conflicts

**Symptoms**: Git reports conflicts, PR status shows conflict

**Resolution**:

1. Fetch and merge the PR base branch:

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
     [ -n "$BASE_BRANCH" ] && echo "Using inferred git base branch: $BASE_BRANCH"
   fi
   test -n "$BASE_BRANCH" || { echo "Could not determine PR base branch"; exit 1; }
   git fetch origin "$BASE_BRANCH"
   git merge "origin/$BASE_BRANCH"
   ```

2. Identify conflicting files:

   ```bash
   git diff --name-only --diff-filter=U
   ```

3. For each conflicting file:
   - Read the file to see conflict markers (`<<<<<<<`, `=======`, `>>>>>>>`)
   - Understand both versions (ours vs theirs)
   - Resolve by:
     - Keeping both changes if they're independent
     - Choosing the correct version if they conflict
     - Merging logic intelligently if needed
   - Use Edit tool to remove conflict markers and apply resolution

4. Test the resolution with the repo's formatting and test gates:

   ```bash
   # Examples only; use the commands discovered for this repo
   pnpm lint && pnpm test
   go test ./...
   cargo test
   ```

5. Commit the resolution:

   ```bash
   git add <resolved-files>
   git commit -m "$(cat <<'EOF'
   fix(merge): resolve conflicts with base branch

   Resolved merge conflicts by [brief description of strategy].

   Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>
   EOF
   )"
   ```

6. Push the resolution:
   ```bash
   git push
   ```

#### Strategy B: Failing Checks

**Symptoms**: PR checks show failures (tests, lint, build errors)

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
   - `repair_status=unknown`: REST and GraphQL disagree or thread state is unavailable. Retry with explicit repo/pr before concluding there is no review feedback.

2. For each unresolved thread, triage into one of three categories:

   **a) Actionable — fix it:**
   - Read the relevant code/files
   - Implement the requested change
   - Add a reply comment on the thread explaining what was fixed:
     ```bash
     gh api repos/OWNER/REPO/pulls/PR_NUM/comments/COMMENT_ID/replies -f body="Fixed: [brief explanation of what was changed]"
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
     ```bash
     gh api repos/OWNER/REPO/pulls/PR_NUM/comments/COMMENT_ID/replies -f body="Not fixing: [explanation — e.g. 'This duplication is by design to avoid a dependency from the plugin binary to the host config package' or 'This was already fixed in a subsequent commit']"
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
     ```bash
     gh api repos/OWNER/REPO/pulls/PR_NUM/comments/COMMENT_ID/replies -f body="Could you clarify what you meant by [...]?"
     ```
   - Do NOT resolve the thread — leave it open for the reviewer.

   **IMPORTANT**: Every unresolved thread must be handled. Do not silently skip threads. Either fix and resolve, decline and resolve with an explanation, or ask for clarification. Fixed or declined threads must be resolved before the PR is considered clean. Only true clarification requests may remain unresolved.

3. After implementing changes:
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

     Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>
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
```

---

## Edge Cases and Error Handling

### Complex Conflicts

If conflicts are too complex to resolve automatically:

1. Add a PR comment explaining the situation:

   ```bash
   gh pr comment --body "Automatic repair detected complex merge conflicts that require manual review. Files affected: <list>. Please resolve manually."
   ```

2. Exit with failure status so the cooldown applies

### Cascading Failures

If fixing one issue causes another (e.g., fixing a conflict causes tests to fail):

1. Continue with the next repair strategy
2. Don't give up after the first fix
3. Iterate through strategies until stable

### Missing Context

If the repair requires information not available (e.g., design decisions, external dependencies):

1. Add a PR comment requesting clarification
2. Do NOT make assumptions
3. Exit with failure status

---

## Guidelines

1. **Root Cause Over Symptoms**: Fix the underlying issue, not just the visible error
2. **Minimal Changes**: Only change what's necessary to resolve the issue
3. **Test Locally**: Always run the repo's formatting and test gates before pushing
4. **Clear Commits**: Write descriptive commit messages that explain the fix
5. **Atomic Repairs**: Each repair attempt should be self-contained
6. **Fail Fast**: If unable to fix, exit quickly to avoid wasting time

---

## Anti-Patterns

| Anti-Pattern                             | Problem                             | Fix                               |
| ---------------------------------------- | ----------------------------------- | --------------------------------- |
| Accepting all "ours" or "theirs" blindly | Loses important changes             | Review each conflict individually |
| Skipping tests after conflict resolution | Introduces bugs                     | Always run full test suite        |
| Commenting out failing tests             | Hides problems                      | Fix the root cause                |
| Force pushing                            | Loses history, breaks collaboration | Normal push only                  |
| Making unrelated "improvements"          | Scope creep                         | Fix only the reported issue       |
| Retrying immediately after failure       | Triggers cooldown loops             | Fix the root cause first          |

---

## Example Scenarios

### Scenario 1: Simple Merge Conflict

```
Problem: PR shows conflict status, git reports conflicts in server.go

Resolution:
1. Fetch and merge the PR base branch
2. Read server.go, see conflict in import statements
3. Keep both imports (they're independent)
4. Run repo formatting and test gates (passes)
5. git add server.go && git commit -m "fix(merge): resolve import conflicts"
6. git push

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
- Exit with clear success/failure status
- Provide actionable feedback via PR comments if unable to fix

---

## Watch Mode

**Manual `/boss-repair watch` only.** This mode is active **only** when the skill is invoked with the `watch` argument. The repair plugin always invokes `/boss-repair` with no arguments, so it never enters watch mode; default mode still waits for checks after its pushed fix, but the plugin owns additional repair attempts.

In default mode (no `watch` argument) you MUST behave exactly as Phases 1–3 describe: apply one repair pass, push, poll the resulting PR state once, report the result, and exit so the repair plugin can decide whether to retry. **Do not** start an additional repair loop in default mode. Default mode may exit while checks are pending, but it must not exit while known unresolved review feedback still needs repair.

In watch mode, after completing one normal repair pass (Phases 1–2) and pushing, do **not** exit. Instead poll the PR state directly, bounded to **5 repair passes total** (matching the plugin's own limit):

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
   - **Review threads:** `node scripts/review-feedback-probe.js` — trust `repair_status` (`clean`, `needs_repair`, `unknown`) using the same interpretation rules as Strategy C above.
   - **Conflicts:** `gh pr view --json mergeable -q .mergeable` — `CONFLICTING` means a merge conflict appeared.

4. **Review work first:** if `repair_status=needs_repair`, repair every printed unresolved thread immediately. For each valid comment, fix it, reply with what changed, resolve the parent review thread, push, then start the next poll. For each invalid, stale, or already-handled comment, reply explaining why it is declined, resolve the parent review thread, push if the reply changed local state, then start the next poll. Only true clarification requests may remain unresolved. If `repair_status=unknown`, retry with explicit repo/pr and do not treat reviews as clean.

5. **Conflicts next:** if mergeable is `CONFLICTING`, repair the conflict, commit, push, then start the next poll.

6. **Pending checks:** if checks are pending, sleep 30–60 seconds, then poll checks, review threads, and mergeability again. Do not wait on checks without probing reviews and mergeability first.

7. **Failed checks:** if checks failed, run the matching repair strategy from Phase 2 for the new failure, push, then start the next poll.

8. **Done:** exit success only when checks pass AND `repair_status=clean` AND mergeable is not `CONFLICTING` AND all fixed or declined review threads are resolved.

9. **No-progress stop:** after a repair pass, if `git rev-parse HEAD` equals the `$BEFORE` value captured immediately before that pass (no new commit was pushed) and the failing signal is unchanged, stop and report — do not spin on an unfixable failure. This mirrors the plugin's duplicate-input guard.

10. **Bound:** never exceed 5 repair passes. After the 5th, report the remaining failures and exit.

When watch mode exits (green, no-progress, or 5 attempts reached), print the standard Repair Summary plus a final line stating why the loop ended (`green` / `no-progress` / `max-attempts`).

---

## Checklist

Before completing the repair:

- [ ] Problem identified and categorized
- [ ] Appropriate repair strategy executed
- [ ] Local formatting and test gates passed
- [ ] Changes committed with descriptive message
- [ ] Changes pushed to origin; post-push waiting is delegated to the repair plugin
- [ ] Summary provided with actions taken

---

## Success Criteria

One repair run is successful when:

1. ✅ The immediate issue was identified and fixed
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
