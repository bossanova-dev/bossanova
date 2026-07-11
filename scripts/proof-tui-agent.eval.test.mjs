/**
 * proof-tui-agent.eval.test.mjs — Unit tests for proof-tui-agent.eval.mjs harness logic.
 *
 * Strategy: drive runEval entirely via injected stubs so the suite passes with
 * NO ANTHROPIC_API_KEY, NO node_modules, and NO network access. The `runner` stub
 * returns verdicts by call order (call 1 = positive scenario, call 2 = negative,
 * call 3 = multi-scene ordering (D9)); the fourth scenario (BOS-223 fallback) is
 * routed through the separate injectable `dispatch` seam, NOT the runner — so
 * `runner` is still called exactly three times.
 *
 * Scenario call order (pinned against proof-tui-agent.eval.mjs SCENARIOS):
 *   1 positive (#812) → runner   2 negative → runner   3 multi-scene (D9) → runner
 *   4 fallback (agent-fail → replay, BOS-223) → dispatch
 *
 * Tests:
 *   1. skip: no key → {skipped:true, ok:true}; runner + buildBridge never called
 *   2. all-pass: fake key; runner returns 'passed'/'failed'/'passed' and dispatch
 *      returns a passing replay → ok===true, all FOUR scenarios recorded
 *   3. regression caught: runner returns 'passed' for negative → ok===false
 *   4. positive-fails: runner returns 'failed' for positive → ok===false
 *   5. summary log lines emitted for each scenario
 *   6. sceneTimingsStrictlyIncreasing / allScenesPassed: pure helpers, fixture inputs
 *   7. brief wiring: each runner scenario gets dryRun:true + its matching (matcher-
 *      shaped) brief; the fallback scenario reaches dispatch with its committed
 *      scenario file + sentinel brief
 *   8. multi-scene D9 scene-order gate via injected runContext.localDir seam
 *   9. fallback (BOS-223): dispatch proofSource='replay' + disclosure + strictly-
 *      increasing timings, one entry per committed scene → PASS; a non-fallback
 *      proofSource='agent', a missing disclosure line, non-increasing timings, or a
 *      timing count below the committed scene count (partial/empty artifact) → FAIL
 */

import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { test } from 'node:test'

import { renderConsolidatedComment } from './proof-lib.mjs'
import {
  allScenesPassed,
  committedScenarioSceneCount,
  FALLBACK_DISCLOSURE_SUBSTRING,
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

/**
 * Dispatch stub for the BOS-223 fallback scenario. Mimics the real dispatcher:
 * writes raw/scene-timings.json into the run dir the eval supplied (localDir seam)
 * and returns a classified `{proofSource, fallbackReason, commentBody}`. Defaults
 * model a clean replay fallback (proofSource='replay', strictly-increasing timings,
 * disclosure line present). Override any field to model a failing shape.
 */
function makeDispatchStub({
  proofSource = 'replay',
  disclosure = true,
  timings = [
    { id: 'scene-01', title: 'open repos list', startMs: 0 },
    { id: 'scene-02', title: 'open repo settings', startMs: 1800 },
  ],
} = {}) {
  return async ({ localDir }) => {
    if (timings) {
      const rawDir = path.join(localDir, 'raw')
      fs.mkdirSync(rawDir, { recursive: true })
      fs.writeFileSync(path.join(rawDir, 'scene-timings.json'), JSON.stringify(timings, null, 2))
    }
    // Render the disclosure line through the real renderer so the substring the
    // eval gates on is produced exactly as a posted comment would emit it.
    const commentBody = disclosure
      ? renderConsolidatedComment({
          marker: '<!-- proof -->',
          manifest: { title: 'fallback', commit: 'eval', runId: 'eval' },
          sections: [
            {
              outcome: 'passed',
              label: 'TUI',
              proofSource: 'replay',
              fallbackReason: 'agent-failed',
              summary: '',
              captures: [],
            },
          ],
        })
      : '#### TUI — ✅ proven\n'
    return {
      proofSource,
      fallbackReason: proofSource === 'replay' ? 'agent-failed' : null,
      commentBody,
    }
  }
}

const FAKE_BIN = '/tmp/fake-proof-tui-bridge'
const buildBridgeStub = async () => FAKE_BIN

// ── 1. skip: no ANTHROPIC_API_KEY ─────────────────────────────────────────────

test('runEval: no ANTHROPIC_API_KEY → skipped=true, ok=true, runner never called', async () => {
  let runnerCalled = false
  let buildBridgeCalled = false
  let dispatchCalled = false

  const result = await runEval({
    env: {},
    runner: async () => {
      runnerCalled = true
      return { manifest: { verdict: 'passed' } }
    },
    dispatch: async () => {
      dispatchCalled = true
      return { proofSource: 'replay', commentBody: '' }
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
  assert.equal(dispatchCalled, false, 'dispatch must not be called when key is absent')
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

// ── 2. all-pass: positive='passed', negative='failed', multi-scene='passed', fallback='replay' ──

test('runEval: all-pass → ok===true, results contain all four scenarios', async () => {
  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    dispatch: makeDispatchStub(),
    buildBridge: buildBridgeStub,
    log: () => {},
  })

  assert.equal(result.ok, true, 'ok must be true when all scenarios meet expectation')
  assert.ok(Array.isArray(result.results), 'results must be an array')
  assert.equal(result.results.length, 4, 'must have exactly 4 scenario results')
})

test('runEval: all-pass → positive scenario ok===true with verdict=passed', async () => {
  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    dispatch: makeDispatchStub(),
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
    dispatch: makeDispatchStub(),
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
    dispatch: makeDispatchStub(),
    buildBridge: buildBridgeStub,
    log: () => {},
  })

  const multi = result.results[2]
  assert.ok(multi, 'third result (multi-scene) must exist')
  assert.equal(multi.expectedVerdict, 'passed')
  assert.equal(multi.verdict, 'passed')
  assert.equal(multi.ok, true)
})

test('runEval: all-pass → fallback scenario ok===true with proofSource=replay', async () => {
  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    dispatch: makeDispatchStub(),
    buildBridge: buildBridgeStub,
    log: () => {},
  })

  const fallback = result.results[3]
  assert.ok(fallback, 'fourth result (fallback) must exist')
  assert.equal(fallback.expectedProofSource, 'replay')
  assert.equal(fallback.proofSource, 'replay')
  assert.equal(fallback.ok, true)
})

