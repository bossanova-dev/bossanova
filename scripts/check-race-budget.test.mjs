import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'

import {
  DEFAULT_BUDGET_FILE,
  checkRaceBudget,
  finalAttempts,
  isCachedAttempt,
  parseBepAttempts,
  parseDurationSeconds,
  scoreTarget,
  testAttemptSeconds,
  validateBudgetFile,
} from './check-race-budget.mjs'

const DB = '//services/bossd/internal/db:db_test'

/**
 * One `testResult` event shaped like the ones `bazel test --build_event_json_file`
 * writes. Kept faithful to a captured event: int64 fields arrive as strings, and
 * `cachedLocally` is absent (not `false`) when the attempt really executed.
 */
function testResultEvent(
  label,
  seconds,
  { shard = 1, run = 1, attempt = 1, status = 'PASSED', cached = false, millis = true } = {},
) {
  const result = { status }
  if (millis) result.testAttemptDurationMillis = String(Math.round(seconds * 1000))
  result.testAttemptDuration = `${seconds}s`
  if (cached) result.cachedLocally = true
  else result.executionInfo = { strategy: 'linux-sandbox' }
  return JSON.stringify({ id: { testResult: { label, run, shard, attempt } }, testResult: result })
}

/** A whole BEP stream: some noise events plus one testResult per shard. */
function bep(events) {
  return (
    [
      JSON.stringify({ id: { started: {} }, started: { command: 'test' } }),
      JSON.stringify({ id: { progress: { opaqueCount: 1 } }, progress: {} }),
      ...events,
      JSON.stringify({ id: { buildFinished: {} }, finished: { overallSuccess: true } }),
    ].join('\n') + '\n'
  )
}

/** Shard durations summing to `total`, spread evenly, as one BEP stream. */
function shardedBep(label, total, shards) {
  const each = total / shards
  return bep(
    Array.from({ length: shards }, (_, i) => testResultEvent(label, each, { shard: i + 1 })),
  )
}

function budgetDoc(overrides = {}) {
  return {
    combination: 'summed-across-shards',
    budgetRegime: 'bazel test --config=race, per-shard wall clock summed',
    targets: [
      {
        label: DB,
        budgetSeconds: 420,
        bazelSummedPostFixSeconds: 170.4,
        nativePreFixSeconds: 512.5,
      },
    ],
    ...overrides,
  }
}

// --- duration parsing -------------------------------------------------------

test('parseDurationSeconds reads a protobuf Duration string', () => {
  assert.equal(parseDurationSeconds('5.864s'), 5.864)
  assert.equal(parseDurationSeconds('27s'), 27)
})

test('parseDurationSeconds returns null for an unrecognised shape', () => {
  for (const v of ['5864ms', '', 'abc', null, undefined, 5.864]) {
    assert.equal(parseDurationSeconds(v), null)
  }
})

test('testAttemptSeconds prefers the millis field Bazel encodes as a string', () => {
  assert.equal(testAttemptSeconds({ testAttemptDurationMillis: '5864' }), 5.864)
})

test('testAttemptSeconds falls back to the Duration form when millis is absent', () => {
  assert.equal(testAttemptSeconds({ testAttemptDuration: '7.5s' }), 7.5)
})

test('testAttemptSeconds throws rather than scoring an event with no duration', () => {
  assert.throws(
    () => testAttemptSeconds({ status: 'PASSED' }, 'x'),
    /no usable testAttemptDuration/,
  )
})

test('testAttemptSeconds rejects a non-numeric millis rather than reading it as 0', () => {
  assert.throws(
    () => testAttemptSeconds({ testAttemptDurationMillis: 'soon' }, 'x'),
    /not a non-negative number/,
  )
})

// --- cache detection --------------------------------------------------------

test('isCachedAttempt is false when the flags are absent, as Bazel writes them', () => {
  assert.equal(
    isCachedAttempt({ status: 'PASSED', executionInfo: { strategy: 'linux-sandbox' } }),
    false,
  )
})

