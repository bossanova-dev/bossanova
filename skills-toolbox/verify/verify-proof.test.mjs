import test from 'node:test'
import assert from 'node:assert/strict'
import * as p from './verify-proof.mjs'

function fixture() {
  return p.withDigest({
    schema: p.PROOF_SCHEMA,
    repo: 'owner/repo',
    pr: 1,
    head: 'a'.repeat(40),
    base: 'b'.repeat(40),
    confidence: {
      level: 'moderate',
      asks: ['ui'],
      loweredBy: [{ code: 'needs-review', text: 'Review needed' }],
    },
    whatItDoes: { source: 'issue', bullets: ['Shows a change'] },
    areas: [
      {
        name: 'ui',
        label: 'UI',
        ask: true,
        items: ['A change'],
        undisclosed: false,
        media: [
          {
            kind: 'video',
            url: 'https://example.test/video',
            poster: 'https://example.test/poster',
            caption: 'Preview',
            durationMs: 1,
          },
        ],
        proven: false,
      },
    ],
    review: {
      models: [{ agent: 'reviewer', outcome: 'ran', detail: 'Complete' }],
      blockers: [],
      notes: ['Reviewed'],
    },
    health: [{ code: 'healthy', text: 'Healthy' }],
    page: 'https://example.test/proof',
  })
}

test('accepts complete record', () => assert.equal(p.validateProof(fixture()).ok, true))

function changed(edit) {
  const proof = fixture()
  edit(proof)
  return p.withDigest(proof)
}

const invalidCases = {
  level: (x) => {
    x.confidence.level = 'unknown'
  },
  outcome: (x) => {
    x.review.models[0].outcome = 'unknown'
  },
  kind: (x) => {
    x.areas[0].media[0].kind = 'unknown'
  },
  source: (x) => {
    x.whatItDoes.source = 'invented'
  },
  schema: (x) => {
    x.schema = 'future/2'
  },
  topKey: (x) => {
    x.extra = true
  },
  areaKey: (x) => {
    x.areas[0].extra = true
  },
  confidenceKey: (x) => {
    x.confidence.extra = true
  },
  intentKey: (x) => {
    x.whatItDoes.extra = true
  },
  mediaKey: (x) => {
    x.areas[0].media[0].extra = true
  },
  reviewKey: (x) => {
    x.review.extra = true
  },
  modelKey: (x) => {
    x.review.models[0].extra = true
  },
  blockerKey: (x) => {
    x.review.blockers = [{ title: 'Blocked', extra: true }]
    x.confidence.level = 'blocked'
  },
  loweredKey: (x) => {
    x.confidence.loweredBy[0].extra = true
  },
  healthKey: (x) => {
    x.health[0].extra = true
  },
  duplicateArea: (x) => {
    x.areas.push(structuredClone(x.areas[0]))
    x.confidence.asks.push('ui')
  },
  floatPr: (x) => {
    x.pr = 1.5
  },
  zeroPr: (x) => {
    x.pr = 0
  },
  floatDuration: (x) => {
    x.areas[0].media[0].durationMs = 1.5
  },
  negativeDuration: (x) => {
    x.areas[0].media[0].durationMs = -1
  },
  sha: (x) => {
    x.head = 'abc'
  },
  repo: (x) => {
    x.repo = 'owner/repo/other'
  },
  areaId: (x) => {
    x.areas[0].name = 'Bad'
    x.confidence.asks = ['Bad']
  },
  loweredId: (x) => {
    x.confidence.loweredBy[0].code = 'Bad'
  },
  healthId: (x) => {
    x.health[0].code = 'Bad'
  },
  asks: (x) => {
    x.confidence.asks = []
  },
  asksOrder: (x) => {
    x.areas.push({ ...structuredClone(x.areas[0]), name: 'other' })
    x.confidence.asks = ['other', 'ui']
  },
  blockedNoBlockers: (x) => {
    x.confidence.level = 'blocked'
  },
  blockersNotBlocked: (x) => {
    x.review.blockers.push({ title: 'Blocked' })
  },
  highAsk: (x) => {
    x.confidence.level = 'high'
    x.confidence.loweredBy = []
  },
  highLowered: (x) => {
    x.confidence.level = 'high'
    x.confidence.asks = []
    x.areas[0].ask = false
  },
  highNull: (x) => {
    x.confidence.level = 'high'
    x.confidence.asks = []
    x.confidence.loweredBy = []
    x.areas[0].ask = false
    x.review = null
  },
  moderateNull: (x) => {
    x.review = null
  },
  noReasons: (x) => {
    x.confidence.loweredBy = []
  },
  unknownWithBullets: (x) => {
    x.whatItDoes.source = 'unknown'
  },
  knownWithoutBullets: (x) => {
    x.whatItDoes.bullets = []
  },
  shortOverflow: (x) => {
    x.areas[0].items[0] = 'a'.repeat(p.MAX_TEXT_CHARS + 1)
  },
  missingRequired: (x) => {
    delete x.areas[0].ask
  },
  wrongBool: (x) => {
    x.areas[0].undisclosed = 'false'
  },
}
for (const [name, edit] of Object.entries(invalidCases)) {
  test(`rejects ${name}`, () => {
    const record = fixture()
    edit(record)
    const result = p.validateProof(
      ['floatPr', 'floatDuration'].includes(name) ? record : p.withDigest(record),
    )
    assert.equal(result.ok, false)
    assert.equal(result.proof, null)
    assert.ok(result.errors.length)
  })
}

