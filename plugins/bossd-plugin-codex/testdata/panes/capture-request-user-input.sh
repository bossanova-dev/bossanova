#!/usr/bin/env bash
# capture-request-user-input.sh — refresh testdata/panes/requestuserinput/
# from a live codex CLI.
#
# Requirements:
#   - codex on PATH, authenticated (`codex login status` reports logged in)
#   - tmux on PATH
#   - network access to OpenAI
set -euo pipefail

FIXTURE_DIR="$(cd "$(dirname "$0")" && pwd)"
OUT_DIR="${OUT_DIR:-$FIXTURE_DIR/requestuserinput}"
SOCKET="codex-rui-capture-$$"
TMP_ROOT="${TMPDIR:-/tmp}"
TMP_ROOT="${TMP_ROOT%/}"
TMP_ROOT_REAL="$(cd "$TMP_ROOT" && pwd -P)"
SCRATCH="$(mktemp -d "$TMP_ROOT/codex-rui-capture.XXXXXX")"
SCRATCH_REAL="$(cd "$SCRATCH" && pwd -P)"

cleanup() {
    tmux -L "$SOCKET" kill-server 2>/dev/null || true
    rm -rf "$SCRATCH"
}
trap cleanup EXIT

t() { tmux -L "$SOCKET" "$@"; }

wait_for() {
    local deadline=$((SECONDS + $2))
    while ((SECONDS < deadline)); do
        if t capture-pane -t cap -p | grep -qE -- "$1"; then return 0; fi
        sleep 1
    done
    echo "FAIL: no /$1/ on the pane after $2s:" >&2
    t capture-pane -t cap -p | sed '/^[[:space:]]*$/d' | tail -20 >&2
    exit 1
}

capture() {
    sleep 1
    t capture-pane -t cap -p | sed -E -e "s#($TMP_ROOT_REAL|$TMP_ROOT)/[^ ]*#/tmp/project#g" >"$OUT_DIR/$1.txt"
    echo "captured $1"
}

ask() {
    wait_for 'Ask Codex to do anything' 120
    t send-keys -t cap -l "$1"
    sleep 1
    t send-keys -t cap Enter
    wait_for 'tab to add notes' 180
}

finish() {
    wait_for "$1" 60
    wait_for 'Worked for' 120
    sleep 3
}

mkdir -p "$OUT_DIR"
(cd "$SCRATCH" && git init -q . && echo "# scratch" >README.md)
# default_mode_request_user_input offers the tool outside plan mode.
t new -d -s cap -x 200 -y 50 -c "$SCRATCH" "codex -s read-only --enable default_mode_request_user_input"

# Answer the start-up dialogs this version draws, in whatever order they come,
# until the composer has shown with no dialog for five polls in a row. Run
# without the shared background daemon: restarting it would interrupt other
# codex sessions on this host.
idle=0
for _ in $(seq 1 60); do
    pane="$(t capture-pane -t cap -p)"
    case "$pane" in
    *"Run without daemon this time"*) t send-keys -t cap 1 && idle=0 ;;
    *"Skip until next version"*) t send-keys -t cap 2 && idle=0 ;;
    *"Trust this folder?"*) t send-keys -t cap Enter && idle=0 ;;
    *"Hooks need review"*) t send-keys -t cap 3 && idle=0 ;;
    *"Ask Codex to do anything"*) idle=$((idle + 1)) ;;
    esac
    ((idle < 5)) || break
    sleep 2
done

ONE="Call the request_user_input tool now with exactly one question. Question: Which colour should the button be? Options: Red (warm), Green (calm), Blue (cool). Do not run any commands or read any files first."
ask "$ONE"
capture single_untouched
t send-keys -t cap Down && wait_for '› 2\. Green' 10 && capture single_down
t send-keys -t cap Tab && wait_for 'Add notes' 10 && capture single_notes_open
t send-keys -t cap -l "make it pastel" && wait_for 'make it pastel' 10 && capture single_notes_typed
t send-keys -t cap Escape && wait_for 'tab to add notes' 10
t send-keys -t cap 3 && finish 'Questions 1/1 answered' && capture single_answered

TWO="Call the request_user_input tool now with exactly two questions in one call. Question 1: Which colour should the button be? Options: Red (warm), Green (calm), Blue (cool). Question 2: Which size should the button be? Options: Small (compact), Large (prominent). Do not run any commands or read any files first."
ask "$TWO"
capture two_q1
t send-keys -t cap 2 && wait_for 'Question 2/2' 10 && capture two_q2
t send-keys -t cap Left && wait_for 'Question 1/2' 10 && capture two_q1_back
t send-keys -t cap Right && wait_for 'Question 2/2' 10
t send-keys -t cap Tab && wait_for 'Add notes' 10
t send-keys -t cap -l "rounded corners" && wait_for 'rounded corners' 10 && capture two_q2_notes_typed
t send-keys -t cap Enter && finish 'Questions 2/2 answered'

NONE="Call the request_user_input tool now with exactly two questions in one call. Question 1: Which shape should the badge be? Options: Circle (round), Square (sharp). Question 2: Which border should the badge have? Options: Thin (subtle), Thick (bold). Do not run any commands or read any files first."
ask "$NONE"
capture none_q1
t send-keys -t cap Down Down && wait_for '› 3\. None' 10 && capture none_q1_on_none
t send-keys -t cap Tab && wait_for 'Add notes' 10 && capture none_q1_notes_open
t send-keys -t cap -l "a hexagon" && wait_for 'a hexagon' 10 && capture none_q1_notes_typed
t send-keys -t cap Enter && wait_for 'Question 2/2' 10 && capture none_q2
t send-keys -t cap 2 && finish 'Questions 2/2 answered' && capture none_answered

echo "OK: refreshed $OUT_DIR with $(codex --version)"
echo "Remove the scratch trust entry from ~/.codex/config.toml if codex saved one:"
echo "  [projects.\"$SCRATCH_REAL\"]"
