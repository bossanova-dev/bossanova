// scripts/linear-gate-lib.test.mjs
// Unit tests for the shared cron-gate Linear query helpers. node builtins only.

import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  ISSUES_CANNOT_EVALUATE,
  buildIssueCountFilter,
  issuesExist,
  linearRequest,
  resolveLinearUserId,
  runLinearGate,
  gateExit,
} from './linear-gate-lib.mjs'
import { TRACKER_RETRY_CAPS } from './tracker/outcome.mjs'

test('buildIssueCountFilter: state only', () => {
  assert.deepEqual(buildIssueCountFilter({ state: 'Unplanned' }), {
    state: { name: { eq: 'Unplanned' } },
  })
})

test('buildIssueCountFilter: state and label', () => {
  assert.deepEqual(buildIssueCountFilter({ state: 'Todo', label: 'agent-friendly' }), {
    state: { name: { eq: 'Todo' } },
    labels: { name: { eq: 'agent-friendly' } },
  })
})

test('buildIssueCountFilter: label omitted when absent', () => {
  const filter = buildIssueCountFilter({ state: 'Unplanned', label: undefined })
  assert.equal('labels' in filter, false)
})

test('buildIssueCountFilter: empty input yields empty filter', () => {
  assert.deepEqual(buildIssueCountFilter(), {})
  assert.deepEqual(buildIssueCountFilter({}), {})
})

test('issuesExist: true when a node is present', () => {
  assert.equal(issuesExist({ data: { issues: { nodes: [{ id: 'x' }] } } }), true)
})

test('issuesExist: false when no nodes', () => {
  assert.equal(issuesExist({ data: { issues: { nodes: [] } } }), false)
})

test('issuesExist: true when pageInfo.hasNextPage even with empty page', () => {
  assert.equal(
    issuesExist({ data: { issues: { nodes: [], pageInfo: { hasNextPage: true } } } }),
    true,
  )
})

// BOS-1282 inverted this: a payload the helper cannot READ no longer answers `false`. The
// assertion below is the successor of the one that pinned the defect.
test('issuesExist: cannot-evaluate on a malformed payload', () => {
  assert.equal(issuesExist({}), ISSUES_CANNOT_EVALUATE)
  assert.equal(issuesExist(null), ISSUES_CANNOT_EVALUATE)
})

// A minimal fake fetch builder returning a Response-like object.
function fakeFetch({ ok = true, status = 200, json } = {}) {
  const calls = []
  const impl = async (url, options) => {
    calls.push({ url, options })
    return {
      ok,
      status,
      json: async () => json,
    }
  }
  impl.calls = calls
  return impl
}

test('runLinearGate: returns true when issues exist', async () => {
  const fetchImpl = fakeFetch({ json: { data: { issues: { nodes: [{ id: '1' }] } } } })
  const result = await runLinearGate({
    apiKey: 'key123',
    state: 'Todo',
    label: 'agent-friendly',
    fetchImpl,
  })
  assert.equal(result, true)
  // Auth header is the raw key (no "Bearer" prefix).
  assert.equal(fetchImpl.calls[0].options.headers.Authorization, 'key123')
  // Endpoint defaults to the Linear GraphQL API.
  assert.equal(fetchImpl.calls[0].url, 'https://api.linear.app/graphql')
  // Filter is sent in the request body.
  const body = JSON.parse(fetchImpl.calls[0].options.body)
  assert.deepEqual(body.variables.filter, {
    state: { name: { eq: 'Todo' } },
    labels: { name: { eq: 'agent-friendly' } },
  })
})

test('runLinearGate: returns false when no issues', async () => {
  const fetchImpl = fakeFetch({ json: { data: { issues: { nodes: [] } } } })
  const result = await runLinearGate({ apiKey: 'k', state: 'Unplanned', fetchImpl })
  assert.equal(result, false)
})

test('runLinearGate: throws when apiKey is empty', async () => {
  await assert.rejects(
    () => runLinearGate({ apiKey: '', state: 'Todo', fetchImpl: fakeFetch() }),
    /LINEAR_API_KEY/,
  )
})

