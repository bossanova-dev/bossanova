import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

import { HUMAN_REASONS } from './verify-gate.mjs'
import {
  AFTER_MERGE_MARKER,
  NOTE_CHAR_LIMIT,
  PARK_CODE_WORDS,
  UNKNOWN_CODE_WORDS,
  PARK_NOTE_MARKER,
  PARK_CODES_LINE,
  afterMergeItems,
  afterMergeNote,
  extensionFailures,
  parkNote,
  restatesCategory,
} from './verify-park-note.mjs'

const HEAD = 'a'.repeat(40)

const PARTIAL = (met = 2, total = 5) =>
  `## Summary\nHalf of it.\n\n## Partial\ndo not merge — partial: ${met}/${total} acceptance criteria\n`

// A debt-sweep PR: prose and no sections, as PR #3137's body is.
const PROSE = 'The cron branch pruner used the wrong path.\n\nFiles changed:\n- `scripts/a.mjs`\n'

const AFTER_MERGE = `${PROSE}\n## After merge\n- [ ] Observe the nightly run`

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

test('every park code verify can raise has plain words', () => {
  for (const code of HUMAN_REASONS) assert.ok(PARK_CODE_WORDS[code]?.plain, code)
})

test('the park words equal the table the TUI renders, code for code, so the PR and the TUI explain a park alike', () => {
  const table = JSON.parse(
    readFileSync(
      fileURLToPath(
        new URL('../../lib/bossalib/attention/testdata/park_code_words.json', import.meta.url),
      ),
      'utf8',
    ),
  )
  assert.deepEqual(Object.keys(PARK_CODE_WORDS).sort(), Object.keys(table.codes).sort())
  const withRule = (words) => ({ ...words, restates: restatesCategory(words) })
  for (const [code, words] of Object.entries(table.codes)) {
    assert.deepEqual(withRule(PARK_CODE_WORDS[code]), words, code)
  }
  assert.deepEqual(withRule(UNKNOWN_CODE_WORDS), table.unknown)
})

test('restatesCategory ignores case and articles and nothing else', () => {
  assert.equal(restatesCategory({ plain: 'a UI change', category: 'UI change' }), true)
  assert.equal(restatesCategory({ plain: 'The  ui CHANGE', category: 'an UI change' }), true)
  assert.equal(restatesCategory({ plain: 'a UI change to review', category: 'UI change' }), false)
  assert.equal(
    restatesCategory({ plain: 'a security-sensitive change', category: 'security' }),
    false,
  )
})

test('parkNote names each code by its issue and evidence, once each, and ends on its marker', () => {
  const facts = {
    title: 't',
    body: PARTIAL(),
    label: 'web UI',
    extensionResults: [{ extension: 'boss-verify-evidence', crashed: true }],
  }
  const codes = ['partial-build', 'extension-failed', 'human-review:ui']
  const note = parkNote({ codes, headSha: HEAD, pr: 7, facts })
  const lines = note.body.split('\n')
  assert.match(lines[0], /: partial build, verify extension failed, UI change\.$/)
  const plainLine = (code) => {
    const plain = PARK_CODE_WORDS[code].plain
    return `- ${plain[0].toUpperCase()}${plain.slice(1)}.`
  }
  for (const code of ['partial-build', 'extension-failed'])
    assert.ok(lines.indexOf(plainLine(code)) > 0, code)
  assert.equal(
    lines.filter((l) => /UI change/i.test(l)).length,
    1,
    'the UI change is named once, in the summary, since its own words would only repeat it',
  )
  const shown = note.body.split('\n').filter((l) => !l.startsWith('<!--'))
  for (const code of codes) assert.ok(!shown.join('\n').includes(code), `${code} shown to people`)
  const hidden = lines.map((l) => l.match(PARK_CODES_LINE)).find(Boolean)
  assert.deepEqual(
    JSON.parse(hidden?.[1] ?? 'null'),
    codes,
    'the exact codes ride in a hidden line',
  )
  assert.ok(
    lines.includes('  - `## Partial` reads: do not merge — partial: 2/5 acceptance criteria'),
  )
  assert.ok(lines.includes('  - `boss-verify-evidence` crashed or returned nothing'))
  assert.ok(lines.includes('- The change touches web UI.'), 'the UI change keeps its evidence')
  assert.ok(
    lines.includes(
      'An approval waives exactly what is listed above, on this commit only. A new push is judged afresh.',
    ),
  )
  assert.ok(!note.body.includes('boss pr approve'), 'no instruction to the reader')
  assert.ok(!note.body.includes('send-back') && !note.body.includes('send it back'), note.body)
  assert.ok(!note.body.includes('no tracker ticket'))
  const marker = lines.at(-1).match(PARK_NOTE_MARKER)
  assert.deepEqual([marker?.[1], marker?.[2]], [HEAD, note.digest])
})

