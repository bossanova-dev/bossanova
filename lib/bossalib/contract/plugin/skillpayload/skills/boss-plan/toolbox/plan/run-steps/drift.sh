#!/usr/bin/env bash
set -euo pipefail
[ "$#" -eq 0 ] || exit 64
O=
if BOSS_BIN="$(command -v boss 2>/dev/null)"; then
  if O="$("$BOSS_BIN" skills check --gate --heal 2>&1)" ||
    { case "$O" in
        *"unknown flag: --heal"*) O="$("$BOSS_BIN" skills check --gate 2>&1)" ;;
        *) false ;;
      esac; }; then
    if [ -n "$O" ]; then printf '%s\n' "$O" >&2; fi
  else
    case "$O" in
      *--gate*) node "$BOSS_PLAN_TOOLBOX/toolbox-drift.mjs" --toolbox "$BOSS_PLAN_TOOLBOX" || true ;;
      *)
        printf '%s\n' "$O" >&2
        printf '%s\n' "$O" | node "$BOSS_PLAN_TOOLBOX/skill-drift-verdict.mjs" classify --status 1 >&2 || exit 1
        ;;
    esac
  fi
elif [ -f "$BOSS_PLAN_TOOLBOX/toolbox-drift.mjs" ]; then
  node "$BOSS_PLAN_TOOLBOX/toolbox-drift.mjs" --toolbox "$BOSS_PLAN_TOOLBOX" || true
else
  echo "boss-toolbox-drift: (drift helper not installed) — this install predates the check; drift is UNKNOWN, not clean." >&2
fi
printf '%s\n' "${O:-}" | node "$BOSS_PLAN_TOOLBOX/skill-drift-verdict.mjs" reload --core boss-plan --skill-md "${BOSS_PLAN_TOOLBOX%/toolbox}/SKILL.md" || exit "$?"
