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
| 1   | **All quality gates pass**      | Discover and run the repo's quality gates. Prefer a single project-declared aggregate command when it covers build/lint/test; otherwise run the minimal non-duplicative command set. ALL must pass. Fix failures — do NOT dismiss them as "pre-existing" without verifying on the PR base branch. |
| 2   | **PR number in ALL commits**    | Every commit on this branch (compared to the PR base branch) MUST have `[#PR-NUM]` in the message. Check with `git log origin/$BASE_BRANCH..HEAD --oneline`. If ANY commit is missing it, you MUST run the fix script. |
| 3   | **Commits squashed and tidied** | You MUST squash commits into logical groups and force-push. Do NOT ask for permission — just do it.                                                                                                     |
| 4   | **GitHub checks not failing**   | After pushing, run `gh pr checks` to verify. Checks may be idle, queued, in_progress, or passing. Any **failing/red** check MUST be investigated and fixed before the session is complete.              |
| 5   | **PR marked Ready for Review**  | After all checks pass or are non-blocking, run `gh pr ready` to mark the PR as ready for review. Do NOT leave the PR as a draft.                                                                        |
| 6   | **No merge conflicts**          | Check GitHub for merge conflicts with `gh pr view --json mergeable -q .mergeable`. If `CONFLICTING`, rebase onto the PR base branch and resolve conflicts before completing.                             |

**If you complete without satisfying ALL SIX requirements, you have failed this workflow.**

---

## Workflow Steps

### Step 1: Assess Current State

Run these commands to understand what needs to be done:

```bash
git status                            # Uncommitted changes?
BASE_BRANCH=$(gh pr view --json baseRefName -q .baseRefName 2>/dev/null || true)
if [ -z "$BASE_BRANCH" ]; then
  CURRENT_BRANCH=$(git branch --show-current)
  UPSTREAM_BRANCH=$(git rev-parse --abbrev-ref --symbolic-full-name @{u} 2>/dev/null | sed 's#^origin/##' || true)
  BASE_BRANCH=$(git for-each-ref --format='%(refname:short)' refs/remotes/origin | sed 's#^origin/##' | grep -vx HEAD | grep -vx "$CURRENT_BRANCH" | { if [ -n "$UPSTREAM_BRANCH" ]; then grep -vx "$UPSTREAM_BRANCH"; else cat; fi; } | while read -r branch; do base=$(git merge-base HEAD "origin/$branch" 2>/dev/null) || continue; printf '%s %s\n' "$(git show -s --format=%ct "$base")" "$branch"; done | sort -nr | awk 'NR == 1 {print $2}')
  [ -n "$BASE_BRANCH" ] && echo "Using inferred git base branch: $BASE_BRANCH"
fi
test -n "$BASE_BRANCH" || { echo "Could not determine PR base branch"; exit 1; }
git fetch origin "$BASE_BRANCH"
git log "origin/$BASE_BRANCH"..HEAD --oneline   # ALL commits on this branch (vs PR base)
gh pr view --json number -q .number   # Get PR number
```

**IMPORTANT:** Always compare to `origin/$BASE_BRANCH`, not the feature branch or default branch. If GitHub metadata is unavailable, the git fallback infers the most likely base from fetched `origin/*` branches; verify the printed branch before continuing. This shows ALL commits on your branch that aren't in the PR base branch, regardless of whether they're "pushed" to the feature branch.

**Determine your situation:**

- **Uncommitted changes exist?** → Go to Step 2
- **Commits exist on branch?** → Go to Step 4 (MUST check PR numbers!)
- **No commits on branch vs PR base?** → Skip to Step 7

### Step 2: Run Quality Gates

**This step is NON-NEGOTIABLE. You MUST run the repo's quality gates and they MUST pass.**

#### Step 2a: Discover the Gate Commands

Find the commands this repo expects contributors to run. Check these sources in order:

1. User/project instructions (`AGENTS.md`, `CLAUDE.md`, `README`, `CONTRIBUTING`, package docs)
2. CI workflows (`.github/workflows`, Buildkite, CircleCI, GitLab CI, etc.)
3. Project command files (`Makefile`, `justfile`, `Taskfile.yml`, `package.json`, `go.mod`, `Cargo.toml`, `pyproject.toml`, etc.)

