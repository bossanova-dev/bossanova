import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  PROOF_EVIDENCE_SCHEMA,
  buildProofRecord,
  proofAreaName,
  reviewSidecar,
} from './verify-proof-build.mjs'
import { proofPageModel } from './verify-proof-page.mjs'
import { buildBlockers } from './verify-blockers.mjs'
import { validateProof } from './verify-proof.mjs'
import { canonicalAreaId, configAreaIds, reviewAreas } from './verify-areas.mjs'
import { legacyCodes } from './verify-level.mjs'

const HEAD = 'a'.repeat(40)
const BASE = 'b'.repeat(40)
const schema = {
  area: 'config:api-schema',
  label: 'API schema',
  paths: ['services/api/entity/user.go'],
  defaultAsk: 'always',
  source: 'config',
  items: ['Adds the users.email column'],
  links: [
    { text: 'Schema docs', url: 'https://docs.example/schema' },
    { text: 'Not https', url: 'http://docs.example/schema' },
  ],
  breaking: false,
}
const contract = {
  area: 'api-contract',
  label: 'API contract',
  paths: ['proto/api.proto'],
  defaultAsk: 'always',
  source: 'builtin',
  change: 'additive',
  changeReasons: ['Proto proto: additive'],
}
const other = { area: 'other', label: 'Other changes', paths: ['README.md'], defaultAsk: 'never' }
const confidence = (level, asks = [], loweredBy = level === 'high' ? [] : ['area-asks']) => ({
  level,
  asks: asks.map((area) => ({ area, label: area, source: 'builtin' })),
  reasons: [],
  loweredBy,
  health: ['second-model-skipped'],
})
const judged = (overrides = {}) => ({
  verdict: 'human',
  reason: 'human-review:api-contract',
  reasons: ['human-review:api-contract'],
  headSha: HEAD,
  baseSha: BASE,
  confidence: confidence('moderate', ['config:api-schema', 'api-contract']),
  findingNotes: [{ title: 'Polish naming', severity: 'P3' }],
  notApplicableNotes: [{ extension: 'boss-verify-ui', note: 'no UI paths' }],
  facts: { areas: [schema, contract, other], receipt: { verdict: 'present', state: 'success' } },
  ...overrides,
})
const build = (overrides, intent) =>
  buildProofRecord({ judged: judged(overrides), intent, repo: 'acme/widgets', pr: 7 })
const blocker = {
  id: 'review:0123456789ab',
  kind: 'review-finding',
  severity: 'P1',
  file: 'src/a.mjs',
  line: 4,
  summary: 'Returns the wrong value',
  text: '',
}

test('every level builds a record the proof contract accepts @sessions.AC248601', () => {
  const cases = {
    high: { verdict: 'pass', confidence: confidence('high') },
    moderate: {},
    low: { confidence: confidence('low', ['api-contract'], ['asks-over-3']) },
    blocked: {
      verdict: 'blocked',
      blockers: [blocker],
      confidence: confidence('blocked', [], ['blocker']),
    },
    'cannot-assess': { confidence: confidence('cannot-assess', [], ['evidence-missing']) },
  }
  for (const [level, overrides] of Object.entries(cases)) {
    const built = build(overrides)
    assert.equal(built.ok, true, `${level}: ${built.errors}`)
    assert.equal(built.proof.confidence.level, level)
    assert.equal(validateProof(built.proof).ok, true, level)
    assert.equal(built.proof.page, null)
  }
  const blocked = build(cases.blocked).proof
  assert.deepEqual(blocked.review.blockers, [
    { title: 'Returns the wrong value', where: 'src/a.mjs:4' },
  ])
})

test('asks are the asking areas in area order, under contract-valid names @sessions.AC248605', () => {
  const { proof } = build({})
  assert.deepEqual(proof.confidence.asks, ['config-api-schema', 'api-contract'])
  const area = proof.areas.find((entry) => entry.name === 'config-api-schema')
  assert.equal(area.label, 'API schema')
  assert.deepEqual(area.items, ['Adds the users.email column'])
  assert.deepEqual(area.links, [{ text: 'Schema docs', url: 'https://docs.example/schema' }])
  const api = proof.areas.find((entry) => entry.name === 'api-contract')
  assert.equal(api.change, 'additive')
  assert.equal(proof.areas.find((entry) => entry.name === 'other').ask, false)
  assert.deepEqual(
    proof.confidence.loweredBy.map((r) => r.code),
    ['area-asks'],
  )
  assert.deepEqual(proof.health, [{ code: 'second-model-skipped', text: 'second model skipped' }])
  assert.deepEqual(proof.review.models, [{ agent: 'review', outcome: 'ran' }])
  assert.deepEqual(proof.review.notes, ['Polish naming', 'boss-verify-ui: no UI paths'])
})

