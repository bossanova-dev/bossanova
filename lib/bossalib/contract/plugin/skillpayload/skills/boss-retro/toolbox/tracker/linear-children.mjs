import { readFileSync } from 'node:fs'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'
import { linearRequest, resolveLinearUserId } from '../linear-gate-lib.mjs'
import { TRACKER_CREDENTIALS_MISSING } from './adapter-core.mjs'
import { READ_ISSUE_SNAPSHOT_QUERY } from './linear-issue-snapshot.mjs'

export const LINEAR_LIST_CHILDREN_OPERATION = {
  command: { helper: 'tracker/linear-children.mjs', verb: 'list-children' },
  summary:
    '{parentId} -> {parent, children, relationWarnings} with hydrated relations and full descriptions',
}
/** @param {string} apiKey */
function requireApiKey(apiKey) {
  if (!apiKey?.trim())
    throw Object.assign(new Error('LINEAR_API_KEY is not set'), {
      code: TRACKER_CREDENTIALS_MISSING,
    })
}
const CHILDREN_QUERY = `query IssueChildren($id: String!, $after: String) {
  issue(id: $id) {
    children(first: 250, after: $after) {
      nodes { identifier }
      pageInfo { hasNextPage endCursor }
    }
  }
}`
/** @typedef {{id: string, identifier: string, state: {name: string, type: string}}} RelatedIssue */
/** @typedef {{type: string, issue?: RelatedIssue, relatedIssue?: RelatedIssue}} Relation */
/** @template T @typedef {{nodes: T[], pageInfo: {hasNextPage: boolean}}} Connection */
/** @typedef {RelatedIssue & {title: string, description: string|null, url: string,
 * priority: number, estimate: number|null, createdAt: string, updatedAt: string,
 * archivedAt: string|null, parent: {identifier: string}|null,
 * assignee: {id: string}|null, creator: {id: string}|null, project: {id: string}|null,
 * labels: Connection<{name: string}>, attachments: Connection<object>,
 * relations: Connection<Relation>, inverseRelations: Connection<Relation>}} RawIssue */

