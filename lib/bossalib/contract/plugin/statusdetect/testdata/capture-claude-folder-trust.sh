#!/usr/bin/env bash
# Capture the folder-trust dialog without answering it or sending any keys.
set -euo pipefail
FIXTURE_DIR="$(cd "$(dirname "$0")" && pwd)"
WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/bos2010-trust.XXXXXX")"
SOCKET="bos2010-trust-$$"
cleanup() {
    tmux -L "$SOCKET" kill-server 2>/dev/null || true
    rm -rf "$WORKDIR"
}
trap cleanup EXIT
tmux -L "$SOCKET" new-session -d -s capture -x 160 -y 40 -c "$WORKDIR" 'claude --dangerously-skip-permissions'
# Startup is bounded; no input is sent, including when onboarding or auth blocks it.
for attempt in $(seq 1 20); do
    tmux -L "$SOCKET" capture-pane -t capture -p >"$WORKDIR/plain.txt"
    if rg -q '^ *[❯ ]*No, exit *$' "$WORKDIR/plain.txt" &&
        rg -q '^ *[❯ ]*Yes, I trust this folder *$' "$WORKDIR/plain.txt"; then
        tmux -L "$SOCKET" capture-pane -t capture -p -e >"$WORKDIR/styled.txt"
        cp "$WORKDIR/plain.txt" "$FIXTURE_DIR/claude_folder_trust_real.txt"
        cp "$WORKDIR/styled.txt" "$FIXTURE_DIR/claude_folder_trust_styled_real.txt"
        {
            date -u '+Captured: %Y-%m-%dT%H:%M:%SZ'
            claude --version
            tmux -V
            echo 'Geometry: 160x40; fresh temporary working directory; private tmux socket.'
            echo 'Plain: capture-pane -p; styled: capture-pane -p -e.'
            echo 'No keys sent; tmux server killed without answering the dialog.'
            echo 'Recapture with capture-claude-folder-trust.sh.'
        } >"$FIXTURE_DIR/claude_folder_trust_provenance.txt"
        cat "$FIXTURE_DIR/claude_folder_trust_provenance.txt"
        exit 0
    fi
    sleep 1
done
echo 'No folder-trust dialog captured; inspect startup/authentication. Fixtures unchanged.' >&2
exit 1
