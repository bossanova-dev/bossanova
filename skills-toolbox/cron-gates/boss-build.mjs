#!/usr/bin/env node
// Cron gate for boss-build.
//
// Exit 0 (run the implementer) iff at least one Linear issue is in the configured
// planned state (`trackerConfigFor(config).states.planned`), carries the
// `agent-build` label, AND is not blocked by an uncleared
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
// Selection: the scan is `stageSelectionQuery(config, 'build', flags)` — the planned
// state, AND the build label, AND NOT `needs-human`, narrowed by `trackerConfig.<adapter>.selection`
// (shared block, then `stages.build`) and by the shared selection flags passed on the gate command
// line (`--label`, `--exclude-label`, `--assignee`, `--creator`, `--project` and their `--exclude-`
// forms), per slot, flag first. Any other argument is refused (exit 1): a token this gate ignored
// would be a filter the operator believes is applied.
//
// NARROWING IS NOT SAFE ON ITS OWN. The worker must select through the tracker CLI's
// `list-planned` verb with the same flags, which derives its query from the same
// `stageSelectionQuery`, and stop rather than fall back to the unfiltered descriptor.
//
// Fail-closed: missing key, network failure, or API error exits non-zero with a
// one-line reason on stderr (captured in the scheduler's gate_output log).
//
// Register on the cron job (gate cwd = repo root):
//   node skills-toolbox/cron-gates/boss-build.mjs [selection flags...]
// A repo that carries no skills-toolbox/ (and a launcher wrapper that execs this gate) runs the
// INSTALLED global copy instead, via the recipe in boss-build's references/cron-gate.md:
//   ~/.claude/skills/boss-build/toolbox/cron-gates/boss-build.mjs   (Codex: ~/.codex/skills/...)
// That copy picks up a repo-side edit to this file only after the install is refreshed
// (`boss skills sync`); toolbox-drift.mjs is the probe that reports the two diverging.

import { gateExit } from '../linear-gate-lib.mjs'
import { isMainModule } from '../main-module.mjs'
import { describeEmptyScan, gateSelectionFlags } from '../selection.mjs'
import { resolveTrackerAdapter } from '../tracker/adapter.mjs'
import { loadSkillConfig, stageSelectionQuery } from '../skill-config.mjs'

/**
 * Decide the gate from an injected config, tracker and argv, and say what it filtered on.
 *
 * Split out of the entry point below so the decision is reachable from a unit test with a synthetic
 * config: the module-level code exits the process.
 *
 * @returns {Promise<{hasWork: boolean, reason: string|null}>} `reason` is null when there IS work.
 */
export async function evaluateBossBuildGate({ config, tracker, argv = [] }) {
  // The whole query — state, the build label, the needs-human exclusion and the selection — comes
  // from the one helper the worker's `list-planned` verb also reads, so the gate and the worker
  // cannot narrow differently.
  const query = stageSelectionQuery(config, 'build', gateSelectionFlags('boss-build', argv))
  const hasWork = await tracker.hasUnblockedWork(query)
  return {
    hasWork,
    reason: hasWork ? null : describeEmptyScan('boss-build', query, { unblocked: true }),
  }
}

if (isMainModule(import.meta.url)) {
  try {
    const { hasWork, reason } = await evaluateBossBuildGate({
      config: loadSkillConfig(),
      tracker: resolveTrackerAdapter(),
      argv: process.argv.slice(2),
    })
    gateExit(hasWork, reason)
  } catch (err) {
    gateExit(false, `boss-build gate: ${err.message}`)
  }
}
