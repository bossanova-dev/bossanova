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
hash_file() {
  node - "$1" <<'NODE'
const fs = require('node:fs'), crypto = require('node:crypto')
process.stdout.write(crypto.createHash('sha256').update(fs.readFileSync(process.argv[2])).digest('hex'))
NODE
}
final="$prefix.image-guard-final.md"
before=$(hash_file "$final")
rc=0
report=$(node "$BOSS_PLAN_TOOLBOX/plan/plan-run-guards.mjs" premises "$prefix.premises.json" "$prefix.premise-states.json" --annotate "$final" 2>&1) || rc=$?
after=$(hash_file "$final")
changed=0 merge=1
[ "$before" = "$after" ] || changed=1
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
printf 'PREMISE_CHANGED=%s\n' "$changed"
exit "$rc"
