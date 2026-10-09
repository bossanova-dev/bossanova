// skills-toolbox/tracker/linear.mjs
// Linear reference implementation of the tracker-adapter interface. Preserves
// current behaviour exactly by delegating to the co-located linear-*.mjs
// helpers — it reimplements nothing. node builtins only.
//
// Executable methods cover the capabilities the skills already own in code
// (gates, dependency reading, claim resolution, normalization). The agent-driven
// capabilities (select/list, move state, comments, labels, priority/estimate,
// dependency-edge writes) are captured declaratively in buildLinearOperationMap: the
// skills perform these through the Linear MCP tools today, so 1a records the
// tool + shape rather than reimplementing them as GraphQL mutations (see the
// plan's Open Questions).

import { linearRequest, resolveLinearSelectionRefs, runLinearGate } from '../linear-gate-lib.mjs'
import {
  applyResolution,
  assertEffectiveSelection,
  renderLinearIssueFilter,
  selectionResolutionRefs,
} from '../selection.mjs'
import { runUnblockedGate, extractBlockers, isUnblocked } from '../linear-deps-lib.mjs'
import { claimWinner, formatClaimComment, parseClaimComments } from '../linear-claim.mjs'
import { normalizeTicket } from '../bs-epic-lib.mjs'
import { loadSkillConfig, trackerConfigFor } from '../skill-config.mjs'
// From adapter-core.mjs, not adapter.mjs: adapter.mjs imports THIS module to build
// its registry, so reading the roles from there would make the pair circular.
import { TRACKER_CREDENTIALS_MISSING, TRACKER_STATE_ROLES } from './adapter-core.mjs'
import { TRACKER_VERDICTS, classifyTrackerOutcome } from './outcome.mjs'

export const MARKED_ISSUES_QUERY = `query Marked($filter: IssueFilter!, $after: String) {
  issues(first: 250, filter: $filter, after: $after, includeArchived: true) {
    nodes { identifier title description createdAt completedAt canceledAt state { type } }
    pageInfo { hasNextPage endCursor }
  }
}`

/**
 * Fetch the complete marker snapshot, or with `updatedAfter` only the issues
 * updated since then; refuse partial pagination results either way.
 */
export async function linearSelectMarked({
  apiKey,
  fetchImpl,
  endpoint,
  maxPages = 20,
  markerPrefix,
  updatedAfter,
} = {}) {
  if (!apiKey)
    throw Object.assign(new Error('LINEAR_API_KEY is not set'), {
      code: TRACKER_CREDENTIALS_MISSING,
    })
  if (typeof markerPrefix !== 'string' || markerPrefix.trim() === '')
    throw new Error('linearSelectMarked markerPrefix must be non-empty')
  if (!Number.isInteger(maxPages) || maxPages < 1)
    throw new Error('linearSelectMarked maxPages must be a positive integer')
  if (
    updatedAfter !== undefined &&
    updatedAfter !== null &&
    (typeof updatedAfter !== 'string' || !Number.isFinite(Date.parse(updatedAfter)))
  )
    throw new Error(`linearSelectMarked updatedAfter is not a timestamp: ${updatedAfter}`)
  // With `updatedAfter`, only issues changed since the baseline snapshot are
  // fetched; the caller scans the union of both, so the delta cannot lose one.
  const filter = {
    description: { contains: markerPrefix },
    ...(updatedAfter ? { updatedAt: { gt: updatedAfter } } : {}),
  }
  const nodes = []
  let after = null
  for (let page = 0; page < maxPages; page++) {
    const data = await linearRequest({
      apiKey,
      fetchImpl,
      endpoint,
      query: MARKED_ISSUES_QUERY,
      variables: { filter, after },
    })
    const connection = data?.issues
    if (!connection) throw new Error('Linear marker scan returned no issues connection')
    if (!Array.isArray(connection.nodes)) {
      throw new Error('Linear marker scan returned malformed issue nodes')
    }
    if (
      !connection.nodes.every(
        (node) =>
          node !== null &&
          typeof node === 'object' &&
          !Array.isArray(node) &&
          typeof node.identifier === 'string' &&
          node.identifier.length > 0 &&
          typeof node.description === 'string' &&
          typeof node.title === 'string' &&
          typeof node.state?.type === 'string' &&
          node.state.type.trim().length > 0 &&
          ['createdAt', 'completedAt', 'canceledAt'].every(
            (field) => node[field] === null || typeof node[field] === 'string',
          ),
      )
    ) {
      throw new Error('Linear marker scan returned malformed issue entry')
    }
    if (typeof connection.pageInfo?.hasNextPage !== 'boolean') {
      throw new Error('Linear marker scan returned malformed page info')
    }
    nodes.push(
      ...connection.nodes.map((node) => ({
        identifier: node.identifier,
        title: node.title,
        description: node.description,
        createdAt: node.createdAt,
        resolution:
          node.state.type === 'completed'
            ? 'done'
            : node.state.type === 'canceled'
              ? 'canceled'
              : null,
        resolvedAt: node.completedAt ?? node.canceledAt ?? null,
      })),
    )
    if (!connection.pageInfo.hasNextPage) return nodes
    if (
      typeof connection.pageInfo.endCursor !== 'string' ||
      connection.pageInfo.endCursor.length === 0
    ) {
      throw new Error('Linear marker scan returned no continuation cursor')
    }
    after = connection.pageInfo.endCursor
  }
  throw new Error(`Linear marker scan exceeded ${maxPages} pages — dedupe snapshot incomplete`)
}

