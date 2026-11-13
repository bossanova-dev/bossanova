import test from 'node:test'
import assert from 'node:assert/strict'
import { validateProofSpec, ticketVerdict, MAX_SPEC_RISKS } from './verify-proof-spec.mjs'

const HEAD = 'c'.repeat(40)
// The diff shows lines 8 to 20 of `src/cart.ts` and the whole of `src/cart.test.ts`.
const context = {
  head: HEAD,
  changedPaths: ['src/cart.ts', 'src/cart.test.ts'],
  areaIds: ['ui', 'logic'],
  hasLine: (path, line) =>
    path === 'src/cart.ts' ? (line >= 8 && line <= 20) || null : line <= 30,
  checks: ['unit'],
  acceptance: [
    { id: 'AC9', result: 'met', head: HEAD },
    { id: 'AC8', result: 'met', head: 'd'.repeat(40) },
  ],
  findingIds: ['review:f1'],
}

function spec() {
  return {
    schema: 'proof-spec/1',
    head: HEAD,
    title: 'Show the basket total beside the checkout button',
    waiting: [
      { title: 'The total changed', detail: 'Rounding moved to the server.', area: 'logic' },
    ],
    reason: 'One rounding change in `src/cart.ts`, and a test pins the new total.',
    risks: [
      {
        id: 'R1',
        level: 'medium',
        title: 'Rounding moved',
        impact: 'Totals now round once, so a three-item basket can differ by a cent.',
        guard: 'A test pins the three-item total.',
        at: { path: 'src/cart.ts', line: 12 },
        links: ['D1', 'AC1', 'review:f1'],
      },
    ],
    decisions: [
      {
        id: 'D1',
        question: 'Is a one-cent change on old baskets acceptable?',
        why: 'Saved baskets reprice on their next view.',
        suggestion: 'Yes; nothing stores the old total.',
        at: { path: 'src/cart.ts', line: 12 },
        links: ['R1'],
      },
    ],
    ticketFit: {
      summary: 'The total shows, and only its rounding is tested.',
      criteria: [
        {
          id: 'AC1',
          text: 'Totals round once',
          status: 'done',
          evidence: [{ kind: 'test', label: 'rounds once', ref: 'src/cart.test.ts:4' }],
        },
        {
          id: 'AC2',
          text: 'The total shows beside the button',
          status: 'unverified',
          evidence: [{ kind: 'code', label: 'total', ref: 'src/cart.ts:14' }],
        },
      ],
      skipped: [{ text: 'Discounts', why: 'A later ticket', followUp: 'SHOP-9' }],
      contradictions: [],
    },
    areas: [
      {
        id: 'logic',
        name: 'Logic',
        summary: 'Totals round once.',
        facts: [{ text: 'One rounding step', flag: false }],
        snippets: [{ path: 'src/cart.ts', start: 10, end: 16, pinLine: 12, pinNote: 'Here.' }],
      },
    ],
    checklist: { after: [{ text: 'Watch totals', done: true }] },
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
  const expected = spec()
  expected.ticketFit = { verdict: 'partial', ...expected.ticketFit }
  assert.deepEqual(kept, expected)
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
    [
      'risk on a line the file does not have',
      (s) => (s.risks[0].at = { path: 'src/cart.test.ts', line: 40 }),
      'risks[0]',
      (k) => k.risks.length,
    ],
    [
      'decision line outside the diff',
      (s) => (s.decisions[0].at.line = 30),
      'decisions[0]',
      (k) => k.decisions.length,
    ],
    [
      'decision without a suggestion',
      (s) => delete s.decisions[0].suggestion,
      'decisions[0]',
      (k) => k.decisions.length,
    ],
    [
      'criterion status outside the set',
      (s) => (s.ticketFit.criteria[1].status = 'maybe'),
      'ticketFit.criteria[1]',
      (k) => k.ticketFit.criteria.length - 1,
    ],
    [
      'risk text too long',
      (s) => (s.risks[0].impact = 'x'.repeat(301)),
      'risks[0]',
      (k) => k.risks.length,
    ],
    [
      'risk with the retired body key',
      (s) => (s.risks[0].body = 'Totals round once.'),
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
      (s) => (s.checklist.after[0].done = 'yes'),
      'checklist.after[0]',
      (k) => k.checklist.after.length,
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
    // A dropped item's incoming links go with it.
    assert.deepEqual(
      dropped.filter((d) => !d.at.endsWith('.links')).map((d) => d.at),
      [at],
      name,
    )
    assert.ok(dropped[0].reason.length > 0, name)
    assert.equal(count(kept), 0, name)
    assert.equal(kept.title, spec().title, `${name} keeps the rest`)
  }
})

