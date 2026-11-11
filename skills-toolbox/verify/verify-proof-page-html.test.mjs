import test from 'node:test'
import { PROOF_STYLE } from './verify-proof-style.mjs'
import assert from 'node:assert/strict'
import { withDigest } from './verify-proof.mjs'
import { renderProofHtml } from './verify-proof-page.mjs'
import { NO_UI_RECORDING, fileAnchor } from './verify-proof-page-model.mjs'
import { cutSnippet, parseUnifiedDiff } from './verify-proof-snippet.mjs'
import { region, regionUntilNext } from '../../scripts/test-infra/gate-region-lib.mjs'

const HEAD = '1'.repeat(40)
const BASE = '2'.repeat(40)

function input(areas = ['ui', 'logic']) {
  return {
    proof: withDigest({
      schema: 'boss-proof/1',
      repo: 'acme/shop',
      pr: 7,
      head: HEAD,
      base: BASE,
      confidence: {
        level: 'moderate',
        asks: ['logic'],
        loweredBy: [{ code: 'gap', text: 'Totals need a person' }],
      },
      whatItDoes: { source: 'pr-body', bullets: ['Shows the basket total'] },
      areas: areas.map((name) => ({
        name,
        label: name === 'ui' ? 'UI' : 'Logic',
        ask: name === 'logic',
        items: [`${name} fact`],
        undisclosed: false,
        media: [],
        proven: false,
      })),
      review: { models: [{ agent: 'reviewer', outcome: 'ran' }], blockers: [], notes: [] },
      health: [],
      page: null,
    }),
    timeline: [{ at: '2026-01-01T10:00:00Z', text: 'Add the total' }],
  }
}

const DIFF = parseUnifiedDiff(
  '--- a/src/cart.ts\n+++ b/src/cart.ts\n@@ -1,6 +1,6 @@\n export function total(items) {\n-  return sum(items)\n+  return round(sum(items))\n }\n const a = "<b>"\n // end\n line six\n',
)[0]
const resolver = (request) => cutSnippet(request.path === 'src/cart.ts' ? DIFF : undefined, request)
const files = [
  { path: 'src/cart.ts', adds: 1, dels: 1 },
  { path: 'src/cart.test.ts', adds: 30, dels: 0 },
]

function spec(overrides = {}) {
  return {
    schema: 'proof-spec/1',
    head: HEAD,
    title: 'Round the basket total once',
    waiting: [{ title: 'Rounding moved', detail: 'One step now.', area: 'logic' }],
    risks: [
      {
        level: 'medium',
        title: 'Totals <i>round</i> once',
        body: 'Check `round()`.',
        label: 'Logic',
        at: { path: 'src/cart.ts', line: 2 },
      },
      { level: 'low', title: 'Invented path', body: 'x', at: { path: 'src/nowhere.ts', line: 1 } },
    ],
    areas: [
      {
        id: 'logic',
        name: 'Logic',
        summary: 'Rounds once.',
        facts: [{ text: 'One rounding step', flag: true }],
        snippets: [
          { path: 'src/cart.ts', start: 1, end: 5, pinLine: 2, pinNote: 'The new call.' },
          { path: 'src/cart.ts', start: 3, end: 6 },
        ],
      },
    ],
    checklist: { before: [{ text: 'Try a basket', done: false }], after: [] },
    notes: [{ severity: 'P3', at: 'src/cart.ts:2', text: 'Name it.' }],
    uncertain: ['Discount rounding'],
    ...overrides,
  }
}

function media() {
  const clip = (id, side, title, pairs) => ({
    id,
    side,
    ...(pairs ? { pairs } : {}),
    title,
    file: `${id}.mp4`,
    durationMs: 10_000,
    chapters: [
      { atMs: 1_000, caption: `${title} opens` },
      { atMs: 4_000, caption: `${title} totals` },
    ],
    stills: [{ file: `${id}-still.png`, caption: `${title} still` }],
  })
  return {
    schema: 'proof-media/1',
    head: HEAD,
    clips: [
      clip('base-0', 'base', 'Before', 'head-1'),
      clip('head-0', 'head', 'Basket'),
      clip('head-1', 'head', 'Checkout'),
    ],
    transcripts: [
      {
        title: 'Total stored',
        status: 200,
        ms: 12,
        request: { id: 1 },
        response: { total: '<3>' },
      },
    ],
  }
}

const render = (extras) => renderProofHtml(input(), extras)

