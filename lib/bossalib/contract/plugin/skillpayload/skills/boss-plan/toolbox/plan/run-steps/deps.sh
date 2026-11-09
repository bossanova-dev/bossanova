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
trap 'rm -f -- "$prefix.deps-in.json"' EXIT
set --
if [ -f "$prefix.open-prs.json" ]; then set -- --open-prs "$prefix.open-prs.json"; fi
node "$BOSS_PLAN_TOOLBOX/plan/plan-run-guards.mjs" deps "$prefix.deps-in.json" "$prefix.candidates.json" --subject "$issue" "$@"
