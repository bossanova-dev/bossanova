// skills-toolbox/tracker/adapter.mjs
// The Linear-bound face of the tracker-adapter seam shared by the boss-plan /
// boss-build / boss-epic skills. The tracker-AGNOSTIC contract — typedefs,
// TRACKER_CAPABILITIES, OPTIONAL_TRACKER_CAPABILITIES, OPTIONAL_TRACKER_OPERATIONS,
// TRACKER_STATE_ROLES, REQUIRED_TRACKER_OPERATIONS, assertConforms, and the
// registry-parameterized resolver — lives in ./adapter-core.mjs, which imports
// nothing. This module adds exactly one thing on top: the BUILDERS registry that
// binds "linear" to the reference implementation.
//
// Everything the core exports is re-exported here, so this remains the module every
// existing consumer imports from. Import ./adapter-core.mjs directly only when you
// want the contract WITHOUT the Linear implementation and its transitive helpers
// (linear-gate-lib, linear-deps-lib, linear-claim, bs-epic-lib, dag-scheduler,
// skill-config) — writing a non-Linear adapter, vendoring the interface into a repo
// with a different tracker, or reading the manifests in a lint gate.
// node builtins only (the cron worktree is dependency-free — mirrors
// linear-gate-lib.mjs).

import { createLinearAdapter } from './linear.mjs'
import { resolveTrackerAdapter as resolveFromBuilders } from './adapter-core.mjs'

export {
  TRACKER_CAPABILITIES,
  OPTIONAL_TRACKER_CAPABILITIES,
  OPTIONAL_TRACKER_OPERATIONS,
  TRACKER_STATE_ROLES,
  REQUIRED_TRACKER_OPERATIONS,
  assertConforms,
} from './adapter-core.mjs'

/** @typedef {import('./adapter-core.mjs').TrackerAdapter} TrackerAdapter */
/** @typedef {import('./adapter-core.mjs').TrackerOperation} TrackerOperation */

/**
 * Read the argument names declared by an operation summary's leading
 * `{arg, other=default}` block.
 * @param {TrackerOperation} operation
 * @returns {Set<string>}
 */
export function declaredOperationArgKeys(operation) {
  const match =
    typeof operation?.summary === 'string' ? operation.summary.match(/^\{([^}]*)\}/) : null
  return new Set(
    (match?.[1] ?? '')
      .split(',')
      .map((part) => part.trim().split(/[=:(]/)[0].trim())
      .filter(Boolean),
  )
}

/**
 * Assert that an emitted write-plan entry can be replayed against a tracker
 * adapter operation contract.
 * @param {{op?: string, args?: object, runtimeArgs?: unknown}} entry
 * @param {{operationMap?: Record<string, TrackerOperation>}} adapter
 * @param {{nonAdapterOps?: string[], deliberatelyOmitted?: Record<string, string[]|Set<string>>}} [options]
 */
export function assertWritePlanEntryExecutable(entry, adapter, options = {}) {
  const operationMap = adapter?.operationMap ?? {}
  const nonAdapterOps = new Set(options.nonAdapterOps ?? [])
  for (const op of nonAdapterOps) {
    if (op in operationMap) throw new Error(`${op} is now an adapter operation — re-partition`)
  }

  const op = entry?.op
  if (nonAdapterOps.has(op)) return

  const operation = operationMap[op]
  if (!operation) throw new Error(`${op}: not a tracker adapter operation`)

  const args = entry?.args
  const emittedArgs = args && typeof args === 'object' && !Array.isArray(args) ? args : {}
  if (!Array.isArray(entry?.runtimeArgs))
    throw new Error(`${op}: runtimeArgs must always be present`)

  const declared = declaredOperationArgKeys(operation)
  if (declared.size === 0) throw new Error(`${op}: could not read declared args from its summary`)

  const omitted = new Set(options.deliberatelyOmitted?.[op] ?? [])
  for (const key of Object.keys(emittedArgs)) {
    if (omitted.has(key)) {
      throw new Error(`${op}: "${key}" is pinned as deliberately omitted but is emitted`)
    }
    if (!declared.has(key)) {
      throw new Error(
        `${op}: emits "${key}", which its adapter summary does not declare (${[...declared].join(', ')})`,
      )
    }
  }
  for (const key of entry.runtimeArgs) {
    if (!declared.has(key)) throw new Error(`${op}: runtimeArg "${key}" is not a declared argument`)
  }

  const covered = new Set([...Object.keys(emittedArgs), ...entry.runtimeArgs])
  for (const key of declared) {
    if (omitted.has(key)) continue
    if (!covered.has(key)) {
      throw new Error(
        `${op}: declared argument "${key}" is neither emitted nor listed in runtimeArgs`,
      )
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
 * choke point — new trackers register a builder in BUILDERS above.
 *
 * Signature-compatible with every existing call site: one optional options object,
 * synchronous return. The unknown-tracker / empty-TRACKER rules live in the core.
 * @param {{env?: object, fetchImpl?: typeof fetch}} [opts]
 * @returns {TrackerAdapter}
 */
export function resolveTrackerAdapter({ env = process.env, fetchImpl = fetch } = {}) {
  return resolveFromBuilders({ builders: BUILDERS, env, fetchImpl })
}
