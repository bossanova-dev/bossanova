#!/usr/bin/env bash
# fake_claude.sh — stream-json test double for the Claude CLI.
#
# Used by E2E tests that need to drive the autopilot/repair plugins without
# spawning a real Claude process. The daemon's claude.Runner expects a binary
# that accepts the standard `--print --verbose --output-format stream-json`
# flags (plus optional `--resume <uuid>` and `--session-id <uuid>`), reads the
# prompt from stdin, and emits JSONL lines to stdout. This script satisfies
# that contract.
#
# Environment variables (all optional):
#   FAKE_CLAUDE_LINES        (default 3)   number of JSONL lines to emit
#   FAKE_CLAUDE_EXIT         (default 0)   exit code on clean completion
#   FAKE_CLAUDE_SLEEP_MS     (default 0)   delay between lines in milliseconds
#                                          (useful for pause/cancel timing tests)
#   FAKE_CLAUDE_SIGTERM_EXIT (default 143) exit code when SIGTERM/SIGINT received
#   FAKE_CLAUDE_LOG_FILE                   append args + stdin to this file for
#                                          assertion; default drains stdin to
#                                          /dev/null
#   FAKE_CLAUDE_HANDOFF_DIR                write a unique handoff markdown file
#                                          into this absolute directory before
#                                          exiting (so the autopilot handoff
#                                          loop has something to pick up)
#   FAKE_CLAUDE_SOFT_FAIL    (default 0)   when "1", emit "Unknown skill: fake"
#                                          as the final line so the orchestrator
#                                          treats it as a soft failure
#
# Unknown command-line flags are ignored — the real Claude CLI accepts many
# flags we don't need to model.

set -u

sigterm_exit="${FAKE_CLAUDE_SIGTERM_EXIT:-143}"

# Exit directly from the trap so long sleep()s are interruptible.
on_signal() {
  exit "$sigterm_exit"
}
trap 'on_signal' TERM INT

lines="${FAKE_CLAUDE_LINES:-3}"
sleep_ms="${FAKE_CLAUDE_SLEEP_MS:-0}"
exit_code="${FAKE_CLAUDE_EXIT:-0}"
soft_fail="${FAKE_CLAUDE_SOFT_FAIL:-0}"

# Log args + stdin if requested, else drain stdin so the parent's write to
# our stdin pipe doesn't block on a full OS buffer.
if [ -n "${FAKE_CLAUDE_LOG_FILE:-}" ]; then
  {
    printf '=== fake_claude invocation (pid %s) ===\n' "$$"
    printf 'args: %s\n' "$*"
    printf 'stdin:\n'
    cat -
    printf '\n=== end ===\n'
  } >> "$FAKE_CLAUDE_LOG_FILE"
else
  cat > /dev/null
fi

i=1
while [ "$i" -le "$lines" ]; do
  printf '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"fake line %d"}]},"session_id":"fake"}\n' "$i"
  if [ "$sleep_ms" -gt 0 ]; then
    # bash's `sleep` accepts fractional seconds on macOS and Linux; awk is
    # the portable way to divide integer ms by 1000 without relying on bc.
    secs=$(awk -v ms="$sleep_ms" 'BEGIN { printf "%.3f", ms / 1000 }')
    sleep "$secs"
  fi
  i=$((i + 1))
done

if [ "$soft_fail" = "1" ]; then
  printf '{"type":"error","error":"Unknown skill: fake"}\n'
fi

if [ -n "${FAKE_CLAUDE_HANDOFF_DIR:-}" ]; then
  mkdir -p "$FAKE_CLAUDE_HANDOFF_DIR"
  # Unique per-invocation filename. macOS `date +%N` isn't supported and
  # macOS awk lacks systime(); bash's $RANDOM plus `date +%s` is enough to
  # disambiguate rapid invocations inside one test.
  handoff_path="${FAKE_CLAUDE_HANDOFF_DIR}/handoff-$$-$(date +%s)-${RANDOM}.md"
  {
    printf '# Handoff (fake_claude)\n\n'
    printf 'pid: %s\n' "$$"
    printf 'args: %s\n' "$*"
  } > "$handoff_path"
fi

exit "$exit_code"
