#!/usr/bin/env bash
set -euo pipefail
[ "$#" -gt 0 ] || exit 64
action=$1; shift
design=
path=
while [ "$#" -gt 0 ]; do
  [ "$#" -ge 2 ] && [ -n "$2" ] || exit 64
  case "$1" in --design) design=$2 ;; --path) path=$2 ;; *) exit 64 ;; esac
  shift 2
done
node --input-type=module - "$action" "$design" "$path" <<'NODE'
import {mkdtempSync, mkdirSync, realpathSync, lstatSync, rmSync, existsSync} from 'node:fs'
import {join, dirname, basename, resolve} from 'node:path'
import {tmpdir} from 'node:os'
const [action, design, path] = process.argv.slice(2)
const refuse = () => { console.error('run-tmp: refusing invalid action or unsafe path'); process.exit(64) }
const root = realpathSync(tmpdir())
if (action === 'create' && !path) {
  if (design && !/^[A-Za-z0-9][A-Za-z0-9_-]*$/.test(design)) refuse()
  const dir = mkdtempSync(join(root, 'boss-plan-run.'))
  console.log(`RUN_TMP=${dir}`)
  if (design) { mkdirSync(join(dir,'design')); console.log(`DESIGN_DOC=${join(dir,'design',design+'-design.md')}`) }
} else if (action === 'remove' && path && !design) {
  if (path.split(/[\/]/).includes('..')) refuse()
  const target = resolve(path)
  if (!/^boss-plan-run[.][A-Za-z0-9]+$/.test(basename(target))) refuse()
  if (realpathSync(dirname(target)) !== root) refuse()
  const stat = lstatSync(target,{throwIfNoEntry:false})
  if (stat && (stat.isSymbolicLink() || !stat.isDirectory())) refuse()
  rmSync(target,{recursive:true,force:true,maxRetries:5})
  if (existsSync(target)) throw new Error('run-tmp: directory remains')
} else refuse()
NODE
