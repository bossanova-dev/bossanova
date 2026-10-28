#!/usr/bin/env node

// verify-route.mjs — the verify stage's dispatcher: discover the PRs that need judging, act on a
// bounded batch of them, and get agent work into each PR's own boss session. Both the cron gate
// (cron-gates/boss-verify.mjs) and `/boss-verify` with no argument run this same `sweep`.
//
//   sweep [--dry-run] [--batch N] [--budget-ms MS] [--local-sessions-only] [selection flags]
//
// prints one JSON line per PR, then `{"summary": {...}}`. Per candidate it runs verify-gate's
// zero-token `judge`, then:
//
//   wait                 nothing
//   human                post --verdict human
//   pass                 post --verdict pass, then hand the merge to a detached
//                        `verify-gate merge` (needs `boss`); under an admission tier, a non-fork PR
//                        with no live session is claimed and routed like extensions-required
//   extensions-required  post --verdict claim; on a won claim, route `/boss-verify <pr> --claim
//   review-required      <token>` into the PR's session (needs `boss`)
//
// Merges: a head-pinned merge waits on the repo merge lock and the daemon's post-merge refresh, so
// it can outlast any per-call bound, and a killed merge loses its result and its tracker write. So
// a merge is never run inside the sweep: it is spawned detached (its own log, unbound by this
// budget, so it always finishes and moves the ticket to Done, or prints `merge-error` if it
// crashes), and after the candidate loop the
// sweep polls each merge's log for its result. Once only OBSERVE_TIMEOUT_MS per open merge is left,
// each one still open gets one read-only `verify-gate observe-merge`: merged on GitHub is
// `merged <sha>`, anything else is `dispatched: merge in flight (log …)`.
//
// Every subprocess goes through one injected `run(cmd, args, {timeoutMs, json, env})` seam whose
// failed read is unknown, never an empty success. One deadline bounds the whole sweep: discovery
// collects up to max(25, 3x `--batch`) judge candidates and scans statuses for merge retries
// within min(30 s, remaining - 15 s). Every other call gets min(15 s, remaining), no new PR starts with less than 15 s left, and at most `--batch` PRs are
// judged, plus at most `--batch` verified heads retried for merge. What is left over is `deferred`.
//
// `--local-sessions-only` is for a daemon that shares the repo with another machine: it reads this
// daemon's session list before discovery (an unreadable list throws, so the gate fails closed) and
// acts only on PRs with a live session here. Every other PR is `skipped: session on another
// machine` before any judge, post, claim, spawn or merge, and never counts against `--batch`.

import {
  closeSync,
  mkdirSync,
  openSync,
  readFileSync,
  renameSync,
  rmSync,
  statSync,
  writeFileSync,
} from 'node:fs'
import { spawn as nodeSpawn, spawnSync } from 'node:child_process'
import { tmpdir } from 'node:os'
import { delimiter, dirname, isAbsolute, join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { loadSkillConfig } from '../skill-config.mjs'
import { configuredTrackerName } from '../tracker/adapter.mjs'
import { repoRootFrom, resolveBossBinary } from '../boss-binary.mjs'
import { storeAdmission, writeAtomicJson } from './verify-admission.mjs'
import { isMainModule } from '../main-module.mjs'
import { VERIFY_CHAT_TITLE, pickVerifyChat, verifyChatArgv } from '../stage-chain.mjs'
import { SELECTION_FLAGS, parseSelectionFlags } from '../selection.mjs'
import { REMOTE_SESSION_REASON } from './verify-vocabulary.mjs'

export const DEFAULT_BUDGET_MS = 45_000
export const DEFAULT_BATCH = 3
export const CALL_TIMEOUT_MS = 15_000
// A new PR is not started with less than this left on the deadline.
export const START_FLOOR_MS = 15_000
// Each handed-off merge still open when polling stops gets one read-only observation bounded by
// this; polling stops while that much per open merge is left, so every observation ends in budget.
export const OBSERVE_TIMEOUT_MS = 5_000
export const MERGE_POLL_MS = 500
// An observation is not started with less than this left.
const MIN_OBSERVE_MS = 1_000
// The verify chat's title and finder are stage-chain.mjs's, so this router and boss-build's
// `arm-verify` always pick the same chat.
export { VERIFY_CHAT_TITLE }

const VERIFY_GATE = fileURLToPath(new URL('./verify-gate.mjs', import.meta.url))
const text = (value) => typeof value === 'string' && value.trim() !== ''

export class UsageError extends Error {}

// ---------------------------------------------------------------------------
// The subprocess seam.

/** A `run(cmd, args, {timeoutMs, json, env})` over spawnSync, shaped like verify-gate's reader. */
export function timedRunner({ spawn = spawnSync, cwd = process.cwd(), env = process.env } = {}) {
  return (command, args, { timeoutMs = CALL_TIMEOUT_MS, json = false, env: childEnv } = {}) => {
    let response
    try {
      response = spawn(command, args, {
        cwd,
        env: childEnv ?? env,
        encoding: 'utf8',
        timeout: timeoutMs,
        maxBuffer: 64 * 1024 * 1024,
        killSignal: 'SIGKILL',
        stdio: ['ignore', 'pipe', 'pipe'],
      })
    } catch (error) {
      return { ok: false, error: error?.code ?? 'spawn-error', exit: null, output: '' }
    }
    const output = String(response?.stdout ?? '').trim()
    let payload
    if (json) {
      // `boss --json` pretty-prints one object across many lines; verify-gate prints one JSON
      // line, possibly after log lines. Try the whole output first, then the last line.
      for (const candidate of [output, output.split('\n').filter(Boolean).pop() ?? '']) {
        try {
          payload = JSON.parse(candidate)
          break
        } catch {
          /* Failure remains unknown. */
        }
      }
    }
    if (response?.error || response?.status !== 0)
      return {
        ok: false,
        error:
          payload?.error?.code ??
          payload?.code ??
          response?.error?.code ??
          `exit-${response?.status}`,
        exit: response?.error ? null : (response?.status ?? null),
        payload,
        output,
      }
    if (json && payload === undefined)
      return { ok: false, error: 'unreadable-json', exit: 0, output }
    return { ok: true, payload, output, exit: 0 }
  }
}

/**
 * Spawn a command fully detached: its own process group, stdio to a log file (never the caller's
 * pipes), unref'd. The cron gate's runner waits on its child's stdout pipe, so a child that
 * inherited it would hold the gate open past its own exit and turn every dispatch into a gate
 * failure. `label` names the log (`boss-verify-<label>-<ms>-<pid>.log`), so children spawned in the
 * same millisecond under different labels never share one.
 */
export function spawnDetached(
  command,
  args,
  {
    env = process.env,
    cwd = process.cwd(),
    logDir = tmpdir(),
    now = Date.now,
    label = 'dispatch',
  } = {},
  { spawn = nodeSpawn, open = openSync, close = closeSync } = {},
) {
  const log = join(logDir, `boss-verify-${label}-${now()}-${process.pid}.log`)
  let fd
  try {
    fd = open(log, 'a')
  } catch (error) {
    return { ok: false, error: error?.code ?? 'log-open-failed' }
  }
  try {
    const child = spawn(command, args, { cwd, env, detached: true, stdio: ['ignore', fd, fd] })
    child.on?.('error', () => {})
    child.unref?.()
    return { ok: true, log, pid: child.pid ?? null }
  } catch (error) {
    return { ok: false, error: error?.code ?? 'spawn-error', log }
  } finally {
    close(fd)
  }
}

/**
 * A detached `verify-gate merge`'s result from its log, or `null` while it is not done. verify-gate
 * prints exactly one JSON object on stdout at exit and stderr shares the log, so this is the last
 * complete line that parses to an object carrying a boolean `merged`; a line still being written
 * (no newline yet) is not done.
 */
export function readMergeResult(log) {
  if (typeof log !== 'string') return null
  const complete = log.split('\n').slice(0, -1)
  for (let i = complete.length - 1; i >= 0; i--) {
    let value
    try {
      value = JSON.parse(complete[i])
    } catch {
      continue
    }
    if (value && typeof value === 'object' && typeof value.merged === 'boolean') return value
  }
  return null
}

// A synchronous sleep, so `sweep` stays synchronous while it polls.
function sleepSync(ms) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, Math.max(0, ms))
}

