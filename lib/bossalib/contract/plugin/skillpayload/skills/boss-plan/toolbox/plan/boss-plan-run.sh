#!/usr/bin/env bash
# Locate this install only. Missing shared steps fail loudly here because skill
# drift checks see a step name, rather than the step file's basename.
[ -n "${BASH_VERSION:-}" ] || { echo 'BLOCKED: boss-plan-run.sh must be run with bash'; exit 2; }
set -euo pipefail
RUN_SOURCE=${BASH_SOURCE[0]}
while [ -L "$RUN_SOURCE" ]; do
  RUN_PARENT=$(cd -P -- "$(dirname -- "$RUN_SOURCE")" && pwd -P)
  RUN_SOURCE=$(readlink "$RUN_SOURCE")
  case "$RUN_SOURCE" in /*) ;; *) RUN_SOURCE="$RUN_PARENT/$RUN_SOURCE" ;; esac
done
BOSS_PLAN_TOOLBOX=$(cd -P -- "$(dirname -- "$RUN_SOURCE")/.." && pwd -P)
BOSS_SKILLS_HOME=$(cd -P -- "$(dirname -- "$(dirname -- "$BOSS_PLAN_TOOLBOX")")" && pwd -P)
export BOSS_PLAN_TOOLBOX BOSS_SKILLS_HOME
steps_dir="$BOSS_PLAN_TOOLBOX"/plan/run-steps
usage() { echo 'boss-plan-run: usage: node <helper.mjs> [args …] | steps | <step> [flags …]' >&2; exit 64; }
[ "$#" -gt 0 ] || usage
verb=$1
shift
case "$verb" in
  node)
    [ "$#" -gt 0 ] || usage
    helper=$1
    shift
    case "$helper" in /*|../*|*/../*|*/..|..|*[^a-zA-Z0-9_./-]*) usage ;; esac
    case "$helper" in *.mjs) ;; *) usage ;; esac
    [ -f "$BOSS_PLAN_TOOLBOX/$helper" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }
    exec node "$BOSS_PLAN_TOOLBOX/$helper" "$@"
    ;;
  steps)
    [ "$#" -eq 0 ] || usage
    for step in "$steps_dir"/*.sh; do
      [ -f "$step" ] || continue
      name=${step##*/}
      printf '%s\n' "${name%.sh}"
    done | LC_ALL=C sort
    ;;
  *)
    if [[ ! "$verb" =~ ^[a-z][a-z0-9-]*$ ]] || [ ! -f "$steps_dir/$verb.sh" ]; then
      echo "boss-plan-run: unknown step $verb (see: steps)" >&2
      exit 64
    fi
    exec bash "$steps_dir/$verb.sh" "$@"
    ;;
esac
