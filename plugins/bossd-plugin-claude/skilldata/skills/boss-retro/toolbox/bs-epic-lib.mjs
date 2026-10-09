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
export {
  buildGraph,
  transitiveDependents,
  transitiveDependentCounts,
  readyTickets,
  nextToMerge,
} from './dag-scheduler.mjs'
import { mergeBlockedExternalBlockers as mergeBlockedExternalBlockersPure } from './dag-scheduler.mjs'
import { DEFAULT_TRACKER_STATES } from './skill-config.mjs'
import { hasSelectionFlags, matchIssue, parseSelectionFlags } from './selection.mjs'

// Inlined from the former linear-deps-lib.mjs so this toolbox module is
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
// or null. Retained for display compatibility only: eligibility below requires
// a native attachment, never a link.
function firstPlanUrl(entries) {
  if (!Array.isArray(entries)) return null
  const match = entries.find(
    (entry) => typeof entry?.title === 'string' && entry.title.startsWith('Implementation plan'),
  )
  return match?.url ?? null
}

// A native tracker attachment is the only implementation-plan artifact that an
// epic can hand to boss-build. Keep links in planUrl for callers that display
// legacy metadata, but never let one launch a child session.
function canonicalPlanAttachment(attachments, issueID) {
  if (!Array.isArray(attachments)) return null
  const id = String(issueID ?? '').toLowerCase()
  const isPlan = (title) => {
    const lower = String(title ?? '').toLowerCase()
    return lower.startsWith('implementation plan') && (id === '' || lower.includes(id))
  }
  return attachments
    .filter((attachment) => isPlan(attachment?.title))
    .reduce((newest, attachment) => {
      if (!newest || Date.parse(attachment.createdAt || '') > Date.parse(newest.createdAt || '')) {
        return attachment
      }
      return newest
    }, null)
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
 * `{id, title, priority, createdAt, stateName, stateType, labels, planUrl,
 * planAttachment, blockedBy: [ids]}`.
 *
 * Accepts three issue shapes and normalizes each field independently:
 *   - raw GraphQL (`issue.inverseRelations`, `issue.state.{name,type}`)
 *   - Linear MCP `get_issue includeRelations=true`
 *     (`issue.relations.blockedBy`, `issue.status`/`issue.statusType`,
 *     `issue.priority` as `{value, name}`, `issue.attachments`/`issue.links`)
 *   - an already-normalized ticket (flat `blockedBy`/`stateName`/`stateType`),
 *     which passes through unchanged.
 *
 * It also carries `assigneeId`, `creatorId` and `projectId` for selection matching, read from the
 * raw GraphQL relation objects (`assignee.id`, ...), the MCP spellings (`assigneeId`,
 * `createdById`, `projectId`) or the flat normalized ones. `null` means the source says there is
 * none (unassigned, no project). A source that does not carry the field at all leaves it
 * `undefined` — never `null` — so `matchIssue` reports it as absent instead of treating an
 * unfetched field as "unassigned" and letting it through an exclude filter.
 */
export function normalizeTicket(issue) {
  const id = issue.identifier ?? issue.id
  const priority =
    typeof issue.priority === 'object' && issue.priority !== null
      ? issue.priority.value
      : issue.priority

  return {
    id,
    title: issue.title,
    priority,
    createdAt: issue.createdAt,
    stateName: issue.state?.name ?? issue.status ?? issue.stateName ?? null,
    stateType: issue.state?.type ?? issue.statusType ?? issue.stateType ?? null,
    labels: normalizeLabels(issue.labels),
    planUrl: firstPlanUrl(issue.attachments) ?? firstPlanUrl(issue.links) ?? issue.planUrl ?? null,
    planAttachment:
      canonicalPlanAttachment(issue.attachments, id) ??
      (issue.planAttachment?.title === `Implementation plan (${id})` ? issue.planAttachment : null),
    blockedBy: normalizeBlockedBy(issue),
    assigneeId: relationIdOf(issue, ['assigneeId'], 'assignee'),
    creatorId: relationIdOf(issue, ['creatorId', 'createdById'], 'creator'),
    projectId: relationIdOf(issue, ['projectId'], 'project'),
  }
}

// One relation id off any issue shape: a flat key first, then the GraphQL object. `null` when the
// source carries the relation as empty; `undefined` when it does not carry it. A string-valued
// relation key (an MCP display name) is not an id and reads as not carried.
function relationIdOf(issue, flatKeys, objectKey) {
  for (const key of flatKeys) {
    if (issue[key] !== undefined) return issue[key] ?? null
  }
  const value = issue[objectKey]
  if (value === null) return null
  if (value && typeof value === 'object' && typeof value.id === 'string') return value.id
  return undefined
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
  return pick(adapterStates) ?? pick(trackerConfigStates) ?? pick(DEFAULT_TRACKER_STATES)
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
 *     `agent-build` AND a canonical native `planAttachment` present AND NOT
 *     `needs-human`. A legacy link-only plan is skipped for migration/replanning.
 *   - `done`: state is Done/Canceled (`stateType` in BLOCKER_CLEARED_STATE_TYPES)
 *     — counts as already merged for scheduling purposes.
 *   - `skipped`: everything else (`{ticket, reason}`), e.g. not-yet-planned,
 *     In Progress, In Review, missing plan, `needs-human`.
 *
 * `plannedState` must be a non-empty string (the caller resolves it from the
 * tracker adapter config); an unresolved value throws rather than silently
 * marking every ticket eligible or none — a mis-configured repo must not spawn
 * sessions for unplanned work.
 *
 * `selection` (optional) is a RESOLVED selection (`tracker/cli.mjs resolve-selection --stage
 * epic`). A non-done ticket it does not match goes to `skipped` with reason
 * `excluded by selection: <matchIssue reason>`. It is checked after `done`, so a merged sibling
 * still clears its dependents.
 */
export function classifyTickets(
  tickets,
  plannedState,
  { agentBuildLabel = 'agent-build', needsHumanLabel = 'needs-human', selection = null } = {},
) {
  if (typeof plannedState !== 'string' || plannedState.length === 0) {
    throw new Error('classifyTickets: plannedState (the configured planned-state name) is required')
  }
  // Label and state names are compared the way a person reads them: case, spacing, `-` and `_`
  // do not make `Agent-Build` a different label from `agent_build`.
  const key = (value) =>
    String(value ?? '')
      .toLowerCase()
      .replace(/[\s_-]+/g, '')
  const eligible = []
  const done = []
  const skipped = []
  for (const ticket of tickets) {
    if (BLOCKER_CLEARED_STATE_TYPES.has(ticket.stateType)) {
      done.push(ticket)
      continue
    }
    if (selection) {
      const { matches, reason } = matchIssue(ticket, selection)
      if (!matches) {
        skipped.push({ ticket, reason: `${ticket.id}: excluded by selection: ${reason}` })
        continue
      }
    }
    const labels = new Set((ticket.labels ?? []).map(key))
    if (labels.has(key(needsHumanLabel))) {
      skipped.push({ ticket, reason: `${ticket.id}: needs-human label present` })
      continue
    }
    if (!ticket.planAttachment) {
      skipped.push({
        ticket,
        reason: `${ticket.id}: missing native Implementation plan attachment (migration/replanning required)`,
      })
      continue
    }
    if (key(ticket.stateName) !== key(plannedState)) {
      skipped.push({
        ticket,
        reason: `${ticket.id}: state is ${ticket.stateName}, expected ${plannedState}`,
      })
      continue
    }
    if (!labels.has(key(agentBuildLabel))) {
      skipped.push({ ticket, reason: `${ticket.id}: missing agent-build label` })
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
 * Parses `/bs-epic` CLI args into a DISCRIMINATED `mode`, with the two legacy
 * positional forms carrying exactly the keys and values they always did:
 *
 *   - one positional        → `{mode: 'parent', parentId: id, ids: []}` — the
 *     epic PARENT, whose sub-issues are the work items.
 *   - two or more positional → `{mode: 'list', parentId: null, ids: [...]}` — an
 *     explicit list of work items with no separate parent.
 *   - one or more `--epic`   → `{mode: 'parents', parentId: null, parentIds: [...]}`
 *     — the additive MULTI-ROOT selector: several epic parents driven by ONE
 *     coordinator over one deduplicated child universe.
 *
 * `parentIds` is additive and present in every mode (`[id]` in `parent` mode,
 * `[]` in `list` mode) so a combined-run caller reads one key. `parentId` stays
 * `null` in `parents` mode on purpose: a legacy single-parent consumer must fail
 * to find a root rather than silently run only the first of several requested.
 *
 * Positional refs are NOT deduplicated — the mode is decided on the positional
 * COUNT, so collapsing a repeated id would silently flip an explicit two-ticket
 * list into parent mode. `--epic` refs ARE deduplicated, in first-seen order:
 * there the count carries no meaning.
 *
 * Every ref (positional, `--epic`, `--assume-cleared*`) may be a bare ticket id
 * (`<issue-id>`) OR a pasted Linear issue URL.
 *
 * Flags: `--parallel N` (integer 1..8, default 4), `--agent <name>` (default
 * 'claude'), the repeatable `--epic <ref>` root selector, and two repeatable
 * operator overrides that treat a named external blocker as cleared —
 * `--assume-cleared <ref>` unblocks a parked dependent for LAUNCH only, while
 * `--assume-cleared-and-merge <ref>` additionally lets the serialized merge step
 * merge past that blocker's own still-open gate. Throws when neither a
 * positional nor an `--epic` was given, when `--epic` is MIXED with positional
 * refs (ambiguous: are those roots or work items?), on `--parallel` outside
 * [1, 8] or non-integer, on `--agent` / `--epic` / `--assume-cleared` /
 * `--assume-cleared-and-merge` missing or malformed value, or on a positional
 * that is neither a ticket id nor a Linear URL (catches typo'd flags).
 * `--team <name>` is the tracker team for a repo whose config names
 * none — never a ticket ref; `team` is present in the result only when given.
 * The eight shared selection flags (`--label`, `--exclude-label`, `--assignee`, ...; see
 * selection.mjs) are filters, never ticket refs: they are returned as `selectionFlags` (the
 * parsed slot map) for `resolve-selection --stage epic`, present only when given, like `team`.
 */
export function parseEpicArgs(argv) {
  const ids = []
  const epicRefs = []
  const assumeCleared = []
  const assumeClearedAndMerge = []
  let parallel = 4
  let agent = 'claude'
  let team = null
  // Selection flags first, through the one shared parser, so `--exclude-label infra` is a filter
  // here exactly as it is at every gate. What remains is parsed below as before.
  const selectionParsed = parseSelectionFlags(argv)
  if (selectionParsed.error) throw new Error(`parseEpicArgs: ${selectionParsed.error}`)
  const selectionFlags = selectionParsed.flags
  argv = selectionParsed.positionals
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
    } else if (arg === '--team') {
      const raw = argv[(i += 1)]
      if (raw === undefined || raw.trim() === '' || raw.startsWith('--') || team !== null) {
        throw new Error(`parseEpicArgs: --team requires one non-empty team name, got ${raw}`)
      }
      team = raw.trim()
    } else if (arg === '--epic') {
      epicRefs.push(takeClearedRef(argv[(i += 1)], '--epic'))
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
  const rest = {
    parallel,
    agent,
    assumeCleared,
    assumeClearedAndMerge,
    ...(hasSelectionFlags(selectionFlags) ? { selectionFlags } : {}),
    ...(team === null ? {} : { team }),
  }
  if (epicRefs.length > 0) {
    if (ids.length > 0) {
      throw new Error(
        'parseEpicArgs: --epic cannot be combined with positional ticket refs — a positional ' +
          'means a work item (or, alone, a single parent), so the mix is ambiguous. Pass every ' +
          `root as its own --epic, got positionals: ${ids.join(', ')}`,
      )
    }
    return {
      mode: 'parents',
      parentId: null,
      parentIds: dedupeFirstSeen(epicRefs),
      ids: [],
      ...rest,
    }
  }
  if (ids.length === 0) {
    throw new Error('parseEpicArgs: at least one ticket id is required')
  }
  if (ids.length === 1) {
    return { mode: 'parent', parentId: ids[0], parentIds: [ids[0]], ids: [], ...rest }
  }
  return { mode: 'list', parentId: null, parentIds: [], ids, ...rest }
}

// First-seen-order deduplication over a list of already-canonical ids. The one
// helper both parseEpicArgs (roots) and buildCombinedRun (roots + children) use,
// so "ordered unique" means the same thing across the combined-run contract.
function dedupeFirstSeen(values) {
  const seen = new Set()
  const out = []
  for (const value of values) {
    if (seen.has(value)) continue
    seen.add(value)
    out.push(value)
  }
  return out
}

// A child entry may be a bare id, a normalized ticket (`{id}`), or a raw tracker
// payload (`{identifier}`) — the driver should not have to re-shape what it
// already holds. Anything else is a wiring error, not a ticket.
//
// Precedence is `identifier` FIRST, matching `normalizeTicket`. A raw tracker
// payload carries BOTH fields (`id` a UUID, `identifier` the human ticket ref),
// and the driver feeds those raw per-root lists to `buildCombinedRun` BEFORE
// normalizing them. Preferring `id` here would key `childIds`/`parentsByChild`
// by UUID while every normalized row, graph node and progress projection keys by
// identifier — so per-parent progress would silently omit those children and
// child-keyed reconciliation would diverge. The two resolvers must agree.
function childIdOf(entry, parentId) {
  const raw = typeof entry === 'string' ? entry : (entry?.identifier ?? entry?.id)
  if (typeof raw !== 'string' || raw.trim() === '') {
    throw new Error(
      `buildCombinedRun: childrenByParent[${parentId}] contains an entry with no usable child ` +
        `id (got ${JSON.stringify(entry) ?? String(entry)}). Pass an id string, a normalized ` +
        'ticket, or a raw issue payload.',
    )
  }
  return raw.trim()
}

/**
 * The plain-data COMBINED-RUN model: one coordinator's view of several epic
 * roots. Pure — no tracker I/O, no Map/Set — so it round-trips through
 * `JSON.stringify` and can be persisted for restart/reconciliation exactly as
 * returned.
 *
 * Given the requested `parentIds` (first-seen deduplicated here too, so a
 * caller that did not go through `parseEpicArgs` gets the same guarantee) and a
 * `childrenByParent` membership map, returns:
 *
 *   - `parentIds`        — ordered unique roots.
 *   - `childIds`         — the ORDERED UNIQUE child universe: every child once,
 *     in root order then per-root order. This is the set that is hydrated,
 *     classified, graphed, launched, reconciled and merged — once each, however
 *     many roots contain it.
 *   - `childrenByParent` — per-root ordered unique membership, one entry for
 *     EVERY requested root (a root with no enumerated children gets `[]`, which
 *     is a legitimate empty epic rather than an error).
 *   - `parentsByChild`   — the deterministic reverse index, in root order: which
 *     roots must show this child in their progress projection. A child under two
 *     roots appears under both, which is membership, NOT a dependency edge.
 *
 * Membership naming a root that was not requested throws: it is a driver wiring
 * error, and silently dropping it would quietly under-report a whole epic.
 *
 * @param {{parentIds: string[], childrenByParent: Record<string, (string|{id?: string, identifier?: string})[]>}} args
 * @returns {{parentIds: string[], childIds: string[], childrenByParent: Record<string, string[]>, parentsByChild: Record<string, string[]>}}
 */
export function buildCombinedRun({ parentIds, childrenByParent } = {}) {
  if (!Array.isArray(parentIds) || parentIds.length === 0) {
    throw new Error('buildCombinedRun: parentIds must be a non-empty array of root ticket ids')
  }
  for (const parentId of parentIds) {
    if (typeof parentId !== 'string' || parentId.trim() === '') {
      throw new Error(
        `buildCombinedRun: parentIds contains a blank/non-string root id (${String(parentId)})`,
      )
    }
  }
  const roots = dedupeFirstSeen(parentIds.map((id) => id.trim()))
  const membership = childrenByParent ?? {}
  if (typeof membership !== 'object' || Array.isArray(membership)) {
    throw new Error('buildCombinedRun: childrenByParent must be an object keyed by root ticket id')
  }
  const rootSet = new Set(roots)
  for (const key of Object.keys(membership)) {
    if (!rootSet.has(key)) {
      throw new Error(
        `buildCombinedRun: childrenByParent names ${key}, which is not among the requested ` +
          `roots (${roots.join(', ')}). Dropping it would silently under-report an epic.`,
      )
    }
  }

  const resolved = {}
  const parentsByChild = {}
  const childIds = []
  const seenChild = new Set()
  for (const parentId of roots) {
    const entries = membership[parentId] ?? []
    if (!Array.isArray(entries)) {
      throw new Error(`buildCombinedRun: childrenByParent[${parentId}] must be an array`)
    }
    const perParent = dedupeFirstSeen(entries.map((entry) => childIdOf(entry, parentId)))
    resolved[parentId] = perParent
    for (const childId of perParent) {
      if (!seenChild.has(childId)) {
        seenChild.add(childId)
        childIds.push(childId)
        parentsByChild[childId] = []
      }
      parentsByChild[childId].push(parentId)
    }
  }

  return { parentIds: roots, childIds, childrenByParent: resolved, parentsByChild }
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

export const CHILD_LIVENESS_VERDICTS = new Set([
  'alive',
  'environmental-death',
  'agent-blocked',
  'wall-clock-expired',
  'unknown',
])

export const CHILD_LIVENESS_ACTIONS = new Set([
  'hold',
  'resume',
  'repair',
  'fail-isolate',
  'investigate',
])

const CHILD_ALIVE_CHAT_STATUSES = new Set(['waiting', 'working', 'question'])
const CHILD_USAGE_LIMIT_CHAT_STATUSES = new Set(['limited'])
const CHILD_UNKNOWN_CHAT_STATUSES = new Set(['unspecified'])
const AGENT_STALLED_ATTENTION_REASONS = new Set(['agent-stalled', 'attention-reason-agent-stalled'])

const CHAT_STATUS_NUMBERS = new Map([
  [0, 'unspecified'],
  [1, 'working'],
  [2, 'idle'],
  [3, 'stopped'],
  [4, 'question'],
  [5, 'limited'],
  [6, 'waiting'],
])

const SESSION_STATE_NUMBERS = new Map([[10, 'blocked']])

function normalizeClassifierToken(value) {
  return typeof value === 'string'
    ? value
        .trim()
        .toLowerCase()
        .replace(/[_\s]+/g, '-')
    : null
}

function normalizeEnumToken(value, { numericMap, prefixes }) {
  if (typeof value === 'number' && Number.isInteger(value)) {
    return numericMap.get(value) ?? null
  }
  const token = normalizeClassifierToken(value)
  if (!token) return null
  if (/^\d+$/.test(token)) return numericMap.get(Number(token)) ?? null
  for (const prefix of prefixes) {
    if (token.startsWith(prefix)) return token.slice(prefix.length)
  }
  return token
}

function normalizeChatStatusToken(value) {
  return normalizeEnumToken(value, {
    numericMap: CHAT_STATUS_NUMBERS,
    prefixes: ['chat-status-'],
  })
}

function normalizeSessionStateToken(value) {
  return normalizeEnumToken(value, {
    numericMap: SESSION_STATE_NUMBERS,
    prefixes: ['session-state-'],
  })
}

function tokenListHas(values, wanted) {
  const list = Array.isArray(values)
    ? values
    : values === undefined || values === null
      ? []
      : [values]
  return list.some((value) => wanted.has(normalizeClassifierToken(value)))
}

function childLiveness(verdict, action, reasons) {
  return { verdict, action, reasons }
}

/**
 * Classifies whether a boss-epic child is alive, environmentally dead and
 * resumable, agent-blocked and repairable, wall-clock expired, or unknown.
 *
 * The caller reads the session/chat state and supplies already-classified plain
 * data. This helper does no I/O, reads no clock, parses no transcript text, and
 * mutates nothing. Missing evidence fails toward `unknown`/`investigate`, never
 * toward repair.
 *
 * Inputs are intentionally driver-shaped:
 *   - `chatStatus` — tracked chat status (`WAITING`, `CHAT_STATUS_WAITING`,
 *     numeric enum `6`, ...)
 *   - `chatStatusReadable` / `chatStatusUnreadable` — explicit unreadable status
 *   - `sessionState` — aggregate session state (`BLOCKED`,
 *     `SESSION_STATE_BLOCKED`, numeric enum `10`, ...)
 *   - `lastMessageKind` / `lastMessageClass` — caller-classified last message:
 *     `agent-conclusion`, `usage-limit`, `transient-api-error`, or absent
 *   - `lastMessageIsAgentConclusion`, `lastMessageIsUsageLimit`,
 *     `lastMessageIsTransientApiError` — boolean aliases for the same classes
 *   - `headShaMoved`, `activityStale`, `attentionReasons` — corroborating-only
 *     liveness proxies used for reasons, never as deciding death evidence
 *   - `wallClockExceeded` — explicit epic budget expiry
 *   - `displayLabel` — the session's composite display label; a Ready label
 *     (see `isReadyDisplayLabel`) means the work is already on the head
 *
 * Rules, in order:
 *   1. wall-clock expiry                       → wall-clock-expired/fail-isolate
 *   2. Ready session, IDLE/STOPPED/WAITING chat → alive/hold (never resume/repair);
 *      Ready + LIMITED/UNSPECIFIED falls through, since it can never settle
 *   3. WAITING/WORKING/QUESTION chat           → alive/hold
 *   4. LIMITED or usage-cap last message       → environmental-death/resume
 *   5. transient API/5xx last message          → environmental-death/resume
 *   6. BLOCKED + agent-conclusion last message → agent-blocked/repair
 *   7. unreadable/UNSPECIFIED/anything else    → unknown/investigate
 */
export function classifyChildLiveness({
  chatStatus,
  chatStatusReadable = true,
  chatStatusUnreadable = false,
  sessionState,
  lastMessageKind,
  lastMessageClass,
  lastMessageIsAgentConclusion = false,
  lastMessageIsUsageLimit = false,
  lastMessageIsTransientApiError = false,
  headShaMoved,
  activityStale,
  attentionReasons = [],
  wallClockExceeded = false,
  displayLabel,
} = {}) {
  const reasons = []
  const status = normalizeChatStatusToken(chatStatus)
  const ready = isReadyDisplayLabel(displayLabel)
  const statusUnreadable = chatStatusUnreadable || chatStatusReadable === false
  const state = normalizeSessionStateToken(sessionState)
  const messageKinds = [lastMessageKind, lastMessageClass]
  const messageIsAgentConclusion =
    lastMessageIsAgentConclusion || tokenListHas(messageKinds, new Set(['agent-conclusion']))
  const messageIsUsageLimit =
    lastMessageIsUsageLimit || tokenListHas(messageKinds, new Set(['usage-limit', 'usage-cap']))
  const messageIsTransientApi =
    lastMessageIsTransientApiError ||
    tokenListHas(messageKinds, new Set(['transient-api-error', 'transient-api', 'api-5xx']))

  if (headShaMoved === false) reasons.push('head-sha-unmoved')
  if (activityStale === true) reasons.push('activity-stale')
  if (tokenListHas(attentionReasons, AGENT_STALLED_ATTENTION_REASONS)) {
    reasons.push('attention-agent-stalled')
  }
  if (statusUnreadable) reasons.push('chat-status-unreadable')
  if (status) reasons.push(`chat-status:${status}`)
  if (ready) reasons.push('session-ready')
  if (state) reasons.push(`session-state:${state}`)
  if (messageIsUsageLimit) reasons.push('last-message:usage-limit')
  if (messageIsTransientApi) reasons.push('last-message:transient-api-error')
  if (messageIsAgentConclusion) reasons.push('last-message:agent-conclusion')

  if (wallClockExceeded) {
    reasons.push('wall-clock-exceeded')
    return childLiveness('wall-clock-expired', 'fail-isolate', reasons)
  }
  if (ready && !statusUnreadable && CHILD_READY_PARKED_CHAT_STATUSES.has(status)) {
    return childLiveness('alive', 'hold', reasons)
  }
  if (CHILD_ALIVE_CHAT_STATUSES.has(status)) return childLiveness('alive', 'hold', reasons)
  if (CHILD_USAGE_LIMIT_CHAT_STATUSES.has(status) || messageIsUsageLimit) {
    return childLiveness('environmental-death', 'resume', reasons)
  }
  if (messageIsTransientApi) return childLiveness('environmental-death', 'resume', reasons)
  if (state === 'blocked' && messageIsAgentConclusion) {
    return childLiveness('agent-blocked', 'repair', reasons)
  }
  if (CHILD_UNKNOWN_CHAT_STATUSES.has(status) || statusUnreadable) {
    return childLiveness('unknown', 'investigate', reasons)
  }
  return childLiveness('unknown', 'investigate', reasons)
}

// A leading run of glyphs, whitespace, or JSON-escaped glyphs (`\u2713`) is
// stripped before the word is compared, so a transport that escapes the check
// mark still reads Ready.
const DISPLAY_LABEL_LEADING_GLYPHS = /^(?:\\u[0-9a-f]{4}|[^\p{L}\p{N}])+/iu

/**
 * True iff a session's composite display label is the daemon's computed Ready
 * label (`✓ ready`): the current head carries a successful build receipt, the
 * PR is green, and no live, failing, conflicting, verifying or needs-human
 * state outranks it. Matches the word, not the glyph; `ready to merge` and
 * `not ready` do not match. Pure.
 */
export function isReadyDisplayLabel(label) {
  if (typeof label !== 'string') return false
  return label.replace(DISPLAY_LABEL_LEADING_GLYPHS, '').trim().toLowerCase() === 'ready'
}

const CHILD_IDLE_CHAT_STATUSES = new Set(['idle', 'stopped'])
const CHILD_READY_PARKED_CHAT_STATUSES = new Set(['idle', 'stopped', 'waiting'])

function priorSettleObservation(previous) {
  if (!previous || typeof previous !== 'object' || Array.isArray(previous)) return null
  return {
    idleEligible: previous.idleEligible === true,
    readyEligible: previous.readyEligible === true,
  }
}

/**
 * Classifies whether a boss-epic child's tracked chat has settled — the green
 * admission condition the epic driver owns. Two consecutive polls must agree:
 * the caller persists `observation` and passes it back as `previous` on the
 * next poll.
 *
 * Inputs: `chatStatus` (any spelling `classifyChildLiveness` accepts),
 * `chatStatusReadable`, `spinnerPresent` (the tracked chat's
 * `spinner_present`), `displayLabel` (the session's `display_label`), and
 * `previous` (last poll's `observation`, or absent).
 *
 * Readings, in order:
 *   1. `settled-idle`  — IDLE/STOPPED, readable, no spinner, on both polls
 *   2. `settled-ready` — Ready label and IDLE/STOPPED/WAITING, readable, no
 *                        spinner, on both polls (Ready supersedes waiting)
 *   3. `pending`       — eligible this poll, previous absent or disagreeing
 *   4. `alive`         — WORKING/QUESTION, WAITING without Ready, or a spinner
 *   5. `limited`       — LIMITED
 *   6. `unknown`       — UNSPECIFIED, unreadable, or anything else
 *
 * Only the two settled readings set `settled`. Missing evidence never settles.
 * Ready never bypasses the other green-admission conditions. Pure.
 */
export function classifyChildSettled({
  chatStatus,
  chatStatusReadable = true,
  spinnerPresent,
  displayLabel,
  previous,
} = {}) {
  const reasons = []
  const status = normalizeChatStatusToken(chatStatus)
  const readable = chatStatusReadable !== false && status !== null
  const spinner = spinnerPresent === true
  const ready = isReadyDisplayLabel(displayLabel)
  const prior = priorSettleObservation(previous)

  if (!readable) reasons.push('chat-status-unreadable')
  if (status) reasons.push(`chat-status:${status}`)
  if (spinner) reasons.push('spinner')
  if (ready) reasons.push('session-ready')
  if (!prior) reasons.push('first-poll')

  const idleEligible = readable && !spinner && CHILD_IDLE_CHAT_STATUSES.has(status)
  const readyEligible =
    readable && !spinner && ready && CHILD_READY_PARKED_CHAT_STATUSES.has(status)
  const observation = { idleEligible, readyEligible }
  const verdict = (reading) => ({
    settled: reading === 'settled-idle' || reading === 'settled-ready',
    reading,
    reasons,
    observation,
  })

  if (idleEligible && prior?.idleEligible) return verdict('settled-idle')
  if (readyEligible && prior?.readyEligible) return verdict('settled-ready')
  if (idleEligible || readyEligible) return verdict('pending')
  if (!readable) return verdict('unknown')
  if (spinner || CHILD_ALIVE_CHAT_STATUSES.has(status)) return verdict('alive')
  if (CHILD_USAGE_LIMIT_CHAT_STATUSES.has(status)) return verdict('limited')
  return verdict('unknown')
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
