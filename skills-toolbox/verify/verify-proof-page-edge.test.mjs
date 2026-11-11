// A synthetic edge-case proof page: unbroken 300-character tokens, record labels and facts at their
// caps, quoted and deleted paths in an ask diff, many notes, reviewer outcomes, blank still captions
// and a null transcript response. Each test names the defect it pins.
import test from 'node:test'
import assert from 'node:assert/strict'
import { capText, withDigest } from './verify-proof.mjs'
import { renderProofComment, renderProofHtml } from './verify-proof-page.mjs'
import { FILES_MISSING, fileAnchor } from './verify-proof-page-model.mjs'
import { cutSnippet, lineChecker, parseUnifiedDiff } from './verify-proof-snippet.mjs'
import { PROOF_STYLE } from './verify-proof-style.mjs'
import { validateProofSpec } from './verify-proof-spec.mjs'
import { validateProofMedia } from './verify-proof-media.mjs'
import { region } from '../../scripts/test-infra/gate-region-lib.mjs'

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

function proof({ whatItDoes, notes } = {}) {
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
      asks: ['logic'],
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
        false,
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
        body: 'Check it.',
        at: { path: 'src/cart.ts', line: 2 },
      },
      {
        level: 'high',
        title: 'Invented line',
        body: 'x',
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
    checklist: { before: [{ text: TOKEN, done: false }], after: [{ text: TOKEN, done: false }] },
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
  for (const [, columns] of PROOF_STYLE.matchAll(/grid-template-columns:([^;}]+)/g))
    assert.doesNotMatch(
      columns.replace(/minmax\(0,1fr\)/g, ''),
      /1fr/,
      `a bare 1fr column grows to its longest word: ${columns}`,
    )
  for (const [declaration] of PROOF_STYLE.matchAll(/[^{}\n]+\{[^}]*white-space:nowrap[^}]*\}/g))
    assert.match(
      declaration,
      /text-overflow:ellipsis|overflow-x:auto|^\.area \.count|^\.lb-full|^\.notes \.loc\{/,
      `nowrap text must clip or scroll: ${declaration}`,
    )
})

test('#3 #4 the checklist columns and nested risk items keep their own widths', () => {
  assert.match(rule('.checks'), /grid-template-columns:minmax\(0,1fr\) minmax\(0,1fr\)/)
  assert.doesNotMatch(PROOF_STYLE, /\.risks li[{+,]/, 'a nested ul.items li is not a risk row')
  assert.match(rule('.risks>li'), /grid-template-columns/)
})

test('#5 #6 #7 a 300-character record label and 20 facts are cut and folded', () => {
  const html = page()
  const reasons = region(html, '<ol class="reasons">', '</ol>')
  const link = /<a class="x" href="#a-docs">([^<]*)<\/a>/.exec(reasons)?.[1] ?? ''
  assert.ok(link.length <= 40 && link.endsWith('…'), `waiting link is cut: ${link}`)
  assert.doesNotMatch(rule('.reasons .x'), /nowrap/)
  const docs = region(html, '<details class="area" id="a-docs"', '<section id="evidence">')
  const name = /<span class="name" title="([^"]*)">([^<]*)<\/span>/.exec(docs)
  assert.equal(name?.[1], LONG_LABEL, 'the full label is the title')
  assert.ok((name?.[2].length ?? 99) <= 40)
  const summary = region(docs, '<summary>', '</summary>')
  assert.equal(summary.match(/Docs fact \d+/g)?.length, 6, 'six facts in the summary')
  assert.ok(region(docs, '<ul class="morefacts">', '</ul>').includes('Docs fact 20'))
})

test('#8 the chapter list scrolls inside the player instead of growing the panel', () => {
  assert.match(rule('.chapters'), /max-height:560px/)
  assert.match(rule('.chapters ol'), /overflow:auto;flex:1;min-height:0/)
})

