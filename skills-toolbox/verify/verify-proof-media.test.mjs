import test from 'node:test'
import assert from 'node:assert/strict'
import {
  validateProofMedia,
  mediaName,
  MAX_CLIPS,
  MAX_FILE_BYTES,
  MAX_TOTAL_BYTES,
} from './verify-proof-media.mjs'

const HEAD = 'e'.repeat(40)

function manifest() {
  return {
    schema: 'proof-media/1',
    head: HEAD,
    source: { kind: 'ci-artifact', url: 'https://ci.example/runs/7' },
    clips: [
      {
        id: 'head-0',
        side: 'head',
        title: 'The basket shows its total',
        file: 'clips/head-0.mp4',
        poster: 'clips/head-0.png',
        durationMs: 12_000,
        chapters: [
          { atMs: 1_000, caption: 'Empty basket' },
          { atMs: 6_000, caption: 'Two items and a total' },
        ],
        stills: [{ file: 'clips/head-0-still-0.png', caption: 'The total', atMs: 6_500 }],
      },
    ],
    transcripts: [
      {
        title: 'Totals are stored',
        note: 'Against the test API.',
        status: 200,
        ms: 41,
        request: { q: 1 },
        response: { ok: true },
        rows: 'id | total',
      },
    ],
  }
}
const sized = (bytes = 1024) => ({ head: HEAD, sizeOf: () => bytes })

test('a valid manifest is kept whole with nothing dropped', () => {
  const { media, dropped } = validateProofMedia(manifest(), sized())
  assert.deepEqual(dropped, [])
  assert.deepEqual(media, manifest())
  assert.equal(mediaName('clips/head-0.mp4'), 'media/head-0.mp4')
})

test('each rule drops only its defective item and names why', () => {
  /** @type {[string, (m: any) => void, string, (k: any) => number][]} */
  const cases = [
    ['clip at 60 s', (m) => (m.clips[0].durationMs = 60_000), 'clips[0]', (k) => k.clips.length],
    [
      'clip not an mp4',
      (m) => (m.clips[0].file = 'clips/head-0.mov'),
      'clips[0]',
      (k) => k.clips.length,
    ],
    [
      'clip side outside head/base',
      (m) => (m.clips[0].side = 'left'),
      'clips[0]',
      (k) => k.clips.length,
    ],
    [
      'clip path escapes the evidence dir',
      (m) => (m.clips[0].file = '../secret.mp4'),
      'clips[0]',
      (k) => k.clips.length,
    ],
    [
      'still name the page would mis-address',
      (m) => (m.clips[0].stills[0].file = 'clips/a#b.png'),
      'clips[0].stills[0]',
      (k) => k.clips[0].stills.length,
    ],
    [
      'clip path absolute',
      (m) => (m.clips[0].file = '/tmp/a.mp4'),
      'clips[0]',
      (k) => k.clips.length,
    ],
    [
      'clip title over 120',
      (m) => (m.clips[0].title = 'x'.repeat(121)),
      'clips[0]',
      (k) => k.clips.length,
    ],
    [
      'chapter after the clip ends',
      (m) => (m.clips[0].chapters[1].atMs = 12_000),
      'clips[0].chapters[1]',
      (k) => k.clips[0].chapters.length - 1,
    ],
    [
      'chapters out of order',
      (m) => (m.clips[0].chapters[1].atMs = 500),
      'clips[0].chapters[1]',
      (k) => k.clips[0].chapters.length - 1,
    ],
    [
      'chapter caption over 120',
      (m) => (m.clips[0].chapters[0].caption = 'x'.repeat(121)),
      'clips[0].chapters[0]',
      (k) => k.clips[0].chapters.length - 1,
    ],
    [
      'still not an image',
      (m) => (m.clips[0].stills[0].file = 'clips/a.gif'),
      'clips[0].stills[0]',
      (k) => k.clips[0].stills.length,
    ],
    [
      'still after the clip ends',
      (m) => (m.clips[0].stills[0].atMs = 13_000),
      'clips[0].stills[0]',
      (k) => k.clips[0].stills.length,
    ],
    [
      'poster not an image',
      (m) => (m.clips[0].poster = 'clips/head-0.mp4'),
      'clips[0].poster',
      (k) => (k.clips[0].poster ? 1 : 0),
    ],
    [
      'transcript status not HTTP',
      (m) => (m.transcripts[0].status = 42),
      'transcripts[0]',
      (k) => k.transcripts.length,
    ],
    [
      'transcript response too large',
      (m) => (m.transcripts[0].response = { big: 'x'.repeat(40_000) }),
      'transcripts[0]',
      (k) => k.transcripts.length,
    ],
    [
      'source not https',
      (m) => (m.source.url = 'http://ci.example/runs/7'),
      'media.source',
      (k) => (k.source ? 1 : 0),
    ],
    ['unknown top-level key', (m) => (m.extra = true), 'media.extra', () => 0],
  ]
  for (const [name, mutate, at, count] of cases) {
    const raw = manifest()
    mutate(raw)
    const { media, dropped } = validateProofMedia(raw, sized())
    assert.deepEqual(
      dropped.map((d) => d.at),
      [at],
      name,
    )
    assert.equal(count(media), 0, name)
  }
})