test('runLinearGate: throws on HTTP error status', async () => {
  const fetchImpl = fakeFetch({ ok: false, status: 401, json: {} })
  await assert.rejects(() => runLinearGate({ apiKey: 'k', state: 'Todo', fetchImpl }), /401/)
})

test('runLinearGate: throws on GraphQL errors', async () => {
  const fetchImpl = fakeFetch({ json: { errors: [{ message: 'bad filter' }] } })
  await assert.rejects(() => runLinearGate({ apiKey: 'k', state: 'Todo', fetchImpl }), /bad filter/)
})

test('runLinearGate: honors a custom endpoint', async () => {
  const fetchImpl = fakeFetch({ json: { data: { issues: { nodes: [] } } } })
  await runLinearGate({
    apiKey: 'k',
    state: 'Todo',
    fetchImpl,
    endpoint: 'https://example.test/gql',
  })
  assert.equal(fetchImpl.calls[0].url, 'https://example.test/gql')
})

test('linearRequest: returns json.data and sends raw auth header', async () => {
  const fetchImpl = fakeFetch({ json: { data: { issues: { nodes: [{ id: '1' }] } } } })
  const data = await linearRequest({
    apiKey: 'key123',
    query: 'query X { ok }',
    variables: { a: 1 },
    fetchImpl,
  })
  assert.deepEqual(data, { issues: { nodes: [{ id: '1' }] } })
  assert.equal(fetchImpl.calls[0].options.headers.Authorization, 'key123')
  const body = JSON.parse(fetchImpl.calls[0].options.body)
  assert.deepEqual(body, { query: 'query X { ok }', variables: { a: 1 } })
})

test('linearRequest: throws on missing key, HTTP error, and GraphQL errors', async () => {
  await assert.rejects(
    () => linearRequest({ apiKey: '', query: 'q', fetchImpl: fakeFetch() }),
    /LINEAR_API_KEY/,
  )
  await assert.rejects(
    () =>
      linearRequest({
        apiKey: 'k',
        query: 'q',
        fetchImpl: fakeFetch({ ok: false, status: 500, json: {} }),
      }),
    /500/,
  )
  await assert.rejects(
    () =>
      linearRequest({
        apiKey: 'k',
        query: 'q',
        fetchImpl: fakeFetch({ json: { errors: [{ message: 'boom' }] } }),
      }),
    /boom/,
  )
})

test('gateExit: exits 0 on ok without writing a reason', () => {
  let code
  const writes = []
  gateExit(true, 'should not print', {
    exit: (c) => {
      code = c
    },
    stderr: { write: (s) => writes.push(s) },
  })
  assert.equal(code, 0)
  assert.deepEqual(writes, [])
})

test('gateExit: exits 1 and writes the reason on failure', () => {
  let code
  const writes = []
  gateExit(false, 'no work found', {
    exit: (c) => {
      code = c
    },
    stderr: { write: (s) => writes.push(s) },
  })
  assert.equal(code, 1)
  assert.deepEqual(writes, ['no work found\n'])
})

// ---------------------------------------------------------------------------
// BOS-1282: the choke point retries transient transport failures, and the gate
// stops reporting a payload it could not read as "no work".
// ---------------------------------------------------------------------------

// A fetch that fails the first `failures` times with `error`, then succeeds.
function flakyFetch({ failures, error, json = { data: { issues: { nodes: [] } } } } = {}) {
  let calls = 0
  const impl = async () => {
    calls += 1
    if (calls <= failures) throw error()
    return { ok: true, status: 200, json: async () => json }
  }
  Object.defineProperty(impl, 'callCount', { get: () => calls })
  return impl
}

const noSleep = async () => {}

