import { test } from 'node:test'
import assert from 'node:assert/strict'
import { walkBlockerGraph } from './blocker-graph.mjs'
const node = (id, extra = {}) => ({
  id,
  identifier: id,
  stateType: 'started',
  parentId: null,
  blockedBy: [],
  blocks: [],
  relationsComplete: true,
  ...extra,
})
test('bounded graph follows downstream and parents, deduplicating aliases and settled nodes', async () => {
  const rows = {
    A: node('uuid-a', {
      identifier: 'A',
      blocks: [{ id: 'b', identifier: 'B', stateType: 'started' }],
      parentId: 'P',
    }),
    B: node('b', {
      identifier: 'B',
      blocks: [{ id: 'uuid-a', identifier: 'A', stateType: 'started' }],
    }),
    P: node('P', { stateType: 'completed', parentId: 'unused' }),
  }
  const calls = []
  const graph = await walkBlockerGraph({
    seeds: ['A', 'uuid-a'],
    maxNodes: 10,
    readNode: async (id) => {
      calls.push(id)
      return rows[id]
    },
  })
  assert.equal(graph.complete, true)
  assert.equal(graph.nodes.length, 3)
  assert.deepEqual(calls, ['A', 'B', 'P'])
})
test('cap and incomplete relations retain incomplete evidence; unreadable nodes throw', async () => {
  const readNode = async (id) =>
    node(id, { blocks: [{ id: 'B', identifier: 'B', stateType: 'started' }] })
  assert.equal((await walkBlockerGraph({ seeds: ['A'], readNode, maxNodes: 1 })).complete, false)
  assert.equal(
    (
      await walkBlockerGraph({
        seeds: ['A'],
        readNode: async (id) => node(id, { relationsComplete: false }),
      })
    ).complete,
    false,
  )
  await assert.rejects(walkBlockerGraph({ seeds: ['A'], readNode: async () => null }), /node/)
})
