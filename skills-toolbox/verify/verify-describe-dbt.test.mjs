import assert from 'node:assert/strict'
import { test } from 'node:test'

import { discoverExtensions } from '../skill-extensions.mjs'
import { changedFiles, gitAt } from './describer-kit.mjs'
import { detectAreas } from './verify-areas.mjs'
import { collectItems, piiKind, piiTermsFrom } from './verify-describe-dbt.mjs'
import { refineDescription } from './verify-describe.mjs'
import { readChangedFiles } from './verify-diff.mjs'
import { envelopeFor, gitFixture, runDescriberScript } from './verify-git-fixture.mjs'

const ROOT = 'warehouse/project'
const MODELS = `${ROOT}/models`
const PROJECT = `name: example
models:
  example:
    +materialized: view
    intermediate:
      +materialized: ephemeral
    marts:
      +materialized: table
`
const TERMS = ['medicare', 'membership', 'fund number']
const config = (terms) => JSON.stringify({ proofDefaults: { dbt: { piiTerms: terms } } })
function fixture(t, before, after) {
  const repo = gitFixture(t)
  const initial = {
    [`${ROOT}/dbt_project.yml`]: PROJECT,
    '.boss-skills.json': config(TERMS),
    ...before,
  }
  repo.write(Object.fromEntries(Object.entries(initial).filter(([, value]) => value !== null)))
  const base = repo.commit('base')
  repo.write(after)
  const head = repo.commit('head')
  const git = gitAt(repo.dir)
  const facts = collectItems({ git, base, head, changes: changedFiles(git, base, head) })
  return { repo, base, head, facts }
}
const blocks = (facts) => facts.filter((f) => f.block).map((f) => f.block)
const signal = (facts) => facts.find((f) => f.signal)?.signal

test('six staging-model schema and materialisation moves retain the Database review signal', (t) => {
  const names = [
    'stg_bosso__cloud_accounts',
    'stg_bosso__newsletter_existing_user_imports',
    'stg_bosso__organizations',
    'stg_bosso__user_newsletter_consents',
    'stg_bosso__user_signup_attributions',
    'stg_bosso__users',
  ]
  const project = `${PROJECT}    staging:\n      +schema: staging\n      +materialized: view\n`
  const { repo, base, head, facts } = fixture(
    t,
    {
      [`${ROOT}/dbt_project.yml`]: project,
      ...Object.fromEntries(
        names.map((name) => [`${MODELS}/staging/${name}.sql`, 'select id from t']),
      ),
    },
    {
      [`${ROOT}/dbt_project.yml`]: project
        .replace('+schema: staging', '+schema: reporting')
        .replace('      +materialized: view', '      +materialized: table'),
    },
  )
  const envelope = envelopeFor(repo.dir, base, head, 'other', 'boss-verify-dbt')
  envelope.area.paths = [`${ROOT}/dbt_project.yml`]
  const result = runDescriberScript(
    new URL(
      '../../lib/bossalib/contract/plugin/skillpayload/skills/boss-verify/extensions/boss-verify-dbt/describe.mjs',
      import.meta.url,
    ).href,
    envelope,
    repo.dir,
  )
  assert.equal(blocks(facts).length, names.length)
  assert.equal(signal(facts).state, 'review')
  assert.deepEqual(
    result.signals?.find((entry) => entry.key === 'db'),
    signal(facts),
  )
  assert.deepEqual(refineDescription(result), [])
})

test('existing column type changes include both types and require review', (t) => {
  const file = `${MODELS}/amounts.sql`
  const { facts } = fixture(
    t,
    { [file]: 'select amount::bigint as amount, patient_email::text as patient_email from t' },
    { [file]: 'select amount::numeric as amount, patient_email::varchar as patient_email from t' },
  )
  assert.deepEqual(blocks(facts)[0].columns, [
    { name: 'amount', type: 'numeric', change: 'changed', note: 'was bigint' },
    { name: 'patient_email', type: 'varchar', change: 'changed', pii: true, note: 'was text' },
  ])
  assert.equal(signal(facts).state, 'review')
  assert.match(signal(facts).summary, /2 columns changed/)
})

