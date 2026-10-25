import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { test } from 'node:test'

const cli = path.resolve('scripts/sweeps/bs-architecture.mjs')
function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'bs-architecture-test-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 3 }))
  const run = (...args) =>
    spawnSync(process.execPath, [cli, ...args], {
      cwd: root,
      encoding: 'utf8',
      env: { ...process.env, BS_ARCHITECTURE_STATE_DIR: path.join(root, 'state') },
    })
  const write = (name, data) => {
    const file = path.join(root, name)
    fs.writeFileSync(file, JSON.stringify(data))
    return file
  }
  const git = (...args) => {
    const r = spawnSync('git', args, { cwd: root, encoding: 'utf8' })
    assert.equal(r.status, 0, r.stderr)
    return r.stdout.trim()
  }
  return { root, run, write, git }
}
const metrics = (generatedAt = '2026-09-01T00:00:00Z', measurements = {}) => ({
  schemaVersion: 1,
  generatedAt,
  commit: 'abcdef0123456789',
  measurements,
})

test('history chooses by generatedAt, saves atomically and never replaces another run', (t) => {
  const { run, write } = fixture(t)
  assert.equal(run('previous', '--before', '2026-10-01').status, 3)
  const first = write('first.json', metrics())
  assert.equal(run('save', '--metrics', first).status, 0)
  assert.equal(run('save', '--metrics', first).status, 0)
  assert.equal(
    run('save', '--metrics', write('collision.json', metrics('2026-09-01T12:00:00Z'))).status,
    2,
  )
  const second = write('second.json', metrics('2026-10-01T00:00:00Z'))
  assert.equal(run('save', '--metrics', second).status, 0)
  const prev = run('previous', '--before', '2026-10-01T00:00:00Z')
  assert.equal(
    JSON.parse(fs.readFileSync(prev.stdout.trim(), 'utf8')).generatedAt,
    metrics().generatedAt,
  )
  assert.equal(run('previous', '--before', 'invalid').status, 2)
})

test('diff ranks growth, new subjects, budget threshold and mutation candidates using generator shape', (t) => {
  const { run, write } = fixture(t)
  const before = metrics(undefined, {
    packages: { a: { lines: 20, exported: 2 } },
    interfaces: [],
    fanInOut: {},
    largestFiles: [],
    baselines: { guard: { total: 3 } },
    testRatio: null,
  })
  const packages = Object.fromEntries(
    ['a', 'b', 'c', 'd', 'x/cmd', 'services/boss/internal/daemon', 'x/gen'].map((name, i) => [
      name,
      { lines: 10 + i, testLines: 5 },
    ]),
  )
  packages.a.lines = 80
  packages['generated-only'] = { lines: 0, testLines: 5 }
  packages['without-tests'] = { lines: 5, testLines: 0 }
  const after = metrics('2026-10-01', {
    packages,
    churn: Object.fromEntries(Object.keys(packages).map((k, i) => [k, i + 1])),
    interfaces: [{ pkg: 'b', name: 'Wide', methods: 4 }],
    fanInOut: { a: { fanIn: 3, fanOut: 1 } },
    largestFiles: [{ file: 'huge.go', lines: 100 }],
    baselines: { guard: { total: 4 } },
    budgets: {
      size: {
        subjects: {
          yes: { measured: 90, limit: 100 },
          no: { measured: 91, limit: 100 },
          missing: { measured: null, limit: 100 },
        },
        defaultSubjects: [],
      },
    },
    testRatio: { total: { ratio: 2 } },
  })
  const result = run(
    'diff',
    '--previous',
    write('before.json', before),
    '--current',
    write('after.json', after),
  )
  assert.equal(result.status, 0, result.stderr)
  const diff = JSON.parse(result.stdout)
  assert.deepEqual(diff.movers[0], {
    kind: 'file.lines',
    subject: 'huge.go',
    before: 0,
    after: 100,
    delta: 100,
  })
  assert.equal(diff.movers[1].delta, 60)
  assert.ok(diff.movers.some((row) => row.subject === 'b.Wide' && row.before === 0))
  assert.deepEqual(diff.budgetCuts, [{ guard: 'size', subject: 'yes', before: 100, after: 90 }])
  assert.deepEqual(
    diff.mutationTargets.map((row) => row.subject),
    ['d', 'c', 'b'],
  )
  assert.deepEqual(diff.deltas.testRatio, { unavailable: true })
  assert.equal(diff.deltas['baselines.guard.total'].delta, 1)
})