// ---------------------------------------------------------------------------
// Arguments.

/**
 * `sweep`'s flags: `--batch N`, `--budget-ms MS`, `--dry-run`, `--local-sessions-only`, plus the
 * eight shared selection flags, kept verbatim for the candidates read. Any other token is refused: an ignored token would
 * be a filter the operator believes is applied.
 */
export function parseSweepArgs(argv = []) {
  const parsed = parseSelectionFlags(argv)
  if (parsed.error) throw new UsageError(parsed.error)
  // Re-walk the argv to keep the selection tokens verbatim and in order.
  const selectionArgs = []
  for (let i = 0; i < argv.length; i++) {
    const token = argv[i]
    const name = typeof token === 'string' ? token.split('=')[0] : token
    if (!Object.hasOwn(SELECTION_FLAGS, name)) continue
    selectionArgs.push(token)
    if (!token.includes('=')) selectionArgs.push(argv[++i])
  }
  const out = {
    batch: DEFAULT_BATCH,
    budgetMs: DEFAULT_BUDGET_MS,
    dryRun: false,
    localSessionsOnly: false,
    selectionArgs,
  }
  const rest = parsed.positionals
  for (let i = 0; i < rest.length; i++) {
    const token = rest[i]
    if (token === '--dry-run') {
      out.dryRun = true
      continue
    }
    if (token === '--local-sessions-only') {
      out.localSessionsOnly = true
      continue
    }
    const [name, inline] = typeof token === 'string' ? token.split('=', 2) : []
    if (name !== '--batch' && name !== '--budget-ms')
      throw new UsageError(`unexpected argument ${JSON.stringify(token)}`)
    const raw = inline ?? rest[++i]
    const value = Number(raw)
    if (!Number.isInteger(value) || value < 1)
      throw new UsageError(`${name} must be a positive integer, got ${JSON.stringify(raw)}`)
    out[name === '--batch' ? 'batch' : 'budgetMs'] = value
  }
  return out
}

// ---------------------------------------------------------------------------
// Dispatchability.

// `boss ls --json` failures that mean the CLI or its daemon is not reachable from here — as
// opposed to a well-formed refusal from a daemon that answered.
const UNREACHABLE_CODES = new Set([
  'UNAVAILABLE',
  'UNKNOWN',
  'DEADLINE_EXCEEDED',
  'unreadable-json',
])
export function lsFailureIsUnreachable(result) {
  if (result?.ok) return false
  if (result?.exit === null) return true
  if (result?.payload === undefined) return true
  return UNREACHABLE_CODES.has(result?.error)
}

// ---------------------------------------------------------------------------
// Routing.

// `waive` carries an approved policy park through the claim: posting the claim replaces the parked
// status the in-session hand-back would otherwise recover the waiver from.
export function verifyMessage(pr, token, waive) {
  return `/boss-verify ${pr} --claim ${token}${text(waive) ? ` --waive ${waive}` : ''}`
}

