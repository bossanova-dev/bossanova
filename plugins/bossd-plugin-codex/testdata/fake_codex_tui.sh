#!/usr/bin/env bash
# fake_codex_tui.sh — hermetic stand-in for interactive `codex` in tmux.
set -euo pipefail

log_argv() {
    if [ -n "${FAKE_CODEX_TUI_ARGV_LOG:-}" ]; then
        {
            printf 'PWD=%s\n' "$PWD"
            for arg in "$@"; do
                printf '%s\n' "$arg"
            done
            printf '\n'
        } >> "$FAKE_CODEX_TUI_ARGV_LOG"
    fi
}

rollout_path_for() {
    local id="$1"
    printf '%s/.codex/sessions/2026/05/11/rollout-2026-05-11T00-00-00-%s.jsonl' "$HOME" "$id"
}

sleep_long_enough() {
    sleep "${FAKE_CODEX_TUI_SLEEP_SECONDS:-30}"
}

log_argv "$@"

if [ "${1:-}" = "resume" ]; then
    id="${2:-}"
    if [ -z "$id" ]; then
        echo "missing resume id" >&2
        exit 2
    fi
    rollout="$(rollout_path_for "$id")"
    if [ ! -s "$rollout" ]; then
        echo "missing rollout for $id" >&2
        exit 3
    fi
    printf 'RESUMED %s\n' "$id"
    # The glyph below is U+276F, which is CLAUDE's ReadyMarker
    # (bossd-plugin-claude/server.go); codex's own marker is U+203A. Real codex
    # does not print this, and nothing on the resume path the e2e drives waits
    # on it for input delivery: BuildInteractiveCommand returns
    # ConsumesInitialInput for a resume that carries a prompt, so StartTmuxChat
    # skips injectTmuxChatInput altogether.
    #
    # It is emitted anyway, and it is deliberately NOT codex's own marker:
    # swapping it to U+203A hangs the e2e. Whatever resolves readiness in that
    # scenario is looking for the claude glyph, and untangling that is a
    # separate question from what this fixture is for. Leave it as U+276F.
    printf '\xe2\x9d\xaf\n'
    sleep_long_enough
    exit 0
fi

id="${FAKE_CODEX_TUI_ID_PREFIX:-fake-codex}-$$-$(date +%s%N)"
rollout="$(rollout_path_for "$id")"
mkdir -p "$(dirname "$rollout")"
printf '{"type":"session_meta","payload":{"id":"%s","cwd":"%s","originator":"codex-tui","cli_version":"test"}}\n' "$id" "$PWD" > "$rollout"
sleep_long_enough
