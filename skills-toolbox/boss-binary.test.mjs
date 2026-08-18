import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'

import { BOSS_BINARY_NAME, resolveBossBinary } from './boss-binary.mjs'

// A minimal injectable fs: `files` maps an absolute path to its stat shape.
// Anything absent throws ENOENT exactly the way node:fs does, so a candidate that
// "does not exist" is indistinguishable from the real thing.
function fakeFs(files = {}) {
  return {
    statSync(target) {
      const entry = files[target]
      if (!entry) {
        const error = new Error(`ENOENT: no such file or directory, stat '${target}'`)
        error.code = 'ENOENT'
        throw error
      }
      return {
        mode: entry.mode ?? 0o755,
        isFile: () => entry.isFile !== false,
      }
    },
  }
}

const EXEC = { mode: 0o755 }
const NOT_EXEC = { mode: 0o644 }
const DIR = { mode: 0o755, isFile: false }

// A cwd with no `.git` ancestor at all, so the ./bin/boss fallback resolves against
// the cwd itself and never wanders into the machine's real checkout.
const CWD = '/tmp/nowhere/sub'

test('resolveBossBinary prefers $BOSS_BIN over PATH and over ./bin/boss', () => {
  const fs = fakeFs({
    '/opt/boss-bin/boss': EXEC,
    '/usr/local/bin/boss': EXEC,
    [path.join(CWD, 'bin', 'boss')]: EXEC,
  })
  const result = resolveBossBinary(
    { BOSS_BIN: '/opt/boss-bin/boss', PATH: '/usr/local/bin' },
    { fs, cwd: CWD },
  )
  assert.equal(result.ok, true)
  assert.equal(result.path, '/opt/boss-bin/boss')
  assert.equal(result.reason, '')
})

test('resolveBossBinary falls back to `boss` on PATH when $BOSS_BIN is unset', () => {
  const fs = fakeFs({
    '/usr/local/bin/boss': EXEC,
    [path.join(CWD, 'bin', 'boss')]: EXEC,
  })
  const result = resolveBossBinary({ PATH: '/nope:/usr/local/bin' }, { fs, cwd: CWD })
  assert.equal(result.ok, true)
  assert.equal(result.path, '/usr/local/bin/boss')
})

test('resolveBossBinary falls back to the repo build ./bin/boss last', () => {
  // No BOSS_BIN, empty PATH: only the repo build remains. The repo root is the
  // nearest ancestor of cwd carrying a .git entry (a file in a worktree, a
  // directory in a plain checkout) — both are just a stat here.
  const root = '/repo'
  const fs = fakeFs({
    '/repo/.git': EXEC,
    '/repo/bin/boss': EXEC,
  })
  const result = resolveBossBinary({ PATH: '' }, { fs, cwd: '/repo/services/boss' })
  assert.equal(result.ok, true)
  assert.equal(result.path, path.join(root, 'bin', 'boss'))
})

test('resolveBossBinary reports BOSS_BIN by name when it points at a nonexistent path', () => {
  // The stale-worktree case: BOSS_BIN names a path that no longer exists and
  // nothing else resolves. The reason must name BOSS_BIN so the log says WHY.
  const result = resolveBossBinary(
    { BOSS_BIN: '/gone/worktree/bin/boss', PATH: '' },
    { fs: fakeFs({}), cwd: CWD },
  )
  assert.equal(result.ok, false)
  assert.equal(result.path, null)
  assert.match(result.reason, /BOSS_BIN/)
  assert.match(result.reason, /\/gone\/worktree\/bin\/boss/)
  assert.match(result.reason, /does not exist/)
})

test('resolveBossBinary rejects a candidate that exists but is not executable', () => {
  const fs = fakeFs({ '/opt/boss-bin/boss': NOT_EXEC })
  const result = resolveBossBinary({ BOSS_BIN: '/opt/boss-bin/boss', PATH: '' }, { fs, cwd: CWD })
  assert.equal(result.ok, false)
  assert.match(result.reason, /BOSS_BIN/)
  assert.match(result.reason, /is not executable/)
})

test('resolveBossBinary rejects a candidate that exists and is executable but is a directory', () => {
  // `/some/dir/boss` as a directory is executable by mode (0o755) — the isFile()
  // check is what stops the resolver handing back an unspawnable path.
  const fs = fakeFs({ '/opt/boss-bin/boss': DIR })
  const result = resolveBossBinary({ BOSS_BIN: '/opt/boss-bin/boss', PATH: '' }, { fs, cwd: CWD })
  assert.equal(result.ok, false)
  assert.match(result.reason, /is not a regular file/)
})

