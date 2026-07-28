// scripts/tracker/adapter.mjs
// Pluggable tracker-adapter interface shared by the boss-plan / boss-build /
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
//     state, read/write/update comments, read/merge labels, set
//     priority/estimate, append dependency edge) — the single source of truth
//     later extraction tickets consume.
//
// updateComment — {commentId, body} — updates an EXISTING comment in place
// (as opposed to writeComment, which always creates a new one). Together with
// readComments + writeComment this is exactly the three-op surface the
// single-comment progress protocol (boss-epic: one comment, edited in place,
// with a hidden marker as the resume anchor) needs — the whole protocol must
// be expressible entirely through the adapter, with no raw GraphQL in
// drivers. CLI-based vendored adapters expose it as
// `update-comment --id <commentId> --body-file <path>` (GraphQL
// `commentUpdate` for Linear-backed CLIs). This requires readComments to
// surface each comment's **id**, not just its body/createdAt — without the
// id, updateComment has nothing to target and the protocol falls back to
// create-only (unbounded duplicate comments).
//
// `commentId` is this capability's logical argument name at the adapter-contract
// layer; each adapter maps it onto its own tracker's comment-id argument name when
// it actually invokes the operation — the reference Linear implementation
// (linear.mjs) and the CLI descriptor (cli.mjs) both pass it through as `id`. Both
// names are correct at their own layer: `commentId` here, `id` one layer down.
//
// states — the OPTIONAL capability by which an adapter that already knows its own
// tracker's workflow-state names becomes the PRIMARY authority for them, so a repo
// wired through a vendored adapter never has to restate them in
// `.boss-skills.json`. It is deliberately NOT in TRACKER_CAPABILITIES: assertConforms
// requires every entry there, so listing it would break conforming adapters that
// legitimately omit it. Consumers resolve adapter-first with the config as fallback
// (see resolveStateRole in the skills toolbox) and fail closed only when NEITHER
// source yields a name.
//
// **MUST-populate rule for adapters WITHOUT `states`:** an adapter that omits the
// capability shifts the whole burden onto configuration —
// `trackerConfig.<tracker>.states` becomes REQUIRED for every repo using it, because
// the fallback is then the only source and an empty one BLOCKs the caller.

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
 *           Must include every key in REQUIRED_TRACKER_OPERATIONS (readComments,
 *           writeComment, updateComment, ...) — see assertConforms.
 * @property {() => Record<string, string|null>} [states]
 *           OPTIONAL (OPTIONAL_TRACKER_CAPABILITIES). Synchronous — every caller on
 *           this path is. Returns a plain object mapping every role in
 *           TRACKER_STATE_ROLES to its resolved non-empty state name, or null when
 *           the adapter cannot resolve that role; extra roles beyond that set are
 *           allowed. Must never throw: an adapter that cannot read its own source
 *           returns all-null so the caller falls back to configuration.
 */

/**
 * @typedef {Object} TrackerOperation
 * @property {string} tool     MCP tool name the agent invokes for this capability.
 * @property {string} summary  One line describing the argument/response shape.
 *           updateComment's summary describes {commentId, body} updating an
 *           existing comment in place (never creating a new one).
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

// Capabilities an adapter MAY expose. Never fold these into TRACKER_CAPABILITIES —
// assertConforms requires every entry there, so promoting one would fail every
// conforming adapter that legitimately omits it. assertConforms validates the SHAPE
// of an optional capability when present, and ignores it when absent.
export const OPTIONAL_TRACKER_CAPABILITIES = ['states']

// Storage-aware skills require these only when planStorage selects
// tracker-attachment. Keeping them optional preserves unrelated adapters.
export const OPTIONAL_TRACKER_OPERATIONS = [
  'preparePlanAttachment',
  'finalizePlanAttachment',
  'readPlanAttachment',
  'deletePlanAttachment',
]

// The stable, tracker-agnostic state roles the skills consume by name: the state a
// ticket must sit in to be eligible for scheduling (`planned`), the state a claimed
// ticket moves to (`inProgress`), and the state the merge gate requires (`inReview`).
// An adapter's states() must answer for every role here — with null where it cannot —
// and MAY answer for more.
export const TRACKER_STATE_ROLES = ['planned', 'inProgress', 'inReview']

// The operationMap keys every adapter must populate. This is the required
// surface for the agent-driven capabilities the skills perform through the
// tracker's MCP tools — in particular readComments + writeComment +
// updateComment are the exact three ops the single-comment progress protocol
// needs, with no raw GraphQL required in any driver.
export const REQUIRED_TRACKER_OPERATIONS = [
  'selectPlanned',
  'getIssue',
  'moveState',
  'readComments',
  'writeComment',
  'updateComment',
  'readLabels',
  'extractImages',
  'createLabel',
  'setPriorityEstimate',
  'appendDependency',
]

/**
 * Throw if `adapter` is missing any capability in TRACKER_CAPABILITIES, or if
 * its operationMap is missing any REQUIRED_TRACKER_OPERATIONS entry (or that
 * entry lacks a non-empty string tool/summary). Every adapter's own test
 * calls this to prove conformance.
 *
 * OPTIONAL_TRACKER_CAPABILITIES are never *required* — omitting one conforms — but a
 * present one must be callable. A `states` that is, say, a plain object rather than a
 * function would otherwise pass here and blow up at the call site as a raw TypeError,
 * defeating the fallback the caller wrote.
 * @param {TrackerAdapter} adapter
 */
export function assertConforms(adapter) {
  for (const name of TRACKER_CAPABILITIES) {
    if (adapter?.[name] === undefined) {
      throw new Error(`tracker adapter missing capability: ${name}`)
    }
  }
  for (const name of OPTIONAL_TRACKER_CAPABILITIES) {
    const value = adapter?.[name]
    if (value !== undefined && value !== null && typeof value !== 'function') {
      throw new Error(`tracker adapter optional capability ${name} must be a function`)
    }
  }
  // The capability loop above only proves operationMap is not `undefined`, so a
  // null/array/primitive operationMap would otherwise reach the indexing below and
  // surface as a raw TypeError instead of this contract's own error message.
  if (
    adapter.operationMap === null ||
    typeof adapter.operationMap !== 'object' ||
    Array.isArray(adapter.operationMap)
  ) {
    throw new Error('tracker adapter operationMap must be an object')
  }
  for (const key of REQUIRED_TRACKER_OPERATIONS) {
    const op = adapter.operationMap[key]
    if (!op) {
      throw new Error(`tracker adapter operationMap missing operation: ${key}`)
    }
    // Trimmed: a whitespace-only `tool` is exactly as unusable as an absent one
    // (it names no MCP tool), and `=== ''` alone would let `" "` claim conformance.
    if (typeof op.tool !== 'string' || op.tool.trim() === '') {
      throw new Error(`tracker adapter operation ${key} missing tool`)
    }
    if (typeof op.summary !== 'string' || op.summary.trim() === '') {
      throw new Error(`tracker adapter operation ${key} missing summary`)
    }
  }
  for (const key of OPTIONAL_TRACKER_OPERATIONS) {
    if (!(key in adapter.operationMap)) continue
    const op = adapter.operationMap[key]
    if (!op || typeof op.tool !== 'string' || op.tool.trim() === '') {
      throw new Error(`tracker adapter operation ${key} missing tool`)
    }
    if (typeof op.summary !== 'string' || op.summary.trim() === '') {
      throw new Error(`tracker adapter operation ${key} missing summary`)
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
