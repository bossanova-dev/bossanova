import assert from 'node:assert/strict'
import { test } from 'node:test'
import { writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

import { discoverExtensions } from '../skill-extensions.mjs'
import { boundDescription } from './describer-kit.mjs'
import { refineDescription } from './verify-describe.mjs'
import { refineDescribed } from './verify-describe-blocks.mjs'
import { describeGraphql, graphqlSchemaBlock } from './verify-describe-graphql.mjs'
import { parseGraphqlSchema, graphqlSchemaFindings } from './verify-graphql-diff.mjs'
import { envelopeFor, gitFixture, runDescriberScript } from './verify-git-fixture.mjs'
import { apiSchemaChange } from './verify-tree-facts.mjs'

const moduleUrl = new URL('./verify-describe-graphql.mjs', import.meta.url).href
const builtinDir = fileURLToPath(
  new URL(
    '../../lib/bossalib/contract/plugin/skillpayload/skills/boss-verify/extensions/',
    import.meta.url,
  ),
)

/** @param {string} source */
function schema(source) {
  const parsed = parseGraphqlSchema(source)
  assert.equal(parsed.ok, true, JSON.stringify(parsed))
  if (!parsed.ok) throw new Error(parsed.error)
  return parsed.schema
}

/** @param {string} before @param {string} after */
function block(before, after) {
  const base = schema(before)
  const head = schema(after)
  return graphqlSchemaBlock(base, head, graphqlSchemaFindings(base, head))
}

/** @param {import('node:test').TestContext} t
 * @param {Record<string, string>} before @param {Record<string, string | null>} after */
function describe(t, before, after) {
  const repo = gitFixture(t)
  repo.write(before)
  const baseSha = repo.commit('base')
  repo.write(after)
  const headSha = repo.commit('head')
  const envelope = envelopeFor(repo.dir, baseSha, headSha, 'api-contract', 'boss-verify-graphql')
  const result = describeGraphql(envelope, { cwd: repo.dir })
  const gate = apiSchemaChange({
    mergeBase: baseSha,
    headSha,
    cwd: repo.dir,
    paths: envelope.area.paths,
  })
  return { repo, envelope, result, gate }
}

test('additive schema matches the reference describer across two files', (t) => {
  const { result, gate } = describe(t, additiveBefore, additiveAfter)
  assert.deepEqual(result.blocks?.[0], { kind: 'schema', types: additiveTypes })
  assert.deepEqual(result.items, ['5 additive GraphQL changes, 0 breaking'])
  assert.deepEqual(result.signals, [
    { key: 'api', label: 'API', state: 'additive', summary: result.items[0] },
  ])
  assert.equal(result.breaking, false)
  assert.equal(result.breaking, gate.change === 'breaking')
})

test('breaking schema matches the reference describer and runs as a script and built-in', async (t) => {
  const { repo, envelope, result, gate } = describe(t, breakingBefore, breakingAfter)
  assert.deepEqual(result.blocks?.[0], { kind: 'schema', types: breakingTypes })
  assert.equal(result.items[0], '2 additive GraphQL changes, 8 breaking (1 risky)')
  assert.equal(result.items.filter((s) => s.startsWith('Breaking GraphQL change:')).length, 8)
  assert.deepEqual(result.signals, [
    { key: 'api', label: 'API', state: 'breaking', summary: result.items[0] },
  ])
  assert.equal(result.breaking, true)
  assert.equal(result.breaking, gate.change === 'breaking')
  const output = runDescriberScript(moduleUrl, envelope, repo.dir)
  assert.deepEqual(output.blocks, result.blocks)
  const found = discoverExtensions({
    core: 'boss-verify',
    role: 'proof-describe',
    builtinDir,
    root: repo.dir,
  })
  const ext = found.extensions.find((entry) => entry.name === 'boss-verify-graphql')
  assert.ok(ext)
  assert.equal(ext.builtin, true)
  assert.deepEqual(ext.describeAreas, ['api-contract'])
  const payload = runDescriberScript(
    new URL(`file://${builtinDir}/boss-verify-graphql/describe.mjs`).href,
    envelope,
    repo.dir,
  )
  assert.deepEqual(payload.blocks, result.blocks)
  assert.deepEqual(refineDescription(output), [])
  if (process.env.GRAPHQL_PROOF_OUT)
    writeFileSync(process.env.GRAPHQL_PROOF_OUT, JSON.stringify(output, null, 2))
})

test('directory schema keeps unchanged siblings and excludes other directories', (t) => {
  const { result, gate } = describe(
    t,
    {
      'graph/schema.gql': 'type Query { a: Int }',
      'graph/domain.gql': 'scalar X',
      'other/invalid.gql': 'type {',
    },
    { 'graph/domain.gql': 'scalar X extend type Query { b: Int }' },
  )
  assert.deepEqual(result.blocks?.[0].types, [
    {
      name: 'Query',
      keyword: 'type',
      change: 'changed',
      class: 'safe',
      fields: [{ name: 'b', type: 'Int', change: 'added' }],
    },
  ])
  assert.equal(result.breaking, gate.change === 'breaking')
})

test('directory schema uses the core case-sensitive SDL extension set', (t) => {
  const { result } = describe(
    t,
    {
      'graph/base.GRAPHQL': 'type Query { a: Int }',
      'graph/domain.gql': 'scalar X',
    },
    { 'graph/domain.gql': 'scalar X extend type Query { b: Int }' },
  )
  assert.equal(result.blocks?.[0].types[0].change, 'added')
  assert.deepEqual(result.blocks?.[0].types[0].fields, [{ name: 'b', type: 'Int' }])
})

test('directory context yields to the changed-file contract classification', (t) => {
  const added = describe(
    t,
    { 'graph/types.gql': 'input I { a: Int }' },
    { 'graph/ext.gql': 'extend input I { req: String! }' },
  )
  assert.equal(added.gate.change, 'additive')
  assert.equal(added.result.breaking, false)
  assert.deepEqual(added.result.items, ['1 additive GraphQL change, 0 breaking'])
  assert.equal(added.result.signals?.[0].state, 'additive')
  assert.deepEqual(added.result.blocks?.[0].types, [
    {
      name: 'I',
      keyword: 'input',
      change: 'added',
      class: 'safe',
      fields: [{ name: 'req', type: 'String!' }],
    },
  ])

  const removed = describe(
    t,
    {
      'graph/types.gql': 'type Query { a: Int }',
      'graph/ext.gql': 'extend type Query { a: Int }',
    },
    { 'graph/ext.gql': null },
  )
  assert.equal(removed.gate.change, 'breaking')
  assert.equal(removed.result.breaking, true)
  assert.equal(removed.result.signals?.[0].state, 'breaking')
  assert.equal(removed.result.blocks?.[0].types[0].class, 'breaking')
  assert.equal(removed.result.blocks?.[0].types[0].change, 'removed')
})

test('root changes do not collide with a type named schema', () => {
  const definitions = 'type schema { id: ID } type Query { a: Int } type OtherQuery { b: Int }'
  const before = `schema { query: Query } ${definitions}`
  const after = `schema { query: OtherQuery } ${definitions}`
  assert.equal(block(before, after), null)
  assert.equal(block(definitions, `schema { query: Query } ${definitions}`), null)
  assert.deepEqual(block(before, `${after} extend type schema { extra: Int }`), {
    kind: 'schema',
    types: [
      {
        name: 'schema',
        keyword: 'type',
        change: 'changed',
        class: 'safe',
        fields: [{ name: 'extra', type: 'Int', change: 'added' }],
      },
    ],
  })
})

test('unreadable sibling falls back; unreadable changes do not double-count gate breaking', (t) => {
  const fallback = describe(
    t,
    { 'graph/bad.gql': 'type {', 'graph/a.gql': 'type Query { a: Int }' },
    { 'graph/a.gql': 'type Query { a: Int b: Int }' },
  )
  assert.ok(fallback.result.blocks?.length)
  assert.equal(fallback.result.breaking, fallback.gate.change === 'breaking')
  const invalid = describe(
    t,
    { 'graph/a.gql': 'type Query { a: Int }' },
    { 'graph/a.gql': 'type {' },
  )
  assert.equal(invalid.result.breaking, false)
  assert.equal(invalid.gate.change, 'breaking')
  assert.match(invalid.result.items[0], /^GraphQL schema not described: graph\/a.gql:/)
  assert.equal(invalid.result.blocks, undefined)
  assert.equal(invalid.result.signals, undefined)
})

test('extra contract constructs agree with the gate and produce valid schema facts', (t) => {
  const pairs = [
    ['type Q { x: Int }', 'type Q { x(a: Int): Int }', 'safe'],
    ['type Q { x: [Int] }', 'type Q { x: [Int!] }', 'safe'],
    ['input I { x: Int = 1 }', 'input I { x: Int = 2 }', 'breaking'],
    ['union U = A | B', 'union U = A | C', 'breaking'],
    ['union U = A', 'union U = A | B', 'dangerous'],
    ['type Q implements N { x: Int }', 'type Q { x: Int }', 'breaking'],
    ['scalar X', '', 'breaking'],
  ]
  for (const [before, after, risk] of pairs) {
    const { result, gate } = describe(t, { 'schema.graphql': before }, { 'schema.graphql': after })
    assert.equal(result.breaking, gate.change === 'breaking')
    assert.equal(result.blocks?.[0].types[0].class, risk)
    assert.deepEqual(refineDescription(boundDescription('boss-verify-graphql', result)), [])
  }
  for (const [before, after] of [
    ['directive @tag on FIELD', ''],
    ['schema { query: Q }', 'schema { query: Other }'],
  ]) {
    const { result, gate } = describe(t, { 'schema.graphql': before }, { 'schema.graphql': after })
    assert.equal(result.breaking, gate.change === 'breaking')
    assert.equal(result.breaking, true)
    assert.equal(result.blocks, undefined)
  }
})

test('multiline directives and argument defaults stay single-line and bounded', () => {
  const after =
    'type Q { x(a: String = """first\nsecond"""): ID @deprecated(reason: """old\nnew""") }'
  const result = block('', after)
  assert.ok(result)
  assert.deepEqual(refineDescribed({ blocks: [result] }), [])
  assert.equal(result.types[0].fields[0].args[0].default, '"""first second"""')
  assert.deepEqual(result.types[0].fields[0].directives, ['@deprecated(reason: """old new""")'])
  assert.deepEqual(
    refineDescription(boundDescription('boss-verify-graphql', { blocks: [result] })),
    [],
  )
})

test('blocks and breaking items respect type, field, name, argument and directive bounds', (t) => {
  const name = 'T'.repeat(180)
  const args = Array.from({ length: 10 }, (_, i) => `a${i}: String = "${'a'.repeat(100)}"`).join(
    ' ',
  )
  const directives = Array.from({ length: 6 }, (_, i) => `@d${i}`).join(' ')
  const huge = Array.from(
    { length: 30 },
    (_, i) => `type T${i} { ${Array.from({ length: 70 }, (_, j) => `f${j}: Int`).join(' ')} }`,
  ).join('\n')
  const result = block('', huge)
  assert.deepEqual(refineDescribed({ blocks: [result] }), [])
  assert.equal(result.types.length, 24)
  assert.equal(result.types[0].fields.length, 60)
  const long = block('', `type ${name} { x(${args}): ${name} ${directives} }`)
  assert.deepEqual(refineDescribed({ blocks: [long] }), [])
  assert.equal(long.types[0].name.length, 120)
  assert.equal(long.types[0].fields[0].type.length, 120)
  assert.equal(long.types[0].fields[0].args.length, 8)
  assert.equal(long.types[0].fields[0].args[0].default.length, 80)
  assert.equal(long.types[0].fields[0].directives.length, 4)
  assert.deepEqual(
    refineDescription(boundDescription('boss-verify-graphql', { blocks: [long] })),
    [],
  )
  const removed = describe(t, { 'schema.gql': huge }, { 'schema.gql': '' }).result
  assert.equal(removed.items.length, 20)
  assert.equal(removed.items[0], '0 additive GraphQL changes, 30 breaking')
  assert.equal(removed.items[19], '12 more breaking GraphQL changes')
})

test('no schema change, executable documents and absent GraphQL paths are empty', (t) => {
  for (const [before, after] of [
    [{ 'readme.md': 'before' }, { 'readme.md': 'after' }],
    [{ 'query.graphql': 'query { a }' }, { 'query.graphql': 'query { b }' }],
    [{ 'schema.gql': 'type Q { a: Int }' }, { 'schema.gql': 'type Q { a: Int @tag }' }],
  ])
    assert.deepEqual(describe(t, before, after).result, { items: [], breaking: false })
})

test('deleted SDL and renamed SDL use both sides of the range', (t) => {
  const removed = describe(t, { 'schema.graphqls': 'scalar Old' }, { 'schema.graphqls': null })
  assert.equal(removed.result.blocks?.[0].types[0].change, 'removed')
  const repo = gitFixture(t)
  repo.write({ 'old/schema.graphql': 'type Query { a: Int }' })
  const base = repo.commit()
  repo.move('old/schema.graphql', 'new/schema.graphql')
  const head = repo.commit()
  assert.deepEqual(
    describeGraphql(envelopeFor(repo.dir, base, head, 'api-contract'), { cwd: repo.dir }),
    { items: [], breaking: false },
  )
})

const additiveBefore = {
  'graph/hospital.graphqls':
    '  type Hospital {\n    createdAt: Time!\n    id: ID!\n    latitude: Float!\n    longitude: Float!\n    name: String!\n    providerId: String!\n    updatedAt: Time!\n  }',
  'graph/schema.graphqls':
    'type Mutation { escalator: EscalatorOps! @goField(forceResolver: true) @authenticate outgoingMessage: OutgoingMessageOps! @goField(forceResolver: true) @authenticate }',
}

const additiveAfter = {
  'graph/hospital.graphqls':
    '  type Hospital {\n    claimNumberRequestEmail: String @goField(forceResolver: true)\n createdAt: Time!\n    id: ID!\n    latitude: Float!\n    longitude: Float!\n    name: String!\n    providerId: String!\n    state: String\n updatedAt: Time!\n  }\n  type HospitalOps {\n    update(input: UpdateHospitalInput!): Hospital @goField(forceResolver: true)\n  }\n\n  input UpdateHospitalInput {\n    """\n    Omit to leave the stored address unchanged.\n    """\n    claimNumberRequestEmail: String\n    hospitalId: ID!\n  }',
  'graph/schema.graphqls':
    'type Mutation { escalator: EscalatorOps! @goField(forceResolver: true) @authenticate hospital: HospitalOps! @goField(forceResolver: true) @authenticate outgoingMessage: OutgoingMessageOps! @goField(forceResolver: true) @authenticate }',
}

const additiveTypes = [
  {
    change: 'changed',
    class: 'safe',
    keyword: 'type',
    name: 'Mutation',
    fields: [
      {
        change: 'added',
        directives: ['@goField(forceResolver: true)', '@authenticate'],
        name: 'hospital',
        type: 'HospitalOps!',
      },
    ],
  },
  {
    change: 'changed',
    class: 'safe',
    keyword: 'type',
    name: 'Hospital',
    fields: [
      {
        change: 'added',
        directives: ['@goField(forceResolver: true)'],
        name: 'claimNumberRequestEmail',
        type: 'String',
      },
      {
        change: 'added',
        name: 'state',
        type: 'String',
      },
    ],
  },
  {
    change: 'added',
    class: 'safe',
    keyword: 'type',
    name: 'HospitalOps',
    fields: [
      {
        args: [
          {
            name: 'input',
            type: 'UpdateHospitalInput!',
          },
        ],
        directives: ['@goField(forceResolver: true)'],
        name: 'update',
        type: 'Hospital',
      },
    ],
  },
  {
    change: 'added',
    class: 'safe',
    keyword: 'input',
    name: 'UpdateHospitalInput',
    fields: [
      {
        name: 'claimNumberRequestEmail',
        type: 'String',
      },
      {
        name: 'hospitalId',
        type: 'ID!',
      },
    ],
  },
]

const breakingBefore = {
  'graph/claim.graphqls':
    '  type Claim {\n    id: ID!\n    note: String!\n    total(currency: String): Float\n    status: Status!\n  }\n\n  enum Status {\n    DRAFT\n    SENT\n    VOID\n  }\n\n  input ClaimInput {\n    id: ID!\n    note: String\n  }\n\n  type Legacy {\n    id: ID!\n  }',
  'graph/schema.graphqls': 'type Query { claim(id: ID!): Claim @authenticate legacy: Legacy }',
}

const breakingAfter = {
  'graph/claim.graphqls':
    '  type Claim {\n    id: ID!\n    note: String\n    total: Float\n    status: Status!\n    paidAt: Time @goField(forceResolver: true)\n  }\n\n  enum Status {\n    DRAFT\n    SENT\n    PAID\n  }\n\n  input ClaimInput {\n    id: ID!\n    note: String!\n    reason: String!\n  }',
  'graph/schema.graphqls': 'type Query { claim(id: ID!, region: String!): Claim @authenticate }',
}

const breakingTypes = [
  {
    change: 'changed',
    class: 'breaking',
    keyword: 'type',
    name: 'Query',
    fields: [
      {
        name: 'claim',
        type: 'Claim',
        args: [
          {
            name: 'id',
            type: 'ID!',
          },
          {
            name: 'region',
            type: 'String!',
          },
        ],
        directives: ['@authenticate'],
        change: 'changed',
      },
      {
        name: 'legacy',
        type: 'Legacy',
        change: 'removed',
      },
    ],
  },
  {
    change: 'changed',
    class: 'breaking',
    keyword: 'type',
    name: 'Claim',
    fields: [
      {
        name: 'note',
        type: 'String',
        change: 'changed',
        before: 'String!',
      },
      {
        name: 'total',
        type: 'Float',
        change: 'changed',
      },
      {
        name: 'paidAt',
        type: 'Time',
        directives: ['@goField(forceResolver: true)'],
        change: 'added',
      },
    ],
  },
  {
    change: 'changed',
    class: 'breaking',
    keyword: 'enum',
    name: 'Status',
    fields: [
      {
        name: 'VOID',
        change: 'removed',
      },
      {
        name: 'PAID',
        change: 'added',
      },
    ],
  },
  {
    change: 'changed',
    class: 'breaking',
    keyword: 'input',
    name: 'ClaimInput',
    fields: [
      {
        name: 'note',
        type: 'String!',
        change: 'changed',
        before: 'String',
      },
      {
        name: 'reason',
        type: 'String!',
        change: 'added',
      },
    ],
  },
  {
    change: 'removed',
    class: 'breaking',
    keyword: 'type',
    name: 'Legacy',
    fields: [
      {
        name: 'id',
        type: 'ID!',
      },
    ],
  },
]
