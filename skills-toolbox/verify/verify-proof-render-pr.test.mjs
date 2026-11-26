import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  rmSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import {
  FIXTURES_DIR,
  buildFixtureRepo,
  fixtureNames,
  renderPr,
  synthesizeJudge,
} from './verify-proof-render-pr.mjs'
import { collectProofMedia } from './verify-media-hook.mjs'
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
const HEAD_SECTIONS = ['risks', 'decide', 'fix', 'ticket']
const TAIL_SECTIONS = [
  'evidence',
  'tests',
  'findings',
  'after',
  'guardrails',
  'refactoring',
  'noise',
  'files',
  'debug',
]
const KIND_GROUPS = ['Other changes', 'Tests', 'Generated']

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
  assert.deepEqual(fixtureNames(), [
    'api',
    'bugfix',
    'dbt',
    'infra',
    'mobile',
    'review',
    'sections',
  ])
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

test('the bugfix fixture shows every review section, the lead and the author banner rows', (t) => {
  const { html } = fixturePage(t, 'bugfix')
  const ids = sectionIds(html)
  for (const id of ['fix', 'tests', 'guardrails', 'refactoring', 'noise'])
    assert.ok(ids.includes(id), `#${id} renders`)
  assert.match(region(html, '<header class="pr">', '</header>'), /<p class="lead">Bug fix\. /)
  const banner = region(html, '<div class="sig">', '</section>')
  assert.deepEqual(
    [...banner.matchAll(/<span class="k">([^<]*)<\/span>/g)].map((m) => m[1]),
    ['UI', 'Revert', 'Bug fix', 'Tests', 'Checks'],
  )
  assert.match(section(html, 'guardrails'), /<span class="pill muted">lint<\/span>/)
  assert.match(
    section(html, 'risks'),
    /<a class="locl" href="https:\/\/github\.com\/acme\/shop\/pull\/61\/changes#diff-[0-9a-f]{64}R5" target="_blank"/,
  )
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
    // A repository's own sections go where it declares them; the core sections keep their order.
    const ids = sectionIds(html).filter((id) => !id.startsWith('sec-'))
    assert.equal(ids.at(-1), 'debug', name)
    const rank = (id) =>
      HEAD_SECTIONS.includes(id)
        ? HEAD_SECTIONS.indexOf(id) / HEAD_SECTIONS.length
        : id.startsWith('a-')
          ? 2 + (AREA_ORDER.includes(id) ? AREA_ORDER.indexOf(id) : 9) / 10
          : 3 + TAIL_SECTIONS.indexOf(id)
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
    /<details class="fg"><summary class="frow"><span class="nm">Other changes<\/span><span class="c2">(\d+)<\/span>/.exec(
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

test('with no spec the page is titled from the PR and leads with its first paragraph', (t) => {
  const dir = scratch(t)
  const range = buildFixtureRepo('dbt', join(dir, 'repo'))
  const out = join(dir, 'page')
  renderPr({
    cwd: join(dir, 'repo'),
    base: range.base,
    head: range.head,
    out,
    prTitle: 'Orders mart counts refunds once',
    prBody:
      '## Why\n\nRefunds were **counted twice** in `orders`, see [the report](https://x.test).\n\nSecond paragraph stays off the page.',
  })
  const html = readFileSync(join(out, 'index.html'), 'utf8')
  assert.match(html, /<h1>Orders mart counts refunds once<\/h1>/)
  assert.match(
    html,
    /<p class="lead">Refunds were counted twice in <code>orders<\/code>, see the report\.<\/p>/,
  )
  assert.doesNotMatch(html, /Second paragraph/)
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
  assert.ok(unknown.stderr.includes('api, bugfix, dbt, infra, mobile'))
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

test('an API change the PR description never names raises no risk or section about the description', (t) => {
  const describe = JSON.parse(readFileSync(join(FIXTURES_DIR, 'api', 'describe.json'), 'utf8'))
  const legacy = Object.values(describe)
    .flat()
    .flatMap((result) => result.undisclosed ?? [])
  assert.deepEqual(legacy, ['Adds InvoiceStatus.OVERDUE'], 'a deployed describer still sends one')
  const html = fixturePage(t, 'api').html
  assert.ok(section(html, 'a-api-contract'), 'the API contract area is on the page')
  assert.doesNotMatch(html, /PR description|Not in the description|doesn&#39;t mention/)
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

test('the sections fixture shows the base commit declared sections where it places them, filled from the spec', (t) => {
  const { html } = fixturePage(t, 'sections')
  const ids = sectionIds(html)
  assert.deepEqual(ids, [
    'risks',
    'ticket',
    'sec-testing',
    'a-migrations',
    'sec-infra',
    'files',
    'debug',
  ])
  const infra = section(html, 'sec-infra')
  for (const kind of ['<div class="cl grp">', '<table class="tbl stbl">', '<ul class="kv">'])
    assert.ok(infra.includes(kind), `infra renders ${kind}`)
  assert.ok(section(html, 'sec-testing').includes('<span class="no">3</span>'))
  assert.ok(
    section(html, 'risks').includes('<a class="lk" href="#sec-infra-i1" title="'),
    'a risk links to an infra step',
  )
})

/**
 * A two-commit repository whose head changes billing rates, and an extensions checkout beside it:
 * its config makes billing an always-review area, and it holds a describer for that area, a
 * section and, when `media` is given, a media script.
 * @param {{after: (fn: () => void) => void}} t @param {{media?: boolean}} [options]
 */
function extensionsPair(t, { media = false } = {}) {
  const dir = scratch(t)
  const repo = join(dir, 'repo')
  const ext = join(dir, 'ext')
  const git = (cwd, args) => {
    const out = spawnSync(
      'git',
      ['-c', 'commit.gpgsign=false', '-c', 'core.hooksPath=/dev/null', ...args],
      {
        cwd,
        encoding: 'utf8',
        env: { ...process.env, GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_NOSYSTEM: '1' },
      },
    )
    assert.equal(out.status, 0, out.stderr)
    return out.stdout.trim()
  }
  const write = (root, file, text) => {
    mkdirSync(dirname(join(root, file)), { recursive: true })
    writeFileSync(join(root, file), text)
  }
  const who = ['-c', 'user.name=Fixture', '-c', 'user.email=fixture@example.invalid']
  mkdirSync(repo)
  git(repo, ['init', '--quiet', '--initial-branch=main'])
  write(repo, '.boss-skills.json', '{}\n')
  write(repo, 'billing/rates.txt', 'basic 10\n')
  git(repo, ['add', '--all'])
  git(repo, [...who, 'commit', '--quiet', '-m', 'start'])
  write(repo, 'billing/rates.txt', 'basic 12\n')
  git(repo, ['add', '--all'])
  git(repo, [...who, 'commit', '--quiet', '-m', 'raise the basic rate'])
  mkdirSync(ext)
  git(ext, ['init', '--quiet', '--initial-branch=main'])
  write(
    ext,
    '.boss-skills.json',
    JSON.stringify({ verifyDefaults: { alwaysReview: { 'Billing rules': ['billing/**'] } } }),
  )
  const skill = (name, marker) =>
    write(
      ext,
      `.claude/skills/${name}/SKILL.md`,
      [
        '---',
        `name: ${name}`,
        'x-boss-extension:',
        '  extends: boss-verify',
        ...marker,
        '---',
        '',
        `# ${name}`,
        '',
      ].join('\n'),
    )
  skill('boss-verify-billing-facts', [
    '  role: proof-describe',
    '  describe: describe.mjs',
    '  areas: config:billing-rules',
  ])
  write(
    ext,
    '.claude/skills/boss-verify-billing-facts/describe.mjs',
    `process.stdout.write(${JSON.stringify(
      JSON.stringify({
        ok: true,
        role: 'proof-describe',
        extension: 'boss-verify-billing-facts',
        items: ['Raises the basic plan rate'],
        breaking: false,
        blocks: [
          {
            kind: 'table',
            name: 'plan_rates',
            relation: 'table',
            change: 'changed',
            columns: [{ name: 'basic', type: 'integer', change: 'changed' }],
          },
        ],
        signals: [
          { key: 'rates', label: 'Rates', state: 'review', summary: 'The basic rate rises' },
        ],
      }),
    )})\n`,
  )
  skill('boss-verify-billing-sections', [
    '  role: proof-section',
    '  sections:',
    '    - id: rollout',
    '      title: Rollout',
  ])
  if (media) {
    skill('boss-verify-billing-media', ['  role: proof-media', '  media: media.mjs'])
    write(
      ext,
      '.claude/skills/boss-verify-billing-media/media.mjs',
      [
        "import fs from 'node:fs'",
        "import path from 'node:path'",
        "const envelope = JSON.parse(fs.readFileSync(0, 'utf8'))",
        "for (const f of ['rates.mp4', 'rates.png']) fs.writeFileSync(path.join(envelope.evidenceDir, f), f)",
        'process.stdout.write(JSON.stringify({',
        "  schema: 'proof-media/1', head: envelope.headSha, transcripts: [],",
        "  clips: [{ id: 'head-0', side: 'head', title: 'The new rate', file: 'rates.mp4', durationMs: 2000,",
        "    chapters: [{ atMs: 0, caption: 'Start' }], stills: [{ file: 'rates.png', caption: 'The rate' }] }],",
        '}))',
        '',
      ].join('\n'),
    )
  }
  const head = git(repo, ['rev-parse', 'HEAD'])
  const spec = join(dir, 'spec.json')
  writeFileSync(
    spec,
    JSON.stringify({
      schema: 'proof-spec/1',
      head,
      sections: [
        {
          id: 'rollout',
          parts: [{ kind: 'checklist', items: [{ id: 'R1', text: 'Tell support the new rate.' }] }],
        },
      ],
    }),
  )
  return { dir, repo, ext, spec, base: 'main~1', head: 'main' }
}

/** @param {string} file */
const readInventoryFile = (file) => JSON.parse(readFileSync(file, 'utf8'))

test('--extensions reads the review policy, describers and sections from the checkout it names', (t) => {
  const pair = extensionsPair(t)
  const range = { cwd: pair.repo, base: pair.base, head: pair.head, spec: pair.spec }
  const withExt = join(pair.dir, 'with.json')
  renderPr({ ...range, out: join(pair.dir, 'with'), extensions: pair.ext, inventory: withExt })
  const after = readInventoryFile(withExt)
  assert.equal(after.schema, 'proof-inventory/1')
  assert.ok(after.items.includes('Raises the basic plan rate'), JSON.stringify(after))
  assert.deepEqual(after.blocks, [{ kind: 'table', title: 'plan_rates' }])
  assert.ok(after.signals.includes('Rates'))
  assert.ok(after.sections.some((id) => id.startsWith('a-') && id.includes('billing')))
  assert.ok(after.sections.includes('sec-rollout'))
  const html = readFileSync(join(pair.dir, 'with', 'index.html'), 'utf8')
  assert.ok(html.includes('Raises the basic plan rate'), 'the page shows what the inventory lists')
  assert.ok(html.includes('Tell support the new rate.'))

  const withoutExt = join(pair.dir, 'without.json')
  renderPr({ ...range, out: join(pair.dir, 'without'), inventory: withoutExt })
  const before = readInventoryFile(withoutExt)
  assert.ok(!before.items.includes('Raises the basic plan rate'))
  assert.deepEqual(before.blocks, [])
  assert.ok(!before.signals.includes('Rates'))
  assert.ok(!before.sections.some((id) => id.includes('billing') || id === 'sec-rollout'))
})

test('--extensions collects the checkout media when asked, and renders without clips when none is configured', (t) => {
  const pair = extensionsPair(t, { media: true })
  const collectMedia = (request) => collectProofMedia({ ...request, read: () => ({ ok: false }) })
  const range = {
    cwd: pair.repo,
    base: pair.base,
    head: pair.head,
    extensions: pair.ext,
    collectMedia,
  }
  const out = join(pair.dir, 'page')
  const inventory = join(pair.dir, 'inv.json')
  const rendered = renderPr({ ...range, out, inventory })
  assert.equal(rendered.mediaReason, undefined)
  assert.deepEqual(readInventoryFile(inventory).clips, ['media/rates.mp4', 'media/rates.png'])
  assert.deepEqual(readdirSync(join(out, 'media')).sort(), ['rates.mp4', 'rates.png'])

  rmSync(join(pair.ext, '.claude/skills/boss-verify-billing-media'), {
    recursive: true,
    maxRetries: 3,
  })
  const bare = renderPr({ ...range, out: join(pair.dir, 'bare'), inventory })
  assert.equal(bare.mediaReason, 'not-configured')
  assert.deepEqual(readInventoryFile(inventory).clips, [])
})

test('--extensions naming a missing directory, or given with --fixture, is a usage error that writes nothing', (t) => {
  const pair = extensionsPair(t)
  const out = join(pair.dir, 'out')
  const args = ['--repo', pair.repo, '--base', pair.base, '--head', pair.head, '--out', out]
  for (const ext of [join(pair.dir, 'nowhere'), join(pair.dir)]) {
    const failed = run([...args, '--extensions', ext])
    assert.equal(failed.status, 2)
    assert.ok(failed.stderr.includes(`--extensions ${ext}`), failed.stderr)
  }
  const mixed = run(['--fixture', 'api', '--extensions', pair.ext, '--out', out])
  assert.equal(mixed.status, 2)
  assert.ok(mixed.stderr.includes('--fixture takes only --out'))
  assert.equal(existsSync(out), false)
  const ok = run([...args, '--extensions', pair.ext, '--inventory', join(pair.dir, 'inv.json')])
  assert.equal(ok.status, 0, ok.stderr)
  assert.ok(
    readInventoryFile(join(pair.dir, 'inv.json')).items.includes('Raises the basic plan rate'),
  )
})

test('--inventory lists what the page carries, from the model its HTML is rendered from', (t) => {
  const dir = scratch(t)
  const range = buildFixtureRepo('mobile', join(dir, 'repo'))
  const root = join(FIXTURES_DIR, 'mobile')
  const inventory = join(dir, 'inv', 'mobile.json')
  renderPr({
    cwd: join(dir, 'repo'),
    ...range,
    judge: join(root, 'judge.json'),
    media: join(root, 'media', 'media.json'),
    out: join(dir, 'page'),
    inventory,
  })
  const listed = readInventoryFile(inventory)
  const html = readFileSync(join(dir, 'page', 'index.html'), 'utf8')
  assert.ok(listed.clips.length > 0)
  for (const clip of listed.clips) assert.ok(html.includes(clip), `the page shows ${clip}`)
  for (const id of listed.sections) assert.ok(sectionIds(html).includes(id), `the page has #${id}`)
  for (const item of listed.items) assert.ok(html.includes(item.split(' ').at(-1)), item)
})