test('linearRequest resolves when a transient transport failure clears', async () => {
  const fetchImpl = flakyFetch({
    failures: 1,
    error: () => new Error('fetch failed'),
    json: { data: { ok: true } },
  })
  const data = await linearRequest({ apiKey: 'k', query: 'q', fetchImpl, sleep: noSleep })
  assert.deepEqual(data, { ok: true })
  // Exactly 2: one failure then one success. `> 1` would also pass a regression that kept
  // firing attempts after the call had already resolved.
  assert.equal(fetchImpl.callCount, 2)
})

// BOS-1282 review: the retry choke point must take its operation from the CALLER. It defaults to
// `read`, but a caller that sends a mutation passes `write`, and a write that may already have
// applied is NOT re-sent — it classifies `indeterminate`, which declines the retry. Without this
// the module's whole reason for existing is defeated at the one site that actually writes.
test('linearRequest does not retry a transient failure on an operation: write call', async () => {
  const fetchImpl = flakyFetch({
    failures: 1,
    error: () => new Error('fetch failed'),
    json: { data: { ok: true } },
  })
  await assert.rejects(
    () =>
      linearRequest({
        apiKey: 'k',
        query: 'mutation M { issueUpdate { success } }',
        fetchImpl,
        operation: 'write',
        sleep: noSleep,
      }),
    /fetch failed/,
  )
  assert.equal(fetchImpl.callCount, 1, 'a write that may have applied must not be re-sent')
})

test('linearRequest still retries the same failure when the caller says read', async () => {
  const fetchImpl = flakyFetch({
    failures: 1,
    error: () => new Error('fetch failed'),
    json: { data: { ok: true } },
  })
  const data = await linearRequest({
    apiKey: 'k',
    query: 'q',
    fetchImpl,
    operation: 'read',
    sleep: noSleep,
  })
  assert.deepEqual(data, { ok: true })
  assert.equal(fetchImpl.callCount, 2)
})

// BOS-1282 review: a GraphQL rejection is server-side and deterministic whatever words the server
// puts in it. Classified from prose alone, a message merely CONTAINING a transport token ("the
// connection was terminated by the peer") matched TRANSPORT before VALIDATION and was re-queried.
// The error now carries its provenance on a field, so the text cannot override it.
test('a GraphQL error naming a transport word is still permanent, not retried', async () => {
  let calls = 0
  const fetchImpl = async () => {
    calls += 1
    return {
      ok: true,
      status: 200,
      json: async () => ({
        errors: [{ message: 'upstream subscription terminated while resolving field' }],
      }),
    }
  }
  await assert.rejects(
    () => linearRequest({ apiKey: 'k', query: 'q', fetchImpl, sleep: noSleep }),
    /Linear GraphQL error/,
  )
  assert.equal(calls, 1, 'a GraphQL rejection is deterministic and must not be re-queried')
})

test('linearRequest rejects after exactly the capped number of attempts', async () => {
  const fetchImpl = flakyFetch({ failures: Infinity, error: () => new Error('fetch failed') })
  await assert.rejects(
    () => linearRequest({ apiKey: 'k', query: 'q', fetchImpl, sleep: noSleep }),
    /fetch failed/,
  )
  assert.equal(fetchImpl.callCount, TRACKER_RETRY_CAPS.maxAttempts)
})

test('linearRequest retries an HTTP 429 and resolves', async () => {
  let calls = 0
  const fetchImpl = async () => {
    calls += 1
    if (calls === 1) return { ok: false, status: 429, json: async () => ({}) }
    return { ok: true, status: 200, json: async () => ({ data: { ok: true } }) }
  }
  const data = await linearRequest({ apiKey: 'k', query: 'q', fetchImpl, sleep: noSleep })
  assert.deepEqual(data, { ok: true })
  assert.equal(calls, 2)
})

test('linearRequest never retries a missing key or a GraphQL error', async () => {
  let calls = 0
  const countingFetch = async () => {
    calls += 1
    return { ok: true, status: 200, json: async () => ({ errors: [{ message: 'bad filter' }] }) }
  }
  await assert.rejects(
    () => linearRequest({ apiKey: '', query: 'q', fetchImpl: countingFetch, sleep: noSleep }),
    /LINEAR_API_KEY/,
  )
  assert.equal(calls, 0, 'a missing key must never reach the network')

  await assert.rejects(
    () => linearRequest({ apiKey: 'k', query: 'q', fetchImpl: countingFetch, sleep: noSleep }),
    /bad filter/,
  )
  assert.equal(calls, 1, 'a GraphQL validation error must fail on the first attempt')
})

