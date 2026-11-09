#!/usr/bin/env bash
set -euo pipefail
scratch=
issue=
plan=
body=
allow_empty=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --run-scratch) [ "$#" -ge 2 ] || exit 64; scratch=$2; shift 2 ;;
    --issue) [ "$#" -ge 2 ] || exit 64; issue=$2; shift 2 ;;
    --plan) [ "$#" -ge 2 ] || exit 64; plan=$2; shift 2 ;;
    --body) [ "$#" -ge 2 ] || exit 64; body=$2; shift 2 ;;
    --allow-empty-original) allow_empty=1; shift ;;
    *) exit 64 ;;
  esac
done
[ -n "$scratch" ] && [ -n "$issue" ] || exit 64
prefix="$scratch/$issue"
[ -n "$plan" ] && [ -n "$body" ] || exit 64
orig="$prefix.image-guard-orig.md" safe="$prefix.attachment-guard-orig.md" new="$prefix.image-guard-new.md"
cp "$body" "$new" || { echo "descriptionSummary artifact unreadable — aborting" >&2; exit 1; }
image_gate() {
  local label=$1 rc=0
  shift
  if [ "$allow_empty" = 1 ]; then set -- "$@" --allow-empty-original; fi
  node "$BOSS_PLAN_TOOLBOX/plan/plan-image-guard.mjs" "$@" || rc=$?
  [ "$rc" -ne 0 ] || return 0
  echo "$label failed (guard message above) — no tracker write, aborting" >&2
  if [ "$rc" != 64 ]; then rm -f "$orig" "$safe" "$new" || echo "warning: guard scratch cleanup failed" >&2; fi
  exit 1
}
image_gate "image-parity gate" --original "$orig" --rewritten "$new" --require-unsigned-uploads
image_gate "safe-source gate" --original "$orig" --rewritten "$safe" --require-safe-source
image_gate "description safety gate" --original "$safe" --rewritten "$new" --require-verbatim --require-unsigned-uploads
image_gate "plan-attachment safety gate" --original "$safe" --rewritten "$plan" --require-verbatim --require-unsigned-uploads
