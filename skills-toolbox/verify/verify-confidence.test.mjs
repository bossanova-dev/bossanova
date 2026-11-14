import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  confidence,
  LEVELS,
  STATES,
  MAX_REASONS,
  MAX_ASKS_BEFORE_LOW,
} from './verify-confidence.mjs'
import { reviewAreas } from './verify-areas.mjs'

const base = () => ({ evidence: { review: 'ran', collectors: [] } })
const area = (area = 'migrations', defaultAsk = 'always') => ({
  area,
  label: area,
  defaultAsk,
  paths: [],
})

test('ordered confidence steps and monotone caps', () => {
  for (const [facts, level, codes] of [
    [{}, 'high', []],
    [{ blockers: [{ code: 'ci', label: 'CI red' }] }, 'blocked', ['blocker']],
    [{ evidence: { review: 'missing' } }, 'cannot-assess', ['evidence-missing']],
    [
      { evidence: { collectors: [{ name: 'history', ok: false }] } },
      'cannot-assess',
      ['evidence-missing'],
    ],
    [{ areas: [area()] }, 'moderate', ['area-asks']],
    [{ areas: ['a', 'b', 'c', 'd'].map((a) => area(a)) }, 'low', ['area-asks', 'asks-over-3']],
    [{ proof: { behavior: 'unproven' } }, 'moderate', ['unproven']],
  ]) {
    const result = confidence({ ...base(), ...facts })
    assert.equal(result.level, level)
    assert.deepEqual(result.loweredBy, codes)
  }
  assert.equal(confidence({}).level, 'cannot-assess')
  assert.equal(confidence({ blockers: [{ code: 'ci', label: 'CI red' }] }).level, 'blocked')
  assert.equal(
    confidence({ areas: ['a', 'b', 'c', 'd'].map((a) => area(a)) }).level,
    'cannot-assess',
  )
})

test('a head with no asks is high whatever its size, and a recorded size fact is ignored', () => {
  const recorded = {
    changedLines: 500_000,
    baseline: { ok: true, thresholds: { p90: 100, p99: 200 } },
  }
  for (const size of [recorded, { changedLines: 0, baseline: { ok: false, error: 'offline' } }]) {
    const result = confidence({ ...base(), size })
    assert.equal(result.level, 'high', JSON.stringify(size))
    assert.deepEqual([result.loweredBy, result.reasons, result.health], [[], [], []])
  }
})

test('missing or unreadable evidence is cannot-assess, never high', () => {
  const assessable = base()
  assert.equal(confidence(assessable).level, 'high')
  for (const facts of [
    { ...assessable, evidence: { review: 'skipped', collectors: [] } },
    { ...assessable, evidence: { collectors: [] } },
    { ...assessable, evidence: {} },
    { ...assessable, evidence: { review: 'ran', collectors: [{ name: 'x', ok: false }] } },
  ]) {
    const result = confidence(facts)
    assert.equal(result.level, 'cannot-assess', JSON.stringify(facts))
    assert.deepEqual(result.loweredBy, ['evidence-missing'])
  }
})

test('area policy asks even on short circuit, deduplicates and uses UI proof', () => {
  for (const ui of ['none', undefined]) {
    const result = confidence({
      ...base(),
      areas: [area('ui', 'unless-proven'), area('ui', 'always'), area('other', 'never')],
      proof: { ui },
    })
    assert.equal(result.level, 'moderate')
    assert.deepEqual(result.asks, [{ area: 'ui', label: 'ui', source: 'builtin' }])
  }
  assert.equal(
    confidence({ ...base(), areas: [area('ui', 'unless-proven')], proof: { ui: 'tests' } }).level,
    'high',
  )
  assert.equal(
    confidence({ ...base(), areas: [area('migrations')], proof: { ui: 'tests' } }).asks.length,
    1,
  )
  assert.equal(confidence({ areas: [area()] }).asks.length, 1)
  assert.equal(
    confidence({ blockers: [{ code: 'ci', label: 'CI red' }], areas: [area()] }).asks.length,
    1,
  )
  const result = confidence({
    ...base(),
    areas: ['a', 'b', 'c'].map((a) => area(a)),
    proof: { behavior: 'unproven' },
  })
  assert.equal(result.level, 'low')
  assert.ok(result.loweredBy.includes('asks-over-3'))
  assert.equal(result.asks.at(-1).area, 'unproven-behavior')
})