test('a non-executable $BOSS_BIN still falls through to a healthy PATH binary', () => {
  // Failing a candidate is not an error: it is simply the next candidate's turn.
  const fs = fakeFs({
    '/opt/boss-bin/boss': NOT_EXEC,
    '/usr/local/bin/boss': EXEC,
  })
  const result = resolveBossBinary(
    { BOSS_BIN: '/opt/boss-bin/boss', PATH: '/usr/local/bin' },
    { fs, cwd: CWD },
  )
  assert.equal(result.ok, true)
  assert.equal(result.path, '/usr/local/bin/boss')
})

test('resolveBossBinary returns ok:false when no candidate resolves anywhere', () => {
  const result = resolveBossBinary({ BOSS_SESSION_ID: 'x' }, { fs: fakeFs({}), cwd: CWD })
  assert.equal(result.ok, false)
  assert.equal(result.path, null)
  assert.match(result.reason, /BOSS_BIN is unset/)
  assert.match(result.reason, /PATH/)
  assert.match(result.reason, /bin[/\\]boss/)
})

test('resolveBossBinary never spawns the resolved binary', () => {
  // A hung or slow `boss --version` would hang EVERY gate call, so executability is
  // a stat and only a stat. The guard has to be on the SOURCE. Injecting an exploding
  // `deps.spawnSync` would assert nothing: the resolver reads no such key and never
  // would — a regression arrives as a direct `import … from 'node:child_process'`,
  // which no injected dep can intercept, so that test would pass for EVERY possible
  // implementation. This one fails the moment someone "just adds a --version probe".
  const source = fs.readFileSync(new URL('./boss-binary.mjs', import.meta.url), 'utf8')
  assert.doesNotMatch(source, /child_process/, 'boss-binary.mjs must not reach for child_process')
  assert.doesNotMatch(
    source,
    /\b(?:spawnSync|spawn|execFileSync|execFile|execSync|exec)\s*\(/,
    'boss-binary.mjs must not call any process-spawning function',
  )
})

test('resolveBossBinary resolves through the REAL fs when no deps are injected', () => {
  // Every other case injects `deps.fs`, so the `deps.fs || fs` default — the only
  // path production ever takes, since callbacksAvailable(env) passes deps `{}` — would
  // otherwise never execute. A typo in that default would pass the whole suite.
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'boss-binary-'))
  try {
    const bin = path.join(dir, BOSS_BINARY_NAME)
    fs.writeFileSync(bin, '#!/bin/sh\nexit 0\n')
    fs.chmodSync(bin, 0o755)
    const plain = path.join(dir, 'not-executable')
    fs.writeFileSync(plain, '')
    fs.chmodSync(plain, 0o644)

    // Fully default deps (real fs AND real process.cwd()): the $BOSS_BIN arm wins.
    assert.deepEqual(resolveBossBinary({ BOSS_BIN: bin, PATH: '' }), {
      path: bin,
      ok: true,
      reason: '',
    })
    // Fully default deps again: the PATH arm resolves the same real file.
    assert.equal(resolveBossBinary({ PATH: dir }).path, bin)
    // Real fs, but an anchored cwd so the ./bin/boss arm cannot reach this checkout's
    // own build and turn a rejection into a pass.
    const rejected = resolveBossBinary({ BOSS_BIN: plain, PATH: '' }, { cwd: dir })
    assert.equal(rejected.ok, false)
    assert.equal(rejected.path, null)
    assert.match(rejected.reason, /is not executable/)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('an empty or whitespace-only BOSS_BIN is treated as unset, not as a candidate', () => {
  const fs = fakeFs({ '/usr/local/bin/boss': EXEC })
  for (const blank of ['', '   ']) {
    const result = resolveBossBinary({ BOSS_BIN: blank, PATH: '/usr/local/bin' }, { fs, cwd: CWD })
    assert.equal(result.ok, true, `expected the PATH binary for BOSS_BIN=${JSON.stringify(blank)}`)
    assert.equal(result.path, '/usr/local/bin/boss')
  }
})

test('a PATH entry that is not a directory is skipped rather than throwing', () => {
  const fs = fakeFs({ '/usr/local/bin/boss': EXEC })
  const result = resolveBossBinary(
    { PATH: `/does/not/exist${path.delimiter}/usr/local/bin` },
    { fs, cwd: CWD },
  )
  assert.equal(result.ok, true)
  assert.equal(result.path, '/usr/local/bin/boss')
})

test('a file whose execute bit is not for THIS process is rejected, not resolved', () => {
  // The mode bits answer "may someone run this", not "may I". Mode 0o010 has an
  // execute bit, passes `mode & 0o111`, and still gives EACCES when run — so a
  // mode-only check would arm a callback for a binary that cannot fire. Real fs and
  // a real chmod, because this is precisely the case a hand-written double would beg.
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'boss-binary-acl-'))
  try {
    const bin = path.join(dir, BOSS_BINARY_NAME)
    fs.writeFileSync(bin, '#!/bin/sh\nexit 0\n')
    fs.chmodSync(bin, 0o010) // group-execute only; this process is the OWNER
    assert.notEqual(fs.statSync(bin).mode & 0o111, 0, 'precondition: a mode-only check passes here')
    let denied = false
    try {
      fs.accessSync(bin, fs.constants.X_OK)
    } catch {
      denied = true
    }
    if (!denied) return // running as root, or a fs that ignores the bits — nothing to assert

    const result = resolveBossBinary({ BOSS_BIN: bin, PATH: '' }, { cwd: dir })
    assert.equal(result.ok, false)
    assert.equal(result.path, null)
    assert.match(result.reason, /is not executable by this process/)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('an fs double without accessSync keeps the mode-bit verdict', () => {
  // `deps.fs` is documented as needing only `statSync`, so the X_OK check is guarded
  // on the method existing — a double must not be failed for a method it never
  // promised. This pins that seam so the guard cannot be dropped by accident.
  const fsDouble = fakeFs({ '/opt/boss-bin/boss': EXEC })
  assert.equal(typeof fsDouble.accessSync, 'undefined')
  const result = resolveBossBinary({ BOSS_BIN: '/opt/boss-bin/boss' }, { fs: fsDouble, cwd: CWD })
  assert.equal(result.ok, true)
  assert.equal(result.path, '/opt/boss-bin/boss')
})

test('an injected accessSync that denies X_OK rejects the candidate', () => {
  const fsDouble = {
    ...fakeFs({ '/opt/boss-bin/boss': EXEC }),
    accessSync() {
      const error = new Error('EACCES: permission denied')
      error.code = 'EACCES'
      throw error
    },
  }
  const result = resolveBossBinary(
    { BOSS_BIN: '/opt/boss-bin/boss', PATH: '' },
    {
      fs: fsDouble,
      cwd: CWD,
    },
  )
  assert.equal(result.ok, false)
  assert.match(result.reason, /is not executable by this process/)
})

test('an unresolvable cwd is a rejection note, never a throw out of the gate', () => {
  // `process.cwd()` throws ENOENT once the process's directory has been removed, and
  // this gate runs on every CI wait — a throw there is strictly worse than a `false`.
  // A non-string cwd makes `path.resolve` throw the same way, which is how the guard
  // is reachable from a test. Before the fix the read happened eagerly at the top, so
  // it escaped even when an earlier candidate had already won.
  const result = resolveBossBinary({ PATH: '' }, { fs: fakeFs({}), cwd: 42 })
  assert.equal(result.ok, false)
  assert.equal(result.path, null)
  assert.match(result.reason, /repo build could not be located/)
})

test('a winning earlier candidate never consults the cwd at all', () => {
  // The other half of the same fix: $BOSS_BIN resolving must not depend on the cwd
  // being readable, so a bad cwd is simply never reached.
  const fs = fakeFs({ '/opt/boss-bin/boss': EXEC })
  const result = resolveBossBinary({ BOSS_BIN: '/opt/boss-bin/boss' }, { fs, cwd: 42 })
  assert.equal(result.ok, true)
  assert.equal(result.path, '/opt/boss-bin/boss')
})

test('resolveBossBinary always returns the { path, ok, reason } shape', () => {
  const good = resolveBossBinary(
    { PATH: '/usr/local/bin' },
    { fs: fakeFs({ '/usr/local/bin/boss': EXEC }), cwd: CWD },
  )
  const bad = resolveBossBinary({ PATH: '' }, { fs: fakeFs({}), cwd: CWD })
  for (const result of [good, bad]) {
    assert.deepEqual(Object.keys(result).sort(), ['ok', 'path', 'reason'])
    assert.equal(typeof result.ok, 'boolean')
    assert.equal(typeof result.reason, 'string')
  }
})