test('linearRequest does not retry an HTTP 401', async () => {
  let calls = 0
  const fetchImpl = async () => {
    calls += 1
    return { ok: false, status: 401, json: async () => ({}) }
  }
  await assert.rejects(
    () => linearRequest({ apiKey: 'k', query: 'q', fetchImpl, sleep: noSleep }),
    /401/,
  )
  assert.equal(calls, 1)
})

test('issuesExist returns the cannot-evaluate answer for a payload it cannot read', () => {
  for (const malformed of [{}, null, undefined, { data: {} }, { data: { issues: {} } }]) {
    assert.equal(
      issuesExist(malformed),
      ISSUES_CANNOT_EVALUATE,
      `${JSON.stringify(malformed) ?? String(malformed)} must not read as a clean negative`,
    )
  }
  // The discriminating pair: an unreadable payload and a genuine empty page are NOT the same.
  assert.notEqual(ISSUES_CANNOT_EVALUATE, false)
  assert.equal(issuesExist({ data: { issues: { nodes: [] } } }), false)
})

test('runLinearGate raises on a payload it could not evaluate, naming that it could not', async () => {
  const fetchImpl = fakeFetch({ json: { data: { notIssues: true } } })
  await assert.rejects(
    () => runLinearGate({ apiKey: 'k', state: 'Todo', fetchImpl, sleep: noSleep }),
    /could not evaluate/i,
  )
})

test('runLinearGate still returns false for a genuine zero-row read', async () => {
  const fetchImpl = fakeFetch({ json: { data: { issues: { nodes: [], pageInfo: {} } } } })
  assert.equal(
    await runLinearGate({ apiKey: 'k', state: 'Todo', fetchImpl, sleep: noSleep }),
    false,
  )
})

// BOS-1292: the identity selectors. Every assertion below is deep-equality on the whole returned
// object rather than a membership check, so a clause that appears where none was asked for fails
// the test instead of passing it unnoticed.

// UNIT 4 — the inertness pin. This is the assertion the epic's staged rollout rests on: the
// capability lands with no switch, so the filter a `{state, label}` caller emits after this change
// must be the one the merged tree emitted before it. Asserted as an EXACT object — an extra key
// of any name fails here — and duplicated deliberately from the `state and label` case above,
// because that case pins the builder's behaviour while this one pins the ABSENCE of the new
// clauses. A later edit that made a selector default to something truthy would pass the first and
// fail this one, which is the whole point of writing it twice.
test('buildIssueCountFilter: no selector emits exactly the pre-BOS-1292 filter', () => {
  assert.deepEqual(buildIssueCountFilter({ state: 'Todo', label: 'agent-friendly' }), {
    state: { name: { eq: 'Todo' } },
    labels: { name: { eq: 'agent-friendly' } },
  })
  assert.deepEqual(buildIssueCountFilter({ state: 'Unplanned' }), {
    state: { name: { eq: 'Unplanned' } },
  })
  assert.deepEqual(buildIssueCountFilter(), {})
})

// UNIT 3 — the label OR-set. The STRING path is pinned first and by deep equality, because the
// whole safety argument for this widening is that a caller which passes a single label name emits
// the byte-identical filter it emitted before the array form existed.
test('buildIssueCountFilter: a string label still emits the single equality clause', () => {
  assert.deepEqual(buildIssueCountFilter({ state: 'Todo', label: 'agent-friendly' }), {
    state: { name: { eq: 'Todo' } },
    labels: { name: { eq: 'agent-friendly' } },
  })
  // `eq` and nothing else: an implementation that promoted every label to the membership form
  // would still match "a labels clause is present", and only reading the comparator catches it.
  assert.deepEqual(Object.keys(buildIssueCountFilter({ label: 'agent-friendly' }).labels.name), [
    'eq',
  ])
})

