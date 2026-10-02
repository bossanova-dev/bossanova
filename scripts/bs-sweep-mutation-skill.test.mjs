// Behaviour tests for the bs-sweep-mutation survivor extraction (scripts/bs-sweep-mutation-survivors.mjs).

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { DISPATCH_FAILURE } from '../skills-toolbox/bs-run-sentinel.mjs'
import {
  GATE_FAILED,
  extractSurvivors,
  extractUncoveredRows,
  extractCoverageRows,
  matchMutationResult,
} from './bs-sweep-mutation-survivors.mjs'

const here = (rel) => new URL(rel, import.meta.url)
const read = (rel) => readFileSync(here(rel), 'utf8')

// ---------------------------------------------------------------------------
// Loss-check — the deterministic extraction reproduces EVERY survivor in a
// captured mutation-suite output. This is the gate that must pass before the
// cheap-tier mutation run ships; any dropped survivor fails here.
// ---------------------------------------------------------------------------

const survivorsFixture = read('./fixtures/bs-sweep-mutation/survivors.txt')
const uncoveredFixture = read('./fixtures/bs-sweep-mutation/uncovered.txt')
const coverageFixture = read('./fixtures/bs-sweep-mutation/coverage.txt')

test('loss-check: extractSurvivors reproduces every survivor in the fixture', () => {
  const survivors = extractSurvivors(survivorsFixture)

  // The canonical set of unique, well-formed survivor lines present in the fixture.
  const expected = [
    '[bosso--internal-auth] internal/auth/token.go:42 CONDITIONALS_BOUNDARY',
    '[bosso--internal-auth] internal/auth/token.go:58 INVERT_NEGATIVES',
    '[bosso--internal-auth] internal/auth/jwt.go:113 ARITHMETIC_BASE',
    '[boss--internal-client] internal/client/socket.go:77 CONDITIONALS_NEGATION',
    '[bossd--internal-server] internal/server/session.go:204 INCREMENT_DECREMENT',
    '[bossd--internal-server] internal/server/session.go:204 INCREMENT_DECREMENT',
    '[bossd--internal-server] internal/server/lifecycle.go:319 REMOVE_SELF_ASSIGNMENTS',
  ]
  assert.deepEqual(survivors, expected, 'every survivor must be reproduced, in order')

  // Belt-and-suspenders: no well-formed survivor line in the fixture is dropped.
  const fixtureSurvivors = new Set(
    survivorsFixture
      .split(/\r?\n/)
      .map((l) => l.trim())
      .filter((l) => /^\[[^\]]+\]\s+\S+:\d+\s+\S+/.test(l)),
  )
  for (const line of fixtureSurvivors) {
    assert.ok(survivors.includes(line), `dropped survivor: ${line}`)
  }
})

test('extractUncoveredRows reproduces compact uncovered rows for Phase B', () => {
  const rows = extractUncoveredRows(uncoveredFixture)
  assert.deepEqual(rows, [
    '[bossd--internal-server] internal/server/session.go:204 CONDITIONALS_BOUNDARY',
    '[bossd--internal-server] internal/server/session.go:211 INVERT_NEGATIVES',
    '[bosso--internal-auth] internal/auth/token.go:88 CONDITIONALS_NEGATION',
  ])
})

test('extractSurvivors drops blanks and non-survivor noise', () => {
  const out = extractSurvivors('\n==> Surviving mutants: make mutate-survivors\n   \n')
  assert.deepEqual(out, [], 'noise-only input yields no survivors')
})

test('extractCoverageRows parses the tab-separated coverage view, lowest first', () => {
  const rows = extractCoverageRows(coverageFixture)
  assert.deepEqual(
    rows.map((r) => r.name),
    ['bossd--internal-server', 'bosso--internal-auth', 'boss--internal-client', 'bossd--cmd'],
  )
  assert.equal(rows[0].coverage, '31.2')
  assert.equal(rows[0].notCovered, 22)
})

// ---------------------------------------------------------------------------
// BOS-1278 — a worker that reaches ANY terminal state leaves a bounded sentinel.
// ---------------------------------------------------------------------------

test('a failed make target is a routable verdict, distinct from a dead dispatch', () => {
  // Before this token a red target killed the worker under `set -e` with no sentinel,
  // so the orchestrator read `missing` and synthesized `dispatch-failure` — the same
  // token a dead dispatch produces. The two need different remedies (re-run the
  // target vs re-dispatch the worker), so they may not share one verdict.
  assert.deepEqual(matchMutationResult(GATE_FAILED), { result: GATE_FAILED })
  assert.notEqual(GATE_FAILED, DISPATCH_FAILURE)
  assert.equal(
    matchMutationResult(DISPATCH_FAILURE),
    null,
    'the synthesized token stays unwritable by a worker',
  )
  // Able to fire: a near-miss token still classifies null, so the acceptance above is
  // this token's membership and not a matcher that accepts anything.
  assert.equal(matchMutationResult('gate-failed-ish'), null)
})