for (const level of p.LEVELS) {
  test(`accepts consistent ${level}`, () => {
    const record = changed((x) => {
      x.confidence.level = level
      if (level === 'blocked') x.review.blockers = [{ title: 'Blocker' }]
      if (level === 'cannot-assess') x.review = null
      if (level === 'high') {
        x.confidence.asks = []
        x.areas[0].ask = false
        x.confidence.loweredBy = []
      }
    })
    assert.equal(p.validateProof(record).ok, true)
  })
}

const badUrls = [
  'http://example.test',
  'javascript:alert(1)',
  'https://user:pass@example.test',
  'https://@example.test',
  'https://example.test/a b',
  'https://example.test/\n',
  'https:example.test',
  'https:///',
]
for (const value of badUrls) {
  for (const field of ['page', 'url', 'poster']) {
    test(`rejects ${field} URL ${JSON.stringify(value)}`, () => {
      const record = changed((x) => {
        if (field === 'page') x.page = value
        else x.areas[0].media[0][field] = value
      })
      assert.equal(p.validateProof(record).ok, false)
    })
  }
}
test('accepts URL and short-string limits, rejects URL overflow', () => {
  const prefix = 'https://example.test/'
  const record = changed((x) => {
    x.page = prefix + 'a'.repeat(p.MAX_URL_CHARS - prefix.length)
    x.areas[0].items[0] = 'a'.repeat(p.MAX_TEXT_CHARS)
  })
  assert.equal(p.validateProof(record).ok, true)
  record.page += 'a'
  assert.equal(p.validateProof(p.withDigest(record)).ok, false)
})

const caps = [
  [
    p.MAX_BULLETS,
    (x, n) => {
      x.whatItDoes.bullets = Array(n).fill('Bullet')
    },
  ],
  [
    p.MAX_AREAS,
    (x, n) => {
      x.areas = Array.from({ length: n }, (_, i) => ({
        ...structuredClone(x.areas[0]),
        name: `area-${i}`,
        ask: false,
      }))
      x.confidence.asks = []
    },
  ],
  [
    p.MAX_ITEMS,
    (x, n) => {
      x.areas[0].items = Array(n).fill('Item')
    },
  ],
  [
    p.MAX_MEDIA,
    (x, n) => {
      x.areas[0].media = Array(n).fill(x.areas[0].media[0])
    },
  ],
  [
    p.MAX_LOWERED_BY,
    (x, n) => {
      x.confidence.loweredBy = Array(n).fill(x.confidence.loweredBy[0])
    },
  ],
  [
    p.MAX_MODELS,
    (x, n) => {
      x.review.models = Array(n).fill(x.review.models[0])
    },
  ],
  [
    p.MAX_BLOCKERS,
    (x, n) => {
      x.review.blockers = Array(n).fill({ title: 'Blocker' })
      x.confidence.level = 'blocked'
    },
  ],
  [
    p.MAX_NOTES,
    (x, n) => {
      x.review.notes = Array(n).fill('Note')
    },
  ],
  [
    p.MAX_HEALTH,
    (x, n) => {
      x.health = Array(n).fill(x.health[0])
    },
  ],
]
for (const [i, [cap, edit]] of caps.entries()) {
  test(`array limit ${i} accepts cap and rejects cap + 1`, () => {
    assert.equal(p.validateProof(changed((x) => edit(x, cap))).ok, true)
    assert.equal(p.validateProof(changed((x) => edit(x, cap + 1))).ok, false)
  })
}

