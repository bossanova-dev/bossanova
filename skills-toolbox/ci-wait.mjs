#!/usr/bin/env node

// ci-wait.mjs — the bounded fallback CI wait, as resumable tool-call-sized chunks.
//
// A shell `while … sleep` loop cannot back this wait: its budget outlives one agent tool call, no
// shell state survives between calls, a harness may refuse or neuter the foreground `sleep`, and a
// call killed mid-wait leaves an exit code that is the harness's, not the wait's. This helper owns
// every one of those pieces instead:
//
//   - one `run` returns within `chunkMs` (+ process start), and the CLI refuses any configuration
//     whose `chunkMs + readTimeoutMs` exceeds CEILING_MS, so no call it is asked for can outlive
//     the tool ceiling;
//   - the budget is WALL-CLOCK and lives in a state file keyed by PR and head SHA, which the helper
//     derives from its own read — the caller carries nothing between calls;
//   - the delay between reads is `boss pr wait --until checks-settled`, which the daemon's webhooks
//     wake, while readings are daemon-served; otherwise it is an in-process timer whose elapsed wall
//     time is verified, so an inert delay ends the wait as `unknown`/`delay-inert`;
//   - a chunk leaves an open-chunk marker until it closes, so a killed chunk is detected and
//     counted by the next `run`, and three in a row end the wait rather than retrying forever.
//
// Readings come from the daemon (`boss pr state`, ./pr-state.mjs) and from labelled `gh` only when
// no daemon answers; the verdict says which (`source`). The verdict itself is `classifyChecks` from
// ./pr-check-state.mjs over the raw checks and runs, never the daemon's own verdict; this module
// restates none of its rules, it only maps them onto the CI_WAIT_STATE vocabulary. Every reading
// carries the head SHA's workflow runs, so a set read before the head's workflows attached their
// jobs is `pending`, not green; a reading whose runs were not read never settles.
//
//   settled   green on two consecutive reads of the     terminal
//             same head, the second no smaller
//   failed    failing                                   terminal
//   timeout   budget exhausted while still waiting      terminal (carries `trend`)
//   unknown   anything the classifier will not call     terminal (an `absent-gate` one carries
//             green or red, plus `delay-inert` and      `absentGateRemedy`)
//             `repeated-interruption` or `stranded-check` (carries `stranded` and `strandedRemedy`)
//   continue  chunk used up, budget left                NOT terminal — issue `run` again in a new
//                                                       call; never assign it to CI_WAIT_STATE
//
// A pending `boss/verify` claim (`verifying… <token>`) or `waiting:` hold on the head is the verify
// stage's work, not ordinary CI: the build and repair waits run before verify, so waiting on it would
// wait on a later stage. When the rollup carries a pending `boss/verify` node, the daemon's verify
// phase or, on gh, one extra read of the head's commit statuses classifies it; only a
// pending claim, stale claim or waiting hold is dropped, and it is reported in `verifyHold`. Every
// terminal verify status, and any pending one the read cannot confirm, still counts. A prior-head
// `boss/verify` status judged that head and is never a gate the new head must restore.
//
// A repo's `.boss-skills.json` `ci.required` (read once per `run`, for the PR's base) names checks
// that must have RUN on the head. A head with a workflow run but missing one ends the wait
// `unknown`/`not-requested` once that head's consecutive reads have agreed for
// NOT_REQUESTED_SETTLE_MS, and never settles green on the light checks that did run; the missing
// names are in `notRequested`. A head with no workflow run yet keeps waiting. With nothing required
// the wait is exactly as before.
//
// Project-agnostic: only `boss`, `gh`, Node builtins and sibling toolbox modules.

import { createHash } from 'node:crypto'
import {
  mkdirSync,
  readFileSync,
  readdirSync,
  renameSync,
  rmSync,
  statSync,
  writeFileSync,
} from 'node:fs'
import os from 'node:os'
import path from 'node:path'

import { isMainModule } from './main-module.mjs'
import {
  CHECK_REASONS,
  CHECK_STATES,
  absentGateRemedy,
  classifyChecks,
  strandedRemedy,
} from './pr-check-state.mjs'
import {
  FALLBACK_LABELS,
  PR_WAIT_CAP_MS,
  daemonFirstReader,
  ghCheckReader,
  waitAnswered,
  waitPRState,
} from './pr-state.mjs'
import { VERIFY_CONTEXT } from './verify/verify-vocabulary.mjs'
import {
  VERIFY_HOLD_LABELS,
  dropContext,
  hasPendingVerifyNode,
  verifyHoldFromPhase,
  verifyHoldOf,
} from './ci/verify-hold.mjs'
import { confirmsGreen, freshGreenAnchor } from './ci/green-confirmation.mjs'
import { requiredChecksFor } from './ci/required-checks.mjs'
import { loadSkillConfig } from './skill-config.mjs'

export const CEILING_MS = 570000
export const DEFAULTS = Object.freeze({
  budgetMs: 60 * 60 * 1000,
  intervalMs: 30 * 1000,
  chunkMs: 480 * 1000,
  readTimeoutMs: 60 * 1000,
  extendMs: 30 * 60 * 1000,
  daemonWaitMs: 240 * 1000,
})
export const TREND_WINDOW_MS = 10 * 60 * 1000
export const INTERRUPTION_LIMIT = 3
// How long a head must keep reading not-requested before the wait ends on it: well past the lag
// between a push and Actions creating that head's workflow runs.
export const NOT_REQUESTED_SETTLE_MS = 3 * 60 * 1000
// Allow GitHub time to publish check completion after its workflow completed.
export const STRANDED_SETTLE_MS = 5 * 60 * 1000
// Delays shorter than this cannot be told apart from an inert timer, so none is ever requested.
export const MIN_DELAY_MS = 1000
// A read is only started when at least this much chunk time remains for it.
const READ_FLOOR_MS = 1000
const READINGS_KEPT =
  Math.ceil(Math.max(NOT_REQUESTED_SETTLE_MS, STRANDED_SETTLE_MS) / MIN_DELAY_MS) + 2

