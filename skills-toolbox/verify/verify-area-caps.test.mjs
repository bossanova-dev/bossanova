import assert from 'node:assert/strict'
import test from 'node:test'
import {
  capKeys,
  activeCaps,
  capHistory,
  capAreas,
  capReason,
  renderClearComment,
  parseClearComment,
} from './verify-area-caps.mjs'

const date = (day) => `2026-01-${String(day).padStart(2, '0')}T00:00:00.000Z`
const escape = (original, day, area = 'ui', paths = ['src/view.ts'], fix = original + 100) => ({
  original: { number: original },
  fix: { number: fix, mergedAt: date(day) },
  levelAtMerge: 'high',
  areas: [{ area, paths }],
})
const clear = (day = 10) => ({
  v: 1,
  repo: 'owner/repo',
  key: 'ui',
  clearedAt: date(day),
  clearedBy: 'operator',
  reason: 'Guard now covers the failure',
  through: 102,
})

test('only two distinct High originals fixed within seven days cap an area', () => {
  assert.equal(activeCaps({ escapes: [escape(1, 1), escape(2, 4)] }).length, 1)
  assert.equal(activeCaps({ escapes: [escape(1, 1), escape(2, 8)] }).length, 1)
  assert.deepEqual(activeCaps({ escapes: [escape(1, 1), escape(2, 9)] }), [])
  assert.deepEqual(activeCaps({ escapes: [escape(1, 1), escape(1, 4, 'ui', [], 202)] }), [])
  for (const levelAtMerge of [null, 'moderate', 'low']) {
    assert.deepEqual(activeCaps({ escapes: [escape(1, 1), { ...escape(2, 4), levelAtMerge }] }), [])
  }
})

test('other keys follow two directory segments and affect only their module', () => {
  assert.deepEqual(capKeys('other', ['services/a/x/y.go', 'src/a.ts', 'root.txt']), [
    'other:.',
    'other:services/a',
    'other:src',
  ])
  const first = escape(1, 1, 'other', ['services/a/x.go'])
  assert.deepEqual(activeCaps({ escapes: [first, escape(2, 4, 'other', ['services/b/x.go'])] }), [])
  const caps = activeCaps({ escapes: [first, escape(2, 4, 'other', ['services/a/y.go'])] })
  assert.equal(caps[0].key, 'other:services/a')
  assert.deepEqual(capAreas([{ area: 'other', paths: ['services/b/z.go'] }], caps), [])
  const areas = capAreas(
    [{ area: 'other', paths: ['services/a/z.go', 'services/b/z.go', 'services/a/q.go'] }],
    caps,
  )
  assert.equal(areas.length, 1)
  assert.equal(areas[0].defaultAsk, 'always')
  assert.equal(areas[0].source, 'cap')
  assert.deepEqual(areas[0].paths, ['services/a/q.go', 'services/a/z.go'])
})

test('clears cover late traces and history retains each cap episode', () => {
  const escapes = [escape(1, 1), escape(2, 4), escape(3, 7)]
  assert.deepEqual(activeCaps({ escapes, clears: [clear()] }), [])
  escapes.push(escape(4, 12), escape(5, 14))
  const caps = activeCaps({ escapes, clears: [clear()] })
  assert.equal(caps[0].cappedAt, date(14))
  assert.deepEqual(
    caps[0].escapes.map((e) => e.original),
    [5, 4],
  )
  const history = capHistory({ escapes, clears: [clear()] })
  assert.equal(history.length, 2)
  assert.equal(history[0].cappedAt, date(4))
  assert.equal(history[0].clearedAt, date(10))
  assert.equal(history[0].clearedBy, 'operator')
  assert.equal(history[0].reason, clear().reason)
  assert.equal(history[1].clearedAt, undefined)
  assert.match(capReason(caps[0]), /#5/)
})

test('clear comments round trip, reject mismatched markers and invalid records, and drop secret reasons', () => {
  const record = clear()
  const rendered = renderClearComment(record)
  assert.deepEqual(parseClearComment(rendered), record)
  assert.equal(parseClearComment(rendered.replace('key=ui', 'key=other:src')), null)
  for (const changes of [
    { v: 2 },
    { through: 0 },
    { clearedAt: 'bad' },
    { repo: 'bad' },
    { clearedBy: '' },
    { key: 'ui -->' },
    { reason: 42 },
  ]) {
    const body = rendered.replace(JSON.stringify(record), JSON.stringify({ ...record, ...changes }))
    assert.equal(parseClearComment(body), null)
  }
  assert.equal(parseClearComment(rendered + '\n' + rendered), null)
  const secret = 'ghp_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN'
  assert.ok(!renderClearComment({ ...record, reason: secret }).includes(secret))
})

test('ordering and ties are deterministic without mutating inputs', () => {
  const escapes = [
    escape(2, 4),
    escape(1, 1),
    escape(1, 1, 'ui', ['src/view.ts'], 99),
    escape(3, 4),
  ]
  const before = JSON.stringify(escapes)
  const expected = activeCaps({ escapes })
  assert.deepEqual(activeCaps({ escapes: [...escapes].reverse() }), expected)
  assert.equal(expected[0].escapes.find((e) => e.original === 1).fix, 99)
  assert.equal(JSON.stringify(escapes), before)
  assert.deepEqual(
    capHistory({ escapes, clears: [clear(10), clear(11)] }),
    capHistory({ escapes: [...escapes].reverse(), clears: [clear(11), clear(10)] }),
  )
})

test('a triggered cap remains sticky and a clear includes its exact timestamp', () => {
  const escapes = [escape(1, 1), escape(2, 4), escape(3, 30)]
  const caps = activeCaps({ escapes })
  assert.equal(caps[0].cappedAt, date(4))
  assert.deepEqual(
    caps[0].escapes.map((entry) => entry.original),
    [3, 2, 1],
  )
  assert.deepEqual(activeCaps({ escapes, clears: [clear(30)] }), [])
  const headAreas = [
    { area: 'ui', paths: ['src/a.ts'] },
    { area: 'ui', paths: ['src/b.ts'] },
  ]
  const before = JSON.stringify({ headAreas, caps })
  const extra = capAreas(headAreas, caps)
  assert.equal(extra.length, 1)
  assert.deepEqual(extra[0].paths, ['src/a.ts', 'src/b.ts'])
  assert.equal(JSON.stringify({ headAreas, caps }), before)
})

test('clear keys preserve modules with spaces and Unicode directory names', () => {
  const clear = {
    v: 1,
    repo: 'owner/repo',
    key: 'other:packages/λ api',
    clearedAt: '2026-11-01T00:00:00Z',
    clearedBy: 'operator',
    reason: 'fixed',
    through: 8,
  }
  assert.deepEqual(parseClearComment(renderClearComment(clear)), clear)
})
