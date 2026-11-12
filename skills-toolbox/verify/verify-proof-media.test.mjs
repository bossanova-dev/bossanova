import test from 'node:test'
import assert from 'node:assert/strict'
import {
  validateProofMedia,
  mediaName,
  MAX_CLIPS,
  MAX_CLIP_FILES,
  MAX_DOCUMENT_PAGES,
  MAX_FILE_BYTES,
  MAX_PDF_BYTES,
  MAX_TOTAL_BYTES,
} from './verify-proof-media.mjs'
import { publishContentType } from './verify-publish-hook.mjs'

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
  assert.match(big.dropped[0].reason, /is over 15 MiB$/)
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

test('a base clip pairs with the head clip it names, wherever that clip sits', () => {
  const base = (id, pairs) => ({
    id,
    side: 'base',
    ...(pairs === undefined ? {} : { pairs }),
    title: 'Before',
    file: `${id}.mp4`,
    durationMs: 5_000,
    chapters: [],
    stills: [{ file: `${id}-still.png`, caption: 'Before' }],
  })
  const raw = manifest()
  raw.clips.unshift(base('base-0', 'head-0'))
  raw.clips.push(base('base-1', 'head-9'), base('base-2', 'head-0'), base('base-3'))
  const { media, dropped } = validateProofMedia(raw, sized())
  assert.deepEqual(
    media.clips.map((c) => [c.id, c.pairs]),
    [
      ['base-0', 'head-0'],
      ['head-0', undefined],
    ],
  )
  assert.deepEqual(
    dropped.map((d) => [d.at, d.reason]),
    [
      ['clips[2]', 'pairs: head-9 is not a head clip'],
      ['clips[3]', 'pairs: head-0 already has a base clip'],
      ['clips[4]', 'pairs: expected text'],
    ],
  )
  const headPairs = manifest()
  headPairs.clips[0].pairs = 'head-1'
  assert.deepEqual(validateProofMedia(headPairs, sized()).dropped, [
    { at: 'clips[0]', reason: 'pairs: only a base clip names the head clip it pairs with' },
  ])
})

test('an upper-case extension is dropped, since the upload accepts only the lower-case name', () => {
  const m = manifest()
  m.clips[0].file = 'clips/Head-0.MP4'
  m.clips[0].stills.push({ file: 'clips/Statement.PNG', caption: 'Upper case' })
  m.clips.push({ ...structuredClone(manifest().clips[0]), id: 'head-1', file: 'clips/head-1.mp4' })
  const { media, dropped } = validateProofMedia(m, { head: HEAD })
  assert.deepEqual(dropped, [
    { at: 'clips[0]', reason: 'file: clips/Head-0.MP4 needs a lower-case extension, .mp4' },
  ])
  assert.deepEqual(
    media.clips.map((c) => c.id),
    ['head-1'],
  )
  m.clips[0].file = 'clips/head-0.mp4'
  const still = validateProofMedia(m, { head: HEAD })
  assert.deepEqual(still.dropped, [
    {
      at: 'clips[0].stills[1]',
      reason: 'file: clips/Statement.PNG needs a lower-case extension, .png',
    },
  ])
  const kept = [still.media.clips[0].file, ...still.media.clips[0].stills.map((s) => s.file)]
  for (const file of kept) assert.ok(publishContentType(mediaName(file)), `${file} uploads`)
  assert.equal(publishContentType(mediaName('clips/Statement.PNG')), null, 'the upload refuses it')
})

/** A head statement and its base, three rendered pages each. */
function documents() {
  const doc = (id, side, file, pairs) => ({
    id,
    side,
    ...(pairs ? { pairs } : {}),
    title: 'Monthly statement',
    file: `docs/${file}.pdf`,
    pageCount: 3,
    pages: [1, 2, 3].map((n) => ({
      page: n,
      file: `docs/${file}-p${n}.png`,
      caption: `Page ${n}`,
    })),
  })
  return {
    ...manifest(),
    documents: [
      doc('doc-0', 'head', 'statement-head'),
      doc('doc-0-base', 'base', 'statement-base', 'doc-0'),
    ],
  }
}

test('a manifest without documents validates to no documents key, so old pages are unchanged', () => {
  const { media } = validateProofMedia(manifest(), sized())
  assert.equal(Object.hasOwn(media, 'documents'), false)
})

test('valid documents are kept whole, paired base and head, with nothing dropped', () => {
  const { media, dropped } = validateProofMedia(documents(), sized())
  assert.deepEqual(dropped, [])
  assert.deepEqual(media.documents, documents().documents)
})

