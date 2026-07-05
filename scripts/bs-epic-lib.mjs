// scripts/bs-epic-lib.mjs
// Pure scheduling/decision logic for the /bs-epic orchestration skill: ticket
// eligibility, the in-epic/external dependency graph, ready-set computation,
// cascade-skip on failure, and merge-order tie-breaking. All functions here
// operate on plain data (no I/O, no Linear client) so the whole decision
// surface is unit-testable in isolation and reused verbatim by the skill.
// node builtins only (mirrors linear-deps-lib.mjs — the cron worktree is
// dependency-free).

import { extractBlockers, BLOCKER_CLEARED_STATE_TYPES } from './linear-deps-lib.mjs'

export { BLOCKER_CLEARED_STATE_TYPES }

// Linear numeric priority, ranked most- to least-urgent: Urgent(1) >
// High(2) > Medium(3) > Low(4) > None(0). Same rule bs-plan Phase 1 uses.
const PRIORITY_ORDER = [1, 2, 3, 4, 0]

function priorityRank(priority) {
  const idx = PRIORITY_ORDER.indexOf(priority)
  return idx === -1 ? PRIORITY_ORDER.length : idx
}

// Sort comparator: most-urgent priority first, ties broken by oldest
// createdAt first (serialized-merge / ready-queue order).
function byPriorityThenOldest(a, b) {
  const rankDiff = priorityRank(a.priority) - priorityRank(b.priority)
  if (rankDiff !== 0) return rankDiff
  return new Date(a.createdAt).getTime() - new Date(b.createdAt).getTime()
}

// First `{title, url}` entry whose title starts with "Implementation plan",
// or null. Shared by the attachments/links fallback chain in normalizeTicket.
function firstPlanUrl(entries) {
  if (!Array.isArray(entries)) return null
  const match = entries.find(
    (entry) => typeof entry?.title === 'string' && entry.title.startsWith('Implementation plan'),
  )
  return match?.url ?? null
}

function normalizeLabels(labels) {
  const list = Array.isArray(labels?.nodes) ? labels.nodes : Array.isArray(labels) ? labels : []
  return list.map((label) => (typeof label === 'string' ? label : label?.name)).filter(Boolean)
}

// Blocker ids in precedence order: (a) raw GraphQL `inverseRelations` via
// extractBlockers, (b) MCP `relations.blockedBy` (entries may be a bare
// string or an object — the human identifier is preferred over the uuid),
// (c) an already-flat `blockedBy` array (pre-normalized ticket).
function normalizeBlockedBy(issue) {
  if (issue.inverseRelations) {
    return extractBlockers(issue).map((blocker) => blocker.identifier ?? blocker.id)
  }
  if (Array.isArray(issue.relations?.blockedBy)) {
    return issue.relations.blockedBy.map((entry) =>
      typeof entry === 'string' ? entry : (entry?.identifier ?? entry?.id),
    )
  }
  return Array.isArray(issue.blockedBy) ? issue.blockedBy : []
}

/**
 * Flattens a Linear issue payload into the plain ticket shape every other
 * function in this module consumes:
 * `{id, title, priority, createdAt, stateName, stateType, labels, planUrl, blockedBy: [ids]}`.
 *
 * Accepts three issue shapes and normalizes each field independently:
 *   - raw GraphQL (`issue.inverseRelations`, `issue.state.{name,type}`)
 *   - Linear MCP `get_issue includeRelations=true`
 *     (`issue.relations.blockedBy`, `issue.status`/`issue.statusType`,
 *     `issue.priority` as `{value, name}`, `issue.attachments`/`issue.links`)
 *   - an already-normalized ticket (flat `blockedBy`/`stateName`/`stateType`),
 *     which passes through unchanged.
 */
export function normalizeTicket(issue) {
  const priority =
    typeof issue.priority === 'object' && issue.priority !== null
      ? issue.priority.value
      : issue.priority

  return {
    id: issue.identifier ?? issue.id,
    title: issue.title,
    priority,
    createdAt: issue.createdAt,
    stateName: issue.state?.name ?? issue.status ?? issue.stateName ?? null,
    stateType: issue.state?.type ?? issue.statusType ?? issue.stateType ?? null,
    labels: normalizeLabels(issue.labels),
    planUrl: firstPlanUrl(issue.attachments) ?? firstPlanUrl(issue.links) ?? issue.planUrl ?? null,
    blockedBy: normalizeBlockedBy(issue),
  }
}

/**
 * Splits normalized tickets into three buckets:
 *   - `eligible`: stateName Todo AND labels include `agent-friendly` AND
 *     `planUrl` present AND NOT `needs-human`.
 *   - `done`: state is Done/Canceled (`stateType` in BLOCKER_CLEARED_STATE_TYPES)
 *     — counts as already merged for scheduling purposes.
 *   - `skipped`: everything else (`{ticket, reason}`), e.g. Unplanned,
 *     In Progress, In Review, missing plan, `needs-human`.
 */
