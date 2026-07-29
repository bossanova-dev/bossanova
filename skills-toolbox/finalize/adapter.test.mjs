// skills-toolbox/finalize/adapter.test.mjs
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { resolveFinalizeAdapter, FINALIZE_CAPABILITIES, assertConforms } from './adapter.mjs'

test('resolveFinalizeAdapter defaults to the boss-finalize adapter', () => {
  const a = resolveFinalizeAdapter({})
  assert.equal(a.finalize, 'boss-finalize')
})

test('resolveFinalizeAdapter honours an explicit FINALIZE=boss-finalize', () => {
  const a = resolveFinalizeAdapter({ FINALIZE: 'boss-finalize' })
  assert.equal(a.finalize, 'boss-finalize')
})

test('resolveFinalizeAdapter throws on an unknown FINALIZE', () => {
  assert.throws(() => resolveFinalizeAdapter({ FINALIZE: 'nope' }), /unknown finalize: nope/)
})

test('the boss-finalize adapter conforms to the capability manifest', () => {
  const a = resolveFinalizeAdapter({})
  assert.deepEqual(FINALIZE_CAPABILITIES, ['injectPrTag', 'readyPr', 'repair'])
  assert.equal(assertConforms(a), a)
})

test('assertConforms rejects an adapter missing injectPrTag', () => {
  assert.throws(
    () => assertConforms({ operationMap: { readyPr: {}, repair: {} }, policy: { repairCap: 5 } }),
    /missing method: injectPrTag/,
  )
})

test('assertConforms rejects an adapter missing an operation-map capability', () => {
  assert.throws(
    () =>
      assertConforms({ injectPrTag() {}, operationMap: { readyPr: {} }, policy: { repairCap: 5 } }),
    /missing operation: repair/,
  )
})

test('assertConforms rejects an adapter missing policy.repairCap', () => {
  assert.throws(
    () =>
      assertConforms({
        injectPrTag() {},
        operationMap: { readyPr: {}, repair: {} },
        policy: {},
      }),
    /missing policy\.repairCap/,
  )
})
