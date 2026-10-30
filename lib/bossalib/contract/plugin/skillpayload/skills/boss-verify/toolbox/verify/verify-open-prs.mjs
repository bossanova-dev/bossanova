// Batched discovery data shared by candidate selection.
import { VERIFY_CONTEXT } from './verify-vocabulary.mjs'
const plainObject = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)
const isSha = (value) => typeof value === 'string' && /^[0-9a-f]{40}$/i.test(value)
const text = (value) => typeof value === 'string' && value.trim() !== ''

// One open-PR page is the whole universe a sweep considers; more open PRs than this is a repo the
// verify stage does not scale to yet, and the oldest-first walk below still makes progress.
export const CANDIDATE_PR_LIST_LIMIT = 200
const PR_LIST_FIELDS = 'number,url,title,body,headRefOid,headRefName,isDraft,baseRefName'

// The batched discovery read: every open PR's list row, its head's latest `boss/verify` status and
// its CI rollup, one GraphQL page per OPEN_PRS_PAGE_SIZE PRs, newest first like `gh pr list`. It
// replaces the PR list plus a statuses read and a rollup read per head.
export const OPEN_PRS_PAGE_SIZE = 100
const OPEN_PRS_MARKER = 'pullRequests(states: OPEN'

export const OPEN_PRS_QUERY = `query($owner: String!, $name: String!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    ${OPEN_PRS_MARKER}, first: ${OPEN_PRS_PAGE_SIZE}, after: $cursor, orderBy: {field: CREATED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        ${PR_LIST_FIELDS.split(',').join(' ')} mergeStateStatus updatedAt reviewDecision
        isCrossRepository author { login __typename } labels(first: 20) { nodes { name } }
        baseRef { target { oid } }
        commits(last: 1) { nodes { commit {
          oid
          status { context(name: "${VERIFY_CONTEXT}") { state description createdAt } }
          statusCheckRollup { contexts(first: 100) {
            pageInfo { hasNextPage }
            nodes {
              __typename
              ... on CheckRun { name status conclusion startedAt completedAt detailsUrl checkSuite { workflowRun { workflow { name } } } }
              ... on StatusContext { context state targetUrl createdAt }
            }
          } }
        } } }
      }
    }
  }
}`

const openPrsReadCall = (repo, cursor) => {
  const [owner, name] = repo.split('/')
  return [
    'gh',
    [
      'api',
      'graphql',
      '-f',
      `query=${OPEN_PRS_QUERY}`,
      '-f',
      `owner=${owner}`,
      '-f',
      `name=${name}`,
      ...(cursor ? ['-f', `cursor=${cursor}`] : []),
    ],
    { json: true },
  ]
}

// The latest `boss/verify` status in classifyHeadStatus's shape: null when the head has none,
// undefined when the node is malformed (unknown, never "none").
function batchedStatus(status) {
  if (status === null || status === undefined) return null
  if (!plainObject(status)) return undefined
  const node = status.context
  if (node === null || node === undefined) return null
  if (!plainObject(node) || typeof node.state !== 'string') return undefined
  if (
    node.description !== null &&
    node.description !== undefined &&
    typeof node.description !== 'string'
  )
    return undefined
  if (typeof node.createdAt !== 'string') return undefined
  return {
    state: node.state.toLowerCase(),
    description: node.description ?? '',
    updatedAt: node.createdAt,
  }
}

// One rollup context in `gh pr view --json statusCheckRollup` form, or undefined when malformed.
function batchedContext(node) {
  if (!plainObject(node)) return undefined
  if (node.__typename === 'CheckRun')
    return {
      __typename: 'CheckRun',
      name: node.name ?? '',
      status: node.status ?? '',
      conclusion: node.conclusion ?? '',
      startedAt: node.startedAt ?? '',
      completedAt: node.completedAt ?? '',
      detailsUrl: node.detailsUrl ?? '',
      workflowName: node.checkSuite?.workflowRun?.workflow?.name ?? '',
    }
  if (node.__typename === 'StatusContext')
    return {
      __typename: 'StatusContext',
      context: node.context ?? '',
      state: node.state ?? '',
      targetUrl: node.targetUrl ?? '',
      startedAt: node.createdAt ?? '',
    }
  return undefined
}

// `{rollup}` in `gh pr view` form, `{gap: true}` when the contexts run past one page, or `{}` when
// the node is malformed.
function batchedRollup(rollup) {
  if (rollup === null || rollup === undefined) return { rollup: [] }
  const connection = plainObject(rollup) ? rollup.contexts : undefined
  if (!plainObject(connection) || !Array.isArray(connection.nodes)) return {}
  const more = connection.pageInfo?.hasNextPage
  if (more === true) return { gap: true }
  if (more !== false) return {}
  const contexts = connection.nodes.map(batchedContext)
  return contexts.includes(undefined) ? {} : { rollup: contexts }
}

