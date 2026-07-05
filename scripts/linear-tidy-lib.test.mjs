// scripts/linear-tidy-lib.test.mjs
// Full-branch unit tests for the bs-sweep-tidy-linear tidy logic. No network:
// every I/O boundary is a fixture-backed injected function.

import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  STATE_RANK,
  TERMINAL_STATE_NAMES,
  rankOf,
  isTerminalStateName,
  parseLinearTag,
  groupPrsByTicket,
  computeCloseable,
  computeRollups,
  parseTidyData,
  runTidy,
} from './linear-tidy-lib.mjs'

const st = (name, type = 'unstarted', id = `state-${name}`) => ({ id, name, type })
const pr = (number, title, state) => ({ number, title, state, url: `https://gh/pr/${number}` })

// --- STATE_RANK -------------------------------------------------------------

test('STATE_RANK enumerates exactly the expected states in a strict total order', () => {
  assert.deepEqual(Object.keys(STATE_RANK), [
    'Backlog',
    'Unplanned',
    'Todo',
    'In Progress',
    'In Review',
    'Done',
  ])
  const values = Object.values(STATE_RANK)
  // Strictly increasing → total order, no ties.
  for (let i = 1; i < values.length; i++) assert.ok(values[i] > values[i - 1])
})

test('rankOf / isTerminalStateName', () => {
  assert.equal(rankOf('Backlog'), 0)
  assert.equal(rankOf('Done'), 5)
  assert.equal(rankOf('Nonsense'), null)
  assert.equal(rankOf(undefined), null)
  for (const t of ['Done', 'Canceled', 'Duplicate']) assert.ok(isTerminalStateName(t))
  assert.ok(TERMINAL_STATE_NAMES.has('Duplicate'))
  assert.equal(isTerminalStateName('Todo'), false)
})

// --- parseLinearTag ---------------------------------------------------------

test('parseLinearTag: bracketed BOS ids, case-insensitive, multi, dedup, non-matches', () => {
  assert.deepEqual(parseLinearTag('[BOS-12] fix thing'), ['BOS-12'])
  assert.deepEqual(parseLinearTag('[bos-12] lowercase'), ['BOS-12'])
  assert.deepEqual(parseLinearTag('do [BOS-1] and [BOS-2]'), ['BOS-1', 'BOS-2'])
  assert.deepEqual(parseLinearTag('[BOS-7] and again [bos-7]'), ['BOS-7']) // dedup
  assert.deepEqual(parseLinearTag('no tag here'), [])
  assert.deepEqual(parseLinearTag('[#123] PR-number tag must not match'), [])
  assert.deepEqual(parseLinearTag('bare BOS-9 without brackets'), [])
  assert.deepEqual(parseLinearTag(null), [])
  assert.deepEqual(parseLinearTag(undefined), [])
})

// --- groupPrsByTicket -------------------------------------------------------

test('groupPrsByTicket: multi-tag PR indexed under each; untagged dropped', () => {
  const prs = [
    pr(1, '[BOS-1] a', 'MERGED'),
    pr(2, '[BOS-1] and [BOS-2] b', 'OPEN'),
    pr(3, 'untagged c', 'MERGED'),
  ]
  const g = groupPrsByTicket(prs)
  assert.deepEqual(
    g.get('BOS-1').map((p) => p.number),
    [1, 2],
  )
  assert.deepEqual(
    g.get('BOS-2').map((p) => p.number),
    [2],
  )
  assert.equal(g.size, 2) // PR 3 dropped
  assert.deepEqual(groupPrsByTicket(null).size, 0)
})

// --- computeCloseable -------------------------------------------------------

