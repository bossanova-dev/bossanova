import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const cli = path.join(repoRoot, 'scripts', 'gate-cache.mjs')

test('R1: gateCache gives every gate one verdict, keyed on real make targets (BOS-1339)', async () => {
  const { eligibleGate } = await import('./gate-stamp-lib.mjs')
  const config = JSON.parse(fs.readFileSync(path.join(repoRoot, '.boss-skills.json'), 'utf8'))
  const eligible = Object.keys(config.gateCache.eligible)
  const ineligible = Object.keys(config.gateCache.ineligible)

  // One lookup, one verdict: no key may sit in both tables.
  assert.deepEqual(
    eligible.filter((key) => ineligible.includes(key)),
    [],
  )

  // Every key names a target the root Makefile actually defines, so neither table can describe a
  // gate nothing runs.
  const makefile = fs.readFileSync(path.join(repoRoot, 'Makefile'), 'utf8')
  const escape = (value) => value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  for (const key of [...eligible, ...ineligible]) {
    assert.match(makefile, new RegExp(`^${escape(key)}:`, 'm'), `no Makefile target for ${key}`)
  }

  // The readiness receipt is never cache-eligible, although it normalizes to the eligible
  // `test-full`; iterative full decisions stay cacheable.
  assert.equal(typeof config.commands.testReadiness, 'string')
  assert.equal(eligibleGate(config, config.commands.testReadiness).eligible, false)
  assert.equal(eligibleGate(config, 'make test-full').eligible, true)

  // The narrow gate stays out of the cache for the reason BOS-1265 recorded.
  assert.equal(config.gateCache.eligible['test-affected'], undefined)
  assert.match(config.gateCache.ineligible['test-affected'], /selection-dependent/)
})

function git(root, args) {
  return execFileSync('git', args, { cwd: root, encoding: 'utf8' }).trim()
}

function fixture(
  t,
  {
    testUncached = 'BOSS_GATE_FORCE_UNCACHED=1 make test-affected',
    eligible = { demo: { cacheable: true, reason: 'test' } },
  } = {},
) {
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
      gateCache: { eligible },
    }),
  )
  fs.writeFileSync(path.join(root, 'file.txt'), 'one\n')
  git(root, ['add', '.'])
  git(root, ['commit', '-m', 'initial'])
  return { root, stampDir, base: git(root, ['rev-parse', 'HEAD']) }
}

