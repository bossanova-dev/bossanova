import assert from 'node:assert/strict'
import { test } from 'node:test'

import { classifyHeadCi } from './verify-gate.mjs'
import {
  ciViewOf,
  contextStatusOf,
  ghFallbackOpts,
  headView,
  prStateCall,
  readVerifyPrState,
} from './verify-pr-state.mjs'

const HEAD = 'a'.repeat(40)
const envelope = (checks = []) => ({ head_sha: HEAD, checks })
const ok = (payload) => ({ ok: true, payload, output: JSON.stringify(payload), exit: 0 })

test('daemon reads use the same cached and fresh call triples', () => {
  assert.deepEqual(prStateCall(7, 'o/r'), [
    'boss',
    ['pr', 'state', '7', '--repo', 'o/r', '--json'],
    { json: true },
  ])
  assert.deepEqual(prStateCall(7, 'o/r', { fresh: true })[1].at(-1), '--fresh')
})

test('only unavailable daemon readings enable a sticky labelled gh fallback', () => {
  const failures = [
    ...['code', 'connect_code'].flatMap((field) =>
      ['UNAVAILABLE', 'UNIMPLEMENTED', 'NOT_FOUND'].map((code) => ({
        ...ok({ error: { [field]: code } }),
        ok: false,
        exit: 1,
      })),
    ),
    { ok: false, error: 'ENOENT', exit: null, output: '' },
    { ok: false, error: 'unreadable-json', exit: 0, output: 'Usage: boss' },
    { ok: false, error: 'exit-1', exit: 1, output: 'old CLI' },
  ]
  for (const failure of failures) {
    const calls = []
    const read = (...call) => {
      calls.push(call)
      return call[0] === 'boss' ? failure : ok({ headRefOid: HEAD, statusCheckRollup: [] })
    }
    assert.equal(readVerifyPrState(read, { pr: 7, repo: 'o/r' }).source, 'gh-fallback')
    assert.equal(headView(read, null, { number: 7 }, 'o/r').headRefOid, HEAD)
    assert.equal(headView(read, null, { number: 8 }, 'o/r').headRefOid, HEAD)
    assert.equal(calls.filter((c) => c[0] === 'boss').length, 1)
    assert.deepEqual(
      calls.filter((c) => c[0] === 'gh').map((c) => c[2]),
      [
        { json: true, env: { BOSS_GH_USAGE_LABEL: 'fallback:verify-gate' } },
        { json: true, env: { BOSS_GH_USAGE_LABEL: 'fallback:verify-gate' } },
      ],
    )
  }
  assert.deepEqual(ghFallbackOpts({ raw: true }), {
    raw: true,
    env: { BOSS_GH_USAGE_LABEL: 'fallback:verify-gate' },
  })
})

test('daemon refusals, timeouts and malformed envelopes stay unknown without gh', () => {
  const failures = [
    ...['RESOURCE_EXHAUSTED', 'INVALID_ARGUMENT', 'FAILED_PRECONDITION'].map((code) => ({
      ...ok({ error: { code } }),
      ok: false,
      exit: 1,
    })),
    { ok: false, error: 'ETIMEDOUT', exit: null, output: '' },
    ok({}),
    ok({ head_sha: HEAD }),
    ok({ head_sha: HEAD, checks: null }),
  ]
  for (const failure of failures) {
    const calls = []
    const read = (...call) => {
      calls.push(call)
      return failure
    }
    assert.equal(readVerifyPrState(read, { pr: 7, repo: 'o/r' }).source, null)
    assert.equal(headView(read, null, { number: 7 }, 'o/r'), null)
    assert.equal(
      calls.some((c) => c[0] === 'gh'),
      false,
    )
  }
})

test('context statuses retain verification meaning without inventing timestamps', () => {
  for (const [status, conclusion, state] of [
    ['in_progress', '', 'pending'],
    ['queued', 'success', 'pending'],
    ['completed', 'success', 'success'],
    ['completed', 'failure', 'failure'],
    ['completed', 'cancelled', 'failure'],
    ['completed', '', 'failure'],
  ]) {
    assert.deepEqual(
      contextStatusOf(
        envelope([{ name: 'boss/verify', status, conclusion, description: 'verified' }]),
        'boss/verify',
      ),
      { state, description: 'verified' },
    )
  }
  assert.equal(contextStatusOf(envelope(), 'boss/verify'), null)
  const check = { name: 'boss/verify', status: 'completed', conclusion: 'success' }
  assert.equal(contextStatusOf(envelope([check, check]), 'boss/verify'), undefined)
  assert.equal(
    contextStatusOf(envelope([{ ...check, is_unclassified: true }]), 'boss/verify'),
    undefined,
  )
})

test('daemon CI views match equivalent gh rollups for green, pending, failing and unclassified', () => {
  for (const [status, conclusion, is_unclassified] of [
    ['completed', 'success', false],
    ['in_progress', '', false],
    ['completed', 'failure', false],
    ['completed', 'success', true],
  ]) {
    const check = { name: 'ci', status, conclusion, is_unclassified }
    const gh = {
      headRefOid: HEAD,
      statusCheckRollup: [
        {
          name: 'ci',
          status: status.toUpperCase(),
          conclusion: is_unclassified ? '' : conclusion.toUpperCase(),
          description: '',
        },
      ],
    }
    assert.deepEqual(ciViewOf(envelope([check])), gh)
    assert.deepEqual(classifyHeadCi(ciViewOf(envelope([check])), HEAD), classifyHeadCi(gh, HEAD))
  }
})
