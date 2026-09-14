import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, utimesSync, writeFileSync } from 'node:fs'
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
  dispatchDisposition,
  legTimeoutMsFromEnv,
  probeArtifacts,
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

// Both dispatch-ordering tests below record entry and exit instead of reading a clock. The claim
// in each case is ordering, and a clock cannot establish ordering: it can only establish that a run
// was fast or slow, which a loaded host is free to change underneath the assertion. awaitAll starts
// every member of a wave synchronously (Promise.all over the batch), so a zero-delay yield is enough
// to let a concurrent wave interleave while a sequential one cannot — deterministic, and in
// milliseconds rather than the six seconds of real sleeps this replaced.
const yieldToTheEventLoop = () => new Promise((resolve) => setTimeout(resolve, 0))

async function traceDispatch(trace, node) {
  trace.push(`enter:${node.id}`)
  await yieldToTheEventLoop()
  trace.push(`exit:${node.id}`)
  return node.id
}

test('awaitAll runs one wave concurrently through the injected dispatcher', async () => {
  const trace = []
  const result = await awaitAll([{ id: 'a' }, { id: 'b' }], (node) => traceDispatch(trace, node))
  assert.deepEqual(result.results, [['a', 'b']])
  // BOTH entries precede EITHER exit. Only one concurrent wave produces that; a dispatcher that ran
  // the two one at a time would interleave enter/exit per member and fail here.
  assert.deepEqual(trace, ['enter:a', 'enter:b', 'exit:a', 'exit:b'])
})

