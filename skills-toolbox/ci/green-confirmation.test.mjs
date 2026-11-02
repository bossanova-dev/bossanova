import assert from 'node:assert/strict'
import { test } from 'node:test'

import { confirmsGreen, freshGreenAnchor } from './green-confirmation.mjs'

const A = 'a'.repeat(40)
const B = 'b'.repeat(40)
const T0 = 1_800_000_000_000
const INTERVAL = 30_000
const green = (atMs, fresh, { head = A, total = 4 } = {}) => ({
  atMs: T0 + atMs,
  headSha: head,
  fresh,
  state: 'green',
  total,
})
const pending = (atMs, fresh) => ({
  atMs: T0 + atMs,
  headSha: A,
  fresh,
  state: 'pending',
  total: 4,
})
const daemon = (readings) => confirmsGreen({ readings }, { fresh: true, intervalMs: INTERVAL })

test('daemon path: two fresh greens of one head an interval apart confirm', () => {
  assert.equal(daemon([green(0, true), green(30_000, true)]), true)
  // A cached green in between only woke the wait; it neither confirms nor resets.
  assert.equal(daemon([green(0, true), green(2_000, false), green(31_000, true)]), true)
})

test('daemon path: a cached green never counts as one of the two confirming greens', () => {
  assert.equal(daemon([green(0, false), green(30_000, true)]), false, 'cached first')
  assert.equal(daemon([green(0, true), green(30_000, false)]), false, 'cached last')
  assert.equal(daemon([green(0, false), green(30_000, false), green(60_000, false)]), false)
})

test('daemon path: fresh greens closer than one interval do not confirm', () => {
  assert.equal(daemon([green(0, true), green(1_000, true)]), false)
  // The cross-chunk shape: a fresh green, a cached one at the next chunk's start, a fresh re-read.
  assert.equal(daemon([green(0, true), green(2_000, false), green(3_000, true)]), false)
  assert.equal(daemon([green(0, true), green(29_999, true)]), false)
})

test('daemon path: the pair must be one head, unbroken by a non-green, with no shrinking set', () => {
  assert.equal(daemon([green(0, true, { head: B }), green(30_000, true)]), false)
  assert.equal(daemon([green(0, true), pending(10_000, true), green(40_000, true)]), false)
  assert.equal(daemon([green(0, true, { total: 5 }), green(30_000, true, { total: 4 })]), false)
})

test('gh path: two consecutive greens confirm, as before', () => {
  const gh = (readings) => confirmsGreen({ readings })
  assert.equal(gh([green(0, false), green(1_000, false)]), true)
  assert.equal(gh([green(0, false, { head: B }), green(1_000, false)]), false)
})

test('freshGreenAnchor: the earliest fresh green of the trailing green run, else null', () => {
  assert.equal(freshGreenAnchor([green(0, false)]), null)
  assert.equal(
    freshGreenAnchor([pending(0, true), green(5_000, true), green(9_000, true)]).atMs,
    T0 + 5_000,
  )
  assert.equal(freshGreenAnchor([green(0, true), pending(5_000, true), green(9_000, false)]), null)
})

test('daemon path: a set that shrank after any earlier green does not confirm', () => {
  const fresh = (atMs, total) => green(atMs, true, { total })
  assert.equal(daemon([fresh(0, 4), fresh(30_000, 8), fresh(60_000, 4)]), false)
  assert.equal(daemon([fresh(0, 4), green(10_000, false, { total: 8 }), fresh(40_000, 4)]), false)
  assert.equal(daemon([fresh(0, 4), fresh(30_000, 8), fresh(60_000, 8)]), true)
})

test('freshGreenAnchor: restarts after a shrinking set, so the confirming gap is a full interval', () => {
  const readings = [green(0, true, { total: 8 }), green(30_000, true, { total: 4 })]
  assert.equal(freshGreenAnchor(readings).atMs, T0 + 30_000)
})
