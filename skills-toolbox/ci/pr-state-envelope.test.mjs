import assert from 'node:assert/strict'
import test from 'node:test'

import {
  RUNS_NOT_READ,
  classifyInputsFromPrState,
  rollupFromEnvelope,
  workflowRunsFromEnvelope,
} from './pr-state-envelope.mjs'

const HEAD = 'a'.repeat(40)
const run = (headSha = HEAD) => ({
  name: 'CI',
  status: 'completed',
  conclusion: 'success',
  event: 'push',
  head_sha: headSha,
})
const envelope = (extra = {}) => ({
  head_sha: HEAD,
  checks: [{ name: 'build', status: 'completed', conclusion: 'success', is_unclassified: false }],
  check_verdict: { state: 'green', reason: 'no-gate-ran' },
  workflow_runs: [run()],
  is_workflow_runs_read: true,
  ...extra,
})

test('classifyInputsFromPrState: both SHAs are the head; the bare envelope and the read output agree', () => {
  const bare = classifyInputsFromPrState(envelope())
  assert.equal(bare.headSHA, HEAD)
  assert.equal(bare.observedSHA, HEAD)
  assert.equal(bare.readError, null)
  assert.deepEqual(classifyInputsFromPrState({ source: 'daemon', state: envelope() }), bare)
  assert.deepEqual(
    classifyInputsFromPrState({ source: 'gh-fallback', fallbackReason: 'x', state: envelope() }),
    bare,
  )
})

test('classifyInputsFromPrState: a failure envelope, no head or runs for another head is a read error', () => {
  const failed = classifyInputsFromPrState({
    error: { code: 'RESOURCE_EXHAUSTED', connect_code: 'resource_exhausted', message: 'slow down' },
  })
  assert.match(failed.readError, /RESOURCE_EXHAUSTED: slow down/)
  assert.equal(failed.rollup, null)
  const readFailed = classifyInputsFromPrState({
    source: 'daemon',
    error: { code: 'HEAD_MISMATCH', message: 'two heads' },
  })
  assert.match(readFailed.readError, /HEAD_MISMATCH/)
  assert.match(classifyInputsFromPrState(envelope({ head_sha: '' })).readError, /no head SHA/)
  assert.match(
    classifyInputsFromPrState(envelope({ workflow_runs: [run('b'.repeat(40))] })).readError,
    /other than/,
  )
  assert.match(classifyInputsFromPrState(null).readError, /no PR state/)
})

test('rollupFromEnvelope: an unclassified check keeps no conclusion; the daemon verdict is not read', () => {
  const rollup = rollupFromEnvelope(
    envelope({
      checks: [
        { name: 'odd', status: 'completed', conclusion: 'success', is_unclassified: true },
        { name: 'e2e', status: 'in_progress', conclusion: '' },
        { name: '', status: 'completed', conclusion: 'success' },
      ],
    }),
  )
  assert.deepEqual(rollup, [
    { name: 'odd', status: 'COMPLETED', conclusion: '', description: '' },
    { name: 'e2e', status: 'IN_PROGRESS', conclusion: '', description: '' },
  ])
})

test('workflowRunsFromEnvelope: runs that were not read are null, never an empty list', () => {
  assert.deepEqual(
    workflowRunsFromEnvelope(envelope({ workflow_runs: [], is_workflow_runs_read: false })),
    {
      runs: null,
      error: RUNS_NOT_READ,
    },
  )
  assert.deepEqual(
    workflowRunsFromEnvelope(envelope({ workflow_runs: [], is_workflow_runs_read: true })).runs,
    [],
  )
  // An older daemon sets has_settled_workflow_runs on its own green but never fills workflow_runs:
  // that empty list is no evidence, so in-flight runs cannot read as "no head run".
  for (const has_settled_workflow_runs of [true, false]) {
    assert.deepEqual(
      workflowRunsFromEnvelope(
        envelope({ workflow_runs: [], is_workflow_runs_read: false, has_settled_workflow_runs }),
      ),
      { runs: null, error: RUNS_NOT_READ },
    )
  }
  assert.equal(
    classifyInputsFromPrState(envelope({ is_workflow_runs_read: false })).workflowRuns,
    null,
  )
})
