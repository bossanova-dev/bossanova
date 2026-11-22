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
write_report() {
  node - "$prefix.premise-report.json" "$1" "$2" <<'NODE'
const fs = require('node:fs')
const [file, status, text] = process.argv.slice(2)
const value = { rc: Number(status), report: text, changed: false }
const tmp = `${file}.tmp.${process.pid}`
try { fs.writeFileSync(tmp, JSON.stringify(value) + '\n', { mode: 0o600 }); fs.renameSync(tmp, file) }
finally { fs.rmSync(tmp, { force: true }) }
NODE
}
if [ ! -f "$prefix.premises.json" ]; then
  report="premises-missing: $prefix.premises.json not found; dispatch-classify writes it headless; interactive runs save relied-on tickets as {id, state} entries (or []) there before this step"
  write_report 1 "$report"
  printf '%s\n' "$report" >&2
  exit 1
fi
if node - "$prefix.premises.json" 2>/dev/null <<'NODE'
const p = JSON.parse(require('node:fs').readFileSync(process.argv[2], 'utf8'))
process.exit(Array.isArray(p) && p.length === 0 ? 0 : 1)
NODE
then node "$BOSS_PLAN_TOOLBOX/tracker/cli.mjs" premise-states --premises "$prefix.premises.json" --out-file "$prefix.premise-states.json"; fi
rc=0
report=$(node "$BOSS_PLAN_TOOLBOX/plan/plan-run-guards.mjs" premises "$prefix.premises.json" "$prefix.premise-states.json" --annotate "$description" --annotate "$plan" 2>&1) || rc=$?
write_report "$rc" "$report"
[ -z "$report" ] || printf '%s\n' "$report" >&2
exit "$rc"
