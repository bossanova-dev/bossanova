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

import { mkdirSync, readFileSync, readdirSync, renameSync, statSync, writeFileSync } from 'node:fs'
import { spawn } from 'node:child_process'
import { constants as osConstants } from 'node:os'
import { join, basename, dirname } from 'node:path'
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

// ---------------------------------------------------------------------------
// Liveness — "is this dispatch still working?", settled on an artifact the
// dispatch writes WHILE WORKING.
// ---------------------------------------------------------------------------
//
// The sentinel cannot answer it. A sentinel records a TERMINAL decision exactly
// once, so a seeded sentinel's mtime is the seed's and never moves: a dispatch
// that has been drafting for 34 minutes has the same mtime it had at minute
// zero, and the age derived from it crosses DEFAULT_OPEN_DISPATCH_STALE_MS and
// reports `abandoned` on a worker that is demonstrably alive (measured: a
// drafter at minute 34). The two probes improvised in its place are both false
// oracles: transcript idle time fires while a worker is alive inside one long
// gate call, and `stat` on a `tasks/<id>.output` path measures the symlink and
// returns a constant from the first sample.
//
// The heartbeat follows the shape `worktree-lock.sh` already uses for the same
// question: a recorded heartbeat when one is present, the artifact's own mtime
// when it is not, and a STRICT `age < stale` liveness boundary. Two properties
// make adopting it safe:
//
//   1. **Absent reads `absent`, never `stale`.** A worker that has not touched
//      the heartbeat yet must not be killed for it, so the caller falls back to
//      the seed clock rather than treating silence as death.
//   2. **It can only ever EXTEND liveness.** Not an argument about who writes
//      the artifact first — a CONSTRUCTION: `classifyDispatch` requires a stale
//      SEED for every death, and the heartbeat may only veto one. So consulting
//      it cannot newly kill a dispatch that reads live today, whoever wrote the
//      beat and whatever `heartbeatStaleAfterMs` is set to; it can only keep a
//      beating one alive past the window.
export const HEARTBEAT_LIVENESS = ['live', 'stale', 'absent']
export const HEARTBEAT_LIVE = 'live'
export const HEARTBEAT_STALE = 'stale'
export const HEARTBEAT_ABSENT = 'absent'
/** Default gap between wrapper beats; short enough that one gate call still beats many times. */
export const DEFAULT_HEARTBEAT_INTERVAL_MS = 30_000
/** Bounded — the heartbeat is a liveness signal, never a transport for findings. */
export const HEARTBEAT_NOTE_MAX = 200

/**
 * The effective heartbeat: the recorded epoch when the artifact holds one, else
 * the artifact's own mtime. `null` when there is no artifact at all.
 * @param {string} path
 * @returns {{at: number, source: 'recorded'|'mtime', note: string|null}|null}
 */
export function readHeartbeat(path) {
  if (typeof path !== 'string' || path.length === 0) return null
  let st
  try {
    st = statSync(path)
  } catch {
    return null
  }
  if (!st.isFile()) return null
  let recorded = null
  let note = null
  try {
    const parsed = JSON.parse(readFileSync(path, 'utf8'))
    if (parsed && typeof parsed === 'object' && Number.isFinite(parsed.at)) {
      recorded = parsed.at
      note = typeof parsed.note === 'string' ? parsed.note : null
    }
  } catch {
    // A half-written or hand-touched heartbeat still PROVES something was here;
    // its mtime is the honest reading, so fall through rather than report death.
  }
  return recorded === null
    ? { at: st.mtimeMs, source: 'mtime', note: null }
    : { at: recorded, source: 'recorded', note }
}

/**
 * Classify a heartbeat artifact. `live` is a STRICT `age < staleAfterMs`, so an
 * age exactly at the boundary is decisively stale — the same boundary
 * `worktree-lock.sh` uses, and for the same reason: with `<=` two readers at the
 * boundary both read live and nobody acts.
 * @param {string} path
 * @param {{now?: number, staleAfterMs?: number}} [opts]
 * @returns {{status: 'live'|'stale'|'absent', ageMs: number|null, at: number|null, source: string|null}}
 */
