import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import {
  discoverModules,
  deriveDeps,
  configHashOf,
  isModuleCached,
  gcStamps,
} from './lint-affected.mjs'

function scaffold() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lint-affected-'))
  const write = (rel, contents) => {
    fs.mkdirSync(path.join(root, path.dirname(rel)), { recursive: true })
    fs.writeFileSync(path.join(root, rel), contents)
  }
  write('lib/bossalib/go.mod', 'module github.com/recurser/bossalib\n')
  write(
    'services/boss/go.mod',
    'module x\n\nrequire github.com/recurser/bossalib v0.0.0\n\nreplace github.com/recurser/bossalib => ../../lib/bossalib\n',
  )
  write(
    'plugins/bossd-plugin-claude/go.mod',
    'module y\n\nreplace github.com/recurser/bossalib => ../../lib/bossalib\n',
  )
  // A directory without a go.mod must be ignored.
  fs.mkdirSync(path.join(root, 'services/notamodule'), { recursive: true })
  return root
}

test('discoverModules finds lib/services/plugins go.mod dirs, sorted', () => {
  const root = scaffold()
  assert.deepEqual(discoverModules(root), [
    'lib/bossalib',
    'plugins/bossd-plugin-claude',
    'services/boss',
  ])
})

test('deriveDeps returns lib/bossalib for a module that replaces it', () => {
  const root = scaffold()
  assert.deepEqual(deriveDeps(root, 'services/boss'), ['lib/bossalib'])
  assert.deepEqual(deriveDeps(root, 'plugins/bossd-plugin-claude'), ['lib/bossalib'])
})

test('deriveDeps returns no deps for bossalib itself', () => {
  const root = scaffold()
  assert.deepEqual(deriveDeps(root, 'lib/bossalib'), [])
})

test('deriveDeps returns no deps when go.mod does not replace bossalib', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lint-affected-nodep-'))
  fs.mkdirSync(path.join(root, 'services/lonely'), { recursive: true })
  fs.writeFileSync(path.join(root, 'services/lonely/go.mod'), 'module lonely\n')
  assert.deepEqual(deriveDeps(root, 'services/lonely'), [])
})

// Under-invalidation guard: a module that locally `replace`s a SECOND module
// (beyond bossalib) — as services/bosso replaces ../bossd — is type-checked
// against that module's local source in workspace mode, so its findings depend
// on it. deriveDeps must return the transitive closure of local replaces, not
// just lib/bossalib, or an edit to the depended-on module leaves this one
// reporting a stale `(cached)`.
test('deriveDeps returns the transitive closure of local replaces', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lint-affected-transitive-'))
  const write = (rel, contents) => {
    fs.mkdirSync(path.join(root, path.dirname(rel)), { recursive: true })
    fs.writeFileSync(path.join(root, rel), contents)
  }
  write('lib/bossalib/go.mod', 'module github.com/recurser/bossalib\n')
  // bossd → bossalib
  write(
    'services/bossd/go.mod',
    'module github.com/recurser/bossd\n\nreplace github.com/recurser/bossalib => ../../lib/bossalib\n',
  )
  // bosso → bossd (and → bossalib directly)
  write(
    'services/bosso/go.mod',
    'module github.com/recurser/bosso\n\n' +
      'replace github.com/recurser/bossalib => ../../lib/bossalib\n' +
      'replace github.com/recurser/bossd => ../bossd\n',
  )
  // bosso transitively depends on both bossd and bossalib.
  assert.deepEqual(deriveDeps(root, 'services/bosso'), ['lib/bossalib', 'services/bossd'])
  // bossd depends only on bossalib.
  assert.deepEqual(deriveDeps(root, 'services/bossd'), ['lib/bossalib'])
})

// A versioned single-line replace and a trailing // comment must still parse.
test('deriveDeps parses versioned and commented local replaces', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lint-affected-versioned-'))
  fs.mkdirSync(path.join(root, 'lib/bossalib'), { recursive: true })
  fs.writeFileSync(path.join(root, 'lib/bossalib/go.mod'), 'module github.com/recurser/bossalib\n')
  fs.mkdirSync(path.join(root, 'services/x'), { recursive: true })
  fs.writeFileSync(
    path.join(root, 'services/x/go.mod'),
    'module x\n\nreplace github.com/recurser/bossalib v0.0.0 => ../../lib/bossalib v1.2.3 // pin\n',
  )
  assert.deepEqual(deriveDeps(root, 'services/x'), ['lib/bossalib'])
})

