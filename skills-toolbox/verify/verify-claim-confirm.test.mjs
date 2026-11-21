import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  confirmOwnClaim,
  claimWithdrawal,
  confirmDispatchStarted,
  recoverUnknownDispatch,
} from './verify-claim-confirm.mjs'

const prior = { state: 'pending', description: 'verifying… old', updatedAt: 'old' }
const mine = { state: 'pending', description: 'verifying… mine', updatedAt: 'new' }
const reading = (status) => ({ readable: true, status })

test('claim confirmation retries the previous status and unreadable reads', async () => {
  const reads = [reading(prior), { readable: false }, reading(mine)]
  const sleeps = []
  assert.equal(
    await confirmOwnClaim({
      read: () => reads.shift(),
      prior,
      token: 'mine',
      sleep: (ms) => sleeps.push(ms),
    }),
    'won',
  )
  assert.deepEqual(sleeps, [250, 500])
})

test('claim confirmation bounds lag and leaves a superseding claim or verdict alone', async () => {
  for (const status of [
    { ...mine, description: 'verifying… foreign' },
    { ...mine, description: 'verified' },
  ])
    assert.equal(
      await confirmOwnClaim({
        read: () => reading(status),
        prior,
        token: 'mine',
        sleep: () => assert.fail('no sleep'),
      }),
      'superseded',
    )
  const sleeps = []
  assert.equal(
    await confirmOwnClaim({
      read: () => ({ readable: false }),
      prior,
      token: 'mine',
      sleep: (ms) => sleeps.push(ms),
    }),
    'unconfirmed',
  )
  assert.deepEqual(sleeps, [250, 500, 1000])
})

test('withdrawal restores a park or makes the head dispatchable', () => {
  assert.deepEqual(claimWithdrawal({ park: null, reason: 'claim-unconfirmed' }), {
    state: 'pending',
    description: 'waiting: claim-unconfirmed',
  })
  assert.deepEqual(
    claimWithdrawal({
      park: {
        state: 'success',
        description: 'needs human: partial-build',
        targetUrl: 'https://example.test/note',
      },
      reason: 'dispatch-unknown',
    }),
    {
      state: 'success',
      description: 'needs human: partial-build',
      targetUrl: 'https://example.test/note',
    },
  )
})

test('dispatch confirmation requires WORKING and never polls without reserved budget', () => {
  let clock = 0
  let reads = 0
  const run = () => ({
    ok: true,
    payload: { chats: [{ agent_session_id: 'c', status: ++reads === 2 ? 'WORKING' : 'IDLE' }] },
  })
  const args = {
    run,
    boss: 'boss',
    sessionId: 's',
    chatId: 'c',
    boundMs: 6000,
    sleep: (ms) => {
      clock += ms
    },
    timeout: () => 1000,
    now: () => clock,
  }
  assert.equal(confirmDispatchStarted(args), true)
  assert.equal(clock, 1500)
  assert.equal(
    confirmDispatchStarted({ ...args, boundMs: 0, run: () => assert.fail('no polling') }),
    false,
  )
  reads = 0
  clock = 0
  assert.equal(
    confirmDispatchStarted({ ...args, run: () => ({ ok: false }), boundMs: 3000 }),
    false,
  )
  assert.equal(clock, 3000)
})

for (const why of ['DELIVERY_NOT_SUBMITTED', 'DELIVERY_STATE_UNCONFIRMED']) {
  test(`dispatch ${why} distinguishes refusal from a WORKING confirmation`, () => {
    let polls = 0,
      released = 0
    const result = recoverUnknownDispatch({
      routed: { route: 'new-verify-chat', sessionId: 's', chatId: 'c', why },
      remaining: () => 10000,
      reserveMs: 3000,
      release: () => {
        released++
        return true
      },
      remember: () => {},
      run: () => {
        polls++
        return { ok: true, payload: { chats: [{ agent_session_id: 'c', status: 'WORKING' }] } }
      },
      boss: 'boss',
      sleep: () => assert.fail('no sleep'),
      timeout: () => 1000,
      now: () => 0,
    })
    if (why === 'DELIVERY_NOT_SUBMITTED') {
      assert.equal(polls, 0)
      assert.equal(released, 1)
      assert.match(result, /dispatch-unknown.*released/)
    } else {
      assert.equal(polls, 1)
      assert.equal(released, 0)
      assert.equal(result, null)
    }
  })
}
