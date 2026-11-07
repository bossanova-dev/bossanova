import { test } from 'node:test'
import assert from 'node:assert/strict'
import { settleRead } from './settle-read.mjs'

for (const target of [1, 3, Infinity]) {
  test(`settleRead returns the last value with target attempt ${target}`, async () => {
    let time = 0
    let reads = 0
    const sleeps = []
    const result = await settleRead({
      read: async () => ++reads,
      isSettled: (value) => value === target,
      budgetMs: 10,
      intervalMs: 5,
      now: () => time,
      sleep: async (ms) => {
        sleeps.push(ms)
        time += ms
      },
    })
    const attempts = target === 1 ? 1 : 3
    assert.deepEqual(result, {
      value: attempts,
      settled: target !== Infinity,
      attempts,
      elapsedMs: (attempts - 1) * 5,
    })
    assert.equal(sleeps.length, attempts - 1)
  })
}
test('settleRead propagates the original read failure', async () => {
  const failure = new Error('transport')
  let reads = 0
  let time = 0
  await assert.rejects(
    settleRead({
      read: async () => {
        if (++reads === 2) throw failure
        return 'stale'
      },
      isSettled: () => false,
      budgetMs: 10,
      intervalMs: 5,
      now: () => time,
      sleep: async (ms) => {
        time += ms
      },
    }),
    (error) => error === failure,
  )
})
