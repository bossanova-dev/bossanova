import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  assertFalsifiable,
  assertPolarityBounded,
  assertProhibitionFires,
} from './prose-pin-lib.mjs'

test('assertFalsifiable passes when a mutation breaks the pin', () => {
  assert.doesNotThrow(() =>
    assertFalsifiable({
      source: 'The worker must not push.',
      pattern: /must not push/,
      mutation: { find: 'must not push', replacement: 'may push' },
      label: 'push rule',
    }),
  )
})

test('assertFalsifiable fails when a pin survives its mutation', () => {
  assert.throws(
    () =>
      assertFalsifiable({
        source: 'The worker must not push. The worker must not push.',
        pattern: /must not push/,
        mutation: { find: 'The worker must not push.', replacement: 'The worker may push.' },
        label: 'duplicate push rule',
      }),
    /survived its falsifying mutation/,
  )
})

test('assertFalsifiable requires a mutation argument', () => {
  assert.throws(
    () =>
      assertFalsifiable({
        source: 'The worker must not push.',
        pattern: /must not push/,
      }),
    /requires a mutation mutation object/,
  )
})

test('assertFalsifiable inherits mutate fail-closed behavior for no-op substitutions', () => {
  assert.throws(
    () =>
      assertFalsifiable({
        source: 'The worker must not push.',
        pattern: /must not push/,
        mutation: { find: 'not present', replacement: 'may push' },
      }),
    /not present/,
  )
})

test('assertProhibitionFires passes when a planted violation trips the prohibition', () => {
  assert.doesNotThrow(() =>
    assertProhibitionFires({
      source: 'No shipping workflow here.',
      pattern: /git push/,
      violation: { find: 'No shipping workflow here.', replacement: 'Run git push here.' },
      label: 'push prohibition',
    }),
  )
})

test('assertProhibitionFires fails when a planted violation is invisible', () => {
  assert.throws(
    () =>
      assertProhibitionFires({
        source: 'No shipping workflow here.',
        pattern: /git push/,
        violation: { find: 'No shipping workflow here.', replacement: 'Run gh pr view here.' },
        label: 'push prohibition',
      }),
    /planted violation did not trip/,
  )
})

test('assertProhibitionFires requires a violation argument', () => {
  assert.throws(
    () =>
      assertProhibitionFires({
        source: 'No shipping workflow here.',
        pattern: /git push/,
      }),
    /requires a violation mutation object/,
  )
})

test('assertPolarityBounded accepts the forbidden polarity inside the bounded section', () => {
  assert.doesNotThrow(() =>
    assertPolarityBounded({
      source: '## Rule\nNever run git push.\n\n## Other\nYou may run git push elsewhere.',
      heading: '## Rule',
      rule: /git push/,
      polarity: /Never/,
      label: 'push boundary',
    }),
  )
})

test('assertPolarityBounded rejects a permitting polarity in the rule section', () => {
  assert.throws(
    () =>
      assertPolarityBounded({
        source: '## Rule\nYou may run git push.\n\n## Other\nNever run git push elsewhere.',
        heading: '## Rule',
        rule: /git push/,
        polarity: /Never/,
        label: 'push boundary',
      }),
    /bounded section must state forbidden polarity/,
  )
})
