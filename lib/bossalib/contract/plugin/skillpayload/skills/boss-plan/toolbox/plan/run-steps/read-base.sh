#!/usr/bin/env bash
set -euo pipefail
scratch=
issue=
uuid=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --run-scratch) [ "$#" -ge 2 ] || exit 64; scratch=$2; shift 2 ;;
    --issue) [ "$#" -ge 2 ] || exit 64; issue=$2; shift 2 ;;
    --uuid) [ "$#" -ge 2 ] || exit 64; uuid=$2; shift 2 ;;
    *) exit 64 ;;
  esac
done
[ -n "$scratch" ] && [ -n "$issue" ] || exit 64
prefix="$scratch/$issue"
stored="$prefix.image-guard-base.md" intended="$prefix.image-guard-new.md" stale="$prefix.image-guard-orig.md"
[ -n "$uuid" ] || exit 64
node "$BOSS_PLAN_TOOLBOX/tracker/cli.mjs" read-description --id "$uuid" --out-file "$stored" --expect-file "$intended" --stale-file "$stale" >"$prefix.read-receipt.json"
node - "$prefix.read-receipt.json" "$uuid" <<'NODE'
const receipt = JSON.parse(require('node:fs').readFileSync(process.argv[2], 'utf8'))
if (receipt.id !== process.argv[3]) { console.error('read receipt issue id mismatch'); process.exit(1) }
if (typeof receipt.settled !== 'boolean') { console.error('read receipt settled verdict missing'); process.exit(1) }
process.stdout.write(`SETTLED=${receipt.settled}\n`)
NODE
