#!/usr/bin/env bash
set -euo pipefail
skill=boss-plan
while [ "$#" -gt 0 ]; do
  case "$1" in --skill) [ "$#" -ge 2 ] && [ -n "$2" ] || exit 64; skill=$2; shift 2 ;; *) exit 64 ;; esac
done
sentinel="$BOSS_PLAN_TOOLBOX/bs-run-sentinel.mjs"
ctx=$(node "$sentinel" make-ctx "$skill")
IFS=$'\t' read -r run_id run_dir <<< "$ctx"
[ -n "$run_id" ] && [ -n "$run_dir" ] || exit 1
printf 'RUN_SENTINEL=%s\nRUN_ID=%s\nRUN_DIR=%s\n' "$sentinel" "$run_id" "$run_dir"
