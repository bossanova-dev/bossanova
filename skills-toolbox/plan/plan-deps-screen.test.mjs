import { test } from 'node:test'
import assert from 'node:assert/strict'
import { runDependencyScan } from './plan-run-guards.mjs'
import { DEFAULT_CONFIG, withTrackerDefaults } from '../skill-config.mjs'
const row = (id, extra = {}) => ({
  id,
  identifier: id,
  title: id,
  stateName: 'Todo',
  stateType: 'unstarted',
  parentId: null,
  labels: [],
  priority: 3,
  source: 'id',
  description: '## Planning\n\n- Contract: v1\n\n## Key changes\n\n- `app/x.mjs`: change\n',
  ...extra,
})
const rows = [row('A'), row('B')]
const node = (id, blocks = [], extra = {}) => ({
  id,
  identifier: id,
  stateType: 'unstarted',
  parentId: null,
  blockedBy: [],
  blocks: blocks.map((id) => ({ id, identifier: id, stateType: 'unstarted' })),
  relationsComplete: true,
  ...extra,
})
const graph = (nodes) => ({
  seeds: ['A', 'B'],
  maxNodes: 100,
  complete: !nodes.some((n) => !n.relationsComplete),
  nodes,
})
const input = {
  payload: { logicalDependencies: { B: { direction: 'blockedBy', note: 'requires B' } } },
  subjectId: 'A',
  rows,
  config: withTrackerDefaults({
    ...DEFAULT_CONFIG,
    trackerConfig: {
      linear: {
        mcpServer: 'linear',
        team: 'Example',
        states: { planned: 'Todo', unplanned: 'Backlog' },
      },
    },
  }),
  rootEntries: { dirs: ['app'], files: [], knownFiles: ['app/x.mjs'] },
}
test('deps requires a fetched graph only when a blocking write survives classification', () => {
  const result = runDependencyScan(input)
  assert.equal(result.status, 'refused', JSON.stringify(result))
  assert.equal(result.refusals[0].code, 'blocker-graph-required')
  assert.match(result.refusals[0].message, /A.*B/)
  assert.equal(runDependencyScan({ ...input, payload: {} }).status, 'ok')
  const invalid = runDependencyScan({ ...input, graph: {} })
  assert.equal(invalid.refusals[0].code, 'blocker-graph-not-fetch-blocker-graph')
})
test('deps zeroes cyclic and held writes, retains notes and recomputes verdict', () => {
  const cyclic = runDependencyScan({ ...input, graph: graph([node('A', ['B']), node('B')]) })
  assert.equal(cyclic.status, 'ok')
  assert.equal(cyclic.result.edges[0].write, null)
  assert.equal(cyclic.result.edges[0].screen, 'would-close-cycle')
  assert.equal(cyclic.result.screen.cycleSkips, 1)
  assert.ok(cyclic.result.notes.some((n) => n.destination === 'risks' && n.text.includes('A → B')))
  const held = runDependencyScan({
    ...input,
    graph: graph([node('A', [], { relationsComplete: false }), node('B')]),
  })
  assert.equal(held.result.edges[0].write, null)
  assert.equal(held.result.questions.length, 1)
  assert.equal(held.result.screen.held, 1)
})
test('deps records upstream warnings only for accepted writes', () => {
  const g = graph([
    node('A'),
    node('B', [], { blockedBy: [{ id: 'UP', identifier: 'UP', stateType: 'started' }] }),
  ])
  const result = runDependencyScan({ ...input, graph: g })
  assert.equal(result.result.screen.accepted, 1)
  assert.ok(result.result.notes.some((n) => n.text.startsWith('Transitive-block warning:')))
})
