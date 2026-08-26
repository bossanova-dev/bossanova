// bs-dispatch-await.mjs
//
// Shared awaited-dispatch contract for published boss-* skills.
//
// The four rules are the single editable source every consuming core cites:
// the terminal artefact is the only completion oracle; an absent artefact means
// still-running, never finished; a launcher's exit status is not the job's
// status; and a timeout is reported distinctly from a clean empty result.
// Awaiting means staying in the turn and re-reading through this helper; ending
// the turn is not waiting.
//
// Agent bindings for the neutral dispatch contract:
// - Claude Code: issue awaited `Task` calls (for example with `subagent_type: general-purpose`);
//   never treat a `run_in_background` launcher result as completion.
// - Codex: use `spawn_agent` to create a fresh subagent and `wait_agent` to await its terminal
//   result before consuming the extension output.
//
// A discovered extension is dispatched whenever the running agent exposes an awaited-dispatch
// mechanism. Inline execution is reachable only through the documented tier fallback for that core,
// and every inline fallback writes a ledger line naming the tier and reason.
//
// Node built-ins only — cron worktrees are dependency-free.

import { readdirSync, statSync } from 'node:fs'
import { join, basename } from 'node:path'
import { setTimeout as delay } from 'node:timers/promises'
import { buildGraph, readyTickets } from './dag-scheduler.mjs'
import {
  DISPATCH_FAILURE,
  PROVISIONAL_KEY,
  readSentinel,
  writeSentinel,
} from './bs-run-sentinel.mjs'
import { isMainModule } from './main-module.mjs'

export const DISPATCH_AWAIT_RESULTS = ['completed', 'still-running', 'timed-out', 'abandoned']
export const COMPLETED = 'completed'
export const STILL_RUNNING = 'still-running'
export const TIMED_OUT = 'timed-out'
export const ABANDONED = 'abandoned'

export const DEFAULT_DISPATCH_LEG_TIMEOUT_MS = 300_000
export const DEFAULT_AWAIT_TIMEOUT_MULTIPLIER = 1.25
export const DEFAULT_OPEN_DISPATCH_STALE_MS = 30 * 60 * 1000
// Default for callers that do not supply a width. Merged review barriers can assemble more than four
// read-only nodes, so those callers pass the admitted roster size explicitly; this fallback remains
// the conservative width for unclassified dispatch graphs.
export const MAX_BATCH_WIDTH = 4
export const DEFAULT_POLL_INTERVAL_MS = 1_000

function assertPositiveNumber(value, name) {
  if (!Number.isFinite(value) || value <= 0) throw new Error(`${name} must be positive`)
  return value
}

export function legTimeoutMsFromEnv(env = process.env) {
  const raw = env.BOSS_SKILL_EXTENSION_TIMEOUT_MS ?? `${DEFAULT_DISPATCH_LEG_TIMEOUT_MS}`
  if (!/^[0-9]+$/.test(raw)) throw new Error('BOSS_SKILL_EXTENSION_TIMEOUT_MS must be digits')
  return assertPositiveNumber(Number.parseInt(raw, 10), 'BOSS_SKILL_EXTENSION_TIMEOUT_MS')
}

export function awaitDeadlineMs(opts = {}) {
  const legMs = assertPositiveNumber(
    opts.legTimeoutMs ?? legTimeoutMsFromEnv(opts.env),
    'legTimeoutMs',
  )
  const multiplier = assertPositiveNumber(
    opts.multiplier ?? DEFAULT_AWAIT_TIMEOUT_MULTIPLIER,
    'multiplier',
  )
  return Math.ceil(legMs * multiplier)
}

function ctxFor(dir, runId) {
  return { runId, dir, sentinelPath: (name) => join(dir, `${name}.json`) }
}

function readOpenedSentinel(ctx, name) {
  const read = readSentinel(ctx, name)
  if (read.status !== 'ok') return read
  if (read.payload?.[PROVISIONAL_KEY] === true) return { ...read, status: 'provisional' }
  return read
}