export function newSessionArgv({ pr, repoDir, ticket, token, waive, trackerSource }) {
  return [
    'new',
    '--repo',
    repoDir,
    '--pr',
    String(pr),
    '--prompt',
    verifyMessage(pr, token, waive),
    '--title',
    `verify #${pr}`,
    '--tmux-unattended',
    ...(text(ticket) && text(trackerSource)
      ? ['--tracker-id', ticket, '--tracker-source', trackerSource]
      : []),
  ]
}

// `boss chat send` exits 0 whether the daemon confirmed the send, queued it behind a running turn,
// or could not confirm it at all, so the route reads `--json`'s delivery state. A queued send is
// held by the agent and runs when its turn ends; anything else leaves the claim to go stale.
export const CONFIRMED_DELIVERY_STATES = new Set([
  'DELIVERY_STATE_SUBMITTED',
  'DELIVERY_STATE_QUEUED',
])

function sendVerify(run, boss, chatId, message, timeoutMs) {
  const sent = run(boss, ['chat', 'send', chatId, message, '--json'], { json: true, timeoutMs })
  if (!sent.ok) return { ok: false, why: sent.error }
  const delivery = sent.payload?.delivery
  if (delivery?.delivered === true && CONFIRMED_DELIVERY_STATES.has(delivery.delivery_state))
    return { ok: true }
  return {
    ok: false,
    why: text(delivery?.delivery_state) ? delivery.delivery_state : 'unreadable-delivery',
  }
}

// The live rows bound to a PR URL, newest first.
export function liveSessionsFor(sessions, url) {
  return (Array.isArray(sessions) ? sessions : [])
    .filter((row) => text(url) && row?.pr_url === url && !text(row?.archived_at) && text(row?.id))
    .sort((a, b) => String(b.updated_at ?? '').localeCompare(String(a.updated_at ?? '')))
}

/**
 * Get `/boss-verify <pr> --claim <token>` into the PR's session. Returns `{route, ok, note?, log?,
 * why?}`; `ok: false` is `dispatch-unknown` (the claim goes stale and the next run re-routes).
 */
export function routeCandidate({ pr, url, ticket, token, waive }, ctx) {
  const { run, boss, sessions, dryRun, timeout } = ctx
  const message = verifyMessage(pr, token, waive)
  const rows = liveSessionsFor(sessions, url)
  if (rows.length > 0) {
    const session = rows[0]
    const note = rows.length > 1 ? `newest of ${rows.length} live sessions` : undefined
    const chats = run(boss, ['chats', '--json', session.id], { json: true, timeoutMs: timeout() })
    if (!chats.ok || !Array.isArray(chats.payload?.chats))
      return { route: 'chats-unreadable', ok: false, why: chats.error ?? 'unreadable', note }
    const verify = pickVerifyChat(chats.payload.chats)
    if (verify) {
      if (dryRun) return { route: 'existing-verify-chat', ok: true, note }
      const sent = sendVerify(run, boss, verify.agent_session_id, message, timeout())
      return { route: 'existing-verify-chat', ok: sent.ok, why: sent.why, note }
    }
    if (dryRun) return { route: 'new-verify-chat', ok: true, note }
    const created = run(boss, verifyChatArgv(session.id), { json: true, timeoutMs: timeout() })
    const chatId = created.payload?.chat?.agent_session_id
    if (!created.ok || !text(chatId))
      return { route: 'new-verify-chat', ok: false, why: created.error ?? 'no-chat-id', note }
    const sent = sendVerify(run, boss, chatId, message, timeout())
    return { route: 'new-verify-chat', ok: sent.ok, why: sent.why, note }
  }
  const argv = newSessionArgv({
    pr,
    repoDir: ctx.repoDir,
    ticket,
    token,
    waive,
    trackerSource: ctx.trackerSource,
  })
  if (dryRun) return { route: 'new-session', ok: true, argv }
  const spawned = ctx.spawnDetached(boss, argv)
  return { route: 'new-session', ok: spawned.ok, log: spawned.log, why: spawned.error, argv }
}

// ---------------------------------------------------------------------------
// The sweep. Shared repositories subtract local ownership from the orchestrator's live fleet;
// remote PRs are excluded at discovery, and an unreadable fleet blocks only new sessions.

// Discovery keeps a floor for small batches and reads a few times more judge candidates than
// one run acts on, so waiting PRs do not exhaust a small discovery limit. The wider read gets
// a longer call timeout, still leaving START_FLOOR_MS of the budget for starting a PR.
export const DISCOVERY_FACTOR = 3
export const DISCOVERY_FLOOR = 25
export const discoveryLimit = (batch) => Math.max(DISCOVERY_FLOOR, batch * DISCOVERY_FACTOR)
const OUTCOMES = ['merged', 'parked', 'dispatched', 'skipped', 'deferred']
export const REMOTE_SESSION_SKIP = 'skipped: session on another machine'
// Below this share of either GitHub quota left, a run reads nothing: the token is the user's, and
// every other tool on the machine spends it too.
export const MIN_QUOTA_SHARE = 0.25

/**
 * Why this run should leave GitHub alone, or null. `gh api rate_limit` does not count against
 * either quota; an unreadable answer is null, so a run never stalls on the courtesy check itself.
 */
export function quotaLow(run, timeoutMs) {
  const r = run('gh', ['api', 'rate_limit'], { json: true, timeoutMs })
  if (!r.ok) return null
  for (const name of ['core', 'graphql']) {
    const q = r.payload?.resources?.[name]
    const { limit, remaining, reset } = q ?? {}
    if (![limit, remaining].every(Number.isFinite) || limit <= 0) continue
    if (remaining / limit >= MIN_QUOTA_SHARE) continue
    const at = Number.isFinite(reset) ? `, resets ${new Date(reset * 1000).toISOString()}` : ''
    return `GitHub ${name} quota low (${remaining}/${limit} left${at})`
  }
  return null
}