test('computeCloseable: all-merged closes; other PR mixes skip or escalate', () => {
  const tickets = [
    { id: 'i-allmerged', identifier: 'BOS-1', state: st('In Review', 'started') },
    { id: 'i-mixopen', identifier: 'BOS-2', state: st('In Progress', 'started') },
    { id: 'i-mixclosed', identifier: 'BOS-3', state: st('In Progress', 'started') },
    { id: 'i-nopr', identifier: 'BOS-4', state: st('Todo') },
    { id: 'i-nomerge', identifier: 'BOS-5', state: st('Todo') },
  ]
  const prsByTicket = groupPrsByTicket([
    pr(10, '[BOS-1] one', 'MERGED'),
    pr(11, '[BOS-1] two', 'MERGED'),
    pr(20, '[BOS-2] merged', 'MERGED'),
    pr(21, '[BOS-2] still open', 'OPEN'),
    pr(30, '[BOS-3] merged', 'MERGED'),
    pr(31, '[BOS-3] closed-unmerged', 'CLOSED'),
    // BOS-4: no PR
    pr(50, '[BOS-5] open only', 'OPEN'),
  ])
  const { close, escalate } = computeCloseable(tickets, prsByTicket)
  assert.deepEqual(
    close.map((c) => c.identifier),
    ['BOS-1'],
  )
  assert.equal(close[0].id, 'i-allmerged')
  assert.deepEqual(close[0].prs, [10, 11])
  assert.deepEqual(
    escalate.map((e) => `${e.kind}:${e.identifier}`),
    ['close-conflict:BOS-3'],
  )
})

test('computeCloseable: already-terminal ticket is skipped defensively', () => {
  const tickets = [{ id: 'i', identifier: 'BOS-9', state: st('Done', 'completed') }]
  const prsByTicket = groupPrsByTicket([pr(90, '[BOS-9] merged', 'MERGED')])
  const { close, escalate } = computeCloseable(tickets, prsByTicket)
  assert.deepEqual(close, [])
  assert.deepEqual(escalate, [])
})

test('computeCloseable: opt-in knownIdentifiers escalates merged PR for unknown ticket', () => {
  const tickets = [{ id: 'i', identifier: 'BOS-1', state: st('Todo') }]
  const prsByTicket = groupPrsByTicket([
    pr(1, '[BOS-1] known open', 'OPEN'),
    pr(2, '[BOS-999] merged unknown', 'MERGED'),
    pr(3, '[BOS-888] open unknown (no escalate)', 'OPEN'),
  ])
  // Without the opt-in set, orphan tags are silently skipped (fail-safe default).
  assert.deepEqual(computeCloseable(tickets, prsByTicket).escalate, [])
  // With it, only the MERGED orphan escalates.
  const { escalate } = computeCloseable(tickets, prsByTicket, {
    knownIdentifiers: new Set(['BOS-1']),
  })
  assert.deepEqual(
    escalate.map((e) => `${e.kind}:${e.identifier}`),
    ['unknown-ticket:BOS-999'],
  )
})

test('computeCloseable: a blocked (open-child parent) identifier is never closed on an all-merged match', () => {
  const tickets = [{ id: 'i', identifier: 'BOS-50', state: st('Backlog', 'backlog') }]
  const prsByTicket = groupPrsByTicket([pr(50, '[BOS-50] parent own work', 'MERGED')])
  // Without the block it would close (all matched PRs merged)…
  assert.deepEqual(
    computeCloseable(tickets, prsByTicket).close.map((c) => c.identifier),
    ['BOS-50'],
  )
  // …with it, the parent is skipped so rollup alone governs its state.
  assert.deepEqual(
    computeCloseable(tickets, prsByTicket, { blockedIdentifiers: new Set(['BOS-50']) }).close,
    [],
  )
})

// --- computeRollups ---------------------------------------------------------

test('computeRollups: parent moves forward to least-advanced open child', () => {
  const parents = [
    {
      id: 'p1',
      identifier: 'BOS-100',
      state: st('Backlog', 'backlog'),
      children: [
        { id: 'c1', identifier: 'BOS-101', state: st('Todo') },
        { id: 'c2', identifier: 'BOS-102', state: st('Todo') },
      ],
    },
  ]
  const { move, escalate } = computeRollups(parents)
  assert.equal(escalate.length, 0)
  assert.deepEqual(move, [
    {
      id: 'p1',
      identifier: 'BOS-100',
      from: 'Backlog',
      to: 'Todo',
      toStateId: 'state-Todo',
    },
  ])
})

test('computeRollups: mixed children target the least-advanced OPEN child', () => {
  const parents = [
    {
      id: 'p',
      identifier: 'BOS-200',
      state: st('Backlog', 'backlog'),
      children: [
        { id: 'a', identifier: 'BOS-201', state: st('Done', 'completed') }, // ignored
        { id: 'b', identifier: 'BOS-202', state: st('Todo') }, // least-advanced open
        { id: 'c', identifier: 'BOS-203', state: st('In Progress', 'started') },
      ],
    },
  ]
  const { move } = computeRollups(parents)
  assert.equal(move.length, 1)
  assert.equal(move[0].to, 'Todo')
})

