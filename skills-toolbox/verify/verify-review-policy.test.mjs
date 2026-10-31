import assert from 'node:assert/strict'
import test from 'node:test'
import { reviewPolicyFromBlock } from './verify-review-policy.mjs'

test('new areas keep config order and new keys need no deprecation', () => {
  assert.deepEqual(
    reviewPolicyFromBlock({
      alwaysReview: { 'API schema': ['api/**'], Empty: [], UI: ['web/**'] },
      neverReview: ['vendor/**'],
    }),
    {
      alwaysReview: [
        {
          name: 'API schema',
          globs: ['api/**'],
          parkCode: 'always-human-path',
          label: 'API schema',
        },
        { name: 'Empty', globs: [], parkCode: 'always-human-path', label: 'Empty' },
        { name: 'UI', globs: ['web/**'], parkCode: 'always-human-path', label: 'UI' },
      ],
      neverReview: ['vendor/**'],
      deprecations: [],
    },
  )
})
test('legacy lists group labels and preserve codes after the new areas', () => {
  const block = Object.freeze({
    alwaysReview: Object.freeze({ New: Object.freeze(['new/**']) }),
    alwaysHumanPaths: Object.freeze([
      'a/**',
      { glob: 'b/**', label: ' Billing ' },
      { glob: 'c/**', label: 'Billing' },
    ]),
    humanReview: Object.freeze({
      database: ['db/**'],
      security: [{ glob: 'auth/**', label: 'Auth' }],
      ui: ['web/**'],
    }),
  })
  const r = reviewPolicyFromBlock(block)
  assert.deepEqual(
    r.alwaysReview.map((a) => [a.name, a.globs, a.parkCode, a.label]),
    [
      ['New', ['new/**'], 'always-human-path', 'New'],
      ['Always review', ['a/**'], 'always-human-path', undefined],
      ['Billing', ['b/**', 'c/**'], 'always-human-path', 'Billing'],
      ['Database', ['db/**'], 'human-review:database', undefined],
      ['Auth', ['auth/**'], 'human-review:security', 'Auth'],
      ['UI', ['web/**'], 'human-review:ui', undefined],
    ],
  )
  assert.equal(r.deprecations.length, 2)
  assert.match(r.deprecations[0], /alwaysHumanPaths.*deprecated.*alwaysReview/)
  assert.match(r.deprecations[1], /humanReview.*deprecated.*alwaysReview/)
})
test('legacy presence emits deprecations even for empty or malformed values', () => {
  for (const value of [null, [], {}, 'bad']) {
    assert.equal(reviewPolicyFromBlock({ alwaysHumanPaths: value }).deprecations.length, 1)
    assert.equal(reviewPolicyFromBlock({ humanReview: value }).deprecations.length, 1)
  }
})
test('malformed blocks and entries are harmless and well formed siblings survive', () => {
  for (const block of [undefined, null, 1, 'x', []])
    assert.deepEqual(reviewPolicyFromBlock(block), {
      alwaysReview: [],
      neverReview: [],
      deprecations: [],
    })
  const r = reviewPolicyFromBlock({
    alwaysReview: {
      '': ['bad'],
      'bad\nname': ['bad'],
      Good: ['yes', '', 7, { glob: 'no' }],
      Wrong: 'x',
    },
    neverReview: ['ok', '', null],
  })
  assert.deepEqual(
    r.alwaysReview.map((a) => a.globs),
    [['yes']],
  )
  assert.deepEqual(r.neverReview, ['ok'])
})
