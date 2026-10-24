import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const script = fileURLToPath(new URL('./bs-sweep-debt-burndown.mjs', import.meta.url))

function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'debt-burndown-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 3 }))
  const git = (...args) => {
    const result = spawnSync('/usr/bin/git', args, { cwd: root, encoding: 'utf8' })
    assert.equal(result.status, 0, result.stderr)
    return result.stdout.trim()
  }
  const write = (file, content) => {
    const target = path.join(root, file)
    fs.mkdirSync(path.dirname(target), { recursive: true })
    fs.writeFileSync(target, content)
  }
  const json = (file, value) => write(file, JSON.stringify(value))
  const guard = (id, baseline, scans = ['src/**']) => {
    const baselinePath = `scripts/guards/${id}-baseline.json`
    write(
      `scripts/guards/registry/${id}.mjs`,
      `export default ${JSON.stringify({
        id,
        rule: `Check ${id}`,
        command: 'node check.mjs',
        wiring: { make: 'lint-scripts', ci: { workflow: 'test.yml', job: 'test' } },
        scans,
        baseline: baseline === null ? null : baselinePath,
        proof: { file: 'check.test.mjs', test: 'seeded violation' },
        ...(baseline !== null && !Array.isArray(baseline) ? { baselineFormat: 'json' } : {}),
      })}\n`,
    )
    if (baseline !== null)
      json(baselinePath, Array.isArray(baseline) ? [...baseline].sort() : baseline)
    return baselinePath
  }
  const commit = (message) => {
    git('add', '.')
    git('commit', '-m', message)
  }
  const run = (verb, args = [], prs = []) => {
    json('open-prs.json', prs)
    const result = spawnSync(
      process.execPath,
      [
        script,
        verb,
        '--repo-root',
        root,
        '--open-prs-json',
        path.join(root, 'open-prs.json'),
        ...args,
      ],
      { encoding: 'utf8' },
    )
    assert.equal(result.status, 0, result.stderr)
    return JSON.parse(result.stdout)
  }
  git('init', '-q')
  git('config', 'commit.gpgsign', 'false')
  git('config', 'user.name', 'Fixture')
  git('config', 'user.email', 'fixture@example.test')
  write('src/seed.js', 'export const seed = 1\n')
  return { root, git, write, json, guard, commit, run }
}

test('pick reports all-empty and ignores guards without baselines', (t) => {
  const f = fixture(t)
  f.guard('list', [])
  f.guard('counts', {})
  f.guard('without', null)
  f.commit('seed empty baselines')
  const result = f.run('pick')
  assert.equal(result.verdict, 'nothing-to-do')
  assert.equal(result.reason, 'all-empty')
  assert.equal(
    result.skipped.some(({ id }) => id === 'without'),
    false,
  )
})

test('open PR claims move overlapping picks to another guard and exhaust the pool', (t) => {
  const f = fixture(t)
  f.guard('alpha', ['src/seed.js'])
  f.guard('beta', ['src/seed.js'])
  f.commit('seed candidates')
  const first = f.run('pick', ['--today', '2026-10-24'])
  const claimed = [{ number: 12, body: `Burn-Down-Guard: ${first.guard.id}` }]
  const second = f.run('pick', ['--today', '2026-10-24'], claimed)
  assert.notEqual(second.guard.id, first.guard.id)
  assert.ok(
    second.skipped.some(
      ({ id, reason, pr }) => id === first.guard.id && reason === 'claimed' && pr === 12,
    ),
  )
  const afterRotation = f.run('pick', ['--today', '2026-10-25'], claimed)
  assert.ok(
    afterRotation.skipped.some(
      ({ id, reason, pr }) => id === first.guard.id && reason === 'claimed' && pr === 12,
    ),
    'claim rechecks must expose every peer claim even after rotation changes',
  )
  const exhausted = f.run(
    'pick',
    [],
    [...claimed, { number: 13, body: `Burn-Down-Guard: ${second.guard.id}` }],
  )
  assert.equal(exhausted.verdict, 'nothing-to-do')
  assert.equal(exhausted.reason, 'all-claimed')
})

test('daily rotation visits every guard once and respects exclusions', (t) => {
  const f = fixture(t)
  for (const id of ['alpha', 'beta', 'gamma']) f.guard(id, ['src/seed.js'])
  f.commit('seed rotation')
  const selected = [24, 25, 26].map((day) => f.run('pick', ['--today', `2026-10-${day}`]).guard.id)
  assert.equal(new Set(selected).size, 3)
  const excluded = f.run('pick', ['--today', '2026-10-24', '--exclude-guard', selected[0]])
  assert.notEqual(excluded.guard.id, selected[0])
  assert.ok(excluded.skipped.some(({ id, reason }) => id === selected[0] && reason === 'excluded'))
})

