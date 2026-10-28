import assert from 'node:assert/strict'
import test from 'node:test'
import { selectTargets } from './select-affected-tests.mjs'

test('partial-marker golden edits exercise both Go and JavaScript readers', () => {
  assert.deepEqual(
    selectTargets(['lib/bossalib/kit/vcs/testdata/partial_marker_cases.json']).map(
      ({ target }) => target,
    ),
    ['test-scripts', 'test-bossalib'],
  )
})
