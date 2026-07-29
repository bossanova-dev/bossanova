#!/usr/bin/env node

import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  buildCodexArgs,
  classifyProbe,
  probe,
  resolveCodexBin,
  run,
  sanitizeOutput,
} from './codex-review.mjs'

const SCRIPT_PATH = fileURLToPath(new URL('./codex-review.mjs', import.meta.url))
const PROBE_READY_TIMEOUT_MS = 10000

test('CLI through a symlink rejects a missing command loudly', () => {
  const dir = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), 'codex-review-link-'))
  const link = path.join(dir, 'codex-review.mjs')
  fs.symlinkSync(SCRIPT_PATH, link)
  try {
    const res = spawnSync(process.execPath, [link], { encoding: 'utf8' })
    assert.notEqual(res.status, 0)
    assert.match(res.stderr, /unknown command/i)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/** Create a temporary directory and return its path. Caller is responsible for cleanup. */
function makeTmpDir() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'codex-review-test-'))
}

/**
 * Write a small executable shell script to `dir/<name>` and return its full path.
 * `body` is the shell script body (after the shebang line).
 */
function writeFakeBin(dir, name, body) {
  const p = path.join(dir, name)
  fs.writeFileSync(p, `#!/bin/sh\n${body}\n`)
  fs.chmodSync(p, 0o755)
  return p
}

async function waitForPidFile(pidFile, timeoutMs = 2000) {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    if (fs.existsSync(pidFile)) {
      const pid = Number(fs.readFileSync(pidFile, 'utf8').trim())
      if (Number.isInteger(pid) && pid > 0) return pid
    }
    await new Promise((r) => setTimeout(r, 20))
  }
  throw new Error(`timed out waiting for pid file: ${pidFile}`)
}

// A fake codex that prints every argv element on its own line then exits 0.
// run() returns the sanitized stdout, so the embedded prompt (with any diff) is
// fully observable for assertions.
function writeEchoArgvBin(dir) {
  return writeFakeBin(dir, 'codex', 'for a in "$@"; do printf "%s\\n" "$a"; done\nexit 0')
}

// Run a git command in `cwd`, asserting success. Isolated config (no GPG sign,
// committer/author pinned) so it works in any CI environment.
function git(cwd, args) {
  const res = spawnSync('git', args, {
    cwd,
    encoding: 'utf8',
    env: {
      ...process.env,
      GIT_AUTHOR_NAME: 'Test',
      GIT_AUTHOR_EMAIL: 'test@example.com',
      GIT_COMMITTER_NAME: 'Test',
      GIT_COMMITTER_EMAIL: 'test@example.com',
    },
  })
  assert.equal(res.status, 0, `git ${args.join(' ')} failed: ${res.stderr}`)
  return res.stdout.trim()
}

function initRepo(dir) {
  git(dir, ['init', '-q'])
  git(dir, ['config', 'commit.gpgsign', 'false'])
}

// ---------------------------------------------------------------------------
// 1. classifyProbe — pure classifier, all four outcomes
// ---------------------------------------------------------------------------

test('classifyProbe: ENOENT spawnError → not_installed', () => {
  assert.equal(
    classifyProbe({ spawnError: { code: 'ENOENT' }, status: null, signal: null }),
    'not_installed',
  )
})

test('classifyProbe: status 0 → ready', () => {
  assert.equal(classifyProbe({ spawnError: null, status: 0, signal: null }), 'ready')
})

test('classifyProbe: non-zero status, no signal → not_authed', () => {
  assert.equal(classifyProbe({ spawnError: null, status: 1, signal: null }), 'not_authed')
})

test('classifyProbe: non-zero status 2, no signal → not_authed', () => {
  assert.equal(classifyProbe({ spawnError: null, status: 2, signal: null }), 'not_authed')
})

test('classifyProbe: killed by signal → error', () => {
  assert.equal(classifyProbe({ spawnError: null, status: null, signal: 'SIGKILL' }), 'error')
})