test('buildIssueCountFilter: an array label emits a disjunctive membership clause', () => {
  const filter = buildIssueCountFilter({ state: 'Todo', label: ['label-a', 'label-b'] })
  assert.deepEqual(filter, {
    state: { name: { eq: 'Todo' } },
    labels: { name: { in: ['label-a', 'label-b'] } },
  })
  // Spelled out separately from the deep-equal: the membership clause must AND WITH the state
  // clause rather than replace it, and it must not reach for the single top-level `or`, which
  // belongs to the identity disjunction and cannot be shared.
  assert.deepEqual(filter.state, { name: { eq: 'Todo' } })
  assert.equal('or' in filter, false)
})

// Asserted explicitly rather than folded into the case above: a single-element array and the
// equivalent string are BOTH accepted and emit DIFFERENT comparators, because the emitted filter
// is a function of the caller's spelling alone. Collapsing the one-element array to `eq` would
// pass the array case above and fail here.
test('buildIssueCountFilter: a one-element array and its equivalent string both work', () => {
  assert.deepEqual(buildIssueCountFilter({ label: ['label-a'] }), {
    labels: { name: { in: ['label-a'] } },
  })
  assert.deepEqual(buildIssueCountFilter({ label: 'label-a' }), {
    labels: { name: { eq: 'label-a' } },
  })
})

test('buildIssueCountFilter: an empty label array contributes no label clause at all', () => {
  const filter = buildIssueCountFilter({ state: 'Todo', label: [] })
  assert.deepEqual(filter, { state: { name: { eq: 'Todo' } } })
  // `in` rather than a truthiness read: a `labels` key present and undefined would still be
  // serialised into the GraphQL body and is not the same request.
  assert.equal('labels' in filter, false)
})

test('buildIssueCountFilter: an array label coexists with the identity disjunction', () => {
  // The collision this shape was chosen to avoid: both selectors set at once must still produce
  // ONE top-level `or` (the identity one) with the label set beside it, not under it.
  const filter = buildIssueCountFilter({
    state: 'Todo',
    label: ['label-a', 'label-b'],
    assigneeOrCreatorId: 'usr_1',
  })
  assert.deepEqual(filter, {
    state: { name: { eq: 'Todo' } },
    labels: { name: { in: ['label-a', 'label-b'] } },
    or: [{ assignee: { id: { eq: 'usr_1' } } }, { creator: { id: { eq: 'usr_1' } } }],
  })
  assert.equal(filter.or.length, 2)
})

test('buildIssueCountFilter: assigneeId adds a single equality clause', () => {
  assert.deepEqual(buildIssueCountFilter({ state: 'Todo', assigneeId: 'usr_1' }), {
    state: { name: { eq: 'Todo' } },
    assignee: { id: { eq: 'usr_1' } },
  })
})

test('buildIssueCountFilter: creatorId adds a single equality clause', () => {
  assert.deepEqual(buildIssueCountFilter({ state: 'Todo', creatorId: 'usr_1' }), {
    state: { name: { eq: 'Todo' } },
    creator: { id: { eq: 'usr_1' } },
  })
})

// The shape the MCP `list_issues` surface cannot express at all — no creator filter, no OR — and
// so the reason this capability can only live in the GraphQL-backed gate libraries.
test('buildIssueCountFilter: assigneeOrCreatorId emits a two-element top-level or', () => {
  const filter = buildIssueCountFilter({
    state: 'Todo',
    label: 'agent-friendly',
    assigneeOrCreatorId: 'usr_1',
  })
  assert.deepEqual(filter, {
    state: { name: { eq: 'Todo' } },
    labels: { name: { eq: 'agent-friendly' } },
    or: [{ assignee: { id: { eq: 'usr_1' } } }, { creator: { id: { eq: 'usr_1' } } }],
  })
  // Spelled out separately from the deep-equal above: the `or` must AND WITH the state and label
  // clauses, not replace them. A refactor that moved the whole filter under the disjunction would
  // still be a two-element `or`, and only this pair of assertions catches it.
  assert.equal(filter.or.length, 2)
  assert.deepEqual(filter.state, { name: { eq: 'Todo' } })
})

