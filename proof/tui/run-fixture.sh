#!/usr/bin/env bash
set -euo pipefail
FIXTURE="${1:-demo}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BOSS_DIR="$ROOT/services/boss"
WORK="$(mktemp -d)"
SOCK="$WORK/d.sock"
HOME_DIR="$WORK/home"
mkdir -p "$HOME_DIR"

# Use pre-built binaries when the orchestrator provides them (it builds once,
# before VHS runs, so the in-tape boot Sleep doesn't have to cover a 30-60s
# `go build`). Fall back to building here for a standalone manual smoke run.
if [ -n "${BOSS_PROOF_BOSS_BIN:-}" ] && [ -x "${BOSS_PROOF_BOSS_BIN:-}" ]; then
  BOSS_BIN="$BOSS_PROOF_BOSS_BIN"
else
  BOSS_BIN="$WORK/boss"
  ( cd "$BOSS_DIR" && go build -tags e2e -o "$BOSS_BIN" ./cmd )
fi
if [ -n "${BOSS_PROOF_FIXTURE_DAEMON_BIN:-}" ] && [ -x "${BOSS_PROOF_FIXTURE_DAEMON_BIN:-}" ]; then
  DAEMON_BIN="$BOSS_PROOF_FIXTURE_DAEMON_BIN"
else
  DAEMON_BIN="$WORK/fixture-daemon"
  ( cd "$BOSS_DIR" && go build -o "$DAEMON_BIN" ./cmd/proof-fixture-daemon )
fi

"$DAEMON_BIN" --socket "$SOCK" --fixture "$FIXTURE" --seed-home "$HOME_DIR" \
  >"$WORK/daemon.log" 2>&1 &
DAEMON_PID=$!
trap 'kill "$DAEMON_PID" 2>/dev/null || true; rm -rf "$WORK"' EXIT

# Wait for the daemon's readiness sentinel (the socket file appears before seeding
# completes, so poll the sentinel, not just the socket). Fall back to socket existence.
ready=""
for _ in $(seq 1 100); do
  if grep -q PROOF_FIXTURE_DAEMON_READY "$WORK/daemon.log" 2>/dev/null; then ready=1; break; fi
  sleep 0.1
done
if [ -z "$ready" ]; then
  echo "fixture daemon did not become ready:" >&2
  cat "$WORK/daemon.log" >&2 || true
  exit 1
fi

export BOSS_SOCKET="$SOCK"
export HOME="$HOME_DIR"
export XDG_CONFIG_HOME="$HOME_DIR/.config"
export BOSS_SKIP_SKILLS=1
export BOSS_SKIP_PROVIDER_STARTUP_DAEMON_RESTART=1
export TERM=xterm-256color
unset BOSS_AUTH_E2E_EMAIL
unset BOSS_AUTH_E2E_LOGIN_EMAIL
unset BOSS_SETTINGS_PATH
case "$FIXTURE" in
  demo)
    export BOSS_AUTH_E2E_EMAIL="proof@example.com"
    ;;
  login)
    export BOSS_AUTH_E2E_LOGIN_EMAIL="proof@example.com"
    ;;
  onboarding)
    unset BOSS_SOCKET
    PROVIDER_BIN="$WORK/providers"
    mkdir -p "$PROVIDER_BIN"
    printf '#!/bin/sh\nexit 0\n' >"$PROVIDER_BIN/claude"
    printf '#!/bin/sh\nexit 0\n' >"$PROVIDER_BIN/codex"
    chmod +x "$PROVIDER_BIN/claude" "$PROVIDER_BIN/codex"
    export PATH="$PROVIDER_BIN:$PATH"
    ;;
  *)
    echo "unknown proof fixture: $FIXTURE" >&2
    exit 1
    ;;
esac
# Run boss in the FOREGROUND (not `exec`): exec would replace this shell and
# discard the EXIT trap, orphaning the fixture daemon (it blocks on a signal and
# never self-exits) and leaking $WORK. Running in-foreground lets the trap fire
# when boss exits, killing the daemon and removing the temp dir.
"$BOSS_BIN"
