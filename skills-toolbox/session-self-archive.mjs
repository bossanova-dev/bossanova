#!/usr/bin/env node
// skills-toolbox/session-self-archive.mjs
// Decide how a finished planning run ends its own boss session, and do it.
//
// An unattended run that bossd finalizes — a scheduled job (BOSS_CRON_JOB_ID) or a zero-output run
// (BOSS_ZERO_OUTPUT=true) — gets the `finish` verdict: the helper runs `boss session finish`, and
// bossd runs the run's normal finalize once every chat is idle and the session's agent logs have
// gone quiet. That quiet-log guard is what keeps background agents alive, so every such run goes
// through it rather than archiving itself.
//
// Any other run may archive its own session, launched so it outlives the pane it kills. Archiving
// kills every chat's tmux pane, removes the worktree and marks the row, so that decision refuses
// whenever it could hurt anything: no daemon, a caller that owns the session, an unfinished
// outcome, retained scratch, a PR, another live chat, or a dirty worktree. Unfinished outcomes and
// retained scratch skip a finish too, keeping the session for inspection. The tests are the
// specification of the rule order.
//
//   node session-self-archive.mjs --outcome <o> --run-scratch <dir> [--confirmed] [--suppressed] [--delay-ms N]
//
// Prints one JSON line `{"verdict","reason","sessionId"[,"log"][,"finishOutcome"][,"detail"]}` and
// exits 0 on every decision — the step is never fatal. Exit 64 only on a usage error.
//
// Node built-ins plus ./main-module.mjs and ./boss-binary.mjs only.

import { execFile as defaultExecFile, spawn as defaultSpawn } from 'node:child_process'
import defaultFs from 'node:fs'
import os from 'node:os'
import path from 'node:path'

import { resolveBossBinary } from './boss-binary.mjs'
import { isMainModule } from './main-module.mjs'

// Outcomes that mean the planning run is completely finished and left nothing to inspect.
export const FINISHED_OUTCOMES = Object.freeze(['planned', 'noop', 'epic', 'empty-queue'])

// Long enough for the verdict line to reach the agent before the archive closes its pane.
export const DEFAULT_DELAY_MS = 3000

export const EXIT_USAGE = 64

// The only chat status that is not live. UNSPECIFIED and any unknown value count as live.
const STOPPED = 'STOPPED'

const skip = (reason, detail) =>
  detail ? { verdict: 'skip', reason, detail } : { verdict: 'skip', reason }

/**
 * True when no human is watching this chat. bossd sets BOSS_UNATTENDED=true as the primary marker
 * and BOSS_CRON=true as its legacy spelling; either one counts.
 */
export function isUnattended(env = {}) {
  return env.BOSS_UNATTENDED === 'true' || env.BOSS_CRON === 'true'
}

/** True for an unattended run in the repository checkout, which bossd marks BOSS_ZERO_OUTPUT=true. */
export function isZeroOutput(env) {
  return env.BOSS_ZERO_OUTPUT === 'true'
}

/**
 * True for an unattended run whose cleanup is bossd's finalize: a scheduled job or a zero-output
 * run. Such a run ends with `boss session finish`, never an archive.
 */
export function finishesThroughDaemon(env = {}) {
  return isUnattended(env) && (Boolean(env.BOSS_CRON_JOB_ID) || isZeroOutput(env))
}

function normalizeStatus(status) {
  return String(status ?? '')
    .trim()
    .toUpperCase()
    .replace(/^CHAT_STATUS_/, '')
}

/**
 * The rules that need no daemon or git read. Returns a skip verdict, the finish verdict, or null
 * when the run is eligible to archive so far. The CLI calls this first so it never reads the daemon
 * for a run that the environment or outcome alone decides.
 */
export function preflightSelfArchive({
  env = {},
  outcome,
  runScratchExists = false,
  suppressed = false,
} = {}) {
  if (!env.BOSS_SESSION_ID) return skip('not-managed')
  if (suppressed || env.BOSS_SELF_ARCHIVE_SUPPRESSED === '1') return skip('caller-owns-session')
  if (!FINISHED_OUTCOMES.includes(outcome)) return skip(`outcome-${outcome || 'missing'}`)
  if (runScratchExists) return skip('scratch-retained')
  if (finishesThroughDaemon(env)) return { verdict: 'finish', reason: '' }
  // A job id without the unattended marker is not a shape bossd launches; its finalize still owns
  // cleanup, so it never archives itself.
  if (env.BOSS_CRON_JOB_ID) return skip('cron-finalize-owns-cleanup')
  return null
}

/**
 * The whole decision. `session` is the `session` object from `boss show <id> --json` and `chats`
 * the `chats` array from `boss chats <id> --json`; either being null means it could not be read.
 * `worktreeClean` is true (no porcelain output), false (dirty) or null (git failed).
 * @returns {{verdict: 'archive'|'finish'|'ask'|'skip', reason: string, detail?: string}}
 */