test('computeRollups: no move when parent already at/above least-advanced open child', () => {
  const parents = [
    {
      id: 'p',
      identifier: 'BOS-300',
      state: st('Todo'),
      children: [{ id: 'a', identifier: 'BOS-301', state: st('Todo') }],
    },
  ]
  assert.deepEqual(computeRollups(parents).move, [])
})

test('computeRollups: never regress a parent ahead of its children', () => {
  const parents = [
    {
      id: 'p',
      identifier: 'BOS-400',
      state: st('In Review', 'started'),
      children: [{ id: 'a', identifier: 'BOS-401', state: st('Todo') }],
    },
  ]
  assert.deepEqual(computeRollups(parents).move, [])
})

test('computeRollups: zero open children (none or all-terminal) is skipped', () => {
  const parents = [
    { id: 'p1', identifier: 'BOS-500', state: st('Backlog', 'backlog'), children: [] },
    {
      id: 'p2',
      identifier: 'BOS-501',
      state: st('Backlog', 'backlog'),
      children: [
        { id: 'a', identifier: 'BOS-502', state: st('Done', 'completed') },
        { id: 'b', identifier: 'BOS-503', state: st('Canceled', 'canceled') },
      ],
    },
  ]
  const { move, escalate } = computeRollups(parents)
  assert.deepEqual(move, [])
  assert.deepEqual(escalate, [])
})

test('computeRollups: unrankable open child (or parent) escalates', () => {
  const childCase = computeRollups([
    {
      id: 'p',
      identifier: 'BOS-600',
      state: st('Backlog', 'backlog'),
      children: [{ id: 'a', identifier: 'BOS-601', state: st('Triage', 'started') }],
    },
  ])
  assert.deepEqual(
    childCase.escalate.map((e) => e.kind),
    ['rollup-unrankable-child'],
  )
  assert.deepEqual(childCase.move, [])

  const parentCase = computeRollups([
    {
      id: 'p',
      identifier: 'BOS-700',
      state: st('Mystery', 'started'),
      children: [{ id: 'a', identifier: 'BOS-701', state: st('Todo') }],
    },
  ])
  assert.deepEqual(
    parentCase.escalate.map((e) => e.kind),
    ['rollup-unrankable-parent'],
  )
})

test('computeRollups: truncated child page escalates instead of moving on a partial view', () => {
  const parents = [
    {
      id: 'p',
      identifier: 'BOS-800',
      state: st('Backlog', 'backlog'),
      childrenTruncated: true,
      // Visible subset is all In Review, but an unread page could hold a Todo child;
      // moving to In Review would roll the parent PAST its true least-advanced child.
      children: [{ id: 'a', identifier: 'BOS-801', state: st('In Review', 'started') }],
    },
  ]
  const { move, escalate } = computeRollups(parents)
  assert.deepEqual(move, [])
  assert.deepEqual(
    escalate.map((e) => `${e.kind}:${e.identifier}`),
    ['rollup-children-truncated:BOS-800'],
  )
})

// --- parseTidyData ----------------------------------------------------------

test('parseTidyData: splits tickets/parents, maps states, finds Done id', () => {
  const data = {
    workflowStates: {
      nodes: [
        { id: 's-todo', name: 'Todo', type: 'unstarted' },
        { id: 's-done', name: 'Done', type: 'completed' },
      ],
    },
    issues: {
      nodes: [
        {
          id: 'i1',
          identifier: 'BOS-1',
          state: st('Backlog', 'backlog'),
          children: { nodes: [{ id: 'i2', identifier: 'BOS-2', state: st('Todo') }] },
        },
        { id: 'i2', identifier: 'BOS-2', state: st('Todo'), children: { nodes: [] } },
      ],
      pageInfo: { hasNextPage: false },
    },
  }
  const parsed = parseTidyData(data)
  assert.equal(parsed.doneStateId, 's-done')
  assert.deepEqual(
    parsed.tickets.map((t) => t.identifier),
    ['BOS-1', 'BOS-2'],
  )
  assert.deepEqual(
    parsed.parents.map((p) => p.identifier),
    ['BOS-1'], // only the issue carrying children
  )
  assert.equal(parsed.parents[0].children[0].identifier, 'BOS-2')
  assert.equal(parsed.parents[0].childrenTruncated, false)
})

