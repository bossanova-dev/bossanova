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
check_tier() {
  local tier_args issue estimate
  tier_args=$(node --input-type=module - "$metadata" "$plan" <<'NODE'
import {readFileSync} from 'node:fs'
import {basename} from 'node:path'
const [file, plan] = process.argv.slice(2)
const match = /^([A-Za-z][A-Za-z0-9]*-\d+)-.*\.md$/.exec(basename(plan))
if (!match) throw new Error('cannot resolve issue from plan basename')
const {estimate} = JSON.parse(readFileSync(file, 'utf8'))
console.log(`${match[1]}\t${estimate}`)
NODE
  ) || return 1
  IFS=$'\t' read -r issue estimate <<< "$tier_args"
  node "$BOSS_PLAN_TOOLBOX/plan/plan-draft-tier.mjs" check --run-scratch "$scratch" --issue "$issue" --plan "$plan" --estimate "$estimate"
}
if ! returned_json=$(cat -- "$returned") || ! node "$BOSS_PLAN_TOOLBOX/plan/plan-run-guards.mjs" adopt-metadata "$metadata" "$returned_json" || ! check_tier; then
  echo "dispatch-failure: draft metadata or tier check failed — no Linear write, aborting" >&2
  node "$BOSS_PLAN_TOOLBOX/bs-run-sentinel.mjs" cleanup "$run_dir"
  node "$BOSS_PLAN_TOOLBOX/bs-dispatch-await.mjs" guard-discard "$plan" || exit 1
  bash "$BOSS_PLAN_TOOLBOX/plan/boss-plan-run.sh" cleanup --run-scratch "$scratch" || true
  exit 1
fi
