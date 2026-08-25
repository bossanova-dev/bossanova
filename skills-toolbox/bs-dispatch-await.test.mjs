import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, utimesSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  ABANDONED,
  COMPLETED,
  DEFAULT_AWAIT_TIMEOUT_MULTIPLIER,
  DEFAULT_DISPATCH_LEG_TIMEOUT_MS,
  DEFAULT_OPEN_DISPATCH_STALE_MS,
  DISPATCH_AWAIT_RESULTS,
  MAX_BATCH_WIDTH,
  STILL_RUNNING,
  TIMED_OUT,
  awaitAll,
  awaitDeadlineMs,
  classifyDispatch,
  legTimeoutMsFromEnv,
  openDispatches,
  planBatches,
  toSentinelRouting,
} from './bs-dispatch-await.mjs'
import { DISPATCH_FAILURE, makeRunContext, writeSentinel } from './bs-run-sentinel.mjs'

const scriptPath = fileURLToPath(new URL('./bs-dispatch-await.mjs', import.meta.url))

function runCli(args = []) {
  const res = spawnSync(process.execPath, [scriptPath, ...args], { encoding: 'utf8' })
  return { stdout: res.stdout.trim(), stderr: res.stderr.trim(), status: res.status }
}

function context() {
  return makeRunContext('boss-review', { tmpdir: mkdtempSync(join(tmpdir(), 'bda-')) })
}

test('await result tokens are owned here and exclude sentinel dispatch-failure', () => {
  assert.deepEqual(DISPATCH_AWAIT_RESULTS, ['completed', 'still-running', 'timed-out', 'abandoned'])
  assert.ok(!DISPATCH_AWAIT_RESULTS.includes(DISPATCH_FAILURE))
})

test('await deadline is caller-owned and strictly larger than the leg cap', () => {
  assert.equal(DEFAULT_DISPATCH_LEG_TIMEOUT_MS, 300_000)
  assert.equal(DEFAULT_AWAIT_TIMEOUT_MULTIPLIER, 1.25)
  assert.equal(awaitDeadlineMs({ legTimeoutMs: 300_000 }), 375_000)
  assert.equal(awaitDeadlineMs({ env: { BOSS_SKILL_EXTENSION_TIMEOUT_MS: '600000' } }), 750_000)
  assert.throws(() => legTimeoutMsFromEnv({ BOSS_SKILL_EXTENSION_TIMEOUT_MS: '10s' }), /digits/)
})

test('absent artefact is still-running before the deadline, never completed', () => {
  const ctx = context()
  const result = classifyDispatch(ctx, 'leg-a', {
    now: 1_000,
    deadlineAt: 2_000,
    dispatchedAt: 900,
  })
  assert.equal(result.status, STILL_RUNNING)
})

test('absent artefact is timed-out after the deadline and maps to dispatch-failure', () => {
  const ctx = context()
  const result = classifyDispatch(ctx, 'leg-a', {
    now: 2_001,
    deadlineAt: 2_000,
    dispatchedAt: 900,
  })
  assert.equal(result.status, TIMED_OUT)
  assert.equal(toSentinelRouting(result), DISPATCH_FAILURE)
  assert.equal(Object.hasOwn(result, 'payload'), false)
})

test('a stale run-id never classifies as completed', () => {
  const ctx = context()
  writeSentinel(ctx, 'leg-a', 'clean', {})
  const other = { ...ctx, runId: 'OTHER' }
  assert.equal(
    classifyDispatch(other, 'leg-a', { now: 1_000, deadlineAt: 2_000 }).status,
    STILL_RUNNING,
  )
})

test('a provisional seed never classifies as completed', () => {
  const ctx = context()
  writeSentinel(ctx, 'leg-a', 'clean', { provisional: true })
  assert.equal(
    classifyDispatch(ctx, 'leg-a', { now: 1_000, deadlineAt: 2_000 }).status,
    STILL_RUNNING,
  )
})

test('a non-provisional sentinel classifies as completed and carries payload', () => {
  const ctx = context()
  writeSentinel(ctx, 'leg-a', 'clean', { provisional: false, findings: [] })
  const result = classifyDispatch(ctx, 'leg-a', { now: 1_000, deadlineAt: 2_000 })
  assert.equal(result.status, COMPLETED)
  assert.equal(result.kind, 'clean')
  assert.deepEqual(result.payload.findings, [])
})

test('launcher exit code is never consulted', () => {
  const launcher = spawnSync(process.execPath, ['-e', 'process.exit(0)'])
  assert.equal(launcher.status, 0)
  const ctx = context()
  assert.equal(
    classifyDispatch(ctx, 'leg-a', { now: 1_000, deadlineAt: 2_000 }).status,
    STILL_RUNNING,
  )
  assert.equal(classifyDispatch(ctx, 'leg-a', { now: 2_001, deadlineAt: 2_000 }).status, TIMED_OUT)
})

test('a leg completing at 1.1x the leg cap is completed, not timed-out', () => {
  const ctx = context()
  const legCap = 1_000
  const started = 10_000
  writeSentinel(ctx, 'leg-a', 'clean', { provisional: false })
  const result = classifyDispatch(ctx, 'leg-a', {
    now: started + Math.floor(legCap * 1.1),
    deadlineAt: started + awaitDeadlineMs({ legTimeoutMs: legCap }),
  })
  assert.equal(result.status, COMPLETED)
})