test('a risk with no location is kept: not every failure has one line', () => {
  const raw = spec()
  delete raw.risks[0].at
  const { spec: kept, dropped } = validateProofSpec(raw, context)
  assert.deepEqual(dropped, [])
  assert.equal(kept.risks[0].at, undefined)
})

test('a reason in pipeline words, or longer than one sentence, is rejected', () => {
  for (const reason of [
    'Verify parked it because the API contract area asks for a person.',
    'The judge found nothing.',
    'Every path is out of review by policy.',
    'It touches a neverReview path.',
    'Totals round once. A test pins them.',
  ]) {
    const raw = { ...spec(), reason }
    const { spec: kept, dropped } = validateProofSpec(raw, context)
    assert.equal(kept.reason, undefined, reason)
    assert.deepEqual(
      dropped.map((d) => d.at),
      ['spec.reason'],
      reason,
    )
    assert.equal(kept.risks.length, 1, 'the rest of the spec stays')
  }
  assert.match(
    validateProofSpec({ ...spec(), reason: 'The judge found nothing.' }, context).dropped[0].reason,
    /"judge"/,
  )
})

test('a done criterion whose every ref dangles reads as unverified, and the dangling refs go', () => {
  const dangling = [
    { kind: 'test', label: 'gone', ref: 'src/missing.test.ts' },
    { kind: 'test', label: 'not a test', ref: 'src/cart.ts:12' },
    { kind: 'ci', label: 'e2e', ref: 'e2e' },
    { kind: 'acceptance', label: 'an earlier head', ref: 'AC8' },
    { kind: 'finding', label: 'invented', ref: 'review:nope' },
  ]
  for (const ev of dangling) {
    const raw = spec()
    raw.ticketFit.criteria[0].evidence = [ev]
    const { spec: kept, dropped } = validateProofSpec(raw, context)
    const [criterion] = kept.ticketFit.criteria
    assert.equal(criterion.status, 'unverified', ev.ref)
    assert.deepEqual(criterion.evidence, [], `${ev.ref} is not shown`)
    assert.deepEqual(
      dropped.map((d) => d.at),
      ['ticketFit.criteria[0].evidence[0]', 'ticketFit.criteria[0].status'],
      ev.ref,
    )
  }
})

test('each kind of resolving ref keeps a criterion done; code alone never does', () => {
  for (const ev of [
    { kind: 'test', label: 'rounds once', ref: 'src/cart.test.ts' },
    { kind: 'ci', label: 'unit', ref: 'unit' },
    { kind: 'acceptance', label: 'AC9', ref: 'AC9' },
    { kind: 'finding', label: 'fixed in review', ref: 'review:f1' },
  ]) {
    const raw = spec()
    raw.ticketFit.criteria[0].evidence = [ev]
    const { spec: kept, dropped } = validateProofSpec(raw, context)
    assert.equal(kept.ticketFit.criteria[0].status, 'done', ev.kind)
    assert.deepEqual(dropped, [], ev.kind)
  }
  const raw = spec()
  raw.ticketFit.criteria[0].evidence = [{ kind: 'code', label: 'total', ref: 'src/cart.ts:12' }]
  const { spec: kept } = validateProofSpec(raw, context)
  assert.equal(kept.ticketFit.criteria[0].status, 'unverified')
  assert.equal(kept.ticketFit.criteria[0].evidence.length, 1, 'the code ref still shows')
})

