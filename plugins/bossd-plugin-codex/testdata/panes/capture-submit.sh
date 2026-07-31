#!/usr/bin/env bash
# capture-submit.sh — refresh the submit-verification pane fixtures from a live
# codex CLI.
#
# Sibling of capture.sh (which refreshes the approval-menu fixture). This one
# captures the two panes the tmux submit verifier must tell apart:
#
#   submit_pending.txt   — a single-line payload typed into the live composer
#                          but NOT submitted (Enter never sent).
#   submit_submitted.txt — the same payload one second after Enter, i.e. inside
#                          the window waitForSubmission actually polls.
#
# The submitted capture is the load-bearing artifact for BOS-597: it shows that
# a cleared codex composer renders a *placeholder hint* ("› Run /review on my
# current changes") rather than a bare glyph, and that codex draws its working
# bullet ABOVE the composer. Both facts break a glyph-only cleared-composer
# check. Re-capture whenever the codex CLI is bumped.
#
# Requirements:
#   - codex on PATH, authenticated (`codex login status` reports logged in)
#   - tmux on PATH
#   - network access to OpenAI
#
# The captured codex version is self-recorded in the pane banner
# ("│ >_ OpenAI Codex (vX.Y.Z) │"), so the fixtures carry their own provenance.
set -euo pipefail

SESSION="codex-submit-capture-$$"
FIXTURE_DIR="$(cd "$(dirname "$0")" && pwd)"
PENDING="$FIXTURE_DIR/submit_pending.txt"
SUBMITTED="$FIXTURE_DIR/submit_submitted.txt"

# The payload the fixtures are captured with. The golden tests assert against
# this exact string, so keep the two in sync.
PAYLOAD="write a haiku about autumn leaves"

cleanup() { tmux kill-session -t "$SESSION" 2>/dev/null || true; }
trap cleanup EXIT

# read-only sandbox: the capture must never let codex touch the working tree.
tmux new -d -s "$SESSION" -x 220 -y 60 'codex -s read-only'

# Codex needs ~10s to draw the welcome banner and the live composer.
sleep 12

# Type the payload WITHOUT submitting, and capture the still-pending composer.
tmux send-keys -t "$SESSION" "$PAYLOAD"
sleep 2
tmux capture-pane -t "$SESSION" -p >"$PENDING"

if ! grep -qF "› $PAYLOAD" "$PENDING"; then
    echo "FAIL: pending capture has no composer row holding the payload."
    echo "      Review: $PENDING"
    exit 1
fi

# Submit, then capture inside the window the verifier polls (submitVerifyWait
# defaults to 2s, so 1s is representative of a real poll).
tmux send-keys -t "$SESSION" Enter
sleep 1
tmux capture-pane -t "$SESSION" -p >"$SUBMITTED"

# Sanity-check the captured pane really is post-submit: codex echoes the sent
# message as a history row and renders its working bullet.
if ! grep -qE '^\s*•\s+Working' "$SUBMITTED"; then
    echo "FAIL: submitted capture has no codex working bullet; the send may"
    echo "      never have been accepted. Review: $SUBMITTED"
    exit 1
fi

echo "OK: refreshed $PENDING ($(wc -l <"$PENDING" | tr -d ' ') lines)"
echo "OK: refreshed $SUBMITTED ($(wc -l <"$SUBMITTED" | tr -d ' ') lines)"
