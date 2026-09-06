// scripts/linear-tidy-lib.test.mjs
// Full-branch unit tests for the bs-sweep-tidy-linear tidy logic. No network:
// every I/O boundary is a fixture-backed injected function.

import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'

import {
  STATE_RANK,
  STARTED_RANK,
  rankOf,
  isTerminalStateName,
  isStartedStateName,
  parseLinearTag,
  groupPrsByTicket,
  computeCloseable,
  computeRollups,
  hasImplementationPlan,
  computePlanningReconcile,
  parseTidyData,
  runTidy,
} from './linear-tidy-lib.mjs'

const st = (name, type = 'unstarted', id = `state-${name}`) => ({ id, name, type })
const pr = (number, title, state) => ({ number, title, state, url: `https://gh/pr/${number}` })

// --- STATE_RANK -------------------------------------------------------------

test('STATE_RANK enumerates state roles in a strict total order', () => {
  assert.deepEqual(Object.keys(STATE_RANK), [
    'backlog',
    'unplanned',
    'planned',
    'inProgress',
    'inReview',
    'done',
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
  assert.equal(isTerminalStateName('Todo'), false)
})

test('isStartedStateName / STARTED_RANK: true only for non-terminal started states', () => {
  assert.equal(STARTED_RANK, STATE_RANK.inProgress)
  // Started, non-terminal → true.
  assert.equal(isStartedStateName('In Progress'), true)
  assert.equal(isStartedStateName('In Review'), true)
  // Not-started → false.
  for (const s of ['Backlog', 'Unplanned', 'Todo']) assert.equal(isStartedStateName(s), false)
  // Terminal → false even though Done outranks In Progress.
  for (const s of ['Done', 'Canceled', 'Duplicate']) assert.equal(isStartedStateName(s), false)
  // Unrankable / missing → false, never a throw.
  assert.equal(isStartedStateName('Triage'), false)
  assert.equal(isStartedStateName(undefined), false)
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

test('computeRollups: a mix of not-started and started children rolls the parent to the least-advanced STARTED child', () => {
  const parents = [
    {
      id: 'p',
      identifier: 'BOS-200',
      state: st('Backlog', 'backlog'),
      children: [
        { id: 'a', identifier: 'BOS-201', state: st('Done', 'completed') }, // terminal → ignored
        { id: 'b', identifier: 'BOS-202', state: st('Todo') }, // not-started → does NOT drag the target down
        { id: 'c', identifier: 'BOS-203', state: st('In Progress', 'started') }, // least-advanced STARTED child
      ],
    },
  ]
  const { move } = computeRollups(parents)
  assert.equal(move.length, 1)
  assert.equal(move[0].to, 'In Progress')
})

test('computeRollups: an epic mixing Unplanned and In Progress children rolls to In Progress (started when any started)', () => {
  const parents = [
    {
      id: 'p',
      identifier: 'BOS-250',
      state: st('Unplanned', 'unstarted'),
      children: [
        { id: 'a', identifier: 'BOS-251', state: st('Unplanned', 'unstarted') },
        { id: 'b', identifier: 'BOS-252', state: st('In Progress', 'started') },
      ],
    },
  ]
  const { move } = computeRollups(parents)
  assert.equal(move.length, 1)
  assert.equal(move[0].to, 'In Progress')
})

test('computeRollups: with no started child, the target is still the least-advanced open child', () => {
  const parents = [
    {
      id: 'p',
      identifier: 'BOS-260',
      state: st('Backlog', 'backlog'),
      children: [
        { id: 'a', identifier: 'BOS-261', state: st('Unplanned', 'unstarted') },
        { id: 'b', identifier: 'BOS-262', state: st('Todo') },
      ],
    },
  ]
  const { move } = computeRollups(parents)
  assert.equal(move.length, 1)
  assert.equal(move[0].to, 'Unplanned')
})

test('computeRollups: an epic whose open children are all In Review still rolls to In Review', () => {
  const parents = [
    {
      id: 'p',
      identifier: 'BOS-270',
      state: st('Todo'),
      children: [
        { id: 'a', identifier: 'BOS-271', state: st('In Review', 'started') },
        { id: 'b', identifier: 'BOS-272', state: st('In Review', 'started') },
      ],
    },
  ]
  const { move } = computeRollups(parents)
  assert.equal(move.length, 1)
  assert.equal(move[0].to, 'In Review')
})

test('computeRollups: reporter shape regresses In Review parent to In Progress', () => {
  const { move, escalate } = computeRollups([
    {
      id: 'p',
      identifier: 'BOS-1140',
      state: st('In Review', 'started'),
      children: [
        { id: 'a', identifier: 'BOS-1141', state: st('Done', 'completed') },
        { id: 'b', identifier: 'BOS-1142', state: st('Done', 'completed') },
        { id: 'c', identifier: 'BOS-1143', state: st('In Progress', 'started') },
        { id: 'd', identifier: 'BOS-1144', state: st('Todo') },
      ],
    },
  ])
  assert.deepEqual(escalate, [])
  assert.deepEqual(move, [
    {
      id: 'p',
      identifier: 'BOS-1140',
      from: 'In Review',
      to: 'In Progress',
      toStateId: 'state-In Progress',
    },
  ])
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

test('computeRollups: never regress a started parent toward a not-started child', () => {
  // Non-vacuity proof for the target-state conjunct in the guarded backward branch.
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

test('computeRollups: never move a terminal parent back into a started state', () => {
  // Non-vacuity proof for the parent-state conjunct in the guarded backward branch.
  const result = computeRollups([
    {
      id: 'p',
      identifier: 'BOS-410',
      state: st('Done', 'completed'),
      children: [{ id: 'a', identifier: 'BOS-411', state: st('In Progress', 'started') }],
    },
  ])
  assert.deepEqual(result.move, [])
  assert.deepEqual(result.escalate, [])
})

test('computeRollups: backward rollup is idempotent in the settled reporter shape', () => {
  const result = computeRollups([
    {
      id: 'p',
      identifier: 'BOS-1140',
      state: st('In Progress', 'started'),
      children: [
        { id: 'a', identifier: 'BOS-1141', state: st('Done', 'completed') },
        { id: 'b', identifier: 'BOS-1142', state: st('Done', 'completed') },
        { id: 'c', identifier: 'BOS-1143', state: st('In Progress', 'started') },
        { id: 'd', identifier: 'BOS-1144', state: st('Todo') },
      ],
    },
  ])
  assert.deepEqual(result.move, [])
  assert.deepEqual(result.escalate, [])
})

test('computeRollups: started-state direction table', () => {
  const cases = [
    { parent: 'In Review', children: ['In Progress'], expectedTo: 'In Progress' },
    { parent: 'In Review', children: ['In Review'], expectedTo: null },
    { parent: 'In Progress', children: ['In Review'], expectedTo: 'In Review' },
    { parent: 'In Progress', children: ['In Progress', 'In Review'], expectedTo: null },
    { parent: 'In Review', children: ['In Progress', 'In Review'], expectedTo: 'In Progress' },
  ]

  for (const [caseIndex, entry] of cases.entries()) {
    const result = computeRollups([
      {
        id: `p-${caseIndex}`,
        identifier: `BOS-${420 + caseIndex}`,
        state: st(entry.parent, 'started'),
        children: entry.children.map((name, childIndex) => ({
          id: `c-${caseIndex}-${childIndex}`,
          identifier: `BOS-${520 + caseIndex * 10 + childIndex}`,
          state: st(name, 'started'),
        })),
      },
    ])
    assert.deepEqual(result.escalate, [])
    assert.equal(result.move[0]?.to ?? null, entry.expectedTo, JSON.stringify(entry))
  }
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
      state: st('In Review', 'started'),
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
      children: [{ id: 'a', identifier: 'BOS-701', state: st('In Progress', 'started') }],
    },
  ])
  assert.deepEqual(
    parentCase.escalate.map((e) => e.kind),
    ['rollup-unrankable-parent'],
  )
  assert.deepEqual(parentCase.move, [])

  const canceledParentCase = computeRollups([
    {
      id: 'p',
      identifier: 'BOS-710',
      state: st('Canceled', 'canceled'),
      children: [{ id: 'a', identifier: 'BOS-711', state: st('In Progress', 'started') }],
    },
  ])
  assert.deepEqual(
    canceledParentCase.escalate.map((e) => e.kind),
    ['rollup-unrankable-parent'],
  )
  assert.deepEqual(canceledParentCase.move, [])
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

test('computeRollups: truncated child page never regresses on a partial view', () => {
  const result = computeRollups([
    {
      id: 'p',
      identifier: 'BOS-810',
      state: st('In Review', 'started'),
      childrenTruncated: true,
      children: [{ id: 'a', identifier: 'BOS-811', state: st('In Progress', 'started') }],
    },
  ])
  assert.deepEqual(result.move, [])
  assert.deepEqual(
    result.escalate.map((e) => `${e.kind}:${e.identifier}`),
    ['rollup-children-truncated:BOS-810'],
  )
})

test('bs-sweep-tidy-linear prose matches guarded bidirectional rollup behavior', () => {
  const skill = readFileSync('.claude/skills/bs-sweep-tidy-linear/SKILL.md', 'utf8')
  assert.doesNotMatch(skill, /Forward\s+only\./i)
  assert.doesNotMatch(skill, /never\s+moved\s+backward/i)
  assert.doesNotMatch(skill, /forward-only\s+and\s+idempotent/i)
  assert.match(skill, /regresses\s+only\s+within\s+started,\s+non-terminal\s+states/i)
  assert.match(skill, /never\s+regresses\s+toward\s+a\s+not-started\s+child/i)
  assert.match(skill, /never\s+moves\s+a\s+terminal\s+parent/i)
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

test('runTidy: applies a backward rollup with the child workflow state id', async () => {
  const data = {
    workflowStates: {
      nodes: [
        { id: 's-in-progress', name: 'In Progress', type: 'started' },
        { id: 's-in-review', name: 'In Review', type: 'started' },
        { id: 's-done', name: 'Done', type: 'completed' },
      ],
    },
    issues: {
      nodes: [
        {
          id: 'i-parent',
          identifier: 'BOS-1140',
          state: st('In Review', 'started', 's-in-review'),
          children: {
            nodes: [
              {
                id: 'i-child',
                identifier: 'BOS-1143',
                state: st('In Progress', 'started', 's-in-progress'),
              },
            ],
          },
        },
        {
          id: 'i-child',
          identifier: 'BOS-1143',
          state: st('In Progress', 'started', 's-in-progress'),
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
    readPullRequests: async () => [],
    linearReadImpl: async () => data,
    linearWriteImpl: async ({ variables }) => {
      writes.push(variables)
      return { issueUpdate: { success: true } }
    },
  })

  assert.deepEqual(writes, [{ id: 'i-parent', stateId: 's-in-progress' }])
  assert.deepEqual(
    result.rolledUp.map((move) => `${move.identifier}->${move.to}`),
    ['BOS-1140->In Progress'],
  )
  assert.equal(result.counts.prReads, 1)
  assert.equal(result.counts.linearReads, 1)
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

// --- Behaviour 3: planning-queue reconcile ----------------------------------

const lbl = (name, id = `label-${name}`) => ({ id, name })
const planAttachment = (id = 'BOS-1') => ({
  title: `Implementation plan (${id})`,
  url: `https://uploads.linear.app/bossanova/${id}/implementation-plan.md`,
})
const legacyPlanLink = (id = 'BOS-1') => ({
  title: `Implementation plan (${id})`,
  url: `https://proof.bossanova.dev/plans/bossanova/${id}/abc.md`,
})
// Ticket shape as produced by parseTidyData (id/identifier/state/labels/attachments).
const tk = (id, identifier, stateName, labels = [], attachments = []) => ({
  id,
  identifier,
  state: st(stateName),
  labels,
  attachments,
})

const RECONCILE_IDS = {
  agentPlanId: 'label-agent-plan',
  agentFriendlyId: 'label-agent-friendly',
  todoStateId: 's-todo',
}

test("hasImplementationPlan: accepts only the ticket's native canonical attachment", () => {
  assert.equal(hasImplementationPlan('BOS-7', [planAttachment('BOS-7')]), true)
  assert.equal(hasImplementationPlan('BOS-7', [legacyPlanLink('BOS-7')]), false)
  assert.equal(hasImplementationPlan('BOS-7', [planAttachment('BOS-8')]), false)
  assert.equal(
    hasImplementationPlan('BOS-7', [
      { title: 'renamed', url: 'https://proof.bossanova.dev/plans/bossanova/BOS-7/a.md' },
    ]),
    false,
  )
  assert.equal(
    hasImplementationPlan('BOS-7', [{ title: 'Design doc', url: 'https://example.com' }]),
    false,
  )
  assert.equal(hasImplementationPlan('BOS-7', []), false)
  assert.equal(hasImplementationPlan('BOS-7', undefined), false)
})

test('computePlanningReconcile: agent-friendly + Unplanned + no plan → drop agent-friendly, add agent-plan', () => {
  const tickets = [tk('i1', 'BOS-1', 'Unplanned', [lbl('agent-friendly'), lbl('bug')])]
  const r = computePlanningReconcile(tickets, RECONCILE_IDS)
  assert.deepEqual(r.toTodo, [])
  assert.deepEqual(
    r.toAgentPlan.map((t) => t.identifier),
    ['BOS-1'],
  )
  // Kept bug, dropped agent-friendly, added agent-plan (full replacement set).
  assert.deepEqual(r.toAgentPlan[0].newLabelIds, ['label-bug', 'label-agent-plan'])
  assert.deepEqual(r.escalate, [])
})

test('computePlanningReconcile: agent-friendly + Unplanned + plan attached → move to Todo, labels untouched', () => {
  const tickets = [
    tk('i1', 'BOS-1', 'Unplanned', [lbl('agent-friendly')], [planAttachment('BOS-1')]),
  ]
  const r = computePlanningReconcile(tickets, RECONCILE_IDS)
  assert.deepEqual(r.toAgentPlan, [])
  assert.deepEqual(
    r.toTodo.map((t) => t.identifier),
    ['BOS-1'],
  )
})

test('computePlanningReconcile: a different ticket’s attachment queues the ticket for planning', () => {
  const tickets = [
    tk('i1', 'BOS-7', 'Unplanned', [lbl('agent-friendly')], [planAttachment('BOS-8')]),
  ]
  const r = computePlanningReconcile(tickets, RECONCILE_IDS)
  assert.deepEqual(r.toTodo, [])
  assert.deepEqual(
    r.toAgentPlan.map((t) => t.identifier),
    ['BOS-7'],
  )
})

test('computePlanningReconcile: legacy proof link is missing and requeues for agent-plan', () => {
  const tickets = [
    tk('i1', 'BOS-1', 'Unplanned', [lbl('agent-friendly')], [legacyPlanLink('BOS-1')]),
  ]
  const r = computePlanningReconcile(tickets, RECONCILE_IDS)
  assert.deepEqual(r.toTodo, [])
  assert.deepEqual(
    r.toAgentPlan.map((t) => t.identifier),
    ['BOS-1'],
  )
})

test('computePlanningReconcile: needs-human, already-agent-plan, non-Unplanned, epic parent, and non-agent-friendly are all skipped', () => {
  const tickets = [
    tk('i1', 'BOS-1', 'Unplanned', [lbl('agent-friendly'), lbl('needs-human')]),
    tk('i2', 'BOS-2', 'Unplanned', [lbl('agent-friendly'), lbl('agent-plan')]),
    tk('i3', 'BOS-3', 'Todo', [lbl('agent-friendly')]),
    tk('i4', 'BOS-4', 'Unplanned', [lbl('agent-friendly')]), // epic parent (blocked)
    tk('i5', 'BOS-5', 'Unplanned', [lbl('bug')]), // no agent-friendly
  ]
  const r = computePlanningReconcile(tickets, {
    ...RECONCILE_IDS,
    blockedIdentifiers: new Set(['BOS-4']),
  })
  assert.deepEqual(r.toTodo, [])
  assert.deepEqual(r.toAgentPlan, [])
  assert.deepEqual(r.escalate, [])
})

test('computePlanningReconcile: absent agent-plan label id escalates a no-plan case (never guesses)', () => {
  const tickets = [tk('i1', 'BOS-1', 'Unplanned', [lbl('agent-friendly')])]
  const r = computePlanningReconcile(tickets, { ...RECONCILE_IDS, agentPlanId: undefined })
  assert.deepEqual(r.toAgentPlan, [])
  assert.deepEqual(
    r.escalate.map((e) => `${e.kind}:${e.identifier}`),
    ['no-agent-plan-label:BOS-1'],
  )
})

test('computePlanningReconcile: absent Todo state id escalates a has-plan case', () => {
  const tickets = [
    tk('i1', 'BOS-1', 'Unplanned', [lbl('agent-friendly')], [planAttachment('BOS-1')]),
  ]
  const r = computePlanningReconcile(tickets, { ...RECONCILE_IDS, todoStateId: null })
  assert.deepEqual(r.toTodo, [])
  assert.deepEqual(
    r.escalate.map((e) => `${e.kind}:${e.identifier}`),
    ['no-todo-state:BOS-1'],
  )
})

test('parseTidyData: extracts per-ticket labels/attachments, todoStateId, and label ids by name', () => {
  const data = {
    workflowStates: {
      nodes: [
        { id: 's-todo', name: 'Todo', type: 'unstarted' },
        { id: 's-done', name: 'Done', type: 'completed' },
      ],
    },
    issueLabels: {
      nodes: [
        { id: 'label-agent-plan', name: 'agent-plan' },
        { id: 'label-agent-friendly', name: 'agent-friendly' },
      ],
    },
    issues: {
      nodes: [
        {
          id: 'i1',
          identifier: 'BOS-1',
          state: st('Unplanned'),
          labels: { nodes: [{ id: 'label-agent-friendly', name: 'agent-friendly' }] },
          attachments: {
            nodes: [{ title: 'Implementation plan (BOS-1)', url: 'https://proof.bossanova.dev/x' }],
          },
          children: { nodes: [] },
        },
      ],
      pageInfo: { hasNextPage: false },
    },
  }
  const parsed = parseTidyData(data)
  assert.equal(parsed.todoStateId, 's-todo')
  assert.equal(parsed.labelIdsByName.get('agent-plan'), 'label-agent-plan')
  assert.equal(parsed.labelIdsByName.get('agent-friendly'), 'label-agent-friendly')
  assert.deepEqual(parsed.tickets[0].labels, [
    { id: 'label-agent-friendly', name: 'agent-friendly' },
  ])
  assert.equal(parsed.tickets[0].attachments[0].title, 'Implementation plan (BOS-1)')
})

function reconcileData() {
  return {
    workflowStates: {
      nodes: [
        { id: 's-todo', name: 'Todo', type: 'unstarted' },
        { id: 's-done', name: 'Done', type: 'completed' },
      ],
    },
    issueLabels: {
      nodes: [
        { id: 'label-agent-plan', name: 'agent-plan' },
        { id: 'label-agent-friendly', name: 'agent-friendly' },
      ],
    },
    issues: {
      nodes: [
        // no plan → relabel to agent-plan
        {
          id: 'i-noplan',
          identifier: 'BOS-1',
          state: st('Unplanned'),
          labels: {
            nodes: [
              { id: 'label-agent-friendly', name: 'agent-friendly' },
              { id: 'label-bug', name: 'bug' },
            ],
          },
          attachments: { nodes: [] },
          children: { nodes: [] },
        },
        // has plan → move to Todo
        {
          id: 'i-planned',
          identifier: 'BOS-2',
          state: st('Unplanned'),
          labels: { nodes: [{ id: 'label-agent-friendly', name: 'agent-friendly' }] },
          attachments: {
            nodes: [
              {
                title: 'Implementation plan (BOS-2)',
                url: 'https://uploads.linear.app/bossanova/BOS-2/implementation-plan.md',
              },
            ],
          },
          children: { nodes: [] },
        },
      ],
      pageInfo: { hasNextPage: false },
    },
  }
}

test('runTidy: reconcile applies — planned Unplanned → Todo, no-plan → agent-plan relabel', async () => {
  const writes = []
  const result = await runTidy({
    apiKey: 'k',
    dryRun: false,
    readPullRequests: async () => [],
    linearReadImpl: async () => reconcileData(),
    linearWriteImpl: async ({ variables }) => {
      writes.push(variables)
      return { issueUpdate: { success: true } }
    },
  })
  assert.deepEqual(
    result.reclassified.toTodo.map((t) => `${t.identifier}->${t.to}`),
    ['BOS-2->Todo'],
  )
  assert.deepEqual(
    result.reclassified.toAgentPlan.map((t) => `${t.identifier}->${t.to}`),
    ['BOS-1->agent-plan'],
  )
  // toTodo applies before toAgentPlan; the relabel sends the full replacement set.
  assert.deepEqual(writes, [
    { id: 'i-planned', stateId: 's-todo' },
    { id: 'i-noplan', labelIds: ['label-bug', 'label-agent-plan'] },
  ])
  assert.equal(result.counts.writes, 2)
  assert.deepEqual(result.escalations, [])
})

test('runTidy: --dry-run reconcile computes actions but writes nothing', async () => {
  let writeCalls = 0
  const result = await runTidy({
    apiKey: 'k',
    dryRun: true,
    readPullRequests: async () => [],
    linearReadImpl: async () => reconcileData(),
    linearWriteImpl: async () => {
      writeCalls += 1
      return { issueUpdate: { success: true } }
    },
  })
  assert.equal(writeCalls, 0)
  assert.equal(result.counts.writes, 0)
  assert.deepEqual(
    result.reclassified.toAgentPlan.map((t) => t.identifier),
    ['BOS-1'],
  )
  assert.deepEqual(
    result.reclassified.toTodo.map((t) => t.identifier),
    ['BOS-2'],
  )
})

test('runTidy: reconcile idempotent — converted tickets yield no further actions', async () => {
  const settled = reconcileData()
  // BOS-1 now carries agent-plan instead of agent-friendly.
  settled.issues.nodes[0].labels.nodes = [
    { id: 'label-bug', name: 'bug' },
    { id: 'label-agent-plan', name: 'agent-plan' },
  ]
  // BOS-2 now sits in Todo (no longer Unplanned).
  settled.issues.nodes[1].state = st('Todo')
  let writeCalls = 0
  const result = await runTidy({
    apiKey: 'k',
    dryRun: false,
    readPullRequests: async () => [],
    linearReadImpl: async () => settled,
    linearWriteImpl: async () => {
      writeCalls += 1
      return { issueUpdate: { success: true } }
    },
  })
  assert.deepEqual(result.reclassified.toTodo, [])
  assert.deepEqual(result.reclassified.toAgentPlan, [])
  assert.equal(writeCalls, 0)
})

test('runTidy: reconcile escalates a no-plan ticket when the agent-plan label is absent (no write)', async () => {
  const data = reconcileData()
  data.issueLabels.nodes = data.issueLabels.nodes.filter((l) => l.name !== 'agent-plan')
  data.issues.nodes = [data.issues.nodes[0]] // keep only the no-plan candidate
  const writes = []
  const result = await runTidy({
    apiKey: 'k',
    dryRun: false,
    readPullRequests: async () => [],
    linearReadImpl: async () => data,
    linearWriteImpl: async ({ variables }) => {
      writes.push(variables)
      return { issueUpdate: { success: true } }
    },
  })
  assert.deepEqual(result.reclassified.toAgentPlan, [])
  assert.deepEqual(writes, [])
  assert.ok(result.escalations.some((e) => e.kind === 'no-agent-plan-label'))
})
