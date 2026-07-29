#!/usr/bin/env node

import assert from 'node:assert/strict'
import test from 'node:test'

import { issueSlug } from './plan-upload.mjs'

test('issueSlug lowercases, hyphenates, and upcases the id', () => {
  assert.equal(
    issueSlug('bos-5', 'Add an unsubscribe mechanism!'),
    'BOS-5-add-an-unsubscribe-mechanism',
  )
})

test('issueSlug collapses runs of non-alphanumerics and trims edges', () => {
  assert.equal(issueSlug('BOS-12', '  Review   the   DB schema  '), 'BOS-12-review-the-db-schema')
})
