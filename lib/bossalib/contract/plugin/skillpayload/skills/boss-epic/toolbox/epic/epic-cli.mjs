import { readFileSync } from 'node:fs'
import { parseArgs } from 'node:util'
import { loadSkillConfig, optionalLabelName } from '../skill-config.mjs'
import { isMainModule } from '../main-module.mjs'
import { resolveTrackerAdapter } from '../tracker/adapter.mjs'
import { resolveOperation, runOperation } from '../tracker/operation-runner.mjs'
import { normalizeTicket, classifyTickets } from './bs-epic-lib.mjs'
import { buildGraph, readyTickets, nextToMerge } from './dag-scheduler.mjs'
import { normalizeEpicState, validateEpicState, buildEpicProgressState } from './epic-driver.mjs'
import {
  renderProgressComment,
  projectProgressByParent,
  planProgressCommentUpsert,
  validateProgressState,
} from './progress-comment.mjs'

/** @param {any} input */
function checkedState(input) {
  // Validate the persisted shape first: normalization would erase corrupt collections.
  const validity = validateEpicState(input)
  if (!validity.ok) throw new Error(validity.errors.join('; '))
  return normalizeEpicState(input)
}

/** @param {any} input */
function ticketRows(input) {
  const rows = Array.isArray(input) ? input : (input?.result?.children ?? input?.children)
  if (
    !Array.isArray(rows) ||
    rows.some((row) => !row || typeof row !== 'object' || Array.isArray(row))
  ) {
    throw new Error('tickets must be an array or a listChildren result with a children array')
  }
  return rows
}

/** Compose the scheduler without leaking its Map/Set collections.
 * @param {{tickets: any, plannedState: string, state?: object, assumeCleared?: string[], assumeClearedAndMerge?: string[], selection?: object|null, agentBuildLabel?: string, needsHumanLabel?: string}} input
 */
export function scheduleSnapshot({
  tickets,
  plannedState,
  state,
  assumeCleared = [],
  assumeClearedAndMerge = [],
  selection = null,
  agentBuildLabel,
  needsHumanLabel,
}) {
  const saved = state === undefined ? null : checkedState(state)
  const { eligible, done, skipped } = classifyTickets(
    ticketRows(tickets).map(normalizeTicket),
    plannedState,
    { selection, agentBuildLabel, needsHumanLabel },
  )
  const graph = buildGraph(eligible)
  const externallyCleared = new Set([
    ...done.map((ticket) => ticket.id),
    ...assumeCleared,
    ...assumeClearedAndMerge,
    ...(saved?.externallyCleared ?? []),
  ])
  const merged = new Set(saved?.merged ?? [])
  const needsHuman = new Set(saved?.needsHuman ?? [])
  const ready = readyTickets(graph, {
    merged,
    failed: new Set(saved?.failed ?? []),
    inFlight: new Set(saved?.inFlight ?? []),
    externallyCleared,
  })
    .filter((ticket) => !needsHuman.has(ticket.id))
    .map((ticket) => ticket.id)
  const greens = (saved?.greens ?? []).map((id) => graph.nodes.get(id) ?? { id })
  return {
    eligible: eligible.map(jsonTicket),
    done: done.map(jsonTicket),
    skipped: skipped.map(({ ticket, reason }) => ({ id: ticket.id, title: ticket.title, reason })),
    ready,
    nextToMerge: nextToMerge(greens, graph, merged),
    externallyCleared: [...externallyCleared],
    externalBlockersByTicket: Object.fromEntries(
      [...graph.externalBlockers]
        .map(([id, blockers]) => [
          id,
          blockers.filter((blocker) => !externallyCleared.has(blocker)),
        ])
        .filter(([, blockers]) => blockers.length),
    ),
    clearedForMerge: [...new Set(assumeClearedAndMerge)],
  }
}

/** @param {object} ticket */
function jsonTicket(ticket) {
  return Object.fromEntries(Object.entries(ticket).filter(([, value]) => value !== undefined))
}

/** @param {any} input */
function commentRows(input) {
  if (Array.isArray(input)) return input
  if (input && typeof input === 'object' && Array.isArray(input.comments)) return input.comments
  throw new Error('comments must be an array or an object with a comments array')
}

/**
 * @typedef {{adapter?: {operationMap: object}, runOperation?: typeof runOperation}} TrackerDeps
 * @typedef {{issueId: string, op?: string, commentId?: string, outcome?: string, tool?: string, args?: object, warning?: string, error?: string}} ProgressResult
 */

/** Execute command operations or return MCP descriptors; never mutate persisted state.
 * @param {{state: object, issueId?: string, membership?: {parentIds: string[], childrenByParent?: Record<string, string[]>}, comments?: any, now?: () => string, nextWake?: string}} input
 * @param {TrackerDeps} [deps]
 */
