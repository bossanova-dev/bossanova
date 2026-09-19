#!/usr/bin/env node

// epic-driver.mjs — the executable state machine that owns an epic run's lifecycle.
//
// WHY THIS MODULE EXISTS. The epic lifecycle used to live in SKILL.md prose: launch a child,
// report it, and the model turn ends. Nothing read the result, so a run that launched one child
// and stopped reached a final report byte-identically to one that drove every child to merge.
// This module is the read. It answers one question — may this run emit a final report? — so the
// answer is a checked verdict rather than a remembered step. `assertEpicCanTerminate` is the gate;
// everything else exists to keep the state it reads true.
//
// The three rules that shape the design:
//
//   1. THE END OF A MODEL TURN IS NOT THE END OF A RUN. A launch or adoption is an intermediate
//      checkpoint. `assertEpicCanTerminate` throws while any ready, in-flight, green, pending
//      cascade, unreconciled adoption, unreconciled wake, or unevaluated external-blocker state
//      exists, and the sole DONE transition adds final-reconciliation, all-terminal, final-progress
//      and watch-cleanup requirements on top.
//   2. A WAKE IS A PROMPT, NEVER PROOF. Callback triggers, subscription outcomes, fallback timers,
//      retries and manual resumes are all one input: they select no mutation path of their own.
//      `reconcileEpic` re-reads authoritative state for every tracked child on every wake, whatever
//      woke it. A delivered-twice callback and a stale-SHA green are therefore no-ops.
//   3. STATE IS JSON, AND IT IS WRITTEN AFTER EVERY TRANSITION. `Map`/`Set` are rejected by
//      `validateEpicState` — collections are arrays on disk and are rebuilt inside one cycle, which
//      is what lets a fresh process rehydrate without turn history. The progress comment carries a
//      machine-readable run block (see progress-comment.mjs) so the state file can be FOUND again.
//
// Contract:
//   - The decision surface is pure and offline: no network, no child processes, no `gh`, no `boss`.
//     `reconcileEpic` performs I/O only through the injected `io` bag (EPIC_DRIVER_IO), which is
//     what makes a full launch → wake → merge → resume cycle testable from fixtures alone. Same
//     contract as the sibling `pr-check-state.mjs` / `callback/ci-watch.mjs` verdict helpers.
//   - Persistence touches `node:fs` only, and only through `saveEpicState` / `loadEpicState`, which
//     take an injectable `fs`.
//   - Node builtins plus this toolbox only, so it runs from any installed skill toolbox in any
//     repository. Tracker names, state names, session ids and prompts are all supplied by the
//     caller — nothing project-specific is baked in.
//   - Scheduling is NOT reimplemented here. `dag-scheduler.mjs` owns ready/cascade/merge ordering
//     and `bs-epic-lib.mjs`'s `classifyChildLiveness` owns liveness. This module composes them.

import { readFileSync, writeFileSync, renameSync, mkdirSync, existsSync, unlinkSync } from 'node:fs'
import path from 'node:path'

import {
  buildGraph,
  readyTickets,
  nextToMerge,
  transitiveDependents,
  mergeBlockedExternalBlockers,
} from './dag-scheduler.mjs'
import { classifyChildLiveness } from './bs-epic-lib.mjs'
import { bossCallbackPolicy } from './callback/boss.mjs'
import { isMainModule } from './main-module.mjs'

/**
 * The PR triggers armed per in-flight epic child, and the one that is forbidden. Re-exported from
 * the callback policy rather than restated, so the driver and the adapter cannot disagree about
 * what a draft-aware epic watch is. `checks_passed_ready` (green AND no longer a draft) is the
 * merge-eligibility moment; bare `checks_passed` fires on the first green DRAFT commit and burns
 * the one-shot watch at a moment that can never be merge-eligible.
 */
export const EPIC_CHILD_PR_TRIGGERS = bossCallbackPolicy.epicChildTriggers
export const FORBIDDEN_EPIC_CHILD_PR_TRIGGERS = bossCallbackPolicy.forbiddenDraftTriggers

/** Bump only with a migration in `normalizeEpicState`. */
export const EPIC_STATE_VERSION = 1

/**
 * The four user-facing run statuses, deliberately machine-distinguishable. `success` is NOT one of
 * them: a launched child is not a completed epic, and the vocabulary is what stops a status update
 * reading as a final report.
 */
export const EPIC_RUN_STATUSES = Object.freeze({
  /** Work remains and durable continuation is armed. */
  RUNNING: 'RUNNING',
  /** Work remains but a required wake mechanism is missing; retry or fail closed. */
  RUNNING_BUT_UNWATCHED: 'RUNNING_BUT_UNWATCHED',
  /** A required capability is unavailable and no safe fallback exists. */
  BLOCKED: 'BLOCKED',
  /** The terminal invariant is false and final reconciliation is complete. */
  DONE: 'DONE',
})

/** Every status except DONE. Exported so a caller asserts the non-terminal surface rather than
 *  re-deriving it, and so widening it is a visible edit. */
export const NON_TERMINAL_STATUSES = Object.freeze([
  EPIC_RUN_STATUSES.RUNNING,
  EPIC_RUN_STATUSES.RUNNING_BUT_UNWATCHED,
  EPIC_RUN_STATUSES.BLOCKED,
])

export const EPIC_PHASES = Object.freeze([
  'assembling',
  'reconstructing',
  'scheduling',
  'polling',
  'merging',
  'terminal',
])

/** Wake kinds that enter the ONE reconciliation cycle. The kind is provenance for the log; it
 *  selects no action path, which is rule 2 above expressed as data. */
export const EPIC_WAKE_KINDS = Object.freeze([
  'initial',
  'callback',
  'subscription',
  'fallback',
  'retry',
  'manual-resume',
])

/**
 * The I/O the reconciliation cycle needs, as a declared contract rather than an ambient import.
 * A caller supplies real implementations (MCP tools or the `boss` CLI, per the session/tracker/
 * callback adapter maps); a test supplies fakes and asserts what was and was not called.
 */
export const EPIC_DRIVER_IO = Object.freeze([
  /** `() => Array<{id, title, tracker_id?, agent_session_id?, state?}>` — repo sessions, for adoption. */
  'listSessions',
  /** `({ticketId, sessionId, chatId}) => ChildSnapshot` — ONE authoritative re-read per tracked child. */
  'readSessionSnapshot',
  /** `(ids) => Record<id, 'cleared'|'open'>` — current tracker state of each external blocker. */
  'readExternalBlockers',
  /** `({ticket}) => {sessionId, chatId}` — launch. Never called for an already-adopted ticket. */
  'createSession',
  /** `({ticketId, prNumber, prRepo, triggers, message}) => {callbacks: Array, error?: string}` */
  'armWatches',
  /** `({ticketId, prNumber, prRepo}) => Array` — list-read that VERIFIES a registration took. */
  'listWatches',
  /** `({ticketId, sessionId, message}) => {subscription?: object, error?: string}` */
  'subscribeSessionOutcome',
  /** `({sessionId}) => Array` — list-read that VERIFIES the subscription took. */
  'listSessionSubscriptions',
  /** `({ticketId, callbackIds, subscriptionId}) => void` — terminal-bookkeeping cleanup. */
  'removeWatches',
  /** `({ticketId, reason}) => {mechanism, nextWakeAt} | null` — the bounded fallback wake. */
  'scheduleFallbackWake',
  /** `({ticketId, sessionId}) => {merged: boolean, error?: string}` — at most one call per cycle. */
  'mergeChild',
  /** `({ticketId, sessionId}) => boolean` — provider/session merge verification. */
  'verifyMerged',
  /** `({ticketId}) => boolean` — move the child ticket to its done state. Only after verifyMerged. */
  'moveTicketDone',
  /** `({state, progressState}) => {commentId?: string}` — single-comment upsert. */
  'upsertProgress',
])

/**
 * Throw if `io` is missing any capability. Called at the top of `reconcileEpic` so a half-wired
 * host fails on the first cycle rather than at the moment it would have armed a watch.
 */
export function assertEpicIo(io) {
  const missing = EPIC_DRIVER_IO.filter((name) => typeof io?.[name] !== 'function')
  if (missing.length > 0) {
    throw new Error(`epic-driver: io is missing ${missing.join(', ')}`)
  }
  return io
}

// ---------------------------------------------------------------------------
// State model
// ---------------------------------------------------------------------------

const ID_LIST_FIELDS = Object.freeze([
  'ready',
  'inFlight',
  'greens',
  'merged',
  'failed',
  'cascadeSkipped',
  'externallyCleared',
  'pendingCascade',
  'needsHuman',
  'retainedEvidenceWatches',
])

