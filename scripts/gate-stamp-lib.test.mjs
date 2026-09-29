import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import {
  branchAddsOrRenames,
  computeTreeHash,
  eligibleGate,
  expansionVersions,
  gateKey,
  makeStampDir,
  normalizeGateSite,
  readStamp,
  recordStamp,
  resolveBaseCommit,
} from './gate-stamp-lib.mjs'

function git(root, args) {
  return execFileSync('git', args, { cwd: root, encoding: 'utf8' }).trim()
}

function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'gate-stamp-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true }))
  git(root, ['init'])
  git(root, ['config', 'user.email', 'test@example.com'])
  git(root, ['config', 'user.name', 'Test User'])
  git(root, ['config', 'commit.gpgsign', 'false'])
  git(root, ['config', 'core.filemode', 'true'])
  fs.writeFileSync(path.join(root, 'file.txt'), 'one\n')
  git(root, ['add', 'file.txt'])
  git(root, ['commit', '-m', 'initial'])
  return root
}

test('gateKey is stable and changes for identity inputs', () => {
  const base = {
    treeHash: 'tree',
    command: 'make test-bossd',
    baseCommit: 'base',
    versions: {
      node: 'v1',
      RACE: '',
      BOSS_NO_BAZEL: '',
      BAZEL_TEST_FLAGS: '',
      BAZEL_TEST_EXTRA_FLAGS: '',
      MAKEFLAGS: '',
      MAKEOVERRIDES: '',
    },
  }
  assert.equal(gateKey(base), gateKey(base))
  assert.notEqual(gateKey(base), gateKey({ ...base, command: 'make test-boss' }))
  assert.notEqual(gateKey(base), gateKey({ ...base, versions: { ...base.versions, RACE: '1' } }))
  assert.notEqual(
    gateKey(base),
    gateKey({ ...base, versions: { ...base.versions, MAKEOVERRIDES: 'BOSS_NO_BAZEL=1' } }),
  )
  assert.notEqual(
    gateKey(base),
    gateKey({
      ...base,
      versions: { ...base.versions, BAZEL_TEST_EXTRA_FLAGS: '--nocache_test_results' },
    }),
  )
  assert.notEqual(gateKey(base), gateKey({ ...base, baseCommit: 'other-base' }))
})

test('two commits with identical trees and different messages produce the same tree hash', (t) => {
  const root = fixture(t)
  const before = computeTreeHash(root)
  git(root, ['commit', '--allow-empty', '-m', 'message-only rewrite surrogate'])
  assert.equal(computeTreeHash(root), before)
})

test('working-tree and untracked files change the tree hash', (t) => {
  const root = fixture(t)
  const before = computeTreeHash(root)
  fs.writeFileSync(path.join(root, 'file.txt'), 'two\n')
  assert.notEqual(computeTreeHash(root), before)
  git(root, ['checkout', '--', 'file.txt'])
  const clean = computeTreeHash(root)
  fs.writeFileSync(path.join(root, 'new.txt'), 'new\n')
  assert.notEqual(computeTreeHash(root), clean)
})

test('working-tree mode changes change the tree hash', (t) => {
  const root = fixture(t)
  const before = computeTreeHash(root)
  fs.chmodSync(path.join(root, 'file.txt'), 0o755)
  assert.notEqual(computeTreeHash(root), before)
})

test('recordStamp records only successful gates', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'gate-stamps-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  assert.equal(makeStampDir(dir).ok, true)
  assert.equal(recordStamp(dir, 'ok', { exitStatus: 0, treeHash: 'abc' }), true)
  assert.equal(readStamp(dir, 'ok').hit, true)
  assert.equal(recordStamp(dir, 'fail', { exitStatus: 1, treeHash: 'abc' }), false)
  assert.equal(readStamp(dir, 'fail').hit, false)
})

test('unwritable stamp directories are unusable', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'gate-stamps-readonly-'))
  t.after(() => {
    fs.chmodSync(dir, 0o700)
    fs.rmSync(dir, { recursive: true, force: true })
  })
  fs.chmodSync(dir, 0o500)
  assert.equal(makeStampDir(dir).ok, false)
})

test('corrupt stamp is a miss, not a throw', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'gate-stamps-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  fs.writeFileSync(path.join(dir, 'gate-bad.json'), '{')
  assert.deepEqual(readStamp(dir, 'bad'), {
    hit: false,
    corrupt: true,
    file: path.join(dir, 'gate-bad.json'),
  })
})

