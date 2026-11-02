import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  VERIFY_RETRY_AFTER_MS,
  VERIFY_RETRY_ATTEMPTS,
  applyWake,
  ciGreenOf,
  rearmArgv,
  wakePlan,
} from './verify-wake.mjs'

const READY = { isDraft: false, mergeStateStatus: 'CLEAN' }

// Callback wakes: the transition each one waits for is still to come.
for (const [name, input, owner, waitingOn, trigger] of [
  [
    'conflict takes precedence',
    { verdict: 'blocked', prView: { mergeStateStatus: 'DIRTY' } },
    'repair',
    'conflict',
    'checks_passed_ready',
  ],
  [
    'a dirty merge-state wait belongs to repair',
    { verdict: 'wait', reason: 'merge-state-dirty', prView: READY, ciGreen: true },
    'repair',
    'conflict',
    'checks_passed_ready',
  ],
  [
    'draft waits for a person',
    { verdict: 'wait', reason: 'pr-draft', prView: { isDraft: true }, ciGreen: true },
    'person',
    'draft',
    'checks_passed_ready',
  ],
  [
    'draft findings belong to repair',
    { verdict: 'defect', prView: { isDraft: true } },
    'repair',
    'findings',
    'checks_passed',
  ],
  [
    'blocked draft belongs to repair',
    { verdict: 'blocked', reason: 'pr-draft', prView: { isDraft: true } },
    'repair',
    'blockers',
    'checks_passed',
  ],
  [
    'ready findings belong to repair even on green CI',
    { verdict: 'defect', prView: READY, ciGreen: true },
    'repair',
    'findings',
    'checks_passed_ready',
  ],
  [
    'a ready CI wait whose checks are not green waits for them',
    { verdict: 'wait', reason: 'ci-not-settled', prView: READY, ciGreen: false },
    'verify',
    'ci',
    'checks_passed_ready',
  ],
  [
    'a draft CI wait whose checks are not green waits for checks_passed',
    { verdict: 'wait', reason: 'ci-not-green-on-head', prView: { isDraft: true }, ciGreen: false },
    'verify',
    'ci',
    'checks_passed',
  ],
]) {
  test(`callback: ${name}`, () => {
    assert.deepEqual(wakePlan(input), {
      owner,
      waitingOn,
      wake: 'callback',
      trigger,
      onTransition: true,
    })
    const argv = rearmArgv({ pr: 7, chat: 'verify', repo: 'owner/repo', trigger })
    assert.equal(argv[3], trigger)
    assert.equal(argv[4], '--on-transition')
  })
}

// Retry wakes: a ready PR already meets checks_passed_ready, so no on-transition callback can fire.
for (const [name, input, waitingOn] of [
  ['ci-not-settled on green CI', { reason: 'ci-not-settled', ciGreen: true }, 'ci'],
  ['ci-not-settled on unreadable CI', { reason: 'ci-not-settled', ciGreen: null }, 'ci'],
  ['merge-state-unknown', { reason: 'merge-state-unknown', ciGreen: true }, 'merge-state'],
  ['merge-state-behind', { reason: 'merge-state-behind', ciGreen: true }, 'merge-state'],
  ['merge-state-blocked', { reason: 'merge-state-blocked', ciGreen: true }, 'merge-state'],
  ['merge-state-unstable', { reason: 'merge-state-unstable', ciGreen: null }, 'merge-state'],
  ['merge-state on red CI', { reason: 'merge-state-unknown', ciGreen: false }, 'merge-state'],
  ['another verify-owned reason', { reason: 'stack-busy', ciGreen: true }, 'verify'],
]) {
  test(`retry: ${name}`, () => {
    for (const attempt of [1, VERIFY_RETRY_ATTEMPTS]) {
      const plan = wakePlan({ verdict: 'wait', prView: READY, attempt, ...input })
      assert.deepEqual(plan, {
        owner: 'verify',
        waitingOn,
        wake: 'retry',
        retry: { attempt, of: VERIFY_RETRY_ATTEMPTS, afterMs: VERIFY_RETRY_AFTER_MS },
      })
    }
  })
}

test('the retries run out into the gate fallback', () => {
  const plan = wakePlan({
    verdict: 'wait',
    reason: 'merge-state-unknown',
    prView: READY,
    ciGreen: true,
    attempt: VERIFY_RETRY_ATTEMPTS + 1,
  })
  assert.deepEqual(plan, {
    owner: 'verify',
    waitingOn: 'merge-state',
    wake: 'none',
    fallback: 'gate',
  })
})

test('a closed PR has nothing to wait for', () => {
  const plan = wakePlan({ verdict: 'wait', reason: 'pr-not-open', prView: { state: 'CLOSED' } })
  assert.deepEqual(plan, { owner: 'verify', waitingOn: 'closed', wake: 'none' })
})

test('the retry budget is about three minutes', () => {
  assert.ok(VERIFY_RETRY_ATTEMPTS >= 1)
  assert.ok(VERIFY_RETRY_ATTEMPTS * VERIFY_RETRY_AFTER_MS <= 5 * 60_000)
})

test('ciGreenOf reads green, red or pending, and unknown CI', () => {
  assert.equal(ciGreenOf({ green: true, verdict: { state: 'green' } }), true)
  assert.equal(ciGreenOf({ green: false, verdict: { state: 'failing' } }), false)
  assert.equal(ciGreenOf({ green: false, verdict: { state: 'pending' } }), false)
  assert.equal(ciGreenOf({ green: false, verdict: { state: 'unknown' } }), null)
  assert.equal(ciGreenOf(null), null)
})

test('applyWake sleeps out a retry, arms nothing, and a dry run never sleeps', async () => {
  const plan = wakePlan({ verdict: 'wait', reason: 'merge-state-unknown', prView: READY })
  const slept = []
  const reads = []
  const io = { pr: 7, chat: 'c', read: (...a) => reads.push(a), sleep: (ms) => slept.push(ms) }
  const r = await applyWake(plan, io)
  assert.equal(r.armed, false)
  assert.equal(r.slept, VERIFY_RETRY_AFTER_MS)
  assert.deepEqual(slept, [VERIFY_RETRY_AFTER_MS])
  assert.deepEqual(r.writes, [])
  const dry = await applyWake(plan, { ...io, dryRun: true })
  assert.equal(dry.dryRun, true)
  assert.equal(dry.slept, 0)
  assert.deepEqual(slept, [VERIFY_RETRY_AFTER_MS])
  assert.equal(reads.length, 0)
})
