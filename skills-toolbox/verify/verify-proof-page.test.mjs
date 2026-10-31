import test from 'node:test'
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { mkdtempSync, writeFileSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { spawnSync } from 'node:child_process'
import { withDigest, LEVELS, MAX_AREAS } from './verify-proof.mjs'
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
  for (const render of [renderProofHtml, renderProofComment]) {
    const output = render(input)
    let prior = -1
    for (const section of view.sections) {
      const next = output.indexOf(section.title, prior + 1)
      assert.ok(next > prior)
      prior = next
    }
    for (const fact of [
      'Useful change',
      'Needs inspection',
      'Inspect result',
      'first',
      'second',
      'Plan excerpt',
      'reviewer',
      'Reviewed change',
      'Initial change',
      'Observe rollout',
    ])
      assert.ok(output.includes(fact), fact)
  }
  delete input.timeline
  delete input.afterMerge
  assert.ok(!renderProofHtml(input).includes('<h2>Timeline'))
})
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
    assert.equal(renderProofHtml(input).includes('<h2>Why not high'), level !== 'high')
    input.rates = { [level]: { fixed: 7, total: 287 } }
    assert.ok(renderProofComment(input).includes('2.4%'))
    delete input.rates
    assert.ok(!renderProofComment(input).includes('later fixed:'))
  }
})
test('excerpt caps, width and split suggestion', () => {
  const input = fixture(3)
  input.asks['area-0'].diff = Array.from({ length: MAX_EXCERPT_LINES }, (_, i) => `line ${i}`).join(
    '\n',
  )
  assert.ok(!renderProofHtml(input).includes('split suggested'))
  input.asks['area-0'].diff += '\nhidden-line'
  for (const render of [renderProofHtml, renderProofComment]) {
    const out = render(input)
    assert.ok(out.includes('1 more lines'))
    assert.ok(out.includes(input.filesUrl))
    assert.ok(!out.includes('hidden-line'))
    assert.ok(out.includes('split suggested'))
  }
  delete input.filesUrl
  assert.ok(!renderProofComment(input).includes('code.example'))
  input.asks['area-0'].diff = 'x'.repeat(MAX_LINE_CHARS + 2)
  assert.ok(renderProofHtml(input).includes('x'.repeat(MAX_LINE_CHARS - 1) + '…'))
  assert.ok(renderProofHtml(fixture(4)).includes('split suggested'))
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
      `<head><meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'sha256-${hash}'; img-src 'self'; media-src 'self'; base-uri 'none'; form-action 'none'">`,
    ),
  )
  assert.ok(!/<script|<link|<iframe|<object|\son\w+\s*=|@import|url\(/i.test(html))
  assert.ok(html.includes('<video controls preload="none"'))
  assert.ok(html.includes('<img'))
  for (const match of html.matchAll(/(?:src|poster)="([^"]+)"/g))
    assert.equal(new URL(match[1]).origin, 'https://proof.example')
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
  const html = renderProofHtml(input)
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
    assert.equal(readFileSync(html, 'utf8'), renderProofHtml(input))
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
  for (const output of [renderProofHtml(input), renderProofComment(input)].map(decode)) {
    for (const section of view.sections) {
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
  for (const output of [renderProofHtml(input), renderProofComment(input)]) {
    assert.ok(output.includes('Displays the result'))
    assert.ok(output.includes('&lt;script&gt;'))
    assert.ok(output.includes('&lt;issue&gt;'))
    assert.ok(output.includes('issue'))
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
  for (const output of [renderProofHtml(input), renderProofComment(input)])
    assert.ok(output.includes(section.bullets[0]))
})