Choose the smallest command set that covers the repo's required generate/build/lint/test checks without running the same check twice.

#### Step 2b: Run Project Gates

Prefer an explicit aggregate gate when present and complete:

```bash
make              # Only if Makefile exists and default target is the project gate
make check        # Common aggregate gate
make ci           # Common CI-equivalent gate
just check        # If justfile declares the project gate
task check        # If Taskfile declares the project gate
```

If the aggregate gate does not cover everything, run only the missing targets that exist. Examples:

```bash
make build
make lint
make test
```

For repos without a Makefile, use the native project commands. Examples:

```bash
pnpm lint && pnpm test
npm run lint && npm test
go test ./...
cargo test
pytest
```

**Do not assume `make` exists. Do not blindly run `make`, then `make lint`, then `make test`.** Inspect the repo first. Some `make` targets already include lint and test; some repos have no Makefile.

**If gates fail due to missing dependencies** (e.g., `node_modules missing`, missing codegen tool), install the repo's documented dependencies first, then re-run the same gate commands.

**If format changed files:** Stage the formatting fixes and include them in your commit.

**If any gate fails:** Fix the issues, stage the fixes, and re-run until all pass. Do NOT skip a failing gate. Do NOT proceed to commit until all gates are green.

#### ⛔ "Pre-existing" failures — verify before dismissing

**Do NOT assume a failure is pre-existing.** A failure is only pre-existing if it also fails on the PR base branch. Before dismissing any failure:

1. Check if it's a missing prerequisite (generated code, dependencies) — if so, fix it
2. If you believe it's truly pre-existing, verify by checking CI on the PR base branch or running the same command on the PR base branch
3. Only after verification can you note it and proceed — and you MUST inform the user explicitly

### Step 3: Commit Changes

Use conventional-commit format (see the `git-committing` skill). Always include the PR number.

### Step 4: Fix ALL Commits Missing PR Numbers

**This step is NON-NEGOTIABLE. You MUST fix commits, not just report on them.**

```bash
# Get the PR number
PR_NUM=$(gh pr view --json number -q .number 2>/dev/null || echo "UNKNOWN")
echo "PR number: $PR_NUM"

# Show ALL commits on this branch (compared to PR base)
git log origin/$BASE_BRANCH..HEAD --oneline
```

**Check EVERY commit message for `[#PR-NUM]`:**

- ✅ Good: `feat(mobile): [#2137] add feature X`
- ❌ Bad: `feat(mobile): add feature X` (missing PR number!)

**⛔ If ANY commit is missing the PR number, you MUST run the fix script:**

```bash
# Run from repo root - automatically detects PR number
.claude/skills/boss-finalize/add-pr-numbers.sh
```

**DO NOT skip this step.** Even if the branch is "up to date with origin", the commits still need PR numbers. The script compares against the PR base branch, not the feature branch.

**After the script completes, force-push to update the branch:**

```bash
git push --force-with-lease
```

**If the rebase fails:** Reset with `git rebase --abort` or `git reset --hard origin/<branch-name>` and try again.

**Verify ALL commits now have PR numbers before proceeding:**

```bash
git log origin/$BASE_BRANCH..HEAD --oneline | grep -v "\[#"
# Should return NOTHING - if it returns commits, they still need fixing
```

### Step 5: Squash and Tidy Commits

**This step is NON-NEGOTIABLE. You MUST squash commits into logical groups before pushing.**

```bash
git log origin/$BASE_BRANCH..HEAD --oneline
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
git log origin/$BASE_BRANCH..HEAD --oneline          # Clean, logical commits
git log origin/$BASE_BRANCH..HEAD --oneline | grep -v "\[#"  # All have PR numbers
```

### Step 6: Push to Remote

```bash
git pull --rebase
git push
git status  # Verify "up to date with origin"
```

If push fails, resolve and retry until success.

### Step 6b: Verify GitHub Checks

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

### Step 6c: Mark PR as Ready for Review

**This step is NON-NEGOTIABLE. You MUST mark the PR as ready for review.**

After checks are passing (or pending/in_progress), mark the PR as ready:

```bash
gh pr ready
```

This converts the PR from draft to ready-for-review status. Do NOT leave the PR as a draft when landing the plane.

**If the PR is already ready for review**, this command is a no-op and safe to run.

