import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, writeFileSync, existsSync } from 'node:fs'
import { join } from 'node:path'
import { spawnSync } from 'node:child_process'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { MAX_BULLETS } from './verify-proof.mjs'
import { validateVerifyContext } from './verify-contract.mjs'
import {
  collectIntentSources,
  recordIntent,
  validateIntent,
  INTENT_SOURCE_ORDER,
  INTENT_BULLET_MAX_CHARS,
  INTENT_MAX_SOURCE_CHARS,
  INTENT_MAX_DIFF_LINES,
  INTENT_MAX_DIFF_FILES,
  INTENT_MAX_LINKED_ISSUES,
} from './verify-intent.mjs'

const envelope = (extra = {}) => ({
  pr: { number: 7, title: 'Summary', body: '' },
  headSha: 'a'.repeat(40),
  baseSha: 'b'.repeat(40),
  changedPaths: [],
  ticket: null,
  ...extra,
})
const judge = (extra = {}) => ({
  verdict: 'extensions-required',
  extensionEnvelope: envelope(extra),
  facts: { ticketDescription: 'no-ticket-line' },
})
const read =
  ({
    commits = [],
    numstat = '1\t1\ta.mjs',
    patch = '+new\n-old',
    createdAt = '2026-01-02',
  } = {}) =>
  (command, args) => ({
    ok: true,
    payload: { commits, createdAt },
    output: command === 'git' ? (args.includes('--numstat') ? numstat : patch) : '',
  })
const json = (file) => JSON.parse(readFileSync(file, 'utf8'))
const collect = (t, j, options = {}) =>
  collectIntentSources(j, { outDir: tempDir('intent-', t), read: read(), adapter: {}, ...options })

test('sources skip unsupported judges and invalid envelopes', async (t) => {
  assert.equal((await collect(t, { verdict: 'pass' })).decision, 'skip')
  assert.equal(
    (await collect(t, { verdict: 'extensions-required', extensionEnvelope: {} })).decision,
    'skip',
  )
})

test('sources offer ticket, closing issues, body and commits in rank order', async (t) => {
  const result = await collect(
    t,
    judge({
      ticket: { id: 'ABC-1', description: 'Adds alerts' },
      pr: {
        number: 7,
        title: 'Alerts',
        body: 'Tracker issue: https://example.test/issue/ABC-1/title\nFixes https://example.test/issue/ABC-2/title\nAdds alerts',
      },
    }),
    {
      adapter: {
        readDescription: async (id) => ({
          identifier: id,
          description: 'Shows alerts',
          descriptionUpdatedAt: '2026-01-01',
        }),
      },
      read: read({
        commits: [{ messageHeadline: 'Display alerts', messageBody: 'Adds alert view' }],
      }),
    },
  )
  assert.equal(result.decision, 'derive')
  const sources = json(result.sources)
  assert.deepEqual(
    sources.offered.map((s) => s.kind),
    INTENT_SOURCE_ORDER,
  )
  assert.equal(sources.offered[0].rank, 1)
  assert.equal(sources.offered[0].ref, 'ABC-1')
  assert.deepEqual(sources.offered[1].ref, ['ABC-2'])
  assert.match(readFileSync(sources.offered[3].file, 'utf8'), /\+new/)
  assert.ok(existsSync(result.brief))
})

test('closing issue reads obey edit time, capabilities and link limits', async (t) => {
  const body = Array.from(
    { length: INTENT_MAX_LINKED_ISSUES + 2 },
    (_, i) => `Fixes ABC-${i + 1}`,
  ).join('\n')
  const ids = []
  const result = await collect(t, judge({ pr: { number: 7, body } }), {
    adapter: {
      readDescription: async (id) => {
        ids.push(id)
        return { identifier: id, description: 'late', descriptionUpdatedAt: '2026-02-01' }
      },
    },
  })
  assert.equal(ids.length, INTENT_MAX_LINKED_ISSUES)
  assert.equal(json(result.sources).skipped.find((s) => s.kind === 'issue').why, 'edited-after-pr')
  const absent = await collect(t, judge({ pr: { number: 7, body: 'Fixes ABC-1' } }))
  assert.equal(json(absent.sources).skipped.find((s) => s.kind === 'issue').why, 'no-capability')
  const mention = await collect(
    t,
    judge({ pr: { number: 7, body: 'Related https://example.test/issue/ABC-1' } }),
  )
  assert.equal(json(mention.sources).skipped.find((s) => s.kind === 'issue').why, 'no-links')
})