test('rejects oversized total canonical record within individual limits', () => {
  const record = changed((x) => {
    x.areas = Array.from({ length: p.MAX_AREAS }, (_, i) => ({
      ...structuredClone(x.areas[0]),
      name: `area-${i}`,
      ask: false,
      items: Array(p.MAX_ITEMS).fill('a'.repeat(p.MAX_TEXT_CHARS)),
    }))
    x.confidence.asks = []
  })
  assert.ok(Buffer.byteLength(p.canonicalJson(record)) > p.MAX_PROOF_BYTES)
  assert.equal(p.validateProof(record).ok, false)
})

test('digest pins nested contents and is independent of key order', () => {
  const record = fixture()
  const reversed = Object.fromEntries(Object.entries(record).reverse())
  reversed.areas = reversed.areas.map((area) => Object.fromEntries(Object.entries(area).reverse()))
  assert.equal(p.proofDigest(record), p.proofDigest(reversed))
  assert.deepEqual(p.withDigest(record), record)
  record.areas[0].items[0] += 'x'
  assert.notEqual(p.proofDigest(record), record.digest)
  assert.equal(p.validateProof(record).ok, false)
  assert.equal(p.validateProof(p.withDigest(record)).ok, true)
  assert.equal(
    p.canonicalJson({ z: [true, null, 1], a: { z: 'x', a: false } }),
    '{"a":{"a":false,"z":"x"},"z":[true,null,1]}',
  )
})

test('canonical JSON rejects non-JSON values, cycles and unsafe integers', () => {
  const cyclic = {}
  cyclic.self = cyclic
  for (const value of [
    1.5,
    Infinity,
    NaN,
    undefined,
    () => {},
    1n,
    Number.MAX_SAFE_INTEGER + 1,
    new Date(),
    cyclic,
    Array(1),
  ])
    assert.throws(() => p.canonicalJson(value))
  for (const value of [null, [], 'text', {}, { ...fixture(), pr: NaN }])
    assert.equal(p.validateProof(value).ok, false)
})

test('marker contains exactly the compact summary and escapes HTML characters', () => {
  const record = changed((x) => {
    x.page = 'https://example.test/--><&'
    x.areas[0].items = ['--><&']
    x.areas[0].media[0].caption = '--><&'
  })
  const line = p.proofLine(record)
  assert.ok(line.startsWith(p.PROOF_MARKER))
  assert.ok(line.endsWith(' -->'))
  assert.equal(line.slice(0, -4).includes('-->'), false)
  assert.equal(line.slice(p.PROOF_MARKER.length, -4).includes('<'), false)
  assert.equal(line.slice(p.PROOF_MARKER.length, -4).includes('&'), false)
  assert.deepEqual(JSON.parse(line.slice(p.PROOF_MARKER.length, -4)), {
    schema: record.schema,
    head: record.head,
    page: record.page,
    level: record.confidence.level,
    asks: record.confidence.asks,
    digest: record.digest,
  })
  assert.throws(() => p.proofLine({ ...record, digest: 'bad' }))
})

