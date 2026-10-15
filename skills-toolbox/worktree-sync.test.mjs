// Real-git scenarios for worktree-sync.mjs: a bare remote, a session worktree on the PR branch, and
// a side checkout that rebases and force-pushes the branch the way a repair run did.

import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

import {
  BACKUP_DIR,
  LOCK_FILE,
  LOCK_STALE_MS,
  STATE_FILE,
  VERDICTS,
  sync,
} from './worktree-sync.mjs'

const HELPER = fileURLToPath(new URL('./worktree-sync.mjs', import.meta.url))

const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'worktree-sync-test-'))
const gitConfig = path.join(scratch, 'gitconfig')
fs.writeFileSync(
  gitConfig,
  '[user]\n\tname = t\n\temail = t@example.com\n[commit]\n\tgpgsign = false\n[init]\n\tdefaultBranch = main\n',
)
process.env.GIT_CONFIG_GLOBAL = gitConfig
process.env.GIT_CONFIG_NOSYSTEM = '1'
test.after(() => fs.rmSync(scratch, { recursive: true, force: true }))

function git(cwd, ...args) {
  const r = spawnSync('git', ['-c', 'commit.gpgsign=false', '-C', cwd, ...args], {
    encoding: 'utf8',
  })
  assert.equal(r.status, 0, `git ${args.join(' ')}: ${r.stderr}`)
  return r.stdout.trim()
}

function write(dir, file, body) {
  fs.mkdirSync(path.dirname(path.join(dir, file)), { recursive: true })
  fs.writeFileSync(path.join(dir, file), body)
}

function commit(dir, file, body, msg) {
  write(dir, file, body)
  git(dir, 'add', file)
  git(dir, 'commit', '-q', '-m', msg)
  return git(dir, 'rev-parse', 'HEAD')
}

// The PR branch `feat` is pushed from the session worktree at `oldHead`, then main advances.
function world() {
  const root = fs.mkdtempSync(path.join(scratch, 'w-'))
  const remote = path.join(root, 'remote.git')
  const session = path.join(root, 'session')
  const other = path.join(root, 'other')
  git(root, 'init', '-q', '--bare', remote)
  git(root, 'clone', '-q', remote, session)
  commit(session, 'go.work.sum', 'h1\n', 'base')
  commit(session, 'shared.txt', 'one\n', 'shared')
  git(session, 'push', '-q', 'origin', 'main')
  git(session, 'checkout', '-q', '-b', 'feat')
  const oldHead = commit(session, 'shared.txt', 'one\nfeat\n', 'feat work')
  git(session, 'push', '-q', '-u', 'origin', 'feat')
  git(root, 'clone', '-q', remote, other)
  commit(other, 'shared.txt', 'zero\none\n', 'main moves')
  git(other, 'push', '-q', 'origin', 'main')
  return { root, remote, session, oldHead }
}

// A side checkout rebases `feat` onto the new main, resolving the conflict by hand (so the patch
// id changes), and force-pushes. The session worktree is left on `oldHead`.
function repairFromSideCheckout(w) {
  const side = path.join(w.root, 'side')
  git(w.session, 'fetch', '-q', 'origin')
  git(w.session, 'worktree', 'add', '-q', '--detach', side, 'origin/feat')
  git(side, 'reset', '-q', '--hard', 'origin/main')
  const pushed = commit(side, 'shared.txt', 'zero\none\nfeat (resolved)\n', 'feat work')
  git(side, 'push', '-q', '--force-with-lease=feat', 'origin', 'HEAD:feat')
  return { side, pushed }
}

test('the pre-fix fast-forward cannot recover a session left on the pre-rebase head', () => {
  const w = world()
  repairFromSideCheckout(w)
  git(w.session, 'fetch', '-q', 'origin')
  const ff = spawnSync('git', ['-C', w.session, 'merge', '--ff-only', 'origin/feat'], {
    encoding: 'utf8',
  })
  assert.notEqual(ff.status, 0, 'a diverged session cannot fast-forward')
  assert.equal(git(w.session, 'rev-parse', 'HEAD'), w.oldHead)
})

