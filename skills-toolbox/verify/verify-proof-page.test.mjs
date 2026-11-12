import test from 'node:test'
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { mkdtempSync, writeFileSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { spawnSync } from 'node:child_process'
import { withDigest, LEVELS, MAX_AREAS, MAX_TEXT_CHARS } from './verify-proof.mjs'
import { region, regionUntilNext } from '../../scripts/test-infra/gate-region-lib.mjs'
import { LEVELS as PAGE_LEVELS } from './verify-proof-page-model.mjs'
import {
  validatePageInput,
  proofView,
  renderProofHtml,
  renderProofComment,
  LEVEL_GUIDANCE,
  MAX_EXCERPT_LINES,
  MAX_LINE_CHARS,
  MAX_TIMELINE,
  MAX_AFTER_MERGE,
  MAX_COMMENT_BLOCK_CHARS,
} from './verify-proof-page.mjs'

function fixture(count = 1) {
  return {
    proof: withDigest({
      schema: 'boss-proof/1',
      repo: 'owner/repo',
      pr: 1,
      head: 'a'.repeat(40),
      base: 'b'.repeat(40),
      confidence: {
        level: 'moderate',
        asks: Array.from({ length: count }, (_, i) => `area-${i}`),
        loweredBy: [{ code: 'gap', text: 'Needs inspection' }],
      },
      whatItDoes: { source: 'issue', bullets: ['Useful change'] },
      areas: Array.from({ length: count }, (_, i) => ({
        name: `area-${i}`,
        label: `Area ${i}`,
        ask: true,
        items: ['Inspect result'],
        undisclosed: false,
        media: [],
        proven: false,
      })),
      review: {
        models: [{ agent: 'reviewer', outcome: 'ran' }],
        blockers: [],
        notes: ['Reviewed change'],
      },
      health: [],
      page: 'https://proof.example/proof.html',
    }),
    asks: { 'area-0': { diff: 'first\nsecond', plan: 'Plan excerpt' } },
    timeline: [{ at: '2026-01-01T00:00:00Z', text: 'Initial change' }],
    afterMerge: ['Observe rollout'],
    filesUrl: 'https://code.example/files',
  }
}
/** The body of the page section with this id; a page without one fails the test. */
const pageSection = (html, id) => {
  const found = new RegExp(`<section class="sec[^"]*" id="${id}"[^>]*>([\\s\\S]*?)</section>`).exec(
    html,
  )
  assert.ok(found, `the page has a #${id} section`)
  return found[1]
}
const sectionIds = (html) =>
  [...html.matchAll(/<section class="sec[^"]*" id="([^"]+)"/g)].map((m) => m[1])
const bannerOf = (html) => region(html, '<section class="banner', '</section>')
const outsideDebug = (html) => html.replace(/<section class="sec debug"[\s\S]*?<\/section>/, '')

function resign(input) {
  input.proof = withDigest(input.proof)
  return input
}

test('ordered view and outputs preserve facts and optional sections', () => {
  const input = fixture()
  const view = proofView(input)
  assert.deepEqual(
    view.sections.map((s) => s.title),
    [
      'Confidence',
      'What it does',
      'Why not high',
      'Asks',
      'Checked for you',
      'Timeline',
      'After merge',
    ],
  )
  const comment = renderProofComment(input)
  let prior = -1
  for (const section of view.sections) {
    const next = comment.indexOf(section.title, prior + 1)
    assert.ok(next > prior, section.title)
    prior = next
  }
  const html = renderProofHtml(input)
  assert.deepEqual(sectionIds(html), ['a-area-0', 'findings', 'after', 'debug'])
  const facts = [
    'Inspect result',
    'first',
    'second',
    'Plan excerpt',
    'Reviewed change',
    'Observe rollout',
  ]
  for (const fact of ['Useful change', 'Needs inspection', ...facts, 'Initial change'])
    assert.ok(comment.includes(fact), fact)
  for (const fact of facts) assert.ok(html.includes(fact), fact)
  assert.ok(region(html, '<p class="reason">', '</p>').includes('needs inspection'))
  for (const removed of ['Useful change', 'Initial change'])
    assert.ok(!html.includes(removed), `the page has no What it does list or timeline: ${removed}`)
  assert.ok(comment.includes('reviewer: ran'))
  assert.ok(
    !outsideDebug(html).includes('reviewer'),
    'a reviewer that ran is not a note on the page',
  )
  assert.ok(pageSection(html, 'debug').includes('<li>reviewer: ran</li>'))
  delete input.timeline
  delete input.afterMerge
  const bare = renderProofHtml(input)
  assert.deepEqual(sectionIds(bare), ['a-area-0', 'findings', 'debug'], 'no After merge section')
  assert.ok(!bare.includes('Nothing.'))
})
const EXPECTED_LEVELS = {
  high: { word: 'High', tone: 'success' },
  moderate: { word: 'Moderate', tone: 'info' },
  low: { word: 'Low', tone: 'warning' },
  blocked: { word: 'Blocked', tone: 'danger' },
  'cannot-assess': { word: 'Not rated', tone: 'muted' },
}
test('every manifest level supplies guidance without changing confidence', () => {
  for (const level of LEVELS) {
    const input = fixture(level === 'high' ? 0 : 1)
    input.proof.confidence.level = level
    if (level === 'high') {
      input.proof.confidence.loweredBy = []
      input.asks = {}
    }
    if (level === 'blocked') input.proof.review.blockers = [{ title: 'Blocker', where: 'file:1' }]
    if (level === 'cannot-assess') input.proof.review = null
    resign(input)
    assert.ok(LEVEL_GUIDANCE[level])
    assert.equal(proofView(input).level, level)
    const banner = bannerOf(renderProofHtml(input))
    const shown = PAGE_LEVELS[level === 'cannot-assess' ? 'unrated' : level]
    assert.deepEqual(shown, EXPECTED_LEVELS[level], level)
    assert.ok(banner.startsWith(`<section class="banner lv-${shown.tone}"`), level)
    assert.ok(
      banner.includes(
        `<div class="lv"><b>${shown.word}</b>${level === 'cannot-assess' ? '' : '<span>confidence</span>'}</div>`,
      ),
      level,
    )
    for (const guidance of Object.values(LEVEL_GUIDANCE))
      assert.ok(!banner.includes(guidance), `no pipeline guidance in the banner: ${guidance}`)
    input.rates = { [level]: { fixed: 7, total: 287 } }
    assert.ok(renderProofComment(input).includes('2.4%'))
    delete input.rates
    assert.ok(!renderProofComment(input).includes('later fixed:'))
  }
})
test('not-computed confidence has no level guidance, rate or placeholder reason', () => {
  const input = fixture()
  input.proof.confidence = {
    level: 'cannot-assess',
    asks: input.proof.confidence.asks,
    loweredBy: [{ code: 'confidence-not-computed', text: 'Confidence is not computed yet.' }],
  }
  input.proof.review = null
  input.rates = { 'cannot-assess': { fixed: 7, total: 287 } }
  resign(input)
  assert.equal(proofView(input).computed, false)
  assert.equal(proofView(input).level, 'cannot-assess')
  assert.ok(
    renderProofComment(input).startsWith(
      '<details>\n<summary>Confidence: not computed yet</summary>',
    ),
  )
  assert.ok(renderProofComment(input).includes('Confidence: not computed yet'))
  const banner = bannerOf(renderProofHtml(input))
  assert.ok(banner.startsWith('<section class="banner lv-muted"'))
  assert.ok(banner.includes('<div class="lv"><b>Not rated</b></div>'))
  for (const render of [renderProofHtml, renderProofComment]) {
    const output = render(input)
    for (const absent of [
      'Cannot assess',
      'later fixed:',
      'Why not high',
      'Confidence is not computed yet.',
    ])
      assert.ok(!output.includes(absent), absent)
  }
})

test('not-computed confidence preserves other reasons and review blockers', () => {
  const input = fixture()
  input.proof.confidence.level = 'blocked'
  input.proof.confidence.loweredBy.push({
    code: 'confidence-not-computed',
    text: 'Confidence is not computed yet.',
  })
  input.proof.review.blockers = [{ title: 'Fix the failure', where: 'file:1' }]
  resign(input)
  const comment = renderProofComment(input)
  assert.ok(comment.includes('Why not high'))
  assert.ok(comment.includes('Fix the failure (file:1)'))
  const html = renderProofHtml(input)
  assert.ok(
    pageSection(html, 'findings').includes(
      '<span class="pill danger">blocks merge</span></span><div class="q">Fix the failure</div><code class="loc" title="file:1">file:1</code>',
    ),
    'the blocker still shows when confidence was not worked out',
  )
  assert.ok(comment.includes('Needs inspection'))
  for (const output of [html, comment])
    assert.ok(!output.includes('Confidence is not computed yet.'))
})

test('computed cannot-assess retains its guidance and rate', () => {
  const input = fixture()
  input.proof.confidence.level = 'cannot-assess'
  input.rates = { 'cannot-assess': { fixed: 7, total: 287 } }
  resign(input)
  assert.equal(proofView(input).computed, true)
  const comment = renderProofComment(input)
  assert.ok(comment.includes('Confidence: Cannot assess. Gather evidence before deciding.'))
  assert.ok(comment.includes('later fixed: 2.4%'))
  assert.ok(comment.includes('Needs inspection'))
  const banner = bannerOf(renderProofHtml(input))
  assert.ok(banner.includes('<b>Not rated</b>'))
  assert.ok(region(banner, '<p class="reason">', '</p>').includes('needs inspection'))
  assert.ok(!banner.includes('Gather evidence'))
})

test('excerpt caps, width and split suggestion', () => {
  const input = fixture(3)
  input.asks['area-0'].diff = Array.from({ length: MAX_EXCERPT_LINES }, (_, i) => `line ${i}`).join(
    '\n',
  )
  assert.ok(!renderProofComment(input).includes('split suggested'))
  input.asks['area-0'].diff += '\nhidden-line'
  const comment = renderProofComment(input)
  assert.ok(comment.includes('1 more lines'))
  assert.ok(comment.includes(input.filesUrl))
  assert.ok(comment.includes('split suggested'))
  const area = pageSection(renderProofHtml(input), 'a-area-0')
  assert.ok(
    area.includes(`<div class="more">1 more line in <a href="${input.filesUrl}">Files</a></div>`),
  )
  for (const out of [comment, area]) assert.ok(!out.includes('hidden-line'))
  delete input.filesUrl
  assert.ok(!renderProofComment(input).includes('code.example'))
  input.asks['area-0'].diff = 'x'.repeat(MAX_LINE_CHARS + 2)
  assert.ok(renderProofHtml(input).includes('x'.repeat(MAX_LINE_CHARS - 1) + '…'))
  assert.ok(renderProofComment(fixture(4)).includes('split suggested'))
})
test('CSP hashes the only stylesheet and only own-origin media inline', () => {
  const input = fixture()
  input.proof.areas[0].media = [
    {
      kind: 'video',
      url: 'https://proof.example/movie.mp4',
      poster: 'https://else.example/poster.png',
      durationMs: 1000,
    },
    { kind: 'image', url: 'https://proof.example/image.png' },
    { kind: 'image', url: 'https://else.example/image.png' },
  ]
  resign(input)
  const html = renderProofHtml(input)
  const style = html.match(/<style>([\s\S]*?)<\/style>/)[1]
  const hash = createHash('sha256').update(style).digest('base64')
  assert.ok(
    html.includes(
      `<head><meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'sha256-${hash}'; script-src 'self'; img-src 'self'; media-src 'self'; base-uri 'none'; form-action 'none'">`,
    ),
  )
  assert.deepEqual(
    html.match(/<script[^>]*>/g),
    ['<script src="player.js" defer>'],
    'the only script is the static player, on every page',
  )
  assert.ok(!/<link|<iframe|<object|\son\w+\s*=|\sstyle=|@import|url\(/i.test(html))
  assert.ok(html.includes('<video controls preload="none"'))
  assert.ok(html.includes('<img'))
  for (const match of html.matchAll(/(?:src|poster)="([^"]+)"/g))
    assert.equal(
      new URL(match[1], 'https://proof.example/proof.html').origin,
      'https://proof.example',
    )
  input.proof.page = null
  resign(input)
  assert.ok(!/<video|<img/.test(renderProofHtml(input)))
})
test('hostile text cannot inject markup, links, markers or close excerpt fences', () => {
  const input = fixture()
  const attack =
    '<script></style>"\'><img onerror=x> [x](https://evil.example) <!-- boss-evidence: {} -->\n```'
  input.proof.areas[0].items = [attack]
  input.asks['area-0'].diff = attack
  input.filesUrl = 'https://code.example/a)b'
  resign(input)
  const html = renderProofHtml(input).replace('<script src="player.js" defer></script>', '')
  assert.ok(!/<script|<img|<\/style>"/.test(html))
  assert.ok(html.includes('&lt;script&gt;'))
  const comment = renderProofComment(input)
  assert.ok(!comment.replace(/````diff[\s\S]*?````/g, '').includes('[x](https://evil.example)'))
  assert.ok(!/^<!-- boss-/m.test(comment))
  assert.ok(comment.includes('````diff'))
  assert.ok(comment.includes('&lt;script&gt;'))
  input.asks['area-0'].diff = 'x\n'.repeat(31)
  assert.ok(renderProofComment(input).includes('a%29b'))
})
test('fenced excerpts preserve source characters while neutralizing evidence markers', () => {
  const input = fixture()
  input.asks['area-0'].diff = 'if (count < limit && value > 0)\n<!-- boss-evidence: {} -->\n```'
  const comment = renderProofComment(input)
  assert.ok(comment.includes('if (count < limit && value > 0)'))
  assert.ok(!comment.includes('<!-- boss-evidence:'))
  assert.ok(comment.includes('````diff'))
})
test('strict sidecars reject malformed values in validation and renderers', () => {
  const changes = [
    (i) => (i.proof.digest = 'bad'),
    (i) => (i.filesUrl = 'http://unsafe.example'),
    (i) => (i.asks.other = {}),
    (i) => (i.unknown = true),
    (i) => (i.timeline = Array(MAX_TIMELINE + 1).fill({ at: '2026-01-01T00:00:00Z', text: 'x' })),
    (i) => (i.afterMerge = Array(MAX_AFTER_MERGE + 1).fill('x')),
    (i) => (i.rates = { moderate: { fixed: 2, total: 1 } }),
    (i) => (i.timeline = [{ at: 'yesterday', text: 'x' }]),
  ]
  for (const change of changes) {
    const input = fixture()
    change(input)
    assert.equal(validatePageInput(input).ok, false)
    for (const render of [proofView, renderProofHtml, renderProofComment])
      assert.throws(() => render(input))
  }
})
test('comment size collapses excerpts from last ask', () => {
  const input = fixture(MAX_AREAS)
  input.asks = Object.fromEntries(
    input.proof.confidence.asks.map((name) => [
      name,
      { diff: Array(MAX_EXCERPT_LINES).fill('x'.repeat(MAX_LINE_CHARS)).join('\n') },
    ]),
  )
  const output = renderProofComment(input)
  assert.ok(output.length <= MAX_COMMENT_BLOCK_CHARS)
  assert.ok(output.includes('30 lines'))
  assert.ok(output.includes('```diff'))
})
test('CLI writes both outputs and rejects invalid input and flags', () => {
  const dir = mkdtempSync(join(tmpdir(), 'proof-page-'))
  try {
    const input = fixture()
    const proof = join(dir, 'proof.json')
    const sidecar = join(dir, 'input.json')
    const html = join(dir, 'proof.html')
    const comment = join(dir, 'comment.md')
    writeFileSync(proof, JSON.stringify(input.proof))
    const { proof: _, ...rest } = input
    writeFileSync(sidecar, JSON.stringify(rest))
    const run = (args) =>
      spawnSync(process.execPath, ['skills-toolbox/verify/verify-proof-page.mjs', ...args], {
        encoding: 'utf8',
      })
    assert.equal(
      run(['render', '--proof', proof, '--input', sidecar, '--html', html, '--comment', comment])
        .status,
      0,
    )
    // Each render stamps its own build time.
    const untimed = (/** @type {string} */ page) =>
      page.replace(/<time datetime="[^"]+">[^<]+<\/time>/, '<time>')
    assert.equal(untimed(readFileSync(html, 'utf8')), untimed(renderProofHtml(input)))
    assert.equal(readFileSync(comment, 'utf8'), renderProofComment(input))
    assert.equal(run(['render', '--unknown']).status, 2)
    writeFileSync(sidecar, '{"filesUrl":"http://bad.example"}')
    assert.equal(run(['render', '--proof', proof, '--input', sidecar, '--html', html]).status, 2)
  } finally {
    rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
})

test('view text reaches both outputs including checked areas and escaped facts', () => {
  const input = fixture()
  input.proof.areas.push({
    name: 'checked',
    label: 'Checked label',
    ask: false,
    items: ['Checked fact < & >'],
    undisclosed: false,
    media: [],
    proven: true,
  })
  input.proof.review.models[0].agent = 'Reviewer _one_'
  input.proof.whatItDoes.bullets = ['A *literal* [fact] <tag> & value']
  resign(input)
  const view = proofView(input)
  const decode = (text) =>
    text
      .replace(/\\([\\`*_\[\]|])/g, '$1')
      .replace(/&lt;/g, '<')
      .replace(/&gt;/g, '>')
      .replace(/&quot;/g, '"')
      .replace(/&#39;/g, "'")
      .replace(/&amp;/g, '&')
  const html = renderProofHtml(input)
  const page = decode(html)
  for (const removed of ['Checked fact', 'Checked label', 'Checked: proven', 'Initial change'])
    assert.ok(!page.includes(removed), removed)
  for (const [output, sections] of [
    [page, ['Asks']],
    [decode(renderProofComment(input)), view.sections.map((s) => s.title)],
  ]) {
    for (const section of view.sections.filter((s) => sections.includes(s.title))) {
      for (const bullet of section.bullets) assert.ok(output.includes(bullet), bullet)
      for (const ask of section.asks || []) {
        assert.ok(output.includes(ask.label))
        for (const item of ask.items) assert.ok(output.includes(item))
        for (const excerpt of ask.excerpts)
          for (const line of excerpt.lines) assert.ok(output.includes(line))
      }
    }
  }
})

test('unsafe sidecar shapes fail without coercion and the CLI rejects invalid proofs', () => {
  for (const extra of [
    { asks: [] },
    { asks: { 'area-0': { diff: 4 } } },
    { rates: { unknown: { fixed: 0, total: 1 } } },
    { afterMerge: ['x'.repeat(301)] },
    { timeline: [{ at: '2026-01-01T00:00:00Z', text: 'ok', extra: true }] },
    { filesUrl: 'https://user:password@proof.example/' },
    { timeline: [{ at: '2026-02-31T00:00:00Z', text: 'Impossible date' }] },
  ]) {
    assert.equal(validatePageInput({ ...fixture(), ...extra }).ok, false)
  }
  const dir = mkdtempSync(join(tmpdir(), 'proof-page-invalid-'))
  try {
    const file = join(dir, 'proof.json')
    writeFileSync(file, '{}')
    const result = spawnSync(
      process.execPath,
      [
        'skills-toolbox/verify/verify-proof-page.mjs',
        'render',
        '--proof',
        file,
        '--html',
        join(dir, 'out.html'),
      ],
      { encoding: 'utf8' },
    )
    assert.equal(result.status, 2)
    assert.ok(result.stderr.includes('proof'))
  } finally {
    rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
})

test('intent view exposes known bullets and source in both escaped outputs', () => {
  const input = fixture()
  input.proof.whatItDoes = {
    source: 'issue',
    ref: '<issue> & [ref]',
    bullets: ['Shows <script> & *literal*', 'Displays the result'],
  }
  resign(input)
  const section = proofView(input).sections.find((entry) => entry.title === 'What it does')
  assert.equal(section.state, 'known')
  assert.deepEqual(section.bullets, input.proof.whatItDoes.bullets)
  assert.ok(section.source.includes('issue'))
  assert.ok(section.source.includes(input.proof.whatItDoes.ref))
  const comment = renderProofComment(input)
  assert.ok(comment.includes('Displays the result'))
  assert.ok(comment.includes('&lt;script&gt;'))
  assert.ok(comment.includes('&lt;issue&gt;'))
  const html = renderProofHtml(input)
  assert.ok(!html.includes('Displays the result'), 'the page has no What it does list')
  for (const output of [html, comment]) {
    assert.ok(!output.includes('<script>'))
    assert.ok(!output.includes('<issue>'))
  }
})

test('unknown intent view and both outputs agree on the fallback', () => {
  const input = fixture()
  input.proof.whatItDoes = { source: 'unknown', bullets: [] }
  resign(input)
  const section = proofView(input).sections.find((entry) => entry.title === 'What it does')
  assert.equal(section.state, 'unknown')
  assert.equal(section.source, undefined)
  assert.equal(section.bullets.length, 1)
  assert.ok(renderProofComment(input).includes(section.bullets[0]))
  const html = renderProofHtml(input)
  assert.ok(!html.includes('intent unknown'), 'the page shows nothing for an unknown intent')
  assert.ok(!html.includes('class="does"'))
})

test('optional park verdict opens the page and comment, preserving confidence', () => {
  const input = {
    ...fixture(),
    verdict: { codes: ['always-human-path'], label: 'the verify judge (cannot grade itself)' },
  }
  const sentence =
    'Not verified · waiting for you · this PR changes the verify judge (cannot grade itself)'
  assert.equal(validatePageInput(input).ok, true)
  assert.equal(proofView(input).verdict, sentence)
  assert.ok(renderProofComment(input).startsWith(`<details>\n<summary>${sentence}</summary>`))
  const page = renderProofHtml(input)
  assert.ok(page.includes('<h1>owner/repo #1</h1>'), 'with no spec title the heading is the PR')
  assert.ok(
    region(page, '<p class="reason">', '</p>').includes(
      'the verify judge (cannot grade itself), which a person reviews',
    ),
    'the judge names what a person reviews',
  )
  assert.ok(pageSection(page, 'debug').includes(`Gate: ${sentence}.`))
  assert.ok(!outsideDebug(page).includes('Not verified'), 'the gate state shows only in debug')
  assert.ok(renderProofComment(input).includes('Confidence: Moderate'))
  assert.ok(bannerOf(page).includes('<b>Moderate</b>'))
  input.verdict.label = '<!-- forged --> @ops <script>'
  assert.ok(!renderProofHtml(input).includes('<script>'))
  assert.ok(!renderProofComment(input).includes('<!-- forged'))
  assert.equal(proofView(fixture()).verdict, null)
  assert.equal(validatePageInput({ ...fixture(), verdict: { codes: ['unknown-code'] } }).ok, true)
})

test('park verdict sidecars reject invalid codes, labels and unknown keys', () => {
  for (const verdict of [
    null,
    [],
    {},
    { codes: [] },
    { codes: Array(33).fill('x') },
    { codes: [1] },
    { codes: [''] },
    { codes: ['x'.repeat(MAX_TEXT_CHARS + 1)] },
    { codes: ['x'], label: '' },
    { codes: ['x'], label: 7 },
    { codes: ['x'], label: 'x'.repeat(MAX_TEXT_CHARS + 1) },
    { codes: ['x'], sentence: 'caller supplied' },
  ]) {
    const input = { ...fixture(), verdict }
    assert.equal(validatePageInput(input).ok, false, JSON.stringify(verdict))
    assert.throws(() => proofView(input))
  }
})

// One bullet of each claim status and one "Not in the description" entry, all with hostile text.
function claimed() {
  const input = fixture()
  input.proof.whatItDoes = {
    source: 'pr-body',
    bullets: ['Shows <b>balance</b>', 'Sends the invoice', 'Keeps the export'],
    claims: [
      { proof: 'test', behaviour: true, test: 'shows <balance>', path: 'src/a_test.go' },
      { proof: 'clip', behaviour: true, chapter: 'Sends *the* invoice', atMs: 65_400 },
      { proof: 'unproven', behaviour: true, note: 'no test <named>' },
    ],
  }
  input.proof.confidence.loweredBy.push({
    code: 'unproven-claim',
    text: '1 behaviour claim(s) unproven.',
  })
  input.proof.areas[0].undisclosed = true
  input.proof.areas[0].unmentioned = ['Hospital.state <x>']
  return resign(input)
}

test('each bullet shows its claim status and each area its unmentioned changes in one view', () => {
  const input = claimed()
  const view = proofView(input)
  const what = view.sections.find((entry) => entry.title === 'What it does')
  assert.deepEqual(what.bullets, [
    'Shows <b>balance</b> (proven by test shows <balance> in src/a_test.go)',
    'Sends the invoice (proven by clip: Sends *the* invoice at 1:05)',
    'Keeps the export (unproven: no test <named>)',
  ])
  const missing = view.sections.find((entry) => entry.title === 'Not in the description')
  assert.deepEqual(missing.lists, [{ label: 'Area 0', items: ['Hospital.state <x>'] }])
  const html = renderProofHtml(input)
  const comment = renderProofComment(input)
  assert.ok(comment.includes('Not in the description'))
  assert.ok(comment.includes('proven by test shows &lt;balance&gt;'))
  assert.ok(comment.includes('Hospital.state &lt;x&gt;'))
  assert.ok(comment.includes('unproven: no test &lt;named&gt;'))
  assert.ok(comment.includes('proven by clip: Sends \\*the\\* invoice at 1:05)'))
  assert.ok(
    pageSection(html, 'risks').includes(
      '<div class="q">The PR description doesn&#39;t mention the area 0 change</div></div><ul class="facts"><li>Hospital.state &lt;x&gt;</li>',
    ),
  )
  const gaps = regionUntilNext(bannerOf(html), '<ul class="glist gaps">', '</ul>')
  assert.ok(gaps.includes('Keeps the export: no test &lt;named&gt;'), 'the unproven claim')
  assert.ok(!gaps.includes('balance') && !gaps.includes('invoice'), 'proven claims are not gaps')
  for (const output of [html, comment])
    assert.ok(
      !output.includes('<b>balance') && !output.includes('<x>') && !output.includes('<named>'),
    )
})

test('a bullet without a claim keeps its text and no unmentioned list means no section', () => {
  const input = fixture()
  const view = proofView(input)
  assert.deepEqual(view.sections.find((entry) => entry.title === 'What it does').bullets, [
    'Useful change',
  ])
  assert.ok(!view.sections.some((entry) => entry.title === 'Not in the description'))
  const unproven = claimed()
  unproven.proof.whatItDoes.claims[2] = { proof: 'unproven', behaviour: false }
  resign(unproven)
  assert.equal(
    proofView(unproven).sections.find((entry) => entry.title === 'What it does').bullets[2],
    'Keeps the export (unproven)',
  )
})

test('factory health shows on the page only behind ?debug and as a strip above the comment details', () => {
  const input = fixture()
  input.proof.health = [
    { code: 'second-model-down', text: 'Second model down' },
    { code: 'health-unreadable', text: 'Factory health unreadable: unimplemented' },
  ]
  resign(input)
  const strip = 'Second model down · Factory health unreadable: unimplemented'
  assert.equal(proofView(input).healthStrip, strip)
  const html = renderProofHtml(input)
  assert.ok(!outsideDebug(html).includes('Factory health'))
  assert.ok(pageSection(html, 'debug').includes(`<p class="dim">Factory health: ${strip}.</p>`))
  const comment = renderProofComment(input)
  assert.ok(comment.startsWith(`**Factory health:** ${strip}\n\n<details>\n`))
})

test('empty factory health leaves the view, page and comment unchanged', () => {
  const input = fixture()
  assert.equal(proofView(input).healthStrip, null)
  const html = renderProofHtml(input)
  const comment = renderProofComment(input)
  assert.ok(!html.includes('Factory health'))
  assert.ok(comment.startsWith('<details>\n'))
  assert.ok(!comment.includes('Factory health'))
})

test('hostile factory health text stays one escaped line', () => {
  const input = fixture()
  input.proof.health = [
    {
      code: 'hostile',
      text: '<script>x</script>\n[x](https://evil.example) <!-- boss-proof: {} -->',
    },
  ]
  resign(input)
  const html = renderProofHtml(input)
  assert.ok(!html.includes('<script>'))
  assert.ok(html.includes('&lt;script&gt;'))
  const comment = renderProofComment(input)
  const [first, blank, details] = comment.split('\n')
  assert.ok(first.startsWith('**Factory health:** '))
  assert.equal(blank, '')
  assert.equal(details, '<details>')
  assert.ok(!first.includes('<script>'))
  assert.ok(!first.includes('[x](https://evil.example)'))
  assert.ok(!/<!-- boss-/.test(first))
})

test('API change class leads asking and checked area facts in both outputs', () => {
  for (const ask of [true, false]) {
    for (const change of [undefined, 'additive', 'breaking']) {
      const input = fixture()
      Object.assign(input.proof.areas[0], {
        name: 'api-contract',
        label: 'API contract',
        ask,
        ...(change ? { change, changeReasons: ['field removed', 'type changed'] } : {}),
      })
      input.proof.confidence.asks = ask ? ['api-contract'] : []
      input.asks = {}
      input.proof = withDigest(input.proof)
      const view = proofView(input)
      const section = view.sections.find((s) => s.title === (ask ? 'Asks' : 'Checked for you'))
      const items = ask ? section.asks[0].items : section.bullets
      const expected =
        change === 'breaking'
          ? 'API change: breaking (field removed; type changed)'
          : 'API change: additive'
      if (change) assert.ok(items.indexOf(expected) < items.indexOf('Inspect result'))
      const comment = renderProofComment(input)
      assert.equal(comment.includes('API change:'), !!change)
      if (change) assert.ok(comment.includes(expected))
      const html = renderProofHtml(input)
      const shown = ask || change === 'breaking'
      assert.equal(
        sectionIds(html).includes('a-api-contract'),
        shown,
        'reviewed or breaking areas only',
      )
      if (shown) {
        const facts = pageSection(html, 'a-api-contract')
        assert.equal(facts.includes('API change:'), !!change)
        if (change)
          assert.ok(
            facts.indexOf('API change:') < facts.indexOf('Inspect result'),
            'the change class leads the facts',
          )
      }
    }
  }
})

test('review links require the displayed head and safe HTTPS destinations', () => {
  const input = fixture()
  const url = `https://review.example/review?head=${input.proof.head}`
  assert.equal(validatePageInput({ ...input, reviewUrl: url }).ok, true)
  for (const reviewUrl of [
    url.replace('https:', 'http:'),
    url.replace('review.example', 'user@review.example'),
    'https://review.example/',
    url.replace(input.proof.head, 'c'.repeat(40)),
  ]) {
    assert.equal(validatePageInput({ ...input, reviewUrl }).ok, false)
  }
})

test('review links render once with escaped destinations and leave absent output unchanged', () => {
  const input = fixture()
  const reviewUrl = `https://review.example/review?head=${input.proof.head}&note="'><tag>(x)`
  const html = renderProofHtml({ ...input, reviewUrl })
  const comment = renderProofComment({ ...input, reviewUrl })
  assert.equal((html.match(/Review this head/g) || []).length, 1)
  assert.equal((comment.match(/Review this head/g) || []).length, 1)
  assert.ok(html.includes('&amp;note=&quot;&#39;&gt;&lt;tag&gt;(x)'))
  assert.ok(comment.includes('%22%27%3E%3Ctag%3E%28x%29'))
  assert.ok(!html.includes('<tag>'))
  assert.ok(!comment.includes('<tag>'))
  for (const render of [renderProofHtml, renderProofComment]) {
    const linked = render({
      ...input,
      reviewUrl: `https://review.example/?head=${input.proof.head}`,
    })
    // Each page stamps its own build time.
    const untimed = (/** @type {string} */ out) => out.replace(/<time [^>]+>[^<]+<\/time>/, '')
    const absent = untimed(render(input))
    const stripped = untimed(linked)
      .replace(/<a href="[^"]+">Review this head<\/a>/, '')
      .replace(/\[Review this head\]\([^\n]+\)\n\n/, '')
    assert.equal(stripped, absent)
  }
})
