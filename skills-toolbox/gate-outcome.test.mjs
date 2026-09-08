import { test } from 'node:test'
import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  GATE_OUTCOMES,
  GATE_OUTCOME_DIR,
  GATE_OUTCOME_FILE_ENV,
  GATE_OUTCOME_LINE_PATTERN,
  MAX_REASON_LENGTH,
  UNSPECIFIED_GATE_ID,
  UNSPECIFIED_REASON,
  createGateRecorder,
  formatGateOutcomeLine,
  recordGateOutcome,
  resolveGateOutcomePath,
} from './gate-outcome.mjs'

const scratch = () => mkdtempSync(join(tmpdir(), 'gate-outcome-'))

/** Read a destination as its non-empty lines. */
const lines = (path) =>
  readFileSync(path, 'utf8')
    .split('\n')
    .filter((line) => line !== '')

// ---------------------------------------------------------------------------
// One line per outcome, append-safe.
// ---------------------------------------------------------------------------

test('records exactly one line per recorded outcome', () => {
  const dir = scratch()
  const path = join(dir, 'outcomes.tsv')
  assert.equal(recordGateOutcome('plan-image-guard', 'pass', 'ok', { path }), true)
  const recorded = lines(path)
  assert.equal(recorded.length, 1)
  assert.match(recorded[0], GATE_OUTCOME_LINE_PATTERN)
  assert.equal(recorded[0].split('\t')[1], 'plan-image-guard')
  assert.equal(recorded[0].split('\t')[2], 'pass')
  assert.equal(recorded[0].split('\t')[3], 'ok')
})

test('is append-safe: N calls produce N lines in call order', () => {
  const dir = scratch()
  const path = join(dir, 'outcomes.tsv')
  const calls = [
    ['plan-contract-guard', 'pass', 'ok'],
    ['plan-image-guard', 'fire', 'unreadable-input'],
    ['plan-run-guards.premises', 'fire', 'premise-drift'],
    ['bs-review-caps.admit-fix-round', 'pass', 'within-budget'],
  ]
  for (const [gate, outcome, reason] of calls) {
    assert.equal(recordGateOutcome(gate, outcome, reason, { path }), true)
  }
  const recorded = lines(path)
  assert.equal(recorded.length, calls.length)
  assert.deepEqual(
    recorded.map((line) => line.split('\t').slice(1)),
    calls,
  )
  for (const line of recorded) assert.match(line, GATE_OUTCOME_LINE_PATTERN)
})

test('a repeated identical call appends rather than overwriting', () => {
  const dir = scratch()
  const path = join(dir, 'outcomes.tsv')
  for (let i = 0; i < 5; i += 1)
    recordGateOutcome('plan-writeback-verify', 'fire', 'drift', { path })
  assert.equal(lines(path).length, 5)
})

// ---------------------------------------------------------------------------
// Never throws. A telemetry failure must not change a gate's verdict.
// ---------------------------------------------------------------------------

test('never throws when the destination parent is a regular file', () => {
  const dir = scratch()
  const blocker = join(dir, 'not-a-dir')
  writeFileSync(blocker, 'this is a file, not a directory\n')
  const path = join(blocker, 'outcomes.tsv')
  let result
  assert.doesNotThrow(() => {
    result = recordGateOutcome('plan-contract-guard', 'fire', 'unreadable-input', { path })
  })
  assert.equal(result, false)
})

test('never throws when the destination is a directory', () => {
  const dir = scratch()
  let result
  assert.doesNotThrow(() => {
    result = recordGateOutcome('plan-contract-guard', 'pass', 'ok', { path: dir })
  })
  assert.equal(result, false)
})

test('refuses an outcome outside the published set instead of guessing one', () => {
  const dir = scratch()
  const path = join(dir, 'outcomes.tsv')
  for (const bogus of ['PASS', 'failed', '', 'fire\tpass', undefined]) {
    assert.equal(formatGateOutcomeLine('plan-image-guard', bogus, 'ok'), null)
    assert.equal(recordGateOutcome('plan-image-guard', bogus, 'ok', { path }), false)
  }
  assert.deepEqual(GATE_OUTCOMES, ['pass', 'fire'])
})

