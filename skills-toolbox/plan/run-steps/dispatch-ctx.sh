#!/usr/bin/env bash
set -euo pipefail
precheck=
scratch=
while [ "$#" -gt 0 ]; do
  [ "$#" -ge 2 ] && [ -n "$2" ] || exit 64
  case "$1" in --precheck) precheck=$2 ;; --run-scratch) scratch=$2 ;; *) exit 64 ;; esac
  shift 2
done
[ -n "$precheck" ] && [ -n "$scratch" ] || exit 64
sentinel="$BOSS_PLAN_TOOLBOX/bs-run-sentinel.mjs"
[ -f "$sentinel" ] || { echo "BLOCKED: bs-run-sentinel.mjs missing" >&2; exit 1; }
run_scratch_id=${scratch#.linear-plans/run-}
[ "$scratch" = "$(node "$BOSS_PLAN_TOOLBOX/plan/plan-scratch-paths.mjs" run-dir "$run_scratch_id")" ] || exit 64
plan=$(node "$BOSS_PLAN_TOOLBOX/plan/plan-slug.mjs" plan-path --run-id "$run_scratch_id" --issue-json "$precheck")
printf 'PLAN_PATH=%s\n' "$plan"
bash "$BOSS_PLAN_TOOLBOX/plan/boss-plan-run.sh" sentinel-ctx
printf 'RUN_SCRATCH=%s\nDISPATCH_FAILURE=dispatch-failure\n' "$scratch"