/**
 * Maps one page of OPEN_PRS_QUERY to `{rows, hasNextPage, endCursor}`, or throws when the page as a
 * whole is unreadable (no data, a malformed connection, a null PR, or an error outside one PR). A
 * row is `{pr, mergeStateStatus, updatedAt, baseTip, gap, status, view}`: `pr` carries the
 * PR_LIST_FIELDS keys plus `isCrossRepository`, `author` and `labels` (names); `status` is
 * classifyHeadStatus's input (null for none, undefined when malformed); `view` is `{headRefOid,
 * statusCheckRollup}` in `gh pr view` form (null when malformed). `gap` names why the batch cannot
 * stand for this PR's per-head reads — `error` (a GraphQL error under it), `head` (its last commit
 * is not its head) or `contexts` (more rollup contexts than one page) — and is null otherwise.
 */
export function mapOpenPrs(payload) {
  const fail = (why) => {
    throw new Error(`verify-gate candidates: the open-PR read is unreadable (${why})`)
  }
  if (!plainObject(payload)) fail('not an object')
  const errored = new Set()
  for (const error of Array.isArray(payload.errors)
    ? payload.errors
    : payload.errors
      ? [null]
      : []) {
    const path = Array.isArray(error?.path) ? error.path : []
    const under =
      path[0] === 'repository' &&
      path[1] === 'pullRequests' &&
      path[2] === 'nodes' &&
      Number.isInteger(path[3])
    if (!under) fail(String(error?.message ?? 'graphql error').slice(0, 200))
    errored.add(path[3])
  }
  const connection = payload.data?.repository?.pullRequests
  if (!plainObject(connection) || !Array.isArray(connection.nodes)) fail('no pull requests')
  const hasNextPage = connection.pageInfo?.hasNextPage
  if (typeof hasNextPage !== 'boolean') fail('no page info')
  const rows = connection.nodes.map((node, i) => {
    if (!plainObject(node) || !Number.isInteger(node.number) || node.number < 1)
      fail(`pull request ${i} unreadable`)
    const pr = Object.fromEntries(PR_LIST_FIELDS.split(',').map((key) => [key, node[key]]))
    pr.isCrossRepository = node.isCrossRepository === true
    pr.author = plainObject(node.author) ? node.author : null
    pr.labels = (node.labels?.nodes ?? []).map((label) => label?.name).filter(text)
    const commit = node.commits?.nodes?.[0]?.commit
    const head = String(node.headRefOid ?? '').toLowerCase()
    const rollup = batchedRollup(commit?.statusCheckRollup)
    const gap = errored.has(i)
      ? 'error'
      : !isSha(commit?.oid) || commit.oid.toLowerCase() !== head
        ? 'head'
        : rollup.gap
          ? 'contexts'
          : null
    return {
      pr,
      reviewDecision: ['APPROVED', 'CHANGES_REQUESTED', 'REVIEW_REQUIRED'].includes(
        node.reviewDecision,
      )
        ? node.reviewDecision
        : '',
      mergeStateStatus: node.mergeStateStatus ?? '',
      updatedAt: node.updatedAt ?? '',
      baseTip: node.baseRef?.target?.oid ?? '',
      gap,
      status: gap ? undefined : batchedStatus(commit.status),
      view:
        gap || !rollup.rollup
          ? null
          : { headRefOid: node.headRefOid, statusCheckRollup: rollup.rollup },
    }
  })
  return { rows, hasNextPage, endCursor: connection.pageInfo.endCursor ?? '' }
}

/**
 * Every open PR, newest first, through OPEN_PRS_QUERY pages up to CANDIDATE_PR_LIST_LIMIT:
 * `{rows, truncated}` (mapOpenPrs rows; `truncated` when more open PRs stand beyond the cap). A
 * failed or unreadable page throws, as a failed `gh pr list` did.
 */
export function readOpenPrs(read, repo) {
  const rows = []
  let cursor = ''
  for (;;) {
    const r = read(...openPrsReadCall(repo, cursor))
    if (!r.ok)
      throw new Error(
        `verify-gate candidates: the open-PR read failed (${r.error ?? 'unreadable'})`,
      )
    const page = mapOpenPrs(r.payload)
    rows.push(...page.rows)
    if (!page.hasNextPage) return { rows, truncated: false }
    if (rows.length >= CANDIDATE_PR_LIST_LIMIT)
      return { rows: rows.slice(0, CANDIDATE_PR_LIST_LIMIT), truncated: true }
    if (!text(page.endCursor))
      throw new Error('verify-gate candidates: the open-PR read is unreadable (no end cursor)')
    cursor = page.endCursor
  }
}

// Only a timed-out scan uses its last phase as the failure reason.
export function candidateFailureMessage(found, detail) {
  const phase =
    found.error === 'ETIMEDOUT' && detail.match(/^verify-gate candidates: phase (.+)$/)?.[1]
  return `candidates failed (${found.error ?? 'unreadable'})${phase ? ` in phase ${phase}` : found.output ? `: ${found.output.split('\n').pop()}` : detail ? `: ${detail}` : ''}`
}
