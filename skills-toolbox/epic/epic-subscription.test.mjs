import assert from 'node:assert/strict'
import test from 'node:test'

import {
  GAP_WHEN_SETTLED_STATES,
  SETTLED_SESSION_STATES,
  ensureSettleSubscription,
  needsRearm,
  refreshStoredSubscription,
  sessionReadsSettled,
  verifySubscriptionRow,
} from './epic-subscription.mjs'

const TRIGGER_STATES = [
  'GREEN_DRAFT',
  'READY_FOR_REVIEW',
  'MERGED',
  'CLOSED',
  'BLOCKED',
  'ORPHANED',
]

const IN_FLIGHT_STATES = [
  'CREATING_WORKTREE',
  'STARTING_AGENT',
  'PUSHING_BRANCH',
  'OPENING_DRAFT_PR',
  'IMPLEMENTING_PLAN',
  'AWAITING_CHECKS',
  'FIXING_CHECKS',
  'FINALIZING',
]

const GAP_STATES = ['GREEN_DRAFT', 'BLOCKED', 'ORPHANED']
const NON_GAP_SETTLED_STATES = ['READY_FOR_REVIEW', 'MERGED', 'CLOSED']

function makeSubscription(overrides = {}) {
  return {
    id: 'sub-1',
    ownerSessionId: 'sess-1',
    originChatId: 'chat-coord',
    triggerEvent: 'settled',
    state: 'active',
    firedAt: '',
    expiresAt: '2026-01-08T00:00:00Z',
    ...overrides,
  }
}

function makeLiveRow(overrides = {}) {
  return {
    id: 'sub-1',
    owner_session_id: 'sess-1',
    origin_chat_id: 'chat-coord',
    trigger_event: 'settled',
    state: 'active',
    fired_at: '',
    expires_at: '2026-01-08T00:00:00Z',
    ...overrides,
  }
}

function makeFakeIo({ rows = [], nextId = 'sub-2', listError = null, subscribeError = '' } = {}) {
  const subscribeCalls = []
  const listCalls = []
  let liveRows = [...rows]
  return {
    subscribeCalls,
    listCalls,
    io: {
      async listSessionSubscriptions(args) {
        listCalls.push(args)
        if (listError) throw listError
        return liveRows
      },
      async subscribeSessionOutcome(args) {
        subscribeCalls.push(args)
        if (subscribeError) return { error: subscribeError }
        const created = makeLiveRow({
          id: nextId,
          owner_session_id: args.sessionId,
          state: 'active',
          fired_at: '',
        })
        liveRows = [...liveRows, created]
        return { subscription: created }
      },
    },
  }
}

test('SETTLED_SESSION_STATES and GAP_WHEN_SETTLED_STATES are frozen sets', () => {
  assert.equal(Object.isFrozen(SETTLED_SESSION_STATES), true)
  assert.equal(Object.isFrozen(GAP_WHEN_SETTLED_STATES), true)
  assert.deepEqual([...SETTLED_SESSION_STATES].sort(), [...TRIGGER_STATES].sort())
  assert.deepEqual([...GAP_WHEN_SETTLED_STATES].sort(), [...GAP_STATES].sort())
})

test('sessionReadsSettled returns true for all six trigger states and ignores case and whitespace', () => {
  for (const state of TRIGGER_STATES) {
    assert.equal(sessionReadsSettled(state), true, `${state} should read settled`)
    assert.equal(
      sessionReadsSettled(`  ${state.toLowerCase()}  `),
      true,
      `lowercase trimmed ${state} should read settled`,
    )
  }
})

test('sessionReadsSettled returns false for all in-flight, empty, undefined, and unknown states', () => {
  for (const state of IN_FLIGHT_STATES) {
    assert.equal(sessionReadsSettled(state), false, `${state} should not read settled`)
  }
  assert.equal(sessionReadsSettled(''), false)
  assert.equal(sessionReadsSettled('   '), false)
  assert.equal(sessionReadsSettled(undefined), false)
  assert.equal(sessionReadsSettled(null), false)
  assert.equal(sessionReadsSettled('UNKNOWN'), false)
})

test('refreshStoredSubscription keeps an active matching row active', () => {
  const stored = makeSubscription()
  const refreshed = refreshStoredSubscription(stored, [
    makeLiveRow({ state: 'active', fired_at: '', expires_at: '2026-01-09T00:00:00Z' }),
  ])
  assert.deepEqual(refreshed, {
    ...stored,
    state: 'active',
    firedAt: '',
    expiresAt: '2026-01-09T00:00:00Z',
  })
})