test('a session on a previous PR head is reset to the pushed head, keeping go.work.sum dirt, and the side checkout is removed', () => {
  const w = world()
  const { side, pushed } = repairFromSideCheckout(w)
  write(w.session, 'go.work.sum', 'h1\nlocal churn\n')

  const r = sync({ repo: w.session, removeWorktrees: [side] })

  assert.equal(r.verdict, VERDICTS.RESET, r.reason)
  assert.equal(r.synced, true)
  assert.equal(r.previousHead, w.oldHead)
  assert.equal(git(w.session, 'rev-parse', 'HEAD'), pushed)
  assert.equal(git(w.session, 'rev-parse', 'feat'), pushed, 'the branch ref moves with HEAD')
  assert.equal(fs.readFileSync(path.join(w.session, 'go.work.sum'), 'utf8'), 'h1\nlocal churn\n')
  assert.deepEqual(r.preserved, ['go.work.sum'])
  assert.equal(
    fs.readFileSync(path.join(w.session, 'shared.txt'), 'utf8'),
    'zero\none\nfeat (resolved)\n',
  )
  assert.deepEqual(r.removedWorktrees, [side])
  assert.equal(fs.existsSync(side), false)
  assert.ok(!git(w.session, 'worktree', 'list').includes(side))
})

test('without the remote reflog a previous head passed by the caller still proves the commit was pushed', () => {
  const w = world()
  const { pushed } = repairFromSideCheckout(w)
  git(w.session, 'reflog', 'expire', '--expire=now', '--all')

  const blind = sync({ repo: w.session, escalateAfter: 99 })
  assert.equal(blind.verdict, VERDICTS.UNSAFE)
  assert.deepEqual(blind.unpushed, [w.oldHead])
  assert.equal(git(w.session, 'rev-parse', 'HEAD'), w.oldHead)

  const told = sync({ repo: w.session, previousHeads: [w.oldHead] })
  assert.equal(told.verdict, VERDICTS.RESET, told.reason)
  assert.equal(git(w.session, 'rev-parse', 'HEAD'), pushed)
})

test('an unpushed local commit is never discarded, and the refusal escalates after N attempts', () => {
  const w = world()
  const unpushed = commit(w.session, 'local.txt', 'mine\n', 'unpushed work')
  repairFromSideCheckout(w)

  const attempts = [1, 2, 3].map(() =>
    sync({ repo: w.session, escalateAfter: 3, now: () => Date.UTC(2026, 9, 15) }),
  )

  for (const r of attempts) {
    assert.equal(r.verdict, VERDICTS.UNSAFE)
    assert.equal(r.synced, false)
    assert.deepEqual(r.unpushed, [unpushed])
  }
  assert.deepEqual(
    attempts.map((r) => [r.attempts, r.escalate]),
    [
      [1, false],
      [2, false],
      [3, true],
    ],
  )
  assert.equal(attempts[2].firstSeenAt, '2026-10-15T00:00:00.000Z')
  assert.equal(attempts[2].parkReason, 'worktree-diverged')
  assert.equal(git(w.session, 'rev-parse', 'HEAD'), unpushed)
  assert.equal(fs.readFileSync(path.join(w.session, 'local.txt'), 'utf8'), 'mine\n')
})

test('a later successful sync clears the refusal count', () => {
  const w = world()
  git(w.session, 'reset', '-q', '--hard', 'HEAD~1')
  commit(w.session, 'local.txt', 'mine\n', 'unpushed work')
  const first = sync({ repo: w.session })
  assert.equal(first.verdict, VERDICTS.UNSAFE)
  const stateFile = path.join(w.session, '.git', STATE_FILE)
  assert.equal(JSON.parse(fs.readFileSync(stateFile, 'utf8')).attempts, 1)

  git(w.session, 'reset', '-q', '--hard', w.oldHead)
  const ok = sync({ repo: w.session })
  assert.equal(ok.verdict, VERDICTS.IN_SYNC)
  assert.equal(fs.existsSync(stateFile), false)
})

test('a session behind the PR head fast-forwards', () => {
  const w = world()
  const ahead = path.join(w.root, 'ahead')
  git(w.root, 'clone', '-q', '-b', 'feat', w.remote, ahead)
  const next = commit(ahead, 'more.txt', 'more\n', 'more')
  git(ahead, 'push', '-q', 'origin', 'feat')

  const r = sync({ repo: w.session, head: next })
  assert.equal(r.verdict, VERDICTS.FAST_FORWARDED, r.reason)
  assert.equal(git(w.session, 'rev-parse', 'HEAD'), next)
})

