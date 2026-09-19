// skills-toolbox/session/adapter.mjs
// Pluggable session-runner adapter shared by the boss-epic orchestration skill.
// Abstracts the session-orchestration layer the runner drives — create/get/list/
// merge sessions, read check snapshots, resolve repo context, list agent runners
// — plus the sub-skill dispatch commands the runner fans out per ticket
// (implement-one-ticket / repair-one-PR). A future runner (a different daemon,
// a cloud fleet, ...) can slot in behind resolveSessionRunnerAdapter.
//
// This is the third instance of the adapter pattern the Skill Generalization
// project uses, alongside the tracker adapter. Like the
// tracker adapter it is DECLARATIVE: the boss reference records the MCP tool +
// arg/response shape for each capability rather than issuing the MCP calls
// itself — the agent still performs the calls; the map is the single source of
// truth the SKILL prose reads. node builtins only (the cron worktree is
// dependency-free — mirrors the tracker adapter).

import { createBossSessionRunnerAdapter } from './boss.mjs'

/**
 * @typedef {Object} SessionRunnerAdapter
 * @property {string} runner  Stable adapter id (e.g. "boss").
 * @property {Record<string, SessionOperation>} operationMap
 *           Declarative map of capability -> boss MCP tool / sub-skill dispatch.
 * @property {{implement: string, repair: string}} subSkills
 *           The sub-skill commands the runner fans out per ticket.
 */

/**
 * @typedef {Object} SessionOperation
 * @property {string} [tool]      boss MCP tool name (choreography capability).
 * @property {string} [subSkill]  Sub-skill command (fan-out dispatch capability).
 * @property {string[]} [args]    Argument fields the capability relies on.
 * @property {string[]} [response] Response fields the capability reads.
 */

// The capabilities every session-runner adapter must expose. The first seven
// are the boss MCP choreography (poll-only run driving + serialized merge); the
// last two are the sub-skill dispatch commands the runner fans out per ticket.
export const SESSION_RUNNER_CAPABILITIES = [
  'createSession',
  'getSession',
  'listSessions',
  'listCheckSnapshots',
  'mergeSession',
  'resolveContext',
  'listAgents',
  'dispatchImplement',
  'dispatchRepair',
]

/**
 * The DURABLE SESSION-OUTCOME capabilities: register / list-verify / remove a standing subscription
 * that wakes the orchestrator when a child session reaches an outcome.
 *
 * Deliberately NOT part of SESSION_RUNNER_CAPABILITIES, so `assertConforms` still passes for a
 * runner that cannot carry a durable outcome wake. That is not laxity — it is the distinction the
 * driver needs: a REQUIRED capability that is absent means "misconfigured host, fail fast", whereas
 * an absent durable outcome transport means "record the bounded fallback wake, or report
 * RUNNING_BUT_UNWATCHED". Making it required would turn the second case into the first and BLOCK
 * runs that can legitimately proceed on the fallback.
 */
export const SESSION_OUTCOME_CAPABILITIES = [
  'subscribeSessionOutcome',
  'listSessionSubscriptions',
  'removeSessionSubscription',
]

/**
 * Does this adapter declare a durable session-outcome transport? Returns `{available, missing}` —
 * a VERDICT, never a throw, because absence is a documented degradation rather than a wiring error.
 * The driver reads this before it promises a child is watched.
 * @param {SessionRunnerAdapter} adapter
 * @returns {{available: boolean, missing: string[]}}
 */
export function sessionOutcomeCapability(adapter) {
  const map = adapter?.sessionOutcomeMap ?? {}
  const missing = SESSION_OUTCOME_CAPABILITIES.filter((cap) => !(cap in map))
  return { available: missing.length === 0, missing }
}

/**
 * Throw if an adapter CLAIMS a durable session-outcome transport but declares it incompletely — a
 * partial claim is worse than none, because the driver would arm a subscription it can never list
 * back and so can never verify. An adapter with no `sessionOutcomeMap` at all is fine here; use
 * `sessionOutcomeCapability` to detect that.
 * @param {SessionRunnerAdapter} adapter
 */
export function assertSessionOutcomeConforms(adapter) {
  if (!adapter?.sessionOutcomeMap) return
  const { missing } = sessionOutcomeCapability(adapter)
  if (missing.length > 0) {
    throw new Error(
      `session-runner adapter '${adapter.runner}': claims a session-outcome transport but is ` +
        `missing ${missing.join(', ')} — an unlistable subscription can never be verified`,
    )
  }
  for (const cap of SESSION_OUTCOME_CAPABILITIES) {
    const op = adapter.sessionOutcomeMap[cap]
    if (typeof op?.command !== 'string' || op.command.length === 0) {
      throw new Error(`session-runner adapter '${adapter.runner}': ${cap} declares no command`)
    }
    if (!Array.isArray(op.args) || !Array.isArray(op.response)) {
      throw new Error(
        `session-runner adapter '${adapter.runner}': ${cap} declares no args/response`,
      )
    }
  }
}

const REGISTRY = {
  boss: createBossSessionRunnerAdapter,
}

/**
 * Resolve the configured session-runner adapter. `env.SESSION_RUNNER` selects
 * the adapter (default "boss"); unknown values throw. This is the single
 * pluggable choke point — new runners register a factory here.
 *
 * `??` not `||`: an explicitly-empty SESSION_RUNNER='' is a misconfiguration, and
 * coercing it to the default hides that instead of failing on the unknown-runner
 * path. `Object.hasOwn` not a truthiness check on `REGISTRY[name]`: the registry is a
 * plain object literal, so `SESSION_RUNNER=constructor` would otherwise resolve to
 * `Object` — truthy — and `Object()` would return a plain object that silently
 * impersonates an adapter. Same rule in the tracker, finalize and callback resolvers.
 * @param {object} [env] Environment map (defaults to process.env).
 * @returns {SessionRunnerAdapter}
 */
export function resolveSessionRunnerAdapter(env = process.env) {
  const name = env.SESSION_RUNNER ?? 'boss'
  if (!Object.hasOwn(REGISTRY, name)) throw new Error(`unknown session runner: ${name}`)
  return REGISTRY[name]()
}

/**
 * Throw if `adapter` is missing any required piece: a `runner` name, every
 * capability in SESSION_RUNNER_CAPABILITIES as a key on its `operationMap`, and
 * a `subSkills` block naming both the implement and repair sub-skills. Every
 * adapter's own test calls this to prove conformance.
 * @param {SessionRunnerAdapter} adapter
 */
export function assertConforms(adapter) {
  if (typeof adapter?.runner !== 'string') {
    throw new Error('session-runner adapter: missing runner name')
  }
  const map = adapter.operationMap ?? {}
  for (const cap of SESSION_RUNNER_CAPABILITIES) {
    if (!(cap in map)) {
      throw new Error(`session-runner adapter '${adapter.runner}': missing capability ${cap}`)
    }
  }
  if (!adapter.subSkills || !adapter.subSkills.implement || !adapter.subSkills.repair) {
    throw new Error(
      `session-runner adapter '${adapter.runner}': missing subSkills.implement/repair`,
    )
  }
}
