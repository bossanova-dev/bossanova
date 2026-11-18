// Linear's executable `applyIssueWrites` capability, kept apart from linear.mjs (which re-exports it).
import { linearRequest } from '../linear-gate-lib.mjs'
import { TRACKER_CREDENTIALS_MISSING } from './adapter-core.mjs'
import { TRACKER_VERDICTS, classifyTrackerOutcome } from './outcome.mjs'

const nonEmpty = (value) => typeof value === 'string' && value.trim() !== ''
const connectionNodes = (connection) =>
  Array.isArray(connection) ? connection : Array.isArray(connection?.nodes) ? connection.nodes : []

// The one read behind `applyIssueWrites`: everything the writes resolve against (the issue's team
// and its states, labels, creator, assignee, project, blockers) plus the recent comment bodies the
// marker idempotency check scans. One request, so resolution never sees two different issues.
export const APPLY_ISSUE_WRITES_READ_QUERY = `
  query ApplyIssueWritesRead($id: String!) {
    issue(id: $id) {
      id
      identifier
      team { id states { nodes { id name } } }
      labels { nodes { id name } }
      creator { id name displayName url } assignee { id } project { id }
      comments(first: 100) { nodes { body } }
      inverseRelations(first: 100) { nodes { type issue { id identifier } } }
    }
  }
`
export const APPLY_ISSUE_WRITES_BLOCKER_QUERY = `
  query ApplyIssueWritesBlocker($id: String!) { issue(id: $id) { id identifier } }
`
export const APPLY_ISSUE_WRITES_LABELS_QUERY = `
  query ApplyIssueWritesLabels($names: [String!]!) {
    issueLabels(first: 100, filter: { name: { in: $names } }) {
      nodes { id name team { id } }
    }
  }
`
const ISSUE_COMMENTS_QUERY = `
  query ApplyIssueWritesComments($id: String!) {
    issue(id: $id) { comments(first: 100) { nodes { body } } }
  }
`
const ADD_LABEL_MUTATION = `mutation ApplyAddLabel($id: String!, $labelId: String!) {
  issueAddLabel(id: $id, labelId: $labelId) { success }
}`
const REMOVE_LABEL_MUTATION = `mutation ApplyRemoveLabel($id: String!, $labelId: String!) {
  issueRemoveLabel(id: $id, labelId: $labelId) { success }
}`
const COMMENT_MUTATION = `mutation ApplyComment($input: CommentCreateInput!) {
  commentCreate(input: $input) { success comment { id url } }
}`
const BLOCKER_MUTATION = `mutation ApplyBlocker($input: IssueRelationCreateInput!) {
  issueRelationCreate(input: $input) { success }
}`
const STATE_MUTATION = `mutation ApplyState($id: String!, $input: IssueUpdateInput!) {
  issueUpdate(id: $id, input: $input) { success }
}`

export const APPLY_ISSUE_WRITES_KEYS = new Set([
  'issueId',
  'addLabels',
  'removeLabels',
  'addBlockedBy',
  'comment',
  'mentionCreator',
  'marker',
  'stateName',
])

// Linear turns a plain profile URL in a markdown comment body into an @mention of that user
// (https://linear.app/developers/graphql — "mentions can be created in Markdown by using the plain
// URL of the resource"). A creator with no readable URL falls back to their display name, which
// names them but does not notify them.
export function linearCreatorMention(creator) {
  if (nonEmpty(creator?.url)) return creator.url.trim()
  for (const key of ['displayName', 'name'])
    if (nonEmpty(creator?.[key])) return creator[key].trim()
  return ''
}

function nameList(value, label) {
  if (value === undefined || value === null) return []
  if (!Array.isArray(value) || !value.every(nonEmpty))
    throw new Error(`tracker/linear applyIssueWrites: ${label} must be an array of non-empty names`)
  return [...new Set(value.map((name) => name.trim()))]
}

/**
 * The executable `applyIssueWrites` capability, and the only direct tracker WRITE path in the
 * toolbox: add blocking relations, add and remove labels, post one comment, and move the issue to a
 * state, in that order. `addBlockedBy` names the issues that block this one; an edge that already
 * exists is skipped, so the writes are append-only and idempotent.
 *
 * Every name is resolved before anything is written (blockers by id or identifier, labels by name, preferring the issue's team
 * label over a workspace label; the state by name within the issue's team), so an unknown name fails
 * with nothing applied. Each mutation is sent through `linearRequest({operation: 'write'})`, so a
 * timed-out or dropped mutation classifies `indeterminate` and is never re-sent. With a `marker`,
 * the comment is idempotent: an existing comment carrying the marker is not posted again, and an
 * indeterminate comment write is settled by reading the comments back for the marker.
 *
 * Returns `{ok, applied, outcome, reason?, issue?, comment?}` where `outcome` is `ok`, `failed`
 * or `indeterminate`, `applied` lists, in order, every write known to have landed, and `issue` is
 * `{id, identifier, labels}` as read before any write. Called with no writes it is a pure read of
 * those labels, which is how a caller probes that the write path is usable. Throws only
 * before the network: on malformed input, or (with `code: TRACKER_CREDENTIALS_MISSING`) on an
 * unset key.
 *
 * @param {{
 *   apiKey?: string,
 *   fetchImpl?: typeof fetch,
 *   endpoint?: string,
 *   sleep?: (ms: number) => Promise<void>,
 *   issueId?: string,
 *   addLabels?: string[] | null,
 *   removeLabels?: string[] | null,
 *   addBlockedBy?: string[] | null,
 *   comment?: string | null,
 *   mentionCreator?: boolean,
 *   marker?: string,
 *   stateName?: string,
 * }} [writes]
 */