test('a tracked change outside the incidental set blocks the move', () => {
  const w = world()
  repairFromSideCheckout(w)
  write(w.session, 'shared.txt', 'operator edit\n')

  const r = sync({ repo: w.session })
  assert.equal(r.verdict, VERDICTS.DIRTY)
  assert.deepEqual(r.blocking, ['shared.txt'])
  assert.equal(git(w.session, 'rev-parse', 'HEAD'), w.oldHead)
  assert.equal(fs.readFileSync(path.join(w.session, 'shared.txt'), 'utf8'), 'operator edit\n')
})

test('a side checkout holding work the target lacks is kept', () => {
  const w = world()
  const { side } = repairFromSideCheckout(w)
  commit(side, 'extra.txt', 'x\n', 'side-only work')

  const r = sync({ repo: w.session, removeWorktrees: [side] })
  assert.equal(r.verdict, VERDICTS.RESET, r.reason)
  assert.deepEqual(r.keptWorktrees, [{ path: side, reason: 'head-not-in-target' }])
  assert.equal(fs.existsSync(path.join(side, 'extra.txt')), true)
})

test('the CLI prints one JSON line and exits 0 on a recovery, 1 on a refusal', () => {
  const w = world()
  repairFromSideCheckout(w)
  const run = (...args) =>
    spawnSync(process.execPath, [HELPER, 'sync', '--repo', w.session, ...args], {
      encoding: 'utf8',
    })
  const ok = run()
  assert.equal(ok.status, 0, ok.stdout + ok.stderr)
  assert.equal(JSON.parse(ok.stdout).verdict, VERDICTS.RESET)

  commit(w.session, 'local.txt', 'mine\n', 'unpushed work')
  const refused = run('--escalate-after', '1')
  assert.equal(refused.status, 1)
  const payload = JSON.parse(refused.stdout)
  assert.equal(payload.verdict, VERDICTS.UNSAFE)
  assert.equal(payload.escalate, true)

  const usage = spawnSync(process.execPath, [HELPER, 'sync', '--bogus'], { encoding: 'utf8' })
  assert.equal(usage.status, 2)
})

test('a worktree already on a newer pushed head is never moved back to a stale target', () => {
  const w = world()
  const next = commit(w.session, 'more.txt', 'more\n', 'more')
  git(w.session, 'push', '-q', 'origin', 'feat')

  const descendant = sync({ repo: w.session, head: w.oldHead })
  assert.equal(descendant.verdict, VERDICTS.AHEAD, descendant.reason)
  assert.equal(descendant.synced, false)
  assert.equal(git(w.session, 'rev-parse', 'HEAD'), next)
  assert.equal(git(w.session, 'rev-parse', 'origin/feat'), next)

  git(w.session, 'push', '-q', '--force', 'origin', `${w.oldHead}:feat`)
  const rewound = sync({ repo: w.session })
  assert.equal(rewound.verdict, VERDICTS.AHEAD, rewound.reason)
  assert.equal(rewound.target, w.oldHead)
  assert.equal(git(w.session, 'rev-parse', 'HEAD'), next)
  git(w.session, 'push', '-q', '--force', 'origin', 'feat')

  const { pushed } = repairFromSideCheckout(w)
  assert.equal(sync({ repo: w.session }).verdict, VERDICTS.RESET)
  const rebased = sync({ repo: w.session, head: next })
  assert.equal(rebased.verdict, VERDICTS.AHEAD, rebased.reason)
  assert.equal(rebased.parkReason, 'worktree-diverged')
  assert.equal(git(w.session, 'rev-parse', 'HEAD'), pushed)
})

test('--remove-worktree keeps the current worktree, the main checkout and any branch checkout', () => {
  const w = world()
  const { side, pushed } = repairFromSideCheckout(w)
  const peer = path.join(w.root, 'peer')
  git(w.session, 'worktree', 'add', '-q', '-b', 'peer', peer, 'origin/main')

  const fromSession = sync({ repo: w.session, removeWorktrees: [peer] })
  assert.equal(fromSession.verdict, VERDICTS.RESET, fromSession.reason)
  assert.deepEqual(fromSession.keptWorktrees, [
    { path: peer, reason: 'branch-checked-out: refs/heads/peer' },
  ])
  assert.equal(fs.existsSync(peer), true)

  git(w.session, 'checkout', '-q', '--detach', pushed)
  const fromSide = sync({ repo: side, head: pushed, removeWorktrees: [side, w.session] })
  assert.equal(fromSide.verdict, VERDICTS.IN_SYNC, fromSide.reason)
  assert.deepEqual(fromSide.keptWorktrees, [
    { path: side, reason: 'current-worktree' },
    { path: w.session, reason: 'main-checkout' },
  ])
  assert.equal(fs.existsSync(side), true)
  assert.equal(fs.existsSync(path.join(w.session, '.git')), true)
})

