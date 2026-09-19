// dag-scheduler.test.mjs — pure DAG scheduler unit tests. node builtins only.
// Fixtures are abstract nodes {id, blockedBy, priority, createdAt} — no
// tracker/Linear-shaped payloads — proving the module schedules with zero
// tracker knowledge.
import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  buildGraph,
  transitiveDependents,
  transitiveDependentCounts,
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

// --- unlock-aware ready ordering (BOS-1272) --------------------------------------

test('transitiveDependentCounts: counts TRANSITIVE in-set dependents, not just direct ones', () => {
  // A -> B -> C -> D  (A unlocks three, B two, C one, D none)
  const g = buildGraph([
    n('A'),
    n('B', { blockedBy: ['A'] }),
    n('C', { blockedBy: ['B'] }),
    n('D', { blockedBy: ['C'] }),
  ])
  const counts = transitiveDependentCounts(g)
  assert.equal(counts.get('A'), 3)
  assert.equal(counts.get('B'), 2)
  assert.equal(counts.get('C'), 1)
  assert.equal(counts.get('D'), 0)
})

test('transitiveDependentCounts: a diamond counts each dependent once', () => {
  const g = buildGraph([
    n('A'),
    n('B', { blockedBy: ['A'] }),
    n('C', { blockedBy: ['A'] }),
    n('D', { blockedBy: ['B', 'C'] }),
  ])
  assert.equal(transitiveDependentCounts(g).get('A'), 3)
})

test('transitiveDependentCounts: external blockers are not nodes and count nothing', () => {
  const g = buildGraph([n('A', { blockedBy: ['X'] }), n('B', { blockedBy: ['A'] })])
  const counts = transitiveDependentCounts(g)
  assert.equal(counts.get('A'), 1)
  assert.equal(counts.has('X'), false)
})

test('transitiveDependentCounts: terminates on a cycle and never counts a node as its own dependent', () => {
  const g = buildGraph([n('A', { blockedBy: ['B'] }), n('B', { blockedBy: ['A'] })])
  const counts = transitiveDependentCounts(g)
  assert.equal(counts.get('A'), 1)
  assert.equal(counts.get('B'), 1)
})

test('readyTickets: higher TRANSITIVE unlock count wins over higher priority', () => {
  // U is urgent (priority 1) but unlocks nothing; R is low priority (4) yet
  // transitively unlocks two. Direct-dependent counting alone would tie R with
  // any single-dependent node, so the chain below is deliberately two deep.
  const g = buildGraph([
    n('U', { priority: 1 }),
    n('R', { priority: 4 }),
    n('S', { blockedBy: ['R'] }),
    n('T', { blockedBy: ['S'] }),
  ])
  assert.deepEqual(
    readyTickets(g, READY_EMPTY).map((x) => x.id),
    ['R', 'U'],
  )
})

test('readyTickets: equal unlock counts fall back to the existing priority order', () => {
  const g = buildGraph([n('A', { priority: 4 }), n('B', { priority: 1 }), n('C', { priority: 3 })])
  assert.deepEqual(
    readyTickets(g, READY_EMPTY).map((x) => x.id),
    ['B', 'C', 'A'],
  )
})

test('readyTickets: equal unlock and priority fall back to oldest createdAt', () => {
  const g = buildGraph([
    n('A', { createdAt: '2026-05-01T00:00:00Z' }),
    n('B', { createdAt: '2025-01-01T00:00:00Z' }),
  ])
  assert.deepEqual(
    readyTickets(g, READY_EMPTY).map((x) => x.id),
    ['B', 'A'],
  )
})

test('readyTickets: same unlock, priority AND createdAt tie-break on the stable identifier', () => {
  const g = buildGraph([n('PROJ-3'), n('PROJ-1'), n('PROJ-2')])
  assert.deepEqual(
    readyTickets(g, READY_EMPTY).map((x) => x.id),
    ['PROJ-1', 'PROJ-2', 'PROJ-3'],
  )
  // Deterministic regardless of the order the nodes were built in.
  const reversed = buildGraph([n('PROJ-1'), n('PROJ-2'), n('PROJ-3')])
  assert.deepEqual(
    readyTickets(reversed, READY_EMPTY).map((x) => x.id),
    ['PROJ-1', 'PROJ-2', 'PROJ-3'],
  )
})

test('readyTickets: a high unlock count never overrides an uncleared blocker', () => {
  // R unlocks two but is itself blocked by an uncleared external gate.
  const g = buildGraph([
    n('R', { blockedBy: ['X'] }),
    n('S', { blockedBy: ['R'] }),
    n('T', { blockedBy: ['S'] }),
    n('U', { priority: 4 }),
  ])
  assert.deepEqual(
    readyTickets(g, READY_EMPTY).map((x) => x.id),
    ['U'],
  )
})

test('readyTickets: unlock ranking does not resurrect merged/failed/in-flight/cascade-skipped nodes', () => {
  const g = buildGraph([
    n('A'),
    n('B', { blockedBy: ['A'] }),
    n('F'),
    n('G', { blockedBy: ['F'] }),
    n('M'),
    n('I'),
    n('Z', { priority: 4 }),
  ])
  assert.deepEqual(
    readyTickets(g, {
      merged: new Set(['M']),
      failed: new Set(['F']),
      inFlight: new Set(['I']),
      externallyCleared: new Set(),
    }).map((x) => x.id),
    ['A', 'Z'],
  )
})

