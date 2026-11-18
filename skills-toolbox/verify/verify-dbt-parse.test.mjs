import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  folderConfig,
  materialization,
  modelConfig,
  nameType,
  parseYaml,
  readProject,
  schemaEntries,
  selectColumns,
} from './verify-dbt-parse.mjs'

test('YAML subset reads maps, lists, flow lists, block scalars and comments', () => {
  assert.deepEqual(
    parseYaml(`---
name: 'example'
model-paths: ['models', "more"]
models:
  example:
    +materialized: table # comment
columns:
  - name: a
    description: >
      folded text
      over two lines
    tests:
      - not_null
  - name: b
`),
    {
      name: 'example',
      'model-paths': ['models', 'more'],
      models: { example: { '+materialized': 'table' } },
      columns: [
        { name: 'a', description: 'folded text over two lines', tests: ['not_null'] },
        { name: 'b' },
      ],
    },
  )
})

test('materialisation and schema follow model config and deepest folder config', () => {
  const project = readProject(
    'warehouse/dbt_project.yml',
    `name: example
models:
  example:
    +materialized: table
    marts:
      +materialized: view
      +schema: reporting
`,
  )
  assert.deepEqual(folderConfig(project, 'warehouse/models/marts/orders.sql'), {
    materialized: 'view',
    schema: 'reporting',
  })
  assert.deepEqual(
    modelConfig("{{ config(materialized='ephemeral', schema='private') }} select a"),
    { materialized: 'ephemeral', schema: 'private' },
  )
  assert.equal(materialization('materialized_view'), 'materialized-view')
  assert.equal(materialization('custom'), undefined)
  assert.deepEqual(
    schemaEntries(
      'models:\n  - name: orders\n    config:\n      materialized: table\n    columns:\n      - name: total_cents\n',
    ).get('orders'),
    { columns: ['total_cents'], materialized: 'table' },
  )
})

test('schema entries survive multiline plain and quoted descriptions', () => {
  for (const description of [
    'A model\n      with a longer description',
    '"A model\n      with a longer description"',
  ]) {
    const entries = schemaEntries(`models:
  - name: people
    description: ${description}
    config:
      materialized: table
    columns:
      - name: patient_email
  - name: later
    columns:
      - name: total_cents
`)
    assert.deepEqual(
      [...entries],
      [
        ['people', { columns: ['patient_email'], materialized: 'table' }],
        ['later', { columns: ['total_cents'], materialized: undefined }],
      ],
    )
  }
})

test('select lists follow CTEs and infer outer casts and aliases', () => {
  assert.deepEqual(
    selectColumns(`with source as (
select id, cast(value as numeric(16, 4)) as amount_precise_cents, created_at, value::text as label from t
), final as (select * from source) select * from final`),
    {
      resolved: true,
      columns: [
        { name: 'id', type: '—' },
        { name: 'amount_precise_cents', type: 'numeric(16, 4)' },
        { name: 'created_at', type: 'timestamptz' },
        { name: 'label', type: 'text' },
      ],
    },
  )
  assert.deepEqual(
    selectColumns(`-- select x from y
select distinct on (a) a, 'it''s, quoted' as b, cast(c as numeric(16, 4)) as c from t`).columns.map(
      (c) => `${c.name}:${c.type}`,
    ),
    ['a:—', 'b:—', 'c:numeric(16, 4)'],
  )
})

test('unresolved stars and Jinja statements retain schema fallback eligibility', () => {
  for (const sql of [
    "select * from {{ ref('source') }}",
    'select * from a join b on a.id = b.id',
    'select a, {% for x in xs %} {{ x }} as num_{{ x }}, {% endfor %} b from t',
    'select lower(email) patient_email, count(*) n from source',
    'select id::text from source',
  ]) {
    assert.equal(selectColumns(sql).resolved, false, sql)
  }
})

test('naming types apply only without casts', () => {
  assert.deepEqual(
    [
      'value_precise_cents',
      'value_cents',
      'created_at',
      'paid_on',
      'is_ready',
      'has_email',
      'num_orders',
      'rate_paid',
      'percent_paid',
      'patient_id',
    ].map(nameType),
    [
      'numeric(16,4)',
      'bigint',
      'timestamptz',
      'date',
      'boolean',
      'boolean',
      'bigint',
      'numeric',
      'numeric',
      '—',
    ],
  )
  assert.deepEqual(selectColumns('select total_cents::text as total_cents from t').columns, [
    { name: 'total_cents', type: 'text' },
  ])
  assert.deepEqual(
    selectColumns('select cast(cast(value as bigint) as text) as value from t').columns,
    [{ name: 'value', type: 'text' }],
  )
})

test('compound expressions do not inherit an operand cast type', () => {
  assert.deepEqual(
    selectColumns(`select
      cast(a as numeric) / cast(b as numeric) as rate_x,
      a::date - b::date as elapsed,
      x::text like y as is_match,
      a + b::text as label,
      (a + b)::text as expression_label,
      value::double precision as precise,
      value::numeric(16, 4)::text as cast_chain
    from source`).columns,
    [
      { name: 'rate_x', type: 'numeric' },
      { name: 'elapsed', type: '—' },
      { name: 'is_match', type: 'boolean' },
      { name: 'label', type: '—' },
      { name: 'expression_label', type: 'text' },
      { name: 'precise', type: 'double precision' },
      { name: 'cast_chain', type: 'text' },
    ],
  )
})
