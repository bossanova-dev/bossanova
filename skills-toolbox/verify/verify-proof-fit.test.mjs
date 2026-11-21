import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mediaFiles } from './verify-media-hook.mjs'
import { fitProofMedia, fixedSize, measure, publishWithinLimits } from './verify-proof-fit.mjs'

const HEAD = 'c'.repeat(40)

/** A clip of `file`, optionally with a poster, stills and a head clip it pairs with. */
const clip = (id, file, { side = 'head', pairs, poster, stills = [] } = {}) => ({
  id,
  side,
  ...(pairs ? { pairs } : {}),
  title: `Clip ${id}`,
  file,
  ...(poster ? { poster } : {}),
  durationMs: 5_000,
  chapters: [],
  stills: stills.map((file) => ({ file, caption: `still ${file}` })),
})
const doc = (id, file, pages) => ({
  id,
  side: 'head',
  title: `Doc ${id}`,
  file,
  pageCount: pages.length,
  pages: pages.map((file, i) => ({ page: i + 1, file, caption: `page ${i + 1}` })),
})
const manifest = ({ clips = [], documents, transcripts = [] }) => ({
  schema: 'proof-media/1',
  head: HEAD,
  clips,
  transcripts,
  ...(documents ? { documents } : {}),
})
const sizes = (map) => (file) => map[file] ?? null
const FIXED = { count: 2, bytes: 10, largest: 8 }

// Six files: clip a (a.mp4 + a.png poster + a1.png still), clip b, document d with one page.
const SIZES = { 'a.mp4': 100, 'a.png': 10, 'a1.png': 5, 'b.mp4': 300, 'd.pdf': 50, 'd1.png': 20 }
const MEDIA = manifest({
  clips: [clip('a', 'a.mp4', { poster: 'a.png', stills: ['a1.png'] }), clip('b', 'b.mp4')],
  documents: [doc('d', 'd.pdf', ['d1.png'])],
})
const fit = (limits, media = MEDIA, map = SIZES, fixed = FIXED) =>
  fitProofMedia({ media, sizeOf: sizes(map), head: HEAD, fixed, limits })
const kept = (result) => (result.media ? mediaFiles(result.media) : [])

test('file-count drops whole units, largest first, until the reported count holds', () => {
  assert.deepEqual(measure(MEDIA, sizes(SIZES), FIXED), { count: 8, bytes: 495, largest: 300 })
  const seven = fit([{ kind: 'file-count', max: 7 }])
  assert.deepEqual([seven.ok, seven.omitted], [true, 1])
  assert.deepEqual(kept(seven), ['a.mp4', 'a.png', 'a1.png', 'd.pdf', 'd1.png'])
  // A clip goes with its poster and stills, so a smaller maximum drops three more files.
  const six = fit([{ kind: 'file-count', max: 6 }])
  assert.deepEqual([six.omitted, kept(six)], [4, ['d.pdf', 'd1.png']])
  assert.equal(fit([{ kind: 'file-count', max: 8 }]).omitted, 0, 'a page that fits is unchanged')
})

test('page-bytes drops the largest units until the page total holds', () => {
  const roomy = fit([{ kind: 'page-bytes', max: 200 }])
  assert.deepEqual([roomy.omitted, kept(roomy).includes('b.mp4')], [1, false])
  const tight = fit([{ kind: 'page-bytes', max: 150 }])
  assert.deepEqual([tight.omitted, kept(tight)], [4, ['d.pdf', 'd1.png']])
  // A still is its own unit: only it goes when it alone breaks the total.
  const still = fit([{ kind: 'page-bytes', max: 300 }], MEDIA, {
    ...SIZES,
    'a1.png': 200,
    'b.mp4': 1,
  })
  assert.deepEqual(
    [still.omitted, kept(still)],
    [1, ['a.mp4', 'a.png', 'b.mp4', 'd.pdf', 'd1.png']],
  )
})

