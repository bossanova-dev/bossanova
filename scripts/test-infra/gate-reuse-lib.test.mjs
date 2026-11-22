import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'
import { tempDir } from './test-tmpdir.mjs'
import {
  canonicalCommand,
  claimReuse,
  decideReuse,
  parseMakeDatabase,
  readMakeDatabase,
  reuseKey,
} from './gate-reuse-lib.mjs'

// GNU Make 3.81 prints a tab-only line after each recipe and includes non-target records.
const database = `# Files
# Not a target:
ignored: alias

alias: test
#  Implicit rule search has not been done.

test: test-affected
#  File has not been updated.

test-affected:
#  commands to execute (from 'mk/test.mk', line 40):
\t@echo selected
\t

test-full:
#  commands to execute (from 'mk/test.mk', line 75):
\t$(MAKE) test-all
\t

test-all:
#  commands to execute (from 'Makefile', line 1):
\t@echo all
\t

local:
#  recipe to execute (from 'Makefile', line 1):
\t$(MAKE) -C . test

scoped:
#  commands to execute (from 'Makefile', line 1):
\t$(MAKE) test-all RACE=1

cycle-a: cycle-b

cycle-b: cycle-a
`
const makeDb = parseMakeDatabase(database)
const canonical = (argv) => canonicalCommand(argv, { makeDb, env: {} })

test('Make aliases resolve chains and recursive recipes without losing options or overrides', () => {
  for (const [goal, expected] of [
    ['test', 'test-affected'],
    ['test-full', 'test-all'],
    ['alias', 'test-affected'],
    ['local', 'test-affected'],
    ['scoped', 'scoped'],
    ['ignored', 'ignored'],
    ['missing', 'missing'],
    ['cycle-a', 'cycle-a'],
  ])
    assert.deepEqual(canonical(['make', goal]).argv, ['make', expected])
  assert.deepEqual(canonical(['make', '-j2', 'test', 'RACE=1']).argv, [
    'make',
    '-j2',
    'test-affected',
    'RACE=1',
  ])
  assert.deepEqual(canonical(['env', 'Z=2', 'A=1', 'make', 'test']).argv, [
    'env',
    'A=1',
    'Z=2',
    'make',
    'test-affected',
  ])
})

test('Make option values remain verbatim instead of being resolved as alias goals', () => {
  for (const flag of ['-I', '--include-dir', '-W', '--what-if', '-o', '--old-file']) {
    assert.deepEqual(canonical(['make', flag, 'test', 'test-full']).argv, [
      'make',
      flag,
      'test',
      'test-all',
    ])
  }
  assert.deepEqual(canonical(['make', '-j', '2', 'test']).argv, [
    'make',
    '-j',
    '2',
    'test-affected',
  ])
  assert.deepEqual(canonical(['make', '-sC', '.', 'test']).argv, ['make', '-sC', '.', 'test'])
})

test('repeated env assignments retain their last-wins order in command identity', () => {
  const first = canonical(['env', 'RACE=2', 'RACE=1', 'make', 'test']).argv
  const second = canonical(['env', 'RACE=1', 'RACE=2', 'make', 'test']).argv
  assert.notDeepEqual(first, second)
  assert.deepEqual(first, ['env', 'RACE=2', 'RACE=1', 'make', 'test-affected'])
})

test('other directories, Makefiles and non-make commands keep their exact argv', () => {
  for (const argv of [
    ['make', '-C', '.', 'test'],
    ['make', '-C.', 'test'],
    ['make', '-f', 'else.mk', 'test'],
    ['make', '--file=else.mk', 'test'],
    ['node', 'test'],
  ])
    assert.deepEqual(canonical(argv).argv, argv)
  assert.deepEqual(canonicalCommand(['make', 'test'], { makeDb: null, env: {} }).argv, [
    'make',
    'test',
  ])
})

test('the database probe never asks make to execute a recursive requested goal', (t) => {
  const cwd = tempDir('gate-reuse-db-', t)
  fs.writeFileSync(
    path.join(cwd, 'Makefile'),
    'test-full: ; $(MAKE) test-all\ntest-all: ; touch marker\n',
  )
  const db = readMakeDatabase(['make', 'test-full'], { cwd, env: process.env })
  assert.deepEqual(canonicalCommand(['make', 'test-full'], { makeDb: db, env: {} }).argv, [
    'make',
    'test-all',
  ])
  assert.equal(fs.existsSync(path.join(cwd, 'marker')), false)
  assert.equal(readMakeDatabase(['make', 'test'], { cwd: path.join(cwd, 'absent'), env: {} }), null)
})

