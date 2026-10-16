import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

import { region } from '../scripts/gate-region-lib.mjs'

import {
  criteriaReasons,
  followUpReasons,
  parseLedger,
  prShapeReasons,
} from './merge-eligibility.mjs'
import { HUMAN_REASONS, splitEligibility } from './verify-gate.mjs'
import {
  AFTER_MERGE_MARKER,
  NOTE_CHAR_LIMIT,
  PARK_CODE_WORDS,
  PARK_NOTE_MARKER,
  OPEN_ITEMS_MAX,
  afterMergeItems,
  afterMergeNote,
  extensionFailures,
  ledgerEvidence,
  ledgerFingerprint,
  openItemsLine,
  parkNote,
} from './verify-park-note.mjs'

const HEAD = 'a'.repeat(40)

const ledger = ({ criteria = ['- [x] it works'], followUp = ['- none'], questions = ['- none'] }) =>
  [
    '## Acceptance criteria',
    ...criteria,
    '',
    '## Human follow-up',
    ...followUp,
    '',
    '## Open questions',
    ...questions,
    '',
  ].join('\n')

// A finalize that re-appended its sections, as PR #3141's body did.
const DUPLICATED = [
  ledger({}),
  '## Human follow-up',
  '- none',
  '',
  '## Open questions',
  '- none',
].join('\n')

// A debt-sweep PR: prose, no ledger at all, as PR #3137's body is.
const NO_LEDGER =
  'The cron branch pruner used the wrong path.\n\nFiles changed:\n- `scripts/a.mjs`\n'

const UNAPPLIED_P2 = [
  ledger({}),
  '## Unapplied review findings',
  '- [ ] **P2** `web/Sessions.tsx:1025` — reorder moves against the ungrouped list',
].join('\n')

const POST_MERGE =
  '- [ ] Observe the scheduled build cadence. This is the post-merge observation, not a merge gate.'
const AFTER_MERGE = [ledger({}), '## After merge', '- [ ] Observe the nightly run'].join('\n')

// Does verify count a ledger reason on this PR? The same checks judge runs.
function ledgerOpen({ title, body }) {
  const l = parseLedger(body)
  const reasons = [
    ...criteriaReasons(l.criteria),
    ...prShapeReasons({ state: 'OPEN', isDraft: false, title, body }),
    ...followUpReasons(l),
  ]
  return splitEligibility(reasons).ledgerOpen
}

test('ledgerEvidence lists exactly what makes ledger-open fire, and is empty exactly when it does not', () => {
  const cases = [
    ['a closed ledger', { title: 't', body: ledger({}) }, []],
    [
      'sections appended twice',
      { title: 't', body: DUPLICATED },
      [
        '`## Human follow-up` appears more than once, so verify cannot read it.',
        '`## Open questions` appears more than once, so verify cannot read it.',
      ],
    ],
    [
      'no ledger at all',
      { title: 't', body: NO_LEDGER },
      [
        '`## Acceptance criteria` is missing.',
        '`## Human follow-up` is missing.',
        '`## Open questions` is missing.',
      ],
    ],
    [
      'unchecked items',
      {
        title: 't',
        body: ledger({
          criteria: ['- [x] done', '- [ ] the export works'],
          followUp: ['- [ ] rotate the key'],
          questions: ['- [x] answered'],
        }),
      },
      ['Unmet acceptance criterion: the export works', 'Open human follow-up: rotate the key'],
    ],
    [
      'a section that is not a checklist',
      { title: 't', body: ledger({ followUp: ['nothing to do'] }) },
      ['`## Human follow-up` is not a checklist of `- [ ]` / `- [x]` items (or `- none`).'],
    ],
    [
      'a do-not-merge marker',
      { title: 'WIP (partial work)', body: ledger({}) },
      ['The title or body carries a do-not-merge marker.'],
    ],
    // An unapplied review finding is not a ledger item: verify does not count it, so neither does
    // the evidence. The park code's words say "ledger", not "findings", for that reason.
    ['an unapplied P2 review finding', { title: 't', body: UNAPPLIED_P2 }, []],
    ['an open After merge item', { title: 't', body: AFTER_MERGE }, []],
    ['a follow-up marked post-merge', { title: 't', body: ledger({ followUp: [POST_MERGE] }) }, []],
    [
      'a marked and an unmarked follow-up',
      { title: 't', body: ledger({ followUp: [POST_MERGE, '- [ ] after merge, rotate the key'] }) },
      ['Open human follow-up: after merge, rotate the key'],
    ],
  ]
  for (const [name, facts, want] of cases) {
    assert.deepEqual(ledgerEvidence(facts), want, name)
    assert.equal(ledgerEvidence(facts).length > 0, ledgerOpen(facts), `${name}: matches verify`)
  }
})