// ---------------------------------------------------------------------------
// The unchanged-tick state: `{version: 1, entries: {<scopeKey>: {fingerprint, at}}}` in the repo's
// git common dir, with an additive checkedAt ms-epoch last-check timestamp. Anything unreadable,
// unparseable, of another version or missing reads as no
// previous fingerprint, and a write that fails is never an error: the next tick just runs in full.

export const SWEEP_STATE_VERSION = 1
const FS = { readFileSync, writeFileSync, mkdirSync, renameSync, rmSync }

/** One sweep scope's key: its selection args and whether it acts on local sessions only. */
export function sweepScopeKey({
  selectionArgs = [],
  localSessionsOnly = false,
  shared = false,
  eventPrUrl,
} = {}) {
  return JSON.stringify({
    selection: selectionArgs,
    localSessionsOnly: localSessionsOnly === true,
    shared: shared === true,
    ...(eventPrUrl ? { eventPrUrl } : {}),
  })
}

/** `<git common dir>/boss-verify/sweep-state.json`, or null when git cannot say. */
export function sweepStatePath(run, cwd, timeoutMs) {
  const r = run('git', ['rev-parse', '--git-common-dir'], { timeoutMs })
  if (!r.ok || !text(r.output)) return null
  const common = r.output.trim()
  return join(isAbsolute(common) ? common : join(cwd, common), 'boss-verify', 'sweep-state.json')
}

function readEntries(fs, file) {
  try {
    const state = JSON.parse(fs.readFileSync(file, 'utf8'))
    if (state?.version !== SWEEP_STATE_VERSION) return {}
    const entries = state.entries
    return entries && typeof entries === 'object' && !Array.isArray(entries) ? entries : {}
  } catch {
    return {}
  }
}

/** The quiet fingerprint stored for `key`, or null. */
export function readQuietFingerprint(file, key, fs = FS) {
  if (!text(file)) return null
  const entry = readEntries(fs, file)[key]
  return text(entry?.fingerprint) ? entry.fingerprint : null
}

// Writes the state atomically: a fresh temp file, renamed over the old one.
function writeEntries(fs, file, entries) {
  return writeAtomicJson(fs, file, { version: SWEEP_STATE_VERSION, entries })
}

/** Stores `fingerprint` for `key`; false when it could not be written. */
export function storeQuietFingerprint(file, key, fingerprint, at, fs = FS) {
  if (!text(file)) return false
  return writeEntries(fs, file, {
    ...readEntries(fs, file),
    [key]: { fingerprint, at, checkedAt: Date.parse(at) },
  })
}

/** Removes `key`'s entry; true when none is left. */
export function forgetQuietFingerprint(file, key, fs = FS) {
  if (!text(file)) return true
  const entries = readEntries(fs, file)
  if (!Object.hasOwn(entries, key)) return true
  delete entries[key]
  return writeEntries(fs, file, entries)
}

// The merge waits that may leave a tick quiet: each is a steady state whose way out moves the
// fingerprint (merge state, `boss/verify` status, `updatedAt` on a new operator comment). Any other
// wait, including a reasons-only one, may rest on CI evidence the fingerprint does not hash (an
// unreadable prior-head or workflow-run read, a run the rollup does not show yet): never quiet.
const QUIET_MERGE_WAITS = new Set(['merge-state-dirty', 'not-verified', 'sent-back'])

/**
 * Was this tick quiet, so an unchanged next tick may skip? Discovery was complete (`skippable`) and
 * found no candidate and no `unknown:` skip, nothing was undispatchable, and every record is a
 * remote-session skip or a merge retry that settled as a write-free wait.
 */
export function tickWasQuiet(payload, records, undispatchable) {
  if (payload?.skippable !== true || !text(payload?.fingerprint)) return false
  if (!Array.isArray(payload.candidates) || payload.candidates.length > 0) return false
  const skipped = Array.isArray(payload.skipped) ? payload.skipped : []
  if (skipped.some((s) => String(s?.reason ?? '').startsWith('unknown:'))) return false
  if (undispatchable) return false
  return records.every((r) => r?.remote === true || r?.quiet === true)
}

/**
 * Discover, then act on a bounded batch. Returns `{records, summary, undispatchable, idle?}`, where
 * each record is `{pr, outcome, line, ...}` with `outcome` one of merged / parked / dispatched /
 * skipped / deferred, and `idle` says why a run read nothing (low GitHub quota, or no local PR
 * session under `--local-sessions-only`). With `skipUnchanged`, `unchanged: true` says the tick
 * matched the last quiet one and did nothing. Throws when discovery itself fails (a tracker, config
 * or open-PR read failure): the caller must fail closed rather than read that as "nothing to do".
 */