export const WAIT_STATES = Object.freeze({
  SETTLED: 'settled',
  FAILED: 'failed',
  TIMEOUT: 'timeout',
  UNKNOWN: 'unknown',
  CONTINUE: 'continue',
})

export const WAIT_REASONS = Object.freeze({
  BUDGET_EXHAUSTED: 'budget-exhausted',
  CHUNK_EXHAUSTED: 'chunk-exhausted',
  HEAD_CHANGED: 'head-changed',
  DELAY_INERT: 'delay-inert',
  REPEATED_INTERRUPTION: 'repeated-interruption',
  STRANDED_CHECK: 'stranded-check',
})

export const TRENDS = Object.freeze({
  CONVERGING: 'converging',
  FROZEN: 'frozen',
  UNKNOWN: 'unknown',
})

export class UsageError extends Error {}

// mapVerdict turns one classifier verdict into a wait decision. `wait: true` means keep reading.
export function mapVerdict(verdict) {
  if (verdict.state === CHECK_STATES.GREEN) {
    return { wait: false, state: WAIT_STATES.SETTLED, reason: verdict.reason }
  }
  if (verdict.state === CHECK_STATES.FAILING) {
    return { wait: false, state: WAIT_STATES.FAILED, reason: verdict.reason }
  }
  if (verdict.state === CHECK_STATES.PENDING && verdict.reason !== CHECK_REASONS.ABSENT_GATE) {
    return { wait: true, state: null, reason: verdict.reason }
  }
  if (
    verdict.state === CHECK_STATES.UNKNOWN &&
    (verdict.reason === CHECK_REASONS.NO_CHECKS || verdict.reason === CHECK_REASONS.UNREADABLE)
  ) {
    return { wait: true, state: null, reason: verdict.reason }
  }
  return { wait: false, state: WAIT_STATES.UNKNOWN, reason: verdict.reason }
}

// trendOf reads the pending counts of the reads inside the last TREND_WINDOW_MS. A fall between
// any two consecutive reads in that window is `converging`; none is `frozen`; fewer than two
// countable reads is `unknown`, because one reading cannot show a direction. The window ends at the
// newest read of any kind: a run of unreadable reads ages an old fall out rather than preserving it.
export function trendOf(readings) {
  const all = (readings ?? []).filter((r) => Number.isFinite(r?.atMs))
  const countable = all.filter((r) => Number.isFinite(r?.pending))
  if (countable.length < 2) return TRENDS.UNKNOWN
  const lastAt = Math.max(...all.map((r) => r.atMs))
  const window = countable.filter((r) => r.atMs >= lastAt - TREND_WINDOW_MS)
  if (window.length < 2) return TRENDS.UNKNOWN
  for (let i = 1; i < window.length; i += 1) {
    if (window[i].pending < window[i - 1].pending) return TRENDS.CONVERGING
  }
  return TRENDS.FROZEN
}

function newRecord({ pr, headSha, atMs, options }) {
  return {
    version: 1,
    pr,
    headSha,
    startedAtMs: atMs,
    updatedAtMs: atMs,
    budgetMs: options.budgetMs,
    extended: false,
    extendMs: 0,
    reads: 0,
    readings: [],
    openChunk: null,
    chunks: 0,
    interruptedChunks: 0,
    consecutiveInterruptions: 0,
    lastVerdict: null,
  }
}

function shortSha(sha) {
  return sha ? sha.slice(0, 7) : '(unknown head)'
}

function seconds(ms) {
  return `${Math.round(ms / 1000)}s`
}

