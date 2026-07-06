import { test } from 'node:test'
import assert from 'node:assert/strict'

import { TRACKER_CAPABILITIES, resolveTrackerAdapter, assertConforms } from './adapter.mjs'

test('TRACKER_CAPABILITIES lists the full capability surface', () => {
  assert.deepEqual(TRACKER_CAPABILITIES, [
    'hasWork',
    'hasUnblockedWork',
    'readDependencies',
    'isUnblocked',
    'formatClaimComment',
    'resolveClaim',
    'normalizeTicket',
    'operationMap',
  ])
})

test('resolveTrackerAdapter defaults to the Linear adapter', () => {
  const adapter = resolveTrackerAdapter({ env: { LINEAR_API_KEY: 'k' } })
  assert.equal(adapter.tracker, 'linear')
})

test('resolveTrackerAdapter honours an explicit TRACKER=linear', () => {
  const adapter = resolveTrackerAdapter({ env: { TRACKER: 'linear', LINEAR_API_KEY: 'k' } })
  assert.equal(adapter.tracker, 'linear')
})

test('resolveTrackerAdapter throws on an unknown tracker', () => {
  assert.throws(() => resolveTrackerAdapter({ env: { TRACKER: 'jira' } }), /unknown tracker: jira/)
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

test('assertConforms passes for the Linear adapter', () => {
  const adapter = resolveTrackerAdapter({ env: { LINEAR_API_KEY: 'k' } })
  assert.doesNotThrow(() => assertConforms(adapter))
})

test('assertConforms throws when a capability is missing', () => {
  assert.throws(() => assertConforms({ tracker: 'stub' }), /missing capability: hasWork/)
})
