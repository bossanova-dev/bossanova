#!/usr/bin/env node
// second-model-health.mjs — turn repeated second-model review failures into one host health fact.
//
//   record <verdict-file>   read the verdict's secondModel and update this host's per-agent streak
//
// A review whose second model could not run (`not_authed`, `not_installed` or `error`) extends the
// streak for that agent; the third consecutive failure records one `health`+`second-model` note. A
// review where it ran resets the streak and, when a fact was recorded, records one `recovered` note.
// A second model configured off (`review.secondModel: false`) or a run under BOSS_BS_REVIEW=0 is
// not a failure and leaves the streak alone. The streak lives in the host state directory
// ($BOSS_SECOND_MODEL_HEALTH_DIR, else ~/.local/state/bossanova), written under a lock with a temp
// file and rename, so concurrent reviews on one host lose no increment. Never fatal: every outcome
// prints one JSON line and exits 0; only a usage error exits 2. Node builtins only.
import { spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import defaultFs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { parseArgs } from 'node:util'
import { resolveBossBinary } from '../boss-binary.mjs'
import { isMainModule } from '../main-module.mjs'
import { loadSkillConfig, reviewSecondModel } from '../skill-config.mjs'

export const STREAK_THRESHOLD = 3
export const STATE_FILENAME = 'second-model-health.json'
export const HEALTH_TAGS = Object.freeze(['health', 'second-model'])
export const AGENTS = Object.freeze(['claude', 'codex'])
const LOCK_WAIT_MS = 30_000
const LOCK_STALE_MS = 60_000
const NOTE_TIMEOUT_MS = 10_000
const SHA = /^[0-9a-f]{7,40}$/i

export const USAGE = 'usage: second-model-health.mjs record <verdict-file>'

const oneLine = (value) =>
  String(value ?? '')
    .replace(/\s+/g, ' ')
    .trim()

/** The host state directory the streak file lives in. */
export function stateDir(env = process.env) {
  const override = String(env.BOSS_SECOND_MODEL_HEALTH_DIR ?? '').trim()
  if (override) return override
  return path.join(env.HOME || os.homedir(), '.local', 'state', 'bossanova')
}

/**
 * What a verdict says about the second model.
 * @returns {{kind: 'ran', agent: string} | {kind: 'failed', agent: string, classification: string} | {kind: 'ignored', reason: string}}
 */
export function classifySecondModel(verdict) {
  const second = verdict?.secondModel
  if (second === null || typeof second !== 'object')
    return { kind: 'ignored', reason: 'no secondModel' }
  if (second.ran === true) {
    return AGENTS.includes(second.agent)
      ? { kind: 'ran', agent: second.agent }
      : { kind: 'ignored', reason: 'ran with an unknown agent' }
  }
  if (second.ran !== false) return { kind: 'ignored', reason: 'secondModel.ran is not a boolean' }
  const reason = oneLine(second.reason)
  const host = reason.match(/(?:\b(claude|codex)-review:\s*)?\b(not_authed|not_installed)\b/)
  const error = host ? null : reason.match(/(?:\b(claude|codex)-review:\s*)?\berror:/)
  const match = host ?? error
  if (!match) return { kind: 'ignored', reason: 'not a host failure' }
  const agent = match[1] || second.agent
  if (!AGENTS.includes(agent)) return { kind: 'ignored', reason: 'failure names no known agent' }
  return { kind: 'failed', agent, classification: host ? host[2] : 'error' }
}

const hash = (parts) => createHash('sha256').update(parts.join('|')).digest('hex').slice(0, 32)

/** The note a streak or a recovery records; the key makes retries and concurrent runs record it once. */
export function healthNote({ host, agent, streak, recovered = false }) {
  const key = hash([host, agent, streak.since])
  if (recovered) {
    return {
      tags: [...HEALTH_TAGS, 'recovered'],
      idempotencyKey: `second-model-health:recovered:${key}`,
      body: `second-model review recovered: ${agent} ran again on host ${host} after ${streak.count} consecutive failures (${streak.classification} since ${streak.since})`,
    }
  }
  return {
    tags: [...HEALTH_TAGS],
    idempotencyKey: `second-model-health:${key}`,
    body: `second-model review unavailable: ${agent} ${streak.classification} since ${streak.since}, ${streak.count} consecutive failures on host ${host}`,
  }
}

/**
 * Apply one verdict to the stored streaks. Pure: returns the next state and the note to record.
 * @returns {{state: object, action: string, agent?: string, count?: number, reason?: string, note: object|null}}
 */
export function applyVerdict(state, verdict, { host, now }) {
  const next = { version: 1, agents: { ...(state?.agents ?? {}) } }
  const outcome = classifySecondModel(verdict)
  if (outcome.kind === 'ignored')
    return { state: next, action: 'ignored', reason: outcome.reason, note: null }
  const { agent } = outcome
  const current = next.agents[agent] ?? null
  const key =
    SHA.test(verdict?.base ?? '') && SHA.test(verdict?.head ?? '')
      ? `${verdict.base}|${verdict.head}|${outcome.kind}|${outcome.kind === 'failed' ? outcome.classification : ''}`
      : null
  if (key !== null && current?.lastKey === key)
    return { state: next, action: 'duplicate', agent, count: current.count ?? 0, note: null }
  if (outcome.kind === 'ran') {
    next.agents[agent] = { count: 0, lastKey: key }
    const recovered = current?.emitted === true && current.count > 0
    return {
      state: next,
      action: 'reset',
      agent,
      count: 0,
      note: recovered ? healthNote({ host, agent, streak: current, recovered: true }) : null,
    }
  }
  const failing = current?.count > 0
  const streak = {
    classification: outcome.classification,
    since: failing ? current.since : now.toISOString(),
    count: failing ? current.count + 1 : 1,
    emitted: failing ? current.emitted === true : false,
    lastKey: key,
  }
  next.agents[agent] = streak
  const due = streak.count >= STREAK_THRESHOLD && !streak.emitted
  return {
    state: next,
    action: 'failed',
    agent,
    count: streak.count,
    note: due ? healthNote({ host, agent, streak }) : null,
  }
}

function sleepSync(ms) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms)
}

