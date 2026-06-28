/**
 * proof-tui-agent.eval.test.mjs — Unit tests for proof-tui-agent.eval.mjs harness logic.
 *
 * Strategy: drive runEval entirely via injected stubs so the suite passes with
 * NO ANTHROPIC_API_KEY, NO node_modules, and NO network access. The runner stub
 * returns verdicts by call order (first call = positive scenario, second = negative).
 *
 * Tests:
 *   1. skip: no key → {skipped:true, ok:true}; runner + buildBridge never called
 *   2. all-pass: fake key; runner returns 'passed' (positive) then 'failed' (negative)
 *      → ok===true, both scenarios recorded with matching expected verdicts
 *   3. regression caught: runner returns 'passed' for negative → ok===false
 *   4. positive-fails: runner returns 'failed' for positive → ok===false
 *   5. summary log lines emitted for each scenario
 */

import assert from 'node:assert/strict';
import { test } from 'node:test';

import { runEval } from './proof-tui-agent.eval.mjs';

// ── Helpers ───────────────────────────────────────────────────────────────────

/** Builds a log-capturing array with a push-compatible interface. */
function makeLog() {
  const lines = [];
  const log = (msg) => lines.push(String(msg));
  log.lines = lines;
  return log;
}

/** Runner stub that returns 'passed' for call 1 (positive) and 'failed' for call 2 (negative). */
function makeDefaultRunnerStub() {
  let calls = 0;
  return async () => {
    calls += 1;
    const verdict = calls === 1 ? 'passed' : 'failed';
    return { manifest: { verdict } };
  };
}

/** Runner stub that always returns the given verdict regardless of call order. */
function makeFixedRunnerStub(verdict) {
  return async () => ({ manifest: { verdict } });
}

const FAKE_BIN = '/tmp/fake-proof-tui-bridge';
const buildBridgeStub = async () => FAKE_BIN;

// ── 1. skip: no ANTHROPIC_API_KEY ─────────────────────────────────────────────

test('runEval: no ANTHROPIC_API_KEY → skipped=true, ok=true, runner never called', async () => {
  let runnerCalled = false;
  let buildBridgeCalled = false;

  const result = await runEval({
    env: {},
    runner: async () => {
      runnerCalled = true;
      return { manifest: { verdict: 'passed' } };
    },
    buildBridge: async () => {
      buildBridgeCalled = true;
      return FAKE_BIN;
    },
    log: () => {},
  });

  assert.equal(result.skipped, true);
  assert.equal(result.ok, true);
  assert.equal(runnerCalled, false, 'runner must not be called when key is absent');
  assert.equal(buildBridgeCalled, false, 'buildBridge must not be called when key is absent');
});

test('runEval: no key → log line matches /skipped: no ANTHROPIC_API_KEY/', async () => {
  const log = makeLog();

  await runEval({ env: {}, runner: async () => {}, buildBridge: async () => FAKE_BIN, log });

  assert.ok(
    log.lines.some((l) => /skipped: no ANTHROPIC_API_KEY/.test(l)),
    `Expected a line matching /skipped: no ANTHROPIC_API_KEY/ in: ${JSON.stringify(log.lines)}`,
  );
});

// ── 2. all-pass: positive='passed', negative='failed' ─────────────────────────

test('runEval: all-pass → ok===true, results contain both scenarios', async () => {
  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    buildBridge: buildBridgeStub,
    log: () => {},
  });

  assert.equal(result.ok, true, 'ok must be true when all scenarios meet expectation');
  assert.ok(Array.isArray(result.results), 'results must be an array');
  assert.equal(result.results.length, 2, 'must have exactly 2 scenario results');
});

test('runEval: all-pass → positive scenario ok===true with verdict=passed', async () => {
  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    buildBridge: buildBridgeStub,
    log: () => {},
  });

  const pos = result.results[0];
  assert.ok(pos, 'first result (positive) must exist');
  assert.equal(pos.expectedVerdict, 'passed');
  assert.equal(pos.verdict, 'passed');
  assert.equal(pos.ok, true);
});

test('runEval: all-pass → negative scenario ok===true with verdict=failed', async () => {
  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    buildBridge: buildBridgeStub,
    log: () => {},
  });

  const neg = result.results[1];
  assert.ok(neg, 'second result (negative) must exist');
  assert.equal(neg.expectedVerdict, 'failed');
  assert.equal(neg.verdict, 'failed');
  assert.equal(neg.ok, true);
});

// ── 3. regression caught: negative returns 'passed' → false-positive detected ─

test('runEval: runner returns passed for negative scenario → ok===false', async () => {
  // Both return 'passed': positive is correct, but negative expects 'failed' → caught.
  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeFixedRunnerStub('passed'),
    buildBridge: buildBridgeStub,
    log: () => {},
  });

  assert.equal(result.ok, false, 'ok must be false when negative scenario returns passed');
  const neg = result.results[1];
  assert.equal(neg.ok, false, 'negative scenario must be marked as failed');
});

// ── 4. positive-fails: positive returns 'failed' → ok===false ─────────────────

