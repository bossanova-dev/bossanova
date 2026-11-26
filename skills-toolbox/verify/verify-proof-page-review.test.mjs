import test from 'node:test'
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { withDigest } from './verify-proof.mjs'
import { renderProofHtml, validatePageInput } from './verify-proof-page.mjs'
import { cutSnippet, parseUnifiedDiff } from './verify-proof-snippet.mjs'
import { precedes, region } from '../../scripts/test-infra/gate-region-lib.mjs'

const HEAD = '3'.repeat(40)
const BASE = '4'.repeat(40)
const PR = 'https://github.com/acme/shop/pull/9'
/** @param {string} path @param {number} [line] */
const anchor = (path, line) =>
  `${PR}/changes#diff-${createHash('sha256').update(path).digest('hex')}${line ? `R${line}` : ''}`

/** @param {Record<string, unknown>} [extra] */
function input(extra = {}) {
  return {
    proof: withDigest({
      schema: 'boss-proof/1',
      repo: 'acme/shop',
      pr: 9,
      head: HEAD,
      base: BASE,
      confidence: {
        level: 'moderate',
        asks: ['ui'],
        loweredBy: [{ code: 'gap', text: 'The basket needs a person' }],
      },
      whatItDoes: { source: 'pr-body', bullets: ['Rounds the total once'] },
      areas: [{ name: 'ui', label: 'UI', ask: true, items: ['ui fact'], media: [], proven: false }],
      review: { models: [{ agent: 'reviewer', outcome: 'ran' }], blockers: [], notes: [] },
      health: [],
      page: null,
    }),
    filesUrl: `${PR}/files`,
    afterMerge: ['Tell support about the cent'],
    ...extra,
  }
}

const DIFF = parseUnifiedDiff(
  '--- a/src/cart.ts\n+++ b/src/cart.ts\n@@ -1,5 +1,5 @@\n export function total(items) {\n-  return sum(items)\n+  return round(sum(items))\n }\n // a\n // b\n',
)[0]
const extras = (/** @type {unknown} */ spec) => ({
  spec,
  files: [
    { path: 'src/cart.ts', adds: 1, dels: 1 },
    { path: 'src/cart.test.ts', adds: 12, dels: 0 },
  ],
  hasLine: () => true,
  snippet: (/** @type {any} */ request) =>
    cutSnippet(request.path === 'src/cart.ts' ? DIFF : undefined, request),
})

/** @param {Record<string, unknown>} [extra] */
function spec(extra = {}) {
  return {
    schema: 'proof-spec/1',
    head: HEAD,
    title: 'Round the basket total once',
    risks: [
      {
        id: 'R1',
        level: 'medium',
        title: 'Old baskets reprice',
        impact: 'Saved baskets in `src/cart.ts:2` reprice; `src/other.ts` is untouched.',
        at: { path: 'src/cart.ts', line: 2 },
        links: ['G1'],
      },
    ],
    areas: [
      {
        id: 'ui',
        name: 'UI',
        summary: 'The total shows rounded.',
        facts: [],
        snippets: [{ path: 'src/cart.ts', start: 1, end: 5, pinLine: 2 }],
      },
    ],
    ...extra,
  }
}

const REVIEW = {
  lead: {
    kind: 'bug-fix',
    text: 'Bug fix. Totals rounded twice, so a basket could be a cent out.',
  },
  signals: [
    { key: 'checks', summary: 'All 4 checks passed', pill: 'proven' },
    { key: 'ui', summary: 'The total shows once, rounded', pill: 'safe' },
    { key: 'revert', summary: 'Reverting restores double rounding', pill: 'safe' },
    { key: 'tests', summary: 'A new test pins the three-item total', pill: 'reported' },
  ],
  bugFix: {
    rows: [
      { label: 'symptom', text: 'Totals were a cent out', source: 'reported' },
      { label: 'before', text: 'The new test failed before the fix', source: 'reported' },
      { label: 'after', text: '`unit` passed on this head', source: 'ci' },
      { label: 'not-checked', text: 'Saved baskets' },
    ],
  },
  testing: {
    summary: 'The test pins the rounding.',
    proves: ['A three-item basket totals once'],
    gaps: ['Saved baskets'],
    also: ['One test now ignores the tax line'],
  },
  guardrails: [
    {
      id: 'G1',
      kind: 'lint',
      title: 'Float money slips through',
      body: 'Totals used floats.',
      proposal: 'Turn on the money lint.',
      evidence: 'Two rounding fixes this month.',
      links: ['R1'],
    },
  ],
  refactoring: [{ id: 'RF1', title: 'One rounding helper', body: 'Two copies.', links: [] }],
  noise: [{ id: 'N1', title: 'A huge comment', body: 'It cost a scroll.', proposal: 'Fold it.' }],
}

