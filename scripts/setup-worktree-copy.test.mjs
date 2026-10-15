import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const helper = path.join(repoRoot, 'scripts/setup-worktree-copy.mjs')

function fixture(t) {
  const root = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'setup-worktree-copy-')))
  t.after(() => fs.rmSync(root, { recursive: true, force: true }))
  const repo = path.join(root, 'repo')
  const worktree = path.join(root, 'worktree')
  fs.mkdirSync(repo)
  fs.mkdirSync(worktree)
  const sentinel = path.join(root, 'sentinel')
  fs.writeFileSync(sentinel, 'untouched')
  return { root, repo, worktree, sentinel }
}

function run(f, extra = []) {
  return spawnSync(
    process.execPath,
    [helper, '--repo-dir', f.repo, '--worktree-dir', f.worktree, '--env-keys', 'CC', ...extra],
    { encoding: 'utf8' },
  )
}

function refused(f, result, name) {
  assert.equal(result.status, 1, result.stderr)
  assert.match(result.stderr, /symlink/)
  assert.ok(result.stderr.includes(name), result.stderr)
  assert.equal(fs.readFileSync(f.sentinel, 'utf8'), 'untouched')
}

test('copies regular local files and recursive config, filtering .env with mode 0600', (t) => {
  const f = fixture(t)
  fs.writeFileSync(path.join(f.repo, '.env'), 'CC=clang\nSECRET=must-stay-local\n')
  fs.writeFileSync(path.join(f.repo, '.node-version'), '24\n')
  fs.writeFileSync(path.join(f.repo, '.bazelrc.user'), 'cache\n')
  fs.mkdirSync(path.join(f.repo, '.config/nested'), { recursive: true })
  fs.writeFileSync(path.join(f.repo, '.config/nested/tool'), 'config')
  fs.mkdirSync(path.join(f.repo, '.compound-engineering'))
  fs.writeFileSync(path.join(f.repo, '.compound-engineering/config.local.yaml'), 'setting: true')
  const result = run(f)
  assert.equal(result.status, 0, result.stderr)
  for (const name of [
    '.node-version',
    '.bazelrc.user',
    '.config/nested/tool',
    '.compound-engineering/config.local.yaml',
  ]) {
    assert.equal(
      fs.readFileSync(path.join(f.worktree, name), 'utf8'),
      fs.readFileSync(path.join(f.repo, name), 'utf8'),
    )
  }
  assert.equal(fs.readFileSync(path.join(f.worktree, '.env'), 'utf8'), 'CC=clang\n')
  assert.equal(fs.statSync(path.join(f.worktree, '.env')).mode & 0o777, 0o600)
  assert.ok(!result.stdout.includes('must-stay-local'))
})

test('missing sources are skipped without removing destination files', (t) => {
  const f = fixture(t)
  fs.writeFileSync(path.join(f.worktree, '.node-version'), 'existing')
  const result = run(f)
  assert.equal(result.status, 0, result.stderr)
  assert.equal(fs.readFileSync(path.join(f.worktree, '.node-version'), 'utf8'), 'existing')
  assert.equal(fs.existsSync(path.join(f.worktree, '.env')), false)
})

for (const name of ['.env', '.bazelrc.user', '.node-version']) {
  test(`refuses destination symlink ${name} without changing its target`, (t) => {
    const f = fixture(t)
    fs.writeFileSync(path.join(f.repo, name), 'CC=clang\n')
    fs.symlinkSync(f.sentinel, path.join(f.worktree, name))
    refused(f, run(f), name)
  })
}

test('refuses source symlinks, including dangling ones', (t) => {
  const f = fixture(t)
  fs.symlinkSync(f.sentinel, path.join(f.repo, '.env'))
  refused(f, run(f), '.env')
  fs.unlinkSync(path.join(f.repo, '.env'))
  fs.symlinkSync(path.join(f.root, 'missing'), path.join(f.repo, '.env'))
  refused(f, run(f), '.env')
})

test('refuses a symlink in the source root ancestors', (t) => {
  const f = fixture(t)
  fs.writeFileSync(path.join(f.repo, '.node-version'), '24')
  const alias = path.join(f.root, 'alias')
  fs.symlinkSync(f.root, alias)
  refused(f, run({ ...f, repo: path.join(alias, 'repo') }), 'alias')
})

