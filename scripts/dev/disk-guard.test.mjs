#!/usr/bin/env node
import assert from 'node:assert/strict'
import test from 'node:test'

import { guardReason } from './disk-guard.mjs'

const GiB = 1024 ** 3
const HOUR = 60 * 60 * 1000
const NOW = Date.UTC(2026, 9, 31)

test('cleans up when space is low and the last run was over six hours ago', () => {
  const reason = guardReason({
    freeBytes: 40 * GiB,
    minFreeBytes: 75 * GiB,
    lastRunMs: NOW - 7 * HOUR,
    now: NOW,
  })
  assert.equal(reason, 'low-space')
})

test('a low-space cleanup that could not recover waits six hours before retrying', () => {
  const reason = guardReason({
    freeBytes: 40 * GiB,
    minFreeBytes: 75 * GiB,
    lastRunMs: NOW - 5 * HOUR,
    now: NOW,
  })
  assert.equal(reason, null)
})

test('cleans up once a day even with plenty of space', () => {
  const reason = guardReason({
    freeBytes: 300 * GiB,
    minFreeBytes: 75 * GiB,
    lastRunMs: NOW - 25 * HOUR,
    now: NOW,
  })
  assert.equal(reason, 'scheduled')
})

test('waits while space is fine and the last run is recent', () => {
  const reason = guardReason({
    freeBytes: 300 * GiB,
    minFreeBytes: 75 * GiB,
    lastRunMs: NOW - 2 * HOUR,
    now: NOW,
  })
  assert.equal(reason, null)
})

test('a guard that has never run cleans up on its first start', () => {
  const reason = guardReason({
    freeBytes: 300 * GiB,
    minFreeBytes: 75 * GiB,
    lastRunMs: 0,
    now: NOW,
  })
  assert.equal(reason, 'scheduled')
})