// takeReading performs one read — the rollup, then the head SHA's workflow runs, then the
// named-context payload only when the rollup carries a null-shaped node — and classifies it. Each
// `gh` call gets `min(readTimeoutMs, time left before deadline)`, so the reads together can never
// outlive the chunk. It never throws: a failed read is `unreadable`.
//
// The runs read is what makes green decidable: without it a check set read before the head's
// workflows attached their jobs is indistinguishable from a complete one. A failed runs read
// therefore downgrades a reading that would be green or absent-gate to `unreadable` (keep waiting);
// a failing or pending reading needs no run evidence and keeps its own verdict.
//
// A pending `boss/verify` node adds the statuses read (see the header); with too little time left
// for it the node stays, so the reading stays pending and the next read retries.
export async function takeReading({
  read,
  pr,
  readTimeoutMs,
  deadline,
  priorContexts,
  ciRequired,
  now,
  fresh = false,
}) {
  const timeoutFor = () => Math.min(readTimeoutMs, deadline - now())
  const noHold = { verifyHold: null, excludedContexts: [], verifyStatus: null }
  const view = await read({ what: 'view', pr, fresh, timeoutMs: timeoutFor() })
  if (!view?.ok) {
    const verdict = classifyChecks({ readError: view?.error ?? 'read failed' })
    return { atMs: now(), headSha: '', verdict, fresh: false, ...noHold }
  }
  const headSha = typeof view.headSha === 'string' ? view.headSha : ''
  let runsError = 'no time left to read workflow runs'
  let workflowRuns = null
  if (headSha === '') {
    runsError = 'no head SHA to read workflow runs for'
  } else if (timeoutFor() >= READ_FLOOR_MS) {
    const runs = await read({ what: 'runs', pr, headSha, timeoutMs: timeoutFor() })
    if (runs?.ok) {
      workflowRuns = runs.payload ?? []
      runsError = null
    } else {
      runsError = runs?.error ?? 'workflow runs read failed'
    }
  }
  let held = { status: null, hold: null }
  if (typeof view.payload?.verifyPhase === 'string') {
    held = verifyHoldFromPhase(view.payload)
  } else if (
    headSha !== '' &&
    hasPendingVerifyNode(view.payload) &&
    timeoutFor() >= READ_FLOOR_MS
  ) {
    const statuses = await read({ what: 'verify-status', pr, headSha, timeoutMs: timeoutFor() })
    held = verifyHoldOf(statuses, headSha, now())
  }
  const withoutHold =
    held.hold === null ? (payload) => payload : (payload) => dropContext(payload, VERIFY_CONTEXT)
  const classify = (buckets) =>
    classifyChecks({
      headSHA: headSha,
      observedSHA: headSha,
      rollup: withoutHold(view.payload ?? null),
      buckets: withoutHold(buckets),
      priorContexts: dropContext(priorContexts, VERIFY_CONTEXT),
      workflowRuns,
      requiredChecks: requiredChecksFor(ciRequired, view.payload?.baseRefName ?? ''),
    })
  let verdict = classify(null)
  if (verdict.reason === CHECK_REASONS.UNCLASSIFIED && timeoutFor() >= READ_FLOOR_MS) {
    const checks = await read({ what: 'checks', pr, timeoutMs: timeoutFor() })
    if (checks?.ok) verdict = classify(checks.payload ?? null)
  }
  if (
    runsError !== null &&
    (verdict.state === CHECK_STATES.GREEN ||
      verdict.reason === CHECK_REASONS.ABSENT_GATE ||
      verdict.reason === CHECK_REASONS.NOT_REQUESTED)
  ) {
    verdict = classifyChecks({
      headSHA: headSha,
      observedSHA: headSha,
      readError: `workflow runs: ${runsError}`,
    })
  }
  return {
    atMs: now(),
    headSha,
    verdict,
    fresh: view.fresh === true,
    verifyHold: held.hold,
    excludedContexts: held.hold === null ? [] : [VERIFY_CONTEXT],
    verifyStatus: held.status,
  }
}

function recordReading(record, reading) {
  record.reads += 1
  record.updatedAtMs = reading.atMs
  const v = reading.verdict
  // What the last reading says about missing and unfinished work, so a terminal verdict names it.
  record.lastDetail = {
    absent: v.absent ?? [],
    notTriggered: v.notTriggered ?? [],
    pendingRuns: v.pendingRuns ?? [],
    notRequested: v.notRequested ?? [],
    absentGateRemedy: absentGateRemedy(v),
    stranded: v.stranded ?? [],
    strandedRemedy: strandedRemedy(v),
    verifyHold: reading.verifyHold ?? null,
    excludedContexts: reading.excludedContexts ?? [],
    // The raw latest `boss/verify` status the statuses read returned, kept as evidence.
    verifyStatus: reading.verifyStatus ?? null,
  }
  record.readings.push({
    atMs: reading.atMs,
    headSha: reading.headSha,
    // Whether the reading bypassed every read cache; only a fresh daemon reading may confirm a green.
    fresh: reading.fresh === true,
    strandedKey: JSON.stringify(v.stranded ?? []),
    strandedOnly:
      v.state === CHECK_STATES.PENDING &&
      v.reason === CHECK_REASONS.PENDING &&
      v.pendingRuns?.length === 0 &&
      v.stranded?.length > 0 &&
      v.stranded.length === v.pending,
    state: reading.verdict.state,
    reason: reading.verdict.reason,
    // An unreadable read counted nothing; a zero here would read as a fall to the trend.
    pending: reading.verdict.reason === CHECK_REASONS.UNREADABLE ? null : reading.verdict.pending,
    total: reading.verdict.reason === CHECK_REASONS.UNREADABLE ? null : reading.verdict.total,
  })
  if (record.readings.length > READINGS_KEPT) {
    record.readings.splice(0, record.readings.length - READINGS_KEPT)
  }
}

function heldFor(record, matches, windowMs) {
  const n = record.readings.length
  const last = record.readings[n - 1]
  if (!last || !matches(last)) return false
  let first = n - 1
  while (
    first > 0 &&
    matches(record.readings[first - 1]) &&
    record.readings[first - 1].headSha === last.headSha
  ) {
    first -= 1
  }
  return first < n - 1 && last.atMs - record.readings[first].atMs >= windowMs
}

function totalBudget(record) {
  return record.budgetMs + (record.extended ? record.extendMs : 0)
}

// resumable returns the record a later `run` on its head resumes, and null when that run is a new
// wait. Only a concluded CI run — `settled` or `failed` — ends the wait: jobs rerun on that head are a
// new CI run with a fresh budget. Every other record is resumed, including a `timeout` or `unknown`
// one, so its budget, read count, interruption count and single extension stay latched (R3, R5, R6).
function resumable(record) {
  if (record == null) return null
  const ended = record.lastVerdict?.state
  return ended === WAIT_STATES.SETTLED || ended === WAIT_STATES.FAILED ? null : record
}