test('file-bytes drops every unit that holds an over-max file first, whatever its rank', () => {
  const map = {
    'c.mp4': 80,
    'e.pdf': 60,
    ...Object.fromEntries([1, 2, 3, 4, 5].map((n) => [`e${n}.png`, 60])),
  }
  const media = manifest({
    clips: [clip('c', 'c.mp4')],
    documents: [doc('e', 'e.pdf', ['e1.png', 'e2.png', 'e3.png', 'e4.png', 'e5.png'])],
  })
  const result = fit([{ kind: 'file-bytes', max: 60 }], media, map)
  assert.deepEqual([result.omitted, kept(result).includes('c.mp4')], [1, false])
  assert.equal(kept(result).length, 6, 'the larger document under the cap stays')
  const lower = fit([{ kind: 'file-bytes', max: 59 }], media, map)
  assert.deepEqual([lower.ok, lower.media, lower.omitted], [true, null, 7])
})

test('a dropped head clip takes its paired base clip, and both count as omitted', () => {
  const media = manifest({
    clips: [
      clip('h', 'h.mp4'),
      clip('k', 'k.mp4'),
      clip('base', 'base.mp4', { side: 'base', pairs: 'h' }),
    ],
  })
  const result = fit([{ kind: 'page-bytes', max: 100 }], media, {
    'h.mp4': 500,
    'k.mp4': 40,
    'base.mp4': 30,
  })
  assert.deepEqual([result.omitted, kept(result)], [2, ['k.mp4']])
})

test('a paired clip loses a still together with its pair, so later stills keep their pairs', () => {
  const media = manifest({
    clips: [
      clip('h', 'h.mp4', { stills: ['h1.png', 'h2.png', 'h3.png'] }),
      clip('base', 'base.mp4', {
        side: 'base',
        pairs: 'h',
        stills: ['b1.png', 'b2.png', 'b3.png'],
      }),
    ],
  })
  const map = {
    'h.mp4': 10,
    'base.mp4': 10,
    'h1.png': 1,
    'h2.png': 1,
    'h3.png': 1,
    'b1.png': 1,
    'b2.png': 200,
    'b3.png': 1,
  }
  const result = fit([{ kind: 'file-bytes', max: 100 }], media, map)
  const stills = (id) => result.media?.clips.find((c) => c.id === id)?.stills.map((s) => s.file)
  assert.deepEqual(
    [result.omitted, stills('h'), stills('base')],
    [2, ['h1.png', 'h3.png'], ['b1.png', 'b3.png']],
  )
})

test('ties go to the later unit, and transcripts are kept when every file goes', () => {
  const media = manifest({ clips: [clip('x', 'x.mp4'), clip('y', 'y.mp4')] })
  const tie = fit([{ kind: 'file-count', max: 3 }], media, { 'x.mp4': 9, 'y.mp4': 9 })
  assert.deepEqual(kept(tie), ['x.mp4'])
  const transcript = { title: 'Saved', status: 200, ms: 3, request: {}, response: {} }
  const withCall = fit(
    [{ kind: 'file-count', max: 2 }],
    { ...media, transcripts: [transcript] },
    { 'x.mp4': 9, 'y.mp4': 9 },
  )
  assert.deepEqual(
    [withCall.media?.clips, withCall.media?.transcripts.length, withCall.omitted],
    [[], 1, 2],
  )
  assert.equal(fit([{ kind: 'file-count', max: 2 }], media, { 'x.mp4': 9, 'y.mp4': 9 }).media, null)
})

test('nothing left to drop is a failed fit', () => {
  assert.deepEqual(fit([{ kind: 'file-bytes', max: 7 }]), { ok: false }, 'index.html alone is over')
  assert.deepEqual(fit([{ kind: 'file-count', max: 1 }]), { ok: false })
})

test('the fixed size counts index.html and every non-media asset', () => {
  assert.deepEqual(fixedSize('<p>é</p>', [{ text: 'player()' }]), {
    count: 2,
    bytes: 17,
    largest: 9,
  })
})

/** publishWithinLimits over fakes: `answers` are the upload results in order. */
async function loop(answers, { scan = () => false, render, media = MEDIA, map = SIZES } = {}) {
  const uploads = []
  const renders = []
  const result = await publishWithinLimits({
    upload: async (html, kept) => {
      uploads.push({ html, files: kept ? mediaFiles(kept) : [] })
      const answer = answers[uploads.length - 1]
      if (answer instanceof Error) throw answer
      return answer ?? { ok: true, url: 'https://proof.example/page' }
    },
    render: (kept, omitted) => {
      renders.push(omitted)
      return render ? render(kept, omitted) : `<p>page omitting ${omitted}</p>`
    },
    scan,
    html: '<p>page</p>',
    media,
    sizeOf: sizes(map),
    head: HEAD,
    fixedFor: (html) => fixedSize(html, [{ text: 'player' }]),
  })
  return { result, uploads, renders }
}
const over = (kind, max) => ({ ok: false, reason: 'over-limit', limit: { kind, max } })

