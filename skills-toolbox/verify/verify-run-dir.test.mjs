import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'
import { PIN_TTL_MS } from '../skill-pin.mjs'
import {
  createRunDir,
  removeRunDir,
  resolveRunDir,
  runDirParent,
  runDirPath,
} from './verify-run-dir.mjs'

function fixture(t) {
  const tmpRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'verify-run-dir-'))
  t.after(() => fs.rmSync(tmpRoot, { recursive: true, force: true, maxRetries: 3 }))
  const gitDir = path.join(tmpRoot, 'repo-a', '.git')
  const otherGitDir = path.join(tmpRoot, 'repo-b', '.git')
  return { tmpRoot, gitDir, otherGitDir }
}

const chat = (id) => ({ BOSS_AGENT_SESSION_ID: id })

test('different chat ids in one worktree get disjoint directories with isolated files', (t) => {
  const f = fixture(t)
  const dirA = createRunDir({ gitDir: f.gitDir, env: chat('chat-a'), tmpRoot: f.tmpRoot })
  const dirB = createRunDir({ gitDir: f.gitDir, env: chat('chat-b'), tmpRoot: f.tmpRoot })

  assert.notEqual(dirA, dirB)
  assert.equal(dirA, runDirPath({ gitDir: f.gitDir, env: chat('chat-a'), tmpRoot: f.tmpRoot }))
  assert.equal(dirB, runDirPath({ gitDir: f.gitDir, env: chat('chat-b'), tmpRoot: f.tmpRoot }))
  const uid = typeof process.getuid === 'function' ? process.getuid() : 'user'
  assert.equal(path.dirname(dirA), path.join(fs.realpathSync(f.tmpRoot), `boss-verify-run-${uid}`))
  assert.equal(path.dirname(dirA), runDirParent(f.tmpRoot))
  assert.match(path.basename(dirA), /^[0-9a-f]{12}-chat-a$/)
  assert.match(path.basename(dirB), /^[0-9a-f]{12}-chat-b$/)

  fs.writeFileSync(path.join(dirA, 'judge2.json'), '{"verdict":"pass"}\n')
  assert.equal(fs.existsSync(path.join(dirB, 'judge2.json')), false)

  removeRunDir({ gitDir: f.gitDir, env: chat('chat-a'), tmpRoot: f.tmpRoot })
  assert.equal(fs.existsSync(dirA), false)
  assert.equal(fs.existsSync(dirB), true)
})

test('two git dirs with no session id fall back to local and get different paths', (t) => {
  const f = fixture(t)
  const pathA = runDirPath({ gitDir: f.gitDir, env: {}, tmpRoot: f.tmpRoot })
  const pathB = runDirPath({ gitDir: f.otherGitDir, env: {}, tmpRoot: f.tmpRoot })

  assert.notEqual(pathA, pathB)
  assert.match(path.basename(pathA), /^[0-9a-f]{12}-local$/)
  assert.match(path.basename(pathB), /^[0-9a-f]{12}-local$/)
})

test('create empties an earlier run directory for the same id and sets mode 0700', (t) => {
  const f = fixture(t)
  const first = createRunDir({ gitDir: f.gitDir, env: chat('chat-reuse'), tmpRoot: f.tmpRoot })
  fs.mkdirSync(path.join(first, 'nested'))
  fs.writeFileSync(path.join(first, 'stale.json'), 'old')
  fs.writeFileSync(path.join(first, 'nested', 'deep.json'), 'old')

  const previousUmask = process.umask(0o000)
  try {
    const second = createRunDir({ gitDir: f.gitDir, env: chat('chat-reuse'), tmpRoot: f.tmpRoot })
    assert.equal(second, first)
    assert.deepEqual(fs.readdirSync(second), [])
    assert.equal(fs.statSync(second).mode & 0o777, 0o700)
  } finally {
    process.umask(previousUmask)
  }
})

