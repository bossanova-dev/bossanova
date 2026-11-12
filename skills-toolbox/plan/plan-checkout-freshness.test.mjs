import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { checkoutFreshness } from './plan-checkout-freshness.mjs'

const helper = fileURLToPath(new URL('./plan-checkout-freshness.mjs', import.meta.url))
function git(repo, ...args) {
  const r = spawnSync('git', ['-C', repo, ...args], { encoding: 'utf8' })
  assert.equal(r.status, 0, r.stderr)
  return r.stdout.trim()
}
function commit(repo, name, text) {
  fs.writeFileSync(path.join(repo, name), text)
  git(repo, 'add', name)
  git(repo, 'commit', '-qm', 'fixture change')
  return git(repo, 'rev-parse', 'HEAD')
}
function world(t, behind = true) {
  const root = tempDir('plan-freshness-', t)
  git(root, 'init', '--bare', '--initial-branch=main', 'remote.git')
  const repo = path.join(root, 'repo')
  const peer = path.join(root, 'peer')
  git(root, 'clone', '--no-local', 'remote.git', repo)
  for (const dir of [repo]) configure(dir)
  const oldHead = commit(repo, 'file.txt', 'original\n')
  git(repo, 'push', 'origin', 'main')
  git(repo, 'remote', 'set-head', 'origin', 'main')
  git(root, 'clone', '--no-local', 'remote.git', peer)
  configure(peer)
  if (behind) {
    commit(peer, 'file.txt', 'upstream\n')
    git(peer, 'push', 'origin', 'main')
  }
  return { root, repo, peer, oldHead }
}
function configure(repo) {
  git(repo, 'config', 'commit.gpgsign', 'false')
  git(repo, 'config', 'user.name', 'Fixture')
  git(repo, 'config', 'user.email', 'fixture@example.test')
}
function check(repo, extra = []) {
  const r = spawnSync(process.execPath, [helper, 'check', '--repo', repo, ...extra], {
    encoding: 'utf8',
  })
  assert.equal(r.status, 0, r.stderr)
  assert.equal(r.stdout.trim().split('\n').length, 1)
  return JSON.parse(r.stdout)
}
test('at tip reports fresh and preserves HEAD', (t) => {
  const w = world(t, false)
  const r = check(w.repo)
  assert.equal(r.verdict, 'fresh')
  assert.equal(r.behind, 0)
  assert.equal(r.reconRef, 'HEAD')
  assert.equal(git(w.repo, 'rev-parse', 'HEAD'), w.oldHead)
})
test('clean base with untracked files fast-forwards', (t) => {
  const w = world(t)
  fs.writeFileSync(path.join(w.repo, 'untracked.txt'), 'keep')
  const r = check(w.repo)
  assert.equal(r.verdict, 'refreshed')
  assert.equal(r.behindBefore, 1)
  assert.equal(r.behind, 0)
  assert.equal(r.reconRef, 'HEAD')
  assert.equal(r.fastForward.outcome, 'fast-forwarded')
  assert.equal(git(w.repo, 'rev-parse', 'HEAD'), git(w.peer, 'rev-parse', 'HEAD'))
  assert.equal(fs.readFileSync(path.join(w.repo, 'untracked.txt'), 'utf8'), 'keep')
})
for (const reason of ['dirty', 'off-branch', 'detached', 'diverged', 'ff-failed']) {
  test(`behind checkout skips fast-forward when ${reason}`, (t) => {
    const w = world(t)
    if (reason === 'dirty') fs.writeFileSync(path.join(w.repo, 'file.txt'), 'local edits')
    if (reason === 'off-branch') git(w.repo, 'checkout', '-b', 'topic')
    if (reason === 'detached') git(w.repo, 'checkout', '--detach')
    if (reason === 'diverged') commit(w.repo, 'local.txt', 'local commit')
    if (reason === 'ff-failed') fs.writeFileSync(path.join(w.repo, '.git', 'index.lock'), '')
    const before = git(w.repo, 'rev-parse', 'HEAD')
    const r = check(w.repo)
    assert.equal(r.verdict, 'stale')
    assert.equal(r.fastForward.reason, reason)
    assert.equal(r.reconRef, 'origin/main')
    assert.ok(r.warning)
    assert.equal(git(w.repo, 'rev-parse', 'HEAD'), before)
    if (reason === 'dirty')
      assert.equal(fs.readFileSync(path.join(w.repo, 'file.txt'), 'utf8'), 'local edits')
  })
}
for (const behind of [false, true]) {
  test(`fetch failure is unevaluated with last-known behind=${behind}`, (t) => {
    const w = world(t, behind)
    git(w.repo, 'fetch', 'origin')
    git(w.repo, 'remote', 'set-url', 'origin', path.join(w.root, 'missing'))
    const r = check(w.repo)
    assert.equal(r.verdict, 'unevaluated')
    assert.equal(r.fetchFailed, true)
    assert.equal(r.reconRef, behind ? 'origin/main' : 'HEAD')
    assert.ok(r.warning)
    assert.equal(git(w.repo, 'rev-parse', 'HEAD'), w.oldHead)
  })
}
test('base requires a remote HEAD or explicit flag', (t) => {
  const w = world(t, false)
  git(w.repo, 'symbolic-ref', '--delete', 'refs/remotes/origin/HEAD')
  const r = check(w.repo)
  assert.equal(r.verdict, 'unevaluated')
  assert.equal(r.fastForward.reason, 'base-unresolved')
  assert.match(r.warning, /--base/)
  assert.equal(check(w.repo, ['--base', 'main']).verdict, 'fresh')
})
test('unreadable counts never permit a fast-forward or fresh verdict', (t) => {
  const w = world(t)
  const r = checkoutFreshness({
    repo: w.repo,
    base: 'main',
    run: (repo, args) => {
      if (args[0] === 'rev-list') return { status: 1, stdout: '', stderr: 'unreadable' }
      const r = spawnSync('git', args, { cwd: repo, encoding: 'utf8' })
      return { status: r.status ?? -1, stdout: r.stdout, stderr: r.stderr }
    },
  })
  assert.equal(r.verdict, 'unevaluated')
  assert.equal(r.fastForward.attempted, false)
  assert.ok(r.warning)
  assert.equal(git(w.repo, 'rev-parse', 'HEAD'), w.oldHead)
})
test('CLI help succeeds and invalid arguments fail with usage', () => {
  for (const [args, status] of [
    [['--help'], 0],
    [[], 2],
    [['check', '--unknown'], 2],
  ]) {
    const r = spawnSync(process.execPath, [helper, ...args], { encoding: 'utf8' })
    assert.equal(r.status, status)
    assert.match(r.stdout + r.stderr, /Usage:/)
  }
})
