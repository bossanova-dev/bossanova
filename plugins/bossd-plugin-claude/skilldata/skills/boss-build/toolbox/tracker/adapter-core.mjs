// skills-toolbox/tracker/adapter-core.mjs
// The tracker-AGNOSTIC half of the tracker-adapter seam: the contract every
// adapter conforms to (typedefs, capability/operation manifests, state roles,
// assertConforms) plus the registry-parameterized resolver. Zero imports —
// node builtins only, and not even those.
//
// Why this is its own module. The sibling `adapter.mjs` binds this contract to
// the Linear reference implementation, and that binding is a STATIC import: it
// pulls tracker/linear.mjs and its transitive helpers (linear-gate-lib,
// linear-deps-lib, linear-claim, bs-epic-lib, dag-scheduler, skill-config) into
// the import closure. A consumer that wants only the contract — to vendor the
// interface, to write a non-Linear adapter, to read the manifests in a lint gate —
// cannot take `adapter.mjs` without also taking every one of those. Splitting the
// contract out makes the interface independently vendorable, which is what the
// header of `adapter.mjs` always claimed ("so a future tracker (GitHub Issues,
// Jira, ...) can slot in behind resolveTrackerAdapter") but the static import
// defeated. Its own test asserts this file's transitive closure is exactly
// itself; keep it import-free.
//
// It also breaks a genuine cycle: tracker/linear.mjs needs TRACKER_STATE_ROLES,
// and importing it from `adapter.mjs` made adapter -> linear -> adapter circular.
// linear.mjs reads it from here instead.
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

// Operations an adapter MAY declare. Two groups live here, for two different reasons:
//
//   1. The native plan-attachment operations, which only plan-storage-aware skills use.
//      Keeping them optional preserves unrelated adapters that implement no plan storage.
//   2. extractImages and createLabel, which are left to adapter discretion because that
//      is how every caller already treats them: the single extractImages call site is
//      documented best-effort, and createLabel has no caller at all. Requiring them at
//      LOAD time rejected otherwise-usable adapters over capabilities nothing depends on
//      at CALL time.
//   3. appendRelatedTo, the non-blocking counterpart to appendDependency. It is optional
//      rather than required because a tracker may model only blocking links; a caller
//      that wants a related edge and finds this op absent degrades to a note instead of
//      failing the run, so requiring it would reject adapters over a capability whose
//      absence is already handled at CALL time.
//
// The invariant for every entry here: an ABSENT optional op conforms, while a DECLARED
// one is validated exactly as strictly as a required op (non-empty tool and summary) —
// an adapter that names an op still owes a usable tool. Only the meaning of absence
// differs from REQUIRED_TRACKER_OPERATIONS. Moving an op here is therefore a widening of
// what conforms, never a weakening of how a declared op is checked.
export const OPTIONAL_TRACKER_OPERATIONS = [
  'preparePlanAttachment',
  'finalizePlanAttachment',
  'readPlanAttachment',
  'deletePlanAttachment',
  'extractImages',
  'createLabel',
  'appendRelatedTo',
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
//
// An op earns a place here only when a skill's control flow DEPENDS on it — where an
// adapter that cannot perform it cannot run the protocol at all. An op every caller
// already treats as best-effort, or that no caller invokes, belongs in
// OPTIONAL_TRACKER_OPERATIONS instead: requiring it here rejects an otherwise-usable
// adapter at load time over a capability nothing depends on at call time.
export const REQUIRED_TRACKER_OPERATIONS = [
  'selectPlanned',
  'getIssue',
  'moveState',
  'readComments',
  'writeComment',
  'updateComment',
  'readLabels',
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

/**
 * Resolve a tracker adapter out of the builder registry `builders`. `env.TRACKER`
 * selects the builder (default "linear"); unknown values throw. Taking the registry
 * as an argument is what keeps this module free of any concrete-tracker import —
 * `adapter.mjs` supplies the Linear-bound registry, a foreign repo supplies its own.
 *
 * SYNCHRONOUS, and it must stay that way: no caller awaits the resolver — the cron
 * gates and tracker/cli.mjs all use its return value directly — so lazily
 * `await import()`-ing a builder here instead of splitting the module would hand
 * every one of them a Promise.
 *
 * `??` not `||`: an explicitly-empty TRACKER='' is a misconfiguration, and coercing
 * it to the default hides that instead of failing on the unknown-tracker path.
 * `Object.hasOwn` not a truthiness check on `builders[name]`: the registry is a plain
 * object literal, so `TRACKER=constructor` would otherwise resolve to `Object` —
 * truthy — and `Object({env, fetchImpl})` would return a plain object that silently
 * impersonates an adapter.
 * @param {{builders: Record<string, (opts: {env: object, fetchImpl: typeof fetch}) => TrackerAdapter>,
 *   env?: object, fetchImpl?: typeof fetch}} opts
 * @returns {TrackerAdapter}
 */
export function resolveTrackerAdapter({ builders, env = process.env, fetchImpl = fetch } = {}) {
  const name = env.TRACKER ?? 'linear'
  if (!builders || !Object.hasOwn(builders, name)) throw new Error(`unknown tracker: ${name}`)
  return builders[name]({ env, fetchImpl })
}
