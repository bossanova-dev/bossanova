---
name: boss-finalize
description: End-of-session workflow ensuring all work is committed and pushed. Use when ending a work session or when asked to "land the plane".
---

# Land the Plane: Session Completion

Work is not complete until it is pushed and its PR is ready for review. This skill gets a branch
there without stopping to ask: you are expected to push, force-push with a lease, rebase and mark
the PR ready on your own authority.

## Done means

1. **The repo's quality gates pass** on the final tree.
2. **The PR base is in the branch** (`git merge-base --is-ancestor "origin/$BASE_BRANCH" HEAD`), and
   history is **linear** (`git rev-list --merges --count "origin/$BASE_BRANCH"..HEAD` is `0`).
3. **Commits follow the repo's convention.** If the repo requires a PR reference in commit messages
   — its agent instructions (`AGENTS.md`, `CLAUDE.md`, `CONTRIBUTING`) say so, a commit-msg hook
   enforces it, or the base branch's history uses `[#<PR>]` — every non-empty commit carries it. If
   the repo asks for squashed commits, they are squashed into logical groups. Either way, empty
   scaffolding commits (`chore: [skip ci] create pull request`) are dropped and fix-ups are folded
   into the commits they fix.
4. **Pushed**: the remote branch holds exactly `HEAD`.
5. **No check is failing** on the pushed head (pending and unknown are not failures).
6. **The PR is ready for review** (`isDraft` is `false`) and **mergeable** (not `CONFLICTING`).
7. **The worktree is clean.**