// `owner_session_id`-style identifiers the driver keys ownership on. A blank or non-string value
// here means a watch/session cannot be attributed, which must fail validation rather than be
// silently carried as "" and later compared equal to another blank.
const OWNERSHIP_FIELDS = Object.freeze(['sessionId', 'chatId'])

function isNonEmptyString(value) {
  return typeof value === 'string' && value.trim().length > 0
}

function uniqueIds(values) {
  return [...new Set((Array.isArray(values) ? values : []).filter(isNonEmptyString))]
}

function withoutIds(list, remove) {
  const drop = new Set(remove)
  return list.filter((id) => !drop.has(id))
}

function addId(list, id) {
  return list.includes(id) ? list : [...list, id]
}

/**
 * Build a fresh, normalized run state. `runId` and `epicId` are the storage key; everything else
 * is the immutable invocation/config record plus empty collections.
 */
export function createEpicState({
  runId,
  epicId,
  repoId = '',
  agent = '',
  parallel = 1,
  plannedState = '',
  reviewState = '',
  childWallClockMinutes = 0,
  startedAt = '',
  tickets = [],
  progressMarker = 'boss-epic-progress',
} = {}) {
  return normalizeEpicState({
    version: EPIC_STATE_VERSION,
    runId,
    epicId,
    repoId,
    agent,
    parallel,
    plannedState,
    reviewState,
    childWallClockMinutes,
    startedAt,
    tickets,
    progressMarker,
    phase: 'assembling',
    status: EPIC_RUN_STATUSES.RUNNING,
  })
}

/**
 * Canonicalize a state object: fill defaults, de-duplicate every id list, and coerce the record
 * maps. Idempotent — `normalizeEpicState(normalizeEpicState(s))` is byte-equal, which is what makes
 * a save/load round trip stable.
 *
 * `needsHuman` ids are stripped out of `ready` and `inFlight` HERE, at the one choke point every
 * transition passes through, rather than at each call site. A needs-human ticket must never be
 * launched or counted as in flight, and a rule enforced in normalization cannot be forgotten by a
 * new transition.
 */
export function normalizeEpicState(state = {}) {
  const out = {
    version: EPIC_STATE_VERSION,
    runId: isNonEmptyString(state.runId) ? state.runId : '',
    epicId: isNonEmptyString(state.epicId) ? state.epicId : '',
    repoId: typeof state.repoId === 'string' ? state.repoId : '',
    agent: typeof state.agent === 'string' ? state.agent : '',
    parallel: Number.isFinite(state.parallel) && state.parallel > 0 ? state.parallel : 1,
    plannedState: typeof state.plannedState === 'string' ? state.plannedState : '',
    reviewState: typeof state.reviewState === 'string' ? state.reviewState : '',
    childWallClockMinutes: Number.isFinite(state.childWallClockMinutes)
      ? state.childWallClockMinutes
      : 0,
    startedAt: typeof state.startedAt === 'string' ? state.startedAt : '',
    lastReconciledAt: typeof state.lastReconciledAt === 'string' ? state.lastReconciledAt : '',
    cycle: Number.isFinite(state.cycle) ? state.cycle : 0,
    externalBlockersEvaluatedCycle: Number.isFinite(state.externalBlockersEvaluatedCycle)
      ? state.externalBlockersEvaluatedCycle
      : -1,
    phase: EPIC_PHASES.includes(state.phase) ? state.phase : 'assembling',
    status: Object.values(EPIC_RUN_STATUSES).includes(state.status)
      ? state.status
      : EPIC_RUN_STATUSES.RUNNING,
    tickets: (Array.isArray(state.tickets) ? state.tickets : []).map((ticket) => ({
      id: ticket?.id ?? '',
      title: typeof ticket?.title === 'string' ? ticket.title : '',
      priority: Number.isFinite(ticket?.priority) ? ticket.priority : 0,
      createdAt: typeof ticket?.createdAt === 'string' ? ticket.createdAt : '',
      blockedBy: uniqueIds(ticket?.blockedBy),
      needsHuman: ticket?.needsHuman === true,
    })),
    sessions: {},
    watches: {},
    pendingWakes: (Array.isArray(state.pendingWakes) ? state.pendingWakes : []).map((wake) => ({
      kind: EPIC_WAKE_KINDS.includes(wake?.kind) ? wake.kind : 'manual-resume',
      receivedAt: typeof wake?.receivedAt === 'string' ? wake.receivedAt : '',
      reconciledAt: isNonEmptyString(wake?.reconciledAt) ? wake.reconciledAt : null,
    })),
    progressCommentId: typeof state.progressCommentId === 'string' ? state.progressCommentId : '',
    progressMarker: isNonEmptyString(state.progressMarker)
      ? state.progressMarker
      : 'boss-epic-progress',
    finalProgressWrittenAt:
      typeof state.finalProgressWrittenAt === 'string' ? state.finalProgressWrittenAt : '',
    watchCleanupDoneAt:
      typeof state.watchCleanupDoneAt === 'string' ? state.watchCleanupDoneAt : '',
  }

  for (const field of ID_LIST_FIELDS) out[field] = uniqueIds(state[field])

  const declaredNeedsHuman = out.tickets.filter((t) => t.needsHuman).map((t) => t.id)
  out.needsHuman = uniqueIds([...out.needsHuman, ...declaredNeedsHuman])
  out.ready = withoutIds(out.ready, out.needsHuman)
  out.inFlight = withoutIds(out.inFlight, out.needsHuman)

  const sessions = state.sessions && typeof state.sessions === 'object' ? state.sessions : {}
  for (const [ticketId, record] of Object.entries(sessions)) {
    out.sessions[ticketId] = {
      sessionId: record?.sessionId ?? '',
      chatId: record?.chatId ?? '',
      adopted: record?.adopted === true,
      reconciled: record?.reconciled === true,
      launchedAt: typeof record?.launchedAt === 'string' ? record.launchedAt : '',
      prNumber: Number.isFinite(record?.prNumber) ? record.prNumber : null,
      prRepo: typeof record?.prRepo === 'string' ? record.prRepo : '',
      prUrl: typeof record?.prUrl === 'string' ? record.prUrl : '',
      liveness: typeof record?.liveness === 'string' ? record.liveness : '',
      repairRounds: Number.isFinite(record?.repairRounds) ? record.repairRounds : 0,
      note: typeof record?.note === 'string' ? record.note : '',
    }
  }

  const watches = state.watches && typeof state.watches === 'object' ? state.watches : {}
  for (const [ticketId, record] of Object.entries(watches)) {
    out.watches[ticketId] = {
      targetChatId: typeof record?.targetChatId === 'string' ? record.targetChatId : '',
      repo: typeof record?.repo === 'string' ? record.repo : '',
      callbacks: (Array.isArray(record?.callbacks) ? record.callbacks : []).map((cb) => ({
        id: cb?.id ?? '',
        trigger: cb?.trigger ?? '',
        state: cb?.state ?? '',
      })),
      subscription:
        record?.subscription && typeof record.subscription === 'object'
          ? {
              id: record.subscription.id ?? '',
              ownerSessionId: record.subscription.owner_session_id ?? '',
              originChatId: record.subscription.origin_chat_id ?? '',
              triggerEvent: record.subscription.trigger_event ?? '',
              state: record.subscription.state ?? '',
              firedAt: record.subscription.fired_at ?? '',
              expiresAt: record.subscription.expires_at ?? '',
              ...normalizeCamelSubscription(record.subscription),
            }
          : null,
      verifiedAt: typeof record?.verifiedAt === 'string' ? record.verifiedAt : '',
      unwatchedReason: typeof record?.unwatchedReason === 'string' ? record.unwatchedReason : '',
      fallback:
        record?.fallback && typeof record.fallback === 'object'
          ? {
              mechanism: record.fallback.mechanism ?? '',
              nextWakeAt: record.fallback.nextWakeAt ?? '',
              reason: record.fallback.reason ?? '',
              retryCount: Number.isFinite(record.fallback.retryCount)
                ? record.fallback.retryCount
                : 0,
              lastReconciledAt: record.fallback.lastReconciledAt ?? '',
            }
          : null,
    }
  }

  return out
}

