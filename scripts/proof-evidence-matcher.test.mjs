import test from 'node:test'
import assert from 'node:assert/strict'
import {
  MATCH_MODES,
  normalizeWhitespace,
  normalizeExpectation,
  matchExpectation,
  displayText,
  evaluateExpectations,
} from './proof-evidence-matcher.mjs'

test('normalizeWhitespace collapses runs, newlines and trims', () => {
  assert.equal(normalizeWhitespace('  Archive\n   delay:  45m  '), 'Archive delay: 45m')
})

test('bare string canonicalizes to normalized mode', () => {
  assert.deepEqual(normalizeExpectation('Archive delay'), {
    text: 'Archive delay',
    match: 'normalized',
  })
})

test('normalizeExpectation rejects bad shapes with reasons', () => {
  assert.throws(() => normalizeExpectation({ text: 'x', match: 'fuzzy' }), /unknown match mode/)
  assert.throws(() => normalizeExpectation({ match: 'literal' }), /text or anyOf/)
  assert.throws(() => normalizeExpectation({ text: '[', match: 'regex' }), /invalid regex/)
  assert.throws(() => normalizeExpectation({ text: 'x', label: '' }), /label/)
  assert.throws(() => normalizeExpectation({ anyOf: [] }), /anyOf/)
  assert.throws(() => normalizeExpectation({ text: 'x', anyOf: ['y'] }), /text or anyOf/)
})

test('normalized matching forgives wrap/pad but stays case-sensitive', () => {
  const screen = 'Archive\n    delay: 45m'
  assert.equal(matchExpectation(screen, normalizeExpectation('Archive delay: 45m')), true)
  assert.equal(matchExpectation(screen, normalizeExpectation('archive delay: 45m')), false)
})

test('normalized-ci is the explicit case-insensitive opt-in', () => {
  const exp = normalizeExpectation({ text: 'ARCHIVE DELAY', match: 'normalized-ci' })
  assert.equal(matchExpectation('archive\n delay', exp), true)
})

test('literal matching is exact substring', () => {
  const exp = normalizeExpectation({ text: 'delay:  45m', match: 'literal' })
  assert.equal(matchExpectation('delay:  45m', exp), true)
  assert.equal(matchExpectation('delay: 45m', exp), false)
})

test('regex matches against RAW screen text with /u', () => {
  const exp = normalizeExpectation({ text: 'Archive delay:\\s+\\d+m', match: 'regex' })
  assert.equal(matchExpectation('Archive delay:   45m', exp), true)
  assert.equal(matchExpectation('Archive delay: none', exp), false)
})

test('anyOf passes when any alternative matches', () => {
  const exp = normalizeExpectation({ anyOf: ['45m', { text: '45 minutes', match: 'literal' }] })
  assert.equal(matchExpectation('shows 45 minutes', exp), true)
  assert.equal(matchExpectation('shows nothing', exp), false)
})

test('displayText prefers label, falls back to text, joins anyOf', () => {
  assert.equal(displayText(normalizeExpectation({ text: 'x', label: 'the X' })), 'the X')
  assert.equal(displayText(normalizeExpectation('plain')), 'plain')
  assert.equal(displayText(normalizeExpectation({ anyOf: ['a', 'b'] })), 'a | b')
})

test('MATCH_MODES is the canonical mode list', () => {
  assert.deepEqual(MATCH_MODES, ['literal', 'normalized', 'normalized-ci', 'regex'])
})

test('evaluateExpectations: evidence on early, middle or final screen all pass', () => {
  const expectations = ['alpha', 'beta', 'gamma'].map((t) => normalizeExpectation(t))
  const r = evaluateExpectations({
    expectations,
    texts: ['has alpha here', 'now beta', 'finally gamma'],
  })
  assert.equal(r.passed, true)
  assert.deepEqual(r.missing, [])
  assert.equal(r.lastText, 'finally gamma')
})

test('evaluateExpectations: failure lists missing with displayText and embeds final screen', () => {
  const expectations = [
    normalizeExpectation('present'),
    normalizeExpectation({ text: 'absent thing', label: 'the thing' }),
  ]
  const r = evaluateExpectations({ expectations, texts: ['present here', 'final screen'] })
  assert.equal(r.passed, false)
  assert.equal(r.missing.length, 1)
  assert.equal(r.missing[0].displayText, 'the thing')
  assert.equal(r.lastText, 'final screen')
})

test('evaluateExpectations: no screens → fail with lastText null', () => {
  const r = evaluateExpectations({ expectations: [normalizeExpectation('x')], texts: [] })
  assert.equal(r.passed, false)
  assert.equal(r.lastText, null)
})

test('evaluateExpectations: zero expectations pass vacuously', () => {
  const r = evaluateExpectations({ expectations: [], texts: ['anything'] })
  assert.equal(r.passed, true)
})