// runChunk is the whole wait for one tool call. Every side effect is injected:
//   state   a per-PR store: { load(headSha), latest(), save(record), remove(headSha) };
//           `load('')` is the placeholder used while no read has named the head yet
//   read    async ({what: 'view'|'runs'|'checks'|'verify-status', pr, headSha?, timeoutMs})
//           → {ok, headSha?, payload?, error?}; `runs` and `verify-status` carry the head SHA they
//           read for; `verify-status` is the head's commit statuses (`gh api …/statuses`);
//           optional `read.source()` → 'daemon'|'gh-fallback'|null and `read.fallbackReason()`
//   wait    optional ({pr, until, timeoutMs}) → a pr-state.mjs bossCall result; the delay while
//           `read.source()` is 'daemon'
//   sleep   async (ms) → void; the elapsed wall time is verified against `now`
//   now     () → epoch ms
//   options {pr, budgetMs, intervalMs, chunkMs, readTimeoutMs, daemonWaitMs, extend, extendMs,
//           priorContexts}
// It returns {verdict, record, messages}; `messages` are the stderr lines, in order.
export async function runChunk({ state, read, wait = null, sleep, now, options }) {
  const opts = { ...DEFAULTS, ...options }
  const { pr } = opts
  const messages = []
  const chunkStart = now()
  const deadline = chunkStart + opts.chunkMs
  const remainingChunk = () => deadline - now()

  // R8: every run takes a read before any delay. The first gh fallback in a chunk says so once.
  let fallbackNoted = false
  const readOnce = async ({ fresh = false } = {}) => {
    const reading = await takeReading({
      read,
      pr,
      readTimeoutMs: opts.readTimeoutMs,
      deadline,
      priorContexts: opts.priorContexts ?? null,
      ciRequired: opts.ciRequired ?? null,
      now,
      fresh,
    })
    if (!fallbackNoted && read.source?.() === 'gh-fallback') {
      fallbackNoted = true
      messages.push(
        `ci-wait: PR #${pr}: no daemon answered (${read.fallbackReason?.() ?? 'unknown'}); ` +
          `reading with gh, labelled ${FALLBACK_LABELS.CI_WAIT}`,
      )
    }
    return reading
  }

  // The delay is verified: one that returns well short of its length is an inert timer.
  const verifiedSleep = async (ms) => {
    const before = now()
    await sleep(ms)
    const slept = now() - before
    if (slept >= ms - Math.max(50, ms * 0.05)) return true
    messages.push(
      `ci-wait: PR #${pr}: a ${ms}ms delay returned after ${slept}ms — the timer is inert`,
    )
    return false
  }

  // daemonGap is the delay between daemon-served readings: `boss pr wait --until checks-settled`,
  // within the budget and the chunk less a read floor. A wait that answered is a real delay; one
  // under MIN_DELAY_MS is topped up with a verified sleep, and a second early answer in a row (the
  // daemon's condition holds while this verdict still waits) is paced at the interval instead. The
  // gap before a confirming read (`confirmAtMs` set) lasts until one interval after the fresh green
  // it confirms, or MIN_DELAY_MS when no fresh green stands yet and the next read must take one. A
  // wait that did not answer hands the rest of the chunk to the timer.
  let daemonWait = typeof wait === 'function'
  let earlyAnswers = 0
  const daemonGap = async (budgetLeft, confirmAtMs) => {
    const limits = [opts.daemonWaitMs, PR_WAIT_CAP_MS, budgetLeft, remainingChunk() - READ_FLOOR_MS]
    const timeoutMs = Math.min(...limits)
    if (timeoutMs < MIN_DELAY_MS) return 'continue'
    const before = now()
    const answer = await wait({ pr, until: 'checks-settled', timeoutMs })
    if (!waitAnswered(answer)) {
      daemonWait = false
      messages.push(
        `ci-wait: PR #${pr}: boss pr wait did not answer (${answer?.reason ?? answer?.error ?? 'no answer'}); ` +
          `delaying by the ${seconds(opts.intervalMs)} timer for the rest of this chunk`,
      )
      return 'timer'
    }
    const took = now() - before
    earlyAnswers = took < opts.intervalMs ? earlyAnswers + 1 : 0
    let floor = earlyAnswers > 1 ? Math.min(opts.intervalMs, budgetLeft) : MIN_DELAY_MS
    if (confirmAtMs !== undefined) {
      floor = Math.max(MIN_DELAY_MS, Math.min(confirmAtMs - before, budgetLeft))
    }
    const topUp = floor - took
    if (topUp <= 0) return 'read'
    if (remainingChunk() - topUp < READ_FLOOR_MS) return 'continue'
    return (await verifiedSleep(topUp)) ? 'read' : 'inert'
  }

  // R5: the chunk is marked open BEFORE its first read, so a chunk killed during that read is still
  // counted. The head is not known yet, so the marker goes on the wait this run resumes (see
  // resumable) or, when there is none, on a new placeholder. A wait whose CI concluded (settled or
  // failed) is never resumed, so neither an unreadable read nor a rerun on the same head inherits it;
  // a timed-out or unknown wait is resumed, and one that ended repeated-interruption stays at its
  // retry bound.
  const openChunk = (rec) => {
    let limitReached = false
    if (rec.lastVerdict?.reason === WAIT_REASONS.REPEATED_INTERRUPTION) {
      limitReached = true
    } else if (rec.openChunk != null) {
      // An open-chunk marker left behind is a chunk that never wrote its verdict.
      rec.interruptedChunks += 1
      rec.consecutiveInterruptions += 1
      messages.push(
        `ci-wait: PR #${pr}: the previous chunk (opened ${new Date(rec.openChunk.startedAtMs).toISOString()}) ` +
          `ended without a verdict — interrupted chunk ${rec.consecutiveInterruptions} of ${INTERRUPTION_LIMIT}`,
      )
      limitReached = rec.consecutiveInterruptions >= INTERRUPTION_LIMIT
    } else {
      rec.consecutiveInterruptions = 0
    }
    rec.openChunk = { startedAtMs: chunkStart }
    // The wait is open again; a verdict is only recorded when a chunk ends it.
    rec.lastVerdict = null
    rec.chunks += 1
    state.save(rec)
    return limitReached
  }
  const newest = state.latest()
  let record = resumable(newest)
  let fresh = record === null
  if (fresh) record = newRecord({ pr, headSha: '', atMs: chunkStart, options: opts })
  const resumedAs = JSON.parse(JSON.stringify(record))
  let limitReached = openChunk(record)

  let reading = await readOnce()

  // Resolve the record this reading belongs to.
  let headChanged = false
  if (reading.headSha !== '' && reading.headSha !== record.headSha) {
    if (record.headSha === '') {
      // The placeholder learns its head; the budget it spent is still spent.
      state.remove('')
      record.headSha = reading.headSha
      if (fresh)
        headChanged = newest != null && newest.headSha !== '' && newest.headSha !== record.headSha
    } else {
      // The head moved since the last chunk: leave the old head's wait as it was, and open the new
      // head's wait with a fresh budget.
      state.save({ ...resumedAs, openChunk: null })
      const existing = resumable(state.load(reading.headSha))
      fresh = existing === null
      record =
        existing ?? newRecord({ pr, headSha: reading.headSha, atMs: reading.atMs, options: opts })
      headChanged = fresh
      limitReached = openChunk(record)
    }
  }

  const close = (verdictState, reason, extra = {}) => {
    record.openChunk = null
    record.updatedAtMs = Math.max(record.updatedAtMs, now())
    const elapsedMs = now() - record.startedAtMs
    const budgetMs = totalBudget(record)
    const last = record.readings[record.readings.length - 1] ?? null
    const trend = trendOf(record.readings)
    if (verdictState !== WAIT_STATES.CONTINUE) {
      record.lastVerdict = { state: verdictState, reason, atMs: now(), trend }
    }
    const source = read.source?.() ?? null
    record.source = source
    state.save(record)
    const verdict = {
      state: verdictState,
      reason,
      source,
      ...(source === 'gh-fallback' ? { fallbackReason: read.fallbackReason?.() ?? null } : {}),
      pr,
      headSha: record.headSha,
      headChanged,
      reads: record.reads,
      elapsedMs,
      budgetMs,
      remainingMs: Math.max(0, budgetMs - elapsedMs),
      pending: last?.pending ?? null,
      total: last?.total ?? null,
      trend,
      extended: record.extended,
      interruptedChunks: record.interruptedChunks,
      absent: record.lastDetail?.absent ?? [],
      notTriggered: record.lastDetail?.notTriggered ?? [],
      pendingRuns: record.lastDetail?.pendingRuns ?? [],
      notRequested: record.lastDetail?.notRequested ?? [],
      absentGateRemedy: record.lastDetail?.absentGateRemedy ?? 'none',
      stranded: record.lastDetail?.stranded ?? [],
      strandedRemedy: record.lastDetail?.strandedRemedy ?? [],
      verifyHold: record.lastDetail?.verifyHold ?? null,
      excludedContexts: record.lastDetail?.excludedContexts ?? [],
      stateFile: state.pathFor?.(record.headSha) ?? null,
      ...extra,
    }
    messages.push(
      `ci-wait: PR #${pr} head ${shortSha(record.headSha)}: ${verdictState} (${reason}) after ` +
        `${record.reads} read(s), ${seconds(elapsedMs)} of ${seconds(budgetMs)}; ` +
        `${verdict.pending ?? '?'} pending of ${verdict.total ?? '?'}; trend ${trend}`,
    )
    if (reason === WAIT_REASONS.STRANDED_CHECK) {
      messages.push(
        `ci-wait: stranded checks: ${verdict.stranded.map((entry, i) => `${entry.name}: ${verdict.strandedRemedy[i]}`).join('; ')}`,
      )
    }
    return { verdict, record, messages }
  }

  if (limitReached) {
    recordReading(record, reading)
    return close(WAIT_STATES.UNKNOWN, WAIT_REASONS.REPEATED_INTERRUPTION)
  }
  recordReading(record, reading)
  state.save(record)

  // R7: the first read of a new wait says what it is waiting on.
  if (fresh) {
    const v = reading.verdict
    let signal = `no red signal; polling ${v.pending} pending of ${v.total} checks`
    if (v.state === CHECK_STATES.FAILING)
      signal = `red signal: ${v.failed} failing of ${v.total} checks`
    else if (v.reason === CHECK_REASONS.UNREADABLE)
      signal = 'no red signal; checks unreadable so far'
    const hold = reading.verifyHold
    const excluding =
      hold === null || hold === undefined
        ? ''
        : `; excluding ${VERIFY_HOLD_LABELS[hold.kind]}${hold.token ? ` ${hold.token}` : ''}`
    messages.push(
      `ci-wait: PR #${pr} head ${shortSha(record.headSha)}: ${signal}; budget ${seconds(record.budgetMs)}${excluding}`,
    )
  }

  let extendRefusal = null
  let extendGranted = false
  let reads = 0
  const maxReads = Math.ceil(opts.chunkMs / MIN_DELAY_MS) + 2
  for (;;) {
    reads += 1
    const strandedKey = record.readings.at(-1)?.strandedKey
    const decision = heldFor(
      record,
      (r) => r.strandedOnly === true && r.strandedKey === strandedKey,
      STRANDED_SETTLE_MS,
    )
      ? { wait: false, state: WAIT_STATES.UNKNOWN, reason: WAIT_REASONS.STRANDED_CHECK }
      : mapVerdict(reading.verdict)
    // A green read settles only once a second read of the same head confirms it; until then it is
    // one more interval of waiting like any pending read.
    const daemonPath = read.source?.() === 'daemon'
    const greenRule = { fresh: daemonPath, intervalMs: opts.intervalMs }
    const unconfirmed =
      (decision.state === WAIT_STATES.SETTLED && !confirmsGreen(record, greenRule)) ||
      (decision.reason === CHECK_REASONS.NOT_REQUESTED &&
        !heldFor(record, (r) => r.reason === CHECK_REASONS.NOT_REQUESTED, NOT_REQUESTED_SETTLE_MS))
    // The daemon's checks carry no SHA and may be cached from an older head than its status, so a
    // daemon-path wait ends only on a fresh reading: a cached terminal one is re-read fresh first.
    const cached = daemonPath && reading.fresh !== true
    if (!decision.wait && !unconfirmed && !cached) return close(decision.state, decision.reason)
    if (!decision.wait && !unconfirmed) {
      // The re-read takes no delay, so it is bounded like any read: a reader that answers a fresh
      // request with a cached reading must not spin here.
      if (remainingChunk() < READ_FLOOR_MS || reads >= maxReads) {
        return close(WAIT_STATES.CONTINUE, WAIT_REASONS.CHUNK_EXHAUSTED, { extendRefusal })
      }
      reading = await readOnce({ fresh: true })
      if (reading.headSha !== '' && record.headSha !== '' && reading.headSha !== record.headSha) {
        headChanged = true
        return close(WAIT_STATES.CONTINUE, WAIT_REASONS.HEAD_CHANGED, {
          extendRefusal,
          newHeadSha: reading.headSha,
        })
      }
      recordReading(record, reading)
      state.save(record)
      continue
    }

    let elapsed = now() - record.startedAtMs
    if (elapsed >= totalBudget(record)) {
      // R6: one extension per state file, only on a converging trend, only once exhausted.
      const trend = trendOf(record.readings)
      // `--extend` applies at the moment the budget runs out; while budget remains it does nothing.
      if (opts.extend && !extendGranted && !record.extended && trend === TRENDS.CONVERGING) {
        record.extended = true
        record.extendMs = opts.extendMs
        extendGranted = true
        messages.push(
          `ci-wait: PR #${pr}: budget extended by ${seconds(opts.extendMs)} (trend converging)`,
        )
        elapsed = now() - record.startedAtMs
      } else {
        if (opts.extend && !extendGranted) {
          extendRefusal = record.extended ? 'already-extended' : `trend-${trend}`
          messages.push(`ci-wait: PR #${pr}: --extend refused (${extendRefusal})`)
        }
        return close(WAIT_STATES.TIMEOUT, WAIT_REASONS.BUDGET_EXHAUSTED, { extendRefusal })
      }
    }

    const budgetLeft = totalBudget(record) - elapsed
    if (reads >= maxReads) {
      return close(WAIT_STATES.CONTINUE, WAIT_REASONS.CHUNK_EXHAUSTED, { extendRefusal })
    }
    // R2: the helper takes the delay itself and proves it elapsed.
    const confirming = decision.state === WAIT_STATES.SETTLED
    const anchor = confirming ? freshGreenAnchor(record.readings) : null
    // With no fresh green yet the confirming gap is short: the next read must take the first one.
    const confirmAtMs = confirming ? (anchor ? anchor.atMs + opts.intervalMs : 0) : undefined
    const gap = daemonWait && daemonPath ? await daemonGap(budgetLeft, confirmAtMs) : 'timer'
    if (gap === 'timer') {
      const delay = Math.max(MIN_DELAY_MS, Math.min(opts.intervalMs, budgetLeft))
      if (remainingChunk() - delay < READ_FLOOR_MS) {
        return close(WAIT_STATES.CONTINUE, WAIT_REASONS.CHUNK_EXHAUSTED, { extendRefusal })
      }
      if (!(await verifiedSleep(delay))) {
        return close(WAIT_STATES.UNKNOWN, WAIT_REASONS.DELAY_INERT, { extendRefusal })
      }
    } else if (gap === 'continue') {
      return close(WAIT_STATES.CONTINUE, WAIT_REASONS.CHUNK_EXHAUSTED, { extendRefusal })
    } else if (gap === 'inert') {
      return close(WAIT_STATES.UNKNOWN, WAIT_REASONS.DELAY_INERT, { extendRefusal })
    }

    // R10: no read may outlive the chunk.
    if (remainingChunk() < READ_FLOOR_MS) {
      return close(WAIT_STATES.CONTINUE, WAIT_REASONS.CHUNK_EXHAUSTED, { extendRefusal })
    }
    // The read that would confirm a green is fresh: on the daemon path only fresh greens confirm.
    reading = await readOnce({ fresh: confirming })
    if (reading.headSha !== '' && record.headSha !== '' && reading.headSha !== record.headSha) {
      // A new head is a new wait with a fresh budget; the next `run` opens it.
      headChanged = true
      return close(WAIT_STATES.CONTINUE, WAIT_REASONS.HEAD_CHANGED, {
        extendRefusal,
        newHeadSha: reading.headSha,
      })
    }
    if (reading.headSha !== '' && record.headSha === '') {
      // The placeholder learns its head: move it under the real key.
      state.remove('')
      record.headSha = reading.headSha
    }
    recordReading(record, reading)
    state.save(record)
  }
}

