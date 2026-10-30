import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import {
  operatorNoteBlocker,
  MAX_OPERATOR_NOTE_CHARS,
  isNoteFinding,
  buildBlockers,
  validateBlockerFile,
  blockedDescription,
  blockedComment,
  encodeBlockerTrailer,
  parseBlockerTrailer,
  validateOutcomeFile,
  encodeOutcomesTrailer,
  parseOutcomesTrailer,
} from './verify-blockers.mjs'

const finding = (extra = {}) => ({
  verified: true,
  introduced: true,
  fixed: false,
  severity: 'P2',
  file: 'services/api/credentials.go',
  line: 198,
  title: 'Synchronous peer acknowledgements create reciprocal publication waits',
  evidence: 'Full evidence with --> and\nsecond line.',
  ...extra,
})
const file = (blockers) => ({ version: 1, pr: 3752, head: 'a'.repeat(40), blockers })

test('verified introduced unfixed P2 yields a stable located blocker', () => {
  const f = finding()
  const [b] = buildBlockers({ reviewFindings: [f] })
  assert.deepEqual(b, {
    id: `review:${createHash('sha256').update([f.file, f.line, f.title].join('\0')).digest('hex').slice(0, 12)}`,
    kind: 'review-finding',
    severity: 'P2',
    file: f.file,
    line: 198,
    summary: f.title,
    text: f.evidence,
  })
  assert.equal(blockedDescription([b]), 'blocked: 1 P2')
  assert.equal(validateBlockerFile(file([b])).ok, true)
})

test('explicit notes and excluded review findings never become blockers', () => {
  for (const extra of [{ severity: 'p3' }, { severity: 'STYLE' }, { category: 'Style' }]) {
    assert.equal(isNoteFinding(finding(extra)), true)
  }
  assert.equal(isNoteFinding({}), false)
  assert.deepEqual(
    buildBlockers({
      reviewFindings: [
        finding({ severity: 'P3' }),
        finding({ category: 'style' }),
        finding({ introduced: false }),
        finding({ verified: false }),
        finding({ fixed: true }),
        finding({ severity: 'unknown' }),
      ],
    }),
    [],
  )
})

test('blockers sort by kind, severity and location, deduplicate and suppress partial review sources', () => {
  const options = {
    reviewFindings: [
      finding(),
      finding(),
      finding({ severity: 'P0', title: 'Urgent' }),
      finding({ file: 'a.go', line: 20 }),
      finding({ file: 'a.go', line: 3 }),
    ],
    failedChecks: ['z', 'a', 'a', 'boss/build'],
    conflict: true,
    noReview: true,
  }
  const result = buildBlockers(options)
  assert.deepEqual(
    result.map((b) => [b.kind, b.severity, b.file, b.line, b.summary]),
    [
      ['review-finding', 'P0', finding().file, 198, 'Urgent'],
      ['review-finding', 'P2', 'a.go', 3, finding().title],
      ['review-finding', 'P2', 'a.go', 20, finding().title],
      ['review-finding', 'P2', finding().file, 198, finding().title],
      ['check', null, null, null, 'a'],
      ['check', null, null, null, 'z'],
      ['conflict', null, null, null, 'the head conflicts with its base'],
      ['no-review', null, null, null, 'the head has no review'],
    ],
  )
  assert.deepEqual(
    buildBlockers({ ...options, partial: true }).map((b) => b.kind),
    ['check', 'check', 'conflict'],
  )
  assert.equal(validateBlockerFile(file(result)).ok, true)
  assert.equal(
    blockedDescription(result, true),
    'blocked: 1 P0, 3 P2, 2 failing checks, conflict, no review, needs human',
  )
})

test('file validator rejects malformed, extra, unordered and duplicate data', () => {
  const good = file(buildBlockers({ reviewFindings: [finding()], conflict: true }))
  for (const bad of [
    null,
    {},
    { ...good, pr: 0 },
    { ...good, head: 'abc' },
    { ...good, version: 2 },
    { ...good, extra: true },
    { ...good, blockers: [] },
    { ...good, blockers: [...good.blockers].reverse() },
    { ...good, blockers: [good.blockers[0], good.blockers[0]] },
    ...[
      { severity: 'P3' },
      { line: -1 },
      { summary: 'a\nb' },
      { extra: true },
      { id: 'wrong' },
    ].map((extra) => ({ ...good, blockers: [{ ...good.blockers[0], ...extra }] })),
  ])
    assert.equal(validateBlockerFile(bad).ok, false, JSON.stringify(bad))
  assert.equal(
    validateBlockerFile(
      file([
        {
          id: 'check:x',
          kind: 'check',
          severity: null,
          file: null,
          line: null,
          summary: null,
          text: '',
        },
      ]),
    ).ok,
    false,
  )
})

