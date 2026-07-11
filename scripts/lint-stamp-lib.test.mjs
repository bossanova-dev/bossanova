import { test } from 'node:test'
import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { moduleLintKey, hashTree } from './lint-stamp-lib.mjs'

const base = () => ({
  treeHashes: { 'lib/bossalib': 'aaa', 'services/mcp': 'bbb' },
  moduleDir: 'services/mcp',
  deps: ['lib/bossalib'],
  configHash: 'ccc',
  versions: { golangci: 'v2.11.4', go: 'go1.25.8' },
})

// Deterministic: same inputs → same key.
test('moduleLintKey is deterministic', () => {
  assert.equal(moduleLintKey(base()), moduleLintKey(base()))
})

// The key is a hex sha256.
test('moduleLintKey returns a hex sha256', () => {
  assert.match(moduleLintKey(base()), /^[0-9a-f]{64}$/)
})

// Any ingredient change changes the key.
test('key changes when the module’s own tree hash changes', () => {
  const b = base()
  assert.notEqual(
    moduleLintKey(b),
    moduleLintKey({
      ...b,
      treeHashes: { ...b.treeHashes, 'services/mcp': 'CHANGED' },
    }),
  )
})

test('key changes when a dependency tree hash changes', () => {
  const b = base()
  assert.notEqual(
    moduleLintKey(b),
    moduleLintKey({
      ...b,
      treeHashes: { ...b.treeHashes, 'lib/bossalib': 'CHANGED' },
    }),
  )
})

test('key changes when configHash changes', () => {
  const b = base()
  assert.notEqual(moduleLintKey(b), moduleLintKey({ ...b, configHash: 'CHANGED' }))
})

test('key changes when the golangci version changes', () => {
  const b = base()
  assert.notEqual(
    moduleLintKey(b),
    moduleLintKey({ ...b, versions: { ...b.versions, golangci: 'v2.12.0' } }),
  )
})

test('key changes when the go version changes', () => {
  const b = base()
  assert.notEqual(
    moduleLintKey(b),
    moduleLintKey({ ...b, versions: { ...b.versions, go: 'go1.26.0' } }),
  )
})

// The whole versions object is folded in, so a Go build-environment fingerprint
// (GOOS/GOARCH/build tags/workspace mode) — which selects which build-constrained
// files golangci-lint analyzes — changes the key. Guards against a host-default
// stamp being reused for a different-GOOS `make lint`.
test('key changes when the build-env fingerprint changes', () => {
  const b = base()
  const withEnv = moduleLintKey({
    ...b,
    versions: { ...b.versions, buildEnv: 'GOOS=darwin;GOARCH=arm64;GOWORK=on' },
  })
  const withOtherEnv = moduleLintKey({
    ...b,
    versions: { ...b.versions, buildEnv: 'GOOS=linux;GOARCH=amd64;GOWORK=on' },
  })
  assert.notEqual(withEnv, withOtherEnv)
  // Adding the fingerprint at all must change the key vs. omitting it.
  assert.notEqual(moduleLintKey(b), withEnv)
})

// A dependency's tree hash must actually feed the key: two modules that share
// the same own-tree hash but differ only in a dep's hash must differ.
test('dep hashes are folded in, not ignored', () => {
  const withDep = moduleLintKey({
    treeHashes: { 'lib/bossalib': 'dep1', 'services/mcp': 'same' },
    moduleDir: 'services/mcp',
    deps: ['lib/bossalib'],
    configHash: 'ccc',
    versions: { golangci: 'v2.11.4', go: 'go1.25.8' },
  })
  const withOtherDep = moduleLintKey({
    treeHashes: { 'lib/bossalib': 'dep2', 'services/mcp': 'same' },
    moduleDir: 'services/mcp',
    deps: ['lib/bossalib'],
    configHash: 'ccc',
    versions: { golangci: 'v2.11.4', go: 'go1.25.8' },
  })
  assert.notEqual(withDep, withOtherDep)
})

// Dep ordering must be canonical so key is stable regardless of caller order.
test('dep order does not affect the key', () => {
  const args = (deps) => ({
    treeHashes: { a: '1', b: '2', 'services/x': '3' },
    moduleDir: 'services/x',
    deps,
    configHash: 'ccc',
    versions: { golangci: 'v2.11.4', go: 'go1.25.8' },
  })
  assert.equal(moduleLintKey(args(['a', 'b'])), moduleLintKey(args(['b', 'a'])))
})

