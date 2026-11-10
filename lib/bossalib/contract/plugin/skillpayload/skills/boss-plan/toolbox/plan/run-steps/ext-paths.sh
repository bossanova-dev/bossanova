#!/usr/bin/env bash
set -euo pipefail
scratch_id=
issue=
name=
while [ "$#" -gt 0 ]; do
  [ "$#" -ge 2 ] && [ -n "$2" ] || exit 64
  case "$1" in --run-scratch-id) scratch_id=$2 ;; --issue) issue=$2 ;; --name) name=$2 ;; *) exit 64 ;; esac
  shift 2
done
[ -n "$scratch_id" ] && [ -n "$issue" ] && [ -n "$name" ] || exit 64
for value in "$scratch_id" "$issue" "$name"; do [[ "$value" =~ ^[A-Za-z0-9][A-Za-z0-9_-]*$ ]] || exit 64; done
tmp=$(node "$BOSS_PLAN_TOOLBOX/plan/plan-scratch-paths.mjs" path "$scratch_id" extension-tmp "issueId=$issue" role=plan-reviewer "name=$name")
out=$(node "$BOSS_PLAN_TOOLBOX/plan/plan-scratch-paths.mjs" path "$scratch_id" extension-out "issueId=$issue" role=plan-reviewer "name=$name")
heartbeat=$(node "$BOSS_PLAN_TOOLBOX/plan/plan-scratch-paths.mjs" path "$scratch_id" extension-heartbeat "issueId=$issue" role=plan-reviewer "name=$name")
mkdir -p "$tmp"
printf 'runTmp=%s outPath=%s heartbeat=%s\n' "$tmp" "$out" "$heartbeat"
