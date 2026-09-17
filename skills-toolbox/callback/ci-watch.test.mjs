import assert from 'node:assert/strict'
import test from 'node:test'

import {
  BLOCKING_STATES,
  CI_WATCH_ACTIONS,
  CI_WATCH_REASONS,
  CI_WATCH_STATES,
  classifyCiObservation,
  mayStopObserving,
} from './ci-watch.mjs'

const NOW = '2026-09-17T12:00:00Z'
const CHAT = 'chat-abc'
const PR = 278
const TRIGGERS = ['checks_passed', 'checks_failed', 'merged']

// A live row: active, this chat, this PR, a day of runway.
function row(overrides = {}) {
  return {
    id: 'cb-1',
    state: 'active',
    target_chat_id: CHAT,
    pr_number: PR,
    trigger: 'checks_passed',
    expires_at: '2026-09-18T12:00:00Z',
    ...overrides,
  }
}

function classify(overrides = {}) {
  return classifyCiObservation({
    checkVerdict: { state: 'pending' },
    prView: { state: 'OPEN', isDraft: false, mergedAt: null },
    callbacksAvailable: true,
    targetChatId: CHAT,
    prNumber: PR,
    watches: [],
    requiredTriggers: TRIGGERS,
    now: NOW,
    ...overrides,
  })
}

test('only `unwatched` blocks — the blocking surface is exactly one state', () => {
  assert.deepEqual(BLOCKING_STATES, [CI_WATCH_STATES.UNWATCHED])
})

test('pending checks with no watches is unwatched, and names every missing trigger', () => {
  const v = classify()
  assert.equal(v.state, CI_WATCH_STATES.UNWATCHED)
  assert.equal(v.blocking, true)
  assert.equal(v.action, CI_WATCH_ACTIONS.ARM)
  assert.deepEqual(v.missingTriggers, TRIGGERS)
  assert.equal(mayStopObserving(v), false)
})

test('live watches covering every required trigger is watched', () => {
  const v = classify({
    watches: TRIGGERS.map((trigger, i) => row({ id: `cb-${i}`, trigger })),
  })
  assert.equal(v.state, CI_WATCH_STATES.WATCHED)
  assert.equal(v.blocking, false)
  assert.deepEqual(v.missingTriggers, [])
  assert.deepEqual(v.liveTriggers, TRIGGERS)
  assert.equal(mayStopObserving(v), true)
})

test('one missing trigger among live ones still blocks, and names only that one', () => {
  const v = classify({
    watches: [row({ trigger: 'checks_passed' }), row({ id: 'cb-2', trigger: 'checks_failed' })],
  })
  assert.equal(v.state, CI_WATCH_STATES.UNWATCHED)
  assert.deepEqual(v.missingTriggers, ['merged'])
  assert.deepEqual(v.liveTriggers, ['checks_passed', 'checks_failed'])
})

// --- Liveness: only `active` counts -----------------------------------------------------------
// The daemon lifecycle is active -> leased -> triggered -> delivered. Every non-active value means
// the one-shot watch has already fired or is gone; counting one as observation reports `watched`
// over a PR nobody is watching.
for (const state of ['leased', 'triggered', 'delivered', 'canceled', 'expired']) {
  test(`a ${state} row is not a live watch — it has already fired or is gone`, () => {
    const v = classify({ watches: TRIGGERS.map((trigger) => row({ trigger, state })) })
    assert.equal(v.state, CI_WATCH_STATES.UNWATCHED)
    assert.deepEqual(v.missingTriggers, TRIGGERS)
  })
}

// --- Scoping: the two holes in `boss callback list` --------------------------------------------

test("a peer session's watch on the same PR does not satisfy this chat", () => {
  // `boss callback list` filters by chat only when --chat was explicitly passed, while
  // `boss callback add` defaults the chat. An unscoped list therefore returns other sessions' rows,
  // and counting them reports `watched` while THIS chat gets no wake.
  const v = classify({
    watches: TRIGGERS.map((trigger) => row({ trigger, target_chat_id: 'chat-someone-else' })),
  })
  assert.equal(v.state, CI_WATCH_STATES.UNWATCHED)
  assert.deepEqual(v.missingTriggers, TRIGGERS)
})

