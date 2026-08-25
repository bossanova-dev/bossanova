#!/usr/bin/env node

// Ratchet: per-package `-race` wall-clock budgets for the Go test targets that
// BOS-1022 made cheap, so the cost cannot silently creep back.
//
// Why this exists: `services/bossd/internal/db` and `.../internal/server` once
// cost 512.5s and 400.4s under a single-process `go test -race`. The fix was a
// once-per-binary schema build (services/bossd/internal/dbtest). Nothing stops
// the next contributor reintroducing a per-test migration replay, and the
// regression would be invisible — the suite stays green, just slower, and one
// slow suite looks exactly like a busy runner.
//
// What it reads: the Build Event Protocol stream `bazel test` writes with
// --build_event_json_file. Each `testResult` event carries the label, the run /
// shard / attempt it belongs to, its status, whether it was served from cache,
// and `testAttemptDurationMillis` — the shard's actual wall clock.
//
// Why not JUnit test.xml: rules_go writes `<testsuites></testsuites>` — no
// duration at all — for a *passing* test target, so a test.xml-based gate can
// never score a green run. Setting GO_TEST_WRAP_TESTV=1 populates it, but with
// one <testsuite> per Go test carrying that test's own time, which under
// `t.Parallel()` sums to something that is not the shard's wall clock. The BEP
// duration is the number Bazel itself reports, needs no test-wrapper opt-in,
// and is written client-side so it survives --remote_download_minimal.
//
// Shard combination rule: durations are SUMMED across a target's shards, never
// maxed. Per-shard budgets are gameable by exactly the knob BOS-1022 declares
// out of scope — raising `shard_count` would shrink every shard and satisfy a
// per-shard bound while total CPU grew. Summing measures the thing the budget
// is actually defending.
//
// Freshness: a cached result reports a duration that belongs to a different
// execution, and reads as "under budget" if scored naively — the failure mode
// CLAUDE.md flags as "a cached Bazel gate run is weaker evidence than an
// uncached one". The BEP says so directly: `cachedLocally` / the
// `cached_remotely` execution-info flag. Any cached attempt is a hard failure,
// never a pass, which is what makes --nocache_test_results load-bearing rather
// than belt-and-braces. A non-PASSED attempt is likewise refused: a target that
// failed or timed out did not produce a measurement of a working suite.
//
// Exercised by scripts/check-race-budget.test.mjs and runnable via
// `make check-race-budget` or `node scripts/check-race-budget.mjs`.

import fs from 'node:fs'

import { isMainModule } from '../skills-toolbox/main-module.mjs'

export const DEFAULT_BUDGET_FILE = 'scripts/race-budgets.json'

/**
 * Parse a protobuf Duration rendered as JSON ("5.864s") into seconds.
 * Returns null when the shape is not recognised, so the caller can fail loudly.
 */
export function parseDurationSeconds(value) {
  if (typeof value !== 'string') return null
  const m = /^(\d+(?:\.\d+)?)s$/.exec(value.trim())
  if (!m) return null
  const seconds = Number(m[1])
  return Number.isFinite(seconds) ? seconds : null
}

/**
 * Pull the wall-clock seconds out of one BEP testResult payload.
 *
 * Bazel emits `testAttemptDurationMillis` (an int64, so JSON-encoded as a
 * string) and, depending on version, also `testAttemptDuration` as a Duration.
 * Accept either; throw when neither is usable so a BEP shape change fails loudly
 * instead of scoring 0.
 */
export function testAttemptSeconds(result, label = 'testResult') {
  const millis = result?.testAttemptDurationMillis
  if (millis !== undefined && millis !== null) {
    const n = Number(millis)
    if (!Number.isFinite(n) || n < 0) {
      throw new Error(
        `${label}: testAttemptDurationMillis is not a non-negative number: ${JSON.stringify(millis)}`,
      )
    }
    return n / 1000
  }
  const seconds = parseDurationSeconds(result?.testAttemptDuration)
  if (seconds === null || seconds < 0) {
    throw new Error(`${label}: no usable testAttemptDurationMillis/testAttemptDuration`)
  }
  return seconds
}

/** True when Bazel says this attempt came from a cache rather than an execution. */
export function isCachedAttempt(result) {
  if (result?.cachedLocally === true) return true
  const info = result?.executionInfo
  if (!info) return false
  return info.cachedRemotely === true || info.cached_remotely === true
}