test('judge notes on no-sign-off paths reach the review notes after the finding notes', () => {
  const { proof } = build({
    judgeNotes: [{ label: 'Docs drift', path: 'docs/x.md', reason: 'Stale example' }],
  })
  assert.deepEqual(proof.review.notes, [
    'Polish naming',
    'Docs drift (docs/x.md): Stale example',
    'boss-verify-ui: no UI paths',
  ])
  assert.equal(validateProof(proof).ok, true)
})

test('an ask no judge area carries becomes its own asking area', () => {
  const { proof } = build({
    confidence: confidence('moderate', ['capped:other:docs', 'api-contract']),
  })
  assert.deepEqual(proof.confidence.asks, ['api-contract', 'capped-other-docs'])
  assert.equal(validateProof(proof).ok, true)
})

test('the level always agrees with the blockers and names a reason', () => {
  const lost = build({ verdict: 'blocked', confidence: confidence('blocked', [], ['blocker']) })
  assert.equal(lost.proof.confidence.level, 'cannot-assess')
  const forced = build({ verdict: 'blocked', blockers: [blocker] })
  assert.equal(forced.proof.confidence.level, 'blocked')
  assert.ok(forced.proof.confidence.loweredBy.some((r) => r.code === 'blocker'))
  const bare = build({ confidence: undefined })
  assert.equal(bare.proof.confidence.level, 'cannot-assess')
  assert.deepEqual(
    bare.proof.confidence.loweredBy.map((r) => r.code),
    ['confidence-not-computed'],
  )
  const claimedHigh = build({ verdict: 'pass', confidence: confidence('high', ['api-contract']) })
  assert.equal(claimedHigh.proof.confidence.level, 'moderate')
})

test('what it does comes from a valid intent record, else unknown', () => {
  const intent = { source: 'pr-body', ref: '#7', bullets: ['Adds an email column to users'] }
  assert.deepEqual(build({}, intent).proof.whatItDoes, intent)
  for (const bad of [null, { source: 'pr-body', bullets: [] }, { source: 'tea', bullets: ['x'] }])
    assert.deepEqual(build({}, bad).proof.whatItDoes, { source: 'unknown', bullets: [] })
})

test('a wait, a claim or a judge output without its range builds no record', () => {
  assert.equal(build({ verdict: 'wait' }).reason, 'judge-not-terminal')
  assert.equal(build({ verdict: 'extensions-required' }).reason, 'judge-not-terminal')
  assert.equal(build({ headSha: '' }).reason, 'judge-without-range')
})

test('proof names map every non-identifier run to one dash', () => {
  assert.equal(proofAreaName('config:api-schema'), 'config-api-schema')
  assert.equal(proofAreaName('extension:human-review:ui'), 'extension-human-review-ui')
  assert.equal(proofAreaName(':::'), 'area')
})

const policy = (names) => ({
  alwaysReview: names.map((name, i) => ({
    name,
    globs: [`area${i}/**`],
    parkCode: 'human-review:database',
  })),
  noSignOff: [],
  deprecations: [],
})

test('config area ids are slugs of the configured name, which stays the label @sessions.AC248605', () => {
  const { areas } = reviewAreas(
    ['area0/a.go', 'area1/b.go', 'area2/c.go'],
    policy(['API schema', 'api-schema', 'Docs & Guides']),
  )
  assert.deepEqual(
    areas.map((area) => [area.area, area.label]),
    [
      ['config:api-schema', 'API schema'],
      ['config:api-schema:1', 'api-schema'],
      ['config:docs-guides', 'Docs & Guides'],
    ],
  )
  assert.deepEqual(configAreaIds([{ name: '!!' }]), ['config:area'])
  // A describer can target the slugged id.
  assert.match('config:api-schema', /^[a-z0-9][a-z0-9:-]*$/)
})

test('an ask recorded under the raw config name keeps its park code @sessions.AC248605', () => {
  assert.equal(canonicalAreaId('config:API schema:3'), 'config:api-schema:3')
  assert.equal(canonicalAreaId('api-contract'), 'api-contract')
  const review = policy(['API schema'])
  for (const [area, codes] of [
    ['config:API schema', ['human-review:database']],
    ['config:api-schema', ['human-review:database']],
    ['config:other', ['always-human-path']],
  ]) {
    const result = {
      level: 'moderate',
      asks: [{ area, label: 'API schema', source: 'config' }],
      reasons: [],
      loweredBy: ['area-asks'],
      health: [],
    }
    assert.deepEqual(legacyCodes(result, review), codes, area)
  }
})

