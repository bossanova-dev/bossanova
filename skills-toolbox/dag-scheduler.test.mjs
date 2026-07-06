// dag-scheduler.test.mjs — pure DAG scheduler unit tests. node builtins only.
// Fixtures are abstract nodes {id, blockedBy, priority, createdAt} — no
// tracker/Linear-shaped payloads — proving the module schedules with zero
// tracker knowledge.
import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  buildGraph,
  transitiveDependents,
  readyTickets,
  nextToMerge,
  mergeBlockedExternalBlockers,
} from './dag-scheduler.mjs'

// Abstract node factory — NO tracker/Linear shape, just {id, blockedBy, priority, createdAt}.
const n = (id, over = {}) => ({
  id,
  priority: 3,
  createdAt: '2026-01-01T00:00:00Z',
  blockedBy: [],
  ...over,
})

const READY_EMPTY = {
  merged: new Set(),
  failed: new Set(),
  inFlight: new Set(),
  externallyCleared: new Set(),
}

test('buildGraph: partitions blockedBy into in-set vs external edges', () => {
  const g = buildGraph([n('A', { blockedBy: ['B', 'X'] }), n('B')])
  assert.deepEqual(g.inEpicBlockers.get('A'), ['B'])
  assert.deepEqual(g.externalBlockers.get('A'), ['X'])
  assert.ok(g.nodes.has('A') && g.nodes.has('B'))
})

test('buildGraph: a node with no blockedBy has empty in-set and external edges', () => {
  const g = buildGraph([n('A')])
  assert.deepEqual(g.inEpicBlockers.get('A'), [])
  assert.deepEqual(g.externalBlockers.get('A'), [])
})

test('transitiveDependents: failure cascades through the chain', () => {
  const g = buildGraph([n('A'), n('B', { blockedBy: ['A'] }), n('C', { blockedBy: ['B'] })])
  assert.deepEqual([...transitiveDependents(g, new Set(['A']))].sort(), ['B', 'C'])
})

test('transitiveDependents: terminates on a cyclic graph', () => {
  const g = buildGraph([n('A', { blockedBy: ['B'] }), n('B', { blockedBy: ['A'] })])
  assert.deepEqual([...transitiveDependents(g, new Set(['A']))].sort(), ['A', 'B'])
})

test('transitiveDependents: no failures yields an empty set', () => {
  const g = buildGraph([n('A'), n('B', { blockedBy: ['A'] })])
  assert.equal(transitiveDependents(g, new Set()).size, 0)
})

test('readyTickets: blocked node not ready until in-set blocker merged', () => {
  const g = buildGraph([n('A'), n('B', { blockedBy: ['A'] })])
  assert.deepEqual(
    readyTickets(g, READY_EMPTY).map((x) => x.id),
    ['A'],
  )
  assert.deepEqual(
    readyTickets(g, { ...READY_EMPTY, merged: new Set(['A']) }).map((x) => x.id),
    ['B'],
  )
})

test('readyTickets: in-flight excluded; external blocker gates until externallyCleared', () => {
  const g = buildGraph([n('B', { blockedBy: ['X'] })]) // X is external
  assert.equal(readyTickets(g, READY_EMPTY).length, 0)
  assert.equal(readyTickets(g, { ...READY_EMPTY, externallyCleared: new Set(['X']) }).length, 1)
})

test('readyTickets: an in-flight node is not re-listed', () => {
  const g = buildGraph([n('A'), n('B')])
  assert.deepEqual(
    readyTickets(g, { ...READY_EMPTY, inFlight: new Set(['A']) }).map((x) => x.id),
    ['B'],
  )
})

test('readyTickets: failed ancestor cascade-skips dependents (single authority)', () => {
  const g = buildGraph([n('A'), n('B', { blockedBy: ['A'] }), n('C', { blockedBy: ['B'] })])
  assert.deepEqual(
    readyTickets(g, { ...READY_EMPTY, failed: new Set(['A']) }).map((x) => x.id),
    [],
  )
})

test('readyTickets: priority order then oldest createdAt', () => {
  const g = buildGraph([
    n('A', { priority: 3 }),
    n('B', { priority: 1 }),
    n('C', { priority: 1, createdAt: '2025-01-01T00:00:00Z' }),
  ])
  assert.deepEqual(
    readyTickets(g, READY_EMPTY).map((x) => x.id),
    ['C', 'B', 'A'],
  )
})

test('nextToMerge: prefers a green whose in-set blockers are all merged', () => {
  const g = buildGraph([n('A'), n('B', { blockedBy: ['A'] })])
  assert.equal(nextToMerge([{ id: 'B' }, { id: 'A' }], g, new Set()), 'A')
  assert.equal(nextToMerge([{ id: 'B' }], g, new Set()), null)
})

test('nextToMerge: returns null on an empty green set', () => {
  const g = buildGraph([n('A')])
  assert.equal(nextToMerge([], g, new Set()), null)
})

test('mergeBlockedExternalBlockers: an uncleared external blocker gates the merge', () => {
  const g = buildGraph([n('B', { blockedBy: ['X'] })]) // X external
  assert.deepEqual(
    mergeBlockedExternalBlockers(g.nodes.get('B'), g, { clearedBlockers: new Set() }),
    ['X'],
  )
})

test('mergeBlockedExternalBlockers: a resolved external blocker no longer gates', () => {
  const g = buildGraph([n('B', { blockedBy: ['X'] })])
  assert.deepEqual(
    mergeBlockedExternalBlockers(g.nodes.get('B'), g, { clearedBlockers: new Set(['X']) }),
    [],
  )
})

test('mergeBlockedExternalBlockers: clearedForMerge overrides an open gate', () => {
  const g = buildGraph([n('B', { blockedBy: ['X'] })])
  assert.deepEqual(
    mergeBlockedExternalBlockers(g.nodes.get('B'), g, {
      clearedForMerge: new Set(['X']),
      clearedBlockers: new Set(),
    }),
    [],
  )
})

test('mergeBlockedExternalBlockers: in-set blockers are never external gates', () => {
  const g = buildGraph([n('A'), n('B', { blockedBy: ['A'] })])
  assert.deepEqual(
    mergeBlockedExternalBlockers(g.nodes.get('B'), g, { clearedBlockers: new Set() }),
    [],
  )
})

test('mergeBlockedExternalBlockers: defaults to fail-closed with no options', () => {
  const g = buildGraph([n('B', { blockedBy: ['X'] })])
  assert.deepEqual(mergeBlockedExternalBlockers(g.nodes.get('B'), g), ['X'])
})
