import { test } from 'node:test'
import assert from 'node:assert/strict'

import { cronHitRate } from './cron-hit-rate.mjs'

test('cronHitRate composes expected fires, observed hits, and ratio', () => {
  assert.deepEqual(
    cronHitRate({
      slug: 'bs-sweep-debt',
      schedule: '@daily',
      from: '2026-06-01T00:00:00.000Z',
      to: '2026-06-08T00:00:00.000Z',
      observedHits: 2,
    }),
    {
      slug: 'bs-sweep-debt',
      schedule: '@daily',
      expectedFires: 7,
      observedHits: 2,
      hitRate: 2 / 7,
    },
  )
})

test('cronHitRate keeps hitRate null when expected fire count is unknown', () => {
  assert.deepEqual(
    cronHitRate({
      slug: 'bs-sweep-debt',
      schedule: '@bogus',
      from: '2026-06-01T00:00:00.000Z',
      to: '2026-06-08T00:00:00.000Z',
      observedHits: 2,
    }),
    {
      slug: 'bs-sweep-debt',
      schedule: '@bogus',
      expectedFires: null,
      observedHits: 2,
      hitRate: null,
    },
  )
})
