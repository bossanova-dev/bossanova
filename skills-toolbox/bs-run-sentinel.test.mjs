import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import {
  mkdirSync,
  mkdtempSync,
  writeFileSync,
  readFileSync,
  existsSync,
  utimesSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  REPAIR_RESULTS,
  DISPATCH_FAILURE,
  PROVISIONAL_KEY,
  provisionalPayload,
  isProvisional,
  makeRunContext,
  writeSentinel,
  readSentinel,
  matchRepairResult,
  cleanupRunContext,
  reapStaleRunDirs,
  STALE_RUN_DIR_TTL_MS,
  BOSS_PLAN_RUN_SCRATCH_PREFIXES,
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

test('PROVISIONAL_KEY is the byte-identical payload marker the skill markdown writes', () => {
  // The skill markdown hand-writes `{"provisional":true}` into a shell command rather than
  // spawning a second node process for it, so this constant and that literal are two stores of
  // one string. Pin the byte here; the boss-build content gate joins the markdown against it.
  assert.equal(PROVISIONAL_KEY, 'provisional')
  assert.deepEqual(provisionalPayload(), { provisional: true })
  // The seed payload must survive `JSON.parse(JSON.stringify(…))` unchanged — that is the exact
  // trip it makes through the CLI's `[payloadJson]` argument.
  assert.deepEqual(JSON.parse(JSON.stringify(provisionalPayload())), { provisional: true })
})

test('isProvisional is true only for an ok read carrying strict boolean true', () => {
  const base = mkdtempSync(join(tmpdir(), 'brs-prov-'))
  const ctx = makeRunContext('boss-build', { tmpdir: base })

  // The seed: written before dispatch, read back as a routable verdict.
  writeSentinel(ctx, 'review', 'bs-review capped: after 1 rounds.', provisionalPayload())
  const seeded = readSentinel(ctx, 'review')
  assert.equal(seeded.status, 'ok')
  assert.equal(isProvisional(seeded), true)

  // The upgrade: a later non-provisional write to the same context overwrites it.
  writeSentinel(ctx, 'review', 'bs-review clean: no open must-fix.', { provisional: false })
  const upgraded = readSentinel(ctx, 'review')
  assert.equal(upgraded.status, 'ok')
  assert.equal(isProvisional(upgraded), false)
})

test('isProvisional is false for missing, stale, absent payload, and the STRING "true"', () => {
  const base = mkdtempSync(join(tmpdir(), 'brs-prov-neg-'))
  const ctx = makeRunContext('boss-build', { tmpdir: base })

  // missing — no observation happened at all, so it is a dispatch-failure, never a seed.
  assert.equal(isProvisional(readSentinel(ctx, 'review')), false)

  // stale — a foreign run's leftover is never trusted, marker or not.
  writeFileSync(
    ctx.sentinelPath('review'),
    JSON.stringify({ runId: 'OTHER', kind: 'x', payload: { provisional: true } }),
  )
  const stale = readSentinel(ctx, 'review')
  assert.equal(stale.status, 'stale')
  assert.equal(isProvisional(stale), false)

  // absent payload — `readSentinel` defaults it to `{}`, which is not the marker.
  writeSentinel(ctx, 'review', 'bs-review clean: no open must-fix.')
  assert.equal(isProvisional(readSentinel(ctx, 'review')), false)

  // The string `"true"` is NOT the marker. This is a property of THIS module's API, not of the
  // shipped shell path: the skill's classify block reads the marker with
  // `jq -r '.payload.provisional // empty'`, which cannot tell JSON `true` from JSON `"true"`.
  // That divergence is unreachable (only the skill's own seed writes the marker, as a boolean
  // literal) and its failure direction is safe (a stringified marker over-routes to BLOCKED,
  // never to clean) — but a JS caller reaching for `isProvisional` gets the strict reading, so
  // the two never converge on treating a stringified marker as a seed.
  writeFileSync(
    ctx.sentinelPath('review'),
    JSON.stringify({ runId: ctx.runId, kind: 'k', payload: { provisional: 'true' } }),
  )
  assert.equal(isProvisional(readSentinel(ctx, 'review')), false)

  // Defensive shapes a caller can hand it.
  assert.equal(isProvisional(null), false)
  assert.equal(isProvisional(undefined), false)
  assert.equal(isProvisional({ status: 'ok' }), false)
  assert.equal(isProvisional({ payload: { provisional: true } }), false)
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

test('makeRunContext reaps stale sentinel and boss-plan run dirs but keeps fresh siblings', () => {
  const base = mkdtempSync(join(tmpdir(), 'brs-reap-'))
  const sentinelBase = join(base, 'bs-run-sentinel')
  const staleSentinel = join(sentinelBase, 'boss-plan-OLD')
  const freshSentinel = join(sentinelBase, 'boss-plan-FRESH')
  const staleRunScratch = join(base, 'boss-plan-run.STALE')
  const freshRunScratch = join(base, 'boss-plan-run.FRESH')
  for (const dir of [staleSentinel, freshSentinel, staleRunScratch, freshRunScratch]) {
    mkdirSync(dir, { recursive: true })
    writeFileSync(join(dir, 'x'), 'x')
  }
  const old = new Date(Date.now() - STALE_RUN_DIR_TTL_MS - 1000)
  utimesSync(staleSentinel, old, old)
  utimesSync(staleRunScratch, old, old)

  makeRunContext('boss-plan', { tmpdir: base, runId: 'NEW' })

  assert.equal(existsSync(staleSentinel), false)
  assert.equal(existsSync(staleRunScratch), false)
  assert.equal(existsSync(freshSentinel), true)
  assert.equal(existsSync(freshRunScratch), true)
  assert.equal(existsSync(join(sentinelBase, 'boss-plan-NEW')), true)
})

test('makeRunContext reaps stale boss-plan notes dirs but keeps fresh and current notes dirs', () => {
  assert.ok(BOSS_PLAN_RUN_SCRATCH_PREFIXES.includes('boss-plan-notes.'))
  const base = mkdtempSync(join(tmpdir(), 'brs-notes-reap-'))
  const staleNotes = join(base, 'boss-plan-notes.STALE')
  const freshNotes = join(base, 'boss-plan-notes.FRESH')
  const currentNotes = join(base, 'boss-plan-notes.CURRENT')
  for (const dir of [staleNotes, freshNotes, currentNotes]) {
    mkdirSync(dir, { recursive: true })
    writeFileSync(join(dir, 'x'), 'x')
  }
  const old = new Date(Date.now() - STALE_RUN_DIR_TTL_MS - 1000)
  utimesSync(staleNotes, old, old)
  utimesSync(currentNotes, old, old)

  reapStaleRunDirs({ tmpdir: base, excludeDirs: [currentNotes] })

  assert.equal(existsSync(staleNotes), false)
  assert.equal(existsSync(freshNotes), true)
  assert.equal(existsSync(currentNotes), true)
})

test('reapStaleRunDirs never touches the excluded directory', () => {
  const base = mkdtempSync(join(tmpdir(), 'brs-reap-exclude-'))
  const current = join(base, 'bs-run-sentinel', 'boss-plan-CURRENT')
  mkdirSync(current, { recursive: true })
  const old = new Date(Date.now() - STALE_RUN_DIR_TTL_MS - 1000)
  utimesSync(current, old, old)
  reapStaleRunDirs({ tmpdir: base, excludeDirs: [current] })
  assert.equal(existsSync(current), true)
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