// The candidate read behind the OPTIONAL executable `selectPlanned` capability. It selects exactly
// the fields Step 2's eligibility walk and ranking read — identity, title, priority, estimate,
// creation time, state, label names and the attachment records the canonical-plan selector picks
// from — and nothing else, because the selection set is paid on every sweep. The window is the
// caller's `$first` (default 250, the descriptor path's limit), not a paginated full scan.
export const LIST_PLANNED_QUERY = `
  query ListPlanned($first: Int!, $filter: IssueFilter!) {
    issues(first: $first, filter: $filter) {
      nodes {
        identifier
        title
        priority
        estimate
        createdAt
        state { name type }
        labels { nodes { name } }
        attachments { nodes { id title url createdAt } }
      }
    }
  }
`

const SELECT_PLANNED_MAX_LIMIT = 250
// The closed set of query keys `selectPlanned` knows how to put on the wire — the
// `stageSelectionQuery` shape plus `limit` and `team`. `team` scopes the read to one team when the
// config names none (a zero-config run's resolved team); a configured team always wins over it.
// An unknown key throws, the removed `label` / `assigneeOrCreator` included.
const SELECT_PLANNED_KEYS = new Set([
  'state',
  'selection',
  'requireLabels',
  'excludeLabels',
  'limit',
  'team',
])
// The selection-path query keys the two gates accept (`stageSelectionQuery`'s shape, plus an
// optional team). Presence of any of the first three routes a gate call down the selection path.
const SELECTION_GATE_KEYS = new Set([
  'state',
  'selection',
  'requireLabels',
  'excludeLabels',
  'team',
])
const SELECTION_MARKER_KEYS = ['selection', 'requireLabels', 'excludeLabels']
const LEGACY_GATE_KEYS = ['label', 'assignee', 'creator', 'assigneeOrCreator']

const isSelectionQuery = (query) =>
  SELECTION_MARKER_KEYS.some((key) => Object.hasOwn(query ?? {}, key))

function labelList(caller, name, value) {
  if (value === undefined || value === null) return []
  if (!Array.isArray(value) || !value.every((v) => typeof v === 'string' && v.trim() !== '')) {
    throw new Error(`${caller}: ${name} must be an array of non-empty label names`)
  }
  return value
}

/**
 * Validate a selection-path query: no legacy key beside it, no unknown key, well-formed
 * stage-label lists. Returns the normalized pieces. Throws before any request.
 */
function checkSelectionQuery(caller, query, allowed) {
  const legacy = LEGACY_GATE_KEYS.filter((key) => Object.hasOwn(query, key))
  if (legacy.length > 0) {
    throw new Error(
      `${caller}: legacy key(s) ${legacy.join(', ')} cannot be combined with a selection query; use stageSelectionQuery`,
    )
  }
  const unknown = Object.keys(query).filter((key) => !allowed.has(key))
  if (unknown.length > 0) {
    throw new Error(
      `${caller}: unknown selection key(s) ${unknown.map((key) => JSON.stringify(key)).join(', ')}; refusing to drop a clause the caller asked for`,
    )
  }
  return {
    selection: assertEffectiveSelection(query.selection, caller),
    requireLabels: labelList(caller, 'requireLabels', query.requireLabels),
    excludeLabels: labelList(caller, 'excludeLabels', query.excludeLabels),
  }
}

/**
 * Resolve an effective selection's users, labels and projects against Linear (one batched request,
 * or none when nothing is named) and return the resolved selection. Fails closed on any ref that
 * does not resolve.
 */
