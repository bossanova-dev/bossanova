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

export { DISPATCH_FAILURE }

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

// The single publishable-verdict + recovery disposition. `toSentinelRouting` above collapses
// `timed-out` and `abandoned` into one `dispatch-failure` token, which is right for ROUTING and
// lossy for RECOVERY: the two differ precisely in whether resuming the existing dispatch is worth
// attempting. This is the vocabulary that keeps both questions separable at one call site.
export const DISPATCH_DISPOSITIONS = ['publish', 'wait', 'resume', 'discard']
export const DISPOSITION_REASONS = [
  'derived-verdict',
  'provisional-payload',
  'still-running',
  'surviving-artifact',
  'timed-out',
  'abandoned',
  'unclassified',
]

/**
 * Stat each declared artifact path and split it into what SURVIVED a dispatch death and what did
 * not. "Complete" is deliberately the weakest decidable predicate — an existing regular file with
 * a non-zero size — because this probe's only job is to answer "is there something here nobody has
 * read yet?". It never promotes what it finds: a surviving artifact re-enters through the caller's
 * existing verification, contract and secret gates, exactly as a live dispatch's would.
 * @param {string[]} paths
 * @returns {{surviving: string[], missing: string[]}}
 */
export function probeArtifacts(paths = []) {
  const surviving = []
  const missing = []
  for (const candidate of Array.isArray(paths) ? paths : []) {
    if (typeof candidate !== 'string' || candidate.length === 0) continue
    let st
    try {
      st = statSync(candidate)
    } catch {
      missing.push(candidate)
      continue
    }
    if (st.isFile() && st.size > 0) surviving.push(candidate)
    else missing.push(candidate)
  }
  return { surviving, missing }
}

/**
 * Decide whether a dispatch's verdict is publishable, and — when it is not — whether the dispatch
 * is resume-worthy, whether its scratch still holds unread evidence, and why.
 *
 * Two rules this centralizes, because both were being re-derived per call site and got a different
 * answer at each:
 *
 * 1. **A provisional payload demotes EVERY kind.** A seeded, never-upgraded sentinel is
 *    non-publishable whatever kind string it carries — a `clean` seed is exactly as unearned as a
 *    `capped` one. The read below is deliberately the RAW `readSentinel`, not `classifyDispatch`'s
 *    provisional-aware wrapper, so the reason reported is `provisional-payload` rather than the
 *    age-derived death class that wrapper would fall through to.
 * 2. **Read before discard.** `retainScratch` is true whenever the probe found a complete artifact
 *    nobody has consumed, so a dispatch-failure branch cannot remove the evidence that would have
 *    distinguished "died having finished" from "produced nothing".
 *
 * @param {{runId: string, dir: string, sentinelPath: (n: string) => string}} ctx
 * @param {string} name
 * @param {{artifacts?: string[], surviving?: string[]}} [opts] plus every `classifyDispatch` option
 */
export function dispatchDisposition(ctx, name, opts = {}) {
  const probed = Array.isArray(opts.surviving)
    ? { surviving: opts.surviving.filter((p) => typeof p === 'string' && p.length), missing: [] }
    : probeArtifacts(opts.artifacts ?? [])
  const { surviving, missing } = probed
  const decide = (disposition, reason, extra = {}) => ({
    name,
    disposition,
    reason,
    publishable: disposition === 'publish',
    retainScratch: disposition === 'wait' || surviving.length > 0,
    surviving,
    missing,
    ...extra,
  })

  const raw = readSentinel(ctx, name)
  if (raw.status === 'ok') {
    if (raw.payload?.[PROVISIONAL_KEY] === true) {
      // Non-publishable on EVERY kind. Resume-worthy because the seed proves a dispatch was
      // opened, and normalizing an existing one beats re-dispatching from scratch.
      return decide('resume', 'provisional-payload', {
        status: COMPLETED,
        kind: raw.kind,
        payload: raw.payload,
      })
    }
    return decide('publish', 'derived-verdict', {
      status: COMPLETED,
      kind: raw.kind,
      payload: raw.payload ?? {},
    })
  }

  const classified = classifyDispatch(ctx, name, opts)
  if (classified.status === STILL_RUNNING) {
    return decide('wait', 'still-running', { status: STILL_RUNNING, ageMs: classified.ageMs })
  }
  // Every death arm probes FIRST. A surviving complete artifact outranks the death class: there is
  // something to recover, so recover it rather than removing it.
  if (surviving.length > 0) {
    return decide('resume', 'surviving-artifact', {
      status: classified.status,
      ageMs: classified.ageMs,
    })
  }
  if (classified.status === TIMED_OUT) {
    // The deadline passed but the run is not yet stale, so the agent may still be live — one
    // resume is worth attempting before anything is re-dispatched from scratch.
    return decide('resume', 'timed-out', { status: TIMED_OUT, ageMs: classified.ageMs })
  }
  if (classified.status === ABANDONED) {
    return decide('discard', 'abandoned', { status: ABANDONED, ageMs: classified.ageMs })
  }
  return decide('discard', 'unclassified', { status: classified.status })
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
    'usage: bs-dispatch-await.mjs <classify <dir> <runId> <name> <deadlineAtMs> [nowMs]',
    '  | disposition <dir> <runId> <name> [--deadline <ms>] [--now <ms>] [--artifact <path>]...',
    '  | probe <path>... | guard-discard <path>...',
    '  | open <dir> <runId> [nowMs] | batches <json>>',
  ].join('\n')
}

