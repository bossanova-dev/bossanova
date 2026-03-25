---
name: boss-finalize
description: End-of-session workflow ensuring all work is committed and pushed. Use when ending a work session or when asked to "land the plane".
---

# Land the Plane: Session Completion Workflow

"Landing the plane" is the mandatory end-of-session process ensuring all work is committed and pushed to remote. Work is NOT complete until `git push` succeeds.

---

## ⛔ BLOCKING REQUIREMENTS - READ FIRST ⛔

**You MUST satisfy ALL of these before completing. No exceptions.**

| #   | Requirement                     | How to Verify                                                                                                                                                                                           |
| --- | ------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1   | **All quality gates pass**      | Run `make` (full build), then `make lint` and `make test`. ALL must pass. Fix failures — do NOT dismiss them as "pre-existing" without verifying on origin/main.                                        |
| 2   | **PR number in ALL commits**    | Every commit on this branch (compared to origin/main) MUST have `[#PR-NUM]` in the message. Check with `git log origin/main..HEAD --oneline`. If ANY commit is missing it, you MUST run the fix script. |
| 3   | **Commits squashed and tidied** | You MUST squash commits into logical groups and force-push. Do NOT ask for permission — just do it.                                                                                                     |
| 4   | **GitHub checks not failing**   | After pushing, run `gh pr checks` to verify. Checks may be idle, queued, in_progress, or passing. Any **failing/red** check MUST be investigated and fixed before the session is complete.              |
| 5   | **PR marked Ready for Review**  | After all checks pass or are non-blocking, run `gh pr ready` to mark the PR as ready for review. Do NOT leave the PR as a draft.                                                                        |
| 6   | **No merge conflicts**          | Check GitHub for merge conflicts with `gh pr view --json mergeable -q .mergeable`. If `CONFLICTING`, rebase onto main and resolve conflicts before completing.                                          |

**If you complete without satisfying ALL SIX requirements, you have failed this workflow.**

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
- **No commits on branch vs main?** → Skip to Step 8

### Step 2: File Remaining Work

Create beads issues documenting any incomplete tasks requiring follow-up:

```bash
bd create --title="Follow-up: ..." --type=task --priority=2
```

### Step 3: Run Quality Gates

**This step is NON-NEGOTIABLE. You MUST run ALL quality gates and they MUST pass.**

#### Step 3a: Full Build (includes generate + format)

The default `make` target runs `clean → generate → format → build` in the correct order. This ensures generated code (protobuf, etc.) exists before anything else runs.

```bash
make
```

**If `make` fails due to missing dependencies** (e.g., `node_modules missing`, `protoc-gen-es: no such file`), you MUST install them first:

```bash
cd services/web && pnpm install && cd ../..
make
```

**Do NOT skip `make` and jump straight to individual targets.** The `generate` step creates code that `lint`, `test`, and `build` all depend on. Without it, everything downstream fails.

#### Step 3b: Lint and Test

After `make` succeeds, run lint and test:

```bash
make lint     # Lint all modules (golangci-lint + buf lint)
make test     # Run tests across all modules
```

**If format changed files:** Stage the formatting fixes and include them in your commit.

**If any gate fails:** Fix the issues, stage the fixes, and re-run until all pass. Do NOT skip a failing gate. Do NOT proceed to commit until all gates are green.

#### ⛔ "Pre-existing" failures — verify before dismissing

**Do NOT assume a failure is pre-existing.** A failure is only pre-existing if it also fails on `origin/main`. Before dismissing any failure:

1. Check if it's a missing prerequisite (generated code, dependencies) — if so, fix it
2. If you believe it's truly pre-existing, verify by checking CI on main or running the same command on main
3. Only after verification can you note it and proceed — and you MUST inform the user explicitly

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
.claude/skills/boss-finalize/add-pr-numbers.sh
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

- **Drop empty "create pull request" commits** — these are scaffolding commits (e.g., `chore: [skip ci] create pull request`) with no code changes. Use `drop` in `git rebase -i` to remove them entirely.
- Group commits by logical unit of work (e.g., one commit per service/feature area)
- Squash fix-up commits, lint fixes, and review feedback into their parent commits
- Combine related changes (feature + tests + fixes = one commit)
- Keep genuinely unrelated work in separate commits
- Each final commit should represent a coherent, self-contained change
- Use `git rebase -i` with `fixup` to squash, and `reword` to clean up messages
- Always use `--force-with-lease` when force-pushing after rebase

**Determine the logical grouping, then squash and force-push immediately. Do NOT ask for permission — just do it.**

**After squashing, verify:**

```bash
git log origin/main..HEAD --oneline          # Clean, logical commits
git log origin/main..HEAD --oneline | grep -v "\[#"  # All have PR numbers
```

### Step 7: Push to Remote

```bash
bd sync
git pull --rebase
git push
git status  # Verify "up to date with origin"
```

If push fails, resolve and retry until success.

### Step 7b: Verify GitHub Checks

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

### Step 7c: Mark PR as Ready for Review

