// A synthetic edge-case proof page: unbroken 300-character tokens, record labels and facts at their
// caps, quoted and deleted paths in an ask diff, many notes, reviewer outcomes, blank still captions
// and a null transcript response. Each test names the defect it pins.
import test from 'node:test'
import assert from 'node:assert/strict'
import { capText, withDigest } from './verify-proof.mjs'
import { renderProofComment, renderProofHtml } from './verify-proof-page.mjs'
import { FILES_MISSING, bannerReason, fileAnchor } from './verify-proof-page-model.mjs'
import { cutSnippet, lineChecker, parseUnifiedDiff } from './verify-proof-snippet.mjs'
import { PROOF_STYLE } from './verify-proof-style.mjs'
import { validateProofSpec } from './verify-proof-spec.mjs'
import { validateProofMedia } from './verify-proof-media.mjs'
import { region, regionUntilNext } from '../../scripts/test-infra/gate-region-lib.mjs'

const HEAD = 'a'.repeat(40)
const BASE = 'b'.repeat(40)
const TOKEN = `https://ci.example.com/${'0123456789abcdef'.repeat(17)}`.slice(0, 300)
const LONG_LABEL =
  `A record label that runs to the full three hundred characters ${'and keeps going '.repeat(16)}`.slice(
    0,
    300,
  )
const DUPLICATE = 'Stored revision times are compared at microsecond precision.'
const LONG_PATH = `services/${'deeply/nested/'.repeat(16)}handler.go`
const LONG_LINE = `\tconst value = "${'x'.repeat(260)}"`

const CART = parseUnifiedDiff(
  '--- a/src/cart.ts\n+++ b/src/cart.ts\n@@ -1,6 +1,6 @@\n export function total(items) {\n-  return sum(items)\n+  return round(sum(items))\n }\n const a = 1\n // end\n line six\n',
)[0]
const diffs = (path) => (path === 'src/cart.ts' ? CART : undefined)

const ASK_DIFF = [
  'diff --git "a/docs/R\\303\\251sum\\303\\251.md" "b/docs/R\\303\\251sum\\303\\251.md"',
  '--- "a/docs/R\\303\\251sum\\303\\251.md"',
  '+++ "b/docs/R\\303\\251sum\\303\\251.md"',
  '@@ -1 +1 @@',
  '-old',
  '+new résumé',
  'diff --git a/assets/a file.txt b/assets/a file.txt',
  '--- /dev/null',
  '+++ b/assets/a file.txt\t',
  '@@ -0,0 +1 @@',
  '+one',
  'diff --git a/legacy/remove-me.go b/legacy/remove-me.go',
  '--- a/legacy/remove-me.go',
  '+++ /dev/null',
  '@@ -1,2 +0,0 @@',
  '-func a() {}',
  '-func b() {}',
  'diff --git a/src/cart.ts b/src/cart.ts',
  '--- a/src/cart.ts',
  '+++ b/src/cart.ts',
  '@@ -2 +2 @@',
  '-  return sum(items)',
  '+  return round(sum(items))',
  `diff --git a/${LONG_PATH} b/${LONG_PATH}`,
  `--- a/${LONG_PATH}`,
  `+++ b/${LONG_PATH}`,
  '@@ -1 +1 @@',
  '-x',
  `+${LONG_LINE}`,
].join('\n')

const FILES = [
  { path: 'src/cart.ts', adds: 1, dels: 1 },
  { path: 'docs/Résumé.md', adds: 1, dels: 1 },
  { path: 'assets/a file.txt', adds: 250_000, dels: 0 },
  { path: 'legacy/remove-me.go', adds: 0, dels: 2 },
  { path: LONG_PATH, adds: 1, dels: 1 },
]

