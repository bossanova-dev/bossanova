import test from 'node:test'
import assert from 'node:assert/strict'
import { validateProofSpec } from './verify-proof-spec.mjs'

const HEAD = 'e'.repeat(40)
const context = { head: HEAD, changedPaths: ['src/cart.ts'], hasLine: () => true }
/** @param {Record<string, unknown>} extra */
const validate = (extra) =>
  validateProofSpec({ schema: 'proof-spec/1', head: HEAD, ...extra }, context)
/** @param {{at: string}[]} dropped */
const where = (dropped) => dropped.map((d) => d.at)

test('a spec without the review keys validates as before, with none of them set', () => {
  const { spec, dropped } = validate({ title: 'Round totals once' })
  assert.deepEqual(dropped, [])
  for (const key of ['lead', 'signals', 'bugFix', 'testing', 'guardrails', 'refactoring', 'noise'])
    assert.equal(spec && key in spec, false, key)
})

test('the lead keeps a known kind and text up to 600 characters, and drops whole otherwise', () => {
  assert.deepEqual(validate({ lead: { kind: 'bug-fix', text: 'Totals round once.' } }).spec?.lead, {
    kind: 'bug-fix',
    text: 'Totals round once.',
  })
  for (const lead of [
    { kind: 'bugfix', text: 'Totals round once.' },
    { kind: 'feature', text: 'x'.repeat(601) },
    { kind: 'feature', text: 'Totals.', extra: 1 },
  ]) {
    const { spec, dropped } = validate({ lead })
    assert.equal(spec?.lead, undefined)
    assert.deepEqual(where(dropped), ['lead'])
  }
})

test('author signals keep one valid row per key and drop unknown keys, pills and repeats', () => {
  const { spec, dropped } = validate({
    signals: [
      { key: 'tests', summary: 'New tests prove the rounding', pill: 'proven' },
      { key: 'tests', summary: 'Again', pill: 'safe' },
      { key: 'billing', summary: 'Unknown key', pill: 'safe' },
      { key: 'api', summary: 'Unknown pill', pill: 'fine' },
      { key: 'revert', summary: 'x'.repeat(161), pill: 'safe' },
      { key: 'database', summary: 'Adds one nullable column', pill: 'additive' },
    ],
  })
  assert.deepEqual(spec?.signals, [
    { key: 'tests', summary: 'New tests prove the rounding', pill: 'proven' },
    { key: 'database', summary: 'Adds one nullable column', pill: 'additive' },
  ])
  assert.deepEqual(where(dropped), ['signals[1]', 'signals[2]', 'signals[3]', 'signals[4]'])
})

test('bug-fix rows keep known labels and sources, cap at 8, and an empty account drops', () => {
  const rows = [
    { label: 'symptom', text: 'Totals were a cent out', source: 'reported' },
    { label: 'cause', text: 'Unknown label' },
    { label: 'after', text: 'The test passes', source: 'ci' },
    { label: 'before', text: 'Unknown source', source: 'guess' },
  ]
  const { spec, dropped } = validate({ bugFix: { rows } })
  assert.deepEqual(spec?.bugFix, {
    rows: [
      { label: 'symptom', text: 'Totals were a cent out', source: 'reported' },
      { label: 'after', text: 'The test passes', source: 'ci' },
    ],
  })
  assert.deepEqual(where(dropped), ['bugFix.rows[1]', 'bugFix.rows[3]'])
  const many = validate({
    bugFix: { rows: Array.from({ length: 9 }, () => ({ label: 'symptom', text: 'x' })) },
  })
  assert.equal(many.spec?.bugFix?.rows.length, 8)
  const empty = validate({ bugFix: { rows: [{ label: 'cause', text: 'x' }] } })
  assert.equal(empty.spec?.bugFix, undefined)
  assert.ok(where(empty.dropped).includes('bugFix'))
})

test('testing keeps its lists within their caps and drops a long summary alone', () => {
  const { spec, dropped } = validate({
    testing: {
      summary: 'x'.repeat(501),
      proves: Array.from({ length: 9 }, (_, i) => `proves ${i}`),
      gaps: ['No test makes the server fail'],
      also: ['a', 'b', 'c', 'd', 'e'],
    },
  })
  assert.equal(spec?.testing?.summary, undefined)
  assert.equal(spec?.testing?.proves.length, 8)
  assert.deepEqual(spec?.testing?.gaps, ['No test makes the server fail'])
  assert.equal(spec?.testing?.also.length, 4)
  assert.deepEqual(where(dropped), ['testing.summary', 'testing.proves[8]', 'testing.also[4]'])
  assert.equal(validate({ testing: { proves: [] } }).spec?.testing, undefined)
})

test('guardrail, refactoring and noise ids claim their prefix and become link targets', () => {
  const { spec, dropped } = validate({
    risks: [
      { id: 'R1', level: 'low', title: 'Rounding', impact: 'A cent.', links: ['G1', 'RF1', 'N1'] },
    ],
    guardrails: [
      {
        id: 'G1',
        kind: 'lint',
        title: 'Lint float money',
        body: 'Totals used floats.',
        proposal: 'Turn on the money lint.',
        evidence: 'Two fixes in a month.',
        links: ['R1', 'RF1', 'Z9'],
      },
      { id: 'F2', kind: 'lint', title: 'Wrong prefix', body: 'x' },
      { id: 'G2', kind: 'docs', title: 'Unknown kind', body: 'x' },
    ],
    refactoring: [{ id: 'RF1', title: 'One rounding helper', body: 'Two copies.', links: [] }],
    noise: [
      { id: 'N1', title: 'A huge comment', body: 'It cost a scroll.' },
      { id: 'N2', title: 'Links are not allowed', body: 'x', links: ['R1'] },
    ],
  })
  assert.deepEqual(spec?.risks[0].links, ['G1', 'RF1', 'N1'])
  assert.deepEqual(spec?.guardrails, [
    {
      id: 'G1',
      kind: 'lint',
      title: 'Lint float money',
      body: 'Totals used floats.',
      proposal: 'Turn on the money lint.',
      evidence: 'Two fixes in a month.',
      links: ['R1', 'RF1'],
    },
  ])
  assert.deepEqual(
    spec?.refactoring?.map((f) => f.id),
    ['RF1'],
  )
  assert.deepEqual(spec?.noise, [
    { id: 'N1', title: 'A huge comment', body: 'It cost a scroll.', links: [] },
  ])
  assert.deepEqual(where(dropped).sort(), [
    'guardrails[0].links',
    'guardrails[1]',
    'guardrails[2]',
    'noise[1]',
  ])
})

test('review items cap at five each and an id another item holds drops', () => {
  const guardrails = Array.from({ length: 6 }, (_, i) => ({
    id: `G${i + 1}`,
    kind: 'check',
    title: `Guard ${i + 1}`,
    body: 'x',
  }))
  const { spec, dropped } = validate({
    decisions: [
      { id: 'G1', question: 'q', why: 'w', suggestion: 's', at: { path: 'src/cart.ts', line: 1 } },
    ],
    guardrails,
  })
  assert.deepEqual(
    spec?.guardrails?.map((g) => g.id),
    ['G2', 'G3', 'G4', 'G5', 'G6'],
  )
  assert.deepEqual(where(dropped), ['guardrails[0]'])
})
