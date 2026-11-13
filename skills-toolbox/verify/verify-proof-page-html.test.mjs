import test from 'node:test'
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { PROOF_STYLE } from './verify-proof-style.mjs'
import { PROOF_TOKENS } from './verify-proof-tokens.mjs'
import { withDigest } from './verify-proof.mjs'
import { proseHtml } from './verify-proof-code.mjs'
import { renderProofHtml } from './verify-proof-page.mjs'
import { NO_UI_RECORDING, fileAnchor } from './verify-proof-page-model.mjs'
import { contextResolver, cutSnippet, parseUnifiedDiff } from './verify-proof-snippet.mjs'
import { precedes, region, regionUntilNext } from '../../scripts/test-infra/gate-region-lib.mjs'

const HEAD = '1'.repeat(40)
const BASE = '2'.repeat(40)

function input(areas = ['ui', 'logic'], { ask = ['logic'] } = {}) {
  return {
    proof: withDigest({
      schema: 'boss-proof/1',
      repo: 'acme/shop',
      pr: 7,
      head: HEAD,
      base: BASE,
      confidence: {
        level: 'moderate',
        asks: areas.filter((name) => ask.includes(name)),
        loweredBy: [{ code: 'gap', text: 'Totals need a person' }],
      },
      whatItDoes: { source: 'pr-body', bullets: ['Shows the basket total'] },
      areas: areas.map((name) => ({
        name,
        label: { ui: 'UI', logic: 'Logic' }[name] ?? name,
        ask: ask.includes(name),
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
        impact: 'Check `round()`.',
        at: { path: 'src/cart.ts', line: 2 },
      },
      {
        level: 'low',
        title: 'Invented path',
        impact: 'x',
        at: { path: 'src/nowhere.ts', line: 1 },
      },
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
/** An author decision about `src/cart.ts` line 2. @param {string} question */
const decisionAt = (question, extra = {}) => ({
  id: 'D1',
  question,
  why: 'Totals change by a cent.',
  suggestion: 'Yes.',
  at: { path: 'src/cart.ts', line: 2 },
  ...extra,
})

/** The body of the page section with this id; a page without one fails the test. */
const section = (html, id) => {
  const found = new RegExp(`<section class="sec[^"]*" id="${id}"[^>]*>([\\s\\S]*?)</section>`).exec(
    html,
  )
  assert.ok(found, `the page has a #${id} section`)
  return found[1]
}
const sectionIds = (html) =>
  [...html.matchAll(/<section class="sec[^"]*" id="([^"]+)"/g)].map((m) => m[1])
const navIds = (html) =>
  [...region(html, '<nav class="secnav"', '</nav>').matchAll(/<a href="#([^"]+)"/g)].map(
    (m) => m[1],
  )
const bannerOf = (html) => region(html, '<section class="banner', '</section>')
const outsideDebug = (html) => html.replace(/<section class="sec debug"[\s\S]*?<\/section>/, '')
const debugOf = (html) => section(html, 'debug')
/** The labels of each `<div class="pair">` row, from its shots and missing-page boxes. */
const pairRows = (fragment) =>
  [...fragment.matchAll(/<div class="pair">(.*?)<\/div>/g)].map((m) =>
    [...m[1].matchAll(/<span(?: class="nopage")?>([^<]*)<\/span>/g)].map((c) => c[1]),
  )

test('a spec puts git-quoted, pinned diffs under its area and links its risk to them', () => {
  const html = render({ spec: spec(), snippet: resolver, files })
  assert.ok(html.includes('<h1>Round the basket total once</h1>'))
  const anchor = fileAnchor('src/cart.ts')
  assert.notEqual(anchor, fileAnchor('src/cart_ts'), 'paths that slug alike keep distinct anchors')
  const logic = section(html, 'a-logic')
  assert.ok(logic.includes(`<figure class="code" id="${anchor}">`), 'the snippet sits in its area')
  assert.equal(html.split(`id="${anchor}"`).length, 2, 'one anchor per path')
  const risks = section(html, 'risks')
  assert.ok(
    risks.includes(
      `<a class="lk" href="#${anchor}"><code class="loc" title="src/cart.ts:2">cart.ts:2</code></a>`,
    ),
    'the risk links to the quoted diff',
  )
  assert.ok(logic.includes('<tr class="add pinned"><td class="n"></td><td class="n">2</td>'))
  assert.ok(
    logic.includes('<span class="k">return</span> round(sum(items))'),
    'code comes from the diff',
  )
  assert.ok(logic.includes('<span class="s">&quot;&lt;b&gt;&quot;</span>'))
  assert.ok(logic.includes('<div class="pinnote">The new call.</div>'))
  assert.ok(risks.includes('<div class="q">Totals &lt;i&gt;round&lt;/i&gt; once</div>'))
  assert.ok(risks.includes('<p class="p">Check <code>round()</code>.</p>'))
  assert.ok(logic.includes('<li class="flag">One rounding step</li>'))
  assert.ok(bannerOf(html).includes('<a class="r" href="#a-logic">'), 'the signal row links')
  const meta = region(html, '<div class="meta">', '</header>')
  assert.ok(meta.includes('<span class="a">+31</span> <span class="d">−1</span>'))
  assert.ok(meta.includes('2 files'))
})

test('debug lists every dropped item and snippet problem, and what the agent was unsure of', () => {
  const html = render({ spec: spec(), snippet: resolver, files })
  const debug = debugOf(html)
  assert.ok(debug.includes('<li>spec risks[1]: at.path: src/nowhere.ts is not changed by this PR'))
  assert.ok(debug.includes('<li>src/cart.ts:3-6: shows no changed line, snippet dropped</li>'))
  assert.ok(debug.includes('<h3>The agent was unsure about</h3><ul><li>Discount rounding</li>'))
  assert.ok(!html.includes('Invented path'))
  for (const hidden of ['is not changed by this PR', 'snippet dropped', 'Discount rounding'])
    assert.ok(!outsideDebug(html).includes(hidden), `${hidden} shows only behind ?debug`)
  const clean = render({
    spec: spec({
      risks: [spec().risks[0]],
      areas: [{ ...spec().areas[0], snippets: [spec().areas[0].snippets[0]] }],
    }),
    snippet: resolver,
    files,
  })
  assert.ok(!debugOf(clean).includes('<h3>Dropped</h3>'), 'nothing dropped, no Dropped list')
  const noGit = render({ spec: spec(), files })
  assert.ok(debugOf(noGit).includes('src/cart.ts:1-5: no git to quote it from, snippet dropped'))
  assert.ok(!noGit.includes('class="code"'))
})

test('media becomes one player with chapters per clip, screenshots for the lightbox, pairs and transcripts', () => {
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
    html.includes(
      '<a class="shot" href="media/head-1-still.png" data-cap="Checkout still"><img src="media/head-1-still.png" alt="Checkout still" loading="lazy" decoding="async">',
    ),
    'a screenshot links its own image, so it opens without the script',
  )
  assert.equal(html.match(/<div class="lb" id="lb" hidden /g)?.length, 1, 'one hidden lightbox')
  assert.ok(!html.includes('class="lightbox"'), 'no :target lightbox per still')
  assert.deepEqual(pairRows(section(html, 'evidence')), [
    ['Before · Before still', 'After · Checkout still'],
  ])
  assert.ok(html.includes('&quot;total&quot;: &quot;&lt;3&gt;&quot;'))
  assert.equal(html.match(/<script[^>]*>/g)?.join(), '<script src="player.js" defer>')
  assert.ok(html.endsWith('<script src="player.js" defer></script></body></html>'))
  assert.ok(!/\son\w+\s*=|\sstyle=/.test(html), 'no inline handlers or style attributes')
  assert.ok(!render({}).includes('id="lb"'), 'a page with no screenshot has no lightbox')
})

test('evidence sits in the UI section a person reviews, else in its own section', () => {
  const standalone = render({ media: media() })
  assert.ok(sectionIds(standalone).includes('evidence'), 'a UI area nobody reviews')
  const reviewed = renderProofHtml(input(['ui', 'logic'], { ask: ['ui', 'logic'] }), {
    media: media(),
  })
  assert.ok(!sectionIds(reviewed).includes('evidence'))
  assert.ok(section(reviewed, 'a-ui').includes('<video id="proof-video"'))
  assert.ok(!section(reviewed, 'a-logic').includes('class="shot"'))
})

test('a changed UI area with no head clip is listed as not proven', () => {
  const gaps = (html) => regionUntilNext(bannerOf(html), '<ul class="glist gaps">', '</ul>')
  assert.ok(gaps(render({})).includes(NO_UI_RECORDING))
  assert.ok(!render({ media: media() }).includes(NO_UI_RECORDING))
  assert.ok(!renderProofHtml(input(['logic'])).includes(NO_UI_RECORDING))
})

test('Not proven lists each unproven claim and each review that did not finish', () => {
  const page = input(['logic'])
  page.proof.whatItDoes = {
    source: 'pr-body',
    bullets: ['Rounds the total', 'Keeps the export'],
    claims: [
      { proof: 'test', behaviour: true, test: 'rounds', path: 'src/cart_test.go' },
      { proof: 'unproven', behaviour: true, note: 'no test <named>' },
    ],
  }
  page.proof.review.models.push({ agent: 'Security', outcome: 'failed' })
  page.proof.confidence.loweredBy.push({
    code: 'unproven-claim',
    text: '1 behaviour claim(s) unproven.',
  })
  page.proof = withDigest(page.proof)
  const gaps = regionUntilNext(bannerOf(renderProofHtml(page)), '<ul class="glist gaps">', '</ul>')
  const items = [...gaps.matchAll(/<li>(.*?)<\/li>/g)].map((m) => m[1].replace(/<[^>]+>/g, ''))
  assert.deepEqual(items, [
    'Not provenKeeps the export: no test &lt;named&gt;',
    'The security review did not finish',
  ])
  assert.ok(!gaps.includes('Rounds the total'), 'a proven claim is not a gap')
})

test('a spec or manifest for another head is ignored, and only debug says so', () => {
  const html = render({
    spec: spec({ head: 'f'.repeat(40) }),
    media: { ...media(), head: 'f'.repeat(40) },
  })
  const debug = debugOf(html)
  assert.ok(debug.includes('<li>spec.head: written for another head</li>'))
  assert.ok(debug.includes('<li>media.head: captured for another head</li>'))
  assert.ok(!outsideDebug(html).includes('another head'))
  assert.ok(!html.includes('proof-video'))
  assert.ok(html.includes('<h1>acme/shop #7</h1>'), 'the other head titles nothing')
  assert.ok(!html.includes('Totals &lt;i&gt;'), 'nor adds its risks')
  assert.ok(section(html, 'a-logic').includes('logic fact'), 'the record fallback fills the page')
  assert.match(region(html, '<p class="reason">', '</p>'), /totals need a person/)
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
  const risks = section(html, 'risks')
  assert.ok(
    risks.includes(
      '<div class="item" id="risk-1"><div class="hd"><span class="pill danger">high</span><div class="q">Breaking API change in Logic</div></div><ul class="facts"><li>field removed</li>',
    ),
  )
  assert.ok(
    risks.includes(
      '<div class="item" id="risk-2"><div class="hd"><span class="pill warning">medium</span><div class="q">The PR description doesn&#39;t mention the logic change</div></div><ul class="facts"><li>Cart.total</li>',
    ),
  )
  assert.ok(html.includes('<h2>Risks<span class="n">2 · 1 high, 1 medium</span></h2>'))
  assert.ok(section(html, 'a-logic').includes('<span class="pill danger">breaking</span>'))
})

test('banner risk links resolve to the risks on the page, in the Risks section order', () => {
  const page = input(['logic'])
  Object.assign(page.proof.areas[0], {
    change: 'breaking',
    changeReasons: ['field removed'],
    undisclosed: true,
    unmentioned: ['Cart.total'],
  })
  page.proof = withDigest(page.proof)
  const html = renderProofHtml(page, {
    spec: spec({
      risks: [
        { level: 'low', title: 'Low one', impact: 'x', at: { path: 'src/cart.ts', line: 2 } },
        { level: 'medium', title: 'Medium one', impact: 'x', at: { path: 'src/cart.ts', line: 2 } },
        { level: 'high', title: 'High one', impact: 'x', at: { path: 'src/cart.ts', line: 2 } },
      ],
    }),
    files,
  })
  const links = [...bannerOf(html).matchAll(/<a href="#(risk-\d+)">.*?<\/span>([^<]*)<\/a>/g)]
  const items = [
    ...section(html, 'risks').matchAll(
      /<div class="item" id="(risk-\d+)">.*?<div class="q">([^<]*)<\/div>/g,
    ),
  ]
  assert.deepEqual(
    links.map((m) => [m[1], m[2]]),
    items.map((m) => [m[1], m[2]]),
  )
  assert.deepEqual(
    items.map((m) => m[2]),
    [
      'Breaking API change in Logic',
      'High one',
      'The PR description doesn&#39;t mention the logic change',
      'Medium one',
      'Low one',
    ],
    'high, then medium and low, record before spec within a level',
  )
})

test('an agent spec adds to the record and never hides a blocker, a reason or a breaking change', () => {
  const page = input(['logic'])
  page.proof.confidence.level = 'blocked'
  page.proof.review.blockers = [{ title: 'Data loss on retry', where: 'src/cart.ts:9' }]
  page.proof.review.notes = ['Name it.']
  Object.assign(page.proof.areas[0], { change: 'breaking', changeReasons: ['field removed'] })
  page.proof = withDigest(page.proof)
  const html = renderProofHtml(page, {
    spec: spec({
      areas: [],
      waiting: [{ title: 'Minor naming' }],
      decisions: [decisionAt('Glance at names?')],
    }),
    files,
    hasLine: () => true,
  })
  const banner = bannerOf(html)
  assert.ok(banner.startsWith('<section class="banner lv-danger"'))
  assert.ok(banner.includes('<b>Blocked</b>'))
  const findings = section(html, 'findings')
  assert.ok(
    findings.startsWith(
      '<h2>Review findings<span class="n">2 · 1 blocks merge</span></h2><div class="items"><div class="item" id="finding-1"><div class="hd"><span class="pills"><span class="pill danger">blocks merge</span></span><div class="q">Data loss on retry</div><code class="loc" title="src/cart.ts:9">cart.ts:9</code>',
    ),
    'the blocker leads the findings',
  )
  assert.equal(findings.split('Name it.').length, 2, 'a note the spec repeats shows once')
  const risks = section(html, 'risks')
  assert.ok(precedes(risks, 'Breaking API change in Logic', 'Totals &lt;i&gt;round&lt;/i&gt; once'))
  const decide = section(html, 'decide')
  assert.ok(precedes(decide, 'Glance at names?', 'Minor naming'), 'the author decides first')
})

test('sections follow one order: risks, decisions, reviewed areas by kind, findings, after merge, files, debug', () => {
  const names = ['logic', 'ui', 'security', 'api-contract', 'migrations']
  const page = input(names, { ask: names })
  page.proof.review.notes = ['A note']
  page.proof = withDigest(page.proof)
  const html = renderProofHtml(
    { ...page, afterMerge: ['Watch the rollout'] },
    {
      spec: spec({ decisions: [decisionAt('Is one step enough?')] }),
      snippet: resolver,
      files,
      media: media(),
      hasLine: () => true,
    },
  )
  const order = [
    'risks',
    'decide',
    'a-migrations',
    'a-api-contract',
    'a-security',
    'a-ui',
    'a-logic',
    'findings',
    'after',
    'files',
    'debug',
  ]
  assert.deepEqual(sectionIds(html), order)
  assert.deepEqual(navIds(html), order.slice(0, -1), 'the nav lists every section but debug')
  assert.deepEqual(
    [...bannerOf(html).matchAll(/<a class="r" href="#([^"]+)">/g)].map((m) => m[1]),
    order.filter((id) => id.startsWith('a-')),
    'the banner signals follow the same order',
  )
  assert.ok(precedes(html, '<header class="pr">', '<section class="banner'))
  assert.ok(precedes(html, '<section class="banner', '<nav class="secnav"'))
  assert.ok(precedes(html, '<nav class="secnav"', '<section class="sec"'))
})

test('a record-only page has only the sections it has content for', () => {
  const html = renderProofHtml(input(['logic']))
  assert.deepEqual(sectionIds(html), ['a-logic', 'debug'])
  assert.deepEqual(navIds(html), ['a-logic'])
  const checked = renderProofHtml(input(['logic'], { ask: [] }))
  assert.deepEqual(sectionIds(checked), ['debug'], 'an area nobody reviews has no section')
  assert.ok(!bannerOf(checked).includes('class="sig"'), 'nor a signal row')
  assert.ok(!checked.includes('<nav'), 'and with nothing to list there is no nav')
})

test('the page never shows the placeholders and pipeline lines the banner design removed', () => {
  const REMOVED = [
    "Why it's waiting",
    'Why it&#39;s waiting',
    'Factory health',
    'judge decisions ignored',
    'intent unknown',
    'planned, none captured',
    'Checked: not shown',
    'Checked: proven',
    'Asks for a person',
    'Nothing found.',
    'Nothing is holding it.',
    'Nothing.',
    'Waits for you',
    'Generation check',
    'class="lightbox"',
    'class="does"',
    '<dl class="status">',
    '<ul class="timeline">',
    'Inspect the asks before deciding',
  ]
  const record = input(['logic'])
  record.proof.health = [{ code: 'judge-decisions-ignored', text: 'judge decisions ignored' }]
  record.proof.whatItDoes = { source: 'unknown', bullets: [] }
  record.proof = withDigest(record.proof)
  for (const html of [
    renderProofHtml(record),
    renderProofHtml(input(['logic'], { ask: [] })),
    render({ spec: spec({ head: 'f'.repeat(40) }), files }),
    render({ spec: spec(), snippet: resolver, files, media: media() }),
  ])
    for (const removed of REMOVED) assert.ok(!outsideDebug(html).includes(removed), removed)
})

test('the Files grid groups by reviewed area, then by kind, on the scale of the largest group', () => {
  const page = input(['logic', 'ui'], { ask: ['logic'] })
  const html = renderProofHtml(page, {
    files: [
      { path: 'src/cart.ts', adds: 3, dels: 1 },
      { path: 'src/view.tsx', adds: 5, dels: 0 },
      { path: 'README.md', adds: 2, dels: 0 },
      { path: 'src/cart.test.ts', adds: 40, dels: 0 },
      { path: 'gen/api_gen.go', adds: 10, dels: 10 },
      { path: 'assets/logo.png', adds: null, dels: null },
    ],
    areas: [
      { name: 'logic', paths: ['src/cart.ts'] },
      { name: 'ui', paths: ['src/view.tsx'] },
    ],
  })
  const grid = section(html, 'files')
  const groups = [
    ...grid.matchAll(
      /<summary class="frow"><span class="nm">([^<]*)<\/span><span class="c2">(\d+)<\/span><span class="c3[^"]*">\+(\d+)<\/span><span class="c4[^"]*">−(\d+)<\/span><span class="bar"[^>]*><i class="a bw-(\d+)"><\/i><i class="d bw-(\d+)"><\/i>/g,
    ),
  ].map((m) => [m[1], ...m.slice(2).map(Number)])
  assert.deepEqual(groups, [
    ['Logic', 1, 3, 1, 8, 3],
    ['No sign-off needed', 3, 7, 0, 18, 0],
    ['Tests', 1, 40, 0, 100, 0],
    ['Generated', 1, 10, 10, 25, 25],
  ])
  const rows = (name) =>
    [
      ...regionUntilNext(grid, `<span class="nm">${name}</span>`, '</details>').matchAll(
        /<bdi>([^<]*)<\/bdi>/g,
      ),
    ].map((m) => m[1])
  assert.deepEqual(rows('No sign-off needed'), ['src/view.tsx', 'README.md', 'assets/logo.png'])
  assert.ok(grid.includes('<span class="c3 z">bin</span>'), 'a binary file says so')
  assert.ok(grid.includes('<h2>Files<span class="n">6</span></h2>'))
  const style = /<style>([\s\S]*?)<\/style>/.exec(html)[1]
  for (const width of [0, 3, 8, 18, 25, 100])
    assert.ok(style.includes(`.bw-${width}{width:${width}%}`), `the sheet sizes bw-${width}`)
  assert.ok(
    html.includes(`style-src 'sha256-${createHash('sha256').update(style).digest('base64')}'`),
    'the appended widths are inside the hashed sheet',
  )
  assert.ok(!sectionIds(renderProofHtml(page)).includes('files'), 'no git, no Files section')
})

test('the theme is the design tokens, and diffs and counts colour from them', () => {
  assert.ok(PROOF_STYLE.startsWith(PROOF_TOKENS))
  assert.match(PROOF_TOKENS, /^:root\{--bg:#/, 'dark by default')
  assert.match(PROOF_TOKENS, /@media \(prefers-color-scheme:light\)\{:root:not\(\[data-theme\]\)\{/)
  assert.match(PROOF_TOKENS, /:root\[data-theme="light"\]\{/)
  assert.match(PROOF_TOKENS, /:root\[data-theme="dark"\]\{/)
  const sheet = PROOF_STYLE.slice(PROOF_TOKENS.length)
  assert.doesNotMatch(sheet, /#[0-9a-f]{3,8}\b/i, 'the sheet itself names no colour')
  const rule = (selector) => {
    const found = new RegExp(
      `\\n${selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}\\{[^}]*\\}`,
    ).exec(PROOF_STYLE)
    assert.ok(found, `no rule for ${selector}`)
    return found[0]
  }
  assert.match(rule('.code tr.add'), /var\(--success-bg\)/)
  assert.match(rule('.code tr.del'), /var\(--danger-bg\)/)
  assert.match(rule('.code td.n'), /white-space:nowrap/, 'line numbers never wrap')
  assert.match(rule('.diffstat .a'), /var\(--success\)/)
})

test('every custom property the sheet reads is a token or set by the sheet itself', () => {
  const defined = new Set([...PROOF_TOKENS.matchAll(/(--[\w-]+):/g)].map((m) => m[1]))
  const sheet = PROOF_STYLE.slice(PROOF_TOKENS.length)
  const undefinedNames = [
    ...new Set([...sheet.matchAll(/var\((--[\w-]+)\)/g)].map((m) => m[1])),
  ].filter((name) => !defined.has(name) && !sheet.includes(`${name}:`))
  assert.deepEqual(undefinedNames, [], 'an undefined var() voids the declaration that reads it')
})

test('prose sets identifiers as code, leaves words and brands plain and escapes first', () => {
  const html = proseHtml(
    'Run ./bin/migrate on services/api/main.go, set DATABASE_URL, call fetchAll() and read user.createdAt and due_on with repo:write; GitHub, GraphQL and plain words stay',
  )
  for (const code of [
    './bin/migrate',
    'services/api/main.go',
    'DATABASE_URL',
    'fetchAll()',
    'user.createdAt',
    'due_on',
    'repo:write',
  ])
    assert.ok(html.includes(`<code>${code}</code>`), code)
  for (const plain of ['GitHub', 'GraphQL', 'plain', 'words', 'Run'])
    assert.ok(!html.includes(`<code>${plain}</code>`), plain)
  assert.equal(
    proseHtml('a <b onclick="x()">bold</b> `<i>`'),
    'a &lt;b onclick=&quot;<code>x()</code>&quot;&gt;bold&lt;/b&gt; <code>&lt;i&gt;</code>',
  )
  const tickets = { 'FRE-12': 'https://linear.app/acme/issue/FRE-12' }
  assert.equal(
    proseHtml('Fixes FRE-12 and BOS-9', tickets),
    'Fixes <a class="tk" href="https://linear.app/acme/issue/FRE-12">FRE-12</a> and BOS-9',
  )
})

test('inline code scales with the text around it, so a code span in a title is title-sized', () => {
  const inline = /\ncode\{([^}]*)\}/.exec(PROOF_STYLE)?.[1] ?? ''
  assert.match(inline, /font:[\d.]+em /, 'sized in em')
  for (const [, selector, body] of PROOF_STYLE.matchAll(/\n([^{}\n]*\bcode\b[^{}\n]*)\{([^}]*)\}/g))
    if (!/\.code\b|\.loc|\.sha/.test(selector))
      assert.doesNotMatch(body, /font(-size)?:[^;]*\d+px/, `${selector} sets no fixed size`)
})

test('a table.column or a dotfile is one code span, not a snake_case word and a stray dot', () => {
  assert.equal(
    proseHtml('Adds stg_users.is_claims_staff and edits .boss-skills.json'),
    'Adds <code>stg_users.is_claims_staff</code> and edits <code>.boss-skills.json</code>',
  )
})

test('every banner block puts its label in one shared column and hangs the risk dot in the gutter', () => {
  const rule = (/** @type {string} */ selector) =>
    new RegExp(`\\n${selector.replace(/[.*]/g, '\\$&')}\\{([^}]*)\\}`).exec(PROOF_STYLE)?.[1] ?? ''
  assert.match(rule('.banner'), /--lbl-w:\d+px/)
  for (const selector of ['.glist li', '.sig .r'])
    assert.match(rule(selector), /grid-template-columns:var\(--lbl-w\) /, selector)
  const gap = Number(/gap:(\d+)px/.exec(rule('.rsum a'))?.[1])
  const dot = Number(/width:(\d+)px/.exec(rule('.dot'))?.[1])
  assert.match(
    rule('.rsum .dot'),
    new RegExp(`margin-left:-${dot + gap}px`),
    'dot and gap hang outside',
  )
})

test('a ticket id links only where an area link in the record names that ticket', () => {
  const page = input(['logic'])
  page.proof.areas[0].links = [{ text: 'FRE-12', url: 'https://linear.app/acme/issue/FRE-12' }]
  page.proof.review.notes = ['Follows FRE-12, unlike BOS-9 and FRE-13']
  page.proof = withDigest(page.proof)
  const findings = section(renderProofHtml(page), 'findings')
  assert.ok(
    findings.includes('<a class="tk" href="https://linear.app/acme/issue/FRE-12">FRE-12</a>'),
  )
  assert.ok(
    findings.includes('unlike BOS-9 and FRE-13'),
    'tickets the record never links stay text',
  )
  const byUrl = input(['logic'])
  byUrl.proof.areas[0].links = [{ text: 'the ticket', url: 'https://linear.app/acme/issue/FRE-13' }]
  byUrl.proof.review.notes = ['See FRE-13']
  byUrl.proof = withDigest(byUrl.proof)
  assert.ok(
    section(renderProofHtml(byUrl), 'findings').includes(
      '<a class="tk" href="https://linear.app/acme/issue/FRE-13">FRE-13</a>',
    ),
    'a link whose URL names the ticket counts too',
  )
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
  assert.deepEqual(pairRows(docs), [
    ['Before · base page 1', 'After · head page 1'],
    ['Before · base page 2', 'After · no page 2'],
    ['Before · no page 3', 'After · head page 3'],
  ])
  assert.ok(
    docs.includes(
      '<a class="shot" href="media/statement-head-p3.png" data-cap="head page 3"><img src="media/statement-head-p3.png" alt="head page 3" loading="lazy" decoding="async">',
    ),
    'a page opens its own image in the lightbox',
  )
  assert.ok(!docs.includes('statement-head-p2.png'))
  assert.equal(
    new Set([...html.matchAll(/ id="([^"]+)"/g)].map((m) => m[1])).size,
    html.match(/ id="/g).length,
    'every id is unique',
  )
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
  assert.deepEqual(pairRows(docs), [
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
    'a framed review view, which may not download, says where the PDF downloads',
  )
  assert.ok(!docs.includes('Download base PDF'))
  assert.match(PROOF_STYLE, /\n\.framed-note\{display:none\}/)
  assert.match(PROOF_STYLE, /\n\.proof-framed \.doc header a\{display:none\}/)
  assert.match(PROOF_STYLE, /\.proof-framed \.framed-note\{display:inline\}/)
  assert.ok(!docs.includes('class="pair"'))
  assert.equal(
    region(docs, '<div class="thumbs">', '</article>').match(
      /<a class="shot" href="media\/statement-head-p\d\.png"/g,
    )?.length,
    3,
  )
})

const PARENT = {
  pr: 12,
  title: 'Add the refunds model',
  url: 'https://github.com/acme/shop/pull/12',
  branch: 'feat/refunds',
  shared: 3,
  total: 5,
}
/** The notice above the title for this branch context, or '' when there is none. */
const notice = (branch) =>
  /<header class="pr">(<div class="bnotice[\s\S]*?<\/div><\/div>)?/.exec(
    renderProofHtml({ ...input(), branch }),
  )?.[1] ?? ''
const text = (html) => html.replace(/<[^>]+>/g, '')

test('a PR stacked by its base says so in amber above the title, and links the parent in a new tab', () => {
  const html = notice({
    base: 'feat/refunds',
    default: 'main',
    stackedOn: [{ ...PARENT, how: 'base' }],
  })
  assert.ok(html.startsWith('<div class="bnotice stacked">'))
  assert.equal(
    text(html),
    'Stacked on #12.It merges into feat/refunds, not main. Review it after #12 merges.',
  )
  assert.ok(
    html.includes(
      '<a href="https://github.com/acme/shop/pull/12" target="_blank" rel="noopener noreferrer">#12</a>',
    ),
  )
  assert.ok(html.includes('<code>feat/refunds</code>'))
})

test('a PR that carries another PR’s commits says how many, and how far behind its copy is', () => {
  const behind = notice({
    base: 'main',
    default: 'main',
    stackedOn: [{ ...PARENT, how: 'content' }],
  })
  assert.ok(behind.startsWith('<div class="bnotice stacked">'))
  assert.equal(
    text(behind),
    "Carries 3 of #12's 5 commits.Merging this ships them too, and its copy is 2 commits behind. Review it after #12 merges.",
  )
  const current = notice({
    base: 'main',
    default: 'main',
    stackedOn: [{ ...PARENT, shared: 5, how: 'content' }],
  })
  assert.equal(
    text(current),
    "Carries 5 of #12's 5 commits.Merging this ships them too. Review it after #12 merges.",
  )
})

test('a PR into a branch that is not the default says so in blue; into the default, nothing shows', () => {
  const html = notice({ base: 'production', default: 'main', stackedOn: [] })
  assert.ok(html.startsWith('<div class="bnotice branch">'))
  assert.equal(text(html), 'Merges into production.Not the default branch main.')
  assert.equal(notice({ base: 'main', default: 'main', stackedOn: [] }), '')
  assert.equal(notice(null), '')
})

test('branch context is validated strictly before it reaches the page', () => {
  for (const branch of [
    'main',
    { base: 'main', default: 'main' },
    { base: '', default: 'main', stackedOn: [] },
    { base: 'main', default: 'main', stackedOn: [], extra: 1 },
    { base: 'main', default: 'main', stackedOn: [{ ...PARENT, how: 'sideways' }] },
    {
      base: 'main',
      default: 'main',
      stackedOn: [{ ...PARENT, how: 'base', url: 'javascript:alert(1)' }],
    },
    { base: 'main', default: 'main', stackedOn: [{ ...PARENT, how: 'base', shared: 6 }] },
    { base: 'main', default: 'main', stackedOn: [{ ...PARENT, how: 'base', pr: 0 }] },
  ])
    assert.throws(() => renderProofHtml({ ...input(), branch }), TypeError, JSON.stringify(branch))
})

test('a hostile branch name is escaped', () => {
  const html = notice({ base: '<img src=x onerror=alert(1)>', default: 'main', stackedOn: [] })
  assert.ok(html.includes('<code>&lt;img src=x onerror=alert(1)&gt;</code>'))
})

const GONE = parseUnifiedDiff(
  '--- a/src/old.ts\n+++ /dev/null\n@@ -1,4 +0,0 @@\n-export const a = 1\n-export const b = 2\n-export const c = 3\n-export const d = 4\n',
)[0]
const context = contextResolver((path) => ({ 'src/cart.ts': DIFF, 'src/old.ts': GONE })[path])
/** @param {object} review @param {object} [extra] */
const withReview = (review, extra = {}) => ({
  ...input(['logic']),
  review: { findings: [], acceptance: [], ...review },
  ...extra,
})
const finding = (over = {}) => ({
  id: 'review:aaa',
  severity: 'P2',
  status: 'open',
  title: 'Rounds twice',
  detail: 'round() already ran upstream.',
  file: 'src/cart.ts',
  line: 2,
  ...over,
})
/** The markup of the item with this id, up to the next item. @param {string} html @param {string} id */
const item = (html, id) => {
  const start = html.indexOf(`<div class="item" id="${id}">`)
  assert.ok(start >= 0, `the page has item #${id}`)
  const next = html.indexOf('<div class="item"', start + 1)
  return html.slice(start, next < 0 ? undefined : next)
}

test('a review finding shows its severity, its status and the code around its line', () => {
  const html = renderProofHtml(
    withReview({
      findings: [
        finding(),
        finding({
          id: 'review:bbb',
          severity: 'P1',
          status: 'fixed',
          title: 'Total was unrounded',
          line: 4,
        }),
      ],
    }),
    { context, files },
  )
  const open = item(section(html, 'findings'), 'finding-1')
  assert.ok(
    open.includes(
      '<span class="pills"><span class="pill warning">P2</span><span class="pill warning">open</span></span><div class="q">Rounds twice</div>',
    ),
  )
  assert.ok(open.includes('<p class="p"><code>round()</code> already ran upstream.</p>'))
  assert.ok(!open.includes('class="loc"'), 'the code names the file, so no location repeats it')
  const rows = [
    ...open.matchAll(
      /<tr class="(add|del|ctx)( pinned)?"><td class="n">(\d*)<\/td><td class="n">(\d*)<\/td>/g,
    ),
  ]
  assert.deepEqual(
    rows.map(([, kind, pinned, old, now]) => `${kind}${pinned ? '*' : ''} ${old}/${now}`),
    ['ctx 1/1', 'del 2/', 'add* /2', 'ctx 3/3', 'ctx 4/4'],
    'two lines either side of line 2, the base line it replaced, and line 2 highlighted',
  )
  const fixed = item(section(html, 'findings'), 'finding-2')
  assert.ok(
    fixed.includes('<span class="pill danger">P1</span><span class="pill success">fixed</span>'),
  )
  assert.ok(fixed.includes('<tr class="ctx pinned"><td class="n">4</td><td class="n">4</td>'))
  assert.ok(
    section(html, 'findings').startsWith('<h2>Review findings<span class="n">2 · 1 open</span>'),
  )
})

test('a finding on a deleted line quotes the base side', () => {
  const html = renderProofHtml(
    withReview({ findings: [finding({ file: 'src/old.ts', line: 3 })] }),
    {
      context,
      files,
    },
  )
  const rows = [
    ...item(html, 'finding-1').matchAll(/<tr class="(del)( pinned)?"><td class="n">(\d+)/g),
  ]
  assert.deepEqual(
    rows.map(([, , pinned, old]) => `${old}${pinned ? '*' : ''}`),
    ['1', '2', '3*', '4'],
  )
})

test('a finding shows once, and a note with no place shows its text once with no code', () => {
  const page = withReview({
    findings: [
      finding({ status: 'note', severity: 'P3', title: 'Name it' }),
      finding({ status: 'note', severity: 'P3', title: 'Name it' }),
      { id: 'review:ccc', severity: 'P3', status: 'note', title: 'Consider a helper' },
    ],
  })
  page.proof.review.notes = ['Name it', 'Consider a helper']
  page.proof = withDigest(page.proof)
  const html = section(renderProofHtml(page, { context, files }), 'findings')
  assert.equal(html.split('Name it').length, 2, 'one review id and its unlocated repeat show once')
  assert.equal(html.split('Consider a helper').length, 2)
  const bare = item(html, 'finding-2')
  assert.ok(bare.includes('<div class="q">Consider a helper</div></div></div>'), 'text only')
})

test('the ticket section marks a criterion done only when its check passed on this head', () => {
  const OLD = '3'.repeat(40)
  const row = (id, text, result, check, head = HEAD) => ({
    id,
    text,
    result,
    ...(check ? { check } : {}),
    head,
  })
  const html = renderProofHtml(
    withReview(
      {
        findings: [finding()],
        acceptance: [
          row('AC1', 'Totals round once in src/cart.ts', 'met', 'pnpm test cart'),
          row('AC2', 'Shows the total', 'met', 'pnpm test total', OLD),
          row('AC3', 'Keeps discounts', 'unmet', 'pnpm test discount'),
          row('AC4', 'Reads well', 'unchecked'),
        ],
      },
      { ticket: { id: 'SHOP-12', url: 'https://linear.app/acme/issue/SHOP-12' } },
    ),
    { context, files },
  )
  const ticket = section(html, 'ticket')
  assert.ok(ticket.startsWith('<h2>Does it satisfy SHOP-12?<span class="n">4 · 3 not done</span>'))
  const status = (id) =>
    new RegExp(`<tr id="ac-${id}">.*?<span class="pill \\w+">(\\w+)</span></td></tr>`).exec(
      ticket,
    )?.[1]
  assert.deepEqual(['ac1', 'ac2', 'ac3', 'ac4'].map(status), [
    'done',
    'unverified',
    'missed',
    'unverified',
  ])
  assert.ok(ticket.includes('Checked on <code>3333333</code>, an earlier head'))
  assert.ok(
    ticket.includes(
      '<span class="evc" title="pnpm test cart">check: <code>pnpm test cart</code></span>',
    ),
  )
  assert.ok(
    ticket.includes('<a class="evc" href="#finding-1" title="Rounds twice">finding: F1</a>'),
    'the criterion naming the file links its finding',
  )
  assert.ok(
    item(section(html, 'findings'), 'finding-1').includes(
      '<a class="lk" href="#ac-ac1" title="Totals round once in src/cart.ts">AC1</a>',
    ),
  )
  assert.ok(precedes(html, 'id="ticket"', 'id="findings"'))
})

test('link chips join a finding to the decision that names its file and the risk at its lines', () => {
  const html = renderProofHtml(withReview({ findings: [finding()] }), {
    context,
    files,
    snippet: resolver,
    spec: spec({
      areas: [],
      decisions: [decisionAt('Is one rounding step in src/cart.ts enough?')],
      notes: [],
    }),
    hasLine: () => true,
  })
  const chips = item(section(html, 'findings'), 'finding-1')
  assert.ok(
    chips.includes(
      '<a class="lk" href="#risk-1" title="Totals &lt;i&gt;round&lt;/i&gt; once">R1</a>',
    ),
  )
  assert.ok(
    chips.includes(
      '<a class="lk" href="#decide-1" title="Is one rounding step in src/cart.ts enough?">D1</a>',
    ),
  )
  assert.ok(
    section(html, 'decide').includes('<a class="lk" href="#finding-1" title="Rounds twice">F1</a>'),
    'the decision links back',
  )
  assert.ok(
    section(html, 'risks').includes('<a class="lk" href="#finding-1" title="Rounds twice">F1</a>'),
  )
})

/** The record-only page input with describer facts for the asking `logic` area. */
const described = (facts) => ({ ...input(), described: { logic: facts } })
const SCHEMA = {
  kind: 'schema',
  types: [
    {
      keyword: 'type',
      name: 'Mutation',
      change: 'changed',
      class: 'breaking',
      mobile: 'Shipped builds still send `archive`',
      fields: [
        {
          name: 'markOverdue',
          args: [
            { name: 'asOf', type: 'Date!' },
            { name: 'dryRun', type: 'Boolean', default: 'false' },
          ],
          type: 'Int!',
          directives: ['@auth'],
          change: 'added',
        },
        { name: 'archive', type: 'Boolean', change: 'removed' },
      ],
    },
    {
      keyword: 'enum',
      name: 'Status',
      change: 'changed',
      fields: [{ name: 'SENT' }, { name: 'OVERDUE', change: 'added' }],
    },
  ],
}
const CALL = {
  kind: 'client-call',
  name: 'Mutation.markOverdue',
  path: 'web/src/useMarkOverdue.ts',
  operation: 'mutation M($asOf: Date!) { markOverdue(asOf: $asOf) @auth { count total } }',
  before: ['useInvoices()'],
  after: ['useMarkOverdue()'],
}
const column = (name, extra = {}) => ({ name, type: 'text', ...extra })
const TABLES = [
  {
    kind: 'table',
    name: 'refunds',
    relation: 'dbt-model',
    materialization: 'incremental',
    change: 'added',
    columns: [
      column('id', { constraints: 'not null', note: 'primary key' }),
      column('email', { pii: true }),
      ...['a', 'b', 'c', 'd', 'e'].map((name) => column(name)),
    ],
  },
  {
    kind: 'table',
    name: 'invoices',
    relation: 'table',
    change: 'changed',
    columns: [column('due_on', { change: 'added' }), column('legacy', { change: 'removed' })],
  },
]

test('a schema block reads as names and types in two columns, with no added-line markers', () => {
  const area = section(renderProofHtml(described({ blocks: [SCHEMA] }), { files }), 'a-logic')
  const schema = region(area, '<div class="schema">', '</div></div></div>')
  assert.ok(
    schema.includes(
      '<div class="sbh"><span class="kw">type</span><span class="tn">Mutation</span><span class="dim">1 added, 1 removed</span><span class="pill danger">breaking</span></div>',
    ),
  )
  assert.ok(
    schema.includes(
      '<div class="fr ad"><span class="fnc"><span class="fn">markOverdue</span><span class="pu">(</span><span class="args"><span><span class="fn">asOf</span><span class="pu">: </span><span class="ty">Date</span><span class="nn">!</span></span><span><span class="fn">dryRun</span><span class="pu">: </span><span class="ty">Boolean</span><span class="dr"> = false</span></span></span><span class="pu">)</span></span><span class="ftc"><span class="ty">Int</span><span class="nn">!</span> <span class="dr">@auth</span></span></div>',
    ),
    'two arguments stack, the type sits in its own column and the directive is faint',
  )
  assert.ok(schema.includes('<div class="fr rm"><span class="fnc"><span class="fn">archive</span>'))
  assert.ok(schema.includes('<span class="enumv">SENT</span><span class="enumv ad">OVERDUE</span>'))
  assert.ok(
    schema.includes(
      '<div class="mob"><b>Old mobile builds</b><span>Shipped builds still send <code>archive</code></span></div>',
    ),
  )
  assert.doesNotMatch(schema, /class="(add|al)"|>\+ /, 'no wall of green plus signs')
  assert.match(PROOF_STYLE, /\.fields\{display:grid;grid-template-columns:fit-content\(60%\)/)
})

test('a client call prints its operation indented and its call site as a red and green diff', () => {
  const area = section(renderProofHtml(described({ blocks: [CALL] }), { files }), 'a-logic')
  assert.ok(area.includes('<h3 class="sub">Client calls</h3>'))
  assert.ok(
    area.includes(
      '<pre class="gql"><span class="kw">mutation</span> M(<span class="nn">$asOf</span>: <span class="ty">Date</span>!) {\n  markOverdue(asOf: <span class="nn">$asOf</span>) @auth {\n    count\n    total\n  }\n}</pre>',
    ),
  )
  assert.ok(
    area.includes(
      '<pre class="gql"><span class="dl">- useInvoices()</span>\n<span class="al">+ useMarkOverdue()</span></pre>',
    ),
  )
  assert.ok(
    area.includes('<code class="loc" title="web/src/useMarkOverdue.ts">useMarkOverdue.ts</code>'),
  )
})

test('a route names its method, path and auth, and an unauthenticated one is flagged', () => {
  const route = { kind: 'route', method: 'POST', path: '/hooks/run', auth: 'none', change: 'added' }
  const html = renderProofHtml(
    described({ blocks: [route, { ...route, method: 'GET', auth: 'session', change: 'removed' }] }),
    { files },
  )
  const routes = section(html, 'a-logic')
  assert.ok(
    routes.includes(
      '<div class="rt"><span class="mt">POST</span><span class="rp"><code>/hooks/run</code></span><span class="pill danger">no auth</span></div>',
    ),
  )
  assert.ok(
    routes.includes(
      '<div class="rt rm"><span class="mt">GET</span><span class="rp"><code>/hooks/run</code> <span class="ch">removed</span></span><span class="pill muted">session</span></div>',
    ),
  )
})

test('tables share one column grid, flag personal data, and a long new table names its plain columns once', () => {
  const area = section(renderProofHtml(described({ blocks: TABLES }), { files }), 'a-logic')
  assert.equal(
    area.split('<colgroup><col class="cn"><col class="ct"><col></colgroup>').length - 1,
    2,
    'every table uses the shared column widths',
  )
  assert.match(PROOF_STYLE, /table\.cols\{width:100%;table-layout:fixed/)
  assert.ok(
    area.includes(
      '<span class="dim">dbt model · incremental</span><span class="pill success">added</span>',
    ),
  )
  assert.ok(area.includes('<td class="cn">email<span class="pii">PII</span></td>'))
  assert.ok(
    area.includes(
      '<td class="ct">text · not null</td><td class="cx"><span>primary key</span></td>',
    ),
  )
  assert.ok(
    area.includes('<tr class="also"><td colspan="3">Also <span>a, b, c, d, e</span></td></tr>'),
  )
  assert.ok(
    !area.includes('<td class="cn">a</td>'),
    'a plain column of a long new table is not a row',
  )
  assert.ok(area.includes('<td class="cx"><span class="pill success">added</span></td>'))
  assert.ok(area.includes('<tr class="rm"><td class="cn">legacy</td>'))
})

test('a code block numbers its lines from its start, and steps join After merge as ticks', () => {
  const html = renderProofHtml(
    described({
      blocks: [
        { kind: 'code', path: 'src/cart.ts', start: 41, lines: ['export function total() {', '}'] },
        { kind: 'steps', steps: ['Run `./migrate`', 'Run `./seed`'] },
      ],
    }),
    { files },
  )
  const area = section(html, 'a-logic')
  assert.ok(area.includes('<span class="path" title="src/cart.ts">src/cart.ts:41</span>'))
  assert.ok(area.includes('<tr><td class="n">42</td><td class="t">}</td></tr>'))
  const after = section(html, 'after')
  assert.ok(
    after.includes('<input type="checkbox" class="check"><span>Run <code>./migrate</code></span>'),
  )
  assert.ok(after.includes('<span>Run <code>./seed</code></span>'))
  assert.ok(!area.includes('./migrate'), 'steps show only under After merge')
})

test("describer signals replace their area's banner row, and a none signal shows nothing", () => {
  const html = renderProofHtml(
    described({
      signals: [
        { key: 'db', label: 'Database', state: 'additive', summary: 'Adds a column' },
        { key: 'sec', label: 'Security', state: 'risky', summary: 'A public route' },
        { key: 'ui', label: 'UI', state: 'none', summary: 'Nothing' },
      ],
    }),
  )
  const rows = [
    ...bannerOf(html).matchAll(
      /<a class="r" href="([^"]+)"><span class="k">([^<]*)<\/span><span class="v">[^]*?<\/span><span class="st"><span class="pill (\w+)">([^<]*)<\/span>/g,
    ),
  ].map((m) => m.slice(1))
  assert.deepEqual(rows, [
    ['#a-logic', 'Database', 'success', 'additive'],
    ['#a-logic', 'Security', 'warning', 'risky'],
  ])
  assert.ok(!bannerOf(html).includes('<span class="k">Logic</span>'), 'the generic row is replaced')
})

test('a describer risk shows with its guard, and one outside the diff is dropped and logged', () => {
  const risk = {
    level: 'high',
    title: 'Anyone can run billing',
    impact: 'An open POST moves invoices.',
    guard: 'None',
    at: { path: 'src/cart.ts', line: 2 },
  }
  const html = renderProofHtml(
    described({
      risks: [risk, { ...risk, title: 'Elsewhere', at: { path: 'src/other.ts', line: 1 } }],
    }),
    { files },
  )
  const risks = section(html, 'risks')
  assert.ok(risks.includes('<div class="q">Anyone can run billing</div>'))
  assert.ok(risks.includes('<p class="p">An open POST moves invoices.</p>'))
  assert.ok(risks.includes('<div class="guard"><b>Guarded by</b><span>None</span></div>'))
  assert.ok(!outsideDebug(html).includes('Elsewhere'))
  assert.ok(
    debugOf(html).includes(
      '<li>describe logic risks.1: at.path: src/other.ts is not changed by this PR</li>',
    ),
  )
})

test('an old-shape input with no described facts renders no block markup and the generic rows', () => {
  const generatedAt = '2026-01-01T00:00:00.000Z'
  const old = renderProofHtml(input(), { files, generatedAt })
  assert.equal(old, renderProofHtml({ ...input(), described: {} }, { files, generatedAt }))
  for (const mark of [
    'class="schema"',
    'class="tbox"',
    'class="routes"',
    'class="calls"',
    'class="code excerpt"',
  ])
    assert.ok(
      !outsideDebug(old)
        .replace(/<style>[\s\S]*?<\/style>/, '')
        .includes(mark),
      mark,
    )
  assert.ok(bannerOf(old).includes('<span class="k">Logic</span>'))
})

test('the sidecar refuses described facts for an unknown area or with an unknown block kind', () => {
  assert.throws(
    () => renderProofHtml(described({ blocks: [{ kind: 'chart' }] })),
    /described\.logic\.blocks\.0\.kind: unknown block kind "chart"/,
  )
  assert.throws(
    () => renderProofHtml({ ...input(), described: { nowhere: {} } }),
    /described\.nowhere: not a proof area/,
  )
  assert.throws(
    () => renderProofHtml(described({ notes: [] })),
    /described\.logic\.notes: unknown key/,
  )
})

test('a clip that proves only files outside the change is dropped, and debug says so', () => {
  const clips = media()
  clips.clips[1] = { ...clips.clips[1], files: ['src/elsewhere.ts'] }
  clips.clips[2] = { ...clips.clips[2], files: ['src/elsewhere.ts', 'src/cart.ts'] }
  const html = render({ media: clips, files })
  assert.ok(!html.includes('data-title="Basket"'), 'the clip of other files is gone')
  assert.ok(html.includes('data-title="Checkout"'), 'a clip that proves one changed file stays')
  assert.ok(debugOf(html).includes('<li>media clips.head-0: proves none of the changed files</li>'))
})

test('each Files row links to its file in the GitHub changes view, by the SHA-256 of its path', () => {
  const page = { ...input(), filesUrl: 'https://github.com/acme/shop/pull/7/files' }
  const grid = section(renderProofHtml(page, { files }), 'files')
  assert.ok(
    grid.includes(
      '<a class="nm" href="https://github.com/acme/shop/pull/7/changes#diff-3e111233cdeb1e3f31646b9088b8e9f55eba99c0cf0a167026d8503c5408c24b" target="_blank" rel="noopener noreferrer" title="src/cart.ts"><bdi>src/cart.ts</bdi></a>',
    ),
  )
  const other = { ...input(), filesUrl: 'https://forge.example/acme/shop/merge/7' }
  assert.ok(
    section(renderProofHtml(other, { files }), 'files').includes(
      '<a class="nm" href="https://forge.example/acme/shop/merge/7" target="_blank"',
    ),
    'another forge links its files view, without an anchor',
  )
  assert.ok(
    section(renderProofHtml(input(), { files }), 'files').includes(
      '<span class="nm" title="src/cart.ts"><bdi>src/cart.ts</bdi></span>',
    ),
    'no files link, no link',
  )
})

test('an area nobody reviews still shows the blocks a describer typed for it', () => {
  const page = { ...input(), described: { ui: { blocks: [SCHEMA] } } }
  const html = renderProofHtml(page, { files })
  assert.ok(section(html, 'a-ui').includes('<div class="schema">'))
  assert.ok(!sectionIds(renderProofHtml(input(), { files })).includes('a-ui'))
})

test('an argument list after a space still reads as the field it belongs to', () => {
  const call = { ...CALL, operation: 'query Q { invoice (id: 1) { id } }', before: [], after: [] }
  const area = section(renderProofHtml(described({ blocks: [call] }), { files }), 'a-logic')
  assert.ok(area.includes('<span class="kw">query</span> Q {\n  invoice(id: 1) {\n    id\n  }\n}'))
})

/** A spec with the author's reason, linked risk and decision, and ticket fit for `src/cart.ts`. */
const authored = (over = {}) =>
  spec({
    reason: 'One rounding change, and a test pins the new total.',
    risks: [
      {
        id: 'R1',
        level: 'medium',
        title: 'A saved basket reprices by a cent',
        impact: 'Shoppers with saved baskets.',
        guard: 'None.',
        at: { path: 'src/cart.ts', line: 2 },
        links: ['D1', 'AC1', 'review:aaa', 'D9'],
      },
    ],
    decisions: [decisionAt('Is a one-cent change acceptable?', { links: ['R1'] })],
    ticketFit: {
      summary: 'Rounding is tested; the display is not.',
      verdict: 'complete',
      criteria: [
        {
          id: 'AC1',
          text: 'Totals round once',
          status: 'done',
          evidence: [{ kind: 'test', label: 'rounds once', ref: 'src/cart.test.ts:3' }],
        },
        {
          id: 'AC2',
          text: 'The total shows beside the button',
          status: 'done',
          evidence: [{ kind: 'ci', label: 'e2e', ref: 'e2e' }],
        },
      ],
      skipped: [{ text: 'Discounts', why: 'Out of scope', followUp: 'SHOP-9' }],
      contradictions: ['The body says totals round twice'],
    },
    areas: [],
    notes: [],
    ...over,
  })
const authorExtras = (over = {}) => ({
  spec: authored(),
  context,
  files,
  hasLine: () => true,
  checks: ['unit'],
  ...over,
})

test('the author reason leads the banner, but a blocked page keeps its own', () => {
  const html = renderProofHtml(withReview({ findings: [finding()] }), authorExtras())
  assert.ok(
    bannerOf(html).includes(
      '<p class="reason">One rounding change, and a test pins the new total.</p>',
    ),
  )
  const blocked = input(['logic'])
  blocked.proof.confidence.level = 'blocked'
  blocked.proof.review.blockers = [{ title: 'Data loss', where: 'src/cart.ts:9' }]
  blocked.proof = withDigest(blocked.proof)
  const page = renderProofHtml(blocked, authorExtras())
  assert.ok(bannerOf(page).includes('Review left one blocker to resolve before merging.'))
  const vocab = renderProofHtml(
    withReview({ findings: [] }),
    authorExtras({ spec: authored({ reason: 'The judge parked it.' }) }),
  )
  assert.ok(!bannerOf(vocab).includes('judge'), 'a reason in pipeline words is not shown')
})

test('an author decision quotes about ten lines around its line, with the suggested answer', () => {
  const html = renderProofHtml(withReview({ findings: [finding()] }), authorExtras())
  const decision = section(html, 'decide')
  assert.ok(
    decision.includes('<div class="q" id="decide-1-q">Is a one-cent change acceptable?</div>'),
  )
  assert.ok(decision.includes('<div class="guard"><b>Suggested</b><span>Yes.</span></div>'))
  const rows = [...decision.matchAll(/<tr class="(add|del|ctx)( pinned)?">/g)]
  assert.equal(rows.length, 7, 'every row of the six-line diff within five lines of line 2')
  assert.ok(decision.includes('<tr class="add pinned">'), 'its line is pinned')
  assert.ok(
    decision.includes(
      '<a class="lk" href="#risk-1" title="A saved basket reprices by a cent">R1</a>',
    ),
  )
  const dropped = renderProofHtml(
    withReview({ findings: [] }),
    authorExtras({ hasLine: (path, line) => line < 2 }),
  )
  assert.ok(!dropped.includes('id="decide"'), 'a decision off the diff is dropped')
})

test('author links join the risk to its decision, criterion and finding both ways, and a dangling id shows nothing', () => {
  const html = renderProofHtml(withReview({ findings: [finding()] }), authorExtras())
  const risk = item(section(html, 'risks'), 'risk-1')
  const chips = [...risk.matchAll(/<a class="lk" href="#([^"]+)"[^>]*>([^<]+)<\/a>/g)].map(
    (m) => `${m[2]}:${m[1]}`,
  )
  assert.deepEqual(chips.sort(), ['AC1:ac-ac1', 'D1:decide-1', 'F1:finding-1'])
  assert.ok(!risk.includes('D9'))
  assert.ok(
    item(section(html, 'findings'), 'finding-1').includes('href="#risk-1"'),
    'the finding links back',
  )
  assert.ok(
    section(html, 'ticket').includes(
      '<a class="lk" href="#risk-1" title="A saved basket reprices by a cent">R1</a>',
    ),
    'the criterion links back',
  )
})

test('author ticket fit: done needs a ref that resolves, and the verdict is worked out', () => {
  const html = renderProofHtml(
    {
      ...withReview({ findings: [finding()] }),
      ticket: { id: 'SHOP-4', url: 'https://linear.app/acme/issue/SHOP-4' },
      filesUrl: 'https://github.com/acme/shop/pull/7/files',
    },
    authorExtras(),
  )
  const ticket = section(html, 'ticket')
  assert.ok(
    ticket.startsWith(
      '<h2>Does it satisfy SHOP-4?<span class="n">2 · 1 not done</span><span class="pill warning">partial</span></h2>',
    ),
    'the author said complete; one criterion has no evidence that resolves',
  )
  assert.ok(ticket.includes('<p class="lead">Rounding is tested; the display is not.</p>'))
  const row = (id) => regionUntilNext(ticket, `<tr id="ac-${id}">`, '</tr>')
  assert.ok(row('ac1').includes('<span class="pill success">done</span>'))
  assert.match(
    row('ac1'),
    /<a class="evc" href="[^"]*" title="src\/cart.test.ts:3">test: rounds once<\/a>/,
  )
  assert.ok(row('ac2').includes('<span class="pill warning">unverified</span>'), 'e2e did not pass')
  assert.ok(!row('ac2').includes('e2e'), 'the dangling ref is not shown')
  assert.ok(ticket.includes('<h3 class="sub">Skipped</h3>'))
  assert.ok(ticket.includes('Discounts<span class="dim"> · Out of scope · follow-up SHOP-9</span>'))
  assert.ok(
    ticket.includes(
      '<h3 class="sub">Contradicts the PR</h3><ul class="facts"><li>The body says totals round twice</li>',
    ),
  )
  const passed = renderProofHtml(withReview({ findings: [] }), authorExtras({ checks: ['e2e'] }))
  assert.ok(section(passed, 'ticket').includes('<span class="pill success">complete</span>'))
  const none = renderProofHtml(
    withReview({ findings: [] }),
    authorExtras({
      spec: authored({ ticketFit: { ...authored().ticketFit, skipped: [], contradictions: [] } }),
    }),
  )
  assert.ok(!section(none, 'ticket').includes('<h3 class="sub">'), 'empty lists render nothing')
})

test('a describer risk the author restated at the same lines shows once, as the author wrote it', () => {
  const page = described({
    risks: [
      {
        level: 'high',
        title: 'Totals change',
        impact: 'Described impact.',
        at: { path: 'src/cart.ts', line: 3 },
      },
      {
        level: 'low',
        title: 'Elsewhere',
        impact: 'Another file.',
        at: { path: 'src/cart.test.ts', line: 1 },
      },
    ],
  })
  const html = renderProofHtml(page, authorExtras())
  const risks = section(html, 'risks')
  assert.ok(!risks.includes('Described impact.'), 'the author risk two lines away replaces it')
  assert.ok(risks.includes('A saved basket reprices by a cent'))
  assert.ok(risks.includes('Elsewhere'), 'a describer risk the author did not state stays')
})

test('without a spec the page falls back to the record: computed reason, no author sections', () => {
  const html = renderProofHtml(input(), { files, context })
  assert.ok(bannerOf(html).includes('<p class="reason">It changes logic, which a person reviews'))
  assert.ok(!html.includes('id="decide"') && !html.includes('id="ticket"'))
})

test('the footer shows the head, base and proof with the build time the player formats', () => {
  const html = renderProofHtml(input(), { generatedAt: '2026-11-12T23:59:00.000Z' })
  const foot = region(html, '<footer class="foot">', '</footer>')
  assert.ok(
    foot.includes(
      `Head <code>${HEAD.slice(0, 12)}</code> on <code>${BASE.slice(0, 12)}</code>, proof <code>sha256:`,
    ),
  )
  assert.ok(
    foot.includes(
      ' · Generated <time datetime="2026-11-12T23:59:00.000Z">2026-11-12T23:59:00.000Z</time>',
    ),
  )
  assert.ok(precedes(html, 'id="debug"', '<footer class="foot">'), 'below everything')
  const now = renderProofHtml(input())
  const at = /<time datetime="([^"]+)">/.exec(now)?.[1] ?? ''
  assert.ok(Math.abs(Date.parse(at) - Date.now()) < 60_000, 'it defaults to the build time')
})

test('review rows the author did not restate still show beside the author criteria', () => {
  const page = withReview({
    findings: [],
    acceptance: [
      { id: 'AC1', text: 'Totals round once', result: 'met', check: 'unit', head: HEAD },
      { id: 'AC3', text: 'Discounts round too', result: 'unmet', head: HEAD },
    ],
  })
  const ticket = section(renderProofHtml(page, authorExtras()), 'ticket')
  assert.equal(ticket.split('<tr id="ac-ac1">').length, 2, 'AC1 shows once, as the author wrote it')
  assert.ok(regionUntilNext(ticket, '<tr id="ac-ac3">', '</tr>').includes('pill danger">missed'))
  assert.match(
    ticket,
    /^<h2>[^]*?3 · 2 not done[^]*?<span class="pill warning">partial<\/span><\/h2>/,
  )
  const empty = authored({ ticketFit: { criteria: [], skipped: [], contradictions: [] } })
  const fallback = section(renderProofHtml(page, authorExtras({ spec: empty })), 'ticket')
  assert.ok(fallback.includes('<tr id="ac-ac3">'), 'no author criteria leaves review rows')
})

/** The two sections a repository declares, and a spec that fills them. */
const DECLARED = [
  { id: 'infra', title: 'Infrastructure', after: 'a-logic' },
  { id: 'testing', title: 'How to test it', after: 'risks' },
  { id: 'unfilled', title: 'Never shown' },
]
const sectionSpecFor = (risks) =>
  spec({
    risks,
    sections: [
      {
        id: 'infra',
        sub: 'one step',
        parts: [
          {
            kind: 'checklist',
            items: [
              {
                id: 'I1',
                group: 'Deploy',
                text: 'Run `./migrate`.',
                tag: 'production',
                links: ['R1'],
              },
              { id: 'I2', group: 'Deploy', text: 'Redeploy.' },
              { id: 'I3', group: 'After', text: 'Check the pod.', tag: 'both', tone: 'info' },
            ],
          },
          {
            kind: 'table',
            title: 'Settings',
            columns: ['Setting', 'Production'],
            rows: [
              {
                cells: [
                  { text: 'A\nB', code: true, note: 'Two names' },
                  { text: 'set', pill: 'success' },
                ],
              },
              { cells: ['C', { text: 'empty', pill: 'neutral' }] },
            ],
          },
          { kind: 'facts', rows: [{ label: 'Staging', text: 'No number.', tone: 'warning' }] },
        ],
      },
      {
        id: 'testing',
        parts: [
          {
            kind: 'checklist',
            numbered: true,
            items: [
              { id: 'T1', context: 'Admin', text: 'Send a text.', expect: 'It arrives.' },
              { id: 'T2', text: 'Send another.' },
            ],
          },
        ],
      },
    ],
  })
const RISK_TO_SECTION = [
  {
    id: 'R1',
    level: 'medium',
    title: 'Texts wait',
    impact: 'Until the number is set.',
    links: ['T1'],
  },
]

test('a declared section sits after the section it names, with a nav entry, count and anchor', () => {
  const html = render({
    spec: sectionSpecFor(RISK_TO_SECTION),
    snippet: resolver,
    files,
    sections: DECLARED,
  })
  const ids = sectionIds(html)
  assert.equal(ids.indexOf('sec-testing'), ids.indexOf('risks') + 1, 'testing follows Risks')
  assert.equal(ids.indexOf('sec-infra'), ids.indexOf('a-logic') + 1, 'infra follows its area')
  assert.ok(!ids.includes('sec-unfilled'), 'a section the spec leaves out is not shown')
  assert.deepEqual(
    navIds(html),
    ids.filter((id) => id !== 'debug'),
  )
  assert.ok(
    html.includes('<a href="#sec-infra" class="hot">Infrastructure<span class="n">3</span></a>'),
    'the nav counts checklist items',
  )
  assert.ok(
    html.includes(
      '<section class="sec" id="sec-infra"><h2>Infrastructure<span class="n">3 · one step</span></h2>',
    ),
  )
})

test('an unknown or absent anchor puts a section just before Files, and an unshown area after the areas', () => {
  const html = render({
    spec: sectionSpecFor([]),
    snippet: resolver,
    files,
    sections: [
      { id: 'infra', title: 'Infrastructure', after: 'a-database' },
      { id: 'testing', title: 'How to test it' },
    ],
  })
  const ids = sectionIds(html)
  const lastArea = ids.findLastIndex((id) => id.startsWith('a-'))
  assert.equal(ids[lastArea + 1], 'sec-infra')
  assert.ok(ids.includes('files'), 'the page has Files')
  assert.equal(ids[ids.indexOf('files') - 1], 'sec-testing', 'testing sits just before Files')
})

test('section items carry their own anchors and tick keys, and a risk links to one and back', () => {
  const html = render({
    spec: sectionSpecFor(RISK_TO_SECTION),
    snippet: resolver,
    files,
    sections: DECLARED,
  })
  const testing = section(html, 'sec-testing')
  assert.ok(
    testing.includes('<div class="si tick rich" id="sec-testing-t1" data-tick="sec-testing-t1">'),
  )
  assert.ok(testing.includes('<span class="no">1</span>'), 'a numbered list numbers its items')
  assert.ok(
    testing.includes(
      '<span class="cx">Admin</span><span class="it" id="sec-testing-t1-t">Send a text.</span><span class="ex"><b>Expect</b><span>It arrives.</span></span>',
    ),
  )
  const risk = section(html, 'risks')
  assert.ok(
    risk.includes('<a class="lk" href="#sec-testing-t1" title="Send a text.">T1</a>'),
    'the risk links to the step',
  )
  assert.ok(
    testing.includes('<a class="lk" href="#risk-1" title="Texts wait">R1</a>'),
    'the step links back',
  )
  const ticks = [...html.matchAll(/data-tick="([^"]+)"/g)].map((m) => m[1])
  assert.equal(new Set(ticks).size, ticks.length, 'every tick key is distinct')
  assert.ok(ticks.includes('sec-infra-i1') && ticks.includes('sec-testing-t2'))
})

test('a checklist shows its group once per run, and tables and facts keep their tones', () => {
  const html = render({ spec: sectionSpecFor([]), snippet: resolver, files, sections: DECLARED })
  const infra = section(html, 'sec-infra')
  assert.deepEqual(
    [...infra.matchAll(/<span class="g">([^<]*)<\/span>/g)].map((m) => m[1]),
    ['Deploy', '', 'After'],
  )
  assert.ok(
    infra.includes('<div class="si tick first" id="sec-infra-i3"'),
    'a new group rules above',
  )
  assert.ok(infra.includes('<span class="st"><span class="pill muted">production</span></span>'))
  assert.ok(infra.includes('<span class="pill info">both</span>'))
  assert.ok(
    infra.includes('<th>Setting</th><th class="pc">Production</th>'),
    'a pill column aligns right',
  )
  assert.ok(
    infra.includes(
      '<td><span class="mono">A</span><span class="mono">B</span><div class="sn">Two names</div></td>',
    ),
  )
  assert.ok(infra.includes('<td class="pc"><span class="pill muted">empty</span></td>'))
  assert.ok(
    infra.includes('<li><span class="k warning">Staging</span><span>No number.</span></li>'),
  )
})

test('a page with no declared sections shows none, and a spec section with no declaration is dropped', () => {
  const html = render({ spec: sectionSpecFor([]), snippet: resolver, files })
  assert.ok(!sectionIds(html).some((id) => id.startsWith('sec-')))
  assert.ok(
    debugOf(html).includes(
      '<li>spec sections[0]: id: infra is not a section the repository declares</li>',
    ),
  )
})