export async function linearResolveSelection({ apiKey, fetchImpl, endpoint, selection }) {
  const effective = assertEffectiveSelection(selection, 'tracker/linear resolveSelection')
  const resolved = await resolveLinearSelectionRefs({
    apiKey,
    refs: selectionResolutionRefs(effective),
    fetchImpl,
    endpoint,
  })
  return applyResolution(effective, resolved)
}

// The two gates' selection path: resolve, render, then hand the rendered filter to the gate runner.
async function selectionGateFilter(caller, query, { apiKey, fetchImpl, endpoint }) {
  const { selection, requireLabels, excludeLabels } = checkSelectionQuery(
    caller,
    query,
    SELECTION_GATE_KEYS,
  )
  if (typeof query.state !== 'string' || query.state.trim() === '') {
    throw new Error(`${caller}: a non-empty state is required; refusing to scan every state`)
  }
  const resolved = await linearResolveSelection({ apiKey, fetchImpl, endpoint, selection })
  return renderLinearIssueFilter(resolved, {
    state: query.state,
    team: query.team,
    requireLabels,
    excludeLabels,
  })
}
const SELECT_CANDIDATES_KEYS = new Set(['states', 'ids', 'limit', 'team'])

const CANDIDATE_FIELDS = `id identifier title priority createdAt description
  state { name type } parent { identifier } labels { nodes { name } }`

export const LIST_CANDIDATES_QUERY = `
  query ListCandidates($first: Int!, $filter: IssueFilter!, $after: String) {
    issues(first: $first, filter: $filter, after: $after) {
      nodes { ${CANDIDATE_FIELDS} }
      pageInfo { hasNextPage endCursor }
    }
  }
`
const READ_CANDIDATE_QUERY = `query ReadCandidate($id: String!) {
  issue(id: $id) { ${CANDIDATE_FIELDS} }
}`

const nonEmpty = (value) => typeof value === 'string' && value.trim() !== ''

function flattenCandidate(node, source) {
  if (
    !node ||
    !['id', 'identifier', 'title'].every((key) => nonEmpty(node[key])) ||
    !nonEmpty(node.state?.name) ||
    !nonEmpty(node.state?.type) ||
    (node.description !== null && typeof node.description !== 'string') ||
    (node.parent !== null && !nonEmpty(node.parent?.identifier)) ||
    !Array.isArray(node.labels?.nodes) ||
    !node.labels.nodes.every((entry) => entry && nonEmpty(entry.name))
  ) {
    throw new Error('tracker/linear selectCandidates: unreadable candidate fields')
  }
  return {
    id: node.id,
    identifier: node.identifier,
    title: node.title,
    priority: node.priority,
    createdAt: node.createdAt,
    description: node.description ?? '',
    stateName: node.state.name,
    stateType: node.state.type,
    parentId: node.parent?.identifier ?? null,
    labels: node.labels.nodes.map((entry) => entry.name),
    source,
  }
}

export async function linearSelectCandidates({
  apiKey,
  fetchImpl,
  endpoint,
  team,
  states,
  ids = [],
  limit = 250,
}) {
  if (!Array.isArray(states) || states.length === 0 || !states.every(nonEmpty)) {
    throw new Error('tracker/linear selectCandidates: non-empty states required')
  }
  if (!nonEmpty(team))
    throw new Error(
      'tracker/linear selectCandidates: a team is required (trackerConfig.linear.team or --team)',
    )
  if (!Number.isInteger(limit) || limit < 1 || limit > 250)
    throw new Error('tracker/linear selectCandidates: limit must be 1-250')
  if (!Array.isArray(ids) || !ids.every(nonEmpty))
    throw new Error('tracker/linear selectCandidates: ids must be non-empty strings')
  if (!apiKey)
    throw Object.assign(new Error('LINEAR_API_KEY is not set'), {
      code: TRACKER_CREDENTIALS_MISSING,
    })
  const filter = { team: { name: { eq: team } }, state: { name: { in: states } } }
  const records = new Map()
  const cursors = new Set()
  let after = null
  do {
    const data = await linearRequest({
      apiKey,
      fetchImpl,
      endpoint,
      query: LIST_CANDIDATES_QUERY,
      variables: { first: limit, filter, after },
    })
    const { nodes, pageInfo } = data?.issues ?? {}
    if (!Array.isArray(nodes) || typeof pageInfo?.hasNextPage !== 'boolean') {
      throw new Error('tracker/linear selectCandidates: unreadable issues page')
    }
    for (const node of nodes) records.set(node.id, flattenCandidate(node, 'state'))
    if (!pageInfo.hasNextPage) break
    if (!nonEmpty(pageInfo.endCursor) || cursors.has(pageInfo.endCursor)) {
      throw new Error('tracker/linear selectCandidates: pagination did not advance')
    }
    after = pageInfo.endCursor
    cursors.add(after)
  } while (true)
  for (const id of new Set(ids.map((value) => value.trim()))) {
    const data = await linearRequest({
      apiKey,
      fetchImpl,
      endpoint,
      query: READ_CANDIDATE_QUERY,
      variables: { id },
    })
    const record = flattenCandidate(data?.issue, 'id')
    records.set(record.id, record)
  }
  return [...records.values()]
}