test('sequential waves remain sequential', async () => {
  const trace = []
  const result = await awaitAll([{ id: 'a' }, { id: 'b', blockedBy: ['a'] }], (node) =>
    traceDispatch(trace, node),
  )
  assert.deepEqual(result.results, [['a'], ['b']])
  // `b` has no entry before `a`'s exit. A planBatches that ignored blockedBy would put both in one
  // wave and produce the concurrent trace above, so this cannot pass by accident.
  assert.deepEqual(trace, ['enter:a', 'exit:a', 'enter:b', 'exit:b'])
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

// ---------------------------------------------------------------------------
// dispatchDisposition — the single publishable-verdict + recovery decision.
// Every case is built from CONSTRUCTED sentinel state (a real run file written through
// writeSentinel, or a real file on disk), never from a returned prose fixture.
// ---------------------------------------------------------------------------

function artifact(name, body = 'complete') {
  const path = join(mkdtempSync(join(tmpdir(), 'bda-art-')), name)
  writeFileSync(path, body)
  return path
}

test('probeArtifacts splits complete survivors from absent and empty paths', () => {
  const complete = artifact('plan.md')
  const empty = artifact('empty.md', '')
  const absent = join(tmpdir(), 'bda-absent-artifact.md')
  const dir = mkdtempSync(join(tmpdir(), 'bda-dir-'))
  const probe = probeArtifacts([complete, empty, absent, dir, '', null])
  assert.deepEqual(probe.surviving, [complete], 'only a non-empty regular file survives')
  assert.deepEqual(probe.missing, [empty, absent, dir])
})

test('a provisional payload is non-publishable on EVERY kind, clean included', () => {
  for (const kind of ['clean', 'capped', 'bs-review clean: no open must-fix findings.']) {
    const ctx = context()
    writeSentinel(ctx, 'review', kind, { provisional: true })
    const d = dispatchDisposition(ctx, 'review', { now: 1_000, deadlineAt: 2_000 })
    assert.equal(d.publishable, false, `kind ${kind} must not be publishable while provisional`)
    assert.equal(d.disposition, 'resume')
    assert.equal(d.reason, 'provisional-payload')
    assert.equal(d.kind, kind, 'the kind is still reported, so the caller can say what was demoted')
  }
  // Able to fire: the SAME kind with the marker explicitly false is publishable, so the demotion
  // is the marker's doing and not a blanket refusal.
  const ctx = context()
  writeSentinel(ctx, 'review', 'clean', { provisional: false })
  const earned = dispatchDisposition(ctx, 'review', { now: 1_000, deadlineAt: 2_000 })
  assert.equal(earned.publishable, true)
  assert.equal(earned.disposition, 'publish')
  assert.equal(earned.reason, 'derived-verdict')
})

test('the disposition splits a resume-worthy dispatch death from a discardable one', () => {
  // Past the deadline but inside the staleness window: the agent may still be live, so one resume
  // is worth attempting before anything is re-dispatched from scratch.
  const live = context()
  const timedOut = dispatchDisposition(live, 'draft', {
    now: 2_001,
    deadlineAt: 2_000,
    dispatchedAt: 2_000,
  })
  assert.equal(timedOut.status, TIMED_OUT)
  assert.deepEqual(
    [timedOut.disposition, timedOut.reason, timedOut.retainScratch],
    ['resume', 'timed-out', false],
  )

  // Stale, and the probe found nothing: there is no evidence left to salvage, so removal is the
  // honest action. This is the ONLY arm that authorises a destructive cleanup.
  const dead = context()
  const abandoned = dispatchDisposition(dead, 'draft', {
    now: DEFAULT_OPEN_DISPATCH_STALE_MS + 1,
    deadlineAt: 1,
    dispatchedAt: 0,
    openedAt: 0,
    artifacts: [join(tmpdir(), 'bda-never-written.md')],
  })
  assert.equal(abandoned.status, ABANDONED)
  assert.deepEqual(
    [abandoned.disposition, abandoned.reason, abandoned.retainScratch],
    ['discard', 'abandoned', false],
  )
})

test('a surviving complete artifact outranks the death class and blocks the discard', () => {
  const plan = artifact('BOS-plan.md', '# a finished plan the dispatch never got to report\n')
  const ctx = context()
  const salvaged = dispatchDisposition(ctx, 'draft', {
    now: DEFAULT_OPEN_DISPATCH_STALE_MS + 1,
    deadlineAt: 1,
    dispatchedAt: 0,
    openedAt: 0,
    artifacts: [plan],
  })
  assert.equal(salvaged.status, ABANDONED, 'the death class itself is unchanged')
  assert.equal(salvaged.disposition, 'resume', 'but recovery, not removal, is the disposition')
  assert.equal(salvaged.reason, 'surviving-artifact')
  assert.equal(salvaged.retainScratch, true, 'unread evidence must survive the failure branch')
  assert.deepEqual(salvaged.surviving, [plan])
  assert.equal(salvaged.publishable, false, 'salvage is never a verdict')
})

test('a still-running dispatch waits and keeps its scratch', () => {
  const ctx = context()
  const d = dispatchDisposition(ctx, 'draft', { now: 1_000, deadlineAt: 2_000, dispatchedAt: 900 })
  assert.deepEqual(
    [d.status, d.disposition, d.reason, d.publishable, d.retainScratch],
    [STILL_RUNNING, 'wait', 'still-running', false, true],
  )
})

test('CLI `disposition` and `probe` expose the same decision to shell callers', () => {
  const ctx = context()
  writeSentinel(ctx, 'review', 'clean', { provisional: true })
  const cli = JSON.parse(runCli(['disposition', ctx.dir, ctx.runId, 'review']).stdout)
  assert.equal(cli.disposition, 'resume')
  assert.equal(cli.reason, 'provisional-payload')
  assert.equal(cli.publishable, false)

  const plan = artifact('plan.md')
  const probed = JSON.parse(runCli(['probe', plan, join(tmpdir(), 'bda-nope.md')]).stdout)
  assert.deepEqual(probed.surviving, [plan])

  // Repeated --artifact flags, never a comma list: zsh does not word-split an unquoted parameter
  // expansion, so a joined list would arrive as one path that exists nowhere.
  const withArtifact = JSON.parse(
    runCli(['disposition', ctx.dir, ctx.runId, 'absent', '--artifact', plan, '--deadline', '1'])
      .stdout,
  )
  assert.deepEqual(withArtifact.surviving, [plan])
  assert.equal(withArtifact.retainScratch, true)

  const bad = runCli(['disposition', ctx.dir, ctx.runId, 'review', '--nope', 'x'])
  assert.equal(bad.status, 2)
  assert.match(bad.stderr, /unknown disposition flag/)

  // A non-numeric deadline must FAIL, not fall through: NaN survives the `??=` default and loses
  // every comparison, so an unrejected typo would classify a dead dispatch as `still-running`.
  for (const flag of ['--deadline', '--now']) {
    const typo = runCli(['disposition', ctx.dir, ctx.runId, 'review', flag, '3O0'])
    assert.equal(typo.status, 2, `${flag} with a non-numeric value must fail, never default`)
    assert.match(typo.stderr, /requires a finite number/)
  }
})

test('CLI `guard-discard` authorises an empty probe and refuses a surviving one', () => {
  const absent = join(tmpdir(), 'bda-guard-absent.md')
  const authorised = runCli(['guard-discard', absent])
  assert.equal(authorised.status, 0, 'nothing survived — the removal is authorised')
  assert.equal(authorised.stderr, '')

  const plan = artifact('plan.md', '# finished\n')
  const refused = runCli(['guard-discard', absent, plan])
  assert.equal(refused.status, 1, 'a survivor must REFUSE the removal, not merely report it')
  assert.match(refused.stderr, /REFUSING removal/)
  assert.ok(refused.stderr.includes(plan), 'the refusal must NAME what it saved')

  assert.equal(
    runCli(['guard-discard']).status,
    2,
    'a path-less call is a wiring error, not a pass',
  )
})