// statusOf is the latch: the last recorded reading, never a fresh probe.
export function statusOf({ record, now }) {
  if (record == null) {
    return { fresh: false, found: false, reason: 'no-state' }
  }
  const last = record.readings[record.readings.length - 1] ?? null
  return {
    fresh: false,
    found: true,
    pr: record.pr,
    headSha: record.headSha,
    ageMs: last ? now() - last.atMs : null,
    lastReading: last,
    lastVerdict: record.lastVerdict,
    reads: record.reads,
    elapsedMs: (last?.atMs ?? record.startedAtMs) - record.startedAtMs,
    budgetMs: totalBudget(record),
    extended: record.extended,
    interruptedChunks: record.interruptedChunks,
    chunkOpen: record.openChunk != null,
    source: record.source ?? null,
  }
}

// ---------------------------------------------------------------------------
// File-backed state store and the raw-gh reader.

export function fileStore({ dir, pr }) {
  const pathFor = (headSha) => path.join(dir, `pr${pr}-${headSha === '' ? 'nohead' : headSha}.json`)
  const readJson = (file) => {
    try {
      return JSON.parse(readFileSync(file, 'utf8'))
    } catch {
      // Missing, torn, or foreign: not a record this wait can resume; start over rather than guess.
      return null
    }
  }
  return {
    pathFor,
    load: (headSha) => readJson(pathFor(headSha)),
    latest: () => {
      let entries
      try {
        entries = readdirSync(dir)
      } catch {
        return null
      }
      let best = null
      for (const name of entries) {
        if (!name.startsWith(`pr${pr}-`) || !name.endsWith('.json')) continue
        const rec = readJson(path.join(dir, name))
        if (rec && (best === null || (rec.updatedAtMs ?? 0) > (best.updatedAtMs ?? 0))) best = rec
      }
      return best
    },
    save: (record) => {
      mkdirSync(dir, { recursive: true })
      const file = pathFor(record.headSha)
      const tmp = `${file}.${process.pid}.tmp`
      writeFileSync(tmp, `${JSON.stringify(record)}\n`)
      renameSync(tmp, file)
    },
    remove: (headSha) => rmSync(pathFor(headSha), { force: true }),
  }
}

