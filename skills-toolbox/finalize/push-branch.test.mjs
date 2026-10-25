import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { pushBranch } from './push-branch.mjs'

const HERE = path.dirname(fileURLToPath(import.meta.url))
const noSleep = () => {}

function git(cwd, ...args) {
  return execFileSync('git', args, {
    cwd,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
  }).trim()
}

function commit(cwd, file, content) {
  fs.writeFileSync(path.join(cwd, file), content)
  git(cwd, 'add', file)
  git(cwd, 'commit', '-q', '-m', `edit ${file}`)
  return git(cwd, 'rev-parse', 'HEAD')
}

// A bare remote with `main`, plus a clone on `feature` branched from it.
function setup() {
  const root = tempDir('push-branch-')
  const remote = path.join(root, 'remote.git')
  const work = path.join(root, 'work')
  git(root, 'init', '-q', '--bare', '-b', 'main', remote)
  git(root, 'init', '-q', '-b', 'main', work)
  git(work, 'config', 'user.email', 't@example.com')
  git(work, 'config', 'user.name', 'T')
  git(work, 'config', 'commit.gpgsign', 'false')
  git(work, 'remote', 'add', 'origin', remote)
  commit(work, 'base.txt', 'base\n')
  git(work, 'push', '-q', 'origin', 'main')
  git(work, 'checkout', '-q', '-b', 'feature')
  return { root, remote, work }
}

// A second clone that pushes `file` onto the remote's `feature` branch.
function pushFromPeer(root, remote, file, content) {
  const peer = path.join(root, `peer-${file}`)
  git(root, 'clone', '-q', '-b', 'feature', remote, peer)
  git(peer, 'config', 'user.email', 'p@example.com')
  git(peer, 'config', 'user.name', 'P')
  git(peer, 'config', 'commit.gpgsign', 'false')
  commit(peer, file, content)
  git(peer, 'push', '-q', 'origin', 'feature')
  return git(peer, 'rev-parse', 'HEAD')
}

test('pushes a branch the remote does not have', () => {
  const { work, remote } = setup()
  const head = commit(work, 'a.txt', 'a\n')
  const res = pushBranch({ branch: 'feature', cwd: work, sleep: noSleep })
  assert.equal(res.pushed, 'yes')
  assert.equal(res.attempts, 1)
  assert.equal(git(remote, 'rev-parse', 'refs/heads/feature'), head)
})

test('reports yes without pushing when the remote already contains HEAD', () => {
  const { work, root, remote } = setup()
  commit(work, 'a.txt', 'a\n')
  git(work, 'push', '-q', '-u', 'origin', 'feature')
  const peerHead = pushFromPeer(root, remote, 'b.txt', 'b\n')
  const res = pushBranch({ branch: 'feature', cwd: work, sleep: noSleep })
  assert.equal(res.pushed, 'yes')
  assert.equal(res.attempts, 0)
  assert.equal(git(remote, 'rev-parse', 'refs/heads/feature'), peerHead)
})

test('rebases onto a remote that moved and keeps both sides', () => {
  const { work, root, remote } = setup()
  commit(work, 'a.txt', 'a\n')
  git(work, 'push', '-q', '-u', 'origin', 'feature')
  const peerHead = pushFromPeer(root, remote, 'b.txt', 'b\n')
  commit(work, 'c.txt', 'c\n')
  const res = pushBranch({ branch: 'feature', cwd: work, sleep: noSleep })
  assert.equal(res.pushed, 'yes')
  assert.equal(res.attempts, 2)
  assert.equal(res.refused, null)
  const tip = git(remote, 'rev-parse', 'refs/heads/feature')
  assert.equal(git(work, 'rev-parse', 'HEAD'), tip)
  git(work, 'merge-base', '--is-ancestor', peerHead, tip)
  assert.equal(git(work, 'rev-list', '--merges', '--count', 'HEAD'), '0')
})

