import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import { mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  FIXTURES_DIR,
  buildFixtureRepo,
  fixtureNames,
  renderPr,
  synthesizeJudge,
} from './verify-proof-render-pr.mjs'
import { proofPlayerScript } from './verify-proof-page.mjs'
import { region } from '../../scripts/test-infra/gate-region-lib.mjs'

const CLI = 'skills-toolbox/verify/verify-proof-page.mjs'
/** Copy and markup the banner design removed; none of it may come back outside #debug. */
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
  'Not verified',
  'class="lightbox"',
  'class="does"',
  '<dl class="status">',
  '<ul class="timeline">',
  'Inspect the asks before deciding',
  'Gather evidence',
]
const AREA_ORDER = ['a-migrations', 'a-api-contract', 'a-security', 'a-ui']
const KIND_GROUPS = ['No sign-off needed', 'Tests', 'Generated']

/** Every fixture's page, rendered once through the CLI. */
const pages = new Map()
/** @param {{after: (fn: () => void) => void}} t @param {string} name */
function fixturePage(t, name) {
  if (!pages.has(name)) {
    const out = join(scratch(t), 'page')
    const result = run(['--fixture', name, '--out', out])
    assert.equal(result.status, 0, `${name}: ${result.stderr}`)
    pages.set(name, {
      html: readFileSync(join(out, 'index.html'), 'utf8'),
      player: readFileSync(join(out, 'player.js'), 'utf8'),
    })
  }
  return pages.get(name)
}
const sectionIds = (html) =>
  [...html.matchAll(/<section class="sec[^"]*" id="([^"]+)"/g)].map((m) => m[1])
const section = (html, id) => {
  const found = new RegExp(`<section class="sec[^"]*" id="${id}"[^>]*>([\\s\\S]*?)</section>`).exec(
    html,
  )
  assert.ok(found, `the page has a #${id} section`)
  return found[1]
}
const outsideDebug = (html) => html.replace(/<section class="sec debug"[\s\S]*?<\/section>/, '')

/** @param {{after: (fn: () => void) => void}} t */
function scratch(t) {
  const dir = mkdtempSync(join(tmpdir(), 'render-pr-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  return dir
}

/** @param {string[]} args */
const run = (args) => spawnSync(process.execPath, [CLI, 'render-pr', ...args], { encoding: 'utf8' })

/** @param {string} dir */
const tree = (dir) =>
  Object.fromEntries(
    readdirSync(dir, { recursive: true, withFileTypes: true })
      .filter((entry) => entry.isFile())
      .map((entry) => {
        const path = join(entry.parentPath, entry.name)
        return [path.slice(dir.length + 1), readFileSync(path).toString('base64')]
      }),
  )

test('every committed fixture renders a validated page that names its head', (t) => {
  assert.deepEqual(fixtureNames(), ['api', 'dbt', 'infra', 'mobile', 'review'])
  for (const name of fixtureNames()) {
    const { html, player } = fixturePage(t, name)
    const spec = JSON.parse(readFileSync(join(FIXTURES_DIR, name, 'spec.json'), 'utf8'))
    assert.ok(html.includes(spec.head.slice(0, 7)), `${name}: page names head ${spec.head}`)
    const debug = section(html, 'debug')
    assert.ok(!debug.includes('<h3>Dropped</h3>'), `${name}: spec and media validate against git`)
    assert.ok(debug.includes('agent spec'), `${name}: the spec was read`)
    assert.ok(html.includes(`<h1>${spec.title}</h1>`), `${name}: the spec titles the page`)
    assert.equal(player, proofPlayerScript())
  }
})

test('no fixture page shows a placeholder or pipeline line the banner design removed', (t) => {
  for (const name of fixtureNames()) {
    const page = outsideDebug(fixturePage(t, name).html)
    for (const removed of REMOVED) assert.ok(!page.includes(removed), `${name}: ${removed}`)
  }
})

test('every fixture page keeps the CSP shape: one hashed sheet, one own-origin script, no inline code', (t) => {
  for (const name of fixtureNames()) {
    const { html } = fixturePage(t, name)
    const styles = [...html.matchAll(/<style>([\s\S]*?)<\/style>/g)]
    assert.equal(styles.length, 1, `${name}: one stylesheet`)
    const hash = createHash('sha256').update(styles[0][1]).digest('base64')
    const csp = /<meta http-equiv="Content-Security-Policy" content="([^"]+)">/.exec(html)?.[1]
    assert.equal(
      csp,
      `default-src 'none'; style-src 'sha256-${hash}'; script-src 'self'; img-src 'self'; media-src 'self'; base-uri 'none'; form-action 'none'`,
      name,
    )
    assert.deepEqual(html.match(/<script[^>]*>[^<]*<\/script>/g), [
      '<script src="player.js" defer></script>',
    ])
    assert.ok(
      !/<[a-z][^>]*\s(?:style|on\w+)=/i.test(html),
      `${name}: no style or handler attribute`,
    )
  }
})

test('every fixture page orders its sections and its Files grid the same way', (t) => {
  for (const name of fixtureNames()) {
    const { html } = fixturePage(t, name)
    const ids = sectionIds(html)
    assert.equal(ids.at(-1), 'debug', name)
    const rank = (id) =>
      ['risks', 'decide', 'ticket'].includes(id)
        ? ['risks', 'decide', 'ticket'].indexOf(id) / 3
        : id.startsWith('a-')
          ? 2 + (AREA_ORDER.includes(id) ? AREA_ORDER.indexOf(id) : 9) / 10
          : 3 + ['evidence', 'findings', 'after', 'files', 'debug'].indexOf(id)
    assert.deepEqual(
      ids,
      [...ids].sort((a, b) => rank(a) - rank(b)),
      `${name}: sections in order ${ids}`,
    )
    const reviewed = ids.filter((id) => id.startsWith('a-'))
    // Describer signals carry their own labels, so the area order is the sections' order.
    const titles = reviewed.map(
      (id) => new RegExp(`<section class="sec" id="${id}"><h2>([^<]*)`).exec(html)?.[1],
    )
    const groups = [
      ...section(html, 'files').matchAll(/<summary class="frow"><span class="nm">([^<]*)<\/span>/g),
    ].map((m) => m[1])
    const areaGroups = groups.filter((g) => !KIND_GROUPS.includes(g))
    assert.deepEqual(
      areaGroups,
      titles.filter((title) => areaGroups.includes(title)),
      `${name}: area groups lead, in section order`,
    )
    assert.deepEqual(
      groups.slice(areaGroups.length),
      KIND_GROUPS.filter((g) => groups.includes(g)),
      `${name}: then the kinds, in order`,
    )
    assert.ok(reviewed.length >= 1, `${name}: some area asks a person`)
    const bars = [
      ...section(html, 'files').matchAll(
        /<summary class="frow">[\s\S]*?<i class="a bw-(\d+)"><\/i><i class="d bw-(\d+)"><\/i>/g,
      ),
    ].map((m) => Number(m[1]) + Number(m[2]))
    assert.ok(
      Math.max(...bars) >= 99 && Math.max(...bars) <= 101,
      `${name}: the largest group fills the bar`,
    )
    for (const [, id] of region(html, '<section class="banner', '</section>').matchAll(
      /href="#(risk-\d+)"/g,
    ))
      assert.ok(
        section(html, 'risks').includes(`<div class="item" id="${id}">`),
        `${name}: ${id} resolves`,
      )
  }
})

test('review findings carry their code and status, and the criteria their checks', (t) => {
  const { html } = fixturePage(t, 'review')
  const findings = section(html, 'findings')
  const items = findings.split('<div class="item" id="').slice(1)
  const pills = (item) =>
    [...item.matchAll(/<span class="pill (\w+)">([^<]+)<\/span>/g)].map((m) => m[2])
  assert.deepEqual(
    items.map((item) => [...pills(item).slice(0, 2), item.includes('<figure class="code"')]),
    [
      ['P2', 'open', true],
      ['P3', 'note', true],
      ['P3', 'note', false],
      ['P1', 'fixed', true],
      ['P2', 'fixed', true],
    ],
    'each finding shows once: located ones with their code, the extension note as text',
  )
  assert.equal(findings.split('could live with the other auth constants').length, 2)
  assert.ok(items[0].includes('<tr class="add pinned"><td class="n"></td><td class="n">7</td>'))
  assert.ok(
    items[4].includes('<tr class="del pinned"><td class="n">4</td><td class="n"></td>'),
    'a finding on a deleted file quotes its base line',
  )
  const ticket = section(html, 'ticket')
  assert.ok(ticket.startsWith('<h2>Does it satisfy SHOP-12?'))
  assert.deepEqual(
    [...ticket.matchAll(/<span class="pill \w+">(\w+)<\/span><\/td><\/tr>/g)].map((m) => m[1]),
    ['done', 'done', 'unverified'],
  )
  assert.ok(
    items[0].includes('<a class="lk" href="#decide-1"'),
    'the open finding links its decision',
  )
})

test('the no-sign-off area says what changed there and never reads as an ask or a risk', (t) => {
  const { html } = fixturePage(t, 'dbt')
  const banner = /<section class="banner[\s\S]*?<\/section>/.exec(html)?.[0] ?? ''
  const row = /<a class="r" href="#a-no-sign-off">([\s\S]*?)<\/a>/.exec(banner)?.[1] ?? ''
  assert.ok(row.includes('<code>stg_refunds</code>'), 'the row names the model that changed')
  assert.ok(row.includes('<span class="pill muted">no sign-off</span>'), 'a muted pill, not an ask')
  assert.ok(!/No sign-off|no-sign-off/i.test(section(html, 'risks')), 'it raises no risk')
  assert.ok(!/no sign-off/i.test(/<p class="reason">[\s\S]*?<\/p>/.exec(banner)?.[0] ?? ''))
  assert.match(
    section(html, 'findings'),
    /<div class="q">Settled refunds only: [^]*?<\/div><code class="loc" title="analytics\/models\/staging\/stg_refunds\.sql">/,
    'the judge note on its path is a finding with that path',
  )
  const group =
    /<details class="fg"><summary class="frow"><span class="nm">No sign-off needed<\/span><span class="c2">(\d+)<\/span>/.exec(
      html,
    )
  assert.equal(group?.[1], '2', 'its files join the files that need no sign-off')
})

test('every banner row says what changed, and no generated line uses the pipeline vocabulary', (t) => {
  for (const name of fixtureNames()) {
    const { html } = fixturePage(t, name)
    const banner = /<section class="banner[\s\S]*?<\/section>/.exec(html)?.[0] ?? ''
    for (const [, summary] of banner.matchAll(
      /<span class="v">([\s\S]*?)<\/span><span class="st">/g,
    ))
      assert.ok(summary.replace(/<[^>]+>/g, '').trim(), `${name}: a signal row has a summary`)
    const prose = outsideDebug(html)
      .replace(/<style>[\s\S]*?<\/style>|<figure class="code"[\s\S]*?<\/figure>/g, '')
      .replace(/<[^>]+>/g, ' ')
    assert.doesNotMatch(prose, /undisclosed|\bjudge|\bverify\b|\basks?\b/i, name)
  }
  const dbt = fixturePage(t, 'dbt').html
  assert.ok(
    dbt.includes('<span>A behavior change that no test or recording shows</span>'),
    'an asking part with nothing to show joins Not proven',
  )
})

test('the mobile fixture stages every media file the page links', (t) => {
  const out = join(scratch(t), 'page')
  assert.equal(run(['--fixture', 'mobile', '--out', out]).status, 0)
  const html = readFileSync(join(out, 'index.html'), 'utf8')
  for (const file of ['check-in.mp4', 'visits-before.png', 'visits-after.png']) {
    assert.ok(html.includes(`media/${file}`), `page links ${file}`)
    assert.deepEqual(
      readFileSync(join(out, 'media', file)),
      readFileSync(join(FIXTURES_DIR, 'mobile', 'media', file)),
    )
  }
})

test('a second run into the same directory writes the same bytes and drops stale media', (t) => {
  const out = join(scratch(t), 'page')
  assert.equal(run(['--fixture', 'mobile', '--out', out]).status, 0)
  const first = tree(out)
  writeFileSync(join(out, 'media', 'stale.png'), 'old')
  assert.equal(run(['--fixture', 'mobile', '--out', out]).status, 0)
  assert.deepEqual(tree(out), first)
})

test('without a judge the page comes from the diff and the base tree review policy', (t) => {
  const dir = scratch(t)
  const range = buildFixtureRepo('dbt', join(dir, 'repo'))
  const judged = synthesizeJudge({ cwd: join(dir, 'repo'), base: range.base, head: range.head })
  assert.equal(judged.confidence.level, 'cannot-assess')
  assert.deepEqual(
    judged.confidence.asks.map((ask) => ask.area),
    ['config:revenue-models'],
  )
  const out = join(dir, 'page')
  renderPr({ cwd: join(dir, 'repo'), base: 'main~2', head: 'main', out })
  const html = readFileSync(join(out, 'index.html'), 'utf8')
  assert.ok(html.includes('Revenue models'))
  assert.ok(html.includes('<section class="banner lv-muted"'), 'a synthesized judge rates nothing')
  assert.ok(html.includes('<b>Not rated</b>'))
  assert.ok(html.includes('local/repo'))
})

test('a branch context puts the notice above the title', (t) => {
  const dir = scratch(t)
  const range = buildFixtureRepo('dbt', join(dir, 'repo'))
  const out = join(dir, 'page')
  const branch = { base: 'staging', default: 'main', stackedOn: [] }
  renderPr({ cwd: join(dir, 'repo'), base: range.base, head: range.head, out, branch })
  const html = readFileSync(join(out, 'index.html'), 'utf8')
  assert.match(html, /<div class="bnotice branch">.*Merges into <code>staging<\/code>/)
})

test('a bad judge, an unknown ref or a mixed fixture call fails naming the input', (t) => {
  const dir = scratch(t)
  const range = buildFixtureRepo('api', join(dir, 'repo'))
  const base = ['--repo', join(dir, 'repo'), '--base', range.base, '--head', range.head]
  const judge = join(dir, 'judge.json')
  writeFileSync(judge, '{"verdict": ')
  const malformed = run([...base, '--judge', judge, '--out', join(dir, 'out')])
  assert.equal(malformed.status, 2)
  assert.ok(malformed.stderr.includes(judge))
  writeFileSync(judge, JSON.stringify({ verdict: 'human', headSha: range.base }))
  const other = run([...base, '--judge', judge, '--out', join(dir, 'out')])
  assert.equal(other.status, 2)
  assert.ok(other.stderr.includes(judge))
  const missing = run([...base.slice(0, 4), '--head', 'no-such-ref', '--out', join(dir, 'out')])
  assert.equal(missing.status, 2)
  assert.ok(missing.stderr.includes('no-such-ref'))
  assert.equal(run(['--fixture', 'api', '--repo', '.', '--out', join(dir, 'out')]).status, 2)
  const unknown = run(['--fixture', 'nope', '--out', join(dir, 'out')])
  assert.equal(unknown.status, 2)
  assert.ok(unknown.stderr.includes('api, dbt, infra, mobile'))
})

test('the api and dbt fixtures carry describer output that renders every block kind', (t) => {
  const api = fixturePage(t, 'api').html
  const dbt = fixturePage(t, 'dbt').html
  const both = `${api}${dbt}`
  for (const mark of [
    'class="schema"',
    'class="calls"',
    'class="routes"',
    'class="tbox"',
    'class="code excerpt"',
    '<span class="pii">PII</span>',
  ])
    assert.ok(outsideDebug(both).includes(mark), mark)
  assert.ok(section(api, 'after').includes('Run <code>./migrate</code> in staging'))
  assert.ok(section(dbt, 'after').includes('<code>dbt build --select stg_refunds+</code>'))
  assert.ok(section(api, 'risks').includes('<b>Guarded by</b><span>None in this PR</span>'))
  const labels = [
    ...region(api, '<section class="banner', '</section>').matchAll(
      /<span class="k">([^<]*)<\/span><span class="v">/g,
    ),
  ].map((m) => m[1])
  assert.deepEqual(labels, ['Database', 'Manual steps', 'API', 'Security', 'Old clients'])
})

test('a describe file the gate would reject fails the render with its first error', (t) => {
  const dir = scratch(t)
  const range = buildFixtureRepo('api', join(dir, 'repo'))
  const describe = join(dir, 'describe.json')
  const result = {
    ok: true,
    extension: 'x',
    role: 'proof-describe',
    items: [],
    breaking: false,
    undisclosed: [],
    blocks: [{ kind: 'chart' }],
  }
  writeFileSync(describe, JSON.stringify({ 'api-contract': [result] }))
  const args = ['--repo', join(dir, 'repo'), '--base', range.base, '--head', range.head]
  const judge = join(FIXTURES_DIR, 'api', 'judge.json')
  const rejected = run([
    ...args,
    '--judge',
    judge,
    '--describe',
    describe,
    '--out',
    join(dir, 'out'),
  ])
  assert.equal(rejected.status, 2)
  assert.ok(
    rejected.stderr.includes(
      `${describe}: api-contract.0: blocks.0.kind: unknown block kind "chart"`,
    ),
    rejected.stderr,
  )
  writeFileSync(describe, JSON.stringify({ nowhere: [] }))
  const unknown = run([
    ...args,
    '--judge',
    judge,
    '--describe',
    describe,
    '--out',
    join(dir, 'out'),
  ])
  assert.ok(unknown.stderr.includes(`${describe}: nowhere is not a judged area`), unknown.stderr)
})
