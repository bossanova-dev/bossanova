// The host-facing entry point for a boss-epic run: `init` writes a fresh run's state, `reconcile`
// runs exactly one `reconcileEpic` cycle against live I/O (`epic-io.mjs`) and prints what the host
// must still do. Every wake runs the same `reconcile` call; the wake kind selects nothing.
//
//   init      --tickets-file F --planned-state S --epic ID --repo-id R --state-dir D
//             [--review-state S] [--agent A] [--parallel N] [--wall-clock-minutes M]
//             [--run-id ID] [--selection-file F] [--now ISO]
//   reconcile --state-file F --repo-id R --pr-repo owner/repo --driver-chat CHAT
//             [--wake kind] [--boss PATH] [--model M] [--prompt-suffix TEXT]
//             [--tracker-source S] [--done-state S] [--blocker-states-file F] [--tickets-file F]
//             [--require-status CONTEXT]   e.g. boss/verify: merge only what the repo's gate passed
//             [--fallback-mechanism M --fallback-wake-at ISO] [--now ISO]
//
// `reconcile` prints `{status, blockers, actions, routes, deferred, warnings}`:
//   - `routes`: children whose liveness action (resume / repair / investigate) the host carries out;
//   - `deferred`: tracker operations with MCP transport, for the host to execute once each.