test('schema-only relocation reports both locations without personal columns', (t) => {
  for (const [beforeSchema, afterSchema] of [
    [null, 'reporting'],
    ['private', 'reporting'],
    ['private', null],
  ]) {
    const project = (schema) => PROJECT + (schema ? `    +schema: ${schema}\n` : '')
    const { facts } = fixture(
      t,
      {
        [`${MODELS}/counts.sql`]: 'select id, num_orders from t',
        [`${ROOT}/dbt_project.yml`]: project(beforeSchema),
      },
      { [`${ROOT}/dbt_project.yml`]: project(afterSchema) },
    )
    assert.equal(signal(facts).state, 'review')
    const location = (schema) => (schema ? `in the ${schema} schema` : 'in the target schema')
    assert.ok(
      facts.some(
        (fact) =>
          fact.text === `counts: was ${location(beforeSchema)}, now ${location(afterSchema)}`,
      ),
    )
  }
})

test('whitespace-controlled config changes report materialisation and exposure', (t) => {
  const file = `${MODELS}/people.sql`
  const sql =
    "{{- config(materialized='ephemeral', schema='private') -}} select patient_email from t"
  const { facts } = fixture(t, { [file]: sql }, { [file]: sql.replace('ephemeral', 'view') })
  assert.equal(blocks(facts)[0].materialization, 'view')
  assert.equal(blocks(facts)[0].columns[0].note, 'email, in the private schema (was ephemeral)')
  assert.ok(facts.some((fact) => fact.text === 'people: materialised as ephemeral, now view'))
  assert.equal(signal(facts).state, 'review')
})

test('unnamed SQL expressions retain schema fallback and personal-data flags', (t) => {
  const { facts } = fixture(
    t,
    {},
    {
      [`${MODELS}/people.sql`]: 'select lower(email) patient_email from source',
      [`${MODELS}/schema.yml`]:
        'models:\n  - name: people\n    columns:\n      - name: patient_email\n',
    },
  )
  assert.deepEqual(blocks(facts)[0].columns, [
    {
      name: 'patient_email',
      type: '—',
      pii: true,
      note: 'email, in the target schema',
    },
  ])
})

test('root model paths discover root SQL and schema fallback columns', (t) => {
  const { facts } = fixture(
    t,
    { 'dbt_project.yml': "name: root_project\nmodel-paths: ['.']\n" },
    {
      'people.sql': "select * from {{ ref('source') }}",
      'schema.yml': 'models:\n  - name: people\n    columns:\n      - name: patient_email\n',
    },
  )
  assert.deepEqual(
    blocks(facts).map((block) => block.name),
    ['people'],
  )
  assert.equal(blocks(facts)[0].columns[0].pii, true)
  assert.equal(signal(facts).state, 'additive')
})

test('Database signal counts all column changes before trimming table display', (t) => {
  const additions = Array.from({ length: 90 }, (_, index) => `new_${index}`)
  const { facts } = fixture(
    t,
    { [`${MODELS}/wide.sql`]: 'select old_column from source' },
    { [`${MODELS}/wide.sql`]: `select ${additions.join(', ')} from source` },
  )
  assert.equal(blocks(facts)[0].columns.length, 80)
  assert.equal(signal(facts).state, 'review')
  assert.match(signal(facts).summary, /90 columns added, 1 column removed/)
})

test('default personal-data kinds exclude descriptors and places', () => {
  for (const [name, kind] of Object.entries({
    patient_first_name: 'name',
    surname: 'name',
    date_of_birth: 'date of birth',
    patient_dob: 'date of birth',
    street_address: 'address',
    patient_phone: 'phone',
    contact_email: 'email',
  }))
    assert.equal(piiKind(name), kind, name)
  for (const name of [
    'email_domain',
    'facility_name',
    'has_email',
    'hospital_address',
    'hospital_phone',
    'num_with_phone',
    'patient_id',
    'scope_name',
  ])
    assert.equal(piiKind(name), null, name)
})

test('configured terms add word-set kinds after descriptor exclusions', () => {
  for (const [name, kind] of Object.entries({
    ai_medicare_number: 'medicare',
    ai_medicare_ref: 'medicare',
    ai_membership_number: 'membership',
    patient_fund_number: 'fund number',
  })) {
    assert.equal(piiKind(name, TERMS), kind, name)
    assert.equal(piiKind(name), null, name)
  }
  for (const name of [
    'ai_fund_eclipse_id',
    'fund_disposition',
    'medicare_number_disposition',
    'membership_number_disposition',
  ])
    assert.equal(piiKind(name, TERMS), null, name)
  assert.equal(piiKind('number_patient_fund', ['FUND-number']), 'fund number')
  assert.equal(piiKind('patient_email', ['passport']), 'email')
  assert.equal(piiKind('patient_fund_number', ['number', 'fund number']), 'number')
  assert.deepEqual(
    piiTermsFrom({ proofDefaults: { dbt: { piiTerms: [' passport ', null, '', 12] } } }),
    [' passport '],
  )
  assert.deepEqual(piiTermsFrom({ proofDefaults: { dbt: { piiTerms: 'passport' } } }), [])
})