test('the loop uploads once and renders nothing when the first upload succeeds @sessions.AC261201', async () => {
  const { result, uploads, renders } = await loop([])
  assert.deepEqual(result, {
    uploaded: { ok: true, url: 'https://proof.example/page' },
    html: '<p>page</p>',
    omitted: 0,
  })
  assert.deepEqual([uploads.length, renders.length], [1, 0])
})

test('an over-limit refusal is fitted, rendered with the omitted count, scanned and retried @sessions.AC261201', async () => {
  const scanned = []
  const { result, uploads, renders } = await loop([over('file-count', 7)], {
    scan: (html) => (scanned.push(html), false),
  })
  assert.equal(result.uploaded.ok, true)
  assert.deepEqual([result.html, result.omitted], ['<p>page omitting 1</p>', 1])
  assert.deepEqual(scanned, ['<p>page omitting 1</p>'])
  assert.deepEqual(renders, [1])
  assert.deepEqual(uploads[1].files, ['a.mp4', 'a.png', 'a1.png', 'd.pdf', 'd1.png'])
  // A second, different limit keeps the first fit and adds its own.
  const twice = await loop([over('file-count', 7), over('page-bytes', 150)])
  assert.equal(twice.uploads.length, 3)
  assert.deepEqual(
    [twice.result.uploaded.ok, twice.result.omitted, twice.uploads[2].files],
    [true, 4, ['d.pdf', 'd1.png']],
  )
})

test('a re-rendered page that breaks the limit is fitted again before it is uploaded @sessions.AC261201', async () => {
  // The note grows the page past the first fit, so a second fit drops the document as well.
  const { result, uploads } = await loop([over('page-bytes', 150)], {
    render: (_kept, omitted) => `<p>${'n'.repeat(33)}${omitted}</p>`,
    map: { ...SIZES, 'b.mp4': 40 },
  })
  assert.deepEqual([result.uploaded.ok, result.omitted, uploads[1].files], [true, 5, ['b.mp4']])
  const size = measure(null, sizes({}), fixedSize(result.html, [{ text: 'player' }]))
  const media = uploads[1].files.reduce((n, file) => n + { ...SIZES, 'b.mp4': 40 }[file], 0)
  assert.ok(size.bytes + media <= 150, `the uploaded page meets the limit: ${size.bytes + media}`)
})

test('a secret in the re-rendered page stops the loop with no upload @sessions.AC261201', async () => {
  const { result, uploads } = await loop([over('file-count', 7)], { scan: () => true })
  assert.deepEqual(result.uploaded, { ok: false, reason: 'page-contains-secret' })
  assert.equal(uploads.length, 1)
})

test('the loop stops with over-limit when nothing is left or the host repeats a limit @sessions.AC261201', async () => {
  const nothing = await loop([over('page-bytes', 5)])
  assert.deepEqual(
    [nothing.result.uploaded, nothing.uploads.length],
    [{ ok: false, reason: 'over-limit' }, 1],
  )
  const repeated = await loop([over('file-count', 7), over('file-count', 7)])
  assert.deepEqual(
    [repeated.result.uploaded, repeated.uploads.length],
    [{ ok: false, reason: 'over-limit' }, 2],
  )
})

test('any other refusal is returned unchanged, unfitted @sessions.AC261201', async () => {
  for (const refusal of [
    { ok: false, reason: 'quota-exhausted' },
    { ok: false, reason: 'over-limit' },
    { ok: false, reason: 'hook-failed:exit:2' },
  ]) {
    const { result, uploads, renders } = await loop([refusal])
    assert.deepEqual([result.uploaded, uploads.length, renders.length], [refusal, 1, 0])
  }
  const thrown = await loop([new Error('boom')])
  assert.deepEqual(thrown.result.uploaded, { ok: false, reason: 'upload-failed' })
})
