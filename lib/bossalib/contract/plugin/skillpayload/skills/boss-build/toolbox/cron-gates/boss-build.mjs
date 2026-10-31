#!/usr/bin/env node
// Cron gate for boss-build.
// The scan uses the configured tracker team, matching the worker’s list verb.
//
// Exit 0 (run the implementer) iff at least one tracker issue is in the configured
// planned state (`trackerConfigFor(config).states.planned`), carries the
// `agent-build` label, AND is not blocked by an uncleared
// blocker. Completed/canceled states and merged PRs clear; configured review-state
// blockers with green, open, non-draft PRs are possible work pending selection’s file test.
//
// Still a LOOSE superset of the skill's exact filter: it does not read descriptions,
// so a rare false-positive (fires, finds only tickets with no readable plan attachment and
// no title or description text, exits NO_CHANGE) is possible. It scans the same
// candidate window the skill selects from (Step 2's `list_issues ... limit=250`),
// so within that window it is never a false-negative — it cannot skip a run while
// an unblocked candidate the skill would have picked exists.
//
// Selection: the scan is `stageSelectionQuery(config, 'build', flags)` — the planned
// state, AND the build label, AND NOT `needs-human`, narrowed by `trackerConfig.<adapter>.selection`
// (shared block, then `stages.build`) and by the shared selection flags passed on the gate command
// line (`--label`, `--exclude-label`, `--assignee`, `--creator`, `--project` and their `--exclude-`
// forms), per slot, flag first. Any other argument is refused (exit 69): a token this gate ignored
// would be a filter the operator believes is applied.
//
// Opt-in open-PR limit: `--max-open-prs N` exits 1 while the repository has N or more open PRs
// (drafts included, read through `gh`), before the tracker scan. An unreadable count exits 69.
//
// NARROWING IS NOT SAFE ON ITS OWN. The worker must select through the tracker CLI's
// `list-planned` verb with the same flags, which derives its query from the same
// `stageSelectionQuery`, and stop rather than fall back to the unfiltered descriptor.
//
// Outcomes: 0 = work, 1 = no work, 69 = could not evaluate.
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

import { createPrLookup } from '../build/blocker-pr-facts.mjs'
import { checkOpenPrLimit, createOpenPrLister, readOpenPrLimit } from '../build/open-pr-limit.mjs'
import { gateExit, gateFail } from '../linear-gate-lib.mjs'
import { isMainModule } from '../main-module.mjs'
import { describeEmptyScan, gateSelectionFlags } from '../selection.mjs'
import { resolveTrackerAdapter } from '../tracker/adapter.mjs'
import { loadSkillConfig, stageSelectionQuery, trackerConfigFor } from '../skill-config.mjs'

/**
 * Decide the gate from an injected config, tracker and argv, and say what it filtered on.
 *
 * Split out of the entry point below so the decision is reachable from a unit test with a synthetic
 * config: the module-level code exits the process.
 *
 * @returns {Promise<{hasWork: boolean, reason: string|null}>} `reason` is null when there IS work.
 */
export async function evaluateBossBuildGate({
  config,
  tracker,
  argv = [],
  prLookup = createPrLookup(),
  listOpenPrs = createOpenPrLister(),
}) {
  const { limit, rest } = readOpenPrLimit(argv)
  // The whole query — state, the build label, the needs-human exclusion and the selection — comes
  // from the one helper the worker's `list-planned` verb also reads, so the gate and the worker
  // cannot narrow differently.
  const query = stageSelectionQuery(config, 'build', gateSelectionFlags('boss-build', rest))
  const openPrs = await checkOpenPrLimit({ limit, listOpenPrs })
  if (openPrs.blocked) return { hasWork: false, reason: `boss-build gate: ${openPrs.reason}` }
  const hasWork = await tracker.hasUnblockedWork(query, {
    prLookup,
    inReviewState: trackerConfigFor(config).states.inReview,
  })
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
    gateFail(`boss-build gate: ${err.message}`)
  }
}