**This step is NON-NEGOTIABLE. You MUST mark the PR as ready for review.**

After checks are passing (or pending/in_progress), mark the PR as ready:

```bash
gh pr ready
```

This converts the PR from draft to ready-for-review status. Do NOT leave the PR as a draft when landing the plane.

**If the PR is already ready for review**, this command is a no-op and safe to run.

### Step 7d: Check for Merge Conflicts

**This step is NON-NEGOTIABLE. You MUST verify there are no merge conflicts.**

```bash
gh pr view --json mergeable -q .mergeable
```

**Expected result:** `MERGEABLE` — the PR can be merged cleanly.

**If the result is `CONFLICTING`:**

1. Fetch the latest main: `git fetch origin main`
2. Rebase onto main: `git rebase origin/main`
3. Resolve any conflicts during the rebase
4. Re-run quality gates (`make`, `make lint`, `make test`) to ensure nothing broke
5. Force-push: `git push --force-with-lease`
6. Wait and re-check: `gh pr view --json mergeable -q .mergeable`
7. Repeat until `MERGEABLE`

**If the result is `UNKNOWN`:** GitHub is still computing mergeability. Wait a few seconds and re-check.

**Do NOT leave the session with merge conflicts.** A PR with conflicts cannot be merged and blocks the review process.

### Step 8: Clean Up and Verify

```bash
git stash list        # Note any stashes (don't auto-clear without asking)
git remote prune origin
git status            # Confirm clean state
```

### Step 9: Provide Handoff

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

- [ ] Full build passed (`make` — includes clean, generate, format, build)
- [ ] `make lint` passed
- [ ] `make test` passed
- [ ] All commits have `[#PR-NUM]` in message
- [ ] Commits squashed into logical groups (force-pushed)
- [ ] Empty "create pull request" commits dropped
- [ ] `git push` succeeded
- [ ] GitHub checks are not failing (idle/queued/in_progress/passing are OK)
- [ ] PR marked as ready for review (`gh pr ready`)
- [ ] No merge conflicts (`gh pr view --json mergeable -q .mergeable` → `MERGEABLE`)
- [ ] Provided handoff with next steps

---

## Common Failures

| Failure                              | Why It's Wrong         | What You Should Have Done                                                                            |
| ------------------------------------ | ---------------------- | ---------------------------------------------------------------------------------------------------- |
| Skipped `make` (full build)          | Generated code missing | Run `make` first — it does clean, generate, format, build in order. Then `make lint` and `make test` |
| Skipped quality gates                | CI will fail           | Run ALL gates: `make`, `make lint`, `make test`                                                      |
| Dismissed failure as "pre-existing"  | Failure was fixable    | Verify on origin/main before dismissing. Missing generated code is NOT pre-existing — run `make`     |
| Missing dependencies in worktree     | Generate/format fails  | Run `cd services/web && pnpm install` before `make` if node_modules is missing                       |
| Stopped to ask permission to push    | Blocked automation     | Just push — do NOT ask for permission. Force-push is expected and authorized.                        |
| Commit missing `[#PR-NUM]`           | PR not linked          | Run `.claude/skills/boss-finalize/add-pr-numbers.sh` to fix ALL commits                              |
| Reported issue but didn't fix        | Commits still broken   | You MUST run the script, not just report that commits need fixing                                    |
| Used `origin/HEAD` not `origin/main` | Wrong comparison       | Always compare to `origin/main` to find all branch commits                                           |
| Branch "up to date" so skipped       | Commits still need PR# | Even pushed commits need PR numbers - compare to main, not feature branch                            |
| Didn't squash commits                | Messy history          | ALWAYS squash into logical groups — this is mandatory, not optional                                  |
| Said "ready when you are"            | Work stranded          | YOU push immediately — do not wait for user to do it or ask permission                               |
| Left session with failing checks     | CI is red              | Run `gh pr checks`, investigate failures with `gh run view --log-failed`, fix and re-push            |
| Ignored failing check as "unrelated" | CI still red           | Even if unrelated, inform user and get explicit acknowledgment                                       |
| Left empty "create PR" commit        | Messy history          | Use `drop` in rebase to remove empty scaffolding commits like `chore: [skip ci] create pull request` |
| Left PR as draft                     | Not reviewable         | Run `gh pr ready` to mark the PR as ready for review before completing                               |
| Left PR with merge conflicts         | PR can't be merged     | Run `gh pr view --json mergeable -q .mergeable`, rebase onto main if `CONFLICTING`                   |

---

## Related Skills

| Skill                | Relationship                            |
| -------------------- | --------------------------------------- |
| `/boss-plan`         | Create plan before implementation       |
| `/boss-create-tasks` | Create bd tasks from plan               |
| `/boss-verify`       | Verify flight leg before handoff        |
| `/boss-implement`    | Execute tasks, stopping at handoffs     |
| `/boss-handoff`      | Create handoff documents at checkpoints |
| `/boss-resume`       | Resume work from a previous handoff     |
