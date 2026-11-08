import assert from 'node:assert/strict'
import { test } from 'node:test'
import { parseGraphqlSchema, graphqlSchemaChanges } from './verify-graphql-diff.mjs'

/** @param {string} text */
function schema(text) {
  const result = parseGraphqlSchema(text)
  assert.equal(result.ok, true, JSON.stringify(result))
  if (!result.ok) throw new Error(result.error)
  return result.schema
}
/** @param {string} base @param {string} head */
const diff = (base, head) => graphqlSchemaChanges(schema(base), schema(head))

test('field additions and removals classify the server contract', () => {
  assert.equal(
    diff('type Query { id: ID }', 'type Query { id: ID name: String }').breaking.length,
    0,
  )
  assert.deepEqual(diff('type Query { id: ID name: String }', 'type Query { id: ID }').breaking, [
    'Query.name removed',
  ])
})

test('parses SDL kinds, extensions, directives, descriptions and merged definitions', () => {
  const text = `
    "schema" schema @tag { query: Query mutation: Mutation }
    scalar Date @tag
    interface Node { id: ID! }
    interface Named implements Node { id: ID! name: String }
    type Query implements Node & Named @tag { id: ID! name: String node(input: Filter = {ids: [1, 2], enabled: true}): Node }
    extend type Query { today: Date }
    type Query { search: Search }
    type Mutation { save: Boolean }
    union Search = Query | Mutation
    extend union Search = Named
    enum Choice { ONE @tag TWO }
    extend enum Choice { THREE }
    input Filter { ids: [ID!] enabled: Boolean = true }
    extend input Filter { choice: Choice = ONE }
    directive @tag(arg: String = "a  b") repeatable on FIELD_DEFINITION | OBJECT | SCHEMA | SCALAR | ENUM_VALUE
    extend scalar Date @tag
    extend interface Node { extra: String }
    extend schema { subscription: Query }
  `
  assert.deepEqual(graphqlSchemaChanges(schema(text), schema(text)), { breaking: [], additive: [] })
  assert.deepEqual(
    diff(
      'type Query { id: ID } extend type Query { name: String }',
      'type Query { name: String id: ID }',
    ),
    { breaking: [], additive: [] },
  )
})

test('executable documents are validated then ignored', () => {
  const documents = `query Find($id: ID! = "x", $filter: Filter = {ids: [1]}) @skip(if: false) {
    alias: node(id: $id) { ...Bits ... on User @tag { name } }
  }
  mutation { save(value: -2.5e+2) }
  subscription Updates { changed }
  fragment Bits on User @tag { id }
  { node { id } }`
  assert.deepEqual(graphqlSchemaChanges(schema(''), schema(documents)), {
    breaking: [],
    additive: [],
  })
})

test('rejects malformed tokens, values, types and executable definitions', () => {
  for (const text of [
    'type Query {',
    'type { x: ID }',
    'type Query { x }',
    'type Query { x: [ID }',
    'type Query { x: ID!! }',
    'type Query { x: String = "bad" }',
    'scalar',
    'input X { x: Int = 01 }',
    'input X { x: Float = 1. }',
    'input X { x: String = "bad\\q" }',
    'input X { x: String = "unfinished }',
    'input X { x: Int = $var }',
    'directive @x on',
    'union U =',
    'query Q { x( }',
    'query Q { x } junk',
    'fragment F User { id }',
    'type Query { x: ID } %',
    'type Query { x: ID } }',
  ])
    assert.equal(parseGraphqlSchema(text).ok, false, text)
})

test('token/default whitespace is normalized without changing string content', () => {
  assert.deepEqual(diff('input I { x: [Int] = [1, 2] }', 'input I { x: [Int] = [ 1 2 ] }'), {
    breaking: [],
    additive: [],
  })
  assert.ok(diff('input I { x: String = "a  b" }', 'input I { x: String = "a b" }').breaking.length)
  assert.equal(parseGraphqlSchema('"""line\n description""" scalar X # end').ok, true)
  assert.ok(diff('input I { x: Int = 2 }', 'input I { x: Int }').breaking.length)
  assert.ok(diff('input I { x: Int = 2 }', 'input I { x: Int = 3 }').breaking.length)
})

