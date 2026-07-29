// scripts/linear-gate-lib.test.mjs
// Unit tests for the shared cron-gate Linear query helpers. node builtins only.

import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  buildIssueCountFilter,
  issuesExist,
  linearRequest,
  runLinearGate,
  gateExit,
} from './linear-gate-lib.mjs'

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

test('issuesExist: false on malformed payload', () => {
  assert.equal(issuesExist({}), false)
  assert.equal(issuesExist(null), false)
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
