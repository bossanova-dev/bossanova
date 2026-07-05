/**
 * proof-judge.eval.test.mjs — Unit tests for proof-judge.eval.mjs harness logic.
 *
 * Strategy (mirrors proof-tui-agent.eval.test.mjs): drive `runEval` entirely
 * via an injected `runScenario` stub so the suite passes with NO
 * PROOF_ANTHROPIC_API_KEY, NO node_modules SDK, and NO network access. The
 * per-scenario `assert()` functions in SCENARIOS are pure and exercised
 * directly with synthetic verdicts. The one exception — API-error scenario
 * #5 — is run through the REAL `runScenario` (real `judgeProof`), but it
 * injects a fake env key + a throwing model, so `createJudgeModel()` is never
 * constructed and no network call ever happens.
 *
 * Tests:
 *   1. SCENARIOS shape: exactly 5, documented names/order, each has the
 *      required fields (buildInputs, assert, fixtureFiles).
 *   2. Each scenario's assert() logic, exercised with synthetic verdicts
 *      (pure, no judgeProof call).
 *   3. Key-gate skip: no PROOF_ANTHROPIC_API_KEY -> {skipped:true, ok:true},
 *      runScenario never called, skip log line matches.
 *   4. runEval aggregation: all-pass -> ok===true; one failure -> ok===false;
 *      PASS/FAIL labels + scenario names + summary line logged.
 *   5. Scenario 5 (API error) run end-to-end through the REAL runScenario
 *      with its injected stub deps -> {unjudged: true}, no key/network.
 */

import assert from 'node:assert/strict'
import { test } from 'node:test'

import { runEval, runScenario, SCENARIOS } from './proof-judge.eval.mjs'

// ── 1. SCENARIOS shape ───────────────────────────────────────────────────────

test('SCENARIOS: exactly 5 scenarios in the documented order', () => {
  assert.equal(SCENARIOS.length, 5)
  assert.deepEqual(
    SCENARIOS.map((s) => s.name),
    ['complete', 'missing scene', 'adversarial', 'stub without disclosure', 'API error'],
  )
})

test('SCENARIOS: every scenario has buildInputs, assert, and fixtureFiles', () => {
  for (const scenario of SCENARIOS) {
    assert.equal(typeof scenario.buildInputs, 'function', `${scenario.name} needs buildInputs`)
    assert.equal(typeof scenario.assert, 'function', `${scenario.name} needs assert`)
    assert.ok(Array.isArray(scenario.fixtureFiles), `${scenario.name} needs fixtureFiles array`)
  }
})

test('SCENARIOS: buildInputs() returns surfaceRuns + manifest for every scenario', () => {
  for (const scenario of SCENARIOS) {
    const inputs = scenario.buildInputs()
    assert.ok(Array.isArray(inputs.surfaceRuns), `${scenario.name} needs surfaceRuns array`)
    assert.ok(inputs.surfaceRuns.length > 0, `${scenario.name} needs a non-empty surfaceRuns`)
    assert.ok(
      inputs.manifest && typeof inputs.manifest === 'object',
      `${scenario.name} needs manifest`,
    )
  }
})

test('SCENARIOS: fixture files referenced by name only exist for the complete/missing/adversarial/stub scenarios', () => {
  const byName = Object.fromEntries(SCENARIOS.map((s) => [s.name, s]))
  assert.deepEqual(byName['complete'].fixtureFiles, [
    'rename-session-before.png',
    'rename-session-after.png',
  ])
  assert.deepEqual(byName['adversarial'].fixtureFiles, [
    'settings-screen-1.png',
    'settings-screen-2.png',
  ])
  assert.deepEqual(byName['API error'].fixtureFiles, [])
})

// ── 2. Per-scenario assert() logic (pure, synthetic verdicts) ────────────────

test('complete.assert: satisfactory -> ok; partial/unsatisfactory/unjudged -> not ok', () => {
  const scenario = SCENARIOS.find((s) => s.name === 'complete')
  assert.equal(scenario.assert({ evidence: 'satisfactory', confidence: 'high' }).ok, true)
  assert.equal(scenario.assert({ evidence: 'partial', confidence: 'medium' }).ok, false)
  assert.equal(scenario.assert({ evidence: 'unsatisfactory', confidence: 'low' }).ok, false)
  assert.equal(scenario.assert({ unjudged: true, reason: 'missing-key' }).ok, false)
})