export function sweep(options = {}, deps = {}) {
  const env = deps.env ?? process.env
  const cwd = deps.cwd ?? process.cwd()
  let trackerSource
  const routedTrackerSource = () => {
    if (trackerSource === undefined) {
      try {
        trackerSource = trackerSourceFor(
          deps.trackerName ?? configuredTrackerName({ env, config: loadSkillConfig({ cwd }) }),
        )
      } catch {
        trackerSource = ''
      }
    }
    return trackerSource
  }
  const now = deps.now ?? Date.now
  const batch = options.batch ?? DEFAULT_BATCH
  const dryRun = options.dryRun === true
  const localOnly = options.localSessionsOnly === true
  const deadline = now() + (options.budgetMs ?? DEFAULT_BUDGET_MS)
  const remaining = () => deadline - now()
  const timeout = () => Math.max(1000, Math.min(CALL_TIMEOUT_MS, remaining()))
  const run = deps.run ?? timedRunner({ cwd, env })
  const readFile = deps.readFile ?? ((path) => readFileSync(path, 'utf8'))
  const sleep = deps.sleep ?? sleepSync
  const logDir = deps.logDir ?? tmpdir()
  const node = deps.nodeBin ?? process.execPath
  const resolved = (deps.resolveBoss ?? resolveBossBinary)(env, { cwd })
  // verify-gate's own `boss` calls (merge, session reads) go by PATH, so the resolved binary's
  // directory leads it: the resolver may have found `boss` where a bare name would not.
  const childEnv =
    resolved.ok && text(resolved.path)
      ? { ...env, PATH: [dirname(resolved.path), env.PATH].filter(Boolean).join(delimiter) }
      : env
  const gate = (args, timeoutMs = timeout()) =>
    run(node, [VERIFY_GATE, ...args], { json: true, timeoutMs, env: childEnv })
  const dry = dryRun ? ['--dry-run'] : []
  // The unchanged-tick state, for an opted-in tick that writes. A dry run never touches it.
  const eventPrUrl = options.eventPrUrl
  const persist = options.skipUnchanged === true && !eventPrUrl && !dryRun
  const fs = deps.fs ?? FS
  // Only shared repositories need a fleet read. A malformed present config must not
  // silently turn that protection off; it leaves spawning closed for this tick.
  let shared = false
  let fleetUnreadable = null
  if (!localOnly) {
    try {
      const readProjectConfig =
        deps.readProjectConfig ??
        (() => {
          const root = repoRootFrom(cwd, { statSync })
          try {
            return JSON.parse(readFileSync(join(root, '.boss-project.json'), 'utf8'))
          } catch (error) {
            if (error.code === 'ENOENT') return null
            throw error
          }
        })
      shared = (readProjectConfig()?.daemons?.length ?? 0) >= 2
    } catch (error) {
      shared = true
      fleetUnreadable = `project config: ${error.message}`
    }
  }
  const scopeKey = sweepScopeKey({ ...options, eventPrUrl: undefined, shared })
  let stateFile
  const statePath = () =>
    stateFile === undefined
      ? (stateFile = deps.stateFile ?? sweepStatePath(run, cwd, timeout()))
      : stateFile
  const forget = () => {
    if (persist || (eventPrUrl && !dryRun)) forgetQuietFingerprint(statePath(), scopeKey, fs)
  }

  const nothing = (extra) => ({
    records: [],
    summary: Object.fromEntries(OUTCOMES.map((o) => [o, 0])),
    undispatchable: null,
    dryRun,
    ...(fleetUnreadable ? { fleetUnreadable } : {}),
    ...extra,
  })
  // Check the stored idle scope before quota, fleet or local-session subprocess reads.
  if (persist && !fleetUnreadable && options.safetyNetMs > 0) {
    const entry = readEntries(fs, statePath())[scopeKey]
    const checkedAt = entry?.checkedAt ?? Date.parse(entry?.at)
    if (
      text(entry?.fingerprint) &&
      Number.isFinite(checkedAt) &&
      now() - checkedAt < options.safetyNetMs
    )
      return nothing({
        idle: `webhooks deliver; next safety-net sweep after ${new Date(checkedAt + options.safetyNetMs).toISOString()}`,
      })
  }

  let dispatchState = null
  const dispatchable = () => {
    if (dispatchState) return dispatchState
    if (!resolved.ok)
      return (dispatchState = { ok: false, unreachable: true, why: resolved.reason })
    const ls = run(resolved.path, ['ls', '--json'], { json: true, timeoutMs: timeout() })
    if (ls.ok && Array.isArray(ls.payload?.sessions))
      return (dispatchState = { ok: true, sessions: ls.payload.sessions })
    if (lsFailureIsUnreachable(ls))
      return (dispatchState = { ok: false, unreachable: true, why: `boss ls: ${ls.error}` })
    return (dispatchState = { ok: false, unreachable: false, why: `boss ls: ${ls.error}` })
  }

  // Local-only mode cannot tell this machine's PRs from another's without the session list, so it
  // is read before anything else and an unreadable one throws: no discovery, no writes, and no
  // `undispatchable` hand-off to a session whose sweep would route every PR.
  if (localOnly) {
    const d = dispatchable()
    if (!d.ok) {
      forget()
      throw new Error(`--local-sessions-only: this machine's session list is unreadable (${d.why})`)
    }
  }
  let remoteUrls = []
  if (shared) {
    const local = dispatchable()
    if (!local.ok) fleetUnreadable ??= local.why
    if (!fleetUnreadable) {
      const fleet = run(resolved.path, ['ls', '--all-daemons', '--json'], {
        json: true,
        timeoutMs: timeout(),
      })
      if (!fleet.ok) fleetUnreadable = `boss ls --all-daemons: ${fleet.error ?? 'unreadable'}`
      else if (!Array.isArray(fleet.payload?.sessions))
        fleetUnreadable = 'boss ls --all-daemons: missing sessions array'
      else if (fleet.payload.failed_organizations?.length)
        fleetUnreadable = 'boss ls --all-daemons: failed organizations'
      else
        remoteUrls = [
          ...new Set(
            fleet.payload.sessions
              .filter(
                (row) =>
                  liveSessionsFor([row], row?.pr_url).length > 0 &&
                  liveSessionsFor(local.sessions, row.pr_url).length === 0,
              )
              .map((row) => row.pr_url),
          ),
        ]
    }
    if (fleetUnreadable) forget()
  }
  const remote = (c) =>
    (localOnly && liveSessionsFor(dispatchState.sessions, c.url).length === 0) ||
    (eventPrUrl && remoteUrls.includes(c.url))
  const idle = (why) => {
    if (!eventPrUrl) forget()
    return nothing({ idle: why })
  }
  // Discovery reads only this machine's PRs; with none, it does not run at all.
  const localUrls = localOnly
    ? [
        ...new Set(
          dispatchState.sessions
            .filter((row) => liveSessionsFor([row], row?.pr_url).length > 0)
            .map((row) => row.pr_url),
        ),
      ]
    : null
  if (eventPrUrl && localUrls && !localUrls.includes(eventPrUrl))
    return idle('event PR has no session on this machine')
  if (localUrls?.length === 0) return idle('no session on this machine has a PR')
  const prUrls = eventPrUrl ? [eventPrUrl] : localUrls
  const low = quotaLow(run, timeout())
  if (low) return idle(low)

  const quietBefore =
    persist && !fleetUnreadable ? readQuietFingerprint(statePath(), scopeKey, fs) : null
  const found = gate(
    [
      'candidates',
      '--limit',
      String(discoveryLimit(batch)),
      ...(prUrls ? ['--pr-urls', prUrls.join(',')] : []),
      ...(!prUrls && remoteUrls.length ? ['--exclude-pr-urls', remoteUrls.join(',')] : []),
      ...(quietBefore ? ['--quiet-fingerprint', quietBefore] : []),
      ...(options.selectionArgs ?? []),
    ],
    Math.max(1000, Math.min(2 * CALL_TIMEOUT_MS, remaining() - START_FLOOR_MS)),
  )
  if (!found.ok || !Array.isArray(found.payload?.candidates)) {
    forget()
    throw new Error(
      `candidates failed (${found.error ?? 'unreadable'})${found.output ? `: ${found.output.split('\n').pop()}` : ''}`,
    )
  }
  if (
    !dryRun &&
    found.payload.unchanged !== true &&
    Array.isArray(found.payload.admission) &&
    statePath()
  )
    storeAdmission(
      join(dirname(statePath()), 'verify-admission.json'),
      scopeKey,
      found.payload.admission,
      new Date(now()).toISOString(),
      { eventPr: eventPrUrl ? Number(eventPrUrl.split('/').pop()) : null, fs },
    )
  // Include-filtered discovery also reports other PRs as remote-session skips. An event owns
  // only its named PR, including records and quiet-state decisions, never the rest of that scan.
  if (eventPrUrl) {
    const eventPr = Number(eventPrUrl.split('/').pop())
    found.payload = {
      ...found.payload,
      candidates: found.payload.candidates.filter((c) => c.url === eventPrUrl),
      mergeRetries: (found.payload.mergeRetries ?? []).filter((c) => c.url === eventPrUrl),
      skipped: (found.payload.skipped ?? []).filter((s) => s.pr === eventPr),
    }
  }
  // Nothing changed since the last quiet tick: its entry stands, and nothing is judged.
  if (persist && found.payload.unchanged === true) {
    const entries = readEntries(fs, statePath())
    if (entries[scopeKey])
      writeEntries(fs, statePath(), {
        ...entries,
        [scopeKey]: { ...entries[scopeKey], checkedAt: now() },
      })
    return nothing({ unchanged: true })
  }

  const records = []
  const record = (pr, outcome, line, extra = {}) =>
    records.push({ pr, outcome, line: `#${pr} ${line}`, ...extra })
  // Handed-off merges, each holding its PR's record slot so lines stay in candidate order.
  const pending = []
  let acted = 0
  let undispatchable = null
  const needBoss = (pr, extra = {}) => {
    const d = dispatchable()
    if (d.ok) return true
    if (d.unreachable) {
      undispatchable = d.why
      record(pr, 'deferred', `deferred: undispatchable (${d.why})`, {
        ...extra,
        undispatchable: true,
      })
    } else {
      record(pr, 'skipped', `skipped: unknown:sessions (${d.why})`, extra)
    }
    return false
  }

  const skippedRemote = Array.isArray(found.payload.skipped) ? found.payload.skipped : []
  for (const s of skippedRemote)
    if (s?.reason === REMOTE_SESSION_REASON && Number.isInteger(s.pr))
      record(s.pr, 'skipped', REMOTE_SESSION_SKIP, { remote: true })

  let retried = 0
  const mergeRetries = Array.isArray(found.payload.mergeRetries) ? found.payload.mergeRetries : []
  for (const c of mergeRetries) {
    const pr = c.pr
    const extra = { retry: true }
    if (remote(c)) {
      record(pr, 'skipped', REMOTE_SESSION_SKIP, { ...extra, remote: true })
      continue
    }
    if (undispatchable) {
      record(pr, 'deferred', 'deferred: undispatchable', extra)
      continue
    }
    if (retried >= batch || remaining() < START_FLOOR_MS) {
      record(pr, 'deferred', 'deferred: budget', extra)
      continue
    }
    if (!needBoss(pr, extra)) continue
    retried++
    if (dryRun) {
      record(pr, 'merged', `merged (dry-run: would retry merge ${c.headSha})`, extra)
      continue
    }
    const repoArgs = text(c.repo) ? ['--repo', c.repo] : []
    const handed = spawnDetached(
      node,
      [VERIFY_GATE, 'merge', '--pr', String(pr), '--head', c.headSha, ...repoArgs],
      { env: childEnv, cwd, logDir, now, label: `merge-${pr}` },
      { spawn: deps.spawn ?? nodeSpawn },
    )
    if (!handed.ok) {
      record(pr, 'skipped', `merge: spawn failed (${handed.error ?? 'unknown'})`, extra)
      continue
    }
    pending.push({ pr, repoArgs, log: handed.log, slot: records.length, retry: true })
    records.push(null)
  }

  for (const c of found.payload.candidates) {
    const pr = c.pr
    if (remote(c)) {
      record(pr, 'skipped', REMOTE_SESSION_SKIP, { remote: true })
      continue
    }
    if (undispatchable) {
      record(pr, 'deferred', 'deferred: undispatchable')
      continue
    }
    if (acted >= batch || remaining() < START_FLOOR_MS) {
      record(pr, 'deferred', 'deferred: budget')
      continue
    }
    const repoArgs = text(c.repo) ? ['--repo', c.repo] : []
    const judged = gate([
      'judge',
      '--pr',
      String(pr),
      ...repoArgs,
      ...(text(c.waive) ? ['--waive', c.waive] : []),
    ])
    if (!judged.ok) {
      record(pr, 'skipped', `skipped: judge failed (${judged.error})`)
      continue
    }
    const j = judged.payload
    const head = text(j.headSha) ? j.headSha : c.headSha
    const suffix = dryRun ? ' (dry-run)' : ''

    if (j.verdict === 'wait') {
      record(pr, 'skipped', `skipped: wait ${j.reason}`)
      continue
    }
    if (j.verdict === 'human') {
      acted++
      const posted = gate([
        'post',
        '--pr',
        String(pr),
        '--head',
        head,
        '--verdict',
        'human',
        '--reason',
        j.reason,
        ...(text(j.policyFingerprint) ? ['--policy', j.policyFingerprint] : []),
        ...repoArgs,
        ...dry,
      ])
      if (posted.payload?.trackerWrites === 'unavailable')
        record(pr, 'skipped', 'skipped: tracker writes unavailable')
      else if (posted.ok && (posted.payload?.posted || posted.payload?.skipped))
        record(
          pr,
          'parked',
          `parked: needs human (${j.reason})${posted.payload?.why === 'no-ticket' ? ' · no ticket' : ''}${suffix}`,
        )
      else record(pr, 'skipped', `skipped: post failed (${postWhy(posted)})`)
      continue
    }
    // Under an admission tier, a pass with no live session anywhere is merged by the session verify
    // creates, since a detached merge could only park no-session. A fork never reaches a session.
    const tier = j.facts?.tier
    const sessionless =
      j.verdict === 'pass' &&
      tier?.class !== undefined &&
      ![tier.class, c.class].includes('fork') &&
      !fleetUnreadable &&
      dispatchable().ok &&
      liveSessionsFor(dispatchState.sessions, c.url).length === 0
    if (j.verdict === 'pass' && !sessionless) {
      if (!needBoss(pr)) continue
      acted++
      const posted = gate([
        'post',
        '--pr',
        String(pr),
        '--head',
        head,
        '--verdict',
        'pass',
        ...(text(j.reason) ? ['--reason', j.reason] : []),
        ...repoArgs,
        ...dry,
      ])
      if (posted.payload?.abandoned === 'claim-lost') {
        record(pr, 'skipped', 'skipped: claim lost')
        continue
      }
      if (!posted.ok || !(posted.payload?.posted || posted.payload?.skipped)) {
        record(pr, 'skipped', `skipped: post failed (${postWhy(posted)})`)
        continue
      }
      if (dryRun) {
        // A dry-run post wrote no `verified` status, so a dry-run merge could only say
        // not-verified; the plan is the merge itself.
        record(pr, 'merged', `merged (dry-run: would merge ${head})`)
        continue
      }
      const handed = spawnDetached(
        node,
        [VERIFY_GATE, 'merge', '--pr', String(pr), '--head', head, ...repoArgs],
        { env: childEnv, cwd, logDir, now, label: `merge-${pr}` },
        { spawn: deps.spawn ?? nodeSpawn },
      )
      if (!handed.ok) {
        record(pr, 'skipped', `merge: spawn failed (${handed.error ?? 'unknown'})`)
        continue
      }
      pending.push({ pr, repoArgs, log: handed.log, slot: records.length })
      records.push(null)
      continue
    }
    if (['extensions-required', 'review-required'].includes(j.verdict) || sessionless) {
      if (fleetUnreadable && liveSessionsFor(dispatchState?.sessions, c.url).length === 0) {
        record(pr, 'skipped', `skipped: unknown:fleet-sessions (${fleetUnreadable})`)
        continue
      }
      if (!needBoss(pr)) continue
      acted++
      const claimed = gate([
        'post',
        '--pr',
        String(pr),
        '--head',
        head,
        '--verdict',
        'claim',
        ...repoArgs,
        ...dry,
      ])
      if (!claimed.ok || claimed.payload?.won !== true || !text(claimed.payload?.token)) {
        record(pr, 'skipped', 'skipped: claim lost')
        continue
      }
      const routed = routeCandidate(
        { pr, url: c.url, ticket: c.ticket, token: claimed.payload.token, waive: c.waive },
        {
          run,
          boss: resolved.path,
          sessions: dispatchState.sessions,
          dryRun,
          timeout,
          repoDir: text(env.REPO_DIR) ? env.REPO_DIR : cwd,
          trackerSource: routedTrackerSource(),
          spawnDetached: (cmd, args) =>
            spawnDetached(
              cmd,
              args,
              { env, cwd, logDir: deps.logDir ?? tmpdir(), now },
              { spawn: deps.spawn ?? nodeSpawn },
            ),
        },
      )
      const detail = [routed.note, routed.log ? `log ${routed.log}` : ''].filter(Boolean).join('; ')
      if (routed.ok)
        record(
          pr,
          'dispatched',
          `dispatched: ${routed.route}${detail ? ` (${detail})` : ''}${suffix}`,
          {
            route: routed.route,
            ...(dryRun && routed.argv ? { argv: routed.argv } : {}),
          },
        )
      else
        record(pr, 'skipped', `dispatch-unknown: ${routed.route} (${routed.why ?? 'failed'})`, {
          route: routed.route,
        })
      continue
    }
    record(pr, 'skipped', `skipped: unexpected verdict ${j.verdict}`)
  }

  settleMerges(pending, records, { readFile, sleep, remaining, gate })

  if (persist || (eventPrUrl && !dryRun)) {
    if (!fleetUnreadable && tickWasQuiet(found.payload, records, undispatchable)) {
      if (persist)
        storeQuietFingerprint(
          statePath(),
          scopeKey,
          found.payload.fingerprint,
          new Date(now()).toISOString(),
          fs,
        )
    } else forget()
  }
  const summary = Object.fromEntries(OUTCOMES.map((o) => [o, 0]))
  for (const r of records) summary[r.outcome]++
  return {
    records,
    summary,
    undispatchable,
    dryRun,
    ...(fleetUnreadable ? { fleetUnreadable } : {}),
  }
}

