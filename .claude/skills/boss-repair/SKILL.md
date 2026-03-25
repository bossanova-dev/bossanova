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

This skill is NOT manually invoked by users — it runs automatically via the repair plugin.

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

### Phase 2: Execute Repair Strategy

#### Strategy A: Merge Conflicts

**Symptoms**: Git reports conflicts, PR status shows conflict

**Resolution**:

1. Fetch and merge base branch:

   ```bash
   git fetch origin main
   git merge origin/main
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

4. Test the resolution:

   ```bash
   make format && make test
   ```

5. Commit the resolution:

   ```bash
   git add <resolved-files>
   git commit -m "$(cat <<'EOF'
   fix(merge): resolve conflicts with main branch

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
   gh pr checks
   ```

2. Get failure details:

   ```bash
   gh pr checks --watch     # If checks are still running
   gh run view <run-id>     # View specific check run details
   ```

3. For **test failures**:
   - Read test output to identify failing tests
   - Read the test file and implementation
   - Fix the root cause (not just the symptom)
   - Run tests locally to verify:
     ```bash
     make test
     ```
   - Commit the fix:
     ```bash
     git add <fixed-files>
     git commit -m "fix(tests): resolve failing test in <component>"
     git push
     ```

4. For **lint/format failures**:
   - Run formatting:
     ```bash
     make format
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
   - Verify locally:
     ```bash
     make build
     ```
   - Commit and push the fix

#### Strategy C: Review Feedback

**Symptoms**: PR has requested changes or review comments

**Resolution**:

1. List all unresolved review threads:

   ```bash
   gh api graphql -f query='
   {
     repository(owner: "OWNER", name: "REPO") {
       pullRequest(number: PR_NUM) {
         reviewThreads(first: 50) {
           nodes {
             id
             isResolved
             comments(first: 5) {
               nodes {
                 body
                 path
                 author { login }
               }
             }
           }
         }
       }
     }
   }'
   ```

2. For each unresolved thread, triage into one of three categories:

   **a) Actionable — fix it:**
   - Read the relevant code/files
   - Implement the requested change
   - After fixing, resolve the thread with an explanation:
     ```bash
     gh api graphql -f query='
       mutation {
         resolveReviewThread(input: {threadId: "THREAD_ID"}) {
           thread { isResolved }
         }
       }'
     ```
   - Add a reply comment on the thread explaining what was fixed:
     ```bash
     gh api repos/OWNER/REPO/pulls/PR_NUM/comments -f body="Fixed: [brief explanation of what was changed]" -f in_reply_to_id=COMMENT_ID
     ```

   **b) Not actionable — decline and resolve:**
   Some review comments are by design, already fixed, stale (reference old code), or low-priority style suggestions. For these:
   - Add a reply comment explaining why it won't be fixed:
     ```bash
     gh api repos/OWNER/REPO/pulls/PR_NUM/comments -f body="Not fixing: [explanation — e.g. 'This duplication is by design to avoid a dependency from the plugin binary to the host config package' or 'This was already fixed in a subsequent commit']" -f in_reply_to_id=COMMENT_ID
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
     gh api repos/OWNER/REPO/pulls/PR_NUM/comments -f body="Could you clarify what you meant by [...]?" -f in_reply_to_id=COMMENT_ID
     ```
   - Do NOT resolve the thread — leave it open for the reviewer.

   **IMPORTANT**: Every unresolved thread must be handled. Do not silently skip threads. Either fix and resolve, decline and resolve with an explanation, or ask for clarification.

3. After implementing changes:
   - Run tests and formatting:
     ```bash
     make format && make test
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

2. Verify PR status has improved:

   ```bash
   gh pr view
   ```

3. If checks are pending, note that in output:
   ```
   ✓ Repair applied and pushed
   ⏳ Waiting for checks to complete
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
3. **Test Locally**: Always run `make format && make test` before pushing
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
1. git fetch origin main && git merge origin/main
2. Read server.go, see conflict in import statements
3. Keep both imports (they're independent)
4. make format && make test (passes)
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
6. make test → passes
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
5. make format && make test → passes
6. git add . && git commit -m "refactor(handlers): extract validation logic to helper"
7. git push
8. gh pr comment --body "Extracted as requested. PTAL!"

Result: ✓ Review feedback addressed
```

---

## Integration with Repair Plugin

This skill is invoked by the repair plugin via:

```go
CreateAttempt(ctx, &CreateAttemptRequest{
    WorkflowId: workflowID,
    SkillName:  "boss-repair",
    Input:      "/boss-repair",
    WorkDir:    sessionWorkDir,
})
```

The plugin:

- Detects red status changes (Failing/Conflict/Rejected)
- Enforces 5-minute cooldown between attempts
- Prevents concurrent repairs for the same session
- Calls `FireSessionEvent(FixComplete)` on success

This skill should:

- Focus on fixing the immediate problem
- Complete within a reasonable time (< 5 minutes typical)
- Exit with clear success/failure status
- Provide actionable feedback via PR comments if unable to fix

---

## Checklist

Before completing the repair:

- [ ] Problem identified and categorized
- [ ] Appropriate repair strategy executed
- [ ] Local tests passed (`make format && make test`)
- [ ] Changes committed with descriptive message
- [ ] Changes pushed to origin
- [ ] PR status verified (improved or checks pending)
- [ ] Summary provided with actions taken

---

## Success Criteria

The repair is successful when:

1. ✅ Changes are pushed to the PR branch
2. ✅ No conflicts remain (`git status` is clean)
3. ✅ Local tests pass (`make test` succeeds)
4. ✅ PR checks are passing or pending (not failing)
5. ✅ Review feedback addressed (if applicable)
6. ✅ All review threads resolved — either fixed, declined with explanation, or asked for clarification (no silently skipped threads)

If ANY criterion fails, the repair attempt failed and should exit with error status (triggering cooldown).
