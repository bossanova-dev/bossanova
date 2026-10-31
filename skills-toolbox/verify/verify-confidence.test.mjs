import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  confidence,
  LEVELS,
  STATES,
  MAX_REASONS,
  MAX_ASKS_BEFORE_LOW,
  sizeThresholds,
} from './verify-confidence.mjs'
import { reviewAreas } from './verify-areas.mjs'

const size = (changedLines = 0, thresholds = { p90: 100, p99: 200 }) => ({
  changedLines,
  baseline: { ok: true, thresholds },
})
const base = () => ({ evidence: { review: 'ran', collectors: [] }, size: size() })
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
    [{ size: size(101) }, 'moderate', ['size-p90']],
    [{ size: size(201) }, 'low', ['size-p90', 'size-p99']],
    [{ areas: ['a', 'b', 'c', 'd'].map((a) => area(a)) }, 'low', ['area-asks', 'asks-over-3']],
    [{ proof: { behavior: 'unproven' } }, 'moderate', ['unproven']],
    [
      {
        size: size(201),
        proof: { behavior: 'unproven' },
      },
      'low',
      ['size-p90', 'size-p99', 'unproven'],
    ],
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
  assert.equal(confidence({ ...base(), size: size(100) }).level, 'high')
})

test('missing or unreadable evidence is cannot-assess, never high', () => {
  const assessable = base()
  assert.equal(confidence(assessable).level, 'high')
  for (const facts of [
    { ...assessable, evidence: { review: 'skipped', collectors: [] } },
    { ...assessable, evidence: { collectors: [] } },
    { ...assessable, evidence: {} },
    { evidence: assessable.evidence },
    ...[NaN, undefined, null, -1, 1.5, '10', Infinity].map((changedLines) => ({
      ...assessable,
      size: { ...size(), changedLines },
    })),
    { ...assessable, size: { changedLines: 0 } },
    { ...assessable, size: { changedLines: 0, baseline: { ok: false, error: 'offline' } } },
  ]) {
    const result = confidence(facts)
    assert.equal(result.level, 'cannot-assess', JSON.stringify(facts))
    assert.deepEqual(result.loweredBy, ['evidence-missing'])
  }
})

test('area policy asks even on short circuit, deduplicates and uses UI proof', () => {
  for (const ui of ['unproven', 'none', undefined]) {
    const result = confidence({
      ...base(),
      areas: [area('ui', 'unless-proven'), area('ui', 'always'), area('other', 'never')],
      proof: { ui },
    })
    assert.equal(result.level, 'moderate')
    assert.deepEqual(result.asks, [{ area: 'ui', label: 'ui', source: 'builtin' }])
  }
  assert.equal(
    confidence({ ...base(), areas: [area('ui', 'unless-proven')], proof: { ui: 'proven' } }).level,
    'high',
  )
  assert.equal(
    confidence({ ...base(), areas: [area('migrations')], proof: { ui: 'proven' } }).asks.length,
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
    areas: [area('Migrations'), area('API schema'), area('Infra')],
    size: size(2140),
  })
  assert.equal(result.reasons.length, MAX_REASONS)
  assert.match(result.reasons[0], /2,?140/)
  assert.match(result.reasons[0], /99/)
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
    size: size(10000, null),
  }
  const result = confidence(facts)
  assert.equal(result.level, 'high')
  assert.deepEqual(result.health, ['size-baseline-thin', 'second-model-skipped'])
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

test('size thresholds use nearest rank and ignore invalid observations', () => {
  assert.deepEqual(sizeThresholds(Array.from({ length: 100 }, (_, i) => i)), {
    p90: 89,
    p99: 98,
    sample: 100,
  })
  assert.equal(sizeThresholds([1, 2, 3]), null)
  assert.deepEqual(
    sizeThresholds([0, 10, 20, -1, NaN, Infinity, 1.5, '30', null], { minSample: 3 }),
    { p90: 20, p99: 20, sample: 3 },
  )
  assert.equal(sizeThresholds([], { minSample: 0 }), null)
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
  const policy = { alwaysReview: [], neverReview: [] }
  const { areas } = reviewAreas(files, policy)
  const facts = { ...base(), areas, proof: { ui: 'proven' } }
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
      size: size(row.changedLines, fixture.thresholds),
      proof: {
        ui: row.unprovenUi
          ? 'unproven'
          : row.areas.some((entry) => entry.area === 'ui')
            ? 'proven'
            : 'none',
        behavior: row.unprovenBehavior ? 'unproven' : 'none',
      },
      secondModel: { ran: row.secondModelRan },
    })
    table[result.level].n++
    table[result.level].laterFix += Number(row.laterFix)
  }
  for (const row of Object.values(table))
    row.rate = row.n ? Number(((100 * row.laterFix) / row.n).toFixed(2)) : 0
  const expected = {
    high: { n: 148, laterFix: 4, rate: 2.7 },
    moderate: { n: 281, laterFix: 10, rate: 3.56 },
    low: { n: 17, laterFix: 0, rate: 0 },
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
