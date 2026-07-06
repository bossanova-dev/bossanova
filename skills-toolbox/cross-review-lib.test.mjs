#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

import {
  assemblePrompt,
  boundedStderrTail,
  buildExecArgv,
  classifyProbe,
  interpretResult,
  REVIEW_PREAMBLE,
  resolveAgentBin,
  sanitizeOutput,
  sliceLenUtf8Safe,
} from './cross-review-lib.mjs'

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function makeTmpDir() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'cross-review-lib-test-'))
}

function writeFakeBin(dir, name, body) {
  const p = path.join(dir, name)
  fs.writeFileSync(p, `#!/bin/sh\n${body}\n`)
  fs.chmodSync(p, 0o755)
  return p
}

// ---------------------------------------------------------------------------
// classifyProbe — every branch
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

test('classifyProbe: numeric non-zero status → not_authed', () => {
  assert.equal(classifyProbe({ spawnError: null, status: 1, signal: null }), 'not_authed')
  assert.equal(classifyProbe({ spawnError: null, status: 7, signal: null }), 'not_authed')
})

test('classifyProbe: signal → error', () => {
  assert.equal(classifyProbe({ spawnError: null, status: null, signal: 'SIGKILL' }), 'error')
})

test('classifyProbe: non-ENOENT spawnError → error', () => {
  assert.equal(
    classifyProbe({ spawnError: { code: 'EACCES' }, status: null, signal: null }),
    'error',
  )
})

test('classifyProbe: ambiguous (null status/signal/error) → error', () => {
  assert.equal(classifyProbe({ spawnError: null, status: null, signal: null }), 'error')
})

// ---------------------------------------------------------------------------
// resolveAgentBin — generalized resolver
// ---------------------------------------------------------------------------

