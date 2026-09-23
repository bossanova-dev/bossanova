#!/usr/bin/env node

import assert from 'node:assert/strict'
import test from 'node:test'

import {
  CACHE_STATES,
  FAILURE_LINE_PATTERNS,
  gateEvidenceLine,
  readGateLog,
  renderGateEvidence,
} from './gate-log-lib.mjs'

// --- Fixtures: real log shapes, kept verbatim so a reworded upstream string is visible here ---

const BAZEL_FULLY_CACHED = [
  'INFO: Analyzed 60 targets (0 packages loaded, 0 targets configured).',
  'INFO: Found 60 test targets...',
  'INFO: Elapsed time: 1.204s, Critical Path: 0.02s',
  'INFO: Build completed successfully, 1 total action',
  'Executed 0 out of 60 tests: 60 tests pass.',
].join('\n')

const BAZEL_FULLY_EXECUTED = [
  'INFO: Analyzed 17 targets (3 packages loaded, 42 targets configured).',
  'INFO: Elapsed time: 118.771s, Critical Path: 41.55s',
  'Executed 17 out of 17 tests: 17 tests pass.',
].join('\n')

const BAZEL_PARTLY_EXECUTED = ['Executed 3 out of 60 tests: 60 tests pass.'].join('\n')

// A native Go leg and a Bazel leg in ONE log. Every `(cached)` token here belongs to a package Go
// did not re-run; the Bazel leg executed everything. Reading the token would report "cached".
const MIXED_NATIVE_AND_BAZEL = [
  'ok  \tgithub.com/recurser/bossalib/safego\t(cached)',
  'ok  \tgithub.com/recurser/bossalib/sqlutil\t(cached)',
  'ok  \tgithub.com/recurser/bossalib/tuidriver\t0.412s',
  'INFO: Elapsed time: 0.031s, Critical Path: 0.00s',
  'Executed 17 out of 17 tests: 17 tests pass.',
].join('\n')

const SHARDED_FAILURE = [
  'INFO: Found 4 test targets...',
  '//services/bossd/internal/session:session_test                           FAILED in 1 out of 4 (shard 4 of 4) in 12.3s',
  'FAILED: Build did NOT complete successfully',
  'Executed 4 out of 4 tests: 3 tests pass and 1 fails.',
].join('\n')

const RETRIED_FAILURE = [
  '//services/bossd/internal/session:flaky_test                             FAILED in 3 out of 4 in 8.1s',
].join('\n')

// Every TAP suite reports `# fail 0`; the job still exits non-zero because run-nontap-gates.mjs
// emits its own failure line AFTER the last summary.
const POST_SUMMARY_FAILURE = [
  'TAP version 13',
  'ok 1 - parses a cached summary',
  'ok 2 - parses an executed summary',
  '1..2',
  '# tests 2',
  '# suites 0',
  '# pass 2',
  '# fail 0',
  '# cancelled 0',
  '# skipped 0',
  '# todo 0',
  '# duration_ms 41.2',
  'not ok - non-TAP gate failed: scripts/check-skill-symbols.mjs',
  '# gate: scripts/check-skill-symbols.mjs',
  '# exit code 1',
].join('\n')

// The token appears inside test NAMES on passing lines, and once at line start on a real failure.
const FAILURE_TOKEN_IN_NAMES = [
  'ok 11 - renders a FAILED: line without counting it',
  'ok 12 - a not ok inside a name is still a pass',
  '--- PASS: TestFAILEDInOutParsing (0.00s)',
  '    --- PASS: TestFAILEDInOutParsing/not_ok (0.00s)',
  '--- FAIL: TestActuallyBroken (0.02s)',
  'FAIL\tgithub.com/recurser/bossd/internal/session\t0.031s',
].join('\n')

// --- R1: cache state comes from the Bazel executed-count line and nothing else ---

test('a fully cached Bazel summary reports fully-cached with its counts', () => {
  const reading = readGateLog(BAZEL_FULLY_CACHED)
  assert.equal(reading.cacheState, CACHE_STATES.fullyCached)
  assert.equal(reading.executedTests, 0)
  assert.equal(reading.totalTests, 60)
})

test('a fully executed Bazel summary reports fully-executed with its counts', () => {
  const reading = readGateLog(BAZEL_FULLY_EXECUTED)
  assert.equal(reading.cacheState, CACHE_STATES.fullyExecuted)
  assert.equal(reading.executedTests, 17)
  assert.equal(reading.totalTests, 17)
})

test('a partial Bazel summary reports partly-executed, distinct from both extremes', () => {
  const reading = readGateLog(BAZEL_PARTLY_EXECUTED)
  assert.equal(reading.cacheState, CACHE_STATES.partlyExecuted)
  assert.notEqual(reading.cacheState, CACHE_STATES.fullyCached)
  assert.notEqual(reading.cacheState, CACHE_STATES.fullyExecuted)
})