export function decideSelfArchive({
  env = {},
  outcome,
  runScratchExists = false,
  session = null,
  chats = null,
  worktreeClean = null,
  confirmed = false,
  suppressed = false,
} = {}) {
  const early = preflightSelfArchive({ env, outcome, runScratchExists, suppressed })
  if (early) return early

  if (!session || typeof session !== 'object' || !Array.isArray(chats))
    return skip('session-unreadable')
  if (session.archived_at != null && String(session.archived_at).trim() !== '')
    return skip('already-archived')
  if (session.pr_number != null) return skip('session-has-pr')

  const self = env.BOSS_AGENT_SESSION_ID
  if (!self || !chats.some((chat) => chat?.agent_session_id === self))
    return skip('self-chat-unknown')
  const liveOthers = chats.filter(
    (chat) => chat?.agent_session_id !== self && normalizeStatus(chat?.status) !== STOPPED,
  )
  if (liveOthers.length > 0) return skip('other-live-chats')

  // A zero-output run's "worktree" is the shared repository checkout: its state is not this run's
  // to answer for, and archiving never touches it.
  if (!isZeroOutput(env)) {
    if (worktreeClean === false) return skip('worktree-dirty')
    if (worktreeClean !== true) return skip('worktree-unreadable')
  }

  if (!isUnattended(env) && !confirmed) return { verdict: 'ask', reason: 'attended-confirm' }
  return { verdict: 'archive', reason: '' }
}

// ---------------------------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------------------------

export class UsageError extends Error {}

export function parseArgs(argv) {
  const opts = {
    outcome: undefined,
    runScratch: undefined,
    confirmed: false,
    suppressed: false,
    delayMs: DEFAULT_DELAY_MS,
  }
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i]
    const value = () => {
      const next = argv[++i]
      if (next === undefined || next.startsWith('--')) throw new UsageError(`${arg} needs a value`)
      return next
    }
    if (arg === '--outcome') opts.outcome = value()
    else if (arg === '--run-scratch') opts.runScratch = value()
    else if (arg === '--confirmed') opts.confirmed = true
    else if (arg === '--suppressed') opts.suppressed = true
    else if (arg === '--delay-ms') {
      const raw = value()
      if (!/^\d+$/.test(raw)) throw new UsageError('--delay-ms must be a non-negative integer')
      opts.delayMs = Number(raw)
    } else throw new UsageError(`unknown argument: ${arg}`)
  }
  if (!opts.outcome) throw new UsageError('--outcome is required')
  if (!opts.runScratch) throw new UsageError('--run-scratch is required')
  return opts
}

function execFileResult(execFile, file, args, options) {
  return new Promise((resolve) => {
    execFile(file, args, options, (error, stdout) =>
      resolve({ error, stdout: String(stdout ?? '') }),
    )
  })
}

async function readJson(execFile, file, args, key) {
  const { error, stdout } = await execFileResult(execFile, file, args, {
    cwd: os.tmpdir(),
    maxBuffer: 8 * 1024 * 1024,
  })
  if (error) return null
  try {
    const parsed = JSON.parse(stdout)
    return parsed?.[key] ?? null
  } catch {
    return null
  }
}

async function readWorktreeClean(execFile, worktree) {
  if (!worktree) return null
  const { error, stdout } = await execFileResult(
    execFile,
    'git',
    ['-C', worktree, 'status', '--porcelain'],
    {},
  )
  if (error) return null
  return stdout.trim() === ''
}

// Bounds the synchronous finish call. The daemon records the request and answers at once (the
// requester's own finish is always deferred), so this only matters for a wedged daemon socket.
export const FINISH_TIMEOUT_MS = 60_000

/**
 * Run `boss session finish --json` synchronously from the temp dir and report what the daemon did.
 * The CLI reads the session and chat ids from env. Never throws: any failure is skip:finish-failed.
 */
export async function requestFinish({ execFile, bossPath, env, tmpdir }) {
  const { error, stdout } = await new Promise((resolve) => {
    execFile(
      bossPath,
      ['session', 'finish', '--json'],
      { cwd: tmpdir, env, timeout: FINISH_TIMEOUT_MS, maxBuffer: 1024 * 1024 },
      (err, out, errOut) =>
        resolve({
          error: err ? String(errOut ?? '').trim() || String(err?.message ?? err) : null,
          stdout: String(out ?? ''),
        }),
    )
  })
  if (error) return skip('finish-failed', error)
  let parsed
  try {
    parsed = JSON.parse(stdout)
  } catch {
    return skip('finish-failed', `unreadable finish reply: ${stdout.trim().slice(0, 200)}`)
  }
  const finishOutcome = parsed?.outcome
  if (finishOutcome !== 'pending' && finishOutcome !== 'finished')
    return skip('finish-failed', `unexpected finish outcome: ${String(finishOutcome)}`)
  const result = { verdict: 'finish', reason: '', finishOutcome }
  if (parsed.blocking_agent_session_id)
    result.blockingAgentSessionId = String(parsed.blocking_agent_session_id)
  return result
}

