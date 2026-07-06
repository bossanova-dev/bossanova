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
//   node .claude/skills/boss-plan/gate/gate.mjs

import { gateExit } from '../../../../scripts/linear-gate-lib.mjs'
import { resolveTrackerAdapter } from '../../../../scripts/tracker/adapter.mjs'

try {
  const tracker = resolveTrackerAdapter()
  const hasWork = await tracker.hasWork({ state: 'Unplanned' })
  gateExit(hasWork, hasWork ? null : 'boss-plan gate: no Unplanned issues')
} catch (err) {
  gateExit(false, `boss-plan gate: ${err.message}`)
}