test('missing-scene.assert: partial + scene-02 non-passed -> ok; satisfactory -> not ok; scene-02 passed -> not ok', () => {
  const scenario = SCENARIOS.find((s) => s.name === 'missing scene')
  assert.equal(
    scenario.assert({
      evidence: 'partial',
      perScene: [
        { id: 'scene-01', verdict: 'passed' },
        { id: 'scene-02', verdict: 'failed' },
      ],
    }).ok,
    true,
  )
  assert.equal(
    scenario.assert({
      evidence: 'satisfactory',
      perScene: [
        { id: 'scene-01', verdict: 'passed' },
        { id: 'scene-02', verdict: 'passed' },
      ],
    }).ok,
    false,
    'satisfactory must not pass this scenario even if the clamp would normally allow it',
  )
  assert.equal(
    scenario.assert({
      evidence: 'partial',
      perScene: [
        { id: 'scene-01', verdict: 'passed' },
        { id: 'scene-02', verdict: 'passed' },
      ],
    }).ok,
    false,
    'scene-02 must be flagged non-passed by the model, not just capped by the clamp',
  )
  assert.equal(scenario.assert({ unjudged: true, reason: 'x' }).ok, false)
})

test('adversarial.assert: evidence !== satisfactory -> ok; satisfactory -> not ok; unjudged -> not ok', () => {
  const scenario = SCENARIOS.find((s) => s.name === 'adversarial')
  assert.equal(scenario.assert({ evidence: 'unsatisfactory' }).ok, true)
  assert.equal(scenario.assert({ evidence: 'partial' }).ok, true)
  assert.equal(scenario.assert({ evidence: 'satisfactory' }).ok, false)
  assert.equal(scenario.assert({ unjudged: true, reason: 'x' }).ok, false)
})

test('stub-without-disclosure.assert: stub caveat present -> ok; absent -> not ok; high confidence with a failed scene -> not ok', () => {
  const scenario = SCENARIOS.find((s) => s.name === 'stub without disclosure')
  assert.equal(
    scenario.assert({
      caveats: ['agent-runner stubbed: UI + orchestration exercised against a stubbed daemon'],
      confidence: 'medium',
      perScene: [{ id: 'scene-01', verdict: 'passed' }],
    }).ok,
    true,
  )
  assert.equal(
    scenario.assert({ caveats: [], confidence: 'medium', perScene: [] }).ok,
    false,
    'missing the stub caveat must fail regardless of confidence',
  )
  assert.equal(
    scenario.assert({
      caveats: ['agent-runner stubbed: UI + orchestration exercised against a stubbed daemon'],
      confidence: 'high',
      perScene: [{ id: 'scene-01', verdict: 'failed' }],
    }).ok,
    false,
    'high confidence with a failed scene violates the clamp policy',
  )
  assert.equal(scenario.assert({ unjudged: true, reason: 'x' }).ok, false)
})

test('API-error.assert: unjudged:true -> ok; anything else -> not ok', () => {
  const scenario = SCENARIOS.find((s) => s.name === 'API error')
  assert.equal(scenario.assert({ unjudged: true, reason: 'simulated-api-error' }).ok, true)
  assert.equal(scenario.assert({ evidence: 'satisfactory' }).ok, false)
})

// ── 3. Key-gate skip ──────────────────────────────────────────────────────────

test('runEval: no PROOF_ANTHROPIC_API_KEY -> skipped=true, ok=true, runScenario never called', async () => {
  let called = false
  const result = await runEval({
    env: {},
    runScenario: async () => {
      called = true
      return { name: 'x', ok: true, note: '' }
    },
    log: () => {},
  })
  assert.equal(result.skipped, true)
  assert.equal(result.ok, true)
  assert.equal(called, false, 'runScenario must not be called when the key is absent')
})

test('runEval: no key -> log line matches /skipped: no PROOF_ANTHROPIC_API_KEY/', async () => {
  const lines = []
  await runEval({
    env: {},
    runScenario: async () => ({ ok: true }),
    log: (m) => lines.push(String(m)),
  })
  assert.ok(
    lines.some((l) => /skipped: no PROOF_ANTHROPIC_API_KEY/.test(l)),
    `expected a skip line in: ${JSON.stringify(lines)}`,
  )
})