test("a sibling PR's watch does not satisfy this PR", () => {
  // The CLI exposes no --pr filter, so a repo-scoped list returns sibling PRs' watches.
  const v = classify({ watches: TRIGGERS.map((trigger) => row({ trigger, pr_number: 999 })) })
  assert.equal(v.state, CI_WATCH_STATES.UNWATCHED)
  assert.deepEqual(v.missingTriggers, TRIGGERS)
})

test('pr_number compares numerically, so a string row still matches', () => {
  const v = classify({
    watches: TRIGGERS.map((trigger) => row({ trigger, pr_number: String(PR) })),
  })
  assert.equal(v.state, CI_WATCH_STATES.WATCHED)
})

// --- Expiry runway ------------------------------------------------------------------------------

test('a watch expiring inside the runway floor does not count as live', () => {
  // The reported incident: a 30-minute expiry against a longer CI run. 20 minutes of runway is not
  // observation, even though the daemon has not swept the row yet.
  const v = classify({
    watches: TRIGGERS.map((trigger) => row({ trigger, expires_at: '2026-09-17T12:20:00Z' })),
  })
  assert.equal(v.state, CI_WATCH_STATES.UNWATCHED)
  assert.deepEqual(v.missingTriggers, TRIGGERS)
})

test('the default runway floor is 30m — a watch just past it counts', () => {
  const v = classify({
    watches: TRIGGERS.map((trigger) => row({ trigger, expires_at: '2026-09-17T12:31:00Z' })),
  })
  assert.equal(v.state, CI_WATCH_STATES.WATCHED)
})

test('minRemaining is caller-tunable for a slow repo', () => {
  const watches = TRIGGERS.map((trigger) => row({ trigger, expires_at: '2026-09-17T14:00:00Z' }))
  assert.equal(classify({ watches, minRemaining: '1h' }).state, CI_WATCH_STATES.WATCHED)
  assert.equal(classify({ watches, minRemaining: '4h' }).state, CI_WATCH_STATES.UNWATCHED)
})

test('a row with no expiry is not live', () => {
  const v = classify({ watches: TRIGGERS.map((trigger) => row({ trigger, expires_at: '' })) })
  assert.equal(v.state, CI_WATCH_STATES.UNWATCHED)
})

// --- Per-trigger satisfaction: the burn case ----------------------------------------------------

test('a merged PR with checks still pending does not ask for a `merged` watch', () => {
  // The burn case. A global "checks are terminal" gate reads pending here and arms `merged` into an
  // already-true condition, which fires instantly and consumes the one-shot watch. `merged` must be
  // judged against the PR, not the checks.
  const v = classify({
    checkVerdict: { state: 'pending' },
    prView: { state: 'MERGED', isDraft: false, mergedAt: '2026-09-17T11:00:00Z' },
    watches: [row({ trigger: 'checks_passed' }), row({ id: 'cb-2', trigger: 'checks_failed' })],
  })
  assert.equal(v.state, CI_WATCH_STATES.WATCHED)
  assert.deepEqual(v.skippedTriggers, ['merged'])
  assert.deepEqual(v.missingTriggers, [])
})

test('green checks on an unmerged PR still require the merge watch', () => {
  // The mirror failure: a global "checks are terminal" gate stops looking here, while boss-epic is
  // still waiting on the merge that never gets observed.
  //
  // `checks_failed` stays REQUIRED rather than skipped, and that is deliberate: green is not a
  // permanent property of a head. A readied PR can flip to UNSTABLE afterwards, which is the whole
  // reason boss-build has a Step 10 settle loop. Arming it cannot burn the watch either, because its
  // condition (failing) does not currently hold — only a trigger whose condition ALREADY holds is
  // skipped.
  const v = classify({ checkVerdict: { state: 'green' } })
  assert.equal(v.state, CI_WATCH_STATES.UNWATCHED)
  assert.deepEqual(v.skippedTriggers, ['checks_passed'])
  assert.deepEqual(v.missingTriggers, ['checks_failed', 'merged'])
})

