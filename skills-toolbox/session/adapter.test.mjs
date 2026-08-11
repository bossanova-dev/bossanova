import { test } from 'node:test'
import assert from 'node:assert/strict'

import {
  resolveSessionRunnerAdapter,
  SESSION_RUNNER_CAPABILITIES,
  assertConforms,
} from './adapter.mjs'

test('SESSION_RUNNER_CAPABILITIES lists the choreography + dispatch capabilities', () => {
  assert.deepEqual(SESSION_RUNNER_CAPABILITIES, [
    'createSession',
    'getSession',
    'listSessions',
    'listCheckSnapshots',
    'mergeSession',
    'resolveContext',
    'listAgents',
    'dispatchImplement',
    'dispatchRepair',
  ])
})

test('resolveSessionRunnerAdapter defaults to the boss session runner', () => {
  const adapter = resolveSessionRunnerAdapter({})
  assert.equal(adapter.runner, 'boss')
})

test('resolveSessionRunnerAdapter defaults to boss with no argument (process.env)', () => {
  const adapter = resolveSessionRunnerAdapter()
  assert.equal(adapter.runner, 'boss')
})

test('resolveSessionRunnerAdapter honours an explicit SESSION_RUNNER=boss', () => {
  const adapter = resolveSessionRunnerAdapter({ SESSION_RUNNER: 'boss' })
  assert.equal(adapter.runner, 'boss')
})

test('resolveSessionRunnerAdapter throws on an unregistered runner', () => {
  assert.throws(
    () => resolveSessionRunnerAdapter({ SESSION_RUNNER: 'nope' }),
    /unknown session runner: nope/,
  )
})

test('an explicitly EMPTY SESSION_RUNNER fails fast instead of coercing to the default', () => {
  // `||` would silently treat SESSION_RUNNER='' as "boss", hiding a misconfigured
  // host — an unset-vs-blank env var is exactly what a deploy script gets wrong.
  assert.throws(
    () => resolveSessionRunnerAdapter({ SESSION_RUNNER: '' }),
    /unknown session runner: $/,
  )
})

test('an inherited Object.prototype member is NOT a session runner', () => {
  // REGISTRY is a plain object literal: a truthiness check on REGISTRY[name] would
  // resolve `constructor` to `Object` and return `Object()`, a plain object that
  // impersonates an adapter until the first capability call.
  for (const inherited of ['constructor', 'toString', 'valueOf']) {
    assert.throws(
      () => resolveSessionRunnerAdapter({ SESSION_RUNNER: inherited }),
      new RegExp(`unknown session runner: ${inherited}`),
      `expected a throw for SESSION_RUNNER=${inherited}`,
    )
  }
})

test('assertConforms passes for the boss reference adapter', () => {
  assert.doesNotThrow(() => assertConforms(resolveSessionRunnerAdapter({})))
})

test('assertConforms throws when the runner name is missing', () => {
  assert.throws(() => assertConforms({ operationMap: {}, subSkills: {} }), /missing runner name/)
})

test('assertConforms throws when a required capability is missing from operationMap', () => {
  const partial = {
    runner: 'stub',
    operationMap: { createSession: { tool: 'create_session' } },
    subSkills: { implement: '/x', repair: '/y' },
  }
  assert.throws(() => assertConforms(partial), /missing capability getSession/)
})

test('assertConforms throws when subSkills.implement/repair are missing', () => {
  const map = Object.fromEntries(SESSION_RUNNER_CAPABILITIES.map((cap) => [cap, {}]))
  assert.throws(
    () => assertConforms({ runner: 'stub', operationMap: map, subSkills: {} }),
    /missing subSkills\.implement\/repair/,
  )
})