// ---------------------------------------------------------------------------
// No payload. The record is a fixed shape of validated slugs, so a would-be
// leak is discarded WHOLESALE rather than truncated to a leaking prefix.
// ---------------------------------------------------------------------------

test('a leaky gate id and reason are replaced wholesale, not truncated', () => {
  const dir = scratch()
  const path = join(dir, 'outcomes.tsv')
  const planBody = '## Problem Frame\n\nThe defect is an unmeasured population.\n- secret bullet'
  const credential = 'lin_api_0123456789ABCDEFghijklmnop'
  assert.equal(recordGateOutcome(planBody, 'fire', credential, { path }), true)

  const [line] = lines(path)
  assert.match(line, GATE_OUTCOME_LINE_PATTERN)
  const [, gateId, outcome, reason] = line.split('\t')
  assert.equal(gateId, UNSPECIFIED_GATE_ID)
  assert.equal(outcome, 'fire')
  assert.equal(reason, UNSPECIFIED_REASON)

  // No fragment of either input survives — not a prefix, not a word.
  const raw = readFileSync(path, 'utf8')
  for (const leak of ['Problem', 'defect', 'population', 'secret', 'lin_api', 'ABCDEF', '0123']) {
    assert.ok(!raw.includes(leak), `recorded line leaked ${leak}: ${raw}`)
  }
})

test('an over-long reason becomes the placeholder rather than a prefix', () => {
  const dir = scratch()
  const path = join(dir, 'outcomes.tsv')
  const long = 'a'.repeat(MAX_REASON_LENGTH + 1)
  recordGateOutcome('plan-image-guard', 'pass', long, { path })
  const [line] = lines(path)
  assert.match(line, GATE_OUTCOME_LINE_PATTERN)
  assert.equal(line.split('\t')[3], UNSPECIFIED_REASON)
  assert.ok(!line.includes('aaaa'))
})

test('an absent reason records the placeholder and still matches the line shape', () => {
  const dir = scratch()
  const path = join(dir, 'outcomes.tsv')
  recordGateOutcome('plan-image-guard', 'pass', undefined, { path })
  const [line] = lines(path)
  assert.match(line, GATE_OUTCOME_LINE_PATTERN)
  assert.equal(line.split('\t')[3], UNSPECIFIED_REASON)
})

test('a dotted verb id is a valid gate id, so per-verb rates stay distinguishable', () => {
  const dir = scratch()
  const path = join(dir, 'outcomes.tsv')
  recordGateOutcome('plan-run-guards.idempotence', 'pass', 'noop', { path })
  const [line] = lines(path)
  assert.match(line, GATE_OUTCOME_LINE_PATTERN)
  assert.equal(line.split('\t')[1], 'plan-run-guards.idempotence')
})

// ---------------------------------------------------------------------------
// Destination resolution: explicit env wins, a run id derives a default, and
// no run scope at all is INERT.
// ---------------------------------------------------------------------------

test('is inert when neither an explicit path nor any run-scope env var is set', () => {
  const dir = scratch()
  const env = {}
  assert.equal(resolveGateOutcomePath(env), null)
  assert.equal(recordGateOutcome('plan-image-guard', 'pass', 'ok', { env }), false)
  assert.equal(createGateRecorder('plan-image-guard', { env }).record('fire', 'drift'), false)
  assert.deepEqual(readdirSync(dir), [], 'an inert recorder must create no destination')
})

test('an explicit BOSS_GATE_OUTCOME_FILE wins over every derived path', () => {
  const dir = scratch()
  const path = join(dir, 'explicit.tsv')
  const env = { [GATE_OUTCOME_FILE_ENV]: path, BOSS_SESSION_ID: 'deadbeefdeadbeef' }
  assert.equal(resolveGateOutcomePath(env), path)
  assert.equal(recordGateOutcome('plan-image-guard', 'pass', 'ok', { env }), true)
  assert.equal(lines(path).length, 1)
})