test('classifyProbe: other spawnError (non-ENOENT) → error', () => {
  assert.equal(
    classifyProbe({ spawnError: { code: 'EACCES' }, status: null, signal: null }),
    'error',
  )
})

test('classifyProbe: null status, null signal, null spawnError → error (ambiguous)', () => {
  assert.equal(classifyProbe({ spawnError: null, status: null, signal: null }), 'error')
})

// ---------------------------------------------------------------------------
// 2. probe — end-to-end against fake binaries
// ---------------------------------------------------------------------------

test('probe: fake that exits 0 → ready', async () => {
  const dir = makeTmpDir()
  try {
    const bin = writeFakeBin(dir, 'codex', 'exit 0')
    const result = await probe({ env: { BOSS_CODEX_BIN: bin }, timeoutMs: PROBE_READY_TIMEOUT_MS })
    assert.equal(result, 'ready')
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('probe: fake that exits non-zero → not_authed', async () => {
  const dir = makeTmpDir()
  try {
    const bin = writeFakeBin(dir, 'codex', 'exit 1')
    const result = await probe({ env: { BOSS_CODEX_BIN: bin }, timeoutMs: 3000 })
    assert.equal(result, 'not_authed')
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('probe: non-existent BOSS_CODEX_BIN → not_installed', async () => {
  const result = await probe({
    env: { BOSS_CODEX_BIN: '/nonexistent/path/to/codex' },
    timeoutMs: 3000,
  })
  assert.equal(result, 'not_installed')
})

test('probe: fake that sleeps past timeoutMs → error', async () => {
  const dir = makeTmpDir()
  try {
    const bin = writeFakeBin(dir, 'codex', 'sleep 30')
    const t0 = Date.now()
    const result = await probe({ env: { BOSS_CODEX_BIN: bin }, timeoutMs: 300 })
    const elapsed = Date.now() - t0
    assert.equal(result, 'error')
    // Should be significantly faster than the 30s sleep
    assert.ok(elapsed < 5000, `probe took too long: ${elapsed}ms`)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

// ---------------------------------------------------------------------------
// 3. resolveCodexBin — precedence rules
// ---------------------------------------------------------------------------

test('resolveCodexBin: BOSS_CODEX_BIN absolute path wins over PATH', () => {
  const dir = makeTmpDir()
  try {
    const bin = writeFakeBin(dir, 'codex', 'exit 0')
    // Even if `codex` is on PATH, BOSS_CODEX_BIN should win
    const result = resolveCodexBin({ BOSS_CODEX_BIN: bin, PATH: '/usr/bin:/bin' })
    assert.equal(result, bin)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('resolveCodexBin: unset BOSS_CODEX_BIN falls back to PATH lookup', () => {
  const dir = makeTmpDir()
  try {
    writeFakeBin(dir, 'codex', 'exit 0')
    const result = resolveCodexBin({ PATH: dir })
    assert.equal(result, path.join(dir, 'codex'))
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('resolveCodexBin: non-existent BOSS_CODEX_BIN → null (resolution failure)', () => {
  const result = resolveCodexBin({ BOSS_CODEX_BIN: '/nonexistent/path/to/codex' })
  assert.equal(result, null)
})

test('resolveCodexBin: relative BOSS_CODEX_BIN → null (must be absolute, no PATH fallback)', () => {
  // A relative override is a misconfiguration: in cron/daemon the cwd is not
  // predictable, so we must NOT execute a cwd-relative binary, and must NOT
  // silently fall back to PATH (which would mask the bad config).
  const dir = makeTmpDir()
  try {
    writeFakeBin(dir, 'codex', 'exit 0') // present on PATH...
    const result = resolveCodexBin({ BOSS_CODEX_BIN: 'codex', PATH: dir })
    assert.equal(result, null, '...but a relative override must still resolve to null')
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('resolveCodexBin: empty string BOSS_CODEX_BIN falls back to PATH', () => {
  const dir = makeTmpDir()
  try {
    writeFakeBin(dir, 'codex', 'exit 0')
    const result = resolveCodexBin({ BOSS_CODEX_BIN: '', PATH: dir })
    assert.equal(result, path.join(dir, 'codex'))
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('resolveCodexBin: codex not on PATH and no BOSS_CODEX_BIN → null', () => {
  const result = resolveCodexBin({ PATH: '/nonexistent-dir-that-has-no-codex' })
  assert.equal(result, null)
})

// ---------------------------------------------------------------------------
// 4. run — timeout kills process group, no throw, timedOut flag
// ---------------------------------------------------------------------------

test('run: fake codex that sleeps is killed, timedOut=true, process group gone, no throw', async () => {
  const dir = makeTmpDir()
  try {
    const pidFile = path.join(dir, 'child.pid')
    // The fake writes its own PID (the process-group leader, since run() spawns
    // detached) to a file, then sleeps. After the timeout kill, the whole group
    // — including this shell — must be gone.
    const bin = writeFakeBin(dir, 'codex', `echo $$ > "${pidFile}"\nsleep 30`)
    const timeoutMs = 1500
    const t0 = Date.now()
    const resultPromise = run({
      env: { BOSS_CODEX_BIN: bin },
      base: 'abc1234',
      head: 'def5678',
      repo: dir,
      timeoutMs,
    })
    const childPid = await waitForPidFile(pidFile)
    const result = await resultPromise
    const elapsed = Date.now() - t0
    assert.equal(result.timedOut, true)
    assert.equal(result.ok, false)
    assert.equal(typeof result.output, 'string')
    // Tight bound: a broken kill (e.g. killing the wrong pid) would blow past 3x.
    assert.ok(elapsed < timeoutMs * 3, `run took too long: ${elapsed}ms`)

    // Assert the process group is actually gone (no orphan). Give the SIGKILL
    // escalation a moment to land, then probe the group with signal 0.
    await new Promise((r) => setTimeout(r, 400))
    let gone = false
    try {
      // Probe the process GROUP (negative pid) the child led. ESRCH = gone.
      process.kill(-childPid, 0)
    } catch (err) {
      gone = err.code === 'ESRCH'
    }
    assert.ok(gone, `process group ${childPid} should be gone (ESRCH)`)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('run: timeout still reaps a child that ignores SIGTERM after the leader exits', async () => {
  const dir = makeTmpDir()
  try {
    const pidFile = path.join(dir, 'leader.pid')
    // The leader records its pid, backgrounds a child that IGNORES SIGTERM (so it
    // only dies to SIGKILL), then `exec sleep` so the leader itself dies promptly
    // on the timeout SIGTERM. The child redirects its stdio to /dev/null so it
    // does NOT hold the leader's stdout/stderr pipes open — that lets Node's
    // `close` fire (and resolve run()) the instant the leader dies, BEFORE the
    // SIGKILL escalation. If settle cancelled that escalation on close, the
    // trapping child would leak past the timeout. This guards exactly that
    // regression: with the bug the group survives; with the fix it is reaped.
    const bin = writeFakeBin(
      dir,
      'codex',
      [
        `echo $$ > "${pidFile}"`,
        "( trap '' TERM; while true; do sleep 0.2; done ) >/dev/null 2>&1 &",
        'exec sleep 30',
      ].join('\n'),
    )
    const timeoutMs = 1500
    const t0 = Date.now()
    const resultPromise = run({
      env: { BOSS_CODEX_BIN: bin },
      base: 'abc1234',
      head: 'def5678',
      repo: dir,
      timeoutMs,
    })
    const leaderPid = await waitForPidFile(pidFile)
    const result = await resultPromise
    const elapsed = Date.now() - t0
    assert.equal(result.timedOut, true)
    assert.equal(result.ok, false)
    assert.ok(elapsed < timeoutMs * 3, `run took too long: ${elapsed}ms`)

    // Allow the SIGKILL escalation (200ms after SIGTERM) to sweep the group.
    await new Promise((r) => setTimeout(r, 500))
    let gone = false
    try {
      // The trapping child shares the leader's process group; ESRCH = reaped.
      process.kill(-leaderPid, 0)
    } catch (err) {
      gone = err.code === 'ESRCH'
    }
    assert.ok(gone, `process group ${leaderPid} should be fully reaped (ESRCH)`)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('run: large stderr volume then exit 0 completes promptly (no stderr deadlock)', async () => {
  const dir = makeTmpDir()
  try {
    // Write ~200KB to stderr (far above any pipe buffer) then a little stdout
    // and exit 0. If stderr is left undrained, this hangs until timeout.
    const bin = writeFakeBin(
      dir,
      'codex',
      'yes "stderr-noise-line-padding-to-bulk-up-bytes" | head -c 200000 1>&2\n' +
        'echo "review: ok"\n' +
        'exit 0',
    )
    const timeoutMs = 5000
    const t0 = Date.now()
    const result = await run({
      env: { BOSS_CODEX_BIN: bin },
      base: 'abc1234',
      head: 'def5678',
      repo: dir,
      timeoutMs,
    })
    const elapsed = Date.now() - t0
    assert.equal(result.ok, true)
    assert.equal(result.timedOut, false)
    assert.ok(result.output.includes('review: ok'))
    // Must finish well under the timeout — a deadlock would only resolve at timeoutMs.
    assert.ok(elapsed < timeoutMs / 2, `run blocked on stderr: ${elapsed}ms`)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('run: fake codex that exits 0 with output → ok=true, sanitized output returned', async () => {
  const dir = makeTmpDir()
  try {
    const bin = writeFakeBin(dir, 'codex', 'echo "review: looks good"; exit 0')
    const result = await run({
      env: { BOSS_CODEX_BIN: bin },
      base: 'abc1234',
      head: 'def5678',
      repo: dir,
      timeoutMs: 5000,
    })
    assert.equal(result.ok, true)
    assert.equal(result.timedOut, false)
    assert.ok(result.output.includes('review: looks good'))
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('run: fake codex that exits non-zero → ok=false, no throw', async () => {
  const dir = makeTmpDir()
  try {
    const bin = writeFakeBin(dir, 'codex', 'echo "partial output"; exit 2')
    const result = await run({
      env: { BOSS_CODEX_BIN: bin },
      base: 'abc1234',
      head: 'def5678',
      repo: dir,
      timeoutMs: 5000,
    })
    assert.equal(result.ok, false)
    assert.equal(result.timedOut, false)
    assert.ok(result.output.includes('partial output'))
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('run: built args are accepted by a flag-validating fake codex (CLI-surface guard)', async () => {
  // This fake mimics the real `codex exec` arg validation: it rejects any
  // -a/--ask-for-approval flag the way codex-cli 0.142.0 does. Because run()
  // builds its argv from buildCodexArgs(), this end-to-end test FAILS if the
  // helper ever reintroduces an unsupported flag — the exact bug that made the
  // outside voice silently produce no review.
  const dir = makeTmpDir()
  try {
    const bin = writeFakeBin(
      dir,
      'codex',
      'for a in "$@"; do\n' +
        '  case "$a" in\n' +
        '    -a|--ask-for-approval) echo "error: unexpected argument \'$a\' found" 1>&2; exit 2;;\n' +
        '  esac\n' +
        'done\n' +
        'echo "review: cross-model ok"\n' +
        'exit 0',
    )
    const result = await run({
      env: { BOSS_CODEX_BIN: bin },
      base: 'abc1234',
      head: 'def5678',
      repo: dir,
      timeoutMs: 5000,
    })
    assert.equal(result.ok, true, `args rejected by fake codex; stderr: ${result.stderr}`)
    assert.equal(result.timedOut, false)
    assert.ok(result.output.includes('review: cross-model ok'))
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('run: a slow diff.external driver cannot hang the bounded run', async () => {
  const dir = makeTmpDir()
  try {
    initRepo(dir)
    fs.writeFileSync(path.join(dir, 'file.txt'), 'first line\n')
    git(dir, ['add', '.'])
    git(dir, ['commit', '-q', '-m', 'base'])
    const base = git(dir, ['rev-parse', 'HEAD'])
    fs.writeFileSync(path.join(dir, 'file.txt'), 'first line\nSENTINEL-ADDED-LINE\n')
    git(dir, ['add', '.'])
    git(dir, ['commit', '-q', '-m', 'head'])
    const head = git(dir, ['rev-parse', 'HEAD'])

    // A pathologically slow external diff driver. Without --no-ext-diff the
    // pre-arm bestEffortDiff would block on this for 30s before the agent runs.
    const slowDiff = writeFakeBin(dir, 'slow-diff', 'sleep 30')
    git(dir, ['config', 'diff.external', slowDiff])

    const bin = writeEchoArgvBin(dir)
    const t0 = Date.now()
    const result = await run({
      env: { BOSS_CODEX_BIN: bin },
      base,
      head,
      repo: dir,
      timeoutMs: 4000,
    })
    const elapsed = Date.now() - t0
    // --no-ext-diff ignores the slow driver entirely, so the real diff embeds
    // fast and the whole run completes well under its deadline (no hang).
    assert.ok(elapsed < 4000, `slow external diff hung the run: ${elapsed}ms`)
    assert.equal(result.timedOut, false)
    assert.ok(
      result.output.includes('+SENTINEL-ADDED-LINE'),
      'plain internal diff should still be embedded despite the slow driver',
    )
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('run: on failure, sanitized stderr tail is captured (not silently empty)', async () => {
  const dir = makeTmpDir()
  try {
    // Emit an ANSI-decorated CLI-surface error to stderr and exit non-zero with
    // no stdout — the pre-fix silent-failure shape. Use a literal single quote
    // (safe inside the double-quoted printf format) rather than a \x27 hex
    // escape: POSIX printf in dash (CI's /bin/sh) does not interpret \x escapes,
    // only octal, so \x27 would survive verbatim and fail the assertion there.
    const bin = writeFakeBin(
      dir,
      'codex',
      'printf "\\033[31merror: unexpected argument \'-a\' found\\033[0m\\n" 1>&2\nexit 2',
    )
    const result = await run({
      env: { BOSS_CODEX_BIN: bin },
      base: 'abc1234',
      head: 'def5678',
      repo: dir,
      timeoutMs: 5000,
    })
    assert.equal(result.ok, false)
    assert.equal(result.output, '', 'no stdout review')
    assert.ok(
      result.stderr.includes("unexpected argument '-a' found"),
      `stderr was: ${result.stderr}`,
    )
    assert.ok(!result.stderr.includes('\x1b'), 'stderr tail should be ANSI-sanitized')
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('run: stderr tail is bounded under a flood, run still completes promptly', async () => {
  const dir = makeTmpDir()
  try {
    // 200KB of stderr, then a non-zero exit. The tail must stay bounded and the
    // most-recent (most diagnostic) bytes must survive.
    const bin = writeFakeBin(
      dir,
      'codex',
      'yes "noise" | head -c 200000 1>&2\nprintf "FINAL-ERROR-MARKER\\n" 1>&2\nexit 2',
    )
    const timeoutMs = 5000
    const t0 = Date.now()
    const result = await run({
      env: { BOSS_CODEX_BIN: bin },
      base: 'abc1234',
      head: 'def5678',
      repo: dir,
      timeoutMs,
      maxStderrBytes: 4096,
    })
    const elapsed = Date.now() - t0
    assert.equal(result.ok, false)
    assert.ok(Buffer.byteLength(result.stderr, 'utf8') <= 4096, 'stderr tail must be bounded')
    assert.ok(result.stderr.includes('FINAL-ERROR-MARKER'), 'tail keeps the most recent bytes')
    assert.ok(elapsed < timeoutMs / 2, `run blocked on stderr flood: ${elapsed}ms`)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('run: stdout is bounded under a flood, output stays capped and completes promptly', async () => {
  const dir = makeTmpDir()
  try {
    // ~2MB of stdout (far above maxBytes) then exit 0. Retained stdout must stay
    // bounded and the returned output must be capped to maxBytes (with marker).
    const bin = writeFakeBin(dir, 'codex', 'yes "review-noise" | head -c 2000000\nexit 0')
    const timeoutMs = 5000
    const maxBytes = 65536
    const t0 = Date.now()
    const result = await run({
      env: { BOSS_CODEX_BIN: bin },
      base: 'abc1234',
      head: 'def5678',
      repo: dir,
      timeoutMs,
      maxBytes,
    })
    const elapsed = Date.now() - t0
    assert.equal(result.ok, true)
    assert.ok(
      Buffer.byteLength(result.output, 'utf8') <= maxBytes,
      'output must be capped to maxBytes',
    )
    assert.ok(
      result.output.includes('[truncated'),
      'flood beyond maxBytes must be marked truncated',
    )
    assert.ok(elapsed < timeoutMs / 2, `run blocked on stdout flood: ${elapsed}ms`)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

// ---------------------------------------------------------------------------
// 5. sanitizeOutput — strip ANSI/control chars, keep \n/\t, cap to maxBytes
// ---------------------------------------------------------------------------

test('sanitizeOutput: strips ANSI escape sequences', () => {
  const input = '[31mred text[0m normal'
  const out = sanitizeOutput(input, {})
  assert.ok(!out.includes(''), 'should not contain ESC')
  assert.ok(out.includes('red text'), 'should keep text content')
  assert.ok(out.includes('normal'), 'should keep trailing text')
})

test('sanitizeOutput: strips CSI private-mode sequences (e.g. cursor hide/show)', () => {
  // ESC[?25l / ESC[?25h carry the `?` private-parameter prefix; a parameter-byte
  // matcher restricted to [0-9;] would strip the bare ESC but leak `[?25l` text.
  const input = 'before\x1b[?25lhide\x1b[?25hafter'
  const out = sanitizeOutput(input, {})
  assert.equal(
    out,
    'beforehideafter',
    'should strip the whole CSI private-mode sequence, not leak [?25l',
  )
})

test('sanitizeOutput: strips control characters but keeps newlines and tabs', () => {
  const input = 'line1\nline2\ttabbed\x01\x02\x03end'
  const out = sanitizeOutput(input, {})
  assert.ok(out.includes('\n'), 'should keep newlines')
  assert.ok(out.includes('\t'), 'should keep tabs')
  assert.ok(!out.includes('\x01'), 'should strip \\x01')
  assert.ok(!out.includes('\x02'), 'should strip \\x02')
  assert.ok(!out.includes('\x03'), 'should strip \\x03')
})

test('sanitizeOutput: strips C1 control chars including 8-bit CSI/OSC', () => {
  // U+009B is the 8-bit CSI, U+009D the 8-bit OSC. Build via String.fromCharCode
  // so the test source itself stays clean ASCII.
  const csi = String.fromCharCode(0x9b)
  const osc = String.fromCharCode(0x9d)
  const pad = String.fromCharCode(0x80)
  const input = `before${csi}31mmiddle${osc}xterm${pad}after`
  const out = sanitizeOutput(input, {})
  assert.ok(!out.includes(csi), 'should strip 8-bit CSI (U+009B)')
  assert.ok(!out.includes(osc), 'should strip 8-bit OSC (U+009D)')
  assert.ok(!out.includes(pad), 'should strip C1 PAD (U+0080)')
  assert.ok(out.includes('before'), 'keeps surrounding text')
  assert.ok(out.includes('after'), 'keeps trailing text')
})

test('sanitizeOutput: caps TOTAL output (content + marker) to maxBytes', () => {
  // maxBytes must be the hard cap on the whole returned string, marker included.
  const input = 'a'.repeat(1000)
  const maxBytes = 200
  const out = sanitizeOutput(input, { maxBytes })
  assert.ok(
    Buffer.byteLength(out, 'utf8') <= maxBytes,
    `total bytes ${Buffer.byteLength(out, 'utf8')} must not exceed cap ${maxBytes}`,
  )
  assert.ok(out.includes('[truncated'), 'should include truncation marker')
  assert.ok(out.startsWith('a'), 'should keep leading content')
})

test('sanitizeOutput: truncation does not split a multi-byte UTF-8 char (no U+FFFD)', () => {
  // '€' is 3 bytes (E2 82 AC). 50 of them = 150 bytes; cap the content budget so
  // the cut lands mid-character. A naive byte slice would emit U+FFFD.
  const input = '€'.repeat(50)
  const out = sanitizeOutput(input, { maxBytes: 100 })
  assert.ok(!out.includes('�'), 'must not contain the replacement character')
  assert.ok(out.includes('[truncated'), 'should include truncation marker')
  assert.ok(Buffer.byteLength(out, 'utf8') <= 100, 'total bytes within cap')
})

test('sanitizeOutput: returns empty string for empty input', () => {
  assert.equal(sanitizeOutput('', {}), '')
})

test('sanitizeOutput: returns empty string for non-string input', () => {
  assert.equal(sanitizeOutput(null, {}), '')
  assert.equal(sanitizeOutput(undefined, {}), '')
  assert.equal(sanitizeOutput(42, {}), '')
  assert.equal(sanitizeOutput({}, {}), '')
})

test('sanitizeOutput: does not truncate when within maxBytes', () => {
  const input = 'hello world'
  const out = sanitizeOutput(input, { maxBytes: 65536 })
  assert.equal(out, input)
})

// ---------------------------------------------------------------------------
// 6. buildCodexArgs — correct flags and safety preamble
// ---------------------------------------------------------------------------

test('buildCodexArgs: includes -s read-only', () => {
  const args = buildCodexArgs({ base: 'abc', head: 'def', repo: '/tmp/repo' })
  assert.ok(args.includes('-s'), 'should have -s flag')
  const idx = args.indexOf('-s')
  assert.equal(args[idx + 1], 'read-only')
})

test('buildCodexArgs: does NOT include -a (codex exec has no approval flag)', () => {
  // Regression guard: `codex exec` (codex-cli 0.142.0) has no -a/--ask-for-approval
  // flag — passing it makes codex exit with "unexpected argument '-a' found" and
  // produce no review. read-only sandbox (-s read-only) is what prevents writes.
  const args = buildCodexArgs({ base: 'abc', head: 'def', repo: '/tmp/repo' })
  assert.ok(!args.includes('-a'), 'must not pass -a to codex exec')
  assert.ok(!args.includes('never'), 'must not pass the never approval value')
})

test('buildCodexArgs: includes model_reasoning_effort="high"', () => {
  const args = buildCodexArgs({ base: 'abc', head: 'def', repo: '/tmp/repo' })
  const hasReasoningEffort = args.some(
    (a) => a.includes('model_reasoning_effort') && a.includes('high'),
  )
  assert.ok(hasReasoningEffort, 'should include model_reasoning_effort="high"')
})

test('buildCodexArgs: includes -C <repo>', () => {
  const args = buildCodexArgs({ base: 'abc', head: 'def', repo: '/my/repo/path' })
  assert.ok(args.includes('-C'), 'should have -C flag')
  const idx = args.indexOf('-C')
  assert.equal(args[idx + 1], '/my/repo/path')
})

test('buildCodexArgs: includes exec subcommand', () => {
  const args = buildCodexArgs({ base: 'abc', head: 'def', repo: '/tmp/repo' })
  assert.ok(args.includes('exec'), 'should include exec subcommand')
})

test('buildCodexArgs: preamble instructs to ignore skill-definition dirs', () => {
  const args = buildCodexArgs({ base: 'abc', head: 'def', repo: '/tmp/repo' })
  // The prompt is one of the string args — find it
  const prompt = args.find(
    (a) => a.includes('.claude') || a.includes('skills') || a.includes('agents'),
  )
  assert.ok(prompt, 'preamble should mention skill dirs to ignore')
  assert.ok(
    prompt.includes('.claude/') || prompt.includes('~/.claude'),
    'should mention ~/.claude/',
  )
  assert.ok(
    prompt.includes('skills') || prompt.includes('agents'),
    'should mention skills or agents dir',
  )
})

test('buildCodexArgs: preamble instructs to override/ignore AGENTS.md and CLAUDE.md', () => {
  const args = buildCodexArgs({ base: 'abc', head: 'def', repo: '/tmp/repo' })
  const prompt = args.find((a) => a.includes('AGENTS.md') || a.includes('CLAUDE.md'))
  assert.ok(prompt, 'preamble must mention AGENTS.md and CLAUDE.md override')
  assert.ok(prompt.includes('AGENTS.md'), 'should mention AGENTS.md')
  assert.ok(prompt.includes('CLAUDE.md'), 'should mention CLAUDE.md')
})

test('buildCodexArgs: preamble instructs to treat output as data, not instructions', () => {
  const args = buildCodexArgs({ base: 'abc', head: 'def', repo: '/tmp/repo' })
  const prompt = args.find(
    (a) =>
      (a.toLowerCase().includes('data') || a.toLowerCase().includes('instruction')) &&
      a.length > 50,
  )
  assert.ok(prompt, 'preamble should address data-not-instructions constraint')
})

test('buildCodexArgs: preamble mentions BASE...HEAD range', () => {
  const args = buildCodexArgs({ base: 'abc123', head: 'def456', repo: '/tmp/repo' })
  const prompt = args.find((a) => a.includes('abc123') && a.includes('def456'))
  assert.ok(prompt, 'preamble should mention the base...head range')
})

// ---------------------------------------------------------------------------
// CLI smoke tests
// ---------------------------------------------------------------------------

test('CLI probe — exits 0 and prints a valid classification', () => {
  // We can't use a real codex, but we can verify the script exits 0
  // when BOSS_CODEX_BIN points to a fake binary.
  const dir = makeTmpDir()
  try {
    const bin = writeFakeBin(dir, 'codex', 'exit 0')
    const result = spawnSync(process.execPath, [SCRIPT_PATH, 'probe'], {
      encoding: 'utf8',
      env: { ...process.env, BOSS_CODEX_BIN: bin },
    })
    assert.equal(result.status, 0, `stderr: ${result.stderr}`)
    const out = result.stdout.trim()
    assert.ok(
      ['ready', 'not_installed', 'not_authed', 'error'].includes(out),
      `unexpected output: ${out}`,
    )
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('CLI probe — prints "ready" when fake binary exits 0', () => {
  const dir = makeTmpDir()
  try {
    const bin = writeFakeBin(dir, 'codex', 'exit 0')
    const result = spawnSync(process.execPath, [SCRIPT_PATH, 'probe'], {
      encoding: 'utf8',
      env: { ...process.env, BOSS_CODEX_BIN: bin },
    })
    assert.equal(result.status, 0)
    assert.equal(result.stdout.trim(), 'ready')
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('CLI — unknown command exits 1', () => {
  const result = spawnSync(process.execPath, [SCRIPT_PATH, 'bogus'], { encoding: 'utf8' })
  assert.equal(result.status, 1)
  assert.match(result.stderr, /unknown command/)
})

test('CLI run — requires --base and --head flags', () => {
  const result = spawnSync(process.execPath, [SCRIPT_PATH, 'run', '--base', 'abc'], {
    encoding: 'utf8',
  })
  assert.equal(result.status, 1)
  assert.match(result.stderr, /--head/)
})
