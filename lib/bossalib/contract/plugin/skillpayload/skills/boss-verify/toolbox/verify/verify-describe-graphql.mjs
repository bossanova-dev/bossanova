// GraphQL schema facts use the gate's comparator; display risk never changes its verdict.
import path from 'node:path'

import { isMainModule } from '../main-module.mjs'
import {
  blockFact,
  describerMain,
  envelopeDescriber,
  item,
  plural,
  runCli,
  signalFact,
} from './describer-kit.mjs'
import {
  MAX_SCHEMA_FIELDS,
  MAX_SCHEMA_TYPES,
  SCHEMA_CLASSES,
  SCHEMA_KEYWORDS,
} from './verify-describe-blocks.mjs'
import { graphqlSchemaFindings, parseGraphqlSchema } from './verify-graphql-diff.mjs'

/** @typedef {import('./verify-graphql-diff.mjs').GraphqlSchema} GraphqlSchema */
/** @typedef {import('./verify-graphql-diff.mjs').SchemaFinding} SchemaFinding */
/** @typedef {import('./verify-graphql-diff.mjs').Field} Field */
/** @typedef {import('./verify-describe-blocks.mjs').SchemaType} SchemaType */
/** @typedef {import('./verify-describe-blocks.mjs').SchemaField} SchemaField */

/** @param {string} text @param {number} [limit] */
const shorten = (text, limit = 120) => {
  const line = text.replace(/\s+/g, ' ')
  return line.length > limit ? `${line.slice(0, limit - 1)}…` : line
}
/** @param {string} risk */
const rank = (risk) => SCHEMA_CLASSES.indexOf(risk)

/** @param {string} name @param {Field} [field] @returns {SchemaField} */
function displayField(name, field) {
  if (!field) return { name: shorten(name) }
  const args = Object.entries(field.args)
    .slice(0, 8)
    .map(([name, arg]) => ({
      name: shorten(name),
      type: shorten(arg.type),
      ...(arg.defaultValue === undefined ? {} : { default: shorten(arg.defaultValue, 80) }),
    }))
  const directives = (field.directives ?? []).slice(0, 4).map((text) => shorten(text))
  return {
    name: shorten(name),
    type: shorten(field.type),
    ...(args.length ? { args } : {}),
    ...(directives.length ? { directives } : {}),
  }
}

/** Pure schema-block builder, ordered by severity and the source declaration positions.
 * @param {GraphqlSchema} base @param {GraphqlSchema} head @param {SchemaFinding[]} findings
 * @returns {{kind: 'schema', types: SchemaType[]} | null} */
export function graphqlSchemaBlock(base, head, findings) {
  /** @type {Map<string, SchemaType>} */
  const entries = new Map()
  /** @type {Map<string, Map<string, SchemaField>>} */
  const fields = new Map()
  const ordered = [...findings].sort(
    (a, b) =>
      rank(a.class) - rank(b.class) ||
      a.pass - b.pass ||
      a.typeIndex - b.typeIndex ||
      a.fieldIndex - b.fieldIndex,
  )
  for (const finding of ordered) {
    const { type: name, kind } = finding
    if (kind === 'root-changed' || kind === 'root-added') continue
    const definition = kind === 'type-removed' ? base.types[name] : head.types[name]
    if (!definition || !SCHEMA_KEYWORDS.includes(definition.kind)) continue
    let entry = entries.get(name)
    if (!entry) {
      entry = {
        name: shorten(name),
        keyword: definition.kind,
        change: kind === 'type-added' ? 'added' : kind === 'type-removed' ? 'removed' : 'changed',
        class: finding.class,
        fields: [],
      }
      entries.set(name, entry)
      fields.set(name, new Map())
    }
    const contributed = fields.get(name)
    if (kind === 'type-added' || kind === 'type-removed') {
      for (const [fieldName, field] of Object.entries(definition.fields))
        contributed.set(fieldName, displayField(fieldName, field))
      for (const member of definition.members) contributed.set(member, displayField(member))
      continue
    }
    const fieldName = finding.field ?? finding.member
    if (!fieldName || contributed.has(fieldName)) continue
    const removed = kind === 'field-removed' || kind === 'member-removed'
    const added = kind === 'field-added' || kind === 'member-added'
    const source = removed ? base : head
    const field = source.types[name]?.fields[fieldName]
    const before = base.types[name]?.fields[fieldName]
    contributed.set(fieldName, {
      ...displayField(fieldName, field),
      change: removed ? 'removed' : added ? 'added' : 'changed',
      ...(!removed && !added && before && field && before.type !== field.type
        ? { before: shorten(before.type) }
        : {}),
    })
  }
  const roots = new Set(
    Object.keys(head.roots).length
      ? Object.values(head.roots)
      : ['Query', 'Mutation', 'Subscription'],
  )
  const types = [...entries.entries()]
    .sort(
      ([a, first], [b, next]) =>
        rank(first.class) - rank(next.class) || Number(roots.has(b)) - Number(roots.has(a)),
    )
    .slice(0, MAX_SCHEMA_TYPES)
    .map(([name, entry]) => ({
      ...entry,
      fields: [...fields.get(name).values()].slice(0, MAX_SCHEMA_FIELDS),
    }))
  return types.length ? { kind: 'schema', types } : null
}

