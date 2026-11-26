#!/usr/bin/env node
import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import { copyFileSync, mkdirSync, readFileSync, realpathSync, writeFileSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import { globToRegExp } from '../../skills-toolbox/skill-config.mjs'
import { tempDir } from '../test-infra/test-tmpdir.mjs'

const HERE = dirname(fileURLToPath(import.meta.url))
const ROOT = resolve(HERE, '../..')
const SCRIPT = join(HERE, 'git-merge-drivers.mjs')
const GEN = 'services/docs/openapi/bossanova.v1.openapi.yaml'

// No global or system config, so a driver defined on the host cannot mask the fixture's conflict.
const env = { ...process.env, GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_NOSYSTEM: '1' }

function git(cwd, ...args) {
  return execFileSync('git', args, {
    cwd,
    env,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
  })
}

const tryGit = (cwd, ...args) =>
  spawnSync('git', args, { cwd, encoding: 'utf8', env: { ...env, GIT_EDITOR: 'true' } })

const install = (repo) =>
  execFileSync(process.execPath, [SCRIPT, 'install', '--repo', repo], { env, encoding: 'utf8' })

// The generator under test: one source file becomes one base64 descriptor line, the shape
// protoc-gen-es writes for a proto file.
const descriptor = (source) => `export const file = "${Buffer.from(source).toString('base64')}";\n`

function regenerate(repo) {
  const source = readFileSync(join(repo, 'schema.txt'), 'utf8')
  mkdirSync(dirname(join(repo, GEN)), { recursive: true })
  writeFileSync(join(repo, GEN), descriptor(source))
}

function commitSchema(repo, source, message) {
  writeFileSync(join(repo, 'schema.txt'), source)
  regenerate(repo)
  git(repo, 'add', '-A')
  git(repo, 'commit', '-q', '-m', message)
}

/** A repo with the real .gitattributes, a schema on main, and two branches that each extend it. */
function fixture(t) {
  const repo = realpathSync(tempDir('git-merge-drivers-', t))
  git(repo, 'init', '-q', '-b', 'main')
  git(repo, 'config', 'user.email', 'test@example.com')
  git(repo, 'config', 'user.name', 'Test')
  git(repo, 'config', 'commit.gpgsign', 'false')
  copyFileSync(join(ROOT, '.gitattributes'), join(repo, '.gitattributes'))
  commitSchema(repo, 'a\nb\nc\n', 'init')
  git(repo, 'checkout', '-q', '-b', 'one')
  commitSchema(repo, 'a\none\nb\nc\n', 'one')
  git(repo, 'checkout', '-q', '-b', 'two', 'main')
  commitSchema(repo, 'a\nb\nc\ntwo\n', 'two')
  git(repo, 'checkout', '-q', 'main')
  git(repo, 'merge', '-q', '--ff-only', 'one')
  git(repo, 'checkout', '-q', 'two')
  return repo
}

test('without the driver the two descriptor edits conflict', (t) => {
  const repo = fixture(t)
  const rebase = tryGit(repo, 'rebase', 'main')
  assert.notEqual(rebase.status, 0, 'rebase stops on the descriptor line')
  assert.match(git(repo, 'diff', '--name-only', '--diff-filter=U'), /bossanova\.v1\.openapi\.yaml/)
})

test('with the driver installed the rebase is clean and regeneration merges both sides', (t) => {
  const repo = fixture(t)
  install(repo)
  const rebase = tryGit(repo, 'rebase', 'main')
  assert.equal(rebase.status, 0, rebase.stderr)
  assert.equal(git(repo, 'status', '--porcelain'), '')
  assert.equal(
    readFileSync(join(repo, GEN), 'utf8'),
    descriptor('a\none\nb\nc\n'),
    'base side kept',
  )
  regenerate(repo)
  assert.equal(readFileSync(join(repo, GEN), 'utf8'), descriptor('a\none\nb\nc\ntwo\n'))
})

test('install is idempotent, reaches every worktree and leaves the stash alone', (t) => {
  const repo = fixture(t)
  writeFileSync(join(repo, 'schema.txt'), 'stashed\n')
  git(repo, 'stash', 'push', '-q', '-m', 'keep-me')
  const stashBefore = git(repo, 'stash', 'list', '--format=%H')
  const worktree = join(realpathSync(tempDir('git-merge-drivers-wt-', t)), 'wt')
  git(repo, 'worktree', 'add', '-q', '--detach', worktree)
  install(worktree)
  install(worktree)
  install(repo)
  assert.equal(git(repo, 'config', '--get-all', 'merge.generated.driver').trim(), 'true')
  assert.equal(git(worktree, 'config', '--get', 'merge.generated.driver').trim(), 'true')
  assert.equal(git(repo, 'stash', 'list', '--format=%H'), stashBefore)
})

test('every generated path carries the driver, and nothing else does', () => {
  const config = JSON.parse(readFileSync(join(ROOT, '.boss-skills.json'), 'utf8'))
  const generated = config.verifyDefaults.generatedPaths.map(globToRegExp)
  assert.ok(generated.length > 0)
  const files = git(ROOT, 'ls-files', '-z').split('\0').filter(Boolean)
  const attrs = spawnSync('git', ['check-attr', '--stdin', '-z', 'merge'], {
    cwd: ROOT,
    input: files.join('\0'),
    encoding: 'utf8',
    maxBuffer: 256 * 1024 * 1024,
  })
  assert.equal(attrs.status, 0, attrs.stderr)
  const fields = attrs.stdout.split('\0')
  const driven = new Set()
  for (let i = 0; i + 2 < fields.length; i += 3)
    if (fields[i + 2] === 'generated') driven.add(fields[i])
  const expected = files.filter((path) => generated.some((re) => re.test(path)))
  assert.ok(expected.includes(GEN))
  assert.deepEqual([...driven].sort(), expected.sort())
})
