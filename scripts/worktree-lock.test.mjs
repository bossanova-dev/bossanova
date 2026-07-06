#!/usr/bin/env node

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { after, test } from 'node:test'

const scriptPath = path.join(
  path.dirname(fileURLToPath(import.meta.url)),
  '..',
  'skills-toolbox',
  'worktree-lock.sh',
)
const tempRoots = []

function lockHome() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'bli-lockhome-'))
  tempRoots.push(dir)
  return dir
}

function initRepo() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'bli-wt-'))
  tempRoots.push(dir)
  execFileSync('git', ['init', '-q', dir])
  return fs.realpathSync(dir)
}

// Run the lock script. runid/ticket are positional args (the script reads $2/$3);
// env overrides BLI_LOCK_HOME / BLI_LOCK_STALE_SECS.
function lock(cwd, env, ...args) {
  try {
    const stdout = execFileSync('bash', [scriptPath, ...args], {
      cwd,
      encoding: 'utf8',
      env: { ...process.env, BLI_SLUG: 'testslug', ...env },
    })
    return { code: 0, stdout }
  } catch (err) {
    return {
      code: err.status ?? 1,
      stdout: err.stdout?.toString() ?? '',
      stderr: err.stderr?.toString() ?? '',
    }
  }
}

after(() => {
  for (const dir of tempRoots) fs.rmSync(dir, { recursive: true, force: true })
})