test('#9 #10 #11 a lightbox still loads lazily, keeps its own size and offers full size', () => {
  const html = page()
  const box = region(
    html,
    '<div class="lightbox" id="shot-0">',
    '<div class="lightbox" id="shot-1">',
  )
  assert.match(
    box,
    /<img src="media\/head-0-0.png" alt="head-0 first" loading="lazy" decoding="async">/,
  )
  assert.ok(box.includes('<a class="lb-full" href="media/head-0-0.png">Full size</a>'))
  assert.match(rule('.lightbox figure'), /align-items:center/)
  assert.match(rule('.lightbox.full figure'), /overflow:auto/)
  const pairs = region(html, '<h3 class="sub">Before and after</h3>', '<div class="thumbs">')
  assert.equal(pairs.match(/<img /g)?.length, 6)
  assert.equal(
    pairs.match(/loading="lazy" decoding="async"/g)?.length,
    6,
    'every pair image is lazy',
  )
})

test('#12 closing a lightbox returns to its thumb, never to the top of Evidence', () => {
  const html = page()
  const box = region(
    html,
    '<div class="lightbox" id="shot-1">',
    '<div class="lightbox" id="shot-2">',
  )
  assert.ok(box.includes('<a class="lb-bg" href="#thumb-1" aria-label="Close"></a>'))
  assert.ok(box.includes('<a class="lb-x" href="#thumb-1">Close</a>'))
  assert.ok(!box.includes('href="#evidence"'))
  assert.ok(html.includes('<a class="thumb" id="thumb-1" href="#shot-1">'))
  assert.equal(
    rule(':root:not(.proof-js) .lightbox:target,.lightbox.open'),
    ':root:not(.proof-js) .lightbox:target,.lightbox.open{display:grid}',
    'once player.js runs, a :target lightbox shows only while the player keeps it open',
  )
})

test('#13 a clip with no poster shows its first still', () => {
  assert.match(page(), /<video id="proof-video"[^>]* poster="media\/head-0-0.png">/)
})

