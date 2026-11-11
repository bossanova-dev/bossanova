import test from 'node:test'
import assert from 'node:assert/strict'
import { validateProofSpec, MAX_SPEC_RISKS } from './verify-proof-spec.mjs'

const HEAD = 'c'.repeat(40)
const context = {
  head: HEAD,
  changedPaths: ['src/cart.ts', 'src/cart.test.ts'],
  areaIds: ['ui', 'logic'],
}

function spec() {
  return {
    schema: 'proof-spec/1',
    head: HEAD,
    title: 'Show the basket total beside the checkout button',
    waiting: [
      { title: 'The total changed', detail: 'Rounding moved to the server.', area: 'logic' },
    ],
    risks: [
      {
        level: 'medium',
        title: 'Rounding moved',
        body: 'Totals now round once.',
        ifItMatters: 'Compare a three-item basket.',
        label: 'Logic',
        at: { path: 'src/cart.ts', line: 12 },
      },
    ],
    areas: [
      {
        id: 'logic',
        name: 'Logic',
        summary: 'Totals round once.',
        facts: [{ text: 'One rounding step', flag: false }],
        snippets: [{ path: 'src/cart.ts', start: 10, end: 16, pinLine: 12, pinNote: 'Here.' }],
      },
    ],
    checklist: {
      before: [{ text: 'Try a basket', done: false }],
      after: [{ text: 'Watch totals', done: true }],
    },
    notes: [{ severity: 'P3', at: 'src/cart.ts:12', text: 'Name the constant.' }],
    evidencePlan: [
      { claim: 'The total shows', kind: 'still', how: 'Open the basket.' },
      { claim: 'The statement lists fees', kind: 'document', how: 'Render the statement.' },
    ],
    uncertain: ['Whether discounts round too'],
  }
}

test('a valid spec is kept whole with nothing dropped', () => {
  const { spec: kept, dropped } = validateProofSpec(spec(), context)
  assert.deepEqual(dropped, [])
  assert.deepEqual(kept, spec())
})

test('each rule drops only its defective item and names why', () => {
  /** @type {[string, (s: any) => void, string, (k: any) => unknown][]} */
  const cases = [
    [
      'risk cites an unchanged file',
      (s) => (s.risks[0].at.path = 'src/other.ts'),
      'risks[0]',
      (k) => k.risks.length,
    ],
    [
      'risk level outside the set',
      (s) => (s.risks[0].level = 'critical'),
      'risks[0]',
      (k) => k.risks.length,
    ],
    ['risk without a location', (s) => delete s.risks[0].at, 'risks[0]', (k) => k.risks.length],
    [
      'risk text too long',
      (s) => (s.risks[0].body = 'x'.repeat(301)),
      'risks[0]',
      (k) => k.risks.length,
    ],
    [
      'risk label over 40 characters',
      (s) => (s.risks[0].label = 'x'.repeat(41)),
      'risks[0]',
      (k) => k.risks.length,
    ],
    [
      'risk with the retired area key',
      (s) => (s.risks[0].area = 'Logic'),
      'risks[0]',
      (k) => k.risks.length,
    ],
    [
      'risk with an unknown key',
      (s) => (s.risks[0].severity = 'high'),
      'risks[0]',
      (k) => k.risks.length,
    ],
    ['area not judged', (s) => (s.areas[0].id = 'invented'), 'areas[0]', (k) => k.areas.length],
    [
      'fact longer than 90',
      (s) => (s.areas[0].facts[0].text = 'x'.repeat(91)),
      'areas[0].facts[0]',
      (k) => k.areas[0].facts.length,
    ],
    [
      'snippet under 4 lines',
      (s) => (s.areas[0].snippets[0].end = 12),
      'areas[0].snippets[0]',
      (k) => k.areas[0].snippets.length,
    ],
    [
      'snippet over 16 lines',
      (s) => (s.areas[0].snippets[0].end = 26),
      'areas[0].snippets[0]',
      (k) => k.areas[0].snippets.length,
    ],
    [
      'snippet on an unchanged file',
      (s) => (s.areas[0].snippets[0].path = 'README.md'),
      'areas[0].snippets[0]',
      (k) => k.areas[0].snippets.length,
    ],
    [
      'snippet line not a number',
      (s) => (s.areas[0].snippets[0].start = '10'),
      'areas[0].snippets[0]',
      (k) => k.areas[0].snippets.length,
    ],
    [
      'waiting reason for an unknown area',
      (s) => (s.waiting[0].area = 'nowhere'),
      'waiting[0]',
      (k) => k.waiting.length,
    ],
    [
      'checklist done not boolean',
      (s) => (s.checklist.before[0].done = 'yes'),
      'checklist.before[0]',
      (k) => k.checklist.before.length,
    ],
    [
      'evidence kind outside the set',
      (s) => (s.evidencePlan[0].kind = 'gif'),
      'evidencePlan[0]',
      (k) => k.evidencePlan.length - 1,
    ],
    ['note without text', (s) => (s.notes[0].text = ''), 'notes[0]', (k) => k.notes.length],
    [
      'uncertain entry not text',
      (s) => (s.uncertain[0] = 3),
      'uncertain[0]',
      (k) => k.uncertain.length,
    ],
  ]
  for (const [name, mutate, at, count] of cases) {
    const raw = spec()
    mutate(raw)
    const { spec: kept, dropped } = validateProofSpec(raw, context)
    assert.deepEqual(
      dropped.map((d) => d.at),
      [at],
      name,
    )
    assert.ok(dropped[0].reason.length > 0, name)
    assert.equal(count(kept), 0, name)
    assert.equal(kept.title, spec().title, `${name} keeps the rest`)
  }
})