test('parkNote says the park label once, as its own line before the codes', () => {
  const touches = '- The change touches web/src/x.tsx.'
  const both = parkNote({
    codes: ['human-review:security', 'human-review:ui'],
    headSha: HEAD,
    facts: { title: 't', body: PROSE, label: 'web/src/x.tsx' },
  }).body.split('\n')
  assert.equal(
    both.filter((l) => l.includes('web/src/x.tsx')).length,
    1,
    `two labelled codes repeat the label:\n${both.join('\n')}`,
  )
  assert.equal(both.indexOf(touches), 2, both.join('\n'))
  assert.ok(both.includes('- A security-sensitive change for a person to review.'), both.join('\n'))

  const withPartial = parkNote({
    codes: ['partial-build', 'human-review:ui'],
    headSha: HEAD,
    facts: { title: 'Feature (partial 1/2)', body: PROSE, label: 'web/a' },
  }).body.split('\n')
  const label = withPartial.indexOf('- The change touches web/a.')
  const partial = withPartial.findIndex((l) => l.startsWith('- Only part of the ticket'))
  const evidence = withPartial.indexOf('  - The title reads: Feature (partial 1/2)')
  assert.ok(
    label > 0 && label < partial && partial < evidence,
    `the label sits above the partial code:\n${withPartial.join('\n')}`,
  )
  assert.equal(withPartial.filter((l) => l.includes('web/a')).length, 1)

  const unlabelled = parkNote({
    codes: ['partial-build'],
    headSha: HEAD,
    facts: { title: 't', body: PARTIAL(), label: 'web/a' },
  }).body
  assert.ok(!unlabelled.includes('web/a'), 'a park with no labelled code shows no label')
})

test('parkNote says when no ticket stands behind the PR and when it was not given extension results', () => {
  const note = parkNote({
    codes: ['no-receipt', 'extension-failed'],
    headSha: HEAD,
    pr: 3,
    facts: { title: 't', body: PROSE, ticketless: true },
  })
  assert.ok(note.body.includes('No automated build and review has passed on this commit.'))
  assert.ok(!note.body.includes('boss/build'), 'no status context named to people')
  assert.ok(
    note.body.includes("The extensions' results were not available when this comment was written."),
  )
  assert.ok(note.body.includes('This PR has no tracker ticket, so no ticket was updated.'))
})

test('parkNote digests its content: the same facts give the same note, changed evidence another', () => {
  const at = (body) => parkNote({ codes: ['partial-build'], headSha: HEAD, pr: 7, facts: { body } })
  assert.equal(at(PARTIAL()).body, at(PARTIAL()).body)
  assert.notEqual(at(PARTIAL(2, 5)).digest, at(PARTIAL(3, 5)).digest)
  const unknown = parkNote({ codes: ['made-up'], headSha: HEAD, pr: 7 })
  assert.equal(
    unknown.body.split('an issue this version of boss does not recognise').length - 1,
    1,
    unknown.body,
  )
  assert.ok(!/^- /m.test(unknown.body), 'no bullet repeats the summary')
  assert.ok(!unknown.body.includes('park code'), unknown.body)
})

test('a park note lists the After merge items for a person', () => {
  const body = [
    PARTIAL(),
    '## After merge',
    '- [ ] Observe the nightly run',
    '- [x] Already watched',
    '- [ ] Ping @ops <!-- x -->',
  ].join('\n')
  const note = parkNote({ codes: ['partial-build'], headSha: HEAD, pr: 7, facts: { body } })
  const lines = note.body.split('\n')
  const at = lines.indexOf('**After merge, check:**')
  assert.ok(at > 0, 'the heading is there')
  assert.deepEqual(lines.slice(at + 1, at + 4), [
    '- Observe the nightly run',
    '- Ping @\u200bops &lt;!-- x -->',
    '',
  ])
  assert.ok(at < lines.findIndex((l) => l.startsWith('An approval waives')))
  const without = parkNote({
    codes: ['partial-build'],
    headSha: HEAD,
    pr: 7,
    facts: { body: PARTIAL() },
  })
  assert.equal(without.body.includes('After merge'), false)
})