/** @template T @param {Connection<T>} connection @param {string} issue @param {string} field */
function completeNodes(connection, issue, field) {
  if (!Array.isArray(connection?.nodes) || connection?.pageInfo?.hasNextPage !== false)
    throw new Error(`listChildren: ${issue} incomplete ${field}`)
  return connection.nodes
}
/** @param {RawIssue} raw @param {string} requestedId */
function normalizeIssue(raw, requestedId) {
  if (!raw?.id || !raw.identifier) throw new Error(`listChildren: missing issue ${requestedId}`)
  const relations = { blockedBy: [], blocks: [], relatedTo: [] }
  for (const [field, inverse] of /** @type {const} */ ([
    ['relations', false],
    ['inverseRelations', true],
  ])) {
    for (const relation of completeNodes(raw[field], raw.identifier, field)) {
      const target = inverse ? relation.issue : relation.relatedIssue
      const bucket =
        relation.type === 'related'
          ? relations.relatedTo
          : relation.type === 'blocks'
            ? inverse
              ? relations.blockedBy
              : relations.blocks
            : null
      if (!bucket) continue
      if (!target?.id || !target.identifier || !target.state?.name || !target.state.type)
        throw new Error(`listChildren: ${raw.identifier} incomplete ${field} target`)
      if (!bucket.some((entry) => entry.uuid === target.id))
        bucket.push({
          id: target.identifier,
          identifier: target.identifier,
          uuid: target.id,
          state: target.state,
        })
    }
  }
  return {
    id: raw.identifier,
    identifier: raw.identifier,
    uuid: raw.id,
    title: raw.title,
    description: raw.description ?? '',
    url: raw.url,
    priority: raw.priority,
    estimate: raw.estimate,
    createdAt: raw.createdAt,
    updatedAt: raw.updatedAt,
    archivedAt: raw.archivedAt,
    state: raw.state,
    labels: completeNodes(raw.labels, raw.identifier, 'labels').map((label) => label.name),
    parentId: raw.parent?.identifier ?? null,
    assigneeId: raw.assignee?.id ?? null,
    creatorId: raw.creator?.id ?? null,
    projectId: raw.project?.id ?? null,
    attachments: completeNodes(raw.attachments, raw.identifier, 'attachments'),
    relations,
    blockedBy: relations.blockedBy.map((entry) => entry.id),
  }
}
/** @param {{parentId: string, apiKey: string, fetchImpl?: typeof fetch, endpoint?: string}} options */
export async function linearListChildren({ parentId, apiKey, fetchImpl, endpoint }) {
  if (typeof parentId !== 'string' || !parentId.trim())
    throw new Error('listChildren: non-empty parentId required')
  requireApiKey(apiKey)
  const request = (query, variables) =>
    linearRequest({ apiKey, fetchImpl, endpoint, query, variables })
  const ids = []
  const cursors = new Set()
  let after = null
  for (;;) {
    const data = await request(CHILDREN_QUERY, { id: parentId.trim(), after })
    const connection = data?.issue?.children
    if (!Array.isArray(connection?.nodes) || typeof connection?.pageInfo?.hasNextPage !== 'boolean')
      throw new Error(`listChildren: ${parentId} incomplete children`)
    for (const node of connection.nodes) {
      if (!node?.identifier) throw new Error(`listChildren: ${parentId} missing child identity`)
      ids.push(node.identifier)
    }
    if (!connection.pageInfo.hasNextPage) break
    const cursor = connection.pageInfo.endCursor
    if (typeof cursor !== 'string' || !cursor || cursors.has(cursor))
      throw new Error(`listChildren: ${parentId} children cursor did not advance`)
    cursors.add(cursor)
    after = cursor
  }
  const allIds = [parentId.trim(), ...ids]
  const issues = new Array(allIds.length)
  let next = 0
  await Promise.all(
    Array.from({ length: Math.min(4, allIds.length) }, async () => {
      while (next < allIds.length) {
        const index = next++
        const data = await request(READ_ISSUE_SNAPSHOT_QUERY, { id: allIds[index] })
        issues[index] = normalizeIssue(data?.issue, allIds[index])
      }
    }),
  )
  return { parent: issues[0], children: issues.slice(1), relationWarnings: [] }
}
/** @param {string[]} argv
 * @param {{env?: NodeJS.ProcessEnv, fetchImpl?: typeof fetch,
 * write?: (text: string) => unknown, errWrite?: (text: string) => unknown}} [deps] */
export async function main(argv, deps = {}) {
  const write = deps.write ?? ((text) => process.stdout.write(text))
  const errWrite = deps.errWrite ?? ((text) => process.stderr.write(text))
  const env = deps.env ?? process.env
  try {
    const parsed = parseArgs({
      args: argv,
      strict: true,
      allowPositionals: true,
      options: { 'args-file': { type: 'string' } },
    })
    const command = parsed.positionals[0]
    const preflight = command === 'preflight'
    if (
      parsed.positionals.length !== 1 ||
      (command !== 'list-children' && !preflight) ||
      (!preflight && !parsed.values['args-file'])
    )
      throw new Error('expected list-children|preflight --args-file <json>')
    const args = parsed.values['args-file']
      ? JSON.parse(readFileSync(parsed.values['args-file'], 'utf8'))
      : {}
    if (
      !args ||
      typeof args !== 'object' ||
      Array.isArray(args) ||
      Object.keys(args).some((key) => preflight || key !== 'parentId')
    )
      throw new Error(preflight ? 'expected {}' : 'expected {parentId}')
    const options = {
      parentId: args.parentId,
      apiKey: env.LINEAR_API_KEY,
      endpoint: env.LINEAR_API_ENDPOINT || undefined,
      fetchImpl: deps.fetchImpl,
    }
    let result
    if (preflight) {
      requireApiKey(options.apiKey)
      await resolveLinearUserId({ ...options, user: 'me', sleep: undefined })
      result = { ok: true }
    } else result = await linearListChildren(options)
    write(JSON.stringify(result) + '\n')
    return 0
  } catch (error) {
    errWrite(`list-children: ${error.message}\n`)
    return 2
  }
}
if (isMainModule(import.meta.url)) process.exitCode = await main(process.argv.slice(2))
