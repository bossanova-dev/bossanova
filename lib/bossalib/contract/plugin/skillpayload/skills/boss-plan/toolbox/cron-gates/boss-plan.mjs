#!/usr/bin/env node
// Cron gate for boss-plan.
// The scan uses the configured tracker team, matching the worker’s list verb.
//
// Exit 0 (run the planner) iff at least one tracker issue matches the plan stage's scan:
// `stageSelectionQuery(config, 'plan', flags)` — the configured unplanned state, AND the
// `agent-plan` label, AND NOT `needs-human`, narrowed by `trackerConfig.<adapter>.selection`
// (shared block, then `stages.plan`) and by the shared selection flags passed on the gate command
// line (`--label`, `--exclude-label`, `--assignee`, `--creator`, `--project` and their `--exclude-`
// forms), per slot, flag first. Any other outcome exits non-zero so the scheduled run is skipped
// and spends zero agent tokens. Any other argument is refused (exit 69).
//
// Outcomes: 0 = work, 1 = no work, 69 = could not evaluate.
// Fail-closed: a missing key, an unresolvable selection value, a network failure, or an API error
// exits non-zero with a one-line reason on stderr (captured in the scheduler's gate_output log).
//
// Register on the cron job (gate cwd = repo root):
//   node skills-toolbox/cron-gates/boss-plan.mjs [selection flags...]
// A repo with no skills-toolbox/ runs the INSTALLED copy at
//   ~/.claude/skills/boss-plan/toolbox/cron-gates/boss-plan.mjs   (Codex: ~/.codex/skills/...)

import { gateExit, gateFail } from '../linear-gate-lib.mjs'
import { isMainModule } from '../main-module.mjs'
import { describeEmptyScan, gateSelectionFlags } from '../selection.mjs'
import { resolveTrackerAdapter } from '../tracker/adapter.mjs'
import { selectPlanCandidates } from '../tracker/plan-candidates.mjs'
import { loadSkillConfig, trackerConfigFor, stageSelectionQuery } from '../skill-config.mjs'

/**
 * Decide the gate from an injected config, tracker and argv, and say what it filtered on.
 * @returns {Promise<{hasWork: boolean, reason: string|null}>} `reason` is null when there IS work.
 */
export async function evaluateBossPlanGate({ config, tracker, argv = [] }) {
  const query = stageSelectionQuery(config, 'plan', gateSelectionFlags('boss-plan', argv))
  let hasWork = await tracker.hasWork(query)
  const team = trackerConfigFor(config).team
  if (
    hasWork &&
    tracker.planClaimSelection === true &&
    typeof tracker.selectPlanned === 'function' &&
    !(tracker.tracker === 'linear' && !team)
  ) {
    // A full bounded batch cannot prove that every matching issue is claimed: later rows
    // may be free. Preserve the cheap existence answer in that case, accepting a possible
    // no-op planner run rather than hiding work outside the worker's candidate window.
    const limit = 250
    const result = await selectPlanCandidates(tracker, {
      ...query,
      limit,
      ...(team ? { team } : {}),
    })
    hasWork = result.candidates.length > 0 || result.excluded.length >= limit
    if (!hasWork && result.excluded.length > 0)
      return {
        hasWork,
        reason: `boss-plan gate: every matching ${query.state} candidate (${result.excluded.length}) holds a live peer plan claim`,
      }
  }
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
    gateFail(`boss-plan gate: ${err.message}`)
  }
}
