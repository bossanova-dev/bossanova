#!/usr/bin/env node
/**
 * proof-finalize-outcome.test.mjs — table tests for the outcome-classification
 * seam extracted from finalizeAgentProof (BOS-139 T5/D7).
 */

import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  aggregateExitCode,
  classifyRunOutcome,
  classifySurfaceOutcomes,
  surfaceLabel,
} from './proof-finalize-outcome.mjs'

test('classifyRunOutcome: passed + media → not degraded, no reason', () => {
  assert.deepEqual(classifyRunOutcome({ verdict: 'passed', hasMedia: true, noSurface: false }), {
    degraded: false,
    reasonCode: null,
  })
})

test('classifyRunOutcome: failed + media → agent-incomplete', () => {
  assert.deepEqual(classifyRunOutcome({ verdict: 'failed', hasMedia: true, noSurface: false }), {
    degraded: true,
    reasonCode: 'agent-incomplete',
  })
})

test('classifyRunOutcome: passed + no media → no-media', () => {
  assert.deepEqual(classifyRunOutcome({ verdict: 'passed', hasMedia: false, noSurface: false }), {
    degraded: true,
    reasonCode: 'no-media',
  })
})

test('classifyRunOutcome: noSurface wins over verdict → no-ui-surface', () => {
  assert.deepEqual(classifyRunOutcome({ verdict: 'failed', hasMedia: false, noSurface: true }), {
    degraded: true,
    reasonCode: 'no-ui-surface',
  })
})

// ── classifySurfaceOutcomes / aggregateExitCode (BOS-139 P2b) ────────────────

const passedRun = (surface) => ({
  surface,
  hasFailure: false,
  noSurface: false,
  captureShapes: [{ fileName: `${surface}/x.mp4` }],
})
const syntheticDeferred = (surface, reasonCode) => ({
  surface,
  hasFailure: false,
  noSurface: false,
  reasonCode,
  captureShapes: [],
})

test('classifySurfaceOutcomes: passed web + synthetic budget-exceeded → one passed, one deferred', () => {
  const out = classifySurfaceOutcomes([
    passedRun('tui'),
    syntheticDeferred('web', 'budget-exceeded'),
  ])
  assert.deepEqual(out, [
    { surface: 'tui', outcome: 'passed', reasonCode: null, error: null },
    { surface: 'web', outcome: 'deferred', reasonCode: 'budget-exceeded', error: null },
  ])
})

test('classifySurfaceOutcomes: web hasFailure + media → agent-incomplete', () => {
  const out = classifySurfaceOutcomes([
    {
      surface: 'web',
      hasFailure: true,
      noSurface: false,
      captureShapes: [{ fileName: 'w/x.mp4', error: 'stopped' }],
    },
  ])
  assert.equal(out[0].outcome, 'deferred')
  assert.equal(out[0].reasonCode, 'agent-incomplete')
})

test('classifySurfaceOutcomes: pipelineError → pipeline-error deferred', () => {
  const out = classifySurfaceOutcomes([
    {
      surface: 'web',
      hasFailure: true,
      noSurface: false,
      captureShapes: [],
      pipelineError: { stage: 'render', message: 'boom' },
    },
  ])
  assert.equal(out[0].reasonCode, 'pipeline-error')
})

test('aggregateExitCode: tui passed + web budget-exceeded → 0 (partial success neutral)', () => {
  const perSurface = classifySurfaceOutcomes([
    passedRun('tui'),
    syntheticDeferred('web', 'budget-exceeded'),
  ])
  assert.equal(aggregateExitCode(perSurface), 0)
})

test('aggregateExitCode: web agent-incomplete → 1', () => {
  const perSurface = [{ surface: 'web', outcome: 'deferred', reasonCode: 'agent-incomplete' }]
  assert.equal(aggregateExitCode(perSurface), 1)
})

test('aggregateExitCode: TUI agent-incomplete → 0 (Q6 preserved)', () => {
  const perSurface = [{ surface: 'tui', outcome: 'deferred', reasonCode: 'agent-incomplete' }]
  assert.equal(aggregateExitCode(perSurface), 0)
})

test('aggregateExitCode: pipeline-error anywhere → 1', () => {
  const perSurface = [
    { surface: 'tui', outcome: 'passed', reasonCode: null },
    { surface: 'web', outcome: 'deferred', reasonCode: 'pipeline-error' },
  ]
  assert.equal(aggregateExitCode(perSurface), 1)
})

test('aggregateExitCode: no-media / no-ui-surface / env-unavailable → 0', () => {
  for (const reasonCode of ['no-media', 'no-ui-surface', 'env-unavailable', 'budget-exceeded']) {
    assert.equal(
      aggregateExitCode([{ surface: 'web', outcome: 'deferred', reasonCode }]),
      0,
      `${reasonCode} must be neutral`,
    )
  }
})

test('aggregateExitCode: empty → 0', () => {
  assert.equal(aggregateExitCode([]), 0)
})

test('surfaceLabel: tui → TUI, web → Web, other → capitalized', () => {
  assert.equal(surfaceLabel('tui'), 'TUI')
  assert.equal(surfaceLabel('web'), 'Web')
  assert.equal(surfaceLabel('marketing'), 'Marketing')
})