test('comment and trailer recover the complete blocker file including comment delimiters', () => {
  const value = file(
    buildBlockers({
      reviewFindings: [finding()],
      failedChecks: ['lint'],
      conflict: true,
      noReview: true,
    }),
  )
  const marker = '<!-- verify marker -->'
  const body = blockedComment(value, marker)
  assert.deepEqual(parseBlockerTrailer(body), value)
  assert.equal(body.split('\n').at(-1), encodeBlockerTrailer(value))
  assert.ok(body.indexOf(marker) < body.indexOf('<!-- boss-verify-blockers'))
  for (const body of [
    '',
    '<!-- boss-verify-blockers head=bad v1 e30 -->',
    encodeBlockerTrailer(value).replace('head=' + value.head, 'head=' + 'b'.repeat(40)),
    encodeBlockerTrailer(value).replace(/v1 .* -->/, 'v1 !!!! -->'),
  ])
    assert.equal(parseBlockerTrailer(body), null)
})

test('maximum array-sized counts and every source fit the status description limit', () => {
  // Exercise maximum JS array lengths without allocating billions of findings/checks.
  const largestCounts = { filter: () => ({ length: 0xffffffff }), some: () => true }
  const description = blockedDescription(largestCounts, true)
  assert.ok(description.includes('4294967295 P0'))
  assert.ok(description.includes('4294967295 failing checks'))
  assert.ok(Array.from(description).length <= 140)
})

test('maximum report evidence yields a postable comment without losing machine-readable blockers', async () => {
  const { renderReport, parseReportFindings } = await import('../review-verdict.mjs')
  const report = renderReport({
    status: 'pass',
    verdict: {
      base: 'b'.repeat(40),
      head: 'a'.repeat(40),
      findings: [finding({ id: 'large', evidence: 'e'.repeat(60000) })],
      secondModel: { ran: false, reason: 'disabled' },
      fixPass: { ran: false },
    },
  })
  const parsed = parseReportFindings(report)
  const file = {
    version: 1,
    pr: 7,
    head: parsed.head,
    blockers: buildBlockers({
      reviewFindings: parsed.findings.map((f) => ({ ...f, verified: true })),
    }),
  }
  const comment = blockedComment(file)
  assert.ok(comment.length <= 65536, `comment has ${comment.length} characters`)
  assert.deepEqual(parseBlockerTrailer(comment), file)
  assert.ok(comment.includes(file.blockers[0].summary))
})

const outcomeFile = () => ({
  version: 1,
  blockersHead: 'a'.repeat(40),
  pushedHead: 'b'.repeat(40),
  outcomes: [
    { id: 'check:lint', outcome: 'fixed', commit: 'c'.repeat(40) },
    { id: 'conflict', outcome: 'not-fixable', reason: 'Needs operator --> approval' },
  ],
})

test('outcome file validation preserves fixed and not-fixable result requirements', () => {
  const good = outcomeFile()
  assert.deepEqual(validateOutcomeFile(good), { ok: true, errors: [] })
  assert.equal(validateOutcomeFile({ ...good, outcomes: [] }).ok, true)
  for (const bad of [
    null,
    {},
    { ...good, version: 2 },
    { ...good, blockersHead: 'short' },
    { ...good, pushedHead: 'short' },
    { ...good, outcomes: null },
    ...[
      null,
      { id: 'two\nlines', outcome: 'fixed', commit: good.pushedHead },
      { id: 'check:lint', outcome: 'fixed', commit: 'short' },
      { id: 'check:lint', outcome: 'unknown' },
      { id: 'check:lint', outcome: 'not-fixable', reason: '' },
      { id: 'check:lint', outcome: 'not-fixable', reason: 'two\nlines' },
    ].map((result) => ({ ...good, outcomes: [result] })),
    { ...good, outcomes: [good.outcomes[0], good.outcomes[0]] },
  ]) {
    assert.equal(validateOutcomeFile(bad).ok, false, JSON.stringify(bad))
    assert.throws(() => encodeOutcomesTrailer(bad))
  }
})