// A subscription row arrives either as the CLI's snake_case JSON or as an already-normalized
// camelCase record (a save/load round trip). Accept both so normalization is idempotent.
function normalizeCamelSubscription(sub) {
  const out = {}
  if (isNonEmptyString(sub.ownerSessionId)) out.ownerSessionId = sub.ownerSessionId
  if (isNonEmptyString(sub.originChatId)) out.originChatId = sub.originChatId
  if (isNonEmptyString(sub.triggerEvent)) out.triggerEvent = sub.triggerEvent
  if (isNonEmptyString(sub.firedAt)) out.firedAt = sub.firedAt
  if (isNonEmptyString(sub.expiresAt)) out.expiresAt = sub.expiresAt
  return out
}

/**
 * Validate a state object. Repo house style: `{ok, errors}`, never throws.
 *
 * The two checks worth naming, because each fails silently and destructively if tolerated:
 *   - A `Map` or `Set` anywhere in the tree. `JSON.stringify(new Set(['A']))` is `{}`, so a state
 *     persisted with Set collections reloads with every id gone — the resume path reconstructs an
 *     EMPTY in-flight table and relaunches every child. Rejected by shape, at any depth.
 *   - A blank/absent ownership identifier (`sessions[*].sessionId` / `.chatId`). Ownership is what
 *     makes adoption idempotent and a watch attributable; two records with `""` compare equal, so
 *     the driver would adopt one child twice and remove another child's watch.
 */
export function validateEpicState(state) {
  if (state === null || typeof state !== 'object' || Array.isArray(state)) {
    return { ok: false, errors: ['state: required object'] }
  }
  const errors = []
  if (state.version !== EPIC_STATE_VERSION) {
    errors.push(`version: expected ${EPIC_STATE_VERSION}, got ${String(state.version)}`)
  }
  if (!isNonEmptyString(state.runId)) errors.push('runId: required non-empty string')
  if (!isNonEmptyString(state.epicId)) errors.push('epicId: required non-empty string')
  if (!isNonEmptyString(state.progressMarker)) {
    errors.push('progressMarker: required non-empty string')
  }
  if (!EPIC_PHASES.includes(state.phase)) errors.push(`phase: unknown phase ${String(state.phase)}`)
  if (!Object.values(EPIC_RUN_STATUSES).includes(state.status)) {
    errors.push(`status: unknown status ${String(state.status)}`)
  }

  for (const field of ID_LIST_FIELDS) {
    const value = state[field]
    if (!Array.isArray(value)) {
      errors.push(`${field}: required array (Map/Set are never persisted)`)
      continue
    }
    value.forEach((id, index) => {
      if (!isNonEmptyString(id)) errors.push(`${field}[${index}]: required non-empty string`)
    })
  }

  for (const [key, record] of Object.entries(
    state.sessions && typeof state.sessions === 'object' ? state.sessions : {},
  )) {
    for (const field of OWNERSHIP_FIELDS) {
      if (!isNonEmptyString(record?.[field])) {
        errors.push(`sessions.${key}.${field}: required non-empty ownership identifier`)
      }
    }
  }

  for (const [key, record] of Object.entries(
    state.watches && typeof state.watches === 'object' ? state.watches : {},
  )) {
    const sub = record?.subscription
    if (sub && !isNonEmptyString(sub.id)) {
      errors.push(`watches.${key}.subscription.id: required non-empty identifier`)
    }
    if (sub && !isNonEmptyString(sub.ownerSessionId ?? sub.owner_session_id)) {
      errors.push(`watches.${key}.subscription.ownerSessionId: required for ownership verification`)
    }
  }

  const collections = findUnserializableCollections(state)
  for (const where of collections) {
    errors.push(`${where}: Map/Set is not JSON-safe and must be persisted as an array`)
  }

  return { ok: errors.length === 0, errors }
}