test('the review sidecar keeps each finding with its file, line, severity and status', () => {
  const raw = {
    id: 'f1',
    severity: 'P1',
    file: 'src/a.mjs',
    line: 4,
    title: 'Returns the wrong value',
    evidence: 'It returns  the old\n total.',
    introduced: true,
    verified: true,
    fixed: false,
  }
  const [open] = buildBlockers({ reviewFindings: [raw] })
  const note = { ...raw, id: 'n1', severity: 'P3', line: 9, title: 'Name it' }
  const preexisting = { ...raw, id: 'p1', introduced: false, line: 30, title: 'Old race' }
  const fixed = { ...raw, id: 'x1', severity: 'P2', line: 12, title: 'Off by one' }
  const sidecar = reviewSidecar(
    judged({
      blockers: [open, { id: 'check:ci', kind: 'check', summary: 'ci', text: 'ci' }],
      findingNotes: [note, { title: 'Extension note without a place', severity: 'P3' }],
      reviewUnfixed: [raw, note, preexisting],
      reviewFixed: [fixed],
    }),
  )
  assert.deepEqual(
    sidecar.findings.map((f) => [f.status, f.severity, f.file, f.line, f.title]),
    [
      ['open', 'P1', 'src/a.mjs', 4, 'Returns the wrong value'],
      ['open', 'P1', 'src/a.mjs', 30, 'Old race'],
      ['note', 'P3', 'src/a.mjs', 9, 'Name it'],
      ['note', 'P3', undefined, undefined, 'Extension note without a place'],
      ['fixed', 'P2', 'src/a.mjs', 12, 'Off by one'],
    ],
    'a finding read from the blockers and the unfixed list shows once, and checks are not findings',
  )
  assert.equal(sidecar.findings[0].id, open.id)
  assert.equal(sidecar.findings[0].detail, 'It returns the old total.')
  assert.deepEqual(sidecar.acceptance, [])
})

test('the review sidecar keeps well-formed acceptance rows once per criterion', () => {
  const row = { id: 'AC1', text: 'Saves', result: 'met', check: 'go test ./x', head: HEAD }
  const { acceptance } = reviewSidecar(
    judged({
      reviewAcceptance: [
        row,
        { ...row, result: 'unmet' },
        { id: 'AC2', text: '', result: 'unchecked', check: '', head: HEAD },
        { id: 'AC3', text: 'Bad', result: 'pass', head: HEAD },
        { id: 'AC4', text: 'Bad', result: 'met', head: 'nope' },
      ],
    }),
  )
  assert.deepEqual(acceptance, [
    { ...row, result: 'unmet' },
    { id: 'AC2', text: 'AC2', result: 'unchecked', head: HEAD },
  ])
})

test('describer blocks, signals and risks ride the sidecar areas and leave the record digest alone', async () => {
  const { proofPageInput } = await import('./verify-proof-publish.mjs')
  const facts = {
    blocks: [{ kind: 'steps', steps: ['Run `./migrate`'] }],
    signals: [{ key: 'db', label: 'Database', state: 'additive', summary: 'Adds a column' }],
  }
  const plain = build()
  const typed = build({
    facts: { ...judged().facts, areas: [{ ...schema, ...facts }, contract, other] },
  })
  assert.equal(typed.proof.digest, plain.proof.digest, 'the record never carries the blocks')
  assert.deepEqual(typed.proof, plain.proof)
  assert.deepEqual(typed.areas[0].described, facts)
  assert.deepEqual(plain.areas[0].described, {})
  const sidecar = (built) =>
    proofPageInput({
      proof: built.proof,
      areas: built.areas,
      judged: judged(),
      git: () => ({ ok: false }),
      prBody: '',
      repo: 'acme/widgets',
      pr: 7,
    })
  assert.deepEqual(sidecar(typed).described, { [typed.areas[0].name]: facts })
  assert.ok(!Object.hasOwn(sidecar(plain), 'described'))
  const broken = build({
    facts: {
      ...judged().facts,
      areas: [{ ...schema, blocks: [{ kind: 'chart' }] }, contract, other],
    },
  })
  assert.ok(!Object.hasOwn(sidecar(broken), 'described'), 'an invalid area is left out')
  assert.ok(Object.hasOwn(sidecar(broken), 'filesUrl'), 'and the rest of the sidecar stays')
})