test('parseTidyData: childrenTruncated reflects the children pageInfo.hasNextPage', () => {
  const data = {
    workflowStates: { nodes: [{ id: 's-todo', name: 'Todo', type: 'unstarted' }] },
    issues: {
      nodes: [
        {
          id: 'i1',
          identifier: 'BOS-1',
          state: st('Backlog', 'backlog'),
          children: {
            nodes: [{ id: 'i2', identifier: 'BOS-2', state: st('Todo') }],
            pageInfo: { hasNextPage: true },
          },
        },
      ],
      pageInfo: { hasNextPage: false },
    },
  }
  assert.equal(parseTidyData(data).parents[0].childrenTruncated, true)
})

// --- runTidy: budget, apply/dry-run, idempotency ----------------------------

function fixtureData() {
  return {
    workflowStates: {
      nodes: [
        { id: 's-backlog', name: 'Backlog', type: 'backlog' },
        { id: 's-todo', name: 'Todo', type: 'unstarted' },
        { id: 's-done', name: 'Done', type: 'completed' },
      ],
    },
    issues: {
      nodes: [
        // Closeable: In Review, one merged PR.
        {
          id: 'i-close',
          identifier: 'BOS-1',
          state: st('In Review', 'started'),
          children: { nodes: [] },
        },
        // Rollup parent: Backlog with a Todo open child. The child's Todo state id
        // matches workflowStates' Todo id (as it does in real Linear).
        {
          id: 'i-parent',
          identifier: 'BOS-2',
          state: st('Backlog', 'backlog'),
          children: {
            nodes: [
              { id: 'i-child', identifier: 'BOS-3', state: st('Todo', 'unstarted', 's-todo') },
            ],
          },
        },
        {
          id: 'i-child',
          identifier: 'BOS-3',
          state: st('Todo', 'unstarted', 's-todo'),
          children: { nodes: [] },
        },
      ],
      pageInfo: { hasNextPage: false },
    },
  }
}
const fixturePrs = () => [pr(10, '[BOS-1] done work', 'MERGED')]

test('runTidy: ≤1 gh read + 1 Linear read before applying (API budget)', async () => {
  let prReads = 0
  let linearReads = 0
  const writes = []
  const result = await runTidy({
    apiKey: 'k',
    dryRun: false,
    readPullRequests: async () => {
      prReads += 1
      return fixturePrs()
    },
    linearReadImpl: async () => {
      linearReads += 1
      return fixtureData()
    },
    linearWriteImpl: async ({ variables }) => {
      writes.push(variables)
      return { issueUpdate: { success: true } }
    },
  })
  assert.equal(prReads, 1)
  assert.equal(linearReads, 1)
  assert.equal(result.counts.prReads, 1)
  assert.equal(result.counts.linearReads, 1)
  // Applied both deltas: close BOS-1 → Done, roll up BOS-2 → Todo.
  assert.deepEqual(
    result.closed.map((c) => c.identifier),
    ['BOS-1'],
  )
  assert.deepEqual(
    result.rolledUp.map((m) => `${m.identifier}->${m.to}`),
    ['BOS-2->Todo'],
  )
  assert.deepEqual(writes, [
    { id: 'i-close', stateId: 's-done' },
    { id: 'i-parent', stateId: 's-todo' },
  ])
  assert.deepEqual(result.escalations, [])
})

test('runTidy: --dry-run computes the same moves but writes nothing', async () => {
  let writeCalls = 0
  const result = await runTidy({
    apiKey: 'k',
    dryRun: true,
    readPullRequests: async () => fixturePrs(),
    linearReadImpl: async () => fixtureData(),
    linearWriteImpl: async () => {
      writeCalls += 1
      return { issueUpdate: { success: true } }
    },
  })
  assert.equal(writeCalls, 0)
  assert.equal(result.counts.writes, 0)
  assert.deepEqual(
    result.closed.map((c) => c.identifier),
    ['BOS-1'],
  )
  assert.deepEqual(
    result.rolledUp.map((m) => m.identifier),
    ['BOS-2'],
  )
})

