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
// A repo MAY narrow that candidate scan through `trackerConfig.<adapter>.selection` (see
// docs/skills/skill-config.md). The block is absent in every repo in this tree, and while it is
// absent this gate emits byte-identically what it emitted before the seam existed.
//
// NARROWING IS NOT SAFE ON ITS OWN. This gate is only half the pair: when a selection is
// configured, the worker must select through the tracker CLI's `list-planned` verb, which derives
// its query from the same `plannedSelectionQuery` helper, and it stops rather than falling back to
// the unfiltered descriptor — a strict SUPERSET of this scan that would pick an unowned ticket.
//
// Fail-closed: missing key, network failure, or API error exits non-zero with a
// one-line reason on stderr (captured in the scheduler's gate_output log).
//
// Register on the cron job (gate cwd = repo root):
//   node skills-toolbox/cron-gates/boss-build.mjs

import { gateExit } from '../linear-gate-lib.mjs'
import { isMainModule } from '../main-module.mjs'
import { resolveTrackerAdapter } from '../tracker/adapter.mjs'
import { loadSkillConfig, plannedSelectionQuery } from '../skill-config.mjs'

/**
 * Decide the gate from an injected config and tracker, and say what it filtered on.
 *
 * Split out of the entry point below purely so the decision is reachable from a unit test with a
 * synthetic config: the module-level code exits the process, so a read of it was previously
 * unobservable and this gate's argument shape could drift with nothing to catch it.
 *
 * @returns {Promise<{hasWork: boolean, reason: string|null}>} `reason` is null when there IS work.
 */
export async function evaluateBossBuildGate({ config, tracker }) {
  // The planned state is repo-private data (config-driven, never hard-coded here) so the gate
  // matches the skill's own selection filter in any adopting repo. Fail-closed (skip) when it is
  // not configured. The whole query — state, the label set that SUPERSEDES the agentFriendly role,
  // and the optional identity selector as an absent-when-unset key — comes from the one helper the
  // worker's `list-planned` verb also reads, so the gate and the worker cannot narrow differently.
  const query = plannedSelectionQuery(config)
  const hasWork = await tracker.hasUnblockedWork(query)
  return { hasWork, reason: hasWork ? null : describeEmptyScan(query.state, query) }
}

/**
 * The one-line skip reason, naming the filter the scan ACTUALLY applied.
 *
 * An operator reading the scheduler's gate-output log has to be able to tell a narrowed skip from
 * a genuinely empty backlog — those call for opposite responses, and a reason that named only the
 * state and a single label would read identically in both cases. With no `selection` configured
 * this renders the string it rendered before the seam existed.
 */
function describeEmptyScan(plannedState, { label, assigneeOrCreator }) {
  const labelText = Array.isArray(label) ? `[${label.join('|')}]` : label
  const owner = assigneeOrCreator ? ` assigned to or created by ${assigneeOrCreator}` : ''
  return `boss-build gate: no unblocked ${plannedState} ${labelText} issues${owner}`
}

if (isMainModule(import.meta.url)) {
  try {
    const { hasWork, reason } = await evaluateBossBuildGate({
      config: loadSkillConfig(),
      tracker: resolveTrackerAdapter(),
    })
    gateExit(hasWork, reason)
  } catch (err) {
    gateExit(false, `boss-build gate: ${err.message}`)
  }
}
