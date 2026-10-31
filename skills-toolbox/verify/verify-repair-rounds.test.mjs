import { test } from 'node:test'
import assert from 'node:assert/strict'
import { repairRounds } from './verify-repair-rounds.mjs'
import { operatorMarkers } from './verify-gate.mjs'
import { encodeOutcomesTrailer } from './verify-blockers.mjs'

const a = 'a'.repeat(40),
  b = 'b'.repeat(40),
  c = 'c'.repeat(40)
const outcome = (blockersHead, at, outcomes, extra = {}) => ({
  kind: 'repair-outcomes',
  blockersHead,
  head: c,
  at,
  id: at,
  outcomes,
  ...extra,
})
const stuck = (reason = 'needs credentials') => ({ id: 'check:ci', outcome: 'not-fixable', reason })
test('rounds survive pushed heads and repeated publications count once', () => {
  assert.equal(repairRounds([outcome(a, 1, [stuck()]), outcome(b, 2, [stuck()])]).rounds, 2)
  assert.equal(repairRounds([outcome(a, 1, [stuck()]), outcome(a, 2, [stuck()])]).rounds, 1)
})
test('the newest operator decision resets rounds using timestamp and id', () => {
  for (const kind of ['approve', 'send-back']) {
    const markers = [
      outcome(a, 1, [stuck()]),
      { kind, at: 2, id: 4 },
      outcome(b, 2, [stuck()], { id: 3 }),
      outcome(c, 2, [stuck()], { id: 5 }),
    ]
    assert.equal(repairRounds(markers).rounds, 1)
    assert.equal(repairRounds([...markers, { kind, at: 3, id: 1 }]).rounds, 0)
  }
})
test('latest outcome wins and summaries come from the newest matching blocker report', () => {
  const markers = [
    outcome(a, 2, [stuck('first')]),
    outcome(b, 4, [stuck('second')]),
    {
      kind: 'blockers',
      head: b,
      at: 1,
      id: 1,
      blockerFile: { blockers: [{ id: 'check:ci', summary: 'old' }] },
    },
    {
      kind: 'blockers',
      head: b,
      at: 3,
      id: 3,
      blockerFile: {
        blockers: [{ id: 'check:ci', summary: 'CI', severity: 'P1', file: 'a.go', line: 9 }],
      },
    },
  ]
  assert.deepEqual(repairRounds(markers).notFixable, [
    {
      id: 'check:ci',
      summary: 'CI',
      severity: 'P1',
      file: 'a.go',
      line: 9,
      reason: 'second',
      round: 2,
    },
  ])
  assert.equal(
    repairRounds([...markers, outcome(c, 5, [{ id: 'check:ci', outcome: 'fixed', commit: c }])])
      .notFixable.length,
    0,
  )
  assert.equal(repairRounds([outcome(a, 1, [stuck()])]).notFixable[0].summary, 'check:ci')
})
test('duplicate round uses latest publication without changing the round number', () => {
  const result = repairRounds([
    outcome(a, 1, [stuck('old')]),
    outcome(b, 2, [stuck('other')]),
    outcome(a, 3, [stuck('updated')]),
  ])
  assert.equal(result.rounds, 2)
  assert.equal(result.notFixable[0].reason, 'updated')
  assert.equal(result.notFixable[0].round, 1)
})
test('outcomes markers require a valid final trailer and trusted author association', () => {
  const file = { version: 1, blockersHead: a, pushedHead: b, outcomes: [stuck()] }
  const trailer = encodeOutcomesTrailer(file)
  const comment = (body, association = 'OWNER') => ({
    body,
    author_association: association,
    user: { login: 'writer' },
    created_at: '2026-01-01T00:00:00Z',
    id: 1,
  })
  const markers = operatorMarkers([comment(trailer)])
  assert.equal(markers[0].kind, 'repair-outcomes')
  assert.equal(markers[0].head, b)
  assert.equal(markers[0].blockersHead, a)
  assert.deepEqual(markers[0].outcomes, file.outcomes)
  for (const body of [
    trailer + '\nafter',
    trailer.replace('head=' + b, 'head=' + c),
    trailer.replace(' -->', '= -->'),
    '<!-- boss-repair-outcomes broken -->',
  ])
    assert.deepEqual(operatorMarkers([comment(body)]), [])
  assert.deepEqual(operatorMarkers([comment(trailer, 'NONE')]), [])
})

const send = (head, at, id = at) => ({
  kind: 'blockers',
  head,
  at,
  id,
  blockerFile: {
    blockers: [{ id: 'check:ci', summary: 'CI', severity: 'P1', file: 'a.go', line: 9 }],
  },
})
test('not-fixable repetition counts distinct sends on the same head and across heads', () => {
  for (const head of [a, b]) {
    const markers = [
      send(a, 1),
      outcome(a, 2, [stuck('first')]),
      send(head, 3),
      outcome(head, 4, [stuck('second')]),
    ]
    assert.deepEqual(repairRounds(markers).repeatNotFixable, [
      {
        id: 'check:ci',
        summary: 'CI',
        severity: 'P1',
        file: 'a.go',
        line: 9,
        reason: 'second',
        round: head === a ? 1 : 2,
        count: 2,
      },
    ])
    for (const kind of ['approve', 'send-back']) {
      const reset = { kind, at: 3, id: 0 }
      assert.deepEqual(repairRounds([...markers, reset]).repeatNotFixable, [])
      assert.deepEqual(repairRounds([...markers, { kind, at: 5, id: 5 }]).repeatNotFixable, [])
    }
  }
})
test('republished outcomes and reports newer than their outcome never add a send', () => {
  const markers = [send(a, 1), outcome(a, 2, [stuck()]), outcome(a, 3, [stuck()]), send(a, 4)]
  assert.deepEqual(repairRounds(markers).repeatNotFixable, [])
  assert.deepEqual(repairRounds([outcome(a, 1, [stuck()]), ...markers]).repeatNotFixable, [])
  const future = send(a, 5)
  future.blockerFile.blockers[0].summary = 'future report'
  assert.equal(repairRounds([...markers, future]).notFixable[0].summary, 'CI')
})

test('a late republication updates the repeated blocker reason without counting a third send', () => {
  const markers = [
    send(a, 1),
    outcome(a, 2, [stuck('first')]),
    send(b, 3),
    outcome(b, 4, [stuck('second')]),
    outcome(a, 5, [stuck('latest')]),
  ]
  const repeated = repairRounds(markers).repeatNotFixable[0]
  assert.equal(repeated.count, 2)
  assert.equal(repeated.reason, 'latest')
})