export async function linearApplyIssueWrites({
  apiKey,
  fetchImpl,
  endpoint,
  sleep,
  issueId,
  addLabels,
  removeLabels,
  addBlockedBy,
  comment,
  mentionCreator = false,
  marker,
  stateName,
} = {}) {
  if (!nonEmpty(issueId))
    throw new Error('tracker/linear applyIssueWrites: a non-empty issue id is required')
  const add = nameList(addLabels, 'addLabels')
  const remove = nameList(removeLabels, 'removeLabels')
  const blockers = nameList(addBlockedBy, 'addBlockedBy')
  if (comment !== undefined && comment !== null && !nonEmpty(comment))
    throw new Error('tracker/linear applyIssueWrites: comment must be a non-empty string')
  if (marker !== undefined && marker !== null && !nonEmpty(marker))
    throw new Error('tracker/linear applyIssueWrites: marker must be a non-empty string')
  if (stateName !== undefined && stateName !== null && !nonEmpty(stateName))
    throw new Error('tracker/linear applyIssueWrites: stateName must be a non-empty string')
  if (add.some((name) => remove.includes(name)))
    throw new Error('tracker/linear applyIssueWrites: a label cannot be both added and removed')
  if (!apiKey)
    throw Object.assign(new Error('LINEAR_API_KEY is not set'), {
      code: TRACKER_CREDENTIALS_MISSING,
    })
  const transport = { apiKey, fetchImpl, endpoint, ...(sleep ? { sleep } : {}) }
  const applied = []
  const done = (outcome, extra = {}) => ({
    ok: outcome === TRACKER_VERDICTS.OK,
    applied,
    outcome,
    ...extra,
  })
  const failed = (reason, extra) => done('failed', { reason, ...extra })

  let issue
  try {
    issue = (
      await linearRequest({
        ...transport,
        query: APPLY_ISSUE_WRITES_READ_QUERY,
        variables: { id: issueId.trim() },
      })
    )?.issue
  } catch (error) {
    return failed(`issue-read-failed: ${error.message}`)
  }
  if (!nonEmpty(issue?.id) || !nonEmpty(issue?.team?.id)) return failed('issue-unreadable')

  const current = new Map(
    connectionNodes(issue.labels)
      .filter((label) => nonEmpty(label?.id) && nonEmpty(label?.name))
      .map((label) => [label.name, label.id]),
  )
  // The issue as read, before any write. With no writes requested this function is that read.
  const summary = { id: issue.id, identifier: issue.identifier ?? '', labels: [...current.keys()] }
  for (const key of ['assignee', 'creator', 'project']) summary[`${key}Id`] = issue[key]?.id ?? null
  const toAdd = add.filter((name) => !current.has(name))
  const addIds = new Map()
  if (toAdd.length > 0) {
    let nodes
    try {
      nodes = connectionNodes(
        (
          await linearRequest({
            ...transport,
            query: APPLY_ISSUE_WRITES_LABELS_QUERY,
            variables: { names: toAdd },
          })
        )?.issueLabels,
      )
    } catch (error) {
      return failed(`label-read-failed: ${error.message}`, { issue: summary })
    }
    for (const name of toAdd) {
      const matches = nodes.filter((node) => node?.name === name && nonEmpty(node?.id))
      const pick =
        matches.find((node) => node.team?.id === issue.team.id) ??
        matches.find((node) => !node.team)
      if (!pick) return failed(`label-not-found: ${name}`, { issue: summary, permanent: true })
      addIds.set(name, pick.id)
    }
  }
  let stateId = null
  if (nonEmpty(stateName)) {
    const state = connectionNodes(issue.team.states).find((node) => node?.name === stateName.trim())
    if (!nonEmpty(state?.id))
      return failed(`state-not-found: ${stateName}`, { issue: summary, permanent: true })
    stateId = state.id
  }
  const existingBlockers = new Set(
    connectionNodes(issue.inverseRelations)
      .filter((relation) => relation?.type === 'blocks')
      .flatMap((relation) => [relation.issue?.id, relation.issue?.identifier?.toUpperCase()]),
  )
  const blockerIds = []
  for (const ref of blockers.filter((ref) => !existingBlockers.has(ref.toUpperCase()))) {
    let blocker
    try {
      blocker = (
        await linearRequest({
          ...transport,
          query: APPLY_ISSUE_WRITES_BLOCKER_QUERY,
          variables: { id: ref },
        })
      )?.issue
    } catch (error) {
      return failed(`blocker-read-failed: ${ref}: ${error.message}`, { issue: summary })
    }
    if (!nonEmpty(blocker?.id))
      return failed(`blocker-not-found: ${ref}`, { issue: summary, permanent: true })
    if (blocker.id === issue.id)
      return failed(`blocker-is-self: ${ref}`, { issue: summary, permanent: true })
    if (!existingBlockers.has(blocker.id)) blockerIds.push([blocker.id, blocker.identifier ?? ref])
  }
  let body = null
  if (nonEmpty(comment)) {
    const mention = mentionCreator === true ? linearCreatorMention(issue.creator) : ''
    body = mention ? `${mention}\n\n${comment}` : comment
    if (marker !== undefined && marker !== null && !body.includes(marker))
      body = `${body}\n\n${marker}`
  }
  const hasMarker = (nodes) =>
    nonEmpty(marker) &&
    connectionNodes(nodes).some(
      (node) => typeof node?.body === 'string' && node.body.includes(marker),
    )

  const write = async (kind, query, variables, field, detail) => {
    try {
      const data = await linearRequest({ ...transport, query, variables, operation: 'write' })
      if (data?.[field]?.success !== true) return { outcome: 'failed', reason: `${kind}-rejected` }
      applied.push({ kind, ...detail })
      return { outcome: TRACKER_VERDICTS.OK, data: data[field] }
    } catch (error) {
      const verdict = classifyTrackerOutcome(error, { operation: 'write' }).verdict
      return {
        outcome: verdict === TRACKER_VERDICTS.INDETERMINATE ? 'indeterminate' : 'failed',
        reason: `${kind}: ${error.message}`,
      }
    }
  }

  for (const [blockerId, identifier] of blockerIds) {
    const r = await write(
      'addBlockedBy',
      BLOCKER_MUTATION,
      { input: { issueId: blockerId, relatedIssueId: issue.id, type: 'blocks' } },
      'issueRelationCreate',
      { identifier },
    )
    if (r.outcome !== TRACKER_VERDICTS.OK)
      return done(r.outcome, { reason: r.reason, issue: summary })
  }
  for (const name of toAdd) {
    const r = await write(
      'addLabel',
      ADD_LABEL_MUTATION,
      { id: issue.id, labelId: addIds.get(name) },
      'issueAddLabel',
      { name },
    )
    if (r.outcome !== TRACKER_VERDICTS.OK)
      return done(r.outcome, { reason: r.reason, issue: summary })
  }
  for (const name of remove.filter((n) => current.has(n))) {
    const r = await write(
      'removeLabel',
      REMOVE_LABEL_MUTATION,
      { id: issue.id, labelId: current.get(name) },
      'issueRemoveLabel',
      { name },
    )
    if (r.outcome !== TRACKER_VERDICTS.OK)
      return done(r.outcome, { reason: r.reason, issue: summary })
  }
  let posted = null
  if (body !== null) {
    if (hasMarker(issue.comments)) {
      applied.push({ kind: 'comment', existing: true })
    } else {
      const r = await write(
        'comment',
        COMMENT_MUTATION,
        { input: { issueId: issue.id, body } },
        'commentCreate',
        {},
      )
      if (r.outcome === 'indeterminate' && nonEmpty(marker)) {
        // Settle the indeterminate write by reading the comments back, never by re-sending it.
        let landed = null
        try {
          const data = await linearRequest({
            ...transport,
            query: ISSUE_COMMENTS_QUERY,
            variables: { id: issue.id },
          })
          landed = hasMarker(data?.issue?.comments)
        } catch {
          landed = null
        }
        if (landed !== true) return done('indeterminate', { reason: r.reason, issue: summary })
        applied.push({ kind: 'comment', verifiedByMarker: true })
      } else if (r.outcome !== TRACKER_VERDICTS.OK) {
        return done(r.outcome, { reason: r.reason, issue: summary })
      } else {
        posted = r.data?.comment ?? null
      }
    }
  }
  if (stateId !== null) {
    const r = await write(
      'state',
      STATE_MUTATION,
      { id: issue.id, input: { stateId } },
      'issueUpdate',
      { name: stateName.trim() },
    )
    if (r.outcome !== TRACKER_VERDICTS.OK)
      return done(r.outcome, { reason: r.reason, issue: summary })
  }
  return done(TRACKER_VERDICTS.OK, { issue: summary, ...(posted ? { comment: posted } : {}) })
}