test('refuses symlinked worktree root', (t) => {
  const f = fixture(t)
  fs.writeFileSync(path.join(f.repo, '.node-version'), '24')
  const alias = path.join(f.root, 'alias')
  fs.symlinkSync(f.worktree, alias)
  refused(f, run({ ...f, worktree: alias }), 'alias')
})

for (const side of ['repo', 'worktree']) {
  test(`refuses ${side} ancestor symlink for compound config`, (t) => {
    const f = fixture(t)
    const configDir = path.join(f.root, 'config')
    fs.mkdirSync(configDir)
    fs.writeFileSync(path.join(configDir, 'config.local.yaml'), 'config')
    fs.symlinkSync(configDir, path.join(f[side], '.compound-engineering'))
    if (side === 'worktree') {
      fs.mkdirSync(path.join(f.repo, '.compound-engineering'))
      fs.writeFileSync(path.join(f.repo, '.compound-engineering/config.local.yaml'), 'new')
    }
    refused(f, run(f), '.compound-engineering')
    assert.equal(fs.readFileSync(path.join(configDir, 'config.local.yaml'), 'utf8'), 'config')
  })
}

for (const side of ['repo', 'worktree']) {
  test(`refuses recursive .config ${side} symlink before copying`, (t) => {
    const f = fixture(t)
    fs.mkdirSync(path.join(f.repo, '.config/nested'), { recursive: true })
    fs.mkdirSync(path.join(f.worktree, '.config/nested'), { recursive: true })
    fs.writeFileSync(path.join(f.repo, '.config/safe'), 'safe')
    if (side === 'worktree')
      fs.writeFileSync(path.join(f.repo, '.config/nested/link'), 'replacement')
    fs.symlinkSync(f.sentinel, path.join(f[side], '.config/nested/link'))
    refused(f, run(f), '.config/nested/link')
    assert.equal(fs.existsSync(path.join(f.worktree, '.config/safe')), false)
  })
}

test('CLI rejects unknown flags', (t) => {
  const result = run(fixture(t), ['--unknown'])
  assert.equal(result.status, 2, result.stderr)
})

function runMake(f) {
  const bin = path.join(f.root, 'bin')
  fs.mkdirSync(bin)
  for (const command of ['pnpm', 'direnv']) {
    fs.writeFileSync(path.join(bin, command), '#!/bin/sh\nexit 0\n', { mode: 0o755 })
  }
  fs.mkdirSync(path.join(f.worktree, 'scripts'))
  fs.mkdirSync(path.join(f.worktree, 'skills-toolbox'))
  for (const name of ['setup-worktree-copy.mjs', 'write-worktree-env.mjs']) {
    if (fs.existsSync(path.join(repoRoot, 'scripts', name)))
      fs.copyFileSync(path.join(repoRoot, 'scripts', name), path.join(f.worktree, 'scripts', name))
  }
  fs.copyFileSync(
    path.join(repoRoot, 'skills-toolbox/main-module.mjs'),
    path.join(f.worktree, 'skills-toolbox/main-module.mjs'),
  )
  return spawnSync('make', ['-f', path.join(repoRoot, 'Makefile'), 'setup-worktree'], {
    cwd: repoRoot,
    env: {
      ...process.env,
      REPO_DIR: f.repo,
      WORKTREE_DIR: f.worktree,
      PATH: `${bin}:${process.env.PATH}`,
    },
    encoding: 'utf8',
  })
}

test('make setup-worktree refuses a symlinked cache destination in a scratch worktree', (t) => {
  const f = fixture(t)
  fs.writeFileSync(path.join(f.repo, '.bazelrc.user'), 'replacement')
  fs.symlinkSync(f.sentinel, path.join(f.worktree, '.bazelrc.user'))
  const result = runMake(f)
  assert.notEqual(result.status, 0, result.stdout + result.stderr)
  assert.equal(fs.readFileSync(f.sentinel, 'utf8'), 'untouched')
})

test('make setup-worktree copies safe files in a scratch worktree', (t) => {
  const f = fixture(t)
  fs.writeFileSync(path.join(f.repo, '.env'), 'CC=clang\nSECRET=must-stay-local\n')
  const result = runMake(f)
  assert.equal(result.status, 0, result.stdout + result.stderr)
  assert.equal(fs.readFileSync(path.join(f.worktree, '.env'), 'utf8'), 'CC=clang\n')
  assert.match(result.stdout, /Installing JS deps/)
})
