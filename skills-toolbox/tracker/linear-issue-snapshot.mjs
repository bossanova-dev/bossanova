import { linearRequest } from '../linear-gate-lib.mjs'
import { TRACKER_CREDENTIALS_MISSING } from './adapter-core.mjs'
import { issueSnapshotProblems } from './issue-snapshot.mjs'
export const READ_ISSUE_SNAPSHOT_QUERY = `query IssueSnapshot($id: String!) {
  issue(id: $id) {
    id identifier title description url priority estimate createdAt updatedAt archivedAt
    state { name type } team { name } parent { identifier }
    assignee { id } creator { id } project { id }
    labels(first: 250) { nodes { name } pageInfo { hasNextPage } }
    relations(first: 250) {
      nodes { type relatedIssue { id identifier state { name type } } }
      pageInfo { hasNextPage }
    }
    inverseRelations(first: 250) {
      nodes { type issue { id identifier state { name type } } }
      pageInfo { hasNextPage }
    }
    attachments(first: 250) { nodes { id title subtitle url createdAt } pageInfo { hasNextPage } }
  }
}`
/** @typedef {{id: string, identifier: string, state: {name: string, type: string}}} LinearRelatedIssue */
/** @typedef {{type: string, issue?: LinearRelatedIssue, relatedIssue?: LinearRelatedIssue}} LinearRelationNode */
/** @typedef {{nodes: LinearRelationNode[], pageInfo: {hasNextPage: boolean}}} LinearRelationConnection */
/** @typedef {{id: string, identifier: string, uuid: string, state: {name: string, type: string}}} NormalizedLinearRelation */
/** @typedef {{blockedBy: NormalizedLinearRelation[], blocks: NormalizedLinearRelation[], relatedTo: NormalizedLinearRelation[]}} NormalizedLinearRelations */
/** @typedef {{ok: boolean, relations: NormalizedLinearRelations | null, field?: 'relations' | 'inverseRelations', reason?: 'connection' | 'target'}} BucketLinearRelationsResult */
/** @param {unknown} value */
const nonEmpty = (value) => typeof value === 'string' && value.trim().length > 0
/**
 * Bucket Linear's `relations` and `inverseRelations` connections into `{blockedBy, blocks, relatedTo}`,
 * deduplicated by target UUID. Returns `{ok: false, relations: null, field, reason}` on any
 * incomplete connection or target without throwing.
 * @param {{relations?: LinearRelationConnection | null, inverseRelations?: LinearRelationConnection | null} | null | undefined} raw
 * @returns {BucketLinearRelationsResult}
 */
export function bucketLinearRelations(raw) {
  /** @type {NormalizedLinearRelations} */
  const relations = { blockedBy: [], blocks: [], relatedTo: [] }
  for (const [field, inverse] of /** @type {const} */ ([
    ['relations', false],
    ['inverseRelations', true],
  ])) {
    const connection = raw?.[field]
    if (!Array.isArray(connection?.nodes) || connection?.pageInfo?.hasNextPage !== false)
      return { ok: false, relations: null, field, reason: 'connection' }
    for (const relation of connection.nodes) {
      if (!relation || typeof relation !== 'object')
        return { ok: false, relations: null, field, reason: 'target' }
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
      if (
        !nonEmpty(target?.id) ||
        !nonEmpty(target?.identifier) ||
        !nonEmpty(target?.state?.name) ||
        !nonEmpty(target?.state?.type)
      )
        return { ok: false, relations: null, field, reason: 'target' }
      if (!bucket.some((entry) => entry.uuid === target.id))
        bucket.push({
          id: target.identifier,
          identifier: target.identifier,
          uuid: target.id,
          state: target.state,
        })
    }
  }
  return { ok: true, relations }
}
export async function linearReadIssueSnapshot({ apiKey, fetchImpl, endpoint, issueId }) {
  if (typeof issueId !== 'string' || !issueId.trim())
    throw new Error('readIssueSnapshot: non-empty issue id required')
  if (!apiKey)
    throw Object.assign(new Error('LINEAR_API_KEY is not set'), {
      code: TRACKER_CREDENTIALS_MISSING,
    })
  const data = await linearRequest({
    apiKey,
    fetchImpl,
    endpoint,
    query: READ_ISSUE_SNAPSHOT_QUERY,
    variables: { id: issueId.trim() },
  })
  const issue = data?.issue
  if (
    !issue ||
    ![issue.id, issue.identifier].every((value) => typeof value === 'string' && value.trim())
  )
    throw new Error('readIssueSnapshot: missing issue identity')
  if (issue.attachments?.pageInfo?.hasNextPage !== false)
    throw new Error('readIssueSnapshot: incomplete attachments')
  if (!Array.isArray(issue.labels?.nodes) || issue.labels?.pageInfo?.hasNextPage !== false)
    throw new Error('readIssueSnapshot: incomplete labels')
  const labels = issue.labels.nodes.map((node) => {
    if (
      !node ||
      typeof node !== 'object' ||
      Array.isArray(node) ||
      typeof node.name !== 'string' ||
      !node.name.trim()
    )
      throw new Error('readIssueSnapshot: invalid label node')
    return node.name
  })
  const bucketed = bucketLinearRelations(issue)
  const snapshot = {
    id: issue.identifier,
    identifier: issue.identifier,
    uuid: issue.id,
    title: issue.title,
    description: issue.description,
    status: issue.state?.name,
    state: issue.state,
    team: issue.team?.name ?? null,
    labels,
    attachments: issue.attachments.nodes,
    ...(bucketed.ok ? { relations: bucketed.relations } : {}),
  }
  const problems = issueSnapshotProblems(snapshot)
  if (problems.length)
    throw new Error(`readIssueSnapshot: incomplete snapshot (${problems.join(', ')})`)
  snapshot.description ??= ''
  return snapshot
}