/**
 * Fill each handed-off merge's record slot: poll the logs while more than OBSERVE_TIMEOUT_MS per
 * open merge is left, then give each merge still open one read-only `observe-merge` that ends
 * inside the budget. A merge with no result by then is still running detached: `dispatched`.
 */
function settleMerges(pending, records, { readFile, sleep, remaining, gate }) {
  const fill = (p, outcome, line, quiet = false) =>
    (records[p.slot] = {
      pr: p.pr,
      outcome,
      line: `#${p.pr} ${line}${p.retry ? ' (merge retry)' : ''}`,
      log: p.log,
      ...(p.retry ? { retry: true } : {}),
      // A merge retry that waited on a steady state and wrote nothing leaves the tick quiet.
      ...(quiet ? { quiet: true } : {}),
    })
  const result = (p) => {
    try {
      return readMergeResult(readFile(p.log))
    } catch {
      return null
    }
  }
  const observe = (p) => {
    const left = remaining()
    const seen =
      left >= MIN_OBSERVE_MS
        ? gate(
            ['observe-merge', '--pr', String(p.pr), ...p.repoArgs],
            Math.min(OBSERVE_TIMEOUT_MS, left),
          )
        : null
    return seen?.ok && seen.payload?.merged === true && text(seen.payload.mergeSha)
      ? seen.payload.mergeSha
      : null
  }
  const settled = (p) => {
    const m = result(p)
    if (!m) return false
    // A merge that crashed may have merged first (a failed tracker write throws after the merge):
    // GitHub decides, not the crash.
    const crashedButMerged = m.reason === 'merge-error' ? observe(p) : null
    if (crashedButMerged) fill(p, 'merged', `merged ${crashedButMerged}`)
    else if (m.merged === true) fill(p, 'merged', `merged ${m.mergeSha ?? ''}`.trim())
    else if (m.abandoned === 'claim-lost') fill(p, 'skipped', 'skipped: claim lost')
    else {
      const why = m.reason ?? (Array.isArray(m.reasons) ? m.reasons.join(',') : undefined)
      const notice = text(m.notice) ? ` (PR notice ${m.notice})` : ''
      const quiet =
        p.retry === true &&
        m.verdict === 'wait' &&
        Array.isArray(m.writes) &&
        m.writes.length === 0 &&
        QUIET_MERGE_WAITS.has(m.reason)
      fill(p, 'skipped', `${m.verdict ?? 'merge'}: ${why ?? 'unknown'}${notice}`, quiet)
    }
    return true
  }
  let open = pending.filter((p) => !settled(p))
  while (open.length > 0) {
    const slack = remaining() - OBSERVE_TIMEOUT_MS * open.length
    if (slack <= 0) break
    sleep(Math.min(MERGE_POLL_MS, slack))
    open = open.filter((p) => !settled(p))
  }
  for (const p of open) {
    if (settled(p)) continue
    const mergeSha = observe(p)
    if (mergeSha) fill(p, 'merged', `merged ${mergeSha}`)
    else fill(p, 'dispatched', `dispatched: merge in flight (log ${p.log})`)
  }
}

