#!/usr/bin/env bash
# capture.sh — refresh testdata/panes/question.txt from a live codex CLI.
#
# Run this when bumping the codex CLI version (or when
# TestQuestionPromptRealPaneFixture starts failing) to regenerate the real
# approval-menu pane fixture used by the question detector tests.
#
# Requirements:
#   - codex on PATH (any version that emits an approval prompt)
#   - tmux on PATH
#   - codex authenticated (`codex login status` reports logged in)
#   - network access to OpenAI
#
# Output: overwrites plugins/bossd-plugin-codex/testdata/panes/question.txt
# with the freshly captured 60×220 pane.
set -euo pipefail

SESSION="codex-capture-$$"
FIXTURE_DIR="$(cd "$(dirname "$0")" && pwd)"
FIXTURE="$FIXTURE_DIR/question.txt"

cleanup() { tmux kill-session -t "$SESSION" 2>/dev/null || true; }
trap cleanup EXIT

tmux new -d -s "$SESSION" -x 220 -y 60 \
    'codex --ask-for-approval untrusted -s workspace-write'

# Codex needs ~10s to draw the welcome banner.
sleep 10

# Send a prompt that forces codex to ask before it acts.
tmux send-keys -t "$SESSION" \
    "create a new file named /tmp/codex-approval-test.txt containing the word hello"
sleep 1
tmux send-keys -t "$SESSION" Enter

# Wait for codex to think + emit the approval menu. 30s is enough on a
# warm cache; bump if your run is slower.
sleep 30

tmux capture-pane -t "$SESSION" -p > "$FIXTURE"

# Sanity-check: the captured pane must contain the approval-menu footer.
if ! grep -qE '(Press\s+enter\s+to\s+confirm\s+or\s+esc|Press\s+1[-/0-9]*\s+or\s+esc)' "$FIXTURE"; then
    echo "FAIL: captured pane has no approval-menu footer; codex may have"
    echo "      auto-approved or never reached the prompt. Review:"
    echo "      $FIXTURE"
    exit 1
fi

echo "OK: refreshed $FIXTURE ($(wc -l < "$FIXTURE" | tr -d ' ') lines)"
