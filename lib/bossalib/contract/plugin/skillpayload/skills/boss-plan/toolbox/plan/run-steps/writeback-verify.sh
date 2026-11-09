#!/usr/bin/env bash
set -euo pipefail
scratch=
issue=
uuid=
second_save=0
read_fallback=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --run-scratch) [ "$#" -ge 2 ] || exit 64; scratch=$2; shift 2 ;;
    --issue) [ "$#" -ge 2 ] || exit 64; issue=$2; shift 2 ;;
    --uuid) [ "$#" -ge 2 ] || exit 64; uuid=$2; shift 2 ;;
    --second-save) second_save=1; shift ;;
    --read-fallback) read_fallback=1; shift ;;
    *) exit 64 ;;
  esac
done
[ -n "$scratch" ] && [ -n "$issue" ] || exit 64
prefix="$scratch/$issue"
stored="$prefix.image-guard-stored.md" intended="$prefix.image-guard-final.md" stale="$prefix.image-guard-orig.md"
[ "$second_save" = 0 ] || stale="$prefix.image-guard-base.md"
[ -n "$uuid" ] || exit 64
set --
if [ "$read_fallback" = 0 ]; then
node "$BOSS_PLAN_TOOLBOX/tracker/cli.mjs" read-description --id "$uuid" --out-file "$stored" --expect-file "$intended" --stale-file "$stale" >"$prefix.read-receipt.json"
node - "$prefix.read-receipt.json" "$uuid" <<'NODE'
const receipt = JSON.parse(require('node:fs').readFileSync(process.argv[2], 'utf8'))
if (receipt.id !== process.argv[3]) { console.error('read receipt issue id mismatch'); process.exit(1) }
if (typeof receipt.settled !== 'boolean') { console.error('read receipt settled verdict missing'); process.exit(1) }
process.stdout.write(`SETTLED=${receipt.settled}\n`)
NODE
  set -- --read-receipt "$prefix.read-receipt.json"
fi
if ! node "$BOSS_PLAN_TOOLBOX/plan/plan-writeback-verify.mjs" --intended "$intended" --stored "$stored" "$@"; then
  echo "write-back verification failed (verdict above) — the description is ALREADY stored; do NOT rewrite it" >&2
  exit 1
fi
node - "$prefix.premise-report.json" <<'NODE'
const fs = require('node:fs')
const file = process.argv[2]
if (!fs.existsSync(file)) { console.error('premise verdict missing'); process.exit(1) }
const value = JSON.parse(fs.readFileSync(file, 'utf8'))
if (!Number.isInteger(value.rc) || value.rc < 0 || value.rc > 255 || typeof value.report !== 'string') { console.error('premise verdict invalid'); process.exit(1) }
if (value.rc !== 0) { console.error(value.report); process.exit(value.rc) }
NODE
