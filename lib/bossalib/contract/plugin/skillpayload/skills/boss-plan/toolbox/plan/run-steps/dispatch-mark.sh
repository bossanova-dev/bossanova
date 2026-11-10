#!/usr/bin/env bash
set -euo pipefail
run_dir=
name=draft
while [ "$#" -gt 0 ]; do
  [ "$#" -ge 2 ] && [ -n "$2" ] || exit 64
  case "$1" in --run-dir) run_dir=$2 ;; --name) name=$2 ;; *) exit 64 ;; esac
  shift 2
done
[ -n "$run_dir" ] && [ -d "$run_dir" ] || exit 64
[[ "$name" =~ ^[a-z][A-Za-z0-9-]*$ ]] || exit 64
mark_tmp=$(mktemp "$run_dir/$name.dispatched-at.XXXXXX")
trap 'rm -f -- "$mark_tmp"' EXIT
node -e 'process.stdout.write(String(Date.now()) + "\n")' > "$mark_tmp"
mv -- "$mark_tmp" "$run_dir/$name.dispatched-at"