export function heartbeatLiveness(path, opts = {}) {
  const now = opts.now ?? Date.now()
  const staleAfterMs = opts.staleAfterMs ?? DEFAULT_OPEN_DISPATCH_STALE_MS
  const beat = readHeartbeat(path)
  if (!beat) return { status: HEARTBEAT_ABSENT, ageMs: null, at: null, source: null }
  const ageMs = Math.max(0, now - beat.at)
  return {
    status: ageMs < staleAfterMs ? HEARTBEAT_LIVE : HEARTBEAT_STALE,
    ageMs,
    at: beat.at,
    source: beat.source,
  }
}

/**
 * Record a beat. Atomic (write-then-rename) so a concurrent await-side read can
 * never observe a half-written heartbeat and mistake it for a corrupt one.
 *
 * The AWAIT side must never call this: a reader that touches the artifact it is
 * measuring manufactures the liveness it claims to observe. The writers are the
 * dispatched worker and `runWithHeartbeat` below.
 * @param {string} path
 * @param {{now?: number, note?: string}} [opts]
 * @returns {{path: string, at: number, note: string|null}}
 */
export function touchHeartbeat(path, opts = {}) {
  if (typeof path !== 'string' || path.length === 0) {
    throw new Error('touchHeartbeat requires a path')
  }
  const at = opts.now ?? Date.now()
  const note =
    typeof opts.note === 'string' && opts.note.length > 0
      ? opts.note.slice(0, HEARTBEAT_NOTE_MAX)
      : null
  const dir = dirname(path)
  mkdirSync(dir, { recursive: true })
  const tmp = join(dir, `.${basename(path)}.${process.pid}.tmp`)
  writeFileSync(tmp, JSON.stringify({ at, note }))
  renameSync(tmp, path)
  return { path, at, note }
}

/**
 * Run `command` to completion while beating `path` on an interval, and exit with
 * the CHILD's status.
 *
 * This is the half of the heartbeat contract that keeps it from being the false
 * oracle it replaces. A worker blocked inside one long gate call writes nothing
 * between tool calls — exactly the silence that made transcript-idle a false
 * oracle — so a heartbeat only a worker could touch would reproduce the defect
 * in a new file. Wrapping the long call makes the beat come from the call
 * itself: the signal is "this command is still running", not "the model is
 * still typing".
 * @param {{path: string, intervalMs?: number, command: string, args?: string[], onExit?: (code: number) => void}} opts
 */
export function runWithHeartbeat(opts) {
  const { path, command } = opts
  if (!command) throw new Error('runWithHeartbeat requires a command')
  const intervalMs = assertPositiveNumber(
    opts.intervalMs ?? DEFAULT_HEARTBEAT_INTERVAL_MS,
    'intervalMs',
  )
  const args = opts.args ?? []
  const note = [command, ...args].join(' ')
  const beat = () => {
    try {
      touchHeartbeat(path, { note })
    } catch {
      // A failed beat must never cost the wrapped command its run: the beat is a
      // liveness disclosure, the child's exit status is the result.
    }
  }
  beat()
  const child = spawn(command, args, { stdio: 'inherit' })
  const timer = setInterval(beat, intervalMs)
  const finish = (code) => {
    clearInterval(timer)
    beat()
    ;(opts.onExit ?? ((c) => process.exit(c)))(code)
  }
  child.on('error', (err) => {
    process.stderr.write(`runWithHeartbeat: ${command}: ${err.message}\n`)
    finish(127)
  })
  // The child's status, NEVER the launcher's. A wrapper that reported its own
  // exit code would turn every failed gate into a green one.
  child.on('exit', (code, signal) =>
    finish(signal ? 128 + (osConstants.signals[signal] ?? 0) : (code ?? 1)),
  )
  return child
}

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

/**
 * Classify one dispatch's ARTIFACT.
 *
 * `settles: 'artifact'` travels on every result because the verdict's scope is
 * the thing callers kept getting wrong: `COMPLETED` says the terminal artifact
 * landed and says NOTHING about whether the dispatch has returned its bounded
 * metadata object. Compose `dispatchReadiness` with the message the caller
 * actually received to decide that second question — there is deliberately no
 * token here that answers both.
 *
 * Pass `heartbeatPath` to settle liveness on the artifact the dispatch touches
 * while working rather than on the sentinel seed's frozen mtime.
 */
