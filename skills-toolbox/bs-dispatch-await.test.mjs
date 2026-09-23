import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, readFileSync, utimesSync, writeFileSync } from 'node:fs'
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
  HEARTBEAT_ABSENT,
  HEARTBEAT_LIVE,
  HEARTBEAT_LIVENESS,
  HEARTBEAT_STALE,
  MAX_BATCH_WIDTH,
  MESSAGE_NOT_RETURNED,
  MESSAGE_RETURNED,
  STILL_RUNNING,
  TIMED_OUT,
  awaitAll,
  awaitDeadlineMs,
  classifyDispatch,
  dispatchDisposition,
  dispatchReadiness,
  heartbeatLiveness,
  legTimeoutMsFromEnv,
  messageReadiness,
  probeArtifacts,
  openDispatches,
  planBatches,
  readHeartbeat,
  toSentinelRouting,
  touchHeartbeat,
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

// ---------------------------------------------------------------------------
// Liveness — settled on the heartbeat artifact, never on the sentinel seed's
// frozen mtime and never on transcript silence.
// ---------------------------------------------------------------------------

function heartbeatPath() {
  return join(mkdtempSync(join(tmpdir(), 'bda-hb-')), 'BOS-1.dispatch-heartbeat.json')
}

/** A sentinel context whose seed is already `ageMs` old — the 34-minute drafter. */
function agedSeed(ageMs) {
  const ctx = context()
  writeSentinel(ctx, 'draft', 'pending', { provisional: true })
  const old = new Date(Date.now() - ageMs)
  utimesSync(ctx.sentinelPath('draft'), old, old)
  return ctx
}

test('the heartbeat vocabulary is owned here and absent is not stale', () => {
  assert.deepEqual(HEARTBEAT_LIVENESS, ['live', 'stale', 'absent'])
  // Absent must be its OWN status, never folded into stale: a worker that has not
  // touched the heartbeat yet would otherwise be killed for the omission.
  assert.notEqual(HEARTBEAT_ABSENT, HEARTBEAT_STALE)
})

test('touchHeartbeat records an epoch a later read recovers, and bounds its note', () => {
  const path = heartbeatPath()
  assert.equal(readHeartbeat(path), null, 'nothing written yet')
  const written = touchHeartbeat(path, { now: 5_000, note: 'x'.repeat(500) })
  assert.equal(written.at, 5_000)
  const read = readHeartbeat(path)
  assert.equal(read.at, 5_000)
  assert.equal(read.source, 'recorded')
  assert.equal(read.note.length, 200, 'the note is a bounded liveness disclosure, not a transport')
})

test('a heartbeat with no recorded epoch falls back to its own mtime, never to death', () => {
  // The worktree-lock.sh precedent: `eff_heartbeat` reads the recorded value when
  // present and the artifact's mtime when not. A hand-touched or half-written
  // heartbeat still PROVES something was here.
  const path = heartbeatPath()
  writeFileSync(path, 'not json at all')
  const read = readHeartbeat(path)
  assert.equal(read.source, 'mtime')
  assert.equal(heartbeatLiveness(path, { now: Date.now() }).status, HEARTBEAT_LIVE)
})

test('heartbeat liveness is a STRICT age < stale boundary', () => {
  const path = heartbeatPath()
  touchHeartbeat(path, { now: 0 })
  assert.equal(heartbeatLiveness(path, { now: 999, staleAfterMs: 1_000 }).status, HEARTBEAT_LIVE)
  // Exactly at the threshold is decisively STALE — the same boundary
  // worktree-lock.sh uses, so two readers at the boundary cannot both back off.
  assert.equal(heartbeatLiveness(path, { now: 1_000, staleAfterMs: 1_000 }).status, HEARTBEAT_STALE)
})

test('a fresh heartbeat keeps a long-running dispatch live past the stale window', () => {
  // The measured BOS-1269 failure: a drafter alive at minute 34 classified
  // `abandoned` because the age came from the SEED's mtime, which never moves.
  const ctx = agedSeed(DEFAULT_OPEN_DISPATCH_STALE_MS + 4 * 60_000)
  const now = Date.now()

  const withoutHeartbeat = classifyDispatch(ctx, 'draft', { now, deadlineAt: now + 60_000 })
  assert.equal(withoutHeartbeat.status, ABANDONED, 'the seed clock alone still reports death')

  const path = heartbeatPath()
  touchHeartbeat(path, { now: now - 1_000 })
  const withHeartbeat = classifyDispatch(ctx, 'draft', {
    now,
    deadlineAt: now + 60_000,
    heartbeatPath: path,
  })
  assert.equal(withHeartbeat.status, STILL_RUNNING, 'the dispatch itself says it is working')
  assert.equal(withHeartbeat.heartbeat.status, HEARTBEAT_LIVE)
  assert.equal(
    withHeartbeat.ageMs,
    1_000,
    'the reported age is the heartbeat age, not the seed age',
  )
})

