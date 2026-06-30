#!/usr/bin/env node

import assert from 'node:assert/strict'
import test from 'node:test'

import { classifyCronBranch } from './prune-cron-branches.mjs'

test('keeps a branch with a live bossd session even if merged', () => {
  const r = classifyCronBranch({ hasLiveSession: true, prState: 'MERGED', mergedIntoBase: true })
  assert.equal(r.action, 'keep')
})

test('keeps a branch with an open PR', () => {
  const r = classifyCronBranch({ hasLiveSession: false, prState: 'OPEN', mergedIntoBase: false })
  assert.equal(r.action, 'keep')
})

test('deletes a branch whose PR is merged', () => {
  const r = classifyCronBranch({ hasLiveSession: false, prState: 'MERGED', mergedIntoBase: false })
  assert.equal(r.action, 'delete')
})

test('deletes a branch whose tip is merged into base (no PR)', () => {
  const r = classifyCronBranch({ hasLiveSession: false, prState: 'NONE', mergedIntoBase: true })
  assert.equal(r.action, 'delete')
})

test('flags a closed-unmerged PR for review, never auto-deletes', () => {
  const r = classifyCronBranch({ hasLiveSession: false, prState: 'CLOSED', mergedIntoBase: false })
  assert.equal(r.action, 'review')
})

test('flags an orphan with unmerged commits and no PR for review', () => {
  const r = classifyCronBranch({ hasLiveSession: false, prState: 'NONE', mergedIntoBase: false })
  assert.equal(r.action, 'review')
})

test('unknown PR state with unmerged commits is reviewed, not deleted', () => {
  const r = classifyCronBranch({
    hasLiveSession: false,
    prState: 'UNKNOWN',
    mergedIntoBase: false,
  })
  assert.equal(r.action, 'review')
})

test('live-session guard wins over an open PR (kept regardless)', () => {
  const r = classifyCronBranch({ hasLiveSession: true, prState: 'OPEN', mergedIntoBase: false })
  assert.equal(r.action, 'keep')
})
