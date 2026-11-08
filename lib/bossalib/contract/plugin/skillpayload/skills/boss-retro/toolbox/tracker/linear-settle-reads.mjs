import { linearRequest } from '../linear-gate-lib.mjs'
import { TRACKER_CREDENTIALS_MISSING } from './adapter-core.mjs'

const COMMENT_PAGE_CAP = 5
const COMMENTS_QUERY = `query ClaimComments($id: String!, $after: String) {
  issue(id: $id) { id identifier comments(first: 100, after: $after) {
    nodes { id body createdAt } pageInfo { hasNextPage endCursor }
  } }
}`
const STATE_QUERY = `query IssueState($id: String!) {
  issue(id: $id) { id identifier state { name type } }
}`

/** @param {unknown} value @param {string} field */
function requireString(value, field) {
  if (typeof value !== 'string' || !value.trim())
    throw new Error(`tracker/linear: missing ${field}`)
}

/** @param {{apiKey?: string, fetchImpl?: typeof fetch, endpoint?: string, issueId: string}} options
 * @param {string} query @param {string|null} [after] */
async function readIssue(options, query, after = null) {
  requireString(options.issueId, 'issue id')
  if (!options.apiKey) {
    throw Object.assign(new Error('LINEAR_API_KEY is not set'), {
      code: TRACKER_CREDENTIALS_MISSING,
    })
  }
  const data = await linearRequest({
    ...options,
    apiKey: options.apiKey,
    query,
    variables: { id: options.issueId.trim(), after },
  })
  const issue = data?.issue
  requireString(issue?.id, 'issue id')
  requireString(issue?.identifier, 'issue identifier')
  return issue
}

/** @param {{apiKey?: string, fetchImpl?: typeof fetch, endpoint?: string, issueId: string}} options */
export async function linearReadClaimComments(options) {
  const comments = []
  let after = null
  for (let page = 1; page <= COMMENT_PAGE_CAP; page++) {
    const issue = await readIssue(options, COMMENTS_QUERY, after)
    const connection = issue.comments
    if (
      !Array.isArray(connection?.nodes) ||
      typeof connection?.pageInfo?.hasNextPage !== 'boolean'
    ) {
      throw new Error('tracker/linear: missing comments or pageInfo')
    }
    for (const node of connection.nodes) {
      requireString(node?.id, 'comment id')
      requireString(node?.createdAt, 'comment createdAt')
      if (typeof node?.body !== 'string') throw new Error('tracker/linear: missing comment body')
      comments.push({ id: node.id, body: node.body, createdAt: node.createdAt })
    }
    if (!connection.pageInfo.hasNextPage) {
      comments.sort((a, b) => a.createdAt.localeCompare(b.createdAt))
      return { id: issue.id, identifier: issue.identifier, comments }
    }
    if (page === COMMENT_PAGE_CAP)
      throw new Error(`tracker/linear: comments exceed ${COMMENT_PAGE_CAP} page cap`)
    requireString(connection.pageInfo.endCursor, 'comments endCursor')
    after = connection.pageInfo.endCursor
  }
  throw new Error('tracker/linear: comment pagination incomplete')
}

/** @param {{apiKey?: string, fetchImpl?: typeof fetch, endpoint?: string, issueId: string}} options */
export async function linearReadIssueState(options) {
  const issue = await readIssue(options, STATE_QUERY)
  requireString(issue.state?.name, 'state name')
  requireString(issue.state?.type, 'state type')
  return {
    id: issue.id,
    identifier: issue.identifier,
    state: { name: issue.state.name, type: issue.state.type },
  }
}