function proof({ whatItDoes, notes, docsAsk = false } = {}) {
  const area = (name, label, ask, items) => ({
    name,
    label,
    ask,
    items,
    undisclosed: false,
    media: [],
    proven: false,
  })
  return withDigest({
    schema: 'boss-proof/1',
    repo: 'acme/edge',
    pr: 9,
    head: HEAD,
    base: BASE,
    confidence: {
      level: 'moderate',
      asks: docsAsk ? ['logic', 'docs'] : ['logic'],
      loweredBy: [{ code: 'gap', text: TOKEN }],
    },
    whatItDoes: whatItDoes ?? {
      source: 'pr-body',
      bullets: ['Rounds the total once with `round()`'],
    },
    areas: [
      area('logic', 'Logic', true, ['logic fact']),
      area(
        'docs',
        LONG_LABEL,
        docsAsk,
        Array.from({ length: 20 }, (_, i) => `Docs fact ${i + 1}`),
      ),
    ],
    review: {
      models: [
        { agent: 'review', outcome: 'ran' },
        { agent: 'codex', outcome: 'skipped', detail: 'No quota' },
        { agent: 'security', outcome: 'failed' },
      ],
      blockers: [],
      notes: notes ?? [DUPLICATE, ...Array.from({ length: 19 }, (_, i) => `Record note ${i + 1}`)],
    },
    health: [{ code: 'judge-decisions-ignored', text: 'judge decisions ignored' }],
    page: null,
  })
}

function spec(overrides = {}) {
  return {
    schema: 'proof-spec/1',
    head: HEAD,
    title: 'Round the basket total once',
    waiting: [{ title: 'Docs moved', area: 'docs' }],
    risks: [
      {
        level: 'medium',
        title: 'Rounds once',
        impact: 'Check it.',
        at: { path: 'src/cart.ts', line: 2 },
      },
      {
        level: 'high',
        title: 'Invented line',
        impact: 'x',
        at: { path: 'src/cart.ts', line: 999_999 },
      },
    ],
    areas: [
      {
        id: 'logic',
        name: 'Logic',
        summary: TOKEN,
        facts: [{ text: 'One rounding step', flag: true }],
        snippets: [{ path: 'src/cart.ts', start: 1, end: 5, pinLine: 2, pinNote: 'The new call.' }],
      },
    ],
    decisions: [
      {
        id: 'D1',
        question: TOKEN,
        why: TOKEN,
        suggestion: TOKEN,
        at: { path: 'src/cart.ts', line: 2 },
      },
    ],
    checklist: { after: [{ text: TOKEN, done: false }] },
    notes: [
      { severity: 'P3', at: 'lib/rev.go:7', text: `${DUPLICATE} ` },
      {
        severity: 'Important',
        at: `lib/${'VeryLongFileNameSegment'.repeat(3)}.go:42`,
        text: 'A word severity',
      },
      { text: 'No severity at all' },
      { severity: 'Far too long a severity', text: 'Kept without its severity' },
      ...Array.from({ length: 6 }, (_, i) => ({ severity: 'P2', text: `Spec note ${i + 1}` })),
    ],
    ...overrides,
  }
}

function media() {
  const stills = (id) => [
    { file: `${id}-0.png`, caption: `${id} first` },
    { file: `${id}-1.png`, caption: ' ', atMs: 65_000 - 60_000 },
    { file: `${id}-2.png`, caption: `${id} last` },
  ]
  return {
    schema: 'proof-media/1',
    head: HEAD,
    clips: [
      {
        id: 'base-0',
        side: 'base',
        pairs: 'head-0',
        title: 'Before',
        file: 'base-0.mp4',
        durationMs: 9_000,
        chapters: [],
        stills: stills('base-0'),
      },
      {
        id: 'head-0',
        side: 'head',
        title: 'Basket',
        file: 'head-0.mp4',
        durationMs: 9_000,
        chapters: [{ atMs: 1_000, caption: `https://example.com/${'a1b2c3d4e5'.repeat(10)}` }],
        stills: stills('head-0'),
      },
    ],
    transcripts: [{ title: 'Deleted', status: 204, ms: 3, request: { id: 1 }, response: null }],
  }
}

