#!/usr/bin/env node
// Cron gate for boss-build.
//
// Exit 0 (run the implementer) iff at least one Linear issue is in the configured
// planned state (`trackerConfigFor(config).states.planned`), carries the
// `agent-friendly` label, AND is not blocked by an uncleared
// blocker (a blocker whose PR is unmerged — state not Done/Canceled). This keeps
// the cron from waking to find every candidate blocked and exiting with no work.
//
// Still a LOOSE superset of the skill's exact filter: it does not check for the
// `Implementation plan (...)` link, so a rare false-positive (fires, finds no
// plan-linked candidate, exits NO_CHANGE) is possible. It scans the same
// candidate window the skill selects from (Step 2's `list_issues ... limit=250`),
// so within that window it is never a false-negative — it cannot skip a run while
// an unblocked candidate the skill would have picked exists.
//
// Fail-closed: missing key, network failure, or API error exits non-zero with a
// one-line reason on stderr (captured in the scheduler's gate_output log).
//
// Register on the cron job (gate cwd = repo root):
//   node scripts/cron-gates/boss-build.mjs

import { gateExit } from '../linear-gate-lib.mjs'
import { resolveTrackerAdapter } from '../tracker/adapter.mjs'
import { labelName, loadSkillConfig, stateName } from '../../skills-toolbox/skill-config.mjs'

try {
  // The planned state is repo-private data (config-driven, never hard-coded here) so the gate
  // matches the skill's own selection filter in any adopting repo, not just one whose planned
  // state is literally named `Todo`. Fail-closed (skip) when it is not configured.
  const config = loadSkillConfig()
  const plannedState = stateName(config, 'planned')
  const agentFriendlyLabel = labelName(config, 'agentFriendly')
  const tracker = resolveTrackerAdapter()
  const hasWork = await tracker.hasUnblockedWork({ state: plannedState, label: agentFriendlyLabel })
  gateExit(
    hasWork,
    hasWork ? null : `boss-build gate: no unblocked ${plannedState} ${agentFriendlyLabel} issues`,
  )
} catch (err) {
  gateExit(false, `boss-build gate: ${err.message}`)
}
