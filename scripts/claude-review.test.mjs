#!/usr/bin/env node

import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  buildClaudeArgs,
  classifyProbe,
  probe,
  resolveClaudeBin,
  resolveTimeoutMs,
  run,
  sanitizeOutput,
} from './claude-review.mjs'

const SCRIPT_PATH = fileURLToPath(new URL('./claude-review.mjs', import.meta.url))

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/** Create a temporary directory and return its path. Caller is responsible for cleanup. */
function makeTmpDir() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'claude-review-test-'))
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

// A fake claude that prints every argv element on its own line then exits 0.
// run() returns the sanitized stdout, so the embedded prompt (the last arg) is
// fully observable for assertions.
function writeEchoArgvBin(dir) {
  return writeFakeBin(dir, 'claude', 'for a in "$@"; do printf "%s\\n" "$a"; done\nexit 0')
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

/**
 * Report whether every remaining member of process group `pgid` is a zombie
 * (`<defunct>`). A killed child that has been reparented to PID 1 lingers in the
 * process table as a zombie until PID 1 reaps it, so `process.kill(-pgid, 0)`
 * keeps succeeding for that window even though nothing is actually running.
 * Zombies are dead — they just have not been reaped — so we treat them as gone.
 * Returns false if `ps` is unavailable or any live (non-`Z`) member remains.
 */
function processGroupOnlyZombies(pgid) {
  const res = spawnSync('ps', ['-eo', 'pid=,pgid=,stat='], { encoding: 'utf8' })
  if (res.status !== 0 || typeof res.stdout !== 'string') return false
  const members = res.stdout
    .split('\n')
    .map((line) => line.trim().split(/\s+/))
    .filter((fields) => fields.length >= 3 && Number(fields[1]) === pgid)
  // No group members left, or every survivor is a zombie (state starts with Z).
  return members.every((fields) => fields[2].startsWith('Z'))
}

/**
 * Poll until process group `pgid` is gone: either `process.kill(-pgid, 0)`
 * throws ESRCH, or the only survivors are unreaped zombies. Returns true when
 * the group is gone, false if it is still live after `timeoutMs`.
 */
async function waitForGroupGone(pgid, timeoutMs = 3000) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    try {
      process.kill(-pgid, 0)
    } catch (err) {
      if (err.code === 'ESRCH') return true
      // EPERM (exists but not ours) — fall through to the zombie check.
    }
    if (processGroupOnlyZombies(pgid)) return true
    if (Date.now() >= deadline) return false
    await new Promise((r) => setTimeout(r, 50))
  }
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
// 1. classifyProbe — pure classifier (re-exported from the lib)
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

test('classifyProbe: killed by signal → error', () => {
  assert.equal(classifyProbe({ spawnError: null, status: null, signal: 'SIGKILL' }), 'error')
})

test('classifyProbe: null status, null signal, null spawnError → error (ambiguous)', () => {
  assert.equal(classifyProbe({ spawnError: null, status: null, signal: null }), 'error')
})

// ---------------------------------------------------------------------------
// 2. probe — end-to-end against fake binaries (`claude --version`)
// ---------------------------------------------------------------------------