// The suite may itself run under a readiness gate that exports BOSS_GATE_FORCE_UNCACHED; scrub it
// (and its companion marker) so a test only sees the forcing it passes in extraEnv.
function run(root, stampDir, args, extraEnv = {}) {
  const env = { ...process.env, BOSS_GATE_STAMP_DIR: stampDir }
  delete env.BOSS_GATE_FORCE_UNCACHED
  delete env.BOSS_GATE_UNCACHED_COMMAND_ACTIVE
  return spawnSync(process.execPath, [cli, ...args], {
    cwd: root,
    encoding: 'utf8',
    env: { ...env, ...extraEnv },
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

test('an inherited BOSS_GATE_FORCE_UNCACHED=1 is never served from a stamp', (t) => {
  const { root, stampDir, base } = fixture(t)
  const counter = path.join(stampDir, 'counter')
  const command = `${process.execPath} -e "require('fs').appendFileSync(process.env.COUNTER,'x')"`
  const args = (mode) => [
    mode,
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
  assert.equal(run(root, stampDir, args('run'), { COUNTER: counter }).status, 0)
  const forced = { COUNTER: counter, BOSS_GATE_FORCE_UNCACHED: '1' }
  const second = run(root, stampDir, args('run'), forced)
  assert.equal(second.status, 0, second.stderr)
  assert.doesNotMatch(second.stdout, /cached at tree/)
  assert.match(second.stdout, /inherited BOSS_GATE_FORCE_UNCACHED=1/)
  assert.equal(fs.readFileSync(counter, 'utf8'), 'xx')
  // `check` agrees: a stamp exists for this tree, yet the inherited env is not eligible.
  assert.equal(run(root, stampDir, args('check').slice(0, 7), forced).status, 3)
  assert.equal(run(root, stampDir, args('check').slice(0, 7)).status, 0)
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

test('narrow selections never share a full readiness gate, while full gates still cache', (t) => {
  const { root, stampDir, base } = fixture(t, {
    eligible: { 'test-full': { cacheable: true, reason: 'final tree full gate' } },
  })
  const counter = path.join(stampDir, 'counter')
  const command = `${process.execPath} -e "require('fs').appendFileSync(process.env.COUNTER,'x')"`
  const args = (site) => [
    'run',
    '--site',
    site,
    '--command',
    command,
    '--base-ref',
    base,
    '--',
    process.execPath,
    '-e',
    "require('fs').appendFileSync(process.env.COUNTER,'x')",
  ]
  // `test-affected` is deliberately unlisted. Two otherwise identical narrow selections must
  // both execute rather than sharing a tree-hash stamp whose key lacks the selected files.
  assert.equal(run(root, stampDir, args('test-affected'), { COUNTER: counter }).status, 0)
  assert.equal(run(root, stampDir, args('test-affected'), { COUNTER: counter }).status, 0)
  assert.equal(run(root, stampDir, args('test-full'), { COUNTER: counter }).status, 0)
  assert.equal(run(root, stampDir, args('test-full'), { COUNTER: counter }).status, 0)
  assert.equal(fs.readFileSync(counter, 'utf8'), 'xxx')
})

// BOS-1339: `make test-affected` asks this mode whether the branch adds or renames an input, and
// forces its selected commands uncached on exit 0. `unknown` is fail-safe: it exits 0 as well.
function addsOrRenames(root, stampDir, base) {
  const result = run(root, stampDir, ['adds-or-renames', '--base-ref', base])
  return { status: result.status, line: result.stdout.trim(), stderr: result.stderr }
}

test('adds-or-renames answers yes for a committed add', (t) => {
  const { root, stampDir, base } = fixture(t)
  fs.writeFileSync(path.join(root, 'added.txt'), 'new\n')
  git(root, ['add', 'added.txt'])
  git(root, ['commit', '-m', 'add'])
  const verdict = addsOrRenames(root, stampDir, base)
  assert.equal(verdict.status, 0, verdict.stderr)
  assert.match(verdict.line, /^adds-or-renames: yes \(.+\)$/)
})

test('adds-or-renames answers yes for a committed rename', (t) => {
  const { root, stampDir, base } = fixture(t)
  git(root, ['mv', 'file.txt', 'renamed.txt'])
  git(root, ['commit', '-m', 'rename'])
  const verdict = addsOrRenames(root, stampDir, base)
  assert.equal(verdict.status, 0, verdict.stderr)
  assert.match(verdict.line, /^adds-or-renames: yes \(.+\)$/)
})

test('adds-or-renames answers yes for an untracked file', (t) => {
  const { root, stampDir, base } = fixture(t)
  fs.writeFileSync(path.join(root, 'untracked.txt'), 'new\n')
  const verdict = addsOrRenames(root, stampDir, base)
  assert.equal(verdict.status, 0, verdict.stderr)
  assert.match(verdict.line, /^adds-or-renames: yes \(.+\)$/)
})

test('adds-or-renames answers no (exit 1) for a modify-only branch', (t) => {
  const { root, stampDir, base } = fixture(t)
  fs.writeFileSync(path.join(root, 'file.txt'), 'changed\n')
  git(root, ['commit', '-am', 'modify'])
  const verdict = addsOrRenames(root, stampDir, base)
  assert.equal(verdict.status, 1, verdict.stderr)
  assert.match(verdict.line, /^adds-or-renames: no \(.+\)$/)
})

test('adds-or-renames answers unknown (exit 0, fail safe) when the base ref cannot be resolved', (t) => {
  const { root, stampDir } = fixture(t)
  const verdict = addsOrRenames(root, stampDir, 'refs/heads/no-such-base')
  assert.equal(verdict.status, 0, verdict.stderr)
  assert.match(verdict.line, /^adds-or-renames: unknown \(.+\)$/)
})
