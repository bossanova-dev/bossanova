#!/usr/bin/env bash
set -euo pipefail
scratch=
issue=
plan=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --run-scratch) [ "$#" -ge 2 ] || exit 64; scratch=$2; shift 2 ;;
    --issue) [ "$#" -ge 2 ] || exit 64; issue=$2; shift 2 ;;
    --plan) [ "$#" -ge 2 ] || exit 64; plan=$2; shift 2 ;;
    *) exit 64 ;;
  esac
done
[ -n "$scratch" ] && [ -n "$issue" ] || exit 64
prefix="$scratch/$issue"
[ -n "$plan" ] || exit 64
orig="$prefix.image-guard-orig.md" safe="$prefix.attachment-guard-orig.md" new="$prefix.image-guard-new.md"
roots=$(git ls-tree --name-only HEAD | paste -sd, -)
if report=$(node "$BOSS_PLAN_TOOLBOX/plan/plan-contract-guard.mjs" --description "$new" --plan "$plan" --module-roots "$roots" 2>&1); then
  if [ -n "$report" ]; then printf '%s\n' "$report" >&2; fi
else
  printf '%s\n' "$report" >&2
  echo "plan-contract gate failed (guard message above) — no Linear write, aborting" >&2
  rm -f "$orig" "$safe" "$new" || echo "warning: contract gate scratch cleanup failed" >&2
  exit 1
fi