// --- hashTree, against a hermetic temp git repo ---

function initRepo() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lint-stamp-tree-'))
  const git = (...args) => execFileSync('git', args, { cwd: root, encoding: 'utf8' })
  git('init', '-q')
  git('config', 'user.email', 'test@example.com')
  git('config', 'user.name', 'Test')
  fs.mkdirSync(path.join(root, 'mod'))
  fs.writeFileSync(path.join(root, 'mod', 'a.go'), 'package mod\n')
  git('add', '-A')
  git('commit', '-q', '-m', 'init')
  return { root, git }
}

test('hashTree is deterministic for a clean tree', () => {
  const { root } = initRepo()
  assert.equal(hashTree(root, 'mod'), hashTree(root, 'mod'))
})

test('hashTree changes when a tracked file is edited (dirty overlay)', () => {
  const { root } = initRepo()
  const clean = hashTree(root, 'mod')
  fs.appendFileSync(path.join(root, 'mod', 'a.go'), '\n// edit\n')
  assert.notEqual(clean, hashTree(root, 'mod'))
})

test('hashTree changes when an untracked file is added', () => {
  const { root } = initRepo()
  const clean = hashTree(root, 'mod')
  fs.writeFileSync(path.join(root, 'mod', 'b.go'), 'package mod\n')
  assert.notEqual(clean, hashTree(root, 'mod'))
})

// Under-invalidation guard: a brand-new fully-untracked subdirectory collapses
// to a single `?? mod/pkg/` porcelain entry under the default -unormal mode. The
// hash MUST fold in the content of the .go files inside it (golangci-lint lints
// untracked files), so adding a second file — or editing one — inside that
// untracked package must change the tree hash. This fails if the overlay records
// only the collapsed directory path (tombstone) instead of per-file content.
test('hashTree folds content of files inside an untracked subdirectory', () => {
  const { root } = initRepo()
  fs.mkdirSync(path.join(root, 'mod', 'pkg'))
  fs.writeFileSync(path.join(root, 'mod', 'pkg', 'x.go'), 'package pkg\n')
  const withOne = hashTree(root, 'mod')
  // Add a second untracked file in the same untracked subdir: findings would
  // change, so the hash must change too.
  fs.writeFileSync(path.join(root, 'mod', 'pkg', 'y.go'), 'package pkg\nvar Unused int\n')
  const withTwo = hashTree(root, 'mod')
  assert.notEqual(withOne, withTwo)
  // Editing a file already inside the untracked subdir must also change the hash.
  fs.appendFileSync(path.join(root, 'mod', 'pkg', 'x.go'), '\n// edit\n')
  assert.notEqual(withTwo, hashTree(root, 'mod'))
})

// Under-invalidation guard: git's default porcelain octal-escapes and quotes
// non-ASCII paths (core.quotePath), which would resolve to the wrong path and
// tombstone a real file. The -z parse reads raw bytes, so a non-ASCII-named
// file's content is genuinely folded in — editing it changes the hash.
test('hashTree folds content of a non-ASCII (quotePath) filename', () => {
  const { root } = initRepo()
  const unicodeName = 'café.go'
  fs.writeFileSync(path.join(root, 'mod', unicodeName), 'package mod\n')
  const withFile = hashTree(root, 'mod')
  fs.appendFileSync(path.join(root, 'mod', unicodeName), 'var Unused int\n')
  assert.notEqual(withFile, hashTree(root, 'mod'))
})

// A staged rename emits a second NUL field (the original path) under -z; the
// parser must consume it (not treat it as a separate changed path) and still
// reflect the rename in the hash.
test('hashTree handles a staged rename', () => {
  const { root, git } = initRepo()
  const before = hashTree(root, 'mod')
  git('mv', 'mod/a.go', 'mod/renamed.go')
  assert.notEqual(before, hashTree(root, 'mod'))
})

test('hashTree isolates directories', () => {
  const { root, git } = initRepo()
  fs.mkdirSync(path.join(root, 'other'))
  fs.writeFileSync(path.join(root, 'other', 'c.go'), 'package other\n')
  git('add', '-A')
  git('commit', '-q', '-m', 'other')
  const before = hashTree(root, 'mod')
  fs.appendFileSync(path.join(root, 'other', 'c.go'), '\n// unrelated\n')
  // Editing a sibling directory must not change mod's tree hash.
  assert.equal(before, hashTree(root, 'mod'))
})
