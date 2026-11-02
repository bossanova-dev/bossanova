#!/usr/bin/env node
import { tempDir } from '../test-infra/test-tmpdir.mjs'

import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import * as fs from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  detachPass,
  graceMinutesFrom,
  passFiles,
  reapAgentWorktrees,
} from './agent-worktree-reaper.mjs'
import { removeTree } from './recover-disk-space.mjs'

const MINUTE = 60 * 1000
const HOUR = 60 * MINUTE
const NOW = Date.now()
const OLD = `@${Math.floor((NOW - 3 * HOUR) / 1000)} +0000`
const SCRIPT = join(dirname(fileURLToPath(import.meta.url)), 'agent-worktree-reaper.mjs')

function git(cwd, ...args) {
  return execFileSync('git', args, {
    cwd,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
    env: { ...process.env, GIT_COMMITTER_DATE: OLD, GIT_AUTHOR_DATE: OLD },
  }).trim()
}

function touch(path, ms) {
  const when = new Date(ms)
  fs.utimesSync(path, when, when)
}

/** A repository with a bare origin that holds main, and an empty Claude Code projects folder. */
function fixture(t) {
  const root = fs.realpathSync(tempDir('agent-worktree-reaper-', t))
  const origin = join(root, 'origin.git')
  const repo = join(root, 'repo')
  git(root, 'init', '-q', '--bare', '-b', 'main', origin)
  fs.mkdirSync(repo)
  git(repo, 'init', '-q', '-b', 'main')
  git(repo, 'config', 'user.email', 'test@example.com')
  git(repo, 'config', 'user.name', 'Test')
  git(repo, 'config', 'commit.gpgsign', 'false')
  fs.writeFileSync(
    join(repo, '.gitignore'),
    ['.claude/', '.worktrees/', 'docs/handoffs/', 'node_modules/', 'dist/', '/bazel-*', ''].join(
      '\n',
    ),
  )
  fs.writeFileSync(join(repo, 'README'), 'repo\n')
  git(repo, 'add', '.')
  git(repo, 'commit', '-q', '-m', 'init')
  git(repo, 'remote', 'add', 'origin', origin)
  git(repo, 'push', '-q', 'origin', 'main')
  const projects = join(root, 'projects')
  fs.mkdirSync(projects)
  return { root, repo, projects }
}

/**
 * An agent worktree with one commit of its own, spawned by a parent session whose transcripts and
 * the worktree itself have been quiet for two hours. `push` lands the commit on an origin branch.
 */
function agent(fx, id, { push = true, session = `session-${id}` } = {}) {
  const path = join(fx.repo, '.claude', 'worktrees', `agent-${id}`)
  const branch = `worktree-agent-${id}`
  git(fx.repo, 'worktree', 'add', '-q', '-b', branch, path)
  fs.writeFileSync(join(path, `${id}.txt`), `${id}\n`)
  git(path, 'add', '.')
  git(path, 'commit', '-q', '-m', `agent ${id}`)
  if (push) git(path, 'push', '-q', 'origin', `${branch}:refs/heads/feature-${id}`)
  const slugDir = join(fx.projects, '-repo')
  const subagents = join(slugDir, session, 'subagents')
  fs.mkdirSync(subagents, { recursive: true })
  const transcript = join(slugDir, `${session}.jsonl`)
  fs.writeFileSync(transcript, '{}\n')
  const meta = join(subagents, `agent-${id}.meta.json`)
  fs.writeFileSync(
    meta,
    JSON.stringify({
      worktreePath: path,
      spawnedWithWorktree: true,
      worktreeBranch: branch,
      finished: true,
    }),
  )
  fs.writeFileSync(join(subagents, `agent-${id}.jsonl`), '{}\n')
  const gitDir = git(path, 'rev-parse', '--absolute-git-dir')
  for (const file of [
    transcript,
    meta,
    join(subagents, `agent-${id}.jsonl`),
    join(gitDir, 'HEAD'),
  ]) {
    touch(file, NOW - 2 * HOUR)
  }
  return { path, branch, gitDir, transcript, meta, subagents }
}

function reap(fx, options = {}) {
  const lines = []
  reapAgentWorktrees({
    repoRoot: fx.repo,
    projectsRoots: [fx.projects],
    outputBases: [],
    removeTree,
    cwds: [],
    now: NOW,
    graceMs: HOUR,
    write: (line) => lines.push(line),
    ...options,
  })
  return lines
}

function branchExists(fx, branch) {
  try {
    git(fx.repo, 'rev-parse', '--verify', '-q', `refs/heads/${branch}`)
    return true
  } catch {
    return false
  }
}

