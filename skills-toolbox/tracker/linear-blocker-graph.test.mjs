import { test } from 'node:test'
import assert from 'node:assert/strict'
import { linearReadBlockerGraphNode } from './linear-blocker-graph.mjs'
import { createLinearAdapter } from './linear.mjs'
const ref = (id) => ({ id, identifier: id, state: { type: 'started' } })
const connection = (nodes = [], hasNextPage = false) => ({ nodes, pageInfo: { hasNextPage } })
test('Linear graph orients blocking relations and retains overflow on mixed pages', async () => {
  const queries = []
  const options = {
    apiKey: 'key',
    issueId: 'A',
    fetchImpl: async (_url, init) => {
      queries.push(JSON.parse(init.body))
      return {
        ok: true,
        json: async () => ({
          data: {
            issue: {
              ...ref('A'),
              parent: { identifier: 'P' },
              relations: connection(
                [
                  { type: 'blocks', relatedIssue: ref('B') },
                  { type: 'related', relatedIssue: ref('Z') },
                ],
                true,
              ),
              inverseRelations: connection([{ type: 'blocks', issue: ref('C') }]),
            },
          },
        }),
      }
    },
  }
  const result = await linearReadBlockerGraphNode(options)
  assert.deepEqual(result, {
    id: 'A',
    identifier: 'A',
    stateType: 'started',
    parentId: 'P',
    blockedBy: [{ id: 'C', identifier: 'C', stateType: 'started' }],
    blocks: [{ id: 'B', identifier: 'B', stateType: 'started' }],
    relationsComplete: false,
  })
  assert.deepEqual(queries[0].variables, { id: 'A' })
  const graph = await createLinearAdapter({
    apiKey: 'key',
    fetchImpl: options.fetchImpl,
  }).readBlockerGraph({ ids: ['A'], maxNodes: 1 })
  assert.equal(graph.complete, false)
})
test('Linear graph refuses missing credentials, relation pages and parent', async () => {
  await assert.rejects(linearReadBlockerGraphNode({ issueId: 'A' }), {
    code: 'TRACKER_CREDENTIALS_MISSING',
  })
  for (const partial of [{}, { relations: connection(), inverseRelations: connection() }]) {
    await assert.rejects(
      linearReadBlockerGraphNode({
        issueId: 'A',
        apiKey: 'key',
        fetchImpl: async () => ({
          ok: true,
          json: async () => ({ data: { issue: { ...ref('A'), ...partial } } }),
        }),
      }),
      /unreadable/,
    )
  }
})
