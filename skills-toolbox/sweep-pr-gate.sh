#!/usr/bin/env bash
# sweep-pr-gate.sh — the invariant bs-sweep-* PR gate: find-or-create the PR, inject
# [#N] into every commit, force-push with lease, and flip draft -> ready.
# Inputs (env): SESSION_BRANCH, BASE_BRANCH, PR_BODY, START_SHA. Output: the PR number on stdout.
# Diagnostics go to stderr — stdout is captured by the caller, so a stray stdout write
# corrupts the caller's PR_NUMBER. Also implicit: the CWD must be the session worktree
# (every git/gh call resolves against it, and PR_BODY may be a worktree-relative path).
set -euo pipefail
trap 'echo "sweep-pr-gate: failed at line $LINENO (exit $?)" >&2' ERR
: "${SESSION_BRANCH:?SESSION_BRANCH required}"
: "${BASE_BRANCH:?BASE_BRANCH required}"
: "${PR_BODY:?PR_BODY required}"
: "${START_SHA:?START_SHA required}"

# add-pr-numbers.sh lives in the RUNNING agent's global skills home, and boss installs only
# the homes whose agent binary is present. This helper is a .sh, so sync-codex-skills.mjs'
# `~/.claude/skills/ -> ~/.codex/skills/` rewrite — which used to repoint the mirrored
# SKILL.md copy of this line — never applies to it. Probe both homes: hard-coding either one
# makes the gate exit 127 under `set -e` on a host that has only the other.
# Neither home having it is checked HERE, before the first gh call, so the gate refuses while
# it has changed nothing. Discovering it at the injection step instead would leave an
# already-created, still-draft PR whose commits carry no [#N] — a half-done state a rerun has
# to clean up. The one sweep whose variant gate was not extracted carries the same precondition.
ADD_PR_NUMBERS="$HOME/.claude/skills/bossanova/boss-finalize/add-pr-numbers.sh"
if [ ! -x "$ADD_PR_NUMBERS" ]; then
  ADD_PR_NUMBERS="$HOME/.codex/skills/bossanova/boss-finalize/add-pr-numbers.sh"
fi
if [ ! -x "$ADD_PR_NUMBERS" ]; then
  echo "sweep-pr-gate: no executable add-pr-numbers.sh in ~/.claude or ~/.codex skills homes; install the boss skills before running the PR gate." >&2
  exit 1
fi

# Branch-safety: everything below retags every commit in origin/$BASE_BRANCH..HEAD, force-pushes
# with lease, and flips the branch's open draft PR to ready. On a branch that already carried
# someone else's work that is a PR hijack, so refuse HERE, before the first gh call, while the
# gate has changed nothing. The signal is the commits present at START_SHA, not the PR's
# identity: bossd bootstraps every session by pushing an EMPTY placeholder commit
# (DraftPRPlaceholderCommitSubject, services/bossd/internal/git/worktree.go) and opening a draft
# PR from it, which a normal cron sweep MUST adopt — so a "refuse any commit ahead of base" or an
# "adopt only PRs I created" guard would disable all four sweeps instead of protecting them.
# The pattern is that constant plus draftPRPlaceholderTagRE's stacking `[#N] ` tags, duplicated
# across languages and pinned against the Go source by scripts/sweep-pr-gate.test.mjs.
# Same own-line-assignment discipline as the [#N] check below, for the same errexit reason.
# The `%h ` in the format is load-bearing, not decoration. A commit with an EMPTY subject
# (--allow-empty-message) is foreign, but under a bare `%s` it logs as a blank line, and BOTH
# command substitutions strip blank lines — so PRE_EXISTING and FOREIGN come back empty and the
# guard fails OPEN on exactly the branch it exists to refuse. Prefixing the abbreviated hash makes
# every line unconditionally non-empty, so emptiness means "no commits" and nothing else; it also
# gives the operator the SHA to inspect. The pattern re-anchors on that prefix, so a subject that
# merely CONTAINS the placeholder ("x chore: [skip ci] create pull request") stays foreign.
# (Go's IsDraftPRPlaceholderSubject TrimSpace's first and this does not, so a leading-space
# subject is refused here and accepted there — an over-rejection, i.e. fail-closed.)
git fetch origin "$BASE_BRANCH" >&2
PRE_EXISTING="$(git log "origin/$BASE_BRANCH".."$START_SHA" --format='%h %s')"
FOREIGN="$(printf '%s' "$PRE_EXISTING" | grep -Ev '^[0-9a-f]+ chore: (\[#[0-9]+\] )*\[skip ci\] create pull request$' || true)"
if [ -n "$FOREIGN" ]; then
  echo "$FOREIGN" >&2
  echo "sweep-pr-gate: branch $SESSION_BRANCH already carried commits before this sweep started; refusing to retag, force-push and ready a PR this sweep does not own." >&2
  exit 1
fi

PR_NUMBER="$(gh pr list --head "$SESSION_BRANCH" --state open --json number -q '.[0].number // empty')"
if [ -z "$PR_NUMBER" ]; then
  gh pr create \
    --base "$BASE_BRANCH" \
    --head "$SESSION_BRANCH" \
    --title "$(git log -1 --pretty=%s)" \
    --body-file "$PR_BODY" >&2
  PR_NUMBER="$(gh pr list --head "$SESSION_BRANCH" --state open --json number -q '.[0].number // empty')"
fi
test -n "$PR_NUMBER"
BASE_BRANCH="$(gh pr view "$PR_NUMBER" --json baseRefName -q .baseRefName)"
git fetch origin "$BASE_BRANCH" >&2
BASE_BRANCH="$BASE_BRANCH" "$ADD_PR_NUMBERS" "$PR_NUMBER" >&2
# Run git log on its OWN line, not as the head of the `if` condition's pipeline. Bash suppresses
# errexit inside an `if` condition, and under pipefail a FAILING git log (a missing
# origin/$BASE_BRANCH, a corrupt object, an unborn HEAD) yields the same false condition as "grep
# found no untagged commits" — so the [#N] check would silently pass and the gate would ready an
# untagged PR. As an assignment it aborts instead. `|| true` now covers only the grep, never
# git log; it absorbs grep's no-match exit 1 (and, harmlessly, a grep error, which this fixed BRE
# over a numeric PR_NUMBER cannot produce).
COMMITS="$(git log "origin/$BASE_BRANCH"..HEAD --oneline)"
UNTAGGED="$(printf '%s' "$COMMITS" | grep -v "\[#$PR_NUMBER\]" || true)"
if [ -n "$UNTAGGED" ]; then
  echo "$UNTAGGED" >&2
  echo "Commits still missing [#$PR_NUMBER]; rerun PR-number finalization before readying the PR." >&2
  exit 1
fi
git push --force-with-lease origin "$SESSION_BRANCH" >&2
test "$(git rev-parse HEAD)" = "$(git rev-parse @{u})"
if [ "$(gh pr view "$PR_NUMBER" --json isDraft -q .isDraft)" = "true" ]; then
  gh pr ready "$PR_NUMBER" >&2
fi
test "$(gh pr view "$PR_NUMBER" --json isDraft -q .isDraft)" = "false"
printf '%s\n' "$PR_NUMBER"