// ── 3. regression caught: negative returns 'passed' → false-positive detected ─

test('runEval: runner returns passed for negative scenario → ok===false', async () => {
  // Both return 'passed': positive is correct, but negative expects 'failed' → caught.
  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeFixedRunnerStub('passed'),
    dispatch: makeDispatchStub(),
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
    dispatch: makeDispatchStub(),
    buildBridge: buildBridgeStub,
    log: () => {},
  })

  assert.equal(result.ok, false, 'ok must be false when positive scenario returns failed')
  const pos = result.results[0]
  assert.equal(pos.ok, false, 'positive scenario must be marked as failed')
})

// ── 5. summary log lines ───────────────────────────────────────────────────────

test('runEval: log lines include all scenario names and a summary', async () => {
  const log = makeLog()

  await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    dispatch: makeDispatchStub(),
    buildBridge: buildBridgeStub,
    log,
  })

  const joined = log.lines.join('\n')
  assert.ok(/positive/.test(joined), 'log must mention the positive scenario')
  assert.ok(/negative/.test(joined), 'log must mention the negative scenario')
  assert.ok(/D9/.test(joined), 'log must mention the multi-scene (D9) scenario')
  assert.ok(/fallback/.test(joined), 'log must mention the fallback scenario')
  assert.ok(/summary/i.test(joined), 'log must include a summary line')
})