test('an agent worktree whose commit landed patch-identical on main goes with its base and branch', (t) => {
  const fx = fixture(t)
  const landed = agent(fx, 'a1', { push: false })
  git(fx.repo, 'cherry-pick', git(landed.path, 'rev-parse', 'HEAD'))
  git(fx.repo, 'push', '-q', 'origin', 'main')
  const base = join(fx.root, 'bazel', 'a'.repeat(32))
  fs.mkdirSync(base, { recursive: true })

  const lines = reap(fx, { outputBases: [{ path: base, workspace: landed.path, alive: false }] })

  assert.equal(fs.existsSync(landed.path), false)
  assert.equal(fs.existsSync(base), false, 'its Bazel output base goes with it')
  assert.equal(branchExists(fx, landed.branch), false, 'its worktree-agent branch is deleted')
  assert.doesNotMatch(git(fx.repo, 'worktree', 'list'), /agent-a1/)
  assert.ok(lines.includes('agent worktrees: removed 1; kept 0'), lines.join('\n'))
})

test('an agent worktree whose commits are on a pushed origin branch goes the same way', (t) => {
  const fx = fixture(t)
  const pushed = agent(fx, 'b1')
  reap(fx)
  assert.equal(fs.existsSync(pushed.path), false)
  assert.equal(branchExists(fx, pushed.branch), false)
})

/** Detach the agent worktree from its branch, as a subagent that rebased leaves it. */
function detach(a) {
  git(a.path, 'checkout', '-q', '--detach')
  touch(join(a.gitDir, 'HEAD'), NOW - 2 * HOUR)
}

test('a detached agent worktree goes, with its branch only when that branch landed too', (t) => {
  const fx = fixture(t)
  const both = agent(fx, 'e1')
  detach(both)
  const ahead = agent(fx, 'e2')
  detach(ahead)
  git(ahead.path, 'checkout', '-q', ahead.branch)
  fs.writeFileSync(join(ahead.path, 'later.txt'), 'later\n')
  git(ahead.path, 'add', '.')
  git(ahead.path, 'commit', '-q', '-m', 'only on the branch')
  detach(ahead)
  git(ahead.path, 'checkout', '-q', '--detach', 'HEAD~1')
  touch(join(ahead.gitDir, 'HEAD'), NOW - 2 * HOUR)

  const lines = reap(fx)

  assert.equal(fs.existsSync(both.path), false)
  assert.equal(branchExists(fx, both.branch), false, 'a landed branch goes with the tree')
  assert.equal(fs.existsSync(ahead.path), false, 'the tree itself had landed')
  assert.equal(branchExists(fx, ahead.branch), true, 'an unlanded branch is kept')
  assert.ok(lines.includes('agent worktrees: removed 2; kept 0'), lines.join('\n'))
})

test('each hold keeps the agent worktree and its branch, and names its reason', (t) => {
  const fx = fixture(t)
  const control = agent(fx, 'control')
  /** @type {Array<[string, string, (a: ReturnType<typeof agent>) => void]>} */
  const cases = [
    ['unpushed', 'commits not on a remote or patch-identical upstream', () => {}],
    ['dirty', 'uncommitted changes', (a) => fs.writeFileSync(join(a.path, 'new.txt'), 'x\n')],
    ['locked', 'locked', (a) => git(fx.repo, 'worktree', 'lock', a.path)],
    ['cwd', 'a process is working in it', () => {}],
    ['bazel', 'its Bazel server is running', () => {}],
    ['active', 'used recently', (a) => touch(join(a.gitDir, 'HEAD'), NOW - MINUTE)],
    ['parent', 'parent session active recently', (a) => touch(a.transcript, NOW - MINUTE)],
    [
      'nested',
      'parent session active recently',
      (a) => fs.writeFileSync(join(a.subagents, 'agent-sibling.jsonl'), '{}\n'),
    ],
    ['nometa', 'parent session unknown', (a) => fs.rmSync(a.meta)],
    [
      'handoff',
      'ignored files that are not regenerable caches',
      (a) => {
        fs.mkdirSync(join(a.path, 'docs', 'handoffs'), { recursive: true })
        fs.writeFileSync(join(a.path, 'docs', 'handoffs', 'notes.md'), 'the only copy\n')
      },
    ],
    [
      'bazeldir',
      'ignored files that are not regenerable caches',
      (a) => {
        fs.writeFileSync(join(a.path, 'bazel-notes.md'), 'a file, not a link\n')
      },
    ],
    [
      'running',
      'its subagent has not finished',
      (a) => {
        const { finished, ...running } = JSON.parse(fs.readFileSync(a.meta, 'utf8'))
        fs.writeFileSync(a.meta, JSON.stringify(running))
        touch(a.meta, NOW - 2 * HOUR)
      },
    ],
    [
      'unfinished',
      'its subagent has not finished',
      (a) => {
        const meta = JSON.parse(fs.readFileSync(a.meta, 'utf8'))
        fs.writeFileSync(a.meta, JSON.stringify({ ...meta, finished: false }))
        touch(a.meta, NOW - 2 * HOUR)
      },
    ],
    [
      'outer',
      'another worktree is inside it',
      (a) => git(fx.repo, 'worktree', 'add', '-q', '-b', 'inner', join(a.path, 'inner')),
    ],
    [
      'elsewhere',
      'its subagent record names another worktree',
      (a) =>
        fs.writeFileSync(
          a.meta,
          JSON.stringify({ worktreePath: control.path, spawnedWithWorktree: true, finished: true }),
        ),
    ],
  ]
  const agents = new Map()
  for (const [id, , perturb] of cases) {
    const a = agent(fx, id, { push: id !== 'unpushed' })
    perturb(a)
    agents.set(id, a)
  }

  const lines = reap(fx, {
    cwds: [join(agents.get('cwd').path, 'sub')],
    outputBases: [{ path: join(fx.root, 'b'), workspace: agents.get('bazel').path, alive: true }],
  })

  assert.equal(fs.existsSync(control.path), false, 'the unperturbed control is removed')
  for (const [id, reason] of cases) {
    const a = agents.get(id)
    assert.equal(fs.existsSync(a.path), true, `${id} keeps its worktree`)
    assert.equal(branchExists(fx, a.branch), true, `${id} keeps its branch`)
    const line = lines.find((l) => l.startsWith(`agent worktrees: kept ${a.path}:`))
    assert.ok(line?.includes(reason), `${id}: ${line}`)
  }
})

