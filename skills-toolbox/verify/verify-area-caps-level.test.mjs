import { test } from 'node:test'
import assert from 'node:assert/strict'
import { levelConfidence, legacyCodes, levelLabel, levelNoteLead } from './verify-level.mjs'
import { decideVerdict } from './verify-gate.mjs'
import { reviewAreas } from './verify-areas.mjs'

const cap = {
  key: 'other:services/a',
  label: 'services/a',
  cappedAt: '2026-10-29T00:00:00Z',
  escapes: [
    { original: 1, fix: 3, fixMergedAt: '2026-10-29T00:00:00Z' },
    { original: 2, fix: 4, fixMergedAt: '2026-10-28T00:00:00Z' },
  ],
}
const gate = {
  changedPaths: ['services/a/code.go'],
  areas: [{ area: 'other', label: 'Other', paths: ['services/a/code.go'], defaultAsk: 'never' }],
  changedLines: 20,
  sizeBaseline: { ok: true, thresholds: { p90: 100, p99: 200 } },
  areaCaps: { caps: [cap], health: [] },
  range: { kind: 'full' },
}

test('a matching module cap lowers High and leads its label, reason and park banner', () => {
  const result = levelConfidence(gate)
  assert.equal(result.level, 'moderate')
  assert.equal(result.asks[0].source, 'cap')
  assert.match(levelLabel(result), /^moderate: services\/a \(capped\)/)
  assert.match(result.reasons[0], /services\/a.*capped.*Moderate/)
  const lead = levelNoteLead(result)
  assert.match(lead[0], /services\/a.*2026-10-29/)
  assert.match(lead[0], /#1.*#3/)
  assert.match(lead[0], /clear --area other:services\/a --reason/)
  assert.deepEqual(legacyCodes(result), ['always-human-path'])
  assert.equal(
    levelConfidence({
      ...gate,
      changedPaths: ['services/b/code.go'],
      areas: [{ ...gate.areas[0], paths: ['services/b/code.go'] }],
    }).level,
    'high',
  )
})

test('configured review filtering cannot erase a matching module cap', () => {
  for (const review of [
    { alwaysReview: [], neverReview: ['services/a/**'] },
    { alwaysReview: [{ name: 'Code', globs: ['services/a/**'] }], neverReview: [] },
  ]) {
    const { areas } = reviewAreas(gate.changedPaths, review)
    const result = levelConfidence({ ...gate, areas })
    assert.equal(result.level, 'moderate')
    assert.equal(result.asks[0].source, 'cap')
    assert.equal(result.asks[0].cap.key, cap.key)
    assert.match(levelNoteLead(result)[0], /clear --area other:services\/a/)
  }
})

test('cap asks preserve UI and configured approval codes', () => {
  for (const [key, source, code] of [
    ['ui', 'builtin', 'human-review:ui'],
    ['config:Secrets', 'config', 'human-review:security'],
  ]) {
    const result = levelConfidence({
      ...gate,
      areas: [{ area: key, label: key, defaultAsk: 'never', source, paths: ['file'] }],
      areaCaps: { caps: [{ ...cap, key, label: key }], health: [] },
    })
    assert.deepEqual(
      legacyCodes(result, {
        alwaysReview: [{ name: 'Secrets', parkCode: 'human-review:security' }],
      }),
      [code],
    )
  }
})

test('cap health is reported without lowering the head by itself', () => {
  for (const health of ['area-caps-unknown', 'area-caps-stale']) {
    const result = levelConfidence({ ...gate, areaCaps: { caps: [], health: [health] } })
    assert.equal(result.level, 'high')
    assert.ok(result.health.includes(health))
  }
})

test('a cap replaces the same area ask instead of counting it twice toward Low', () => {
  const result = levelConfidence({
    ...gate,
    changedPaths: ['src/View.tsx', 'migrations/001.sql', 'infra/main.tf'],
    areas: ['ui', 'migrations', 'infra'].map((area) => ({
      area,
      label: area,
      defaultAsk: 'always',
      paths: [],
    })),
    areaCaps: { caps: [{ ...cap, key: 'ui', label: 'UI' }], health: [] },
  })
  assert.equal(result.level, 'moderate')
  assert.equal(result.asks.length, 3)
  assert.equal(result.asks[0].source, 'cap')
  assert.equal(
    result.asks.some((ask) => ask.area === 'ui'),
    false,
  )
})

test('a capped High head parks and its ordinary approval passes as approved', () => {
  const facts = {
    ...gate,
    judgeAreas: [],
    prState: 'OPEN',
    isDraft: false,
    headSha: 'a'.repeat(40),
    ci: { settled: true, green: true, state: 'green' },
    receipt: 'success',
    extensions: [],
  }
  const parked = decideVerdict(facts)
  assert.equal(parked.verdict, 'human')
  assert.equal(parked.confidence.level, 'moderate')
  const approved = decideVerdict({ ...facts, waive: parked.reason })
  assert.equal(approved.verdict, 'pass')
  assert.equal(approved.reason, 'approved')
})