export function classifyDispatch(ctx, name, opts = {}) {
  const now = opts.now ?? Date.now()
  const deadlineAt = opts.deadlineAt ?? now + awaitDeadlineMs(opts)
  const staleAfterMs = opts.staleAfterMs ?? DEFAULT_OPEN_DISPATCH_STALE_MS
  const read = readOpenedSentinel(ctx, name)
  if (read.status === 'ok') {
    return { status: COMPLETED, name, kind: read.kind, payload: read.payload ?? {} }
  }

  const path = ctx.sentinelPath(name)
  let openedAt = opts.openedAt
  try {
    openedAt ??= statSync(path).mtimeMs
  } catch {
    openedAt ??= opts.dispatchedAt ?? now
  }
  const ageMs = Math.max(0, now - openedAt)
  if (ageMs >= staleAfterMs) return { status: ABANDONED, name, ageMs }
  if (now >= deadlineAt) return { status: TIMED_OUT, name, ageMs }
  return { status: STILL_RUNNING, name, ageMs }
}

export function toSentinelRouting(result) {
  if (result?.status === COMPLETED) return result.kind
  if (result?.status === TIMED_OUT || result?.status === ABANDONED) return DISPATCH_FAILURE
  return null
}

function sentinelNames(runDir) {
  return readdirSync(runDir)
    .filter((name) => name.endsWith('.json'))
    .map((name) => name.slice(0, -'.json'.length))
}

export function openDispatches(ctx, opts = {}) {
  const now = opts.now ?? Date.now()
  const staleAfterMs = opts.staleAfterMs ?? DEFAULT_OPEN_DISPATCH_STALE_MS
  const names = opts.names ?? sentinelNames(ctx.dir)
  const opened = []
  for (const name of names) {
    const read = readOpenedSentinel(ctx, name)
    if (read.status !== 'missing' && read.status !== 'stale' && read.status !== 'provisional') {
      continue
    }
    let ageMs = 0
    try {
      ageMs = Math.max(0, now - statSync(ctx.sentinelPath(name)).mtimeMs)
    } catch {
      ageMs = Math.max(0, now - (opts.dispatchedAt ?? now))
    }
    opened.push({ name, ageMs, stale: ageMs >= staleAfterMs })
  }
  return opened.sort((a, b) => a.name.localeCompare(b.name))
}

function normalizeNode(node, index) {
  if (!node || typeof node.id !== 'string' || node.id.length === 0) {
    throw new Error('dispatch node id is required')
  }
  return {
    id: node.id,
    blockedBy: node.blockedBy ?? [],
    priority: 0,
    createdAt: new Date(index).toISOString(),
    original: node,
  }
}

function uniqueIds(nodes) {
  const ids = new Set()
  for (const node of nodes) {
    if (ids.has(node.id)) throw new Error(`duplicate dispatch id: ${node.id}`)
    ids.add(node.id)
  }
  return ids
}

function asArray(value) {
  if (!value) return []
  if (!Array.isArray(value)) throw new Error('dispatch path declarations must be arrays')
  return value
}

function pathSet(node, key) {
  return new Set(asArray(node[key]).filter(Boolean))
}

function intersects(a, b) {
  for (const value of a) if (b.has(value)) return true
  return false
}

function conflicts(a, b) {
  const aMutates = pathSet(a, 'mutates')
  const bMutates = pathSet(b, 'mutates')
  if ((a.mutates && aMutates.size === 0) || (b.mutates && bMutates.size === 0)) return true
  if (intersects(aMutates, bMutates)) return true
  if (intersects(aMutates, pathSet(b, 'reads'))) return true
  if (intersects(bMutates, pathSet(a, 'reads'))) return true
  return Boolean(a.outPath && b.outPath && a.outPath === b.outPath)
}

function partitionReady(ready, maxWidth) {
  const waves = []
  for (const node of ready) {
    let placed = false
    for (const wave of waves) {
      if (wave.length >= maxWidth) continue
      if (wave.every((existing) => !conflicts(existing, node))) {
        wave.push(node)
        placed = true
        break
      }
    }
    if (!placed) waves.push([node])
  }
  return waves
}

