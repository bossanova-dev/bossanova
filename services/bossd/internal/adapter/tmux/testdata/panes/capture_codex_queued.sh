#!/usr/bin/env bash
# capture_codex_queued.sh — refresh codex_queued_midturn_real.txt from a live codex CLI.
#
# PROVENANCE OF THE COMMITTED FIXTURE
#   codex_queued_midturn_real.txt was captured on 2026-10-15 with:
#   codex-cli 0.161.0   (codex --version; also in the fixture's banner row)
#   tmux 3.6b
#   pane geometry 200x50 (bossd's NewSession default)
#
# It launches codex with a scratch CODEX_HOME and a model provider pointing at a
# closed port, so no tokens are spent and the first turn keeps running while
# codex retries. It submits one message, then types codexFixturePayload and
# presses Enter mid-turn: codex accepts it into its "to be submitted" list,
# renders it on an "↳" row, clears the composer to a placeholder, and records no
# user turn in the rollout until the running turn ends.
#
# Usage: capture_codex_queued.sh [output-file]
set -euo pipefail

out=${1:-"$(cd "$(dirname "$0")" && pwd)/codex_queued_midturn_real.txt"}
payload="write a haiku about autumn leaves"
root=$(mktemp -d /tmp/codex-queued-capture.XXXXXX)
sess="codex-queued-capture-$$"
mkdir -p "$root/home" "$root/work"
trap 'tmux kill-session -t "$sess" 2>/dev/null || true; rm -rf "$root"' EXIT

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

tmux send-keys -t "$sess" -l -- "first message keeps the turn running"
tmux send-keys -t "$sess" Enter
sleep 1.5
tmux send-keys -t "$sess" -l -- "$payload"
tmux send-keys -t "$sess" Enter
sleep 0.7

tmux capture-pane -p -t "$sess" | sed 's/[[:space:]]*$//' >"$out"
grep -q 'Messages to be submitted' "$out" || {
  echo "capture shows no queued-messages header; inspect $out" >&2
  exit 3
}
echo "wrote $out"
