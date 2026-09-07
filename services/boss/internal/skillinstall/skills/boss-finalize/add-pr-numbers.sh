#!/bin/bash
#
# add-pr-numbers.sh - Add PR numbers to commit messages
#
# Usage: ./add-pr-numbers.sh [PR_NUMBER]
#
# If PR_NUMBER is not provided, it will be fetched from the current PR using gh cli.
# Set BASE_BRANCH to override PR base discovery.
# This script rebases all commits since the branch diverged from the PR base branch,
# adding [#PR_NUMBER] to any commit message that doesn't already have it.
#

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Get PR number from argument or gh cli
if [ -n "$1" ]; then
  PR_NUM="$1"
else
  PR_NUM=$(gh pr view --json number -q .number 2>/dev/null)
  if [ -z "$PR_NUM" ]; then
    echo "Error: Could not determine PR number. Pass it as an argument or ensure you're on a PR branch."
    exit 1
  fi
fi

# Reject anything that is not a PR number. An option-shaped argument (`--pr`, when the
# caller meant `--pr 42`) would otherwise be embedded verbatim as `[#--pr]` and satisfy
# every check below, because the tag this script writes and the tag it verifies are built
# from the same bad value -- success reported, wrong tag shipped.
case "$PR_NUM" in
'' | *[!0-9]*)
  echo "Error: PR number must be numeric, got: $PR_NUM" >&2
  exit 2
  ;;
esac

echo "PR number: #$PR_NUM"

# The one definition of "is this commit empty" lives in the shared predicate module,
# vendored beside this script. Both places that used to answer the question by hand --
# the rebase --exec helper and the post-condition below -- now read one classification
# produced by it, so they cannot drift apart the way two hand-written copies did.
# Resolve it BEFORE any fetch or rebase: a missing dependency must fail with history
# untouched, never half-rewritten.
COMMIT_WORK_PREDICATE="$SCRIPT_DIR/toolbox/commit-work-predicate.mjs"
if [ ! -f "$COMMIT_WORK_PREDICATE" ]; then
  echo "Error: shared commit predicate not found at $COMMIT_WORK_PREDICATE" >&2
  exit 1
fi
if ! command -v node > /dev/null 2>&1; then
  echo "Error: node is required to classify empty commits ($COMMIT_WORK_PREDICATE)." >&2
  exit 1
fi

# Print the SHA of every empty commit in "$1"..HEAD, one per line.
#
# ONE node invocation per phase, never one per commit: the inner helper runs inside
# `git rebase --exec`, where a per-commit subprocess that fails strands the worktree
# mid-rebase. The base commit is included so the oldest commit in the range has a
# parent tree to compare against; its own emptiness is never asked, and the module
# reports a commit whose first parent is outside the batch as non-empty -- the
# fail-safe direction, since an unclassifiable commit is then treated as real work
# that still owes a tag.
empty_commit_shas() {
  local base empty_tree rows
  base="$1"
  empty_tree=$(git hash-object -t tree /dev/null) || return 1
  rows=$(
    git show -s --format='%H%x09%T%x09%P' "$base"
    git log --format='%H%x09%T%x09%P' "$base"..HEAD
  ) || return 1
  printf '%s\n' "$rows" | node "$COMMIT_WORK_PREDICATE" empty-commits --empty-tree "$empty_tree"
}

# Find the base commit (where branch diverged from the PR base branch)
if [ -z "$BASE_BRANCH" ]; then
  BASE_BRANCH=$(gh pr view --json baseRefName -q .baseRefName 2>/dev/null || true)
fi
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
if [ -z "$BASE_BRANCH" ]; then
  echo "Error: Could not determine PR base branch. Set BASE_BRANCH or ensure you're on a PR branch."
  exit 1
fi
echo "Base branch: $BASE_BRANCH"
git fetch origin "$BASE_BRANCH"
BASE_COMMIT=$(git merge-base HEAD "origin/$BASE_BRANCH")
echo "Base commit: $BASE_COMMIT"

