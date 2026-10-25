import assert from 'node:assert/strict'
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { spawnSync } from 'node:child_process'
import {
  goE2eTests,
  playwrightTests,
  lowerLevelProofs,
  lintOffenders,
  unprovenCriteria,
} from './feature-coverage.mjs'

const features = [
  {
    id: 'scheduled-jobs',
    title: 'Jobs',
    criteria: [{ id: 'scheduled-jobs.AC1', text: 'Lists jobs' }],
  },
]
const file = 'services/web/e2e/jobs.spec.ts'
const parse = (source, name = file) => playwrightTests(name, source)
const lint = (source) => lintOffenders(features, parse(source))
test('e2e criterion lint rejects an unknown criterion id', () => {
  assert.deepEqual(lint("test('unknown', { tag: '@scheduled-jobs.AC9' }, () => {})"), [
    `unknown-id ${file}::unknown::scheduled-jobs.AC9`,
  ])
})
test('e2e criterion lint rejects an e2e test with no id or allowed tag', () => {
  assert.deepEqual(lint("test('missing', () => {})"), [`missing-id ${file}::missing`])
})
test('e2e criterion lint rejects two tests for one criterion on one surface', () => {
  const source =
    "test('first', { tag: '@scheduled-jobs.AC1' }, () => {}); test('second', { tag: '@scheduled-jobs.AC1' }, () => {})"
  assert.deepEqual(lint(source), ['duplicate scheduled-jobs.AC1 web'])
})
test('e2e criterion lint accepts one test per surface and an @layout spec', () => {
  const tests = [
    ...parse(
      "test('web', { tag: '@scheduled-jobs.AC1' }, () => {}); test('layout', { tag: '@layout' }, () => {})",
    ),
    ...goE2eTests(
      'services/boss/e2e/jobs_cli_test.go',
      'func TestJourney_ScheduledJobs_AC1_List(t *testing.T) {}',
      features,
    ),
  ]
  assert.deepEqual(lintOffenders(features, tests), [])
})
test('e2e criterion lint inherits a describe tag', () => {
  const tests = parse(
    "test.describe('outer', { tag: '@scheduled-jobs.AC1' }, () => { test.describe('inner', () => { test('lists', () => {}) }) })",
  )
  assert.equal(tests[0].name, 'outer > inner > lists')
  assert.deepEqual(lintOffenders(features, tests), [])
})
test('e2e criterion lint rejects a computed tag', () => {
  assert.ok(
    lint("test('computed', { tag: dynamic }, () => {})").includes(`computed-tag ${file}::computed`),
  )
})
test('e2e criterion lint allows quarantine as a modifier but not an id', () => {
  assert.deepEqual(
    lint("test('ok', { tag: ['@quarantine', '@scheduled-jobs.AC1'] }, () => {})"),
    [],
  )
  assert.deepEqual(lint("test('missing', { tag: '@quarantine' }, () => {})"), [
    `missing-id ${file}::missing`,
  ])
})
test('go feature segments match ids case-insensitively without hyphens and reject a colliding id', () => {
  const tests = goE2eTests(
    'services/boss/e2e/jobs_tui_test.go',
    'func TestJourney_scheduledjobs_AC1_List(t *testing.T) {}',
    features,
  )
  assert.deepEqual(tests[0].ids, ['scheduled-jobs.AC1'])
  assert.throws(
    () => goE2eTests('x_cli_test.go', '', [...features, { id: 'scheduledjobs', criteria: [] }]),
    /collid/i,
  )
})
test('lower-level proofs include Go segments and vitest title tokens', () => {
  const go = lowerLevelProofs(
    'lib/jobs/jobs_test.go',
    'func TestProof_ScheduledJobs_AC1_List(t *testing.T) {}',
    features,
  )
  const web = lowerLevelProofs(
    'services/web/src/jobs.test.ts',
    "it('lists @scheduled-jobs.AC1', () => {})",
    features,
  )
  assert.deepEqual(unprovenCriteria(features, [...go, ...web]), [])
  assert.equal(go[0].level, 'go')
  assert.equal(web[0].level, 'vitest')
})
function fixture(t) {
  const root = mkdtempSync(path.join(tmpdir(), 'feature-coverage-'))
  t.after(() => rmSync(root, { recursive: true, force: true, maxRetries: 3 }))
  for (const dir of ['docs/features', 'scripts/guards', 'services/web/e2e'])
    mkdirSync(path.join(root, dir), { recursive: true })
  writeFileSync(
    path.join(root, 'docs/features/README.md'),
    '---\nuiRoots: []\nuserFacingRoots: []\nignore: []\n---\n# Features\n',
  )
  writeFileSync(
    path.join(root, 'docs/features/jobs.md'),
    '---\nid: scheduled-jobs\ntitle: Jobs\nsurfaces: [web]\nowns: [services/web/e2e/**]\n---\n- AC1 Lists jobs.\n',
  )
  writeFileSync(path.join(root, file), "test('list', { tag: '@scheduled-jobs.AC1' }, () => {})")
  spawnSync('git', ['init', '-q'], { cwd: root })
  spawnSync('git', ['add', '.'], { cwd: root })
  return root
}
function run(root, ...args) {
  return spawnSync(
    process.execPath,
    ['scripts/guards/feature-coverage.mjs', ...args, '--root', root],
    { encoding: 'utf8' },
  )
}
test('feature coverage page rejects a hand edit', (t) => {
  const root = fixture(t)
  assert.equal(run(root, 'page', '--write').status, 0)
  assert.equal(run(root, 'page', '--check').status, 0)
  writeFileSync(path.join(root, 'docs/features/coverage.md'), 'hand edit')
  assert.equal(run(root, 'page', '--check').status, 1)
})
test("criterion proofs reject deleting a criterion's only proof", (t) => {
  const root = fixture(t)
  assert.equal(run(root, 'proofs', '--update').status, 0)
  assert.equal(run(root, 'proofs', '--check').status, 0)
  writeFileSync(path.join(root, file), "test('layout', { tag: '@layout' }, () => {})")
  const result = run(root, 'proofs', '--check')
  assert.equal(result.status, 1)
  assert.match(result.stderr, /scheduled-jobs.AC1/)
})

test('e2e criterion lint counts a generated test once and ignores hooks and steps', () => {
  const tests = parse(
    "test.beforeEach(() => {}); for (const name of names) { test(`lists ${name}`, { tag: '@scheduled-jobs.AC1' }, () => { test.step('open', () => {}) }) }",
  )
  assert.equal(tests.length, 1)
  assert.deepEqual(lintOffenders(features, tests), [])
})
test('lint command rejects seeded unknown, missing and duplicate proofs beyond the baseline', (t) => {
  const root = fixture(t)
  assert.equal(run(root, 'lint', '--update').status, 0)
  assert.equal(run(root, 'lint', '--check').status, 0)
  for (const [source, expected] of [
    ["test('unknown', { tag: '@scheduled-jobs.AC9' }, () => {})", 'unknown-id'],
    ["test('missing', () => {})", 'missing-id'],
    [
      "test('one', { tag: '@scheduled-jobs.AC1' }, () => {}); test('two', { tag: '@scheduled-jobs.AC1' }, () => {})",
      'duplicate',
    ],
  ]) {
    writeFileSync(path.join(root, file), source)
    const result = run(root, 'lint', '--check')
    assert.equal(result.status, 1)
    assert.ok(result.stderr.includes(expected), result.stderr)
  }
})
