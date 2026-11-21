#!/usr/bin/env bash
set -euo pipefail
scratch=
while [ "$#" -gt 0 ]; do
  case "$1" in --run-scratch) [ "$#" -ge 2 ] || exit 64; scratch=$2; shift 2 ;; *) exit 64 ;; esac
done
case "$scratch" in .linear-plans/run-?*) ;; *) exit 64 ;; esac
case "$scratch" in *..*|*\\*) exit 64 ;; esac
id=${scratch#.linear-plans/run-}
expected=$(node "$BOSS_PLAN_TOOLBOX/plan/plan-scratch-paths.mjs" run-dir "$id") || exit 64
[ "$scratch" = "$expected" ] || exit 64
# Refuse a symlinked root, which would redirect removal into another directory.
[ ! -L .linear-plans ] || exit 64
pin_root=$(node "$BOSS_PLAN_TOOLBOX/plan/plan-scratch-paths.mjs" pin-dir "$id") || exit 64
if ! result=$(node "$BOSS_PLAN_TOOLBOX/skill-pin.mjs" create --skill boss-plan --skills-home "$BOSS_SKILLS_HOME" --pin-root "$pin_root" 2>&1); then
  printf 'BLOCKED: %s\n' "$result" >&2
  exit 1
fi
# Age alone cannot establish that an interactive run has finished waiting.
# Unknown ownership is retained; only unattended runs opt into idle cleanup.
if [ "${BOSS_UNATTENDED:-${BOSS_CRON:-false}}" = "true" ]; then
  touch -- "$pin_root/.unattended"
fi
printf 'pin: %s/boss-plan\n%s\n' "$pin_root" "$result"