test('worktree-lock: acquire / re-entrancy / peer / stale / release / isolation / TOCTOU / races / collision', () => {
  assert.ok(fs.existsSync(scriptPath), 'worktree-lock.sh must exist')
  const HOME = lockHome()
  const env = { BLI_LOCK_HOME: HOME }
  const repo1 = initRepo()

  // 1. Clean acquire succeeds.
  let r = lock(repo1, env, 'acquire', 'runA', 'BOS-1')
  assert.equal(r.code, 0, r.stderr)
  assert.match(r.stdout, /ACQUIRED/)

  // 2. RE-ENTRANT: same runid acquiring again succeeds (a run can't collide with itself).
  r = lock(repo1, env, 'acquire', 'runA', 'BOS-1')
  assert.equal(r.code, 0, r.stderr)
  assert.match(r.stdout, /ACQUIRED/)

  // 3. PEER: a different runid is refused while the owner is fresh.
  r = lock(repo1, env, 'acquire', 'runB', 'BOS-1')
  assert.equal(r.code, 3)
  assert.match(r.stdout, /HELD_BY_PEER/)

  // 4. STATUS reports the owner.
  r = lock(repo1, env, 'status', 'runA')
  assert.match(r.stdout, /runA/)

  // 5. STALE TAKEOVER: with a 1s window and a 2s sleep, a different run takes over.
  execFileSync('sleep', ['2'])
  r = lock(repo1, { ...env, BLI_LOCK_STALE_SECS: '1' }, 'acquire', 'runB', 'BOS-1')
  assert.equal(r.code, 0, r.stderr)
  assert.match(r.stdout, /TOOK_OVER_STALE/)
  assert.match(lock(repo1, env, 'status', 'runB').stdout, /runB/)

  // 6. RELEASE by owner frees the lock; a fresh run can acquire.
  assert.match(lock(repo1, env, 'release', 'runB').stdout, /RELEASED/)
  // The atomic rename-aside release must leave no `.releasing.*` orphan behind.
  const orphans = execFileSync(
    'bash',
    ['-c', `find "${HOME}" -path '*${path.basename(repo1)}*.releasing.*' | head -1`],
    { encoding: 'utf8' },
  ).trim()
  assert.equal(orphans, '', `release must not leak a .releasing orphan: ${orphans}`)
  r = lock(repo1, env, 'acquire', 'runC', 'BOS-1')
  assert.equal(r.code, 0, r.stderr)
  assert.match(r.stdout, /ACQUIRED/)

  // 7. RELEASE by a non-owner is refused.
  r = lock(repo1, env, 'release', 'runZ')
  assert.equal(r.code, 3)

  // 8. PER-WORKTREE ISOLATION: a different worktree path gets an independent lock.
  const repo2 = initRepo()
  r = lock(repo2, env, 'acquire', 'runD', 'BOS-2')
  assert.equal(r.code, 0, r.stderr)
  assert.match(r.stdout, /ACQUIRED/)

  // 9. TOCTOU GUARD: lock dir present but owner-meta missing (mid-acquire window) → HELD_BY_PEER.
  const repo3 = initRepo()
  lock(repo3, env, 'acquire', 'runE', 'BOS-3')
  const meta3 = execFileSync(
    'bash',
    ['-c', `find "${HOME}" -path '*${path.basename(repo3)}*/owner' -type f | head -1`],
    { encoding: 'utf8' },
  ).trim()
  assert.ok(meta3, 'should locate repo3 owner meta')
  fs.rmSync(meta3)
  r = lock(repo3, env, 'acquire', 'runF', 'BOS-3')
  assert.equal(r.code, 3, `empty-meta lock must read HELD_BY_PEER: ${r.stdout}`)
  assert.match(r.stdout, /HELD_BY_PEER/)

  // 10. CONCURRENT RACE: two acquires on one fresh worktree → exactly one ACQUIRED + one HELD_BY_PEER.
  const repo4 = initRepo()
  const race = execFileSync(
    'bash',
    [
      '-c',
      `BLI_LOCK_HOME="${HOME}" BLI_SLUG=testslug bash "${scriptPath}" acquire A BOS-4 >a.out 2>&1 & ` +
        `BLI_LOCK_HOME="${HOME}" BLI_SLUG=testslug bash "${scriptPath}" acquire B BOS-4 >b.out 2>&1 & wait; ` +
        `cat a.out b.out`,
    ],
    { cwd: repo4, encoding: 'utf8' },
  )
  assert.equal((race.match(/ACQUIRED/g) || []).length, 1, `one ACQUIRED: ${race}`)
  assert.equal((race.match(/HELD_BY_PEER/g) || []).length, 1, `one HELD_BY_PEER: ${race}`)
  assert.equal((race.match(/TOOK_OVER_STALE/g) || []).length, 0, `no STALE: ${race}`)

  // 11. CONCURRENT STALE REVIVAL stays atomic: exactly one winner + one HELD_BY_PEER,
  // never a double takeover. The winner usually reports TOOK_OVER_STALE, but a plain
  // ACQUIRED is equally valid and safe: stealing a stale lock renames the lock dir aside
  // (mv "$LOCK" "$stamp"), which briefly frees the canonical name, so the racing peer can
  // legitimately win it with a fresh mkdir. Asserting TOOK_OVER_STALE specifically made
  // this case flaky; the invariant that actually matters is single ownership, not which
  // code path the winner took (the single-process takeover path is covered by case 5).
  const repo5 = initRepo()
  const revival = execFileSync(
    'bash',
    [
      '-c',
      `BLI_LOCK_HOME="${HOME}" BLI_SLUG=testslug BLI_LOCK_STALE_SECS=1 bash "${scriptPath}" acquire old BOS-5 >/dev/null; ` +
        `sleep 2; ` +
        `BLI_LOCK_HOME="${HOME}" BLI_SLUG=testslug BLI_LOCK_STALE_SECS=1 bash "${scriptPath}" acquire P BOS-5 >p.out 2>&1 & ` +
        `BLI_LOCK_HOME="${HOME}" BLI_SLUG=testslug BLI_LOCK_STALE_SECS=1 bash "${scriptPath}" acquire Q BOS-5 >q.out 2>&1 & wait; ` +
        `cat p.out q.out`,
    ],
    { cwd: repo5, encoding: 'utf8' },
  )
  const revivalWinners =
    (revival.match(/TOOK_OVER_STALE/g) || []).length + (revival.match(/ACQUIRED/g) || []).length
  assert.equal(revivalWinners, 1, `exactly one winner (TOOK_OVER_STALE or ACQUIRED): ${revival}`)
  assert.equal((revival.match(/HELD_BY_PEER/g) || []).length, 1, `one HELD_BY_PEER: ${revival}`)

  // 12. dir_mtime fallback must feed numeric arithmetic (no abort), yielding a numeric age.
  const repo6 = initRepo()
  lock(repo6, env, 'acquire', 'm', 'BOS-6')
  const meta6 = execFileSync(
    'bash',
    ['-c', `find "${HOME}" -path '*${path.basename(repo6)}*/owner' -type f | head -1`],
    { encoding: 'utf8' },
  ).trim()
  fs.rmSync(meta6)
  r = lock(repo6, env, 'acquire', 'n', 'BOS-6')
  assert.equal(r.code, 3, `mtime-fallback must exit 3: ${r.stdout}`)
  assert.match(r.stdout, /age=[0-9]+s/)

  // 13. If the lock dir has no owner and mtime cannot be read, treat it as live.
  const repo6b = initRepo()
  lock(repo6b, env, 'acquire', 'm', 'BOS-6B')
  const meta6b = execFileSync(
    'bash',
    ['-c', `find "${HOME}" -path '*${path.basename(repo6b)}*/owner' -type f | head -1`],
    { encoding: 'utf8' },
  ).trim()
  fs.rmSync(meta6b)
  const fakeBin = fs.mkdtempSync(path.join(os.tmpdir(), 'bli-fakebin-'))
  tempRoots.push(fakeBin)
  fs.writeFileSync(path.join(fakeBin, 'stat'), '#!/usr/bin/env sh\nexit 1\n', { mode: 0o755 })
  r = lock(
    repo6b,
    { ...env, BLI_LOCK_STALE_SECS: '1', PATH: `${fakeBin}:${process.env.PATH}` },
    'acquire',
    'n',
    'BOS-6B',
  )
  assert.equal(r.code, 3, `unreadable mtime must not look stale: ${r.stdout}${r.stderr}`)
  assert.match(r.stdout, /HELD_BY_PEER/)

  // 14. RELEASE must never delete a SUCCESSOR's lock.
  const repo7 = initRepo()
  lock(repo7, env, 'acquire', 'A', 'BOS-7')
  const meta7 = execFileSync(
    'bash',
    ['-c', `find "${HOME}" -path '*${path.basename(repo7)}*/owner' -type f | head -1`],
    { encoding: 'utf8' },
  ).trim()
  fs.writeFileSync(meta7, `B\n999\n${Math.floor(Date.now() / 1000)}\nBOS-7\n`)
  r = lock(repo7, env, 'release', 'A')
  assert.equal(r.code, 3, `releasing a successor-owned lock must be NOT_OWNER: ${r.stdout}`)
  assert.ok(fs.existsSync(meta7), 'release must NOT delete the successor lock')
  assert.equal(fs.readFileSync(meta7, 'utf8').split('\n')[0], 'B')

  // 14. NAME-COLLISION SAFETY: two paths that collide under slashes→underscores get distinct locks.
  //     `<root>/x/y_z` and `<root>/x_y/z` both map to `..._x_y_z` naively; the sha256 key separates them.
  const collRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'bli-coll-'))
  tempRoots.push(collRoot)
  const p1 = path.join(collRoot, 'x', 'y_z')
  const p2 = path.join(collRoot, 'x_y', 'z')
  fs.mkdirSync(p1, { recursive: true })
  fs.mkdirSync(p2, { recursive: true })
  execFileSync('git', ['init', '-q', p1])
  execFileSync('git', ['init', '-q', p2])
  assert.match(lock(fs.realpathSync(p1), env, 'acquire', 'c1', 'BOS-8').stdout, /ACQUIRED/)
  const r2 = lock(fs.realpathSync(p2), env, 'acquire', 'c2', 'BOS-8')
  assert.equal(r2.code, 0, `colliding path must get its own lock, not HELD_BY_PEER: ${r2.stdout}`)
  assert.match(r2.stdout, /ACQUIRED/)
})
