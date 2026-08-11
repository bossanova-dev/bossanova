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