test('isCachedAttempt detects a locally cached attempt', () => {
  assert.equal(isCachedAttempt({ cachedLocally: true }), true)
})

test('isCachedAttempt detects a remotely cached attempt', () => {
  assert.equal(isCachedAttempt({ executionInfo: { cachedRemotely: true } }), true)
})

// --- BEP stream parsing -----------------------------------------------------

test('parseBepAttempts keeps testResult events and ignores every other event', () => {
  const attempts = parseBepAttempts(bep([testResultEvent(DB, 12.5, { shard: 3 })]))
  assert.equal(attempts.length, 1)
  assert.deepEqual(
    { label: attempts[0].label, shard: attempts[0].shard, seconds: attempts[0].seconds },
    { label: DB, shard: 3, seconds: 12.5 },
  )
})

test('parseBepAttempts rejects an empty stream', () => {
  assert.throws(() => parseBepAttempts('   ', 'bep'), /empty build event stream/)
})

test('parseBepAttempts fails loudly on a truncated line rather than skipping it', () => {
  const text = bep([testResultEvent(DB, 5)]) + '{"id":{"testResult":'
  assert.throws(() => parseBepAttempts(text, 'bep'), /not valid JSON/)
})

test('parseBepAttempts rejects a testResult event with no label', () => {
  const text = JSON.stringify({ id: { testResult: { run: 1 } }, testResult: { status: 'PASSED' } })
  assert.throws(() => parseBepAttempts(text, 'bep'), /without a label/)
})

test('finalAttempts keeps the last attempt of each shard, not every retry', () => {
  const attempts = parseBepAttempts(
    bep([
      testResultEvent(DB, 30, { shard: 1, attempt: 1, status: 'FAILED' }),
      testResultEvent(DB, 10, { shard: 1, attempt: 2 }),
      testResultEvent(DB, 11, { shard: 2 }),
    ]),
  )
  const finals = finalAttempts(attempts)
  assert.equal(finals.length, 2)
  assert.equal(finals[0].attempt, 2)
  assert.equal(finals[0].seconds, 10)
})

// --- scoring ----------------------------------------------------------------

test('scoreTarget sums across shards rather than taking the max', () => {
  const attempts = parseBepAttempts(shardedBep(DB, 160, 8))
  const r = scoreTarget({ label: DB, budgetSeconds: 420 }, attempts)
  assert.equal(r.shards, 8)
  assert.equal(r.measured, 160)
  assert.equal(r.ok, true)
})

test('scoreTarget fails loudly when the target produced no testResult event', () => {
  const attempts = parseBepAttempts(shardedBep('//other:other_test', 5, 1))
  assert.throws(
    () => scoreTarget({ label: DB, budgetSeconds: 420 }, attempts),
    /no testResult events in the build event stream/,
  )
})

test('scoreTarget refuses a cache-served shard rather than reading it as under budget', () => {
  const attempts = parseBepAttempts(
    bep([
      testResultEvent(DB, 20, { shard: 1 }),
      testResultEvent(DB, 20, { shard: 2, cached: true }),
    ]),
  )
  assert.throws(
    () => scoreTarget({ label: DB, budgetSeconds: 420 }, attempts),
    /served from cache.*--nocache_test_results/s,
  )
})

test('scoreTarget refuses a failing target rather than scoring its duration', () => {
  const attempts = parseBepAttempts(bep([testResultEvent(DB, 20, { shard: 1, status: 'TIMEOUT' })]))
  assert.throws(() => scoreTarget({ label: DB, budgetSeconds: 420 }, attempts), /did not pass/)
})

// --- budget-file validation -------------------------------------------------

test('validateBudgetFile accepts a well-formed document', () => {
  assert.deepEqual(validateBudgetFile(budgetDoc()), [])
})

test('the committed budget file is valid', () => {
  const doc = JSON.parse(fs.readFileSync(DEFAULT_BUDGET_FILE, 'utf8'))
  assert.deepEqual(validateBudgetFile(doc), [])
})