test('regenerable caches alone do not keep a landed agent worktree', (t) => {
  const fx = fixture(t)
  const a = agent(fx, 'r1')
  fs.mkdirSync(join(a.path, 'node_modules', 'pkg'), { recursive: true })
  fs.writeFileSync(join(a.path, 'node_modules', 'pkg', 'index.js'), '\n')
  fs.mkdirSync(join(a.path, 'web', 'dist'), { recursive: true })
  fs.writeFileSync(join(a.path, 'web', 'dist', 'app.js'), '\n')
  fs.symlinkSync(fx.root, join(a.path, 'bazel-out'))

  const lines = reap(fx)

  assert.equal(fs.existsSync(a.path), false, lines.join('\n'))
  assert.equal(fs.existsSync(fx.root), true, 'a cache link is removed, never followed')
  assert.ok(lines.includes('agent worktrees: removed 1; kept 0'), lines.join('\n'))
})

test('a blank, invalid or negative grace falls back to 60 minutes', () => {
  for (const value of [undefined, '', '  ', 'soon', '-5', 'NaN', 'Infinity']) {
    assert.equal(
      graceMinutesFrom({ RECOVER_AGENT_WORKTREE_GRACE_MINUTES: value }),
      60,
      String(value),
    )
  }
  assert.equal(graceMinutesFrom({ RECOVER_AGENT_WORKTREE_GRACE_MINUTES: '15' }), 15)
  assert.equal(graceMinutesFrom({ RECOVER_AGENT_WORKTREE_GRACE_MINUTES: '0' }), 0)
})

test('an unavailable process listing keeps every agent worktree', (t) => {
  const fx = fixture(t)
  const a = agent(fx, 'c1')
  const lines = reap(fx, { cwds: null })
  assert.equal(fs.existsSync(a.path), true)
  assert.ok(lines.some((l) => l.includes('process working directories could not be listed')))
})

test('a write or a process that arrives after the first look keeps the agent worktree', (t) => {
  const fx = fixture(t)
  const written = agent(fx, 'w1')
  const entered = agent(fx, 'w2')
  let looks = 0
  const lines = reap(fx, {
    // The first call is the pass's snapshot; later calls are the per-tree re-check before removal.
    cwds: () => {
      looks++
      if (looks === 1) return []
      fs.writeFileSync(join(written.path, 'late.txt'), 'late\n')
      return [entered.path]
    },
  })
  assert.equal(fs.existsSync(join(written.path, 'late.txt')), true, 'the late write survives')
  assert.equal(fs.existsSync(entered.path), true)
  assert.equal(branchExists(fx, written.branch), true)
  assert.equal(branchExists(fx, entered.branch), true)
  assert.ok(
    lines.some((l) => l.startsWith(`agent worktrees: kept ${written.path}: uncommitted changes`)),
    lines.join('\n'),
  )
  assert.ok(
    lines.some(
      (l) =>
        l.startsWith(`agent worktrees: kept ${entered.path}:`) &&
        l.includes('a process is working in it'),
    ),
    lines.join('\n'),
  )
  assert.ok(lines.includes('agent worktrees: removed 0; kept 2'), lines.join('\n'))
})

