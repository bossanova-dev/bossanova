import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

import { rankWork } from './rank-work.mjs'

const TODAY = '2026-11-07'
const [URGENT, HIGH, MEDIUM, LOW, NONE] = [1, 2, 3, 4, 0]

const ticket = (identifier, priority, more = {}) => ({
  identifier,
  priority,
  createdAt: '2026-10-01T00:00:00.000Z',
  ...more,
})
const blocks = (...refs) => ({
  relations: {
    nodes: refs.map((ref) => ({
      type: 'blocks',
      relatedIssue:
        typeof ref === 'string' ? { identifier: ref, state: { type: 'unstarted' } } : ref,
    })),
  },
})
const order = (nodes) => rankWork(nodes, { today: TODAY }).map((n) => n.identifier)
const rankOf = (nodes, identifier) =>
  rankWork(nodes, { today: TODAY }).find((n) => n.identifier === identifier).rank

test('estimate never orders work: the older ticket wins over a smaller one', () => {
  assert.deepEqual(
    order([
      ticket('small-new', HIGH, { estimate: 1, createdAt: '2026-10-05T00:00:00.000Z' }),
      ticket('big-old', HIGH, { estimate: 8, createdAt: '2026-10-01T00:00:00.000Z' }),
    ]),
    ['big-old', 'small-new'],
  )
})

test('a Medium blocker of Urgent work ranks Urgent, ahead of High work', () => {
  const nodes = [
    ticket('high', HIGH),
    ticket('blocker', MEDIUM, blocks('urgent')),
    ticket('urgent', URGENT, { createdAt: '2026-10-09T00:00:00.000Z' }),
  ]
  assert.deepEqual(order(nodes).slice(0, 2), ['blocker', 'urgent'])
  assert.deepEqual(
    [rankOf(nodes, 'blocker').tier, rankOf(nodes, 'blocker').tierFrom],
    [URGENT, 'blocks urgent'],
  )
})

test('a blocked ticket that is not a candidate still lifts its blocker through its own facts', () => {
  const nodes = [
    ticket('high', HIGH),
    ticket(
      'blocker',
      LOW,
      blocks({ identifier: 'X-9', state: { type: 'backlog' }, priority: URGENT }),
    ),
  ]
  assert.deepEqual(order(nodes), ['blocker', 'high'])
})

test('a closed dependent lifts nothing', () => {
  const nodes = [
    ticket('high', HIGH),
    ticket(
      'blocker',
      LOW,
      blocks({ identifier: 'X-9', state: { type: 'completed' }, priority: URGENT }),
    ),
  ]
  assert.deepEqual(order(nodes), ['high', 'blocker'])
  assert.equal(rankOf(nodes, 'blocker').leverage, 0)
})

test('an Urgent grandparent epic lifts a Low ticket', () => {
  const nodes = [
    ticket('high', HIGH),
    ticket('child', LOW, {
      parent: { identifier: 'E-1', priority: LOW, parent: { identifier: 'E-0', priority: URGENT } },
    }),
  ]
  assert.deepEqual(order(nodes), ['child', 'high'])
  assert.equal(rankOf(nodes, 'child').tierFrom, 'epic E-0')
})

test('an Urgent project lifts its issues the way an epic does', () => {
  const nodes = [
    ticket('high', HIGH),
    ticket('in-project', LOW, { project: { id: 'p1', name: 'P1', priority: URGENT } }),
  ]
  assert.deepEqual(order(nodes), ['in-project', 'high'])
  assert.equal(rankOf(nodes, 'in-project').tierFrom, 'project')
})

test('a blocker inherits the deadline of the work it holds up', () => {
  const nodes = [
    ticket('later', URGENT, { project: { id: 'p1', targetDate: '2026-11-20' } }),
    ticket('blocker', URGENT, blocks('due-soon')),
    ticket('due-soon', URGENT, { dueDate: '2026-11-10', createdAt: '2026-10-09T00:00:00.000Z' }),
  ]
  assert.deepEqual(order(nodes), ['blocker', 'due-soon', 'later'])
  assert.deepEqual(
    [rankOf(nodes, 'blocker').deadline, rankOf(nodes, 'blocker').deadlineFrom],
    ['2026-11-10', 'blocks due-soon'],
  )
})

test("a project that must end before another starts takes that project's priority and start date", () => {
  const dependent = {
    id: 'q',
    name: 'Q',
    priority: URGENT,
    startDate: '2026-11-12',
    status: { type: 'planned' },
  }
  const nodes = [
    ticket('high', HIGH, { dueDate: '2026-11-08' }),
    ticket('prerequisite', LOW, {
      project: {
        id: 'p',
        name: 'P',
        relations: {
          nodes: [
            {
              type: 'dependency',
              anchorType: 'end',
              relatedAnchorType: 'start',
              relatedProject: dependent,
            },
          ],
        },
      },
    }),
  ]
  assert.deepEqual(order(nodes), ['prerequisite', 'high'])
  assert.deepEqual(rankOf(nodes, 'prerequisite'), {
    tier: URGENT,
    tierFrom: 'blocks project Q',
    deadline: '2026-11-12',
    deadlineFrom: 'blocks project Q',
    scheduled: false,
    leverage: 0,
  })
})

test('overdue work sorts first and undated work last', () => {
  assert.deepEqual(
    order([
      ticket('undated', URGENT),
      ticket('next-month', URGENT, { dueDate: '2026-12-01' }),
      ticket('overdue', URGENT, { dueDate: '2026-11-01' }),
    ]),
    ['overdue', 'next-month', 'undated'],
  )
})