import { mkdtempSync, writeFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'

test('CLI validates, renders markers and rejects malformed input and unknown flags', () => {
  const dir = mkdtempSync(join(tmpdir(), 'verify-proof-'))
  try {
    const file = join(dir, 'proof.json')
    const cli = fileURLToPath(new URL('./verify-proof.mjs', import.meta.url))
    const run = (...args) => spawnSync(process.execPath, [cli, ...args], { encoding: 'utf8' })
    const record = fixture()
    writeFileSync(file, JSON.stringify(record))
    assert.equal(run('validate', file).status, 0)
    assert.equal(run('marker', file).stdout, `${p.proofLine(record)}\n`)
    assert.equal(run('validate', file, '--unknown').status, 2)
    assert.equal(run('unknown', file).status, 2)
    assert.equal(run('validate').status, 2)
    assert.equal(run('validate', file, 'extra').status, 2)
    writeFileSync(file, JSON.stringify({ ...record, head: 'bad' }))
    const invalid = run('validate', file)
    assert.equal(invalid.status, 2)
    assert.ok(invalid.stderr.length)
    assert.equal(run('marker', file).status, 2)
    writeFileSync(file, '{broken')
    assert.equal(run('validate', file).status, 2)
    assert.equal(run('validate', join(dir, 'missing')).status, 2)
  } finally {
    rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
})

test('accepts nullable page and review, optional fields omitted, and all enum members', () => {
  const record = changed((x) => {
    x.page = null
    x.review = null
    x.confidence.level = 'cannot-assess'
    x.whatItDoes = { source: 'unknown', bullets: [] }
    delete x.areas[0].label
    x.areas[0].media = [{ kind: 'image', url: 'https://example.test/image' }]
  })
  assert.equal(p.validateProof(record).ok, true)
  for (const source of p.INTENT_SOURCES.filter((source) => source !== 'unknown')) {
    assert.equal(
      p.validateProof(
        changed((x) => {
          x.whatItDoes.source = source
        }),
      ).ok,
      true,
    )
  }
  for (const outcome of p.REVIEW_OUTCOMES) {
    assert.equal(
      p.validateProof(
        changed((x) => {
          x.review.models[0].outcome = outcome
          delete x.review.models[0].detail
        }),
      ).ok,
      true,
    )
  }
})

test('optional described links and breaking are validated and digested', () => {
  const original = fixture()
  const record = changed((x) => {
    x.areas[0].links = [{ text: 'Schema', url: 'https://example.test/schema' }]
    x.areas[0].breaking = true
  })
  assert.equal(p.validateProof(record).ok, true)
  assert.notEqual(record.digest, original.digest)
  const relinked = structuredClone(record)
  relinked.areas[0].links[0].url = 'https://example.test/other'
  assert.notEqual(p.proofDigest(relinked), record.digest)
  for (const patch of [
    { links: [{ text: 'bad', url: 'http://example.test' }] },
    { links: Array(p.MAX_DESCRIBE_LINKS + 1).fill({ text: 'x', url: 'https://example.test' }) },
    { breaking: false },
  ]) {
    assert.equal(p.validateProof(changed((x) => Object.assign(x.areas[0], patch))).ok, false)
  }
  const without = structuredClone(record)
  delete without.areas[0].links
  delete without.areas[0].breaking
  assert.equal(p.proofDigest(without), original.digest)
})

test('intent reference is bounded, forbidden on unknown, and covered by the digest', () => {
  const record = fixture()
  record.whatItDoes.ref = 'ISSUE-42'
  const signed = p.withDigest(record)
  assert.equal(p.validateProof(signed).ok, true)
  assert.notEqual(signed.digest, record.digest)
  record.whatItDoes.ref = 'ISSUE-43'
  assert.equal(p.validateProof({ ...record, digest: signed.digest }).ok, false)
  record.whatItDoes.ref = 'x'.repeat(p.MAX_TEXT_CHARS)
  assert.equal(p.validateProof(p.withDigest(record)).ok, true)
  record.whatItDoes.ref += 'x'
  assert.equal(p.validateProof(p.withDigest(record)).ok, false)
  record.whatItDoes = { source: 'unknown', bullets: [], ref: 'ISSUE-42' }
  assert.equal(p.validateProof(p.withDigest(record)).ok, false)
})

const clip = (caption, extra = {}) => ({
  kind: 'video',
  url: `https://example.test/${encodeURIComponent(caption)}.mp4`,
  caption,
  durationMs: 1000,
  ...extra,
})
const still = (caption) => ({ kind: 'image', url: `https://example.test/${caption}.png`, caption })
const withMedia = (media) => changed((x) => (x.areas[0].media = media))
const errorsFor = (media) => p.validateProof(withMedia(media)).errors

test('a video is a clip: it needs a duration below CLIP_MAX_MS', () => {
  const { durationMs: _drop, ...undated } = clip('a')
  assert.notDeepEqual(errorsFor([undated]), [])
  assert.notDeepEqual(errorsFor([clip('a', { durationMs: p.CLIP_MAX_MS })]), [])
  assert.deepEqual(errorsFor([clip('a', { durationMs: p.CLIP_MAX_MS - 1 })]), [])
  assert.deepEqual(errorsFor([still('a')]), [], 'an image needs no duration')
})

test('a base clip needs a captioned head video partner with the same caption', () => {
  assert.notDeepEqual(errorsFor([clip('a', { side: 'base' })]), [], 'no head partner')
  assert.notDeepEqual(errorsFor([clip('a', { side: 'base' }), clip('b')]), [], 'other caption')
  assert.notDeepEqual(errorsFor([{ ...still('a'), side: 'base' }, clip('a')]), [], 'base image')
  const { caption: _drop, ...uncaptioned } = clip('a', { side: 'base' })
  assert.notDeepEqual(errorsFor([uncaptioned, clip('a')]), [], 'base without caption')
  assert.notDeepEqual(errorsFor([clip('a', { side: 'side' }), clip('a')]), [], 'unknown side')
  assert.deepEqual(errorsFor([clip('a', { side: 'base' }), clip('a', { side: 'head' })]), [])
  assert.deepEqual(errorsFor([clip('a', { side: 'base' }), clip('a')]), [])
})

test('mediaStrength ranks a base/head pair above a head clip above a still', () => {
  assert.equal(p.mediaStrength({ media: [clip('a', { side: 'base' }), clip('a')] }), 'compared')
  assert.equal(p.mediaStrength({ media: [clip('a', { side: 'base' }), clip('b')] }), 'recorded')
  assert.equal(p.mediaStrength({ media: [still('a'), clip('b')] }), 'recorded')
  assert.equal(p.mediaStrength({ media: [still('a')] }), 'still')
  assert.equal(p.mediaStrength({ media: [] }), 'none')
  assert.deepEqual(
    p.MEDIA_STRENGTHS.map((s) => p.MEDIA_STRENGTHS.indexOf(s)),
    [0, 1, 2, 3],
  )
})

const group = (caption, kind = 'video', base = false) => ({
  caption,
  head: kind === 'video' ? clip(caption) : still(caption),
  ...(base ? { base: clip(caption) } : {}),
})
const picked = (media) =>
  media.map((m) => `${m.side === 'base' ? 'base:' : ''}${m.kind}:${m.caption}`)

test('selectMedia puts a pair first with its base before its head, then clips, then stills', () => {
  const media = p.selectMedia([group('s', 'image'), group('h'), group('pair', 'video', true)])
  assert.deepEqual(picked(media), ['base:video:pair', 'video:pair', 'video:h'])
  assert.equal(p.mediaStrength({ media }), 'compared')
})

test('selectMedia never splits a pair at the cap and skips it for the next single', () => {
  const media = p.selectMedia([
    group('a', 'video', true),
    group('b', 'video', true),
    group('c'),
    group('d', 'image'),
  ])
  assert.deepEqual(picked(media), ['base:video:a', 'video:a', 'video:c'])
  assert.deepEqual(picked(p.selectMedia([group('a', 'video', true)], 1)), [])
})

test('selectMedia keeps input order within one strength and returns at most MAX_MEDIA', () => {
  const groups = ['e', 'd', 'c', 'b', 'a'].map((c) => group(c))
  assert.deepEqual(picked(p.selectMedia(groups)), ['video:e', 'video:d', 'video:c'])
  assert.equal(p.selectMedia([...groups, ...groups]).length, p.MAX_MEDIA)
  const stills = p.selectMedia(['y', 'x'].map((c) => group(c, 'image')))
  assert.deepEqual(picked(stills), ['image:y', 'image:x'])
})

test('selected media always validate in a proof area', () => {
  const media = p.selectMedia([group('a', 'video', true), group('b'), group('c', 'image')])
  assert.deepEqual(errorsFor(media), [])
})