test('the ticket verdict comes from the criteria, never from the author', () => {
  const statuses = (/** @type {string[]} */ list) => list.map((status) => ({ status }))
  assert.equal(ticketVerdict(statuses(['done', 'done'])), 'complete')
  assert.equal(ticketVerdict(statuses(['done', 'skipped'])), 'partial')
  assert.equal(ticketVerdict(statuses(['unverified', 'missed'])), 'incomplete')
  assert.equal(ticketVerdict([]), null)
  const raw = spec()
  raw.ticketFit.verdict = 'complete'
  const { spec: kept, dropped } = validateProofSpec(raw, context)
  assert.equal(kept.ticketFit.verdict, 'partial', 'one criterion is unverified')
  assert.deepEqual(
    dropped.map((d) => d.at),
    ['ticketFit.verdict'],
  )
  raw.ticketFit.criteria[1].status = 'done'
  raw.ticketFit.criteria[1].evidence.push({ kind: 'ci', label: 'unit', ref: 'unit' })
  assert.equal(validateProofSpec(raw, context).spec.ticketFit.verdict, 'complete')
})

test('a link that names nothing kept is dropped, including one to a dropped decision', () => {
  const raw = spec()
  raw.risks[0].links = ['D1', 'D7', 'R1', 'review:nope', 'AC2']
  raw.decisions[0].at.line = 40
  const { spec: kept, dropped } = validateProofSpec(raw, context)
  assert.deepEqual(kept.risks[0].links, ['AC2'])
  assert.deepEqual(
    dropped.map((d) => [d.at, d.reason]),
    [
      ['decisions[0]', 'at: src/cart.ts:40 is not a line the diff shows'],
      ['risks[0].links', 'D1 names nothing on this page'],
      ['risks[0].links', 'D7 names nothing on this page'],
      ['risks[0].links', 'R1 names nothing on this page'],
      ['risks[0].links', 'review:nope names nothing on this page'],
    ],
  )
})

test('an id two items claim stays with the first', () => {
  const raw = spec()
  raw.decisions[0].id = 'R1'
  const { spec: kept, dropped } = validateProofSpec(raw, context)
  assert.equal(kept.decisions.length, 0)
  assert.deepEqual(dropped[0], { at: 'decisions[0]', reason: 'id: R1 appears twice' })
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
  raw.risks = Array.from({ length: MAX_SPEC_RISKS + 2 }, (_, i) => ({
    ...spec().risks[0],
    id: `R${i + 1}`,
  }))
  const { spec: kept, dropped } = validateProofSpec(raw, context)
  assert.equal(kept.risks.length, MAX_SPEC_RISKS)
  assert.deepEqual(
    dropped.map((d) => d.at),
    ['risks[12]', 'risks[13]'],
  )
})

test('without the changed files a located risk cannot be checked and is dropped', () => {
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
    'reason',
    'waiting',
    'risks',
    'decisions',
    'ticketFit',
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
    raw.risks[0].links = value
    raw.decisions[0].at = value
    raw.ticketFit.criteria[0].evidence = [value]
    raw.ticketFit.skipped = [value]
    raw.checklist.after = value
    assert.doesNotThrow(() => validateProofSpec(raw, context))
  }
})

test('a test ref must name a test file the PR adds lines to', () => {
  for (const [ref, extra] of [
    [
      'e2e/fixtures/seed.json',
      { changedPaths: [...context.changedPaths, 'e2e/fixtures/seed.json'] },
    ],
    ['src/cart.test.ts', { addedPaths: ['src/cart.ts'] }],
  ]) {
    const raw = spec()
    raw.ticketFit.criteria[0].evidence = [{ kind: 'test', label: 'x', ref }]
    const { spec: kept, dropped } = validateProofSpec(raw, { ...context, ...extra })
    assert.equal(kept.ticketFit.criteria[0].status, 'unverified', ref)
    assert.equal(dropped[0].at, 'ticketFit.criteria[0].evidence[0]', ref)
  }
})