test('each document rule drops only its defective item and names why', () => {
  const docs = (/** @type {any} */ k) => k.documents.length
  const pages = (/** @type {any} */ k) => k.documents[0].pages.length
  /** @type {[string, (m: any) => void, string, RegExp, (k: any) => number, number][]} */
  const cases = [
    [
      'base names a head document that does not exist',
      (m) => (m.documents[1].pairs = 'doc-9'),
      'documents[1]',
      /doc-9 is not a head document/,
      docs,
      1,
    ],
    [
      'document file not a pdf',
      (m) => (m.documents[1].file = 'docs/statement-base.docx'),
      'documents[1]',
      /expected one of \.pdf/,
      docs,
      1,
    ],
    [
      'document path escapes the evidence dir',
      (m) => (m.documents[1].file = '../../etc/statement.pdf'),
      'documents[1]',
      /relative path inside the evidence directory/,
      docs,
      1,
    ],
    [
      'document path absolute',
      (m) => (m.documents[1].file = '/tmp/statement.pdf'),
      'documents[1]',
      /relative path inside the evidence directory/,
      docs,
      1,
    ],
    [
      'document path is a URL',
      (m) => (m.documents[1].file = 'https://cdn.example/statement.pdf'),
      'documents[1]',
      /relative path inside the evidence directory/,
      docs,
      1,
    ],
    [
      'two documents share an id',
      (m) => (m.documents[1].id = 'doc-0'),
      'documents[1]',
      /doc-0 appears twice/,
      docs,
      1,
    ],
    [
      'side outside head/base',
      (m) => (m.documents[1].side = 'left'),
      'documents[1]',
      /expected head or base/,
      docs,
      1,
    ],
    ['no page count', (m) => delete m.documents[1].pageCount, 'documents[1]', /pageCount/, docs, 1],
    [
      'a page count of zero',
      (m) => (m.documents[1].pageCount = 0),
      'documents[1]',
      /pageCount/,
      docs,
      1,
    ],
    [
      'page image not an image',
      (m) => (m.documents[0].pages[1].file = 'docs/statement-head-p2.pdf'),
      'documents[0].pages[1]',
      /expected one of \.png/,
      pages,
      2,
    ],
    [
      'page path is a data URL',
      (m) => (m.documents[0].pages[0].file = 'data:image/png;base64,AAAA.png'),
      'documents[0].pages[0]',
      /a file name uses only/,
      pages,
      2,
    ],
    [
      'page caption over 120',
      (m) => (m.documents[0].pages[2].caption = 'x'.repeat(121)),
      'documents[0].pages[2]',
      /longer than 120/,
      pages,
      2,
    ],
  ]
  for (const [name, mutate, at, reason, count, kept] of cases) {
    const raw = documents()
    mutate(raw)
    const { media, dropped } = validateProofMedia(raw, sized())
    assert.equal(dropped.length, 1, name)
    assert.equal(dropped[0].at, at, name)
    assert.match(dropped[0].reason, reason, name)
    assert.equal(count(media), kept, name)
  }
})

test('a document keeps at most 8 pages, and none past its page count', () => {
  const raw = documents()
  const head = raw.documents[0]
  head.pageCount = 12
  head.pages = Array.from({ length: MAX_DOCUMENT_PAGES + 1 }, (_, i) => ({
    page: i + 1,
    file: `docs/statement-head-p${i + 1}.png`,
    caption: `Page ${i + 1}`,
  }))
  raw.documents[1].pageCount = 2
  const { media, dropped } = validateProofMedia(raw, sized())
  assert.equal(media.documents[0].pages.length, MAX_DOCUMENT_PAGES)
  assert.equal(media.documents[1].pages.length, 2)
  assert.deepEqual(
    dropped.map((d) => [d.at, d.reason]),
    [
      ['documents[0].pages[8]', 'over the cap of 8'],
      ['documents[1].pages[2]', 'page: 3 is past pageCount 2'],
    ],
  )
})

test('a PDF over 10 MiB is dropped while page images keep the 15 MB cap, and both count toward the total', () => {
  const pdfSize = (bytes) => ({
    head: HEAD,
    sizeOf: (file) => (file.endsWith('.pdf') ? bytes : 1024),
  })
  const over = validateProofMedia(documents(), pdfSize(MAX_PDF_BYTES + 1))
  assert.deepEqual(
    over.media.documents.map((d) => d.id),
    [],
    'the base is dropped with the head it pairs with',
  )
  assert.deepEqual(
    over.dropped.map((d) => d.at),
    ['documents[0]', 'documents[1]'],
  )
  assert.match(over.dropped[0].reason, /is over 10 MiB$/)
  assert.equal(validateProofMedia(documents(), pdfSize(MAX_PDF_BYTES)).dropped.length, 0)
  const page = validateProofMedia(documents(), {
    head: HEAD,
    sizeOf: (file) => (file.endsWith('-p2.png') ? MAX_FILE_BYTES + 1 : 1024),
  })
  assert.deepEqual(
    page.dropped.map((d) => d.at),
    ['documents[0].pages[1]', 'documents[1].pages[1]'],
  )
  const total = validateProofMedia(documents(), {
    head: HEAD,
    sizeOf: (file) => (file.endsWith('.pdf') ? MAX_PDF_BYTES : Math.floor(MAX_TOTAL_BYTES / 5)),
  })
  assert.ok(
    total.dropped.some((d) => /takes the media over/.test(d.reason)),
    'document files count toward the 60 MB total',
  )
})