test('#15 before and after images share one height so their captions line up', () => {
  assert.match(
    rule('.pair img'),
    /[{;]height:360px;object-fit:contain/,
    'a fixed height, not a max',
  )
  assert.match(
    rule('.pair .nopage::before'),
    /[{;]height:360px;/,
    'a missing document page holds the same height, so the captions beside it line up',
  )
})

test('#16 #17 #18 #20 ask excerpts name files right, skip quoted ones and count the PR', () => {
  const html = page()
  const logic = region(
    html,
    '<details class="area" id="a-logic"',
    '<details class="area" id="a-docs"',
  )
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
    /assets\/a file\.txt<\/span><span class="stat"><b class="plus">\+250000<\/b>/,
    'excerpt counts come from the changed-file list',
  )
  assert.ok(logic.includes('@@ -1,1 +1,1 @@'), 'hunk headers carry counts')
  assert.ok(logic.includes('@@ -1,2 +0,0 @@'))
  assert.ok(logic.includes('<span class="cut">… line cut</span>'), 'a cut line says so')
  assert.match(rule('.diff td.t'), /tab-size:4/)
})

test('#17 git header paths unquote, lose their trailing tab and fall back to the base side', () => {
  const parsed = parseUnifiedDiff(ASK_DIFF).map((f) => f.path)
  assert.deepEqual(parsed.slice(0, 3), [
    'docs/Résumé.md',
    'assets/a file.txt',
    'legacy/remove-me.go',
  ])
})

test('#21 #22 #23 #24 notes show once, reviewers wait, the list is capped and :line survives', () => {
  const html = page()
  const notes = region(html, '<section id="notes">', '<section id="files">')
  const shown = [...notes.matchAll(/<li>.*?<\/li>/g)].map(([li]) => li.replace(/<[^>]+>/g, ''))
  assert.deepEqual(
    shown,
    [DUPLICATE, ...Array.from({ length: 19 }, (_, i) => `Record note ${i + 1}`)],
    'twenty reviewer notes fill the list and no agent note hides one',
  )
  assert.ok(
    notes.includes('<li class="more">9 more notes not shown.</li>'),
    'the agent notes count as more',
  )
  assert.ok(
    !notes.includes('lib/rev.go'),
    'a restated note keeps the record copy, not the agent location',
  )
  const few = region(
    page(undefined, { notes: [DUPLICATE, 'Record note 1'] }),
    '<section id="notes">',
    '<section id="files">',
  )
  const fewShown = [...few.matchAll(/<li>.*?<\/li>/g)].map(([li]) => li.replace(/<[^>]+>/g, ''))
  assert.deepEqual(fewShown.slice(0, 3), [
    DUPLICATE,
    'Record note 1',
    'P1…entVeryLongFileNameSegment.go:42A word severity',
  ])
  assert.equal(
    fewShown.length,
    11,
    'agent notes fill the slots the record leaves, the restated one dropped',
  )
  assert.ok(!few.includes('class="more"'))
  assert.ok(few.includes('<span class="ln">:42</span>'), 'the line number is never cut')
  assert.ok(few.includes('>…'), 'a long file name loses its start')
  const reasons = region(html, '<ol class="reasons">', '</ol>')
  assert.ok(
    reasons.indexOf('<li class="high"><div><div class="t">Security review failed</div>') <
      reasons.indexOf('Codex review was skipped'),
    'a failed security review leads, as a high reason',
  )
  assert.ok(reasons.includes('<div class="d">No quota</div>'))
  assert.ok(!reasons.includes('Review was'), 'a reviewer that ran is not a reason')
})

test('#25 a note severity is optional, words map to P-levels and a long one is left out', () => {
  const { spec: kept, dropped } = validateProofSpec(spec(), {
    head: HEAD,
    changedPaths: FILES.map((f) => f.path),
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

test('#26 a missing changed-file list is one explicit Generation check line', () => {
  const html = page({ files: null })
  const qa = region(html, 'id="generation"', '<footer>')
  assert.ok(qa.includes(`${FILES_MISSING} (3 dropped).`))
  assert.ok(!qa.includes('the changed files are unknown'))
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
  const qa = region(page({ spec: { ...spec(), extra: 1 } }), 'id="generation"', '<footer>')
  assert.ok(qa.includes('<li>spec.extra: unknown key</li>'))
})

test('#28 a risk that cites a line the file does not have at head is dropped', () => {
  const html = page()
  assert.ok(!html.includes('Invented line'))
  assert.ok(html.includes('spec risks[1]: at.line: src/cart.ts has no line 999999 at head'))
  const noGit = renderProofHtml(input(), extras({ hasLine: undefined }))
  assert.ok(noGit.includes('Invented line'), 'without git the line is not checked')
})

test('#29 #30 with no spec title the heading is short and the verdict sits in the meta line', () => {
  const html = renderProofHtml(
    { ...input(), verdict: { codes: ['always-human-path'], label: 'the verify judge' } },
    extras({ spec: spec({ title: undefined }) }),
  )
  assert.ok(html.includes('<h1>Confidence: Moderate. Inspect the asks before deciding.</h1>'))
  assert.match(region(html, '<div class="meta">', '<dl class="status">'), /<span>Not verified/)
  assert.ok(html.includes('<li>Rounds the total once with <code>round()</code></li>'))
})

test('leftovers: no intent-unknown bullet, no em dash, and factory health is a quiet line', () => {
  const unknown = page({}, { whatItDoes: { source: 'unknown', bullets: [] } })
  assert.ok(!unknown.includes('intent unknown'))
  for (const output of [page(), renderProofComment(input())])
    assert.ok(!output.includes('—'), 'no em dash in page or comment copy')
  const html = page()
  assert.ok(!region(html, '<header class="pr">', '</header>').includes('Factory health'))
  assert.ok(
    region(html, 'id="generation"', '<footer>').includes(
      '<p class="dim">Factory health: judge decisions ignored.</p>',
    ),
  )
})

test('display caps cut at a word boundary and end in an ellipsis', () => {
  const words = 'Record finding with more words to reach the cap '.repeat(10)
  const capped = capText(words, 300)
  assert.ok(capped.length <= 300)
  assert.match(capped, /\S\u2026$/, 'ends in an ellipsis right after a word')
  assert.ok(words.startsWith(`${capped.slice(0, -1)} `), 'the cut falls between two words')
  assert.equal(capText(TOKEN, 40).length, 40, 'an unbroken token is cut mid-token')
  assert.equal(capText('short', 40), 'short')
  const docs = region(page(), '<details class="area" id="a-docs"', '<section id="evidence">')
  assert.match(
    docs,
    /<span class="name" title="[^"]*">A record label that runs to the full\u2026<\/span>/,
  )
})