test('runTidy: idempotent — post-move state yields empty deltas', async () => {
  // Feed back the world AFTER the first run applied: BOS-1 is Done (so it is no
  // longer a non-terminal ticket and is not returned), and the parent BOS-2 now
  // sits at Todo alongside its child.
  const settled = {
    workflowStates: fixtureData().workflowStates,
    issues: {
      nodes: [
        {
          id: 'i-parent',
          identifier: 'BOS-2',
          state: st('Todo'),
          children: { nodes: [{ id: 'i-child', identifier: 'BOS-3', state: st('Todo') }] },
        },
        { id: 'i-child', identifier: 'BOS-3', state: st('Todo'), children: { nodes: [] } },
      ],
      pageInfo: { hasNextPage: false },
    },
  }
  let writeCalls = 0
  const result = await runTidy({
    apiKey: 'k',
    dryRun: false,
    readPullRequests: async () => [], // BOS-1 already Done → its merged PR no longer matches an open ticket
    linearReadImpl: async () => settled,
    linearWriteImpl: async () => {
      writeCalls += 1
      return { issueUpdate: { success: true } }
    },
  })
  assert.deepEqual(result.closed, [])
  assert.deepEqual(result.rolledUp, [])
  assert.deepEqual(result.escalations, [])
  assert.equal(writeCalls, 0)
})

test('runTidy: escalation surfaces without blocking mechanical closes', async () => {
  const data = fixtureData()
  // Add a conflicting-PR ticket that must escalate.
  data.issues.nodes.push({
    id: 'i-conflict',
    identifier: 'BOS-9',
    state: st('In Progress', 'started'),
    children: { nodes: [] },
  })
  const prs = [
    ...fixturePrs(),
    pr(90, '[BOS-9] merged', 'MERGED'),
    pr(91, '[BOS-9] closed-unmerged', 'CLOSED'),
  ]
  const result = await runTidy({
    apiKey: 'k',
    dryRun: false,
    readPullRequests: async () => prs,
    linearReadImpl: async () => data,
    linearWriteImpl: async () => ({ issueUpdate: { success: true } }),
  })
  // Mechanical close still applied.
  assert.deepEqual(
    result.closed.map((c) => c.identifier),
    ['BOS-1'],
  )
  assert.deepEqual(
    result.escalations.map((e) => `${e.kind}:${e.identifier}`),
    ['close-conflict:BOS-9'],
  )
})

test('runTidy: an epic parent with a merged own-PR and open children is rolled up, never closed (no double-write)', async () => {
  const data = {
    workflowStates: fixtureData().workflowStates,
    issues: {
      nodes: [
        {
          id: 'i-parent',
          identifier: 'BOS-50',
          state: st('Backlog', 'backlog'),
          children: {
            nodes: [
              { id: 'i-child', identifier: 'BOS-51', state: st('Todo', 'unstarted', 's-todo') },
            ],
          },
        },
        {
          id: 'i-child',
          identifier: 'BOS-51',
          state: st('Todo', 'unstarted', 's-todo'),
          children: { nodes: [] },
        },
      ],
      pageInfo: { hasNextPage: false },
    },
  }
  const writes = []
  const result = await runTidy({
    apiKey: 'k',
    dryRun: false,
    readPullRequests: async () => [pr(50, '[BOS-50] parent own work', 'MERGED')],
    linearReadImpl: async () => data,
    linearWriteImpl: async ({ variables }) => {
      writes.push(variables)
      return { issueUpdate: { success: true } }
    },
  })
  // Not closed to Done despite the all-merged own PR (children still open)…
  assert.deepEqual(result.closed, [])
  // …only rolled FORWARD to its least-advanced open child, with exactly one write.
  assert.deepEqual(
    result.rolledUp.map((m) => `${m.identifier}->${m.to}`),
    ['BOS-50->Todo'],
  )
  assert.deepEqual(writes, [{ id: 'i-parent', stateId: 's-todo' }])
})

