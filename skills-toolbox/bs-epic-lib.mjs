// bs-epic-lib.mjs
// Tracker-coupled surface for the /bs-epic orchestration skill: ticket
// normalization/eligibility (classify/normalize/parse-ref) plus the tracker
// wrapper for the merge-time external-blocker re-check. The pure, tracker-
// agnostic scheduling core (graph construction, ready-set, transitive cascade-
// skip, merge-order tie-breaking) now lives in dag-scheduler.mjs and is
// re-exported here so every existing importer is unchanged (a prior refactor). All
// functions here operate on plain data (no I/O, no Linear client). node
// builtins only (mirrors linear-deps-lib.mjs — the cron worktree is
// dependency-free).

// dag-scheduler.mjs owns the pure scheduling functions; re-export them so the
// skill's `bs-epic-lib.mjs` import surface is preserved. `mergeBlockedExternalBlockers`
// is re-exported below via a tracker wrapper that injects BLOCKER_CLEARED_STATE_TYPES.
export { buildGraph, transitiveDependents, readyTickets, nextToMerge } from './dag-scheduler.mjs'
import { mergeBlockedExternalBlockers as mergeBlockedExternalBlockersPure } from './dag-scheduler.mjs'

// Inlined from the former scripts/linear-deps-lib.mjs so this toolbox module is
// self-contained (a prior inlining). Linear state.type values that mean a blocker no
// longer blocks — the one tracker-vocabulary seam the pure module refuses to know.
const BLOCKER_CLEARED_STATE_TYPES = new Set(['completed', 'canceled'])

// Blockers of `issue`: the source issue of every inverse "blocks" relation.
function extractBlockers(issue) {
  const nodes = issue?.inverseRelations?.nodes
  if (!Array.isArray(nodes)) return []
  return nodes.filter((r) => r?.type === 'blocks' && r?.issue).map((r) => r.issue)
}

export { BLOCKER_CLEARED_STATE_TYPES }

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
 * Resolves ONE tracker workflow-state role to a concrete state name, adapter-first.
 *
 * A repo can be fully functional through a vendored tracker adapter that already
 * knows its own state names, with no `trackerConfig.<tracker>.states` block at all;
 * resolving from configuration alone would self-BLOCK such a repo for a value the
 * adapter was holding the whole time. So the order is:
 *   1. the adapter's `states` capability (the PRIMARY authority — it speaks for the
 *      tracker it wraps), then
 *   2. `trackerConfig.<tracker>.states` (the FALLBACK, and the only source for an
 *      adapter that omits the optional capability), then
 *   3. `null` — fail closed. The caller BLOCKs naming BOTH probed sources, because
 *      "which of the two do I fix?" is the only useful thing to say at that point.
 *
 * A value counts only when it is a non-empty, non-whitespace string: a blank name is
 * exactly as unusable as an absent one and must fall through, not win. Either source
 * may be null/undefined/a non-object (an absent capability, an unparsed CLI probe) —
 * all resolve to `null` rather than throwing, since throwing would defeat the very
 * fallback this function exists to perform. Pure; no I/O.
 *
 * @param {{role: string, adapterStates?: object|null, trackerConfigStates?: object|null}} opts
 * @returns {string|null}
 */
export function resolveStateRole({ role, adapterStates, trackerConfigStates } = {}) {
  const pick = (source) => {
    if (!source || typeof source !== 'object') return null
    const value = source[role]
    return typeof value === 'string' && value.trim() !== '' ? value : null
  }
  return pick(adapterStates) ?? pick(trackerConfigStates)
}

/**
 * The `planned` role via resolveStateRole — the state a ticket must sit in to be
 * eligible, and the value classifyTickets is called with. Named separately because it
 * is the one role Phase 0 resolves before it can schedule anything at all.
 * @param {{adapterStates?: object|null, trackerConfigStates?: object|null}} opts
 * @returns {string|null}
 */
export function resolvePlannedState({ adapterStates, trackerConfigStates } = {}) {
  return resolveStateRole({ role: 'planned', adapterStates, trackerConfigStates })
}

/**
 * Splits normalized tickets into three buckets against the tracker's configured
 * planned-state name (`plannedState`, e.g. the reference impl's
 * `trackerConfigFor(config).states.planned`) — the one workflow-state word this
 * pure module refuses to bake in, so the published core stays project-agnostic:
 *   - `eligible`: stateName is the planned state AND labels include
 *     `agent-friendly` AND `planUrl` present AND NOT `needs-human`.
 *   - `done`: state is Done/Canceled (`stateType` in BLOCKER_CLEARED_STATE_TYPES)
 *     — counts as already merged for scheduling purposes.
 *   - `skipped`: everything else (`{ticket, reason}`), e.g. not-yet-planned,
 *     In Progress, In Review, missing plan, `needs-human`.
 *
 * `plannedState` must be a non-empty string (the caller resolves it from the
 * tracker adapter config); an unresolved value throws rather than silently
 * marking every ticket eligible or none — a mis-configured repo must not spawn
 * sessions for unplanned work.
 */