test('a stale heartbeat is abandoned, and a live one still times out on the caller budget', () => {
  const now = Date.now()
  const ctx = agedSeed(DEFAULT_OPEN_DISPATCH_STALE_MS + 60_000)

  const stale = heartbeatPath()
  touchHeartbeat(stale, { now: now - DEFAULT_OPEN_DISPATCH_STALE_MS - 1 })
  const dead = classifyDispatch(ctx, 'draft', {
    now,
    deadlineAt: now + 60_000,
    heartbeatPath: stale,
  })
  assert.equal(dead.status, ABANDONED)
  assert.equal(dead.heartbeat.status, HEARTBEAT_STALE)
  assert.equal(toSentinelRouting(dead), DISPATCH_FAILURE)

  // A live heartbeat suppresses `abandoned` but NOT `timed-out`: the two answer
  // different questions, and a live worker can still blow the caller's budget.
  const live = heartbeatPath()
  touchHeartbeat(live, { now })
  const expired = classifyDispatch(ctx, 'draft', { now, deadlineAt: now - 1, heartbeatPath: live })
  assert.equal(expired.status, TIMED_OUT)
  assert.equal(expired.heartbeat.status, HEARTBEAT_LIVE)
})

test('a stale or foreign heartbeat over a FRESH seed can never kill the dispatch', () => {
  // The header states "it can only ever EXTEND liveness" as a CONSTRUCTION, so it has to hold for a
  // beat this dispatch did not write. `readHeartbeat` cannot attribute a beat to anyone, and
  // `heartbeatPath` is caller-supplied, so a leftover or foreign artifact at that path reads `stale`
  // over a seed of age ~0. Gating death on the beat alone would abandon a brand-new dispatch.
  const now = Date.now()
  const ctx = agedSeed(1_000)
  const foreign = heartbeatPath()
  touchHeartbeat(foreign, { now: now - DEFAULT_OPEN_DISPATCH_STALE_MS - 1 })

  const classified = classifyDispatch(ctx, 'draft', {
    now,
    deadlineAt: now + 60_000,
    heartbeatPath: foreign,
  })
  assert.equal(classified.status, STILL_RUNNING, 'a fresh seed outranks a stale beat')
  assert.equal(classified.heartbeat.status, HEARTBEAT_STALE, 'the stale reading is still REPORTED')

  // Same property under the one knob that could break it: `heartbeatStaleAfterMs` is free to be
  // TIGHTER than `staleAfterMs`, which makes a beat read stale while the seed is still young.
  const tight = classifyDispatch(ctx, 'draft', {
    now,
    deadlineAt: now + 60_000,
    heartbeatPath: foreign,
    heartbeatStaleAfterMs: 1,
  })
  assert.equal(tight.status, STILL_RUNNING, 'no value of the knob may introduce a death')
})

test('classifyDispatch reports the seed age beside the liveness age, never instead of it', () => {
  // `ageMs` changes SUBJECT with the presence of a heartbeat — beat age when one is consulted, seed
  // age otherwise — so "how long has this dispatch been open" must stay separately readable, here
  // and through `dispatchDisposition`, which copies both into its recovery verdicts.
  const now = Date.now()
  const ctx = agedSeed(DEFAULT_OPEN_DISPATCH_STALE_MS + 4 * 60_000)
  const path = heartbeatPath()
  touchHeartbeat(path, { now: now - 1_000 })

  const classified = classifyDispatch(ctx, 'draft', {
    now,
    deadlineAt: now + 60_000,
    heartbeatPath: path,
  })
  assert.equal(classified.ageMs, 1_000, 'the liveness age is the beat age')
  assert.ok(
    classified.seedAgeMs >= DEFAULT_OPEN_DISPATCH_STALE_MS,
    'the seed age survives on the result rather than being discarded',
  )
  // With no heartbeat consulted the two are the same number, so a caller reading `seedAgeMs`
  // unconditionally never has to branch on whether a beat was supplied.
  const blind = classifyDispatch(ctx, 'draft', { now, deadlineAt: now + 60_000 })
  assert.equal(blind.ageMs, blind.seedAgeMs)
})

