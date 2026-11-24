// A UI change verify rates moderate only because its UI area asks, recorded on the page: the
// banner must rate it and count the recording, from the judge output through the page model.
import test from 'node:test'
import assert from 'node:assert/strict'
import { buildProofRecord } from './verify-proof-build.mjs'
import { NO_UI_RECORDING, UI_RECORDED } from './verify-proof-page-model.mjs'
import { proofPageModel, renderProofHtml } from './verify-proof-page.mjs'

const HEAD = 'c'.repeat(40)
const BASE = 'd'.repeat(40)
const UI = {
  area: 'ui',
  label: 'UI',
  paths: ['services/web/src/Chat.tsx'],
  defaultAsk: 'always',
  source: 'config',
}

/** The record verify builds for a UI change, its judge confidence given. */
function record(confidence) {
  const built = buildProofRecord({
    judged: {
      verdict: 'human',
      reason: 'human-review:ui',
      headSha: HEAD,
      baseSha: BASE,
      confidence,
      facts: { areas: [UI] },
    },
    repo: 'acme/shop',
    pr: 9,
  })
  assert.equal(built.ok, true, String(built.errors))
  return built.proof
}

const asking = { level: 'moderate', asks: [{ area: 'ui', label: 'UI', source: 'config' }] }

const media = {
  schema: 'proof-media/1',
  head: HEAD,
  clips: [
    {
      id: 'head-0',
      side: 'head',
      title: 'Reply box focused',
      file: 'head-0.mp4',
      durationMs: 5_000,
      chapters: [{ atMs: 0, caption: 'Open a chat' }],
      stills: [{ file: 'head-0-still.png', caption: 'Cursor in the reply box' }],
    },
  ],
}

/** The page's banner for a record. @param {any} proof @param {object} [extras] */
const banner = (proof, extras) => proofPageModel({ proof }, extras).banner

test('a moderate level only an asking area lowers is rated, with the area as its reason', () => {
  const proof = record({ ...asking, loweredBy: [] })
  assert.deepEqual(
    proof.confidence.loweredBy.map((reason) => reason.code),
    ['area-asks'],
  )
  const { level, word, reason } = banner(proof)
  assert.equal(level, 'moderate')
  assert.equal(word, 'Moderate')
  assert.equal(reason, 'It changes UI, which a person reviews.')
})

test('a high level an asking area lowers is rated moderate for the area', () => {
  const proof = record({ ...asking, level: 'high', loweredBy: [] })
  assert.equal(proof.confidence.level, 'moderate')
  assert.deepEqual(
    proof.confidence.loweredBy.map((reason) => reason.code),
    ['area-asks'],
  )
})

test('a record with no level and no reason still reads as not worked out', () => {
  const proof = record(undefined)
  assert.deepEqual(
    proof.confidence.loweredBy.map((reason) => reason.code),
    ['confidence-not-computed'],
  )
  const { level, reason } = banner(proof)
  assert.equal(level, 'unrated')
  assert.equal(reason, 'Confidence was not worked out for this head.')
})

test('a UI area the page records is a signal row pointing at the recording', () => {
  const proof = record({ ...asking, loweredBy: [] })
  const { signals, unproven } = banner(proof, { media })
  const row = signals.find((signal) => signal.label === 'UI')
  assert.equal(row?.summary, UI_RECORDED)
  assert.deepEqual(unproven, [])
  const html = renderProofHtml({ proof }, { media })
  assert.ok(row.href, 'the row links to the recording')
  assert.ok(
    html.includes(`id="${row.href.slice(1)}"`),
    `${row.href} names the section the clip renders in`,
  )
  assert.ok(html.includes('<video id="proof-video"'), 'the page plays the clip')
})

test('a UI area with no media on the page stays not proven', () => {
  const { signals, unproven } = banner(record({ ...asking, loweredBy: [] }))
  assert.equal(
    signals.some((signal) => signal.label === 'UI'),
    false,
  )
  assert.deepEqual(unproven, [NO_UI_RECORDING, 'UI: nothing on this page shows it'])
})