/** @param {string} html @param {string} id */
const section = (html, id) => {
  const found = new RegExp(`<section class="sec" id="${id}">([\\s\\S]*?)</section>`).exec(html)
  assert.ok(found, `the page has a #${id} section`)
  return found[1]
}
const sectionIds = (/** @type {string} */ html) =>
  [...html.matchAll(/<section class="sec[^"]*" id="([^"]+)"/g)].map((m) => m[1])
const navIds = (/** @type {string} */ html) =>
  [...region(html, '<nav class="secnav"', '</nav>').matchAll(/href="#([^"]+)"/g)].map((m) => m[1])

test('the lead opens with its kind, once, under the title', () => {
  const html = renderProofHtml(input(), extras(spec({ lead: REVIEW.lead })))
  const header = region(html, '<header class="pr">', '</header>')
  assert.match(
    header,
    /<p class="lead">Bug fix\. Totals rounded twice, so a basket could be a cent out\.<\/p>/,
  )
  precedes(header, '<h1>', '<p class="lead">')
})

test('with no spec the page titles itself from the PR and leads with its first paragraph', () => {
  const body = `<!-- template -->\n## Summary\n\nRounds the **basket** total once, in [the cart](https://x.test) code.\n\nSecond paragraph.`
  const html = renderProofHtml(
    input({ pullRequest: { title: 'Round the basket total once', body } }),
    extras(undefined),
  )
  const header = region(html, '<header class="pr">', '</header>')
  assert.match(header, /<h1>Round the basket total once<\/h1>/)
  assert.match(header, /<p class="lead">Rounds the basket total once, in the cart code\.<\/p>/)
  const long = renderProofHtml(
    input({ pullRequest: { title: 't'.repeat(130), body: 'word '.repeat(200) } }),
    extras(undefined),
  )
  const h1 = /<h1>([^<]*)<\/h1>/.exec(long)?.[1] ?? ''
  assert.equal(h1.length, 110)
  assert.ok(h1.endsWith('…'))
  const lead = /<p class="lead">([^<]*)<\/p>/.exec(region(long, '<header', '</header>'))?.[1] ?? ''
  assert.ok(lead.length <= 400 && lead.endsWith('…'), lead)
  const empty = renderProofHtml(input({ pullRequest: { title: 'Round' } }), extras(undefined))
  assert.ok(!region(empty, '<header', '</header>').includes('class="lead"'))
})

test('a PR body over the page limit is refused as page input', () => {
  const ok = validatePageInput(input({ pullRequest: { title: 'Round', body: 'x'.repeat(8000) } }))
  assert.equal(ok.ok, true)
  const long = validatePageInput(input({ pullRequest: { title: 'Round', body: 'x'.repeat(8001) } }))
  assert.equal(long.ok, false)
})

test('author rows replace the summary of the row with their key and keep the more severe pill', () => {
  const html = renderProofHtml(input(), extras(spec(REVIEW)))
  const rows = [
    ...region(html, '<div class="sig">', '</section>').matchAll(
      /<(?:a class="r" href="([^"]*)"|div class="r")><span class="k">([^<]*)<\/span><span class="v">([^<]*)<\/span><span class="st"><span class="pill ([a-z]+)">([^<]*)<\/span>/g,
    ),
  ].map(([, href, label, summary, tone, word]) => ({ href, label, summary, tone, word }))
  assert.deepEqual(rows, [
    {
      href: '#a-ui',
      label: 'UI',
      summary: 'The total shows once, rounded',
      tone: 'warning',
      word: 'to review',
    },
    {
      href: '#after',
      label: 'Revert',
      summary: 'Reverting restores double rounding',
      tone: 'success',
      word: 'safe',
    },
    {
      href: '#tests',
      label: 'Tests',
      summary: 'A new test pins the three-item total',
      tone: 'info',
      word: 'reported',
    },
    {
      href: '#tests',
      label: 'Checks',
      summary: 'All 4 checks passed',
      tone: 'success',
      word: 'proven',
    },
  ])
})

test('a more severe author pill wins over the area row it replaces', () => {
  const html = renderProofHtml(
    input(),
    extras(spec({ signals: [{ key: 'ui', summary: 'The total breaks', pill: 'breaking' }] })),
  )
  assert.match(
    region(html, '<div class="sig">', '</section>'),
    /<span class="k">UI<\/span><span class="v">The total breaks<\/span><span class="st"><span class="pill danger">breaking<\/span>/,
  )
})

