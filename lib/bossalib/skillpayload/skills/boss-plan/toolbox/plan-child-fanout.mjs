#!/usr/bin/env node
// skills-toolbox/plan-child-fanout.mjs
//
// The deterministic core of boss-plan's CHILD FAN-OUT route: `/boss-plan <PARENT>` on an issue
// whose sub-issues carry the planning-queue label plans each of those children with its own
// awaited child-planner dispatch, at most `--parallel` at a time, instead of planning the
// container as one ticket. Every decision that can be computed is computed here; the skill prose
// only does I/O (tracker reads, dispatches, holds) and feeds this module files.
//
//   parsePlanArgs        `/boss-plan [<ID|Linear URL>] [--parallel N] [--team T] [selection flags]`
//   routeSelectedIssue   single | fan-out | epic-resume | epic-noop | abort, for the selected issue
//   initFanoutLedger     eligible children, their planning order, and why every other one is skipped
//   nextFanoutStep       which children to launch into free slots and which in-flight one to poll
//   recordChildOutcome   the only legal status transitions (queued → dispatched → planned | failed)
//   fanoutSummary        the per-child report rows
//   parentFlipDecision   the one parent write, computed only when every eligible child planned
//
// The ledger file is the run's only fan-out state, and `next` / `record` are its only mutators,
// each writing it atomically (write-then-rename) so a crash mid-write never leaves half a ledger.
//
// PROJECT-AGNOSTIC BY CONSTRUCTION. This module ships inside the published `boss-plan` core. Every
// label and state name resolves through `labelName` / `stateName` against the caller's config; no
// tracker, team, label or MCP server of any one repo is named here.

import { readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs'
import { basename, dirname, join } from 'node:path'

import { BLOCKER_CLEARED_STATE_TYPES, normalizeTicket, parseTicketRef } from './bs-epic-lib.mjs'
import { buildGraph, readyTickets } from './dag-scheduler.mjs'
import { isMainModule } from './main-module.mjs'
import { detectEpicParent, readLabels, readState } from './plan-epic-phase25.mjs'
import { planIdempotencePrecheck } from './plan-run-guards.mjs'
import { hasSelectionFlags, parseSelectionFlags } from './selection.mjs'
import { labelName, loadSkillConfig, stateName } from './skill-config.mjs'

/** Same bounds and default as boss-epic's `--parallel` (`parseEpicArgs`). */
export const PARALLEL_DEFAULT = 4
export const PARALLEL_MIN = 1
export const PARALLEL_MAX = 8

/** A child dispatch that dies on transport is retried once; the second death fails it. */
export const MAX_DISPATCH_ATTEMPTS = 2

export const SKIP_REASONS = Object.freeze({
  archived: 'archived',
  done: 'done',
  alreadyPlanned: 'already-planned',
  noAgentPlanLabel: 'no-agent-plan-label',
  needsHuman: 'needs-human',
})

export const CHILD_STATUSES = Object.freeze(['queued', 'dispatched', 'planned', 'failed'])

// A label carried by some trackers' agent-readiness workflows that is not a pipeline role in the
// skill config. It is stripped from the parent alongside the role labels because a parent that
// still says "agent-friendly" invites a build run onto a container. Removal only: a repo that never
// uses it is unaffected.
const EXTRA_PARENT_STRIP_LABELS = Object.freeze(['agent-friendly'])

const isPlainObject = (value) =>
  typeof value === 'object' && value !== null && !Array.isArray(value)

// Label and state names compare the way a person reads them — case, spacing, `-` and `_` do not
// make `Agent-Plan` a different label from `agent_plan` (the `classifyTickets` normalisation).
const nameKey = (value) =>
  String(value ?? '')
    .toLowerCase()
    .replace(/[\s_-]+/g, '')

const hasLabel = (issue, name) =>
  readLabels(issue).some((label) => nameKey(label) === nameKey(name))

const fail = (message) => {
  throw new Error(`plan-child-fanout: ${message}`)
}

// --- args --------------------------------------------------------------------------------------

function parseParallel(raw) {
  const n = typeof raw === 'string' && raw.trim() !== '' ? Number(raw) : Number.NaN
  if (!Number.isInteger(n) || n < PARALLEL_MIN || n > PARALLEL_MAX) {
    throw new Error(
      `parsePlanArgs: --parallel must be an integer between ${PARALLEL_MIN} and ${PARALLEL_MAX}, got ${raw}`,
    )
  }
  return n
}

/**
 * Parse boss-plan's own arguments. At most one positional, a bare ticket id or a pasted Linear
 * issue URL (`parseTicketRef`); none means the unplanned sweep. `--parallel N` is an integer in
 * [1, 8], default 4. `--team <name>` and the shared selection flags are passed through, so this
 * never rejects an argument boss-plan already accepted. Throws on an out-of-range or non-integer
 * `--parallel`, an unknown flag, a second positional, or a positional that is not a ticket ref.
 *
 * @param {string[]} argv
 * @returns {{mode: 'named'|'sweep', ticketId: string|null, parallel: number, team?: string,
 *            selectionFlags?: object}}
 */
export function parsePlanArgs(argv = []) {
  const selection = parseSelectionFlags(Array.isArray(argv) ? argv : [])
  if (selection.error) throw new Error(`parsePlanArgs: ${selection.error}`)
  const rest = selection.positionals
  let ticketId = null
  let parallel = PARALLEL_DEFAULT
  let team = null
  for (let i = 0; i < rest.length; i += 1) {
    const arg = String(rest[i])
    if (arg === '--parallel') {
      parallel = parseParallel(rest[(i += 1)])
    } else if (arg.startsWith('--parallel=')) {
      parallel = parseParallel(arg.slice('--parallel='.length))
    } else if (arg === '--team') {
      const raw = rest[(i += 1)]
      if (typeof raw !== 'string' || raw.trim() === '' || raw.startsWith('--') || team !== null) {
        throw new Error(`parsePlanArgs: --team requires one non-empty team name, got ${raw}`)
      }
      team = raw.trim()
    } else if (arg.startsWith('--')) {
      throw new Error(`parsePlanArgs: unknown flag ${arg}`)
    } else {
      const ref = parseTicketRef(arg)
      if (!ref) throw new Error(`parsePlanArgs: not a ticket id or Linear URL: ${arg}`)
      if (ticketId !== null) {
        throw new Error(
          `parsePlanArgs: at most one ticket may be named, got ${ticketId} and ${ref}`,
        )
      }
      ticketId = ref
    }
  }
  return {
    mode: ticketId === null ? 'sweep' : 'named',
    ticketId,
    parallel,
    ...(team === null ? {} : { team }),
    ...(hasSelectionFlags(selection.flags) ? { selectionFlags: selection.flags } : {}),
  }
}

// --- route -------------------------------------------------------------------------------------

function assertBundle(bundle) {
  if (!isPlainObject(bundle)) fail('the route bundle must be an object {parent, children}')
  if (!isPlainObject(bundle.parent)) fail('the route bundle carries no parent issue object')
  if (!Array.isArray(bundle.children)) fail('the route bundle children must be an array')
  const parentId = normalizeTicket(bundle.parent).id
  if (typeof parentId !== 'string' || parentId.trim() === '') {
    fail('the parent issue carries neither an identifier nor an id')
  }
  bundle.children.forEach((child, index) => {
    if (!isPlainObject(child)) fail(`child #${index + 1} is not an issue object`)
    const id = normalizeTicket(child).id
    if (typeof id !== 'string' || id.trim() === '') {
      fail(`child #${index + 1} carries neither an identifier nor an id`)
    }
  })
}

const isArchived = (issue) => issue.archivedAt !== undefined && issue.archivedAt !== null

// The tracker UUID, which `read-description --id` takes. MCP payloads carry it as `uuid` beside a
// human `id`; raw GraphQL carries it as `id` beside an `identifier`. Null when neither shape says.
const uuidOf = (issue) => {
  if (typeof issue.uuid === 'string' && issue.uuid !== '') return issue.uuid
  if (typeof issue.identifier === 'string' && typeof issue.id === 'string') return issue.id
  return null
}

/**
 * Decide how boss-plan treats the issue it selected. Precedence:
 *   `epic-resume` — a boss-plan-built epic parent (`detectEpicParent`); `abort` when ambiguous.
 *   `fan-out`     — at least one non-archived child carries the planning-queue label.
 *   `epic-noop`   — the issue carries the epic label and has children, none labelled: nothing to
 *                   plan, and planning the container as one ticket would be wrong.
 *   `single`      — everything else; today's single-ticket path.
 *
 * @param {{parent: object, children: object[], config: object}} input
 * @returns {{verdict: 'single'|'fan-out'|'epic-resume'|'epic-noop'|'abort', parentId: string,
 *            childCount: number, labelledCount: number, reasons: string[]}}
 */
export function routeSelectedIssue({ parent, children, config } = {}) {
  assertBundle({ parent, children })
  const parentId = normalizeTicket(parent).id
  const agentPlan = labelName(config, 'agentPlan')
  const epicLabel = labelName(config, 'epic')
  const live = children.filter((child) => !isArchived(child))
  const labelled = live.filter((child) => hasLabel(child, agentPlan))
  const base = { parentId, childCount: live.length, labelledCount: labelled.length }

  const epic = detectEpicParent(parent)
  if (epic.isEpicParent) {
    return {
      ...base,
      verdict: epic.ambiguous ? 'abort' : 'epic-resume',
      reasons: epic.reasons,
    }
  }
  if (labelled.length > 0) {
    return {
      ...base,
      verdict: 'fan-out',
      reasons: [`${labelled.length} of ${live.length} child(ren) carry "${agentPlan}"`],
    }
  }
  if (live.length > 0 && hasLabel(parent, epicLabel)) {
    return {
      ...base,
      verdict: 'epic-noop',
      reasons: [
        `the issue carries "${epicLabel}" and has ${live.length} child(ren), none labelled "${agentPlan}" — nothing to plan; label children "${agentPlan}"`,
      ],
    }
  }
  return {
    ...base,
    verdict: 'single',
    reasons: [
      live.length === 0
        ? 'the issue has no live children'
        : `none of its ${live.length} child(ren) carry "${agentPlan}" and it carries no "${epicLabel}" label`,
    ],
  }
}

// --- init --------------------------------------------------------------------------------------

// Urgent first, None last: 1 > 2 > 3 > 4 > 0 (the dag-scheduler total order).
const PRIORITY_ORDER = [1, 2, 3, 4, 0]
const priorityRank = (priority) => {
  const index = PRIORITY_ORDER.indexOf(priority)
  return index === -1 ? PRIORITY_ORDER.length : index
}
const createdMs = (value) => {
  const ms = new Date(value).getTime()
  return Number.isNaN(ms) ? Number.POSITIVE_INFINITY : ms
}
function byPriorityOldestId(a, b) {
  const rank = priorityRank(a.priority) - priorityRank(b.priority)
  if (rank !== 0) return rank
  const left = createdMs(a.createdAt)
  const right = createdMs(b.createdAt)
  if (left !== right) return left < right ? -1 : 1
  if (a.id === b.id) return 0
  return String(a.id) < String(b.id) ? -1 : 1
}

/**
 * Topological order over the existing intra-sibling `blockedBy` edges (a blocker before its
 * dependent), ties broken by priority, oldest `createdAt`, then id. The ready set comes from the
 * shared `readyTickets`; the pick within it is by priority rather than by unlock count, because
 * planning has no merge critical path to shorten. A cycle does not abort: the whole set falls back
 * to priority order with a warning naming the ids caught in it.
 */
function planningOrder(tickets) {
  const nodes = tickets.map(({ id, blockedBy, priority, createdAt }) => ({
    id,
    blockedBy: Array.isArray(blockedBy) ? blockedBy : [],
    priority,
    createdAt,
  }))
  const graph = buildGraph(nodes)
  // Blockers outside the eligible set (done, skipped or foreign tickets) never gate planning.
  const externallyCleared = new Set([...graph.externalBlockers.values()].flat())
  const placed = new Set()
  const order = []
  while (order.length < nodes.length) {
    const ready = readyTickets(graph, {
      merged: placed,
      failed: new Set(),
      inFlight: new Set(),
      externallyCleared,
    })
    if (ready.length === 0) {
      const stuck = nodes.filter((node) => !placed.has(node.id)).map((node) => node.id)
      return {
        order: [...nodes].sort(byPriorityOldestId).map((node) => node.id),
        warnings: [
          `cycle among existing blockedBy edges between ${stuck.join(', ')} — fell back to priority order`,
        ],
      }
    }
    const next = [...ready].sort(byPriorityOldestId)[0]
    placed.add(next.id)
    order.push(next.id)
  }
  return { order, warnings: [] }
}

function isClosed(ticket, config) {
  if (BLOCKER_CLEARED_STATE_TYPES.has(ticket.stateType)) return true
  let done = null
  try {
    done = stateName(config, 'done')
  } catch {
    done = null
  }
  return done !== null && ticket.stateName !== null && nameKey(ticket.stateName) === nameKey(done)
}

/**
 * Build the fan-out ledger from the hydrated route bundle. Eligible = carries the planning-queue
 * label, not closed (state type completed/canceled, or the configured done state), not
 * `needs-human` (the unplanned sweep's own exclusion) and not already planned
 * (`planIdempotencePrecheck` → `noop`). Every other child lands in `skipped` with its reason.
 *
 * @param {{parent: object, children: object[], config: object, parallel?: number}} input
 * @returns {object} the ledger
 */
export function initFanoutLedger({ parent, children, config, parallel = PARALLEL_DEFAULT } = {}) {
  assertBundle({ parent, children })
  if (!Number.isInteger(parallel) || parallel < PARALLEL_MIN || parallel > PARALLEL_MAX) {
    fail(`parallel must be an integer between ${PARALLEL_MIN} and ${PARALLEL_MAX}, got ${parallel}`)
  }
  const parentTicket = normalizeTicket(parent)
  const agentPlan = labelName(config, 'agentPlan')
  const needsHuman = labelName(config, 'needsHuman')
  const skipped = []
  const eligible = []
  const siblingIds = []

  for (const child of children) {
    const ticket = normalizeTicket(child)
    const row = { id: ticket.id, title: ticket.title ?? '' }
    if (isArchived(child)) {
      skipped.push({ ...row, reason: SKIP_REASONS.archived })
      continue
    }
    siblingIds.push(ticket.id)
    if (isClosed(ticket, config)) {
      skipped.push({ ...row, reason: SKIP_REASONS.done })
    } else if (
      planIdempotencePrecheck({ issue: child, selectedID: ticket.id, config }).action === 'noop'
    ) {
      skipped.push({ ...row, reason: SKIP_REASONS.alreadyPlanned })
    } else if (!hasLabel(child, agentPlan)) {
      skipped.push({ ...row, reason: SKIP_REASONS.noAgentPlanLabel })
    } else if (hasLabel(child, needsHuman)) {
      skipped.push({ ...row, reason: SKIP_REASONS.needsHuman })
    } else {
      eligible.push({ ...ticket, uuid: uuidOf(child) })
    }
  }

  const { order, warnings } = planningOrder(eligible)
  const byId = new Map(eligible.map((ticket) => [ticket.id, ticket]))
  const entries = {}
  order.forEach((id, index) => {
    const ticket = byId.get(id)
    entries[id] = {
      title: ticket.title ?? '',
      uuid: ticket.uuid,
      status: 'queued',
      earlierSiblings: order.slice(0, index),
      // Already-planned siblings run no concurrent planner, so an edge toward one cannot race.
      plannedSiblings: skipped
        .filter((row) => row.reason === SKIP_REASONS.alreadyPlanned)
        .map((row) => row.id),
      scratch: null,
      dispatchedAt: null,
      lastPolledAt: null,
      attempts: 0,
      reason: null,
    }
  })

  return {
    version: 1,
    parentId: parentTicket.id,
    parentUuid: uuidOf(parent),
    parentTitle: parentTicket.title ?? '',
    parallel,
    order,
    siblingIds,
    children: entries,
    skipped,
    warnings,
  }
}

// --- next / record -----------------------------------------------------------------------------

function assertLedger(ledger) {
  if (!isPlainObject(ledger)) fail('the ledger must be an object')
  if (!Array.isArray(ledger.order) || !isPlainObject(ledger.children)) {
    fail('the ledger carries no order/children — was it written by `init`?')
  }
  if (!Number.isInteger(ledger.parallel) || ledger.parallel < PARALLEL_MIN) {
    fail(`the ledger parallel is not a positive integer: ${ledger.parallel}`)
  }
  for (const id of ledger.order) {
    const child = ledger.children[id]
    if (!isPlainObject(child) || !CHILD_STATUSES.includes(child.status)) {
      fail(`ledger child ${id} has no valid status`)
    }
  }
}

const idsWithStatus = (ledger, status) =>
  ledger.order.filter((id) => ledger.children[id].status === status)

/**
 * One scheduling step. `launch` fills the free slots (`parallel` minus in-flight) with queued
 * children in planning order — the caller dispatches each and `record`s it `dispatched`. `poll` is
 * the in-flight child polled longest ago (never-polled first, then oldest dispatch, then planning
 * order); its `lastPolledAt` is stamped, so successive steps round-robin across in-flight ids.
 * `complete` is true only when nothing is queued or in flight.
 *
 * Pure: returns the stepped ledger rather than mutating the input.
 *
 * @param {object} ledger
 * @param {{now?: number}} [options]
 * @returns {{ledger: object, step: {launch: string[], poll: string|null, complete: boolean,
 *            inFlight: number, queued: number}}}
 */
export function nextFanoutStep(ledger, { now = Date.now() } = {}) {
  assertLedger(ledger)
  const next = structuredClone(ledger)
  const dispatched = idsWithStatus(next, 'dispatched')
  const queued = idsWithStatus(next, 'queued')
  const free = Math.max(0, next.parallel - dispatched.length)
  const launch = queued.slice(0, free)
  const position = new Map(next.order.map((id, index) => [id, index]))
  const pollKey = (id) => {
    const child = next.children[id]
    return [
      child.lastPolledAt ?? Number.NEGATIVE_INFINITY,
      child.dispatchedAt ?? Number.NEGATIVE_INFINITY,
      position.get(id),
    ]
  }
  const poll =
    [...dispatched].sort((a, b) => {
      const left = pollKey(a)
      const right = pollKey(b)
      for (let i = 0; i < left.length; i += 1) {
        if (left[i] !== right[i]) return left[i] < right[i] ? -1 : 1
      }
      return 0
    })[0] ?? null
  if (poll !== null) next.children[poll].lastPolledAt = now
  return {
    ledger: next,
    step: {
      launch,
      poll,
      complete: dispatched.length === 0 && queued.length === 0,
      inFlight: dispatched.length,
      queued: queued.length,
    },
  }
}

/**
 * Apply one child outcome. Legal transitions only:
 *   queued     → dispatched            (`attempts` + 1, `dispatchedAt` stamped)
 *   dispatched → planned               (the caller verified `idempotence` → `noop`)
 *   dispatched → failed                (any reason; reason `transport` on a first attempt instead
 *                                       re-queues the child, keeping its scratch, for one retry)
 *   queued     → failed                (a pre-dispatch failure, e.g. an unreadable snapshot)
 * Anything else — an unknown child, a terminal child, `planned → dispatched` — throws.
 *
 * @param {object} ledger
 * @param {string} childId
 * @param {'dispatched'|'planned'|'failed'} outcome
 * @param {{reason?: string|null, scratch?: string|null, now?: number}} [options]
 * @returns {{ledger: object, child: object}}
 */
export function recordChildOutcome(
  ledger,
  childId,
  outcome,
  { reason = null, scratch = null, now = Date.now() } = {},
) {
  assertLedger(ledger)
  const next = structuredClone(ledger)
  const child = next.children[childId]
  if (!isPlainObject(child)) {
    const skipped = (next.skipped ?? []).find((row) => row?.id === childId)
    fail(
      skipped
        ? `child ${childId} was skipped (${skipped.reason}) and cannot be recorded`
        : `child ${childId} is not in this ledger`,
    )
  }
  const from = child.status
  const illegal = () => fail(`illegal transition for ${childId}: ${from} → ${outcome}`)

  if (outcome === 'dispatched') {
    if (from !== 'queued') illegal()
    child.status = 'dispatched'
    child.attempts += 1
    child.dispatchedAt = now
    child.lastPolledAt = null
    child.scratch = scratch ?? child.scratch
    child.reason = null
  } else if (outcome === 'planned') {
    if (from !== 'dispatched') illegal()
    child.status = 'planned'
    child.reason = reason
  } else if (outcome === 'failed') {
    if (from !== 'dispatched' && from !== 'queued') illegal()
    const retry =
      from === 'dispatched' && reason === 'transport' && child.attempts < MAX_DISPATCH_ATTEMPTS
    child.status = retry ? 'queued' : 'failed'
    child.reason = retry ? 'transport (retry pending)' : (reason ?? 'unspecified')
  } else {
    fail(`unknown outcome ${outcome} — expected dispatched, planned or failed`)
  }
  return { ledger: next, child: { id: childId, ...child } }
}

// --- summary / parent flip ---------------------------------------------------------------------

/**
 * The per-child report: every ledger child in planning order, then every skipped child.
 * `ok` is true only when the fan-out is complete and nothing failed.
 */
export function fanoutSummary(ledger) {
  assertLedger(ledger)
  const counts = { planned: 0, failed: 0, queued: 0, dispatched: 0, skipped: 0 }
  const rows = []
  for (const id of ledger.order) {
    const child = ledger.children[id]
    counts[child.status] += 1
    const outcome =
      child.status === 'failed' ? `failed: ${child.reason ?? 'unspecified'}` : child.status
    rows.push({ id, title: child.title ?? '', outcome })
  }
  for (const row of Array.isArray(ledger.skipped) ? ledger.skipped : []) {
    counts.skipped += 1
    rows.push({ id: row.id, title: row.title ?? '', outcome: `skipped: ${row.reason}` })
  }
  const complete = counts.queued === 0 && counts.dispatched === 0
  return {
    parentId: ledger.parentId,
    complete,
    ok: complete && counts.failed === 0,
    counts,
    rows,
  }
}

/**
 * The single parent write, last, and only on full success: labels = existing ∪ {epic} minus the
 * planning-queue, build-exposure and needs-human labels (read-merge-write — `save_issue` `labels`
 * replaces the set), and the planned state only when the parent sits in the unplanned state. The
 * description, estimate and attachments are never part of it. Any queued, in-flight or failed
 * child ⇒ `flip: false`, as does a run where no child was ever planned (every labelled child
 * skipped as needs-human or archived); already-planned children count, so a resume still flips.
 *
 * @param {{ledger: object, parent: object, config: object}} input
 * @returns {{flip: boolean, write?: boolean, id?: string, labels?: string[], state?: string|null,
 *            added?: string[], removed?: string[], reasons: string[]}}
 */
export function parentFlipDecision({ ledger, parent, config } = {}) {
  const summary = fanoutSummary(ledger)
  if (!isPlainObject(parent)) fail('the parent payload must be an issue object')
  const parentIds = [parent.identifier, parent.id, parent.uuid].filter(
    (value) => typeof value === 'string',
  )
  if (!parentIds.includes(ledger.parentId)) {
    fail(
      `the parent payload (${parentIds.join(' / ')}) is not the ledger parent ${ledger.parentId}`,
    )
  }
  if (!summary.complete) {
    return {
      flip: false,
      reasons: [
        `${summary.counts.queued + summary.counts.dispatched} child(ren) still queued or in flight`,
      ],
    }
  }
  if (summary.counts.failed > 0) {
    const failed = ledger.order.filter((id) => ledger.children[id].status === 'failed')
    return {
      flip: false,
      reasons: [
        `${failed.length} child(ren) failed (${failed.join(', ')}) — the parent is untouched`,
      ],
    }
  }

  const alreadyPlanned = (Array.isArray(ledger.skipped) ? ledger.skipped : []).filter(
    (row) => row?.reason === SKIP_REASONS.alreadyPlanned,
  ).length
  if (summary.counts.planned === 0 && alreadyPlanned === 0) {
    return {
      flip: false,
      reasons: ['no child was planned this run or earlier — the parent is untouched'],
    }
  }

  const epicLabel = labelName(config, 'epic')
  const strip = [
    labelName(config, 'agentPlan'),
    labelName(config, 'agentBuild'),
    labelName(config, 'needsHuman'),
    ...EXTRA_PARENT_STRIP_LABELS,
  ]
  const stripKeys = new Set(strip.map(nameKey))
  const current = readLabels(parent)
  const removed = current.filter((label) => stripKeys.has(nameKey(label)))
  const labels = current.filter((label) => !stripKeys.has(nameKey(label)))
  const added = []
  if (!labels.some((label) => nameKey(label) === nameKey(epicLabel))) {
    labels.push(epicLabel)
    added.push(epicLabel)
  }
  const unplanned = stateName(config, 'unplanned')
  const planned = stateName(config, 'planned')
  const currentState = readState(parent)
  const state = nameKey(currentState) === nameKey(unplanned) ? planned : null
  return {
    flip: true,
    write: added.length > 0 || removed.length > 0 || state !== null,
    id: ledger.parentId,
    labels,
    state,
    added,
    removed,
    reasons: [
      `all ${summary.counts.planned} eligible child(ren) planned (${alreadyPlanned} already planned)`,
      state === null
        ? `the parent is in "${currentState}", not "${unplanned}" — its state is kept`
        : `the parent moves "${unplanned}" → "${planned}"`,
    ],
  }
}

// --- files -------------------------------------------------------------------------------------

/**
 * Write the ledger atomically: serialise first (a throw leaves the file untouched), write a
 * sibling temp file, then rename over the target.
 */
export function writeLedgerFile(path, ledger) {
  const text = `${JSON.stringify(ledger, null, 2)}\n`
  const tmp = join(dirname(path), `.${basename(path)}.${process.pid}.tmp`)
  try {
    writeFileSync(tmp, text)
    renameSync(tmp, path)
  } catch (error) {
    rmSync(tmp, { force: true })
    throw error
  }
}

const readJSON = (path) => {
  let raw
  try {
    raw = readFileSync(path, 'utf8')
  } catch (error) {
    fail(`cannot read ${path}: ${error.message}`)
  }
  try {
    return JSON.parse(raw)
  } catch (error) {
    fail(`${path} is not JSON: ${error.message}`)
  }
}

function flagValue(argv, flag) {
  const index = argv.indexOf(flag)
  if (index === -1) return null
  const value = argv[index + 1]
  if (typeof value !== 'string' || value === '' || value.startsWith('--')) {
    fail(`${flag} requires a value`)
  }
  return value
}

const USAGE =
  'usage: plan-child-fanout.mjs args [--] <argv…> | route <bundle.json> | init <bundle.json> <ledger.json> [--parallel N] | next <ledger.json> | record <ledger.json> <child-id> <dispatched|planned|failed> [--reason r] [--scratch dir] | summary <ledger.json> | parent-flip <ledger.json> <parent.json>'

/**
 * CLI. Every verb prints exactly one JSON object on stdout. Exit 0 on any verdict, 1 on bad input
 * (named on stderr), 2 on a usage error.
 */
export function runCli(argv, { now = Date.now(), cwd = process.cwd() } = {}) {
  const [cmd, first, second, third] = argv
  const print = (value) => process.stdout.write(`${JSON.stringify(value)}\n`)
  const config = () => loadSkillConfig({ cwd })
  try {
    if (cmd === 'args') {
      const rest = argv.slice(1)
      print(parsePlanArgs(rest[0] === '--' ? rest.slice(1) : rest))
      return 0
    }
    if (cmd === 'route' && first) {
      const bundle = readJSON(first)
      print(routeSelectedIssue({ ...bundle, config: config() }))
      return 0
    }
    if (cmd === 'init' && first && second) {
      const bundle = readJSON(first)
      const raw = flagValue(argv, '--parallel')
      const parallel = raw === null ? PARALLEL_DEFAULT : parseParallel(raw)
      const ledger = initFanoutLedger({ ...bundle, parallel, config: config() })
      writeLedgerFile(second, ledger)
      print({
        parentId: ledger.parentId,
        parallel: ledger.parallel,
        order: ledger.order,
        skipped: ledger.skipped,
        warnings: ledger.warnings,
      })
      return 0
    }
    if (cmd === 'next' && first) {
      const { ledger, step } = nextFanoutStep(readJSON(first), { now })
      writeLedgerFile(first, ledger)
      print(step)
      return 0
    }
    if (cmd === 'record' && first && second && third) {
      const { ledger, child } = recordChildOutcome(readJSON(first), second, third, {
        reason: flagValue(argv, '--reason'),
        scratch: flagValue(argv, '--scratch'),
        now,
      })
      writeLedgerFile(first, ledger)
      print(child)
      return 0
    }
    if (cmd === 'summary' && first) {
      print(fanoutSummary(readJSON(first)))
      return 0
    }
    if (cmd === 'parent-flip' && first && second) {
      print(
        parentFlipDecision({ ledger: readJSON(first), parent: readJSON(second), config: config() }),
      )
      return 0
    }
  } catch (error) {
    process.stderr.write(`${error?.message ?? error}\n`)
    return 1
  }
  process.stderr.write(`${USAGE}\n`)
  return 2
}

if (isMainModule(import.meta.url)) {
  process.exitCode = runCli(process.argv.slice(2))
}
