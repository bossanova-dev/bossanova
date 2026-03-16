---
name: land-the-plane
description: End-of-session workflow ensuring all work is committed and pushed. Use when ending a work session or when asked to "land the plane".
---

# Land the Plane: Session Completion Workflow

"Landing the plane" is the mandatory end-of-session process ensuring all work is committed and pushed to remote. Work is NOT complete until `git push` succeeds.

---

## ⛔ BLOCKING REQUIREMENTS - READ FIRST ⛔

**You MUST satisfy ALL of these before completing. No exceptions.**

| #   | Requirement                                   | How to Verify                                                                                                                                                                                           |
| --- | --------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1   | **Format and lint pass for ALL changed code** | Run `git diff --name-only origin/main..HEAD` to find changed areas, then run `make format` and `make lint`. Fix any failures before proceeding.                                                         |
| 2   | **PR number in ALL commits**                  | Every commit on this branch (compared to origin/main) MUST have `[#PR-NUM]` in the message. Check with `git log origin/main..HEAD --oneline`. If ANY commit is missing it, you MUST run the fix script. |
| 3   | **User approval obtained**                    | You MUST ask the user "Do I have permission to push?" and WAIT for their explicit "yes" before ANY push or force-push.                                                                                  |
| 4   | **Commits squashed and tidied**               | You MUST squash commits into logical groups and force-push. Show the user the proposed grouping for approval.                                                                                           |
| 5   | **GitHub checks not failing**                 | After pushing, run `gh pr checks` to verify. Checks may be idle, queued, in_progress, or passing. Any **failing/red** check MUST be investigated and fixed before the session is complete.              |

**If you complete without satisfying ALL FIVE requirements, you have failed this workflow.**

---

## Workflow Steps

### Step 1: Assess Current State

Run these commands to understand what needs to be done:

```bash
git status                            # Uncommitted changes?
git log origin/main..HEAD --oneline   # ALL commits on this branch (vs main)
gh pr view --json number -q .number   # Get PR number
```

**IMPORTANT:** Always compare to `origin/main`, not `origin/HEAD`. This shows ALL commits on your branch that aren't in main, regardless of whether they're "pushed" to the feature branch.

**Determine your situation:**

- **Uncommitted changes exist?** → Go to Step 2
- **Commits exist on branch?** → Go to Step 5 (MUST check PR numbers!)
- **No commits on branch vs main?** → Skip to Step 9

### Step 2: File Remaining Work

Create beads issues documenting any incomplete tasks requiring follow-up:

```bash
bd create --title="Follow-up: ..." --type=task --priority=2
```

### Step 3: Run Quality Gates

**This step is NON-NEGOTIABLE. You MUST lint AND format all changed code.**

**Step 3a: Identify what has changed:**

```bash
# Find all files with changes on this branch
git diff --name-only origin/main..HEAD
```

**Step 3b: Run format:**

```bash
make format
```

**Step 3c: Run lint:**

```bash
make lint
```

**Step 3d: Fix any lint or format errors before proceeding.**

Do NOT skip this step. Do NOT proceed to commit if lint or format fails. Fix the errors first.

**If format changed files:** Stage the formatting fixes and include them in your commit.

**If lint/format fails:** Fix the issues, stage the fixes, and re-run until both pass.

### Step 4: Update Beads Issues

```bash
bd close <id1> <id2> ...                                  # Close finished work
bd update <id> --notes="Progress notes for next session"  # Update in-progress
```

### Step 5: Fix ALL Commits Missing PR Numbers

**This step is NON-NEGOTIABLE. You MUST fix commits, not just report on them.**

```bash
# Get the PR number
PR_NUM=$(gh pr view --json number -q .number 2>/dev/null || echo "UNKNOWN")
echo "PR number: $PR_NUM"

# Show ALL commits on this branch (compared to main)
git log origin/main..HEAD --oneline
```

**Check EVERY commit message for `[#PR-NUM]`:**

- ✅ Good: `feat(mobile): [#2137] add feature X`
- ❌ Bad: `feat(mobile): add feature X` (missing PR number!)

**⛔ If ANY commit is missing the PR number, you MUST run the fix script:**

```bash
# Run from repo root - automatically detects PR number
.claude/skills/land-the-plane/add-pr-numbers.sh
```

**DO NOT skip this step.** Even if the branch is "up to date with origin", the commits still need PR numbers. The script compares against origin/main, not the feature branch.

**After the script completes, force-push to update the branch:**

```bash
git push --force-with-lease
```

**If the rebase fails:** Reset with `git rebase --abort` or `git reset --hard origin/<branch-name>` and try again.

**Verify ALL commits now have PR numbers before proceeding:**

```bash
git log origin/main..HEAD --oneline | grep -v "\[#"
# Should return NOTHING - if it returns commits, they still need fixing
```

### Step 6: Squash and Tidy Commits

**This step is NON-NEGOTIABLE. You MUST squash commits into logical groups before pushing.**

```bash
git log origin/main..HEAD --oneline
```

**Squashing rules:**

- Group commits by logical unit of work (e.g., one commit per service/feature area)
- Squash fix-up commits, lint fixes, and review feedback into their parent commits
- Combine related changes (feature + tests + fixes = one commit)
- Keep genuinely unrelated work in separate commits
- Each final commit should represent a coherent, self-contained change
- Use `git rebase -i` with `fixup` to squash, and `reword` to clean up messages
- Always use `--force-with-lease` when force-pushing after rebase

**Show the user your proposed grouping:**

> "I'll squash these [N] commits into [M] logical commits:
>
> 1. `type(scope): [#PR] description` — [what it contains]
> 2. `type(scope): [#PR] description` — [what it contains]
>
> Does this grouping look right?"