test('runEval: runner returns failed for positive scenario → ok===false', async () => {
  // Call 1 returns 'failed' (positive), call 2 returns 'failed' (negative).
  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeFixedRunnerStub('failed'),
    buildBridge: buildBridgeStub,
    log: () => {},
  });

  assert.equal(result.ok, false, 'ok must be false when positive scenario returns failed');
  const pos = result.results[0];
  assert.equal(pos.ok, false, 'positive scenario must be marked as failed');
});

// ── 5. summary log lines ───────────────────────────────────────────────────────

test('runEval: log lines include both scenario names and a summary', async () => {
  const log = makeLog();

  await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    buildBridge: buildBridgeStub,
    log,
  });

  const joined = log.lines.join('\n');
  assert.ok(/positive/.test(joined), 'log must mention the positive scenario');
  assert.ok(/negative/.test(joined), 'log must mention the negative scenario');
  assert.ok(/summary/i.test(joined), 'log must include a summary line');
});

test('runEval: log lines include PASS/FAIL status labels', async () => {
  const log = makeLog();

  await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    buildBridge: buildBridgeStub,
    log,
  });

  assert.ok(
    log.lines.some((l) => /PASS/.test(l)),
    'at least one PASS label in log',
  );
});

// ── 6. buildBridge called when no BOSS_PROOF_TUI_BRIDGE_BIN in env ────────────

test('runEval: buildBridge called when BOSS_PROOF_TUI_BRIDGE_BIN absent from env', async () => {
  let bridgeCalled = false;

  await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner: makeDefaultRunnerStub(),
    buildBridge: async () => {
      bridgeCalled = true;
      return FAKE_BIN;
    },
    log: () => {},
  });

  assert.equal(bridgeCalled, true, 'buildBridge must be called when no bridge bin in env');
});

test('runEval: buildBridge NOT called when BOSS_PROOF_TUI_BRIDGE_BIN already in env', async () => {
  let bridgeCalled = false;

  await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key', BOSS_PROOF_TUI_BRIDGE_BIN: '/existing/bin' },
    runner: makeDefaultRunnerStub(),
    buildBridge: async () => {
      bridgeCalled = true;
      return FAKE_BIN;
    },
    log: () => {},
  });

  assert.equal(bridgeCalled, false, 'buildBridge must NOT be called when bridge bin already set');
});

test('runEval: buildBridge throw rejects and restores BOSS_PROOF_UPLOAD', async () => {
  const savedUpload = process.env.BOSS_PROOF_UPLOAD;
  delete process.env.BOSS_PROOF_UPLOAD;

  let runnerCalled = false;
  await assert.rejects(
    runEval({
      env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
      runner: async () => {
        runnerCalled = true;
        return { manifest: { verdict: 'passed' } };
      },
      buildBridge: async () => {
        throw new Error('go build failed');
      },
      log: () => {},
    }),
    /go build failed/,
  );

  assert.equal(runnerCalled, false, 'runner must not run when the bridge build fails');
  assert.equal(
    process.env.BOSS_PROOF_UPLOAD,
    undefined,
    'BOSS_PROOF_UPLOAD must be restored (deleted) after a bridge-build failure',
  );

  if (savedUpload !== undefined) process.env.BOSS_PROOF_UPLOAD = savedUpload;
});

test('runEval: each scenario invokes the runner with dryRun:true and its matching brief', async () => {
  const fs = await import('node:fs');
  // Capture, per runner call, the dryRun flag and the brief the eval pointed
  // BOSS_PROOF_BRIEF at — this is how the eval communicates the scenario brief
  // to runTuiAgentProof. Guards against a regression that drops dryRun or wires
  // the wrong brief to a scenario (both would otherwise leave verdicts green).
  const calls = [];
  const runner = async (opts) => {
    const briefPath = process.env.BOSS_PROOF_BRIEF;
    const brief = JSON.parse(fs.readFileSync(briefPath, 'utf8'));
    calls.push({ dryRun: opts.dryRun, title: brief.title, evidence: brief.expectedEvidence });
    // First call (positive) → passed, second (negative) → failed.
    return { manifest: { verdict: calls.length === 1 ? 'passed' : 'failed' } };
  };

  const result = await runEval({
    env: { ANTHROPIC_API_KEY: 'sk-fake-key' },
    runner,
    buildBridge: buildBridgeStub,
    log: () => {},
  });

  assert.equal(result.ok, true);
  assert.equal(calls.length, 2, 'both scenarios must invoke the runner');
  assert.ok(
    calls.every((c) => c.dryRun === true),
    'every runner call must pass dryRun:true (no upload/post)',
  );
  // Call 1 = positive (#812, reachable evidence); call 2 = negative (unreachable sentinel).
  assert.match(calls[0].title, /#812/);
  assert.ok(!calls[0].evidence.includes('__UNREACHABLE_EVIDENCE_BOS71__'));
  assert.deepEqual(calls[1].evidence, ['__UNREACHABLE_EVIDENCE_BOS71__']);
});
