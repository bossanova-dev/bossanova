#!/usr/bin/env bash
# capture-ask-question.sh — refresh testdata/panes/askquestion/ from a live
# agy CLI.
#
# Requirements:
#   - agy on PATH, signed in
#   - tmux on PATH
set -euo pipefail

FIXTURE_DIR="$(cd "$(dirname "$0")" && pwd)"
OUT_DIR="${OUT_DIR:-$FIXTURE_DIR/askquestion}"
SOCKET="agy-ask-capture-$$"
TMP_ROOT="${TMPDIR:-/tmp}"
TMP_ROOT="${TMP_ROOT%/}"
TMP_ROOT_REAL="$(cd "$TMP_ROOT" && pwd -P)"
SCRATCH="$(mktemp -d "$TMP_ROOT/agy-ask-capture.XXXXXX")"
SCRATCH_REAL="$(cd "$SCRATCH" && pwd -P)"
SETTINGS="$HOME/.gemini/antigravity-cli/settings.json"

cleanup() {
    tmux -L "$SOCKET" kill-server 2>/dev/null || true
    if [ -f "$SETTINGS" ]; then
        node -e 'const fs=require("fs"),[p,...dirs]=process.argv.slice(1),j=JSON.parse(fs.readFileSync(p,"utf8"));
j.trustedWorkspaces=(j.trustedWorkspaces||[]).filter(w=>!dirs.includes(w));fs.writeFileSync(p,JSON.stringify(j,null,2)+"\n")' \
            "$SETTINGS" "$SCRATCH" "$SCRATCH_REAL" || true
    fi
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
    t capture-pane -t cap -p |
        sed -E -e "s#($TMP_ROOT_REAL|$TMP_ROOT)/[^ ]*#/tmp/project#g" \
            -e 's#[[:alnum:]._%+-]+@[[:alnum:].-]+\.[[:alpha:]]+#user@example.com#g' >"$OUT_DIR/$1.txt"
    echo "captured $1"
}

ask() {
    wait_for '\? for shortcuts' 120
    t send-keys -t cap -l "$1"
    sleep 1
    t send-keys -t cap Enter
    wait_for '↑/↓ Navigate' 180
}

mkdir -p "$OUT_DIR"
(cd "$SCRATCH" && git init -q . && echo "# scratch" >README.md)
t new -d -s cap -x 200 -y 50 -c "$SCRATCH" agy
wait_for 'Do you trust the contents of this project\?' 60
t send-keys -t cap Enter

ONE="Call the ask_question tool now with exactly one single-select question. Question: Which colour should the button be? Options: Red (warm), Green (calm), Blue (cool). Do not run any commands or read any files first."
ask "$ONE"
capture single
t send-keys -t cap Down Down Down && wait_for '> 4\. Write-in' 10 && capture single_on_writein
t send-keys -t cap Enter && wait_for 'Your answer:' 10
t send-keys -t cap -l "Purple" && wait_for 'Purple' 10 && capture single_writein_typed
t send-keys -t cap Enter

TWO="Call the ask_question tool now with exactly two questions in one call. Question 1 (single-select): Which size should the button be? Options: Small (compact), Large (prominent). Question 2 (multi-select): Which effects should the button have? Options: Shadow (depth), Glow (light), Border (outline). Do not run any commands or read any files first."
ask "$TWO"
capture two_q1
t send-keys -t cap Enter && wait_for 'Question 2/2' 10 && capture two_q2
t send-keys -t cap Space && wait_for '\[x\] Shadow' 10 && capture two_q2_space
t send-keys -t cap Left && wait_for 'Question 1/2' 10 && capture two_q1_back
t send-keys -t cap Right && wait_for 'Question 2/2' 10
t send-keys -t cap Enter && wait_for '\? for shortcuts' 120 && capture two_answered

echo "OK: refreshed $OUT_DIR with agy $(agy --version)"