/** @param {string} file */
const graphql = (file) => /\.(?:graphqls?|gql)$/.test(file)

/** @param {Map<string, string>} files */
const parseFiles = (files) => parseGraphqlSchema([...files.values()].join('\n'))

/** @param {Parameters<import('./describer-kit.mjs').CollectItems>[0]} input
 * @returns {import('./describer-kit.mjs').Fact[]} */
export function collectGraphqlFacts({ git, base, head, changes }) {
  const paths = [
    ...new Set(
      changes.flatMap((change) =>
        [change.path, change.previousPath].filter((file) => file && graphql(file)),
      ),
    ),
  ]
  if (!paths.length) return []
  const dirs = [...new Set(paths.map((file) => path.posix.dirname(file)))]
  const sides = [base, head].map((ref) => {
    const files = git
      .filesAt(ref, dirs)
      .filter(
        (file) =>
          graphql(file) && (paths.includes(file) || dirs.includes(path.posix.dirname(file))),
      )
      .sort()
    return git.readFiles(ref, files)
  })
  const changed = sides.map((files) => new Map([...files].filter(([file]) => paths.includes(file))))
  const changedSchemas = changed.map(parseFiles)
  let schemas = sides.map(parseFiles)
  if (schemas.some((schema) => !schema.ok)) {
    schemas = changedSchemas
    if (schemas.some((schema) => !schema.ok)) {
      for (const files of changed)
        for (const [file, text] of files) {
          const parsed = parseGraphqlSchema(text)
          if (parsed.ok === false)
            return [
              item('api-contract', `GraphQL schema not described: ${file}: ${parsed.error}`, paths),
            ]
        }
      const failed = schemas.find((schema) => !schema.ok)
      return [
        item(
          'api-contract',
          `GraphQL schema not described: the combined schema: ${failed?.ok === false ? failed.error : 'unreadable'}`,
          paths,
        ),
      ]
    }
  }
  let [before, after] = schemas
  if (!before.ok || !after.ok) return []
  let findings = graphqlSchemaFindings(before.schema, after.schema)
  const [changedBefore, changedAfter] = changedSchemas
  if (changedBefore.ok && changedAfter.ok) {
    const changedFindings = graphqlSchemaFindings(changedBefore.schema, changedAfter.schema)
    const breaks = (/** @type {SchemaFinding[]} */ entries) =>
      entries.some((finding) => finding.class === 'breaking')
    // Sibling declarations can turn a new input type into a required-field addition.
    // Keep directory context only when it preserves the gate's changed-file verdict.
    if (breaks(findings) !== breaks(changedFindings)) {
      before = changedBefore
      after = changedAfter
      findings = changedFindings
    }
  }
  if (!findings.length) return []
  const breaking = findings.filter((finding) => finding.class === 'breaking')
  const risky = findings.filter((finding) => finding.class === 'dangerous').length
  const summary = `${plural(findings.length - breaking.length, 'additive GraphQL change')}, ${breaking.length} breaking${risky ? ` (${risky} risky)` : ''}`
  const facts = [
    item('api-contract', summary, paths),
    ...breaking
      .slice(0, 18)
      .map((finding) =>
        item('api-contract', `Breaking GraphQL change: ${finding.text}`, paths, true),
      ),
  ]
  if (breaking.length > 18)
    facts.push(
      item('api-contract', `${breaking.length - 18} more breaking GraphQL changes`, paths, true),
    )
  const block = graphqlSchemaBlock(before.schema, after.schema, findings)
  if (block) facts.push(blockFact('api-contract', block, paths))
  facts.push(
    signalFact(
      {
        key: 'api',
        label: 'API',
        state: breaking.length ? 'breaking' : risky ? 'review' : 'additive',
        summary,
      },
      paths,
    ),
  )
  return facts
}

export const describeGraphql = envelopeDescriber(collectGraphqlFacts)
export const main = () => describerMain(describeGraphql)

if (isMainModule(import.meta.url)) {
  if (process.argv.length > 2) runCli(import.meta.url, 'boss-verify-graphql', collectGraphqlFacts)
  else await main()
}