Never merge the base into the branch, never `git pull`, never `--rebase-merges`: a merge commit makes
a rebase-merge repo refuse the PR however green it is. Rebase instead
(`git fetch origin "$BASE_BRANCH"` then `git rebase --no-fork-point FETCH_HEAD` — a rebasing pull's
fork-point heuristic can silently drop this run's commits after a force-push). Never clear stashes.

## Dispatch

Run Steps 1–7 in **one** fresh awaited subagent (`general-purpose`, `model: "sonnet"` — the happy
path is mechanical), bounded by `BOSS_SKILL_EXTENSION_TIMEOUT_MS` (default 300000 ms) and classified
with `toolbox/bs-dispatch-await.mjs`; never background it. Give it the branch, PR URL and number, base
branch, the gate commands and the "Done means" list above. It keeps all bulk output (logs, diffs,
check tables) in its own context and returns only the final PR state, check status and what it
pushed. If it meets a real merge conflict or a failing gate that needs a code change, it stops and
returns `NEEDS_OPUS: <reason>` rather than resolving it; you then run the steps inline on your own
model, as you also do if the dispatch itself fails. Afterwards re-verify with one call:
`gh pr view --json isDraft,mergeable,statusCheckRollup`.

## Step 1: Find the base and make sure it is in the branch

```bash
git status                            # Uncommitted changes?
BASE_BRANCH=$(gh pr view --json baseRefName -q .baseRefName 2>/dev/null || true)
if [ -z "$BASE_BRANCH" ]; then
  CURRENT_BRANCH=$(git branch --show-current)
  UPSTREAM_BRANCH=$(git rev-parse --abbrev-ref --symbolic-full-name @{u} 2>/dev/null | sed 's#^origin/##' || true)
  # `head -1 | cut` and not an `awk` field reference: a slash-command invocation rewrites every
  # positional parameter in this body — a dollar sign followed by one digit — before any shell
  # runs it, replacing each with nothing when the command was invoked without arguments. An awk
  # program that selects a field then arrives with an empty print list, prints the whole line,
  # and hands back a wrong branch at exit 0. (This comment spells no positional itself, or the
  # substitution would eat the example too.) The `test -n` guard below covers the unrelated
  # case: no candidate branch at all. One behavioural difference the rewrite does introduce:
  # `head -1` exits as soon as it has the first line where awk consumed all of the input. This
  # block sets no `pipefail`, so the substitution's status is unchanged today — but under a future
  # `set -o pipefail` a SIGPIPE'd `sort` would fail it.
  BASE_BRANCH=$(git for-each-ref --format='%(refname:short)' refs/remotes/origin | sed 's#^origin/##' | grep -Fvx HEAD | grep -Fvx "$CURRENT_BRANCH" | { if [ -n "$UPSTREAM_BRANCH" ]; then grep -Fvx "$UPSTREAM_BRANCH"; else cat; fi; } | while read -r branch; do base=$(git merge-base HEAD "origin/$branch" 2>/dev/null) || continue; git merge-base --is-ancestor HEAD "origin/$branch" 2>/dev/null && continue; printf '%s %s\n' "$(git show -s --format=%ct "$base")" "$branch"; done | sort -nr | head -1 | cut -d' ' -f2)
  if [ -n "$BASE_BRANCH" ]; then echo "Using inferred git base branch: $BASE_BRANCH"; fi
fi
test -n "$BASE_BRANCH" || { echo "Could not determine PR base branch"; exit 1; }
git fetch origin "$BASE_BRANCH"
git log "origin/$BASE_BRANCH"..HEAD --oneline   # ALL commits on this branch (vs PR base)
gh pr view --json number -q .number   # Get PR number
```

Check the inferred base before continuing when GitHub metadata was unavailable. Then:

```bash
BASE_TIP=$(git rev-parse "origin/$BASE_BRANCH")
MERGE_BASE=$(git merge-base HEAD "origin/$BASE_BRANCH")
BRANCH_OWNED_FILES=$(mktemp)
git diff --name-only "$MERGE_BASE"..HEAD > "$BRANCH_OWNED_FILES"

if ! git merge-base --is-ancestor "origin/$BASE_BRANCH" HEAD; then
  echo "PR base is not included in HEAD. Rebase before squashing or pushing."
  git rebase "origin/$BASE_BRANCH"
  MERGE_BASE=$(git merge-base HEAD "origin/$BASE_BRANCH")
  git diff --name-only "$MERGE_BASE"..HEAD > "$BRANCH_OWNED_FILES"
fi

BASE_REVERTS=$(
  git diff --name-only "$MERGE_BASE".."origin/$BASE_BRANCH" | while IFS= read -r file; do
    base_blob=$(git rev-parse "$MERGE_BASE:$file" 2>/dev/null || true)
    head_blob=$(git rev-parse "HEAD:$file" 2>/dev/null || true)
    base_tip_blob=$(git rev-parse "origin/$BASE_BRANCH:$file" 2>/dev/null || true)
    if [ -n "$base_blob" ] && [ "$head_blob" = "$base_blob" ] && [ "$base_tip_blob" != "$base_blob" ]; then
      echo "$file"
    fi
  done
)
test -z "$BASE_REVERTS" || { echo "HEAD reverts files changed on origin/$BASE_BRANCH:"; echo "$BASE_REVERTS"; exit 1; }
test "$BASE_TIP" = "$(git rev-parse origin/$BASE_BRANCH)" || { echo "origin/$BASE_BRANCH moved during finalize; restart Step 1"; exit 1; }
```

Never `git reset --soft origin/$BASE_BRANCH` unless the base is already an ancestor of `HEAD` — on a
stale branch that stages reverse diffs that revert other people's work.

## Step 2: Run the quality gates

Find the commands this repo expects (agent instructions, then CI workflows, then `Makefile` /
`justfile` / `package.json` / `go.mod` / …) and run the smallest set that covers its generate,
build, lint and test checks without running any twice. Install documented dependencies if a gate
fails for want of them. Fix failures and re-run until green; stage formatter output.

A failure is **pre-existing** only once you have seen it fail on the base branch too (its CI, or the
same command on `origin/$BASE_BRANCH`). Missing generated code or dependencies are never
pre-existing. A proven pre-existing failure is recorded in the handoff and does not block.

## Step 3: Commit, tag and tidy

Commit remaining work with the repo's commit convention (conventional commits by default). Then:

- **PR reference, when the repo requires one** (see "Done means" 3):

  ```bash
  ~/.claude/skills/boss-finalize/add-pr-numbers.sh   # run from the repo root; detects the PR number
  ```

  It rebases since the base, tags every non-empty commit, and exits non-zero naming any commit it
  could not tag (usually a message a repo hook rejected — fix exactly what the hook names). Verify:

  ```bash
  PR_NUM=$(gh pr view --json number -q .number)
  git log origin/$BASE_BRANCH..HEAD --format='%H%x09%s' |
    while IFS=$'\t' read -r sha subject; do
      tree=$(git show -s --format=%T "$sha") || exit 1
      parent=$(git rev-parse --verify "$sha^" 2>/dev/null || true)
      if [ -n "$parent" ]; then
        if ! parent_tree=$(git show -s --format=%T "$parent"); then exit 1; fi
      else
        if ! parent_tree=$(git hash-object -t tree /dev/null); then exit 1; fi
      fi
      [ "$tree" = "$parent_tree" ] && continue
      case "$subject" in *"[#$PR_NUM]"*) ;; *) printf '%s %s\n' "${sha:0:12}" "$subject";; esac
    done
  # Expected output is empty; any listed non-empty commit still needs fixing
  ```

- **Squash, when the repo asks for it**, into coherent logical commits (feature + its tests + its
  fixes together), with `git rebase` `fixup`/`reword`. Then confirm the branch touches only its own
  files:

  ```bash
  git log origin/$BASE_BRANCH..HEAD --oneline
  MERGE_BASE=$(git merge-base HEAD "origin/$BASE_BRANCH")
  if [ -z "${BRANCH_OWNED_FILES:-}" ]; then BRANCH_OWNED_FILES=$(mktemp); fi
  git diff --name-only "$MERGE_BASE"..HEAD > "$BRANCH_OWNED_FILES"
  comm -13 <(sort "$BRANCH_OWNED_FILES") <(git diff --name-only "origin/$BASE_BRANCH"..HEAD | sort)
  ```

  Any output means the rewrite pulled in base-branch files: rebuild from `origin/$BASE_BRANCH`
  before pushing.

## Step 4: Push

```bash
git fetch origin "$BASE_BRANCH"
git merge-base --is-ancestor "origin/$BASE_BRANCH" HEAD || { echo "origin/$BASE_BRANCH is not in HEAD; rebase before push"; exit 1; }
MERGE_COUNT=$(git rev-list --merges --count "origin/$BASE_BRANCH"..HEAD) || exit 1
test "$MERGE_COUNT" = 0 || { echo "Merge commit(s) on this branch; linearize before pushing"; exit 1; }
git push --force-with-lease || { echo "push rejected or failed; the branch did not land"; exit 1; }
# A rejected push leaves `@{push}` as it was, so the exit check above comes first. Then HEAD must
# equal the ref it updated (`@{push}`), by object id. Each capture fails loud: empty never equals empty.
LOCAL_HEAD="$(git rev-parse HEAD)" || exit 1
PUSHED_HEAD="$(git rev-parse '@{push}')" || exit 1
test -n "$LOCAL_HEAD" && test "$LOCAL_HEAD" = "$PUSHED_HEAD" || { echo "HEAD is not what @{push} holds; the push did not land"; exit 1; }
```

Compare counts as strings (`test "$X" = 0`): `-eq` treats an empty operand as `0` and fails open. If
merge commits are on the branch, list them (`git rev-list --merges --oneline
"origin/$BASE_BRANCH..HEAD"`), carry any manual conflict resolution they hold into a normal commit,
then linearize:

```bash
git fetch origin "$BASE_BRANCH"
git rebase --onto "origin/$BASE_BRANCH" "$(git merge-base "origin/$BASE_BRANCH" HEAD)"
# Resolve any conflicts until the rebase COMPLETES, then re-assert before pushing:
git merge-base --is-ancestor "origin/$BASE_BRANCH" HEAD || { echo "rebase did not land on the base"; exit 1; }
MERGE_COUNT=$(git rev-list --merges --count "origin/$BASE_BRANCH"..HEAD) || exit 1
test "$MERGE_COUNT" = 0 || { echo "Branch still has merge commits; resolve by hand"; exit 1; }
git push --force-with-lease
```

## Step 5: Checks

Let the helper decide; a bucket table on its own cannot tell a gate that passed from one that never
ran:

```bash
BOSS_FINALIZE_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-finalize/toolbox"
if [ ! -d "$BOSS_FINALIZE_TOOLBOX" ]; then BOSS_FINALIZE_TOOLBOX="$HOME/.codex/skills/boss-finalize/toolbox"; fi
CHECK_DIR="$(mktemp -d)"
HEAD_SHA="$(gh pr view --json headRefOid -q .headRefOid)"
gh pr checks --json name,state,bucket > "$CHECK_DIR/checks.json"
gh api "repos/OWNER/REPO/commits/$HEAD_SHA/check-runs?per_page=100" --paginate --slurp > "$CHECK_DIR/runs.json"
gh run list --commit "$HEAD_SHA" --json name,workflowName,status,conclusion,headSha,event --limit 100 > "$CHECK_DIR/workflow-runs.json"
node "$BOSS_FINALIZE_TOOLBOX/pr-check-state.mjs" classify \
  --head-sha "$HEAD_SHA" --observed-sha "$HEAD_SHA" \
  --checks "$CHECK_DIR/checks.json" --check-runs "$CHECK_DIR/runs.json" \
  --workflow-runs "$CHECK_DIR/workflow-runs.json"
```

- `green` — done (`provesGreen: true` means a gate actually ran and the set was compared with the
  prior head).
- `pending` — keep waiting (a head workflow run still queued or running is `pending` too), except
  reason `absent-gate` (a gate the prior head had is missing; it will never arrive) — report it with
  its `absentGateRemedy`: `re-trigger-absent-gate`, or `reported-on-prior-head` (it already passed
  on the prior head and will not re-run; treat it as advisory or read `merge-state`).
- `unknown` — unobserved; report it, never call it green.
- `failing` — find the failing check (`gh pr checks --json name,state,bucket`), read only the failing
  log lines (`gh run view <run-id> --log-failed | tail`, or in a subagent), fix, push, re-check. A
  failure you have proven also fails on the base branch is not yours: record it in the handoff and a
  PR comment, and continue.

## Step 6: Ready and mergeable

```bash
PR_URL=$(gh pr view --json url -q .url)
gh pr ready "$PR_URL"

IS_DRAFT=""
for attempt in 1 2 3 4 5 6; do
  IS_DRAFT=$(gh pr view "$PR_URL" --json isDraft -q .isDraft)
  if [ "$IS_DRAFT" = "false" ]; then break; fi
  sleep 5
done

test "$IS_DRAFT" = "false" || {
  echo "PR is still draft after gh pr ready: $PR_URL"
  exit 1
}
```

`gh pr view --json mergeable -q .mergeable`: `MERGEABLE` is done; `UNKNOWN` means wait and re-read;
`CONFLICTING` means rebase onto `origin/$BASE_BRANCH`, resolve, re-run the gates, assert zero merge
commits, push with lease, and re-check.

## Step 7: Clean up and hand off

```bash
git stash list        # Note any stashes (don't auto-clear without asking)
git remote prune origin
BOSS_FINALIZE_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-finalize/toolbox"
if [ ! -d "$BOSS_FINALIZE_TOOLBOX" ]; then BOSS_FINALIZE_TOOLBOX="$HOME/.codex/skills/boss-finalize/toolbox"; fi
node "$BOSS_FINALIZE_TOOLBOX/worktree-state.mjs"   # Confirm clean state: must print verdict: clean
```

Act on the printed verdict, never on `git status` (a shell hook can fabricate a clean status):
`dirty` lists what is left; `unknown` is never clean.

Hand off: what was done, gate status, push status, any pre-existing failure you recorded, and next
steps with a suggested prompt for continuing.