const input = (overrides) => ({ proof: proof(overrides), asks: { logic: { diff: ASK_DIFF } } })
const extras = (more = {}) => ({
  spec: spec(),
  media: media(),
  snippet: (request) => cutSnippet(diffs(request.path), request),
  hasLine: lineChecker(diffs),
  files: FILES,
  ...more,
})
const page = (more, overrides) => renderProofHtml(input(overrides), extras(more))
/** The body of the page section with this id; a page without one fails the test. */
const section = (html, id) => {
  const found = new RegExp(`<section class="sec[^"]*" id="${id}"[^>]*>([\\s\\S]*?)</section>`).exec(
    html,
  )
  assert.ok(found, `the page has a #${id} section`)
  return found[1]
}
const bannerOf = (html) => region(html, '<section class="banner', '</section>')
const outsideDebug = (html) => html.replace(/<section class="sec debug"[\s\S]*?<\/section>/, '')
/** The CSS rule for `selector`, exactly as the sheet writes it. */
const rule = (selector) => {
  const found = new RegExp(`\\n${selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}\\{[^}]*\\}`).exec(
    PROOF_STYLE,
  )
  assert.ok(found, `no rule for ${selector}`)
  return found[0].trim()
}

test('#1 #2 #14 #19 every text container breaks an unbroken token instead of widening the page', () => {
  assert.match(rule('body'), /overflow-wrap:anywhere/)
  assert.match(rule('pre'), /white-space:pre-wrap;overflow-wrap:anywhere/)
  assert.match(rule('.code .scroll'), /overflow-x:auto/, 'a long code line scrolls in its figure')
  // A pill word, a count, a line number, a nav label in a scroller and a capped location.
  const short =
    /^(\.pill|\.diffstat|\.secnav a|\.code td\.[nm]|\.frow \.c2,\.frow \.c3,\.frow \.c4|\.loc)\{/
  for (const [declaration] of PROOF_STYLE.matchAll(/[^{}\n]+\{[^}]*white-space:nowrap[^}]*\}/g))
    assert.ok(
      /text-overflow:ellipsis|overflow-x:auto/.test(declaration) || short.test(declaration),
      `nowrap text must clip, scroll or be short: ${declaration}`,
    )
  const html = page()
  const text = (fragment) => fragment.replace(/<[^>]+>/g, '')
  assert.ok(text(region(html, '<p class="reason">', '</p>')).includes(TOKEN), 'the reason keeps it')
  assert.ok(text(region(section(html, 'a-logic'), '<p class="lead">', '</p>')).includes(TOKEN))
})

test('#5 #6 #7 a 300-character record label is cut at a word, and none of 20 facts is lost', () => {
  const html = page({}, { docsAsk: true })
  const cut = 'A record label that runs to the full…'
  const docs = section(html, 'a-docs')
  assert.ok(docs.startsWith(`<h2>${cut}<span class="pill`), 'the heading is cut at a word')
  assert.ok(bannerOf(html).includes(`<span class="k">${cut}</span>`), 'so is its signal row')
  assert.ok(region(html, '<nav class="secnav"', '</nav>').includes(`>${cut}</a>`))
  assert.equal(
    regionUntilNext(docs, '<ul class="facts">', '</ul>').match(/<li>Docs fact \d+<\/li>/g)?.length,
    20,
  )
  assert.match(rule('.sig .v'), /-webkit-line-clamp:2/, 'the signal summary stays two lines')
})

test('#8 the chapter list scrolls inside the player instead of growing the panel', () => {
  assert.match(rule('.chapters'), /max-height:560px;overflow-y:auto/)
})

