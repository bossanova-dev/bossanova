import { test } from 'node:test'
import assert from 'node:assert/strict'

import { expectedFires } from './cron-schedule.mjs'

test('expectedFires counts supported descriptor schedules in [from, to)', () => {
  const from = '2026-06-01T00:00:00.000Z'
  const to = '2026-06-08T00:00:00.000Z'

  assert.equal(expectedFires('@hourly', from, to), 168)
  assert.equal(expectedFires('@daily', from, to), 7)
  assert.equal(expectedFires('@weekly', from, to), 1)
})

test('expectedFires counts a representative 5-field expression', () => {
  assert.equal(
    expectedFires('0 9 * * 1-5', '2026-06-01T00:00:00.000Z', '2026-06-08T00:00:00.000Z'),
    5,
  )
})

test('expectedFires treats the window as [from, to)', () => {
  assert.equal(
    expectedFires('0 0 * * *', '2026-06-01T00:00:00.000Z', '2026-06-02T00:00:00.000Z'),
    1,
  )
})

test('expectedFires returns null for unsupported or invalid schedules', () => {
  assert.equal(expectedFires('', '2026-06-01T00:00:00.000Z', '2026-06-02T00:00:00.000Z'), null)
  assert.equal(
    expectedFires('@every 30m', '2026-06-01T00:00:00.000Z', '2026-06-02T00:00:00.000Z'),
    null,
  )
  assert.equal(
    expectedFires('not a schedule', '2026-06-01T00:00:00.000Z', '2026-06-02T00:00:00.000Z'),
    null,
  )
})
