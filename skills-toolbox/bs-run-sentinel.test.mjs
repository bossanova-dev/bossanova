import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, writeFileSync, readFileSync, existsSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  REPAIR_RESULTS,
  DISPATCH_FAILURE,
  makeRunContext,
  writeSentinel,
  readSentinel,
  matchRepairResult,
  cleanupRunContext,
} from './bs-run-sentinel.mjs'

const scriptPath = fileURLToPath(new URL('./bs-run-sentinel.mjs', import.meta.url))

/** Run the thin CLI as a subprocess; return trimmed stdout + status. */
function runCli(args = []) {
  const res = spawnSync(process.execPath, [scriptPath, ...args], { encoding: 'utf8' })
  return { stdout: res.stdout.trim(), stderr: res.stderr.trim(), status: res.status }
}

test('REPAIR_RESULTS is the byte-identical published set', () => {
  assert.deepEqual(REPAIR_RESULTS, ['green', 'no-progress', 'max-attempts', 'blocked'])
  assert.equal(DISPATCH_FAILURE, 'dispatch-failure')
})

test('a fresh context has a unique dir under the given tmpdir', () => {
  const base = mkdtempSync(join(tmpdir(), 'brs-'))
  const a = makeRunContext('bs-sweep-security', { tmpdir: base })
  const b = makeRunContext('bs-sweep-security', { tmpdir: base })
  assert.notEqual(a.runId, b.runId)
  assert.notEqual(a.dir, b.dir)
})

test('an explicit runId is honored (orchestrator re-derives the same context)', () => {
  const base = mkdtempSync(join(tmpdir(), 'brs-'))
  const a = makeRunContext('bs-sweep-security', { tmpdir: base, runId: 'FIXED' })
  const b = makeRunContext('bs-sweep-security', { tmpdir: base, runId: 'FIXED' })
  assert.equal(a.runId, 'FIXED')
  assert.equal(a.dir, b.dir)
})

test('write-then-read round-trips the payload for the owning run', () => {
  const base = mkdtempSync(join(tmpdir(), 'brs-'))
  const ctx = makeRunContext('bs-sweep-security', { tmpdir: base })
  writeSentinel(ctx, 'repair', 'green', { sha: 'abc123' })
  const r = readSentinel(ctx, 'repair')
  assert.equal(r.status, 'ok')
  assert.equal(r.kind, 'green')
  assert.equal(r.runId, ctx.runId)
  assert.equal(r.payload.sha, 'abc123')
})

test('a missing sentinel reads as dispatch-failure (missing), never green', () => {
  const base = mkdtempSync(join(tmpdir(), 'brs-'))
  const ctx = makeRunContext('bs-sweep-security', { tmpdir: base })
  assert.equal(readSentinel(ctx, 'repair').status, 'missing')
})

test('a leftover sentinel from a different run reads as stale, never ok', () => {
  const base = mkdtempSync(join(tmpdir(), 'brs-'))
  const ctx = makeRunContext('bs-sweep-security', { tmpdir: base })
  // Simulate a crashed prior run's file with a foreign runId at the same path.
  writeFileSync(
    ctx.sentinelPath('repair'),
    JSON.stringify({ runId: 'OTHER', kind: 'green', payload: {} }),
  )
  const r = readSentinel(ctx, 'repair')
  assert.equal(r.status, 'stale')
  assert.equal(r.runId, 'OTHER')
})

test('a corrupt (non-JSON) sentinel reads as stale, never ok/green', () => {
  const base = mkdtempSync(join(tmpdir(), 'brs-'))
  const ctx = makeRunContext('bs-sweep-security', { tmpdir: base })
  writeFileSync(ctx.sentinelPath('repair'), 'not json at all')
  assert.equal(readSentinel(ctx, 'repair').status, 'stale')
})

test('matchRepairResult classifies only the published tokens', () => {
  assert.deepEqual(matchRepairResult('green'), { result: 'green' })
  assert.deepEqual(matchRepairResult('max-attempts'), { result: 'max-attempts' })
  assert.equal(matchRepairResult('dispatch-failure'), null) // orchestrator-only
  assert.equal(matchRepairResult('bogus'), null)
  assert.equal(matchRepairResult(''), null)
  assert.equal(matchRepairResult(undefined), null)
})