test('a log with no Bazel summary is unknown, never a pass-equivalent', () => {
  const reading = readGateLog('ok 1 - something\n1..1\n# fail 0\n')
  assert.equal(reading.cacheState, CACHE_STATES.unknown)
  assert.equal(reading.executedTests, null)
  assert.equal(reading.totalTests, null)
  // Unknown is its own answer: it must not collapse into any of the three that describe a run.
  for (const state of [
    CACHE_STATES.fullyCached,
    CACHE_STATES.partlyExecuted,
    CACHE_STATES.fullyExecuted,
  ]) {
    assert.notEqual(reading.cacheState, state)
  }
  const rendered = renderGateEvidence(reading)
  assert.match(rendered, /cache: unknown \(no Bazel executed-count line in this log\)/)
  assert.doesNotMatch(rendered, /cached/)
})

test('an empty log is unknown rather than cached', () => {
  assert.equal(readGateLog('').cacheState, CACHE_STATES.unknown)
  assert.equal(readGateLog(null).cacheState, CACHE_STATES.unknown)
  assert.equal(readGateLog(undefined).cacheState, CACHE_STATES.unknown)
})

test('a Bazel leg with zero test targets is unknown, not fully cached', () => {
  // `Executed 0 out of 0 tests` states that nothing was SELECTED, which is not a cache claim.
  assert.equal(readGateLog('Executed 0 out of 0 tests: no tests.').cacheState, CACHE_STATES.unknown)
})

test('a (cached) token from a native Go leg cannot move the reported cache state', () => {
  const reading = readGateLog(MIXED_NATIVE_AND_BAZEL)
  assert.match(MIXED_NATIVE_AND_BAZEL, /\(cached\)/)
  assert.equal(reading.cacheState, CACHE_STATES.fullyExecuted)
  assert.equal(reading.executedTests, 17)

  // And the token alone, with no Bazel leg at all, yields no cache claim whatsoever.
  const tokenOnly = readGateLog('ok  \tgithub.com/recurser/bossalib/safego\t(cached)\n')
  assert.equal(tokenOnly.cacheState, CACHE_STATES.unknown)
})

test('elapsed time is not an input to the cache state', () => {
  const slowCached = readGateLog(
    ['INFO: Elapsed time: 942.118s, Critical Path: 900.01s', 'Executed 0 out of 60 tests.'].join(
      '\n',
    ),
  )
  assert.equal(slowCached.cacheState, CACHE_STATES.fullyCached)

  const fastExecuted = readGateLog(
    ['INFO: Elapsed time: 0.204s, Critical Path: 0.00s', 'Executed 60 out of 60 tests.'].join('\n'),
  )
  assert.equal(fastExecuted.cacheState, CACHE_STATES.fullyExecuted)

  // Elapsed time with no summary at all stays unknown, however long or short it is.
  assert.equal(
    readGateLog('INFO: Elapsed time: 0.004s, Critical Path: 0.00s').cacheState,
    CACHE_STATES.unknown,
  )
})

test('an executed-count line quoted mid-sentence supplies no cache state', () => {
  const quoted = 'the gate prints Executed 0 out of 60 tests when everything is cached'
  assert.equal(readGateLog(quoted).cacheState, CACHE_STATES.unknown)
})

test('several Bazel legs in one log are summed rather than last-one-wins', () => {
  const reading = readGateLog(`${BAZEL_FULLY_CACHED}\n${BAZEL_FULLY_EXECUTED}`)
  assert.equal(reading.executedTests, 17)
  assert.equal(reading.totalTests, 77)
  assert.equal(reading.cacheState, CACHE_STATES.partlyExecuted)
})

// --- R2: a shard denominator is reported as one, with retry evidence stated ---

test('a sharded failure reports a shard denominator with retry evidence explicitly absent', () => {
  const [failure, ...rest] = readGateLog(SHARDED_FAILURE).shardFailures
  assert.equal(rest.length, 0)
  assert.equal(failure.target, '//services/bossd/internal/session:session_test')
  assert.equal(failure.failedRuns, 1)
  assert.equal(failure.totalRuns, 4)
  assert.equal(failure.denominator, 'shards')
  assert.equal(failure.shard, 4)
  assert.equal(failure.shardCount, 4)
  assert.equal(failure.retryEvidence, 'absent')

  const rendered = renderGateEvidence(readGateLog(SHARDED_FAILURE))
  assert.match(rendered, /1 of 4 shards, retry evidence absent/)
  assert.doesNotMatch(rendered, /retry evidence present/)
})

test('an unsharded FAILED in line is a retry denominator, which is the other answer', () => {
  const [failure] = readGateLog(RETRIED_FAILURE).shardFailures
  assert.equal(failure.denominator, 'runs')
  assert.equal(failure.retryEvidence, 'present')
  assert.equal(failure.shard, null)
  assert.equal(failure.shardCount, null)
})

test('a log with no FAILED in line reports no shard failures', () => {
  assert.deepEqual(readGateLog(BAZEL_FULLY_EXECUTED).shardFailures, [])
})

// --- R3: a failure after the last TAP summary is reported as such ---

