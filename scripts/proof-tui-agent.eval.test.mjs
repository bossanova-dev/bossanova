/**
 * proof-tui-agent.eval.test.mjs — Unit tests for proof-tui-agent.eval.mjs harness logic.
 *
 * Strategy: drive runEval entirely via injected stubs so the suite passes with
 * NO ANTHROPIC_API_KEY, NO node_modules, and NO network access. The runner stub
 * returns verdicts by call order (call 1 = positive scenario, call 2 = negative,
 * call 3 = multi-scene ordering (D9)).
 *
 * Tests:
 *   1. skip: no key → {skipped:true, ok:true}; runner + buildBridge never called
 *   2. all-pass: fake key; runner returns 'passed' (positive), 'failed' (negative),
 *      'passed' (multi-scene) → ok===true, all three scenarios recorded with
 *      matching expected verdicts
 *   3. regression caught: runner returns 'passed' for negative → ok===false
 *   4. positive-fails: runner returns 'failed' for positive → ok===false
 *   5. summary log lines emitted for each scenario
 *   6. sceneTimingsStrictlyIncreasing / allScenesPassed: pure helpers, fixture inputs
 *   7. multi-scene scenario: runEval reads raw/scene-timings.json + manifest.captures
 *      via the injected runContext.localDir seam and enforces the D9 scene-order gate
 */

import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { test } from 'node:test'

import {
  allScenesPassed,
  runEval,
  sceneTimingsStrictlyIncreasing,
} from './proof-tui-agent.eval.mjs'

// ── Helpers ───────────────────────────────────────────────────────────────────

/** Builds a log-capturing array with a push-compatible interface. */
function makeLog() {
  const lines = []
  const log = (msg) => lines.push(String(msg))
  log.lines = lines
  return log
}

/**
 * Runner stub that returns 'passed' for call 1 (positive), 'failed' for call 2
 * (negative), and 'passed' for call 3 (multi-scene ordering, D9). Never
 * populates `manifest.captures`, so the D9 scene-order gate stays a no-op for
 * generic harness tests that don't care about it — only verdict match governs `ok`.
 */
function makeDefaultRunnerStub() {
  let calls = 0
  return async () => {
    calls += 1
    const verdict = calls === 2 ? 'failed' : 'passed'
    return { manifest: { verdict } }
  }
}

/** Runner stub that always returns the given verdict regardless of call order. */
function makeFixedRunnerStub(verdict) {
  return async () => ({ manifest: { verdict } })
}

const FAKE_BIN = '/tmp/fake-proof-tui-bridge'
const buildBridgeStub = async () => FAKE_BIN

// ── 1. skip: no ANTHROPIC_API_KEY ─────────────────────────────────────────────

test('runEval: no ANTHROPIC_API_KEY → skipped=true, ok=true, runner never called', async () => {
  let runnerCalled = false
  let buildBridgeCalled = false

  const result = await runEval({
    env: {},
    runner: async () => {
      runnerCalled = true
      return { manifest: { verdict: 'passed' } }
    },
    buildBridge: async () => {
      buildBridgeCalled = true
      return FAKE_BIN
    },
    log: () => {},
  })

  assert.equal(result.skipped, true)
  assert.equal(result.ok, true)
  assert.equal(runnerCalled, false, 'runner must not be called when key is absent')
  assert.equal(buildBridgeCalled, false, 'buildBridge must not be called when key is absent')
})

test('runEval: no key → log line matches /skipped: no ANTHROPIC_API_KEY/', async () => {
  const log = makeLog()

  await runEval({ env: {}, runner: async () => {}, buildBridge: async () => FAKE_BIN, log })

  assert.ok(
    log.lines.some((l) => /skipped: no ANTHROPIC_API_KEY/.test(l)),
    `Expected a line matching /skipped: no ANTHROPIC_API_KEY/ in: ${JSON.stringify(log.lines)}`,
  )
})

// ── 2. all-pass: positive='passed', negative='failed', multi-scene='passed' ───

test('runEval: all-pass → ok===true, results contain all three scenarios', async () => {
  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    buildBridge: buildBridgeStub,
    log: () => {},
  })

  assert.equal(result.ok, true, 'ok must be true when all scenarios meet expectation')
  assert.ok(Array.isArray(result.results), 'results must be an array')
  assert.equal(result.results.length, 3, 'must have exactly 3 scenario results')
})