test('runTidy: a parent whose children are all terminal IS closeable on an all-merged own PR', async () => {
  const data = {
    workflowStates: fixtureData().workflowStates,
    issues: {
      nodes: [
        {
          id: 'i-parent',
          identifier: 'BOS-60',
          state: st('In Review', 'started'),
          children: {
            nodes: [{ id: 'i-child', identifier: 'BOS-61', state: st('Done', 'completed') }],
          },
        },
      ],
      pageInfo: { hasNextPage: false },
    },
  }
  const result = await runTidy({
    apiKey: 'k',
    dryRun: true,
    readPullRequests: async () => [pr(60, '[BOS-60] epic wrap-up', 'MERGED')],
    linearReadImpl: async () => data,
    linearWriteImpl: async () => ({ issueUpdate: { success: true } }),
  })
  // All children terminal → not blocked → the parent closes on its merged PR.
  assert.deepEqual(
    result.closed.map((c) => c.identifier),
    ['BOS-60'],
  )
})

test('runTidy: a truncated issue page escalates board-truncated (never a silent under-tidy)', async () => {
  const data = fixtureData()
  data.issues.pageInfo = { hasNextPage: true }
  const result = await runTidy({
    apiKey: 'k',
    dryRun: true,
    readPullRequests: async () => fixturePrs(),
    linearReadImpl: async () => data,
    linearWriteImpl: async () => ({ issueUpdate: { success: true } }),
  })
  // Mechanical deltas on the visible page still computed…
  assert.deepEqual(
    result.closed.map((c) => c.identifier),
    ['BOS-1'],
  )
  // …and the unread tail is surfaced as a residual so the agent wakes.
  assert.ok(result.escalations.some((e) => e.kind === 'board-truncated'))
})

test('runTidy: a write returning success:false fails loud (no silent partial close)', async () => {
  await assert.rejects(
    runTidy({
      apiKey: 'k',
      dryRun: false,
      readPullRequests: async () => fixturePrs(),
      linearReadImpl: async () => fixtureData(),
      linearWriteImpl: async () => ({ issueUpdate: { success: false } }),
    }),
    /failed to close BOS-1/,
  )
})

test('runTidy: a Linear read error fails closed with zero writes (reads precede writes)', async () => {
  let writes = 0
  await assert.rejects(
    runTidy({
      apiKey: 'k',
      dryRun: false,
      readPullRequests: async () => fixturePrs(),
      linearReadImpl: async () => {
        throw new Error('Linear GraphQL error: boom')
      },
      linearWriteImpl: async () => {
        writes += 1
        return { issueUpdate: { success: true } }
      },
    }),
    /Linear GraphQL error: boom/,
  )
  assert.equal(writes, 0)
})

test('runTidy: a gh read failure fails closed before any Linear read or write', async () => {
  let linearReads = 0
  let writes = 0
  await assert.rejects(
    runTidy({
      apiKey: 'k',
      dryRun: false,
      readPullRequests: async () => {
        throw new Error('gh: not authenticated')
      },
      linearReadImpl: async () => {
        linearReads += 1
        return fixtureData()
      },
      linearWriteImpl: async () => {
        writes += 1
        return { issueUpdate: { success: true } }
      },
    }),
    /gh: not authenticated/,
  )
  assert.equal(linearReads, 0) // gh read is first → a gh failure never reaches Linear
  assert.equal(writes, 0)
})

test('runTidy: fail-closed when LINEAR_API_KEY missing (no reads, no writes)', async () => {
  let reads = 0
  await assert.rejects(
    runTidy({
      apiKey: '',
      readPullRequests: async () => {
        reads += 1
        return []
      },
      linearReadImpl: async () => {
        reads += 1
        return fixtureData()
      },
    }),
    /LINEAR_API_KEY is not set/,
  )
  assert.equal(reads, 0)
})

test('runTidy: no Done workflow state escalates the close rather than guessing', async () => {
  const data = fixtureData()
  data.workflowStates.nodes = data.workflowStates.nodes.filter((s) => s.type !== 'completed')
  const result = await runTidy({
    apiKey: 'k',
    dryRun: true,
    readPullRequests: async () => fixturePrs(),
    linearReadImpl: async () => data,
    linearWriteImpl: async () => ({ issueUpdate: { success: true } }),
  })
  assert.deepEqual(result.closed, [])
  assert.ok(result.escalations.some((e) => e.kind === 'no-done-state'))
})
