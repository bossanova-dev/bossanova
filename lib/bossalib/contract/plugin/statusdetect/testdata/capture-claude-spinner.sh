#!/usr/bin/env bash
# Refresh real Claude spinner panes; do not hand-edit captures, recapture them.
# Provenance is written to claude_spinner_provenance.txt with CLI/tmux versions,
# capture date and geometry. Credentials come only from the operator environment:
# the private tmux server inherits it, so no secret is ever passed as an argument.
set -euo pipefail
# Probe the same inherited environment the capture will use before starting tmux.
# Suppress provider output: errors can echo the proxy URL's private path token.
PROXY_ENV=unset
if [ -n "${ANTHROPIC_BASE_URL:-}" ]; then PROXY_ENV=set; fi
if ! PREFLIGHT_OUTPUT="$(claude -p 'reply with the word ok' 2>/dev/null)" ||
    ! printf '%s\n' "$PREFLIGHT_OUTPUT" | rg -qi '\bok\b'; then
    printf 'FAIL: Claude round trip failed (ANTHROPIC_BASE_URL %s); the CLI auth-status report is not evidence inside bossd.\n' "$PROXY_ENV" >&2
    exit 1
fi
unset PREFLIGHT_OUTPUT
FIXTURE_DIR="$(cd "$(dirname "$0")" && pwd)"
# Claude's banner prints the workdir into the scrollback; a per-user $TMPDIR
# would bake a host-specific path into every committed pane.
WORKDIR="$(mktemp -d /tmp/bos1996-spinner.XXXXXX)"
SOCKET="bos1995-spinner-$$"
cleanup() {
    tmux -L "$SOCKET" kill-server 2>/dev/null || true
    rm -rf "$WORKDIR"
}
trap cleanup EXIT
BUSY_PROMPT='Run this bash command in the foreground and tell me the number it prints: find / -xdev -type f 2>/dev/null | wc -l'
tmux -L "$SOCKET" new-session -d -s capture -x 200 -y 50 -c "$WORKDIR" 'claude --dangerously-skip-permissions'
sleep 12
# A fresh workdir opens on the folder-trust dialog, whose default answer exits.
if tmux -L "$SOCKET" capture-pane -t capture -p | rg -q 'I trust this folder'; then
    tmux -L "$SOCKET" send-keys -t capture Down
    sleep 1
    tmux -L "$SOCKET" send-keys -t capture Enter
    sleep 6
fi
tmux -L "$SOCKET" send-keys -t capture -l "$BUSY_PROMPT"
tmux -L "$SOCKET" send-keys -t capture Enter
capture() { tmux -L "$SOCKET" capture-pane -t capture -p -S -1000 "$@"; }
# The first frames render the verb alone; wait (bounded) for the counter group.
for attempt in $(seq 1 30); do
    if capture | rg -q '^[ \t]*[·✻✽✢✳✶∗*] [^…]+… \((?:[0-9]+h )?(?:[0-9]+m )?[0-9]+s'; then break; fi
    sleep 1
done
# Let one-time transitions (the tool's run-in-background hint, startup toasts)
# land before the pair, so the frames differ only by redraw.
sleep 12
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
# Mirrors claudeRunningToolRe: a running tool's blinking glyph and timer.
tool = re.compile(r'^(?:⏺ |  )(\S[^\n]*) · (?:\d+h )?(?:\d+m )?\d+s[ \t]*$', re.M)
a, b = [(root / name).read_text() for name in ('redraw_a_plain.txt', 'redraw_b_plain.txt')]
if not spinner.search(a) or not spinner.search(b):
    raise SystemExit('FAIL: no live spinner captured; inspect authentication/startup')
chrome = lambda s: tool.sub(r'  \1 · #', spinner.sub('', s))
if a == b or chrome(a) != chrome(b):
    raise SystemExit('FAIL: redraws must differ only in spinner rows; recapture')
CHECK
tmux -L "$SOCKET" resize-window -t capture -x 43 -y 50
sleep 2
capture >"$WORKDIR/narrow_plain.txt"
tmux -L "$SOCKET" resize-window -t capture -x 200 -y 50
# Bound the wait for the foreground command to finish.
for attempt in $(seq 1 150); do
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
    echo 'Retained fallback: claude_working_empty_composer_real.txt and claude_swallowed_enter_midturn_real.txt are byte-for-byte real Claude Code 2.1.220 panes (tmux 3.6b, 200x50, captured 2026-07-31) from services/bossd/internal/adapter/tmux/testdata/panes/.'
    echo 'Do not hand-edit; recapture with capture-claude-spinner.sh.'
} >"$FIXTURE_DIR/claude_spinner_provenance.txt"
cat "$FIXTURE_DIR/claude_spinner_provenance.txt"