// A GraphQL connection read as a plain array: `{nodes: [...]}` -> `[...]`. An already-flat array
// passes through, and anything else is an empty list rather than a throw, matching the shared
// ticket normalizer's own tolerance for these two fields.
function connectionNodes(connection) {
  if (Array.isArray(connection)) return connection
  return Array.isArray(connection?.nodes) ? connection.nodes : []
}

/**
 * The executable `selectPlanned` capability: the candidate list for one stage, narrowed by exactly
 * the filter that stage's cron gate applies. The query is `stageSelectionQuery`'s shape; the filter
 * is resolved and rendered by the SAME `renderLinearIssueFilter` the gates use — never a second
 * construction path — with the team ANDed as a sibling key, so the worker's candidate universe can
 * only ever be the gate's, restricted to one team.
 *
 * Fails CLOSED on every input that would otherwise widen the scan: a missing state, a missing team,
 * a malformed selection or stage-label list, an unresolvable user/label/project, an out-of-range
 * limit — each throws, before the list request. A payload whose `issues.nodes` is not an array
 * throws too: an empty list is an answer, and an unreadable payload is not one.
 *
 * Exported for its own tests. The team comes from `trackerConfig.linear.team` or, in a zero-config
 * repo, from the run's resolved team (`query.team` / `--team`); with neither, the guard below
 * refuses before any request rather than scanning every team.
 */
export async function linearSelectPlanned({
  apiKey,
  fetchImpl,
  endpoint,
  team,
  state,
  selection,
  requireLabels,
  excludeLabels,
  limit = SELECT_PLANNED_MAX_LIMIT,
}) {
  if (typeof state !== 'string' || state.trim() === '') {
    throw new Error(
      'tracker/linear selectPlanned: a non-empty state is required; refusing to scan every state',
    )
  }
  if (typeof team !== 'string' || team.trim() === '') {
    throw new Error(
      'tracker/linear selectPlanned: trackerConfig.linear.team is required to scope the candidate list; add it to .boss-skills.json or pass --team <name>',
    )
  }
  const checked = checkSelectionQuery(
    'tracker/linear selectPlanned',
    { selection, requireLabels, excludeLabels },
    SELECT_PLANNED_KEYS,
  )
  if (!Number.isInteger(limit) || limit < 1 || limit > SELECT_PLANNED_MAX_LIMIT) {
    throw new Error(
      `tracker/linear selectPlanned: limit must be an integer from 1 to ${SELECT_PLANNED_MAX_LIMIT}; got ${JSON.stringify(limit)}`,
    )
  }
  const resolved = await linearResolveSelection({
    apiKey,
    fetchImpl,
    endpoint,
    selection: checked.selection,
  })
  const filter = renderLinearIssueFilter(resolved, {
    state,
    team,
    requireLabels: checked.requireLabels,
    excludeLabels: checked.excludeLabels,
  })
  const data = await linearRequest({
    apiKey,
    query: LIST_PLANNED_QUERY,
    variables: { first: limit, filter },
    fetchImpl,
    endpoint,
  })
  const nodes = data?.issues?.nodes
  if (!Array.isArray(nodes)) {
    throw new Error(
      'tracker/linear selectPlanned: the issues payload cannot be evaluated (issues.nodes is not an array), so "no candidates" cannot be distinguished from "no answer"',
    )
  }
  return nodes.map((node) => ({
    ...node,
    labels: connectionNodes(node?.labels)
      .map((entry) => (typeof entry === 'string' ? entry : entry?.name))
      .filter((name) => typeof name === 'string' && name !== ''),
    attachments: connectionNodes(node?.attachments),
  }))
}

// The read behind the OPTIONAL executable `readDescription` capability. `issue(id:)` resolves a UUID
// or a human identifier, so the id is forwarded as given; `id` and `identifier` come back so the
// caller can confirm WHICH issue answered — a key scoped to another workspace can resolve a
// colliding identifier to a different issue, and a UUID cannot collide.
export const READ_DESCRIPTION_QUERY = `
  query ReadDescription($id: String!) {
    issue(id: $id) {
      id
      identifier
      description
    }
  }
`