# Count commits to process
COMMIT_COUNT=$(git rev-list --count "$BASE_COMMIT"..HEAD)
echo "Commits to process: $COMMIT_COUNT"

if [ "$COMMIT_COUNT" -eq 0 ]; then
  echo "No commits to process."
  exit 0
fi

# Show commits before
echo ""
echo "Before:"
git log "$BASE_COMMIT"..HEAD --oneline
echo ""

# Copy helper script to /tmp (so it exists during rebase when working tree changes).
# The helper reports every non-empty commit it could not amend to
# PR_TAG_SKIP_REPORT, which must reach it through the environment: the heredoc
# below is unexpanded. Empty commits are intentionally skipped before any amend
# attempt, so they never write a skip-report row.
HELPER_SCRIPT="/tmp/add-pr-to-commit-$$.sh"
PR_TAG_SKIP_REPORT="/tmp/add-pr-skips-$$.tsv"
export PR_TAG_SKIP_REPORT
# Classify the range ONCE, here, and hand the answer to the helper through the
# environment -- the only channel across the quoted heredoc boundary below. Keyed on
# the PRE-rebase SHAs, which is what the helper's current_commit() reads out of the
# rebase done-file. On the rare fall-back path where it reports the rewritten HEAD
# instead, the SHA is simply absent from the set and the helper proceeds to the amend,
# where git's own "would make it empty" rejection is already converted to a skip.
# Fail here, before the rebase, rather than mid-rewrite.
PR_TAG_EMPTY_SHAS=$(empty_commit_shas "$BASE_COMMIT") || {
  echo "ERROR: could not classify the empty commits in $BASE_COMMIT..HEAD." >&2
  exit 1
}
export PR_TAG_EMPTY_SHAS
cleanup_temp() {
  rm -f "$PR_TAG_SKIP_REPORT"
  # Keep the helper if the rebase stopped part-way (a conflict when the branch carries a
  # merge from the base). `git rebase --continue` re-runs the --exec, and a helper deleted
  # underneath it degrades to a bare `warning: execution failed`, leaving every remaining
  # commit untagged -- the silent skip this script exists to prevent.
  if [ -d "$(git rev-parse --git-path rebase-merge 2>/dev/null)" ] ||
    [ -d "$(git rev-parse --git-path rebase-apply 2>/dev/null)" ]; then
    echo "NOTE: the rebase is unfinished. Resolve the conflict and 'git rebase --continue'" >&2
    echo "      (the helper is kept at $HELPER_SCRIPT), or 'git rebase --abort'." >&2
    # The post-condition below belongs to THIS process and cannot run for a rebase it did
    # not finish, so a --continue that hits another rejected amend would warn and stop
    # there. Re-running is the verification step; it is a no-op for commits already tagged.
    echo "      Either way, RE-RUN this script afterwards -- it is the only thing that" >&2
    echo "      verifies every commit ended up tagged." >&2
    return 0
  fi
  rm -f "$HELPER_SCRIPT"
  # Never let cleanup decide the script's exit status: a trap that ends non-zero would
  # turn a clean run into a reported failure, the inverse of the bug this script fixes.
  return 0
}
trap cleanup_temp EXIT
: > "$PR_TAG_SKIP_REPORT"
cat > "$HELPER_SCRIPT" << 'HELPER_EOF'
#!/bin/bash
PR_NUM="$1"
if [ -z "$PR_NUM" ]; then
  echo "Error: PR number required"
  exit 1
fi
MSG=$(git log -1 --format=%B)

# Emptiness is NOT re-derived here. The caller classified the whole range through the
# shared predicate module and passed the answer in as PR_TAG_EMPTY_SHAS, because this
# helper runs inside the quoted heredoc and cannot see the outer shell's functions --
# the duplication that boundary used to force is exactly what drifted.
is_known_empty_commit() {
  # Refuse an empty needle. `grep -qxF ""` MATCHES the blank line printf emits for an
  # empty set, so without this an unresolved commit id would classify as empty and skip
  # the amend -- the one direction this predicate must never fail in.
  [ -n "$1" ] || return 1
  printf '%s\n' "$PR_TAG_EMPTY_SHAS" | grep -qxF "$1"
}

