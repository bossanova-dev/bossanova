#!/usr/bin/env bash
set -euo pipefail
scratch=
issue=
plan=
description=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --run-scratch) [ "$#" -ge 2 ] || exit 64; scratch=$2; shift 2 ;;
    --issue) [ "$#" -ge 2 ] || exit 64; issue=$2; shift 2 ;;
    --plan) [ "$#" -ge 2 ] || exit 64; plan=$2; shift 2 ;;
    --description) [ "$#" -ge 2 ] || exit 64; description=$2; shift 2 ;;
    *) exit 64 ;;
  esac
done
[ -n "$scratch" ] && [ -n "$issue" ] || exit 64
prefix="$scratch/$issue"
[ -n "$plan" ] && [ -n "$description" ] || exit 64
if node - "$prefix.premises.json" <<'NODE'
const p = JSON.parse(require('node:fs').readFileSync(process.argv[2], 'utf8'))
process.exit(Array.isArray(p) && p.length === 0 ? 0 : 1)
NODE
then printf '{}\n' >"$prefix.premise-states.json"; fi
rc=0
report=$(node "$BOSS_PLAN_TOOLBOX/plan/plan-run-guards.mjs" premises "$prefix.premises.json" "$prefix.premise-states.json" --annotate "$description" --annotate "$plan" 2>&1) || rc=$?
changed=0 merge=0
node - "$prefix.premise-report.json" "$rc" "$report" "$changed" "$merge" <<'NODE'
const fs = require('node:fs')
const [file, status, text, changed, merge] = process.argv.slice(2)
let prior = { rc: 0, report: '', changed: false }
if (merge === '1') prior = JSON.parse(fs.readFileSync(file, 'utf8'))
const value = { rc: prior.rc || Number(status), report: [prior.report, text].filter(Boolean).join('\n'), changed: prior.changed || changed === '1' }
const tmp = `${file}.tmp.${process.pid}`
try { fs.writeFileSync(tmp, JSON.stringify(value) + '\n', { mode: 0o600 }); fs.renameSync(tmp, file) }
finally { fs.rmSync(tmp, { force: true }) }
NODE
[ -z "$report" ] || printf '%s\n' "$report" >&2
exit "$rc"