test('runEval: all-pass → positive scenario ok===true with verdict=passed', async () => {
  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    buildBridge: buildBridgeStub,
    log: () => {},
  })

  const pos = result.results[0]
  assert.ok(pos, 'first result (positive) must exist')
  assert.equal(pos.expectedVerdict, 'passed')
  assert.equal(pos.verdict, 'passed')
  assert.equal(pos.ok, true)
})

test('runEval: all-pass → negative scenario ok===true with verdict=failed', async () => {
  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    buildBridge: buildBridgeStub,
    log: () => {},
  })

  const neg = result.results[1]
  assert.ok(neg, 'second result (negative) must exist')
  assert.equal(neg.expectedVerdict, 'failed')
  assert.equal(neg.verdict, 'failed')
  assert.equal(neg.ok, true)
})

test('runEval: all-pass → multi-scene (D9) scenario ok===true with verdict=passed', async () => {
  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    buildBridge: buildBridgeStub,
    log: () => {},
  })

  const multi = result.results[2]
  assert.ok(multi, 'third result (multi-scene) must exist')
  assert.equal(multi.expectedVerdict, 'passed')
  assert.equal(multi.verdict, 'passed')
  assert.equal(multi.ok, true)
})

// ── 3. regression caught: negative returns 'passed' → false-positive detected ─

test('runEval: runner returns passed for negative scenario → ok===false', async () => {
  // Both return 'passed': positive is correct, but negative expects 'failed' → caught.
  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeFixedRunnerStub('passed'),
    buildBridge: buildBridgeStub,
    log: () => {},
  })

  assert.equal(result.ok, false, 'ok must be false when negative scenario returns passed')
  const neg = result.results[1]
  assert.equal(neg.ok, false, 'negative scenario must be marked as failed')
})

// ── 4. positive-fails: positive returns 'failed' → ok===false ─────────────────

test('runEval: runner returns failed for positive scenario → ok===false', async () => {
  // Call 1 returns 'failed' (positive), call 2 returns 'failed' (negative).
  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeFixedRunnerStub('failed'),
    buildBridge: buildBridgeStub,
    log: () => {},
  })

  assert.equal(result.ok, false, 'ok must be false when positive scenario returns failed')
  const pos = result.results[0]
  assert.equal(pos.ok, false, 'positive scenario must be marked as failed')
})

// ── 5. summary log lines ───────────────────────────────────────────────────────

test('runEval: log lines include both scenario names and a summary', async () => {
  const log = makeLog()

  await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    buildBridge: buildBridgeStub,
    log,
  })

  const joined = log.lines.join('\n')
  assert.ok(/positive/.test(joined), 'log must mention the positive scenario')
  assert.ok(/negative/.test(joined), 'log must mention the negative scenario')
  assert.ok(/D9/.test(joined), 'log must mention the multi-scene (D9) scenario')
  assert.ok(/summary/i.test(joined), 'log must include a summary line')
})

test('runEval: log lines include PASS/FAIL status labels', async () => {
  const log = makeLog()

  await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    buildBridge: buildBridgeStub,
    log,
  })

  assert.ok(
    log.lines.some((l) => /PASS/.test(l)),
    'at least one PASS label in log',
  )
})

// ── 6. buildBridge called when no BOSS_PROOF_TUI_BRIDGE_BIN in env ────────────

test('runEval: buildBridge called when BOSS_PROOF_TUI_BRIDGE_BIN absent from env', async () => {
  let bridgeCalled = false

  await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    buildBridge: async () => {
      bridgeCalled = true
      return FAKE_BIN
    },
    log: () => {},
  })

  assert.equal(bridgeCalled, true, 'buildBridge must be called when no bridge bin in env')
})

test('runEval: buildBridge NOT called when BOSS_PROOF_TUI_BRIDGE_BIN already in env', async () => {
  let bridgeCalled = false

  await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key', BOSS_PROOF_TUI_BRIDGE_BIN: '/existing/bin' },
    runner: makeDefaultRunnerStub(),
    buildBridge: async () => {
      bridgeCalled = true
      return FAKE_BIN
    },
    log: () => {},
  })

  assert.equal(bridgeCalled, false, 'buildBridge must NOT be called when bridge bin already set')
})