// `--artifact` is REPEATED rather than comma-joined: a comma list would have to be split by the
// caller's shell, and zsh does not word-split an unquoted parameter expansion, so a two-path list
// would silently arrive as one path that exists nowhere. One flag per path has no such failure.
function parseDispositionFlags(argv, fail) {
  const opts = { artifacts: [] }
  for (let i = 0; i < argv.length; i += 1) {
    const flag = argv[i]
    const value = argv[i + 1]
    if (!['--artifact', '--deadline', '--now'].includes(flag)) {
      return fail(`unknown disposition flag: ${flag}`)
    }
    if (value === undefined) return fail(`${flag} requires a value`)
    if (flag === '--artifact') opts.artifacts.push(value)
    else {
      // A non-numeric value must FAIL, never default. `Number('3O0')` is NaN, NaN is neither null
      // nor undefined so it survives the `??=` below, and every NaN comparison is false — so
      // `classifyDispatch` falls past both its death tests to `still-running` and a typo'd
      // deadline reports a dead dispatch as live, which is the one answer this helper exists to
      // never give.
      const parsed = Number(value)
      if (!Number.isFinite(parsed)) return fail(`${flag} requires a finite number, got: ${value}`)
      if (flag === '--deadline') opts.deadlineAt = parsed
      else opts.now = parsed
    }
    i += 1
  }
  // With no explicit deadline the dispatch has already returned, so `still-running` is not a
  // reachable state: default the deadline to now and let the staleness window pick the death class.
  opts.now ??= Date.now()
  opts.deadlineAt ??= opts.now
  return opts
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
    } else if (cmd === 'disposition') {
      const [dir, runId, name, ...flags] = rest
      if (!dir || !runId || !name) fail('disposition requires <dir> <runId> <name> [flags]')
      const opts = parseDispositionFlags(flags, fail)
      process.stdout.write(
        `${JSON.stringify(dispatchDisposition(ctxFor(dir, runId), name, opts))}\n`,
      )
    } else if (cmd === 'probe') {
      if (rest.length === 0) fail('probe requires at least one <path>')
      process.stdout.write(`${JSON.stringify(probeArtifacts(rest))}\n`)
    } else if (cmd === 'guard-discard') {
      // The read-before-discard guard, as ONE call a dispatch-failure branch puts ahead of its
      // removal. Exit 0 AUTHORISES the discard — the probe found nothing, so nothing unexamined is
      // lost. Exit 1 REFUSES it and names what survived, so the caller retains the scratch and
      // resumes instead of re-dispatching from scratch. Written once here rather than re-derived
      // per branch, because a branch that re-derives it is a branch that gets it wrong once.
      if (rest.length === 0) fail('guard-discard requires at least one <path>')
      const { surviving } = probeArtifacts(rest)
      if (surviving.length > 0) {
        process.stderr.write(
          `guard-discard: REFUSING removal — ${surviving.length} complete artifact(s) survived the dispatch and nothing has read them: ${surviving.join(', ')}. Retain the scratch and resume; a salvaged artifact re-enters through the caller's own verification gates, never around them.\n`,
        )
        process.exit(1)
      }
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