test('a failure past the last TAP summary is reported even though every suite reads fail 0', () => {
  const reading = readGateLog(POST_SUMMARY_FAILURE)
  assert.match(POST_SUMMARY_FAILURE, /# fail 0/)
  assert.equal(reading.postSummaryFailures.length, 1)
  assert.match(reading.postSummaryFailures[0], /not ok - non-TAP gate failed:/)
  assert.match(renderGateEvidence(reading), /post-summary failures: 1/)
  assert.match(renderGateEvidence(reading), /first past the last TAP summary: not ok/)
})

test('a failure BEFORE the last summary is a failure line but not a post-summary failure', () => {
  const reading = readGateLog(
    ['TAP version 13', 'not ok 1 - broken', '1..1', '# fail 1', '# duration_ms 3'].join('\n'),
  )
  assert.equal(reading.failureLines.length, 1)
  assert.deepEqual(reading.postSummaryFailures, [])
})

test('a log with no TAP summary at all invents no post-summary finding', () => {
  const reading = readGateLog('--- FAIL: TestThing (0.01s)\nFAIL\tpkg\t0.02s\n')
  assert.equal(reading.failureLines.length, 2)
  assert.deepEqual(reading.postSummaryFailures, [])
})

// --- R4: failure tokens are anchored at line start ---

test('a failure token inside a test name is not counted, a line-start token is', () => {
  const reading = readGateLog(FAILURE_TOKEN_IN_NAMES)
  assert.equal(reading.failureLines.length, 2)
  assert.match(reading.failureLines[0], /^--- FAIL: TestActuallyBroken/)
  assert.match(reading.failureLines[1], /^FAIL\t/)
  for (const line of reading.failureLines) {
    assert.doesNotMatch(line, /^ok /)
    assert.doesNotMatch(line, /PASS/)
  }
})

test('an indented Go subtest failure is counted, an indented PASS is not', () => {
  const reading = readGateLog(
    ['    --- FAIL: TestParent/child (0.00s)', '    --- PASS: TestParent/other (0.00s)'].join('\n'),
  )
  assert.equal(reading.failureLines.length, 1)
  assert.match(reading.failureLines[0], /--- FAIL: TestParent\/child/)
})

test('every failure pattern is anchored at line start', () => {
  for (const pattern of FAILURE_LINE_PATTERNS) {
    assert.match(pattern.source, /^\^/, `${pattern} must be anchored at line start`)
  }
})

// --- R5 support: the rendered evidence line ---

test('the evidence line always carries all four counts, including the zeroes', () => {
  const rendered = renderGateEvidence(readGateLog(BAZEL_FULLY_CACHED))
  assert.match(rendered, /^evidence: /)
  assert.match(rendered, /cache: fully cached \(executed 0 of 60 tests - nothing re-ran\)/)
  assert.match(rendered, /failure lines: 0/)
  assert.match(rendered, /post-summary failures: 0/)
  assert.match(rendered, /shard-denominated failures: 0/)
})

test('the evidence line is a single line for every fixture', () => {
  for (const fixture of [
    BAZEL_FULLY_CACHED,
    BAZEL_FULLY_EXECUTED,
    MIXED_NATIVE_AND_BAZEL,
    SHARDED_FAILURE,
    POST_SUMMARY_FAILURE,
    FAILURE_TOKEN_IN_NAMES,
    '',
  ]) {
    const rendered = gateEvidenceLine(fixture)
    assert.equal(rendered.includes('\n'), false, `evidence must stay one line for: ${rendered}`)
    assert.match(rendered, /^evidence: /)
  }
})

test('gateEvidenceLine tolerates a missing log without throwing', () => {
  assert.match(gateEvidenceLine(null), /^evidence: cache: unknown/)
  assert.match(gateEvidenceLine(undefined), /^evidence: cache: unknown/)
})

// --- BOS-1276 follow-up: the shard reader is anchored, like the failure-line reader -------------

test('a PASSING TAP line that merely contains the shard phrasing is not a shard failure', () => {
  // This module's own test names carry the phrase, which is exactly why the target must be a
  // `//`-rooted Bazel label rather than a lazy line prefix. Before the anchor, this read as one
  // shard-denominated failure whose target was the test name.
  const passingTap = [
    'TAP version 13',
    'ok 1 - names the denominator when FAILED in 1 out of 4 (shard 4 of 4) is present',
    'ok 2 - an unsharded FAILED in 3 out of 4 line is a retry denominator',
    '1..2',
    '# tests 2',
    '# pass 2',
    '# fail 0',
  ].join('\n')
  const reading = readGateLog(passingTap)
  assert.deepEqual(reading.shardFailures, [])
  assert.match(renderGateEvidence(reading), /shard-denominated failures: 0/)
})

test('an indented bazel target still reports its shard failure', () => {
  const indented =
    '  //services/bossd/internal/session:session_test  FAILED in 1 out of 4 (shard 4 of 4) in 9s'
  const [failure] = readGateLog(indented).shardFailures
  assert.equal(failure.denominator, 'shards')
  assert.equal(failure.target, '//services/bossd/internal/session:session_test')
})
