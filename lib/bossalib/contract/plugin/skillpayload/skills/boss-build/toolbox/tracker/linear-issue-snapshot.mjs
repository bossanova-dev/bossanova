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
  const snapshot = {
    id: issue.identifier,
    identifier: issue.identifier,
    uuid: issue.id,
    title: issue.title,
    description: issue.description,
    status: issue.state?.name,
    state: issue.state,
    team: issue.team?.name ?? null,
    attachments: issue.attachments.nodes,
  }
  const problems = issueSnapshotProblems(snapshot)
  if (problems.length)
    throw new Error(`readIssueSnapshot: incomplete snapshot (${problems.join(', ')})`)
  snapshot.description ??= ''
  return snapshot
}
