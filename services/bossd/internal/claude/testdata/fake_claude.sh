#!/bin/sh
# fake_claude.sh — a test double for the `claude` binary.
#
# Accepts (and ignores) the real claude-cli flags used by the Runner:
#   --print --verbose --output-format stream-json [--resume <uuid>]
#   [--session-id <uuid>] [--dangerously-skip-permissions]
#
# Behaviour is env-driven so a single script can exercise the full matrix of
# runner scenarios without per-test scripts:
#
#   FAKE_CLAUDE_LINES=<n>         emit n lines of valid stream-json (default 1)
#   FAKE_CLAUDE_DELAY_MS=<ms>     sleep between lines (default 0)
#   FAKE_CLAUDE_START_DELAY_MS=<ms>  sleep before emitting the first line
#                                   (default 0). Lets tests subscribe before
#                                   emission begins, avoiding races.
#   FAKE_CLAUDE_EXIT=<code>       exit code (default 0)
#   FAKE_CLAUDE_IGNORE_SIGTERM=1  ignore SIGTERM — forces the runner's
#                                 force-kill path after its 10s timeout
#   FAKE_CLAUDE_ECHO_ARGS_FILE=<path>  write the argv (one per line) so tests
#                                     can assert flags like --resume pass through
#
# Reads (and discards) all of stdin — the runner pipes the plan there.

set -u

lines="${FAKE_CLAUDE_LINES:-1}"
delay_ms="${FAKE_CLAUDE_DELAY_MS:-0}"
start_delay_ms="${FAKE_CLAUDE_START_DELAY_MS:-0}"
exit_code="${FAKE_CLAUDE_EXIT:-0}"
ignore_sigterm="${FAKE_CLAUDE_IGNORE_SIGTERM:-0}"
echo_args_file="${FAKE_CLAUDE_ECHO_ARGS_FILE:-}"

if [ -n "$echo_args_file" ]; then
  for a in "$@"; do
    printf '%s\n' "$a" >> "$echo_args_file"
  done
fi

if [ "$ignore_sigterm" = "1" ]; then
  # Catch SIGTERM and do nothing — the runner will force-kill with SIGKILL
  # after its 10s graceful-shutdown timeout.
  trap '' TERM
fi

# Drain stdin so writers don't block on a full pipe.
cat > /dev/null &
drain_pid=$!

# Optional pre-emission delay — gives callers a window to subscribe before
# the first line is broadcast.
if [ "$start_delay_ms" -gt 0 ]; then
  if sleep "0.$(printf '%03d' "$start_delay_ms")" 2>/dev/null; then :; else sleep 1; fi
fi

i=1
while [ "$i" -le "$lines" ]; do
  # Emit plausible stream-json so the runner's line splitter exercises
  # realistic payloads (the runner itself doesn't parse the JSON, but the
  # shape mirrors production output).
  printf '{"type":"assistant","line":%d}\n' "$i"
  if [ "$delay_ms" -gt 0 ]; then
    # POSIX sleep only accepts integers on some platforms; fall back to
    # 1s resolution when sub-second sleep isn't available.
    if sleep "0.$(printf '%03d' "$delay_ms")" 2>/dev/null; then :; else sleep 1; fi
  fi
  i=$((i + 1))
done

wait "$drain_pid" 2>/dev/null || true

# If we're in ignore-sigterm mode, sit idle until the runner force-kills us.
if [ "$ignore_sigterm" = "1" ]; then
  while :; do sleep 1; done
fi

exit "$exit_code"