test('buildIssueCountFilter: all three selectors coexist when all are set', () => {
  assert.deepEqual(
    buildIssueCountFilter({ assigneeId: 'a', creatorId: 'c', assigneeOrCreatorId: 'x' }),
    {
      assignee: { id: { eq: 'a' } },
      creator: { id: { eq: 'c' } },
      or: [{ assignee: { id: { eq: 'x' } } }, { creator: { id: { eq: 'x' } } }],
    },
  )
})

test('buildIssueCountFilter: each selector contributes no key when falsy', () => {
  for (const falsy of [undefined, null, '', false, 0]) {
    const filter = buildIssueCountFilter({
      state: 'Todo',
      assigneeId: falsy,
      creatorId: falsy,
      assigneeOrCreatorId: falsy,
    })
    assert.deepEqual(
      filter,
      { state: { name: { eq: 'Todo' } } },
      `${JSON.stringify(falsy) ?? String(falsy)} must contribute no clause at all`,
    )
    // `in` rather than a truthiness read: a key present and undefined would still be serialised
    // into the GraphQL body, which is the failure this guards.
    assert.equal('assignee' in filter, false)
    assert.equal('creator' in filter, false)
    assert.equal('or' in filter, false)
  }
})

// A fetch that counts calls and answers every query with the same viewer payload. The COUNT is
// the assertion in the resolver tests below — a comment claiming "no request is issued" proves
// nothing, whereas a counter that stayed at zero does.
function countingViewerFetch(json = { data: { viewer: { id: 'usr_viewer' } } }) {
  const impl = async () => {
    impl.count += 1
    return { ok: true, status: 200, json: async () => json }
  }
  impl.count = 0
  return impl
}

test('resolveLinearUserId: a falsy selector resolves to undefined with zero requests', async () => {
  for (const falsy of [undefined, null, '', false]) {
    const fetchImpl = countingViewerFetch()
    assert.equal(await resolveLinearUserId({ apiKey: 'k', user: falsy, fetchImpl }), undefined)
    assert.equal(fetchImpl.count, 0, `${String(falsy)} must not reach the network`)
  }
})

test('resolveLinearUserId: a concrete id passes through with zero requests', async () => {
  const fetchImpl = countingViewerFetch()
  assert.equal(await resolveLinearUserId({ apiKey: 'k', user: 'usr_abc', fetchImpl }), 'usr_abc')
  assert.equal(fetchImpl.count, 0)
})

test('resolveLinearUserId: "me" issues exactly one request and returns the viewer id', async () => {
  const fetchImpl = countingViewerFetch()
  assert.equal(await resolveLinearUserId({ apiKey: 'k', user: 'me', fetchImpl }), 'usr_viewer')
  assert.equal(fetchImpl.count, 1)
})

// Fail CLOSED. Returning undefined here would drop the clause and widen the gate to the whole
// board, which is the exact fail-open this ticket exists to make impossible.
test('resolveLinearUserId: throws when the viewer payload carries no id', async () => {
  for (const json of [{ data: { viewer: {} } }, { data: { viewer: null } }, { data: {} }]) {
    await assert.rejects(
      () => resolveLinearUserId({ apiKey: 'k', user: 'me', fetchImpl: countingViewerFetch(json) }),
      /viewer lookup returned no user id/i,
    )
  }
})