/**
 * The executable `readDescription` capability: the issue's STORED description, verbatim. The
 * string is returned exactly as the tracker sent it — Linear stores descriptions without a trailing
 * newline, so adding one (or stripping one) would make every byte-compare against it report drift.
 * A `null` description is the tracker's spelling of "empty" and maps to `''`.
 *
 * Fails CLOSED: a blank id throws before any request, an unset key throws before the network with
 * `code: TRACKER_CREDENTIALS_MISSING`, and a missing issue or a payload whose `id`, `identifier` or `description` it
 * cannot read throws rather than answering with a description it did not read.
 */
export async function linearReadDescription({ apiKey, fetchImpl, endpoint, issueId }) {
  if (typeof issueId !== 'string' || issueId.trim() === '') {
    throw new Error('tracker/linear readDescription: a non-empty issue id is required')
  }
  const id = issueId.trim()
  if (!apiKey) {
    throw Object.assign(new Error('LINEAR_API_KEY is not set'), {
      code: TRACKER_CREDENTIALS_MISSING,
    })
  }
  const data = await linearRequest({
    apiKey,
    query: READ_DESCRIPTION_QUERY,
    variables: { id },
    fetchImpl,
    endpoint,
  })
  const issue = data?.issue
  if (!issue || typeof issue !== 'object') {
    throw new Error(`tracker/linear readDescription: no issue found for id ${JSON.stringify(id)}`)
  }
  if (typeof issue.id !== 'string' || issue.id === '') {
    throw new Error('tracker/linear readDescription: the issue payload carries no id')
  }
  if (typeof issue.identifier !== 'string' || issue.identifier === '') {
    throw new Error('tracker/linear readDescription: the issue payload carries no identifier')
  }
  const { description } = issue
  if (description !== null && typeof description !== 'string') {
    throw new Error(
      `tracker/linear readDescription: the stored description is ${
        description === undefined ? 'absent from the payload' : `a ${typeof description}`
      }, not a string, so it cannot be written verbatim`,
    )
  }
  return { id: issue.id, identifier: issue.identifier, description: description ?? '' }
}