/**
 * Read a --build_event_json_file stream and return the testResult attempts it
 * contains, one entry per (label, run, shard, attempt).
 *
 * Throws on a malformed line so a truncated or half-written BEP fails loudly.
 */
export function parseBepAttempts(text, sourceLabel = 'bep') {
  if (typeof text !== 'string' || text.trim() === '') {
    throw new Error(`${sourceLabel}: empty build event stream`)
  }
  const attempts = []
  const lines = text.split('\n')
  for (const [i, line] of lines.entries()) {
    if (line.trim() === '') continue
    let event
    try {
      event = JSON.parse(line)
    } catch (err) {
      throw new Error(`${sourceLabel}:${i + 1}: not valid JSON: ${err.message}`)
    }
    const id = event?.id?.testResult
    const result = event?.testResult
    if (!id || !result) continue
    if (typeof id.label !== 'string' || id.label === '') {
      throw new Error(`${sourceLabel}:${i + 1}: testResult event without a label`)
    }
    attempts.push({
      label: id.label,
      run: Number(id.run ?? 1),
      shard: Number(id.shard ?? 1),
      attempt: Number(id.attempt ?? 1),
      status: result.status,
      cached: isCachedAttempt(result),
      seconds: testAttemptSeconds(result, `${sourceLabel}: ${id.label}`),
    })
  }
  return attempts
}

/**
 * Reduce a target's attempts to the one that counts per (run, shard): the
 * highest attempt number, i.e. the retry Bazel actually reports as the result.
 */
export function finalAttempts(attempts) {
  const best = new Map()
  for (const a of attempts) {
    const key = `${a.run}/${a.shard}`
    const prev = best.get(key)
    if (!prev || a.attempt > prev.attempt) best.set(key, a)
  }
  return [...best.values()].sort((x, y) => x.run - y.run || x.shard - y.shard)
}

/**
 * Score one budget entry against the parsed BEP attempts. Returns
 * {label, measured, budget, shards, ok} or throws with a loud message when the
 * evidence is missing, cached, or not a clean pass.
 */
export function scoreTarget(entry, attempts) {
  const mine = attempts.filter((a) => a.label === entry.label)
  if (mine.length === 0) {
    throw new Error(
      `${entry.label}: no testResult events in the build event stream. The target did not run, ` +
        `or --bep points at a stream from a different invocation.`,
    )
  }
  const finals = finalAttempts(mine)
  const cached = finals.filter((a) => a.cached)
  if (cached.length > 0) {
    throw new Error(
      `${entry.label}: ${cached.length} of ${finals.length} shard(s) were served from cache, so their ` +
        `duration is not a measurement of this run. Re-run with --nocache_test_results.`,
    )
  }
  const bad = finals.filter((a) => a.status !== 'PASSED')
  if (bad.length > 0) {
    throw new Error(
      `${entry.label}: ${bad.length} of ${finals.length} shard(s) did not pass ` +
        `(${[...new Set(bad.map((a) => a.status))].join(', ')}); a failing target is not a budget measurement.`,
    )
  }
  const measured = Math.round(finals.reduce((sum, a) => sum + a.seconds, 0) * 100) / 100
  return {
    label: entry.label,
    measured,
    budget: entry.budgetSeconds,
    shards: finals.length,
    ok: measured <= entry.budgetSeconds,
  }
}

