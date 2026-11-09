#!/usr/bin/env bash
set -euo pipefail
scratch=
while [ "$#" -gt 0 ]; do
  case "$1" in --run-scratch) [ "$#" -ge 2 ] || exit 64; scratch=$2; shift 2 ;; *) exit 64 ;; esac
done
case "$scratch" in .linear-plans/run-?*) ;; *) exit 64 ;; esac
case "$scratch" in *..*|*\\*) exit 64 ;; esac
id=${scratch#.linear-plans/run-}
expected=$(node "$BOSS_PLAN_TOOLBOX/plan/plan-scratch-paths.mjs" run-dir "$id") || exit 64
[ "$scratch" = "$expected" ] || exit 64
# Refuse a symlinked root, which would redirect removal into another directory.
[ ! -L .linear-plans ] || exit 64
cleanup_rc=0
rm -rf -- "$scratch" || cleanup_rc=1
if [ -e "$scratch" ] || [ -L "$scratch" ]; then cleanup_rc=1; fi
if [ "$cleanup_rc" -ne 0 ]; then
  echo "scratch cleanup failed — $scratch may still hold plan text, tracker state, signed upload headers or the epic spec" >&2
  exit 1
fi
