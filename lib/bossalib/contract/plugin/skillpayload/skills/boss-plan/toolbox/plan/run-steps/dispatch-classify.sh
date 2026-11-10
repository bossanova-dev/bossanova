#!/usr/bin/env bash
set -euo pipefail
RUN_DIR= RUN_ID= RUN_SCRATCH= ISSUE= PLAN_PATH=
while [ "$#" -gt 0 ]; do
  [ "$#" -ge 2 ] && [ -n "$2" ] || exit 64
  case "$1" in
    --run-dir) RUN_DIR=$2 ;; --run-id) RUN_ID=$2 ;; --run-scratch) RUN_SCRATCH=$2 ;;
    --issue) ISSUE=$2 ;; --plan-path) PLAN_PATH=$2 ;; *) exit 64 ;;
  esac
  shift 2
done
[ -n "$RUN_DIR" ] && [ -n "$RUN_ID" ] && [ -n "$RUN_SCRATCH" ] && [ -n "$ISSUE" ] && [ -n "$PLAN_PATH" ] || exit 64
RUN_SENTINEL="$BOSS_PLAN_TOOLBOX/bs-run-sentinel.mjs"
DISPATCH_FAILURE=dispatch-failure
READ="$(node "$RUN_SENTINEL" read "$RUN_DIR" "$RUN_ID" draft)"
AWAIT="${RUN_SENTINEL%/*}/bs-dispatch-await.mjs"
# `disposition` demotes a provisional (never-upgraded) payload on EVERY kind.
DISP="$(node "$AWAIT" disposition "$RUN_DIR" "$RUN_ID" draft --heartbeat "$RUN_SCRATCH/${ISSUE}.dispatch-heartbeat.json" --dispatched-at "$(cat "$RUN_DIR/draft.dispatched-at")")"
if [ "$(printf '%s' "$DISP" | jq -r '.publishable')" != true ]; then
  # SAFE branch — NO Linear write, non-zero exit.
  echo "$DISPATCH_FAILURE: no publishable sentinel ($(printf '%s' "$DISP" | jq -r '.reason')) — aborting" >&2
  node "$RUN_SENTINEL" cleanup "$RUN_DIR"
  # Probe every artifact the payload DECLARES: an epic outcome carries no `planPath`, so a
  # plan-only probe finds nothing, authorises the removal below and eats finished child plans.
  set -- "$PLAN_PATH"
  while IFS= read -r A; do set -- "$@" "$A"; done <<<"$(printf '%s' "$READ" | jq -r '[.payload.epicSpecPaths,.payload.guardScratchPaths,(.payload.childPlanPaths|if type=="object" then [.[]] else . end)]|flatten|.[]|strings')"
  node "$AWAIT" guard-discard "$@" || exit 1   # read before discard; non-zero = retain
  # Abort skips Phase 5; delete the epic/guard/run-boundary scratch families now.
  bash "$BOSS_PLAN_TOOLBOX/plan/boss-plan-run.sh" cleanup --run-scratch "$RUN_SCRATCH" || true
  exit 1
fi
EPIC="$(printf '%s' "$READ" | jq -r '.payload.epic // empty')"
node "${RUN_SENTINEL%/*}/plan/plan-draft-manifest.mjs" verify \
  --payload "$(printf '%s' "$READ" | jq -c '.payload // {}')" --plan-path "$PLAN_PATH" \
  --run-scratch "$RUN_SCRATCH" --label "$DISPATCH_FAILURE" ||
  {
    node "$RUN_SENTINEL" cleanup "$RUN_DIR"
    if [ "$EPIC" = "true" ]; then
      echo "$DISPATCH_FAILURE: epic manifest verification failed — needs a human; run scratch kept ($RUN_SCRATCH)" >&2
      exit 1
    fi
    # Artifact verification failure also skips Phase 5; remove the same scratch families.
    bash "$BOSS_PLAN_TOOLBOX/plan/boss-plan-run.sh" cleanup --run-scratch "$RUN_SCRATCH" || true
    exit 1
  }
PREMISES="$(printf '%s' "$READ" | jq -c '.payload.premises // []')"
if [ "$EPIC" = "true" ]; then
  # EPIC: inputs hydrated before this block (headless-dispatch.md).
  RC=0; RV="$(node "${RUN_SENTINEL%/*}/plan/plan-run-guards.mjs" epic-reverify "$RUN_SCRATCH/${ISSUE}.epic-reverify.json")" || RC=$?
  # A crashed node exits 1 too: only a printed resumable verdict deletes scratch.
  case "$RC:$(printf '%s' "$RV" | jq -r .class 2>/dev/null)" in
    0:*) ;;
    1:resumable)
    echo "$DISPATCH_FAILURE: epic reverify failed, parent still unplanned — next sweep resumes it" >&2
    node "$RUN_SENTINEL" cleanup "$RUN_DIR"
    bash "$BOSS_PLAN_TOOLBOX/plan/boss-plan-run.sh" cleanup --run-scratch "$RUN_SCRATCH" || true
    exit 1 ;;
    *)
    echo "$DISPATCH_FAILURE: epic reverify exit $RC — needs a human; run scratch kept" >&2
    node "$RUN_SENTINEL" cleanup "$RUN_DIR"
    exit 1 ;;
  esac
  # PASSED: no single-ticket plan/metadata — SKIP Phase 3.5-4; Phase 5 + 6 report epicParentId, childIds.
  node "$RUN_SENTINEL" cleanup "$RUN_DIR"
else
  PLAN_FILE_RAW="$(printf '%s' "$READ" | jq -r '.payload.planPath // empty')"
  # Normalize an equivalent absolute path; reject every path resolving elsewhere.
  PLAN_FILE="$(node -e 'const {resolve}=require("node:path");const [reportedPath,expectedPath]=process.argv.slice(1);if(!reportedPath||resolve(reportedPath)!==resolve(expectedPath))process.exit(1);process.stdout.write(expectedPath)' "$PLAN_FILE_RAW" "$PLAN_PATH")" || PLAN_FILE=
  # single-ticket `ok` sentinel → re-verify the expected plan file is non-empty.
  if [ "$PLAN_FILE" != "$PLAN_PATH" ] || [ ! -s "$PLAN_FILE" ]; then
    echo "$DISPATCH_FAILURE: sentinel ok but plan file missing/empty or wrong path ($PLAN_FILE_RAW) — no Linear write, aborting" >&2
    node "$RUN_SENTINEL" cleanup "$RUN_DIR"
    exit 1
  fi
  node "$RUN_SENTINEL" cleanup "$RUN_DIR"
fi

if [ "$EPIC" = true ]; then
  printf 'OUTCOME=epic\n'
else
  printf '%s\n' "$PREMISES" > "$RUN_SCRATCH/$ISSUE.premises.json"
  printf 'OUTCOME=single\nPLAN_FILE=%s\n' "$PLAN_FILE"
fi