test('runEval: buildBridge throw rejects and restores BOSS_PROOF_UPLOAD', async () => {
  const savedUpload = process.env.BOSS_PROOF_UPLOAD
  delete process.env.BOSS_PROOF_UPLOAD

  let runnerCalled = false
  await assert.rejects(
    runEval({
      env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
      runner: async () => {
        runnerCalled = true
        return { manifest: { verdict: 'passed' } }
      },
      buildBridge: async () => {
        throw new Error('go build failed')
      },
      log: () => {},
    }),
    /go build failed/,
  )

  assert.equal(runnerCalled, false, 'runner must not run when the bridge build fails')
  assert.equal(
    process.env.BOSS_PROOF_UPLOAD,
    undefined,
    'BOSS_PROOF_UPLOAD must be restored (deleted) after a bridge-build failure',
  )

  if (savedUpload !== undefined) process.env.BOSS_PROOF_UPLOAD = savedUpload
})

test('runEval: each scenario invokes the runner with dryRun:true and its matching brief', async () => {
  // Capture, per runner call, the dryRun flag and the brief the eval pointed
  // BOSS_PROOF_BRIEF at — this is how the eval communicates the scenario brief
  // to runTuiAgentProof. Guards against a regression that drops dryRun or wires
  // the wrong brief to a scenario (both would otherwise leave verdicts green).
  const calls = []
  const runner = async (opts) => {
    const briefPath = process.env.BOSS_PROOF_BRIEF
    const brief = JSON.parse(fs.readFileSync(briefPath, 'utf8'))
    calls.push({
      dryRun: opts.dryRun,
      title: brief.title,
      evidence: brief.expectedEvidence,
      scenes: brief.scenes,
    })
    // Call 1 (positive) → passed, call 2 (negative) → failed, call 3 (multi-scene) → passed.
    const verdict = calls.length === 2 ? 'failed' : 'passed'
    return { manifest: { verdict } }
  }

  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner,
    buildBridge: buildBridgeStub,
    log: () => {},
  })

  assert.equal(result.ok, true)
  assert.equal(calls.length, 3, 'all three scenarios must invoke the runner')
  assert.ok(
    calls.every((c) => c.dryRun === true),
    'every runner call must pass dryRun:true (no upload/post)',
  )
  // Call 1 = positive (#812, reachable evidence); call 2 = negative (unreachable sentinel);
  // call 3 = multi-scene ordering (D9, two scenes).
  assert.match(calls[0].title, /#812/)
  assert.ok(!calls[0].evidence.includes('__UNREACHABLE_EVIDENCE_BOS71__'))
  assert.deepEqual(calls[1].evidence, ['__UNREACHABLE_EVIDENCE_BOS71__'])
  assert.match(calls[2].title, /D9/)
  assert.equal(calls[2].scenes.length, 2)
  assert.equal(calls[2].scenes[0].id, 'scene-01')
  assert.equal(calls[2].scenes[1].id, 'scene-02')
  assert.deepEqual(calls[2].scenes[1].expectedEvidence, ['Settings'])
})

// ── 7. sceneTimingsStrictlyIncreasing / allScenesPassed (pure helpers) ────────

test('sceneTimingsStrictlyIncreasing: strictly increasing startMs → true', () => {
  assert.equal(sceneTimingsStrictlyIncreasing([{ startMs: 0 }, { startMs: 1500 }]), true)
})

test('sceneTimingsStrictlyIncreasing: single entry → true (vacuous)', () => {
  assert.equal(sceneTimingsStrictlyIncreasing([{ startMs: 0 }]), true)
})

test('sceneTimingsStrictlyIncreasing: empty array → true (vacuous)', () => {
  assert.equal(sceneTimingsStrictlyIncreasing([]), true)
})

test('sceneTimingsStrictlyIncreasing: equal startMs → false', () => {
  assert.equal(sceneTimingsStrictlyIncreasing([{ startMs: 0 }, { startMs: 0 }]), false)
})

test('sceneTimingsStrictlyIncreasing: decreasing startMs → false', () => {
  assert.equal(sceneTimingsStrictlyIncreasing([{ startMs: 1500 }, { startMs: 200 }]), false)
})

test('sceneTimingsStrictlyIncreasing: one non-increasing pair among three → false', () => {
  assert.equal(
    sceneTimingsStrictlyIncreasing([{ startMs: 0 }, { startMs: 500 }, { startMs: 500 }]),
    false,
  )
})