test('boss-proof-evidence/1 sidecar contributes claims and health, caps high on unproven behaviour claims, and keeps non-behaviour claims out of Not proven', () => {
  const intent = {
    source: 'pr-body',
    bullets: [
      'Fixes proof page area accounting',
      'Generated vendored skill toolboxes',
      'Regression test additions',
    ],
  }
  const evidence = {
    schema: PROOF_EVIDENCE_SCHEMA,
    head: HEAD,
    intent,
    claims: [
      { proof: 'test', behaviour: true, test: 'areaAccounting', path: 'a.test.mjs' },
      { proof: 'unproven', behaviour: false, note: 'generated output' },
      { proof: 'unproven', behaviour: false, note: 'test addition' },
    ],
    health: [{ code: 'second-model-down', text: 'Second model down' }],
  }
  const readHeadFile = (file) => (file === 'a.test.mjs' ? "test('areaAccounting', () => {})" : null)
  const built = buildProofRecord({
    judged: judged({ verdict: 'pass', confidence: confidence('high') }),
    intent,
    evidence,
    repo: 'acme/widgets',
    pr: 4668,
    readHeadFile,
    changedPaths: ['a.test.mjs'],
  })
  assert.equal(built.ok, true, built.errors?.join('; '))
  assert.equal(built.proof.confidence.level, 'high')
  assert.equal(built.proof.health[0].code, 'second-model-down')
  assert.equal(built.proof.whatItDoes.claims.length, 3)
  const model = proofPageModel({ proof: built.proof })
  assert.deepEqual(model.banner.unproven, [])

  // An unproven behaviour claim caps high -> moderate and appears in Not proven.
  const unprovenBehaviour = buildProofRecord({
    judged: judged({ verdict: 'pass', confidence: confidence('high') }),
    intent,
    evidence: {
      ...evidence,
      claims: [
        { proof: 'unproven', behaviour: true, note: 'no test' },
        { proof: 'unproven', behaviour: false, note: 'generated output' },
        { proof: 'unproven', behaviour: false, note: 'test addition' },
      ],
    },
    repo: 'acme/widgets',
    pr: 4668,
    readHeadFile,
    changedPaths: ['a.test.mjs'],
  })
  assert.equal(unprovenBehaviour.ok, true)
  assert.equal(unprovenBehaviour.proof.confidence.level, 'moderate')
  assert.ok(unprovenBehaviour.proof.confidence.loweredBy.some((r) => r.code === 'unproven-claim'))
  const cappedModel = proofPageModel({ proof: unprovenBehaviour.proof })
  assert.deepEqual(cappedModel.banner.unproven, ['Fixes proof page area accounting: no test'])

  // Unverified test/clip sidecar claims fail closed to unproven.
  const forged = buildProofRecord({
    judged: judged({ verdict: 'pass', confidence: confidence('high') }),
    intent,
    evidence: {
      ...evidence,
      claims: [
        { proof: 'test', behaviour: true, test: 'forgedTest', path: 'a.test.mjs' },
        { proof: 'clip', behaviour: true, chapter: 'Forged clip', atMs: 100 },
        { proof: 'unproven', behaviour: false, note: 'test addition' },
      ],
    },
    repo: 'acme/widgets',
    pr: 4668,
    readHeadFile,
    changedPaths: ['a.test.mjs'],
    hasHeadClip: false,
  })
  assert.equal(forged.ok, true)
  assert.equal(forged.proof.whatItDoes.claims[0].proof, 'unproven')
  assert.equal(forged.proof.whatItDoes.claims[1].proof, 'unproven')
  assert.equal(forged.proof.confidence.level, 'moderate')

  // Stale sidecar claims with mismatched bullets are dropped; unknown caller intent falls back.
  const mismatched = buildProofRecord({
    judged: judged({ verdict: 'pass', confidence: confidence('high') }),
    intent: { source: 'pr-body', bullets: ['Different bullet 1', 'Different 2', 'Different 3'] },
    evidence,
    repo: 'acme/widgets',
    pr: 4668,
    readHeadFile,
    changedPaths: ['a.test.mjs'],
  })
  assert.equal(mismatched.ok, true)
  assert.equal(mismatched.proof.whatItDoes.claims, undefined)

  const unknownCaller = buildProofRecord({
    judged: judged({ verdict: 'pass', confidence: confidence('high') }),
    intent: { source: 'unknown', bullets: [] },
    evidence,
    repo: 'acme/widgets',
    pr: 4668,
    readHeadFile,
    changedPaths: ['a.test.mjs'],
  })
  assert.equal(unknownCaller.ok, true)
  assert.deepEqual(unknownCaller.proof.whatItDoes.bullets, intent.bullets)
  assert.equal(unknownCaller.proof.whatItDoes.claims.length, 3)

  // Many sidecar health entries cannot crowd out judge health.
  const crowded = buildProofRecord({
    judged: judged({
      verdict: 'pass',
      confidence: { ...confidence('high'), health: ['second-model-skipped'] },
    }),
    intent,
    evidence: {
      ...evidence,
      health: Array.from({ length: 25 }, (_, i) => ({ code: `factory-${i}`, text: `F${i}` })),
    },
    repo: 'acme/widgets',
    pr: 4668,
    readHeadFile,
    changedPaths: ['a.test.mjs'],
  })
  assert.equal(crowded.ok, true)
  assert.ok(crowded.proof.health.some((entry) => entry.code === 'second-model-skipped'))
})
