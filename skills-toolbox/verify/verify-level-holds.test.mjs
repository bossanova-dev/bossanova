import { test } from 'node:test'
import assert from 'node:assert/strict'
import { levelConfidence, holdFacts } from './verify-level.mjs'
import { proseFacts } from './verify-merge-prose.mjs'

const gate = {
  changedPaths: [],
  areas: [],
}
const rule = {
  decision: 'human',
  rule: 'Billing changes need review',
  reason: 'Changes billing',
  path: 'billing.js',
  verified: true,
}

test('human policy decisions and unreadable policy facts explain holds', () => {
  const result = levelConfidence(gate)
  assert.equal(Object.hasOwn(result, 'holds'), false)
  for (const verified of [true, false]) {
    const decision = { ...rule, verified }
    const facts = {
      ...gate,
      prosePolicy: {
        state: 'ok',
        decisions: [decision, { ...rule, decision: 'auto' }],
        stale: false,
      },
    }
    assert.deepEqual(holdFacts(facts, result), {
      policy: [{ kind: 'rule', ...decision }].map(({ decision, ...entry }) => entry),
      describers: [],
    })
    assert.deepEqual(levelConfidence(facts).holds, holdFacts(facts, result))
  }
  const policy = { state: 'unreadable', path: 'policy.md', why: 'missing' }
  const prose = proseFacts(policy, [], { headSha: 'a'.repeat(40), files: [] })
  assert.deepEqual(holdFacts({ ...gate, prosePolicy: prose }, result), {
    policy: [{ kind: 'unreadable', path: 'policy.md', why: 'missing' }],
    describers: [],
  })
})

test('only asking areas carry holding describers and bound their items', () => {
  const area = {
    area: 'api-contract',
    label: 'API contract',
    defaultAsk: 'always',
    describers: [
      { extension: 'proto', outcome: 'breaking', items: ['one', 'two', 'three', 'four'] },
      { extension: 'schema', outcome: 'failed', reason: 'exit:1' },
    ],
  }
  const result = levelConfidence({ ...gate, areas: [area] })
  assert.deepEqual(result.holds.describers, [
    {
      area: 'api-contract',
      label: 'API contract',
      code: 'human-review:api-contract',
      extension: 'proto',
      outcome: 'breaking',
      items: ['one', 'two', 'three'],
      more: 1,
    },
    {
      area: 'api-contract',
      label: 'API contract',
      code: 'human-review:api-contract',
      extension: 'schema',
      outcome: 'failed',
      reason: 'exit:1',
    },
  ])
  assert.equal(holdFacts({ ...gate, areas: [area] }, { ...result, asks: [] }), null)
  assert.equal(
    holdFacts(
      {
        ...gate,
        areas: Array(20).fill(area),
        prosePolicy: { state: 'ok', decisions: Array(30).fill(rule) },
      },
      result,
    ).policy.length,
    16,
  )
  assert.equal(holdFacts({ ...gate, areas: Array(20).fill(area) }, result).describers.length, 8)
})