test('falls back to a rescue ref when the branch cannot be reconciled', () => {
  const { work, root, remote } = setup()
  commit(work, 'a.txt', 'a\n')
  git(work, 'push', '-q', '-u', 'origin', 'feature')
  pushFromPeer(root, remote, 'a.txt', 'peer version\n')
  const head = commit(work, 'a.txt', 'local version\n')
  const res = pushBranch({ branch: 'feature', cwd: work, attempts: 2, sleep: noSleep })
  assert.equal(res.pushed, 'rescue')
  assert.equal(res.attempts, 2)
  assert.equal(res.rescue, `feature-blocked-${head.slice(0, 12)}`)
  assert.equal(git(remote, 'rev-parse', `refs/heads/${res.rescue}`), head)
  assert.equal(fs.existsSync(path.join(work, '.git', 'rebase-merge')), false)
})

test('reports no when the remote is unreachable', () => {
  const { work } = setup()
  commit(work, 'a.txt', 'a\n')
  git(work, 'remote', 'set-url', 'origin', path.join(os.tmpdir(), 'no-such-remote-push-branch.git'))
  let slept = 0
  const res = pushBranch({ branch: 'feature', cwd: work, attempts: 3, sleep: () => (slept += 1) })
  assert.equal(res.pushed, 'no')
  assert.equal(res.attempts, 3)
  assert.equal(res.rescue, null)
  assert.equal(slept, 2)
})

test('the CLI prints JSON and exits non-zero unless pushed', () => {
  const { work } = setup()
  commit(work, 'a.txt', 'a\n')
  const out = execFileSync(
    'node',
    [path.join(HERE, 'push-branch.mjs'), '--branch', 'feature', '--initial-delay', '0'],
    { cwd: work, encoding: 'utf8' },
  )
  assert.equal(JSON.parse(out).pushed, 'yes')
  let status = 0
  try {
    execFileSync('node', [path.join(HERE, 'push-branch.mjs'), '--bogus'], {
      cwd: work,
      stdio: 'ignore',
    })
  } catch (error) {
    status = error.status
  }
  assert.equal(status, 2)
})

function bootstrap(work) {
  git(work, 'commit', '-q', '--allow-empty', '-m', 'bootstrap')
  const tip = git(work, 'rev-parse', 'HEAD')
  git(work, 'push', '-q', 'origin', 'feature')
  git(work, 'reset', '--hard', 'HEAD^')
  return tip
}

function advanceBase({ root, remote, work }) {
  const peer = path.join(root, 'base-peer')
  git(root, 'clone', '-q', '-b', 'main', remote, peer)
  git(peer, 'config', 'user.email', 'p@example.com')
  git(peer, 'config', 'user.name', 'P')
  git(peer, 'config', 'commit.gpgsign', 'false')
  commit(peer, 'new-base.txt', 'new base\n')
  git(peer, 'push', '-q', 'origin', 'main')
  git(work, 'fetch', '-q', 'origin', 'main')
  git(work, 'rebase', 'origin/main')
}

function assertSupersededThenLease({ work, remote }, remoteTip) {
  const head = git(work, 'rev-parse', 'HEAD')
  const res = pushBranch({ branch: 'feature', base: 'origin/main', cwd: work, sleep: noSleep })
  assert.equal(res.pushed, 'no')
  assert.equal(res.refused, 'superseded')
  assert.equal(res.rescue, null)
  assert.equal(res.remoteTip, remoteTip)
  assert.equal(git(work, 'rev-parse', 'HEAD'), head)
  assert.equal(git(remote, 'rev-parse', 'refs/heads/feature'), remoteTip)
  assert.equal(fs.existsSync(path.join(work, '.git', 'rebase-merge')), false)
  const leased = pushBranch({
    branch: 'feature',
    expectRemote: remoteTip,
    cwd: work,
    sleep: noSleep,
  })
  assert.equal(leased.pushed, 'yes')
  assert.equal(leased.refused, null)
  assert.equal(leased.remoteTip, remoteTip)
  assert.equal(git(remote, 'rev-parse', 'refs/heads/feature'), head)
  assert.equal(git(work, 'rev-parse', 'HEAD'), head)
}

