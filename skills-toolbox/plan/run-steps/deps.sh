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
# A previous classification's graph must not survive a failed fresh read.
rm -f -- "$prefix.blocker-graph.json"
set -- --id "$issue"
while IFS= read -r endpoint; do
  set -- "$@" --id "$endpoint"
done < <(node -e 'const fs=require("node:fs"); const p=JSON.parse(fs.readFileSync(process.argv[1],"utf8")); for(const [id,v] of Object.entries(p.logicalDependencies||{})) if(v===true||(typeof v==="string"&&v!=="")||(v&&typeof v==="object")) console.log(id)' "$prefix.deps-in.json")
if [ "$#" -gt 2 ]; then
  if ! node "$BOSS_PLAN_TOOLBOX/tracker/cli.mjs" fetch-blocker-graph --out-file "$prefix.blocker-graph.json" "$@" >&2; then
    echo "cycle safety could not be evaluated; no blocking writes without a fetched graph" >&2
  fi
fi
set --
if [ -f "$prefix.blocker-graph.json" ]; then set -- --graph "$prefix.blocker-graph.json"; fi
if [ -f "$prefix.open-prs.json" ]; then set -- "$@" --open-prs "$prefix.open-prs.json"; fi
if [ -f "$prefix.image-guard-final.md" ]; then set -- "$@" --saved-description "$prefix.image-guard-final.md"; fi
if [ -f "$prefix.premises.json" ]; then set -- "$@" --premises "$prefix.premises.json"; fi
node "$BOSS_PLAN_TOOLBOX/plan/plan-run-guards.mjs" deps "$prefix.deps-in.json" "$prefix.candidates.json" --subject "$issue" "$@"