test('a spec puts git-quoted, pinned diffs under its area and links its risk to them', () => {
  const html = render({ spec: spec(), snippet: resolver, files })
  assert.ok(html.includes('<h1>Round the basket total once</h1>'))
  const anchor = fileAnchor('src/cart.ts')
  assert.notEqual(anchor, fileAnchor('src/cart_ts'), 'paths that slug alike keep distinct anchors')
  assert.ok(html.includes(`<figure class="diff" id="${anchor}">`))
  assert.equal(html.split(`id="${anchor}"`).length, 2, 'one anchor per path')
  assert.ok(html.includes(`<a class="where" href="#${anchor}"><span>Logic</span>cart.ts:2</a>`))
  assert.ok(html.includes('<tr class="add pinned"><td class="n"></td><td class="n">2</td>'))
  assert.ok(
    html.includes('<span class="k">return</span> round(sum(items))'),
    'code comes from the diff',
  )
  assert.ok(html.includes('<span class="s">&quot;&lt;b&gt;&quot;</span>'))
  assert.ok(html.includes('<div class="pinnote"><span class="pinicon">!</span>The new call.</div>'))
  assert.ok(
    html.includes(
      '<h3>Totals &lt;i&gt;round&lt;/i&gt; once</h3><p>Check <code>round()</code>.</p>',
    ),
  )
  assert.ok(html.includes('<span class="warn">One rounding step</span>'))
  assert.ok(html.includes('<a class="x" href="#a-logic">Logic</a>'))
  assert.ok(html.includes('+31</b> <b class="rem">−1</b> in 2 files'))
  assert.ok(html.includes('.k-logic{flex:1}.k-test{flex:30}.k-gen{flex:0}'))
})

test('the generation check lists every dropped item and snippet problem, and what the agent was unsure of', () => {
  const html = render({ spec: spec(), snippet: resolver, files })
  const qa = region(html, 'id="generation"')
  assert.ok(html.includes('<section class="qa flagged" id="generation">'))
  assert.ok(qa.includes('spec risks[1]: at.path: src/nowhere.ts is not changed by this PR'))
  assert.ok(qa.includes('src/cart.ts:3-6: shows no changed line, snippet dropped'))
  assert.ok(qa.includes('<h3>The agent was unsure about</h3><ul><li>Discount rounding</li>'))
  assert.ok(!html.includes('Invented path'))
  const clean = render({
    spec: spec({
      risks: [spec().risks[0]],
      areas: [{ ...spec().areas[0], snippets: [spec().areas[0].snippets[0]] }],
    }),
    snippet: resolver,
    files,
  })
  assert.ok(clean.includes('<section class="qa" id="generation">'))
  assert.ok(clean.includes('Every spec and media item passed validation'))
  const noGit = render({ spec: spec(), files })
  assert.ok(noGit.includes('src/cart.ts:1-5: no git to quote it from, snippet dropped'))
  assert.ok(!noGit.includes('class="diff"'))
})

test('media becomes one player with chapters per clip, portrait stills with a lightbox, pairs and transcripts', () => {
  const html = render({ media: media() })
  assert.ok(
    html.includes(
      '<video id="proof-video" data-clip="0" controls playsinline preload="metadata" src="media/head-0.mp4" poster="media/head-0-still.png">',
    ),
  )
  const buttons = [
    ...html.matchAll(
      /<button type="button" data-chapter="" data-clip="(\d)" data-src="([^"]+)"[^>]*data-t="([\d.]+)">/g,
    ),
  ].map((m) => m.slice(1))
  assert.deepEqual(buttons, [
    ['0', 'media/head-0.mp4', '1.00'],
    ['0', 'media/head-0.mp4', '4.00'],
    ['1', 'media/head-1.mp4', '1.00'],
    ['1', 'media/head-1.mp4', '4.00'],
  ])
  assert.ok(!html.includes('media/base-0.mp4'), 'a base clip is never played')
  assert.ok(
    html.includes('<a class="thumb" id="thumb-1" href="#shot-1"><img src="media/head-1-still.png"'),
  )
  assert.ok(html.includes('<div class="lightbox" id="shot-0">'))
  assert.ok(html.includes('<a class="lb-next" href="#shot-1"'))
  assert.ok(html.includes('<figcaption>Before · Before still</figcaption>'))
  assert.ok(
    html.includes('<figcaption>After · Checkout still</figcaption>'),
    'a base clip pairs with the head clip it names, not the one beside it',
  )
  assert.ok(html.includes('&quot;total&quot;: &quot;&lt;3&gt;&quot;'))
  assert.ok(html.includes('2 recordings, 2 stills, base compared, 1 API call'))
  assert.equal(html.match(/<script[^>]*>/g)?.join(), '<script src="player.js" defer>')
  assert.ok(html.endsWith('<script src="player.js" defer></script></body></html>'))
  assert.ok(!/\son\w+\s*=|\sstyle=/.test(html), 'no inline handlers or style attributes')
})

