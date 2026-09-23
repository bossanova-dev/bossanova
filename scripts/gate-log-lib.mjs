#!/usr/bin/env node
// Gate-log reader (BOS-1276).
//
// `readGateLog(text)` is a PURE read of a gate's own output. It decides nothing about exit codes —
// callers keep owning the verdict — and it answers only the four facts a gate result
// under-determines today, each of which has been misread in this repo:
//
//   1. cache state      — was anything actually re-run, or did a green come out of a cache?
//   2. shard failures    — `FAILED in X out of Y` counts SHARDS when `(shard S of Y)` is present,
//                          and reading that as retries turns a deterministic red into "a flake".
//   3. post-summary      — a failure emitted AFTER the last TAP summary, so every suite can read
//                          `# fail 0` while the job still exits non-zero.
//   4. failure lines     — anchored at line start, because this repo's test NAMES contain `FAIL`.
//
// Purity is the point: every reading above is exercised from a fixture string in
// scripts/gate-log-lib.test.mjs without spawning Bazel, `make`, or a test runner.

// The four answers R1 demands. `unknown` is a first-class answer, never a pass-equivalent: a log
// with no Bazel leg says nothing at all about what re-ran, and rendering that as "cached" or as
// "ok" is the exact substitution this reader exists to remove.
export const CACHE_STATES = Object.freeze({
  unknown: 'unknown',
  fullyCached: 'fully-cached',
  partlyExecuted: 'partly-executed',
  fullyExecuted: 'fully-executed',
})

// Bazel's own end-of-run summary, and the ONLY signal cache state is derived from.
//
// Deliberately not the `(cached)` token: a mixed leg (native `go test` plus Bazel) prints
// `ok <pkg> (cached)` for every package Go did not re-run, so a log whose Bazel leg executed
// everything still carries `(cached)` lines. Deliberately not elapsed time either — a warm remote
// cache and a wedged upload tail are indistinguishable by duration.
//
// Anchored at line start (leading indentation tolerated) so a line that merely QUOTES the wording
// — a test fixture, a doc excerpt echoed by a gate — cannot supply a cache state.
const BAZEL_EXECUTED_SUMMARY = /^[ \t]*Executed (\d+) out of (\d+) tests?\b/gm

// `//target:name    FAILED in 1 out of 4 (shard 4 of 4) in 12.3s`
//
// The optional `(shard S of Y)` group is what distinguishes a SHARD denominator from a retry
// denominator. Both spellings share the `X out of Y` prefix, which is why reading the prefix alone
// is wrong in one of the two cases.
//
// The target is REQUIRED to be a `//`-rooted Bazel label, for the same reason FAILURE_LINE_PATTERNS
// are line-anchored: a lazy `^(.*?)` prefix is not an anchor at all, so it matched any line merely
// CONTAINING the phrase. This module's own test names contain it, and a passing
// `ok 1 - names the denominator when FAILED in 1 out of 4 (shard 4 of 4) is present` was being
// reported as a shard-denominated failure whose target was the test name.
const BAZEL_FAILED_IN =
  /^[ \t]*(\/\/\S+)\s+FAILED in (\d+) out of (\d+)(?:\s*\(shard (\d+) of (\d+)\))?/gm