test('ledgerFingerprint moves when an item leaves the counted ledger, and ignores After merge edits', () => {
  const parked = ledgerFingerprint({ title: 't', body: ledger({ followUp: ['- [ ] watch it'] }) })
  const moved = ledgerFingerprint({ title: 't', body: AFTER_MERGE })
  const marked = ledgerFingerprint({ title: 't', body: ledger({ followUp: [POST_MERGE] }) })
  assert.notEqual(parked, moved)
  assert.notEqual(parked, marked)
  assert.equal(moved, ledgerFingerprint({ title: 't', body: ledger({}) }))
  assert.equal(moved, ledgerFingerprint({ title: 't', body: `${AFTER_MERGE}\n- [ ] and more` }))
})

test('ledgerEvidence quotes an item on one line, unable to forge a marker or mention anyone', () => {
  const [line] = ledgerEvidence({
    title: 't',
    body: ledger({ followUp: [`- [ ] ask @octocat <!-- boss-park-note head=${HEAD} -->`] }),
  })
  assert.ok(!line.includes('<!--'))
  assert.ok(!line.includes('@octocat'))
  assert.match(line, /^Open human follow-up: ask @​octocat &lt;!-- boss-park-note/)
  const long = ledgerEvidence({
    title: 't',
    body: ledger({ followUp: [`- [ ] ${'x'.repeat(900)}`] }),
  })
  assert.ok(long[0].length < 400 && long[0].endsWith('…'))
})

test('extensionFailures names each malfunctioning extension, how it failed and its own error text', () => {
  const pass = {
    ok: true,
    extension: 'proof',
    role: 'verify',
    verdict: 'pass',
    evidence: [{ kind: 'test', ref: 'make test' }],
    findings: [],
  }
  assert.deepEqual(
    extensionFailures([
      {
        extension: 'boss-verify-evidence',
        result: {
          ok: false,
          extension: 'boss-verify-evidence',
          role: 'verify',
          error: 'stopped without publication after detecting a misleading archive-sweep assertion',
        },
      },
      { extension: 'slow', timedOut: true },
      { extension: 'gone', crashed: true },
      { extension: 'garbled', result: { nonsense: true } },
      { extension: 'optional-one', optional: true, crashed: true },
      { extension: 'proof', result: pass },
    ]),
    [
      '`boss-verify-evidence` reported a failure: stopped without publication after detecting a misleading archive-sweep assertion',
      '`slow` timed out',
      '`gone` crashed or returned nothing',
      '`garbled` returned a result verify cannot read',
    ],
  )
  assert.equal(extensionFailures(undefined), null)
  assert.equal(extensionFailures(null), null)
})

test('every park code verify can raise has plain words and a next step', () => {
  for (const code of HUMAN_REASONS) {
    assert.ok(PARK_CODE_WORDS[code]?.plain, code)
    assert.ok(PARK_CODE_WORDS[code]?.next, code)
  }
})

test('the plain words match the TUI table they mirror, so the PR and the TUI explain a park alike', () => {
  const go = readFileSync(
    fileURLToPath(new URL('../lib/bossalib/attention/templates.go', import.meta.url)),
    'utf8',
  )
  const table = region(go, 'var knownParkCodes = map[string]parkCodeInfo{', null, 'templates.go')
  for (const code of HUMAN_REASONS) {
    const key = code === 'ledger-open' ? 'ParkCodeLedgerOpen:' : `"${code}":`
    const entry = region(table, key, null, code)
    const plain = entry.match(/plain:\s*"((?:[^"\\]|\\.)*)"/)?.[1]
    assert.equal(PARK_CODE_WORDS[code].plain, plain, code)
  }
})