### Step 6d: Check for Merge Conflicts

**This step is NON-NEGOTIABLE. You MUST verify there are no merge conflicts.**

```bash
gh pr view --json mergeable -q .mergeable
```

**Expected result:** `MERGEABLE` — the PR can be merged cleanly.

**If the result is `CONFLICTING`:**

1. Fetch the PR base branch: `git fetch origin $BASE_BRANCH`
2. Rebase onto the PR base branch: `git rebase origin/$BASE_BRANCH`
3. Resolve any conflicts during the rebase
4. Re-run the repo's quality gates to ensure nothing broke
5. Force-push: `git push --force-with-lease`
6. Wait and re-check: `gh pr view --json mergeable -q .mergeable`
7. Repeat until `MERGEABLE`

**If the result is `UNKNOWN`:** GitHub is still computing mergeability. Wait a few seconds and re-check.

**Do NOT leave the session with merge conflicts.** A PR with conflicts cannot be merged and blocks the review process.

### Step 7: Clean Up and Verify

```bash
git stash list        # Note any stashes (don't auto-clear without asking)
git remote prune origin
git status            # Confirm clean state
```

### Step 8: Provide Handoff

**Provide a summary:**

> **Session Complete**
>
> **Completed:** [summary of work]
> **Quality gates:** [pass/fail status]
> **Push status:** All commits pushed to origin
>
> **Next steps:**
>
> - [follow-up item 1]
> - [follow-up item 2]
>
> **Recommended prompt:** "Continue work on [task]: [context]"

---

## Checklist

Before saying "done", verify ALL items:

- [ ] Repo quality gates discovered from project instructions, CI, or command files
- [ ] Minimal non-duplicative gate command set passed
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
| Skipped project gate discovery       | Wrong commands run     | Inspect project instructions, CI, and command files before choosing gates                            |
| Ran duplicate gate commands          | Slow and noisy         | Prefer one aggregate command when it covers build/lint/test; otherwise run only missing targets      |
| Skipped quality gates                | CI will fail           | Run the repo's required build/lint/test gates                                                        |
| Dismissed failure as "pre-existing"  | Failure was fixable    | Verify on the PR base branch before dismissing. Missing generated code or dependencies are NOT pre-existing |
| Missing dependencies in worktree     | Generate/format fails  | Install the repo's documented dependencies, then re-run the same gate commands                       |
| Stopped to ask permission to push    | Blocked automation     | Just push — do NOT ask for permission. Force-push is expected and authorized.                        |
| Commit missing `[#PR-NUM]`           | PR not linked          | Run `.claude/skills/boss-finalize/add-pr-numbers.sh` to fix ALL commits                              |
| Reported issue but didn't fix        | Commits still broken   | You MUST run the script, not just report that commits need fixing                                    |
| Compared against feature branch      | Wrong comparison       | Always compare to `origin/$BASE_BRANCH` to find all branch commits                                   |
| Branch "up to date" so skipped       | Commits still need PR# | Even pushed commits need PR numbers - compare to the PR base branch, not feature branch              |
| Didn't squash commits                | Messy history          | ALWAYS squash into logical groups — this is mandatory, not optional                                  |
| Said "ready when you are"            | Work stranded          | YOU push immediately — do not wait for user to do it or ask permission                               |
| Left session with failing checks     | CI is red              | Run `gh pr checks`, investigate failures with `gh run view --log-failed`, fix and re-push            |
| Ignored failing check as "unrelated" | CI still red           | Even if unrelated, inform user and get explicit acknowledgment                                       |
| Left empty "create PR" commit        | Messy history          | Use `drop` in rebase to remove empty scaffolding commits like `chore: [skip ci] create pull request` |
| Left PR as draft                     | Not reviewable         | Run `gh pr ready` to mark the PR as ready for review before completing                               |
| Left PR with merge conflicts         | PR can't be merged     | Run `gh pr view --json mergeable -q .mergeable`, rebase onto the PR base branch if `CONFLICTING`     |

---

## Related Skills

| Skill             | Relationship                         |
| ----------------- | ------------------------------------ |
| `/boss-verify`    | Run verification before finalizing   |
| `/boss-repair`    | Repair PR conflicts / failing checks |
| `/git-committing` | Conventional commit format reference |
