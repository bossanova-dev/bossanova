#!/usr/bin/env bash
set -euo pipefail
returned= metadata= run_dir= plan= scratch=
while [ "$#" -gt 0 ]; do
  [ "$#" -ge 2 ] && [ -n "$2" ] || exit 64
  case "$1" in
    --returned) returned=$2 ;; --metadata) metadata=$2 ;; --run-dir) run_dir=$2 ;;
    --plan-path) plan=$2 ;; --run-scratch) scratch=$2 ;; *) exit 64 ;;
  esac
  shift 2
done
[ -n "$returned" ] && [ -n "$metadata" ] && [ -n "$run_dir" ] && [ -n "$plan" ] && [ -n "$scratch" ] || exit 64
if ! returned_json=$(cat -- "$returned") || ! node "$BOSS_PLAN_TOOLBOX/plan/plan-run-guards.mjs" adopt-metadata "$metadata" "$returned_json"; then
  echo "dispatch-failure: draft metadata failed plan-run-guards.mjs adopt-metadata — no Linear write, aborting" >&2
  node "$BOSS_PLAN_TOOLBOX/bs-run-sentinel.mjs" cleanup "$run_dir"
  node "$BOSS_PLAN_TOOLBOX/bs-dispatch-await.mjs" guard-discard "$plan" || exit 1
  bash "$BOSS_PLAN_TOOLBOX/plan/boss-plan-run.sh" cleanup --run-scratch "$scratch" || true
  exit 1
fi