test('refreshStoredSubscription marks fired when row state is fired or fired_at is non-empty', () => {
  const stored = makeSubscription()

  const byState = refreshStoredSubscription(stored, [
    makeLiveRow({ state: 'fired', fired_at: '2026-01-02T00:00:00Z' }),
  ])
  assert.equal(byState?.state, 'fired')
  assert.equal(byState?.firedAt, '2026-01-02T00:00:00Z')

  const byTimestamp = refreshStoredSubscription(stored, [
    makeLiveRow({ state: 'active', fired_at: '2026-01-02T00:00:00Z' }),
  ])
  assert.equal(byTimestamp?.state, 'fired')
  assert.equal(byTimestamp?.firedAt, '2026-01-02T00:00:00Z')
})

test('refreshStoredSubscription returns null for missing row or null/id-less stored record', () => {
  assert.equal(refreshStoredSubscription(makeSubscription({ id: 'sub-1' }), []), null)
  assert.equal(
    refreshStoredSubscription(makeSubscription({ id: 'sub-1' }), [makeLiveRow({ id: 'sub-9' })]),
    null,
  )
  assert.equal(refreshStoredSubscription(null, [makeLiveRow()]), null)
  assert.equal(refreshStoredSubscription(undefined, [makeLiveRow()]), null)
  assert.equal(refreshStoredSubscription(makeSubscription({ id: '' }), [makeLiveRow()]), null)
})

test('refreshStoredSubscription accepts camelCase rows', () => {
  const stored = makeSubscription()
  const active = refreshStoredSubscription(stored, [
    {
      id: 'sub-1',
      ownerSessionId: 'sess-1',
      originChatId: 'chat-coord',
      triggerEvent: 'settled',
      state: 'active',
      firedAt: '',
      expiresAt: '2026-01-10T00:00:00Z',
    },
  ])
  assert.equal(active?.state, 'active')
  assert.equal(active?.expiresAt, '2026-01-10T00:00:00Z')

  const fired = refreshStoredSubscription(stored, [
    {
      id: 'sub-1',
      state: 'active',
      firedAt: '2026-01-03T00:00:00Z',
      expiresAt: '2026-01-10T00:00:00Z',
    },
  ])
  assert.equal(fired?.state, 'fired')
  assert.equal(fired?.firedAt, '2026-01-03T00:00:00Z')
})

test('ensureSettleSubscription re-arms when stored says active but live row fired in flight (fault 3)', async () => {
  const { io, subscribeCalls } = makeFakeIo({
    rows: [makeLiveRow({ id: 'sub-1', state: 'fired', fired_at: '2026-01-02T00:00:00Z' })],
    nextId: 'sub-2',
  })
  const actions = []
  const result = await ensureSettleSubscription({
    io,
    record: { ticketId: 'T-1', sessionId: 'sess-1' },
    stored: makeSubscription({ id: 'sub-1', state: 'active' }),
    sessionState: 'IMPLEMENTING_PLAN',
    message: 'wake up',
    actions,
  })

  assert.equal(subscribeCalls.length, 1)
  assert.equal(result.error, '')
  assert.equal(result.subscription?.id, 'sub-2')
  assert.equal(result.subscription?.state, 'active')
  assert.deepEqual(actions, ['subscribe:T-1:verified'])
})

test('ensureSettleSubscription re-arms when stored id is missing from live list in flight (fault 3)', async () => {
  const { io, subscribeCalls } = makeFakeIo({
    rows: [],
    nextId: 'sub-2',
  })
  const actions = []
  const result = await ensureSettleSubscription({
    io,
    record: { ticketId: 'T-1', sessionId: 'sess-1' },
    stored: makeSubscription({ id: 'sub-missing', state: 'active' }),
    sessionState: 'IMPLEMENTING_PLAN',
    message: 'wake up',
    actions,
  })

  assert.equal(subscribeCalls.length, 1)
  assert.equal(result.error, '')
  assert.equal(result.subscription?.id, 'sub-2')
  assert.deepEqual(actions, ['subscribe:T-1:verified'])
})

test('ensureSettleSubscription skips and reports coverage gap in GREEN_DRAFT, BLOCKED, ORPHANED (fault 1)', async () => {
  for (const state of GAP_STATES) {
    const { io, subscribeCalls } = makeFakeIo()
    const actions = []
    const result = await ensureSettleSubscription({
      io,
      record: { ticketId: 'T-1', sessionId: 'sess-1' },
      stored: null,
      sessionState: state,
      message: 'wake up',
      actions,
    })

    assert.equal(subscribeCalls.length, 0, `${state} must not subscribe`)
    assert.match(result.error, new RegExp(`reads ${state}`))
    assert.equal(result.subscription, null)
    assert.deepEqual(actions, [`subscribe:T-1:skipped:${state}`])
  }
})