test("readyTickets: cross-root edges rank a shared unlocker above both roots' leaves", () => {
  // SHARED is a member of two epics and unlocks one leaf in each.
  const g = buildGraph([
    n('SHARED', { priority: 4 }),
    n('A-LEAF', { blockedBy: ['SHARED'] }),
    n('B-LEAF', { blockedBy: ['SHARED'] }),
    n('A-SOLO', { priority: 1 }),
  ])
  assert.deepEqual(
    readyTickets(g, READY_EMPTY).map((x) => x.id),
    ['SHARED', 'A-SOLO'],
  )
})

test('nextToMerge: merge ordering is UNCHANGED by unlock ranking', () => {
  // C unlocks two; A is the higher-priority green. Merge order stays
  // priority-then-oldest, so A still wins.
  const g = buildGraph([
    n('A', { priority: 1 }),
    n('C', { priority: 4 }),
    n('D', { blockedBy: ['C'] }),
    n('E', { blockedBy: ['D'] }),
  ])
  assert.equal(nextToMerge([{ id: 'C' }, { id: 'A' }], g, new Set()), 'A')
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

// --- comparator NaN guard: the level-4 identifier tie-break must be reachable ---

test('readyTickets: an ABSENT createdAt still falls through to the identifier tie-break', () => {
  // `new Date(undefined).getTime()` is NaN, so the age difference is NaN. Without
  // the comparator's NaN guard, byPriorityThenOldest returns NaN, readyTickets'
  // `ordered !== 0` check is TRUE (NaN !== 0), and byId never runs — the sort
  // spec then coerces NaN to +0 and insertion order silently wins.
  const nodes = [
    { id: 'C', priority: 3, blockedBy: [] },
    { id: 'A', priority: 3, blockedBy: [] },
    { id: 'B', priority: 3, blockedBy: [] },
  ]
  for (const node of nodes) assert.equal(node.createdAt, undefined)
  const g = buildGraph(nodes)
  assert.deepEqual(
    readyTickets(g, READY_EMPTY).map((node) => node.id),
    ['A', 'B', 'C'],
  )
})

test('readyTickets: an UNPARSEABLE createdAt still falls through to the identifier tie-break', () => {
  const g = buildGraph([
    n('C', { createdAt: 'nope' }),
    n('A', { createdAt: 'nope' }),
    n('B', { createdAt: 'nope' }),
  ])
  assert.deepEqual(
    readyTickets(g, READY_EMPTY).map((node) => node.id),
    ['A', 'B', 'C'],
  )
})

test('readyTickets: identifier order does not depend on insertion order', () => {
  // Same three nodes, opposite insertion order: the result must be identical.
  const forward = buildGraph([n('A', { createdAt: 'nope' }), n('B', { createdAt: 'nope' })])
  const reverse = buildGraph([n('B', { createdAt: 'nope' }), n('A', { createdAt: 'nope' })])
  assert.deepEqual(
    readyTickets(forward, READY_EMPTY).map((node) => node.id),
    ['A', 'B'],
  )
  assert.deepEqual(
    readyTickets(reverse, READY_EMPTY).map((node) => node.id),
    ['A', 'B'],
  )
})

test('readyTickets: one usable createdAt against an unusable one is a tie, not a reversal', () => {
  // Mixed fixture: B has a real timestamp, A does not. The age comparison is
  // unusable, so the identifier decides rather than the parseable side winning
  // by accident of NaN coercion.
  const g = buildGraph([
    n('B', { createdAt: '2020-01-01T00:00:00Z' }),
    n('A', { createdAt: undefined }),
  ])
  assert.deepEqual(
    readyTickets(g, READY_EMPTY).map((node) => node.id),
    ['A', 'B'],
  )
})

test('readyTickets: a usable createdAt still outranks the identifier', () => {
  // The guard must not flatten real ages into identifier order.
  const g = buildGraph([
    n('A', { createdAt: '2026-06-01T00:00:00Z' }),
    n('B', { createdAt: '2020-01-01T00:00:00Z' }),
  ])
  assert.deepEqual(
    readyTickets(g, READY_EMPTY).map((node) => node.id),
    ['B', 'A'],
  )
})

test('nextToMerge: an unusable createdAt does not corrupt the merge pick', () => {
  // nextToMerge sorts by byPriorityThenOldest alone (no identifier level). The
  // guard keeps an unusable timestamp from being a non-zero comparator result,
  // so priority still decides.
  const g = buildGraph([
    n('A', { priority: 4, createdAt: undefined }),
    n('B', { priority: 1, createdAt: undefined }),
  ])
  assert.equal(nextToMerge([{ id: 'A' }, { id: 'B' }], g, new Set()), 'B')
  assert.equal(nextToMerge([{ id: 'B' }, { id: 'A' }], g, new Set()), 'B')
})