test('an absent heartbeat falls back to the seed clock and can never be MORE aggressive', () => {
  const now = Date.now()
  // Young dispatch that has not beaten yet: still-running, never killed for the
  // omission. This is the TOCTOU window worktree-lock.sh's mtime fallback closes.
  const young = agedSeed(1_000)
  const absent = join(tmpdir(), 'bda-hb-never-written.json')
  const fresh = classifyDispatch(young, 'draft', {
    now,
    deadlineAt: now + 60_000,
    heartbeatPath: absent,
  })
  assert.equal(fresh.status, STILL_RUNNING)
  assert.equal(
    Object.hasOwn(fresh, 'heartbeat'),
    false,
    'no heartbeat was consulted, so none is reported',
  )

  // The safety property that makes adoption free: the heartbeat is written no
  // earlier than the seed, so at the same window a live-today dispatch can never
  // be newly killed by consulting it. Every seed age is classified identically
  // with an absent heartbeat and without one.
  for (const ageMs of [
    0,
    1_000,
    DEFAULT_OPEN_DISPATCH_STALE_MS - 1,
    DEFAULT_OPEN_DISPATCH_STALE_MS,
  ]) {
    const ctx = agedSeed(ageMs)
    const opts = { now, deadlineAt: now + 60_000 }
    assert.equal(
      classifyDispatch(ctx, 'draft', { ...opts, heartbeatPath: absent }).status,
      classifyDispatch(ctx, 'draft', opts).status,
      `seed age ${ageMs} classified differently with an absent heartbeat`,
    )
  }
})

test('the await side never writes the heartbeat it reads', () => {
  // A reader that touched the artifact it measures would manufacture the liveness
  // it claims to observe — the defect, moved into a new file.
  const path = heartbeatPath()
  touchHeartbeat(path, { now: 1_000 })
  const before = readFileSync(path, 'utf8')
  const ctx = agedSeed(1_000)
  classifyDispatch(ctx, 'draft', {
    now: Date.now(),
    deadlineAt: Date.now() + 1,
    heartbeatPath: path,
  })
  heartbeatLiveness(path, { now: Date.now() })
  assert.equal(readFileSync(path, 'utf8'), before, 'classification must not beat')
})

test('`beat` keeps beating through one long command and exits with the CHILD status', async () => {
  // The risk this closes: a worker blocked inside one long gate call writes
  // nothing between tool calls, so a heartbeat only the worker could touch would
  // reproduce the false oracle it replaces. The wrapper beats from the call.
  const path = heartbeatPath()
  const wrapped = runCli([
    'beat',
    path,
    '--interval',
    '15',
    '--',
    process.execPath,
    '-e',
    'setTimeout(()=>process.exit(3),220)',
  ])
  assert.equal(wrapped.status, 3, 'the wrapped command status, never the launcher 0')
  const after = readHeartbeat(path)
  assert.ok(after.at >= Date.now() - 5_000, 'the final beat lands at the end of the long command')
  assert.match(after.note, /-e/, 'the beat names the command it is reporting on')

  // A beat DURING the command, not merely at its ends: the file is younger than
  // the command's own start by construction only if the interval fired.
  const beats = runCli([
    'beat',
    path,
    '--interval',
    '15',
    '--',
    process.execPath,
    '-e',
    'setTimeout(()=>process.exit(0),200)',
  ])
  assert.equal(beats.status, 0)

  assert.equal(runCli(['beat', path, '--', process.execPath, '-e', 'process.exit(7)']).status, 7)
  assert.equal(runCli(['beat', path]).status, 2, 'no `--` separator is a wiring error')
  assert.equal(runCli(['beat', path, '--interval', 'abc', '--', 'true']).status, 2)
})

test('CLI heartbeat + liveness expose the same oracle to shell callers', () => {
  const path = heartbeatPath()
  assert.equal(runCli(['heartbeat', path, 'make test']).status, 0)
  const live = JSON.parse(runCli(['liveness', path]).stdout)
  assert.equal(live.status, HEARTBEAT_LIVE)
  assert.equal(live.source, 'recorded')

  const stale = JSON.parse(runCli(['liveness', path, String(Date.now() + 10_000), '1000']).stdout)
  assert.equal(stale.status, HEARTBEAT_STALE)

  const absent = JSON.parse(runCli(['liveness', join(tmpdir(), 'bda-hb-absent.json')]).stdout)
  assert.equal(absent.status, HEARTBEAT_ABSENT)

  // A non-numeric clock FAILS rather than defaulting — NaN loses every comparison
  // and would report a dead dispatch as live.
  assert.equal(runCli(['liveness', path, '3O0']).status, 2)
  assert.equal(runCli(['heartbeat']).status, 2)
})

