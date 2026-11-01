#!/usr/bin/env node
import { tempDir } from '../test-infra/test-tmpdir.mjs'

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import * as fs from 'node:fs'
import { join } from 'node:path'
import test from 'node:test'

import { removeTree } from './recover-disk-space.mjs'
import {
  discoverRepos,
  liveSessionBranches,
  reapWorktrees,
  salvageWorktree,
} from './worktree-reaper.mjs'

const DAY = 24 * 60 * 60 * 1000
const NOW = Date.now()

const THREE_DAYS_AGO = `@${Math.floor((NOW - 3 * DAY) / 1000)} +0000`

function git(cwd, ...args) {
  return execFileSync('git', args, {
    cwd,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
    env: { ...process.env, GIT_COMMITTER_DATE: THREE_DAYS_AGO, GIT_AUTHOR_DATE: THREE_DAYS_AGO },
  }).trim()
}

function backdate(path, ms) {
  const when = new Date(NOW - ms)
  fs.utimesSync(path, when, when)
}

/**
 * A repository under root/code/repo with one linked worktree per name, whose HEADs last moved three
 * days ago. Their reflog files are freshly written, as git gc leaves them.
 */
function repoWithWorktrees(t, names) {
  const root = fs.realpathSync(tempDir('worktree-reaper-', t))
  const repo = join(root, 'code', 'repo')
  fs.mkdirSync(repo, { recursive: true })
  git(repo, 'init', '-q', '-b', 'main')
  git(repo, 'config', 'user.email', 'test@example.com')
  git(repo, 'config', 'user.name', 'Test')
  git(repo, 'config', 'commit.gpgsign', 'false')
  fs.writeFileSync(join(repo, 'README'), 'repo\n')
  fs.writeFileSync(join(repo, 'go.work.sum'), 'pinned\n')
  git(repo, 'add', '.')
  git(repo, 'commit', '-q', '-m', 'init')
  const worktrees = {}
  for (const name of names) {
    const path = join(repo, '.worktrees', name)
    git(repo, 'worktree', 'add', '-q', '-b', name, path)
    worktrees[name] = path
  }
  const age = () => {
    for (const path of Object.values(worktrees)) {
      const gitDir = git(path, 'rev-parse', '--absolute-git-dir')
      backdate(join(gitDir, 'HEAD'), 3 * DAY)
    }
  }
  return { root, repo, worktrees, age }
}

function outputBase(root, name) {
  const path = join(root, 'bazel', name)
  fs.mkdirSync(path, { recursive: true })
  fs.writeFileSync(join(path, 'command.log'), '')
  backdate(join(path, 'command.log'), 3 * DAY)
  return path
}

function reap(fixture, options) {
  const lines = []
  reapWorktrees({
    roots: [join(fixture.root, 'code')],
    outputBases: [],
    removeTree,
    salvageDir: join(fixture.root, 'salvage'),
    now: NOW,
    minIdleMs: DAY,
    cwds: [],
    sessions: new Set(),
    sizeKiB: () => 0,
    write: (line) => lines.push(line),
    ...options,
  })
  return lines
}

test('removes only clean, idle, unused worktrees and their output bases, keeping branches', (t) => {
  const fixture = repoWithWorktrees(t, [
    'idle',
    'dirty',
    'detached',
    'busy',
    'session',
    'recent',
    'locked',
    'building',
  ])
  const { repo, worktrees: w, root } = fixture

  fs.writeFileSync(join(w.dirty, 'notes.txt'), 'unsaved\n')
  git(w.detached, 'checkout', '-q', '--detach')
  fs.writeFileSync(join(w.detached, 'README'), 'only here\n')
  git(w.detached, 'commit', '-q', '-am', 'unreachable')
  git(repo, 'branch', '-q', '-D', 'detached')
  git(repo, 'worktree', 'lock', w.locked)
  fixture.age()
  execFileSync('git', ['commit', '-q', '--allow-empty', '-m', 'today'], { cwd: w.recent })

  const idleBase = outputBase(root, 'a'.repeat(32))
  const buildingBase = outputBase(root, 'b'.repeat(32))
  reap(fixture, {
    outputBases: [
      { path: idleBase, workspace: w.idle, alive: false },
      { path: buildingBase, workspace: w.building, alive: true },
    ],
    cwds: [join(w.busy, 'services')],
    sessions: new Set(['session']),
  })

  const survivors = Object.entries(w)
    .filter(([, path]) => fs.existsSync(path))
    .map(([name]) => name)
  assert.deepEqual(survivors, [
    'dirty',
    'detached',
    'busy',
    'session',
    'recent',
    'locked',
    'building',
  ])
  assert.equal(fs.existsSync(idleBase), false, 'the reaped worktree takes its output base with it')
  assert.equal(fs.existsSync(buildingBase), true)
  assert.doesNotMatch(git(repo, 'worktree', 'list'), /\.worktrees\/idle/)
  assert.equal(
    git(repo, 'rev-parse', '--verify', '-q', 'refs/heads/idle').length,
    40,
    'its branch survives',
  )
})