test('numbered pages sort by number, and one numbered 0, repeated or past the count drops alone', () => {
  const raw = documents()
  const page = (n, caption = `Page ${n}`) => ({
    page: n,
    file: `docs/h-p${n}-${caption.length}.png`,
    caption,
  })
  raw.documents[0].pages = [page(3), page(1, ' '), page(1, 'Again'), page(4), page(0)]
  const { media, dropped } = validateProofMedia(raw, sized())
  assert.deepEqual(
    media.documents[0].pages.map((p) => [p.page, p.caption]),
    [
      [1, 'Monthly statement page 1'],
      [3, 'Page 3'],
    ],
    'kept pages sort by number, and a blank caption names the document and page',
  )
  assert.deepEqual(
    dropped.map((d) => [d.at, d.reason]),
    [
      ['documents[0].pages[2]', 'page: 1 appears twice'],
      ['documents[0].pages[3]', 'page: 4 is past pageCount 3'],
      ['documents[0].pages[4]', 'page: expected a page number'],
    ],
  )
})

test('a base whose head is missing or dropped never counts toward the 60 MiB total', () => {
  const MiB = 1024 * 1024
  const clip = (id, side, pairs) => ({
    id,
    side,
    ...(pairs ? { pairs } : {}),
    title: id,
    file: `${id}.mp4`,
    durationMs: 5_000,
    chapters: [],
    stills: [],
  })
  const doc = (id, side, pairs) => ({
    id,
    side,
    ...(pairs ? { pairs } : {}),
    title: id,
    file: `${id}.pdf`,
    pageCount: 1,
    pages: [],
  })
  const raw = {
    schema: 'proof-media/1',
    head: HEAD,
    clips: [
      clip('orphan', 'base', 'head-9'),
      { ...clip('bad-head', 'head'), durationMs: 0 },
      clip('of-bad-head', 'base', 'bad-head'),
      clip('head-0', 'head'),
      clip('head-1', 'head'),
      clip('head-1-base', 'base', 'head-1'),
      clip('second-base', 'base', 'head-1'),
    ],
    transcripts: [],
    documents: [doc('doc-orphan', 'base', 'doc-9'), doc('doc-0', 'head')],
  }
  const sizes = {
    'orphan.mp4': 14 * MiB,
    'of-bad-head.mp4': 14 * MiB,
    'second-base.mp4': 14 * MiB,
    'doc-orphan.pdf': 10 * MiB,
    'head-0.mp4': 14 * MiB,
    'head-1.mp4': 14 * MiB,
    'head-1-base.mp4': 14 * MiB,
    'doc-0.pdf': 10 * MiB,
  }
  const { media, dropped } = validateProofMedia(raw, { head: HEAD, sizeOf: (f) => sizes[f] })
  assert.deepEqual(
    media.clips.map((c) => c.id),
    ['head-0', 'head-1', 'head-1-base'],
  )
  assert.deepEqual(
    media.documents.map((d) => d.id),
    ['doc-0'],
    '52 MiB of kept files fit, whatever the dropped bases weighed',
  )
  assert.ok(!dropped.some((d) => /takes the media over/.test(d.reason)), JSON.stringify(dropped))
})

test('pages with no numbers pair by position, and a dropped one leaves its position empty', () => {
  const raw = documents()
  for (const doc of raw.documents) for (const page of doc.pages) delete page.page
  raw.documents[0].pages[1].file = 'docs/statement-head-p2.pdf'
  raw.documents[0].pages[2].caption = ''
  const { media, dropped } = validateProofMedia(raw, sized())
  assert.deepEqual(
    media.documents[0].pages.map((p) => [p.page, p.caption]),
    [
      [1, 'Page 1'],
      [3, 'Monthly statement page 3'],
    ],
    'the third page keeps position 3 after the second is dropped',
  )
  assert.deepEqual(
    media.documents[1].pages.map((p) => p.page),
    [1, 2, 3],
  )
  assert.deepEqual(
    dropped.map((d) => d.at),
    ['documents[0].pages[1]'],
  )
})

test('a page list that numbers some pages and not others is dropped whole', () => {
  const raw = documents()
  delete raw.documents[0].pages[1].page
  const { media, dropped } = validateProofMedia(raw, sized())
  assert.deepEqual(media.documents[0].pages, [], 'the document stays, with no pages')
  assert.deepEqual(media.documents[1].pages.length, 3)
  assert.deepEqual(dropped, [
    {
      at: 'documents[0].pages',
      reason: 'page: either every page names its page number or none does',
    },
  ])
})

test('a clip keeps the changed files it proves, and an empty or oversized list drops the clip', () => {
  const withFiles = (files) => {
    const m = manifest()
    m.clips[0] = { ...m.clips[0], files }
    return validateProofMedia(m, sized())
  }
  assert.deepEqual(withFiles(['src/cart.ts']).media?.clips[0].files, ['src/cart.ts'])
  for (const files of [[], Array(MAX_CLIP_FILES + 1).fill('a.ts'), 'src/cart.ts'])
    assert.deepEqual(withFiles(files).dropped, [
      { at: 'clips[0]', reason: `files: expected 1 to ${MAX_CLIP_FILES} paths` },
    ])
  assert.equal(withFiles([' ']).dropped[0]?.at, 'clips[0]')
})
