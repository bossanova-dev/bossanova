import { test } from 'node:test'
import assert from 'node:assert/strict'
import { listPlanCandidates, selectPlanCandidates } from './plan-candidates.mjs'
import { formatClaimComment } from '../linear-claim.mjs'
import { runCli } from './cli.mjs'
import { DEFAULT_CONFIG, mergeConfig } from '../skill-config.mjs'

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
    excluded: [
      { identifier: 'APP-1', reason: 'live-plan-claim', claimCreatedAt: '2026-11-08T11:00:00Z' },
    ],
    claimAware: true,
  })
})

test('reference exclusions persist across sweeps and clear with the tolerance or description', async () => {
  const rows = [{ identifier: 'APP-1', description: 'See APP-2 and #3.' }]
  const adapter = {
    referenceRewrites: ['bare-reference-autolinking', 'pull-request-reference-autolinking'],
    selectPlanned: async () => rows,
  }
  const options = { tolerated: new Set(['bare-reference-autolinking']) }
  for (let sweep = 0; sweep < 2; sweep += 1) {
    const result = await selectPlanCandidates(adapter, {}, options)
    assert.deepEqual(result.candidates, [])
    assert.deepEqual(result.excluded, [
      {
        identifier: 'APP-1',
        reason: 'unmodelled-references',
        tokens: [{ token: '#3', transform: 'pull-request-reference-autolinking' }],
      },
    ])
  }
  assert.deepEqual(
    (
      await selectPlanCandidates(
        adapter,
        {},
        {
          tolerated: new Set(adapter.referenceRewrites),
        },
      )
    ).candidates,
    rows,
  )
  assert.deepEqual((await selectPlanCandidates(adapter, {})).candidates, rows)
  assert.deepEqual(
    (await selectPlanCandidates({ ...adapter, referenceRewrites: undefined }, {}, options))
      .candidates,
    rows,
  )
  rows[0].description = 'See PR 3.'
  assert.deepEqual((await selectPlanCandidates(adapter, {}, options)).candidates, rows)
})

test('selection keeps references in multiline code containing definition-like text', async () => {
  const rows = [
    {
      identifier: 'APP-1',
      description: 'Literal `\n[ref]: https://example.com\nAPP-123 #123\nAnother `',
    },
  ]
  const adapter = {
    referenceRewrites: ['bare-reference-autolinking', 'pull-request-reference-autolinking'],
    selectPlanned: async () => rows,
  }
  assert.deepEqual(await selectPlanCandidates(adapter, {}, { tolerated: new Set() }), {
    candidates: rows,
    excluded: [],
    claimAware: false,
  })
})

test('selection keeps references in adjacent inline-link destinations', async () => {
  const rows = [
    {
      identifier: 'APP-1',
      description: '[first](https://example.com)[ref](APP-123)\n\n[ref]: https://example.com',
    },
  ]
  const adapter = {
    referenceRewrites: ['bare-reference-autolinking', 'pull-request-reference-autolinking'],
    selectPlanned: async () => rows,
  }
  assert.deepEqual(await selectPlanCandidates(adapter, {}, { tolerated: new Set() }), {
    candidates: rows,
    excluded: [],
    claimAware: false,
  })
})

test('claims take precedence over reference exclusions and diagnostics bound token output', async () => {
  const adapter = {
    planClaimSelection: true,
    referenceRewrites: ['bare-reference-autolinking', 'pull-request-reference-autolinking'],
    selectPlanned: async () => [
      {
        identifier: 'APP-1',
        description: '#1',
        planClaimComments: [claim('2026-11-08T11:00:00Z')],
      },
      { identifier: 'APP-2', description: '#1 #2 #3 #4 #5 #6 APP-99', planClaimComments: [] },
    ],
  }
  const options = { now, tolerated: new Set() }
  const result = await selectPlanCandidates(adapter, {}, options)
  assert.deepEqual(
    result.excluded.map((entry) => entry.reason),
    ['live-plan-claim', 'unmodelled-references'],
  )
  let stderr = ''
  assert.deepEqual(
    await listPlanCandidates(
      adapter,
      {},
      (text) => {
        stderr += text
      },
      options,
    ),
    [],
  )
  assert.match(stderr, /excluded APP-1 \(live peer plan claim/)
  assert.match(stderr, /excluded APP-2.*#1, #2, #3, #4, #5/)
  assert.doesNotMatch(stderr, /#6/)
  assert.match(stderr, /bare-reference-autolinking/)
  assert.match(stderr, /pull-request-reference-autolinking/)
  assert.match(stderr, /descriptionNormalization.tolerated.*edit the description/)
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

test('list-unplanned applies configured reference tolerances while list-planned keeps build candidates', async () => {
  const config = mergeConfig(DEFAULT_CONFIG, {
    trackerConfig: {
      linear: {
        team: 'Demo',
        descriptionNormalization: { tolerated: [] },
        states: { unplanned: 'Unplanned', planned: 'Todo' },
        labels: { agentPlan: 'agent-plan', agentBuild: 'agent-build', needsHuman: 'needs-human' },
      },
    },
  })
  const adapter = {
    referenceRewrites: ['pull-request-reference-autolinking'],
    selectPlanned: async () => [{ identifier: 'APP-1', description: '#42' }],
  }
  for (const verb of ['list-unplanned', 'list-planned']) {
    let out = '',
      err = ''
    const code = await runCli([verb], {
      resolveAdapter: () => adapter,
      loadConfig: () => config,
      env: {},
      write: (text) => {
        out += text
      },
      errWrite: (text) => {
        err += text
      },
    })
    assert.equal(code, 0)
    assert.equal(JSON.parse(out).length, verb === 'list-unplanned' ? 0 : 1)
    if (verb === 'list-unplanned') {
      assert.match(err, /APP-1.*#42.*pull-request-reference-autolinking/)
    }
  }
})