export function classifyDispatch(ctx, name, opts = {}) {
  const now = opts.now ?? Date.now()
  const deadlineAt = opts.deadlineAt ?? now + awaitDeadlineMs(opts)
  const staleAfterMs = opts.staleAfterMs ?? DEFAULT_OPEN_DISPATCH_STALE_MS
  const read = readOpenedSentinel(ctx, name)
  if (read.status === 'ok') {
    return {
      status: COMPLETED,
      settles: 'artifact',
      name,
      kind: read.kind,
      payload: read.payload ?? {},
    }
  }

  const path = ctx.sentinelPath(name)
  let openedAt = opts.openedAt
  try {
    openedAt ??= statSync(path).mtimeMs
  } catch {
    openedAt ??= opts.dispatchedAt ?? now
  }
  const seedAgeMs = Math.max(0, now - openedAt)
  const beat = opts.heartbeatPath
    ? heartbeatLiveness(opts.heartbeatPath, {
        now,
        staleAfterMs: opts.heartbeatStaleAfterMs ?? staleAfterMs,
      })
    : { status: HEARTBEAT_ABSENT, ageMs: null, at: null, source: null }
  // A live heartbeat is the dispatch's OWN evidence that it is still working, so
  // it outranks the seed clock and suppresses `abandoned` — but not `timed-out`.
  // Those answer different questions: `abandoned` says nobody is home, while
  // `timed-out` says this caller's budget expired, which a live worker can hit.
  // `ageMs` is the LIVENESS age the decision below is taken on — the beat's when one was consulted,
  // the seed's otherwise. `seedAgeMs` ("how long has this dispatch been open") is reported BESIDE it
  // on every result rather than discarded, so a caller that wants the open-age never has to infer it
  // from a field whose subject changes with the presence of a heartbeat. `openDispatches` consults
  // no heartbeat, so ITS `ageMs` is always a seed age and is NOT this `ageMs`; it reports the open
  // age under `seedAgeMs` as well, and that is the field the two share.
  const ageMs = beat.status === HEARTBEAT_ABSENT ? seedAgeMs : beat.ageMs
  const result = { name, ageMs, seedAgeMs, settles: 'artifact' }
  if (beat.status !== HEARTBEAT_ABSENT) {
    result.heartbeat = { status: beat.status, ageMs: beat.ageMs, source: beat.source }
  }
  // ENFORCED, not argued: death always requires a stale SEED, so consulting the heartbeat can only
  // ever SUPPRESS `abandoned` (a live beat) and never introduce it. Gating on the beat alone would
  // make the adoption claim false in two reachable cases — a leftover or FOREIGN beat at a
  // caller-supplied path, which `readHeartbeat` cannot attribute to this dispatch and which reads
  // `stale` over a seed of age ~0; and a `heartbeatStaleAfterMs` tighter than `staleAfterMs`, which
  // the API permits. Requiring the seed too makes the knob unable to kill anything the seed clock
  // would have kept alive, which is what the header promises.
  const dead = seedAgeMs >= staleAfterMs && beat.status !== HEARTBEAT_LIVE
  if (dead) return { ...result, status: ABANDONED }
  if (now >= deadlineAt) return { ...result, status: TIMED_OUT }
  return { ...result, status: STILL_RUNNING }
}

// ---------------------------------------------------------------------------
// Completion — artifact readiness and returned-message readiness are SEPARATELY
// decidable, and no token stands for both.
// ---------------------------------------------------------------------------
//
// Shape chosen: a composed PREDICATE, not a second member of
// DISPATCH_AWAIT_RESULTS. A new token would be a new routing value every
// existing `switch` over that set silently falls through — the same silent
// regrowth this ticket exists to close — and `toSentinelRouting` /
// `dispatchDisposition` would each need a fresh arm to stay correct. A predicate
// cannot be reached by accident instead: the caller has to hand over the message
// it received, so "the message is in hand" can never be inferred from a file.
export const MESSAGE_RETURNED = 'returned'
export const MESSAGE_NOT_RETURNED = 'not-returned'
export const MESSAGE_READINESS = [MESSAGE_RETURNED, MESSAGE_NOT_RETURNED]