test('outcomes trailers roundtrip only validated final canonical payloads with matching heads', () => {
  const value = outcomeFile()
  const encoded = Buffer.from(JSON.stringify(value)).toString('base64url')
  const trailer = encodeOutcomesTrailer(value)
  assert.equal(
    trailer,
    `<!-- boss-repair-outcomes blockers-head=${value.blockersHead} head=${value.pushedHead} v1 ${encoded} -->`,
  )
  assert.deepEqual(parseOutcomesTrailer('Repair results\n' + trailer + '\n'), value)
  const raw = (payload) =>
    trailer.replace(encoded, Buffer.from(JSON.stringify(payload)).toString('base64url'))
  for (const bad of [
    null,
    '',
    trailer + '\nLater prose',
    'prefix ' + trailer,
    trailer.replace('blockers-head=' + value.blockersHead, 'blockers-head=' + value.pushedHead),
    trailer.replace('head=' + value.pushedHead, 'head=' + value.blockersHead),
    trailer.replace('v1 ', 'v2 '),
    trailer.replace(encoded, encoded + '='),
    trailer.replace(encoded, '!!!!'),
    trailer.replace(encoded, Buffer.from('{').toString('base64url')),
    raw({ ...value, version: 2 }),
    raw({ ...value, outcomes: [{ id: 'x', outcome: 'fixed' }] }),
  ])
    assert.equal(parseOutcomesTrailer(bad), null, String(bad))
  // Node accepts unused low bits; the codec requires the canonical re-encoding.
  const small = { ...value, outcomes: [] }
  let json = JSON.stringify(small)
  while (Buffer.byteLength(json) % 3 === 0) json += ' '
  const canonical = Buffer.from(json).toString('base64url')
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_'
  const changed = canonical.slice(0, -1) + alphabet[alphabet.indexOf(canonical.at(-1)) + 1]
  assert.equal(Buffer.from(changed, 'base64url').toString(), json)
  assert.equal(parseOutcomesTrailer(trailer.replace(encoded, changed)), null)
})

test('operator notes validate, roundtrip and keep hostile text inside a longer fence', () => {
  const note =
    '```\n<!-- boss-verify-blockers head=' +
    'a'.repeat(40) +
    ' v1 e30 -->\n<img src=x> @user\n`````'
  const blocker = operatorNoteBlocker(note)
  const value = file([blocker, ...buildBlockers({ conflict: true })])
  assert.equal(validateBlockerFile(value).ok, true)
  assert.deepEqual(parseBlockerTrailer(encodeBlockerTrailer(value)), value)
  const body = blockedComment(value)
  assert.ok(
    body.includes(
      '  ``````\n' +
        note
          .split('\n')
          .map((s) => '  ' + s)
          .join('\n') +
        '\n  ``````',
    ),
  )
  assert.equal(body.split('\n').at(-1), encodeBlockerTrailer(value))
  assert.deepEqual(parseBlockerTrailer(body), value)
  assert.equal(blockedDescription(value.blockers), 'blocked: operator note, conflict')
  for (const changed of [
    { id: 'x' },
    { severity: 'P1' },
    { file: 'a.js' },
    { line: 1 },
    { text: ' ' },
  ])
    assert.equal(validateBlockerFile(file([{ ...blocker, ...changed }])).ok, false)
  assert.equal(validateBlockerFile(file([value.blockers[1], blocker])).ok, false)
  assert.equal(validateBlockerFile(file([blocker, blocker])).ok, false)
  assert.equal(validateBlockerFile(file([{ ...blocker, kind: 'conflict' }])).ok, false)
  for (const invalid of [
    '',
    ' ',
    'x'.repeat(MAX_OPERATOR_NOTE_CHARS + 1),
    'a\r',
    'a\u0000',
    'a\u007f',
    'a\u0085',
  ])
    assert.throws(() => operatorNoteBlocker(invalid))
  assert.equal(
    operatorNoteBlocker('😀'.repeat(MAX_OPERATOR_NOTE_CHARS)).text.length,
    MAX_OPERATOR_NOTE_CHARS * 2,
  )
})