test('runEval: log lines include PASS/FAIL status labels', async () => {
  const log = makeLog()

  await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    dispatch: makeDispatchStub(),
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
    dispatch: makeDispatchStub(),
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
    dispatch: makeDispatchStub(),
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

// ── 7. brief wiring: runner scenarios + fallback dispatch ────────────────────

test('runEval: each runner scenario gets dryRun:true + its matching matcher-shaped brief', async () => {
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

  // Capture the fallback scenario's dispatch wiring: it must reach `dispatch`
  // (NOT the runner) with the committed scenario file in changedFiles and the
  // sentinel brief.
  let dispatchCall = null
  const dispatch = async (opts) => {
    dispatchCall = {
      changedFiles: opts.changedFiles,
      scenarioFile: opts.scenarioFile,
      briefTitle: opts.brief.title,
      evidence: opts.brief.expectedEvidence,
    }
    const rawDir = path.join(opts.localDir, 'raw')
    fs.mkdirSync(rawDir, { recursive: true })
    fs.writeFileSync(
      path.join(rawDir, 'scene-timings.json'),
      JSON.stringify([{ startMs: 0 }, { startMs: 100 }]),
    )
    return {
      proofSource: 'replay',
      commentBody: `x _${FALLBACK_DISCLOSURE_SUBSTRING}_ x`,
    }
  }

  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner,
    dispatch,
    buildBridge: buildBridgeStub,
    log: () => {},
  })

  assert.equal(result.ok, true)
  assert.equal(calls.length, 3, 'exactly the three navigation scenarios invoke the runner')
  assert.ok(
    calls.every((c) => c.dryRun === true),
    'every runner call must pass dryRun:true (no upload/post)',
  )
  // Call 1 = positive (#812, reachable evidence); call 2 = negative (unreachable sentinel);
  // call 3 = multi-scene ordering (D9, two scenes, matcher-shaped evidence).
  assert.match(calls[0].title, /#812/)
  assert.ok(!calls[0].evidence.includes('__UNREACHABLE_EVIDENCE_BOS71__'))
  assert.deepEqual(calls[1].evidence, ['__UNREACHABLE_EVIDENCE_BOS71__'])
  assert.match(calls[2].title, /D9/)
  assert.equal(calls[2].scenes.length, 2)
  assert.equal(calls[2].scenes[0].id, 'scene-01')
  assert.equal(calls[2].scenes[1].id, 'scene-02')
  // BOS-222 matcher shapes flow through verbatim: scene-01 carries the plain
  // 'PATH' token AND the normalized-vs-literal matcher object; scene-02 carries
  // the anyOf group anchored to the settings screen.
  assert.ok(
    calls[2].scenes[0].expectedEvidence.includes('PATH'),
    'scene-01 keeps the grounded PATH token',
  )
  const normalizedCase = calls[2].scenes[0].expectedEvidence.find(
    (e) => e && typeof e === 'object' && e.match === 'normalized',
  )
  assert.ok(normalizedCase, 'scene-01 carries a normalized matcher object')
  assert.equal(normalizedCase.text, 'NAME PATH STATUS')
  const anyOfCase = calls[2].scenes[1].expectedEvidence.find(
    (e) => e && typeof e === 'object' && Array.isArray(e.anyOf),
  )
  assert.ok(anyOfCase, 'scene-02 carries an anyOf matcher object')
  // BOS-213 key vocabulary: scene-01 must still drive the repo table with the new
  // arrow keys. Without this, a regression that dropped the `down`/`up` stepsHints
  // from the eval brief would leave every other assertion (and verdict) green while
  // silently violating the acceptance criterion's "≥1 flow using a BOS-213 key".
  const arrowHints = calls[2].scenes[0].stepsHints.filter((h) => /\b(down|up)\b/i.test(h))
  assert.ok(
    arrowHints.length >= 2,
    `scene-01 must drive the repo table with BOS-213 down/up arrow keys; got: ${JSON.stringify(
      calls[2].scenes[0].stepsHints,
    )}`,
  )

  // Fallback scenario reached dispatch (not the runner) with its committed
  // scenario file + sentinel brief.
  assert.ok(dispatchCall, 'the fallback scenario must invoke dispatch')
  assert.deepEqual(dispatchCall.changedFiles, [
    'proof/scenarios/bos224-tui-navigation-fallback.scenario.json',
  ])
  assert.equal(
    dispatchCall.scenarioFile,
    'proof/scenarios/bos224-tui-navigation-fallback.scenario.json',
  )
  assert.deepEqual(dispatchCall.evidence, ['__UNREACHABLE_EVIDENCE_BOS224__'])
})

// ── 8. sceneTimingsStrictlyIncreasing / allScenesPassed (pure helpers) ────────

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

// ── 9. D9 scene-order gate: runEval reads raw/scene-timings.json + manifest.captures ──

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
    dispatch: makeDispatchStub(),
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
    dispatch: makeDispatchStub(),
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
    dispatch: makeDispatchStub(),
    buildBridge: buildBridgeStub,
    log: () => {},
  })

  assert.equal(result.ok, false, 'overall eval must fail when any scene missed its evidence gate')
  assert.equal(result.results[2].ok, false, 'multi-scene scenario must fail')
})

// ── 10. Fallback path (BOS-223): dispatch → replay classification gate ─────────

test('fallback: real renderer emits the pinned disclosure substring for a replay section', () => {
  // Anti-drift: FALLBACK_DISCLOSURE_SUBSTRING must be a substring of what the real
  // proof-lib renderer emits for a proofSource='replay' section, so the eval's gate
  // and the comment renderer can never silently diverge.
  const body = renderConsolidatedComment({
    marker: '<!-- proof -->',
    manifest: { title: 't', commit: 'c', runId: 'r' },
    sections: [
      {
        outcome: 'passed',
        label: 'TUI',
        proofSource: 'replay',
        fallbackReason: 'agent-failed',
        summary: '',
        captures: [],
      },
    ],
  })
  assert.ok(
    body.includes(FALLBACK_DISCLOSURE_SUBSTRING),
    `renderer body must contain the pinned disclosure substring; got: ${body}`,
  )
})

