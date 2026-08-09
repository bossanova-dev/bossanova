#!/usr/bin/env node

// Integration coverage for run()'s diff-embedding path against a REAL git repo.
//
// The existing run() tests all use a non-git tmpdir, so bestEffortDiff always
// returns '' and run() always takes instruct-mode. These tests exercise the
// headline behavior end-to-end: fetch `git diff base...head`, embed it under the
// EMBED_DIFF_LIMIT_BYTES cap, and fall back to instruct-mode over the cap.
//
// Strategy: `git init` a temp repo, make two commits, point run() at it with a
// FAKE `codex` (via BOSS_CODEX_BIN) that echoes its argv to stdout. run()
// captures+sanitizes stdout, so the spawned prompt arg (the last argv element)
// is observable in result.output. We then assert on the embed-mode delimiter
// (`===== BEGIN DIFF (base...head) =====`, emitted by assemblePrompt's embed
// branch) vs. the instruct-mode scope guard.

import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

import { resolveTimeoutMs, run } from './codex-review.mjs'

// ---------------------------------------------------------------------------
// Helpers (replicated locally from the codex-review.test.mjs pattern)
// ---------------------------------------------------------------------------

function makeTmpDir() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'codex-review-embed-test-'))
}

// A fake codex that prints every argv element on its own line then exits 0.
// run() returns the sanitized stdout, so the embedded prompt (the last arg) is
// fully observable for assertions.
function writeEchoArgvBin(dir) {
  const p = path.join(dir, 'codex')
  fs.writeFileSync(p, '#!/bin/sh\nfor a in "$@"; do printf "%s\\n" "$a"; done\nexit 0\n')
  fs.chmodSync(p, 0o755)
  return p
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
// run() embed-mode — small diff is fetched and embedded verbatim
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
      env: { BOSS_CODEX_BIN: bin },
      base,
      head,
      repo: dir,
      timeoutMs: 5000,
    })

    assert.equal(result.ok, true, `run failed; stderr: ${result.stderr}`)
    assert.equal(result.timedOut, false)
    // Embed-mode delimiter from assemblePrompt's embed branch, with the range.
    assert.ok(
      result.output.includes(`===== BEGIN DIFF (${base}...${head}) =====`),
      'prompt should embed the diff under a BEGIN DIFF delimiter',
    )
    assert.ok(result.output.includes('===== END DIFF'), 'prompt should close the embedded diff')
    // The actual diff content (the added line) must be present in the embed.
    assert.ok(
      result.output.includes('+SENTINEL-ADDED-LINE'),
      'embedded diff should contain the added line',
    )
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

// ---------------------------------------------------------------------------
// run() instruct-mode fallback — an over-cap diff is NOT embedded
// ---------------------------------------------------------------------------

