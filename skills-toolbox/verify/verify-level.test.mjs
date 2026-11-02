import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { confidence } from './verify-confidence.mjs'
import { levelFacts, legacyCodes, levelLabel, parkLevel, levelNoteLead } from './verify-level.mjs'

const facts = {
  changedPaths: [],
  areas: [],
  changedLines: 20,
  sizeBaseline: { ok: true, thresholds: { p90: 100, p99: 200 } },
  range: { kind: 'full' },
}
test('plain code is High; migrations ask through a legacy code', () => {
  assert.equal(confidence(levelFacts(facts)).level, 'high')
  const result = confidence(
    levelFacts({
      ...facts,
      areas: [{ area: 'migrations', label: 'Migrations', defaultAsk: 'always' }],
    }),
  )
  assert.equal(result.level, 'moderate')
  assert.deepEqual(legacyCodes(result), ['human-review:database'])
  assert.match(levelLabel(result), /^moderate:/)
})
test('changed-files collection requires an array and accepts an empty diff', () => {
  const { changedPaths: _changedPaths, ...withoutPaths } = facts
  const missing = [
    withoutPaths,
    ...[undefined, null, '', 'src/code.mjs', {}, 0, false].map((changedPaths) => ({
      ...facts,
      changedPaths,
    })),
  ]
  for (const gate of missing) {
    const result = confidence(levelFacts(gate))
    assert.equal(result.level, 'cannot-assess', JSON.stringify(gate))
    assert.deepEqual(legacyCodes(result), ['cannot-assess'])
    assert.match(result.reasons.join(' '), /changed-files/)
  }
  for (const changedPaths of [[], ['src/code.mjs']]) {
    assert.equal(confidence(levelFacts({ ...facts, changedPaths })).level, 'high')
  }
})
test('legacy config codes and extension asks dedupe in report order', () => {
  const result = confidence(
    levelFacts(
      {
        ...facts,
        areas: [{ area: 'config:Database', label: 'DB', defaultAsk: 'always', source: 'config' }],
      },
      { extensionReasons: ['human-review:ui', 'always-human-path'] },
    ),
  )
  assert.deepEqual(
    legacyCodes(result, {
      alwaysReview: [{ name: 'Database', parkCode: 'human-review:database' }],
    }),
    ['always-human-path', 'human-review:database', 'human-review:ui'],
  )
})
test('proof, policy, size, and missing collectors affect confidence', () => {
  assert.equal(
    levelFacts(facts, { extensionReasons: ['human-review:unproven'] }).proof.behavior,
    'unproven',
  )
  assert.equal(confidence(levelFacts({ ...facts, policySurfaceHit: true })).level, 'moderate')
  const large = confidence(levelFacts({ ...facts, changedLines: 201 }))
  assert.equal(large.level, 'low')
  assert.deepEqual(legacyCodes(large), ['always-human-path'])
  assert.deepEqual(legacyCodes(confidence(levelFacts({ ...facts, changedPaths: null }))), [
    'cannot-assess',
  ])
  assert.equal(levelFacts({ ...facts, range: { kind: 'delta' } }).size, undefined)
})
test('park levels include parks from older callers', () => {
  assert.equal(parkLevel({ codes: ['human-review:ui'], label: '' }), 'moderate')
  assert.equal(parkLevel({ codes: ['human-review:ui'], label: 'low: UI' }), 'low')
  assert.equal(parkLevel({ codes: ['cannot-assess'], label: '' }), 'cannot-assess')
  assert.equal(parkLevel({ codes: ['partial-build'], label: '' }), null)
})
test('labels cap asks while Low notes retain every checklist item', () => {
  const asks = ['Migrations', 'API', 'UI', 'Dependencies'].map((area) => ({
    area,
    label: area,
    source: 'builtin',
  }))
  const result = {
    level: 'low',
    asks,
    reasons: ['Splitting would raise this.'],
    loweredBy: [],
    health: [],
  }
  assert.match(levelLabel(result), /^low: Migrations, API, UI \+1$/)
  assert.equal(levelNoteLead(result).filter((line) => line.startsWith('- [ ]')).length, 4)
})

test('park level matches the shared Go contract fixture', () => {
  const rows = JSON.parse(
    readFileSync(
      new URL('../../lib/bossalib/attention/testdata/park_level.json', import.meta.url),
      'utf8',
    ),
  )
  for (const { name, codes, label, level } of rows) {
    assert.equal(parkLevel({ codes, label }), level, name)
  }
})
