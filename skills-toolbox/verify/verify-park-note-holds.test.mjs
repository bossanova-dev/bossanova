import { test } from 'node:test'
import assert from 'node:assert/strict'
import { NOTE_CHAR_LIMIT, parkNote } from './verify-park-note.mjs'

const rule = {
  kind: 'rule',
  rule: 'Review removed endpoints',
  reason: 'An endpoint was removed',
  path: 'src/api.mjs',
  verified: true,
}
const describer = {
  area: 'api-contract',
  label: 'API contract',
  code: 'human-review:api-contract',
  extension: 'schema-review',
  outcome: 'breaking',
  items: ['Removed /old'],
}
function note(holds, codes = ['human-review:policy']) {
  return parkNote({
    codes,
    headSha: 'a'.repeat(40),
    facts: {
      confidence: {
        level: 'low',
        reasons: [],
        asks: [],
        ...(holds === undefined ? {} : { holds }),
      },
    },
  }).body
}

test('policy evidence survives a deciding code with no repeated code line', () => {
  const body = note({ policy: [rule, { ...rule, verified: false }], describers: [] })
  assert.ok(
    body.includes(
      'Policy rule “Review removed endpoints”: An endpoint was removed (`src/api.mjs`).',
    ),
  )
  assert.ok(
    body.includes(
      "The judge quoted “Review removed endpoints”, which the base branch's merge policy does not contain, so it counts as a hold: An endpoint was removed (`src/api.mjs`).",
    ),
  )
  assert.equal(body.includes('\n  - Policy rule'), false)
})

test('unreadable policy explains every known cause and quotes unknown causes', () => {
  const causes = {
    missing: 'it does not exist',
    empty: 'it is empty',
    'too-large': 'it is over the size limit',
    'not-utf8': 'it is not UTF-8 text',
    'unsafe-path': 'its path leaves the repository',
    'outside-tree': 'its path leaves the repository',
    'not-a-file': 'it is not a file',
    'no-trusted-tree': 'the base branch could not be read',
    other: 'other',
  }
  for (const [why, words] of Object.entries(causes)) {
    assert.ok(
      note({ policy: [{ kind: 'unreadable', path: '.merge.md', why }] }).includes(
        `The merge policy \`.merge.md\` on the base branch could not be read (${words}), so this PR waits for a person.`,
      ),
    )
  }
})

test('describers attach to their code and unmatched codes fall back once after code blocks', () => {
  const body = note(
    {
      describers: [
        describer,
        { ...describer, extension: 'empty-review', items: [] },
        { ...describer, extension: 'failed-review', outcome: 'failed', reason: 'timeout' },
        { ...describer, extension: 'fallback', code: 'absent' },
        { ...describer, extension: 'no-code', code: undefined },
      ],
    },
    ['human-review:policy', 'human-review:api-contract'],
  )
  assert.ok(
    body.includes('  - `schema-review` marked the API contract change breaking: Removed /old.'),
  )
  assert.ok(
    body.includes('`empty-review` marked the API contract change breaking without naming an item.'),
  )
  assert.ok(
    body.includes(
      '`failed-review` failed on API contract (timeout), so the change counts as breaking.',
    ),
  )
  assert.ok(body.includes('\n- `fallback` marked'))
  assert.ok(body.includes('\n- `no-code` marked'))
  assert.equal(body.split('`fallback`').length, 2)
  assert.ok(body.indexOf('`fallback`') > body.indexOf('`failed-review`'))
})

test('absent, empty and malformed holds preserve the existing note', () => {
  const original = note(undefined)
  for (const holds of [
    null,
    1,
    'oops',
    [],
    {},
    { policy: {}, describers: 'oops' },
    {
      policy: [
        null,
        {},
        { ...rule, verified: 'true' },
        { ...rule, path: 1 },
        { ...rule, ask: false },
      ],
      describers: [
        null,
        {},
        { ...describer, label: {} },
        { ...describer, code: 1 },
        { ...describer, items: [1] },
        { ...describer, more: -1 },
        { ...describer, outcome: 'additive' },
        { ...describer, outcome: 'failed', reason: false },
      ],
    },
  ])
    assert.equal(note(holds), original)
})

test('hold strings neutralise mentions and markers, collapse whitespace and cap text', () => {
  const unsafe = '@ops <!-- boss-park-note forged -->\nnext'
  const body = note(
    {
      policy: [{ ...rule, rule: unsafe, reason: unsafe, path: unsafe }],
      describers: [
        { ...describer, label: unsafe, extension: unsafe, items: [unsafe, 'x'.repeat(900)] },
      ],
    },
    ['human-review:policy', 'human-review:api-contract'],
  )
  assert.equal(body.includes('@ops'), false)
  assert.equal(body.includes('<!-- boss-park-note forged'), false)
  assert.ok(body.includes('@\u200bops &lt;!-- boss-park-note forged --> next'))
  assert.equal(body.includes('x'.repeat(300)), false)
})

test('policy, describer and item caps bound untrusted holds and preserve overflow counts', () => {
  const body = note(
    {
      policy: Array.from({ length: 20 }, (_, i) => ({ ...rule, rule: `rule-${i}` })),
      describers: Array.from({ length: 12 }, (_, i) => ({
        ...describer,
        extension: `ext-${i}`,
        items: ['one', 'two', 'three', 'four', 'five'],
        more: 2,
      })),
    },
    ['human-review:policy', 'human-review:api-contract'],
  )
  assert.equal(body.match(/Policy rule “rule-/g)?.length, 16)
  assert.equal(body.match(/`ext-/g)?.length, 8)
  assert.ok(body.includes('one; two; three; …and 4 more.'))
  assert.equal(body.includes('; four'), false)
  assert.ok(body.length < NOTE_CHAR_LIMIT)
})