test('path fails closed when missing and returns the created directory otherwise', (t) => {
  const f = fixture(t)
  assert.throws(
    () => resolveRunDir({ gitDir: f.gitDir, env: chat('chat-missing'), tmpRoot: f.tmpRoot }),
    /BLOCKED: boss-verify run dir missing - re-run the first block/,
  )

  const created = createRunDir({ gitDir: f.gitDir, env: chat('chat-missing'), tmpRoot: f.tmpRoot })
  assert.equal(
    resolveRunDir({ gitDir: f.gitDir, env: chat('chat-missing'), tmpRoot: f.tmpRoot }),
    created,
  )

  fs.rmSync(created, { recursive: true, force: true, maxRetries: 3 })
  fs.writeFileSync(created, 'not-a-directory')
  assert.throws(
    () => resolveRunDir({ gitDir: f.gitDir, env: chat('chat-missing'), tmpRoot: f.tmpRoot }),
    /BLOCKED: boss-verify run dir missing - re-run the first block/,
  )
})

test('remove deletes the directory and is idempotent', (t) => {
  const f = fixture(t)
  const dir = createRunDir({ gitDir: f.gitDir, env: chat('chat-rm'), tmpRoot: f.tmpRoot })
  fs.writeFileSync(path.join(dir, 'notes.txt'), 'temp')

  removeRunDir({ gitDir: f.gitDir, env: chat('chat-rm'), tmpRoot: f.tmpRoot })
  assert.equal(fs.existsSync(dir), false)

  assert.doesNotThrow(() =>
    removeRunDir({ gitDir: f.gitDir, env: chat('chat-rm'), tmpRoot: f.tmpRoot }),
  )
  assert.equal(fs.existsSync(dir), false)
})

test('create reaps siblings older than PIN_TTL_MS, keeps fresh siblings, and never reaps itself', (t) => {
  const f = fixture(t)
  const staleDir = createRunDir({ gitDir: f.gitDir, env: chat('chat-stale'), tmpRoot: f.tmpRoot })
  const freshDir = createRunDir({ gitDir: f.gitDir, env: chat('chat-fresh'), tmpRoot: f.tmpRoot })
  fs.writeFileSync(path.join(staleDir, 'leftover.json'), 'stale')
  fs.writeFileSync(path.join(freshDir, 'active.json'), 'fresh')

  const now = Date.now()
  const staleAt = new Date(now - PIN_TTL_MS - 60_000)
  const freshAt = new Date(now - PIN_TTL_MS + 60_000)
  fs.utimesSync(staleDir, staleAt, staleAt)
  fs.utimesSync(freshDir, freshAt, freshAt)

  const current = createRunDir({
    gitDir: f.gitDir,
    env: chat('chat-current'),
    tmpRoot: f.tmpRoot,
    now,
  })
  assert.equal(fs.existsSync(staleDir), false)
  assert.equal(fs.existsSync(freshDir), true)
  assert.equal(fs.readFileSync(path.join(freshDir, 'active.json'), 'utf8'), 'fresh')
  assert.equal(fs.existsSync(current), true)

  // Even when `now` is far ahead of the filesystem clock, create must never reap its own directory.
  const ownReCheck = createRunDir({
    gitDir: f.gitDir,
    env: chat('chat-current'),
    tmpRoot: f.tmpRoot,
    now: now + PIN_TTL_MS * 10,
  })
  assert.equal(fs.existsSync(ownReCheck), true)
})

test('invalid run ids are refused through pinRun', (t) => {
  const f = fixture(t)
  for (const badId of ['../escape', 'nested/id', '.hidden', 'space id']) {
    assert.throws(
      () => runDirPath({ gitDir: f.gitDir, env: chat(badId), tmpRoot: f.tmpRoot }),
      /BLOCKED/,
    )
    assert.throws(
      () => createRunDir({ gitDir: f.gitDir, env: chat(badId), tmpRoot: f.tmpRoot }),
      /BLOCKED/,
    )
    assert.throws(
      () => resolveRunDir({ gitDir: f.gitDir, env: chat(badId), tmpRoot: f.tmpRoot }),
      /BLOCKED/,
    )
    assert.throws(
      () => removeRunDir({ gitDir: f.gitDir, env: chat(badId), tmpRoot: f.tmpRoot }),
      /BLOCKED/,
    )
  }
})