test('BOSS_SESSION_ID alone derives a run-scoped default that actually receives the line', () => {
  // The live-fire check. A real cron session sets BOSS_SESSION_ID and nothing
  // else, so if the derived path were wrong or inert the whole feature would
  // ship recording nothing while every explicit-path test stayed green.
  const sessionId = randomUUID().replace(/-/g, '')
  const expected = join(tmpdir(), GATE_OUTCOME_DIR, `${sessionId}.tsv`)
  const env = { BOSS_SESSION_ID: sessionId }
  try {
    assert.equal(resolveGateOutcomePath(env), expected)
    assert.equal(
      recordGateOutcome('plan-contract-guard', 'fire', 'missing-sections', { env }),
      true,
    )
    const [line] = lines(expected)
    assert.match(line, GATE_OUTCOME_LINE_PATTERN)
    assert.equal(line.split('\t')[1], 'plan-contract-guard')
    assert.equal(line.split('\t')[2], 'fire')
  } finally {
    rmSync(expected, { force: true })
  }
})

test('the run-scope env vars are consulted in priority order', () => {
  const gateRun = 'gaterunid00000001'
  const agent = 'agentsessionid001'
  const session = 'sessionid00000001'
  const base = join(tmpdir(), GATE_OUTCOME_DIR)
  assert.equal(
    resolveGateOutcomePath({
      BOSS_GATE_RUN_ID: gateRun,
      BOSS_AGENT_SESSION_ID: agent,
      BOSS_SESSION_ID: session,
    }),
    join(base, `${gateRun}.tsv`),
  )
  assert.equal(
    resolveGateOutcomePath({ BOSS_AGENT_SESSION_ID: agent, BOSS_SESSION_ID: session }),
    join(base, `${agent}.tsv`),
  )
  assert.equal(resolveGateOutcomePath({ BOSS_SESSION_ID: session }), join(base, `${session}.tsv`))
  // Empty is not a run scope.
  assert.equal(resolveGateOutcomePath({ BOSS_SESSION_ID: '   ' }), null)
})

test('a run id carrying unexpected bytes still derives a safe path rather than going inert', () => {
  // Inertness here would silently disable the mechanism in production, which is
  // strictly worse than normalising a name that never appears in a record.
  const path = resolveGateOutcomePath({ BOSS_SESSION_ID: 'Session/ID With Spaces!' })
  assert.equal(path, join(tmpdir(), GATE_OUTCOME_DIR, 'session-id-with-spaces.tsv'))
})

// ---------------------------------------------------------------------------
// createGateRecorder: the latch is what makes "exactly one" structural.
// ---------------------------------------------------------------------------

test('createGateRecorder latches after the first record', () => {
  const dir = scratch()
  const path = join(dir, 'outcomes.tsv')
  const recorder = createGateRecorder('plan-writeback-verify', { path })
  assert.equal(recorder.recorded, false)
  assert.equal(recorder.record('fire', 'drift'), true)
  assert.equal(recorder.recorded, true)
  assert.equal(recorder.record('pass', 'ok'), false)
  assert.equal(recorder.record('fire', 'drift'), false)

  const recorded = lines(path)
  assert.equal(recorded.length, 1, 'the latch must permit exactly one line')
  assert.equal(recorded[0].split('\t')[2], 'fire')
  assert.equal(recorded[0].split('\t')[3], 'drift')
})

test('the latch holds even when the first record could not be written', () => {
  const recorder = createGateRecorder('plan-writeback-verify', { env: {} })
  assert.equal(recorder.record('pass', 'ok'), false)
  assert.equal(recorder.recorded, true)
  assert.equal(recorder.record('fire', 'drift'), false)
})

test('two recorders are independent, so two gates in one process each record once', () => {
  const dir = scratch()
  const path = join(dir, 'outcomes.tsv')
  createGateRecorder('plan-image-guard', { path }).record('pass', 'ok')
  createGateRecorder('plan-contract-guard', { path }).record('fire', 'missing-sections')
  const recorded = lines(path)
  assert.equal(recorded.length, 2)
  assert.deepEqual(
    recorded.map((line) => line.split('\t')[1]),
    ['plan-image-guard', 'plan-contract-guard'],
  )
})

