#!/usr/bin/env bash
set -euo pipefail
body=
snapshot=
while [ "$#" -gt 0 ]; do
  [ "$#" -ge 2 ] && [ -n "$2" ] || exit 64
  case "$1" in --body) body=$2 ;; --snapshot) snapshot=$2 ;; *) exit 64 ;; esac
  shift 2
done
[ -n "$body" ] && [ -n "$snapshot" ] || exit 64
node --input-type=module - "$body" "$snapshot" <<'NODE'
import {readFileSync, appendFileSync} from 'node:fs'
const [body, snapshot] = process.argv.slice(2)
const bytes = readFileSync(body)
if (!bytes.subarray(-19).equals(Buffer.from('## Original notes\n\n'))) {
  console.error('append-original-notes: body must end with Original notes and one blank line')
  process.exit(1)
}
const original = readFileSync(snapshot)
appendFileSync(body, original)
NODE
