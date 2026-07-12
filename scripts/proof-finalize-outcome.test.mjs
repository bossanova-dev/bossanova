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
  softenTuiExit,
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

test('aggregateExitCode: TUI agent-incomplete → 1 (BOS-226 fail-loud flip)', () => {
  const perSurface = [{ surface: 'tui', outcome: 'deferred', reasonCode: 'agent-incomplete' }]
  assert.equal(aggregateExitCode(perSurface), 1)
})

test('aggregateExitCode: mixed [tui agent-incomplete, web passed] → 1', () => {
  const perSurface = [
    { surface: 'tui', outcome: 'deferred', reasonCode: 'agent-incomplete' },
    { surface: 'web', outcome: 'passed', reasonCode: null },
  ]
  assert.equal(aggregateExitCode(perSurface), 1)
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

// BOS-226: `scenario-missing` was a warn-only TUI authoring deferral (BOS-220,
// neutral → 0). Epic 4 (BOS-226) makes proof required for TUI: a missing
// scenario now contributes exit 1 on both surfaces. Pin the single-surface and
// aggregate contributions so the fail-loud semantics can't silently regress.
test('aggregateExitCode: TUI scenario-missing → 1 (BOS-226 required-for-TUI flip)', () => {
  const perSurface = [{ surface: 'tui', outcome: 'deferred', reasonCode: 'scenario-missing' }]
  assert.equal(aggregateExitCode(perSurface), 1)
})

test('aggregateExitCode: web scenario-missing → 1 (BOS-226)', () => {
  const perSurface = [{ surface: 'web', outcome: 'deferred', reasonCode: 'scenario-missing' }]
  assert.equal(aggregateExitCode(perSurface), 1)
})

test('aggregateExitCode: passed web + TUI scenario-missing → 1 (BOS-226 fatal)', () => {
  const perSurface = [
    { surface: 'web', outcome: 'passed', reasonCode: null },
    { surface: 'tui', outcome: 'deferred', reasonCode: 'scenario-missing' },
  ]
  assert.equal(aggregateExitCode(perSurface), 1)
})

test('aggregateExitCode: empty → 0', () => {
  assert.equal(aggregateExitCode([]), 0)
})

// BOS-226 rollback lever: `softenTuiExit(perSurface, { soft })` is the pure
// escape hatch. With soft=true it maps ONLY tui agent-incomplete/scenario-missing
// entries to softened → 0; web contributions and pipeline-error are never
// softened, and soft=false is a no-op.
test('softenTuiExit: soft TUI agent-incomplete → aggregates 0', () => {
  const softened = softenTuiExit(
    [{ surface: 'tui', outcome: 'deferred', reasonCode: 'agent-incomplete' }],
    { soft: true },
  )
  assert.equal(aggregateExitCode(softened), 0)
})

test('softenTuiExit: soft TUI scenario-missing → aggregates 0', () => {
  const softened = softenTuiExit(
    [{ surface: 'tui', outcome: 'deferred', reasonCode: 'scenario-missing' }],
    { soft: true },
  )
  assert.equal(aggregateExitCode(softened), 0)
})

test('softenTuiExit: web agent-incomplete NOT softened → still 1', () => {
  const softened = softenTuiExit(
    [{ surface: 'web', outcome: 'deferred', reasonCode: 'agent-incomplete' }],
    { soft: true },
  )
  assert.equal(aggregateExitCode(softened), 1)
})

test('softenTuiExit: TUI pipeline-error NEVER softened → still 1', () => {
  const softened = softenTuiExit(
    [{ surface: 'tui', outcome: 'failed', reasonCode: 'pipeline-error' }],
    { soft: true },
  )
  assert.equal(aggregateExitCode(softened), 1)
})

test('softenTuiExit: soft=false is a no-op (no softened flag, aggregate unchanged)', () => {
  const input = [{ surface: 'tui', outcome: 'deferred', reasonCode: 'agent-incomplete' }]
  const out = softenTuiExit(input, { soft: false })
  assert.deepEqual(out, input)
  assert.equal(aggregateExitCode(out), 1)
})

test('surfaceLabel: tui → TUI, web → Web, other → capitalized', () => {
  assert.equal(surfaceLabel('tui'), 'TUI')
  assert.equal(surfaceLabel('web'), 'Web')
  assert.equal(surfaceLabel('marketing'), 'Marketing')
})