// A mkdir lock: atomic on every local filesystem. A lock older than LOCK_STALE_MS belongs to a
// crashed run and is taken over.
function withLock(fs, dir, fn) {
  const lock = path.join(dir, `${STATE_FILENAME}.lock`)
  const deadline = Date.now() + LOCK_WAIT_MS
  for (;;) {
    try {
      fs.mkdirSync(lock)
      break
    } catch (error) {
      if (error.code !== 'EEXIST') throw error
      try {
        if (Date.now() - fs.statSync(lock).mtimeMs > LOCK_STALE_MS) {
          fs.rmSync(lock, { recursive: true, force: true })
          continue
        }
      } catch {
        continue
      }
      if (Date.now() >= deadline) throw new Error('streak lock is busy')
      sleepSync(25 + Math.floor(Math.random() * 25))
    }
  }
  try {
    return fn()
  } finally {
    fs.rmSync(lock, { recursive: true, force: true })
  }
}

function readState(fs, file) {
  try {
    const state = JSON.parse(fs.readFileSync(file, 'utf8'))
    return state && typeof state === 'object' && !Array.isArray(state) ? state : {}
  } catch {
    return {}
  }
}

function writeState(fs, file, state) {
  const temp = `${file}.${process.pid}.${Date.now()}.tmp`
  fs.writeFileSync(temp, `${JSON.stringify(state, null, 2)}\n`)
  fs.renameSync(temp, file)
}

