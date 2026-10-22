#!/usr/bin/env bash
# capture_typed_slash.sh refreshes claude_typed_slash_undrawn_real.txt and
# claude_typed_slash_enter_newline_real.txt.
#
# The committed fixtures were captured on 2026-10-17 on a loaded macOS host
# (load average ~11) with Claude Code 2.1.302 and tmux 3.8, under bossd's
# extended-keys options. They show what happens when a slash command is typed
# into a Claude pane that has just drawn its composer and Enter follows at once:
#   undrawn  the pane right after Enter, still showing the placeholder because
#            Claude has not read the typed keys yet
#   newline  the same pane 3s later: Claude read the line and its Enter in one
#            chunk, took the Enter as a newline, and left the line unsubmitted
# The race is load-dependent, so the script retries until one trial lands it.
# It needs an authenticated claude on PATH. The slash command names a real skill
# because an unknown command does not reproduce; it runs in an empty temp dir and
# is interrupted with Escape when it does submit.
set -u
L=capture-typed-slash
S=cts
OUT_DIR=$(cd "$(dirname "$0")" && pwd)
WORK=$(mktemp -d)
PAYLOAD='/boss-plan --exclude-creator alice --exclude-assignee bob'
trap 'tmux -L $L kill-server 2>/dev/null; rm -rf "$WORK"' EXIT

composer_row() {
  printf '%s\n' "$1" | grep '^❯' | tail -1
}

for trial in $(seq 1 30); do
  tmux -L $L kill-server 2>/dev/null
  (cd "$WORK" && tmux -L $L -f /dev/null new-session -d -s $S -x 160 -y 40 "claude --dangerously-skip-permissions")
  tmux -L $L set-option -t $S extended-keys always
  tmux -L $L set-option -sa terminal-features ',xterm*:extkeys'
  tmux -L $L set-option -g extended-keys-format csi-u
  for _ in $(seq 1 60); do
    sleep 0.25
    tmux -L $L capture-pane -p -t $S | grep -q '❯' && break
  done
  if tmux -L $L capture-pane -p -t $S | grep -q 'trust this folder'; then
    tmux -L $L send-keys -t $S Down
    sleep 0.3
    tmux -L $L send-keys -t $S Enter
    for _ in $(seq 1 40); do
      sleep 0.25
      tmux -L $L capture-pane -p -t $S | grep -q 'trust this folder' || break
    done
  fi
  tmux -L $L send-keys -t $S -l -- "$PAYLOAD"
  tmux -L $L send-keys -t $S Enter
  pre=$(tmux -L $L capture-pane -p -t $S)
  sleep 3
  post=$(tmux -L $L capture-pane -p -t $S)
  tmux -L $L send-keys -t $S Escape
  if composer_row "$pre" | grep -q '^❯ Try' && composer_row "$post" | grep -q 'boss-plan'; then
    printf '%s\n' "$pre" >"$OUT_DIR/claude_typed_slash_undrawn_real.txt"
    printf '%s\n' "$post" >"$OUT_DIR/claude_typed_slash_enter_newline_real.txt"
    echo "captured on trial $trial"
    exit 0
  fi
done
echo "no trial reproduced the race; rerun on a busier host" >&2
exit 1