test('run: over-cap diff (>=200KB) falls back to instruct-mode, no embedded diff', async () => {
  const dir = makeTmpDir()
  try {
    initRepo(dir)
    fs.writeFileSync(path.join(dir, 'file.txt'), 'seed\n')
    git(dir, ['add', '.'])
    git(dir, ['commit', '-q', '-m', 'base'])
    const base = git(dir, ['rev-parse', 'HEAD'])

    // Build a > EMBED_DIFF_LIMIT_BYTES (200KB) diff: ~3500 added lines of 80
    // chars → each becomes a `+<80 chars>\n` (82 bytes) line in the unified diff,
    // comfortably above 204800 bytes so bestEffortDiff returns '' → instruct-mode.
    const big = `${'x'.repeat(80)}\n`.repeat(3500)
    fs.writeFileSync(path.join(dir, 'big.txt'), big)
    git(dir, ['add', '.'])
    git(dir, ['commit', '-q', '-m', 'head'])
    const head = git(dir, ['rev-parse', 'HEAD'])

    // Sanity-check the diff really exceeds the cap so the test asserts the
    // intended branch (not an accidentally-small diff).
    const diffBytes = Buffer.byteLength(git(dir, ['diff', `${base}...${head}`]) + '\n', 'utf8')
    assert.ok(diffBytes >= 200 * 1024, `diff should exceed 200KB cap, got ${diffBytes}`)

    const bin = writeEchoArgvBin(dir)
    const result = await run({
      env: { BOSS_CODEX_BIN: bin },
      base,
      head,
      repo: dir,
      timeoutMs: 5000,
    })

    assert.equal(result.ok, true, `run failed; stderr: ${result.stderr}`)
    assert.equal(result.timedOut, false)
    // Over the cap → no embedded diff.
    assert.ok(!result.output.includes('BEGIN DIFF'), 'over-cap diff must NOT be embedded')
    // Instruct-mode scope guard + range must be present.
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
// resolveTimeoutMs — strict parsing rejects trailing garbage
// ---------------------------------------------------------------------------

test('resolveTimeoutMs: valid positive integer is honored', () => {
  assert.equal(resolveTimeoutMs({ BOSS_CROSS_REVIEW_TIMEOUT_MS: '120000' }), 120000)
})

test('resolveTimeoutMs: unset / empty / non-positive / garbage → default 300000', () => {
  assert.equal(resolveTimeoutMs({}), 300000)
  assert.equal(resolveTimeoutMs({ BOSS_CROSS_REVIEW_TIMEOUT_MS: '' }), 300000)
  assert.equal(resolveTimeoutMs({ BOSS_CROSS_REVIEW_TIMEOUT_MS: '0' }), 300000)
  assert.equal(resolveTimeoutMs({ BOSS_CROSS_REVIEW_TIMEOUT_MS: '-5' }), 300000)
  // Strict Number() parsing rejects trailing garbage (parseInt would yield 100).
  assert.equal(resolveTimeoutMs({ BOSS_CROSS_REVIEW_TIMEOUT_MS: '100abc' }), 300000)
  assert.equal(resolveTimeoutMs({ BOSS_CROSS_REVIEW_TIMEOUT_MS: '1.5' }), 300000)
})

test('resolveTimeoutMs: exotic Number() forms the shell gates cannot express → default 300000', () => {
  // BOS-758 round 3. These are all LEGAL Number() input and a bare `Number()` honoured every one of
  // them, but the budget gates that price this timeout are POSIX shell and their normalization is a
  // digits-only `case` glob plus `$(( 10# ))` — no glob can match an exponent, a sign, a hex prefix,
  // or leading whitespace. Each accepted-here/rejected-there form is a leg the gate reserves the
  // 300000 ms default for while the helper grants the override: `1.8e6` reserves five minutes for a
  // thirty-minute Codex leg and the overrun lands in the post-review reserve. Rejecting them here is
  // what makes "plain positive decimal digits" the one contract BOTH readers implement.
  for (const raw of ['1.8e6', '1e3', '0x2710', '0b11', '0o17', '+600000', ' 1800000', '1800000 ']) {
    assert.equal(
      resolveTimeoutMs({ BOSS_CROSS_REVIEW_TIMEOUT_MS: raw }),
      300000,
      `${JSON.stringify(raw)} is not plain decimal digits and must take the default`,
    )
  }
})

test('resolveTimeoutMs: plain digits stay honored, leading zeros included', () => {
  // The paired positive: the gate above must not be satisfiable by a resolver that simply refuses
  // everything. A zero-padded POSITIVE is plain digits, so it is a real override — and it is the one
  // both readers agree on, because the shell side converts through an explicit base-10 radix rather
  // than letting `$(( ))` read the leading zero as octal.
  assert.equal(resolveTimeoutMs({ BOSS_CROSS_REVIEW_TIMEOUT_MS: '0600000' }), 600000)
  assert.equal(resolveTimeoutMs({ BOSS_CROSS_REVIEW_TIMEOUT_MS: '08' }), 8)
  assert.equal(resolveTimeoutMs({ BOSS_CROSS_REVIEW_TIMEOUT_MS: '1800000' }), 1800000)
})
