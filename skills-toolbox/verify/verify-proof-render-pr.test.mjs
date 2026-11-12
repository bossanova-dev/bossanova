import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
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

const CLI = 'skills-toolbox/verify/verify-proof-page.mjs'
const VALIDATED =
  'Every spec and media item passed validation, and every snippet is quoted from git.'

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
  assert.deepEqual(fixtureNames(), ['api', 'dbt', 'infra', 'mobile'])
  for (const name of fixtureNames()) {
    const out = join(scratch(t), 'page')
    const result = run(['--fixture', name, '--out', out])
    assert.equal(result.status, 0, `${name}: ${result.stderr}`)
    const html = readFileSync(join(out, 'index.html'), 'utf8')
    const { head } = JSON.parse(readFileSync(join(FIXTURES_DIR, name, 'spec.json'), 'utf8'))
    assert.ok(html.includes(head.slice(0, 7)), `${name}: page names head ${head}`)
    assert.ok(html.includes(VALIDATED), `${name}: spec and media validate against git`)
    assert.equal(readFileSync(join(out, 'player.js'), 'utf8'), proofPlayerScript())
  }
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
  assert.ok(html.includes('Cannot assess'))
  assert.ok(html.includes('local/repo'))
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