test('a dropped criterion frees its id, and a long summary drops alone', () => {
  const raw = spec()
  raw.ticketFit.criteria[0].text = ''
  raw.ticketFit.criteria.push({ ...spec().ticketFit.criteria[0] })
  raw.ticketFit.summary = 'x'.repeat(301)
  const { spec: kept, dropped } = validateProofSpec(raw, context)
  assert.deepEqual(
    kept.ticketFit.criteria.map((c) => c.id),
    ['AC2', 'AC1'],
  )
  assert.equal(kept.ticketFit.summary, undefined)
  assert.deepEqual(
    dropped.map((d) => d.at),
    ['ticketFit.criteria[0]', 'ticketFit.summary'],
  )
})

test('a link that is not text drops alone, and ids that share an anchor clash', () => {
  const raw = spec()
  raw.risks[0].links = ['D1', 7]
  raw.decisions[0].id = 'r1'
  const { spec: kept, dropped } = validateProofSpec(raw, context)
  assert.equal(kept.risks.length, 1, 'the risk stays')
  assert.deepEqual(kept.risks[0].links, [], 'D1 went with its decision')
  assert.equal(kept.decisions.length, 0, 'r1 would share R1 its anchor')
  assert.deepEqual(
    dropped.map((d) => d.at),
    ['risks[0].links[1]', 'decisions[0]', 'risks[0].links'],
  )
})

test('sign-off is a pipeline word too', () => {
  const { spec: kept } = validateProofSpec(
    { ...spec(), reason: 'Every changed path needs no sign-off, and CI is green.' },
    context,
  )
  assert.equal(kept.reason, undefined)
})

/** A spec filling the declared `infra` section with one part of each kind. */
function sectionSpec(parts, more = {}) {
  return {
    schema: 'proof-spec/1',
    head: HEAD,
    risks: [{ id: 'R1', level: 'low', title: 'Texts wait', impact: 'x', links: ['I1'] }],
    sections: [{ id: 'infra', sub: 'nothing before deploy', parts }],
    ...more,
  }
}
const CHECKLIST = {
  kind: 'checklist',
  numbered: true,
  items: [
    {
      id: 'I1',
      group: 'Deploy',
      context: 'Admin, production',
      text: 'Deploy as usual.',
      expect: 'The pods restart.',
      tag: 'production',
      tone: 'success',
      links: ['R1'],
    },
  ],
}
const TABLE = {
  kind: 'table',
  title: 'Settings',
  note: 'set in `tfvars`',
  columns: ['Setting', 'Production'],
  rows: [
    {
      cells: [
        { text: 'SMS_FROM', code: true, note: 'The number' },
        { text: 'set', pill: 'success' },
      ],
    },
  ],
}
const FACTS = {
  kind: 'facts',
  title: 'If missing',
  rows: [{ label: 'API start', text: 'Unaffected.', tone: 'warning' }],
}
const declared = { ...context, sections: ['infra', 'testing'] }

test('a section keeps every part kind whole, and its items link both ways', () => {
  const { spec: kept, dropped } = validateProofSpec(
    sectionSpec([CHECKLIST, TABLE, FACTS]),
    declared,
  )
  assert.deepEqual(dropped, [])
  assert.deepEqual(kept.sections, [
    {
      id: 'infra',
      sub: 'nothing before deploy',
      parts: [CHECKLIST, { ...TABLE, rows: [{ cells: TABLE.rows[0].cells }] }, FACTS],
    },
  ])
  assert.deepEqual(kept.risks[0].links, ['I1'], 'a risk links to a section item')
})

test('a section the repository does not declare, or fills twice, is dropped and listed', () => {
  const value = sectionSpec([FACTS], { risks: [] })
  value.sections.push({ id: 'infra', parts: [FACTS] }, { id: 'deploy', parts: [FACTS] })
  const { spec: kept, dropped } = validateProofSpec(value, declared)
  assert.deepEqual(
    kept.sections.map((s) => s.id),
    ['infra'],
  )
  assert.deepEqual(dropped, [
    { at: 'sections[1]', reason: 'id: infra appears twice' },
    { at: 'sections[2]', reason: 'id: deploy is not a section the repository declares' },
  ])
  const undeclared = validateProofSpec(sectionSpec([FACTS], { risks: [] }), context)
  assert.equal(undeclared.spec.sections, undefined, 'no declarations, no sections')
  assert.deepEqual(undeclared.dropped, [
    { at: 'sections[0]', reason: 'id: infra is not a section the repository declares' },
  ])
})