test('ensureSettleSubscription skips without error where PR callbacks remain', async () => {
  for (const state of NON_GAP_SETTLED_STATES) {
    const { io, subscribeCalls } = makeFakeIo()
    const actions = []
    const result = await ensureSettleSubscription({
      io,
      record: { ticketId: 'T-1', sessionId: 'sess-1' },
      stored: null,
      sessionState: state,
      message: 'wake up',
      actions,
    })

    assert.equal(subscribeCalls.length, 0, `${state} must not subscribe`)
    assert.equal(result.error, '', `${state} should not report a coverage gap`)
    assert.equal(result.subscription, null)
    assert.deepEqual(actions, [`subscribe:T-1:skipped:${state}`])
  }
})

test('ensureSettleSubscription avoids wake loop across three cycles in GREEN_DRAFT', async () => {
  const { io, subscribeCalls } = makeFakeIo({
    rows: [makeLiveRow({ id: 'sub-1', state: 'fired', fired_at: '2026-01-02T00:00:00Z' })],
  })
  let stored = makeSubscription({ id: 'sub-1', state: 'active' })
  const actions = []

  for (let cycle = 0; cycle < 3; cycle += 1) {
    const result = await ensureSettleSubscription({
      io,
      record: { ticketId: 'T-1', sessionId: 'sess-1' },
      stored,
      sessionState: 'GREEN_DRAFT',
      message: 'wake up',
      actions,
    })
    assert.match(result.error, /reads GREEN_DRAFT/)
    assert.equal(result.subscription?.id, 'sub-1')
    assert.equal(result.subscription?.state, 'fired')
    stored = result.subscription
  }

  assert.equal(subscribeCalls.length, 0)
  assert.deepEqual(actions, [
    'subscribe:T-1:skipped:GREEN_DRAFT',
    'subscribe:T-1:skipped:GREEN_DRAFT',
    'subscribe:T-1:skipped:GREEN_DRAFT',
  ])
})

test('ensureSettleSubscription keeps a live subscription regardless of session state', async () => {
  for (const sessionState of ['IMPLEMENTING_PLAN', 'GREEN_DRAFT', 'READY_FOR_REVIEW']) {
    const { io, subscribeCalls } = makeFakeIo({
      rows: [makeLiveRow({ id: 'sub-1', state: 'active', fired_at: '' })],
    })
    const actions = []
    const stored = makeSubscription({ id: 'sub-1', state: 'active' })
    const result = await ensureSettleSubscription({
      io,
      record: { ticketId: 'T-1', sessionId: 'sess-1' },
      stored,
      sessionState,
      message: 'wake up',
      actions,
    })

    assert.equal(subscribeCalls.length, 0)
    assert.equal(result.error, '')
    assert.equal(result.subscription?.id, 'sub-1')
    assert.equal(result.subscription?.state, 'active')
    assert.deepEqual(actions, [])
  }
})

test('ensureSettleSubscription preserves stored record and does not re-subscribe when list read throws', async () => {
  const { io, subscribeCalls } = makeFakeIo({
    listError: new Error('daemon unavailable'),
  })
  const stored = makeSubscription({ id: 'sub-1', state: 'active' })
  const actions = []
  const result = await ensureSettleSubscription({
    io,
    record: { ticketId: 'T-1', sessionId: 'sess-1' },
    stored,
    sessionState: 'IMPLEMENTING_PLAN',
    message: 'wake up',
    actions,
  })

  assert.equal(subscribeCalls.length, 0)
  assert.deepEqual(result.subscription, stored)
  assert.match(result.error, /list read failed/)
  assert.match(result.error, /daemon unavailable/)
  assert.deepEqual(actions, ['subscribe:T-1:unverified'])
})

test('needsRearm and verifySubscriptionRow preserve expected behavior', () => {
  assert.equal(needsRearm({ registration: null, inFlight: false }), false)
  assert.equal(needsRearm({ registration: null, inFlight: true }), true)
  assert.equal(needsRearm({ registration: { state: 'active' }, inFlight: true }), false)
  assert.equal(needsRearm({ registration: { state: 'fired' }, inFlight: true }), true)

  assert.deepEqual(verifySubscriptionRow(makeLiveRow(), { ownerSessionId: 'sess-1' }), {
    ok: true,
    reason: '',
  })
  assert.equal(
    verifySubscriptionRow(makeLiveRow({ fired_at: '2026-01-02T00:00:00Z' }), {
      ownerSessionId: 'sess-1',
    }).ok,
    false,
  )
})