export function planBatches(dispatchNodes, opts = {}) {
  if (!Array.isArray(dispatchNodes)) throw new Error('dispatchNodes must be an array')
  const maxWidth = assertPositiveNumber(opts.maxWidth ?? MAX_BATCH_WIDTH, 'maxWidth')
  const normalized = dispatchNodes.map(normalizeNode)
  const ids = uniqueIds(normalized)
  for (const node of normalized) {
    const unknown = node.blockedBy.filter((id) => !ids.has(id))
    if (unknown.length > 0) {
      throw new Error(`unknown dispatch blocker for ${node.id}: ${unknown.join(', ')}`)
    }
  }

  const graph = buildGraph(normalized)
  const merged = new Set()
  const waves = []
  while (merged.size < normalized.length) {
    const ready = readyTickets(graph, {
      merged,
      failed: new Set(),
      inFlight: new Set(),
      externallyCleared: new Set(),
    })
    if (ready.length === 0) {
      const remaining = normalized.map((node) => node.id).filter((id) => !merged.has(id))
      throw new Error(`unschedulable dispatch graph: ${remaining.join(', ')}`)
    }
    const partitions = partitionReady(
      ready.map((node) => node.original),
      maxWidth,
    )
    for (const wave of partitions) {
      waves.push(wave)
      for (const node of wave) merged.add(node.id)
    }
  }
  return waves
}

export async function awaitAll(dispatchNodes, dispatcher, opts = {}) {
  if (typeof dispatcher !== 'function') throw new Error('dispatcher must be a function')
  const batches = planBatches(dispatchNodes, opts)
  const results = []
  for (const batch of batches) {
    results.push(await Promise.all(batch.map((node) => dispatcher(node))))
    if (opts.betweenBatchesMs) await delay(opts.betweenBatchesMs)
  }
  return { batches, results }
}

function usage() {
  return [
    'usage: bs-dispatch-await.mjs <classify <dir> <runId> <name> <deadlineAtMs> [nowMs] | open <dir> <runId> [nowMs] | batches <json>>',
  ].join('\n')
}

if (isMainModule(import.meta.url)) {
  const [cmd, ...rest] = process.argv.slice(2)
  const fail = (msg) => {
    process.stderr.write(`${msg}\n`)
    process.exit(2)
  }
  try {
    if (cmd === 'classify') {
      const [dir, runId, name, deadlineAtRaw, nowRaw] = rest
      if (!dir || !runId || !name || !deadlineAtRaw)
        fail('classify requires <dir> <runId> <name> <deadlineAtMs> [nowMs]')
      process.stdout.write(
        `${JSON.stringify(
          classifyDispatch(ctxFor(dir, runId), name, {
            deadlineAt: Number(deadlineAtRaw),
            now: nowRaw ? Number(nowRaw) : Date.now(),
          }),
        )}\n`,
      )
    } else if (cmd === 'open') {
      const [dir, runId, nowRaw] = rest
      if (!dir || !runId) fail('open requires <dir> <runId> [nowMs]')
      process.stdout.write(
        `${JSON.stringify(openDispatches(ctxFor(dir, runId), { now: nowRaw ? Number(nowRaw) : Date.now() }))}\n`,
      )
    } else if (cmd === 'batches') {
      const [json] = rest
      if (!json) fail('batches requires <json>')
      process.stdout.write(`${JSON.stringify(planBatches(JSON.parse(json)))}\n`)
    } else if (cmd === 'seed') {
      const [dir, runId, name, kind] = rest
      if (!dir || !runId || !name || !kind) fail('seed requires <dir> <runId> <name> <kind>')
      process.stdout.write(
        `${writeSentinel(ctxFor(dir, runId), name, kind, { [PROVISIONAL_KEY]: true })}\n`,
      )
    } else {
      fail(usage())
    }
  } catch (err) {
    fail(err.message)
  }
}
