/**
 * proof-brief.eval.test.mjs — Unit tests for the proof-brief.eval.mjs harness.
 *
 * Drives runEval via an injected `generate` stub so the suite passes with NO
 * PROOF_ANTHROPIC_API_KEY, NO node_modules, and NO network. Mirrors
 * proof-tui-agent.eval.test.mjs.
 */

import assert from 'node:assert/strict'
import { test } from 'node:test'

import { runEval } from './proof-brief.eval.mjs'

function makeLog() {
  const lines = []
  const log = (msg) => lines.push(String(msg))
  log.lines = lines
  return log
}

// A brief covering BOTH required demonstrations (compact TUI row + web toggle).
const COVERING_BRIEF = {
  title: 'Prove compact mode',
  description: 'Demonstrate the new Compact mode on both surfaces.',
  targetRoutes: ['/account'],
  stepsHints: [
    'Open TUI settings and observe the Compact mode row',
    'Toggle Compact mode on /account',
  ],
  expectedEvidence: ['Compact mode'],
}
// A brief covering NEITHER required demonstration.
const EMPTY_BRIEF = {
  title: 'Unrelated',
  description: 'Shows something else entirely.',
  targetRoutes: ['/'],
  stepsHints: ['Open the home page'],
  expectedEvidence: [],
}

test('runEval: no PROOF_ANTHROPIC_API_KEY → skipped=true, ok=true, generate never called', async () => {
  let called = false
  const result = await runEval({
    env: {},
    generate: async () => {
      called = true
      return COVERING_BRIEF
    },
    log: () => {},
  })
  assert.equal(result.skipped, true)
  assert.equal(result.ok, true)
  assert.equal(called, false, 'generate must not run without a key')
})

test('runEval: no key → log line matches /skipped: no PROOF_ANTHROPIC_API_KEY/', async () => {
  const log = makeLog()
  await runEval({ env: {}, generate: async () => COVERING_BRIEF, log })
  assert.ok(log.lines.some((l) => /skipped: no PROOF_ANTHROPIC_API_KEY/.test(l)))
})

test('runEval: covering brief → ok=true, both required bullets covered', async () => {
  const result = await runEval({
    env: { PROOF_ANTHROPIC_API_KEY: 'sk-fake' },
    generate: async () => COVERING_BRIEF,
    log: () => {},
  })
  assert.equal(result.ok, true)
  assert.equal(result.results.length, 2)
  assert.ok(result.results.every((r) => r.covered))
})

test('runEval: empty brief → ok=false (uncovered required bullets caught)', async () => {
  const result = await runEval({
    env: { PROOF_ANTHROPIC_API_KEY: 'sk-fake' },
    generate: async () => EMPTY_BRIEF,
    log: () => {},
  })
  assert.equal(result.ok, false)
  assert.ok(result.results.some((r) => !r.covered))
})

test('runEval: passes the two scoped required bullets to generate as planRequiredProof', async () => {
  let seen = null
  await runEval({
    env: { PROOF_ANTHROPIC_API_KEY: 'sk-fake' },
    generate: async (opts) => {
      seen = opts
      return COVERING_BRIEF
    },
    log: () => {},
  })
  assert.ok(Array.isArray(seen.planRequiredProof))
  assert.equal(seen.planRequiredProof.length, 2)
  assert.ok(seen.diff.includes('Compact mode'), 'fixture diff touches both surfaces')
})

test('runEval: log lines include PASS/FAIL labels and a summary', async () => {
  const log = makeLog()
  await runEval({
    env: { PROOF_ANTHROPIC_API_KEY: 'sk-fake' },
    generate: async () => COVERING_BRIEF,
    log,
  })
  const joined = log.lines.join('\n')
  assert.ok(/PASS/.test(joined))
  assert.ok(/summary/i.test(joined))
})