test('worktrees that are not agent worktrees are never touched or reported', (t) => {
  const fx = fixture(t)
  const plain = join(fx.repo, '.worktrees', 'foo')
  git(fx.repo, 'worktree', 'add', '-q', '-b', 'foo', plain)
  const misnamed = join(fx.repo, '.claude', 'worktrees', 'agent-x')
  git(fx.repo, 'worktree', 'add', '-q', '-b', 'other', misnamed)
  for (const path of [plain, misnamed]) {
    touch(join(git(path, 'rev-parse', '--absolute-git-dir'), 'HEAD'), NOW - 2 * HOUR)
  }

  const lines = reap(fx, { now: NOW + 30 * 24 * HOUR })

  assert.equal(fs.existsSync(plain), true)
  assert.equal(fs.existsSync(misnamed), true)
  assert.deepEqual(lines, [])
})

test('a dry run names what it would remove and changes nothing', (t) => {
  const fx = fixture(t)
  const a = agent(fx, 'd1')
  const lines = reap(fx, { dryRun: true })
  assert.equal(fs.existsSync(a.path), true)
  assert.equal(branchExists(fx, a.branch), true)
  assert.ok(lines.some((l) => l.startsWith(`agent worktrees: would remove ${a.path}`)))
  assert.ok(lines.includes('agent worktrees: would remove 1; kept 0'))
})

test('detach spawns one detached child per rate-limit window and none while a pass holds the claim', (t) => {
  const stateDir = fs.realpathSync(tempDir('agent-worktree-reaper-state-', t))
  const spawned = []
  const spawn = (cmd, args, options) => {
    spawned.push({ cmd, args, options })
    return { unref: () => {} }
  }
  const options = {
    commonDir: '/repo/.git',
    stateDir,
    log: join(stateDir, 'pass.log'),
    script: SCRIPT,
    cwd: '/repo',
    spawn,
  }
  assert.equal(detachPass({ ...options, now: NOW }), true)
  assert.equal(spawned.length, 1)
  assert.equal(spawned[0].options.detached, true)
  assert.deepEqual(spawned[0].args, [SCRIPT])

  assert.equal(detachPass({ ...options, now: NOW + 5 * MINUTE }), false, 'within the window')
  assert.equal(detachPass({ ...options, now: NOW + 11 * MINUTE }), false, 'the pass still runs')
  assert.equal(spawned.length, 1)

  const { claim } = passFiles(options.commonDir, stateDir)
  touch(claim, NOW - 31 * MINUTE)
  assert.equal(detachPass({ ...options, now: NOW + 11 * MINUTE }), true, 'a stale claim is taken')
  assert.equal(spawned.length, 2)
})

test('a pass log past 1 MiB keeps its newest half, from a whole line', (t) => {
  const stateDir = fs.realpathSync(tempDir('agent-worktree-reaper-log-', t))
  const log = join(stateDir, 'pass.log')
  const line = (n) => `${String(n).padStart(8, '0')} ${'x'.repeat(91)}\n`
  fs.writeFileSync(log, Array.from({ length: 12000 }, (_, n) => line(n)).join(''))
  const spawn = () => ({ unref: () => {} })
  detachPass({ commonDir: '/repo/.git', stateDir, log, script: SCRIPT, cwd: '/repo', spawn })
  const kept = fs.readFileSync(log, 'utf8')
  assert.ok(kept.length <= 512 * 1024, `${kept.length} bytes`)
  assert.ok(kept.length > 500 * 1024, `${kept.length} bytes`)
  assert.match(kept, /^\d{8} x+\n/, 'it starts at a whole line')
  assert.ok(kept.endsWith(line(11999)), 'the newest line survives')
})

test('the --detach hook exits 0 and prints nothing', (t) => {
  const home = fs.realpathSync(tempDir('agent-worktree-reaper-home-', t))
  const stateDir = join(home, '.cache', 'bossanova')
  fs.mkdirSync(stateDir, { recursive: true })
  const repoRoot = resolve(dirname(SCRIPT), '..', '..')
  const commonDir = git(repoRoot, 'rev-parse', '--path-format=absolute', '--git-common-dir')
  // A pass started just now, so the hook must not start another against this checkout.
  fs.writeFileSync(passFiles(commonDir, stateDir).stamp, '')
  const result = spawnSync(process.execPath, [SCRIPT, '--detach'], {
    encoding: 'utf8',
    // An empty projects folder makes any pass hold every tree, should one start by mistake.
    env: { ...process.env, HOME: home, RECOVER_CLAUDE_PROJECTS_DIRS: join(home, 'none') },
  })
  assert.equal(result.status, 0)
  assert.equal(result.stdout, '')
  assert.equal(fs.existsSync(join(stateDir, 'agent-worktree-reaper.log')), false, 'no child')
})
