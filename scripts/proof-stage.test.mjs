import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { pathToFileURL } from 'node:url'

import {
  DEFAULT_FFMPEG_TIMEOUT_MS,
  StageError,
  reasonCodeForStageError,
  runFfmpegStage,
  runStage,
  scrubSecrets,
  tailLines,
  toStageErrorRecord,
  wasKilledByTimeout,
} from './proof-stage.mjs'
import { renderDeferredComment } from './proof-lib.mjs'

// ── scrubSecrets (BOS-138 defense-in-depth) ─────────────────────────────────
// A pipeline-error note embeds a raw subprocess stderr tail, and that
// subprocess runs with the injected proof secrets in its env. If a crashing
// tool dumps its environment, the secret VALUE must never reach the posted note.

test('scrubSecrets redacts injected secret values from a stderr tail', () => {
  const env = {
    PROOF_ANTHROPIC_API_KEY: 'sk-ant-THISVALUE-must-never-post',
    CLOUDFLARE_API_TOKEN: 'cf-THISVALUE-must-never-post',
  }
  const raw =
    'fatal: env dump\nPROOF_ANTHROPIC_API_KEY=sk-ant-THISVALUE-must-never-post\nCLOUDFLARE_API_TOKEN=cf-THISVALUE-must-never-post'
  const scrubbed = scrubSecrets(raw, env)
  assert.ok(!scrubbed.includes('sk-ant-THISVALUE-must-never-post'), 'anthropic value redacted')
  assert.ok(!scrubbed.includes('cf-THISVALUE-must-never-post'), 'cloudflare value redacted')
  assert.ok(scrubbed.includes('«redacted-secret»'))
})

test('scrubSecrets ignores absent/short values and is null-safe', () => {
  assert.equal(scrubSecrets(null, {}), '')
  // A short (<8 char) value is not scrubbed — too short to be a real token and
  // scrubbing it would mangle unrelated text.
  assert.equal(scrubSecrets('abcdef here', { PROOF_ANTHROPIC_API_KEY: 'abc' }), 'abcdef here')
})

test('toStageErrorRecord scrubs the stderr tail (secret cannot reach the record)', () => {
  const prev = process.env.CLOUDFLARE_API_TOKEN
  process.env.CLOUDFLARE_API_TOKEN = 'cf-LEAKY-token-value-1234'
  try {
    const err = new StageError('ffmpeg', 'boom', {
      stderrTail: 'crash: CLOUDFLARE_API_TOKEN=cf-LEAKY-token-value-1234',
    })
    const record = toStageErrorRecord(err)
    assert.ok(!record.stderrTail.includes('cf-LEAKY-token-value-1234'), 'record tail is scrubbed')
    assert.ok(record.stderrTail.includes('«redacted-secret»'))
  } finally {
    if (prev === undefined) delete process.env.CLOUDFLARE_API_TOKEN
    else process.env.CLOUDFLARE_API_TOKEN = prev
  }
})

// ── tailLines ─────────────────────────────────────────────────────────────────

test('tailLines keeps the last N non-empty lines', () => {
  const text = 'a\n\nb\nc\n\nd\n'
  assert.equal(tailLines(text, 2), 'c\nd')
})

test('tailLines is null/undefined safe', () => {
  assert.equal(tailLines(null), '')
  assert.equal(tailLines(undefined), '')
})

test('tailLines caps by characters from the end', () => {
  const text = 'x'.repeat(10_000)
  assert.ok(tailLines(text, 20, 100).length <= 100)
})

// ── runStage ────────────────────────────────────────────────────────────────

test('runStage returns the value on success', async () => {
  assert.equal(await runStage('encode', () => 42), 42)
})

test('runStage wraps a thrown error as a StageError naming the stage', async () => {
  await assert.rejects(
    runStage('cast-to-webm', () => {
      throw new Error('boom')
    }),
    (err) => {
      assert.ok(err instanceof StageError)
      assert.equal(err.stage, 'cast-to-webm')
      assert.equal(err.message, 'boom')
      assert.ok(err.stderrTail.includes('boom'))
      return true
    },
  )
})

test('runStage does not re-wrap an inner StageError (innermost stage wins)', async () => {
  const inner = new StageError('render-terminal', 'inner', { stderrTail: 'tail' })
  await assert.rejects(
    runStage('outer', () =>
      runStage('render-terminal', () => {
        throw inner
      }),
    ),
    (err) => {
      assert.equal(err.stage, 'render-terminal')
      return true
    },
  )
})

// ── CRITICAL regression fixture #1: TDZ-class render crash ──────────────────
// A renderer module that throws a ReferenceError at import/first-frame (the
// proof-render-terminal top-of-file CLI-entry → const TDZ regression) MUST
// classify as pipeline-error: stage named, stderr tail present, and the note
// NEVER contains "environment limitation".

