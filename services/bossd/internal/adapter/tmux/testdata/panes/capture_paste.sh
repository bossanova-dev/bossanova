#!/usr/bin/env bash
# capture_paste.sh — refresh the *_paste_*_real.txt and claude_typed_*_real.txt
# pane fixtures from live Claude Code and Codex CLIs. Sibling of capture.sh,
# which owns the mid-turn queued / swallowed-Enter fixtures.
#
# PROVENANCE OF THE COMMITTED FIXTURES
#   claude_typed_long_line_stuck_real.txt
#   claude_typed_long_line_tail_only_real.txt
#   claude_paste_long_line_stuck_real.txt
#   claude_paste_multiline_stuck_real.txt
#   claude_paste_long_line_submitted_real.txt
#   claude_paste_multiline_submitted_real.txt
#   codex_paste_stuck_real.txt
#   codex_paste_submitted_real.txt
# were captured on 2026-10-12 with:
#   Claude Code 2.1.296   (claude --version)
#   codex-cli 0.161.0     (codex --version, run from the capture workdir)
#   tmux 3.8              (tmux -V)
#   pane geometry 200x50  (bossd's NewSession default — services/bossd/internal/adapter/tmux/tmux.go)
#   macOS 26.4.1 (darwin/arm64)
#
# Re-run this when bumping either CLI, or when the collapsed-paste tests in
# tmux_submit_verify_paste_test.go start failing. Do not hand-edit the .txt
# files — recapture them. The fixtures exist because a long payload is
# collapsed by both TUIs into a placeholder that does not prefix-match the
# payload, so a verifier keyed on the payload text reads a held composer as
# cleared (BOS-1506).
#
# Requirements:
#   - claude on PATH and authenticated (a detached tmux server inherits this
#     shell's environment; ANTHROPIC_API_KEY / ANTHROPIC_BASE_URL are forwarded
#     explicitly when set)
#   - codex on PATH and authenticated. Export CODEX_HOME when the logged-in
#     account lives outside ~/.codex — bossd's own codex accounts live under
#     ~/Library/Application Support/bossanova/accounts/codex/<id>.
#   - tmux on PATH
#   - network access to both providers. Two short turns run per CLI.
#
# WHAT EACH FIXTURE IS
#   claude_typed_long_line_stuck      the pre-BOS-1506 delivery: a 2 KB line
#                                     typed with send-keys -l, NO Enter. Claude
#                                     reads the burst as a paste and renders
#                                     "❯ [Pasted text #N]…<tail>".
#   claude_typed_long_line_tail_only  the same keystrokes on a run where Claude
#                                     kept only the tail typed after the burst
#                                     ("❯ lorem ipsum padding 85 … 90"). Written
#                                     only when a run renders it.
#   claude_paste_long_line_stuck      the same line bracketed-pasted, NO Enter:
#                                     "❯ [Pasted text #N]".
#   claude_paste_multiline_stuck      an 11-line payload pasted, NO Enter:
#                                     "❯ [Pasted text #N +10 lines]".
#   claude_paste_*_submitted          the same pastes followed by Enter: the
#                                     composer is cleared.
#   codex_paste_stuck                 the 2 KB line pasted into codex, NO
#                                     Enter: "› [Pasted Content 2181 chars]".
#   codex_paste_submitted             the same paste followed by Enter.
set -euo pipefail

FIXTURE_DIR="$(cd "$(dirname "$0")" && pwd)"
WORKDIR="${TMPDIR:-/tmp}/bos1506-capture"
SOCKET="bos1506-capture-$$"
LONG="$WORKDIR/payload_long_line.txt"
MULTI="$WORKDIR/payload_multiline.txt"

# A private tmux server, so nothing here can touch the sessions bossd runs on
# the default socket.
t() { tmux -L "$SOCKET" "$@"; }
cleanup() {
    t kill-server 2>/dev/null || true
    rm -f "$LONG" "$MULTI"
}
trap cleanup EXIT

fail() {
    echo "FAIL: $1" >&2
    echo "      Review the pane above; the CLI may have changed its paste rendering." >&2
    exit 1
}

mkdir -p "$WORKDIR"

# The payloads ask for a one-word reply so the submitted captures cost a
# trivial turn. The long line is ~2 KB — past both TUIs' paste-collapse
# threshold — and contains no newline.
long="Reply with only the single word OK and nothing else. Ignore the rest of this message, it is padding for a delivery test:"
for i in $(seq 1 90); do long="$long lorem ipsum padding $i"; done
printf '%s' "$long" >"$LONG"
# 11 lines, 10 separators, no trailing newline: Claude renders "+10 lines".
multi="Reply with only the single word OK and nothing else."
for i in $(seq 1 10); do multi="$multi"$'\n'"padding line $i of a multi-line delivery test"; done
printf '%s' "$multi" >"$MULTI"

capture() { t capture-pane -t "$1" -p >"$FIXTURE_DIR/$2"; }

# live_row prints the bottom-most row that starts with the composer glyph. The
# glyph is followed by a NO-BREAK SPACE (U+00A0), not an ASCII space, so every
# match below allows one or two bytes between the glyph and the content.
live_row() { t capture-pane -t "$1" -p | grep "^$2" | tail -1 || true; }

# wait_for polls the pane for a pattern for up to $3 seconds.
wait_for() {
    local i
    for ((i = 0; i < $3 * 10; i++)); do
        if t capture-pane -t "$1" -p | grep -q -- "$2"; then return 0; fi
        sleep 0.1
    done
    return 1
}