/**
 * Is the dispatch's returned message in hand? This settles ARRIVAL only —
 * whether the returned object satisfies its contract is the consuming guard's
 * question, and deliberately not answered here.
 * @param {unknown} returned the object/string the dispatch returned, as received
 * @returns {{status: 'returned'|'not-returned', reason: string}}
 */
export function messageReadiness(returned) {
  if (returned === undefined || returned === null) {
    return { status: MESSAGE_NOT_RETURNED, reason: 'no-message' }
  }
  if (typeof returned === 'string' && returned.trim().length === 0) {
    return { status: MESSAGE_NOT_RETURNED, reason: 'empty-message' }
  }
  return { status: MESSAGE_RETURNED, reason: 'message-in-hand' }
}

/**
 * Compose the two readiness questions into one report that never collapses them.
 * `done` is the conjunction and is the ONLY field that may be read as "this
 * dispatch is finished"; `artifactReady` alone never is.
 * @param {{status?: string}|null|undefined} classified a `classifyDispatch` result
 * @param {unknown} returned
 */
export function dispatchReadiness(classified, returned) {
  const artifactReady = classified?.status === COMPLETED
  const message = messageReadiness(returned)
  const messageReady = message.status === MESSAGE_RETURNED
  const reason = !artifactReady
    ? `artifact-${classified?.status ?? 'unclassified'}`
    : messageReady
      ? 'artifact-and-message'
      : `message-${message.reason}`
  return {
    artifactReady,
    artifactStatus: classified?.status ?? null,
    messageReady,
    messageStatus: message.status,
    done: artifactReady && messageReady,
    reason,
  }
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
    return decide('wait', 'still-running', {
      status: STILL_RUNNING,
      ageMs: classified.ageMs,
      seedAgeMs: classified.seedAgeMs,
    })
  }
  // Every death arm probes FIRST. A surviving complete artifact outranks the death class: there is
  // something to recover, so recover it rather than removing it.
  if (surviving.length > 0) {
    return decide('resume', 'surviving-artifact', {
      status: classified.status,
      ageMs: classified.ageMs,
      seedAgeMs: classified.seedAgeMs,
    })
  }
  if (classified.status === TIMED_OUT) {
    // The deadline passed but the run is not yet stale, so the agent may still be live — one
    // resume is worth attempting before anything is re-dispatched from scratch.
    return decide('resume', 'timed-out', {
      status: TIMED_OUT,
      ageMs: classified.ageMs,
      seedAgeMs: classified.seedAgeMs,
    })
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
    // `seedAgeMs` is the field `classifyDispatch` shares with this verb; `ageMs` is kept as the
    // long-standing name and is the same number here, because this verb consults no heartbeat.
    opened.push({ name, ageMs, seedAgeMs: ageMs, stale: ageMs >= staleAfterMs })
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
    'usage: bs-dispatch-await.mjs <classify <dir> <runId> <name> <deadlineAtMs> [nowMs] [heartbeatPath]',
    '  | disposition <dir> <runId> <name> [--deadline <ms>] [--now <ms>] [--artifact <path>]... [--heartbeat <path>]',
    '  | probe <path>... | guard-discard <path>...',
    '  | heartbeat <path> [note] | liveness <path> [nowMs] [staleMs]',
    '  | beat <path> [--interval <ms>] -- <command> [args...]',
    '  | open <dir> <runId> [nowMs] | batches <json>>',
  ].join('\n')
}

// The finite-or-fail rule for every number read from argv, stated ONCE rather than re-derived per
// verb. A non-numeric value must FAIL, never default: `Number('3O0')` is NaN, NaN is neither null
// nor undefined so it survives `??`/`??=`, and every NaN comparison is false — so `classifyDispatch`
// falls past BOTH its death tests to `still-running` and a typo'd deadline reports a dead dispatch
// as live, which is the one answer this helper exists to never give. `undefined` is not a typo, it
// is an omission, and each caller owns its own default for that.
function requireFiniteArgs(pairs, fail) {
  for (const [label, raw] of pairs) {
    if (raw !== undefined && !Number.isFinite(Number(raw))) {
      fail(`${label} requires a finite number, got: ${raw}`)
    }
  }
}