// ghReader is the raw-gh reader, labelled as ci-wait's fallback (see ./pr-state.mjs).
export function ghReader({ spawn } = {}) {
  return ghCheckReader({ label: FALLBACK_LABELS.CI_WAIT, deps: spawn ? { spawn } : undefined })
}

export function realSleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

// ---------------------------------------------------------------------------
// CLI.

const CLI_FLAGS = Object.freeze({
  run: Object.freeze([
    'pr',
    'budget-ms',
    'interval-ms',
    'chunk-ms',
    'read-timeout-ms',
    'daemon-wait-ms',
    'prior',
    'extend',
    'extend-ms',
    'state-dir',
  ]),
  status: Object.freeze(['pr', 'head-sha', 'state-dir']),
})
const CLI_BOOLEAN_FLAGS = Object.freeze({
  run: Object.freeze(['extend']),
  status: Object.freeze([]),
})

function parseFlags(argv) {
  const flags = {}
  for (let i = 0; i < argv.length; i += 1) {
    const key = argv[i]
    if (typeof key !== 'string' || !key.startsWith('--')) {
      throw new UsageError(`ci-wait: unexpected argument ${JSON.stringify(key)}`)
    }
    const next = argv[i + 1]
    if (typeof next !== 'string' || next.startsWith('--')) {
      flags[key.slice(2)] = true
    } else {
      flags[key.slice(2)] = next
      i += 1
    }
  }
  return flags
}

