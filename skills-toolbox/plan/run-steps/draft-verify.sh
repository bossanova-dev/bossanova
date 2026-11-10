#!/usr/bin/env bash
set -euo pipefail
scratch=
issue=
plan=
run_dir=
run_id=
allow_empty=0
verify_only=0
children=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    --verify-only) verify_only=1; shift; continue ;;
    --allow-empty-original) allow_empty=1; shift; continue ;;
  esac
  [ "$#" -ge 2 ] && [ -n "$2" ] || exit 64
  case "$1" in
    --run-scratch) scratch=$2 ;; --issue) issue=$2 ;; --plan) plan=$2 ;;
    --run-dir) run_dir=$2 ;; --run-id) run_id=$2 ;; --child) children+=("$2") ;;
    *) exit 64 ;;
  esac
  shift 2
done
[ -n "$scratch" ] && [ -n "$issue" ] && [ -n "$run_dir" ] && [ -n "$run_id" ] || exit 64
prefix="$scratch/$issue"
extra=()
if [ "$allow_empty" = 1 ]; then extra+=(--allow-empty-original); fi
[ -n "$plan" ] && [ "${#children[@]}" = 0 ] || exit 64
body="$prefix.description.md"
node "$BOSS_PLAN_TOOLBOX/plan/plan-image-guard.mjs" --original "$prefix.image-guard-orig.md" --rewritten "$prefix.attachment-guard-orig.md" --require-safe-source ${extra[@]+"${extra[@]}"}
node "$BOSS_PLAN_TOOLBOX/plan/plan-image-guard.mjs" --original "$prefix.attachment-guard-orig.md" --rewritten "$body" --require-verbatim --require-unsigned-uploads ${extra[@]+"${extra[@]}"}
roots=$(git ls-tree --name-only HEAD | paste -sd, -)
node "$BOSS_PLAN_TOOLBOX/plan/plan-contract-guard.mjs" --description "$body" --plan "$plan" --module-roots "$roots"
if [ "$verify_only" = 1 ]; then exit 0; fi
payload=$(node --input-type=module - "$plan" "$prefix.premises.json" <<'NODE'
import {existsSync, readFileSync} from 'node:fs'
const [planPath, file] = process.argv.slice(2)
const premises = existsSync(file) ? JSON.parse(readFileSync(file,'utf8')) : []
if (!Array.isArray(premises)) throw new Error('premises must be an array')
console.log(JSON.stringify({planPath,premises}))
NODE
)
exec node "$BOSS_PLAN_TOOLBOX/bs-run-sentinel.mjs" write "$run_dir" "$run_id" draft ok "$payload"