test('a changed UI area with no head clip leads why it is waiting', () => {
  const without = render({})
  const first = without.indexOf('<ol class="reasons"><li class="proof">')
  assert.ok(first > 0)
  assert.ok(
    without
      .slice(first)
      .startsWith(
        `<ol class="reasons"><li class="proof"><div><div class="t">${NO_UI_RECORDING}</div>`,
      ),
  )
  assert.ok(!render({ media: media() }).includes(NO_UI_RECORDING))
  assert.ok(!renderProofHtml(input(['logic'])).includes(NO_UI_RECORDING))
})

test('a spec or manifest for another head is ignored and says so', () => {
  const html = render({
    spec: spec({ head: 'f'.repeat(40) }),
    media: { ...media(), head: 'f'.repeat(40) },
  })
  assert.ok(html.includes('<li>spec.head: written for another head</li>'))
  assert.ok(html.includes('<li>media.head: captured for another head</li>'))
  assert.ok(!html.includes('proof-video'))
  assert.ok(
    html.includes('<div class="t">Totals need a person</div>'),
    'the record fallback still fills the page',
  )
})

test('without a spec, a breaking API change and unmentioned changes are the risks', () => {
  const page = input(['logic'])
  Object.assign(page.proof.areas[0], {
    change: 'breaking',
    changeReasons: ['field removed'],
    undisclosed: true,
    unmentioned: ['Cart.total'],
  })
  page.proof = withDigest(page.proof)
  const html = renderProofHtml(page)
  assert.ok(
    html.includes(
      '<span class="lvl high">High</span><div><h3>Breaking API change in Logic</h3><ul class="items"><li>field removed</li>',
    ),
  )
  assert.ok(
    html.includes('<h3>Not in the description: Logic</h3><ul class="items"><li>Cart.total</li>'),
  )
  assert.ok(html.includes('<dd>1 high, 1 medium</dd>'))
})

test('an agent spec adds to the record and never hides a blocker, a reason or a breaking change', () => {
  const page = input(['logic'])
  page.proof.confidence.level = 'blocked'
  page.proof.review.blockers = [{ title: 'Data loss on retry', where: 'src/cart.ts:9' }]
  Object.assign(page.proof.areas[0], { change: 'breaking', changeReasons: ['field removed'] })
  page.proof = withDigest(page.proof)
  const html = renderProofHtml(page, {
    spec: spec({
      areas: [],
      waiting: [{ title: 'Minor naming' }],
      checklist: { before: [{ text: 'Glance at names' }] },
    }),
    files,
  })
  const waiting = region(html, 'id="waiting"', 'id="risks"')
  assert.ok(waiting.indexOf('Data loss on retry') < waiting.indexOf('Totals need a person'))
  assert.ok(waiting.indexOf('Totals need a person') < waiting.indexOf('Minor naming'))
  assert.ok(html.includes('<h3>Breaking API change in Logic</h3>'))
  assert.ok(html.includes('<h3>Totals &lt;i&gt;round&lt;/i&gt; once</h3>'))
  const before = region(html, 'Before you approve', 'After merge</h3>')
  assert.ok(before.indexOf('Resolve: Data loss on retry') < before.indexOf('Glance at names'))
})

/** A head statement of three pages paired with a base of two. */
function statement() {
  const doc = (id, side, name, pageCount, pairs) => ({
    id,
    side,
    ...(pairs ? { pairs } : {}),
    title: 'Monthly <statement>',
    file: `docs/${name}.pdf`,
    pageCount,
    pages: Array.from({ length: pageCount }, (_, i) => ({
      page: i + 1,
      file: `docs/${name}-p${i + 1}.png`,
      caption: `${side} page ${i + 1}`,
    })),
  })
  return {
    schema: 'proof-media/1',
    head: HEAD,
    clips: [],
    transcripts: [],
    documents: [
      doc('doc-0-base', 'base', 'statement-base', 2, 'doc-0'),
      doc('doc-0', 'head', 'statement-head', 3),
    ],
  }
}