**Wait for user approval before rebasing.**

**After squashing, verify:**

```bash
git log origin/main..HEAD --oneline          # Clean, logical commits
git log origin/main..HEAD --oneline | grep -v "\[#"  # All have PR numbers
```

### Step 7: Present Changes and Request Permission

**⛔ MANDATORY STOP - DO NOT SKIP ⛔**

Present a summary to the user:

```
## Changes Ready to Push

**PR:** #[PR-NUM]
**Branch:** [branch-name]

**Commits to push:**
- [commit 1 - with PR number]
- [commit 2 - with PR number]
- ...

**Files changed:** [count]

Please review. Do I have permission to push these commits?
```

**WAIT for explicit user approval before proceeding.**

Acceptable responses: "yes", "go ahead", "push it", "approved", etc.

**If user says no or asks for changes:** Make the requested changes and return to Step 5.

### Step 8: Push to Remote

Only after user approval:

```bash
bd sync
git pull --rebase
git push
git status  # Verify "up to date with origin"
```

If push fails, resolve and retry until success.

### Step 8b: Verify GitHub Checks

**This step is NON-NEGOTIABLE. You MUST verify checks are not failing.**

After pushing, wait a moment for checks to register, then run:

```bash
gh pr checks
```

**Acceptable statuses (session can complete):**

- ✅ **pass** — Check succeeded
- ⏳ **pending** / **queued** — Check hasn't started yet (OK, not a failure)
- 🔄 **in_progress** — Check is currently running (OK, not a failure)
- ⏸️ **idle** — Check is waiting to run (OK, not a failure)

**Blocking statuses (MUST be fixed before completing):**

- ❌ **fail** — Check has failed. You MUST investigate and fix it.

**If any check is failing:**

1. Run `gh pr checks` to identify which check(s) failed
2. Run `gh run view <run-id> --log-failed` to see the failure logs
3. Investigate the root cause and fix it locally
4. Commit the fix, push again, and re-check
5. Repeat until no checks are red/failing

**Do NOT leave the session with failing checks.** If a check failure is unrelated to your changes (e.g., a flaky test or pre-existing CI issue), you MUST inform the user and get their explicit acknowledgment before proceeding.

### Step 9: Clean Up and Verify

```bash
git stash list        # Note any stashes (don't auto-clear without asking)
git remote prune origin
git status            # Confirm clean state
```

### Step 10: Provide Handoff

Pull next steps from beads. If working on a flight plan, filter by flight label:

```bash
bd ready                                  # All ready tasks
bd ready --label "flight:fp-..."          # Ready tasks for specific flight (if applicable)
bd list --status=in_progress
bd list --status=open
```

**Provide a summary:**

> **Session Complete**
>
> **Completed:** [summary of work]
> **Flight ID:** [fp-... if working on a flight plan, otherwise N/A]
> **Issues filed:** [any new beads issues]
> **Quality gates:** [pass/fail status]
> **Push status:** All commits pushed to origin
>
> **Next steps (from bd ready):**
>
> - [task 1]
> - [task 2]
>
> **Recommended prompt:** "Continue work on [task]: [context]"
> **Resume command:** `bd ready --label "flight:fp-..."` (if applicable)

---

## Checklist

Before saying "done", verify ALL items:

- [ ] Format and lint passed for ALL changed code
- [ ] All commits have `[#PR-NUM]` in message
- [ ] Commits squashed into logical groups (force-pushed)
- [ ] User explicitly approved the push
- [ ] `git push` succeeded
- [ ] GitHub checks are not failing (idle/queued/in_progress/passing are OK)
- [ ] Provided handoff with next steps

---

## Common Failures

| Failure                              | Why It's Wrong         | What You Should Have Done                                                                 |
| ------------------------------------ | ---------------------- | ----------------------------------------------------------------------------------------- |
| Skipped format/lint for changed code | CI will fail           | Run `make format` and `make lint` before committing                                       |
| Pushed without asking                | User didn't approve    | ALWAYS ask "Do I have permission to push?" and WAIT                                       |
| Commit missing `[#PR-NUM]`           | PR not linked          | Run `.claude/skills/land-the-plane/add-pr-numbers.sh` to fix ALL commits                  |
| Reported issue but didn't fix        | Commits still broken   | You MUST run the script, not just report that commits need fixing                         |
| Used `origin/HEAD` not `origin/main` | Wrong comparison       | Always compare to `origin/main` to find all branch commits                                |
| Branch "up to date" so skipped       | Commits still need PR# | Even pushed commits need PR numbers - compare to main, not feature branch                 |
| Didn't squash commits                | Messy history          | ALWAYS squash into logical groups — this is mandatory, not optional                       |
| Said "ready when you are"            | Work stranded          | YOU push, don't wait for user to do it                                                    |
| Left session with failing checks     | CI is red              | Run `gh pr checks`, investigate failures with `gh run view --log-failed`, fix and re-push |
| Ignored failing check as "unrelated" | CI still red           | Even if unrelated, inform user and get explicit acknowledgment                            |

---

## Related Skills

| Skill                 | Relationship                            |
| --------------------- | --------------------------------------- |
| `/file-a-flight-plan` | Create plan before implementation       |
| `/pre-flight-checks`  | Create bd tasks from plan               |
| `/post-flight-checks` | Verify flight leg before handoff        |
| `/take-off`           | Execute tasks, stopping at handoffs     |
| `/handoff-task`       | Create handoff documents at checkpoints |
| `/resume-handoff`     | Resume work from a previous handoff     |