test('a dry run removes nothing and names what it would remove', (t) => {
  const fixture = repoWithWorktrees(t, ['idle'])
  fixture.age()
  const lines = reap(fixture, { dryRun: true, sizeKiB: () => 2 * 1024 ** 2 })
  assert.equal(fs.existsSync(fixture.worktrees.idle), true)
  assert.ok(lines.some((line) => line.includes(`would remove ${fixture.worktrees.idle}`)))
  assert.ok(lines.some((line) => line === 'worktrees: would remove 1, 2.0 GiB; kept 0'))
})

test('keeps every worktree when sessions or process directories cannot be listed', (t) => {
  const fixture = repoWithWorktrees(t, ['a', 'b'])
  fixture.age()
  reap(fixture, { sessions: null })
  reap(fixture, { cwds: null })
  assert.equal(fs.existsSync(fixture.worktrees.a), true)
  assert.equal(fs.existsSync(fixture.worktrees.b), true)
})

test('finds a repository once, however many of its worktrees the roots contain', (t) => {
  const fixture = repoWithWorktrees(t, ['one'])
  const elsewhere = join(fixture.root, 'elsewhere', 'two')
  git(fixture.repo, 'worktree', 'add', '-q', '-b', 'two', elsewhere)
  const repos = discoverRepos([join(fixture.root, 'code'), join(fixture.root, 'elsewhere')])
  assert.deepEqual(repos, [fs.realpathSync(join(fixture.repo, '.git'))])
})

test('boss missing from PATH means sessions are unknown on a host that runs bossd', (t) => {
  const withBossd = fs.realpathSync(tempDir('worktree-reaper-home-', t))
  fs.mkdirSync(join(withBossd, '.bossanova'))
  const withoutBossd = fs.realpathSync(tempDir('worktree-reaper-home-', t))
  const env = {
    BOSS_BIN: join(withoutBossd, 'no-such-boss'),
    BOSS_SOCKET: join(withBossd, 'bossd.sock'),
  }
  assert.equal(liveSessionBranches({ env, home: withBossd }), null)
  assert.deepEqual(liveSessionBranches({ env, home: withoutBossd }), new Set())
})

function flakyBoss(t, failures) {
  const dir = fs.realpathSync(tempDir('worktree-reaper-boss-', t))
  const counter = join(dir, 'calls')
  const bin = join(dir, 'boss')
  fs.writeFileSync(
    bin,
    `#!/bin/sh\nn=$(cat '${counter}' 2>/dev/null || echo 0); n=$((n+1)); echo $n > '${counter}'\n` +
      `[ $n -le ${failures} ] && exit 1\necho '{"sessions":[{"branch":"live"}]}'\n`,
    { mode: 0o755 },
  )
  return {
    env: { ...process.env, BOSS_BIN: bin, BOSS_SOCKET: join(dir, 'bossd.sock') },
    home: dir,
    counter,
  }
}

test('session lookup waits out a bossd restart before giving up', (t) => {
  const pauses = []
  const restarting = flakyBoss(t, 2)
  const branches = liveSessionBranches({ ...restarting, sleep: (ms) => pauses.push(ms) })
  assert.deepEqual(branches, new Set(['live']))
  assert.deepEqual(pauses, [20000, 20000])

  pauses.length = 0
  const down = flakyBoss(t, 99)
  assert.equal(liveSessionBranches({ ...down, sleep: (ms) => pauses.push(ms) }), null)
  assert.equal(pauses.length, 3, 'no pause after the last attempt')
})

test('never runs boss without a daemon socket, so it cannot start a daemon of its own', (t) => {
  const host = flakyBoss(t, 0)
  fs.mkdirSync(join(host.home, '.bossanova'))
  const noSocket = { ...host.env }
  delete noSocket.BOSS_SOCKET
  assert.equal(liveSessionBranches({ env: noSocket, home: host.home, sleep: () => {} }), null)
  assert.equal(fs.existsSync(host.counter), false, 'boss was never invoked')
})

test('a loose commit whose patch already landed on main does not hold its worktree', (t) => {
  const fixture = repoWithWorktrees(t, ['landed'])
  const { repo, worktrees: w } = fixture
  git(w.landed, 'checkout', '-q', '--detach')
  fs.writeFileSync(join(w.landed, 'README'), 'shipped\n')
  git(w.landed, 'commit', '-q', '-am', 'ship it')
  fs.writeFileSync(join(repo, 'other.txt'), 'main moved on\n')
  git(repo, 'add', 'other.txt')
  git(repo, 'commit', '-q', '-m', 'main moves on')
  git(repo, 'cherry-pick', git(w.landed, 'rev-parse', 'HEAD'))
  git(repo, 'branch', '-q', '-D', 'landed')
  fixture.age()
  reap(fixture)
  assert.equal(fs.existsSync(w.landed), false)
})