current_commit() {
  local done line sha
  done=$(git rev-parse --git-path rebase-merge/done 2>/dev/null || true)
  if [ -f "$done" ]; then
    line=$(awk '$1 == "pick" || $1 == "reword" || $1 == "edit" {last=$0} END {print last}' "$done")
    sha=$(printf '%s\n' "$line" | awk '{print $2}')
    if [ -n "$sha" ] && git cat-file -e "$sha^{commit}" 2>/dev/null; then
      printf '%s\n' "$sha"
      return 0
    fi
  fi
  git rev-parse --verify HEAD
}

CURRENT_COMMIT=$(current_commit)
if is_known_empty_commit "$CURRENT_COMMIT"; then
  echo "Skipped empty commit $(git show -s --format=%h "$CURRENT_COMMIT") $(git show -s --format=%s "$CURRENT_COMMIT")"
  exit 0
fi

# Scope "already tagged" to the SUBJECT, the same place the caller's post-condition
# and every downstream consumer look. Checking the whole body instead would treat a
# commit that merely *mentions* [#N] in its prose as done, skip the amend, and then
# trip the post-condition on that commit forever -- a re-run could never clear it.
if git log -1 --format=%s | grep -q "\[#$PR_NUM\]"; then
  echo "Already has [#$PR_NUM]: $(git log -1 --format=%s)"
  exit 0
fi

# Try to add PR number after "): " (conventional commit with scope, e.g., "feat(web): ")
# Use "1s" to only replace on the first line (subject), preserving multi-line body
NEW_MSG=$(echo "$MSG" | sed "1s/): /): [#$PR_NUM] /")

# If no change, try after ": " for commits without scope (e.g., "fix: ")
if [ "$NEW_MSG" = "$MSG" ]; then
  # Match "type: " at start of first line (without scope)
  NEW_MSG=$(echo "$MSG" | sed "1s/^\([a-z]*\): /\1: [#$PR_NUM] /")

  # If still no change, append PR number to the SUBJECT as fallback. Consumers
  # grep line 1, so appending to the end of a multi-line body would be invisible.
  if [ "$NEW_MSG" = "$MSG" ]; then
    NEW_MSG=$(echo "$MSG" | sed "1s/\$/ [#$PR_NUM]/")
    echo "Warning: Could not find conventional commit pattern, appending PR number"
  fi
fi

# The amend can be rejected by the repo's hooks (an over-long subject or body
# line, or any other hook policy) or by git itself. Report the real error
# rather than claiming success -- and rather than naming a cause we did not
# check: without this the helper's exit status is the echo's, i.e. always 0,
# and the caller ships a partially tagged branch believing it worked.
if ! AMEND_ERR=$(git commit --amend -m "$NEW_MSG" 2>&1); then
  if printf '%s' "$AMEND_ERR" | grep -q "would make it empty"; then
    echo "Skipped empty commit $(git log -1 --format=%h) $(git log -1 --format=%s)"
    exit 0
  fi
  # Flatten to one line: the reason is the third TSV field, so a newline or tab in it
  # would split the record. The subject gets the same treatment for the same reason --
  # a tab there shifts the reason into field 4 and garbles the caller's annotation.
  REASON=$(printf '%s' "$AMEND_ERR" | tr '\n\t' '  ' | sed 's/  */ /g; s/^ //; s/ $//')
  # A hook may reject with no output at all. Say so, rather than emitting a bare entry
  # that reads exactly like a commit the helper never reached.
  [ -n "$REASON" ] || REASON="the amend failed without printing a reason"
  SUBJECT=$(git log -1 --format=%s | tr '\t' ' ' | tr -d '\n')
  echo "WARNING: could not amend $(git log -1 --format=%h) $SUBJECT: $REASON" >&2
  if [ -n "$PR_TAG_SKIP_REPORT" ]; then
    printf '%s\t%s\t%s\n' "$(git log -1 --format=%H)" "$SUBJECT" "$REASON" \
      >> "$PR_TAG_SKIP_REPORT"
  fi
  # Deliberately exit 0: a non-zero --exec strands the worktree mid-rebase, which
  # is worse for unattended callers. The outer script fails after the rebase ends.
  exit 0