export function classifyTickets(tickets) {
  const eligible = []
  const done = []
  const skipped = []
  for (const ticket of tickets) {
    if (BLOCKER_CLEARED_STATE_TYPES.has(ticket.stateType)) {
      done.push(ticket)
      continue
    }
    const labels = ticket.labels ?? []
    if (labels.includes('needs-human')) {
      skipped.push({ ticket, reason: `${ticket.id}: needs-human label present` })
      continue
    }
    if (!ticket.planUrl) {
      skipped.push({ ticket, reason: `${ticket.id}: missing Implementation plan link` })
      continue
    }
    if (ticket.stateName !== 'Todo') {
      skipped.push({ ticket, reason: `${ticket.id}: state is ${ticket.stateName}, expected Todo` })
      continue
    }
    if (!labels.includes('agent-friendly')) {
      skipped.push({ ticket, reason: `${ticket.id}: missing agent-friendly label` })
      continue
    }
    eligible.push(ticket)
  }
  return { eligible, done, skipped }
}

/**
 * Builds the epic's dependency graph from normalized tickets, partitioning
 * each ticket's `blockedBy` into edges inside the epic set (must be merged
 * before the ticket can run) vs. outside it (must be externally cleared).
 * Returns `{nodes: Map<id, ticket>, inEpicBlockers: Map<id, [ids]>, externalBlockers: Map<id, [ids]>}`.
 */
export function buildGraph(tickets) {
  const nodes = new Map(tickets.map((ticket) => [ticket.id, ticket]))
  const inEpicBlockers = new Map()
  const externalBlockers = new Map()
  for (const ticket of tickets) {
    const blockedBy = ticket.blockedBy ?? []
    inEpicBlockers.set(
      ticket.id,
      blockedBy.filter((id) => nodes.has(id)),
    )
    externalBlockers.set(
      ticket.id,
      blockedBy.filter((id) => !nodes.has(id)),
    )
  }
  return { nodes, inEpicBlockers, externalBlockers }
}

/**
 * Set of ticket ids downstream (transitively, in-epic only) of any id in
 * `failedIds` — these must be skipped, with the failed ancestor named in the
 * skip reason by the caller. Finite even over a cyclic graph: each id is
 * enqueued at most once.
 */
export function transitiveDependents(graph, failedIds) {
  const dependents = new Map()
  for (const [id, blockers] of graph.inEpicBlockers) {
    for (const blockerId of blockers) {
      if (!dependents.has(blockerId)) dependents.set(blockerId, [])
      dependents.get(blockerId).push(id)
    }
  }
  const result = new Set()
  const queue = [...failedIds]
  while (queue.length > 0) {
    const current = queue.shift()
    for (const dependentId of dependents.get(current) ?? []) {
      if (!result.has(dependentId)) {
        result.add(dependentId)
        queue.push(dependentId)
      }
    }
  }
  return result
}

/**
 * Sorted (priority, then oldest createdAt) array of tickets that are ready
 * to start: every in-epic blocker is in `merged`, every external blocker is
 * in `externallyCleared`, and the ticket itself is not merged/failed/
 * in-flight nor a transitive dependent of a failed ticket (cascade-skip is
 * computed here — the single authority other callers should defer to).
 */
export function readyTickets(graph, { merged, failed, inFlight, externallyCleared }) {
  const cascadeSkipped = transitiveDependents(graph, failed)
  const ready = []
  for (const [id, ticket] of graph.nodes) {
    if (merged.has(id) || failed.has(id) || inFlight.has(id) || cascadeSkipped.has(id)) continue
    const inEpicClear = (graph.inEpicBlockers.get(id) ?? []).every((b) => merged.has(b))
    const externalClear = (graph.externalBlockers.get(id) ?? []).every((b) =>
      externallyCleared.has(b),
    )
    if (inEpicClear && externalClear) ready.push(ticket)
  }
  return ready.sort(byPriorityThenOldest)
}

/**
 * Of the given green (passed-review, not-yet-merged) sessions, returns the
 * single ticket id that is mergeable right now — every in-epic blocker
 * already in `merged` — tie-broken by priority then oldest createdAt
 * (serialized-merge order). Returns null when none are mergeable.
 */
export function nextToMerge(greens, graph, merged) {
  const mergeable = greens
    .map((green) => graph.nodes.get(green.id) ?? green)
    .filter((ticket) => (graph.inEpicBlockers.get(ticket.id) ?? []).every((b) => merged.has(b)))
  if (mergeable.length === 0) return null
  return [...mergeable].sort(byPriorityThenOldest)[0].id
}