test('CLI create, path, and remove work end-to-end with isolated --git-dir and --tmp-root', (t) => {
  const f = fixture(t)
  const cli = fileURLToPath(new URL('./verify-run-dir.mjs', import.meta.url))
  const run = (args, sessionId = 'chat-cli-1') =>
    spawnSync(process.execPath, [cli, ...args, '--git-dir', f.gitDir, '--tmp-root', f.tmpRoot], {
      encoding: 'utf8',
      env: { ...process.env, BOSS_AGENT_SESSION_ID: sessionId },
    })

  const missing = run(['path'])
  assert.equal(missing.status, 1)
  assert.match(missing.stderr, /BLOCKED: boss-verify run dir missing - re-run the first block/)

  const created = run(['create'])
  assert.equal(created.status, 0, created.stderr)
  const dir = created.stdout.trim()
  assert.equal(dir, runDirPath({ gitDir: f.gitDir, env: chat('chat-cli-1'), tmpRoot: f.tmpRoot }))
  assert.equal(fs.statSync(dir).mode & 0o777, 0o700)

  const resolved = run(['path'])
  assert.equal(resolved.status, 0, resolved.stderr)
  assert.equal(resolved.stdout.trim(), dir)

  const peer = run(['create'], 'chat-cli-2')
  assert.equal(peer.status, 0, peer.stderr)
  const peerDir = peer.stdout.trim()
  assert.notEqual(peerDir, dir)

  const removed = run(['remove'])
  assert.equal(removed.status, 0, removed.stderr)
  assert.equal(fs.existsSync(dir), false)
  assert.equal(fs.existsSync(peerDir), true)

  const removedAgain = run(['remove'])
  assert.equal(removedAgain.status, 0, removedAgain.stderr)

  const missingAgain = run(['path'])
  assert.equal(missingAgain.status, 1)
  assert.match(missingAgain.stderr, /BLOCKED: boss-verify run dir missing - re-run the first block/)

  const badId = run(['create'], '../escape')
  assert.equal(badId.status, 1)
  assert.match(badId.stderr, /BLOCKED/)

  const badVerb = run(['unknown'])
  assert.equal(badVerb.status, 1)
  assert.match(badVerb.stderr, /usage:/)
})

test('create scopes parent per-UID and enforces mode 0700 on parent even under umask 0000', (t) => {
  const f = fixture(t)
  const previousUmask = process.umask(0o000)
  try {
    const dir = createRunDir({ gitDir: f.gitDir, env: chat('chat-umask'), tmpRoot: f.tmpRoot })
    const parent = runDirParent(f.tmpRoot)
    assert.equal(path.dirname(dir), parent)
    if (typeof process.getuid === 'function') {
      assert.equal(path.basename(parent), `boss-verify-run-${process.getuid()}`)
      assert.equal(fs.lstatSync(parent).mode & 0o777, 0o700)
      assert.equal(fs.lstatSync(dir).mode & 0o777, 0o700)
    }
  } finally {
    process.umask(previousUmask)
  }
})

test('symlinked parent is refused by create, resolve, and remove without touching victim files', (t) => {
  const f = fixture(t)
  const victimDir = path.join(f.tmpRoot, 'victim')
  fs.mkdirSync(victimDir, { mode: 0o700 })
  const expectedDir = runDirPath({ gitDir: f.gitDir, env: chat('chat-sym'), tmpRoot: f.tmpRoot })
  const victimMatchingChild = path.join(victimDir, path.basename(expectedDir))
  const victimStaleEntry = path.join(victimDir, 'important-old-file')
  fs.mkdirSync(victimMatchingChild, { mode: 0o700 })
  fs.writeFileSync(path.join(victimMatchingChild, 'keep.txt'), 'do-not-delete')
  fs.writeFileSync(victimStaleEntry, 'precious')
  const staleAt = new Date(Date.now() - PIN_TTL_MS * 2)
  fs.utimesSync(victimStaleEntry, staleAt, staleAt)

  const parent = runDirParent(f.tmpRoot)
  fs.symlinkSync(victimDir, parent)

  const opts = { gitDir: f.gitDir, env: chat('chat-sym'), tmpRoot: f.tmpRoot }
  assert.throws(() => createRunDir(opts), /BLOCKED: boss-verify run dir parent is untrusted/)
  assert.throws(() => resolveRunDir(opts), /BLOCKED: boss-verify run dir parent is untrusted/)
  assert.throws(() => removeRunDir(opts), /BLOCKED: boss-verify run dir parent is untrusted/)

  assert.equal(fs.readFileSync(victimStaleEntry, 'utf8'), 'precious')
  assert.equal(fs.readFileSync(path.join(victimMatchingChild, 'keep.txt'), 'utf8'), 'do-not-delete')
})

