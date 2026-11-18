#!/usr/bin/env bash
set -euo pipefail
mode=
scratch=
issue=
parent=
source_title=
while [ "$#" -gt 0 ]; do
  [ "$#" -ge 2 ] && [ -n "$2" ] || exit 64
  case "$1" in
    --mode) mode=$2 ;; --run-scratch) scratch=$2 ;; --issue) issue=$2 ;;
    --parent) parent=$2 ;; --source-title) source_title=$2 ;; *) exit 64 ;;
  esac
  shift 2
done
[ -n "$scratch" ] && [ -n "$issue" ] && [ -n "$parent" ] || exit 64
case "$mode" in fanout|epic) ;; siblings) [ -n "$source_title" ] || exit 64 ;; *) exit 64 ;; esac
prefix="$scratch/$issue"
input=$(mktemp "$prefix.children-input.XXXXXX")
listing=$(mktemp "$prefix.children-listing.XXXXXX")
trap 'rm -f -- "$input" "$listing"' EXIT
jq -n --arg parentId "$parent" '{parentId:$parentId}' >"$input"
node "$BOSS_PLAN_TOOLBOX/tracker/cli.mjs" invoke-operation listChildren --args-file "$input" --result-only >"$listing"
case "$mode" in
  fanout)
    mv -- "$listing" "$prefix.fanout-route.json"
    node "$BOSS_PLAN_TOOLBOX/plan/plan-child-fanout.mjs" route "$prefix.fanout-route.json"
    ;;
  epic)
    jq '.parent' "$listing" >"$prefix.epic-reverify-payload-parent.json"
    mv -- "$listing" "$prefix.epic-reverify-payload-children.json"
    ;;
  siblings)
    jq --arg sourceId "$issue" --arg sourceTitle "$source_title" --arg parentId "$parent" \
      --slurpfile metadata "$prefix.draft-metadata.json" \
      '{sourceId:$sourceId, sourceTitle:$sourceTitle, parentId:$parentId, siblings:$metadata[0].siblings, existingChildren:.children}' \
      "$listing" >"$input"
    mv -- "$input" "$prefix.siblings-in.json"
    ;;
esac
