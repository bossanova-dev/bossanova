#!/usr/bin/env bash
set -euo pipefail
scratch=
issue=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --run-scratch) [ "$#" -ge 2 ] || exit 64; scratch=$2; shift 2 ;;
    --issue) [ "$#" -ge 2 ] || exit 64; issue=$2; shift 2 ;;
    *) exit 64 ;;
  esac
done
[ -n "$scratch" ] && [ -n "$issue" ] || exit 64
prefix="$scratch/$issue"
if [ ! -f "$prefix.open-prs.json" ]; then
  node "$BOSS_PLAN_TOOLBOX/plan/plan-open-prs.mjs" fetch --out-file "$prefix.open-prs.json"
fi
