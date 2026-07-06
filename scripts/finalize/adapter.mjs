// scripts/finalize/adapter.mjs
// Pluggable finalize-adapter interface for the boss-implement skill. Abstracts the
// tag-injection -> draft->ready -> repair finalize policy the skill owns today so a
// future project can slot a different finalize workflow in behind
// resolveFinalizeAdapter. Mirrors the capability-manifest + operation-map structure of
// scripts/tracker/adapter.mjs (the factory takes a positional env here). node builtins
// only (the cron worktree is dependency-free).
//
// A FinalizeAdapter has:
//   - injectPrTag(prNumber, {baseBranch}) — the one executable capability today
//     (delegate to the tag-injection command); and
//   - operationMap, a declarative description of the agent-driven finalize
//     capabilities (readyPr, repair) plus the tag-injection command — the single
//     source of truth the spine names instead of hard-wiring commands; and
//   - policy, the finalize policy constants (tag format, repair cap, settle cap).

import { createBossFinalizeAdapter } from './boss-finalize.mjs'

/**
 * @typedef {Object} FinalizeAdapter
 * @property {string} finalize   Stable adapter id (e.g. "boss-finalize").
 * @property {(prNumber: string|number, opts: {baseBranch: string}) => any} injectPrTag
 *           Inject the PR-number tag into the branch's commits (delegates to the
 *           tag-injection command with the PR number + BASE_BRANCH).
 * @property {Record<string, object>} operationMap
 *           Declarative map of finalize capability -> concrete command/skill.
 * @property {{tagFormat: string, repairCap: number, settleCap: number}} policy
 *           Finalize policy constants the spine references instead of magic numbers.
 */

export const FINALIZE_CAPABILITIES = Object.freeze(['injectPrTag', 'readyPr', 'repair'])

/**
 * Throw if `adapter` is missing any capability in FINALIZE_CAPABILITIES or the policy
 * constants. injectPrTag is an executable method; readyPr/repair are declarative
 * operation-map entries. Every adapter's own test calls this to prove conformance.
 * @param {FinalizeAdapter} adapter
 * @returns {FinalizeAdapter}
 */
export function assertConforms(adapter) {
  for (const cap of FINALIZE_CAPABILITIES) {
    if (cap === 'injectPrTag') {
      if (typeof adapter?.injectPrTag !== 'function') {
        throw new Error('finalize adapter missing method: injectPrTag')
      }
    } else if (!adapter?.operationMap?.[cap]) {
      throw new Error(`finalize adapter missing operation: ${cap}`)
    }
  }
  if (!adapter?.policy || typeof adapter.policy.repairCap !== 'number') {
    throw new Error('finalize adapter missing policy.repairCap')
  }
  return adapter
}

const BUILDERS = {
  'boss-finalize': ({ runImpl }) => createBossFinalizeAdapter({ runImpl }),
}

/**
 * Resolve the configured finalize adapter. `env.FINALIZE` selects the adapter
 * (default "boss-finalize"); unknown values throw. This is the single pluggable
 * choke point — new finalize workflows register a builder here.
 * @param {object} [env]
 * @param {{runImpl?: Function}} [opts]
 * @returns {FinalizeAdapter}
 */
export function resolveFinalizeAdapter(env = process.env, { runImpl } = {}) {
  const name = env.FINALIZE || 'boss-finalize'
  const build = BUILDERS[name]
  if (!build) throw new Error(`unknown finalize: ${name}`)
  return assertConforms(build({ runImpl }))
}