for (const newerBase of [false, true]) {
  test(`preserves a dropped bootstrap with newer base: ${newerBase}`, () => {
    const fixture = setup()
    const remoteTip = bootstrap(fixture.work)
    commit(fixture.work, 'work.txt', 'work\n')
    if (newerBase) advanceBase(fixture)
    assertSupersededThenLease(fixture, remoteTip)
    assert.equal(git(fixture.work, 'rev-list', '--count', 'origin/main..feature'), '1')
    assert.notEqual(
      git(fixture.work, 'rev-parse', 'HEAD^{tree}'),
      git(fixture.work, 'rev-parse', 'HEAD^' + '^{tree}'),
    )
  })
}

test('recognizes patch-equivalent work rebased onto a newer base', () => {
  const fixture = setup()
  const tip = commit(fixture.work, 'work.txt', 'work\n')
  git(fixture.work, 'push', '-q', 'origin', 'feature')
  advanceBase(fixture)
  assertSupersededThenLease(fixture, tip)
  assert.equal(git(fixture.work, 'rev-list', '--count', 'origin/main..feature'), '1')
})

test('recognizes a squash with the same tree', () => {
  const fixture = setup()
  const { work } = fixture
  const base = git(work, 'rev-parse', 'HEAD')
  commit(work, 'a.txt', 'a\n')
  const tip = commit(work, 'b.txt', 'b\n')
  git(work, 'push', '-q', 'origin', 'feature')
  git(work, 'reset', '--soft', base)
  git(work, 'commit', '-q', '-m', 'squash')
  assertSupersededThenLease(fixture, tip)
})

test('rescues mixed peer history instead of replaying base commits', () => {
  const fixture = setup()
  const { work, remote, root } = fixture
  bootstrap(work)
  const peerHead = pushFromPeer(root, remote, 'peer.txt', 'peer\n')
  commit(work, 'work.txt', 'work\n')
  advanceBase(fixture)
  const head = git(work, 'rev-parse', 'HEAD')
  const res = pushBranch({ branch: 'feature', base: 'origin/main', cwd: work, sleep: noSleep })
  assert.equal(res.pushed, 'rescue')
  assert.equal(res.refused, 'replays-base')
  assert.equal(res.remoteTip, peerHead)
  assert.equal(git(remote, 'rev-parse', 'refs/heads/feature'), peerHead)
  assert.equal(git(remote, 'rev-parse', `refs/heads/${res.rescue}`), head)
  assert.equal(git(work, 'rev-parse', 'HEAD'), head)
})

test('stops on a stale exact lease without overwriting peer work', () => {
  const { work, root, remote } = setup()
  const oldTip = commit(work, 'a.txt', 'a\n')
  git(work, 'push', '-q', 'origin', 'feature')
  const peerHead = pushFromPeer(root, remote, 'peer.txt', 'peer\n')
  git(work, 'commit', '-q', '--amend', '-m', 'rewritten message')
  const head = git(work, 'rev-parse', 'HEAD')
  const res = pushBranch({ branch: 'feature', expectRemote: oldTip, cwd: work, sleep: noSleep })
  assert.equal(res.pushed, 'rescue')
  assert.equal(res.refused, 'lease-stale')
  assert.equal(res.attempts, 1)
  assert.equal(res.remoteTip, peerHead)
  assert.equal(git(remote, 'rev-parse', 'refs/heads/feature'), peerHead)
  assert.equal(git(remote, 'rev-parse', `refs/heads/${res.rescue}`), head)
  assert.equal(git(work, 'rev-parse', 'HEAD'), head)
})

test('CLI validates full leases and accepts the base flag', () => {
  const { work, remote } = setup()
  const tip = bootstrap(work)
  const head = commit(work, 'work.txt', 'work\n')
  const cli = path.join(HERE, 'push-branch.mjs')
  assert.throws(
    () =>
      execFileSync('node', [cli, '--branch', 'feature', '--expect-remote', 'nothex'], {
        cwd: work,
        stdio: 'ignore',
      }),
    { status: 2 },
  )
  const out = execFileSync(
    'node',
    [
      cli,
      '--branch',
      'feature',
      '--expect-remote',
      tip,
      '--base',
      'origin/main',
      '--initial-delay',
      '0',
    ],
    { cwd: work, encoding: 'utf8' },
  )
  assert.equal(JSON.parse(out).pushed, 'yes')
  assert.equal(git(remote, 'rev-parse', 'refs/heads/feature'), head)
})