test('churn ranks files and directory sums, resolves scan-relative paths and truncates', (t) => {
  const f = fixture(t)
  f.guard(
    'churn',
    [
      'fakeClock',
      'src/cold.js',
      'src/hot.js — migrate later',
      'src/group/',
      'components/legacy.js',
    ],
    ['src/**', 'services/web/**'],
  )
  for (const file of [
    'src/hot.js',
    'src/cold.js',
    'src/group/one.js',
    'src/group/two.js',
    'services/web/components/legacy.js',
  ])
    f.write(file, 'seed\n')
  f.commit('seed churn')
  f.write('src/hot.js', 'second\n')
  f.write('src/group/one.js', 'second\n')
  f.write('src/group/two.js', 'second\n')
  f.commit('second change')
  f.write('src/hot.js', 'third\n')
  f.commit('third change')
  const result = f.run('pick', ['--limit', '10', '--ref', 'HEAD'])
  assert.deepEqual(
    result.entries.map(({ key }) => key),
    ['src/group/', 'src/hot.js', 'components/legacy.js', 'src/cold.js', 'fakeClock'],
  )
  assert.deepEqual(
    result.entries.map(({ churn }) => churn),
    [4, 3, 1, 1, 0],
  )
  assert.deepEqual(result.entries[0].paths, ['src/group/one.js', 'src/group/two.js'])
  assert.equal(result.entries[1].reason, 'migrate later')
  assert.deepEqual(result.entries[2].paths, ['services/web/components/legacy.js'])
  assert.deepEqual(f.run('pick', ['--limit', '2']).entries, result.entries.slice(0, 2))
})

test('count maps preserve counts and nested baselines are unsupported', (t) => {
  const f = fixture(t)
  f.guard('counts', { 'src/seed.js': 7, fakeClock: 2 })
  f.guard('nested', { packages: { fakeClock: 2 } })
  f.commit('seed counts')
  const result = f.run('pick')
  assert.equal(result.guard.id, 'counts')
  assert.equal(result.guard.shape, 'counts')
  assert.equal(result.guard.size, 2)
  assert.deepEqual(
    result.entries.map(({ key, count }) => ({ key, count })),
    [
      { key: 'src/seed.js', count: 7 },
      { key: 'fakeClock', count: 2 },
    ],
  )
  assert.ok(
    result.skipped.some(({ id, reason }) => id === 'nested' && reason === 'unsupported-shape'),
  )
})

test('expiry respects date boundaries, registry scans, malformed markers and PR claims', (t) => {
  const f = fixture(t)
  f.guard('registered', [], ['src/**'])
  f.write(
    'src/expired.js',
    '// guard-exception(registered): repair; expires 2026-10-23\n\nconst x = 1\n',
  )
  f.write('src/today.js', 'const x = 1 // guard-exception(registered): valid; expires 2026-10-24\n')
  f.write(
    'src/unknown.js',
    'const x = 1 // guard-exception(unknown): fixture; expires 2026-10-23\n',
  )
  f.write('docs/example.md', 'x // guard-exception(registered): example; expires 2026-10-23\n')
  f.write('src/malformed.js', 'const x = 1 // guard-exception(registered): no expiry\n')
  f.commit('seed expiry markers')
  const result = f.run('expired', ['--today', '2026-10-24'])
  assert.equal(result.verdict, 'expired')
  assert.deepEqual(result.exceptions, [
    {
      file: 'src/expired.js',
      line: 1,
      coversLine: 3,
      guardId: 'registered',
      reason: 'repair',
      expires: '2026-10-23',
    },
  ])
  assert.equal(result.ignored.length, 2)
  assert.equal(result.malformed.length, 1)
  const claimed = f.run(
    'expired',
    ['--today', '2026-10-24'],
    [{ number: 19, body: 'Exception-Expiry: yes' }],
  )
  assert.equal(claimed.verdict, 'claimed')
  assert.equal(claimed.pr, 19)
  f.write('src/expired.js', 'const x = 1\n')
  assert.equal(f.run('expired', ['--today', '2026-10-24']).verdict, 'none')
})

test('shrunk distinguishes smaller, identical and increased list and count baselines', (t) => {
  const f = fixture(t)
  const list = f.guard('list', ['src/one.js', 'src/two.js'])
  const counts = f.guard('counts', { 'src/one.js': 3, 'src/two.js': 2 })
  f.commit('seed baseline comparison')
  const base = f.git('rev-parse', 'HEAD')
  for (const guard of ['list', 'counts']) {
    assert.equal(f.run('shrunk', ['--guard', guard, '--base', base]).verdict, 'unchanged')
  }
  f.json(list, ['src/one.js'])
  f.json(counts, { 'src/one.js': 2, 'src/two.js': 2 })
  for (const guard of ['list', 'counts']) {
    assert.equal(f.run('shrunk', ['--guard', guard, '--base', base]).verdict, 'shrank')
  }
  f.json(list, ['src/one.js', 'src/two.js', 'src/three.js'])
  // A lower total must not hide a raised individual count.
  f.json(counts, { 'src/one.js': 4 })
  for (const guard of ['list', 'counts']) {
    assert.equal(f.run('shrunk', ['--guard', guard, '--base', base]).verdict, 'grew')
  }
})

test('CLI handles help, invalid arguments and failed gh reads with honest exit statuses', (t) => {
  const f = fixture(t)
  f.guard('list', [])
  f.commit('seed CLI fixture')
  const invoke = (args, env = process.env) =>
    spawnSync(process.execPath, [script, ...args], { cwd: f.root, encoding: 'utf8', env })
  assert.equal(invoke(['--help']).status, 0)
  assert.equal(invoke(['unknown']).status, 2)
  assert.equal(invoke(['pick', '--unknown']).status, 2)
  f.write('bin/gh', '#!/bin/sh\nexit 1\n')
  fs.chmodSync(path.join(f.root, 'bin/gh'), 0o755)
  const env = {
    ...process.env,
    PATH: `${path.join(f.root, 'bin')}${path.delimiter}${process.env.PATH}`,
  }
  delete env.BS_CRON_OPEN_PRS_JSON
  const failed = invoke(['pick', '--repo-root', f.root], env)
  assert.equal(failed.status, 2, failed.stderr)
  assert.equal(failed.stdout.includes('nothing-to-do'), false)
})
