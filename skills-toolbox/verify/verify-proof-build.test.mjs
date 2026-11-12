import { test } from 'node:test'
import assert from 'node:assert/strict'
import { buildProofRecord, proofAreaName, reviewSidecar } from './verify-proof-build.mjs'
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
  undisclosed: ['Drops users.legacy_id'],
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
  health: ['size-baseline-thin'],
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
    low: { confidence: confidence('low', ['api-contract'], ['size-p99']) },
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
  assert.equal(area.undisclosed, true)
  assert.deepEqual(area.unmentioned, ['Drops users.legacy_id'])
  const api = proof.areas.find((entry) => entry.name === 'api-contract')
  assert.equal(api.change, 'additive')
  assert.equal(proof.areas.find((entry) => entry.name === 'other').ask, false)
  assert.deepEqual(
    proof.confidence.loweredBy.map((r) => r.code),
    ['area-asks'],
  )
  assert.deepEqual(proof.health, [{ code: 'size-baseline-thin', text: 'size baseline thin' }])
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