const TICKET_ID_RE = /^[A-Za-z]+-\d+$/i
// A pasted Linear issue URL: https://linear.app/<workspace>/issue/BOS-179/<slug>.
// The captured group is the ticket id; the trailing slug/query/hash is ignored.
const LINEAR_ISSUE_URL_RE = /^https?:\/\/linear\.app\/[^/]+\/issue\/([A-Za-z]+-\d+)(?:[/?#]|$)/i

/**
 * Resolves a single CLI token to a ticket id, accepting either a bare id
 * (`BOS-179`) or a pasted Linear issue URL
 * (`https://linear.app/<workspace>/issue/BOS-179/<slug>`). Returns the ticket
 * id verbatim, or null when the token is neither (so callers can reject typo'd
 * flags / stray args).
 */
export function parseTicketRef(arg) {
  if (typeof arg !== 'string') return null
  const trimmed = arg.trim()
  if (TICKET_ID_RE.test(trimmed)) return trimmed
  const match = LINEAR_ISSUE_URL_RE.exec(trimmed)
  return match ? match[1] : null
}

/**
 * Parses `/bs-epic` CLI args. A single positional ticket ref is treated as
 * the epic PARENT (`{parentId: id, ids: []}`) — its sub-issues are the work
 * items. Two or more positional refs are an explicit list of work items
 * (`{parentId: null, ids: [...]}`) with no separate parent. Each positional may
 * be a bare ticket id (`BOS-179`) OR a pasted Linear issue URL.
 *
 * Flags: `--parallel N` (integer 1..8, default 4), `--agent <name>` (default
 * 'claude'), and two repeatable operator overrides that treat a named external
 * blocker as cleared — `--assume-cleared <ref>` unblocks a parked dependent for
 * LAUNCH only, while `--assume-cleared-and-merge <ref>` additionally lets the
 * serialized merge step merge past that blocker's own still-open gate. Both
 * accept a bare id or a Linear URL. Throws on zero positional refs, on
 * `--parallel` outside [1, 8] or non-integer, on `--agent` / `--assume-cleared`
 * / `--assume-cleared-and-merge` missing or malformed value, or on a positional
 * that is neither a ticket id nor a Linear URL (catches typo'd flags).
 */
export function parseEpicArgs(argv) {
  const ids = []
  const assumeCleared = []
  const assumeClearedAndMerge = []
  let parallel = 4
  let agent = 'claude'
  const takeClearedRef = (raw, flag) => {
    const ref = parseTicketRef(raw)
    if (!ref) {
      throw new Error(`parseEpicArgs: ${flag} requires a ticket id or Linear URL, got ${raw}`)
    }
    return ref
  }
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i]
    if (arg === '--parallel') {
      const raw = argv[(i += 1)]
      const n = Number(raw)
      if (!Number.isInteger(n) || n < 1 || n > 8) {
        throw new Error(`parseEpicArgs: --parallel must be an integer between 1 and 8, got ${raw}`)
      }
      parallel = n
    } else if (arg === '--agent') {
      const raw = argv[(i += 1)]
      if (raw === undefined || raw.startsWith('--')) {
        throw new Error(`parseEpicArgs: --agent requires a runner name, got ${raw}`)
      }
      agent = raw
    } else if (arg === '--assume-cleared') {
      assumeCleared.push(takeClearedRef(argv[(i += 1)], '--assume-cleared'))
    } else if (arg === '--assume-cleared-and-merge') {
      assumeClearedAndMerge.push(takeClearedRef(argv[(i += 1)], '--assume-cleared-and-merge'))
    } else if (arg.startsWith('--')) {
      throw new Error(`parseEpicArgs: unknown flag ${arg}`)
    } else {
      const ref = parseTicketRef(arg)
      if (!ref) {
        throw new Error(`parseEpicArgs: not a ticket id or Linear URL: ${arg}`)
      }
      ids.push(ref)
    }
  }
  if (ids.length === 0) {
    throw new Error('parseEpicArgs: at least one ticket id is required')
  }
  if (ids.length === 1) {
    return { parentId: ids[0], ids: [], parallel, agent, assumeCleared, assumeClearedAndMerge }
  }
  return { parentId: null, ids, parallel, agent, assumeCleared, assumeClearedAndMerge }
}

/**
 * Merge-time external-blocker re-check. `readyTickets` clears external blockers
 * for LAUNCH via the union of both `--assume-cleared*` sets, but the serialized
 * merge step must NOT merge past a ticket's own still-open external gate (e.g. a
 * `needs-human` go/no-go) unless the operator explicitly cleared it FOR MERGE.
 * Given the ticket about to be merged, the graph, the merge-clearance set
 * (`--assume-cleared-and-merge` only), and a freshly-fetched map of external
 * blocker id → current Linear state type, returns the external blocker ids that
 * are STILL OPEN: neither in a cleared state (Done/Canceled) nor cleared for
 * merge. Empty → safe to merge; non-empty → the merge step skips-with-note.
 * A blocker with no fetched state is treated as still open (fail-closed).
 */
export function mergeBlockedExternalBlockers(
  ticket,
  graph,
  { clearedForMerge = new Set(), blockerStateTypes = new Map() } = {},
) {
  const externals = graph.externalBlockers.get(ticket.id) ?? []
  return externals.filter((id) => {
    if (clearedForMerge.has(id)) return false
    return !BLOCKER_CLEARED_STATE_TYPES.has(blockerStateTypes.get(id))
  })
}