test('CLI `classify` and `disposition` accept the heartbeat and route from it', () => {
  // A leftover sentinel from a prior run: `stale`, so both verbs fall through to
  // the age path this heartbeat governs rather than short-circuiting on a seed.
  const seeded = agedSeed(DEFAULT_OPEN_DISPATCH_STALE_MS + 60_000)
  const { dir } = seeded
  const runId = `${seeded.runId}-OTHER`
  const now = Date.now()
  const path = heartbeatPath()
  touchHeartbeat(path, { now })

  const classified = JSON.parse(
    runCli(['classify', dir, runId, 'draft', String(now + 60_000), String(now), path]).stdout,
  )
  assert.equal(classified.status, STILL_RUNNING)

  // Without the heartbeat the same state discards; with a live one it waits.
  const blind = JSON.parse(
    runCli(['disposition', dir, runId, 'draft', '--now', String(now)]).stdout,
  )
  assert.equal(blind.disposition, 'discard')
  assert.equal(blind.reason, 'abandoned')
  const seeing = JSON.parse(
    runCli([
      'disposition',
      dir,
      runId,
      'draft',
      '--now',
      String(now),
      '--deadline',
      String(now + 60_000),
      '--heartbeat',
      path,
    ]).stdout,
  )
  assert.equal(seeing.disposition, 'wait')
  assert.equal(seeing.reason, 'still-running')
  // `ageMs` here is the BEAT's age, so the recovery caller's own question — how long has this
  // dispatch been open — has to survive under its own name rather than be inferred from it.
  assert.ok(seeing.ageMs < 1_000)
  assert.ok(seeing.seedAgeMs >= DEFAULT_OPEN_DISPATCH_STALE_MS)
})

test('every verb that reads a number from argv rejects a non-numeric one', () => {
  // The rule is one helper, not three restatements: `Number('3O0')` is NaN, NaN survives `??`, and
  // every NaN comparison is false — so an unrejected typo falls past both death tests and reports a
  // dead dispatch as `still-running`, the one answer this helper exists to never give. `classify` is
  // the verb skills invoke from a bash block, so it is the one that must not be the exception.
  const ctx = context()
  const now = String(Date.now())
  const cases = [
    ['classify', [ctx.dir, ctx.runId, 'draft', '3O0']],
    ['classify', [ctx.dir, ctx.runId, 'draft', now, '3O0']],
    ['disposition', [ctx.dir, ctx.runId, 'draft', '--deadline', '3O0']],
    ['disposition', [ctx.dir, ctx.runId, 'draft', '--now', '3O0']],
    ['liveness', [join(tmpdir(), 'bda-hb-none.json'), '3O0']],
  ]
  for (const [verb, args] of cases) {
    const typo = runCli([verb, ...args])
    assert.equal(typo.status, 2, `${verb} ${args.join(' ')} must fail, never default`)
    assert.match(typo.stderr, /requires a finite number/)
    assert.equal(typo.stdout, '', 'a rejected verb must emit no verdict at all')
  }
})

// ---------------------------------------------------------------------------
// Completion — artifact readiness and returned-message readiness are separately
// decidable, and no single token stands for both.
// ---------------------------------------------------------------------------

test('every classification states the scope it settles, and that scope is the artifact', () => {
  const ctx = context()
  writeSentinel(ctx, 'leg-a', 'clean', { provisional: false })
  assert.equal(classifyDispatch(ctx, 'leg-a', { now: 1, deadlineAt: 2 }).settles, 'artifact')
  const open = context()
  assert.equal(classifyDispatch(open, 'leg-a', { now: 1, deadlineAt: 2 }).settles, 'artifact')
})

test('messageReadiness settles ARRIVAL only, and absence is never arrival', () => {
  for (const missing of [undefined, null, '', '   ']) {
    assert.equal(
      messageReadiness(missing).status,
      MESSAGE_NOT_RETURNED,
      `${JSON.stringify(missing)}`,
    )
  }
  for (const arrived of [{}, { planPath: 'p.md' }, 'ok', 0, false]) {
    assert.equal(messageReadiness(arrived).status, MESSAGE_RETURNED, `${JSON.stringify(arrived)}`)
  }
})