test('add-or-rename predicate detects committed and untracked new inputs', (t) => {
  const root = fixture(t)
  const base = git(root, ['rev-parse', 'HEAD'])
  assert.equal(branchAddsOrRenames(root, base), false)
  fs.writeFileSync(path.join(root, 'added.txt'), 'added\n')
  assert.equal(branchAddsOrRenames(root, base), true)
  git(root, ['add', 'added.txt'])
  git(root, ['commit', '-m', 'add file'])
  assert.equal(branchAddsOrRenames(root, base), true)
})

test('add-or-rename predicate ignores modify-only changes', (t) => {
  const root = fixture(t)
  const base = git(root, ['rev-parse', 'HEAD'])
  fs.writeFileSync(path.join(root, 'file.txt'), 'changed\n')
  git(root, ['add', 'file.txt'])
  git(root, ['commit', '-m', 'modify file'])
  assert.equal(branchAddsOrRenames(root, base), false)
})

test('normalizeGateSite extracts make target through env prefixes', () => {
  assert.equal(normalizeGateSite('make test-bossd'), 'test-bossd')
  assert.equal(
    normalizeGateSite("GO_TEST_PACKAGES='./internal/session' make test-bossd"),
    'test-bossd',
  )
})

test('eligibleGate is opt-in', () => {
  const config = {
    gateCache: {
      eligible: {
        'test-bossd': { cacheable: true, reason: 'whole tree hashed' },
        'test-race': { cacheable: false, reason: 'nondeterministic' },
      },
    },
  }
  assert.equal(eligibleGate(config, 'make test-bossd').eligible, true)
  assert.equal(eligibleGate(config, 'make test-race').eligible, false)
  assert.equal(eligibleGate(config, 'make test-web-e2e').eligible, false)
})

test('eligibleGate refuses a command that forces an uncached run (BOS-1339)', () => {
  // `normalizeGateSite` reduces every row below to an eligible make target, so only the
  // token-anchored env assignment can decide - a readiness receipt must never be a stamp hit.
  const config = {
    gateCache: {
      eligible: {
        'test-full': { cacheable: true, reason: 'iterative full decisions' },
        'test-boss': { cacheable: true, reason: 'module target' },
      },
    },
  }
  const refused = [
    'BOSS_GATE_FORCE_UNCACHED=1 make test-full',
    'FOO=1 BOSS_GATE_FORCE_UNCACHED=1 make test-boss',
    "BOSS_GATE_FORCE_UNCACHED='1' make test-full",
  ]
  for (const site of refused) {
    const verdict = eligibleGate(config, site)
    assert.equal(verdict.eligible, false, site)
    assert.match(verdict.reason, /forces an uncached run; never served from a stamp/, site)
  }
  const accepted = [
    'make test-full',
    'BOSS_GATE_FORCE_UNCACHED=0 make test-full',
    'XBOSS_GATE_FORCE_UNCACHED=1 make test-full',
    'BOSS_GATE_FORCE_UNCACHED=10 make test-full',
  ]
  for (const site of accepted) {
    assert.equal(eligibleGate(config, site).eligible, true, site)
  }
  // A caller that names the site separately still cannot smuggle the forced command past it.
  const split = eligibleGate(config, 'test-full', 'BOSS_GATE_FORCE_UNCACHED=1 make test-full')
  assert.equal(split.eligible, false)
  assert.equal(split.site, 'test-full')
})

test('resolveBaseCommit uses the merge-base, not the base tip', (t) => {
  const root = fixture(t)
  const defaultBranch = git(root, ['branch', '--show-current'])
  git(root, ['checkout', '-b', 'feature'])
  fs.writeFileSync(path.join(root, 'file.txt'), 'feature\n')
  git(root, ['commit', '-am', 'feature'])
  git(root, ['checkout', defaultBranch])
  fs.writeFileSync(path.join(root, 'base.txt'), 'base\n')
  git(root, ['add', 'base.txt'])
  git(root, ['commit', '-m', 'base advance'])
  git(root, ['checkout', 'feature'])
  assert.equal(resolveBaseCommit(root, defaultBranch), git(root, ['rev-parse', 'HEAD^']))
})

test('expansionVersions includes make-expansion variables', () => {
  const versions = expansionVersions({
    RACE: '1',
    BOSS_NO_BAZEL: '1',
    BAZEL_TEST_FLAGS: '--test_output=errors',
    BAZEL_TEST_EXTRA_FLAGS: '--nocache_test_results',
    MAKEFLAGS: 'BOSS_NO_BAZEL=1',
    MAKEOVERRIDES: 'BOSS_NO_BAZEL=1',
  })
  assert.equal(versions.RACE, '1')
  assert.equal(versions.BOSS_NO_BAZEL, '1')
  assert.equal(versions.BAZEL_TEST_EXTRA_FLAGS, '--nocache_test_results')
  assert.equal(versions.MAKEOVERRIDES, 'BOSS_NO_BAZEL=1')
})