test('a title over 110 characters is dropped and the page keeps the rest', () => {
  const raw = spec()
  raw.title = 'x'.repeat(111)
  const { spec: kept, dropped } = validateProofSpec(raw, context)
  assert.deepEqual(
    dropped.map((d) => d.at),
    ['spec.title'],
  )
  assert.equal(kept.title, undefined)
  assert.equal(kept.risks.length, 1)
})

test('risks over the cap are dropped from the thirteenth on', () => {
  const raw = spec()
  raw.risks = Array.from({ length: MAX_SPEC_RISKS + 2 }, () => spec().risks[0])
  const { spec: kept, dropped } = validateProofSpec(raw, context)
  assert.equal(kept.risks.length, MAX_SPEC_RISKS)
  assert.deepEqual(
    dropped.map((d) => d.at),
    ['risks[12]', 'risks[13]'],
  )
})

test('without the changed files a risk cannot be checked and is dropped', () => {
  const { spec: kept, dropped } = validateProofSpec(spec(), { head: HEAD })
  assert.equal(kept.risks.length, 0)
  assert.match(dropped[0].reason, /changed files are unknown/)
})

test('a spec for another head or of another schema is ignored whole', () => {
  for (const raw of [
    { ...spec(), head: 'd'.repeat(40) },
    { ...spec(), schema: 'proof-spec/2' },
    null,
    [],
    'spec',
  ]) {
    const { spec: kept, dropped } = validateProofSpec(raw, context)
    assert.equal(kept, null)
    assert.equal(dropped.length, 1)
  }
})

test('garbage agent output never throws', () => {
  const junk = [undefined, null, 0, 'x', [], {}, { a: 1 }, [null], { text: {} }]
  for (const key of [
    'title',
    'waiting',
    'risks',
    'areas',
    'checklist',
    'notes',
    'evidencePlan',
    'uncertain',
  ])
    for (const value of junk) {
      const raw = { ...spec(), [key]: value }
      assert.doesNotThrow(() => validateProofSpec(raw, context), `${key}=${JSON.stringify(value)}`)
    }
  for (const value of junk) {
    const raw = spec()
    raw.areas[0].snippets = [value]
    raw.areas[0].facts = [value]
    raw.risks[0].at = value
    raw.checklist.before = value
    assert.doesNotThrow(() => validateProofSpec(raw, context))
  }
})