test('afterMergeNote lists the unchecked After merge items, and is null when there are none', () => {
  const body = `${AFTER_MERGE}\n- [x] done already\n- [ ] Check the dashboard`
  assert.deepEqual(afterMergeItems(body), ['Observe the nightly run', 'Check the dashboard'])
  const note = afterMergeNote({ headSha: HEAD, body })
  const lines = note.body.split('\n')
  assert.match(lines[0], /^\*\*This PR passed verification\*\* on `aaaaaaaaaaaa`\./)
  assert.deepEqual(lines.slice(2, 5), [
    '**After merge, check:**',
    '- Observe the nightly run',
    '- Check the dashboard',
  ])
  const marker = lines.at(-1).match(AFTER_MERGE_MARKER)
  assert.deepEqual([marker?.[1], marker?.[2]], [HEAD, note.digest])
  assert.equal(afterMergeNote({ headSha: HEAD, body }).digest, note.digest)
  assert.notEqual(afterMergeNote({ headSha: HEAD, body: AFTER_MERGE }).digest, note.digest)
  assert.equal(afterMergeNote({ headSha: HEAD, body: PROSE }), null)
  assert.equal(
    afterMergeNote({ headSha: HEAD, body: `${PROSE}\n## After merge\n- [x] done` }),
    null,
  )
  const many = [PROSE, '## After merge', ...Array.from({ length: 25 }, (_, i) => `- [ ] o${i}`)]
  const capped = afterMergeNote({ headSha: HEAD, body: many.join('\n') }).body.split('\n')
  assert.equal(capped.filter((l) => /^- o\d+$/.test(l)).length, 20)
  assert.ok(capped.includes('- …and 5 more in the PR description.'))
})

test('parkNote stays under the comment size limit, cutting evidence with a count of what it left out', () => {
  const failed = Array.from({ length: 3000 }, (_, i) => ({
    extension: `ext-${i}-${'x'.repeat(40)}`,
    crashed: true,
  }))
  const note = parkNote({
    codes: ['extension-failed', 'no-receipt'],
    headSha: HEAD,
    pr: 7,
    facts: { body: PROSE, extensionResults: failed },
  })
  assert.ok(note.body.length <= NOTE_CHAR_LIMIT, `${note.body.length} chars`)
  const lines = note.body.split('\n')
  const shown = lines.filter((line) => /^ {2}- `ext-\d+-x+` crashed/.test(line)).length
  assert.ok(shown > 0 && shown < failed.length, `${shown} shown`)
  assert.ok(lines.includes(`  - …and ${failed.length - shown} more.`))
  const words = PARK_CODE_WORDS['no-receipt']
  const receipt = lines.indexOf(`- ${words.plain[0].toUpperCase()}${words.plain.slice(1)}.`)
  assert.ok(receipt > 0, 'every code keeps its line')
  assert.equal(lines[receipt + 1], '  - …and 1 more.')
  assert.ok(lines.some((line) => line.startsWith('An approval waives')))
  assert.match(lines.at(-1), PARK_NOTE_MARKER)

  const small = parkNote({
    codes: ['extension-failed'],
    headSha: HEAD,
    pr: 7,
    facts: { body: PROSE, extensionResults: failed.slice(0, 3) },
  })
  assert.equal(small.body.includes('more.'), false)
})

test('a park note carries no open-items line and its marker no ledger', () => {
  const note = parkNote({
    codes: ['partial-build'],
    headSha: HEAD,
    pr: 7,
    facts: { body: PARTIAL() },
  })
  assert.ok(!note.body.includes('boss-open-items'), note.body)
  assert.ok(!note.body.includes('ledger='), note.body)
})

test('a note written under the retired checklist gate still reads as its head note', () => {
  const old = `<!-- boss-park-note head=${HEAD} digest=${'0'.repeat(16)} ledger=${'1'.repeat(16)} -->`
  const marker = old.match(PARK_NOTE_MARKER)
  assert.deepEqual([marker?.[1], marker?.[2]], [HEAD, '0'.repeat(16)])
  const note = parkNote({ codes: ['ledger-open'], headSha: HEAD, pr: 7, facts: { body: PROSE } })
  assert.match(note.body, /: open checklist items\.$/m)
  assert.ok(!note.body.includes('an issue this version of boss does not recognise'), note.body)
})