test('commits omit patches above either diff limit while preserving messages', async (t) => {
  for (const numstat of [
    `${INTENT_MAX_DIFF_LINES + 1}\t0\ta`,
    Array.from({ length: INTENT_MAX_DIFF_FILES + 1 }, (_, i) => `0\t0\tf${i}`).join('\n'),
  ]) {
    const result = await collect(t, judge(), {
      read: read({ commits: [{ messageHeadline: 'Adds alerts' }], numstat }),
    })
    const source = json(result.sources).offered.find((s) => s.kind === 'commits')
    assert.equal(source.diffOmitted, true)
    assert.match(readFileSync(source.file, 'utf8'), /Adds alerts/)
    assert.doesNotMatch(readFileSync(source.file, 'utf8'), /\+new/)
  }
})

test('sources bound text and remove ticket lines and HTML comments from body', async (t) => {
  const result = await collect(
    t,
    judge({
      ticket: { id: 'ABC-1', description: 'x'.repeat(INTENT_MAX_SOURCE_CHARS + 1) },
      pr: {
        number: 7,
        title: 'Title',
        body: 'Tracker issue: https://example.test/issue/ABC-1\n<!-- hidden -->\nBody',
      },
    }),
  )
  const offered = json(result.sources).offered
  assert.equal(offered[0].truncated, true)
  assert.equal(readFileSync(offered[0].file, 'utf8').length, INTENT_MAX_SOURCE_CHARS)
  assert.equal(readFileSync(offered[1].file, 'utf8'), 'Title\n\nBody')
})

test('no usable sources writes unknown envelope and no brief even with title', async (t) => {
  const dir = tempDir('intent-', t)
  const result = await collect(t, judge(), { outDir: dir })
  assert.equal(result.decision, 'unknown')
  assert.equal(existsSync(join(dir, 'deriver-brief.md')), false)
  assert.deepEqual(json(result.envelope).intent, { source: 'unknown', bullets: [] })
  assert.equal(validateVerifyContext(json(result.envelope)).ok, true)
})

const sourceRecord = () => ({
  envelope: envelope(),
  offered: [
    { rank: 1, kind: 'acceptance-criteria', ref: 'ABC-1' },
    { rank: 3, kind: 'pr-body', ref: '#7' },
  ],
  skipped: [],
})
const valid = () => ({
  source: 'pr-body',
  bullets: ['Adds alerts'],
  skipped: [{ kind: 'acceptance-criteria', why: 'No behavior described' }],
})

test('validation binds source refs mechanically and accounts for higher sources', () => {
  assert.deepEqual(validateIntent({ ...valid(), ref: 'spoofed' }, sourceRecord()).intent, {
    source: 'pr-body',
    ref: '#7',
    bullets: ['Adds alerts'],
  })
  for (const output of [
    { ...valid(), bullets: Array.from({ length: MAX_BULLETS + 1 }, (_, i) => `Fact ${i}`) },
    { ...valid(), bullets: ['two\nlines'] },
    { ...valid(), bullets: ['x'.repeat(INTENT_BULLET_MAX_CHARS + 1)] },
    { ...valid(), bullets: ['same', 'same'] },
    { ...valid(), bullets: ['- marker'] },
    { ...valid(), bullets: [' untrimmed'] },
    { ...valid(), source: 'issue' },
    { ...valid(), skipped: [] },
    { ...valid(), skipped: 'invalid' },
    { ...valid(), unexpected: true },
    { bullets: [] },
  ])
    assert.equal(validateIntent(output, sourceRecord()).ok, false, JSON.stringify(output))
})

test('record writes valid intent or unknown on every unusable answer', (t) => {
  const outDir = tempDir('intent-', t)
  const intentFile = join(outDir, 'answer.json')
  writeFileSync(intentFile, JSON.stringify(valid()))
  let result = recordIntent({ sources: sourceRecord(), outDir, intentFile })
  assert.equal(result.decision, 'intent')
  assert.deepEqual(json(result.envelope).intent, {
    source: 'pr-body',
    ref: '#7',
    bullets: ['Adds alerts'],
  })
  assert.equal(validateVerifyContext(json(result.envelope)).ok, true)
  for (const text of [
    '{',
    JSON.stringify({ bullets: [] }),
    JSON.stringify({ ...valid(), bullets: ['bad\nline'] }),
  ]) {
    writeFileSync(intentFile, text)
    result = recordIntent({ sources: sourceRecord(), outDir, intentFile })
    assert.equal(result.decision, 'unknown')
    assert.deepEqual(json(result.envelope).intent, { source: 'unknown', bullets: [] })
  }
  assert.equal(recordIntent({ sources: sourceRecord(), outDir }).decision, 'unknown')
})

test('CLI rejects unknown options and unreadable sources with usage status', () => {
  for (const args of [
    ['sources', '--unknown'],
    ['record', '--sources', '/absent-intent-sources.json', '--out-dir', '/tmp'],
  ]) {
    const child = spawnSync(
      process.execPath,
      ['skills-toolbox/verify/verify-intent.mjs', ...args],
      { encoding: 'utf8' },
    )
    assert.equal(child.status, 2)
  }
})
