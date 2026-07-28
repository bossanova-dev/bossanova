// scripts/tracker/linear.mjs
// Linear reference implementation of the tracker-adapter interface. Preserves
// current behaviour exactly by DELEGATING to the existing scripts/linear-*.mjs
// helpers — it reimplements nothing. node builtins only.
//
// Executable methods cover the capabilities the skills already own in code
// (gates, dependency reading, claim resolution, normalization). The agent-driven
// capabilities (select/list, move state, comments, labels, priority/estimate,
// dependency-edge writes) are captured declaratively in linearOperationMap: the
// skills perform these through the Linear MCP tools today, so 1a records the
// tool + shape rather than reimplementing them as GraphQL mutations (see the
// plan's Open Questions).

import { runLinearGate } from '../linear-gate-lib.mjs'
import { runUnblockedGate, extractBlockers, isUnblocked } from '../linear-deps-lib.mjs'
import { formatClaimComment, isClaimWon, parseClaimComments } from '../linear-claim.mjs'
import { normalizeTicket } from '../../skills-toolbox/bs-epic-lib.mjs'
import { loadSkillConfig, trackerConfigFor } from '../../skills-toolbox/skill-config.mjs'
import { TRACKER_STATE_ROLES } from './adapter.mjs'

// Declarative map of each agent-driven capability to the Linear MCP tool the
// skills invoke today, with the argument/response shape they rely on. This is
// the single source of truth later extraction tickets (and generalized skill
// prose) consume — change the tracker, change this map.
export const linearOperationMap = {
  selectPlanned: {
    tool: 'mcp__bossanova-linear__list_issues',
    summary: 'team=Bossanova state=<Unplanned|Todo> [label] limit=250 -> nodes ranked by priority',
  },
  getIssue: {
    tool: 'mcp__bossanova-linear__get_issue',
    summary: 'id[, includeRelations=true] -> issue with labels + blockedBy relations',
  },
  moveState: {
    tool: 'mcp__bossanova-linear__save_issue',
    summary: '{id, state} -> transition (e.g. Unplanned->Todo, Todo->In Progress, ->Done)',
  },
  readComments: {
    tool: 'mcp__bossanova-linear__list_comments',
    summary:
      'issueId -> [{id, body, createdAt}] (claim resolution reads these; the id is what ' +
      'updateComment needs to edit a comment in place)',
  },
  writeComment: {
    tool: 'mcp__bossanova-linear__save_comment',
    summary: '{issueId, body} -> posts a comment (claim comment / PR link)',
  },
  updateComment: {
    tool: 'mcp__bossanova-linear__save_comment',
    summary:
      '{id, body} -> updates that existing comment in place (save_comment updates when id is set)',
  },
  readLabels: {
    tool: 'mcp__bossanova-linear__get_issue',
    summary: 'id -> current labels to MERGE with (never overwrite)',
  },
  extractImages: {
    tool: 'mcp__bossanova-linear__extract_images',
    summary:
      '{markdown} -> renders embedded reporter screenshots (image markdown / attachment URLs)',
  },
  createLabel: {
    tool: 'mcp__bossanova-linear__create_issue_label',
    summary: '{name} -> ensure a label exists before applying it',
  },
  setPriorityEstimate: {
    tool: 'mcp__bossanova-linear__save_issue',
    summary: '{id, priority(1-4), estimate(fib)} -> set on plan finalize',
  },
  appendDependency: {
    tool: 'mcp__bossanova-linear__save_issue',
    summary: '{id, blockedBy: [ids]} -> add a dependency edge (cycle-checked by caller)',
  },
  preparePlanAttachment: {
    tool: 'mcp__bossanova-linear__prepare_attachment_upload',
    summary:
      '{issue, filename, contentType="text/markdown", size} -> signed upload request + assetUrl',
  },
  finalizePlanAttachment: {
    tool: 'mcp__bossanova-linear__create_attachment_from_upload',
    summary: '{issue, assetUrl, title} -> issue attachment',
  },
  readPlanAttachment: {
    tool: 'mcp__bossanova-linear__get_attachment',
    summary: '{id} -> attached Markdown text',
  },
  deletePlanAttachment: {
    tool: 'mcp__bossanova-linear__delete_attachment',
    summary: '{id} -> delete a stale issue attachment',
  },
}

/**
 * Reference implementation of the OPTIONAL `states` capability: the adapter is the
 * PRIMARY authority for the tracker's workflow-state names, so a repo wired through a
 * vendored adapter need not restate them in `.boss-skills.json`. This reference
 * derives them FROM that same configuration, so the reference path's resolution ends
 * at exactly the values it always did — the capability adds an authority, not a new
 * answer. A vendored adapter for a tracker whose state names it already knows
 * (hard-coded, or read from the tracker itself) returns them here instead.
 *
 * Never throws: a missing/unreadable/state-less config yields every role => null, which
 * is precisely the signal the caller needs to fall back to its own config read. A throw
 * here would instead take down a caller that has a perfectly good fallback available.
 * Synchronous, because loadSkillConfig is and every consumer of this path is.
 * @param {{cwd?: string}} [opts]
 * @returns {Record<string, string|null>}
 */
function linearStates(opts) {
  // Destructuring in the signature would default only on `undefined` and throw a raw
  // TypeError on an explicit `null` — which the pass-through `states: (opts) => ...`
  // hands straight over — breaking the "never throws" contract two lines above it.
  const cwd = opts?.cwd
  let configured = null
  try {
    configured = trackerConfigFor(loadSkillConfig(cwd ? { cwd } : {}))?.states ?? null
  } catch {
    configured = null
  }
  const resolved = {}
  for (const role of TRACKER_STATE_ROLES) {
    const name = configured?.[role]
    resolved[role] = typeof name === 'string' && name.trim() !== '' ? name : null
  }
  return resolved
}

/**
 * @param {{apiKey: string, fetchImpl?: typeof fetch, endpoint?: string}} config
 * @returns {import('./adapter.mjs').TrackerAdapter}
 */
export function createLinearAdapter({ apiKey, fetchImpl, endpoint }) {
  return {
    tracker: 'linear',
    hasWork: ({ state, label } = {}) =>
      runLinearGate({ apiKey, state, label, fetchImpl, endpoint }),
    hasUnblockedWork: ({ state, label } = {}) =>
      runUnblockedGate({ apiKey, state, label, fetchImpl, endpoint }),
    readDependencies: (issue) => extractBlockers(issue),
    isUnblocked: (issue) => isUnblocked(issue),
    formatClaimComment: (token) => formatClaimComment(token),
    resolveClaim: (comments, myToken) => isClaimWon(parseClaimComments(comments), myToken),
    normalizeTicket: (issue) => normalizeTicket(issue),
    states: (opts) => linearStates(opts),
    operationMap: linearOperationMap,
  }
}
