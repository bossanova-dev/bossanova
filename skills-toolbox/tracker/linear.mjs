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

import {
  buildIssueCountFilter,
  linearRequest,
  resolveLinearUserId,
  runLinearGate,
} from '../linear-gate-lib.mjs'
import { runUnblockedGate, extractBlockers, isUnblocked } from '../linear-deps-lib.mjs'
import { claimWinner, formatClaimComment, parseClaimComments } from '../linear-claim.mjs'
import { normalizeTicket } from '../bs-epic-lib.mjs'
import { loadSkillConfig, trackerConfigFor } from '../skill-config.mjs'
// From adapter-core.mjs, not adapter.mjs: adapter.mjs imports THIS module to build
// its registry, so reading the roles from there would make the pair circular.
import { TRACKER_CREDENTIALS_MISSING, TRACKER_STATE_ROLES } from './adapter-core.mjs'

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
// The closed set of query keys `selectPlanned` knows how to put on the wire. See the adapter wrapper.
const SELECT_PLANNED_KEYS = new Set(['state', 'label', 'assigneeOrCreator', 'limit'])

// A GraphQL connection read as a plain array: `{nodes: [...]}` -> `[...]`. An already-flat array
// passes through, and anything else is an empty list rather than a throw, matching the shared
// ticket normalizer's own tolerance for these two fields.
function connectionNodes(connection) {
  if (Array.isArray(connection)) return connection
  return Array.isArray(connection?.nodes) ? connection.nodes : []
}

/**
 * The executable `selectPlanned` capability: the planned-candidate list, narrowed by exactly the
 * filter the cron gate applies. The filter is composed by the SAME `buildIssueCountFilter` the gate
 * uses — never a second construction path — and ANDed with the configured team as a sibling key, so
 * the worker's candidate universe can only ever be the gate's, restricted to one team.
 *
 * Fails CLOSED on every input that would otherwise widen the scan: a missing state, a missing
 * team, an empty or malformed label set, a blank identity selector, an out-of-range limit — each
 * throws before any request. `me` resolves through the one identity contract (one viewer lookup,
 * or a throw, never a dropped clause). A payload whose `issues.nodes` is not an array throws too:
 * an empty list is an answer, and an unreadable payload is not one.
 *
 * Exported for its own tests: `validateConfig` already rejects a Linear block with no `team`, so
 * the team guard is unreachable through `createLinearAdapter` and is pinned here directly — it is
 * the last line of defence for a caller that hands this a team from anywhere else.
 */
export async function linearSelectPlanned({
  apiKey,
  fetchImpl,
  endpoint,
  team,
  state,
  label,
  assigneeOrCreator,
  limit = SELECT_PLANNED_MAX_LIMIT,
}) {
  if (typeof state !== 'string' || state.trim() === '') {
    throw new Error(
      'tracker/linear selectPlanned: a non-empty state is required; refusing to scan every state',
    )
  }
  if (typeof team !== 'string' || team.trim() === '') {
    throw new Error(
      'tracker/linear selectPlanned: trackerConfig.linear.team is required to scope the candidate list; add it to .boss-skills.json',
    )
  }
  if (label !== undefined && label !== null) {
    const names = Array.isArray(label) ? label : [label]
    if (
      names.length === 0 ||
      !names.every((name) => typeof name === 'string' && name.trim() !== '')
    ) {
      throw new Error(
        'tracker/linear selectPlanned: label must be a non-empty name or a non-empty array of non-empty names',
      )
    }
  }
  if (
    assigneeOrCreator !== undefined &&
    assigneeOrCreator !== null &&
    (typeof assigneeOrCreator !== 'string' || assigneeOrCreator.trim() === '')
  ) {
    throw new Error(
      'tracker/linear selectPlanned: assigneeOrCreator must be a non-empty string when set',
    )
  }
  if (!Number.isInteger(limit) || limit < 1 || limit > SELECT_PLANNED_MAX_LIMIT) {
    throw new Error(
      `tracker/linear selectPlanned: limit must be an integer from 1 to ${SELECT_PLANNED_MAX_LIMIT}; got ${JSON.stringify(limit)}`,
    )
  }
  const assigneeOrCreatorId = await resolveLinearUserId({
    apiKey,
    user: assigneeOrCreator ?? undefined,
    fetchImpl,
    endpoint,
  })
  const filter = {
    ...buildIssueCountFilter({ state, label: label ?? undefined, assigneeOrCreatorId }),
    team: { name: { eq: team } },
  }
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

// Declarative map of each agent-driven capability to the Linear MCP tool the
// skills invoke today, with the argument/response shape they rely on. This is
// the single source of truth later extraction tickets (and generalized skill
// prose) consume — change the tracker, change this map.
export function buildLinearOperationMap(mcpServer) {
  // The single positional argument is the MCP SERVER NAME, a bare string. Anything else — an options
  // object, above all, because every other helper in this tree takes one — interpolates into all
  // fifteen template literals below and yields a perfectly well-formed map whose every tool name is
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
        'file, so an already-gated body is never retyped into a tool argument',
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
    // The identity selectors are forwarded, not dropped. A caller that needs to narrow the gate
    // to its own work would otherwise have to bypass this seam and reach the gate libraries
    // directly, which is exactly the fork this capability exists to make unnecessary. Each is
    // optional and inert, so a `{state, label}` caller emits the filter it always did.
    hasWork: ({ state, label, assignee, creator, assigneeOrCreator } = {}) =>
      runLinearGate({
        apiKey,
        state,
        label,
        assignee,
        creator,
        assigneeOrCreator,
        fetchImpl,
        endpoint,
      }),
    hasUnblockedWork: ({ state, label, assignee, creator, assigneeOrCreator } = {}) =>
      runUnblockedGate({
        apiKey,
        state,
        label,
        assignee,
        creator,
        assigneeOrCreator,
        fetchImpl,
        endpoint,
      }),
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
    // identity disjunction and the label set, so it is the worker's route whenever a repo narrows.
    // `team` is read from the same config block as `mcpServer`, at call time checked, so a repo
    // with no team still loads — it just cannot take the narrowed route. An unknown key throws
    // rather than being dropped: `list-planned` forwards `plannedSelectionQuery` whole, so a
    // clause this wrapper silently ignored would WIDEN the worker's read past the gate's scan.
    selectPlanned: async (query = {}) => {
      const unknown = Object.keys(query ?? {}).filter((key) => !SELECT_PLANNED_KEYS.has(key))
      if (unknown.length > 0) {
        throw new Error(
          `tracker/linear selectPlanned: unknown selection key(s) ${unknown.map((key) => JSON.stringify(key)).join(', ')}; refusing to drop a clause the caller asked for`,
        )
      }
      const { state, label, assigneeOrCreator, limit } = query ?? {}
      return linearSelectPlanned({
        apiKey,
        fetchImpl,
        endpoint,
        team: trackerConfig?.team,
        state,
        label,
        assigneeOrCreator,
        limit,
      })
    },
    // The executable stored-description read behind `tracker/cli.mjs read-description`. Not an
    // operationMap entry: it is a read the gate files are written from by code, so it never enters
    // the MCP approval surface and its bytes never pass through model context.
    readDescription: (issueId) => linearReadDescription({ apiKey, fetchImpl, endpoint, issueId }),
    operationMap: buildLinearOperationMap(mcpServer),
  }
}
