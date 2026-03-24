#!/bin/bash
#
# add-pr-numbers.sh - Add PR numbers to commit messages
#
# Usage: ./add-pr-numbers.sh [PR_NUMBER]
#
# If PR_NUMBER is not provided, it will be fetched from the current PR using gh cli.
# This script rebases all commits since the branch diverged from origin/main,
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

echo "PR number: #$PR_NUM"

# Find the base commit (where branch diverged from main)
BASE_COMMIT=$(git merge-base HEAD origin/main)
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

# Copy helper script to /tmp (so it exists during rebase when working tree changes)
HELPER_SCRIPT="/tmp/add-pr-to-commit-$$.sh"
cat > "$HELPER_SCRIPT" << 'HELPER_EOF'
#!/bin/bash
PR_NUM="$1"
if [ -z "$PR_NUM" ]; then
  echo "Error: PR number required"
  exit 1
fi
MSG=$(git log -1 --format=%B)
if echo "$MSG" | grep -q "\[#$PR_NUM\]"; then
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

  # If still no change, append PR number at the end as fallback
  if [ "$NEW_MSG" = "$MSG" ]; then
    NEW_MSG="$MSG [#$PR_NUM]"
    echo "Warning: Could not find conventional commit pattern, appending PR number"
  fi
fi

git commit --amend -m "$NEW_MSG"
echo "Added [#$PR_NUM] to: $(git log -1 --format=%s)"
HELPER_EOF
chmod +x "$HELPER_SCRIPT"

# Run rebase with exec to add PR numbers
git rebase "$BASE_COMMIT" --exec "$HELPER_SCRIPT $PR_NUM"

# Clean up
rm -f "$HELPER_SCRIPT"

# Show commits after
echo ""
echo "After:"
git log "$BASE_COMMIT"..HEAD --oneline
echo ""
echo "Done. Verify the commits above, then force-push with: git push --force-with-lease"