test('#9 #10 #11 every screenshot loads lazily and opens its own image in the one lightbox', () => {
  const html = page()
  const pairs = regionUntilNext(
    html,
    '<h3 class="sub">Before and after</h3>',
    '<div class="thumbs">',
  )
  assert.equal(pairs.match(/<img /g)?.length, 6)
  assert.equal(
    pairs.match(/loading="lazy" decoding="async"/g)?.length,
    6,
    'every pair image is lazy',
  )
  const shots = [
    ...html.matchAll(/<a class="shot" href="([^"]+)" data-cap="[^"]*"><img src="([^"]+)"/g),
  ]
  assert.equal(shots.length, 9, 'three pairs and three head stills')
  for (const [, href, src] of shots)
    assert.equal(href, src, 'without the script it opens the image')
  const box = region(html, '<div class="lb" id="lb" hidden', '<script')
  for (const id of ['lb-cap', 'lb-zoom', 'lb-close', 'lb-stage', 'lb-img'])
    assert.ok(box.includes(`id="${id}"`), id)
  assert.equal(html.split('id="lb"').length, 2, 'one lightbox for every screenshot')
})

test('#13 a clip with no poster shows its first still', () => {
  assert.match(page(), /<video id="proof-video"[^>]* poster="media\/head-0-0.png">/)
})

test('#15 before and after images share one height so their captions line up', () => {
  const pairImage = /\n\.pair (?:\.shot )?img\{[^}]*\}/.exec(PROOF_STYLE)
  assert.ok(pairImage, 'a before and after pair sets its image height')
  assert.match(pairImage[0], /[{;]height:\d+px/, 'a fixed height, not a max')
})

test('#16 #17 #18 #20 ask excerpts name files right, skip quoted ones and count the PR', () => {
  const logic = section(page(), 'a-logic')
  const captions = [...logic.matchAll(/<span class="path" title="[^"]*">([^<]*)<\/span>/g)].map(
    (m) => m[1],
  )
  assert.deepEqual(captions, [
    'src/cart.ts',
    'docs/Résumé.md',
    'assets/a file.txt',
    'legacy/remove-me.go',
    LONG_PATH,
  ])
  assert.equal(logic.split(`id="${fileAnchor('src/cart.ts')}"`).length, 2)
  assert.equal(captions.filter((c) => c === 'src/cart.ts').length, 1, 'a quoted file is shown once')
  assert.match(
    logic,
    /assets\/a file\.txt<\/span><span class="diffstat"><span class="a">\+250,000<\/span>/,
    'excerpt counts come from the changed-file list',
  )
  assert.ok(logic.includes('@@ -1,1 +1,1 @@'), 'hunk headers carry counts')
  assert.ok(logic.includes('@@ -1,2 +0,0 @@'))
  assert.ok(logic.includes('<span class="cut">… line cut</span>'), 'a cut line says so')
  assert.match(rule('.code td.t'), /tab-size:4/)
})

test('#17 git header paths unquote, lose their trailing tab and fall back to the base side', () => {
  const parsed = parseUnifiedDiff(ASK_DIFF).map((f) => f.path)
  assert.deepEqual(parsed.slice(0, 3), [
    'docs/Résumé.md',
    'assets/a file.txt',
    'legacy/remove-me.go',
  ])
})

/** Each finding's text, in page order. */
const findingTexts = (html) =>
  [...section(html, 'findings').matchAll(/<div class="q">(.*?)<\/div>/g)].map(([, q]) =>
    q.replace(/<[^>]+>/g, '').trim(),
  )