test('openDispatches reports provisional sentinels with age and stale marker', () => {
  const ctx = context()
  writeSentinel(ctx, 'opened', 'pending', { provisional: true })
  const old = new Date(Date.now() - DEFAULT_OPEN_DISPATCH_STALE_MS - 1_000)
  utimesSync(ctx.sentinelPath('opened'), old, old)
  const [opened] = openDispatches(ctx, { now: Date.now() })
  assert.equal(opened.name, 'opened')
  assert.equal(opened.stale, true)
  assert.ok(opened.ageMs >= DEFAULT_OPEN_DISPATCH_STALE_MS)
})

test('classifyDispatch reports abandoned when the open entry is stale', () => {
  const ctx = context()
  writeSentinel(ctx, 'opened', 'pending', { provisional: true })
  const old = new Date(Date.now() - DEFAULT_OPEN_DISPATCH_STALE_MS - 1_000)
  utimesSync(ctx.sentinelPath('opened'), old, old)
  const result = classifyDispatch(ctx, 'opened', {
    now: Date.now(),
    deadlineAt: Date.now() + 1_000,
  })
  assert.equal(result.status, ABANDONED)
  assert.equal(toSentinelRouting(result), DISPATCH_FAILURE)
})

test('two independent dispatches produce one wave', () => {
  assert.deepEqual(
    planBatches([{ id: 'a' }, { id: 'b' }]).map((wave) => wave.map((node) => node.id)),
    [['a', 'b']],
  )
})

test('a dependent pair produces two waves in dependency order', () => {
  assert.deepEqual(
    planBatches([{ id: 'a' }, { id: 'b', blockedBy: ['a'] }]).map((wave) =>
      wave.map((node) => node.id),
    ),
    [['a'], ['b']],
  )
})

test('mutates/mutates and mutates/reads intersections split waves', () => {
  assert.deepEqual(
    planBatches([
      { id: 'a', mutates: ['x'] },
      { id: 'b', mutates: ['x'] },
      { id: 'c', reads: ['y'] },
      { id: 'd', mutates: ['y'] },
    ]).map((wave) => wave.map((node) => node.id)),
    [
      ['a', 'c'],
      ['b', 'd'],
    ],
  )
})

test('duplicate outPath splits waves', () => {
  assert.deepEqual(
    planBatches([
      { id: 'a', outPath: 'review.json' },
      { id: 'b', outPath: 'review.json' },
    ]).map((wave) => wave.map((node) => node.id)),
    [['a'], ['b']],
  )
})

test('a mutating member declaring no paths conflicts with every other member', () => {
  assert.deepEqual(
    planBatches([{ id: 'a', mutates: [] }, { id: 'b' }]).map((wave) => wave.map((node) => node.id)),
    [['a'], ['b']],
  )
})

test('unknown blocker and cycle throw named errors', () => {
  assert.throws(
    () => planBatches([{ id: 'a', blockedBy: ['missing'] }]),
    /unknown dispatch blocker/,
  )
  assert.throws(
    () =>
      planBatches([
        { id: 'a', blockedBy: ['b'] },
        { id: 'b', blockedBy: ['a'] },
      ]),
    /unschedulable dispatch graph/,
  )
})

test('MAX_BATCH_WIDTH caps independent waves', () => {
  const nodes = Array.from({ length: MAX_BATCH_WIDTH + 1 }, (_, index) => ({ id: `n${index}` }))
  assert.deepEqual(
    planBatches(nodes).map((wave) => wave.length),
    [MAX_BATCH_WIDTH, 1],
  )
})

test('planBatches is pure', () => {
  const nodes = [{ id: 'a' }, { id: 'b', blockedBy: ['a'] }]
  const before = JSON.stringify(nodes)
  planBatches(nodes)
  assert.equal(JSON.stringify(nodes), before)
})

test('awaitAll runs one wave concurrently through the injected dispatcher', async () => {
  const started = Date.now()
  const result = await awaitAll([{ id: 'a' }, { id: 'b' }], async (node) => {
    await new Promise((resolve) => setTimeout(resolve, 2_000))
    return node.id
  })
  const elapsed = Date.now() - started
  assert.ok(elapsed < 3_000, `expected one concurrent wave under 3000ms; got ${elapsed}`)
  assert.deepEqual(result.results, [['a', 'b']])
})

test('sequential waves remain sequential', async () => {
  const started = Date.now()
  await awaitAll([{ id: 'a' }, { id: 'b', blockedBy: ['a'] }], async () => {
    await new Promise((resolve) => setTimeout(resolve, 2_000))
  })
  const elapsed = Date.now() - started
  assert.ok(elapsed > 4_000, `expected ordered waves over 4000ms; got ${elapsed}`)
})

test('CLI seed, classify, open, and batches are exercised by subprocesses', () => {
  const ctx = context()
  assert.equal(runCli(['seed', ctx.dir, ctx.runId, 'leg-a', 'pending']).status, 0)
  const classified = JSON.parse(
    runCli(['classify', ctx.dir, ctx.runId, 'leg-a', String(Date.now() + 1_000)]).stdout,
  )
  assert.equal(classified.status, STILL_RUNNING)
  const opened = JSON.parse(runCli(['open', ctx.dir, ctx.runId]).stdout)
  assert.deepEqual(
    opened.map((entry) => entry.name),
    ['leg-a'],
  )
  const batches = JSON.parse(runCli(['batches', JSON.stringify([{ id: 'a' }, { id: 'b' }])]).stdout)
  assert.deepEqual(
    batches.map((wave) => wave.map((node) => node.id)),
    [['a', 'b']],
  )
})

test('CLI exits non-zero on bad input', () => {
  assert.notEqual(runCli(['batches', '[{\"id\":\"a\",\"blockedBy\":[\"missing\"]}]']).status, 0)
})
