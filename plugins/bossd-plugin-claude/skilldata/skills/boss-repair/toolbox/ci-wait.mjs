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
//   - the delay between reads is an in-process timer whose elapsed wall time is verified, so an
//     inert delay ends the wait as `unknown`/`delay-inert` instead of spinning the budget away;
//   - a chunk leaves an open-chunk marker until it closes, so a killed chunk is detected and
//     counted by the next `run`, and three in a row end the wait rather than retrying forever.
//
// The verdict itself is `classifyChecks` from ./pr-check-state.mjs, called in-process; this module
// restates none of its rules, it only maps them onto the CI_WAIT_STATE vocabulary:
//
//   settled   green                                     terminal
//   failed    failing                                   terminal
//   timeout   budget exhausted while still waiting      terminal (carries `trend`)
//   unknown   anything the classifier will not call     terminal
//             green or red, plus `delay-inert` and
//             `repeated-interruption`
//   continue  chunk used up, budget left                NOT terminal — issue `run` again in a new
//                                                       call; never assign it to CI_WAIT_STATE
//
// Project-agnostic: only `gh` and Node builtins.

import { spawnSync } from 'node:child_process'
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
import { CHECK_REASONS, CHECK_STATES, classifyChecks } from './pr-check-state.mjs'

export const CEILING_MS = 570000
export const DEFAULTS = Object.freeze({
  budgetMs: 60 * 60 * 1000,
  intervalMs: 30 * 1000,
  chunkMs: 480 * 1000,
  readTimeoutMs: 60 * 1000,
  extendMs: 30 * 60 * 1000,
})
export const TREND_WINDOW_MS = 10 * 60 * 1000
export const INTERRUPTION_LIMIT = 3
// Delays shorter than this cannot be told apart from an inert timer, so none is ever requested.
export const MIN_DELAY_MS = 1000
// A read is only started when at least this much chunk time remains for it.
const READ_FLOOR_MS = 1000
const READINGS_KEPT = 200

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

// takeReading performs one read — the rollup, then the named-context payload only when the rollup
// carries a null-shaped node — and classifies it. Each `gh` call gets `min(readTimeoutMs, time left
// before deadline)`, so the pair together can never outlive the chunk. It never throws: a failed
// read is `unreadable`.
async function takeReading({ read, pr, readTimeoutMs, deadline, priorContexts, now }) {
  const timeoutFor = () => Math.min(readTimeoutMs, deadline - now())
  const view = await read({ what: 'view', pr, timeoutMs: timeoutFor() })
  if (!view?.ok) {
    const verdict = classifyChecks({ readError: view?.error ?? 'read failed' })
    return { atMs: now(), headSha: '', verdict }
  }
  const headSha = typeof view.headSha === 'string' ? view.headSha : ''
  let verdict = classifyChecks({
    headSHA: headSha,
    observedSHA: headSha,
    rollup: view.payload ?? null,
    priorContexts,
  })
  if (verdict.reason === CHECK_REASONS.UNCLASSIFIED && timeoutFor() >= READ_FLOOR_MS) {
    const checks = await read({ what: 'checks', pr, timeoutMs: timeoutFor() })
    if (checks?.ok) {
      verdict = classifyChecks({
        headSHA: headSha,
        observedSHA: headSha,
        rollup: view.payload ?? null,
        buckets: checks.payload ?? null,
        priorContexts,
      })
    }
  }
  return { atMs: now(), headSha, verdict }
}

