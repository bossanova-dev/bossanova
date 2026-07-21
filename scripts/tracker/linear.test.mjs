import { test } from 'node:test'
import assert from 'node:assert/strict'

import { createLinearAdapter, linearOperationMap } from './linear.mjs'
import { assertConforms } from './adapter.mjs'
import { formatClaimComment } from '../linear-claim.mjs'

// A fetchImpl that records the POST and returns a canned GraphQL payload.
function fakeFetch(nodes) {
  const calls = []
  const impl = async (url, init) => {
    calls.push({ url, body: JSON.parse(init.body), headers: init.headers })
    return {
      ok: true,
      json: async () => ({ data: { issues: { nodes, pageInfo: { hasNextPage: false } } } }),
    }
  }
  return { impl, calls }
}

test('the Linear adapter conforms to the interface', () => {
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {} })
  assert.equal(adapter.tracker, 'linear')
  assert.doesNotThrow(() => assertConforms(adapter))
})

test('hasWork delegates to runLinearGate and preserves the auth header', async () => {
  const { impl, calls } = fakeFetch([{ id: 'iss_1' }])
  const adapter = createLinearAdapter({ apiKey: 'secret-key', fetchImpl: impl })
  const has = await adapter.hasWork({ state: 'Unplanned' })
  assert.equal(has, true)
  // Auth header carries the key directly, no "Bearer" prefix.
  assert.equal(calls[0].headers.Authorization, 'secret-key')
})

test('a custom endpoint threads through to the fetchImpl URL', async () => {
  const { impl, calls } = fakeFetch([{ id: 'iss_1' }])
  const adapter = createLinearAdapter({
    apiKey: 'k',
    fetchImpl: impl,
    endpoint: 'https://linear.example/graphql',
  })
  await adapter.hasWork({ state: 'Unplanned' })
  assert.equal(calls[0].url, 'https://linear.example/graphql')
})

test('hasWork returns false when no issue matches', async () => {
  const { impl } = fakeFetch([])
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: impl })
  assert.equal(await adapter.hasWork({ state: 'Unplanned' }), false)
})

test('hasUnblockedWork delegates to runUnblockedGate', async () => {
  const { impl } = fakeFetch([
    { id: 'iss_1', identifier: 'BOS-1', inverseRelations: { nodes: [] } },
  ])
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: impl })
  assert.equal(await adapter.hasUnblockedWork({ state: 'Todo', label: 'agent-friendly' }), true)
})

test('resolveClaim reproduces first-writer-wins', () => {
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {} })
  const mine = 'a'.repeat(32)
  const theirs = 'b'.repeat(32)
  const comments = [
    { body: formatClaimComment(theirs), createdAt: '2026-01-01T00:00:02Z' },
    { body: formatClaimComment(mine), createdAt: '2026-01-01T00:00:01Z' },
  ]
  assert.equal(adapter.resolveClaim(comments, mine), true)
  assert.equal(adapter.resolveClaim(comments, theirs), false)
})

test('normalizeTicket flattens a raw GraphQL issue', () => {
  const adapter = createLinearAdapter({ apiKey: 'k', fetchImpl: async () => {} })
  const ticket = adapter.normalizeTicket({
    identifier: 'BOS-1',
    title: 'x',
    priority: 2,
    createdAt: '2026-01-01T00:00:00Z',
    state: { name: 'Todo', type: 'unstarted' },
    labels: { nodes: [{ name: 'agent-friendly' }] },
    inverseRelations: { nodes: [] },
  })
  assert.equal(ticket.id, 'BOS-1')
  assert.equal(ticket.stateName, 'Todo')
  assert.deepEqual(ticket.labels, ['agent-friendly'])
})

test('linearOperationMap names the agent-driven MCP tools', () => {
  assert.equal(linearOperationMap.selectPlanned.tool, 'mcp__bossanova-linear__list_issues')
  assert.equal(linearOperationMap.moveState.tool, 'mcp__bossanova-linear__save_issue')
  assert.equal(linearOperationMap.readComments.tool, 'mcp__bossanova-linear__list_comments')
  assert.equal(linearOperationMap.writeComment.tool, 'mcp__bossanova-linear__save_comment')
  assert.equal(linearOperationMap.readLabels.tool, 'mcp__bossanova-linear__get_issue')
  assert.equal(linearOperationMap.extractImages.tool, 'mcp__bossanova-linear__extract_images')
  assert.equal(linearOperationMap.createLabel.tool, 'mcp__bossanova-linear__create_issue_label')
  assert.equal(linearOperationMap.setPriorityEstimate.tool, 'mcp__bossanova-linear__save_issue')
  assert.equal(linearOperationMap.appendDependency.tool, 'mcp__bossanova-linear__save_issue')
})
