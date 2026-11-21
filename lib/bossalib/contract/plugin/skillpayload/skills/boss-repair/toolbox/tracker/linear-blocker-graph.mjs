import { linearRequest } from '../linear-gate-lib.mjs'
import { TRACKER_CREDENTIALS_MISSING } from './adapter-core.mjs'

const QUERY = `query BlockerGraphNode($id: String!) {
  issue(id: $id) {
    id identifier state { type } parent { identifier }
    relations(first: 50) { nodes { type relatedIssue { id identifier state { type } } } pageInfo { hasNextPage } }
    inverseRelations(first: 50) { nodes { type issue { id identifier state { type } } } pageInfo { hasNextPage } }
  }
}`

/** @param {{apiKey?: string, fetchImpl?: typeof fetch, endpoint?: string, issueId: string}} options
 * @returns {Promise<import('./blocker-graph.mjs').BlockerGraphNode>} */
export async function linearReadBlockerGraphNode(options) {
  if (!options.issueId?.trim()) throw new Error('tracker/linear: missing issue id')
  if (!options.apiKey)
    throw Object.assign(new Error('LINEAR_API_KEY is not set'), {
      code: TRACKER_CREDENTIALS_MISSING,
    })
  const { issue } = await linearRequest({
    ...options,
    apiKey: options.apiKey,
    query: QUERY,
    variables: { id: options.issueId },
  })
  const ref = (value) => {
    if (!value?.id || !value.identifier || !value.state?.type)
      throw new Error('tracker/linear: unreadable blocker graph node')
    return { id: value.id, identifier: value.identifier, stateType: value.state.type }
  }
  const edges = (connection, key) => {
    if (!Array.isArray(connection?.nodes) || typeof connection.pageInfo?.hasNextPage !== 'boolean')
      throw new Error('tracker/linear: unreadable blocker graph relations')
    return connection.nodes.flatMap((row) => {
      if (typeof row?.type !== 'string') throw new Error('tracker/linear: unreadable relation type')
      return row.type === 'blocks' ? [ref(row[key])] : []
    })
  }
  const blocks = edges(issue?.relations, 'relatedIssue'),
    blockedBy = edges(issue?.inverseRelations, 'issue')
  if (issue.parent !== null && !issue.parent?.identifier)
    throw new Error('tracker/linear: unreadable graph parent')
  return {
    ...ref(issue),
    parentId: issue.parent?.identifier ?? null,
    blocks,
    blockedBy,
    relationsComplete:
      !issue.relations.pageInfo.hasNextPage && !issue.inverseRelations.pageInfo.hasNextPage,
  }
}