test('#21 #22 #23 notes show once with their location, and the list is capped', () => {
  const html = page()
  const shown = findingTexts(html)
  assert.equal(shown.length, 20, 'the list is capped')
  assert.deepEqual(
    shown.slice(0, 19),
    Array.from({ length: 19 }, (_, i) => `Record note ${i + 1}`),
    'record notes lead, so spec notes never push one past the cap',
  )
  assert.equal(shown[19], DUPLICATE, 'the spec notes fill what the record notes leave')
  assert.equal(shown.filter((text) => text === DUPLICATE).length, 1, 'a restated note shows once')
  assert.match(
    section(html, 'findings'),
    /<div class="q">Stored revision times[^<]*<\/div><code class="loc" title="lib\/rev\.go:7">rev\.go:7<\/code>/,
    'with the location the spec gave it',
  )
  const few = findingTexts(page(undefined, { notes: [DUPLICATE, 'Record note 1'] }))
  assert.equal(few.length, 11, 'the record note nobody restated and ten spec notes')
  assert.equal(few[0], 'Record note 1')
})

test('#24 a long file location is cut to fit and keeps its line number', () => {
  const findings = section(page(undefined, { notes: [] }), 'findings')
  const at = `lib/${'VeryLongFileNameSegment'.repeat(3)}.go:42`
  const loc = new RegExp(`<code class="loc" title="${at}">([^<]*)</code>`).exec(findings)
  assert.ok(loc, 'the full location is the title')
  assert.ok(loc[1].endsWith('.go:42'), 'the line number is never cut')
  assert.ok(loc[1].length <= 40, `a long file name is cut, since .loc never wraps: ${loc[1]}`)
})

test('reviewers: one that failed is not proven, one that skipped or ran shows only in debug', () => {
  const html = page()
  const gaps = regionUntilNext(bannerOf(html), '<ul class="glist gaps">', '</ul>')
  assert.ok(gaps.includes('The security review did not finish'))
  for (const quiet of ['codex', 'No quota', 'review: ran'])
    assert.ok(!outsideDebug(html).includes(quiet), quiet)
  assert.ok(section(html, 'debug').includes('<li>codex: skipped (No quota)</li>'))
})

test('a skipped security review is not proven; a skipped second model stays in debug', () => {
  const { digest, ...record } = proof()
  record.review.models = [
    { agent: 'codex', outcome: 'skipped', detail: 'No quota' },
    { agent: 'security', outcome: 'skipped' },
  ]
  const html = renderProofHtml({ proof: withDigest(record) }, extras())
  const gaps = regionUntilNext(bannerOf(html), '<ul class="glist gaps">', '</ul>')
  assert.ok(gaps.includes('The security review was skipped'))
  assert.ok(!outsideDebug(html).includes('codex'))
})

test('the banner reason reads as one sentence: names lower-cased mid-sentence, one size clause', () => {
  const reason = (/** @type {string[]} */ codes, /** @type {string} */ label) =>
    bannerReason(
      /** @type {any} */ ({
        confidence: {
          level: 'low',
          asks: ['a'],
          loweredBy: codes.map((code) => ({ code, text: TEXTS[code] })),
        },
        areas: [{ name: 'a', label, ask: true }],
        review: null,
      }),
      undefined,
    )
  const TEXTS = /** @type {Record<string, string>} */ ({
    'area-asks': 'It changes areas a person reviews.',
    'size-p90': 'It is larger than nine in ten merged PRs here.',
    'size-p99': 'It is larger than 99 in 100 merged PRs here.',
  })
  assert.equal(
    reason(['area-asks', 'size-p90', 'size-p99'], 'Revenue models'),
    'It changes revenue models, which a person reviews, and it is larger than 99 in 100 merged PRs here.',
  )
  assert.equal(
    reason(['area-asks'], 'GraphQL schema'),
    'It changes GraphQL schema, which a person reviews.',
  )
  assert.equal(
    reason(['area-asks'], 'API contract'),
    'It changes API contract, which a person reviews.',
  )
})

test('#25 a note severity is optional, words map to P-levels and a long one is left out', () => {
  const { spec: kept, dropped } = validateProofSpec(spec(), {
    head: HEAD,
    changedPaths: FILES.map((f) => f.path),
    hasLine: () => true,
  })
  assert.deepEqual(
    kept.notes.slice(1, 4).map((n) => [n.severity, n.text]),
    [
      ['P1', 'A word severity'],
      ['', 'No severity at all'],
      ['', 'Kept without its severity'],
    ],
  )
  assert.deepEqual(dropped, [
    { at: 'notes[3].severity', reason: 'expected at most 12 characters, severity left out' },
  ])
})