function assertKnownFlags(verb, flags) {
  const accepted = CLI_FLAGS[verb]
  const valueless = CLI_BOOLEAN_FLAGS[verb]
  const shape = accepted.map((name) => `--${name}`).join(', ')
  for (const [name, value] of Object.entries(flags)) {
    if (!accepted.includes(name)) {
      throw new UsageError(`ci-wait: ${verb}(${shape}) — unrecognised flag --${name}`)
    }
    if (value === true && !valueless.includes(name)) {
      throw new UsageError(`ci-wait: ${verb}(${shape}) — --${name} needs a value`)
    }
  }
}

function intFlag(flags, name, fallback, { min = 1 } = {}) {
  const raw = flags[name]
  if (raw === undefined) return fallback
  const value = Number(raw)
  if (!Number.isInteger(value) || value < min) {
    throw new UsageError(
      `ci-wait: --${name} must be an integer >= ${min}, got ${JSON.stringify(raw)}`,
    )
  }
  return value
}

// parseRunOptions validates everything before any read, so a refused configuration prints no
// verdict at all (exit 2) rather than a verdict a caller might route.
export function parseRunOptions(argv) {
  const flags = parseFlags(argv)
  assertKnownFlags('run', flags)
  if (flags.pr === undefined) throw new UsageError('ci-wait: run needs --pr <number>')
  const options = {
    pr: intFlag(flags, 'pr', undefined),
    budgetMs: intFlag(flags, 'budget-ms', DEFAULTS.budgetMs),
    intervalMs: intFlag(flags, 'interval-ms', DEFAULTS.intervalMs, { min: MIN_DELAY_MS }),
    chunkMs: intFlag(flags, 'chunk-ms', DEFAULTS.chunkMs),
    readTimeoutMs: intFlag(flags, 'read-timeout-ms', DEFAULTS.readTimeoutMs),
    // Clamped to `boss pr wait`'s own ceiling, which would clamp it anyway.
    daemonWaitMs: Math.min(
      PR_WAIT_CAP_MS,
      intFlag(flags, 'daemon-wait-ms', DEFAULTS.daemonWaitMs, { min: MIN_DELAY_MS }),
    ),
    extend: flags.extend === true,
    extendMs: intFlag(flags, 'extend-ms', DEFAULTS.extendMs),
    stateDir: typeof flags['state-dir'] === 'string' ? flags['state-dir'] : defaultStateDir(),
    prior: typeof flags.prior === 'string' ? flags.prior : null,
  }
  if (options.chunkMs + options.readTimeoutMs > CEILING_MS) {
    throw new UsageError(
      `ci-wait: --chunk-ms (${options.chunkMs}) + --read-timeout-ms (${options.readTimeoutMs}) exceeds ` +
        `the ${CEILING_MS}ms single-call ceiling; refusing to start a call that could outlive the tool`,
    )
  }
  return options
}