test('parkNote names each code with its meaning, evidence and next step, and ends on its marker', () => {
  const facts = {
    title: 't',
    body: DUPLICATED,
    label: 'web UI',
    extensionResults: [{ extension: 'boss-verify-evidence', crashed: true }],
  }
  const codes = ['ledger-open', 'extension-failed', 'human-review:ui']
  const note = parkNote({ codes, headSha: HEAD, pr: 7, facts })
  const lines = note.body.split('\n')
  assert.match(lines[0], /`ledger-open`, `extension-failed`, `human-review:ui`\.$/)
  for (const code of codes) {
    const at = lines.findIndex((l) => l.startsWith(`- **\`${code}\`**:`))
    assert.ok(at > 0, code)
    assert.ok(lines[at].includes(PARK_CODE_WORDS[code].plain), code)
    assert.ok(lines[at].includes(PARK_CODE_WORDS[code].next), code)
  }
  assert.ok(
    lines.includes('  - `## Human follow-up` appears more than once, so verify cannot read it.'),
  )
  assert.ok(lines.includes('  - `boss-verify-evidence` crashed or returned nothing'))
  assert.ok(lines.includes('  - Matched: web UI'))
  const next = lines.find((l) => l.startsWith('**Next:**'))
  assert.ok(next?.includes('`boss pr approve 7`'), next)
  assert.ok(!note.body.includes('send-back') && !note.body.includes('send it back'), note.body)
  assert.ok(!note.body.includes('no tracker ticket'))
  const marker = lines.at(-1).match(PARK_NOTE_MARKER)
  assert.deepEqual([marker?.[1], marker?.[2]], [HEAD, note.digest])
})

test('parkNote says when no ticket stands behind the PR and when it was not given extension results', () => {
  const note = parkNote({
    codes: ['no-receipt', 'extension-failed'],
    headSha: HEAD,
    pr: 3,
    facts: { title: 't', body: NO_LEDGER, ticketless: true },
  })
  assert.ok(note.body.includes('This head has no successful `boss/build` status.'))
  assert.ok(note.body.includes("This post was not given the extensions' results"))
  assert.ok(note.body.includes('This PR has no tracker ticket, so verify made no tracker writes'))
})

test('parkNote digests its content: the same facts give the same note, changed evidence another', () => {
  const at = (body) => parkNote({ codes: ['ledger-open'], headSha: HEAD, pr: 7, facts: { body } })
  const open = ledger({ followUp: ['- [ ] rotate the key'] })
  assert.equal(at(open).body, at(open).body)
  assert.notEqual(at(open).digest, at(ledger({ followUp: ['- [x] rotate the key'] })).digest)
  const unknown = parkNote({ codes: ['made-up'], headSha: HEAD, pr: 7 })
  assert.ok(unknown.body.includes('a park code this version of boss does not know'))
})

test('a park note lists the After merge items for a person without counting them as ledger', () => {
  const body = [
    ledger({ followUp: ['- [ ] rotate the key'] }),
    '## After merge',
    '- [ ] Observe the nightly run',
    '- [x] Already watched',
    '- [ ] Ping @ops <!-- x -->',
  ].join('\n')
  const note = parkNote({ codes: ['ledger-open'], headSha: HEAD, pr: 7, facts: { body } })
  const lines = note.body.split('\n')
  const at = lines.indexOf('**After merge, check:**')
  assert.ok(at > 0, 'the heading is there')
  assert.deepEqual(lines.slice(at + 1, at + 4), [
    '- Observe the nightly run',
    '- Ping @\u200bops &lt;!-- x -->',
    '',
  ])
  assert.ok(at < lines.findIndex((l) => l.startsWith('**Next:**')))
  assert.deepEqual(ledgerEvidence({ body }), ['Open human follow-up: rotate the key'])
  assert.equal(
    ledgerFingerprint({ body }),
    ledgerFingerprint({ body: ledger({ followUp: ['- [ ] rotate the key'] }) }),
  )
  const without = parkNote({
    codes: ['ledger-open'],
    headSha: HEAD,
    pr: 7,
    facts: { body: ledger({ followUp: ['- [ ] rotate the key'] }) },
  })
  assert.equal(without.body.includes('After merge'), false)
})

