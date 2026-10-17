#!/usr/bin/env bash
# capture_typed_slash_menu.sh refreshes claude_typed_slash_menu_real.txt.
#
# Usage: capture_typed_slash_menu.sh AGENT_LOG
#
# The committed fixture was rendered on 2026-10-17 from the bossd agent log
# (~/.bossanova/agent-logs/<chat>.log, the pane's pipe-pane output) of a repair
# chat that failed to start: bossd typed the bare command /boss-repair into
# Claude Code 2.1.302 in a 200-column pane, and Claude opened its slash-command
# menu below the input box with the selected entry marked "❯". The log is
# replayed into a fresh tmux pane of the same width and the screen captured,
# which shows the typed line and the menu as the draw check saw them. Typing a
# bare slash command into a live claude reproduces the same screen; this route
# needs no claude and accepts no dialog.
set -euo pipefail
LOG=$1
L=capture-typed-slash-menu
S=ctsm
OUT=$(cd "$(dirname "$0")" && pwd)/claude_typed_slash_menu_real.txt
trap 'tmux -L $L kill-server 2>/dev/null' EXIT
tmux -L $L -f /dev/null new-session -d -s $S -x 200 -y 50 "cat '$LOG'; sleep 30"
sleep 1
tmux -L $L capture-pane -p -t $S | awk '{ rows[NR] = $0; if ($0 ~ /[^ ]/) last = NR } END { for (i = 1; i <= last; i++) print rows[i] }' >"$OUT"
grep -q '^  ❯ /' "$OUT" || { echo "no slash-command menu in the replayed screen" >&2; exit 1; }