// Under-invalidation guard: lint runs in Go workspace mode, so a go.work /
// go.work.sum edit (e.g. bumping a workspace-only `replace`) changes the versions
// golangci-lint type-checks against and therefore its findings. The global config
// hash must fold in both workspace files so such an edit invalidates every
// module's stamp, not just .golangci.yml.
test('configHashOf folds in .golangci.yml, go.work, and go.work.sum', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lint-affected-confighash-'))
  fs.writeFileSync(path.join(root, '.golangci.yml'), 'linters:\n  enable: [staticcheck]\n')
  fs.writeFileSync(path.join(root, 'go.work'), 'go 1.25\n\nuse ./services/mcp\n')
  fs.writeFileSync(path.join(root, 'go.work.sum'), 'example.com/x v1.0.0 h1:aaa\n')
  const baseline = configHashOf(root)

  // Deterministic for identical inputs.
  assert.equal(baseline, configHashOf(root))

  // A go.work edit (workspace replace bump) must change the hash.
  fs.writeFileSync(
    root + '/go.work',
    'go 1.25\n\nreplace foo => bar v1.2.3\n\nuse ./services/mcp\n',
  )
  const afterWork = configHashOf(root)
  assert.notEqual(baseline, afterWork)

  // A go.work.sum change must change the hash.
  fs.writeFileSync(path.join(root, 'go.work.sum'), 'example.com/x v1.0.1 h1:bbb\n')
  assert.notEqual(afterWork, configHashOf(root))

  // A .golangci.yml change must still change the hash.
  const beforeConfig = configHashOf(root)
  fs.writeFileSync(path.join(root, '.golangci.yml'), 'linters:\n  enable: [staticcheck, gosec]\n')
  assert.notEqual(beforeConfig, configHashOf(root))
})

// An explicit GOWORK file selects workspace directives outside the repository
// root. Its contents (and adjacent go.work.sum) affect type checking, so they
// must invalidate lint stamps even though the cache key deliberately omits the
// machine-specific workspace path.
test('configHashOf folds in an explicit GOWORK file and its sum', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'lint-affected-explicit-gowork-root-'))
  const workspaceDir = fs.mkdtempSync(path.join(os.tmpdir(), 'lint-affected-explicit-gowork-'))
  const workspaceFile = path.join(workspaceDir, 'workspace.work')
  fs.writeFileSync(path.join(root, '.golangci.yml'), 'linters:\n  enable: [staticcheck]\n')
  fs.writeFileSync(workspaceFile, 'go 1.25\n\nuse ./services/mcp\n')
  fs.writeFileSync(path.join(workspaceDir, 'go.work.sum'), 'example.com/x v1.0.0 h1:aaa\n')
  const baseline = configHashOf(root, workspaceFile)

  fs.appendFileSync(workspaceFile, '\nreplace example.com/x => example.com/x v1.0.1\n')
  const afterWork = configHashOf(root, workspaceFile)
  assert.notEqual(baseline, afterWork)

  fs.writeFileSync(path.join(workspaceDir, 'go.work.sum'), 'example.com/x v1.0.1 h1:bbb\n')
  assert.notEqual(afterWork, configHashOf(root, workspaceFile))
})

// The cache-skip gate must fail open: only an enabled + unforced + present stamp
// yields a skip; every other combination routes to a real lint.
test('isModuleCached gates on enabled + !force + stamp presence', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lint-affected-cached-'))
  const present = path.join(dir, 'present')
  fs.writeFileSync(present, 'x')
  const missing = path.join(dir, 'missing')
  assert.equal(isModuleCached({ stampsEnabled: true, force: false, stampPath: present }), true)
  assert.equal(isModuleCached({ stampsEnabled: true, force: true, stampPath: present }), false)
  assert.equal(isModuleCached({ stampsEnabled: false, force: false, stampPath: present }), false)
  assert.equal(isModuleCached({ stampsEnabled: true, force: false, stampPath: missing }), false)
})

// GC drops stamps past the 30-day TTL, keeps fresh ones, and no-ops on a missing
// dir (fail-open bounded growth).
test('gcStamps deletes only stale stamps and tolerates a missing dir', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lint-affected-gc-'))
  const stale = path.join(dir, 'stale')
  const fresh = path.join(dir, 'fresh')
  fs.writeFileSync(stale, 'x')
  fs.writeFileSync(fresh, 'x')
  const old = Date.now() / 1000 - 31 * 24 * 60 * 60 // 31 days ago, in seconds
  fs.utimesSync(stale, old, old)
  gcStamps(dir)
  assert.equal(fs.existsSync(stale), false)
  assert.equal(fs.existsSync(fresh), true)
  // Missing dir must not throw.
  gcStamps(path.join(dir, 'does-not-exist'))
})