test('#26 a missing changed-file list is one debug line and no Files section', () => {
  const html = page({ files: null })
  const debug = section(html, 'debug')
  assert.ok(debug.includes(`${FILES_MISSING} (4 dropped).`), 'two risks, a decision and a snippet')
  assert.ok(!debug.includes('the changed files are unknown'))
  assert.ok(!html.includes('id="files"'))
})

test('#27 #31 a blank still caption keeps the still, null stays null, MiB and one prefix', () => {
  const { media: kept } = validateProofMedia(media(), { head: HEAD })
  assert.equal(kept.clips[1].stills[1].caption, 'Basket at 0:05')
  const html = page()
  assert.ok(html.includes('<h4>Response</h4><pre>null</pre>'))
  const { dropped } = validateProofMedia(media(), {
    head: HEAD,
    sizeOf: (file) => (file === 'base-0.mp4' ? 16 * 1024 * 1024 : 1),
  })
  assert.equal(dropped[0].reason, 'file: base-0.mp4 is over 15 MiB')
  const debug = section(page({ spec: { ...spec(), extra: 1 } }), 'debug')
  assert.ok(debug.includes('<li>spec.extra: unknown key</li>'))
})

test('#28 a risk that cites a line the file does not have at head is dropped', () => {
  const html = page()
  assert.ok(!html.includes('Invented line'))
  assert.ok(
    section(html, 'debug').includes(
      'spec risks[1]: at.line: src/cart.ts has no line 999999 at head',
    ),
  )
  const noGit = renderProofHtml(input(), extras({ hasLine: undefined }))
  assert.ok(
    section(noGit, 'risks').includes('Invented line'),
    'without git the line is not checked',
  )
})

test('#29 #30 with no spec title the heading is the PR and the judge names what a person reviews', () => {
  const html = renderProofHtml(
    { ...input(), verdict: { codes: ['always-human-path'], label: 'the verify judge' } },
    extras({ spec: spec({ title: undefined }) }),
  )
  assert.ok(html.includes('<h1>acme/edge #9</h1>'))
  assert.ok(
    region(html, '<p class="reason">', '</p>').startsWith(
      '<p class="reason">It changes the verify judge, which a person reviews',
    ),
  )
  assert.ok(!outsideDebug(html).includes('Not verified'), 'the gate state is a debug line')
})

test('leftovers: no intent-unknown bullet, no em dash, and factory health only behind ?debug', () => {
  const unknown = page({}, { whatItDoes: { source: 'unknown', bullets: [] } })
  assert.ok(!unknown.includes('intent unknown'))
  for (const output of [page(), renderProofComment(input())])
    assert.ok(!output.includes('—'), 'no em dash in page or comment copy')
  const html = page()
  assert.ok(!outsideDebug(html).includes('Factory health'))
  assert.ok(!outsideDebug(html).includes('judge decisions ignored'))
  assert.ok(
    section(html, 'debug').includes('<p class="dim">Factory health: judge decisions ignored.</p>'),
  )
})

test('display caps cut at a word boundary and end in an ellipsis', () => {
  const words = 'Record finding with more words to reach the cap '.repeat(10)
  const capped = capText(words, 300)
  assert.ok(capped.length <= 300)
  assert.match(capped, /\S…$/, 'ends in an ellipsis right after a word')
  assert.ok(words.startsWith(`${capped.slice(0, -1)} `), 'the cut falls between two words')
  assert.equal(capText(TOKEN, 40).length, 40, 'an unbroken token is cut mid-token')
  assert.equal(capText('short', 40), 'short')
})
