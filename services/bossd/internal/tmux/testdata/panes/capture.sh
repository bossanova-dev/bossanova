#!/usr/bin/env bash
# capture.sh — refresh the claude_*_real.txt pane fixtures from a live Claude Code CLI.
#
# PROVENANCE OF THE COMMITTED FIXTURES
#   claude_queued_midturn_real.txt
#   claude_swallowed_enter_midturn_real.txt
#   claude_working_empty_composer_real.txt
# were captured on 2026-07-31 with:
#   Claude Code 2.1.220   (claude --version)
#   tmux 3.6b             (tmux -V)
#   pane geometry 200x50  (bossd's NewSession default — services/bossd/internal/tmux/tmux.go)
#   macOS 25.5.0 (darwin/arm64)
#
# Re-run this when bumping the Claude Code CLI, or when the queued/swallowed-Enter
# tests in tmux_submit_verify_queued_test.go start failing. The fixtures are real
# captures on purpose: BOS-599's first pass authored them from a plan's prose and
# every load-bearing claim in them was refuted the moment a real pane existed (a
# queued send CLEARS the composer; the mid-turn spinner carries no "esc to
# interrupt" clause). Do not hand-edit the .txt files — recapture them.
#
# Requirements:
#   - claude on PATH and authenticated (or ANTHROPIC_API_KEY exported; a detached
#     tmux server does NOT inherit your shell env, which is why the session below
#     passes credentials explicitly with -e)
#   - tmux on PATH
#   - network access to the Anthropic API
#
# WHAT EACH FIXTURE IS
#   queued           mid-turn, Enter ACCEPTED   -> composer cleared to the queue
#                                                 hint, payload echoed above it
#   swallowed_enter  mid-turn, Enter NOT sent   -> composer still holds the payload
#   working_empty    the same turn as above,    -> control: proves the spinner
#                    composer cleared (C-u)        wording is not caused by
#                                                  composer text
set -euo pipefail

FIXTURE_DIR="$(cd "$(dirname "$0")" && pwd)"
PAYLOAD="also update the changelog"
# A prompt that keeps the agent genuinely mid-turn for long enough to capture.
# A foreground shell command is used rather than "sleep": Claude Code's own
# harness guard refuses standalone sleeps and backgrounds them, which ENDS the
# turn and defeats the capture.
BUSY_PROMPT="Run this bash command in the foreground and tell me the number it prints: find / -xdev -type f 2>/dev/null | wc -l"
WORKDIR="${TMPDIR:-/tmp}/bos599-capture"

SESSION="claude-capture-$$"
cleanup() { tmux kill-session -t "$SESSION" 2>/dev/null || true; }
trap cleanup EXIT

mkdir -p "$WORKDIR"

start_session() {
    cleanup
    local env_args=()
    [ -n "${ANTHROPIC_API_KEY:-}" ] && env_args+=(-e "ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY")
    [ -n "${ANTHROPIC_BASE_URL:-}" ] && env_args+=(-e "ANTHROPIC_BASE_URL=$ANTHROPIC_BASE_URL")
    tmux new -d -s "$SESSION" -x 200 -y 50 -c "$WORKDIR" "${env_args[@]}" \
        'claude --dangerously-skip-permissions'
    # Claude Code needs ~10s to draw its welcome banner and composer.
    sleep 12
}

# go_mid_turn submits the long-running prompt and waits for the turn to start.
go_mid_turn() {
    tmux send-keys -t "$SESSION" "$BUSY_PROMPT"
    sleep 1
    tmux send-keys -t "$SESSION" Enter
    sleep 8
}

fail() {
    echo "FAIL: $1" >&2
    echo "      Review the pane above; the agent may have finished the turn early." >&2
    exit 1
}

# --- swallowed Enter (and its cleared-composer control), one running turn ------
start_session
go_mid_turn
tmux send-keys -t "$SESSION" "$PAYLOAD" # deliberately NO Enter
sleep 2
tmux capture-pane -t "$SESSION" -p >"$FIXTURE_DIR/claude_swallowed_enter_midturn_real.txt"
grep -q "^❯ $PAYLOAD" "$FIXTURE_DIR/claude_swallowed_enter_midturn_real.txt" ||
    fail "the composer does not hold the unsent payload"

tmux send-keys -t "$SESSION" C-u # clear the composer, same turn still running
sleep 2
tmux capture-pane -t "$SESSION" -p >"$FIXTURE_DIR/claude_working_empty_composer_real.txt"

# --- queued (Enter accepted mid-turn) ----------------------------------------
start_session
go_mid_turn
tmux send-keys -t "$SESSION" "$PAYLOAD"
sleep 1
tmux send-keys -t "$SESSION" Enter
sleep 3
tmux capture-pane -t "$SESSION" -p >"$FIXTURE_DIR/claude_queued_midturn_real.txt"
grep -qi 'queued messages\?' "$FIXTURE_DIR/claude_queued_midturn_real.txt" ||
    fail "the captured pane shows no queued-messages hint; the agent may have been idle when Enter landed"

echo "OK: refreshed 3 real pane fixtures in $FIXTURE_DIR"
echo "    Record the claude/tmux versions above in this script's PROVENANCE block."