// The one read behind `applyIssueWrites`: everything the writes resolve against (the issue's team,
// that team's workflow states, the labels it carries, its creator) plus the recent comment bodies the
// marker idempotency check scans. One request, so resolution never sees two different issues.
export const APPLY_ISSUE_WRITES_READ_QUERY = `
  query ApplyIssueWritesRead($id: String!) {
    issue(id: $id) {
      id
      identifier
      team { id states { nodes { id name } } }
      labels { nodes { id name } }
      creator { id name displayName url }
      comments(first: 100) { nodes { body } }
    }
  }
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
const STATE_MUTATION = `mutation ApplyState($id: String!, $input: IssueUpdateInput!) {
  issueUpdate(id: $id, input: $input) { success }
}`

const APPLY_ISSUE_WRITES_KEYS = new Set([
  'issueId',
  'addLabels',
  'removeLabels',
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
 * toolbox: add and remove labels, post one comment, and move the issue to a state, in that order.
 *
 * Every name is resolved before anything is written (labels by name, preferring the issue's team
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
 */
export async function linearApplyIssueWrites({
  apiKey,
  fetchImpl,
  endpoint,
  sleep,
  issueId,
  addLabels,
  removeLabels,
  comment,
  mentionCreator = false,
  marker,
  stateName,
} = {}) {
  if (!nonEmpty(issueId))
    throw new Error('tracker/linear applyIssueWrites: a non-empty issue id is required')
  const add = nameList(addLabels, 'addLabels')
  const remove = nameList(removeLabels, 'removeLabels')
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
  // The labels as read, before any write. With no writes requested this function is that read.
  const summary = { id: issue.id, identifier: issue.identifier ?? '', labels: [...current.keys()] }

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
      if (!pick) return failed(`label-not-found: ${name}`, { issue: summary })
      addIds.set(name, pick.id)
    }
  }
  let stateId = null
  if (nonEmpty(stateName)) {
    const state = connectionNodes(issue.team.states).find((node) => node?.name === stateName.trim())
    if (!nonEmpty(state?.id)) return failed(`state-not-found: ${stateName}`, { issue: summary })
    stateId = state.id
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

// Declarative map of each agent-driven capability to the Linear MCP tool the
// skills invoke today, with the argument/response shape they rely on. This is
// the single source of truth later extraction tickets (and generalized skill
// prose) consume — change the tracker, change this map.
export function buildLinearOperationMap(mcpServer) {
  // The single positional argument is the MCP SERVER NAME, a bare string. Anything else — an options
  // object, above all, because every other helper in this tree takes one — interpolates into all
  // eighteen template literals below and yields a perfectly well-formed map whose every tool name is
  // `mcp__[object Object]__*`. Nothing here fails, nothing downstream inspects the names, and the
  // misuse surfaces only much later as an unknown-tool error against the live tracker. `createLinearAdapter`
  // already refuses a missing `trackerConfig.linear.mcpServer`; this closes the same hole for every
  // direct caller of the builder.
  if (typeof mcpServer !== 'string' || mcpServer.trim() === '') {
    throw new Error(
      `tracker/linear: buildLinearOperationMap(mcpServer) — expected the MCP server NAME as a non-empty string, got ${
        mcpServer === null ? 'null' : typeof mcpServer
      } ${JSON.stringify(mcpServer)}. A non-string interpolates into every tool name as mcp__[object Object]__*, which fails only at invocation against the tracker.`,
    )
  }
  return {
    selectPlanned: {
      tool: `mcp__${mcpServer}__list_issues`,
      summary:
        'team=<trackerConfig.team> state=<configured planned state> [label] limit=250 -> nodes ranked by priority',
    },
    createIssue: {
      tool: `mcp__${mcpServer}__save_issue`,
      summary:
        '{team, title, description, state, labels, parentId, project} -> created issue identifier (create mode: no id)',
    },
    getIssue: {
      tool: `mcp__${mcpServer}__get_issue`,
      summary: 'id[, includeRelations=true] -> issue with labels + blockedBy relations',
    },
    moveState: {
      tool: `mcp__${mcpServer}__save_issue`,
      summary: '{id, state} -> transition between configured state roles',
    },
    readComments: {
      tool: `mcp__${mcpServer}__list_comments`,
      summary:
        'issueId -> [{id, body, createdAt}] (claim resolution reads these; the id is what ' +
        'updateComment needs to edit a comment in place)',
    },
    writeComment: {
      tool: `mcp__${mcpServer}__save_comment`,
      summary: '{issueId, body} -> posts a comment (claim comment / PR link)',
    },
    updateComment: {
      tool: `mcp__${mcpServer}__save_comment`,
      summary:
        '{id, body} -> updates that existing comment in place (save_comment updates when id is set)',
    },
    readLabels: {
      tool: `mcp__${mcpServer}__get_issue`,
      summary: 'id -> current labels to MERGE with (never overwrite)',
    },
    extractImages: {
      tool: `mcp__${mcpServer}__extract_images`,
      summary:
        '{markdown} -> renders embedded reporter screenshots (image markdown / attachment URLs)',
    },
    createLabel: {
      tool: `mcp__${mcpServer}__create_issue_label`,
      summary: '{name} -> ensure a label exists before applying it',
    },
    setPriorityEstimate: {
      tool: `mcp__${mcpServer}__save_issue`,
      summary: '{id, priority(1-4), estimate(fib)} -> set on plan finalize',
    },
    appendDependency: {
      tool: `mcp__${mcpServer}__save_issue`,
      summary: '{id, blockedBy: [ids]} -> add a dependency edge (cycle-checked by caller)',
    },
    appendRelatedTo: {
      tool: `mcp__${mcpServer}__save_issue`,
      summary: '{id, relatedTo: [ids]} -> add a non-blocking related edge',
    },
    // The argument key `description` is load-bearing, not decoration: tracker/cli.mjs's
    // write-description verb builds its descriptor `args` around it, so a summary that
    // named some other key would emit a save the tracker accepts and that changes nothing.
    writeDescription: {
      tool: `mcp__${mcpServer}__save_issue`,
      summary:
        '{id, description} -> replace the issue description wholesale with bytes read from a ' +
        'file (the already-gated body); the argument is inline-only, so executing it re-emits ' +
        'those bytes and the stored description must be read back to confirm what landed',
    },
    // `size` carries a UNIT, because the unit is the contract: it is the file's BYTE count,
    // measured on the exact file about to be PUT. A character count, or a count taken from the
    // buffer the file was built from, makes the signed upload fail for a reason the rejection
    // never names.
    preparePlanAttachment: {
      tool: `mcp__${mcpServer}__prepare_attachment_upload`,
      // The unit note sits AFTER the closing brace on purpose: `declaredOperationArgKeys` parses
      // the leading `{...}` as the argument list, so a parenthetical inside it becomes a phantom
      // argument no write plan can satisfy.
      summary:
        '{issue, filename, contentType="text/markdown", size} -> signed upload request + ' +
        'assetUrl. `size` is the BYTE count measured on the exact file about to be PUT -- never ' +
        'a character count and never a count taken from the buffer the file was built from.',
    },
    finalizePlanAttachment: {
      tool: `mcp__${mcpServer}__create_attachment_from_upload`,
      summary: '{issue, assetUrl, title} -> issue attachment',
    },
    // The MODE is named, because one of the two returns no content at all: `format="url"` hands
    // back a URL, so a read-back specified against it is unexecutable rather than merely weak.
    readPlanAttachment: {
      tool: `mcp__${mcpServer}__get_attachment`,
      summary:
        '{id, format="content"} -> attached Markdown text; format="url" returns a URL and NO ' +
        "content, and an attachment record's own unsigned url is never a body source",
    },
    deletePlanAttachment: {
      tool: `mcp__${mcpServer}__delete_attachment`,
      summary: '{id} -> delete a stale issue attachment',
    },
    // Optional: zero-config team detection. The MCP result carries no team key and is paginated,
    // so it is handed verbatim to resolveTrackerTeam, which treats hasNextPage as ambiguous.
    listTeams: {
      tool: `mcp__${mcpServer}__list_teams`,
      summary:
        '{limit=50} -> {teams:[{id,name}], hasNextPage}; no team key; pass the result verbatim to resolveTrackerTeam',
    },
  }
}

/**
 * Reference implementation of the OPTIONAL `states` capability: the adapter is the
 * PRIMARY authority for the tracker's workflow-state names, so a repo wired through a
 * vendored adapter need not restate them in `.boss-skills.json`. This reference
 * derives them FROM that same configuration, so the reference path's resolution ends
 * at exactly the values it always did — the capability adds an authority, not a new
 * answer. A vendored adapter for a tracker whose state names it already knows
 * (hard-coded, or read from the tracker itself) returns them here instead.
 *
 * Never throws: a missing/unreadable/state-less config yields every role => null, which
 * is precisely the signal the caller needs to fall back to its own config read. A throw
 * here would instead take down a caller that has a perfectly good fallback available.
 * Synchronous, because loadSkillConfig is and every consumer of this path is.
 * @param {{cwd?: string}} [opts]
 * @returns {Record<string, string|null>}
 */
function linearStates(opts) {
  // Destructuring in the signature would default only on `undefined` and throw a raw
  // TypeError on an explicit `null` — which the pass-through `states: (opts) => ...`
  // hands straight over — breaking the "never throws" contract two lines above it.
  const cwd = opts?.cwd
  let configured = null
  try {
    configured = trackerConfigFor(loadSkillConfig(cwd ? { cwd } : {}))?.states ?? null
  } catch {
    configured = null
  }
  const resolved = {}
  for (const role of TRACKER_STATE_ROLES) {
    const name = configured?.[role]
    resolved[role] = typeof name === 'string' && name.trim() !== '' ? name : null
  }
  return resolved
}

/**
 * @param {{apiKey: string, fetchImpl?: typeof fetch, endpoint?: string, cwd?: string}} config
 * @returns {import('./adapter.mjs').TrackerAdapter}
 */
export function createLinearAdapter({ apiKey, fetchImpl, endpoint, cwd }) {
  const trackerConfig = trackerConfigFor(loadSkillConfig(cwd ? { cwd } : {}), 'linear')
  const mcpServer = trackerConfig?.mcpServer
  if (typeof mcpServer !== 'string' || mcpServer.trim() === '') {
    throw new Error(
      "tracker adapter: trackerConfig.linear.mcpServer is required to name the tracker's MCP tools; add it to .boss-skills.json",
    )
  }
  return {
    tracker: 'linear',
    // Two query shapes. The selection shape (`stageSelectionQuery`: `{state, selection,
    // requireLabels, excludeLabels}`) resolves, renders and sends one pre-built filter. The legacy
    // `{state, label, assignee, creator, assigneeOrCreator}` shape is kept for callers that never
    // adopted selection (e.g. repo-local sweep gates). Mixing the two throws.
    hasWork: async (query = {}) => {
      if (!isSelectionQuery(query)) {
        const { state, label, assignee, creator, assigneeOrCreator } = query ?? {}
        return runLinearGate({
          apiKey,
          state,
          label,
          assignee,
          creator,
          assigneeOrCreator,
          fetchImpl,
          endpoint,
        })
      }
      const filter = await selectionGateFilter('tracker/linear hasWork', query, {
        apiKey,
        fetchImpl,
        endpoint,
      })
      return runLinearGate({ apiKey, filter, fetchImpl, endpoint })
    },
    hasUnblockedWork: async (query = {}) => {
      if (!isSelectionQuery(query)) {
        const { state, label, assignee, creator, assigneeOrCreator } = query ?? {}
        return runUnblockedGate({
          apiKey,
          state,
          label,
          assignee,
          creator,
          assigneeOrCreator,
          fetchImpl,
          endpoint,
        })
      }
      const filter = await selectionGateFilter('tracker/linear hasUnblockedWork', query, {
        apiKey,
        fetchImpl,
        endpoint,
      })
      return runUnblockedGate({ apiKey, filter, fetchImpl, endpoint })
    },
    // The resolved selection for a `stageSelectionQuery`, so a caller already holding fetched
    // issues (boss-epic's classifier; verify/retro later) can run `matchIssue` with no issue query.
    resolveSelection: (query = {}) =>
      linearResolveSelection({ apiKey, fetchImpl, endpoint, selection: query?.selection }),
    readDependencies: (issue) => extractBlockers(issue),
    isUnblocked: (issue) => isUnblocked(issue),
    formatClaimComment: (token, sessionId) => formatClaimComment(token, sessionId),
    resolveClaim: (comments, myToken, options = null) => {
      const winner = claimWinner(parseClaimComments(comments), options)
      if (winner === null) return null
      return winner === myToken
    },
    normalizeTicket: (issue) => normalizeTicket(issue),
    states: (opts) => linearStates(opts),
    // The executable half of `operationMap.selectPlanned`: the only path that can express the
    // selection filter, so it is the worker's route whenever a stage narrows. `team` is read from
    // the same config block as `mcpServer`; a repo with no configured team (zero-config) passes the
    // run's resolved team as `query.team`, and a configured team always wins over it. With neither,
    // the call is checked and refused. An unknown key throws rather than being dropped: a clause
    // this wrapper silently ignored would WIDEN the worker's read past the gate's scan.
    selectPlanned: async (query = {}) => {
      const unknown = Object.keys(query ?? {}).filter((key) => !SELECT_PLANNED_KEYS.has(key))
      if (unknown.length > 0) {
        throw new Error(
          `tracker/linear selectPlanned: unknown selection key(s) ${unknown.map((key) => JSON.stringify(key)).join(', ')}; refusing to drop a clause the caller asked for`,
        )
      }
      const { state, selection, requireLabels, excludeLabels, limit, team } = query ?? {}
      return linearSelectPlanned({
        apiKey,
        fetchImpl,
        endpoint,
        team: trackerConfig?.team ?? team,
        state,
        selection,
        requireLabels,
        excludeLabels,
        limit,
      })
    },
    // The executable stored-description read behind `tracker/cli.mjs read-description`. Not an
    // operationMap entry: it is a read the gate files are written from by code, so it never enters
    // the MCP approval surface and its bytes never pass through model context.
    readDescription: (issueId) => linearReadDescription({ apiKey, fetchImpl, endpoint, issueId }),
    selectMarked: (query = {}) => {
      const allowed = new Set(['markerPrefix', 'updatedAfter', 'maxPages'])
      if (Object.keys(query ?? {}).some((key) => !allowed.has(key)))
        throw new Error('tracker/linear selectMarked: unknown query keys')
      return linearSelectMarked({ apiKey, fetchImpl, endpoint, ...query })
    },
    selectCandidates: (query = {}) => {
      const unknown = Object.keys(query ?? {}).filter((key) => !SELECT_CANDIDATES_KEYS.has(key))
      if (unknown.length > 0)
        throw new Error('tracker/linear selectCandidates: unknown selection keys')
      const { team, ...selectors } = query ?? {}
      return linearSelectCandidates({
        apiKey,
        fetchImpl,
        endpoint,
        ...selectors,
        team: trackerConfig?.team ?? team,
      })
    },
    // The executable write behind the verify stage's tracker writes. Like readDescription it is not
    // an operationMap entry: nothing in the MCP approval surface changes, and an unknown key throws
    // rather than being dropped, so a caller cannot believe a write was requested that was not.
    applyIssueWrites: (writes = {}) => {
      const unknown = Object.keys(writes ?? {}).filter((key) => !APPLY_ISSUE_WRITES_KEYS.has(key))
      if (unknown.length > 0)
        throw new Error(
          `tracker/linear applyIssueWrites: unknown key(s) ${unknown.map((key) => JSON.stringify(key)).join(', ')}`,
        )
      return linearApplyIssueWrites({ apiKey, fetchImpl, endpoint, ...writes })
    },
    operationMap: buildLinearOperationMap(mcpServer),
  }
}