fi
echo "Added [#$PR_NUM] to: $(git log -1 --format=%s)"
HELPER_EOF
chmod +x "$HELPER_SCRIPT"

# Run rebase with exec to add PR numbers
git rebase "$BASE_COMMIT" --exec "$HELPER_SCRIPT $PR_NUM"

# Post-condition: re-derive the truth from the log rather than trusting the
# helper's own prose. This scan is independent of the skip report (which only
# supplies a reason), so it also catches a pattern miss or an amend that
# succeeded without landing the tag.
#
# Capture the log BEFORE the loop. `while ... done < <(git log ...)` discards git's exit
# status: neither the loop nor `set -e` observes it, so an unreadable range would hand the
# loop EOF, leave SKIPPED_COUNT=0, and print "Done" having verified nothing. Fail CLOSED --
# an unverifiable post-condition is a failure, not a pass.
COMMIT_LOG=$(git log --reverse --format='%h%x09%H%x09%s' "$BASE_COMMIT"..HEAD) ||
  {
    echo "ERROR: could not read $BASE_COMMIT..HEAD to verify the tags." >&2
    exit 1
  }
# COMMIT_COUNT > 0 is guaranteed above, so an empty scan means the range stopped describing
# the branch -- again unverifiable, not clean.
if [ -z "$COMMIT_LOG" ]; then
  echo "ERROR: no commits found in $BASE_COMMIT..HEAD, but $COMMIT_COUNT were expected." >&2
  exit 1
fi
SKIPPED_COUNT=0

# Re-classify through the SAME module, over the rebased range. The pre-rebase set
# cannot be reused: the rebase rewrote every SHA in it. Re-deriving the truth from the
# log is the whole point of this post-condition -- it must not trust the helper's exit
# code or its prose -- so an unclassifiable range fails closed here too.
POST_EMPTY_SHAS=$(empty_commit_shas "$BASE_COMMIT") || {
  echo "ERROR: could not classify the empty commits in $BASE_COMMIT..HEAD." >&2
  exit 1
}
is_empty_commit() {
  # Same empty-needle refusal as the rebase helper's copy, for the same reason.
  [ -n "$1" ] || return 1
  printf '%s\n' "$POST_EMPTY_SHAS" | grep -qxF "$1"
}

while IFS=$'\t' read -r short sha subject; do
  case "$subject" in
  *"[#$PR_NUM]"*) continue ;;
  esac
  if is_empty_commit "$sha"; then
    # The rebase helper already announced this commit when it skipped the
    # amend. The post-condition only reclassifies it so it is not an error.
    continue
  fi
  if [ "$SKIPPED_COUNT" -eq 0 ]; then
    echo "" >&2
    echo "ERROR: these commits were left untagged:" >&2
  fi
  SKIPPED_COUNT=$((SKIPPED_COUNT + 1))
  # Third column of the skip report, when the helper recorded one: git's own error.
  # An entry with no recorded reason means the amend was never attempted for it.
  RECORDED=$(grep -m 1 "^$sha" "$PR_TAG_SKIP_REPORT" 2>/dev/null | cut -f3- || true)
  REASON=""
  if [ -n "$RECORDED" ]; then
    REASON=" (amend rejected: $RECORDED)"
  fi
  echo "  $short $subject$REASON" >&2
done <<< "$COMMIT_LOG"

if [ "$SKIPPED_COUNT" -gt 0 ]; then
  echo "" >&2
  echo "$SKIPPED_COUNT of $COMMIT_COUNT commits do not carry [#$PR_NUM]." >&2
  echo "Fix those commit messages (e.g. with git rebase -i) and re-run." >&2
  exit 1
fi

# Show commits after
echo ""
echo "After:"
git log "$BASE_COMMIT"..HEAD --oneline
echo ""
echo "Done. Verify the commits above, then force-push with: git push --force-with-lease"
