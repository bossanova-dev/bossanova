import { test } from 'node:test'
import assert from 'node:assert/strict'

import {
  resolveCallbackAdapter,
  CALLBACK_CAPABILITIES,
  assertConforms,
  callbacksAvailable,
} from './adapter.mjs'

test('CALLBACK_CAPABILITIES lists the register / list / remove capabilities', () => {
  assert.deepEqual(CALLBACK_CAPABILITIES, ['registerWatch', 'listWatches', 'removeWatch'])
})

test('callbacksAvailable is true iff BOSS_SESSION_ID is set in the passed env', () => {
  // BOS-495: the single "callbacks usable" gate. Present → true; unset/empty → false.
  assert.equal(callbacksAvailable({ BOSS_SESSION_ID: 'abc123' }), true)
  assert.equal(callbacksAvailable({}), false)
  assert.equal(callbacksAvailable({ BOSS_SESSION_ID: '' }), false)
})

test('callbacksAvailable honours an explicit env object (no process.env bleed)', () => {
  // Passing an explicit env must decide from THAT env only, even when process.env
  // carries a real BOSS_SESSION_ID (the daemon injects one into managed sessions).
  const saved = process.env.BOSS_SESSION_ID
  try {
    process.env.BOSS_SESSION_ID = 'ambient-session'
    // Explicit env without the var → false, despite the ambient process.env value.
    assert.equal(callbacksAvailable({ CALLBACK: 'boss' }), false)
    // Explicit env with the var → true.
    assert.equal(callbacksAvailable({ BOSS_SESSION_ID: 'explicit' }), true)
  } finally {
    if (saved === undefined) delete process.env.BOSS_SESSION_ID
    else process.env.BOSS_SESSION_ID = saved
  }
})

test('resolveCallbackAdapter defaults to the boss notifier', () => {
  const adapter = resolveCallbackAdapter({})
  assert.equal(adapter.notifier, 'boss')
})

test('resolveCallbackAdapter defaults to boss with no argument (process.env)', () => {
  const adapter = resolveCallbackAdapter()
  assert.equal(adapter.notifier, 'boss')
})

test('resolveCallbackAdapter honours an explicit CALLBACK=boss', () => {
  const adapter = resolveCallbackAdapter({ CALLBACK: 'boss' })
  assert.equal(adapter.notifier, 'boss')
})

test('resolveCallbackAdapter throws on an unregistered notifier', () => {
  assert.throws(
    () => resolveCallbackAdapter({ CALLBACK: 'nope' }),
    /unknown callback notifier: nope/,
  )
})

test('the boss adapter maps every capability to a `boss callback` sub-command', () => {
  const { operationMap } = resolveCallbackAdapter({})
  assert.equal(operationMap.registerWatch.command, 'boss callback add')
  assert.equal(operationMap.listWatches.command, 'boss callback list')
  assert.equal(operationMap.removeWatch.command, 'boss callback remove')
})

test('registerWatch documents the grouped one-shot flags (group, message, expiresIn)', () => {
  const { operationMap } = resolveCallbackAdapter({})
  for (const flag of ['group', 'message', 'expiresIn']) {
    assert.ok(operationMap.registerWatch.args.includes(flag), `expected registerWatch arg ${flag}`)
  }
})

test('the message body is a secret: it is never echoed in ANY capability response', () => {
  const { operationMap } = resolveCallbackAdapter({})
  // registerWatch *takes* --message as input, so it is the most plausible accidental
  // echo site; assert the secret is absent from every response, not just listWatches.
  for (const cap of ['registerWatch', 'listWatches', 'removeWatch']) {
    assert.ok(
      !operationMap[cap].response.includes('message'),
      `secret --message must not appear in ${cap}.response`,
    )
  }
})

test('policy names the grouped green/red/merged triggers', () => {
  const { policy } = resolveCallbackAdapter({})
  assert.deepEqual(policy.watchTriggers, ['checks_passed', 'checks_failed', 'merged'])
})

test('policy enables reconcile-before-act, re-arm-while-waiting, dedup, and a fallback poll', () => {
  const { policy } = resolveCallbackAdapter({})
  assert.equal(policy.reconcileBeforeAct, true)
  assert.equal(policy.rearmWhileWaiting, true)
  assert.equal(policy.dedupById, true)
  assert.match(policy.fallbackPoll, /gh pr checks/)
})

test('assertConforms passes for the boss reference adapter', () => {
  assert.doesNotThrow(() => assertConforms(resolveCallbackAdapter({})))
})

test('assertConforms throws when the notifier name is missing', () => {
  assert.throws(() => assertConforms({ operationMap: {}, policy: {} }), /missing notifier name/)
})

test('assertConforms throws when a required capability is missing from operationMap', () => {
  const partial = {
    notifier: 'stub',
    operationMap: { registerWatch: { command: 'x' } },
    policy: { watchTriggers: ['merged'], reconcileBeforeAct: true, rearmWhileWaiting: true },
  }
  assert.throws(() => assertConforms(partial), /missing capability listWatches/)
})

test('assertConforms throws when policy.watchTriggers is empty', () => {
  const map = Object.fromEntries(CALLBACK_CAPABILITIES.map((cap) => [cap, {}]))
  assert.throws(
    () => assertConforms({ notifier: 'stub', operationMap: map, policy: { watchTriggers: [] } }),
    /missing policy\.watchTriggers/,
  )
})

test('assertConforms throws when reconcile/re-arm policy flags are missing', () => {
  const map = Object.fromEntries(CALLBACK_CAPABILITIES.map((cap) => [cap, {}]))
  assert.throws(
    () =>
      assertConforms({
        notifier: 'stub',
        operationMap: map,
        policy: { watchTriggers: ['merged'] },
      }),
    /missing reconcile\/re-arm policy/,
  )
})

test('assertConforms throws when the dedup/fallback policy the prose depends on is missing', () => {
  const map = Object.fromEntries(CALLBACK_CAPABILITIES.map((cap) => [cap, {}]))
  assert.throws(
    () =>
      assertConforms({
        notifier: 'stub',
        operationMap: map,
        // Has reconcile/re-arm but omits dedupById + fallbackPoll.
        policy: { watchTriggers: ['merged'], reconcileBeforeAct: true, rearmWhileWaiting: true },
      }),
    /missing dedup\/fallback policy/,
  )
})
