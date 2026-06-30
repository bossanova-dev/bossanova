// scripts/linear-deps-lib.test.mjs
// Unit tests for the shared blocking-dependency helpers. node builtins only.

import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  BLOCKER_CLEARED_STATE_TYPES,
  extractBlockers,
  isUnblocked,
  countUnblocked,
  runUnblockedGate,
} from './linear-deps-lib.mjs'

const blocker = (type) => ({
  type: 'blocks',
  issue: { id: type, identifier: type, state: { name: type, type } },
})
const issueWith = (...relTypes) => ({
  id: 'C',
  identifier: 'C',
  inverseRelations: { nodes: relTypes.map(blocker) },
})

test('BLOCKER_CLEARED_STATE_TYPES contains only completed and canceled', () => {
  assert.deepEqual([...BLOCKER_CLEARED_STATE_TYPES].sort(), ['canceled', 'completed'])
})

test('extractBlockers: only blocks-type inverse relations count', () => {
  const issue = {
    inverseRelations: {
      nodes: [blocker('completed'), { type: 'related', issue: { state: { type: 'started' } } }],
    },
  }
  assert.equal(extractBlockers(issue).length, 1)
})

test('extractBlockers: tolerates missing relations', () => {
  assert.deepEqual(extractBlockers({}), [])
  assert.deepEqual(extractBlockers({ inverseRelations: { nodes: [] } }), [])
})

test('isUnblocked: no blockers -> unblocked', () => {
  assert.equal(isUnblocked(issueWith()), true)
})

test('isUnblocked: open blocker (started) blocks', () => {
  assert.equal(isUnblocked(issueWith('started')), false)
})

test('isUnblocked: unstarted blocker (Todo/Unplanned) blocks', () => {
  assert.equal(isUnblocked(issueWith('unstarted')), false)
})

test('isUnblocked: Done blocker clears', () => {
  assert.equal(isUnblocked(issueWith('completed')), true)
})

test('isUnblocked: Canceled blocker clears', () => {
  assert.equal(isUnblocked(issueWith('canceled')), true)
})

test('isUnblocked: all blockers must clear (one open -> blocked)', () => {
  assert.equal(isUnblocked(issueWith('completed', 'started')), false)
  assert.equal(isUnblocked(issueWith('completed', 'canceled')), true)
})

test('countUnblocked: counts only unblocked issues', () => {
  assert.equal(countUnblocked([issueWith(), issueWith('started'), issueWith('completed')]), 2)
})

function fakeFetch({ ok = true, status = 200, json } = {}) {
  const calls = []
  const impl = async (url, options) => {
    calls.push({ url, options })
    return { ok, status, json: async () => json }
  }
  impl.calls = calls
  return impl
}

test('runUnblockedGate: true when at least one candidate is unblocked', async () => {
  const fetchImpl = fakeFetch({
    json: { data: { issues: { nodes: [issueWith('started'), issueWith('completed')] } } },
  })
  const result = await runUnblockedGate({
    apiKey: 'k',
    state: 'Todo',
    label: 'agent-friendly',
    fetchImpl,
  })
  assert.equal(result, true)
  const body = JSON.parse(fetchImpl.calls[0].options.body)
  assert.deepEqual(body.variables.filter, {
    state: { name: { eq: 'Todo' } },
    labels: { name: { eq: 'agent-friendly' } },
  })
  // The candidate window mirrors the skill's `list_issues ... limit=250` universe.
  assert.equal(body.variables.first, 250)
})

test('runUnblockedGate: honors an explicit maxCandidates', async () => {
  const fetchImpl = fakeFetch({ json: { data: { issues: { nodes: [] } } } })
  await runUnblockedGate({
    apiKey: 'k',
    state: 'Todo',
    label: 'agent-friendly',
    maxCandidates: 7,
    fetchImpl,
  })
  assert.equal(JSON.parse(fetchImpl.calls[0].options.body).variables.first, 7)
})

test('runUnblockedGate: false on a malformed (null issues) payload', async () => {
  const fetchImpl = fakeFetch({ json: { data: null } })
  assert.equal(
    await runUnblockedGate({ apiKey: 'k', state: 'Todo', label: 'agent-friendly', fetchImpl }),
    false,
  )
})

test('runUnblockedGate: false when every candidate is blocked', async () => {
  const fetchImpl = fakeFetch({
    json: { data: { issues: { nodes: [issueWith('started'), issueWith('unstarted')] } } },
  })
  assert.equal(
    await runUnblockedGate({ apiKey: 'k', state: 'Todo', label: 'agent-friendly', fetchImpl }),
    false,
  )
})

test('runUnblockedGate: false when no candidates at all', async () => {
  const fetchImpl = fakeFetch({ json: { data: { issues: { nodes: [] } } } })
  assert.equal(
    await runUnblockedGate({ apiKey: 'k', state: 'Todo', label: 'agent-friendly', fetchImpl }),
    false,
  )
})

test('runUnblockedGate: throws on missing key', async () => {
  await assert.rejects(
    () => runUnblockedGate({ apiKey: '', state: 'Todo', fetchImpl: fakeFetch() }),
    /LINEAR_API_KEY/,
  )
})