test('unavailable groups never become new growth or empty successful mutation results', (t) => {
  const { run, write } = fixture(t)
  const r = run(
    'diff',
    '--previous',
    write('p', metrics(undefined, { packages: null })),
    '--current',
    write('c', metrics(undefined, { packages: { a: { lines: 50, testLines: 2 } }, churn: null })),
  )
  const result = JSON.parse(r.stdout)
  assert.deepEqual(result.movers, [])
  assert.equal(result.mutationTargets, null)
  assert.deepEqual(result.deltas.packages, { unavailable: true })
})

test('report validates review shape, covers fixed sections and enforces byte cap', (t) => {
  const { run, write, root } = fixture(t)
  const diff = write('diff', {
    comparison: { previous: 'seed', current: 'current' },
    movers: [],
    budgetCuts: [],
    mutationTargets: null,
    deltas: {},
  })
  const review = {
    movers: [],
    survivors: 'skipped: gremlins missing',
    deadTests: 'unproven: no mutation run',
    testValue: 'No suspects',
    suggestedEpic: {
      title: 'Improve structure',
      issues: [{ title: 'Split package', description: 'Reduce coupling' }],
    },
  }
  const out = path.join(root, 'body')
  assert.equal(
    run('report', '--diff', diff, '--review', write('review', review), '--out', out).status,
    0,
  )
  const body = fs.readFileSync(out, 'utf8')
  assert.match(body, /seed/)
  assert.match(body, /skipped: gremlins missing/)
  assert.ok(body.endsWith('Reduce coupling\n'))
  assert.equal(
    run(
      'report',
      '--diff',
      diff,
      '--review',
      write('bad', { ...review, movers: [{ subject: 'a', verdict: 'maybe' }] }),
      '--out',
      out,
    ).status,
    2,
  )
  assert.equal(
    run(
      'report',
      '--diff',
      diff,
      '--review',
      write('big', { ...review, testValue: 'é'.repeat(31000) }),
      '--out',
      out,
    ).status,
    2,
  )
})

for (const mutation of ['commit', 'detached-commit', 'branch', 'dirty', 'remote-fast-forward']) {
  test(`readonly detects ${mutation}`, (t) => {
    const { run, root, git } = fixture(t)
    git('init', '-b', 'main')
    git('config', 'commit.gpgsign', 'false')
    git('config', 'user.name', 'Fixture')
    git('config', 'user.email', 'fixture@example.com')
    fs.writeFileSync(path.join(root, 'tracked'), 'one')
    git('add', 'tracked')
    git('commit', '-m', 'initial')
    git('update-ref', 'refs/remotes/origin/main', 'HEAD')
    if (mutation === 'detached-commit') git('checkout', '--detach', 'HEAD')
    const snap = path.join(os.tmpdir(), `bs-architecture-snapshot-${path.basename(root)}.json`)
    t.after(() => fs.rmSync(snap, { force: true }))
    assert.equal(run('snapshot', '--out', snap).status, 0)
    if (mutation === 'branch') git('branch', 'extra')
    else {
      fs.writeFileSync(path.join(root, 'tracked'), 'two')
      if (mutation !== 'dirty') {
        git('add', 'tracked')
        git('commit', '-m', 'next')
      }
      if (mutation === 'remote-fast-forward') git('update-ref', 'refs/remotes/origin/main', 'HEAD')
    }
    const result = run('verify-readonly', '--snapshot', snap)
    assert.equal(result.status, mutation === 'remote-fast-forward' ? 0 : 1, result.stderr)
  })
}