test('size caps drop a file over 15 MB and the files that take the total over 60 MB', () => {
  const big = validateProofMedia(manifest(), sized(MAX_FILE_BYTES + 1))
  assert.equal(big.media.clips.length, 0)
  assert.match(big.dropped[0].reason, /over 15728640 bytes/)
  const raw = manifest()
  raw.clips = Array.from({ length: 5 }, (_, i) => ({
    ...manifest().clips[0],
    id: `head-${i}`,
    file: `head-${i}.mp4`,
    poster: undefined,
    stills: [],
  }))
  const each = Math.floor(MAX_TOTAL_BYTES / 4)
  const { media, dropped } = validateProofMedia(JSON.parse(JSON.stringify(raw)), sized(each))
  assert.deepEqual(
    media.clips.map((c) => c.id),
    ['head-0', 'head-1', 'head-2', 'head-3'],
  )
  assert.deepEqual(
    dropped.map((d) => d.at),
    ['clips[4]'],
  )
  const missing = validateProofMedia(manifest(), { head: HEAD, sizeOf: () => null })
  assert.equal(missing.media.clips.length, 0)
  assert.match(missing.dropped[0].reason, /missing/)
})

test('clips over the cap and two files sharing a published name are dropped', () => {
  const raw = manifest()
  raw.clips = Array.from({ length: MAX_CLIPS + 2 }, (_, i) => ({
    ...manifest().clips[0],
    id: `head-${i}`,
    file: `head-${i}.mp4`,
    poster: `head-${i}.png`,
    stills: [],
  }))
  raw.clips[1].file = 'other/head-0.mp4'
  const { media, dropped } = validateProofMedia(raw, sized())
  assert.equal(media.clips.length, MAX_CLIPS)
  assert.deepEqual(
    dropped.map((d) => d.at),
    ['clips[1]', 'clips[7]'],
  )
  assert.match(dropped[0].reason, /media\/head-0\.mp4 is already published/)
})

test('a manifest for another head or of another schema is ignored whole, and garbage never throws', () => {
  for (const raw of [
    { ...manifest(), head: 'f'.repeat(40) },
    { ...manifest(), schema: 'x' },
    null,
    7,
  ]) {
    const { media, dropped } = validateProofMedia(raw, sized())
    assert.equal(media, null)
    assert.equal(dropped.length, 1)
  }
  for (const value of [undefined, null, 0, 'x', [], {}, [null], [{ chapters: {} }]])
    for (const key of ['clips', 'transcripts', 'source']) {
      assert.doesNotThrow(() => validateProofMedia({ ...manifest(), [key]: value }, sized()))
      const raw = manifest()
      raw.clips[0].chapters = value
      raw.clips[0].stills = value
      assert.doesNotThrow(() => validateProofMedia(raw, sized()))
    }
})