test('nullability is monotonic at every list level for inputs and outputs', () => {
  assert.equal(diff('type Q { x: [ID] }', 'type Q { x: [ID!]! }').breaking.length, 0)
  assert.ok(diff('type Q { x: [ID!]! }', 'type Q { x: [ID] }').breaking.length)
  assert.equal(diff('input I { x: [ID!]! }', 'input I { x: [ID] }').breaking.length, 0)
  assert.ok(diff('input I { x: [ID] }', 'input I { x: [ID!] }').breaking.length)
  assert.ok(diff('type Q { x(id: ID): ID }', 'type Q { x(id: ID!): ID }').breaking.length)
  assert.equal(diff('type Q { x(id: ID!): ID }', 'type Q { x(id: ID): ID }').breaking.length, 0)
  assert.ok(diff('type Q { x: ID }', 'type Q { x: [ID] }').breaking.length)
})

test('required new inputs break unless they have defaults', () => {
  for (const pair of [
    ['input I { a: ID }', 'input I { a: ID b: ID! }'],
    ['type Q { x: ID }', 'type Q { x(b: ID!): ID }'],
  ])
    assert.ok(diff(...pair).breaking.length)
  assert.equal(diff('input I { a: ID }', 'input I { a: ID b: ID! = "x" }').breaking.length, 0)
  assert.equal(diff('type Q { x: ID }', 'type Q { x(b: ID): ID }').breaking.length, 0)
})

test('removed contract members, changed kinds and roots break', () => {
  for (const [base, head] of [
    ['scalar X', ''],
    ['scalar X', 'enum X { A }'],
    ['enum X { A B }', 'enum X { A }'],
    ['union X = A | B', 'union X = A'],
    [
      'interface N { id: ID } type Q implements N { id: ID }',
      'interface N { id: ID } type Q { id: ID }',
    ],
    ['directive @x on FIELD', ''],
    ['type Q { x(a: ID): ID }', 'type Q { x: ID }'],
    ['schema { query: A }', 'schema { query: B }'],
  ])
    assert.ok(diff(base, head).breaking.length, base)
  assert.equal(
    diff('enum X { A } union U = A', 'enum X { A B } union U = A | B').breaking.length,
    0,
  )
})

test('repeated definitions merge argument contributions and reject contradictory signatures', () => {
  assert.deepEqual(
    diff('type Q { x(a: ID): ID } type Q { x(b: ID): ID }', 'type Q { x(a: ID b: ID): ID }'),
    { breaking: [], additive: [] },
  )
  assert.equal(parseGraphqlSchema('type Q { x: ID } type Q { x: Int }').ok, false)
  assert.equal(parseGraphqlSchema('scalar tag directive @tag on FIELD').ok, true)
})

test('explicit roots are compared with the conventional default roots', () => {
  assert.ok(
    diff(
      'type Query { x: ID } type Other { x: ID }',
      'schema { query: Other } type Query { x: ID } type Other { x: ID }',
    ).breaking.length,
  )
  assert.deepEqual(diff('type Query { x: ID }', 'schema { query: Query } type Query { x: ID }'), {
    breaking: [],
    additive: [],
  })
})

test('removing directive repeatability breaks existing uses, while adding it is additive', () => {
  const single = 'directive @tag on FIELD'
  const repeatable = 'directive @tag repeatable on FIELD'
  assert.ok(diff(repeatable, single).breaking.length)
  assert.equal(diff(single, repeatable).breaking.length, 0)
  assert.ok(diff(single, repeatable).additive.length)
  assert.equal(parseGraphqlSchema(`${single} ${repeatable}`).ok, false)
})