test('reasons prioritize the final level and name their facts', () => {
  const result = confidence({
    ...base(),
    areas: [area('Migrations'), area('API schema'), area('Infra'), area('Auth')],
  })
  assert.equal(result.reasons.length, MAX_REASONS)
  assert.match(result.reasons[0], /4 areas/)
  assert.ok(result.reasons.some((reason) => reason.includes('Migrations')))
  const config = confidence({
    ...base(),
    areas: [{ ...area('schema'), label: 'API schema', source: 'config' }],
  })
  assert.match(config.reasons[0], /API schema/)
  const bump = confidence({
    ...base(),
    areas: [{ ...area('dependencies'), bumps: [{ name: 'react', from: '18', to: '19' }] }],
  })
  assert.match(bump.reasons[0], /react.*18.*19/)
  const askHeavy = confidence({ ...base(), areas: ['a', 'b', 'c', 'd', 'e'].map((a) => area(a)) })
  assert.match(askHeavy.reasons[0], /5/)
})

test('duplicate asking areas retain the first reason metadata', () => {
  const result = confidence({
    ...base(),
    areas: [
      { ...area('dependencies'), bumps: [{ name: 'react', from: '18', to: '19' }] },
      { ...area('dependencies'), bumps: [{ name: 'vue', from: '2', to: '3' }] },
    ],
  })
  assert.equal(result.asks.length, 1)
  assert.match(result.reasons[0], /react.*18.*19/)
})

test('approval and self report never count; warnings never cap', () => {
  const facts = {
    ...base(),
    secondModel: { ran: false },
  }
  const result = confidence(facts)
  assert.equal(result.level, 'high')
  assert.deepEqual(result.health, ['second-model-skipped'])
  assert.deepEqual(
    confidence({ ...facts, approved: true, selfConfidence: 'low', arbitrary: 99 }),
    result,
  )
  assert.deepEqual(LEVELS, ['high', 'moderate', 'low'])
  assert.deepEqual(STATES, ['blocked', 'cannot-assess'])
  assert.equal(MAX_ASKS_BEFORE_LOW, 3)
  assert.ok(Object.isFrozen(LEVELS) && Object.isFrozen(STATES))
})

test('pure deterministic evaluation accepts deeply frozen input and stable step codes', () => {
  const freeze = (value) => {
    if (value && typeof value === 'object') {
      Object.values(value).forEach(freeze)
      Object.freeze(value)
    }
    return value
  }
  const facts = freeze({
    ...base(),
    areas: [area('a'), area('b')],
    blockers: [
      { code: 'b', label: 'B' },
      { code: 'a', label: 'A' },
    ],
  })
  const result = confidence(facts)
  assert.deepEqual(confidence(facts), result)
  const shuffled = confidence({
    ...facts,
    areas: [...facts.areas].reverse(),
    blockers: [...facts.blockers].reverse(),
  })
  for (const key of ['level', 'loweredBy', 'health']) assert.deepEqual(shuffled[key], result[key])
})

