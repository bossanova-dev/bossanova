#!/usr/bin/env node
// Cron gate for boss-plan.
//
// Exit 0 (run the planner) iff at least one tracker issue matches the plan stage's scan:
// `stageSelectionQuery(config, 'plan', flags)` — the configured unplanned state, AND the
// `agent-plan` label, AND NOT `needs-human`, narrowed by `trackerConfig.<adapter>.selection`
// (shared block, then `stages.plan`) and by the shared selection flags passed on the gate command
// line (`--label`, `--exclude-label`, `--assignee`, `--creator`, `--project` and their `--exclude-`
// forms), per slot, flag first. Any other outcome exits non-zero so the scheduled run is skipped
// and spends zero agent tokens. Any other argument is refused (exit 1).
//
// Fail-closed: a missing key, an unresolvable selection value, a network failure, or an API error
// exits non-zero with a one-line reason on stderr (captured in the scheduler's gate_output log).
//
// Register on the cron job (gate cwd = repo root):
//   node skills-toolbox/cron-gates/boss-plan.mjs [selection flags...]
// A repo with no skills-toolbox/ runs the INSTALLED copy at
//   ~/.claude/skills/boss-plan/toolbox/cron-gates/boss-plan.mjs   (Codex: ~/.codex/skills/...)

import { gateExit } from '../linear-gate-lib.mjs'
import { isMainModule } from '../main-module.mjs'
import { describeEmptyScan, gateSelectionFlags } from '../selection.mjs'
import { resolveTrackerAdapter } from '../tracker/adapter.mjs'
import { loadSkillConfig, stageSelectionQuery } from '../skill-config.mjs'

/**
 * Decide the gate from an injected config, tracker and argv, and say what it filtered on.
 * @returns {Promise<{hasWork: boolean, reason: string|null}>} `reason` is null when there IS work.
 */
export async function evaluateBossPlanGate({ config, tracker, argv = [] }) {
  const query = stageSelectionQuery(config, 'plan', gateSelectionFlags('boss-plan', argv))
  const hasWork = await tracker.hasWork(query)
  return { hasWork, reason: hasWork ? null : describeEmptyScan('boss-plan', query) }
}

if (isMainModule(import.meta.url)) {
  try {
    const { hasWork, reason } = await evaluateBossPlanGate({
      config: loadSkillConfig(),
      tracker: resolveTrackerAdapter(),
      argv: process.argv.slice(2),
    })
    gateExit(hasWork, reason)
  } catch (err) {
    gateExit(false, `boss-plan gate: ${err.message}`)
  }
}
