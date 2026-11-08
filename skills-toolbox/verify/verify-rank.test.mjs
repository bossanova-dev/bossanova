import { test } from 'node:test'
import assert from 'node:assert/strict'
import { rankOrder, rankFacts, rankRefreshes, storedRankOf } from './verify-rank.mjs'

const rank = (extra = {}) => ({
  tier: 3,
  tierFrom: 'own',
  deadline: null,
  deadlineFrom: null,
  scheduled: true,
  leverage: 0,
  ...extra,
})
const entry = (number, value) => ({
  pr: { number, url: `https://github.com/o/r/pull/${number}` },
  ticket: value === undefined ? null : { identifier: `T-${number}`, rank: value },
})
const stored = (r) => ({
  tier: r.tier,
  tierSource: r.tierFrom,
  deadlineOn: r.deadline ?? undefined,
  deadlineSource: r.deadlineFrom ?? '',
  isScheduled: r.scheduled,
  unblockedTicketCount: r.leverage,
  rankedAt: '2026-11-01T00:00:00Z',
})

test('rank order is stable, leaves its input alone and sorts unranked PRs last', () => {
  const entries = [
    entry(10, rank()),
    entry(11, rank({ tier: 1 })),
    entry(12, {}),
    entry(9),
    entry(14, rank({ deadline: '2026-01-01' })),
    entry(13, rank({ leverage: 1 })),
  ]
  const before = structuredClone(entries)
  assert.deepEqual(
    rankOrder(entries).map((e) => e.pr.number),
    [11, 14, 13, 10, 9, 12],
  )
  assert.deepEqual(entries, before)
  const equal = [entry(1, rank()), entry(1, rank())]
  assert.equal(rankOrder(equal)[0], equal[0])
})

test('rank facts include all ranked linked PRs in scope, deterministically', () => {
  const entries = [entry(11, rank()), entry(9), entry(10, rank()), entry(12, {})]
  assert.deepEqual(
    rankFacts(entries, (p) => p.number !== 11),
    [{ ticket: 'T-10', pr: 10, url: entries[2].pr.url, rank: rank() }],
  )
})

test('refreshes normalize the shipped trackerRank row and compare all six keys', () => {
  const fresh = rank()
  const facts = [{ ticket: 'T-1', pr: 1, rank: fresh }]
  const row = { id: 's1', tracker_id: 't-1', trackerRank: stored(fresh) }
  assert.deepEqual(storedRankOf(row), fresh)
  assert.deepEqual(rankRefreshes(facts, [row]), [])
  for (const changed of [
    undefined,
    { ...stored(fresh), deadlineSource: undefined },
    stored(rank({ tier: 1 })),
    stored(rank({ tierFrom: 'project' })),
    stored(rank({ deadline: '2026-01-01' })),
    stored(rank({ deadlineFrom: 'project' })),
    stored(rank({ scheduled: false })),
    stored(rank({ leverage: 2 })),
  ]) {
    const calls = rankRefreshes(facts, [{ ...row, trackerRank: changed }])
    assert.equal(calls.length, 1)
    assert.deepEqual(calls[0].argv.slice(0, -1), [
      'session',
      'set-rank',
      's1',
      'T-1',
      '--rank-json',
    ])
    assert.deepEqual(JSON.parse(calls[0].argv.at(-1)), fresh)
  }
  assert.deepEqual(
    rankRefreshes(facts, [
      { ...row, archived_at: 'today' },
      { ...row, tracker_id: 'T-2' },
      { ...row, id: '' },
    ]),
    [],
  )
})
