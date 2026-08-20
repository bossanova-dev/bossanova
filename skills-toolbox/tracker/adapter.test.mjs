import { test } from 'node:test'
import assert from 'node:assert/strict'

import * as core from './adapter-core.mjs'
import {
  TRACKER_CAPABILITIES,
  OPTIONAL_TRACKER_CAPABILITIES,
  OPTIONAL_TRACKER_OPERATIONS,
  TRACKER_STATE_ROLES,
  REQUIRED_TRACKER_OPERATIONS,
  resolveTrackerAdapter,
  assertConforms,
} from './adapter.mjs'

// The contract itself is tested in adapter-core.test.mjs. This suite covers what
// adapter.mjs adds: the re-export surface every existing consumer imports from, and
// the Linear-bound BUILDERS registry.

test('every contract symbol is still importable from adapter.mjs (re-export surface)', () => {
  // 14 call sites import these from HERE. The split must not have moved any of them,
  // and identity — not just presence — is what proves there is no second copy.
  assert.equal(TRACKER_CAPABILITIES, core.TRACKER_CAPABILITIES)
  assert.equal(OPTIONAL_TRACKER_CAPABILITIES, core.OPTIONAL_TRACKER_CAPABILITIES)
  assert.equal(OPTIONAL_TRACKER_OPERATIONS, core.OPTIONAL_TRACKER_OPERATIONS)
  assert.equal(TRACKER_STATE_ROLES, core.TRACKER_STATE_ROLES)
  assert.equal(REQUIRED_TRACKER_OPERATIONS, core.REQUIRED_TRACKER_OPERATIONS)
  assert.equal(assertConforms, core.assertConforms)
})

test('resolveTrackerAdapter is adapter.mjs’s OWN registry-bound resolver', () => {
  // Not the core's — the core's requires a `builders` argument. Re-exporting that one
  // would break every call site, all of which call it with at most {env, fetchImpl}.
  assert.notEqual(resolveTrackerAdapter, core.resolveTrackerAdapter)
})

test('resolveTrackerAdapter defaults to the Linear adapter', () => {
  const adapter = resolveTrackerAdapter({ env: { LINEAR_API_KEY: 'k' } })
  assert.equal(adapter.tracker, 'linear')
})

test('resolveTrackerAdapter honours an explicit TRACKER=linear', () => {
  const adapter = resolveTrackerAdapter({ env: { TRACKER: 'linear', LINEAR_API_KEY: 'k' } })
  assert.equal(adapter.tracker, 'linear')
})

test('resolveTrackerAdapter takes no arguments at all', () => {
  // cron-gates/boss-build.mjs:34 calls `resolveTrackerAdapter()` bare, so the whole
  // options object must stay optional and default env to process.env.
  const prev = process.env.LINEAR_API_KEY
  process.env.LINEAR_API_KEY = 'k'
  try {
    assert.equal(resolveTrackerAdapter().tracker, 'linear')
  } finally {
    if (prev === undefined) delete process.env.LINEAR_API_KEY
    else process.env.LINEAR_API_KEY = prev
  }
})

test('resolveTrackerAdapter is SYNCHRONOUS — no call site awaits it', () => {
  // The alternative fix for the coupling (`await import('./linear.mjs')` inside the
  // builder) would return a Promise here and silently break all 14 call sites.
  const adapter = resolveTrackerAdapter({ env: { LINEAR_API_KEY: 'k' } })
  assert.ok(!(adapter instanceof Promise), 'resolveTrackerAdapter must not return a Promise')
})

test('resolveTrackerAdapter throws on an unknown tracker', () => {
  assert.throws(() => resolveTrackerAdapter({ env: { TRACKER: 'jira' } }), /unknown tracker: jira/)
})

test('an explicitly EMPTY TRACKER fails fast instead of resolving to linear', () => {
  assert.throws(() => resolveTrackerAdapter({ env: { TRACKER: '' } }), /unknown tracker: $/)
})

test('an inherited Object.prototype member is NOT a tracker', () => {
  // BUILDERS is a plain object literal, so `TRACKER=constructor` would resolve to
  // `Object` under a truthiness check and hand back a plain object.
  for (const inherited of ['constructor', 'toString', 'valueOf']) {
    assert.throws(
      () => resolveTrackerAdapter({ env: { TRACKER: inherited } }),
      new RegExp(`unknown tracker: ${inherited}`),
      `expected a throw for TRACKER=${inherited}`,
    )
  }
})

test('resolveTrackerAdapter treats a blank LINEAR_API_ENDPOINT as unset', async () => {
  const calls = []
  const fetchImpl = async (url) => {
    calls.push(url)
    return {
      ok: true,
      json: async () => ({ data: { issues: { nodes: [], pageInfo: { hasNextPage: false } } } }),
    }
  }
  const adapter = resolveTrackerAdapter({
    env: { LINEAR_API_KEY: 'k', LINEAR_API_ENDPOINT: '' },
    fetchImpl,
  })
  await adapter.hasWork({ state: 'Unplanned' })
  // Blank endpoint must fall through to Linear's default, never POST to ''.
  assert.equal(calls[0], 'https://api.linear.app/graphql')
})

test('assertConforms passes for the Linear adapter, with all 16 operations declared', () => {
  const adapter = resolveTrackerAdapter({ env: { LINEAR_API_KEY: 'k' } })
  assert.doesNotThrow(() => assertConforms(adapter))
  // Moving extractImages/createLabel to the optional list widened what CONFORMS; it did
  // not shrink what the reference adapter declares. Pinning the count here proves the
  // reference impl is still validated over its whole surface — 9 required + 7 optional —
  // rather than quietly dropping an op now that omitting one would still pass.
  assert.equal(Object.keys(adapter.operationMap).length, 16)
  for (const key of [...REQUIRED_TRACKER_OPERATIONS, ...OPTIONAL_TRACKER_OPERATIONS]) {
    assert.ok(key in adapter.operationMap, `the reference adapter must still declare ${key}`)
  }
})