test('resolveAgentBin: absolute override wins over PATH', () => {
  const dir = makeTmpDir()
  try {
    const bin = writeFakeBin(dir, 'codex', 'exit 0')
    const result = resolveAgentBin(
      { BOSS_CODEX_BIN: bin, PATH: '/usr/bin:/bin' },
      { overrideVar: 'BOSS_CODEX_BIN', binName: 'codex' },
    )
    assert.equal(result, bin)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('resolveAgentBin: unset override falls back to PATH lookup', () => {
  const dir = makeTmpDir()
  try {
    writeFakeBin(dir, 'codex', 'exit 0')
    const result = resolveAgentBin(
      { PATH: dir },
      { overrideVar: 'BOSS_CODEX_BIN', binName: 'codex' },
    )
    assert.equal(result, path.join(dir, 'codex'))
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('resolveAgentBin: relative override → null (no PATH fallback)', () => {
  const dir = makeTmpDir()
  try {
    writeFakeBin(dir, 'codex', 'exit 0') // present on PATH...
    const result = resolveAgentBin(
      { BOSS_CODEX_BIN: 'codex', PATH: dir },
      { overrideVar: 'BOSS_CODEX_BIN', binName: 'codex' },
    )
    assert.equal(result, null, 'relative override must resolve to null')
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('resolveAgentBin: missing absolute override → null', () => {
  const result = resolveAgentBin(
    { BOSS_CODEX_BIN: '/nonexistent/path/to/codex' },
    { overrideVar: 'BOSS_CODEX_BIN', binName: 'codex' },
  )
  assert.equal(result, null)
})

test('resolveAgentBin: empty-string override falls back to PATH', () => {
  const dir = makeTmpDir()
  try {
    writeFakeBin(dir, 'codex', 'exit 0')
    const result = resolveAgentBin(
      { BOSS_CODEX_BIN: '', PATH: dir },
      { overrideVar: 'BOSS_CODEX_BIN', binName: 'codex' },
    )
    assert.equal(result, path.join(dir, 'codex'))
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('resolveAgentBin: not on PATH and no override → null', () => {
  const result = resolveAgentBin(
    { PATH: '/nonexistent-dir-that-has-no-codex' },
    { overrideVar: 'BOSS_CODEX_BIN', binName: 'codex' },
  )
  assert.equal(result, null)
})

test('resolveAgentBin: generalizes to a different binName/overrideVar', () => {
  const dir = makeTmpDir()
  try {
    const bin = writeFakeBin(dir, 'claude', 'exit 0')
    const result = resolveAgentBin(
      { BOSS_CLAUDE_BIN: bin, PATH: '/usr/bin' },
      { overrideVar: 'BOSS_CLAUDE_BIN', binName: 'claude' },
    )
    assert.equal(result, bin)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

// ---------------------------------------------------------------------------
// assemblePrompt — embed vs instruct, preamble passthrough
// ---------------------------------------------------------------------------

test('assemblePrompt: embed mode includes diff text + review-only-diff language', () => {
  const diffText = 'diff --git a/x b/x\n+added line\n-removed line'
  const prompt = assemblePrompt({
    preamble: REVIEW_PREAMBLE,
    range: 'abc123...def456',
    diffText,
  })
  assert.ok(prompt.includes(diffText), 'should embed the diff text verbatim')
  assert.ok(prompt.includes('BEGIN DIFF'), 'should delimit the embedded diff')
  assert.ok(prompt.includes('END DIFF'), 'should delimit the embedded diff')
  assert.ok(/only/i.test(prompt), 'should instruct to review ONLY the embedded diff')
  assert.ok(prompt.includes('abc123...def456'), 'embed mode still names the range')
})

test('assemblePrompt: instruct mode includes range + no-explore guard + base/head', () => {
  const prompt = assemblePrompt({ preamble: REVIEW_PREAMBLE, range: 'abc123...def456' })
  assert.ok(prompt.includes('abc123'), 'should mention base')
  assert.ok(prompt.includes('def456'), 'should mention head')
  assert.ok(prompt.includes('abc123...def456'), 'should mention the range')
  assert.ok(
    /do NOT run find\/ls\/grep\/cat/i.test(prompt),
    'should carry the no-tree-exploration scope guard',
  )
  assert.ok(!prompt.includes('BEGIN DIFF'), 'instruct mode must not claim an embedded diff')
})

test('assemblePrompt: empty diffText falls back to instruct mode', () => {
  const prompt = assemblePrompt({ preamble: REVIEW_PREAMBLE, range: 'a...b', diffText: '' })
  assert.ok(!prompt.includes('BEGIN DIFF'))
  assert.ok(prompt.includes('a...b'))
})

test('assemblePrompt: preamble keeps data / AGENTS.md / CLAUDE.md / skill-dirs constraints', () => {
  for (const prompt of [
    assemblePrompt({ preamble: REVIEW_PREAMBLE, range: 'a...b' }),
    assemblePrompt({ preamble: REVIEW_PREAMBLE, range: 'a...b', diffText: 'x' }),
  ]) {
    assert.ok(prompt.includes('AGENTS.md'), 'preamble keeps AGENTS.md')
    assert.ok(prompt.includes('CLAUDE.md'), 'preamble keeps CLAUDE.md')
    assert.ok(prompt.includes('.claude/'), 'preamble keeps skill/agent dirs')
    assert.ok(/DATA/.test(prompt), 'preamble keeps data-not-instructions constraint')
  }
})

// ---------------------------------------------------------------------------
// buildExecArgv — pure argv assembly from an adapter spec
// ---------------------------------------------------------------------------

test('buildExecArgv: assembles [subcommand, -C, repo, ...flags, prompt]', () => {
  const adapter = { subcommand: 'exec', flags: ['-s', 'read-only', '-c', 'k="v"'] }
  const argv = buildExecArgv(adapter, { repo: '/my/repo', prompt: 'PROMPT' })
  assert.deepEqual(argv, ['exec', '-C', '/my/repo', '-s', 'read-only', '-c', 'k="v"', 'PROMPT'])
})

test('buildExecArgv: prompt is always the last arg', () => {
  const adapter = { subcommand: 'exec', flags: ['-s', 'read-only'] }
  const argv = buildExecArgv(adapter, { repo: '/r', prompt: 'THE-PROMPT' })
  assert.equal(argv[argv.length - 1], 'THE-PROMPT')
  assert.equal(argv[0], 'exec')
})

test('buildExecArgv: missing flags array tolerated', () => {
  const argv = buildExecArgv({ subcommand: 'exec' }, { repo: '/r', prompt: 'P' })
  assert.deepEqual(argv, ['exec', '-C', '/r', 'P'])
})

// ---------------------------------------------------------------------------
// interpretResult — single source of truth for skip detection
// ---------------------------------------------------------------------------

test('interpretResult: success (code 0, non-empty stdout) → ok', () => {
  const r = interpretResult({ code: 0, signal: null, stdout: 'review: ok', timedOut: false })
  assert.equal(r.ok, true)
  assert.equal(r.skipReason, null)
  assert.equal(r.output, 'review: ok')
  assert.equal(r.timedOut, false)
})

test('interpretResult: non-zero exit → skip', () => {
  const r = interpretResult({ code: 2, signal: null, stdout: 'partial', timedOut: false })
  assert.equal(r.ok, false)
  assert.match(r.skipReason, /non-zero exit/)
})

test('interpretResult: empty stdout → skip', () => {
  const r = interpretResult({ code: 0, signal: null, stdout: '', timedOut: false })
  assert.equal(r.ok, false)
  assert.match(r.skipReason, /empty/)
})

test('interpretResult: timedOut → skip with timedOut flag', () => {
  const r = interpretResult({ code: null, signal: 'SIGKILL', stdout: '', timedOut: true })
  assert.equal(r.ok, false)
  assert.equal(r.timedOut, true)
  assert.match(r.skipReason, /timed out/)
})

test('interpretResult: killed by signal (no timeout) → skip', () => {
  const r = interpretResult({ code: null, signal: 'SIGSEGV', stdout: 'x', timedOut: false })
  assert.equal(r.ok, false)
  assert.match(r.skipReason, /signal/)
})

test('interpretResult: missing exit code → skip', () => {
  const r = interpretResult({ code: null, signal: null, stdout: 'x', timedOut: false })
  assert.equal(r.ok, false)
  assert.match(r.skipReason, /no exit code/)
})

test('interpretResult: requireJson rejects non-JSON, accepts JSON', () => {
  const bad = interpretResult({
    code: 0,
    signal: null,
    stdout: 'plain text review',
    timedOut: false,
    requireJson: true,
  })
  assert.equal(bad.ok, false)
  assert.match(bad.skipReason, /non-JSON/)

  const good = interpretResult({
    code: 0,
    signal: null,
    stdout: '{"findings": []}',
    timedOut: false,
    requireJson: true,
  })
  assert.equal(good.ok, true)
})

// ---------------------------------------------------------------------------
// boundedStderrTail — rolling bounded tail
// ---------------------------------------------------------------------------

test('boundedStderrTail: keeps the most-recent bytes under a flood, bounded', () => {
  const cap = 256
  const tail = boundedStderrTail(cap)
  for (let i = 0; i < 1000; i += 1) {
    tail.push(Buffer.from('noise-line-padding\n'))
  }
  tail.push(Buffer.from('FINAL-ERROR-MARKER\n'))
  const out = tail.tail()
  assert.ok(Buffer.byteLength(out, 'utf8') <= cap, 'tail must stay within cap')
  assert.ok(out.includes('FINAL-ERROR-MARKER'), 'tail keeps the most recent bytes')
})

test('boundedStderrTail: sanitizes ANSI/control bytes', () => {
  const tail = boundedStderrTail(4096)
  tail.push(Buffer.from('\x1b[31merror: boom\x1b[0m\n'))
  const out = tail.tail()
  assert.ok(out.includes('error: boom'))
  assert.ok(!out.includes('\x1b'), 'ANSI escapes stripped')
})

test('boundedStderrTail: small input passes through', () => {
  const tail = boundedStderrTail(4096)
  tail.push(Buffer.from('just one line\n'))
  assert.equal(tail.tail(), 'just one line\n')
})

// ---------------------------------------------------------------------------
// sanitizeOutput / sliceLenUtf8Safe — moved verbatim, smoke-covered
// ---------------------------------------------------------------------------

test('sanitizeOutput: strips ANSI, keeps text', () => {
  const out = sanitizeOutput('\x1b[31mred\x1b[0m normal', {})
  assert.ok(!out.includes('\x1b'))
  assert.ok(out.includes('red'))
  assert.ok(out.includes('normal'))
})

test('sanitizeOutput: caps total to maxBytes with marker, no U+FFFD', () => {
  const out = sanitizeOutput('€'.repeat(50), { maxBytes: 100 })
  assert.ok(Buffer.byteLength(out, 'utf8') <= 100)
  assert.ok(out.includes('[truncated'))
  assert.ok(!out.includes('�'))
})

test('sanitizeOutput: empty/non-string → empty string', () => {
  assert.equal(sanitizeOutput('', {}), '')
  assert.equal(sanitizeOutput(null, {}), '')
  assert.equal(sanitizeOutput(42, {}), '')
})

test('sliceLenUtf8Safe: backs up off a continuation byte', () => {
  const buf = Buffer.from('€€€', 'utf8') // 9 bytes, 3 per char
  // cut at 4 (middle of 2nd char) → backs up to 3 (char boundary)
  assert.equal(sliceLenUtf8Safe(buf, 4), 3)
  assert.equal(sliceLenUtf8Safe(buf, 100), buf.length)
})

// ---------------------------------------------------------------------------
// REVIEW_PREAMBLE — shared safety constraints present
// ---------------------------------------------------------------------------

test('REVIEW_PREAMBLE carries the core safety constraints', () => {
  assert.ok(REVIEW_PREAMBLE.includes('AGENTS.md'))
  assert.ok(REVIEW_PREAMBLE.includes('CLAUDE.md'))
  assert.ok(REVIEW_PREAMBLE.includes('.claude/'))
  assert.ok(/DATA/.test(REVIEW_PREAMBLE))
  assert.ok(/read-only/i.test(REVIEW_PREAMBLE))
})