test('an artifact that landed while the message has not is NEVER one "done" verdict', () => {
  const ctx = context()
  writeSentinel(ctx, 'draft', 'ok', { provisional: false, planPath: 'plan.md' })
  const classified = classifyDispatch(ctx, 'draft', { now: 1_000, deadlineAt: 2_000 })
  assert.equal(classified.status, COMPLETED, 'the artifact verdict is unchanged')

  const landedOnly = dispatchReadiness(classified, undefined)
  assert.equal(landedOnly.artifactReady, true)
  assert.equal(landedOnly.messageReady, false)
  assert.equal(landedOnly.done, false, 'COMPLETED alone must never read as finished')
  assert.equal(landedOnly.reason, 'message-no-message')

  // Able to fire: the SAME artifact verdict with the message in hand IS done, so
  // the refusal above is the missing message's doing and not a blanket false.
  const both = dispatchReadiness(classified, { planPath: 'plan.md' })
  assert.equal(both.done, true)
  assert.equal(both.reason, 'artifact-and-message')
})

test('a returned message never promotes a dispatch whose artifact has not landed', () => {
  const ctx = context()
  const open = classifyDispatch(ctx, 'draft', { now: 1_000, deadlineAt: 2_000 })
  const readiness = dispatchReadiness(open, { planPath: 'plan.md' })
  assert.equal(readiness.messageReady, true)
  assert.equal(readiness.artifactReady, false)
  assert.equal(readiness.done, false)
  assert.equal(readiness.reason, `artifact-${STILL_RUNNING}`)
})

test('scoping COMPLETED left every existing consumer answering exactly as before', () => {
  // `toSentinelRouting` and `dispatchDisposition` read the artifact verdict, which
  // is the one they were always asking for. Adding the second question must not
  // have moved either answer.
  const ctx = context()
  writeSentinel(ctx, 'review', 'clean', { provisional: false })
  const classified = classifyDispatch(ctx, 'review', { now: 1_000, deadlineAt: 2_000 })
  assert.equal(toSentinelRouting(classified), 'clean')
  assert.equal(
    dispatchDisposition(ctx, 'review', { now: 1_000, deadlineAt: 2_000 }).publishable,
    true,
  )
  assert.equal(dispatchReadiness(classified, undefined).done, false)
})

test('BOS-1278: CLI `open` rejects a non-finite nowMs rather than reporting every dispatch fresh', () => {
  const ctx = context()
  writeSentinel(ctx, 'review', 'kind', { provisional: true })

  // Without the guard, `Number('3O0')` is NaN, every `now - mtime` is NaN, and `NaN >= stale` is
  // false — so a typo'd clock reported every open dispatch as not-stale. That is the same
  // silent-false-liveness class the `disposition` deadline guard already closes, so `open` is held
  // to the same rule rather than being the one numeric verb that defaults.
  const typo = runCli(['open', ctx.dir, ctx.runId, '3O0'])
  assert.equal(typo.status, 2, 'a non-numeric nowMs must fail, never default')
  assert.match(typo.stderr, /requires a finite number/)

  const ok = runCli(['open', ctx.dir, ctx.runId, `${Date.now()}`])
  assert.equal(ok.status, 0)
  assert.equal(JSON.parse(ok.stdout)[0].name, 'review')
})

test('BOS-1278: openDispatches shares `seedAgeMs` with classifyDispatch, and `ageMs` is not shared', () => {
  const ctx = context()
  writeSentinel(ctx, 'review', 'kind', { provisional: true })
  const now = Date.now()

  // `openDispatches` consults no heartbeat, so its open-age and its liveness-age are the same
  // number — it publishes `seedAgeMs` so the shared quantity has ONE name across both verbs.
  const [opened] = openDispatches(ctx, { now })
  assert.equal(typeof opened.seedAgeMs, 'number')
  assert.equal(opened.seedAgeMs, opened.ageMs, 'no heartbeat here, so the two ages coincide')

  // In `classifyDispatch` they do NOT coincide once a heartbeat is consulted: `ageMs` becomes the
  // beat's age while `seedAgeMs` stays the open-age. Reconciling on `ageMs` would compare a
  // liveness age against a seed age, which is what the old comment wrongly licensed.
  const hb = join(ctx.dir, 'hb.json')
  touchHeartbeat(hb, { now: now - 1_000 })
  const classified = classifyDispatch(ctx, 'review', {
    now,
    openedAt: now - 10 * 60 * 1000,
    deadlineAt: now + 60_000,
    heartbeatPath: hb,
  })
  assert.equal(classified.seedAgeMs, 10 * 60 * 1000)
  assert.equal(classified.ageMs, 1_000, '`ageMs` follows the beat once one is consulted')
  assert.notEqual(classified.ageMs, classified.seedAgeMs)
})
