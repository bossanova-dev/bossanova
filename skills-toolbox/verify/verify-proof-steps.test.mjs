import assert from 'node:assert/strict'
import test from 'node:test'
import { createProof, MAX_CAPTION, PROOF_CHAPTER, PROOF_STILL } from './verify-proof-steps.mjs'

function fixture(enabled, pacing) {
  const calls = []
  const info = { annotations: [], outputPath: (name) => `/results/${name}` }
  const page = {
    evaluate: async (...args) => calls.push(['evaluate', ...args]),
    screenshot: async (options) => calls.push(['screenshot', options]),
    waitForTimeout: async (ms) => calls.push(['wait', ms]),
  }
  const target = {
    scrollIntoViewIfNeeded: async () => calls.push(['scroll']),
    boundingBox: async () => ({ x: 10, y: 20, width: 100, height: 40 }),
    click: async () => calls.push(['click']),
    fill: async (text) => calls.push(['fill', text]),
    pressSequentially: async (text, options) => calls.push(['type', text, options]),
  }
  return {
    calls,
    info,
    page,
    target,
    proof: createProof({ test: { info: () => info }, enabled, pacing }),
  }
}

test('disabled proof steps leave test actions fast and record nothing', async () => {
  const { calls, info, page, target, proof } = fixture(false)
  proof.begin()
  await proof.chapter(page, 'x'.repeat(MAX_CAPTION + 1))
  await proof.show(page, target, 'state')
  await proof.tap(page, target, 'tap')
  await proof.type(target, 'name')
  assert.deepEqual(info.annotations, [])
  assert.deepEqual(calls, [['click'], ['fill', 'name']])
})

test('enabled steps record timestamped chapters and stills with default pacing', async () => {
  const { calls, info, page, target, proof } = fixture(true)
  proof.begin()
  await proof.chapter(page, 'Claim')
  await proof.show(page, target, 'Before')
  await proof.tap(page, target, 'Submit')
  await proof.type(target, 'name')
  assert.deepEqual(
    info.annotations.map(({ type }) => type),
    [PROOF_CHAPTER, PROOF_STILL],
  )
  const [chapter, still] = info.annotations.map(({ description }) => JSON.parse(description))
  assert.deepEqual(Object.keys(chapter), ['atMs', 'caption'])
  assert.equal(chapter.caption, 'Claim')
  assert.ok(Number.isSafeInteger(chapter.atMs) && chapter.atMs >= 0)
  assert.equal(still.caption, 'Before')
  assert.equal(still.file, '/results/still-1.png')
  assert.ok(still.atMs >= chapter.atMs)
  assert.deepEqual(
    calls.filter(([kind]) => kind === 'wait'),
    [
      ['wait', 500],
      ['wait', 2500],
      ['wait', 800],
    ],
  )
  assert.deepEqual(calls.at(-1), ['type', 'name', { delay: 120 }])
  assert.ok(calls.some(([kind, options]) => kind === 'screenshot' && options.path === still.file))
})

test('pacing overrides apply and captions fail before capture or click', async () => {
  const { calls, page, target, proof } = fixture(true, {
    leadInMs: 1,
    holdStateMs: 2,
    holdTapMs: 3,
    typeDelayMs: 4,
  })
  await proof.chapter(page, 'Claim')
  await proof.show(page, target, 'State')
  await proof.tap(page, target)
  await proof.type(target, 'text')
  assert.deepEqual(
    calls.filter(([kind]) => kind === 'wait'),
    [
      ['wait', 1],
      ['wait', 2],
      ['wait', 3],
    ],
  )
  assert.deepEqual(calls.at(-1), ['type', 'text', { delay: 4 }])
  const long = 'x'.repeat(MAX_CAPTION + 1)
  const before = calls.length
  await assert.rejects(proof.chapter(page, long), /120 characters/)
  await assert.rejects(proof.show(page, target, long), /120 characters/)
  await assert.rejects(proof.tap(page, target, long), /120 characters/)
  assert.equal(calls.length, before)
})
