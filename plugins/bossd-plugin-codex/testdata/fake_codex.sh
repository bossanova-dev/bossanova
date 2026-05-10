#!/usr/bin/env bash
# fake_codex.sh — hermetic stand-in for the real codex CLI binary.
#
# Used by main_test.go to drive the runner end-to-end without needing
# `codex` to be installed on the test host. Behavior:
#
#   1. If FAKE_CODEX_ARGV_LOG is set, append argv (one arg per line,
#      followed by a blank separator line) to that file. Lets the
#      resume+argv integration test assert the runner's buildArgv output
#      reached the subprocess unchanged.
#   2. Read stdin (the plan / follow-up prompt) into a variable.
#   3. Emit a `thread.started` JSONL event so the runner's
#      SessionIDFromOutput hook discovers the agent-generated UUID.
#   4. Echo "fake codex received: <plan>" so the integration test can
#      confirm stdin propagation.
#   5. Exit 0 cleanly so ExitStatus reports IsComplete=true.
#
# The shape of the `thread.started` event matches the real codex
# `codex exec --json` JSONL surface (see Lane 0 spike).
set -euo pipefail

# 1) Optional argv recording.
if [ -n "${FAKE_CODEX_ARGV_LOG:-}" ]; then
    {
        for arg in "$@"; do
            printf '%s\n' "$arg"
        done
        printf '\n'
    } >> "$FAKE_CODEX_ARGV_LOG"
fi

# 2) Drain stdin into a variable so we can echo it back as proof the runner
# wrote the plan to the subprocess.
plan=$(cat || true)

# 3) thread.started — the runner's SessionIDFromOutput hook reads this.
echo '{"type":"thread.started","thread_id":"fake-uuid-0001"}'

# 4) Echo proof-of-stdin so the integration test can grep the log file.
echo "fake codex received: ${plan}"

# 5) Token-count event so the log isn't trivially short (helps validate
#    that the lineWriter and PostExit hook see realistic byte volumes).
echo '{"type":"event_msg","payload":{"type":"task_complete","duration_ms":1}}'

exit 0