test('every required condition already holding is settled, with nothing to arm', () => {
  const v = classify({
    checkVerdict: { state: 'green' },
    prView: { state: 'MERGED', isDraft: false, mergedAt: '2026-09-17T11:00:00Z' },
    requiredTriggers: ['checks_passed', 'merged'],
  })
  assert.equal(v.state, CI_WATCH_STATES.SETTLED)
  assert.equal(v.blocking, false)
  assert.equal(v.action, CI_WATCH_ACTIONS.PROCEED)
  assert.deepEqual(v.missingTriggers, [])
})

test('failing checks satisfy checks_failed, not checks_passed', () => {
  const v = classify({ checkVerdict: { state: 'failing' }, requiredTriggers: ['checks_failed'] })
  assert.equal(v.state, CI_WATCH_STATES.SETTLED)
  assert.deepEqual(v.skippedTriggers, ['checks_failed'])
})

test('checks_passed_ready needs green AND out-of-draft AND open', () => {
  const draftAware = ['checks_passed_ready']
  assert.equal(
    classify({
      checkVerdict: { state: 'green' },
      prView: { state: 'OPEN', isDraft: true, mergedAt: null },
      requiredTriggers: draftAware,
    }).state,
    CI_WATCH_STATES.UNWATCHED,
    'a green DRAFT is not merge-eligible, so the watch is still needed',
  )
  assert.equal(
    classify({
      checkVerdict: { state: 'green' },
      prView: { state: 'OPEN', isDraft: false, mergedAt: null },
      requiredTriggers: draftAware,
    }).state,
    CI_WATCH_STATES.SETTLED,
  )
})

test('an unreadable PR leaves every PR-derived trigger unsatisfied', () => {
  const v = classify({
    checkVerdict: { state: 'green' },
    prView: null,
    requiredTriggers: ['merged'],
  })
  assert.equal(v.state, CI_WATCH_STATES.UNWATCHED)
  assert.deepEqual(v.missingTriggers, ['merged'])
})

// --- `unknown` never blocks ---------------------------------------------------------------------

test('an unreadable check state is reported, never blocking', () => {
  // The deadlock this avoids: the protocol forbids arming on could-not-evaluate, so a blocking
  // `unknown` could reach neither `watched` nor `settled` nor (before the poll) `polled` — an
  // unbounded hang in a headless run at the exact moment PR state is unreadable.
  const v = classify({ checkVerdict: { state: 'unknown' } })
  assert.equal(v.state, CI_WATCH_STATES.UNKNOWN)
  assert.equal(v.blocking, false)
  assert.equal(v.action, CI_WATCH_ACTIONS.POLL)
  assert.equal(mayStopObserving(v), true)
})

test('an unreadable check state after the bounded poll is polled, and proceeds', () => {
  const v = classify({ checkVerdict: { state: 'unknown' }, fallbackPollCompleted: true })
  assert.equal(v.state, CI_WATCH_STATES.POLLED)
  assert.equal(v.reason, CI_WATCH_REASONS.UNREADABLE_POLLED)
  assert.equal(v.action, CI_WATCH_ACTIONS.PROCEED)
})

test('a missing check verdict is treated as unreadable, not as settled', () => {
  const v = classify({ checkVerdict: null })
  assert.equal(v.state, CI_WATCH_STATES.UNKNOWN)
  assert.equal(v.blocking, false)
})

// --- Degrades -----------------------------------------------------------------------------------