test('each section rule drops only its defective part, row or item and names why', () => {
  const long = 'x'.repeat(301)
  const parts = [
    {
      ...CHECKLIST,
      items: [...CHECKLIST.items, { id: 'I2', text: long }, { id: 'R1', text: 'ok' }],
    },
    {
      ...TABLE,
      rows: [...TABLE.rows, { cells: ['only one'] }, { cells: ['a', { text: 'b', pill: 'pink' }] }],
    },
    { ...FACTS, rows: [...FACTS.rows, { label: 'l'.repeat(41), text: 't' }] },
    { kind: 'chart', rows: [] },
    { ...FACTS, extra: true },
    { kind: 'table', columns: ['1', '2', '3', '4', '5', '6', '7'], rows: [] },
    { kind: 'checklist', items: [] },
  ]
  const { spec: kept, dropped } = validateProofSpec(sectionSpec(parts), declared)
  assert.equal(kept.sections[0].parts.length, 3)
  assert.deepEqual(dropped, [
    { at: 'sections[0].parts[0].items[1]', reason: 'text: longer than 300 characters' },
    { at: 'sections[0].parts[0].items[2]', reason: 'id: R1 appears twice' },
    { at: 'sections[0].parts[1].rows[1]', reason: 'cells: expected 2, one per column' },
    {
      at: 'sections[0].parts[1].rows[2]',
      reason: 'cells.pill: expected one of success, warning, danger, info, neutral',
    },
    { at: 'sections[0].parts[2].rows[1]', reason: 'label: longer than 40 characters' },
    { at: 'sections[0].parts[3]', reason: 'kind: expected one of checklist, table, facts' },
    { at: 'sections[0].parts[4]', reason: 'extra: unknown key' },
    { at: 'sections[0].parts[5]', reason: 'columns: expected 1 to 6 labels' },
    { at: 'sections[0].parts[6]', reason: 'items: nothing to show' },
  ])
})

test('a section caps its parts at 8 and its items at 20, and a sub at 80 characters', () => {
  const items = Array.from({ length: 21 }, (_, i) => ({ id: `I${i + 1}`, text: 'step' }))
  const parts = [{ kind: 'checklist', items }, ...Array.from({ length: 8 }, () => FACTS)]
  const value = sectionSpec(parts, { risks: [] })
  const { spec: kept, dropped } = validateProofSpec(value, declared)
  assert.equal(kept.sections[0].parts.length, 8)
  assert.equal(kept.sections[0].parts[0].items.length, 20)
  assert.deepEqual(dropped, [
    { at: 'sections[0].parts[0].items[20]', reason: 'over the cap of 20' },
    { at: 'sections[0].parts[8]', reason: 'over the cap of 8' },
  ])
  const sub = sectionSpec([FACTS], { risks: [] })
  sub.sections[0].sub = 's'.repeat(81)
  assert.deepEqual(validateProofSpec(sub, declared).dropped, [
    { at: 'sections[0]', reason: 'sub: longer than 80 characters' },
  ])
})

test('a section left with no part is dropped, and a link to its items goes with it', () => {
  const value = sectionSpec([{ kind: 'chart' }], {
    risks: [{ id: 'R1', level: 'low', title: 't', impact: 'i', links: ['I1'] }],
  })
  const { spec: kept, dropped } = validateProofSpec(value, declared)
  assert.equal(kept.sections, undefined)
  assert.deepEqual(dropped, [
    { at: 'sections[0].parts[0]', reason: 'kind: expected one of checklist, table, facts' },
    { at: 'sections[0]', reason: 'parts: nothing to show' },
    { at: 'risks[0].links', reason: 'I1 names nothing on this page' },
  ])
})