/** Validate the budget file's shape so a typo cannot silently disable the gate. */
export function validateBudgetFile(doc) {
  const problems = []
  if (!doc || typeof doc !== 'object') return ['budget file is not a JSON object']
  if (doc.combination !== 'summed-across-shards') {
    problems.push(
      `combination must be "summed-across-shards" (got ${JSON.stringify(doc.combination)})`,
    )
  }
  if (typeof doc.budgetRegime !== 'string' || doc.budgetRegime.trim() === '') {
    // BOS-1022 review finding: every second in this file belongs to a
    // measurement harness, and a reader who re-derives a budget from the wrong
    // one silently defeats the ratchet. Naming the regime is mandatory.
    problems.push('budgetRegime must name the harness budgetSeconds is enforced against')
  }
  if (!Array.isArray(doc.targets) || doc.targets.length === 0) {
    problems.push('targets must be a non-empty array')
    return problems
  }
  for (const [i, t] of doc.targets.entries()) {
    const at = `targets[${i}]`
    if (typeof t.label !== 'string' || !t.label.startsWith('//'))
      problems.push(`${at}.label must be a // Bazel label`)
    if (typeof t.budgetSeconds !== 'number' || !(t.budgetSeconds > 0)) {
      problems.push(`${at}.budgetSeconds must be a positive number`)
      continue
    }
    if (typeof t.bazelSummedPostFixSeconds !== 'number' || !(t.bazelSummedPostFixSeconds > 0)) {
      problems.push(
        `${at}.bazelSummedPostFixSeconds must be a positive number: the budget is enforced in that ` +
          `regime, so it must be derived from a figure measured in it`,
      )
    } else if (t.budgetSeconds <= t.bazelSummedPostFixSeconds) {
      problems.push(
        `${at}.budgetSeconds (${t.budgetSeconds}) must exceed bazelSummedPostFixSeconds ` +
          `(${t.bazelSummedPostFixSeconds}), or a green run would already be over budget`,
      )
    }
    if (typeof t.nativePreFixSeconds === 'number' && t.budgetSeconds >= t.nativePreFixSeconds) {
      // The whole point of the ratchet is that it would have failed before the
      // fix. A budget at or above the pre-fix figure proves nothing. The
      // comparison is deliberately cross-regime and conservative in the safe
      // direction: summing per-shard wall clock adds each shard's fixed startup
      // to the same total work, so the Bazel-summed pre-fix figure is strictly
      // larger than the native one. Staying below the native figure therefore
      // also stays below the enforced-regime one.
      problems.push(
        `${at}.budgetSeconds (${t.budgetSeconds}) must be strictly below nativePreFixSeconds (${t.nativePreFixSeconds}), ` +
          `or the budget could not have caught the regression it exists to catch`,
      )
    }
  }
  return problems
}

export function checkRaceBudget({ budgetDoc, attempts, log = console.error }) {
  const problems = validateBudgetFile(budgetDoc)
  if (problems.length > 0) {
    for (const p of problems) log(`race-budget: invalid budget file: ${p}`)
    return false
  }
  const results = []
  const errors = []
  for (const entry of budgetDoc.targets) {
    try {
      results.push(scoreTarget(entry, attempts))
    } catch (err) {
      errors.push(err.message)
    }
  }
  for (const r of results) {
    const verdict = r.ok ? 'ok' : 'OVER BUDGET'
    log(
      `race-budget: ${r.label}: ${r.measured}s across ${r.shards} shard(s), budget ${r.budget}s — ${verdict}`,
    )
  }
  for (const e of errors) log(`race-budget: ${e}`)
  const over = results.filter((r) => !r.ok)
  if (over.length > 0) {
    for (const r of over) {
      log(
        `race-budget: FAIL ${r.label} took ${r.measured}s (summed across ${r.shards} shard(s)) ` +
          `but its budget is ${r.budget}s.`,
      )
    }
  }
  return errors.length === 0 && over.length === 0
}

function parseArgs(argv) {
  const out = { budgetFile: DEFAULT_BUDGET_FILE, bep: null }
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i]
    if (a === '--budget-file') out.budgetFile = argv[++i]
    else if (a === '--bep') out.bep = argv[++i]
    else throw new Error(`unknown argument: ${a}`)
  }
  return out
}

if (isMainModule(import.meta.url)) {
  let args
  try {
    args = parseArgs(process.argv.slice(2))
  } catch (err) {
    console.error(`race-budget: ${err.message}`)
    console.error('usage: check-race-budget.mjs --bep <build_event_json_file> [--budget-file f]')
    process.exit(2)
  }
  if (!args.bep) {
    console.error(
      'race-budget: --bep is required (pass the file given to `bazel test --build_event_json_file`)',
    )
    process.exit(2)
  }
  let budgetDoc
  try {
    budgetDoc = JSON.parse(fs.readFileSync(args.budgetFile, 'utf8'))
  } catch (err) {
    console.error(`race-budget: cannot read budget file ${args.budgetFile}: ${err.message}`)
    process.exit(2)
  }
  let attempts
  try {
    attempts = parseBepAttempts(fs.readFileSync(args.bep, 'utf8'), args.bep)
  } catch (err) {
    console.error(`race-budget: ${err.message}`)
    process.exit(2)
  }
  process.exit(checkRaceBudget({ budgetDoc, attempts }) ? 0 : 1)
}
