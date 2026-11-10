#!/usr/bin/env bash
set -euo pipefail
[ "$#" -eq 0 ] || exit 64
CONFIGURED=$(node -e 'import(require("node:url").pathToFileURL(process.env.BOSS_PLAN_TOOLBOX+"/skill-config.mjs").href).then(m=>{const c=m.loadSkillConfig({cwd:process.cwd()});process.stdout.write(m.trackerReadiness(c,{planning:true}))}).catch(e=>{process.stderr.write("boss-plan preflight: "+(e&&e.message||e)+"\n");process.stdout.write("error")})')
# `trackerReadiness` requires the tracker identity AND the full state role map
# (`states.{unplanned,planned,inProgress,inReview}`), so a repo configured only for a stateless
# core self-disables cleanly ('unconfigured') instead of running with undefined state names.
# 'ready' includes a configured team; 'needs-team' resolves it after the tracker preflight below.
# Distinguish a loader failure (malformed/invalid .boss-skills.json → 'error' or empty) from a
# valid "not planning-ready" ('unconfigured'): loadSkillConfig throws a `skill-config:` error on a present
# but broken config, so a broken config must abort loudly, never skip silently as a clean no-op.
if [ "$CONFIGURED" != "ready" ] && [ "$CONFIGURED" != "unconfigured" ] && [ "$CONFIGURED" != "needs-team" ]; then
  echo "boss-plan: .boss-skills.json is present but could not be loaded (see error above) — aborting instead of skipping." >&2
  exit 1
fi
if [ "$CONFIGURED" = "unconfigured" ]; then
  echo "boss-plan: no configured tracker in .boss-skills.json for this repo — nothing to plan here; skipping."
  exit 0
fi
printf 'configured: %s\n' "$CONFIGURED"
# Amortized self-heal for plan scratch — files and directories alike — orphaned by runs
# that abort before cleanup. It is TTL-gated, so a live peer run's scratch is never touched.
node "$BOSS_PLAN_TOOLBOX/plan/plan-scratch-reap.mjs" .linear-plans ||
  echo "warning: stale plan-scratch reap failed (non-fatal)" >&2
# Mint this run's own scratch directory. `mktemp -d` creates it atomically, so two runs
# started at the same instant cannot collide on it even when they plan the same ticket.
node "$BOSS_PLAN_TOOLBOX/plan/plan-scratch-paths.mjs" ensure-root >/dev/null || { echo "boss-plan: cannot create plan scratch root" >&2; exit 1; }
RUN_SCRATCH="$(mktemp -d .linear-plans/run-XXXXXXXX)" || { echo "boss-plan: cannot create run scratch directory" >&2; exit 1; }
echo "run scratch: $RUN_SCRATCH"
