import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  CEILING_MS,
  DEFAULTS,
  TRENDS,
  TREND_WINDOW_MS,
  UsageError,
  WAIT_REASONS,
  WAIT_STATES,
  defaultStateDir,
  fileStore,
  ghReader,
  main,
  mapVerdict,
  parseRunOptions,
  runChunk,
  statusOf,
  trendOf,
} from './ci-wait.mjs'
import { classifyChecks } from './pr-check-state.mjs'

const HELPER = fileURLToPath(new URL('./ci-wait.mjs', import.meta.url))
const HEAD_A = 'a'.repeat(40)
const HEAD_B = 'b'.repeat(40)
const T0 = 1_800_000_000_000

// ---------------------------------------------------------------------------
// Fixtures: a fake clock, a scripted reader, and an in-memory store.

function view(headSha, nodes) {
  return { ok: true, headSha, payload: { headRefOid: headSha, statusCheckRollup: nodes } }
}
const passing = (name) => ({ name, status: 'COMPLETED', conclusion: 'SUCCESS' })
const running = (name) => ({ name, status: 'IN_PROGRESS', conclusion: '' })
const failing = (name) => ({ name, status: 'COMPLETED', conclusion: 'FAILURE' })
const skipped = (name) => ({ name, status: 'COMPLETED', conclusion: 'SKIPPED' })
const nullShaped = (name) => ({ name, status: 'COMPLETED', conclusion: null })

// pendingView builds a rollup with `pending` running checks out of `total`.
function pendingView(headSha, pending, total = 4) {
  const nodes = []
  for (let i = 0; i < total; i += 1) nodes.push(i < pending ? running(`c${i}`) : passing(`c${i}`))
  return view(headSha, nodes)
}

function memStore() {
  const records = new Map()
  const clone = (v) => (v == null ? null : JSON.parse(JSON.stringify(v)))
  return {
    records,
    pathFor: (sha) => `mem:${sha || 'nohead'}`,
    load: (sha) => clone(records.get(sha) ?? null),
    latest: () => {
      let best = null
      for (const r of records.values()) if (!best || r.updatedAtMs > best.updatedAtMs) best = r
      return clone(best)
    },
    save: (r) => records.set(r.headSha, clone(r)),
    remove: (sha) => records.delete(sha),
  }
}

// harness: `script` is a function (request, index) → response; each response may carry `costMs`,
// the fake time the read takes (default 0). `sleep` advances the clock unless `inert`.
//
// The head workflow-runs read is answered by `runs` (request, index) → response instead, and is
// recorded in `runCalls` rather than `calls`, so a script indexed by its own reads keeps its
// meaning. The default is a successful read with no runs — every head run completed.
const completedRuns = () => ({ ok: true, payload: [] })
function harness({ script, runs = completedRuns, start = T0, inert = false }) {
  let clock = start
  const calls = []
  const runCalls = []
  const sleeps = []
  return {
    now: () => clock,
    advance: (ms) => {
      clock += ms
    },
    calls,
    runCalls,
    sleeps,
    read: async (req) => {
      if (req.what === 'runs') {
        runCalls.push({ ...req, atMs: clock })
        return runs(req, runCalls.length - 1)
      }
      calls.push({ ...req, atMs: clock })
      const res = script(req, calls.length - 1)
      clock += typeof res?.costMs === 'function' ? res.costMs(req) : (res?.costMs ?? 0)
      return res
    },
    sleep: async (ms) => {
      sleeps.push(ms)
      if (!inert) clock += ms
    },
  }
}

function run(h, store, options = {}) {
  return runChunk({
    state: store,
    read: h.read,
    sleep: h.sleep,
    now: h.now,
    options: { pr: 7, ...options },
  })
}

// ---------------------------------------------------------------------------
// R1: bounded call.

test('R1: a chunk whose reads all hang returns within chunkMs + readTimeoutMs on a fake clock', async () => {
  const chunkMs = 120_000
  const readTimeoutMs = 60_000
  // Every read hangs for its whole timeout and then fails: the worst case for the call bound.
  const h = harness({
    script: () => ({ ok: false, error: 'ETIMEDOUT', costMs: (req) => req.timeoutMs }),
  })
  const { verdict } = await run(h, memStore(), { chunkMs, readTimeoutMs, intervalMs: 30_000 })
  assert.equal(verdict.state, WAIT_STATES.CONTINUE)
  assert.equal(verdict.reason, WAIT_REASONS.CHUNK_EXHAUSTED)
  assert.ok(h.now() - T0 <= chunkMs + readTimeoutMs, `took ${h.now() - T0}ms`)
  assert.ok(h.now() - T0 <= chunkMs, 'reads are capped by the chunk deadline, not appended to it')
})

test('R1: per-read timeout is capped by the remaining chunk time', async () => {
  const chunkMs = 100_000
  const h = harness({ script: () => pendingView(HEAD_A, 2) })
  await run(h, memStore(), { chunkMs, readTimeoutMs: 60_000, intervalMs: 30_000 })
  assert.ok(h.calls.length >= 3)
  for (const call of h.calls) {
    const remaining = T0 + chunkMs - call.atMs
    assert.ok(call.timeoutMs <= 60_000)
    assert.ok(call.timeoutMs <= remaining, `timeout ${call.timeoutMs} > remaining ${remaining}`)
  }
  assert.ok(
    h.calls.some((call) => call.timeoutMs < 60_000),
    'a late read in the chunk must get less than the full read timeout',
  )
})