const identity = {
  cwd: '/',
  canonical: ['make', 'test-affected'],
  treeHash: 'tree',
  baseCommit: 'base',
  forced: false,
  env: {},
}
test('reuse keys bind canonical argv to cwd, tree, merge-base, forced intent and test environment', () => {
  const key = reuseKey(identity)
  assert.equal(reuseKey({ ...identity, canonical: canonical(['make', 'test']).argv }), key)
  for (const patch of [
    { cwd: '/other' },
    { treeHash: 'changed' },
    { baseCommit: 'other' },
    { forced: true },
    ...[
      'RACE',
      'MAKEOVERRIDES',
      'GO_TEST_PACKAGES',
      'GO_TEST_RUN',
      'GO_TEST_COUNT',
      'SHORT',
      'CI',
    ].map((name) => ({
      env: { [name]: 'changed' },
    })),
  ])
    assert.notEqual(reuseKey({ ...identity, ...patch }), key)
  assert.equal(reuseKey({ ...identity, treeHash: null }), null)
  assert.equal(
    canonicalCommand(['make', 'test'], { makeDb, env: { BOSS_GATE_FORCE_UNCACHED: '1' } }).forced,
    true,
  )
  assert.equal(canonical(['env', 'BOSS_GATE_FORCE_UNCACHED=1', 'make', 'test']).forced, true)
})

const now = 100_000
const entry = {
  runDir: '/run',
  startedAt: now - 500,
  treeHash: 'tree',
  canonical: ['make', 'test'],
}
const passed = {
  status: 0,
  stopped: false,
  alive: false,
  tree: { start: 'tree', end: 'tree' },
  finishedAt: now - 100,
}
const decision = (source, options = {}) =>
  decideReuse({ entry, source, now, env: {}, forced: false, ...options })

test('only live runs or fresh matching passes can be reused', () => {
  assert.equal(decision({ status: null, alive: true }), 'join')
  assert.equal(decision({ status: null, alive: true }, { forced: true }), 'join')
  assert.equal(decision(passed), 'reuse-passed')
  for (const source of [
    { ...passed, status: 1 },
    { ...passed, status: 75 },
    { ...passed, status: 95 },
    { ...passed, status: null },
    { ...passed, stopped: true },
    { ...passed, tree: { start: 'tree', end: 'changed' } },
    { ...passed, tree: { start: 'changed', end: 'changed' } },
    { ...passed, finishedAt: now - 3_600_001 },
  ])
    assert.equal(decision(source), 'fresh')
  assert.equal(decision(passed, { forced: true }), 'fresh')
  assert.equal(decision(passed, { env: { BOSS_GATE_REUSE: '0' } }), 'fresh')
  assert.equal(decision({ status: null, alive: true }, { env: { BOSS_GATE_REUSE: '0' } }), 'fresh')
  assert.equal(decision(passed, { entry: null }), 'fresh')
  assert.equal(decision(passed, { entry: { ...entry, canonical: ['make', 'test-race'] } }), 'fresh')
  assert.equal(decision(passed, { env: { BOSS_GATE_REUSE_WINDOW_MS: '50' } }), 'fresh')
})

test('fresh decisions replace unusable index entries and later callers find the new owner', (t) => {
  const dir = tempDir('gate-reuse-index-', t)
  const source = path.join(dir, 'source')
  const replacement = path.join(dir, 'replacement')
  fs.mkdirSync(source)
  fs.mkdirSync(replacement)
  const context = { key: 'key', treeHash: 'tree', canonical: ['make', 'test'], forced: false }
  const env = { BOSS_GATE_RUN_INDEX_DIR: path.join(dir, 'index') }
  assert.equal(claimReuse(context, source, { env }).kind, 'fresh')
  fs.writeFileSync(path.join(source, 'status'), '1\n')
  assert.equal(claimReuse(context, replacement, { env }).kind, 'fresh')
  fs.writeFileSync(path.join(replacement, 'pid'), String(process.pid))
  const later = claimReuse(context, path.join(dir, 'later'), { env })
  assert.equal(later.kind, 'join')
  assert.equal(later.from, replacement)
})
