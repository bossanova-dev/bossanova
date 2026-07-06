// scripts/tracker/adapter.mjs
// Pluggable tracker-adapter interface shared by the boss-plan / boss-implement /
// boss-epic skills. Abstracts the Linear coupling those skills share so a future
// tracker (GitHub Issues, Jira, ...) can slot in behind resolveTrackerAdapter.
// node builtins only (the cron worktree is dependency-free — mirrors
// scripts/linear-gate-lib.mjs).
//
// A TrackerAdapter has two kinds of member:
//   - executable methods for the capabilities the skills already own in code
//     (gates, dependency reading, claim resolution, ticket normalization); and
//   - operationMap, a declarative description of the agent-driven capabilities
//     the skills perform through the tracker's MCP tools (select/list, move
//     state, read/write comments, read/merge labels, set priority/estimate,
//     append dependency edge) — the single source of truth later extraction
//     tickets consume.

import { createLinearAdapter } from './linear.mjs'

/**
 * @typedef {Object} TrackerAdapter
 * @property {string} tracker           Stable adapter id (e.g. "linear").
 * @property {(opts: {state: string, label?: string}) => Promise<boolean>} hasWork
 *           Existence gate: does at least one matching issue exist? (select capability)
 * @property {(opts: {state: string, label?: string}) => Promise<boolean>} hasUnblockedWork
 *           Existence gate restricted to issues with no uncleared blocker.
 * @property {(issue: object) => object[]} readDependencies
 *           Blocker issues of a raw tracker issue payload. (read dependency edges)
 * @property {(issue: object) => boolean} isUnblocked
 *           True iff every blocker of the issue is in a cleared state.
 * @property {(token: string) => string} formatClaimComment
 *           The claim-comment body for a run token. (claim capability)
 * @property {(comments: object[], myToken: string) => boolean} resolveClaim
 *           First-writer-wins: did myToken win the claim over these comments?
 * @property {(issue: object) => object} normalizeTicket
 *           Flatten a raw tracker issue into the shared ticket shape.
 * @property {Record<string, TrackerOperation>} operationMap
 *           Declarative map of agent-driven capability -> tracker MCP operation.
 */

/**
 * @typedef {Object} TrackerOperation
 * @property {string} tool     MCP tool name the agent invokes for this capability.
 * @property {string} summary  One line describing the argument/response shape.
 */

export const TRACKER_CAPABILITIES = [
  'hasWork',
  'hasUnblockedWork',
  'readDependencies',
  'isUnblocked',
  'formatClaimComment',
  'resolveClaim',
  'normalizeTicket',
  'operationMap',
]

/**
 * Throw if `adapter` is missing any capability in TRACKER_CAPABILITIES. Every
 * adapter's own test calls this to prove conformance.
 * @param {TrackerAdapter} adapter
 */
export function assertConforms(adapter) {
  for (const name of TRACKER_CAPABILITIES) {
    if (adapter?.[name] === undefined) {
      throw new Error(`tracker adapter missing capability: ${name}`)
    }
  }
}

const BUILDERS = {
  linear: ({ env, fetchImpl }) =>
    createLinearAdapter({
      apiKey: env.LINEAR_API_KEY,
      fetchImpl,
      // Coalesce falsy (incl. empty string) to undefined so an unset OR blank
      // LINEAR_API_ENDPOINT both fall through to the helper's default endpoint;
      // a bare `''` would otherwise POST to an empty URL.
      endpoint: env.LINEAR_API_ENDPOINT || undefined,
    }),
}

/**
 * Resolve the configured tracker adapter. `env.TRACKER` selects the adapter
 * (default "linear"); unknown values throw. This is the single pluggable
 * choke point — new trackers register a builder here.
 * @param {{env?: object, fetchImpl?: typeof fetch}} [opts]
 * @returns {TrackerAdapter}
 */
export function resolveTrackerAdapter({ env = process.env, fetchImpl = fetch } = {}) {
  const name = env.TRACKER || 'linear'
  const build = BUILDERS[name]
  if (!build) throw new Error(`unknown tracker: ${name}`)
  return build({ env, fetchImpl })
}