/**
 * Launch `boss archive <id>` detached after `delayMs`, so it survives the pane it kills. The cwd is
 * the temp dir so nothing keeps the doomed worktree open; output goes to a log under the temp dir.
 * Resolves once the child has spawned and rejects if it could not: spawn reports EAGAIN or ENOENT
 * as an asynchronous 'error' event, which would crash the helper with no listener attached.
 */
export async function launchDetachedArchive({
  bossPath,
  sessionId,
  delayMs,
  spawn,
  fs,
  tmpdir,
  now,
}) {
  const log = path.join(tmpdir, `boss-self-archive-${sessionId}-${now}.log`)
  const fd = fs.openSync(log, 'a')
  try {
    const child = spawn(
      '/bin/sh',
      [
        '-c',
        'sleep "$1"; exec "$2" archive "$3"',
        'boss-self-archive',
        String(delayMs / 1000),
        bossPath,
        sessionId,
      ],
      { detached: true, cwd: tmpdir, stdio: ['ignore', fd, fd] },
    )
    if (typeof child?.once === 'function') {
      await new Promise((resolve, reject) => {
        child.once('spawn', resolve)
        child.once('error', reject)
      })
    }
    child?.unref?.()
  } finally {
    fs.closeSync(fd)
  }
  return log
}

/**
 * Gather the inputs, decide, and act. Every side effect is injected so tests never touch a real
 * daemon, PATH or repository.
 */
export async function runSelfArchive(opts, deps = {}) {
  const env = deps.env ?? process.env
  const fs = deps.fs ?? defaultFs
  const execFile = deps.execFile ?? defaultExecFile
  const spawn = deps.spawn ?? defaultSpawn
  const tmpdir = deps.tmpdir ?? os.tmpdir()
  const cwd = deps.cwd ?? process.cwd()
  const now = deps.now ?? Date.now()
  const sessionId = env.BOSS_SESSION_ID || ''

  const runScratchExists = fs.existsSync(path.resolve(cwd, opts.runScratch))
  const base = {
    env,
    outcome: opts.outcome,
    runScratchExists,
    confirmed: opts.confirmed,
    suppressed: opts.suppressed,
  }
  const early = preflightSelfArchive(base)
  if (early && early.verdict !== 'finish') return { ...early, sessionId }

  const resolved = resolveBossBinary(env, { fs, cwd })
  if (!resolved.ok) {
    const reason = early ? 'finish-failed' : 'session-unreadable'
    return { ...skip(reason, resolved.reason), sessionId }
  }
  // The resolver may return a relative path (BOSS_BIN=./bin/boss, a relative PATH entry), checked
  // against this cwd; every exec below runs from the temp dir, so anchor it here.
  const bossPath = path.resolve(cwd, resolved.path)

  if (early) return { ...(await requestFinish({ execFile, bossPath, env, tmpdir })), sessionId }

  const [session, chats, worktreeClean] = await Promise.all([
    readJson(execFile, bossPath, ['show', sessionId, '--json'], 'session'),
    readJson(execFile, bossPath, ['chats', sessionId, '--json'], 'chats'),
    isZeroOutput(env) ? null : readWorktreeClean(execFile, env.BOSS_WORKTREE),
  ])
  const decision = decideSelfArchive({ ...base, session, chats, worktreeClean })
  if (decision.verdict !== 'archive') return { ...decision, sessionId }

  let log
  try {
    log = await launchDetachedArchive({
      bossPath,
      sessionId,
      delayMs: opts.delayMs,
      spawn,
      fs,
      tmpdir,
      now,
    })
  } catch (error) {
    return { ...skip('error', String(error?.message ?? error)), sessionId }
  }
  return { ...decision, sessionId, log }
}

async function main(argv) {
  let opts
  try {
    opts = parseArgs(argv)
  } catch (error) {
    if (!(error instanceof UsageError)) throw error
    process.stderr.write(
      `session-self-archive: ${error.message}\nusage: node session-self-archive.mjs --outcome <o> --run-scratch <dir> [--confirmed] [--suppressed] [--delay-ms N]\n`,
    )
    return EXIT_USAGE
  }
  let result
  try {
    result = await runSelfArchive(opts)
  } catch (error) {
    // Never fatal: an unexpected failure is a skip the agent prints and moves past.
    result = {
      ...skip('error', String(error?.message ?? error)),
      sessionId: process.env.BOSS_SESSION_ID || '',
    }
  }
  process.stdout.write(`${JSON.stringify(result)}\n`)
  return 0
}

if (isMainModule(import.meta.url)) {
  process.exitCode = await main(process.argv.slice(2))
}