function findUnserializableCollections(value, trail = 'state', seen = new Set()) {
  if (value === null || typeof value !== 'object') return []
  if (seen.has(value)) return []
  seen.add(value)
  if (value instanceof Map || value instanceof Set) return [trail]
  const found = []
  if (Array.isArray(value)) {
    value.forEach((item, index) => {
      found.push(...findUnserializableCollections(item, `${trail}[${index}]`, seen))
    })
    return found
  }
  for (const [key, item] of Object.entries(value)) {
    found.push(...findUnserializableCollections(item, `${trail}.${key}`, seen))
  }
  return found
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

/**
 * The stable storage path for a run's state, keyed by epic and run so two concurrent epics — and
 * two runs of one epic — never share a file. Deterministic from `{epicId, runId}` alone, which is
 * what lets a FRESH process find the file again given only the progress comment's run block.
 */
export function epicStatePath({ epicId, runId, dir } = {}) {
  if (!isNonEmptyString(epicId)) throw new Error('epicStatePath: epicId is required')
  if (!isNonEmptyString(runId)) throw new Error('epicStatePath: runId is required')
  if (!isNonEmptyString(dir)) throw new Error('epicStatePath: dir is required')
  return path.join(dir, `${slugForPath(epicId)}.${slugForPath(runId)}.json`)
}

function slugForPath(value) {
  return value.trim().replace(/[^A-Za-z0-9._-]+/g, '-')
}

/**
 * Persist state by atomic replacement: write a sibling temp file, then `rename`. A crash mid-write
 * therefore leaves the PREVIOUS state intact rather than a truncated file that fails to parse — and
 * a state file that fails to parse is indistinguishable from a fresh run, which is the relaunch-
 * everything failure this whole module exists to prevent.
 *
 * Validates before writing: an invalid state on disk is worse than a failed save, because the save
 * fails where the driver can still see it.
 */
export function saveEpicState(state, { dir, fs = defaultFs } = {}) {
  const normalized = normalizeEpicState(state)
  const verdict = validateEpicState(normalized)
  if (!verdict.ok) {
    throw new Error(
      `saveEpicState: refusing to persist invalid state — ${verdict.errors.join('; ')}`,
    )
  }
  const target = epicStatePath({ epicId: normalized.epicId, runId: normalized.runId, dir })
  fs.mkdirSync(path.dirname(target), { recursive: true })
  const temp = `${target}.tmp`
  fs.writeFileSync(temp, `${JSON.stringify(normalized, null, 2)}\n`, 'utf8')
  fs.renameSync(temp, target)
  return { path: target, state: normalized }
}

/**
 * Rehydrate state. A missing file returns `null` (a genuinely fresh run); an unparseable or invalid
 * file throws, because silently starting fresh on top of a live epic relaunches every child.
 */
export function loadEpicState({ epicId, runId, dir, fs = defaultFs } = {}) {
  const target = epicStatePath({ epicId, runId, dir })
  if (!fs.existsSync(target)) return null
  let parsed
  try {
    parsed = JSON.parse(fs.readFileSync(target, 'utf8'))
  } catch (error) {
    throw new Error(
      `loadEpicState: ${target} is unreadable (${error?.message ?? error}) — a fresh start here ` +
        'would relaunch live children; repair or remove the file deliberately',
    )
  }
  const normalized = normalizeEpicState(parsed)
  const verdict = validateEpicState(normalized)
  if (!verdict.ok) {
    throw new Error(`loadEpicState: ${target} is invalid — ${verdict.errors.join('; ')}`)
  }
  return normalized
}

const defaultFs = {
  readFileSync,
  writeFileSync,
  renameSync,
  mkdirSync,
  existsSync,
  unlinkSync,
}

// ---------------------------------------------------------------------------
// The terminal guard
// ---------------------------------------------------------------------------

/**
 * Every reason this run may not emit a final report, as `label:ids` strings. Empty means the
 * non-terminal invariant is false.
 *
 * The seven conditions are the invariant verbatim: a ready eligible ticket; an in-flight ticket; a
 * green queued for serialized merge; an adopted session not yet reconciled to merged or
 * fail-isolated; a failed ticket whose dependents' cascade outcome is unrecorded; a wake awaiting
 * reconciliation; external-blocker state not evaluated for the current cycle.
 */
export function epicTerminalBlockers(state) {
  const s = normalizeEpicState(state)
  const blockers = []
  if (s.ready.length) blockers.push(`ready:${s.ready.join(',')}`)
  if (s.inFlight.length) blockers.push(`inFlight:${s.inFlight.join(',')}`)
  if (s.greens.length) blockers.push(`greens:${s.greens.join(',')}`)
  if (s.pendingCascade.length) blockers.push(`cascade:${s.pendingCascade.join(',')}`)

  const unreconciled = Object.entries(s.sessions)
    .filter(([id, record]) => record.adopted && !record.reconciled && !isTerminalTicket(s, id))
    .map(([id]) => id)
  if (unreconciled.length) blockers.push(`unreconciled:${unreconciled.join(',')}`)

  const pendingWakes = s.pendingWakes.filter((wake) => wake.reconciledAt === null)
  if (pendingWakes.length) {
    blockers.push(`pendingWake:${pendingWakes.map((wake) => wake.kind).join(',')}`)
  }

  // Fail closed: an external-blocker read from a PREVIOUS cycle is not an evaluation of this one.
  // A ticket can be unparked (or re-parked) by a blocker that moved since, so a stale read is
  // exactly as uninformative as no read at all.
  if (s.externalBlockersEvaluatedCycle !== s.cycle) {
    blockers.push('externalBlockers:not-evaluated-this-cycle')
  }
  return blockers
}

function isTerminalTicket(state, id) {
  return state.merged.includes(id) || state.failed.includes(id) || state.cascadeSkipped.includes(id)
}

/** Is the non-terminal invariant false — i.e. may the run consider terminating at all? */
export function canEpicTerminate(state) {
  return epicTerminalBlockers(state).length === 0
}

/**
 * The gate the final-report path MUST call. Throws naming every blocker; returns `true` otherwise.
 *
 * Deliberately a throw rather than a boolean: the caller that forgets to check a boolean ships the
 * premature final report this exists to prevent, whereas the caller that forgets to catch a throw
 * ships nothing at all. Failing loud is the correct direction here.
 */
export function assertEpicCanTerminate(state) {
  const blockers = epicTerminalBlockers(state)
  if (blockers.length > 0) {
    throw new Error(`epic is non-terminal; continuation required (${blockers.join('; ')})`)
  }
  return true
}

/**
 * The additional DONE requirements, on top of `epicTerminalBlockers`. Returns the unmet ones.
 * Separated from the invariant because these are completion-of-bookkeeping facts rather than
 * outstanding-work facts, and a reader needs to know which kind is holding the run open.
 */
export function epicDoneBlockers(state) {
  const s = normalizeEpicState(state)
  const blockers = epicTerminalBlockers(s)
  if (!isNonEmptyString(s.lastReconciledAt)) blockers.push('finalReconciliation:missing')
  const nonTerminal = s.tickets
    .map((ticket) => ticket.id)
    .filter((id) => !s.needsHuman.includes(id) && !isTerminalTicket(s, id))
  if (nonTerminal.length) blockers.push(`notTerminal:${nonTerminal.join(',')}`)
  if (!isNonEmptyString(s.finalProgressWrittenAt)) blockers.push('finalProgress:not-written')
  if (!isNonEmptyString(s.watchCleanupDoneAt)) blockers.push('watchCleanup:not-done')
  return blockers
}

/**
 * The SOLE DONE transition. Throws unless the invariant is false AND final reconciliation, all-
 * terminal children, the final progress upsert and watch cleanup are all recorded. There is no
 * other way to set `status: DONE` — every other transition below returns a non-terminal status,
 * which is what makes "a launch cannot be reported as success" structural rather than remembered.
 */
export function transitionToDone(state) {
  const blockers = epicDoneBlockers(state)
  if (blockers.length > 0) {
    throw new Error(`epic cannot be reported DONE (${blockers.join('; ')})`)
  }
  return normalizeEpicState({
    ...normalizeEpicState(state),
    phase: 'terminal',
    status: EPIC_RUN_STATUSES.DONE,
  })
}

// ---------------------------------------------------------------------------
// Transitions (pure: (state, input) -> state)
// ---------------------------------------------------------------------------

/**
 * Record a launch or an adoption as ONE transaction: identity, in-flight membership, and the
 * not-yet-reconciled mark for an adoption. Never DONE, by construction — `status` is forced back to
 * RUNNING here, so the step that adds work cannot also be the step that declares completion.
 *
 * A `needsHuman` ticket is refused outright rather than normalized away, because a caller that
 * tried to launch one has a bug the normalization would hide.
 */
export function recordLaunch(
  state,
  {
    ticketId,
    sessionId,
    chatId,
    at = '',
    prNumber = null,
    prRepo = '',
    prUrl = '',
    adopted = false,
  } = {},
) {
  const s = normalizeEpicState(state)
  if (!isNonEmptyString(ticketId)) throw new Error('recordLaunch: ticketId is required')
  if (!isNonEmptyString(sessionId)) throw new Error('recordLaunch: sessionId is required')
  if (!isNonEmptyString(chatId)) throw new Error('recordLaunch: chatId is required')
  if (s.needsHuman.includes(ticketId)) {
    throw new Error(
      `recordLaunch: ${ticketId} is needs-human and must never be launched or adopted`,
    )
  }
  return normalizeEpicState({
    ...s,
    ready: withoutIds(s.ready, [ticketId]),
    inFlight: addId(s.inFlight, ticketId),
    status: EPIC_RUN_STATUSES.RUNNING,
    sessions: {
      ...s.sessions,
      [ticketId]: {
        ...(s.sessions[ticketId] ?? {}),
        sessionId,
        chatId,
        adopted,
        // An adoption starts UNRECONCILED: the point of adopting is that the driver does not yet
        // know what the live child did while no driver was watching.
        reconciled: adopted ? false : true,
        launchedAt: at,
        prNumber,
        prRepo,
        prUrl,
      },
    },
  })
}

/** Adoption is a launch that did not create a session. Same transaction, `adopted: true`. */
export function recordAdoption(state, input = {}) {
  return recordLaunch(state, { ...input, adopted: true })
}

/** Mark an adopted child reconciled once this cycle has re-read its authoritative state. */
export function recordAdoptionReconciled(state, { ticketId, liveness = '' } = {}) {
  const s = normalizeEpicState(state)
  const record = s.sessions[ticketId]
  if (!record) return s
  return normalizeEpicState({
    ...s,
    sessions: { ...s.sessions, [ticketId]: { ...record, reconciled: true, liveness } },
  })
}

/** Record the verified durable coverage for a child: callbacks, the settled subscription, and the
 *  verification timestamp. Called only AFTER a list-read proved each registration took. */
export function recordWatchCoverage(
  state,
  {
    ticketId,
    targetChatId = '',
    repo = '',
    callbacks = [],
    subscription = null,
    verifiedAt = '',
  } = {},
) {
  const s = normalizeEpicState(state)
  return normalizeEpicState({
    ...s,
    watches: {
      ...s.watches,
      [ticketId]: {
        ...(s.watches[ticketId] ?? {}),
        targetChatId,
        repo,
        callbacks,
        subscription,
        verifiedAt,
        unwatchedReason: '',
        fallback: null,
      },
    },
  })
}

/**
 * Registration or list-verification failed. This is a TRANSITION, not a log line: either a bounded
 * fallback wake is recorded (and the run stays RUNNING, because it is still genuinely observed), or
 * no fallback exists and the run reports RUNNING_BUT_UNWATCHED / BLOCKED. What it can never be is
 * DONE, and what it can never do is carry on as if watched.
 */
export function recordWatchFailure(
  state,
  { ticketId, reason = '', fallback = null, at = '', blocked = false } = {},
) {
  const s = normalizeEpicState(state)
  const prior = s.watches[ticketId] ?? {}
  const retryCount = (prior.fallback?.retryCount ?? 0) + 1
  const next = {
    ...s,
    watches: {
      ...s.watches,
      [ticketId]: {
        ...prior,
        verifiedAt: '',
        unwatchedReason: reason,
        fallback: fallback
          ? {
              mechanism: fallback.mechanism ?? '',
              nextWakeAt: fallback.nextWakeAt ?? '',
              reason,
              retryCount,
              lastReconciledAt: s.lastReconciledAt || at,
            }
          : null,
      },
    },
  }
  if (fallback) return normalizeEpicState({ ...next, status: EPIC_RUN_STATUSES.RUNNING })
  return normalizeEpicState({
    ...next,
    status: blocked ? EPIC_RUN_STATUSES.BLOCKED : EPIC_RUN_STATUSES.RUNNING_BUT_UNWATCHED,
  })
}

/** Admit a child to the merge queue. `greens` is a SUBSET of `inFlight` — a green keeps its
 *  concurrency slot until merged, so `readyTickets` never re-lists it. */
export function recordGreen(state, { ticketId } = {}) {
  const s = normalizeEpicState(state)
  return normalizeEpicState({ ...s, greens: addId(s.greens, ticketId) })
}

/** Demote a green back out of the merge queue (a conflict, a lost race, a re-read that no longer
 *  passes). Keeps the ticket in flight. */
export function recordGreenDemotion(state, { ticketId, note = '' } = {}) {
  const s = normalizeEpicState(state)
  const record = s.sessions[ticketId]
  return normalizeEpicState({
    ...s,
    greens: withoutIds(s.greens, [ticketId]),
    sessions: record ? { ...s.sessions, [ticketId]: { ...record, note } } : s.sessions,
  })
}

/**
 * Record a VERIFIED merge. `verified` is a required, explicit argument rather than a default,
 * because "the merge call returned" and "the provider says merged" are different facts and only the
 * second may write the ticket to Done.
 */
export function recordMerge(state, { ticketId, verified } = {}) {
  const s = normalizeEpicState(state)
  if (verified !== true) {
    throw new Error(
      `recordMerge: ${ticketId} — refusing to record an unverified merge; re-read the provider PR ` +
        'state first (a merge error can follow a merge that landed, and a merge return is not a merge)',
    )
  }
  return normalizeEpicState({
    ...s,
    greens: withoutIds(s.greens, [ticketId]),
    inFlight: withoutIds(s.inFlight, [ticketId]),
    merged: addId(s.merged, ticketId),
    // A non-node adopted child gates its dependents EXTERNALLY, so fold it in here too or they
    // never unpark.
    externallyCleared: addId(s.externallyCleared, ticketId),
  })
}

/**
 * Fail-isolate a ticket and record its FULL transitive cascade in the same transition. The cascade
 * is computed by `transitiveDependents` — the single authority — and every dependent lands in
 * `cascadeSkipped` before the terminal guard can pass, which is what stops a failure from leaving
 * silently-stranded dependents behind a green terminal report.
 *
 * The session is deliberately left open: isolate preserves evidence for a human, and `retainEvidenceWatch`
 * below keeps its watches armed when the child is still live.
 */
export function recordFailure(state, { ticketId, reason = '', retainEvidenceWatch = false } = {}) {
  const s = normalizeEpicState(state)
  const failed = addId(s.failed, ticketId)
  const graph = buildGraph(s.tickets)
  const cascade = [...transitiveDependents(graph, new Set(failed))].filter(
    (id) => !failed.includes(id) && !s.merged.includes(id),
  )
  const record = s.sessions[ticketId]
  return normalizeEpicState({
    ...s,
    failed,
    inFlight: withoutIds(s.inFlight, [ticketId, ...cascade]),
    ready: withoutIds(s.ready, [ticketId, ...cascade]),
    greens: withoutIds(s.greens, [ticketId]),
    cascadeSkipped: uniqueIds([...s.cascadeSkipped, ...cascade]),
    pendingCascade: [],
    retainedEvidenceWatches: retainEvidenceWatch
      ? addId(s.retainedEvidenceWatches, ticketId)
      : s.retainedEvidenceWatches,
    sessions: record
      ? { ...s.sessions, [ticketId]: { ...record, note: reason || record.note } }
      : s.sessions,
  })
}

/** Register an incoming wake. It is UNRECONCILED until `recordReconciliation` clears it, so a wake
 *  that arrives and is never acted on holds the run open instead of vanishing. */
export function recordWake(state, { kind = 'manual-resume', at = '' } = {}) {
  const s = normalizeEpicState(state)
  return normalizeEpicState({
    ...s,
    pendingWakes: [...s.pendingWakes, { kind, receivedAt: at, reconciledAt: null }],
  })
}

/** Close out the cycle: stamp the reconciliation, clear every pending wake, and advance the cycle
 *  counter so the external-blocker evaluation must be redone for the next one. */
export function recordReconciliation(state, { at = '', externalBlockersEvaluated = false } = {}) {
  const s = normalizeEpicState(state)
  return normalizeEpicState({
    ...s,
    lastReconciledAt: at,
    pendingWakes: s.pendingWakes.map((wake) => ({
      ...wake,
      reconciledAt: wake.reconciledAt ?? at,
    })),
    externalBlockersEvaluatedCycle: externalBlockersEvaluated
      ? s.cycle
      : s.externalBlockersEvaluatedCycle,
  })
}

/** Open a new cycle. Called at the top of every reconciliation so the external-blocker evaluation
 *  from the previous cycle stops counting. */
export function beginCycle(state) {
  const s = normalizeEpicState(state)
  return normalizeEpicState({ ...s, cycle: s.cycle + 1 })
}

/** Record the single progress-comment id, and (at terminal) that the FINAL upsert succeeded. */
export function recordProgressUpsert(state, { commentId = '', finalAt = '' } = {}) {
  const s = normalizeEpicState(state)
  return normalizeEpicState({
    ...s,
    progressCommentId: commentId || s.progressCommentId,
    finalProgressWrittenAt: finalAt || s.finalProgressWrittenAt,
  })
}

/** Record that settled children's watches were torn down (retained evidence watches excepted). */
export function recordWatchCleanup(state, { at = '' } = {}) {
  const s = normalizeEpicState(state)
  return normalizeEpicState({ ...s, watchCleanupDoneAt: at })
}

// ---------------------------------------------------------------------------
// Continuation prompts
// ---------------------------------------------------------------------------

/**
 * The directives every callback / subscription / fallback payload must carry, as NAMED rules rather
 * than one prose blob. A test asserts the rule names and the interpolated epic id, so the wording
 * can be improved without a gate going red over a rewrapped sentence — and a payload that drops a
 * rule still fails.
 */
export const CONTINUATION_DIRECTIVES = Object.freeze({
  resume: 'resume the epic run rather than answering about it',
  noSummarize: 'do not summarize and stop',
  rehydrate: 'rehydrate the persisted run state and the progress marker',
  authoritativeReread: 'treat this wake as a prompt and re-read authoritative state before acting',
  fullCycle: 'execute one full reconciliation and scheduling cycle',
  noFinalUntilTerminal: 'do not return a final answer until the terminal invariant is false',
})

/**
 * Build the wake payload for a durable registration. The epic id is INTERPOLATED — never a literal
 * — so the published core carries no example ticket from any backlog.
 *
 * Every payload is a continuation COMMAND, not a notification: an informational wording is what let
 * a delivered callback be read as "the child is done, report it" instead of "run a cycle".
 */
export function buildContinuationPrompt({ epicId, kind = 'callback', runId = '' } = {}) {
  if (!isNonEmptyString(epicId)) throw new Error('buildContinuationPrompt: epicId is required')
  if (!EPIC_WAKE_KINDS.includes(kind)) {
    throw new Error(`buildContinuationPrompt: unknown wake kind ${String(kind)}`)
  }
  const lines = [
    `Resume the ${epicId} epic orchestrator (wake: ${kind}${runId ? `, run ${runId}` : ''}).`,
    `This is a wake signal, not a result: ${CONTINUATION_DIRECTIVES.noSummarize}.`,
    `Rehydrate step: ${CONTINUATION_DIRECTIVES.rehydrate}.`,
    `Evidence step: ${CONTINUATION_DIRECTIVES.authoritativeReread}.`,
    `Then ${CONTINUATION_DIRECTIVES.fullCycle} for ${epicId}.`,
    `Terminal rule: ${CONTINUATION_DIRECTIVES.noFinalUntilTerminal}; ${CONTINUATION_DIRECTIVES.resume}.`,
  ]
  return lines.join('\n')
}

/**
 * Verify a payload carries every directive plus the epic id. Exported so the driver can check a
 * payload it did not build itself (an operator-supplied message, a payload read back from a watch
 * row) before promising the registration is a continuation.
 */
export function validateContinuationPrompt(text, { epicId } = {}) {
  const errors = []
  if (typeof text !== 'string' || text.trim() === '') {
    return { ok: false, errors: ['message: required non-empty string'] }
  }
  if (isNonEmptyString(epicId) && !text.includes(epicId)) {
    errors.push(`message: must name the epic (${epicId})`)
  }
  for (const [name, directive] of Object.entries(CONTINUATION_DIRECTIVES)) {
    if (!text.includes(directive)) errors.push(`message: missing ${name} directive`)
  }
  return { ok: errors.length === 0, errors }
}

/**
 * The status line a user-facing response must carry. Rendered from the state's own status so an
 * intermediate checkpoint cannot be worded as a completion — `DONE` is unreachable here unless
 * `transitionToDone` already set it.
 */
export function renderRunStatusLine(state) {
  const s = normalizeEpicState(state)
  const counts = [
    `ready=${s.ready.length}`,
    `inFlight=${s.inFlight.length}`,
    `greens=${s.greens.length}`,
    `merged=${s.merged.length}`,
    `failed=${s.failed.length}`,
    `skipped=${s.cascadeSkipped.length}`,
  ].join(' ')
  return `${s.status} ${s.epicId} (${counts})`
}

// ---------------------------------------------------------------------------
// Progress metadata
// ---------------------------------------------------------------------------

const DRIVER_STATUS_TO_PROGRESS = Object.freeze({
  merged: 'merged',
  failed: 'failed',
  skipped: 'skipped',
  green: 'green',
  building: 'building',
  pending: 'pending',
})

/**
 * Project the run state onto the `ProgressState` the shared renderer consumes, including the
 * `run` metadata block that makes a fresh driver's resume possible without turn history.
 *
 * Deliberately a projection rather than a second renderer: `renderProgressComment` /
 * `planProgressCommentUpsert` keep the one-comment invariant, so the driver feeds them and never
 * builds a comment body itself.
 */
export function buildEpicProgressState(state, { updatedAt = '', nextWake = '' } = {}) {
  const s = normalizeEpicState(state)
  const tickets = s.tickets.map((ticket) => {
    const record = s.sessions[ticket.id]
    const watch = s.watches[ticket.id]
    const notes = []
    if (record?.sessionId) notes.push(`session ${record.sessionId}`)
    if (record?.chatId) notes.push(`chat ${record.chatId}`)
    if (record?.liveness) notes.push(`liveness ${record.liveness}`)
    if (watch?.verifiedAt) notes.push('watch verified')
    else if (watch?.fallback?.mechanism) notes.push(`fallback ${watch.fallback.mechanism}`)
    else if (watch?.unwatchedReason) notes.push(`unwatched: ${watch.unwatchedReason}`)
    if (record?.note) notes.push(record.note)
    return {
      id: ticket.id,
      title: ticket.title,
      status: DRIVER_STATUS_TO_PROGRESS[progressStatusForTicket(s, ticket.id)],
      pr: record?.prUrl || (record?.prNumber ? String(record.prNumber) : ''),
      session: record?.sessionId ?? '',
      rounds: record?.repairRounds ?? 0,
      note: notes.join('; '),
    }
  })
  return {
    epicId: s.epicId,
    marker: s.progressMarker,
    updatedAt,
    run: {
      runId: s.runId,
      phase: s.phase,
      status: s.status,
      lastReconciledAt: s.lastReconciledAt,
      nextWake,
    },
    tickets,
  }
}

function progressStatusForTicket(state, id) {
  if (state.merged.includes(id)) return 'merged'
  if (state.failed.includes(id)) return 'failed'
  if (state.cascadeSkipped.includes(id)) return 'skipped'
  if (state.greens.includes(id)) return 'green'
  if (state.inFlight.includes(id)) return 'building'
  return 'pending'
}

// ---------------------------------------------------------------------------
// Reconciliation
// ---------------------------------------------------------------------------

/** Green admission conditions, each judged on an authoritative re-read. All five must hold. */
export function greenAdmissionBlockers(snapshot = {}) {
  const blockers = []
  if (snapshot.checkVerdict?.state !== 'passing') {
    blockers.push(`checks:${snapshot.checkVerdict?.state ?? 'unknown'}`)
  }
  if (snapshot.prView?.isDraft !== false) blockers.push('pr:draft-or-unknown')
  if (snapshot.reviewStateMatches !== true) blockers.push('ticket:not-in-review-state')
  if (snapshot.partialMarker === true) blockers.push('pr:partial-marker')
  if (snapshot.chatSettled !== true) blockers.push('chat:unsettled')
  return blockers
}

/**
 * Should a durable registration be re-armed while the child stays in flight? A one-shot watch is
 * CONSUMED when it fires, and a settled subscription can fire mid-flight — which burns the wake the
 * driver was relying on. So a fired/expired/canceled registration on a still-in-flight child is not
 * coverage; it is a hole that must be re-armed.
 */
export function needsRearm({ registration, inFlight } = {}) {
  if (!inFlight) return false
  if (!registration) return true
  const state = String(registration.state ?? '').toLowerCase()
  if (state === 'active') return false
  return ['fired', 'expired', 'canceled', 'cancelled', ''].includes(state)
}

/**
 * Verify a durable registration is OURS and live, from a list-read row. Ownership is
 * `owner_session_id` + `origin_chat_id`; liveness is `state` plus the absence of a `fired_at`.
 * A row that is not ours is not coverage, however active it looks.
 */
export function verifySubscriptionRow(row, { ownerSessionId, originChatId } = {}) {
  const id = row?.id ?? ''
  const owner = row?.owner_session_id ?? row?.ownerSessionId ?? ''
  const origin = row?.origin_chat_id ?? row?.originChatId ?? ''
  const state = String(row?.state ?? '').toLowerCase()
  const firedAt = row?.fired_at ?? row?.firedAt ?? ''
  if (!isNonEmptyString(id)) return { ok: false, reason: 'subscription row has no id' }
  if (isNonEmptyString(ownerSessionId) && owner !== ownerSessionId) {
    return { ok: false, reason: 'subscription owner_session_id does not match the child session' }
  }
  if (isNonEmptyString(originChatId) && origin !== originChatId) {
    return { ok: false, reason: 'subscription origin_chat_id does not match the orchestrator chat' }
  }
  if (state !== 'active')
    return { ok: false, reason: `subscription state is ${state || 'unknown'}` }
  if (isNonEmptyString(firedAt)) return { ok: false, reason: 'subscription already fired' }
  return { ok: true, reason: '' }
}

/**
 * ONE reconciliation cycle, entered identically by every wake kind.
 *
 * Pipeline, in order: open a cycle → re-list sessions and adopt (never duplicate) → re-read every
 * tracked child's authoritative state → classify liveness → re-read external blockers → admit only
 * genuinely green children → compute the ready set → launch up to the concurrency headroom → arm
 * and list-VERIFY durable coverage for everything in flight → merge at most one target after a
 * merge-time external re-check → write the single progress comment → persist → evaluate the
 * terminal guard → arm the next wake or report DONE.
 *
 * Returns `{state, status, blockers, actions}`. `actions` is the audit trail of what this cycle did;
 * `blockers` is `epicTerminalBlockers(state)` at the end of it.
 */
export async function reconcileEpic({ state, wake = 'initial', io, now = '', save = null } = {}) {
  assertEpicIo(io)
  if (!EPIC_WAKE_KINDS.includes(wake))
    throw new Error(`reconcileEpic: unknown wake ${String(wake)}`)

  const actions = []
  const persist = async (next) => {
    if (typeof save === 'function') await save(next)
    return next
  }

  let s = beginCycle(recordWake(state, { kind: wake, at: now }))
  s = normalizeEpicState({ ...s, phase: 'reconstructing' })
  s = await persist(s)
  actions.push(`wake:${wake}`)

  // --- Adoption. A live session for a tracked ticket is ADOPTED, never duplicated. The match is
  // by tracker id first and the `[TICKET] title` convention second; exactly one session per child.
  const liveSessions = (await io.listSessions()) ?? []
  const adoptedThisCycle = []
  for (const ticket of s.tickets) {
    if (s.needsHuman.includes(ticket.id)) continue
    if (isTerminalTicket(s, ticket.id)) continue
    if (s.sessions[ticket.id]?.sessionId) continue
    const match = matchSessionForTicket(liveSessions, ticket)
    if (!match) continue
    s = recordAdoption(s, {
      ticketId: ticket.id,
      sessionId: match.id,
      chatId: match.agent_session_id ?? match.chatId ?? '',
      at: now,
    })
    adoptedThisCycle.push(ticket.id)
    actions.push(`adopt:${ticket.id}`)
  }
  if (adoptedThisCycle.length > 0) s = await persist(s)

  // --- Authoritative re-read of every tracked child. Nothing below this point reads the wake kind.
  s = normalizeEpicState({ ...s, phase: 'polling' })
  const snapshots = new Map()
  for (const ticketId of [...s.inFlight]) {
    const record = s.sessions[ticketId]
    if (!record?.sessionId) continue
    const snapshot =
      (await io.readSessionSnapshot({
        ticketId,
        sessionId: record.sessionId,
        chatId: record.chatId,
      })) ?? {}
    snapshots.set(ticketId, snapshot)
    const liveness = classifyChildLiveness(snapshot)
    s = recordAdoptionReconciled(s, { ticketId, liveness: liveness.verdict })
    s = normalizeEpicState({
      ...s,
      sessions: {
        ...s.sessions,
        [ticketId]: {
          ...s.sessions[ticketId],
          prNumber: Number.isFinite(snapshot.prView?.number)
            ? snapshot.prView.number
            : s.sessions[ticketId].prNumber,
          prRepo: snapshot.prView?.repo ?? s.sessions[ticketId].prRepo,
          prUrl: snapshot.prView?.url ?? s.sessions[ticketId].prUrl,
        },
      },
    })
    actions.push(`liveness:${ticketId}:${liveness.verdict}/${liveness.action}`)

    if (liveness.action === 'fail-isolate') {
      s = recordFailure(s, {
        ticketId,
        reason: `${liveness.verdict}: ${liveness.reasons.join(',')}`,
        retainEvidenceWatch: true,
      })
      actions.push(`fail-isolate:${ticketId}`)
      s = await persist(s)
      continue
    }

    const blockers = greenAdmissionBlockers(snapshot)
    if (blockers.length === 0) {
      s = recordGreen(s, { ticketId })
      actions.push(`green:${ticketId}`)
    } else {
      s = recordGreenDemotion(s, { ticketId, note: `held: ${blockers.join(',')}` })
      actions.push(`hold:${ticketId}:${blockers.join(',')}`)
    }
    s = await persist(s)
  }

  // --- External blockers, re-read every cycle so an external ticket finishing mid-run unparks its
  // dependents. Until this read lands, the terminal guard refuses DONE for THIS cycle.
  const graph = buildGraph(s.tickets)
  const externalIds = uniqueIds(
    [...graph.externalBlockers.values()].flat().filter((id) => !s.merged.includes(id)),
  )
  const blockerStates =
    externalIds.length > 0 ? ((await io.readExternalBlockers(externalIds)) ?? {}) : {}
  const clearedExternals = uniqueIds([
    ...s.externallyCleared,
    ...Object.entries(blockerStates)
      .filter(([, value]) => value === 'cleared')
      .map(([id]) => id),
  ])
  s = normalizeEpicState({ ...s, externallyCleared: clearedExternals })
  s = recordReconciliation(s, { at: now, externalBlockersEvaluated: true })
  actions.push(`external-blockers:evaluated:${externalIds.length}`)

  // --- Ready set + launch. `readyTickets` is the single authority; cascade-skip happens inside it.
  s = normalizeEpicState({ ...s, phase: 'scheduling' })
  s = await scheduleReadyWork({ state: s, io, now, actions, persist })

  // --- Durable coverage for everything in flight, armed AND list-verified before yielding.
  for (const ticketId of [...s.inFlight]) {
    const coverage = await ensureDurableCoverage({ state: s, ticketId, io, now, actions })
    s = coverage.state
    s = await persist(s)
  }

  // --- Serialized merge: at most ONE per cycle, after a merge-time external re-check.
  s = normalizeEpicState({ ...s, phase: 'merging' })
  const mergeGraph = buildGraph(s.tickets)
  const target = nextToMerge(
    s.greens.map((id) => ({ id })),
    mergeGraph,
    new Set(s.merged),
  )
  if (target) {
    const node = mergeGraph.nodes.get(target) ?? { id: target }
    const openBlockers = mergeBlockedExternalBlockers(node, mergeGraph, {
      clearedBlockers: new Set(
        Object.entries(blockerStates)
          .filter(([, value]) => value === 'cleared')
          .map(([id]) => id),
      ),
    })
    if (openBlockers.length > 0) {
      actions.push(`merge-skip:${target}:external:${openBlockers.join(',')}`)
    } else {
      const record = s.sessions[target]
      const result = (await io.mergeChild({ ticketId: target, sessionId: record.sessionId })) ?? {}
      // A merge error is never a merge failure until the provider says so: an error can follow a
      // merge that landed. Verify either way.
      const verified =
        (await io.verifyMerged({ ticketId: target, sessionId: record.sessionId })) === true
      if (verified) {
        const moved = (await io.moveTicketDone({ ticketId: target })) === true
        s = recordMerge(s, { ticketId: target, verified: true })
        actions.push(`merge:${target}${moved ? '' : ':ticket-writeback-failed'}`)
      } else {
        s = recordGreenDemotion(s, {
          ticketId: target,
          note: `merge not verified: ${result.error ?? 'provider state is not merged'}`,
        })
        actions.push(`merge-unverified:${target}`)
      }
      s = await persist(s)
    }
  }

  // --- Ready set again, and LAUNCH into the slot the merge just freed.
  //
  // Recomputing alone is not enough. A merge both unparks its dependents and frees a concurrency
  // slot, so a cycle that only recomputed could end with a non-empty ready set and an EMPTY
  // in-flight table — correctly non-terminal, but with no armed watch and no in-flight child left
  // to wake it, which is a run that is holding itself open with nothing due to arrive. Scheduling
  // here closes that: progress is monotonic within the cycle, and every non-terminal exit leaves
  // either a verified watch or a recorded fallback behind it.
  s = await scheduleReadyWork({ state: s, io, now, actions, persist })
  for (const ticketId of [...s.inFlight]) {
    if (isNonEmptyString(s.watches[ticketId]?.verifiedAt) || s.watches[ticketId]?.fallback) continue
    const coverage = await ensureDurableCoverage({ state: s, ticketId, io, now, actions })
    s = coverage.state
    s = await persist(s)
  }

  // --- The single progress comment, after every transition.
  const upsert =
    (await io.upsertProgress({
      state: s,
      progressState: buildEpicProgressState(s, { updatedAt: now, nextWake: nextWakeSummary(s) }),
    })) ?? {}
  s = recordProgressUpsert(s, { commentId: upsert.commentId ?? '' })

  const blockers = epicTerminalBlockers(s)
  if (blockers.length > 0) {
    s = normalizeEpicState({ ...s, status: worstNonTerminalStatus(s) })
    s = await persist(s)
    return { state: s, status: s.status, blockers, actions }
  }

  // --- Terminal. Clean up settled children's watches (retaining explicitly-recorded evidence
  // watches for live fail-isolated children), write the FINAL progress comment, then and only then
  // transition to DONE.
  for (const [ticketId, watch] of Object.entries(s.watches)) {
    if (s.retainedEvidenceWatches.includes(ticketId)) {
      actions.push(`retain-evidence-watch:${ticketId}`)
      continue
    }
    await io.removeWatches({
      ticketId,
      callbackIds: watch.callbacks.map((cb) => cb.id).filter(isNonEmptyString),
      subscriptionId: watch.subscription?.id ?? '',
    })
    actions.push(`cleanup-watch:${ticketId}`)
  }
  s = recordWatchCleanup(s, { at: now })
  const finalUpsert =
    (await io.upsertProgress({
      state: s,
      progressState: buildEpicProgressState(s, { updatedAt: now }),
    })) ?? {}
  s = recordProgressUpsert(s, { commentId: finalUpsert.commentId ?? '', finalAt: now })
  s = transitionToDone(s)
  s = await persist(s)
  actions.push('terminal:done')
  return { state: s, status: s.status, blockers: [], actions }
}

// The status a non-terminal cycle reports: the worst degradation any in-flight child is under.
// BLOCKED beats RUNNING_BUT_UNWATCHED beats RUNNING, because a run reporting the best of its
// children's coverage is a run that reads as observed while a child is not.
function worstNonTerminalStatus(state) {
  if (state.status === EPIC_RUN_STATUSES.BLOCKED) return EPIC_RUN_STATUSES.BLOCKED
  const unwatched = state.inFlight.some((id) => {
    const watch = state.watches[id]
    return !watch || (!isNonEmptyString(watch.verifiedAt) && !watch.fallback)
  })
  return unwatched ? EPIC_RUN_STATUSES.RUNNING_BUT_UNWATCHED : EPIC_RUN_STATUSES.RUNNING
}

function nextWakeSummary(state) {
  const fallbacks = Object.values(state.watches)
    .map((watch) => watch.fallback?.nextWakeAt)
    .filter(isNonEmptyString)
  if (fallbacks.length > 0) return `fallback ${fallbacks.sort()[0]}`
  const verified = Object.values(state.watches).some((watch) => isNonEmptyString(watch.verifiedAt))
  return verified ? 'callback/subscription (verified)' : 'none'
}

/**
 * Recompute the eligible-only ready set and launch into the available concurrency headroom.
 *
 * Extracted because the cycle runs it TWICE — once before the merge step and once after, since a
 * verified merge both unparks dependents and frees a slot. `readyTickets` is the single scheduling
 * authority (cascade-skip happens inside it) and `needsHuman` work is excluded here as well as in
 * normalization, so a ready set handed to the launcher can never contain it.
 */
export async function scheduleReadyWork({ state, io, now = '', actions = [], persist } = {}) {
  let s = normalizeEpicState(state)
  const ready = readyTickets(buildGraph(s.tickets), {
    merged: new Set(s.merged),
    failed: new Set(s.failed),
    inFlight: new Set(s.inFlight),
    externallyCleared: new Set(s.externallyCleared),
  }).filter((ticket) => !s.needsHuman.includes(ticket.id))
  s = normalizeEpicState({ ...s, ready: ready.map((ticket) => ticket.id) })

  const headroom = Math.max(0, s.parallel - s.inFlight.length)
  for (const ticket of ready.slice(0, headroom)) {
    const created = (await io.createSession({ ticket })) ?? {}
    s = recordLaunch(s, {
      ticketId: ticket.id,
      sessionId: created.sessionId,
      chatId: created.chatId,
      at: now,
    })
    actions.push(`launch:${ticket.id}`)
    if (typeof persist === 'function') s = await persist(s)
  }

  // Whatever just launched is no longer ready; whatever could not fit still is, and holds the run
  // open through the terminal guard.
  const remaining = readyTickets(buildGraph(s.tickets), {
    merged: new Set(s.merged),
    failed: new Set(s.failed),
    inFlight: new Set(s.inFlight),
    externallyCleared: new Set(s.externallyCleared),
  }).filter((ticket) => !s.needsHuman.includes(ticket.id))
  return normalizeEpicState({ ...s, ready: remaining.map((ticket) => ticket.id) })
}

/**
 * Arm and LIST-VERIFY durable coverage for one in-flight child, re-arming a consumed or expired
 * registration. Failure is a transition (`recordWatchFailure`), never a silent continue: either a
 * bounded fallback wake is recorded, or the run reports a non-terminal unwatched/blocked status.
 */
export async function ensureDurableCoverage({ state, ticketId, io, now = '', actions = [] } = {}) {
  let s = normalizeEpicState(state)
  const record = s.sessions[ticketId]
  if (!record?.sessionId) return { state: s }

  const watch = s.watches[ticketId] ?? {}
  const existingCallbacks = watch.callbacks ?? []
  const liveRows =
    (await io.listWatches({
      ticketId,
      prNumber: record.prNumber,
      prRepo: record.prRepo,
    })) ?? []
  const liveByTrigger = new Map(
    liveRows.filter((row) => isNonEmptyString(row?.trigger)).map((row) => [row.trigger, row]),
  )
  const rearmNeeded = EPIC_CHILD_PR_TRIGGERS.filter((trigger) =>
    needsRearm({ registration: liveByTrigger.get(trigger), inFlight: true }),
  )

  let callbacks = existingCallbacks
  let armError = ''
  if (rearmNeeded.length > 0) {
    const armed =
      (await io.armWatches({
        ticketId,
        prNumber: record.prNumber,
        prRepo: record.prRepo,
        triggers: rearmNeeded,
        message: buildContinuationPrompt({ epicId: s.epicId, kind: 'callback', runId: s.runId }),
      })) ?? {}
    armError = armed.error ?? ''
    // The list-read is the VERIFICATION. An arm that returned without error but does not appear in
    // the list did not take, and is indistinguishable from one that did unless we look.
    const verifyRows =
      (await io.listWatches({
        ticketId,
        prNumber: record.prNumber,
        prRepo: record.prRepo,
      })) ?? []
    const verifiedTriggers = new Set(verifyRows.map((row) => row?.trigger))
    const stillMissing = rearmNeeded.filter((trigger) => !verifiedTriggers.has(trigger))
    callbacks = verifyRows
      .filter((row) => isNonEmptyString(row?.id))
      .map((row) => ({ id: row.id, trigger: row.trigger ?? '', state: row.state ?? '' }))
    if (stillMissing.length > 0) {
      armError = armError || `list verification found no watch for ${stillMissing.join(',')}`
    }
    actions.push(`arm:${ticketId}:${rearmNeeded.join(',')}${armError ? ':failed' : ':verified'}`)
  }

  // The durable session-outcome subscription, verified the same way and by ownership.
  let subscription = watch.subscription
  let subError = ''
  if (needsRearm({ registration: subscription, inFlight: true })) {
    const created =
      (await io.subscribeSessionOutcome({
        ticketId,
        sessionId: record.sessionId,
        message: buildContinuationPrompt({
          epicId: s.epicId,
          kind: 'subscription',
          runId: s.runId,
        }),
      })) ?? {}
    subError = created.error ?? ''
    const rows = (await io.listSessionSubscriptions({ sessionId: record.sessionId })) ?? []
    const wantedId = created.subscription?.id ?? ''
    const row = rows.find((candidate) => (candidate?.id ?? '') === wantedId) ?? null
    const verdict = row
      ? verifySubscriptionRow(row, { ownerSessionId: record.sessionId })
      : { ok: false, reason: 'subscription absent from the list read' }
    if (verdict.ok) {
      subscription = {
        id: row.id,
        ownerSessionId: row.owner_session_id ?? row.ownerSessionId ?? '',
        originChatId: row.origin_chat_id ?? row.originChatId ?? '',
        triggerEvent: row.trigger_event ?? row.triggerEvent ?? '',
        state: row.state ?? '',
        firedAt: row.fired_at ?? row.firedAt ?? '',
        expiresAt: row.expires_at ?? row.expiresAt ?? '',
      }
      subError = ''
    } else {
      subscription = null
      subError = subError || verdict.reason
    }
    actions.push(`subscribe:${ticketId}${subError ? `:failed:${subError}` : ':verified'}`)
  }

  if (!armError && !subError) {
    return {
      state: recordWatchCoverage(s, {
        ticketId,
        targetChatId: watch.targetChatId ?? '',
        repo: record.prRepo,
        callbacks,
        subscription,
        verifiedAt: now,
      }),
    }
  }

  const reason = [armError, subError].filter(Boolean).join('; ')
  const fallback = (await io.scheduleFallbackWake({ ticketId, reason })) ?? null
  s = recordWatchFailure(s, { ticketId, reason, fallback, at: now, blocked: false })
  actions.push(
    fallback ? `fallback:${ticketId}:${fallback.mechanism}` : `unwatched:${ticketId}:${reason}`,
  )
  return { state: s }
}

function matchSessionForTicket(sessions, ticket) {
  const byTracker = sessions.find((session) => (session?.tracker_id ?? '') === ticket.id)
  if (byTracker) return byTracker
  const prefix = `[${ticket.id}]`
  return sessions.find((session) => String(session?.title ?? '').startsWith(prefix)) ?? null
}

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

function parseFlags(argv) {
  const flags = {}
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i]
    if (!arg.startsWith('--')) continue
    const name = arg.slice(2)
    const next = argv[i + 1]
    if (next === undefined || next.startsWith('--')) flags[name] = true
    else {
      flags[name] = next
      i += 1
    }
  }
  return flags
}

