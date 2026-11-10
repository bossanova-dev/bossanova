#!/usr/bin/env bash
set -euo pipefail
scratch=
issue=
parallel=
while [ "$#" -gt 0 ]; do
  [ "$#" -ge 2 ] && [ -n "$2" ] || exit 64
  case "$1" in --run-scratch) scratch=$2 ;; --issue) issue=$2 ;; --parallel) parallel=$2 ;; *) exit 64 ;; esac
  shift 2
done
[ -n "$scratch" ] && [ -n "$issue" ] && [ -n "$parallel" ] || exit 64
node "$BOSS_PLAN_TOOLBOX/plan/plan-child-fanout.mjs" init "$scratch/$issue.fanout-route.json" "$scratch/$issue.fanout-ledger.json" --parallel "$parallel"
exec bash "$BOSS_PLAN_TOOLBOX/plan/run-steps/sentinel-ctx.sh"
