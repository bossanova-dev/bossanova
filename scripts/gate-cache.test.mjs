import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const cli = path.join(repoRoot, 'scripts', 'gate-cache.mjs')

function git(root, args) {
  return execFileSync('git', args, { cwd: root, encoding: 'utf8' }).trim()
}

function fixture(t, { testUncached = 'BOSS_GATE_FORCE_UNCACHED=1 make test-affected' } = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'gate-cache-'))
  const stampDir = fs.mkdtempSync(path.join(os.tmpdir(), 'gate-cache-stamps-'))
  t.after(() => {
    fs.rmSync(root, { recursive: true, force: true })
    fs.rmSync(stampDir, { recursive: true, force: true })
  })
  git(root, ['init'])
  git(root, ['config', 'user.email', 'test@example.com'])
  git(root, ['config', 'user.name', 'Test User'])
  git(root, ['config', 'commit.gpgsign', 'false'])
  fs.writeFileSync(
    path.join(root, '.boss-skills.json'),
    JSON.stringify({
      commands: testUncached ? { testUncached } : {},
      gateCache: { eligible: { demo: { cacheable: true, reason: 'test' } } },
    }),
  )
  fs.writeFileSync(path.join(root, 'file.txt'), 'one\n')
  git(root, ['add', '.'])
  git(root, ['commit', '-m', 'initial'])
  return { root, stampDir, base: git(root, ['rev-parse', 'HEAD']) }
}

function run(root, stampDir, args, extraEnv = {}) {
  return spawnSync(process.execPath, [cli, ...args], {
    cwd: root,
    encoding: 'utf8',
    env: { ...process.env, BOSS_GATE_STAMP_DIR: stampDir, ...extraEnv },
  })
}

test('run records a successful gate and skips the identical second run', (t) => {
  const { root, stampDir, base } = fixture(t)
  const counter = path.join(stampDir, 'counter')
  const command = `${process.execPath} -e "require('fs').appendFileSync(process.env.COUNTER,'x')"`
  const first = run(
    root,
    stampDir,
    [
      'run',
      '--site',
      'demo',
      '--command',
      command,
      '--base-ref',
      base,
      '--',
      process.execPath,
      '-e',
      "require('fs').appendFileSync(process.env.COUNTER,'x')",
    ],
    { COUNTER: counter },
  )
  assert.equal(first.status, 0, first.stderr)
  assert.equal(fs.readFileSync(counter, 'utf8'), 'x')
  const second = run(
    root,
    stampDir,
    [
      'run',
      '--site',
      'demo',
      '--command',
      command,
      '--base-ref',
      base,
      '--',
      process.execPath,
      '-e',
      "require('fs').appendFileSync(process.env.COUNTER,'x')",
    ],
    { COUNTER: counter },
  )
  assert.equal(second.status, 0, second.stderr)
  assert.match(second.stdout, /cached at tree [0-9a-f]{12}/)
  assert.equal(fs.readFileSync(counter, 'utf8'), 'x')
})

test('non-zero gate status is not recorded', (t) => {
  const { root, stampDir, base } = fixture(t)
  const command = `${process.execPath} -e "process.exit(7)"`
  const args = [
    'run',
    '--site',
    'demo',
    '--command',
    command,
    '--base-ref',
    base,
    '--',
    process.execPath,
    '-e',
    'process.exit(7)',
  ]
  assert.equal(run(root, stampDir, args).status, 7)
  assert.equal(run(root, stampDir, args).status, 7)
})

test('forced uncached miss marks the child environment', (t) => {
  const { root, stampDir, base } = fixture(t)
  fs.writeFileSync(path.join(root, 'added.txt'), 'new\n')
  const command = `${process.execPath} -e "process.exit(process.env.BOSS_GATE_FORCE_UNCACHED==='1'&&process.env.GO_TEST_COUNT==='1'?0:9)"`
  const result = run(root, stampDir, [
    'run',
    '--site',
    'demo',
    '--command',
    command,
    '--base-ref',
    base,
    '--',
    process.execPath,
    '-e',
    "process.exit(process.env.BOSS_GATE_FORCE_UNCACHED==='1'&&process.env.GO_TEST_COUNT==='1'?0:9)",
  ])
  assert.equal(result.status, 0, result.stderr)
  assert.match(result.stdout, /forced uncached/)
})

test('forced uncached miss without commands.testUncached runs but does not stamp', (t) => {
  const { root, stampDir, base } = fixture(t, { testUncached: '' })
  fs.writeFileSync(path.join(root, 'added.txt'), 'new\n')
  const command = `${process.execPath} -e "require('fs').appendFileSync(process.env.COUNTER,'x')"`
  const counter = path.join(stampDir, 'counter')
  const args = [
    'run',
    '--site',
    'demo',
    '--command',
    command,
    '--base-ref',
    base,
    '--',
    process.execPath,
    '-e',
    "require('fs').appendFileSync(process.env.COUNTER,'x')",
  ]
  assert.equal(run(root, stampDir, args, { COUNTER: counter }).status, 0)
  assert.equal(run(root, stampDir, args, { COUNTER: counter }).status, 0)
  assert.equal(fs.readFileSync(counter, 'utf8'), 'xx')
})

test('not-eligible gates run and do not stamp', (t) => {
  const { root, stampDir, base } = fixture(t)
  const command = `${process.execPath} -e "require('fs').appendFileSync('counter','x')"`
  const args = [
    'run',
    '--site',
    'unknown',
    '--command',
    command,
    '--base-ref',
    base,
    '--',
    process.execPath,
    '-e',
    "require('fs').appendFileSync('counter','x')",
  ]
  assert.equal(run(root, stampDir, args).status, 0)
  assert.equal(run(root, stampDir, args).status, 0)
  assert.equal(fs.readFileSync(path.join(root, 'counter'), 'utf8'), 'xx')
})