function readJsonFlag(flags, name, fs = defaultFs) {
  const value = flags[name]
  if (typeof value !== 'string' || value === '') return null
  const raw = value === '-' ? fs.readFileSync(0, 'utf8') : fs.readFileSync(value, 'utf8')
  return raw.trim() === '' ? null : JSON.parse(raw)
}

/**
 * The agent-callable verbs. Deliberately the DECISION surface only — `reconcileEpic` needs live
 * I/O and is driven from the scheduling process, not from a shell.
 *
 *   validate  --state <file|->    → `{ok, errors}`
 *   blockers  --state <file|->    → `{blockers, canTerminate}`
 *   status    --state <file|->    → `{status, line}`
 *   prompt    --epic <id> [--kind k] [--run id] → `{message}`
 */
export function main(argv, { fs = defaultFs } = {}) {
  const [cmd, ...rest] = argv
  const flags = parseFlags(rest)

  if (cmd === 'validate') {
    return validateEpicState(normalizeEpicState(readJsonFlag(flags, 'state', fs) ?? {}))
  }
  if (cmd === 'blockers') {
    const state = normalizeEpicState(readJsonFlag(flags, 'state', fs) ?? {})
    const blockers = epicTerminalBlockers(state)
    return { blockers, canTerminate: blockers.length === 0 }
  }
  if (cmd === 'status') {
    const state = normalizeEpicState(readJsonFlag(flags, 'state', fs) ?? {})
    return { status: state.status, line: renderRunStatusLine(state) }
  }
  if (cmd === 'prompt') {
    return {
      message: buildContinuationPrompt({
        epicId: typeof flags.epic === 'string' ? flags.epic : '',
        kind: typeof flags.kind === 'string' ? flags.kind : 'callback',
        runId: typeof flags.run === 'string' ? flags.run : '',
      }),
    }
  }
  throw new Error(
    `unknown command: ${cmd ?? '(none)'} (expected "validate", "blockers", "status" or "prompt")`,
  )
}

if (isMainModule(import.meta.url)) {
  try {
    process.stdout.write(`${JSON.stringify(main(process.argv.slice(2)))}\n`)
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`)
    process.exitCode = 1
  }
}
