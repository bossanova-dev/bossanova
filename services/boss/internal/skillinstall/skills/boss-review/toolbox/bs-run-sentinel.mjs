// bs-run-sentinel.mjs
//
// Generic run-file sentinel mechanics for the bs-* skills (the bs-* run-sentinel
// epic convention-setter). A skill orchestrator dispatches an awaited subagent that
// writes its terminal decision to a run file; the orchestrator then classifies
// **from that file only** — never from returned prose. The file is keyed by a
// run id so a leftover sentinel from a crashed prior run reads as `stale`
// (never trusted), and an absent file reads as `missing` (a dead/failed
// subagent → the orchestrator synthesizes a `dispatch-failure`, which routes to
// the safe non-green branch and is never `green`).
//
// This module owns the *mechanics* (per-run dir, atomic write-then-rename,
// run-id stale guard, read/classify, cleanup) plus the first token vocabulary —
// bs-sweep-security's `REPAIR_RESULT` set and its matcher. Children 2–5 reuse
// the mechanics verbatim and layer their own token sets on top.
//
// Node built-ins only — cron worktrees are dependency-free. Mirrors the shape
// of bs-review-caps.mjs (module + thin CLI + a byte-stable token set
// pinned by bs-run-sentinel.test.mjs).

import {
  existsSync,
  mkdirSync,
  readdirSync,
  writeFileSync,
  readFileSync,
  renameSync,
  rmSync,
  statSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { randomUUID } from 'node:crypto'

export const STALE_RUN_DIR_TTL_MS = 24 * 60 * 60 * 1000
export const BOSS_PLAN_RUN_SCRATCH_PREFIXES = ['boss-plan-run.', 'boss-plan-notes.']

// The byte-identical REPAIR_RESULT vocabulary a repair-watch subagent may write.
// This is the single source of truth the bs-sweep-security SKILL.md documents
// and the content test pins.
export const REPAIR_RESULTS = ['green', 'no-progress', 'max-attempts', 'blocked']

// The orchestrator-only synthesized result. A subagent NEVER writes this — the
// orchestrator sets it when readSentinel reports `missing`/`stale`. It is
// deliberately not a member of REPAIR_RESULTS, so matchRepairResult rejects it.
export const DISPATCH_FAILURE = 'dispatch-failure'

// The payload key an orchestrator-SEEDED, not-yet-upgraded sentinel carries.
//
// An orchestrator may seed a pessimistic sentinel BEFORE it dispatches, so a
// subagent that dies at any point still leaves a routable verdict instead of a
// `missing` file. That makes one kind string reachable from two opposite causes
// — an honest cap a reviewer earned, and a seed nobody ever upgraded — and a
// sentinel rendered for two opposite causes is not a diagnostic. The payload is
// the discriminator: branch on whether the observation happened, NEVER on the
// kind string, the round count, or the returned prose.
//
// Byte-stable: the skill markdown writes this literal by hand (`jq` reads it in
// the classify block, so no second `node` spawn is added for it), and the
// content gate joins that literal against this constant so the two cannot drift.
export const PROVISIONAL_KEY = 'provisional'

/**
 * The payload an orchestrator passes to `writeSentinel` when it seeds a
 * pessimistic verdict before dispatch. Every LATER write carries the marker
 * explicitly false rather than omitting it, so "not provisional" is stated
 * rather than inferred from absence.
 * @returns {{provisional: true}}
 */
export function provisionalPayload() {
  return { [PROVISIONAL_KEY]: true }
}

/**
 * True iff `result` is a `readSentinel` result that was actually READ (`ok`) and
 * carries the marker as a strict boolean `true`. `missing`/`stale` are never
 * provisional — they carry no observation at all — and the string `'true'` is
 * not the marker: a payload that round-trips through a shell must survive as
 * JSON `true` or it is not the seed this discriminates.
 * @param {{status?: string, payload?: object}|null|undefined} result
 * @returns {boolean}
 */
export function isProvisional(result) {
  return result?.status === 'ok' && result?.payload?.[PROVISIONAL_KEY] === true
}

/**
 * Build a run context: a unique per-run dir under `$TMPDIR` (or an explicit
 * `opts.tmpdir`) keyed by a run id. The dir is created eagerly.
 * @param {string} skill  the skill slug (namespaces the dir)
 * @param {{tmpdir?: string, runId?: string}} [opts]
 * @returns {{runId: string, dir: string, sentinelPath: (name: string) => string}}
 */
export function makeRunContext(skill, opts = {}) {
  const runId = opts.runId ?? randomUUID()
  const base = opts.tmpdir ?? tmpdir()
  const dir = join(base, 'bs-run-sentinel', `${skill}-${runId}`)
  reapStaleRunDirs({ tmpdir: base, excludeDirs: [dir] })
  mkdirSync(dir, { recursive: true })
  return {
    runId,
    dir,
    sentinelPath: (name) => join(dir, `${name}.json`),
  }
}

/**
 * Atomically write a sentinel: serialize `{runId, kind, payload}` to a temp file
 * in `ctx.dir`, then rename into place. Rename is atomic on the same filesystem,
 * so a concurrent reader never observes a half-written file.
 * @param {{runId: string, dir: string, sentinelPath: (n: string) => string}} ctx
 * @param {string} name    sentinel name (e.g. 'repair')
 * @param {string} kind    the terminal token (a REPAIR_RESULTS member)
 * @param {object} [payload]
 * @returns {string} the absolute path written
 */
export function writeSentinel(ctx, name, kind, payload = {}) {
  const finalPath = ctx.sentinelPath(name)
  const tmpPath = join(ctx.dir, `${name}.tmp`)
  writeFileSync(tmpPath, JSON.stringify({ runId: ctx.runId, kind, payload }))
  renameSync(tmpPath, finalPath)
  return finalPath
}

/**
 * Read + verify a sentinel. Returns one of:
 *   { status: 'ok',      runId, kind, payload }  file exists AND runId === ctx.runId
 *   { status: 'missing' }                        file absent (dead/failed subagent)
 *   { status: 'stale',   runId }                 file exists but carries a DIFFERENT
 *                                                 runId, or is unparseable (leftover)
 * A `stale`/`missing` result must NEVER be trusted as a repair outcome.
 * @param {{runId: string, sentinelPath: (n: string) => string}} ctx
 * @param {string} name
 */
export function readSentinel(ctx, name) {
  let raw
  try {
    raw = readFileSync(ctx.sentinelPath(name), 'utf8')
  } catch (err) {
    if (err && err.code === 'ENOENT') return { status: 'missing' }
    throw err
  }
  let parsed
  try {
    parsed = JSON.parse(raw)
  } catch {
    // A corrupt/half-written leftover is treated exactly like a foreign run:
    // never trusted, never `ok`.
    return { status: 'stale', runId: null }
  }
  if (parsed.runId !== ctx.runId) return { status: 'stale', runId: parsed.runId ?? null }
  return { status: 'ok', runId: parsed.runId, kind: parsed.kind, payload: parsed.payload ?? {} }
}

/**
 * Classify a REPAIR_RESULT token. Returns `{result}` iff `token` is a published
 * REPAIR_RESULTS member, else `null`. `DISPATCH_FAILURE` is deliberately not a
 * valid token here — only the orchestrator synthesizes it.
 * @param {string} token
 * @returns {{result: string}|null}
 */
export function matchRepairResult(token) {
  if (typeof token !== 'string') return null
  const t = token.trim()
  return REPAIR_RESULTS.includes(t) ? { result: t } : null
}

/**
 * Best-effort recursive removal of `ctx.dir`. Idempotent — a second call on an
 * already-removed dir must not throw.
 * @param {{dir: string}} ctx
 */
export function cleanupRunContext(ctx) {
  rmSync(ctx.dir, { recursive: true, force: true })
  const residue = existsSync(ctx.dir)
  return { removed: !residue, residue }
}

function reapDirectory(path, onReport) {
  try {
    rmSync(path, { recursive: true, force: true })
    if (!existsSync(path)) {
      onReport?.(`removed stale run directory: ${path}`)
      return true
    }
    onReport?.(`warning: stale run directory survived removal: ${path}`)
  } catch (err) {
    onReport?.(`warning: failed to reap stale run directory ${path}: ${err.message}`)
  }
  return false
}

function reapEntries({ base, names, now, ttlMs, exclude, onReport }) {
  for (const name of names) {
    const path = join(base, name)
    if (exclude.has(path)) continue
    let st
    try {
      st = statSync(path)
    } catch {
      continue
    }
    if (!st.isDirectory() || now - st.mtimeMs < ttlMs) continue
    reapDirectory(path, onReport)
  }
}

/**
 * Opportunistically reap stale run directories. Failures are reported but never fatal.
 * @param {{tmpdir?: string, now?: number, ttlMs?: number, excludeDirs?: string[], onReport?: (msg: string) => void}} [opts]
 * @returns {{removed: string[], failed: string[]}}
 */
export function reapStaleRunDirs(opts = {}) {
  const base = opts.tmpdir ?? tmpdir()
  const now = opts.now ?? Date.now()
  const ttlMs = opts.ttlMs ?? STALE_RUN_DIR_TTL_MS
  const exclude = new Set(opts.excludeDirs ?? [])
  const removed = []
  const failed = []
  const onReport = (message) => {
    if (message.startsWith('removed ')) removed.push(message)
    else failed.push(message)
    opts.onReport?.(message)
  }
  try {
    const sentinelBase = join(base, 'bs-run-sentinel')
    const sentinelNames = readdirSync(sentinelBase)
    reapEntries({ base: sentinelBase, names: sentinelNames, now, ttlMs, exclude, onReport })
  } catch {
    /* Missing sentinel root is a clean no-op. */
  }
  try {
    const runScratchNames = readdirSync(base).filter((name) =>
      BOSS_PLAN_RUN_SCRATCH_PREFIXES.some((prefix) => name.startsWith(prefix)),
    )
    reapEntries({ base, names: runScratchNames, now, ttlMs, exclude, onReport })
  } catch {
    /* Missing tmp root is a clean no-op. */
  }
  return { removed, failed }
}

// Thin CLI (the surface the skill body shells out to):
//   node bs-run-sentinel.mjs make-ctx <skill> [tmpdir]  → prints `runId\tdir`
//   node bs-run-sentinel.mjs write <dir> <runId> <name> <kind> [payloadJson]
//   node bs-run-sentinel.mjs read  <dir> <runId> <name>  → prints the JSON result
//   node bs-run-sentinel.mjs match <token>              → JSON ({result}|null)
//   node bs-run-sentinel.mjs cleanup <dir>
//
// The CLI reconstructs a context from an explicit <dir>/<runId> so the
// orchestrator and its subagent can share one run file across process
// boundaries.
function ctxFor(dir, runId) {
  return { runId, dir, sentinelPath: (name) => join(dir, `${name}.json`) }
}

import { isMainModule } from './main-module.mjs'

if (isMainModule(import.meta.url)) {
  const [cmd, ...rest] = process.argv.slice(2)
  const fail = (msg) => {
    process.stderr.write(`${msg}\n`)
    process.exit(2)
  }
  if (cmd === 'make-ctx') {
    const skill = rest[0]
    if (!skill) fail('make-ctx requires a <skill> slug')
    const ctx = makeRunContext(skill, rest[1] ? { tmpdir: rest[1] } : {})
    process.stdout.write(`${ctx.runId}\t${ctx.dir}\n`)
  } else if (cmd === 'write') {
    const [dir, runId, name, kind, payloadJson] = rest
    if (!dir || !runId || !name || !kind)
      fail('write requires <dir> <runId> <name> <kind> [payloadJson]')
    const payload = payloadJson ? JSON.parse(payloadJson) : {}
    process.stdout.write(`${writeSentinel(ctxFor(dir, runId), name, kind, payload)}\n`)
  } else if (cmd === 'read') {
    const [dir, runId, name] = rest
    if (!dir || !runId || !name) fail('read requires <dir> <runId> <name>')
    process.stdout.write(`${JSON.stringify(readSentinel(ctxFor(dir, runId), name))}\n`)
  } else if (cmd === 'match') {
    process.stdout.write(`${JSON.stringify(matchRepairResult(rest[0] ?? ''))}\n`)
  } else if (cmd === 'cleanup') {
    const [dir] = rest
    if (!dir) fail('cleanup requires <dir>')
    const result = cleanupRunContext(ctxFor(dir, ''))
    if (result.residue) {
      process.stderr.write(`cleanup residue remains: ${dir}\n`)
      process.exit(1)
    }
  } else {
    fail(
      'usage: bs-run-sentinel.mjs <make-ctx <skill> [tmpdir] | write <dir> <runId> <name> <kind> [payloadJson] | read <dir> <runId> <name> | match <token> | cleanup <dir>>',
    )
  }
}