paste_file() {
    t load-buffer "$2"
    t paste-buffer -d -p -t "$1"
}

clear_composer() {
    local _
    for _ in 1 2 3; do
        t send-keys -t "$1" C-u
        sleep 0.5
    done
}

# --- Claude ------------------------------------------------------------------
env_args=()
[ -n "${ANTHROPIC_API_KEY:-}" ] && env_args+=(-e "ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY")
[ -n "${ANTHROPIC_BASE_URL:-}" ] && env_args+=(-e "ANTHROPIC_BASE_URL=$ANTHROPIC_BASE_URL")
t new -d -s claude -x 200 -y 50 -c "$WORKDIR" "${env_args[@]}" 'claude --dangerously-skip-permissions'
sleep 10
if t capture-pane -t claude -p | grep -q 'Yes, I trust this folder'; then
    t send-keys -t claude Down
    sleep 0.5
    t send-keys -t claude Enter
    sleep 6
fi
wait_for claude '^❯' 20 || fail "claude never drew its composer"

paste_file claude "$LONG"
sleep 2
capture claude claude_paste_long_line_stuck_real.txt
grep -q '^❯.\{1,2\}\[Pasted text #[0-9]*\]' "$FIXTURE_DIR/claude_paste_long_line_stuck_real.txt" ||
    fail "pasted long line did not collapse into a [Pasted text #N] placeholder"
clear_composer claude

paste_file claude "$MULTI"
sleep 2
capture claude claude_paste_multiline_stuck_real.txt
grep -q '^❯.\{1,2\}\[Pasted text #[0-9]* +10 lines\]' "$FIXTURE_DIR/claude_paste_multiline_stuck_real.txt" ||
    fail "pasted multi-line payload did not collapse into a [Pasted text #N +10 lines] placeholder"
clear_composer claude

paste_file claude "$LONG"
sleep 1
t send-keys -t claude Enter
sleep 2
capture claude claude_paste_long_line_submitted_real.txt
case "$(live_row claude '❯')" in
*"Pasted text"*) fail "the long-line paste is still in the composer after Enter" ;;
esac
sleep 25 # let the one-word turn finish before the next paste

paste_file claude "$MULTI"
sleep 1
t send-keys -t claude Enter
sleep 2
capture claude claude_paste_multiline_submitted_real.txt
case "$(live_row claude '❯')" in
*"Pasted text"*) fail "the multi-line paste is still in the composer after Enter" ;;
esac
sleep 25 # let the turn finish so the typed burst lands in an idle composer

# The pre-change delivery: literal keystrokes, which Claude reads as a paste.
# Its rendering is not deterministic on 2.1.296: one burst renders
# "[Pasted text #N]…<tail>", another drops the head entirely and leaves only
# the tail typed after the burst. Both are kept when seen; the placeholder one
# is required, so retry for it. Fresh sessions rendered the bare tail on every
# attempt; the placeholder appeared only once the session had taken pastes and
# finished a turn, which is why this runs last.
got_placeholder=""
for attempt in 1 2 3 4; do
    t send-keys -t claude -l -- "$long"
    sleep 2
    capture claude claude_typed_long_line_attempt.txt
    row="$(live_row claude '❯')"
    case "$row" in
    "❯"*"[Pasted text #"*)
        mv "$FIXTURE_DIR/claude_typed_long_line_attempt.txt" "$FIXTURE_DIR/claude_typed_long_line_stuck_real.txt"
        got_placeholder=1
        ;;
    *"padding 90")
        echo "attempt $attempt: typed line rendered as its bare tail"
        mv "$FIXTURE_DIR/claude_typed_long_line_attempt.txt" "$FIXTURE_DIR/claude_typed_long_line_tail_only_real.txt"
        ;;
    *)
        rm -f "$FIXTURE_DIR/claude_typed_long_line_attempt.txt"
        fail "typed long line rendered as neither a placeholder nor its tail: $row"
        ;;
    esac
    clear_composer claude
    [ -n "$got_placeholder" ] && break
done
[ -n "$got_placeholder" ] || fail "typed long line never collapsed into a [Pasted text #N] placeholder"


# --- Codex -------------------------------------------------------------------
codex_env=()
[ -n "${CODEX_HOME:-}" ] && codex_env+=(-e "CODEX_HOME=$CODEX_HOME")
t new -d -s codex -x 200 -y 50 -c "$WORKDIR" "${codex_env[@]}" 'codex -c check_for_update_on_startup=false'
wait_for codex '^› ' 30 || fail "codex never drew its composer (is it logged in? see CODEX_HOME above)"
sleep 2

paste_file codex "$LONG"
sleep 2
capture codex codex_paste_stuck_real.txt
grep -q '^›.\{1,2\}\[Pasted Content' "$FIXTURE_DIR/codex_paste_stuck_real.txt" ||
    fail "codex did not collapse the paste into a [Pasted Content …] placeholder"

t send-keys -t codex Enter
sleep 2
capture codex codex_paste_submitted_real.txt
case "$(live_row codex '›')" in
*"Pasted Content"*) fail "the codex paste is still in the composer after Enter" ;;
esac

echo "OK: refreshed the real paste pane fixtures in $FIXTURE_DIR"
echo "    Record the claude/codex/tmux versions above in this script's PROVENANCE block."
