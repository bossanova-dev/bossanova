#!/usr/bin/env node
// Cron gate for boss-plan.
//
// Exit 0 (run the planner) iff at least one Linear issue is in the `Unplanned`
// state — the backlog this skill plans from. Any other outcome exits non-zero so
// the scheduled run is skipped and spends zero agent tokens.
//
// Fail-closed: a missing key, network failure, or API error exits non-zero with a
// one-line reason on stderr (captured in the scheduler's gate_output log). If we
// cannot prove work exists, we do not run.
//
// Register on the cron job (gate cwd = repo root):
//   node scripts/cron-gates/boss-plan.mjs

import { gateExit } from '../../skills-toolbox/linear-gate-lib.mjs'
import { resolveTrackerAdapter } from '../../skills-toolbox/tracker/adapter.mjs'
import { loadSkillConfig, stateName } from '../../skills-toolbox/skill-config.mjs'

try {
  const unplannedState = stateName(loadSkillConfig(), 'unplanned')
  const tracker = resolveTrackerAdapter()
  const hasWork = await tracker.hasWork({ state: unplannedState })
  gateExit(hasWork, hasWork ? null : `boss-plan gate: no ${unplannedState} issues`)
} catch (err) {
  gateExit(false, `boss-plan gate: ${err.message}`)
}