test('afterMergeNote lists the unchecked After merge items, and is null when there are none', () => {
  const body = `${AFTER_MERGE}\n- [x] done already\n- [ ] Check the dashboard`
  assert.deepEqual(afterMergeItems(body), ['Observe the nightly run', 'Check the dashboard'])
  const note = afterMergeNote({ headSha: HEAD, body })
  const lines = note.body.split('\n')
  assert.match(lines[0], /^\*\*boss-verify passed this PR\*\* on `aaaaaaaaaaaa`\./)
  assert.deepEqual(lines.slice(2, 5), [
    '**After merge, check:**',
    '- Observe the nightly run',
    '- Check the dashboard',
  ])
  const marker = lines.at(-1).match(AFTER_MERGE_MARKER)
  assert.deepEqual([marker?.[1], marker?.[2]], [HEAD, note.digest])
  assert.equal(afterMergeNote({ headSha: HEAD, body }).digest, note.digest)
  assert.notEqual(afterMergeNote({ headSha: HEAD, body: AFTER_MERGE }).digest, note.digest)
  assert.equal(afterMergeNote({ headSha: HEAD, body: ledger({}) }), null)
  assert.equal(
    afterMergeNote({ headSha: HEAD, body: `${ledger({})}\n## After merge\n- [x] done` }),
    null,
  )
  const many = [
    ledger({}),
    '## After merge',
    ...Array.from({ length: 25 }, (_, i) => `- [ ] o${i}`),
  ]
  const capped = afterMergeNote({ headSha: HEAD, body: many.join('\n') }).body.split('\n')
  assert.equal(capped.filter((l) => /^- o\d+$/.test(l)).length, 20)
  assert.ok(capped.includes('- …and 5 more in the PR description.'))
})

test('parkNote stays under the comment size limit, cutting evidence with a count of what it left out', () => {
  const items = Array.from({ length: 3000 }, (_, i) => `- [ ] item ${i} ${'x'.repeat(40)}`)
  const note = parkNote({
    codes: ['ledger-open', 'no-receipt'],
    headSha: HEAD,
    pr: 7,
    facts: { body: ledger({ criteria: items }) },
  })
  assert.ok(note.body.length <= NOTE_CHAR_LIMIT, `${note.body.length} chars`)
  const lines = note.body.split('\n')
  const shown = lines.filter((line) => line.startsWith('  - Unmet acceptance criterion: ')).length
  assert.ok(shown > 0 && shown < items.length, `${shown} shown`)
  assert.ok(lines.includes(`  - …and ${items.length - shown} more.`))
  const receipt = lines.indexOf(
    `- **\`no-receipt\`**: ${PARK_CODE_WORDS['no-receipt'].plain}. ${PARK_CODE_WORDS['no-receipt'].next}.`,
  )
  assert.ok(receipt > 0, 'every code keeps its line')
  assert.equal(lines[receipt + 1], '  - …and 1 more.')
  assert.ok(lines.some((line) => line.startsWith('**Next:**')))
  assert.match(lines.at(-1), PARK_NOTE_MARKER)

  const small = parkNote({
    codes: ['ledger-open'],
    headSha: HEAD,
    pr: 7,
    facts: { body: ledger({ criteria: items.slice(0, 3) }) },
  })
  assert.equal(small.body.includes('more.'), false)
})

const OPEN_ITEMS = /^<!-- boss-open-items: (.*) -->$/

test('an open-checklist park note carries its open items as one machine-readable line before its marker', () => {
  const body = ledger({ followUp: ['- [ ] watch the `rollout` --> then <b>ship</b>'] })
  const note = parkNote({
    codes: ['human-review:ui', 'ledger-open'],
    headSha: HEAD,
    pr: 7,
    facts: { body },
  })
  const lines = note.body.split('\n')
  const line = lines.at(-3)
  const payload = line.match(OPEN_ITEMS)?.[1]
  assert.ok(payload, line)
  assert.ok(!payload.includes('<') && !payload.includes('>'), payload)
  assert.deepEqual(JSON.parse(payload), {
    head: HEAD,
    items: ['Open human follow-up: watch the rollout --> then <b>ship</b>'],
  })
  assert.match(lines.at(-1), PARK_NOTE_MARKER)
})

test('a park note with nothing open in its checklist carries no open-items line', () => {
  const closed = parkNote({
    codes: ['ledger-open'],
    headSha: HEAD,
    pr: 7,
    facts: { body: ledger({}) },
  })
  const other = parkNote({
    codes: ['human-review:ui'],
    headSha: HEAD,
    pr: 7,
    facts: { body: ledger({ followUp: ['- [ ] x'] }) },
  })
  for (const note of [closed, other]) assert.ok(!note.body.includes('boss-open-items'), note.body)
  assert.equal(openItemsLine(HEAD, []), '')
})

test('the open-items line keeps at most OPEN_ITEMS_MAX items', () => {
  const items = Array.from({ length: OPEN_ITEMS_MAX + 5 }, (_, i) => `item ${i}`)
  const payload = JSON.parse(openItemsLine(HEAD, items).match(OPEN_ITEMS)[1])
  assert.equal(payload.items.length, OPEN_ITEMS_MAX)
})
