import { test } from 'node:test'
import assert from 'node:assert/strict'

import {
  resolveCallbackAdapter,
  CALLBACK_CAPABILITIES,
  assertConforms,
  callbacksAvailable,
  callbacksUnavailableReason,
} from './adapter.mjs'

// A minimal injectable fs (statSync only) so no gate case reads the machine's real
// PATH or its real ./bin/boss — the verdict must come from the injected world alone.
function fakeFs(files = {}) {
  return {
    statSync(target) {
      const entry = files[target]
      if (!entry) {
        const error = new Error(`ENOENT: no such file or directory, stat '${target}'`)
        error.code = 'ENOENT'
        throw error
      }
      return { mode: entry.mode ?? 0o755, isFile: () => entry.isFile !== false }
    },
  }
}

// No BOSS_BIN, no PATH entry, no .git ancestor and so no ./bin/boss: the managed
// cron environment, which exports BOSS_SESSION_ID and nothing else useful.
const NO_BINARY = { fs: fakeFs({}), cwd: '/tmp/nowhere/sub' }
// A healthy host: `boss` really is on PATH.
const HAS_BINARY = { fs: fakeFs({ '/usr/local/bin/boss': { mode: 0o755 } }), cwd: '/tmp/nowhere' }
const HAS_BINARY_ENV = { PATH: '/usr/local/bin' }

test('CALLBACK_CAPABILITIES lists the register / list / remove capabilities', () => {
  assert.deepEqual(CALLBACK_CAPABILITIES, ['registerWatch', 'listWatches', 'removeWatch'])
})

test('callbacksAvailable is FALSE when BOSS_SESSION_ID is set but no boss binary resolves', () => {
  // BOS-785, the decisive case. This is the managed cron environment exactly: bossd
  // injects BOSS_SESSION_ID, but the cron PATH has no `boss`, so every CI wait armed a
  // registration that could not run and then fell back to polling anyway. Before this
  // change the gate returned TRUE here. It must be false, and it must say why.
  assert.equal(callbacksAvailable({ BOSS_SESSION_ID: 'x' }, NO_BINARY), false)
  assert.match(callbacksUnavailableReason({ BOSS_SESSION_ID: 'x' }, NO_BINARY), /boss executable/)
})

test('callbacksAvailable is true only when BOTH conjuncts hold', () => {
  // managed + binary → true; drop either conjunct → false.
  assert.equal(
    callbacksAvailable({ BOSS_SESSION_ID: 'abc123', ...HAS_BINARY_ENV }, HAS_BINARY),
    true,
  )
  assert.equal(callbacksAvailable({ ...HAS_BINARY_ENV }, HAS_BINARY), false)
  assert.equal(callbacksAvailable({ BOSS_SESSION_ID: 'abc123' }, NO_BINARY), false)
})

test('callbacksAvailable stays false for an absent or empty BOSS_SESSION_ID', () => {
  // BOS-495: unset/empty is standalone, and a reachable binary does not change that —
  // there is no daemon behind the `boss callback` interface to answer.
  assert.equal(callbacksAvailable({}, NO_BINARY), false)
  assert.equal(callbacksAvailable({ ...HAS_BINARY_ENV }, HAS_BINARY), false)
  assert.equal(callbacksAvailable({ BOSS_SESSION_ID: '' }, HAS_BINARY), false)
})

test('callbacksUnavailableReason names WHICH conjunct failed, and is empty when the gate is true', () => {
  // The point of the reason string: "polling, because <x>" instead of a silent degrade.
  assert.match(callbacksUnavailableReason({}, HAS_BINARY), /BOSS_SESSION_ID/)
  assert.match(
    callbacksUnavailableReason({ BOSS_SESSION_ID: 'x', BOSS_BIN: '/gone/boss' }, NO_BINARY),
    /BOSS_BIN=\/gone\/boss/,
  )
  assert.equal(
    callbacksUnavailableReason({ BOSS_SESSION_ID: 'x', ...HAS_BINARY_ENV }, HAS_BINARY),
    '',
  )
})

test('callbacksAvailable honours an explicit env object (no process.env bleed)', () => {
  // Passing an explicit env must decide from THAT env only, even when process.env
  // carries a real BOSS_SESSION_ID (the daemon injects one into managed sessions).
  const saved = process.env.BOSS_SESSION_ID
  try {
    process.env.BOSS_SESSION_ID = 'ambient-session'
    // Explicit env without the var → false, despite the ambient process.env value.
    assert.equal(callbacksAvailable({ CALLBACK: 'boss', ...HAS_BINARY_ENV }, HAS_BINARY), false)
    // Explicit env with the var → true.
    assert.equal(
      callbacksAvailable({ BOSS_SESSION_ID: 'explicit', ...HAS_BINARY_ENV }, HAS_BINARY),
      true,
    )
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

test('an explicitly EMPTY CALLBACK fails fast instead of coercing to the default', () => {
  // `||` would silently treat CALLBACK='' as "boss", hiding a misconfigured host —
  // an unset-vs-blank env var is exactly what a deploy script gets wrong. Here the
  // stakes are callbacks silently going to the wrong notifier rather than failing.
  assert.throws(() => resolveCallbackAdapter({ CALLBACK: '' }), /unknown callback notifier: /)
})

test('an inherited Object.prototype member is NOT a callback notifier', () => {
  // REGISTRY is a plain object literal: a truthiness check on REGISTRY[name] would
  // resolve `constructor` to `Object` and return `Object()`, a plain object that
  // would only fail later, deep inside assertConforms.
  for (const inherited of ['constructor', 'toString', 'valueOf']) {
    assert.throws(
      () => resolveCallbackAdapter({ CALLBACK: inherited }),
      new RegExp(`unknown callback notifier: ${inherited}`),
      `expected a throw for CALLBACK=${inherited}`,
    )
  }
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

test('callback operations declare their supported chat and repository scopes', () => {
  const { operationMap } = resolveCallbackAdapter({})

  // Register and reconciliation list calls are always scoped to the verified
  // callback chat and child PR repository. `remove` deliberately has no --repo
  // flag in the generic CLI, so cleanup first finds ids through the scoped list
  // and then removes each id with its --chat scope.
  assert.deepEqual(operationMap.registerWatch.scope, { chat: true, repo: true })
  assert.deepEqual(operationMap.listWatches.scope, { chat: true, repo: true })
  assert.deepEqual(operationMap.removeWatch.scope, { chat: true, repo: false })
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