test('group/world-accessible or non-directory parent is refused by create, resolve, and remove', (t) => {
  if (typeof process.getuid !== 'function') return t.skip('POSIX permissions required')
  for (const badMode of [0o777, 0o755, 0o770, 0o702]) {
    const f = fixture(t)
    const parent = runDirParent(f.tmpRoot)
    fs.mkdirSync(parent)
    fs.chmodSync(parent, badMode)
    const opts = { gitDir: f.gitDir, env: chat('chat-mode'), tmpRoot: f.tmpRoot }
    assert.throws(() => createRunDir(opts), /BLOCKED: boss-verify run dir parent is untrusted/)
    assert.throws(() => resolveRunDir(opts), /BLOCKED: boss-verify run dir parent is untrusted/)
    assert.throws(() => removeRunDir(opts), /BLOCKED: boss-verify run dir parent is untrusted/)
  }

  const fFile = fixture(t)
  fs.writeFileSync(runDirParent(fFile.tmpRoot), 'not-a-dir')
  const fileOpts = { gitDir: fFile.gitDir, env: chat('chat-file'), tmpRoot: fFile.tmpRoot }
  assert.throws(() => createRunDir(fileOpts), /BLOCKED: boss-verify run dir parent is untrusted/)
  assert.throws(() => resolveRunDir(fileOpts), /BLOCKED: boss-verify run dir parent is untrusted/)
  assert.throws(() => removeRunDir(fileOpts), /BLOCKED: boss-verify run dir parent is untrusted/)
})

test('parent owned by another UID is refused by create, resolve, and remove', (t) => {
  if (typeof process.getuid !== 'function') return t.skip('POSIX getuid required')
  const f = fixture(t)
  const foreignUid = process.getuid() + 1
  const foreignParent = path.join(fs.realpathSync(f.tmpRoot), `boss-verify-run-${foreignUid}`)
  fs.mkdirSync(foreignParent, { mode: 0o700 })

  t.mock.method(process, 'getuid', () => foreignUid)
  const opts = { gitDir: f.gitDir, env: chat('chat-uid'), tmpRoot: f.tmpRoot }
  assert.throws(() => createRunDir(opts), /BLOCKED: boss-verify run dir parent is untrusted/)
  assert.throws(() => resolveRunDir(opts), /BLOCKED: boss-verify run dir parent is untrusted/)
  assert.throws(() => removeRunDir(opts), /BLOCKED: boss-verify run dir parent is untrusted/)
})

test('resolve refuses group/world-accessible or symlinked run dir', (t) => {
  if (typeof process.getuid !== 'function') return t.skip('POSIX permissions required')
  const f = fixture(t)
  const opts = { gitDir: f.gitDir, env: chat('chat-dir-perm'), tmpRoot: f.tmpRoot }
  const dir = createRunDir(opts)

  for (const badMode of [0o777, 0o755, 0o770]) {
    fs.chmodSync(dir, badMode)
    assert.throws(() => resolveRunDir(opts), /BLOCKED: boss-verify run dir is untrusted/)
  }

  fs.chmodSync(dir, 0o700)
  assert.equal(resolveRunDir(opts), dir)

  fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  const decoy = path.join(f.tmpRoot, 'decoy')
  fs.mkdirSync(decoy, { mode: 0o700 })
  fs.symlinkSync(decoy, dir)
  assert.throws(
    () => resolveRunDir(opts),
    /BLOCKED: boss-verify run dir missing - re-run the first block/,
  )
})