test('validateBudgetFile rejects a budget at or above the pre-fix figure', () => {
  const doc = budgetDoc()
  doc.targets[0].budgetSeconds = 600
  assert.match(validateBudgetFile(doc).join('\n'), /strictly below nativePreFixSeconds/)
})

test('validateBudgetFile rejects a budget below the measured post-fix figure', () => {
  const doc = budgetDoc()
  doc.targets[0].budgetSeconds = 100
  assert.match(validateBudgetFile(doc).join('\n'), /must exceed bazelSummedPostFixSeconds/)
})

test('validateBudgetFile requires a measurement in the regime it enforces', () => {
  const doc = budgetDoc()
  delete doc.targets[0].bazelSummedPostFixSeconds
  assert.match(validateBudgetFile(doc).join('\n'), /bazelSummedPostFixSeconds must be a positive/)
})

test('validateBudgetFile requires the harness regime to be named', () => {
  const doc = budgetDoc()
  delete doc.budgetRegime
  assert.match(validateBudgetFile(doc).join('\n'), /budgetRegime must name the harness/)
})

test('validateBudgetFile rejects an unknown shard-combination rule', () => {
  assert.match(
    validateBudgetFile(budgetDoc({ combination: 'max-across-shards' })).join('\n'),
    /summed-across-shards/,
  )
})

test('validateBudgetFile rejects an empty target list', () => {
  assert.match(validateBudgetFile(budgetDoc({ targets: [] })).join('\n'), /non-empty array/)
})

test('validateBudgetFile rejects a non-positive budget', () => {
  const doc = budgetDoc()
  doc.targets[0].budgetSeconds = 0
  assert.match(validateBudgetFile(doc).join('\n'), /must be a positive number/)
})

// --- end-to-end: the pre-fix / post-fix proof the plan asks for -------------

test('checkRaceBudget FAILS on a synthetic BEP at the pre-fix duration', () => {
  const logs = []
  // 8 shards summing to the 512.5s that BOS-1022 set out to remove.
  const attempts = parseBepAttempts(shardedBep(DB, 512.5, 8))
  assert.equal(
    checkRaceBudget({ budgetDoc: budgetDoc(), attempts, log: (m) => logs.push(m) }),
    false,
  )
  assert.match(logs.join('\n'), /OVER BUDGET/)
  assert.match(logs.join('\n'), /512\.5s.*budget 420s/)
})

test('checkRaceBudget PASSES on a synthetic BEP at the post-fix duration', () => {
  const logs = []
  const attempts = parseBepAttempts(shardedBep(DB, 170.4, 8))
  assert.equal(
    checkRaceBudget({ budgetDoc: budgetDoc(), attempts, log: (m) => logs.push(m) }),
    true,
  )
  assert.match(logs.join('\n'), /170\.4s across 8 shard\(s\), budget 420s — ok/)
})

test('checkRaceBudget fails when evidence is missing rather than reporting under budget', () => {
  const logs = []
  assert.equal(
    checkRaceBudget({ budgetDoc: budgetDoc(), attempts: [], log: (m) => logs.push(m) }),
    false,
  )
  assert.match(logs.join('\n'), /no testResult events/)
})

test('checkRaceBudget fails on a cache-served run rather than scoring it', () => {
  const logs = []
  const attempts = parseBepAttempts(bep([testResultEvent(DB, 1, { cached: true })]))
  assert.equal(
    checkRaceBudget({ budgetDoc: budgetDoc(), attempts, log: (m) => logs.push(m) }),
    false,
  )
  assert.match(logs.join('\n'), /served from cache/)
})

test('checkRaceBudget refuses to run at all with an invalid budget file', () => {
  const logs = []
  const attempts = parseBepAttempts(shardedBep(DB, 10, 1))
  const doc = budgetDoc({ combination: 'max-across-shards' })
  assert.equal(checkRaceBudget({ budgetDoc: doc, attempts, log: (m) => logs.push(m) }), false)
  assert.match(logs.join('\n'), /invalid budget file/)
  assert.doesNotMatch(logs.join('\n'), /— ok/)
})