test('probe: fake that exits 0 → ready', async () => {
  const dir = makeTmpDir()
  try {
    const bin = writeFakeBin(dir, 'claude', 'exit 0')
    const result = await probe({ env: { BOSS_CLAUDE_BIN: bin }, timeoutMs: 3000 })
    assert.equal(result, 'ready')
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('probe: non-existent BOSS_CLAUDE_BIN → not_installed', async () => {
  const result = await probe({
    env: { BOSS_CLAUDE_BIN: '/nonexistent/path/to/claude' },
    timeoutMs: 3000,
  })
  assert.equal(result, 'not_installed')
})

test('probe: fake that exits non-zero → error (not not_authed)', async () => {
  // `claude --version` has no auth semantics, so a non-zero exit is a broken
  // CLI, not "not authenticated" (codex's meaning of a non-zero login-status).
  const dir = makeTmpDir()
  try {
    const bin = writeFakeBin(dir, 'claude', 'exit 1')
    const result = await probe({ env: { BOSS_CLAUDE_BIN: bin }, timeoutMs: 3000 })
    assert.equal(result, 'error')
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('probe: fake that sleeps past timeoutMs → error', async () => {
  const dir = makeTmpDir()
  try {
    const bin = writeFakeBin(dir, 'claude', 'sleep 30')
    const t0 = Date.now()
    const result = await probe({ env: { BOSS_CLAUDE_BIN: bin }, timeoutMs: 300 })
    const elapsed = Date.now() - t0
    assert.equal(result, 'error')
    // Should be significantly faster than the 30s sleep
    assert.ok(elapsed < 5000, `probe took too long: ${elapsed}ms`)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

// ---------------------------------------------------------------------------
// 3. resolveClaudeBin — precedence rules
// ---------------------------------------------------------------------------

test('resolveClaudeBin: BOSS_CLAUDE_BIN absolute path wins over PATH', () => {
  const dir = makeTmpDir()
  try {
    const bin = writeFakeBin(dir, 'claude', 'exit 0')
    const result = resolveClaudeBin({ BOSS_CLAUDE_BIN: bin, PATH: '/usr/bin:/bin' })
    assert.equal(result, bin)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('resolveClaudeBin: unset BOSS_CLAUDE_BIN falls back to PATH lookup', () => {
  const dir = makeTmpDir()
  try {
    writeFakeBin(dir, 'claude', 'exit 0')
    const result = resolveClaudeBin({ PATH: dir })
    assert.equal(result, path.join(dir, 'claude'))
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('resolveClaudeBin: non-existent BOSS_CLAUDE_BIN → null (resolution failure)', () => {
  const result = resolveClaudeBin({ BOSS_CLAUDE_BIN: '/nonexistent/path/to/claude' })
  assert.equal(result, null)
})

test('resolveClaudeBin: relative BOSS_CLAUDE_BIN → null (must be absolute, no PATH fallback)', () => {
  const dir = makeTmpDir()
  try {
    writeFakeBin(dir, 'claude', 'exit 0') // present on PATH...
    const result = resolveClaudeBin({ BOSS_CLAUDE_BIN: 'claude', PATH: dir })
    assert.equal(result, null, '...but a relative override must still resolve to null')
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('resolveClaudeBin: empty string BOSS_CLAUDE_BIN falls back to PATH', () => {
  const dir = makeTmpDir()
  try {
    writeFakeBin(dir, 'claude', 'exit 0')
    const result = resolveClaudeBin({ BOSS_CLAUDE_BIN: '', PATH: dir })
    assert.equal(result, path.join(dir, 'claude'))
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('resolveClaudeBin: claude not on PATH and no BOSS_CLAUDE_BIN → null', () => {
  const result = resolveClaudeBin({ PATH: '/nonexistent-dir-that-has-no-claude' })
  assert.equal(result, null)
})

// ---------------------------------------------------------------------------
// 4. buildClaudeArgs — print mode, read-only, safety preamble, no -C
// ---------------------------------------------------------------------------

test('buildClaudeArgs: includes -p (print / non-interactive)', () => {
  const args = buildClaudeArgs({ base: 'abc', head: 'def' })
  assert.ok(args.includes('-p'), 'should have -p print flag')
})

test('buildClaudeArgs: includes --permission-mode plan (read-only)', () => {
  const args = buildClaudeArgs({ base: 'abc', head: 'def' })
  assert.ok(args.includes('--permission-mode'), 'should have --permission-mode flag')
  const idx = args.indexOf('--permission-mode')
  assert.equal(args[idx + 1], 'plan')
})

test('buildClaudeArgs: includes --bare (hermetic) when ANTHROPIC_API_KEY is set', () => {
  const args = buildClaudeArgs({ base: 'abc', head: 'def', env: { ANTHROPIC_API_KEY: 'sk-test' } })
  assert.ok(args.includes('--bare'), 'bare mode should be enabled when the API key is present')
  // --bare must precede the prompt (a flag, not the positional prompt).
  assert.ok(args.indexOf('--bare') < args.length - 1, '--bare must not be the final (prompt) arg')
})

test('buildClaudeArgs: omits --bare when ANTHROPIC_API_KEY is absent (preserves OAuth/keychain auth)', () => {
  const args = buildClaudeArgs({ base: 'abc', head: 'def', env: {} })
  assert.ok(
    !args.includes('--bare'),
    'bare mode must be off without an API key, or auth would break',
  )
})

test('buildClaudeArgs: omits --bare when ANTHROPIC_API_KEY is empty/whitespace', () => {
  assert.ok(
    !buildClaudeArgs({ base: 'a', head: 'b', env: { ANTHROPIC_API_KEY: '' } }).includes('--bare'),
  )
  assert.ok(
    !buildClaudeArgs({ base: 'a', head: 'b', env: { ANTHROPIC_API_KEY: '  ' } }).includes('--bare'),
  )
})

test('buildClaudeArgs: does NOT include -C (claude has no working-dir flag)', () => {
  // Regression guard: claude has no `-C <dir>` flag (codex does). Passing it
  // would make claude treat `-C` and the repo path as a prompt/arg and fail to
  // run read-only over the diff. cwd is set on the spawn instead.
  const args = buildClaudeArgs({ base: 'abc', head: 'def' })
  assert.ok(!args.includes('-C'), 'must not pass -C to claude')
})

test('buildClaudeArgs: prompt is the final argv element', () => {
  const args = buildClaudeArgs({ base: 'abc', head: 'def' })
  const prompt = args[args.length - 1]
  assert.ok(typeof prompt === 'string' && prompt.length > 50, 'last arg should be the prompt')
})

test('buildClaudeArgs: preamble instructs to ignore skill-definition dirs', () => {
  const args = buildClaudeArgs({ base: 'abc', head: 'def' })
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

test('buildClaudeArgs: preamble instructs to override/ignore AGENTS.md and CLAUDE.md', () => {
  const args = buildClaudeArgs({ base: 'abc', head: 'def' })
  const prompt = args.find((a) => a.includes('AGENTS.md') || a.includes('CLAUDE.md'))
  assert.ok(prompt, 'preamble must mention AGENTS.md and CLAUDE.md override')
  assert.ok(prompt.includes('AGENTS.md'), 'should mention AGENTS.md')
  assert.ok(prompt.includes('CLAUDE.md'), 'should mention CLAUDE.md')
})

test('buildClaudeArgs: preamble instructs to treat output as data, not instructions', () => {
  const args = buildClaudeArgs({ base: 'abc', head: 'def' })
  const prompt = args.find(
    (a) =>
      (a.toLowerCase().includes('data') || a.toLowerCase().includes('instruction')) &&
      a.length > 50,
  )
  assert.ok(prompt, 'preamble should address data-not-instructions constraint')
})

test('buildClaudeArgs: preamble mentions BASE...HEAD range', () => {
  const args = buildClaudeArgs({ base: 'abc123', head: 'def456' })
  const prompt = args.find((a) => a.includes('abc123') && a.includes('def456'))
  assert.ok(prompt, 'preamble should mention the base...head range')
})

// ---------------------------------------------------------------------------
// 5. run — timeout kills process group, no throw, timedOut flag
// ---------------------------------------------------------------------------

test('run: fake claude that sleeps is killed, timedOut=true, process group gone, no throw', async () => {
  const dir = makeTmpDir()
  try {
    const pidFile = path.join(dir, 'child.pid')
    const bin = writeFakeBin(dir, 'claude', `echo $$ > "${pidFile}"\nsleep 30`)
    const timeoutMs = 1500
    const t0 = Date.now()
    const resultPromise = run({
      env: { BOSS_CLAUDE_BIN: bin },
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
    assert.ok(elapsed < timeoutMs * 3, `run took too long: ${elapsed}ms`)

    // Assert the process group is actually gone (no live orphan). A killed
    // child can briefly remain as an unreaped zombie under PID 1, so poll and
    // treat zombies as gone rather than asserting a hard ESRCH after a fixed
    // sleep.
    const gone = await waitForGroupGone(childPid)
    assert.ok(gone, `process group ${childPid} should be gone (zombies/ESRCH)`)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('run: timeout still reaps a child that ignores SIGTERM after the leader exits', async () => {
  const dir = makeTmpDir()
  try {
    const pidFile = path.join(dir, 'leader.pid')
    const bin = writeFakeBin(
      dir,
      'claude',
      [
        `echo $$ > "${pidFile}"`,
        "( trap '' TERM; while true; do sleep 0.2; done ) >/dev/null 2>&1 &",
        'exec sleep 30',
      ].join('\n'),
    )
    const timeoutMs = 1500
    const t0 = Date.now()
    const resultPromise = run({
      env: { BOSS_CLAUDE_BIN: bin },
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

    // A SIGTERM-ignoring child that outlives the leader gets SIGKILLed and may
    // linger as an unreaped zombie under PID 1; poll and treat zombies as gone.
    const gone = await waitForGroupGone(leaderPid)
    assert.ok(gone, `process group ${leaderPid} should be gone (zombies/ESRCH)`)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('run: large stderr volume then exit 0 completes promptly (no stderr deadlock)', async () => {
  const dir = makeTmpDir()
  try {
    const bin = writeFakeBin(
      dir,
      'claude',
      'yes "stderr-noise-line-padding-to-bulk-up-bytes" | head -c 200000 1>&2\n' +
        'echo "review: ok"\n' +
        'exit 0',
    )
    const timeoutMs = 5000
    const t0 = Date.now()
    const result = await run({
      env: { BOSS_CLAUDE_BIN: bin },
      base: 'abc1234',
      head: 'def5678',
      repo: dir,
      timeoutMs,
    })
    const elapsed = Date.now() - t0
    assert.equal(result.ok, true)
    assert.equal(result.timedOut, false)
    assert.ok(result.output.includes('review: ok'))
    assert.ok(elapsed < timeoutMs / 2, `run blocked on stderr: ${elapsed}ms`)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('run: fake claude that exits 0 with output → ok=true, sanitized output returned', async () => {
  const dir = makeTmpDir()
  try {
    const bin = writeFakeBin(dir, 'claude', 'echo "review: looks good"; exit 0')
    const result = await run({
      env: { BOSS_CLAUDE_BIN: bin },
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

test('run: fake claude that exits non-zero → ok=false, no throw, stderr tail captured', async () => {
  const dir = makeTmpDir()
  try {
    const bin = writeFakeBin(
      dir,
      'claude',
      'printf "\\033[31mError: authentication required\\033[0m\\n" 1>&2\nexit 1',
    )
    const result = await run({
      env: { BOSS_CLAUDE_BIN: bin },
      base: 'abc1234',
      head: 'def5678',
      repo: dir,
      timeoutMs: 5000,
    })
    assert.equal(result.ok, false)
    assert.equal(result.timedOut, false)
    assert.equal(result.output, '', 'no stdout review')
    assert.ok(
      result.stderr.includes('authentication required'),
      `stderr tail should be captured, was: ${result.stderr}`,
    )
    assert.ok(!result.stderr.includes('\x1b'), 'stderr tail should be ANSI-sanitized')
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('run: stderr tail is bounded under a flood, run still completes promptly', async () => {
  const dir = makeTmpDir()
  try {
    const bin = writeFakeBin(
      dir,
      'claude',
      'yes "noise" | head -c 200000 1>&2\nprintf "FINAL-ERROR-MARKER\\n" 1>&2\nexit 2',
    )
    const timeoutMs = 5000
    const t0 = Date.now()
    const result = await run({
      env: { BOSS_CLAUDE_BIN: bin },
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
    const bin = writeFakeBin(dir, 'claude', 'yes "review-noise" | head -c 2000000\nexit 0')
    const timeoutMs = 5000
    const maxBytes = 65536
    const t0 = Date.now()
    const result = await run({
      env: { BOSS_CLAUDE_BIN: bin },
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
// 6. run — diff embedding against a REAL git repo (echo-argv fake)
// ---------------------------------------------------------------------------

test('run: small diff in a real git repo is embedded under the cap', async () => {
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

    const bin = writeEchoArgvBin(dir)
    const result = await run({
      env: { BOSS_CLAUDE_BIN: bin },
      base,
      head,
      repo: dir,
      timeoutMs: 5000,
    })

    assert.equal(result.ok, true, `run failed; stderr: ${result.stderr}`)
    assert.equal(result.timedOut, false)
    // -p and --permission-mode plan must be present in the spawned argv.
    assert.ok(result.output.includes('-p'), 'spawned argv should include -p')
    assert.ok(
      result.output.includes('--permission-mode'),
      'spawned argv should include --permission-mode',
    )
    assert.ok(
      result.output.includes('plan'),
      'spawned argv should include the plan permission mode',
    )
    // Embed-mode delimiter from assemblePrompt's embed branch, with the range.
    assert.ok(
      result.output.includes(`===== BEGIN DIFF (${base}...${head}) =====`),
      'prompt should embed the diff under a BEGIN DIFF delimiter',
    )
    assert.ok(result.output.includes('===== END DIFF'), 'prompt should close the embedded diff')
    assert.ok(
      result.output.includes('+SENTINEL-ADDED-LINE'),
      'embedded diff should contain the added line',
    )
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
      env: { BOSS_CLAUDE_BIN: bin },
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

test('run: spawns claude with --bare when ANTHROPIC_API_KEY is set (hermetic reviewer)', async () => {
  const dir = makeTmpDir()
  try {
    const bin = writeEchoArgvBin(dir)
    const withKey = await run({
      env: { BOSS_CLAUDE_BIN: bin, ANTHROPIC_API_KEY: 'sk-test' },
      base: 'abc1234',
      head: 'def5678',
      repo: dir,
      timeoutMs: 5000,
    })
    assert.equal(withKey.ok, true, `run failed; stderr: ${withKey.stderr}`)
    assert.ok(withKey.output.includes('--bare'), 'API-keyed run should pass --bare')

    // Without a key, --bare would break OAuth/keychain auth, so it must be off.
    const noKey = await run({
      env: { BOSS_CLAUDE_BIN: bin },
      base: 'abc1234',
      head: 'def5678',
      repo: dir,
      timeoutMs: 5000,
    })
    assert.equal(noKey.ok, true, `run failed; stderr: ${noKey.stderr}`)
    assert.ok(!noKey.output.includes('--bare'), 'keyless run must not pass --bare')
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('run: over-cap diff (>=200KB) falls back to instruct-mode, no embedded diff', async () => {
  const dir = makeTmpDir()
  try {
    initRepo(dir)
    fs.writeFileSync(path.join(dir, 'file.txt'), 'seed\n')
    git(dir, ['add', '.'])
    git(dir, ['commit', '-q', '-m', 'base'])
    const base = git(dir, ['rev-parse', 'HEAD'])

    const big = `${'x'.repeat(80)}\n`.repeat(3500)
    fs.writeFileSync(path.join(dir, 'big.txt'), big)
    git(dir, ['add', '.'])
    git(dir, ['commit', '-q', '-m', 'head'])
    const head = git(dir, ['rev-parse', 'HEAD'])

    const diffBytes = Buffer.byteLength(git(dir, ['diff', `${base}...${head}`]) + '\n', 'utf8')
    assert.ok(diffBytes >= 200 * 1024, `diff should exceed 200KB cap, got ${diffBytes}`)

    const bin = writeEchoArgvBin(dir)
    const result = await run({
      env: { BOSS_CLAUDE_BIN: bin },
      base,
      head,
      repo: dir,
      timeoutMs: 5000,
    })

    assert.equal(result.ok, true, `run failed; stderr: ${result.stderr}`)
    assert.equal(result.timedOut, false)
    assert.ok(!result.output.includes('BEGIN DIFF'), 'over-cap diff must NOT be embedded')
    assert.ok(
      /do NOT run find\/ls\/grep\/cat/i.test(result.output),
      'instruct-mode carries the no-tree-exploration scope guard',
    )
    assert.ok(
      result.output.includes(`${base}...${head}`),
      'instruct-mode prompt still names the range',
    )
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

// ---------------------------------------------------------------------------
// 7. resolveTimeoutMs — strict parsing rejects trailing garbage
// ---------------------------------------------------------------------------

test('resolveTimeoutMs: valid positive integer is honored', () => {
  assert.equal(resolveTimeoutMs({ BOSS_CROSS_REVIEW_TIMEOUT_MS: '120000' }), 120000)
})

test('resolveTimeoutMs: unset / empty / non-positive / garbage → default 300000', () => {
  assert.equal(resolveTimeoutMs({}), 300000)
  assert.equal(resolveTimeoutMs({ BOSS_CROSS_REVIEW_TIMEOUT_MS: '' }), 300000)
  assert.equal(resolveTimeoutMs({ BOSS_CROSS_REVIEW_TIMEOUT_MS: '0' }), 300000)
  assert.equal(resolveTimeoutMs({ BOSS_CROSS_REVIEW_TIMEOUT_MS: '-5' }), 300000)
  assert.equal(resolveTimeoutMs({ BOSS_CROSS_REVIEW_TIMEOUT_MS: '100abc' }), 300000)
  assert.equal(resolveTimeoutMs({ BOSS_CROSS_REVIEW_TIMEOUT_MS: '1.5' }), 300000)
})

// ---------------------------------------------------------------------------
// 8. sanitizeOutput — re-exported from the lib (smoke coverage)
// ---------------------------------------------------------------------------

test('sanitizeOutput: strips ANSI escape sequences but keeps text', () => {
  const input = '\x1b[31mred text\x1b[0m normal'
  const out = sanitizeOutput(input, {})
  assert.ok(!out.includes('\x1b'), 'should not contain ESC')
  assert.ok(out.includes('red text'), 'should keep text content')
  assert.ok(out.includes('normal'), 'should keep trailing text')
})

test('sanitizeOutput: returns empty string for non-string input', () => {
  assert.equal(sanitizeOutput(null, {}), '')
  assert.equal(sanitizeOutput(undefined, {}), '')
})

// ---------------------------------------------------------------------------
// 9. CLI smoke tests
// ---------------------------------------------------------------------------

test('CLI probe — prints "ready" when fake binary exits 0', () => {
  const dir = makeTmpDir()
  try {
    const bin = writeFakeBin(dir, 'claude', 'exit 0')
    const result = spawnSync(process.execPath, [SCRIPT_PATH, 'probe'], {
      encoding: 'utf8',
      env: { ...process.env, BOSS_CLAUDE_BIN: bin },
    })
    assert.equal(result.status, 0, `stderr: ${result.stderr}`)
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
