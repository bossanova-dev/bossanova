#!/usr/bin/env bash
# Refresh real Claude spinner panes; do not hand-edit captures, recapture them.
# Provenance is written to claude_spinner_provenance.txt with CLI/tmux versions,
# capture date and geometry. Credentials come only from the operator environment.
set -euo pipefail
FIXTURE_DIR="$(cd "$(dirname "$0")" && pwd)"
WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/bos1995-spinner.XXXXXX")"
SOCKET="bos1995-spinner-$$"
cleanup() {
    tmux -L "$SOCKET" kill-server 2>/dev/null || true
    rm -rf "$WORKDIR"
}
trap cleanup EXIT
BUSY_PROMPT='Run this bash command in the foreground and tell me the number it prints: find / -xdev -type f 2>/dev/null | wc -l'
env_args=()
[ -z "${ANTHROPIC_API_KEY:-}" ] || env_args+=(-e "ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY")
[ -z "${ANTHROPIC_BASE_URL:-}" ] || env_args+=(-e "ANTHROPIC_BASE_URL=$ANTHROPIC_BASE_URL")
tmux -L "$SOCKET" new-session -d -s capture -x 200 -y 50 -c "$WORKDIR" "${env_args[@]}" 'claude --dangerously-skip-permissions'
sleep 12
tmux -L "$SOCKET" send-keys -t capture -l "$BUSY_PROMPT"
tmux -L "$SOCKET" send-keys -t capture Enter
sleep 8
capture() { tmux -L "$SOCKET" capture-pane -t capture -p -S -1000 "$@"; }
capture >"$WORKDIR/midturn_plain.txt"
capture -e >"$WORKDIR/midturn_styled.txt"
capture >"$WORKDIR/redraw_a_plain.txt"
sleep 3
capture >"$WORKDIR/redraw_b_plain.txt"
# Only real spinner-row changes are acceptable in the redraw pair.
python3 - "$WORKDIR" <<'CHECK'
import pathlib, re, sys
root = pathlib.Path(sys.argv[1])
spinner = re.compile(r'^[ \t]*[·✻✽✢✳✶∗*] [^…\n]+… \((?:\d+h )?(?:\d+m )?\d+s(?: · [^\n]*)?\)[ \t]*$', re.M)
a, b = [(root / name).read_text() for name in ('redraw_a_plain.txt', 'redraw_b_plain.txt')]
if not spinner.search(a) or not spinner.search(b):
    raise SystemExit('FAIL: no live spinner captured; inspect authentication/startup')
if a == b or spinner.sub('', a) != spinner.sub('', b):
    raise SystemExit('FAIL: redraws must differ only in spinner rows; recapture')
CHECK
tmux -L "$SOCKET" resize-window -t capture -x 43 -y 50
sleep 2
capture >"$WORKDIR/narrow_plain.txt"
tmux -L "$SOCKET" resize-window -t capture -x 200 -y 50
# Bound the wait for the foreground command to finish.
for attempt in $(seq 1 90); do
    capture >"$WORKDIR/turn_done_plain.txt"
    if rg -q '^[✻✽✢✳✶∗] [^…]* for [0-9]' "$WORKDIR/turn_done_plain.txt"; then break; fi
    sleep 2
done
rg -q '^[✻✽✢✳✶∗] [^…]* for [0-9]' "$WORKDIR/turn_done_plain.txt"
for name in midturn_plain midturn_styled redraw_a_plain redraw_b_plain narrow_plain; do
    cp "$WORKDIR/$name.txt" "$FIXTURE_DIR/claude_spinner_$name.txt"
done
cp "$WORKDIR/turn_done_plain.txt" "$FIXTURE_DIR/claude_turn_done_plain.txt"
{
    date -u '+Captured: %Y-%m-%dT%H:%M:%SZ'
    claude --version
    tmux -V
    echo 'Geometry: 200x50; narrow: 43x50'
    echo 'Plain: capture-pane -p -S -1000; styled: capture-pane -p -S -1000 -e'
    echo 'Midturn above composer/status; redraw pair same foreground turn; done same session.'
    echo 'Do not hand-edit; recapture with capture-claude-spinner.sh.'
} >"$FIXTURE_DIR/claude_spinner_provenance.txt"
cat "$FIXTURE_DIR/claude_spinner_provenance.txt"
