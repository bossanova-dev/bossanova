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

// BOS-1282: the retry lives in the choke point, so this gate inherits it THROUGH ITS IMPORT with
// no edit of its own. That inheritance is the whole argument for putting the policy at
// `linearRequest` rather than in each caller, so it is proven here rather than assumed.
//
// This is the one test in the tree that lets a real backoff elapse: `runUnblockedGate` takes no
// sleep injection, and giving it one purely to speed up a test would be the edit the claim says it
// does not need. One base-delay sleep is the honest price of proving the inheritance end to end.
test('runUnblockedGate inherits the bounded retry with no edit of its own', async () => {
  let calls = 0
  const fetchImpl = async () => {
    calls += 1
    if (calls === 1) throw new Error('fetch failed')
    return {
      ok: true,
      status: 200,
      json: async () => ({
        data: {
          issues: { nodes: [{ id: 'i1', identifier: 'BOS-1', inverseRelations: { nodes: [] } }] },
        },
      }),
    }
  }
  const result = await runUnblockedGate({ apiKey: 'k', state: 'Todo', fetchImpl })
  assert.equal(result, true, 'a transient first failure must no longer end the gate run')
  assert.equal(calls, 2)
})

// BOS-1292: the identity selectors. `runUnblockedGate` is the function the shipped cron gate
// actually reaches, so its forwarding is the one that matters — and the no-selector case below is
// what proves this ticket ships capability with no behavioural switch.

// A fetch that answers the viewer lookup and the gate query differently, keyed off the query text,
// and records every body it was handed. Both the COUNT and the emitted filter are asserted from
// these records rather than claimed in a comment.
function selectorFetch(viewerId = 'usr_me', nodes = [issueWith()]) {
  const bodies = []
  const impl = async (url, options) => {
    const body = JSON.parse(options.body)
    bodies.push(body)
    return {
      ok: true,
      status: 200,
      json: async () =>
        body.query.includes('viewer')
          ? { data: { viewer: { id: viewerId } } }
          : { data: { issues: { nodes } } },
    }
  }
  impl.bodies = bodies
  return impl
}

test('runUnblockedGate: no selector issues exactly one request and the pre-BOS-1292 filter', async () => {
  const fetchImpl = selectorFetch()
  assert.equal(
    await runUnblockedGate({ apiKey: 'k', state: 'Todo', label: 'agent-friendly', fetchImpl }),
    true,
  )
  assert.equal(fetchImpl.bodies.length, 1, 'no stray viewer lookup when no selector is set')
  // The inertness pin for the gate the cron job actually reaches: byte-for-byte the filter the
  // merged tree built before the selectors existed.
  assert.deepEqual(fetchImpl.bodies[0].variables.filter, {
    state: { name: { eq: 'Todo' } },
    labels: { name: { eq: 'agent-friendly' } },
  })
})

test('runUnblockedGate: forwards a resolved assignee into the filter', async () => {
  const fetchImpl = selectorFetch()
  await runUnblockedGate({ apiKey: 'k', state: 'Todo', assignee: 'me', fetchImpl })
  assert.equal(fetchImpl.bodies.length, 2, 'one viewer lookup, then one gate query')
  assert.deepEqual(fetchImpl.bodies[1].variables.filter, {
    state: { name: { eq: 'Todo' } },
    assignee: { id: { eq: 'usr_me' } },
  })
})

test('runUnblockedGate: forwards a concrete creator with no viewer lookup', async () => {
  const fetchImpl = selectorFetch()
  await runUnblockedGate({ apiKey: 'k', state: 'Todo', creator: 'usr_c', fetchImpl })
  assert.equal(fetchImpl.bodies.length, 1, 'a concrete id must not cost a viewer round trip')
  assert.deepEqual(fetchImpl.bodies[0].variables.filter, {
    state: { name: { eq: 'Todo' } },
    creator: { id: { eq: 'usr_c' } },
  })
})

test('runUnblockedGate: forwards assigneeOrCreator as a top-level or', async () => {
  const fetchImpl = selectorFetch('usr_owner')
  await runUnblockedGate({
    apiKey: 'k',
    state: 'Todo',
    label: 'agent-friendly',
    assigneeOrCreator: 'me',
    fetchImpl,
  })
  assert.deepEqual(fetchImpl.bodies[1].variables.filter, {
    state: { name: { eq: 'Todo' } },
    labels: { name: { eq: 'agent-friendly' } },
    or: [{ assignee: { id: { eq: 'usr_owner' } } }, { creator: { id: { eq: 'usr_owner' } } }],
  })
  // The candidate window is untouched by the selectors.
  assert.equal(fetchImpl.bodies[1].variables.first, 250)
})

test('runUnblockedGate: forwards all three selectors at once', async () => {
  const fetchImpl = selectorFetch()
  await runUnblockedGate({
    apiKey: 'k',
    assignee: 'usr_a',
    creator: 'usr_c',
    assigneeOrCreator: 'usr_x',
    fetchImpl,
  })
  assert.deepEqual(fetchImpl.bodies[0].variables.filter, {
    assignee: { id: { eq: 'usr_a' } },
    creator: { id: { eq: 'usr_c' } },
    or: [{ assignee: { id: { eq: 'usr_x' } } }, { creator: { id: { eq: 'usr_x' } } }],
  })
})

// BOS-1292 must-fix: the runner comments used to claim a set selector costs "exactly one" round
// trip. The resolver is not memoised, so that bound only holds while no caller sets two selectors
// to the literal `me` — and the multi-selector test above uses CONCRETE ids, so it exercises zero
// viewer lookups and could never falsify the claim. The numbers below are MEASURED against this
// code, and they are what make the corrected comments load-bearing.
test('runUnblockedGate: each "me" selector costs a viewer lookup of its own', async () => {
  const fetchImpl = selectorFetch()
  await runUnblockedGate({ apiKey: 'k', state: 'Todo', assignee: 'me', creator: 'me', fetchImpl })
  const viewerLookups = fetchImpl.bodies.filter((b) => b.query.includes('viewer'))
  assert.equal(viewerLookups.length, 2, 'two "me" selectors cost two viewer lookups, not one')
  assert.equal(fetchImpl.bodies.length, 3, 'two viewer lookups plus the one gate query')
  assert.deepEqual(fetchImpl.bodies[2].variables.filter, {
    state: { name: { eq: 'Todo' } },
    assignee: { id: { eq: 'usr_me' } },
    creator: { id: { eq: 'usr_me' } },
  })
})

// Fail closed, all the way through the gate rather than only in the resolver: a gate asked to
// narrow by identity must abort, never fall back to scanning the whole board.
test('runUnblockedGate: rejects when the viewer lookup yields no id', async () => {
  const fetchImpl = async () => ({
    ok: true,
    status: 200,
    json: async () => ({ data: { viewer: {} } }),
  })
  await assert.rejects(
    () => runUnblockedGate({ apiKey: 'k', state: 'Todo', assigneeOrCreator: 'me', fetchImpl }),
    /viewer lookup returned no user id/i,
  )
})
