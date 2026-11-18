#!/usr/bin/env bash
set -euo pipefail
scratch=
run_dir=
run_id=
while [ "$#" -gt 0 ]; do
  [ "$#" -ge 2 ] && [ -n "$2" ] || exit 64
  case "$1" in
    --run-scratch) scratch=$2 ;; --run-dir) run_dir=$2 ;; --run-id) run_id=$2 ;; *) exit 64 ;;
  esac
  shift 2
done
[ -n "$scratch" ] && [ -n "$run_dir" ] && [ -n "$run_id" ] || exit 64
payload=$(node "$BOSS_PLAN_TOOLBOX/bs-run-sentinel.mjs" read "$run_dir" "$run_id" draft | jq -c '.payload // {}')
exec node "$BOSS_PLAN_TOOLBOX/plan/plan-run-guards.mjs" epic-reverify-bundle --run-scratch "$scratch" --payload "$payload"
