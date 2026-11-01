import test from 'node:test'
import assert from 'node:assert/strict'
import { predictPathParks } from './verify-path-parks.mjs'
import { reviewPolicyFromBlock } from './verify-review-policy.mjs'
import { decideVerdict } from './verify-gate.mjs'

const review = reviewPolicyFromBlock({
  alwaysHumanPaths: [{ glob: 'judge/**', label: 'The judge' }],
  humanReview: { security: ['wire/**'], ui: ['screens/**'] },
})
for (const paths of [
  ['judge/main.mjs'],
  ['wire/types.proto'],
  ['screens/view.tsx'],
  ['.boss-skills.json'],
  ['plain/file.txt'],
  ['judge/main.mjs', 'wire/types.proto', '.boss-skills.json'],
]) {
  test(`path prediction matches the current judge: ${paths.join(', ')}`, () => {
    const prediction = predictPathParks(paths, { review })
    const verdict = decideVerdict({
      ...prediction,
      review,
      changedPaths: paths,
      changedLines: 0,
      sizeBaseline: { ok: true, thresholds: null },
      prState: 'OPEN',
      isDraft: false,
      ci: { settled: true, green: true },
      receipt: 'success',
      extensions: [],
    })
    assert.deepEqual(prediction.codes, verdict.reasons ?? [])
    assert.deepEqual(prediction.labels, verdict.labels ?? [])
  })
}
test('empty policy does not park its surface; applicability scripts do with a policy', () => {
  assert.equal(
    predictPathParks(['.boss-skills.json'], { review: reviewPolicyFromBlock(null) })
      .policySurfaceHit,
    false,
  )
  assert.equal(
    predictPathParks(['checks/applies.mjs'], { review }, { appliesScripts: ['checks/applies.mjs'] })
      .policySurfaceHit,
    true,
  )
})
