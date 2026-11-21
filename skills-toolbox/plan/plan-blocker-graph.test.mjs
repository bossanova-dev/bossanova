import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  screenBlockingWrites,
  validateBlockerGraph,
  BLOCKER_SCREEN_REASONS,
  main,
} from './plan-blocker-graph.mjs'
import { writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
const ref = (id) => ({ id, identifier: id, stateType: 'started' })
const node = (id, blocks = [], extra = {}) => ({
  ...ref(id),
  blocks: blocks.map(ref),
  blockedBy: [],
  parentId: null,
  relationsComplete: true,
  ...extra,
})
const graph = (nodes) => ({
  seeds: ['A'],
  maxNodes: 100,
  complete: !nodes.some((n) => !n.relationsComplete),
  nodes,
})
const write = (id, blocker) => ({ id, blockedBy: [blocker] })
test('screen rejects direct, transitive, epic-parent, pending and self cycles deterministically', () => {
  for (const [nodes, writes] of [
    [[node('A', ['B']), node('B')], [write('A', 'B')]],
    [[node('A', ['B']), node('B', ['C']), node('C')], [write('A', 'C')]],
    [[node('A', [], { parentId: 'P' }), node('P', ['B']), node('B')], [write('A', 'B')]],
    [
      [node('A'), node('B')],
      [write('A', 'B'), write('B', 'A')],
    ],
    [[node('A')], [write('A', 'A')]],
  ]) {
    const result = screenBlockingWrites({ graph: graph(nodes), writes, subjectId: 'A' })
    assert.equal(result.cycleSkips.length, 1)
    assert.equal(result.cycleSkips[0].reason, 'would-close-cycle')
    assert.ok(result.cycleSkips[0].via.length >= 1)
    assert.deepEqual(
      screenBlockingWrites({ graph: graph(nodes), writes: [...writes].reverse(), subjectId: 'A' }),
      result,
    )
  }
})
test('settled nodes break paths, incomplete and missing nodes hold, found cycles win over gaps', () => {
  for (const stateType of ['completed', 'canceled'])
    assert.equal(
      screenBlockingWrites({
        graph: graph([node('A', ['B']), node('B', ['C'], { stateType }), node('C')]),
        writes: [write('A', 'C')],
        subjectId: 'A',
      }).accepted.length,
      1,
    )
  for (const [nodes, reason] of [
    [[node('A', [], { relationsComplete: false }), node('B')], 'blocker-graph-incomplete'],
    [[node('A', ['M']), node('B')], 'blocker-graph-missing-node'],
  ])
    assert.equal(
      screenBlockingWrites({ graph: graph(nodes), writes: [write('A', 'B')], subjectId: 'A' })
        .held[0].reason,
      reason,
    )
  assert.equal(
    screenBlockingWrites({
      graph: graph([node('A', ['B', 'missing'], { relationsComplete: false }), node('B')]),
      writes: [write('A', 'B')],
      subjectId: 'A',
    }).cycleSkips.length,
    1,
  )
})
test('screen resolves UUID aliases and warns only on accepted writes with escalation', () => {
  const nodes = [node('uuid-a', ['B'], { identifier: 'A' }), node('B'), node('C')]
  assert.equal(
    screenBlockingWrites({ graph: graph(nodes), writes: [write('A', 'B')], subjectId: 'A' })
      .cycleSkips.length,
    1,
  )
  const result = screenBlockingWrites({
    graph: graph([
      node('A'),
      node('B', [], { blockedBy: [ref('UP')] }),
      node('C', ['DOWN']),
      node('DOWN'),
    ]),
    writes: [write('A', 'B'), write('C', 'A')],
    subjectId: 'A',
    subjectAgentBuild: false,
  })
  assert.deepEqual(result.transitiveWarnings.map((w) => w.reason).sort(), [
    'transitive-block-downstream',
    'transitive-block-upstream',
  ])
  assert.ok(result.transitiveWarnings.some((w) => w.severity === 'escalated'))
  assert.ok(
    result.transitiveWarnings.every((w) =>
      Object.values(BLOCKER_SCREEN_REASONS).includes(w.reason),
    ),
  )
})
test('screen CLI accepts sibling and append shapes and refuses invalid graph', async (t) => {
  const dir = tempDir('blocker-screen-', t),
    g = join(dir, 'graph.json'),
    w = join(dir, 'writes.json')
  writeFileSync(g, JSON.stringify(graph([node('A'), node('B')])))
  writeFileSync(w, JSON.stringify([{ issue: 'A', blockedBy: 'B' }]))
  let stdout = '',
    stderr = ''
  assert.equal(
    main(['screen', '--graph', g, '--writes', w, '--subject', 'A'], {
      stdout: (s) => (stdout += s),
      stderr: (s) => (stderr += s),
    }),
    0,
  )
  assert.deepEqual(JSON.parse(stdout).accepted, [write('A', 'B')])
  assert.equal(stderr, '')
  assert.ok(validateBlockerGraph({}))
  writeFileSync(g, '{}')
  assert.equal(
    main(['screen', '--graph', g, '--writes', w, '--subject', 'A'], {
      stdout: () => {},
      stderr: () => {},
    }),
    1,
  )
})

test('cyclic and held writes do not emit transitive warnings', () => {
  for (const subject of [node('A', ['B']), node('A', [], { relationsComplete: false })]) {
    const result = screenBlockingWrites({
      graph: graph([subject, node('B', [], { blockedBy: [ref('UP')] })]),
      writes: [write('A', 'B')],
      subjectId: 'A',
    })
    assert.equal(result.accepted.length, 0)
    assert.deepEqual(result.transitiveWarnings, [])
  }
})