test('new synthetic multi-model project matches ported flags, types and materialisations', (t) => {
  const { facts } = fixture(
    t,
    {},
    {
      [`${MODELS}/intermediate/guesses.sql`]:
        'select ai_medicare_number::text as ai_medicare_number, ai_membership_number, num_attempts from t',
      [`${MODELS}/intermediate/numbers.sql`]:
        'select patient_fund_number, patient_medicare_ref from t',
      [`${MODELS}/staging/people.sql`]: 'select patient_first_name, created_at, is_latest from t',
      [`${MODELS}/marts/orders.sql`]: 'select claim_id, total_cents from t',
    },
  )
  const tables = blocks(facts)
  assert.deepEqual(
    tables.map((b) => `${b.name} ${b.materialization} ${b.change}`),
    [
      'guesses ephemeral added',
      'numbers ephemeral added',
      'orders table added',
      'people view added',
    ],
  )
  assert.deepEqual(tables[0].columns, [
    { name: 'ai_medicare_number', type: 'text', pii: true },
    { name: 'ai_membership_number', type: '—', pii: true },
    { name: 'num_attempts', type: 'bigint' },
  ])
  assert.deepEqual(tables[1].columns, [
    { name: 'patient_fund_number', type: '—', pii: true },
    { name: 'patient_medicare_ref', type: '—', pii: true },
  ])
  assert.deepEqual(signal(facts), {
    key: 'db',
    label: 'Database',
    state: 'additive',
    summary: 'dbt: 4 models added',
  })
  assert.equal(
    facts.some((f) => f.risk || f.signal?.key === 'security'),
    false,
  )
})

test('leaving ephemeral exposes flagged columns with generic notes and review signal', (t) => {
  const file = `${MODELS}/staging/numbers.sql`
  const sql =
    "{{ config(materialized='ephemeral') }} select patient_fund_number, patient_medicare_ref from t"
  const { facts } = fixture(t, { [file]: sql }, { [file]: sql.replace('ephemeral', 'view') })
  assert.deepEqual(
    blocks(facts)[0].columns.map((c) => [c.name, c.note]),
    [
      ['patient_fund_number', 'fund number, in the target schema (was ephemeral)'],
      ['patient_medicare_ref', 'medicare, in the target schema (was ephemeral)'],
    ],
  )
  assert.equal(
    facts.some((f) => f.text === 'numbers: materialised as ephemeral, now view'),
    true,
  )
  assert.equal(signal(facts).state, 'review')
  assert.equal(
    facts.some((f) => f.risk),
    false,
  )
})

test('added personal column is annotated; unchanged neighbours omitted and custom schema named', (t) => {
  const file = `${MODELS}/marts/orders.sql`
  const { facts } = fixture(
    t,
    { [file]: "{{ config(schema='private') }} select claim_id, patient_email from t" },
    {
      [file]:
        "{{ config(schema='private') }} select claim_id, patient_email, patient_medicare_number::text as patient_medicare_number from t",
    },
  )
  assert.deepEqual(blocks(facts)[0].columns, [
    {
      name: 'patient_medicare_number',
      type: 'text',
      change: 'added',
      pii: true,
      note: 'medicare, in the private schema',
    },
  ])
})

test('removed model or column raises review; schema moves expose retained personal columns', (t) => {
  const a = `${MODELS}/a.sql`,
    b = `${MODELS}/b.sql`
  const { facts } = fixture(
    t,
    { [a]: 'select patient_email, id from t', [b]: 'select id from t' },
    { [a]: "{{ config(schema='restricted') }} select patient_email from t", [b]: null },
  )
  assert.equal(signal(facts).state, 'review')
  assert.equal(blocks(facts).find((b) => b.name === 'b').change, 'removed')
  assert.deepEqual(blocks(facts).find((b) => b.name === 'a').columns, [
    { name: 'patient_email', type: '—', pii: true, note: 'email, in the restricted schema' },
    { name: 'id', type: '—', change: 'removed' },
  ])
})