test('TDZ-class: a ReferenceError at module import → pipeline-error, stage named, never "environment limitation"', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tdz-'))
  const fixture = path.join(dir, 'tdz-renderer.mjs')
  // Accessing a `const` before its lexical declaration throws ReferenceError at
  // module evaluation — exactly the TDZ crash shape, triggered on import.
  fs.writeFileSync(
    fixture,
    ['export function render() {}', 'renderFrame()', 'const renderFrame = () => {}', ''].join('\n'),
  )

  let caught
  try {
    await runStage('render-terminal', async () => {
      await import(pathToFileURL(fixture).href)
    })
    assert.fail('expected the TDZ import to throw')
  } catch (err) {
    caught = err
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }

  assert.ok(caught instanceof StageError)
  assert.equal(caught.stage, 'render-terminal')
  assert.ok(caught.stderrTail.length > 0, 'a stderr/stack tail must be captured')
  assert.match(caught.message, /is not defined|before initialization|ReferenceError/i)

  // Classification + rendered note.
  assert.equal(reasonCodeForStageError(caught), 'pipeline-error')
  const record = toStageErrorRecord(caught)
  const note = renderDeferredComment({
    marker: '<!-- m -->',
    manifest: { commit: 'abc1234' },
    reasonCode: 'pipeline-error',
    stage: record.stage,
    stderrTail: record.stderrTail,
  })
  assert.ok(note.includes('render-terminal'), 'the note names the failed stage')
  assert.ok(!note.includes('environment limitation'), 'MUST NOT claim an environment limitation')
})

// ── CRITICAL regression fixture #2: ffmpeg-hang-class ───────────────────────
// A hung ffmpeg (fake binary that sleeps past the timeout) MUST be killed by the
// timeout wrapper and classify as pipeline-error at stage `ffmpeg`, exit 1.

test('ffmpeg-hang-class: a hung encode is killed by the timeout → pipeline-error at stage ffmpeg', () => {
  let caught
  try {
    // Use node itself as a fake, unbounded "ffmpeg" that sleeps far past the
    // 200ms timeout. Cross-platform (no shell / executable-bit needed).
    runFfmpegStage(['-e', 'setTimeout(() => {}, 30000)'], {
      bin: process.execPath,
      timeoutMs: 200,
    })
    assert.fail('expected the hung ffmpeg to be killed')
  } catch (err) {
    caught = err
  }

  assert.ok(caught instanceof StageError)
  assert.equal(caught.stage, 'ffmpeg')
  assert.ok(typeof caught.elapsedMs === 'number', 'the note carries the elapsed time')
  assert.match(caught.message, /killed|timeout/i)

  // Exit-code policy: pipeline-error → exit 1.
  assert.equal(reasonCodeForStageError(caught), 'pipeline-error')
})

test('runFfmpegStage returns the spawn result on success', () => {
  const res = runFfmpegStage(['-e', 'process.exit(0)'], {
    bin: process.execPath,
    timeoutMs: 10_000,
  })
  assert.equal(res.status, 0)
})

test('runFfmpegStage: a non-zero exit is a stage error (not a timeout)', () => {
  assert.throws(
    () => runFfmpegStage(['-e', 'process.exit(3)'], { bin: process.execPath, timeoutMs: 10_000 }),
    (err) => {
      assert.ok(err instanceof StageError)
      assert.equal(err.stage, 'ffmpeg')
      assert.match(err.message, /exited 3/)
      return true
    },
  )
})

test('wasKilledByTimeout detects ETIMEDOUT and null-status kill signals', () => {
  assert.equal(wasKilledByTimeout({ error: { code: 'ETIMEDOUT' } }), true)
  assert.equal(wasKilledByTimeout({ status: null, signal: 'SIGKILL' }), true)
  assert.equal(wasKilledByTimeout({ status: 0, signal: null }), false)
})

test('DEFAULT_FFMPEG_TIMEOUT_MS is a sane positive bound', () => {
  assert.ok(DEFAULT_FFMPEG_TIMEOUT_MS > 0)
})

// ── renderDeferredComment: new reason codes ─────────────────────────────────

test('env-unavailable note lists the missing prerequisites and points at the doctor', () => {
  const note = renderDeferredComment({
    marker: '<!-- m -->',
    manifest: { commit: 'deadbee' },
    reasonCode: 'env-unavailable',
    missing: ['ffmpeg', 'CLOUDFLARE_API_TOKEN'],
    details: '```\ndoctor report\n```',
  })
  assert.ok(note.includes('ffmpeg'))
  assert.ok(note.includes('CLOUDFLARE_API_TOKEN'))
  assert.ok(note.includes('proof.mjs doctor'))
  assert.ok(!note.includes('environment limitation'))
})

test('pipeline-error note never claims an environment limitation', () => {
  const note = renderDeferredComment({
    marker: '<!-- m -->',
    manifest: { commit: 'abc' },
    reasonCode: 'pipeline-error',
    stage: 'ffmpeg',
    stderrTail: 'Encoder timed out',
  })
  assert.ok(!note.includes('environment limitation'))
  assert.ok(note.includes('ffmpeg'))
  assert.ok(note.includes('Encoder timed out'))
})