test('fresh-claim schema changes ask through the real area policy', () => {
  const files = [
    'lib/fresh-claim-graphql/core/gql.ts',
    'lib/fresh-claim-graphql/core/graphql.ts',
    'lib/fresh-claim-queries/Hospital.ts',
    'services/admin/src/components/domain/hospitals/EditHospitalContactModal.test.tsx',
    'services/admin/src/components/domain/hospitals/EditHospitalContactModal.tsx',
    'services/admin/src/components/domain/hospitals/HospitalTable.test.tsx',
    'services/admin/src/components/domain/hospitals/HospitalTable.tsx',
    'services/admin/src/components/domain/hospitals/index.ts',
    'services/admin/src/layouts/Application.tsx',
    'services/admin/src/pages/[organizationId]/settings/hospitals.tsx',
    'services/admin/src/services/Routes.ts',
    'services/api/api/graph/generated/generated.go',
    'services/api/api/graph/policy/hospital_policy.go',
    'services/api/api/graph/policy/hospital_policy_test.go',
    'services/api/api/graph/resolver/hospital.resolvers.go',
    'services/api/api/graph/resolver/hospital_resolvers_test.go',
    'services/api/api/graph/resolver/schema.resolvers.go',
    'services/api/api/graph/schema/hospital.graphqls',
    'services/api/api/graph/schema/schema.graphqls',
    'services/api/entity/hospital.go',
    'services/api/internal/migration/seeds/hospitals.go',
    'services/api/internal/migration/seeds/hospitals_db_test.go',
    'services/api/internal/service/claim_patient_email.go',
    'services/api/internal/service/email_address_normalize.go',
    'services/api/internal/service/hospital_update_claim_number_request_email.go',
    'services/api/internal/service/hospital_update_claim_number_request_email_test.go',
    'services/api/internal/storage/hospital.go',
  ]
  const policy = { alwaysReview: [], noSignOff: [] }
  const { areas } = reviewAreas(files, policy)
  const facts = { ...base(), areas, proof: { ui: 'tests' } }
  assert.equal(confidence(facts).level, 'moderate')
  assert.ok(confidence(facts).asks.some((a) => a.area === 'api-contract'))
  assert.equal(
    confidence({
      ...facts,
      areas: areas.map((a) => (a.area === 'api-contract' ? { ...a, defaultAsk: 'never' } : a)),
    }).level,
    'high',
  )
})

test('backtest pins the later-fix rate of every confidence outcome', () => {
  const fixture = JSON.parse(
    readFileSync(
      new URL('../../scripts/fixtures/verify-confidence/backtest.json', import.meta.url),
      'utf8',
    ),
  )
  const table = Object.fromEntries(
    [...LEVELS, ...STATES].map((level) => [level, { n: 0, laterFix: 0, rate: 0 }]),
  )
  for (const row of fixture.rows) {
    const result = confidence({
      blockers: row.blockers,
      evidence: { review: row.reviewRan ? 'ran' : 'missing', collectors: [] },
      areas: row.areas.map((entry) => ({ ...entry, label: entry.area })),
      proof: {
        ui: row.unprovenUi
          ? 'none'
          : row.areas.some((entry) => entry.area === 'ui')
            ? 'tests'
            : 'none',
        behavior: row.unprovenBehavior ? 'none' : 'none',
      },
      secondModel: { ran: row.secondModelRan },
    })
    table[result.level].n++
    table[result.level].laterFix += Number(row.laterFix)
  }
  for (const row of Object.values(table))
    row.rate = row.n ? Number(((100 * row.laterFix) / row.n).toFixed(2)) : 0
  const expected = {
    high: { n: 156, laterFix: 4, rate: 2.56 },
    moderate: { n: 288, laterFix: 10, rate: 3.47 },
    low: { n: 2, laterFix: 0, rate: 0 },
    blocked: { n: 49, laterFix: 5, rate: 10.2 },
    'cannot-assess': { n: 505, laterFix: 63, rate: 12.48 },
  }
  assert.deepEqual(
    table,
    expected,
    'Confidence rule changed its historical outcomes. Old and new tables:\n' +
      JSON.stringify({ old: expected, new: table }, null, 2),
  )
})

test('both UI proof kinds lift only the unless-proven ask and unproven cap', () => {
  for (const ui of ['tests', 'recording']) {
    const result = confidence({ ...base(), areas: [area('ui', 'unless-proven')], proof: { ui } })
    assert.equal(result.level, 'high')
    assert.deepEqual(result.asks, [])
    assert.deepEqual(result.loweredBy, [])
    const always = confidence({ ...base(), areas: [area('ui', 'always')], proof: { ui } })
    assert.equal(always.asks.length, 1)
    assert.equal(always.loweredBy.includes('unproven'), false)
  }
})