test('changes only to go.work.sum do not hold a worktree', (t) => {
  const fixture = repoWithWorktrees(t, ['sum'])
  fs.writeFileSync(join(fixture.worktrees.sum, 'go.work.sum'), 'drifted\n')
  fixture.age()
  reap(fixture)
  assert.equal(fs.existsSync(fixture.worktrees.sum), false)
})

test('a long-idle worktree with unsaved work is salvaged, then removed', (t) => {
  const fixture = repoWithWorktrees(t, ['draft'])
  const { repo, root, worktrees: w } = fixture
  git(w.draft, 'checkout', '-q', '--detach')
  fs.writeFileSync(join(w.draft, 'README'), 'draft commit\n')
  git(w.draft, 'commit', '-q', '-am', 'draft')
  const draft = git(w.draft, 'rev-parse', 'HEAD')
  git(repo, 'branch', '-q', '-D', 'draft')
  fs.writeFileSync(join(w.draft, 'README'), 'uncommitted edit\n')
  fs.writeFileSync(join(w.draft, 'note.txt'), 'untracked\n')
  fixture.age()

  reap(fixture, { now: NOW + 40 * DAY, salvageAfterMs: 30 * DAY })

  assert.equal(fs.existsSync(w.draft), false)
  const [saved] = fs.readdirSync(join(root, 'salvage')).map((name) => join(root, 'salvage', name))
  assert.match(fs.readFileSync(join(saved, 'changes.patch'), 'utf8'), /\+uncommitted edit/)
  const tarball = execFileSync('tar', ['-tzf', join(saved, 'untracked.tar.gz')], {
    encoding: 'utf8',
  })
  assert.match(tarball, /note\.txt/)
  assert.match(git(root, 'bundle', 'list-heads', join(saved, 'commits.bundle')), new RegExp(draft))
})

test('a fresh edit counts as activity, so an old checkout being edited is not salvaged', (t) => {
  const fixture = repoWithWorktrees(t, ['editing'])
  fixture.age()
  fs.writeFileSync(join(fixture.worktrees.editing, 'README'), 'edited just now\n')
  reap(fixture, { salvageAfterMs: DAY })
  assert.equal(fs.existsSync(fixture.worktrees.editing), true)
})

test('salvage never overwrites an earlier salvage of the same worktree', (t) => {
  const fixture = repoWithWorktrees(t, ['twice'])
  const salvageDir = join(fixture.root, 'salvage')
  fs.writeFileSync(join(fixture.worktrees.twice, 'README'), 'first\n')
  const facts = { path: fixture.worktrees.twice, branch: 'twice', headOnRef: true }
  const first = salvageWorktree(facts, salvageDir, NOW)
  fs.writeFileSync(join(fixture.worktrees.twice, 'README'), 'second\n')
  assert.throws(() => salvageWorktree(facts, salvageDir, NOW), { code: 'EEXIST' })
  assert.match(fs.readFileSync(join(first, 'changes.patch'), 'utf8'), /\+first/)
})

test('a worktree whose submodule has changes is kept, because salvage cannot save them', (t) => {
  const fixture = repoWithWorktrees(t, ['withsub'])
  const { root, worktrees: w } = fixture
  const source = join(root, 'subsource')
  fs.mkdirSync(source)
  git(source, 'init', '-q', '-b', 'main')
  git(source, 'config', 'commit.gpgsign', 'false')
  git(
    source,
    '-c',
    'user.email=t@example.com',
    '-c',
    'user.name=T',
    'commit',
    '-q',
    '--allow-empty',
    '-m',
    'sub',
  )
  fs.writeFileSync(join(source, 'lib.txt'), 'lib\n')
  git(source, 'add', '.')
  git(source, '-c', 'user.email=t@example.com', '-c', 'user.name=T', 'commit', '-q', '-m', 'lib')
  git(w.withsub, '-c', 'protocol.file.allow=always', 'submodule', 'add', '-q', source, 'vendored')
  git(w.withsub, 'commit', '-q', '-m', 'add submodule')
  fs.writeFileSync(join(w.withsub, 'vendored', 'lib.txt'), 'edited inside the submodule\n')
  fixture.age()

  const lines = reap(fixture, { now: NOW + 40 * DAY, salvageAfterMs: 30 * DAY })
  assert.equal(fs.existsSync(join(w.withsub, 'vendored', 'lib.txt')), true)
  assert.ok(lines.some((line) => line.includes('could not salvage it')))
})