test('cleanupRunContext removes the dir and is idempotent', () => {
  const base = mkdtempSync(join(tmpdir(), 'brs-'))
  const ctx = makeRunContext('bs-sweep-security', { tmpdir: base })
  writeSentinel(ctx, 'repair', 'blocked')
  cleanupRunContext(ctx)
  cleanupRunContext(ctx) // second call must not throw
  assert.equal(existsSync(ctx.dir), false)
  assert.equal(readSentinel(ctx, 'repair').status, 'missing')
})

test('writeSentinel is atomic: no partial file is ever left at the final path', () => {
  const base = mkdtempSync(join(tmpdir(), 'brs-'))
  const ctx = makeRunContext('bs-sweep-security', { tmpdir: base })
  writeSentinel(ctx, 'repair', 'no-progress', { attempt: 2 })
  const raw = readFileSync(ctx.sentinelPath('repair'), 'utf8')
  const parsed = JSON.parse(raw) // must be valid JSON — never truncated
  assert.equal(parsed.kind, 'no-progress')
  assert.equal(parsed.runId, ctx.runId)
})

// ---------------------------------------------------------------------------
// Thin CLI — the surface the skill body shells out to.
// ---------------------------------------------------------------------------

test('CLI make-ctx → write → read round-trips through the run file only', () => {
  const base = mkdtempSync(join(tmpdir(), 'brs-cli-'))
  const mk = runCli(['make-ctx', 'bs-sweep-security', base])
  assert.equal(mk.status, 0)
  const [runId, dir] = mk.stdout.split('\t')
  assert.ok(runId && dir)

  const w = runCli(['write', dir, runId, 'repair', 'green', JSON.stringify({ sha: 'deadbee' })])
  assert.equal(w.status, 0)

  const r = runCli(['read', dir, runId, 'repair'])
  assert.equal(r.status, 0)
  const parsed = JSON.parse(r.stdout)
  assert.equal(parsed.status, 'ok')
  assert.equal(parsed.kind, 'green')
  assert.equal(parsed.payload.sha, 'deadbee')
})

test('CLI read of an absent sentinel prints missing (dispatch-failure), exit 0', () => {
  const base = mkdtempSync(join(tmpdir(), 'brs-cli-'))
  const [runId, dir] = runCli(['make-ctx', 'bs-sweep-security', base]).stdout.split('\t')
  const r = runCli(['read', dir, runId, 'repair'])
  assert.equal(r.status, 0)
  assert.equal(JSON.parse(r.stdout).status, 'missing')
})

test('CLI read with a foreign runId prints stale, never ok', () => {
  const base = mkdtempSync(join(tmpdir(), 'brs-cli-'))
  const [runId, dir] = runCli(['make-ctx', 'bs-sweep-security', base]).stdout.split('\t')
  runCli(['write', dir, runId, 'repair', 'green'])
  const r = runCli(['read', dir, 'SOMEONE-ELSE', 'repair'])
  assert.equal(JSON.parse(r.stdout).status, 'stale')
})

test('CLI match classifies a REPAIR_RESULT token as JSON', () => {
  assert.deepEqual(JSON.parse(runCli(['match', 'green']).stdout), { result: 'green' })
  assert.equal(JSON.parse(runCli(['match', 'dispatch-failure']).stdout), null)
})

test('CLI cleanup removes the dir and is idempotent', () => {
  const base = mkdtempSync(join(tmpdir(), 'brs-cli-'))
  const [runId, dir] = runCli(['make-ctx', 'bs-sweep-security', base]).stdout.split('\t')
  runCli(['write', dir, runId, 'repair', 'blocked'])
  assert.equal(runCli(['cleanup', dir]).status, 0)
  assert.equal(runCli(['cleanup', dir]).status, 0) // idempotent
  assert.equal(existsSync(dir), false)
})

test('CLI exits non-zero on an unknown subcommand', () => {
  assert.notEqual(runCli(['bogus']).status, 0)
})