test('an unresolvable target or a failed fetch counts toward escalation and parks as unsynced', () => {
  const w = world()
  const missing = [1, 2].map(() =>
    sync({ repo: w.session, head: 'f'.repeat(40), escalateAfter: 2 }),
  )
  assert.deepEqual(
    missing.map((r) => [r.verdict, r.attempts, r.escalate, r.parkReason]),
    [
      [VERDICTS.UNKNOWN, 1, false, 'worktree-unsynced'],
      [VERDICTS.UNKNOWN, 2, true, 'worktree-unsynced'],
    ],
  )

  git(w.session, 'remote', 'set-url', 'origin', path.join(w.root, 'gone.git'))
  const offline = [1, 2].map(() => sync({ repo: w.session, escalateAfter: 2 }))
  assert.match(offline[0].reason, /^fetch origin feat/)
  assert.deepEqual(
    offline.map((r) => [r.verdict, r.attempts, r.escalate, r.parkReason]),
    [
      [VERDICTS.UNKNOWN, 1, false, 'worktree-unsynced'],
      [VERDICTS.UNKNOWN, 2, true, 'worktree-unsynced'],
    ],
  )
  assert.equal(git(w.session, 'rev-parse', 'HEAD'), w.oldHead)
})

test('an incidental file the target changes takes the target version and the local bytes are backed up', () => {
  const w = world()
  const side = path.join(w.root, 'side')
  git(w.session, 'fetch', '-q', 'origin')
  git(w.session, 'worktree', 'add', '-q', '--detach', side, 'origin/feat')
  git(side, 'reset', '-q', '--hard', 'origin/main')
  write(side, 'go.work.sum', 'h1\nh2-from-pr\n')
  git(side, 'add', 'go.work.sum')
  const pushed = commit(side, 'shared.txt', 'zero\none\nfeat (resolved)\n', 'feat work')
  git(side, 'push', '-q', '--force-with-lease=feat', 'origin', 'HEAD:feat')
  write(w.session, 'go.work.sum', 'h1\nlocal churn\n')

  const r = sync({ repo: w.session })
  assert.equal(r.verdict, VERDICTS.RESET, r.reason)
  assert.equal(git(w.session, 'rev-parse', 'HEAD'), pushed)
  assert.equal(fs.readFileSync(path.join(w.session, 'go.work.sum'), 'utf8'), 'h1\nh2-from-pr\n')
  assert.deepEqual(r.preserved, [])
  const backup = path.join(fs.realpathSync(w.session), '.git', BACKUP_DIR, 'go.work.sum')
  assert.deepEqual(r.superseded, [{ path: 'go.work.sum', backup }])
  assert.equal(fs.readFileSync(backup, 'utf8'), 'h1\nlocal churn\n')
})

test('a live lock refuses a concurrent run; a stale one is taken over', () => {
  const w = world()
  const { pushed } = repairFromSideCheckout(w)
  const lock = path.join(w.session, '.git', LOCK_FILE)
  fs.writeFileSync(lock, '1\n')

  const busy = sync({ repo: w.session })
  assert.equal(busy.verdict, VERDICTS.UNKNOWN)
  assert.match(busy.reason, /another sync holds/)
  assert.equal(git(w.session, 'rev-parse', 'HEAD'), w.oldHead)
  assert.equal(fs.existsSync(lock), true)

  const stale = new Date(Date.now() - LOCK_STALE_MS - 1000)
  fs.utimesSync(lock, stale, stale)
  const r = sync({ repo: w.session })
  assert.equal(r.verdict, VERDICTS.RESET, r.reason)
  assert.equal(git(w.session, 'rev-parse', 'HEAD'), pushed)
  assert.equal(fs.existsSync(lock), false)
})