test('work in a project not yet due to start comes after started and unprojected work', () => {
  assert.deepEqual(
    order([
      ticket('future', HIGH, {
        project: { id: 'f', startDate: '2026-12-01', status: { type: 'planned' } },
        createdAt: '2026-09-01T00:00:00.000Z',
      }),
      ticket('started', HIGH, { project: { id: 's', status: { type: 'started' } } }),
      ticket('due-to-start', HIGH, {
        project: { id: 'd', startDate: '2026-11-02', status: { type: 'planned' } },
        createdAt: '2026-10-02T00:00:00.000Z',
      }),
      ticket('no-project', HIGH, { createdAt: '2026-10-03T00:00:00.000Z' }),
    ]),
    ['started', 'due-to-start', 'no-project', 'future'],
  )
})

test('the ticket that unblocks more open work goes first', () => {
  const nodes = [
    ticket('unblocks-one', MEDIUM, { ...blocks('a'), createdAt: '2026-09-01T00:00:00.000Z' }),
    ticket('unblocks-three', MEDIUM, blocks('b')),
    ticket('b', MEDIUM, { ...blocks('c', 'd'), createdAt: '2026-10-08T00:00:00.000Z' }),
  ]
  assert.deepEqual(order(nodes)[0], 'unblocks-three')
  assert.equal(rankOf(nodes, 'unblocks-three').leverage, 3)
})

test("the tracker's manual order breaks ties: project order, then issue order", () => {
  assert.deepEqual(
    order([
      ticket('second-project', LOW, {
        project: { id: 'b', prioritySortOrder: 20 },
        prioritySortOrder: -5,
      }),
      ticket('first-project-late', LOW, {
        project: { id: 'a', prioritySortOrder: 10 },
        prioritySortOrder: 7,
      }),
      ticket('first-project-early', LOW, {
        project: { id: 'a', prioritySortOrder: 10 },
        prioritySortOrder: 3,
      }),
    ]),
    ['first-project-early', 'first-project-late', 'second-project'],
  )
})

test('cycles in blocks relations and parent chains end the walk instead of hanging', () => {
  const epic = { identifier: 'E-1', priority: LOW }
  epic.parent = epic
  const nodes = [
    ticket('a', LOW, { ...blocks('b'), parent: epic }),
    ticket('b', MEDIUM, blocks('a')),
  ]
  assert.deepEqual(
    [rankOf(nodes, 'a').tier, rankOf(nodes, 'a').leverage, rankOf(nodes, 'b').leverage],
    [MEDIUM, 1, 1],
  )
})

test('missing facts are neutral: nothing throws and unknown values sort after known ones', () => {
  const nodes = [
    { identifier: 'bare' },
    ticket('none', NONE, { createdAt: '2026-09-01T00:00:00.000Z' }),
    ticket('low', LOW, { project: null, parent: null, relations: null }),
  ]
  assert.deepEqual(order(nodes), ['low', 'none', 'bare'])
  assert.deepEqual(rankOf(nodes, 'bare'), {
    tier: NONE,
    tierFrom: 'own',
    deadline: null,
    deadlineFrom: null,
    scheduled: true,
    leverage: 0,
  })
})

// Anonymised from one adopter's planned backlog on 2026-11-07. Expected order from the reference
// implementation of this rule: the three tickets of the overdue Urgent project, then the Urgent
// ticket that unblocks nine, then the Urgent ticket that unblocks ten.
const fixture = (name) =>
  JSON.parse(readFileSync(new URL(`./testdata/rank-work/${name}`, import.meta.url), 'utf8'))

test('on a real backlog the first unblocked picks follow tier, deadline and leverage', () => {
  const excluded = new Set(['needs-human', 'excluded', 'epic'])
  const nodes = fixture('planned-backlog.json').filter(
    (n) => !n.labels.nodes.some((label) => excluded.has(label.name)),
  )
  const unblocked = rankWork(nodes, { today: TODAY }).filter(
    (n) =>
      !n.inverseRelations.nodes.some(
        (r) => r.type === 'blocks' && r.issue.state.type !== 'completed',
      ),
  )
  assert.deepEqual(
    unblocked.slice(0, 5).map((n) => [n.identifier, n.rank.tier, n.rank.deadline, n.rank.leverage]),
    [
      ['ACME-78', URGENT, '2026-11-06', 0],
      ['ACME-77', URGENT, '2026-11-06', 0],
      ['ACME-76', URGENT, '2026-11-06', 0],
      ['ACME-45', URGENT, '2026-11-13', 9],
      ['ACME-69', URGENT, '2026-11-27', 10],
    ],
  )
})

test('on the list a build once picked a Low ticket from, no Low or Medium work outranks unblocked Urgent work', () => {
  const ranked = rankWork(fixture('list-planned-failing-run.json'), { today: TODAY })
  const unblocked = ranked.filter((n) => n.blockerVerdict === 'unblocked')
  const lastUrgent = unblocked.findLastIndex((n) => n.rank.tier === URGENT)
  assert.ok(lastUrgent >= 3, 'the run had four unblocked Urgent candidates')
  assert.ok(unblocked.slice(0, lastUrgent + 1).every((n) => n.rank.tier === URGENT))
  assert.ok(
    unblocked.findIndex((n) => n.identifier === 'ACME-92') > lastUrgent,
    'the Low ticket that run claimed ranks after every unblocked Urgent one',
  )
})