// ---------------------------------------------------------------------------
// Test isolation for the adopting guards. THE single home for this rationale —
// the suites that pin a destination carry a one-line pointer here.
//
// A guard CLI resolves its destination from the ambient environment, and a test
// that spawns one hands it `{...process.env}`. Under a live boss session that
// environment carries BOSS_SESSION_ID / BOSS_AGENT_SESSION_ID, so the recorder
// resolves the DERIVED per-run path and every test invocation appends to the
// file the firing-rate reader counts — measured at 960 test-spawned lines in one
// session, which is fabricated telemetry for the exact question this mechanism
// exists to answer, and it surfaces as no red test at all.
//
// The fix is one line per suite: point BOSS_GATE_OUTCOME_FILE at a scratch file
// at module scope, before any test runs. The explicit destination wins over
// every derived path, so it also covers a spawn site added to that suite later.
// This test is what makes the omission unrepeatable — it scans the directory
// rather than a hardcoded list, so a NEW qualifying suite fails it too.
// ---------------------------------------------------------------------------

const TOOLBOX_DIR = fileURLToPath(new URL('.', import.meta.url))

/** The guard scripts that record a gate outcome. Reaching one of these can write a line. */
const RECORDING_GUARDS = Object.freeze([
  'bs-review-caps.mjs',
  'plan-contract-guard.mjs',
  'plan-image-guard.mjs',
  'plan-run-guards.mjs',
  'plan-writeback-verify.mjs',
])

/** Child-process APIs. A suite that reaches a guard through one of these inherits the ambient env. */
const SPAWNS_A_CHILD = /\b(?:spawnSync|execFileSync|execSync|fork)\s*\(/

/** `runCli` from plan-run-guards.mjs records in-process — no child needed. */
function importsRecordingRunCli(source) {
  const imports = /import\s*\{([^}]*)\}\s*from\s*['"]\.\/plan-run-guards\.mjs['"]/g
  for (const match of source.matchAll(imports)) {
    if (/\brunCli\b/.test(match[1])) return true
  }
  return false
}

/** Module scope means column zero: an assignment nested inside a test runs too late. */
const PINS_DESTINATION = new RegExp(`^process\\.env\\.${GATE_OUTCOME_FILE_ENV}\\s*=`, 'm')

test('every suite that can trigger a recording pins its own gate-outcome destination', () => {
  const suites = readdirSync(TOOLBOX_DIR)
    .filter((name) => name.endsWith('.test.mjs') && name !== 'gate-outcome.test.mjs')
    .sort()
  const qualifying = []
  const offenders = []
  for (const name of suites) {
    const source = readFileSync(join(TOOLBOX_DIR, name), 'utf8')
    if (!RECORDING_GUARDS.some((guard) => source.includes(guard))) continue
    if (!SPAWNS_A_CHILD.test(source) && !importsRecordingRunCli(source)) continue
    qualifying.push(name)
    if (!PINS_DESTINATION.test(source)) offenders.push(name)
  }

  assert.ok(
    qualifying.length >= RECORDING_GUARDS.length,
    `expected at least one suite per recording guard, scanned ${suites.length} suites and ` +
      `qualified ${qualifying.length} — the directory scan found nothing, so this proves nothing`,
  )
  assert.deepEqual(
    offenders,
    [],
    `these suites can trigger a gate recording but do not pin their own destination, so under a ` +
      `live boss session they append to the real run's telemetry file:\n` +
      offenders.map((name) => `  skills-toolbox/${name}`).join('\n') +
      `\n\nAdd this at module scope (column zero), before the tests:\n` +
      `  process.env.${GATE_OUTCOME_FILE_ENV} = join(\n` +
      `    mkdtempSync(join(tmpdir(), 'gate-outcome-suite-')),\n` +
      `    'outcomes.tsv',\n` +
      `  )\n` +
      `See the block comment above this test in skills-toolbox/gate-outcome.test.mjs for why.`,
  )
})