import { randomUUID } from 'node:crypto'
import { mkdirSync, readFileSync, renameSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'
import { loadSkillConfig, optionalLabelName } from '../skill-config.mjs'
import { resolveTrackerAdapter } from '../tracker/adapter.mjs'
import { buildGraph } from './dag-scheduler.mjs'
import * as epicDriver from './epic-driver.mjs'
import { scheduleSnapshot, upsertProgress } from './epic-cli.mjs'
import { createEpicIo } from './epic-io.mjs'

// epic-driver.mjs is on the js-types baseline, so its inferred parameter types are wrong; type
// the boundary once here instead of casting at each call.
/** @type {any} */
const driver = epicDriver
const {
  createEpicState,
  epicStatePath,
  normalizeEpicState,
  reconcileEpic,
  recordFailure,
  recordMerge,
  validateEpicState,
  EPIC_WAKE_KINDS,
} = driver

const LIVENESS_ROUTE = /^liveness:([^:]+):([^/]+)\/(resume|repair|investigate)$/

/** Persist by validate + temp file + rename, so a crash never leaves a truncated state. */
export function writeStateFile(file, state) {
  const normalized = normalizeEpicState(state)
  const verdict = validateEpicState(normalized)
  if (!verdict.ok)
    throw new Error(`refusing to persist invalid state — ${verdict.errors.join('; ')}`)
  mkdirSync(path.dirname(file), { recursive: true })
  const temp = `${file}.tmp`
  writeFileSync(temp, `${JSON.stringify(normalized, null, 2)}\n`, 'utf8')
  renameSync(temp, file)
  return normalized
}

/** Read and validate a state file; a corrupt file throws rather than reading as a fresh run. */
export function readStateFile(file, read = (f) => readFileSync(f, 'utf8')) {
  const parsed = JSON.parse(read(file))
  const verdict = validateEpicState(parsed)
  if (!verdict.ok) throw new Error(`state file ${file} is invalid — ${verdict.errors.join('; ')}`)
  return normalizeEpicState(parsed)
}

/** External blocker ids the cycle will read: every non-member blocker not yet merged. */
export function externalBlockerIds(state) {
  const graph = buildGraph(state.tickets)
  return [
    ...new Set(
      [...graph.externalBlockers.values()].flat().filter((id) => !state.merged.includes(id)),
    ),
  ]
}

/**
 * Fold in provider outcomes the driver did not cause: a child PR merged by another actor (a verify
 * gate, a human) is a landed merge, so finish its bookkeeping; a child PR closed unmerged can never
 * go green, so fail-isolate it rather than wait out its wall clock.
 */
export function reconcileProviderOutcomes(state, { prState, moveTicketDone }) {
  let s = normalizeEpicState(state)
  const actions = []
  for (const ticketId of [...s.inFlight]) {
    const number = s.sessions[ticketId]?.prNumber
    if (!Number.isFinite(number)) continue
    const observed = prState(number)
    if (observed === 'MERGED') {
      s = recordMerge(s, { ticketId, verified: true })
      const moved = moveTicketDone({ ticketId }) === true
      actions.push(`external-merge:${ticketId}${moved ? '' : ':ticket-writeback-failed'}`)
    } else if (observed === 'CLOSED') {
      s = recordFailure(s, {
        ticketId,
        reason: `PR #${number} closed without merging`,
        retainEvidenceWatch: true,
      })
      actions.push(`external-close:${ticketId}`)
    }
  }
  return { state: s, actions }
}

/** The liveness actions a cycle leaves to the host, one per child. */
export function livenessRoutes(state, actions) {
  return actions
    .map((action) => LIVENESS_ROUTE.exec(action))
    .filter(Boolean)
    .filter(([, ticketId]) => state.inFlight.includes(ticketId))
    .map(([, ticketId, verdict, action]) => ({
      ticketId,
      verdict,
      action,
      sessionId: state.sessions[ticketId]?.sessionId ?? '',
      chatId: state.sessions[ticketId]?.chatId ?? '',
      prNumber: state.sessions[ticketId]?.prNumber ?? null,
    }))
}

/** `{id: url}` from raw tracker tickets (an array or a listChildren result). */
export function ticketUrlMap(input) {
  const rows = Array.isArray(input) ? input : (input?.result?.children ?? input?.children ?? [])
  return Object.fromEntries(
    rows
      .map((row) => [row?.identifier ?? row?.id, row?.url])
      .filter(([id, url]) => typeof id === 'string' && typeof url === 'string' && url),
  )
}

function trackerSourceFor(name) {
  return ['linear', 'sentry'].includes(name) ? name : ''
}

export async function runInit(values, deps = {}) {
  const read = deps.readJson ?? ((file) => JSON.parse(readFileSync(file, 'utf8')))
  const config = deps.config ?? loadSkillConfig()
  const now = values.now ?? new Date().toISOString()
  const snapshot = scheduleSnapshot({
    tickets: read(values['tickets-file']),
    plannedState: values['planned-state'],
    selection: values['selection-file'] ? read(values['selection-file']) : null,
    agentBuildLabel: optionalLabelName(config, 'agentBuild') ?? undefined,
    needsHumanLabel: optionalLabelName(config, 'needsHuman') ?? undefined,
  })
  const runId =
    values['run-id'] ?? `run-${now.replace(/[^0-9]/g, '').slice(0, 14)}-${randomUUID().slice(0, 8)}`
  const state = normalizeEpicState({
    ...createEpicState({
      runId,
      epicId: values.epic,
      repoId: values['repo-id'],
      agent: values.agent ?? 'claude',
      parallel: Number(values.parallel ?? 4),
      plannedState: values['planned-state'],
      reviewState: values['review-state'] ?? '',
      childWallClockMinutes: Number(values['wall-clock-minutes'] ?? 360),
      startedAt: now,
      tickets: snapshot.eligible,
    }),
    externallyCleared: snapshot.externallyCleared,
    ready: snapshot.ready,
    phase: 'polling',
  })
  const file = epicStatePath({ epicId: values.epic, runId, dir: values['state-dir'] })
  const write = deps.writeState ?? writeStateFile
  write(file, state)
  return {
    outcome: 'initialized',
    statePath: file,
    runId,
    eligible: snapshot.eligible.map((ticket) => ticket.id),
    ready: snapshot.ready,
    done: snapshot.done.map((ticket) => ticket.id),
    skipped: snapshot.skipped,
  }
}

export async function runReconcile(values, deps = {}) {
  const file = values['state-file']
  const state = (deps.readState ?? readStateFile)(file)
  const read = deps.readJson ?? ((f) => JSON.parse(readFileSync(f, 'utf8')))
  const blockerStates = values['blocker-states-file'] ? read(values['blocker-states-file']) : {}
  const missing = externalBlockerIds(state).filter((id) => !Object.hasOwn(blockerStates, id))
  if (missing.length > 0) {
    return {
      outcome: 'blocker-states-required',
      required: missing,
      message: `read each external blocker's tracker state and pass --blocker-states-file ({id: "cleared"|"open"}): ${missing.join(', ')}`,
    }
  }
  const env = deps.env ?? process.env
  const config = deps.config ?? loadSkillConfig()
  const trackerName = env.TRACKER ?? config?.adapters?.tracker ?? 'linear'
  const now = values.now ?? new Date().toISOString()
  const fallbackWake =
    values['fallback-mechanism'] && values['fallback-wake-at']
      ? { mechanism: values['fallback-mechanism'], nextWakeAt: values['fallback-wake-at'] }
      : null
  const trackerAdapter = deps.adapter ?? resolveTrackerAdapter({ env })
  // Normalized state tickets carry no URL; the raw tickets file, when given, supplies it.
  const ticketUrls = values['tickets-file'] ? ticketUrlMap(read(values['tickets-file'])) : {}
  const built = createEpicIo({
    ticketUrls,
    requiredStatus: values['require-status'] ?? '',
    state,
    repoId: values['repo-id'],
    prRepo: values['pr-repo'],
    driverChatId: values['driver-chat'],
    boss: values.boss ?? env.BOSS ?? 'boss',
    cwd: deps.cwd,
    model: values.model ?? '',
    promptSuffix: values['prompt-suffix'] ?? '',
    trackerSource: values['tracker-source'] ?? trackerSourceFor(trackerName),
    trackerAdapter,
    doneState: values['done-state'] ?? 'completed',
    blockerStates,
    fallbackWake,
    upsertProgressImpl: deps.upsertProgress ?? upsertProgress,
    exec: deps.exec,
    runOperationImpl: deps.runOperation,
    now,
    sleep: deps.sleep,
  })
  const { io, deferred, warnings, tracker } = built
  const write = deps.writeState ?? writeStateFile
  const save = (next) => write(file, next)

  const pre = reconcileProviderOutcomes(state, {
    prState: (number) => built.prView(number)?.state ?? '',
    moveTicketDone: io.moveTicketDone,
  })
  let current = pre.actions.length > 0 ? save(pre.state) : pre.state

  const wake = EPIC_WAKE_KINDS.includes(values.wake) ? values.wake : 'manual-resume'
  const cycle = await reconcileEpic({ state: current, wake, io, now, save })
  current = cycle.state

  // Green admission does not move the ticket; the run does, so the board shows review-ready work.
  const reviewState = current.reviewState
  const reviewMoves = []
  if (reviewState) {
    for (const ticketId of current.greens) {
      try {
        tracker('moveState', { id: ticketId, state: reviewState })
        reviewMoves.push(ticketId)
      } catch (error) {
        warnings.push(`${ticketId}: move to ${reviewState} failed: ${error.message}`)
      }
    }
  }

  return {
    outcome: 'reconciled',
    status: cycle.status,
    blockers: cycle.blockers,
    actions: [...pre.actions, ...cycle.actions],
    routes: livenessRoutes(current, cycle.actions),
    reviewMoves,
    deferred,
    warnings,
    inFlight: current.inFlight,
    greens: current.greens,
    merged: current.merged,
    failed: current.failed,
    cascadeSkipped: current.cascadeSkipped,
    ready: current.ready,
    statePath: file,
  }
}

const OPTIONS = {
  init: {
    'tickets-file': { type: 'string' },
    'planned-state': { type: 'string' },
    'review-state': { type: 'string' },
    epic: { type: 'string' },
    'repo-id': { type: 'string' },
    agent: { type: 'string' },
    parallel: { type: 'string' },
    'wall-clock-minutes': { type: 'string' },
    'state-dir': { type: 'string' },
    'run-id': { type: 'string' },
    'selection-file': { type: 'string' },
    now: { type: 'string' },
  },
  reconcile: {
    'state-file': { type: 'string' },
    'repo-id': { type: 'string' },
    'pr-repo': { type: 'string' },
    'driver-chat': { type: 'string' },
    wake: { type: 'string' },
    boss: { type: 'string' },
    model: { type: 'string' },
    'prompt-suffix': { type: 'string' },
    'tracker-source': { type: 'string' },
    'done-state': { type: 'string' },
    'blocker-states-file': { type: 'string' },
    'tickets-file': { type: 'string' },
    'require-status': { type: 'string' },
    'fallback-mechanism': { type: 'string' },
    'fallback-wake-at': { type: 'string' },
    now: { type: 'string' },
  },
}
const REQUIRED = {
  init: ['tickets-file', 'planned-state', 'epic', 'repo-id', 'state-dir'],
  reconcile: ['state-file', 'repo-id', 'pr-repo', 'driver-chat'],
}

export async function main(argv, deps = {}) {
  const fail = (message, exitCode) => ({
    exitCode,
    stdout: '',
    stderr: `epic-run: ${String(message).replace(/[\r\n]+/g, ' ')}\n`,
  })
  const [cmd, ...rest] = argv
  /** @type {Record<string, any>} */
  let values
  try {
    if (!Object.hasOwn(OPTIONS, cmd)) throw new Error('expected init or reconcile')
    values = parseArgs({ args: rest, strict: true, options: OPTIONS[cmd] }).values
    for (const flag of REQUIRED[cmd]) if (!values[flag]) throw new Error(`--${flag} is required`)
    if (values.wake !== undefined && !EPIC_WAKE_KINDS.includes(values.wake)) {
      throw new Error(`--wake must be one of ${EPIC_WAKE_KINDS.join(', ')}`)
    }
  } catch (error) {
    return fail(error.message, 64)
  }
  try {
    const result =
      cmd === 'reconcile'
        ? await runReconcile(values, deps)
        : cmd === 'init'
          ? await runInit(values, deps)
          : null
    const exitCode = result.outcome === 'blocker-states-required' ? 3 : 0
    return { exitCode, stdout: `${JSON.stringify(result)}\n`, stderr: '' }
  } catch (error) {
    return fail(error.message, 2)
  }
}

if (isMainModule(import.meta.url)) {
  const result = await main(process.argv.slice(2))
  process.stdout.write(result.stdout)
  process.stderr.write(result.stderr)
  process.exitCode = result.exitCode
}
