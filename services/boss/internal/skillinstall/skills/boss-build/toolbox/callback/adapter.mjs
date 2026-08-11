// skills-toolbox/callback/adapter.mjs
// Pluggable callback-notifier adapter for the boss-build / boss-epic skills.
// Abstracts one-shot GitHub PR-event callbacks (register / list / remove a durable
// watch) so any host exposing a `boss callback`-style interface can slot
// in behind resolveCallbackAdapter. This is the fourth instance of the adapter
// pattern (tracker, finalize, session-runner). Like the
// session-runner adapter it is DECLARATIVE: the boss reference records the CLI
// sub-command + arg/response shape per capability; the agent issues the calls.
// node builtins only (the cron worktree is dependency-free).

import { createBossCallbackAdapter } from './boss.mjs'
import { isBossdManaged } from '../bossd-present.mjs'

// Capabilities every callback notifier must document.
export const CALLBACK_CAPABILITIES = ['registerWatch', 'listWatches', 'removeWatch']

/**
 * The single "are callbacks usable here?" gate the skills consult before arming a
 * one-shot PR/CI watch. Returns `isBossdManaged(env)` (BOSS_SESSION_ID present):
 * a daemon-provisioned session can reach the `boss callback` interface, a plain
 * standalone runner cannot. When this is false the skills MUST skip `registerWatch`
 * and go straight to `policy.fallbackPoll` — a clean, documented no-op, never a
 * failed wait. It replaces the older "did the `boss callback` CLI happen to fail at
 * runtime" heuristic with an up-front, unit-testable check, and is the one place to
 * extend if the usability signal ever diverges from raw in-boss (e.g. also requiring
 * the `boss` binary on PATH).
 * @param {Record<string,string|undefined>} [env]
 * @returns {boolean}
 */
export function callbacksAvailable(env = process.env) {
  return isBossdManaged(env)
}

// name -> factory. Add a host by implementing the same operationMap + policy shape.
const REGISTRY = {
  boss: createBossCallbackAdapter,
}

/**
 * Resolve the active callback-notifier adapter from the environment.
 * Selection: env.CALLBACK (default 'boss'). Throws on an unknown notifier or a
 * non-conforming adapter so a misconfigured host fails fast rather than silently
 * skipping callbacks.
 * @param {Record<string,string|undefined>} [env]
 */
export function resolveCallbackAdapter(env = process.env) {
  // `??` not `||`, and Object.hasOwn not a truthiness check: an explicitly-empty
  // CALLBACK='' must fail on the unknown-notifier path rather than silently taking
  // the default, and `CALLBACK=constructor` must not resolve to the inherited
  // `Object` — truthy — and hand back `Object()`, a plain object that would then
  // only fail deep inside assertConforms. Same rule in the tracker, session and
  // finalize resolvers.
  const name = env.CALLBACK ?? 'boss'
  if (!Object.hasOwn(REGISTRY, name)) {
    throw new Error(
      `unknown callback notifier: ${name} (known: ${Object.keys(REGISTRY).join(', ')})`,
    )
  }
  return assertConforms(REGISTRY[name]())
}

/**
 * Validate that an adapter exposes every callback capability plus the callback-watch
 * policy the spine depends on (grouped triggers, reconcile-before-act, re-arm).
 * @returns the adapter, for chaining.
 */
export function assertConforms(adapter) {
  if (typeof adapter?.notifier !== 'string') {
    throw new Error('callback adapter: missing notifier name')
  }
  const map = adapter.operationMap ?? {}
  for (const cap of CALLBACK_CAPABILITIES) {
    if (!(cap in map)) {
      throw new Error(`callback adapter '${adapter.notifier}': missing capability ${cap}`)
    }
  }
  const policy = adapter.policy
  if (!policy || !Array.isArray(policy.watchTriggers) || policy.watchTriggers.length === 0) {
    throw new Error(`callback adapter '${adapter.notifier}': missing policy.watchTriggers`)
  }
  if (
    typeof policy.reconcileBeforeAct !== 'boolean' ||
    typeof policy.rearmWhileWaiting !== 'boolean'
  ) {
    throw new Error(`callback adapter '${adapter.notifier}': missing reconcile/re-arm policy`)
  }
  // The shipped SKILL prose leans on both of these (dedup-by-id under at-least-once
  // delivery, and a bounded poll when callbacks are unavailable), so a conforming
  // adapter must document them too — otherwise the prose contract can silently drift.
  if (typeof policy.dedupById !== 'boolean' || typeof policy.fallbackPoll !== 'string') {
    throw new Error(`callback adapter '${adapter.notifier}': missing dedup/fallback policy`)
  }
  return adapter
}