function recordReading(record, reading) {
  record.reads += 1
  record.updatedAtMs = reading.atMs
  record.readings.push({
    atMs: reading.atMs,
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
//   read    async ({what: 'view'|'checks', pr, timeoutMs}) → {ok, headSha?, payload?, error?}
//   sleep   async (ms) → void; the elapsed wall time is verified against `now`
//   now     () → epoch ms
//   options {pr, budgetMs, intervalMs, chunkMs, readTimeoutMs, extend, extendMs, priorContexts}
// It returns {verdict, record, messages}; `messages` are the stderr lines, in order.
export async function runChunk({ state, read, sleep, now, options }) {
  const opts = { ...DEFAULTS, ...options }
  const { pr } = opts
  const messages = []
  const chunkStart = now()
  const deadline = chunkStart + opts.chunkMs
  const remainingChunk = () => deadline - now()

  // R8: every run takes a read before any delay.
  const readOnce = () =>
    takeReading({
      read,
      pr,
      readTimeoutMs: opts.readTimeoutMs,
      deadline,
      priorContexts: opts.priorContexts ?? null,
      now,
    })

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
    state.save(record)
    const verdict = {
      state: verdictState,
      reason,
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
      stateFile: state.pathFor?.(record.headSha) ?? null,
      ...extra,
    }
    messages.push(
      `ci-wait: PR #${pr} head ${shortSha(record.headSha)}: ${verdictState} (${reason}) after ` +
        `${record.reads} read(s), ${seconds(elapsedMs)} of ${seconds(budgetMs)}; ` +
        `${verdict.pending ?? '?'} pending of ${verdict.total ?? '?'}; trend ${trend}`,
    )
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
    messages.push(
      `ci-wait: PR #${pr} head ${shortSha(record.headSha)}: ${signal}; budget ${seconds(record.budgetMs)}`,
    )
  }

  let extendRefusal = null
  let extendGranted = false
  let reads = 0
  const maxReads = Math.ceil(opts.chunkMs / MIN_DELAY_MS) + 2
  for (;;) {
    reads += 1
    const decision = mapVerdict(reading.verdict)
    if (!decision.wait) return close(decision.state, decision.reason)

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
    const delay = Math.max(MIN_DELAY_MS, Math.min(opts.intervalMs, budgetLeft))
    if (remainingChunk() - delay < READ_FLOOR_MS || reads >= maxReads) {
      return close(WAIT_STATES.CONTINUE, WAIT_REASONS.CHUNK_EXHAUSTED, { extendRefusal })
    }

    // R2: the helper takes the delay itself and proves it elapsed.
    const before = now()
    await sleep(delay)
    const slept = now() - before
    if (slept < delay - Math.max(50, delay * 0.05)) {
      messages.push(
        `ci-wait: PR #${pr}: a ${delay}ms delay returned after ${slept}ms — the timer is inert`,
      )
      return close(WAIT_STATES.UNKNOWN, WAIT_REASONS.DELAY_INERT, { extendRefusal })
    }

    // R10: no read may outlive the chunk.
    if (remainingChunk() < READ_FLOOR_MS) {
      return close(WAIT_STATES.CONTINUE, WAIT_REASONS.CHUNK_EXHAUSTED, { extendRefusal })
    }
    reading = await readOnce()
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
  }
}

// ---------------------------------------------------------------------------
// File-backed state store and the real `gh` reader.

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

export function ghReader({ spawn = spawnSync } = {}) {
  return async ({ what, pr, timeoutMs }) => {
    const args =
      what === 'view'
        ? ['pr', 'view', String(pr), '--json', 'headRefOid,statusCheckRollup']
        : ['pr', 'checks', String(pr), '--json', 'name,state,bucket']
    const res = spawn('gh', args, {
      encoding: 'utf8',
      timeout: Math.max(1, Math.floor(timeoutMs)),
      killSignal: 'SIGKILL',
      maxBuffer: 64 * 1024 * 1024,
    })
    // `gh pr checks` exits 8 while checks are pending and still prints the payload.
    if (res.error || (res.status !== 0 && !(what === 'checks' && res.status === 8))) {
      return { ok: false, error: res.error?.code ?? `gh exited ${res.status}` }
    }
    try {
      const payload = JSON.parse(res.stdout)
      return what === 'view'
        ? { ok: true, headSha: payload?.headRefOid ?? '', payload }
        : { ok: true, payload }
    } catch {
      return { ok: false, error: 'unparseable gh output' }
    }
  }
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

export async function main(argv, deps = {}) {
  const [cmd, ...rest] = argv
  const now = deps.now ?? Date.now
  if (cmd === 'run') {
    const options = parseRunOptions(rest)
    const priorContexts = readPrior(options.prior)
    const { verdict, messages } = await runChunk({
      state: deps.store ?? fileStore({ dir: options.stateDir, pr: options.pr }),
      read: deps.read ?? ghReader(),
      sleep: deps.sleep ?? realSleep,
      now,
      options: { ...options, priorContexts },
    })
    return { verdict, messages }
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