test('a paired document pairs pages by number, so a dropped page never shifts the rest', () => {
  const media = statement()
  media.documents[1].pages.splice(1, 1)
  const html = render({ media })
  const docs = regionUntilNext(html, '<h3 class="sub" id="documents">Documents</h3>', '</section>')
  assert.ok(
    docs.includes(
      '<header><b>Monthly &lt;statement&gt;</b><span class="dim">3 pages, 2 before</span><a href="media/statement-head.pdf">Download PDF</a><a href="media/statement-base.pdf">Download base PDF</a><span class="framed-note dim">To download the PDF, open this page in its own tab.</span></header>',
    ),
  )
  const rows = [...docs.matchAll(/<div class="pair">(.*?)<\/div>/g)].map((m) =>
    [...m[1].matchAll(/<figcaption>([^<]*)<\/figcaption>/g)].map((c) => c[1]),
  )
  assert.deepEqual(rows, [
    ['Before · base page 1', 'After · head page 1'],
    ['Before · base page 2', 'After · no page 2'],
    ['Before · no page 3', 'After · head page 3'],
  ])
  assert.ok(
    docs.includes(
      '<a id="doc-0-head-p3-thumb" href="#doc-0-head-p3"><img src="media/statement-head-p3.png" alt="head page 3" loading="lazy" decoding="async"></a>',
    ),
  )
  for (const id of ['doc-0-head-p1', 'doc-0-head-p3', 'doc-0-base-p1', 'doc-0-base-p2'])
    assert.ok(html.includes(`<div class="lightbox" id="${id}">`), id)
  assert.ok(!html.includes('id="doc-0-head-p2"'))
  const box = region(
    html,
    '<div class="lightbox" id="doc-0-head-p3">',
    '<div class="lightbox" id="doc-0-base-p1">',
  )
  assert.ok(
    box.includes(
      '<img src="media/statement-head-p3.png" alt="head page 3" loading="lazy" decoding="async">',
    ),
  )
  assert.ok(box.includes('<a class="lb-full" href="media/statement-head-p3.png">Full size</a>'))
  assert.ok(
    box.includes('<a class="lb-prev" href="#doc-0-head-p1"'),
    'previous skips the missing page',
  )
  assert.ok(!box.includes('lb-next'), 'page 3 is the last head page')
  assert.ok(
    box.includes('<a class="lb-x" href="#doc-0-head-p3-thumb">Close</a>'),
    'closing returns to the page thumbnail it opened from',
  )
  assert.equal(
    new Set([...html.matchAll(/ id="([^"]+)"/g)].map((m) => m[1])).size,
    html.match(/ id="/g).length,
    'every id is unique',
  )
  assert.ok(html.includes('<h2>Evidence <span class="n">1 document</span></h2>'))
  assert.ok(!/<(object|iframe|embed)\b/.test(html), 'the PDF is linked, never embedded')
  assert.deepEqual(
    html.match(/<script[^>]*>/g),
    ['<script src="player.js" defer>'],
    'documents add no script beyond the page player',
  )
})

test('pages with no numbers keep their positions, so a dropped page leaves a gap', () => {
  const media = statement()
  for (const doc of media.documents) for (const page of doc.pages) delete page.page
  media.documents[1].pages[1].file = 'docs/statement-head-p2.pdf'
  const docs = regionUntilNext(
    render({ media }),
    '<h3 class="sub" id="documents">Documents</h3>',
    '</section>',
  )
  const rows = [...docs.matchAll(/<div class="pair">(.*?)<\/div>/g)].map((m) =>
    [...m[1].matchAll(/<figcaption>([^<]*)<\/figcaption>/g)].map((c) => c[1]),
  )
  assert.deepEqual(rows, [
    ['Before · base page 1', 'After · head page 1'],
    ['Before · base page 2', 'After · no page 2'],
    ['Before · no page 3', 'After · head page 3'],
  ])
})

test('an unpaired document shows thumbnails and no base link', () => {
  const media = statement()
  media.documents = [media.documents[1]]
  const docs = regionUntilNext(render({ media }), 'id="documents"', '</section>')
  assert.ok(
    docs.includes(
      '<span class="dim">3 pages</span><a href="media/statement-head.pdf">Download PDF</a><span class="framed-note dim">To download the PDF, open this page in its own tab.</span></header>',
    ),
  )
  assert.ok(!docs.includes('Download base PDF'))
  assert.ok(
    docs.includes(
      '<span class="framed-note dim">To download the PDF, open this page in its own tab.</span>',
    ),
    'a framed review view, which may not download, says where the PDF downloads',
  )
  assert.match(PROOF_STYLE, /\n\.framed-note\{display:none\}/)
  assert.match(PROOF_STYLE, /\n\.proof-framed \.doc>header a\{display:none\}/)
  assert.match(PROOF_STYLE, /\n\.proof-framed \.framed-note\{display:inline\}/)
  assert.ok(!docs.includes('class="pair"'))
  assert.equal(
    docs.match(/<a class="thumb" id="doc-0-head-p\d-thumb" href="#doc-0-head-p\d">/g)?.length,
    3,
  )
})
