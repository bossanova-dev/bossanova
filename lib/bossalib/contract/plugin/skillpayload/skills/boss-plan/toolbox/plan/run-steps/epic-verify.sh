#!/usr/bin/env bash
set -euo pipefail
scratch=
issue=
plan=
run_dir=
run_id=
allow_empty=0
children=()
while [ "$#" -gt 0 ]; do
  case "$1" in
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
[ "${#children[@]}" -gt 0 ] || exit 64
body="$prefix.epic-overview.md"
node "$BOSS_PLAN_TOOLBOX/plan/plan-image-guard.mjs" --original "$prefix.image-guard-orig.md" --rewritten "$prefix.attachment-guard-orig.md" --require-safe-source ${extra[@]+"${extra[@]}"}
node "$BOSS_PLAN_TOOLBOX/plan/plan-image-guard.mjs" --original "$prefix.attachment-guard-orig.md" --rewritten "$body" --require-verbatim --require-unsigned-uploads ${extra[@]+"${extra[@]}"}
roots=$(git ls-tree --name-only HEAD | paste -sd, -)
node "$BOSS_PLAN_TOOLBOX/plan/plan-contract-guard.mjs" --description "$body" --mode epic-parent --module-roots "$roots"
payload=$(node --input-type=module - "$scratch" "$issue" "${children[@]}" <<'NODE'
import {existsSync, readFileSync} from 'node:fs'
import {join} from 'node:path'
import {pathToFileURL} from 'node:url'
const {planScratchBasename} = await import(pathToFileURL(join(process.env.BOSS_PLAN_TOOLBOX,'plan/plan-scratch-paths.mjs')))
const [scratch, issue, ...children] = process.argv.slice(2)
const path = (family, parts) => join(scratch,planScratchBasename(family,parts))
const specPath = path('epic-spec',{issueId:issue})
const spec = JSON.parse(readFileSync(specPath,'utf8'))
const childIds = [], childPlanPaths = {}, guardScratchPaths = []
for (const arg of children) {
  const match = /^([^=]+)=([^=]+)$/.exec(arg)
  if (!match) throw new Error('child must be ID=key')
  const [,id,key] = match
  const child = spec.children.find(child => child.key === key)
  if (!child || childIds.includes(id)) throw new Error('unknown or repeated child')
  childIds.push(id)
  childPlanPaths[id] = path('child-plan',{parentId:issue,key})
  for (const family of ['child-image-guard-orig','child-attachment-guard-orig','child-image-guard-new']) guardScratchPaths.push(path(family,{issueId:issue,childId:id}))
}
for (const family of ['image-guard-orig','attachment-guard-orig']) guardScratchPaths.push(path(family,{issueId:issue}))
guardScratchPaths.push(path('epic-overview',{issueId:issue}))
const premisesPath = path('premises',{issueId:issue})
const premises = existsSync(premisesPath) ? JSON.parse(readFileSync(premisesPath,'utf8')) : []
if (!Array.isArray(premises)) throw new Error('premises must be an array')
console.log(JSON.stringify({epic:true,epicParentId:issue,childIds,childPlanPaths,epicSpecPaths:[specPath],guardScratchPaths,premises}))
NODE
)
node "$BOSS_PLAN_TOOLBOX/plan/plan-draft-manifest.mjs" verify --payload "$payload" --plan-path "${plan:-$body}" --run-scratch "$scratch"
exec node "$BOSS_PLAN_TOOLBOX/bs-run-sentinel.mjs" write "$run_dir" "$run_id" draft ok "$payload"
