#!/usr/bin/env bash
# capture_codex_stuck_composer.sh — refresh the codex stuck-composer fixtures from a live codex CLI.
#
# PROVENANCE OF THE COMMITTED FIXTURES
#   captured on 2026-10-16 with codex-cli 0.161.0, tmux 3.6b, pane 200x50
#   (bossd's NewSession default), launched with tui.disable_paste_burst=true as
#   the codex plugin launches it.
#
# The shape comes from a verify chat that collected ten unsubmitted
# "$boss-verify 3105 --claim <token>" lines in its composer over eight hours
# while every send logged delivery_state=submitted. Codex draws the first
# line after its "›" glyph and each further line as an indented continuation
# row, so the glyph row always holds the OLDEST stuck line.
#
# It launches codex with a scratch CODEX_HOME and a model provider pointing at
# a closed port, so no tokens are spent and no turn ever starts: Ctrl-J inserts
# a composer newline without submitting.
#
#   codex_placeholder_styled_real.txt          idle empty composer, capture-pane -e
#   codex_stuck_composer_stale_real.txt        nine stuck lines, plain
#   codex_stuck_composer_stale_styled_real.txt nine stuck lines, capture-pane -e
#   codex_stuck_composer_real.txt              the tenth line typed on its own row
#   codex_stuck_composer_appended_real.txt     nine lines, the tenth typed onto the last row
#
# Usage: capture_codex_stuck_composer.sh [output-dir]
set -euo pipefail

out=${1:-"$(cd "$(dirname "$0")" && pwd)"}
root=$(mktemp -d /tmp/codex-stuck-capture.XXXXXX)
sess="codex-stuck-capture-$$"
mkdir -p "$root/home" "$root/work"
trap 'tmux kill-session -t "$sess" 2>/dev/null || true; rm -rf "$root"' EXIT

stale=(308f9f37 4f325c1b 9c02d7e1 1b7a4f90 e35c8d22 70f1a6b3 c4d9e015 2a86f7cd 5e0b3a91)
payload='$boss-verify 3105 --claim a414c806'

cat >"$root/launch.sh" <<EOF
#!/usr/bin/env bash
export CODEX_HOME=$root/home
export DEAD_KEY=x
cd $root/work
exec codex \\
  -c model_provider=dead \\
  -c 'model_providers.dead.name="dead"' \\
  -c 'model_providers.dead.base_url="http://127.0.0.1:1/v1"' \\
  -c 'model_providers.dead.env_key="DEAD_KEY"' \\
  -c 'model_providers.dead.wire_api="responses"' \\
  -c 'projects."$root/work".trust_level="trusted"' \\
  -c tui.disable_paste_burst=true
EOF
chmod +x "$root/launch.sh"
tmux new-session -d -s "$sess" -x 200 -y 50 "$root/launch.sh"

for _ in $(seq 60); do
  if tmux capture-pane -p -t "$sess" | grep -q '^›'; then break; fi
  sleep 0.5
done
tmux capture-pane -p -t "$sess" | grep -q '^›' || {
  echo "codex drew no composer within 30s" >&2
  exit 2
}
sleep 1

plain() { tmux capture-pane -p -t "$sess" | sed 's/[[:space:]]*$//' >"$out/$1"; }
styled() { tmux capture-pane -e -p -t "$sess" >"$out/$1"; }

clear_composer() {
  for _ in $(seq 24); do
    tmux send-keys -t "$sess" C-u
    tmux send-keys -t "$sess" BSpace
  done
  sleep 0.5
}

type_stale() {
  local first=1
  for tok in "${stale[@]}"; do
    if [ "$first" -eq 0 ]; then tmux send-keys -t "$sess" C-j; fi
    first=0
    tmux send-keys -t "$sess" -l -- "\$boss-verify 3105 --claim $tok"
    sleep 0.2
  done
  sleep 1
}

styled codex_placeholder_styled_real.txt

type_stale
plain codex_stuck_composer_stale_real.txt
styled codex_stuck_composer_stale_styled_real.txt

tmux send-keys -t "$sess" C-j
tmux send-keys -t "$sess" -l -- "$payload"
sleep 1
plain codex_stuck_composer_real.txt

clear_composer
type_stale
tmux send-keys -t "$sess" -l -- "$payload"
sleep 1
plain codex_stuck_composer_appended_real.txt

grep -q 'Ask Codex\|›' "$out/codex_placeholder_styled_real.txt" || {
  echo "placeholder capture shows no composer; inspect $out" >&2
  exit 3
}
echo "wrote fixtures to $out"
