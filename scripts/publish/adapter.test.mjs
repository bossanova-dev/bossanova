import assert from 'node:assert/strict'
import test from 'node:test'

import { PUBLISH_CAPABILITIES, assertConforms, resolvePublishAdapter } from './adapter.mjs'

test('resolvePublishAdapter defaults to the r2 adapter', () => {
  const adapter = resolvePublishAdapter({
    BOSS_PROOF_R2_BUCKET: 'bucket',
    BOSS_PROOF_PUBLIC_BASE_URL: 'https://example.test',
  })
  assert.equal(adapter.name, 'r2')
  assert.equal(typeof adapter.publish, 'function')
})

test('resolvePublishAdapter honours an explicit PLAN_PUBLISH=r2', () => {
  const adapter = resolvePublishAdapter({ PLAN_PUBLISH: 'r2', BOSS_PROOF_R2_BUCKET: 'b' })
  assert.equal(adapter.name, 'r2')
})

test('resolvePublishAdapter throws for an unregistered adapter', () => {
  assert.throws(
    () => resolvePublishAdapter({ PLAN_PUBLISH: 'nope' }),
    /unknown publish adapter: nope/,
  )
})

test('PUBLISH_CAPABILITIES lists the publish method', () => {
  assert.deepEqual(PUBLISH_CAPABILITIES, ['publish'])
})

test('assertConforms rejects an adapter missing publish', () => {
  assert.throws(() => assertConforms({ name: 'x' }), /missing capability: publish/)
})

test('assertConforms returns a conforming adapter', () => {
  const adapter = { name: 'x', publish: async () => ({ url: '', key: '' }) }
  assert.equal(assertConforms(adapter), adapter)
})
