// skills-toolbox/plan-slug.test.mjs
import assert from 'node:assert/strict'
import test from 'node:test'

import { issueSlug } from './plan-slug.mjs'

test('issueSlug lowercases, hyphenates, and upcases the id', () => {
  assert.equal(
    issueSlug('bos-5', 'Add an unsubscribe mechanism!'),
    'BOS-5-add-an-unsubscribe-mechanism',
  )
})

test('issueSlug collapses runs of non-alphanumerics and trims edges', () => {
  assert.equal(issueSlug('BOS-12', '  Review   the   DB schema  '), 'BOS-12-review-the-db-schema')
})

test('issueSlug leaves an already-uppercase id unchanged', () => {
  assert.equal(issueSlug('XYZ-9', 'Ship the thing!'), 'XYZ-9-ship-the-thing')
})