// `--artifact` is REPEATED rather than comma-joined: a comma list would have to be split by the
// caller's shell, and zsh does not word-split an unquoted parameter expansion, so a two-path list
// would silently arrive as one path that exists nowhere. One flag per path has no such failure.
function parseDispositionFlags(argv, fail) {
  const opts = { artifacts: [] }
  for (let i = 0; i < argv.length; i += 1) {
    const flag = argv[i]
    const value = argv[i + 1]
    if (!['--artifact', '--deadline', '--now', '--heartbeat'].includes(flag)) {
      return fail(`unknown disposition flag: ${flag}`)
    }
    if (value === undefined) return fail(`${flag} requires a value`)
    if (flag === '--artifact') opts.artifacts.push(value)
    else if (flag === '--heartbeat') opts.heartbeatPath = value
    else {
      requireFiniteArgs([[flag, value]], fail)
      const parsed = Number(value)
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
      const [dir, runId, name, deadlineAtRaw, nowRaw, heartbeatPath] = rest
      if (!dir || !runId || !name || !deadlineAtRaw)
        fail('classify requires <dir> <runId> <name> <deadlineAtMs> [nowMs] [heartbeatPath]')
      requireFiniteArgs(
        [
          ['classify deadlineAtMs', deadlineAtRaw],
          ['classify nowMs', nowRaw],
        ],
        fail,
      )
      process.stdout.write(
        `${JSON.stringify(
          classifyDispatch(ctxFor(dir, runId), name, {
            deadlineAt: Number(deadlineAtRaw),
            now: nowRaw ? Number(nowRaw) : Date.now(),
            heartbeatPath,
          }),
        )}\n`,
      )
    } else if (cmd === 'heartbeat') {
      const [path, note] = rest
      if (!path) fail('heartbeat requires <path> [note]')
      process.stdout.write(`${JSON.stringify(touchHeartbeat(path, { note }))}\n`)
    } else if (cmd === 'liveness') {
      const [path, nowRaw, staleRaw] = rest
      if (!path) fail('liveness requires <path> [nowMs] [staleMs]')
      requireFiniteArgs(
        [
          ['liveness nowMs', nowRaw],
          ['liveness staleMs', staleRaw],
        ],
        fail,
      )
      process.stdout.write(
        `${JSON.stringify(
          heartbeatLiveness(path, {
            now: nowRaw ? Number(nowRaw) : Date.now(),
            staleAfterMs: staleRaw ? Number(staleRaw) : undefined,
          }),
        )}\n`,
      )
    } else if (cmd === 'beat') {
      // `beat <path> [--interval <ms>] -- <command> [args...]` — the gate wrapper.
      // A dispatch blocked inside one long command beats through THIS, so the
      // heartbeat reports the command still running rather than the model still
      // typing. It exits with the CHILD's status, never the launcher's.
      const sep = rest.indexOf('--')
      if (sep === -1) fail('beat requires `-- <command> [args...]`')
      const head = rest.slice(0, sep)
      const [path, ...flags] = head
      if (!path) fail('beat requires <path> before `--`')
      let intervalMs
      for (let i = 0; i < flags.length; i += 2) {
        if (flags[i] !== '--interval') fail(`unknown beat flag: ${flags[i]}`)
        const parsed = Number(flags[i + 1])
        if (!Number.isFinite(parsed) || parsed <= 0) {
          fail(`--interval requires a positive number, got: ${flags[i + 1]}`)
        }
        intervalMs = parsed
      }
      const [command, ...args] = rest.slice(sep + 1)
      if (!command) fail('beat requires a command after `--`')
      runWithHeartbeat({ path, intervalMs, command, args })
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
      requireFiniteArgs([['open nowMs', nowRaw]], fail)
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