test('runLinearGate: forwards a resolved assignee into the filter', async () => {
  const calls = []
  const fetchImpl = async (url, options) => {
    calls.push(JSON.parse(options.body))
    const isViewer = calls[calls.length - 1].query.includes('viewer')
    return {
      ok: true,
      status: 200,
      json: async () =>
        isViewer
          ? { data: { viewer: { id: 'usr_me' } } }
          : { data: { issues: { nodes: [{ id: '1' }] } } },
    }
  }
  assert.equal(await runLinearGate({ apiKey: 'k', state: 'Todo', assignee: 'me', fetchImpl }), true)
  assert.equal(calls.length, 2, 'one viewer lookup, then one gate query')
  assert.deepEqual(calls[1].variables.filter, {
    state: { name: { eq: 'Todo' } },
    assignee: { id: { eq: 'usr_me' } },
  })
})

test('runLinearGate: forwards a concrete creator with no viewer lookup', async () => {
  const fetchImpl = fakeFetch({ json: { data: { issues: { nodes: [] } } } })
  await runLinearGate({ apiKey: 'k', state: 'Todo', creator: 'usr_c', fetchImpl })
  assert.equal(fetchImpl.calls.length, 1, 'a concrete id must not cost a viewer round trip')
  assert.deepEqual(JSON.parse(fetchImpl.calls[0].options.body).variables.filter, {
    state: { name: { eq: 'Todo' } },
    creator: { id: { eq: 'usr_c' } },
  })
})

// BOS-1292 must-fix: `runLinearGate` used to destructure only {assignee, creator}, so an
// `assigneeOrCreator` handed to it was dropped by destructuring and the gate scanned the whole
// board. Asserted from the EMITTED filter, not from the arguments, so a forward that stops at the
// signature still fails.
test('runLinearGate: forwards assigneeOrCreator as a top-level or beside state and labels', async () => {
  const calls = []
  const fetchImpl = async (url, options) => {
    calls.push(JSON.parse(options.body))
    const isViewer = calls[calls.length - 1].query.includes('viewer')
    return {
      ok: true,
      status: 200,
      json: async () =>
        isViewer
          ? { data: { viewer: { id: 'usr_owner' } } }
          : { data: { issues: { nodes: [{ id: '1' }] } } },
    }
  }
  assert.equal(
    await runLinearGate({
      apiKey: 'k',
      state: 'Todo',
      label: 'agent-friendly',
      assigneeOrCreator: 'me',
      fetchImpl,
    }),
    true,
  )
  assert.equal(calls.length, 2, 'one viewer lookup, then one gate query')
  // The `or` ANDs with its siblings rather than replacing them: all three keys must survive.
  assert.deepEqual(calls[1].variables.filter, {
    state: { name: { eq: 'Todo' } },
    labels: { name: { eq: 'agent-friendly' } },
    or: [{ assignee: { id: { eq: 'usr_owner' } } }, { creator: { id: { eq: 'usr_owner' } } }],
  })
})

test('runLinearGate: a concrete assigneeOrCreator costs zero viewer round trips', async () => {
  const fetchImpl = fakeFetch({ json: { data: { issues: { nodes: [] } } } })
  await runLinearGate({ apiKey: 'k', state: 'Todo', assigneeOrCreator: 'usr_x', fetchImpl })
  assert.equal(fetchImpl.calls.length, 1, 'a concrete id must not cost a viewer round trip')
  assert.deepEqual(JSON.parse(fetchImpl.calls[0].options.body).variables.filter, {
    state: { name: { eq: 'Todo' } },
    or: [{ assignee: { id: { eq: 'usr_x' } } }, { creator: { id: { eq: 'usr_x' } } }],
  })
})

test('runLinearGate: no selector issues exactly one request and the pre-BOS-1292 filter', async () => {
  const fetchImpl = fakeFetch({ json: { data: { issues: { nodes: [] } } } })
  await runLinearGate({ apiKey: 'k', state: 'Todo', label: 'agent-friendly', fetchImpl })
  assert.equal(fetchImpl.calls.length, 1, 'no stray viewer lookup on the unselected path')
  assert.deepEqual(JSON.parse(fetchImpl.calls[0].options.body).variables.filter, {
    state: { name: { eq: 'Todo' } },
    labels: { name: { eq: 'agent-friendly' } },
  })
})
