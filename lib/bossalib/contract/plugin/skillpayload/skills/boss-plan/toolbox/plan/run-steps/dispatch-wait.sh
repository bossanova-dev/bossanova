#!/usr/bin/env bash
set -euo pipefail
run_dir=
run_id=
heartbeat=
name=draft
budget=
while [ "$#" -gt 0 ]; do
  [ "$#" -ge 2 ] && [ -n "$2" ] || exit 64
  case "$1" in
    --run-dir) run_dir=$2 ;; --run-id) run_id=$2 ;; --heartbeat) heartbeat=$2 ;;
    --name) name=$2 ;; --budget) budget=$2 ;; *) exit 64 ;;
  esac
  shift 2
done
[ -n "$run_dir" ] && [ -n "$run_id" ] && [ -n "$heartbeat" ] || exit 64
[[ "$name" =~ ^[a-z][a-z0-9-]*$ ]] || exit 64
[ -f "$run_dir/$name.dispatched-at" ] || exit 64
stamp=$(cat "$run_dir/$name.dispatched-at")
[[ "$stamp" =~ ^[0-9]+$ ]] || exit 64
args=(wait "$run_dir" "$run_id" "$name" --heartbeat "$heartbeat" --dispatched-at "$stamp" --while-live)
if [ -n "$budget" ]; then
  [[ "$budget" =~ ^[0-9]+$ ]] || exit 64
  args+=(--budget "$budget")
fi
exec node "$BOSS_PLAN_TOOLBOX/bs-dispatch-await.mjs" "${args[@]}"