test('the bug fix, tests and review sections render in page order with nav counts', () => {
  const html = renderProofHtml(input(), extras(spec(REVIEW)))
  assert.deepEqual(sectionIds(html), [
    'risks',
    'fix',
    'a-ui',
    'tests',
    'after',
    'guardrails',
    'refactoring',
    'noise',
    'files',
    'debug',
  ])
  assert.deepEqual(navIds(html), sectionIds(html).slice(0, -1))
  const nav = region(html, '<nav class="secnav"', '</nav>')
  assert.match(nav, /href="#guardrails">Guardrails<span class="n">1<\/span>/)
  assert.match(nav, /href="#noise">Noise<span class="n">1<\/span>/)
  assert.match(nav, /href="#fix">Bug fix<\/a>/)

  const fix = section(html, 'fix')
  assert.match(fix, /<h2>Bug fix<span class="n">from the author&#39;s report and CI<\/span>/)
  assert.deepEqual(
    [...fix.matchAll(/<span class="(k[^"]*)">([^<]*)<\/span>/g)].map((m) => `${m[1]}:${m[2]}`),
    ['k:Symptom', 'k info:Before the fix', 'k success:After the fix', 'k warning:Not checked'],
  )

  const tests = section(html, 'tests')
  assert.match(
    tests,
    /^<h2>Tests and checks<\/h2><p class="lead">The test pins the rounding\.<\/p>/,
  )
  assert.match(tests, /<h3 class="sub">Proves<\/h3><ul class="facts"><li>A three-item basket/)
  assert.match(
    tests,
    /<h3 class="sub">Doesn&#39;t prove<\/h3><ul class="facts"><li class="flag">Saved baskets/,
  )
  assert.match(tests, /<h3 class="sub">Also<\/h3><ul class="facts"><li>One test now ignores/)

  const guard = section(html, 'guardrails')
  assert.match(guard, /<h2>Guardrails to add<span class="n">1<\/span><\/h2>/)
  assert.match(
    guard,
    /<div class="item" id="g1"><div class="hd"><span class="pill muted">lint<\/span><div class="q">Float money slips through<\/div><span class="links"><a class="lk" href="#risk-1"[^>]*>R1<\/a><\/span><\/div><p class="p">Totals used floats\.<\/p><div class="guard"><b>Proposal<\/b><span>Turn on the money lint\.<\/span><\/div><p class="note">Two rounding fixes this month\.<\/p><\/div>/,
  )
  assert.match(section(html, 'risks'), /<a class="lk" href="#g1"[^>]*>G1<\/a>/)
  assert.ok(!section(html, 'refactoring').includes('class="pill'), 'refactoring has no pill')
  assert.match(section(html, 'noise'), /<h2>Review noise<span class="n">1<\/span><\/h2>/)
})

test('a page without the review keys shows none of their sections or nav entries', () => {
  const html = renderProofHtml(input(), extras(spec()))
  for (const id of ['fix', 'tests', 'guardrails', 'refactoring', 'noise']) {
    assert.ok(!sectionIds(html).includes(id), id)
    assert.ok(!navIds(html).includes(id), id)
  }
  assert.ok(!region(html, '<header', '</header>').includes('class="lead"'))
})

test('locations, snippet paths and backticked changed files open the PR diff in a new tab', () => {
  const html = renderProofHtml(input(), extras(spec()))
  const risks = section(html, 'risks')
  assert.ok(
    risks.includes(
      `<a class="locl" href="${anchor('src/cart.ts', 2)}" target="_blank" rel="noopener noreferrer"><code class="loc"`,
    ),
    'the risk location links its head line',
  )
  assert.ok(
    risks.includes(
      `<a class="locl" href="${anchor('src/cart.ts', 2)}" target="_blank" rel="noopener noreferrer" title="src/cart.ts:2"><code>src/cart.ts:2</code></a>`,
    ),
    'a backticked path and line in prose links',
  )
  assert.ok(risks.includes('<code>src/other.ts</code>'), 'an unchanged file stays plain')
  assert.ok(!risks.includes('<a class="locl" href="' + anchor('src/other.ts')))
  assert.ok(
    section(html, 'a-ui').includes(
      `<figcaption><a class="path" href="${anchor('src/cart.ts', 2)}" target="_blank" rel="noopener noreferrer"`,
    ),
    'the snippet path opens its pinned line',
  )
  assert.equal((html.match(/<a\b[^>]*><a\b/g) ?? []).length, 0, 'no link nests in another')
})