// TAP summary markers, used only to locate the END of the last summary block. `node --test`'s TAP
// reporter emits `1..N` plus `# tests/pass/fail/skipped/todo/duration_ms` lines.
const TAP_SUMMARY_LINE = /^(?:1\.\.\d+|# (?:tests|pass|fail|cancelled|skipped|todo|duration_ms)\b)/

// Failure tokens, every one anchored at line start. This repo really does name tests after the
// token — a passing `ok 7 - classifies a FAILED: line` is a PASS — so an unanchored scan reports
// failures that are passing lines. Leading whitespace is tolerated for Go subtests, which indent
// `--- FAIL:` under their parent.
//
// Deliberately NOT shared with TEST_FAILURE_MARKERS in scripts/env-failure-lib.mjs, which spells a
// near-identical set. The two answer different questions and must be free to diverge: that list
// decides whether to WITHHOLD a transport classification from a log that already reports a test
// failure, and widening it silently changes the exit code of a wrapped gate. This list only COUNTS
// lines for a report nobody branches on, so it can grow a token the classifier must never see.
// scripts/env-failure-lib.mjs is also copied standalone into fixture trees by
// scripts/check-background-gate-recipe.test.mjs, so importing this module from it would add a
// dependency that file does not otherwise have.
export const FAILURE_LINE_PATTERNS = Object.freeze([
  /^[ \t]*--- FAIL\b/,
  /^FAIL\b/,
  /^not ok\b/,
  /^FAILED: /,
])

const DETAIL_LIMIT = 120

function truncate(value) {
  const text = String(value ?? '').trim()
  return text.length > DETAIL_LIMIT ? `${text.slice(0, DETAIL_LIMIT)}...` : text
}

function isFailureLine(line) {
  return FAILURE_LINE_PATTERNS.some((pattern) => pattern.test(line))
}

// executed/total summed across every Bazel leg in the log: `make test-all` runs more than one
// bazel invocation, and a reader asking "did anything re-run" wants the whole log's answer.
function readCacheState(text) {
  let executed = null
  let total = null
  for (const match of text.matchAll(BAZEL_EXECUTED_SUMMARY)) {
    executed = (executed ?? 0) + Number.parseInt(match[1], 10)
    total = (total ?? 0) + Number.parseInt(match[2], 10)
  }
  // No Bazel leg at all, or a leg that had no test targets: neither statement can be made, so the
  // answer is `unknown` rather than a cache claim nobody can support.
  if (executed === null || total === null || total === 0) {
    return { cacheState: CACHE_STATES.unknown, executedTests: executed, totalTests: total }
  }
  if (executed === 0) {
    return { cacheState: CACHE_STATES.fullyCached, executedTests: executed, totalTests: total }
  }
  if (executed >= total) {
    return { cacheState: CACHE_STATES.fullyExecuted, executedTests: executed, totalTests: total }
  }
  return { cacheState: CACHE_STATES.partlyExecuted, executedTests: executed, totalTests: total }
}

function readShardFailures(text) {
  const failures = []
  for (const match of text.matchAll(BAZEL_FAILED_IN)) {
    const [line, rawTarget, rawFailed, rawTotal, rawShard, rawShardCount] = match
    const sharded = rawShard !== undefined && rawShardCount !== undefined
    failures.push({
      target: truncate(rawTarget),
      failedRuns: Number.parseInt(rawFailed, 10),
      totalRuns: Number.parseInt(rawTotal, 10),
      // The whole point of the reading: name what the denominator counts instead of leaving the
      // reader to assume. `shards` means Y is a shard count, so 1-of-4 is a deterministic failure
      // in one quarter of the target, NOT one flaky attempt out of four.
      denominator: sharded ? 'shards' : 'runs',
      shard: sharded ? Number.parseInt(rawShard, 10) : null,
      shardCount: sharded ? Number.parseInt(rawShardCount, 10) : null,
      // Stated, never implied. A sharded line carries no retry information whatsoever; an
      // unsharded `X out of Y` is bazel's flaky-attempt reporting, which does.
      retryEvidence: sharded ? 'absent' : 'present',
      line: truncate(line),
    })
  }
  return failures
}

export function readGateLog(text) {
  const input = String(text ?? '')
  const lines = input.split('\n')

  const lastSummaryIndex = lines.reduce(
    (found, line, index) => (TAP_SUMMARY_LINE.test(line) ? index : found),
    -1,
  )

  const failureLines = []
  const postSummaryFailures = []
  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index]
    if (!isFailureLine(line)) continue
    const trimmed = truncate(line)
    failureLines.push(trimmed)
    // Only meaningful when a TAP summary exists: with no summary in the log there is no "after the
    // summary" region, and calling every failure post-summary would invent the finding.
    if (lastSummaryIndex >= 0 && index > lastSummaryIndex) postSummaryFailures.push(trimmed)
  }

  return {
    ...readCacheState(input),
    shardFailures: readShardFailures(input),
    postSummaryFailures,
    failureLines,
  }
}

function describeCache({ cacheState, executedTests, totalTests }) {
  if (cacheState === CACHE_STATES.unknown) {
    return 'unknown (no Bazel executed-count line in this log)'
  }
  const counts = `executed ${executedTests} of ${totalTests} tests`
  if (cacheState === CACHE_STATES.fullyCached) return `fully cached (${counts} - nothing re-ran)`
  if (cacheState === CACHE_STATES.fullyExecuted) return `fully executed (${counts})`
  return `partly executed (${counts})`
}

// One line, fixed shape, every count always present. A count that disappears when it is zero reads
// as "not checked", which is the same under-determination the reader exists to remove.
export function renderGateEvidence(reading) {
  const parts = [
    `cache: ${describeCache(reading)}`,
    `failure lines: ${reading.failureLines.length}`,
    `post-summary failures: ${reading.postSummaryFailures.length}`,
    `shard-denominated failures: ${reading.shardFailures.length}`,
  ]
  const [firstPostSummary] = reading.postSummaryFailures
  if (firstPostSummary) parts.push(`first past the last TAP summary: ${firstPostSummary}`)
  const [firstShard] = reading.shardFailures
  if (firstShard) {
    parts.push(
      `first shard failure: ${firstShard.target} ${firstShard.failedRuns} of ` +
        `${firstShard.totalRuns} ${firstShard.denominator}, retry evidence ${firstShard.retryEvidence}`,
    )
  }
  return `evidence: ${parts.join(' - ')}`
}

// Convenience for the one consumer shape every caller has: a log tail (or nothing at all).
export function gateEvidenceLine(text) {
  return renderGateEvidence(readGateLog(text))
}
