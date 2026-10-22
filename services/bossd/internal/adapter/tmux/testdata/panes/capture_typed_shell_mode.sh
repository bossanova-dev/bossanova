#!/usr/bin/env bash
# capture_typed_shell_mode.sh refreshes claude_typed_shell_mode_real.txt: a
# Claude pane after "!echo hi" is typed with no Enter. The leading "!" switches
# the composer to shell mode, which Claude draws as a "!" glyph in place of the
# prompt marker, with a "! for shell mode" hint below the box.
#
# The committed fixture was captured on 2026-10-17 with Claude Code 2.1.302 and
# tmux 3.7c under bossd's extended-keys options. It needs an authenticated claude
# on PATH, runs in an empty temp dir and never presses Enter.
set -u
L=capture-typed-shell-mode
S=ctsm
OUT_DIR=$(cd "$(dirname "$0")" && pwd)
WORK=$(mktemp -d)
trap 'tmux -L $L kill-server 2>/dev/null; rm -rf "$WORK"' EXIT

tmux -L $L -f /dev/null new-session -d -s $S -x 120 -y 30 -c "$WORK" "claude --dangerously-skip-permissions"
tmux -L $L set-option -t $S extended-keys always
tmux -L $L set-option -sa terminal-features ',xterm*:extkeys'
tmux -L $L set-option -g extended-keys-format csi-u
for _ in $(seq 1 60); do
  sleep 0.25
  tmux -L $L capture-pane -p -t $S | grep -q '^❯' && break
done
tmux -L $L send-keys -t $S -l -- '!echo hi'
sleep 1.5
tmux -L $L capture-pane -p -t $S >"$OUT_DIR/claude_typed_shell_mode_real.txt"
grep -q '^!.*echo hi' "$OUT_DIR/claude_typed_shell_mode_real.txt" || {
  echo "the composer did not draw the shell-mode line" >&2
  exit 1
}