test('terms always come from base; missing or malformed config keeps defaults', (t) => {
  for (const text of [null, '{', config('passport'), config([])]) {
    const before = text === null ? { '.boss-skills.json': null } : { '.boss-skills.json': text }
    const { facts } = fixture(t, before, {
      '.boss-skills.json': config(['passport']),
      [`${MODELS}/people.sql`]: 'select passport_number, patient_email from t',
    })
    assert.deepEqual(
      blocks(facts)[0].columns.map((c) => !!c.pii),
      [false, true],
    )
  }
})

test('root and nested projects are discovered, external SQL ignored', (t) => {
  const { facts } = fixture(
    t,
    { 'dbt_project.yml': 'name: root\n' },
    {
      'models/root_model.sql': 'select email from t',
      [`${MODELS}/nested.sql`]: 'select id from t',
      'elsewhere/raw.sql': 'select phone from t',
    },
  )
  assert.deepEqual(
    blocks(facts).map((b) => b.name),
    ['nested', 'root_model'],
  )
  const alone = fixture(t, {}, { 'outside/query.sql': 'select email from t' })
  assert.deepEqual(alone.facts, [])
})

test('tests, macros, documentation and schema test-only edits describe nothing', (t) => {
  const schema = `${MODELS}/schema.yml`
  const yml = (testName) =>
    `models:\n  - name: people\n    columns:\n      - name: patient_email\n        tests:\n          - ${testName}\n`
  const { facts } = fixture(
    t,
    { [`${MODELS}/people.sql`]: 'select patient_email from t', [schema]: yml('not_null') },
    {
      [schema]: yml('unique'),
      [`${ROOT}/tests/check.sql`]: 'select 1',
      [`${ROOT}/macros/count.sql`]: 'select 1',
      [`${ROOT}/docs/readme.md`]: 'Example',
    },
  )
  assert.deepEqual(facts, [])
})

test('schema fallback and project edits affect only model facts that move', (t) => {
  const file = `${MODELS}/people.sql`,
    schema = `${MODELS}/schema.yml`
  const { facts } = fixture(
    t,
    {
      [file]: "select * from {{ ref('source') }}",
      [schema]: 'models:\n  - name: people\n    columns:\n      - name: id\n',
    },
    {
      [schema]:
        'models:\n  - name: people\n    config:\n      materialized: table\n    columns:\n      - name: id\n      - name: patient_email\n',
    },
  )
  assert.equal(blocks(facts)[0].materialization, 'table')
  assert.equal(blocks(facts)[0].columns[0].pii, true)
  const project = fixture(
    t,
    { [file]: 'select patient_email from t' },
    { [`${ROOT}/dbt_project.yml`]: PROJECT.replace('+materialized: view', '+materialized: table') },
  )
  assert.equal(blocks(project.facts)[0].materialization, 'table')
})

test('built-in discovery and vendored script return valid other-area dbt proof', (t) => {
  const root = new URL(
    '../../lib/bossalib/contract/plugin/skillpayload/skills/boss-verify/extensions/',
    import.meta.url,
  )
  const discovered = discoverExtensions({
    core: 'boss-verify',
    role: 'proof-describe',
    builtinDir: root.pathname,
    root: process.cwd(),
  })
  const ext = discovered.extensions.find((e) => e.name === 'boss-verify-dbt')
  assert.ok(ext)
  assert.equal(ext.builtin, true)
  assert.deepEqual(ext.describeAreas, ['migrations', 'other', 'no-sign-off'])
  const { repo, base, head } = fixture(
    t,
    {},
    { [`${MODELS}/people.sql`]: 'select patient_email from t' },
  )
  const envelope = envelopeFor(repo.dir, base, head, 'other', 'boss-verify-dbt')
  envelope.area = detectAreas(readChangedFiles({ baseSha: base, headSha: head, cwd: repo.dir }), {
    dbtRoots: [ROOT],
  }).find((a) => a.area === 'other')
  const result = runDescriberScript(
    new URL(
      '../../lib/bossalib/contract/plugin/skillpayload/skills/boss-verify/extensions/boss-verify-dbt/describe.mjs',
      import.meta.url,
    ).href,
    envelope,
    repo.dir,
  )
  assert.deepEqual(refineDescription(result), [])
  assert.equal(result.blocks.length, 1)
  assert.equal(result.signals[0].state, 'additive')
})