function defaultAddNote(note, { env }) {
  const binary = resolveBossBinary(env, { fs: defaultFs })
  if (!binary.ok) throw new Error(binary.reason)
  const tagArgs = note.tags.flatMap((tag) => ['--tag', tag])
  const result = spawnSync(
    binary.path,
    [
      'notes',
      'add',
      ...tagArgs,
      '--json',
      '--idempotency-key',
      note.idempotencyKey,
      '--',
      note.body,
    ],
    { encoding: 'utf8', env, timeout: NOTE_TIMEOUT_MS },
  )
  if (result.error) throw result.error
  if (result.status !== 0)
    throw new Error(
      `boss notes add exited ${result.status}: ${oneLine(result.stderr).slice(0, 200)}`,
    )
}

/**
 * Record one verdict file. Never throws.
 * @param {string} verdictFile
 * @param {{env?: object, fs?: typeof defaultFs, cwd?: string, host?: string, now?: () => Date, addNote?: (note: object, ctx: object) => void}} [deps]
 */
export function recordVerdictFile(verdictFile, deps = {}) {
  const env = deps.env ?? process.env
  const fs = deps.fs ?? defaultFs
  try {
    if (env.BOSS_BS_REVIEW === '0') return { action: 'ignored', reason: 'review disabled' }
    let config
    try {
      config = loadSkillConfig({ cwd: deps.cwd ?? process.cwd() })
    } catch {
      config = undefined
    }
    if (!reviewSecondModel(config))
      return { action: 'ignored', reason: 'second model configured off' }
    const verdict = JSON.parse(fs.readFileSync(verdictFile, 'utf8'))
    const host = deps.host ?? os.hostname()
    const now = (deps.now ?? (() => new Date()))()
    const dir = stateDir(env)
    fs.mkdirSync(dir, { recursive: true })
    const file = path.join(dir, STATE_FILENAME)
    return withLock(fs, dir, () => {
      const result = applyVerdict(readState(fs, file), verdict, { host, now })
      let noted = false
      let noteError
      if (result.note && env.BOSS_NOTES_SUPPRESSED !== '1') {
        try {
          ;(deps.addNote ?? defaultAddNote)(result.note, { env })
          noted = true
        } catch (error) {
          noteError = oneLine(error?.message ?? error).slice(0, 200)
        }
      }
      // An unrecorded fact stays due, so the next failure retries it under the same key.
      const streak = result.state.agents[result.agent]
      if (result.action === 'failed' && streak && result.note) streak.emitted = noted
      if (result.action !== 'ignored' && result.action !== 'duplicate')
        writeState(fs, file, result.state)
      const out = { action: result.action, noted }
      if (result.agent) out.agent = result.agent
      if (typeof result.count === 'number') out.count = result.count
      if (result.reason) out.reason = result.reason
      if (noteError) out.noteError = noteError
      return out
    })
  } catch (error) {
    return { action: 'error', noted: false, error: oneLine(error?.message ?? error).slice(0, 200) }
  }
}

/**
 * @param {string[]} argv
 * @param {{stdout?: (s: string) => void, stderr?: (s: string) => void, env?: object, cwd?: string}} [deps]
 * @returns {number}
 */
export function main(argv, deps = {}) {
  const stdout = deps.stdout ?? ((s) => process.stdout.write(s))
  const stderr = deps.stderr ?? ((s) => process.stderr.write(s))
  let parsed
  try {
    parsed = parseArgs({ args: argv, options: {}, allowPositionals: true, strict: true })
  } catch (error) {
    stderr(`second-model-health: ${error.message}\n${USAGE}\n`)
    return 2
  }
  const [command, file, ...extra] = parsed.positionals
  if (command !== 'record' || !file || extra.length) {
    stderr(`second-model-health: bad arguments\n${USAGE}\n`)
    return 2
  }
  stdout(`${JSON.stringify(recordVerdictFile(file, deps))}\n`)
  return 0
}

if (isMainModule(import.meta.url)) process.exitCode = main(process.argv.slice(2))