test('callbacks unavailable degrades to polled and carries the reason', () => {
  const v = classify({
    callbacksAvailable: false,
    unavailableReason: 'not a bossd-managed session: BOSS_SESSION_ID is unset',
  })
  assert.equal(v.state, CI_WATCH_STATES.POLLED)
  assert.equal(v.blocking, false)
  assert.match(v.reason, /BOSS_SESSION_ID is unset/)
  assert.equal(v.action, CI_WATCH_ACTIONS.POLL)
})

test('an unverified callback target degrades to polled, never blocks', () => {
  // boss-epic: no verified target means registration is skipped by contract. Blocking here would
  // hang every epic whose target cannot be resolved.
  const v = classify({ targetVerified: false })
  assert.equal(v.state, CI_WATCH_STATES.POLLED)
  assert.equal(v.reason, CI_WATCH_REASONS.TARGET_UNVERIFIED)
  assert.equal(v.blocking, false)
})

test('callbacks-unavailable outranks the arm bound', () => {
  const v = classify({ callbacksAvailable: false, armAttempts: 1 })
  assert.equal(v.state, CI_WATCH_STATES.POLLED)
  assert.match(v.reason, new RegExp(CI_WATCH_REASONS.CALLBACKS_UNAVAILABLE))
})

// --- The arm bound ------------------------------------------------------------------------------

test('after one arm attempt the verdict degrades instead of blocking again', () => {
  // The livelock bound. A second arm is provably useless: either the first took (and the daemon
  // rejects a co-satisfiable re-arm in the same group), or it failed for a reason a retry will not
  // change. Without this the run arms and re-classifies forever.
  const v = classify({ armAttempts: 1 })
  assert.equal(v.state, CI_WATCH_STATES.POLLED)
  assert.equal(v.blocking, false)
  assert.equal(v.action, CI_WATCH_ACTIONS.POLL)
  assert.equal(mayStopObserving(v), true)
})

test('the arm error is surfaced in the degraded reason', () => {
  const v = classify({ armAttempts: 1, lastArmError: 'group already holds checks_passed' })
  assert.match(v.reason, /group already holds checks_passed/)
})

test('arming once and succeeding reaches watched, not the degrade', () => {
  const v = classify({
    armAttempts: 1,
    watches: TRIGGERS.map((trigger, i) => row({ id: `cb-${i}`, trigger })),
  })
  assert.equal(v.state, CI_WATCH_STATES.WATCHED)
})

// --- Shape ---------------------------------------------------------------------------------------

test('an empty required-trigger list never blocks', () => {
  // NO_CHANGE and the pre-PR yields reach the gate with nothing to observe.
  const v = classify({ requiredTriggers: [] })
  assert.equal(v.blocking, false)
  assert.equal(v.state, CI_WATCH_STATES.WATCHED)
})

test('malformed watch rows are ignored rather than throwing', () => {
  const v = classify({ watches: [null, 'nonsense', 42, row({ trigger: 'checks_passed' })] })
  assert.equal(v.state, CI_WATCH_STATES.UNWATCHED)
  assert.deepEqual(v.liveTriggers, ['checks_passed'])
})

test('a non-array watches payload is treated as no watches', () => {
  assert.equal(classify({ watches: null }).state, CI_WATCH_STATES.UNWATCHED)
})

test('every verdict carries the full reporting shape', () => {
  for (const v of [
    classify(),
    classify({ watches: TRIGGERS.map((t) => row({ trigger: t })) }),
    classify({ callbacksAvailable: false }),
    classify({ checkVerdict: { state: 'unknown' } }),
  ]) {
    assert.ok(Object.values(CI_WATCH_STATES).includes(v.state), `state: ${v.state}`)
    assert.equal(typeof v.reason, 'string')
    assert.equal(typeof v.blocking, 'boolean')
    assert.ok(Array.isArray(v.missingTriggers))
    assert.ok(Array.isArray(v.skippedTriggers))
    assert.ok(Array.isArray(v.liveTriggers))
    assert.equal(v.blocking, BLOCKING_STATES.includes(v.state))
  }
})