test('runEval: fallback passes when dispatch returns replay + disclosure + increasing timings', async () => {
  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    dispatch: makeDispatchStub(),
    buildBridge: buildBridgeStub,
    log: () => {},
  })

  assert.equal(result.ok, true)
  assert.equal(result.results[3].ok, true, 'fallback scenario must pass')
  assert.equal(result.results[3].proofSource, 'replay')
})

test('runEval: fallback FAILS when dispatch returns proofSource=agent (no fallback engaged)', async () => {
  // Regression guard: a non-fallback result (the agent leg "succeeded", so no
  // replay) must FAIL the fallback scenario — the whole point is to prove the
  // automatic replay fallback engaged, not the happy agent path.
  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    dispatch: makeDispatchStub({ proofSource: 'agent', disclosure: false }),
    buildBridge: buildBridgeStub,
    log: () => {},
  })

  assert.equal(result.ok, false, 'overall eval must fail when the fallback did not engage')
  assert.equal(result.results[3].ok, false, 'fallback scenario must fail on proofSource=agent')
  assert.equal(result.results[3].proofSource, 'agent')
})

test('runEval: fallback FAILS when the disclosure line is missing from the comment body', async () => {
  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    dispatch: makeDispatchStub({ proofSource: 'replay', disclosure: false }),
    buildBridge: buildBridgeStub,
    log: () => {},
  })

  assert.equal(result.ok, false, 'overall eval must fail when the disclosure line is absent')
  assert.equal(
    result.results[3].ok,
    false,
    'fallback scenario must fail without the disclosure line',
  )
})

test('runEval: fallback FAILS when the replay scene-timings are not strictly increasing', async () => {
  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    dispatch: makeDispatchStub({
      timings: [
        { id: 'scene-01', startMs: 0 },
        { id: 'scene-02', startMs: 0 },
      ],
    }),
    buildBridge: buildBridgeStub,
    log: () => {},
  })

  assert.equal(result.ok, false, 'overall eval must fail when replay timings are not increasing')
  assert.equal(
    result.results[3].ok,
    false,
    'fallback scenario must fail the timings ordering check',
  )
})

test('runEval: fallback FAILS when replay emits fewer scene-timings than the committed scenario', async () => {
  // Regression guard: the committed fallback scenario has two scenes, so a replay
  // that only records scene-01 (e.g. scene-02 stopped emitting a timing) is still
  // strictly increasing and carries proofSource='replay' + the disclosure line —
  // but it no longer proves ordered replay timings for the FULL scenario, so the
  // count gate must reject it. A partial-but-increasing artifact used to pass under
  // the old `length >= 1` check.
  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    dispatch: makeDispatchStub({
      timings: [{ id: 'scene-01', title: 'open repos list', startMs: 0 }],
    }),
    buildBridge: buildBridgeStub,
    log: () => {},
  })

  assert.equal(result.ok, false, 'overall eval must fail on an incomplete scene-timings artifact')
  assert.equal(
    result.results[3].ok,
    false,
    'fallback scenario must fail when the timing count is below the committed scene count',
  )
})

test('runEval: fallback FAILS when the replay scene-timings.json is empty', async () => {
  // The empty array passes sceneTimingsStrictlyIncreasing([]) vacuously; the count
  // gate is what stops a zero-marker artifact from proving anything.
  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    dispatch: makeDispatchStub({ timings: [] }),
    buildBridge: buildBridgeStub,
    log: () => {},
  })

  assert.equal(result.ok, false, 'overall eval must fail on an empty scene-timings artifact')
  assert.equal(result.results[3].ok, false, 'fallback scenario must fail on zero scene markers')
})

// ── committedScenarioSceneCount: reads scenes.length from the committed JSON ──

test('committedScenarioSceneCount: reads the committed fallback scenario scene count (2)', () => {
  // The gate derives its expected length from the real committed scenario file, so
  // this pins the count the fallback replay must match. Update alongside the file.
  assert.equal(
    committedScenarioSceneCount('proof/scenarios/bos224-tui-navigation-fallback.scenario.json'),
    2,
  )
})

test('committedScenarioSceneCount: unreadable/missing scenario file → 0 (fails closed)', () => {
  assert.equal(committedScenarioSceneCount('proof/scenarios/does-not-exist.scenario.json'), 0)
})
