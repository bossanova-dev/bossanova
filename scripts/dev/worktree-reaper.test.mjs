#!/usr/bin/env node
import { tempDir } from '../test-infra/test-tmpdir.mjs'

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import * as fs from 'node:fs'
import { join } from 'node:path'
import test from 'node:test'

import { removeTree } from './recover-disk-space.mjs'
import { discoverRepos, liveSessionBranches, reapWorktrees } from './worktree-reaper.mjs'

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
  const env = { BOSS_BIN: join(withoutBossd, 'no-such-boss') }
  assert.equal(liveSessionBranches({ env, home: withBossd }), null)
  assert.deepEqual(liveSessionBranches({ env, home: withoutBossd }), new Set())
})