export function classifyTickets(tickets, plannedState) {
  if (typeof plannedState !== 'string' || plannedState.length === 0) {
    throw new Error('classifyTickets: plannedState (the configured planned-state name) is required')
  }
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
    if (ticket.stateName !== plannedState) {
      skipped.push({
        ticket,
        reason: `${ticket.id}: state is ${ticket.stateName}, expected ${plannedState}`,
      })
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

const TICKET_ID_RE = /^[A-Za-z]+-\d+$/i
// A pasted Linear issue URL: https://linear.app/<workspace>/issue/<KEY>-123/<slug>.
// The captured group is the ticket id; the trailing slug/query/hash is ignored.
const LINEAR_ISSUE_URL_RE = /^https?:\/\/linear\.app\/[^/]+\/issue\/([A-Za-z]+-\d+)(?:[/?#]|$)/i

/**
 * Resolves a single CLI token to a ticket id, accepting either a bare id
 * (`<issue-id>`) or a pasted Linear issue URL
 * (`https://linear.app/<workspace>/issue/<KEY>-123/<slug>`). Returns the ticket
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
 * be a bare ticket id (`<issue-id>`) OR a pasted Linear issue URL.
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
 * Tracker-coupled merge-time external-blocker re-check. `readyTickets` clears
 * external blockers for LAUNCH via the union of both `--assume-cleared*` sets,
 * but the serialized merge step must NOT merge past a ticket's own still-open
 * external gate (e.g. a `needs-human` go/no-go) unless the operator explicitly
 * cleared it FOR MERGE. Given the ticket about to be merged, the graph, the
 * merge-clearance set (`--assume-cleared-and-merge` only), and a freshly-fetched
 * map of external blocker id → current Linear state type, resolves which
 * blockers are in a cleared state (Done/Canceled) and delegates to the pure
 * primitive in dag-scheduler.mjs. Returns the external blocker ids STILL OPEN:
 * neither cleared-for-merge nor in a cleared state. Empty → safe to merge;
 * non-empty → the merge step skips-with-note. A blocker with no fetched state is
 * treated as still open (fail-closed). Preserves the original signature so the
 * bs-epic skill body and bs-epic-lib.test.mjs are unchanged.
 */
export function mergeBlockedExternalBlockers(
  ticket,
  graph,
  { clearedForMerge = new Set(), blockerStateTypes = new Map() } = {},
) {
  const clearedBlockers = new Set(
    [...blockerStateTypes]
      .filter(([, stateType]) => BLOCKER_CLEARED_STATE_TYPES.has(stateType))
      .map(([id]) => id),
  )
  return mergeBlockedExternalBlockersPure(ticket, graph, { clearedForMerge, clearedBlockers })
}

/** Default driver-side stall window: the repair lease is presumed dead after
 *  8 minutes of no head-SHA movement AND no repair-chat output. Matches the
 *  daemon-side constant; skew between the two is harmless (whichever trips
 *  first routes to the same 'stalled' handling), so it stays a parameter. */
export const REPAIR_STALL_WINDOW_MS = 8 * 60_000

/**
 * Classifies the repair lease on a polled session so the Phase 3c driver has a
 * TERMINATING escape from a frozen lease. Phase 3c must never dispatch a second
 * repairer while a live lease is held, but with a stuck lease + dead repair chat
 * that rule alone has no terminating condition — the driver re-polls forever.
 * This is the predicate that ends the loop.
 *
 * Inputs are the plain `get_session` payload fields plus the driver's own
 * previous-poll snapshot:
 *   - `repairActive`             — `session.repair_active`
 *   - `repairStalledAt`          — `session.repair_stalled_at` (absent on older daemons)
 *   - `lastRepairHeadSha`        — `session.last_repair_head_sha`, this poll
 *   - `prevLastRepairHeadSha`    — the same field on the previous poll (driver state)
 *   - `repairChatLastOutputAtMs` — tracked repair chat's last_output_at /
 *                                  last_agent_activity_at, epoch ms
 *   - `nowMs`, `stallWindowMs`   — clock + window (default REPAIR_STALL_WINDOW_MS)
 *
 * Rules, in order:
 *   1. not active and no `repairStalledAt`     → `'none'`   (no lease at all)
 *   2. `repairStalledAt` set                   → `'stalled'` (the daemon already decided)
 *   3. active, head SHA unchanged across polls
 *      AND chat output stale >= the window     → `'stalled'` (driver evidence — works
 *                                                against daemons with no stall field)
 *   4. otherwise                               → `'active'`
 *
 * Missing/undefined evidence fails toward `'active'`: absent data must never be
 * read as a dead lease, because `'stalled'` burns a repair round and can
 * fail-isolate a ticket. Pure — no I/O, no clock read of its own.
 */
export function classifyRepairLease({
  repairActive,
  repairStalledAt,
  lastRepairHeadSha,
  prevLastRepairHeadSha,
  repairChatLastOutputAtMs,
  nowMs,
  stallWindowMs = REPAIR_STALL_WINDOW_MS,
} = {}) {
  const daemonSaysStalled =
    repairStalledAt !== undefined && repairStalledAt !== null && repairStalledAt !== ''
  if (!repairActive && !daemonSaysStalled) return 'none'
  if (daemonSaysStalled) return 'stalled'

  // Driver-side evidence. BOTH legs must be proven, from present data.
  const headFrozen =
    typeof lastRepairHeadSha === 'string' &&
    lastRepairHeadSha.length > 0 &&
    lastRepairHeadSha === prevLastRepairHeadSha
  const chatStale =
    Number.isFinite(repairChatLastOutputAtMs) &&
    Number.isFinite(nowMs) &&
    Number.isFinite(stallWindowMs) &&
    nowMs - repairChatLastOutputAtMs >= stallWindowMs
  return headFrozen && chatStale ? 'stalled' : 'active'
}