test('sceneTimingsStrictlyIncreasing: non-array input fails closed', () => {
  assert.equal(sceneTimingsStrictlyIncreasing(null), false)
  assert.equal(sceneTimingsStrictlyIncreasing(undefined), false)
})

test('allScenesPassed: every scene passed → true', () => {
  assert.equal(
    allScenesPassed([
      { id: 'scene-01', passed: true },
      { id: 'scene-02', passed: true },
    ]),
    true,
  )
})

test('allScenesPassed: one scene failed → false', () => {
  assert.equal(
    allScenesPassed([
      { id: 'scene-01', passed: true },
      { id: 'scene-02', passed: false, missing: ['Settings'] },
    ]),
    false,
  )
})

test('allScenesPassed: empty array → false (no scenes is not "all passed")', () => {
  assert.equal(allScenesPassed([]), false)
})

test('allScenesPassed: non-array input fails closed', () => {
  assert.equal(allScenesPassed(null), false)
  assert.equal(allScenesPassed(undefined), false)
})

// ── 8. D9 scene-order gate: runEval reads raw/scene-timings.json + manifest.captures ──

/**
 * Runner stub for the D9 (multi-scene) integration tests: calls 1-2 (positive/
 * negative) return a bare verdict; call 3 (multi-scene) writes
 * raw/scene-timings.json into the `runContext.localDir` the eval supplied
 * (mimicking what the real runTuiAgentProof persists at Step 2) and returns a
 * manifest carrying `captures[0].scenes` with the given per-scene pass state.
 */
function makeSceneAwareRunnerStub({ timings, scenesPassed }) {
  let calls = 0
  return async (opts) => {
    calls += 1
    if (calls < 3) {
      return { manifest: { verdict: calls === 2 ? 'failed' : 'passed' } }
    }
    const rawDir = path.join(opts.runContext.localDir, 'raw')
    fs.mkdirSync(rawDir, { recursive: true })
    fs.writeFileSync(path.join(rawDir, 'scene-timings.json'), JSON.stringify(timings, null, 2))
    return {
      manifest: {
        verdict: 'passed',
        captures: [
          {
            scenes: [
              { id: 'scene-01', title: 'open repos list', passed: scenesPassed[0], missing: [] },
              { id: 'scene-02', title: 'open repo settings', passed: scenesPassed[1], missing: [] },
            ],
          },
        ],
      },
    }
  }
}

test('runEval: D9 multi-scene scenario passes when timings increase and both scenes passed', async () => {
  const runner = makeSceneAwareRunnerStub({
    timings: [
      { id: 'scene-01', title: 'open repos list', startMs: 0 },
      { id: 'scene-02', title: 'open repo settings', startMs: 1800 },
    ],
    scenesPassed: [true, true],
  })

  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner,
    buildBridge: buildBridgeStub,
    log: () => {},
  })

  assert.equal(result.ok, true, 'overall eval must pass when the D9 gate is satisfied')
  assert.equal(result.results[2].ok, true, 'multi-scene scenario must pass')
})

test('runEval: D9 multi-scene scenario fails when scene-timings.json is not strictly increasing', async () => {
  const runner = makeSceneAwareRunnerStub({
    timings: [
      { id: 'scene-01', title: 'open repos list', startMs: 0 },
      { id: 'scene-02', title: 'open repo settings', startMs: 0 },
    ],
    scenesPassed: [true, true],
  })

  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner,
    buildBridge: buildBridgeStub,
    log: () => {},
  })

  assert.equal(
    result.ok,
    false,
    'overall eval must fail when scene ordering is not strictly increasing',
  )
  assert.equal(result.results[2].ok, false, 'multi-scene scenario must fail the ordering check')
})

test('runEval: D9 multi-scene scenario fails when a scene missed its evidence gate', async () => {
  const runner = makeSceneAwareRunnerStub({
    timings: [
      { id: 'scene-01', title: 'open repos list', startMs: 0 },
      { id: 'scene-02', title: 'open repo settings', startMs: 1800 },
    ],
    scenesPassed: [true, false],
  })

  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner,
    buildBridge: buildBridgeStub,
    log: () => {},
  })

  assert.equal(result.ok, false, 'overall eval must fail when any scene missed its evidence gate')
  assert.equal(result.results[2].ok, false, 'multi-scene scenario must fail')
})
