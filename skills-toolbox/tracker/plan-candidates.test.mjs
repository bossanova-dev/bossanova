import { test } from 'node:test'
import assert from 'node:assert/strict'
import { selectPlanCandidates } from './plan-candidates.mjs'
import { formatClaimComment } from '../linear-claim.mjs'

const now = '2026-11-08T12:00:00Z'
const claim = (createdAt, scope = 'plan') => ({
  id: '1',
  body: formatClaimComment('a'.repeat(32), null, { scope }),
  createdAt,
})
test('selection excludes live plan claims, retains forfeited and build claims, and strips comments', async () => {
  const rows = [
    { identifier: 'APP-1', planClaimComments: [claim('2026-11-08T11:00:00Z')] },
    { identifier: 'APP-2', planClaimComments: [claim('2026-11-08T09:00:00Z')] },
    { identifier: 'APP-3', planClaimComments: [claim('2026-11-08T11:00:00Z', 'build')] },
  ]
  const adapter = {
    planClaimSelection: true,
    selectPlanned: async (query) => {
      assert.deepEqual(query, { state: 'Unplanned', planClaims: true })
      return rows
    },
  }
  assert.deepEqual(await selectPlanCandidates(adapter, { state: 'Unplanned' }, { now }), {
    candidates: [{ identifier: 'APP-2' }, { identifier: 'APP-3' }],
    excluded: [{ identifier: 'APP-1', claimCreatedAt: '2026-11-08T11:00:00Z' }],
    claimAware: true,
  })
})
test('selection refuses missing, malformed comments and invalid claim dates', async () => {
  for (const row of [{}, { planClaimComments: null }, { planClaimComments: [claim('invalid')] }]) {
    await assert.rejects(
      selectPlanCandidates(
        { planClaimSelection: true, selectPlanned: async () => [row] },
        {},
        { now },
      ),
    )
  }
})
test('undeclared adapters receive the original query unchanged', async () => {
  const query = { state: 'Unplanned' }
  const adapter = {
    selectPlanned: async (received) => {
      assert.equal(received, query)
      return [{ identifier: 'APP-1' }]
    },
  }
  assert.deepEqual(await selectPlanCandidates(adapter, query), {
    candidates: [{ identifier: 'APP-1' }],
    excluded: [],
    claimAware: false,
  })
})