export async function upsertProgress(
  { state, issueId, membership, comments, now = () => new Date().toISOString(), nextWake = '' },
  deps = {},
) {
  const saved = checkedState(state)
  const progress = buildEpicProgressState(saved, { updatedAt: now(), nextWake })
  const validity = validateProgressState(progress)
  if (!validity.ok) throw new Error(validity.errors.join('; '))
  const targets = membership
    ? projectProgressByParent({ state: progress, ...membership })
    : [{ parentId: issueId ?? saved.epicId, state: progress }]
  const adapter = deps.adapter ?? resolveTrackerAdapter()
  const execute = deps.runOperation ?? runOperation
  /** @type {ProgressResult[]} */
  const results = []
  for (const { parentId, state: projected } of targets) {
    try {
      const read = resolveOperation(adapter.operationMap, 'readComments')
      if (!read) throw new Error('readComments has no usable operation')
      if (comments === undefined && read.transport === 'mcp') {
        results.push({
          issueId: parentId,
          tool: read.tool,
          args: { issueId: parentId },
          outcome: 'comments-required',
        })
        continue
      }
      const supplied = membership ? comments?.[parentId] : comments
      const existing =
        comments === undefined ? (await execute(read, { issueId: parentId })).result : supplied
      const body = renderProgressComment(projected)
      const plan = planProgressCommentUpsert({
        comments: commentRows(existing),
        marker: projected.marker,
        body,
      })
      const operation = resolveOperation(
        adapter.operationMap,
        plan.op === 'create' ? 'writeComment' : 'updateComment',
      )
      if (!operation) throw new Error(`${plan.op} has no usable operation`)
      const args = plan.op === 'create' ? { issueId: parentId, body } : { id: plan.commentId, body }
      const details = {
        issueId: parentId,
        op: plan.op,
        ...(plan.warning ? { warning: plan.warning } : {}),
      }
      if (operation.transport === 'mcp') {
        results.push({
          ...details,
          ...(plan.op === 'update' ? { commentId: plan.commentId } : {}),
          tool: operation.tool,
          args,
          outcome: 'descriptor-emitted',
        })
      } else {
        const response = await execute(operation, args)
        const commentId = response.result?.id
        if (typeof commentId !== 'string' || !commentId.trim())
          throw new Error('comment operation returned no usable id')
        results.push({ ...details, commentId, outcome: 'progress-upserted' })
      }
    } catch (error) {
      results.push({ issueId: parentId, error: error.message })
    }
  }
  return {
    results,
    outcome: results.some((row) => row.error)
      ? 'failed'
      : results.every((row) => row.outcome === 'progress-upserted')
        ? 'progress-upserted'
        : 'descriptor-emitted',
  }
}

/**
 * @param {string[]} argv
 * @param {TrackerDeps & {readJson?: (file: string) => any, config?: object, env?: object, now?: () => string}} [deps]
 * @returns {Promise<{exitCode: number, stdout: string, stderr: string}>}
 */
export async function main(argv, deps = {}) {
  const fail = (message, exitCode) => ({
    exitCode,
    stdout: '',
    stderr: `epic-cli: ${String(message).replace(/[\r\n]+/g, ' ')}\n`,
  })
  const [cmd, ...rest] = argv
  /** @type {{'tickets-file'?: string, 'planned-state'?: string, 'state-file'?: string,
   * 'assume-cleared'?: string[], 'assume-cleared-and-merge'?: string[], 'selection-file'?: string,
   * issue?: string, 'membership-file'?: string, 'comments-file'?: string, 'updated-at'?: string, 'next-wake'?: string}} */
  let values
  try {
    if (cmd !== 'schedule' && cmd !== 'upsert-progress')
      throw new Error('expected schedule or upsert-progress')
    /** @type {Record<string, {type: 'string', multiple?: boolean}>} */
    const options =
      cmd === 'schedule'
        ? {
            'tickets-file': { type: 'string' },
            'planned-state': { type: 'string' },
            'state-file': { type: 'string' },
            'assume-cleared': { type: 'string', multiple: true },
            'assume-cleared-and-merge': { type: 'string', multiple: true },
            'selection-file': { type: 'string' },
          }
        : {
            'state-file': { type: 'string' },
            issue: { type: 'string' },
            'membership-file': { type: 'string' },
            'comments-file': { type: 'string' },
            'updated-at': { type: 'string' },
            'next-wake': { type: 'string' },
          }
    values = /** @type {typeof values} */ (parseArgs({ args: rest, strict: true, options }).values)
    const required = cmd === 'schedule' ? ['tickets-file', 'planned-state'] : ['state-file']
    for (const flag of required) if (!values[flag]) throw new Error(`--${flag} is required`)
  } catch (error) {
    return fail(error.message, 64)
  }
  try {
    const read = deps.readJson ?? ((file) => JSON.parse(readFileSync(file, 'utf8')))
    const state = values['state-file'] ? read(values['state-file']) : undefined
    let result
    if (cmd === 'schedule') {
      const config = deps.config ?? loadSkillConfig()
      const selectionText = (deps.env ?? process.env).BOSS_EPIC_SELECTION
      result = scheduleSnapshot({
        agentBuildLabel: optionalLabelName(config, 'agentBuild') ?? undefined,
        needsHumanLabel: optionalLabelName(config, 'needsHuman') ?? undefined,
        tickets: read(values['tickets-file']),
        plannedState: values['planned-state'],
        state,
        assumeCleared: values['assume-cleared'],
        assumeClearedAndMerge: values['assume-cleared-and-merge'],
        selection: values['selection-file']
          ? read(values['selection-file'])
          : selectionText
            ? JSON.parse(selectionText).selection
            : null,
      })
    } else {
      result = await upsertProgress(
        {
          state,
          issueId: values.issue,
          membership: values['membership-file'] ? read(values['membership-file']) : undefined,
          comments: values['comments-file'] ? read(values['comments-file']) : undefined,
          now: values['updated-at'] ? () => values['updated-at'] : deps.now,
          nextWake: values['next-wake'],
        },
        {
          ...deps,
          adapter: deps.adapter ?? resolveTrackerAdapter({ env: deps.env ?? process.env }),
        },
      )
    }
    return {
      exitCode: result.outcome === 'failed' ? 2 : 0,
      stdout: JSON.stringify(result) + '\n',
      stderr: '',
    }
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