function postWhy(result) {
  return result?.payload?.reason ?? result?.payload?.trackerWrites ?? result?.error ?? 'unknown'
}

// `boss new --tracker-source` accepts only the trackers it knows; anything else binds nothing.
function trackerSourceFor(tracker) {
  return ['linear', 'sentry'].includes(tracker) ? tracker : ''
}

export function summaryLine(prefix, summary) {
  return `${prefix}: ${OUTCOMES.map((o) => `${o}=${summary[o] ?? 0}`).join(' ')}`
}

// ---------------------------------------------------------------------------
// CLI.

export function main(argv, deps = {}) {
  const [cmd, ...rest] = argv
  if (cmd !== 'sweep')
    throw new UsageError(`verify-route: unknown command ${cmd ?? '(none)'} (expected sweep)`)
  return sweep(parseSweepArgs(rest), deps)
}

if (isMainModule(import.meta.url)) {
  try {
    const result = main(process.argv.slice(2))
    for (const r of result.records) process.stdout.write(`${JSON.stringify(r)}\n`)
    if (result.fleetUnreadable)
      process.stdout.write(`${JSON.stringify({ fleetUnreadable: result.fleetUnreadable })}\n`)
    if (result.undispatchable)
      process.stdout.write(`${JSON.stringify({ undispatchable: result.undispatchable })}\n`)
    process.stdout.write(
      `${JSON.stringify({ summary: result.summary, dryRun: result.dryRun, ...(result.unchanged ? { unchanged: true } : {}) })}\n`,
    )
  } catch (error) {
    process.stderr.write(
      `verify-route: ${error instanceof Error ? error.message : String(error)}\n`,
    )
    process.exitCode = error instanceof UsageError ? 2 : 1
  }
}