// gitCommonDir finds the git directory every worktree of the checkout containing `cwd` shares — the
// repository `gh pr` resolves the PR number against — or null outside any checkout.
function gitCommonDir(cwd) {
  for (let dir = path.resolve(cwd); ; dir = path.dirname(dir)) {
    const dotGit = path.join(dir, '.git')
    try {
      if (statSync(dotGit).isDirectory()) return dotGit
      const gitdir = /^gitdir:\s*(.+)$/m.exec(readFileSync(dotGit, 'utf8'))?.[1]?.trim()
      if (gitdir) {
        const own = path.resolve(dir, gitdir)
        try {
          return path.resolve(own, readFileSync(path.join(own, 'commondir'), 'utf8').trim())
        } catch {
          return own
        }
      }
    } catch {
      // No .git here; keep climbing.
    }
    if (path.dirname(dir) === dir) return null
  }
}

// defaultStateDir namespaces the state files by repository, so a same-numbered PR of another
// checkout on this host is never adopted as this wait.
export function defaultStateDir(cwd = process.cwd()) {
  const identity = gitCommonDir(cwd) ?? path.resolve(cwd)
  const key = createHash('sha256').update(identity).digest('hex').slice(0, 16)
  return path.join(os.tmpdir(), 'boss-ci-wait', key)
}

function readPrior(file) {
  if (file === null) return null
  try {
    const raw = readFileSync(file, 'utf8')
    return raw.trim() === '' ? null : JSON.parse(raw)
  } catch (err) {
    if (err?.code === 'ENOENT') {
      process.stderr.write(`ci-wait: --prior payload not found: ${file}\n`)
      return null
    }
    throw new UsageError(`ci-wait: --prior cannot be read: ${err?.code ?? err?.message ?? err}`)
  }
}

// The repo's `ci.required` block, read once per `run`. A config that does not load requires
// nothing (the wait behaves as before) and says so; the skill-config validator rejects a malformed
// block before it lands.
function readCiRequired(load) {
  try {
    const block = load()?.ci?.required ?? null
    requiredChecksFor(block)
    return { block, messages: [] }
  } catch (error) {
    const why = error instanceof Error ? error.message : String(error)
    return { block: null, messages: [`ci-wait: ci.required not applied: ${why}`] }
  }
}

export async function main(argv, deps = {}) {
  const [cmd, ...rest] = argv
  const now = deps.now ?? Date.now
  if (cmd === 'run') {
    const options = parseRunOptions(rest)
    const priorContexts = readPrior(options.prior)
    const config = readCiRequired(deps.loadConfig ?? loadSkillConfig)
    // Daemon-first by default; an injected reader keeps the timer unless a wait is injected too.
    const io = { now, ...deps.io }
    const wait = deps.read
      ? (deps.wait ?? null)
      : (deps.wait ?? ((req) => waitPRState({ ...req, deps: io })))
    const { verdict, messages } = await runChunk({
      state: deps.store ?? fileStore({ dir: options.stateDir, pr: options.pr }),
      read: deps.read ?? daemonFirstReader({ label: FALLBACK_LABELS.CI_WAIT, deps: io }),
      wait,
      sleep: deps.sleep ?? realSleep,
      now,
      options: { ...options, priorContexts, ciRequired: config.block },
    })
    return { verdict, messages: [...config.messages, ...messages] }
  }
  if (cmd === 'status') {
    const flags = parseFlags(rest)
    assertKnownFlags('status', flags)
    if (flags.pr === undefined || typeof flags['head-sha'] !== 'string') {
      throw new UsageError('ci-wait: status needs --pr <number> and --head-sha <sha>')
    }
    const pr = intFlag(flags, 'pr', undefined)
    const dir = typeof flags['state-dir'] === 'string' ? flags['state-dir'] : defaultStateDir()
    const store = deps.store ?? fileStore({ dir, pr })
    return { verdict: statusOf({ record: store.load(flags['head-sha']), now }), messages: [] }
  }
  throw new UsageError(`ci-wait: unknown command ${cmd ?? '(none)'} (expected "run" or "status")`)
}

if (isMainModule(import.meta.url)) {
  try {
    const { verdict, messages } = await main(process.argv.slice(2))
    for (const line of messages) process.stderr.write(`${line}\n`)
    process.stdout.write(`${JSON.stringify(verdict)}\n`)
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`)
    process.exitCode = error instanceof UsageError ? 2 : 1
  }
}
