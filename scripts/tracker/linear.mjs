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
    summary: 'issueId -> [{body, createdAt}] (claim resolution reads these)',
  },
  writeComment: {
    tool: 'mcp__bossanova-linear__save_comment',
    summary: '{issueId, body} -> posts a comment (claim comment / PR link)',
  },
  readLabels: {
    tool: 'mcp__bossanova-linear__get_issue',
    summary: 'id -> current labels to MERGE with (never overwrite)',
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
    operationMap: linearOperationMap,
  }
}