test('R1: the CLI refuses chunk + read timeout over the ceiling with exit 2 and no verdict', () => {
  const dir = mkdtempSync(path.join(os.tmpdir(), 'ci-wait-cli-'))
  try {
    const res = spawnSync(
      process.execPath,
      [
        HELPER,
        'run',
        '--pr',
        '1',
        '--chunk-ms',
        '540000',
        '--read-timeout-ms',
        '60000',
        '--state-dir',
        dir,
      ],
      { encoding: 'utf8', env: { ...process.env, PATH: '' } },
    )
    assert.equal(res.status, 2)
    assert.equal(res.stdout, '')
    assert.match(res.stderr, /exceeds the 570000ms single-call ceiling/)
    assert.deepEqual(readdirSync(dir), [], 'a refused run must write no state')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
  assert.throws(
    () =>
      parseRunOptions(['--pr', '1', '--chunk-ms', String(CEILING_MS), '--read-timeout-ms', '1']),
    UsageError,
  )
  // Exactly at the ceiling is allowed; the defaults sit under it.
  assert.equal(
    parseRunOptions([
      '--pr',
      '1',
      '--chunk-ms',
      String(CEILING_MS - 1000),
      '--read-timeout-ms',
      '1000',
    ]).chunkMs,
    CEILING_MS - 1000,
  )
  assert.ok(DEFAULTS.chunkMs + DEFAULTS.readTimeoutMs <= CEILING_MS)
})

test('CLI: unknown or value-less flags are refused by name with exit 2', () => {
  for (const argv of [
    ['run', '--pr', '1', '--budget', '5'],
    ['run', '--pr', '--chunk-ms', '1000'],
    ['run'],
    ['status', '--pr', '1'],
    ['bogus'],
  ]) {
    const res = spawnSync(process.execPath, [HELPER, ...argv], { encoding: 'utf8' })
    assert.equal(res.status, 2, `${argv.join(' ')} → ${res.stderr}`)
    assert.equal(res.stdout, '')
  }
  assert.throws(
    () => parseRunOptions(['--pr', '1', '--interval-ms', '10']),
    /--interval-ms must be/,
  )
})

// ---------------------------------------------------------------------------
// R2: the delay is owned and verified.

test('R2: an inert delay ends the wait as delay-inert after the first delay, not a spin', async () => {
  const h = harness({ script: () => pendingView(HEAD_A, 2), inert: true })
  const { verdict } = await run(h, memStore())
  assert.equal(verdict.state, WAIT_STATES.UNKNOWN)
  assert.equal(verdict.reason, WAIT_REASONS.DELAY_INERT)
  assert.equal(h.sleeps.length, 1)
  assert.equal(h.calls.length, 1)
  assert.equal(verdict.reads, 1)
})

test('R2: the delay between reads is the interval, and no delay is shorter than a second', async () => {
  const h = harness({ script: () => pendingView(HEAD_A, 2) })
  await run(h, memStore(), { intervalMs: 30_000, chunkMs: 200_000 })
  assert.ok(h.sleeps.length > 1)
  for (const ms of h.sleeps) assert.equal(ms, 30_000)
  // A budget that ends mid-interval clamps the delay to what is left, never below the floor.
  const h2 = harness({ script: () => pendingView(HEAD_A, 2) })
  await run(h2, memStore(), { intervalMs: 30_000, budgetMs: 45_000 })
  assert.deepEqual(h2.sleeps, [30_000, 15_000])
})

// ---------------------------------------------------------------------------
// R3/R4: the wall-clock budget persists across calls.

test('R3: two consecutive runChunk calls report cumulative reads and elapsedMs', async () => {
  const store = memStore()
  const h = harness({ script: () => pendingView(HEAD_A, 2) })
  const first = await run(h, store, { chunkMs: 100_000, intervalMs: 30_000 })
  assert.equal(first.verdict.state, WAIT_STATES.CONTINUE)
  const firstReads = first.verdict.reads
  h.advance(5_000) // the agent's own turn between tool calls is spent budget too
  const second = await run(h, store, { chunkMs: 100_000, intervalMs: 30_000 })
  assert.equal(second.verdict.state, WAIT_STATES.CONTINUE)
  assert.equal(second.verdict.headChanged, false)
  assert.equal(second.verdict.reads, firstReads + h.calls.length - firstReads)
  assert.ok(second.verdict.reads > firstReads)
  assert.equal(second.verdict.elapsedMs, h.now() - T0)
  assert.ok(second.verdict.elapsedMs > first.verdict.elapsedMs + 5_000)
  assert.equal(store.records.size, 1, 'one state file per PR and head')
})

test('R3: a new head starts a fresh wait reported as headChanged', async () => {
  const store = memStore()
  let head = HEAD_A
  const h = harness({ script: () => pendingView(head, 2) })
  await run(h, store, { chunkMs: 100_000 })
  head = HEAD_B
  const { verdict } = await run(h, store, { chunkMs: 100_000 })
  assert.equal(verdict.headSha, HEAD_B)
  assert.equal(verdict.headChanged, true)
  assert.ok(verdict.reads < h.calls.length, 'the new head does not inherit the old head’s reads')
  assert.equal(store.records.size, 2)
})

test('R3: a head change mid-chunk ends the chunk as continue/head-changed', async () => {
  const h = harness({ script: (_req, i) => pendingView(i < 2 ? HEAD_A : HEAD_B, 2) })
  const { verdict } = await run(h, memStore())
  assert.equal(verdict.state, WAIT_STATES.CONTINUE)
  assert.equal(verdict.reason, WAIT_REASONS.HEAD_CHANGED)
  assert.equal(verdict.headChanged, true)
  assert.equal(verdict.newHeadSha, HEAD_B)
})

test('R3: a wait that starts unreadable keeps its spent budget once a read names the head', async () => {
  const store = memStore()
  let readable = false
  const h = harness({
    script: () => (readable ? pendingView(HEAD_A, 2) : { ok: false, error: 'quota' }),
  })
  const first = await run(h, store, { chunkMs: 100_000 })
  assert.equal(first.verdict.headSha, '')
  readable = true
  const second = await run(h, store, { chunkMs: 100_000 })
  assert.equal(second.verdict.headSha, HEAD_A)
  assert.equal(second.verdict.elapsedMs, h.now() - T0, 'the budget started at the first read')
  assert.equal(second.verdict.headChanged, false)
  assert.deepEqual([...store.records.keys()], [HEAD_A], 'the placeholder is moved, not duplicated')
})

test('R4: the budget is wall-clock and exhausts as timeout', async () => {
  const store = memStore()
  const h = harness({ script: () => pendingView(HEAD_A, 2) })
  let verdict
  for (let i = 0; i < 20; i += 1) {
    ;({ verdict } = await run(h, store, { budgetMs: 600_000 }))
    if (verdict.state !== WAIT_STATES.CONTINUE) break
  }
  assert.equal(verdict.state, WAIT_STATES.TIMEOUT)
  assert.equal(verdict.reason, WAIT_REASONS.BUDGET_EXHAUSTED)
  assert.equal(verdict.remainingMs, 0)
  assert.ok(verdict.elapsedMs >= 600_000)
  assert.equal(DEFAULTS.budgetMs, 3_600_000)
})

// ---------------------------------------------------------------------------
// R5: interrupted chunks.

function withOpenChunk(store, extra = {}) {
  const h = harness({ script: () => pendingView(HEAD_A, 2) })
  return run(h, store, { chunkMs: 40_000 }).then(() => {
    const rec = store.load(HEAD_A)
    rec.openChunk = { startedAtMs: T0 }
    Object.assign(rec, extra)
    store.save(rec)
  })
}

test('R5: an open-chunk marker is counted as an interrupted chunk', async () => {
  const store = memStore()
  await withOpenChunk(store)
  const h = harness({ script: () => pendingView(HEAD_A, 2), start: T0 + 60_000 })
  const { verdict, messages } = await run(h, store, { chunkMs: 40_000 })
  assert.equal(verdict.interruptedChunks, 1)
  assert.equal(verdict.state, WAIT_STATES.CONTINUE)
  assert.ok(messages.some((m) => /ended without a verdict/.test(m)))
  assert.equal(store.load(HEAD_A).openChunk, null, 'a chunk that closes clears its marker')
})

test('R5: the third consecutive interrupted chunk ends the wait as repeated-interruption', async () => {
  const store = memStore()
  await withOpenChunk(store, { consecutiveInterruptions: 2, interruptedChunks: 2 })
  const h = harness({ script: () => pendingView(HEAD_A, 2), start: T0 + 60_000 })
  const { verdict } = await run(h, store)
  assert.equal(verdict.state, WAIT_STATES.UNKNOWN)
  assert.equal(verdict.reason, WAIT_REASONS.REPEATED_INTERRUPTION)
  assert.equal(verdict.interruptedChunks, 3)
  assert.equal(h.sleeps.length, 0, 'no further waiting once the retry bound is hit')
})

test('R5: chunks killed during their first read are counted and end the wait', async () => {
  const store = memStore()
  // A kill mid-read never returns to runChunk; a read that throws leaves the store exactly as a kill
  // at that point would.
  const killed = harness({
    script: () => {
      throw new Error('killed during the first read')
    },
  })
  for (let i = 0; i < 3; i += 1) {
    await assert.rejects(run(killed, store, { chunkMs: 40_000 }), /killed during the first read/)
    killed.advance(5_000)
  }
  const h = harness({ script: () => pendingView(HEAD_A, 2), start: killed.now() })
  const { verdict } = await run(h, store, { chunkMs: 40_000 })
  assert.equal(verdict.state, WAIT_STATES.UNKNOWN)
  assert.equal(verdict.reason, WAIT_REASONS.REPEATED_INTERRUPTION)
  assert.equal(verdict.interruptedChunks, 3)
  assert.equal(verdict.headSha, HEAD_A, 'the placeholder that carried the count learned its head')
  assert.deepEqual([...store.records.keys()], [HEAD_A])
})

test('R5: a chunk that closes normally resets the consecutive count', async () => {
  const store = memStore()
  await withOpenChunk(store, { consecutiveInterruptions: 1, interruptedChunks: 1 })
  const h = harness({ script: () => pendingView(HEAD_A, 2), start: T0 + 60_000 })
  await run(h, store, { chunkMs: 40_000 }) // detects #2, then closes cleanly
  await run(h, store, { chunkMs: 40_000 }) // no marker: resets the run of interruptions
  const rec = store.load(HEAD_A)
  assert.equal(rec.consecutiveInterruptions, 0)
  assert.equal(rec.interruptedChunks, 2)
})

// ---------------------------------------------------------------------------
// R6: trend and the single extension.

test('R6: trendOf reads converging, frozen and unknown from the last ten minutes of reads', () => {
  const r = (atMs, pending) => ({ atMs, pending })
  assert.equal(trendOf([]), TRENDS.UNKNOWN)
  assert.equal(trendOf([r(0, 3)]), TRENDS.UNKNOWN)
  assert.equal(trendOf([r(0, 3), r(30_000, 2)]), TRENDS.CONVERGING)
  assert.equal(trendOf([r(0, 3), r(30_000, 3), r(60_000, 3)]), TRENDS.FROZEN)
  assert.equal(trendOf([r(0, 3), r(30_000, 4)]), TRENDS.FROZEN)
  // A fall older than the window no longer counts.
  const old = [r(0, 5), r(30_000, 3)]
  const late = [r(30_000 + TREND_WINDOW_MS + 1, 3), r(60_000 + TREND_WINDOW_MS, 3)]
  assert.equal(trendOf([...old, ...late]), TRENDS.FROZEN)
  // Unreadable reads carry no count and are not a fall.
  assert.equal(trendOf([r(0, 3), { atMs: 30_000, pending: null }, r(60_000, 3)]), TRENDS.FROZEN)
})

test('R6: a fall followed by more than ten minutes of unreadable reads is no longer converging', () => {
  const r = (atMs, pending) => ({ atMs, pending })
  const unreadable = (atMs) => ({ atMs, pending: null })
  const readings = [r(0, 5), r(30_000, 3)]
  for (let at = 60_000; at <= 30_000 + TREND_WINDOW_MS + 30_000; at += 30_000) {
    readings.push(unreadable(at))
  }
  assert.equal(trendOf(readings), TRENDS.UNKNOWN, 'no two countable reads are inside the window')
  // The same fall with the unreadable run still inside the window is converging.
  assert.equal(trendOf([r(0, 5), r(30_000, 3), unreadable(60_000)]), TRENDS.CONVERGING)
})

async function exhaust(store, pendingAt, options = {}) {
  let n = 0
  const h = harness({ script: () => pendingView(HEAD_A, pendingAt(n++), 8) })
  let verdict
  for (let i = 0; i < 20; i += 1) {
    ;({ verdict } = await run(h, store, { budgetMs: 600_000, ...options }))
    if (verdict.state !== WAIT_STATES.CONTINUE) break
  }
  return { verdict, h }
}

test('R6: a timeout carries trend converging when pending fell in the last ten minutes', async () => {
  const { verdict } = await exhaust(memStore(), (n) => Math.max(1, 8 - Math.floor(n / 4)))
  assert.equal(verdict.state, WAIT_STATES.TIMEOUT)
  assert.equal(verdict.trend, TRENDS.CONVERGING)
})

test('R6: a timeout carries trend frozen when pending never fell', async () => {
  const { verdict } = await exhaust(memStore(), () => 5)
  assert.equal(verdict.state, WAIT_STATES.TIMEOUT)
  assert.equal(verdict.trend, TRENDS.FROZEN)
})

test('R6: --extend is granted once on converging and refused on a second request', async () => {
  const store = memStore()
  const falling = (n) => Math.max(1, 8 - Math.floor(n / 4))
  const { verdict: timedOut, h } = await exhaust(store, falling)
  assert.equal(timedOut.trend, TRENDS.CONVERGING)

  // An extension longer than one chunk, so the granting call ends as `continue`.
  const granted = await run(h, store, { extend: true, extendMs: 900_000 })
  assert.equal(granted.verdict.extended, true)
  assert.equal(granted.verdict.state, WAIT_STATES.CONTINUE)
  assert.equal(granted.verdict.extendRefusal, null)
  assert.equal(granted.verdict.budgetMs, 1_500_000)
  assert.ok(granted.messages.some((m) => /budget extended/.test(m)))

  let verdict = granted.verdict
  for (let i = 0; i < 20 && verdict.state === WAIT_STATES.CONTINUE; i += 1) {
    ;({ verdict } = await run(h, store))
  }
  assert.equal(verdict.state, WAIT_STATES.TIMEOUT)
  const again = await run(h, store, { extend: true, extendMs: 900_000 })
  assert.equal(again.verdict.state, WAIT_STATES.TIMEOUT)
  assert.equal(again.verdict.extendRefusal, 'already-extended')
  assert.equal(again.verdict.budgetMs, 1_500_000, 'the second request grants nothing')
})

test('R6: --extend is refused on a frozen trend', async () => {
  const store = memStore()
  const { h } = await exhaust(store, () => 5)
  const { verdict, messages } = await run(h, store, { extend: true })
  assert.equal(verdict.state, WAIT_STATES.TIMEOUT)
  assert.equal(verdict.extended, false)
  assert.equal(verdict.extendRefusal, 'trend-frozen')
  assert.ok(messages.some((m) => /--extend refused \(trend-frozen\)/.test(m)))
})

test('R3: a timed-out or interrupted wait stays latched when run again on the same head', async () => {
  const store = memStore()
  const { verdict: timedOut, h } = await exhaust(store, () => 5)
  assert.equal(timedOut.state, WAIT_STATES.TIMEOUT)
  h.advance(60_000)
  const sleptBefore = h.sleeps.length
  const again = await run(h, store, { budgetMs: 600_000, chunkMs: 40_000 })
  assert.equal(again.verdict.state, WAIT_STATES.TIMEOUT, 'a plain re-run does not reset the budget')
  assert.equal(again.verdict.reason, WAIT_REASONS.BUDGET_EXHAUSTED)
  assert.equal(again.verdict.headSha, HEAD_A)
  assert.equal(again.verdict.extended, false, 'a plain re-run grants no extension')
  assert.equal(again.verdict.budgetMs, 600_000)
  assert.ok(
    again.verdict.elapsedMs >= timedOut.elapsedMs + 60_000,
    `elapsed ${again.verdict.elapsedMs}`,
  )
  assert.equal(again.verdict.reads, timedOut.reads + 1, 'the read count is resumed')
  assert.equal(h.sleeps.length, sleptBefore, 'the re-run does not wait again')
  assert.ok(!again.messages.some((m) => /polling/.test(m)), 'no new wait is opened')

  // Three interrupted chunks end the wait; a later run on the same head is still at that bound.
  const cut = memStore()
  await withOpenChunk(cut, { consecutiveInterruptions: 2, interruptedChunks: 2 })
  const h2 = harness({ script: () => pendingView(HEAD_A, 2), start: T0 + 60_000 })
  assert.equal((await run(h2, cut)).verdict.reason, WAIT_REASONS.REPEATED_INTERRUPTION)
  h2.advance(60_000)
  const retried = await run(h2, cut, { chunkMs: 40_000 })
  assert.equal(retried.verdict.state, WAIT_STATES.UNKNOWN)
  assert.equal(retried.verdict.reason, WAIT_REASONS.REPEATED_INTERRUPTION)
  assert.equal(retried.verdict.interruptedChunks, 3)
  assert.equal(
    cut.load(HEAD_A).consecutiveInterruptions,
    3,
    'the interruption count is not cleared',
  )
  assert.equal(h2.sleeps.length, 0, 'the retry bound still holds')
})

test('R3: a rerun on a head whose CI settled or failed starts a fresh budget', async () => {
  for (const [ended, first] of [
    [WAIT_STATES.SETTLED, passing('a')],
    [WAIT_STATES.FAILED, failing('a')],
  ]) {
    const store = memStore()
    let nodes = [first]
    const h = harness({ script: () => view(HEAD_A, nodes) })
    assert.equal((await run(h, store, { budgetMs: 600_000 })).verdict.state, ended)
    h.advance(700_000)
    nodes = [running('a')]
    const chunkMs = 40_000
    const rerun = await run(h, store, { budgetMs: 600_000, chunkMs })
    assert.equal(rerun.verdict.state, WAIT_STATES.CONTINUE, `a rerun after ${ended} polls`)
    assert.equal(rerun.verdict.headSha, HEAD_A)
    assert.equal(rerun.verdict.headChanged, false)
    assert.ok(rerun.verdict.elapsedMs <= chunkMs, `elapsed ${rerun.verdict.elapsedMs}`)
    assert.ok(
      rerun.messages.some((m) => /polling 1 pending/.test(m)),
      'a new wait reports itself',
    )
  }
})

test('R3: an unreadable first read does not adopt a wait whose CI concluded', async () => {
  const store = memStore()
  const h = harness({ script: () => view(HEAD_A, [passing('a')]) })
  assert.equal((await run(h, store, { budgetMs: 600_000 })).verdict.state, WAIT_STATES.SETTLED)
  h.advance(700_000)
  const blind = harness({ script: () => ({ ok: false, error: 'quota' }), start: h.now() })
  const chunkMs = 40_000
  const { verdict } = await run(blind, store, { budgetMs: 600_000, chunkMs })
  assert.equal(verdict.state, WAIT_STATES.CONTINUE)
  assert.equal(verdict.headSha, '', 'the concluded head record is not resumed')
  assert.ok(verdict.elapsedMs <= chunkMs, `elapsed ${verdict.elapsedMs}`)
  assert.equal(store.load(HEAD_A).lastVerdict.state, WAIT_STATES.SETTLED)

  // A timed-out wait is resumed by an unreadable read too, so it stays exhausted.
  const timed = memStore()
  const { h: h2 } = await exhaust(timed, () => 5)
  h2.advance(60_000)
  const blind2 = harness({ script: () => ({ ok: false, error: 'quota' }), start: h2.now() })
  const resumed = await run(blind2, timed, { budgetMs: 600_000, chunkMs: 40_000 })
  assert.equal(resumed.verdict.state, WAIT_STATES.TIMEOUT)
  assert.equal(resumed.verdict.headSha, HEAD_A)
})

// ---------------------------------------------------------------------------
// R7: the first-read report.

test('R7: the first read of a new wait names PR, short head, pending count and budget', async () => {
  const h = harness({ script: () => pendingView(HEAD_A, 3, 5) })
  const { messages } = await run(h, memStore(), { chunkMs: 40_000 })
  assert.equal(
    messages[0],
    `ci-wait: PR #7 head ${HEAD_A.slice(0, 7)}: no red signal; polling 3 pending of 5 checks; budget 3600s`,
  )
  // Every chunk end prints one progress line.
  assert.match(
    messages.at(-1),
    /^ci-wait: PR #7 head aaaaaaa: continue \(chunk-exhausted\) after \d+ read/,
  )

  const h2 = harness({ script: () => view(HEAD_A, [failing('lint'), running('test')]) })
  const red = await run(h2, memStore())
  assert.match(red.messages[0], /red signal: 1 failing of 2 checks/)

  // A resumed wait does not repeat the opening report.
  const store = memStore()
  const h3 = harness({ script: () => pendingView(HEAD_A, 2) })
  await run(h3, store, { chunkMs: 40_000 })
  const resumed = await run(h3, store, { chunkMs: 40_000 })
  assert.ok(!resumed.messages.some((m) => /polling/.test(m)))
})

// ---------------------------------------------------------------------------
// R8: status is a latch.

test('R8: status returns the last recorded reading with fresh:false and its age', async () => {
  const store = memStore()
  const h = harness({ script: () => pendingView(HEAD_A, 2) })
  await run(h, store, { chunkMs: 40_000 })
  const lastAt = store.load(HEAD_A).readings.at(-1).atMs
  const s = statusOf({ record: store.load(HEAD_A), now: () => lastAt + 90_000 })
  assert.equal(s.fresh, false)
  assert.equal(s.ageMs, 90_000)
  assert.equal(s.lastReading.pending, 2)
  assert.ok(!('state' in s), 'a latch carries no state a caller could assign to CI_WAIT_STATE')
  assert.deepEqual(statusOf({ record: null, now: () => 0 }), {
    fresh: false,
    found: false,
    reason: 'no-state',
  })
})

// ---------------------------------------------------------------------------
// R9: verdict mapping reuses classifyChecks.

test('R9: mapVerdict maps every classifier outcome onto the CI_WAIT_STATE vocabulary', () => {
  const cases = [
    [{ rollup: { statusCheckRollup: [passing('a')] } }, false, 'settled'],
    [{ rollup: { statusCheckRollup: [failing('a')] } }, false, 'failed'],
    [{ rollup: { statusCheckRollup: [running('a')] } }, true, null],
    [{ rollup: { statusCheckRollup: [] } }, true, null], // no-checks keeps waiting
    [{ readError: 'boom' }, true, null], // unreadable keeps waiting
    [
      { rollup: { statusCheckRollup: [passing('a')] }, priorContexts: ['a', 'gone'] },
      false,
      'unknown',
    ],
    [{ rollup: { statusCheckRollup: [nullShaped('a')] } }, false, 'unknown'],
    [{ rollup: { statusCheckRollup: [skipped('a')] } }, false, 'unknown'], // no-gate-ran
    [
      { headSHA: HEAD_A, observedSHA: HEAD_B, rollup: { statusCheckRollup: [passing('a')] } },
      false,
      'unknown',
    ],
  ]
  for (const [input, wait, state] of cases) {
    const verdict = classifyChecks(input)
    const mapped = mapVerdict(verdict)
    assert.equal(mapped.wait, wait, `${verdict.state}/${verdict.reason}`)
    assert.equal(mapped.state, state, `${verdict.state}/${verdict.reason}`)
    assert.notEqual(mapped.state, WAIT_STATES.CONTINUE, 'continue is never a mapped verdict')
  }
})

test('R9: runChunk ends settled, failed and unknown on the classifier verdicts', async () => {
  for (const [nodes, state, reason] of [
    [[passing('a'), passing('b')], 'settled', 'ok'],
    [[passing('a'), failing('b')], 'failed', 'failed'],
    [[skipped('a')], 'unknown', 'no-gate-ran'],
  ]) {
    const h = harness({ script: () => view(HEAD_A, nodes) })
    const { verdict } = await run(h, memStore())
    assert.equal(verdict.state, state)
    assert.equal(verdict.reason, reason)
    // Green waits one interval for a confirming read; every other terminal read ends at once.
    const settled = state === WAIT_STATES.SETTLED
    assert.equal(h.sleeps.length, settled ? 1 : 0, `${state}: delays taken`)
    assert.equal(verdict.reads, settled ? 2 : 1, `${state}: reads`)
  }
  // absent-gate is terminal even though the classifier calls it pending.
  const h = harness({ script: () => view(HEAD_A, [passing('a')]) })
  const absent = await run(h, memStore(), { priorContexts: ['a', 'deploy'] })
  assert.equal(absent.verdict.state, WAIT_STATES.UNKNOWN)
  assert.equal(absent.verdict.reason, 'absent-gate')
  // The terminal verdict names what is missing and the remedy for it.
  assert.deepEqual(absent.verdict.absent, ['deploy'])
  assert.equal(absent.verdict.absentGateRemedy, 're-trigger-absent-gate')
  assert.deepEqual(absent.verdict.notTriggered, [])
  assert.deepEqual(absent.verdict.pendingRuns, [])
})

test('R9: no-checks keeps waiting, then settles once checks attach', async () => {
  const h = harness({ script: (_req, i) => view(HEAD_A, i < 2 ? [] : [passing('a')]) })
  const { verdict } = await run(h, memStore())
  assert.equal(verdict.state, WAIT_STATES.SETTLED)
  assert.equal(verdict.reads, 4, 'two no-checks reads, then a green read and its confirmation')
})

test('R9: a null-shaped node is reconciled against gh pr checks before it may terminate', async () => {
  const h = harness({
    script: (req) =>
      req.what === 'view'
        ? view(HEAD_A, [passing('a'), nullShaped('b')])
        : { ok: true, payload: [{ name: 'b', state: 'SUCCESS', bucket: 'pass' }] },
  })
  const { verdict } = await run(h, memStore())
  assert.equal(verdict.state, WAIT_STATES.SETTLED)
  assert.deepEqual(
    h.calls.map((c) => c.what),
    ['view', 'checks', 'view', 'checks'],
  )
})

// ---------------------------------------------------------------------------
// Head workflow-run evidence and the confirmation read.

const headRun = (name, status, conclusion = '') => ({
  name,
  workflowName: name,
  status,
  conclusion,
  headSha: HEAD_A,
  event: 'pull_request',
})

test('runs: a green read with a queued head run waits, and settles only after a confirming green read', async () => {
  const h = harness({
    script: () => view(HEAD_A, [passing('a'), passing('b')]),
    runs: (_req, i) => ({
      ok: true,
      payload: i === 0 ? [headRun('a', 'completed', 'success'), headRun('bazel', 'queued')] : [],
    }),
  })
  const { verdict } = await run(h, memStore())
  assert.equal(verdict.state, WAIT_STATES.SETTLED)
  assert.equal(verdict.reads, 3, 'pending on queued runs, then green, then the confirming green')
  assert.equal(h.runCalls[0].headSha, HEAD_A, 'runs are read for the head the view named')
  assert.deepEqual(verdict.pendingRuns, [])
})

test('runs: a first-read green with completed runs settles on the second read, never on read 1', async () => {
  const h = harness({
    script: () => view(HEAD_A, [passing('a')]),
    runs: () => ({ ok: true, payload: [headRun('a', 'completed', 'success')] }),
  })
  const { verdict } = await run(h, memStore())
  assert.equal(verdict.state, WAIT_STATES.SETTLED)
  assert.equal(verdict.reads, 2)
  assert.equal(h.sleeps.length, 1)
})

test('runs: a green read whose check set shrank is not confirmed by the read before it', async () => {
  // Read 1 sees 2 green checks, read 2 sees 1: the larger earlier set does not confirm the smaller.
  const h = harness({
    script: (_req, i) => view(HEAD_A, i === 0 ? [passing('a'), passing('b')] : [passing('a')]),
  })
  const { verdict } = await run(h, memStore())
  assert.equal(verdict.state, WAIT_STATES.SETTLED)
  assert.equal(verdict.reads, 3, 'a shrunk set (2 → 1) needs one more matching read')
})

test('runs: a failed runs read never settles, but a red check still fails', async () => {
  const h = harness({
    script: () => view(HEAD_A, [passing('a')]),
    runs: () => ({ ok: false, error: 'HTTP 502' }),
  })
  const { verdict } = await run(h, memStore(), { chunkMs: 120_000 })
  assert.equal(verdict.state, WAIT_STATES.CONTINUE)
  assert.ok(verdict.reads > 2)

  const store = memStore()
  const red = harness({
    script: () => view(HEAD_A, [failing('a')]),
    runs: () => ({ ok: false, error: 'HTTP 502' }),
  })
  assert.equal((await run(red, store)).verdict.state, WAIT_STATES.FAILED)

  // A run list at the cap may be truncated: unreadable, keep waiting.
  const capped = harness({
    script: () => view(HEAD_A, [passing('a')]),
    runs: () => ({
      ok: true,
      payload: Array.from({ length: 100 }, (_, i) => headRun(`wf${i}`, 'completed', 'success')),
    }),
  })
  assert.equal(
    (await run(capped, memStore(), { chunkMs: 120_000 })).verdict.state,
    WAIT_STATES.CONTINUE,
  )
})

test('runs: a prior context whose workflow did not run on the head settles instead of absent-gate', async () => {
  const h = harness({
    script: () => view(HEAD_A, [{ ...passing('test-go'), workflowName: 'test-go' }]),
    runs: () => ({ ok: true, payload: [headRun('test-go', 'completed', 'success')] }),
  })
  const prior = {
    checks: [
      { name: 'test-go', state: 'SUCCESS', bucket: 'pass', workflow: 'test-go' },
      { name: 'guard', state: 'SUCCESS', bucket: 'pass', workflow: 'plugin-distribution' },
    ],
  }
  const { verdict } = await run(h, memStore(), { priorContexts: prior })
  assert.equal(verdict.state, WAIT_STATES.SETTLED)
  assert.deepEqual(verdict.notTriggered, ['guard'])
  assert.deepEqual(verdict.absent, ['guard'])
})

// ---------------------------------------------------------------------------
// The real edges: file store, gh reader, CLI wiring.

test('fileStore: atomic per-PR-per-head state files, latest() across heads', () => {
  const dir = mkdtempSync(path.join(os.tmpdir(), 'ci-wait-store-'))
  try {
    const store = fileStore({ dir, pr: 12 })
    assert.equal(store.load(HEAD_A), null)
    assert.equal(store.latest(), null)
    store.save({ pr: 12, headSha: HEAD_A, updatedAtMs: 1 })
    store.save({ pr: 12, headSha: HEAD_B, updatedAtMs: 2 })
    fileStore({ dir, pr: 13 }).save({ pr: 13, headSha: HEAD_A, updatedAtMs: 9 })
    assert.equal(store.pathFor(HEAD_A), path.join(dir, `pr12-${HEAD_A}.json`))
    assert.equal(store.load(HEAD_A).updatedAtMs, 1)
    assert.equal(store.latest().headSha, HEAD_B)
    assert.deepEqual(
      readdirSync(dir).filter((f) => f.endsWith('.tmp')),
      [],
      'no temp file survives a save',
    )
    store.remove(HEAD_B)
    assert.equal(store.latest().headSha, HEAD_A)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('defaultStateDir: one namespace per repository, shared by its subdirectories and worktrees', () => {
  const root = mkdtempSync(path.join(os.tmpdir(), 'ci-wait-repos-'))
  try {
    const repoA = path.join(root, 'a')
    const repoB = path.join(root, 'b')
    const worktree = path.join(root, 'a-wt')
    mkdirSync(path.join(repoA, '.git', 'worktrees', 'wt'), { recursive: true })
    mkdirSync(path.join(repoA, 'sub', 'deeper'), { recursive: true })
    mkdirSync(path.join(repoB, '.git'), { recursive: true })
    mkdirSync(worktree)
    writeFileSync(
      path.join(worktree, '.git'),
      `gitdir: ${path.join(repoA, '.git', 'worktrees', 'wt')}\n`,
    )
    writeFileSync(path.join(repoA, '.git', 'worktrees', 'wt', 'commondir'), '../..\n')
    const a = defaultStateDir(repoA)
    assert.equal(path.dirname(a), path.join(os.tmpdir(), 'boss-ci-wait'))
    assert.equal(defaultStateDir(path.join(repoA, 'sub', 'deeper')), a)
    assert.equal(defaultStateDir(worktree), a, 'a worktree shares its repository namespace')
    assert.notEqual(defaultStateDir(repoB), a, 'PR #7 of another repository is another file')
  } finally {
    rmSync(root, { recursive: true, force: true })
  }
})

test('ghReader: passes the timeout to gh, tolerates pending exit 8, fails closed on errors', async () => {
  const seen = []
  const spawn = (cmd, args, opts) => {
    seen.push({ cmd, args, timeout: opts.timeout })
    if (args[1] === 'view')
      return { status: 0, stdout: JSON.stringify({ headRefOid: HEAD_A, statusCheckRollup: [] }) }
    return { status: 8, stdout: '[]' }
  }
  const read = ghReader({ spawn })
  assert.equal((await read({ what: 'runs', pr: 3, headSha: HEAD_A, timeoutMs: 77 })).ok, false)
  assert.deepEqual(seen.pop().args, [
    'run',
    'list',
    '--commit',
    HEAD_A,
    '--json',
    'name,workflowName,status,conclusion,headSha,event',
    '--limit',
    '100',
  ])
  assert.deepEqual(await read({ what: 'runs', pr: 3, timeoutMs: 5 }), {
    ok: false,
    error: 'no head SHA',
  })
  assert.deepEqual(await read({ what: 'view', pr: 3, timeoutMs: 1234 }), {
    ok: true,
    headSha: HEAD_A,
    payload: { headRefOid: HEAD_A, statusCheckRollup: [] },
  })
  assert.equal((await read({ what: 'checks', pr: 3, timeoutMs: 99 })).ok, true)
  assert.deepEqual(
    seen.map((s) => [s.args.slice(0, 3).join(' '), s.timeout]),
    [
      ['pr view 3', 1234],
      ['pr checks 3', 99],
    ],
  )
  const timedOut = ghReader({ spawn: () => ({ error: { code: 'ETIMEDOUT' }, status: null }) })
  assert.deepEqual(await timedOut({ what: 'view', pr: 3, timeoutMs: 5 }), {
    ok: false,
    error: 'ETIMEDOUT',
  })
  const garbage = ghReader({ spawn: () => ({ status: 0, stdout: 'not json' }) })
  assert.equal((await garbage({ what: 'view', pr: 3, timeoutMs: 5 })).ok, false)
})

test('main: run persists to the state dir and status reads it back as a latch', async () => {
  const dir = mkdtempSync(path.join(os.tmpdir(), 'ci-wait-main-'))
  try {
    const h = harness({ script: () => view(HEAD_A, [passing('a')]) })
    const out = await main(['run', '--pr', '4', '--state-dir', dir], {
      read: h.read,
      sleep: h.sleep,
      now: h.now,
    })
    assert.equal(out.verdict.state, WAIT_STATES.SETTLED)
    assert.equal(out.verdict.stateFile, path.join(dir, `pr4-${HEAD_A}.json`))
    const saved = JSON.parse(readFileSync(out.verdict.stateFile, 'utf8'))
    assert.equal(saved.openChunk, null)
    assert.equal(saved.lastVerdict.state, WAIT_STATES.SETTLED)
    const status = await main(['status', '--pr', '4', '--head-sha', HEAD_A, '--state-dir', dir], {
      now: () => h.now() + 1000,
    })
    assert.equal(status.verdict.fresh, false)
    assert.equal(status.verdict.ageMs, 1000)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})