test('runEval: ANTHROPIC_API_KEY alone (without PROOF_ANTHROPIC_API_KEY) still skips', async () => {
  // Regression guard for the documented divergence from proof-tui-agent.eval.mjs:
  // the judge eval must gate on the PROOF_ prefixed key specifically.
  let called = false
  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-not-the-right-one' },
    runScenario: async () => {
      called = true
      return { ok: true }
    },
    log: () => {},
  })
  assert.equal(result.skipped, true)
  assert.equal(called, false)
})

// ── 4. runEval aggregation ────────────────────────────────────────────────────

test('runEval: all scenarios pass -> ok===true, results carry all 5 scenario names', async () => {
  const result = await runEval({
    env: { PROOF_ANTHROPIC_API_KEY: 'sk-fake' },
    runScenario: async (scenario) => ({ name: scenario.name, ok: true, note: 'stub-pass' }),
    log: () => {},
  })
  assert.equal(result.ok, true)
  assert.equal(result.results.length, 5)
  assert.deepEqual(
    result.results.map((r) => r.name),
    SCENARIOS.map((s) => s.name),
  )
})

test('runEval: one scenario fails -> ok===false, only that scenario is marked not-ok', async () => {
  const result = await runEval({
    env: { PROOF_ANTHROPIC_API_KEY: 'sk-fake' },
    runScenario: async (scenario) => ({
      name: scenario.name,
      ok: scenario.name !== 'adversarial',
      note: scenario.name === 'adversarial' ? 'forced fail' : 'stub-pass',
    }),
    log: () => {},
  })
  assert.equal(result.ok, false)
  const adversarial = result.results.find((r) => r.name === 'adversarial')
  assert.equal(adversarial.ok, false)
  assert.equal(result.results.filter((r) => r.ok).length, 4)
})

test('runEval: a throwing runScenario is caught per-scenario and marked not-ok (does not abort the suite)', async () => {
  const result = await runEval({
    env: { PROOF_ANTHROPIC_API_KEY: 'sk-fake' },
    runScenario: async (scenario) => {
      if (scenario.name === 'complete') throw new Error('boom')
      return { name: scenario.name, ok: true, note: 'stub-pass' }
    },
    log: () => {},
  })
  assert.equal(result.ok, false)
  assert.equal(result.results.length, 5, 'every scenario must still be recorded')
  const complete = result.results.find((r) => r.name === 'complete')
  assert.equal(complete.ok, false)
  assert.match(complete.note, /boom/)
})

test('runEval: log lines include every scenario name, PASS/FAIL labels, and a summary', async () => {
  const lines = []
  await runEval({
    env: { PROOF_ANTHROPIC_API_KEY: 'sk-fake' },
    runScenario: async (scenario) => ({
      name: scenario.name,
      ok: scenario.name !== 'adversarial',
      note: 'x',
    }),
    log: (m) => lines.push(String(m)),
  })
  const joined = lines.join('\n')
  for (const scenario of SCENARIOS) {
    assert.ok(joined.includes(scenario.name), `log must mention scenario "${scenario.name}"`)
  }
  assert.ok(/PASS/.test(joined), 'log must include a PASS label')
  assert.ok(/FAIL/.test(joined), 'log must include a FAIL label')
  assert.ok(/summary/i.test(joined), 'log must include a summary line')
  assert.ok(/4\/5/.test(joined), 'summary must report 4/5 scenarios passed')
})

// ── 5. Scenario 5 (API error) end-to-end via the REAL runScenario ────────────

test('runScenario: "API error" scenario -> {unjudged:true} via the real judgeProof, no key/network needed', async () => {
  const scenario = SCENARIOS.find((s) => s.name === 'API error')
  const result = await runScenario(scenario)
  assert.equal(result.name, 'API error')
  assert.equal(result.ok, true, `expected ok via unjudged:true, got note: ${result.note}`)
  assert.equal(result.verdict.unjudged, true)
  assert.equal(result.verdict.reason, 'simulated-api-error')
})
