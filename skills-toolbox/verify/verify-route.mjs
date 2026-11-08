#!/usr/bin/env node

// verify-route.mjs — the verify stage's dispatcher: discover the PRs that need judging, act on a
// bounded batch of them, and get agent work into each PR's own boss session. Both the cron gate
// (cron-gates/boss-verify.mjs) and `/boss-verify` with no argument run this same `sweep`.
//
// prints one JSON line per PR, then `{"summary": {...}}`. Per candidate it runs verify-gate's
// zero-token `judge`, then:
//
//   judge failed         claim and route into a live session here, which judges unbudgeted;
//                        otherwise `deferred`, writing nothing, so a later run re-judges
//   human / blocked      post --verdict human / blocked --blockers <file>
//   pass                 post --verdict pass, then hand the merge to a detached
//                        `verify-gate merge` (needs `boss`); under an admission tier, a non-fork PR
//                        with no live session is claimed and routed like extensions-required
//   extensions-required
//   judge-required       post --verdict claim; on a won claim, route `/boss-verify <pr> --claim
//   review-required      <token>` into the PR's session (needs `boss`)
//
// Merges run detached: their lock and daemon refresh can outlast the sweep budget. Poll each
// merge log, then make one bounded observe-merge read per unfinished merge before returning.
// An expired claim is taken over before any verdict posts (verify-stale-claim.mjs).
// Every subprocess uses the injected run seam; failed reads are unknown. One deadline bounds
// discovery, judging and dispatch. Session-local sweeps skip PRs belonging to other machines.

import { closeSync, openSync, readFileSync, statSync } from 'node:fs'
import { spawn as nodeSpawn, spawnSync } from 'node:child_process'
import { tmpdir } from 'node:os'
import { delimiter, dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { loadSkillConfig } from '../skill-config.mjs'
import { verdictFileArgs, postArgv, failedPostRecord } from './verify-post-files.mjs'
import {
  FS,
  forgetQuietFingerprint,
  readQuietEntry,
  readQuietFingerprint,
  safetyNetIdleUntil,
  storeQuietFingerprint,
  sweepScopeKey,
  sweepStatePath,
  touchQuietEntry,
} from './verify-sweep-state.mjs'
import {
  claimForVerdict,
  remoteOrphan,
  takeOverExpiredClaim,
  waitOverExpiredClaim,
  wakeAtOf,
} from './verify-stale-claim.mjs'
import { readFailureDetail } from './verify-gate.mjs'
import { candidateFailureMessage } from './verify-open-prs.mjs'
import { configuredTrackerName } from '../tracker/adapter.mjs'
import { repoRootFrom, resolveBossBinary } from '../boss-binary.mjs'
import {
  adoptsVerifyOnly,
  forgetRemoteAdmission,
  skipReasonText,
  storeScanAdmission,
} from './verify-admission.mjs'
import { isMainModule } from '../main-module.mjs'
import {
  VERIFY_CHAT_TITLE,
  chatIsBusy,
  nextVerifyTitle,
  pickVerifyChat,
  supersedeRoutedCallbacks,
  verifyChatArgv,
} from '../stage-chain.mjs'
import { SELECTION_FLAGS, parseSelectionFlags } from '../selection.mjs'
import { REMOTE_SESSION_REASON } from './verify-vocabulary.mjs'
import { liveSessionsFor, rankRefreshNeedsRetry, refreshRanks } from './verify-rank.mjs'
import { eventWasSettled, tickWasQuiet } from './verify-settlement.mjs'

export { tickWasQuiet } from './verify-settlement.mjs'

export const DEFAULT_BUDGET_MS = 45_000
export const DEFAULT_BATCH = 3
export const CALL_TIMEOUT_MS = 15_000
// A new PR is not started with less than this left on the deadline.
export const START_FLOOR_MS = 15_000
// An event judges one PR, so its judge may run up to this long, but never into the last
// JUDGE_DISPATCH_RESERVE_MS: a judge that fails still leaves time to hand the head to its verify chat.
export const EVENT_JUDGE_TIMEOUT_MS = 2 * CALL_TIMEOUT_MS
export const JUDGE_DISPATCH_RESERVE_MS = 10_000
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
const failureStderr = (stderr) =>
  stderr
    ? readFailureDetail('stderr', {
        stderr: String(stderr).split('\n').filter(text).pop(),
      }).replace(/^stderr: /, '')
    : ''

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
      for (const candidate of [output, output.split('\n').filter(Boolean).pop() ?? '']) {
        try {
          payload = JSON.parse(candidate)
          break
        } catch {}
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
        stderr: failureStderr(response?.stderr),
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

export function newSessionArgv({ pr, repoDir, ticket, token, waive, trackerSource, verifyOnly }) {
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
    ...(verifyOnly ? ['--verify-only'] : []),
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

export { liveSessionsFor } from './verify-rank.mjs'

/**
 * Get `/boss-verify <pr> --claim <token>` into the PR's session. Returns `{route, ok, note?, log?,
 * why?}`; `ok: false` is `dispatch-unknown` (the claim goes stale and the next run re-routes).
 */
export function routeCandidate(
  { pr, headSha, url, ticket, token, waive, class: cls, repairOptIn },
  ctx,
) {
  const { run, boss, sessions, dryRun, timeout } = ctx
  const message = verifyMessage(pr, token, waive)
  const rows = liveSessionsFor(sessions, url)
  if (rows.length > 0) {
    const session = rows[0]
    let note = rows.length > 1 ? `newest of ${rows.length} live sessions` : undefined
    const chats = run(boss, ['chats', '--json', session.id], { json: true, timeoutMs: timeout() })
    if (!chats.ok || !Array.isArray(chats.payload?.chats))
      return { route: 'chats-unreadable', ok: false, why: chats.error ?? 'unreadable', note }
    const verify = pickVerifyChat(chats.payload.chats)
    const finish = (route, sent, routedChats) => {
      const cleanup = sent.ok
        ? supersedeRoutedCallbacks({ run, boss, timeout, pr, head: headSha, chats: routedChats })
        : ''
      return {
        route,
        ok: sent.ok,
        why: sent.why,
        note: [note, cleanup].filter(Boolean).join('; ') || undefined,
      }
    }
    if (chatIsBusy(verify)) note = [note, 'verify chat busy'].filter(Boolean).join('; ')
    if (verify && !chatIsBusy(verify)) {
      if (dryRun) return { route: 'existing-verify-chat', ok: true, note }
      const sent = sendVerify(run, boss, verify.agent_session_id, message, timeout())
      return finish('existing-verify-chat', sent, chats.payload.chats)
    }
    if (dryRun) return { route: 'new-verify-chat', ok: true, note }
    const created = run(boss, verifyChatArgv(session.id, nextVerifyTitle(chats.payload.chats)), {
      json: true,
      timeoutMs: timeout(),
    })
    const chatId = created.payload?.chat?.agent_session_id
    if (!created.ok || !text(chatId))
      return { route: 'new-verify-chat', ok: false, why: created.error ?? 'no-chat-id', note }
    const sent = sendVerify(run, boss, chatId, message, timeout())
    return finish('new-verify-chat', sent, [
      ...chats.payload.chats,
      { ...created.payload.chat, role: 'verify' },
    ])
  }
  const argv = newSessionArgv({
    pr,
    repoDir: ctx.repoDir,
    ticket,
    token,
    waive,
    trackerSource: ctx.trackerSource,
    verifyOnly: adoptsVerifyOnly(cls, repairOptIn),
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
// every other tool on the machine spends it too. bossd's scheduler names the reserve floor it
// already admitted a verify cron above in BOSS_GITHUB_QUOTA_FLOOR, which wins when in (0,1).
export const MIN_QUOTA_SHARE = 0.25

/**
 * Why this run should leave GitHub alone, or null. `gh api rate_limit` does not count against
 * either quota; an unreadable answer is null, so a run never stalls on the courtesy check itself.
 */
export function quotaLow(run, timeoutMs, env) {
  const share = Number(env?.BOSS_GITHUB_QUOTA_FLOOR)
  const floor = share > 0 && share < 1 ? share : MIN_QUOTA_SHARE
  const r = run('gh', ['api', 'rate_limit'], { json: true, timeoutMs })
  if (!r.ok) return null
  for (const name of ['core', 'graphql']) {
    const q = r.payload?.resources?.[name]
    const { limit, remaining, reset } = q ?? {}
    if (![limit, remaining].every(Number.isFinite) || limit <= 0) continue
    if (remaining / limit >= floor) continue
    const at = Number.isFinite(reset) ? `, resets ${new Date(reset * 1000).toISOString()}` : ''
    return `GitHub ${name} quota low (${remaining}/${limit} left${at})`
  }
  return null
}

// The unchanged-tick state lives in verify-sweep-state.mjs; its helpers are re-exported here.
export {
  SWEEP_STATE_VERSION,
  forgetQuietFingerprint,
  readQuietFingerprint,
  storeQuietFingerprint,
  sweepScopeKey,
  sweepStatePath,
} from './verify-sweep-state.mjs'

// The merge waits that may leave a tick quiet: each is a steady state whose way out moves the
// fingerprint (merge state, `boss/verify` status, `updatedAt` on a new operator comment). Any other
// wait, including a reasons-only one, may rest on CI evidence the fingerprint does not hash (an
// unreadable prior-head or workflow-run read, a run the rollup does not show yet): never quiet.
const QUIET_MERGE_WAITS = new Set(['merge-state-dirty', 'not-verified', 'sent-back'])

/**
 * Discover, then act on a bounded batch. Returns `{records, summary, undispatchable, idle?}`, where
 * each record is `{pr, outcome, line, ...}` with `outcome` one of merged / parked / dispatched /
 * skipped / deferred, and `idle` says why a run read nothing (low GitHub quota, or no local PR
 * session under `--local-sessions-only`). With `skipUnchanged`, `unchanged: true` says the tick
 * matched the last quiet one and did nothing. Throws when discovery itself fails (a tracker, config
 * or open-PR read failure): the caller must fail closed rather than read that as "nothing to do".
 * In event mode, `eventSettled` says whether the event head was handled or deliberately excluded;
 * failures, unknown discovery, waits and deferrals leave it eligible for another event.
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
  const eventPr = eventPrUrl ? Number(eventPrUrl.split('/').pop()) : null
  const stretch = () => Math.min(EVENT_JUDGE_TIMEOUT_MS, remaining() - JUDGE_DISPATCH_RESERVE_MS)
  const judgeTimeout = () => Math.max(timeout(), eventPrUrl ? stretch() : 0)
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
  const admissionFile = () => statePath() && join(dirname(statePath()), 'verify-admission.json')
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
    ...(eventPrUrl
      ? { eventSettled: extra?.idle === 'event PR has no session on this machine' }
      : {}),
  })
  // Check the stored idle scope before quota, fleet or local-session subprocess reads. A claim the
  // quiet tick saw expiring (`wakeAt`) ends the idle early: no webhook announces an expiry.
  if (persist && !fleetUnreadable && options.safetyNetMs > 0) {
    const until = safetyNetIdleUntil(readQuietEntry(statePath(), scopeKey, fs), options.safetyNetMs)
    if (until !== null && now() < until)
      return nothing({
        idle: `webhooks deliver; next safety-net sweep after ${new Date(until).toISOString()}`,
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
  // No scan refreshes the remote list, so the stale one is withdrawn rather than left reporting.
  if (localUrls?.length === 0 && !dryRun) forgetRemoteAdmission(admissionFile(), scopeKey, { fs })
  if (localUrls?.length === 0) return idle('no session on this machine has a PR')
  const prUrls = eventPrUrl ? [eventPrUrl] : localUrls
  const low = quotaLow(run, timeout(), env)
  if (low) return idle(low)
  const preparing = !dryRun && deps.prepare?.()
  if (preparing) return idle(preparing)
  const quietBefore =
    persist && !fleetUnreadable ? readQuietFingerprint(statePath(), scopeKey, fs, now()) : null
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
    throw new Error(candidateFailureMessage(found, failureStderr(found.stderr)))
  }
  if (!dryRun && Array.isArray(found.payload.admission) && admissionFile())
    storeScanAdmission(admissionFile(), scopeKey, found.payload, new Date(now()).toISOString(), {
      eventPr,
      trusted: !fleetUnreadable,
      fs,
    })
  // Include-filtered discovery also reports other PRs as remote-session skips. An event owns
  // only its named PR, including records and quiet-state decisions, never the rest of that scan.
  if (eventPrUrl) {
    found.payload = {
      ...found.payload,
      candidates: found.payload.candidates.filter((c) => c.url === eventPrUrl),
      mergeRetries: (found.payload.mergeRetries ?? []).filter((c) => c.url === eventPrUrl),
      skipped: (found.payload.skipped ?? []).filter((s) => s.pr === eventPr),
    }
  }
  const rankOptions = {
    eventPrUrl,
    dispatchable,
    run,
    boss: resolved.path,
    timeout,
    remaining,
    startFloor: START_FLOOR_MS,
    dryRun,
  }
  if (persist && found.payload.unchanged === true) {
    const ranks = refreshRanks(found.payload.ranks, rankOptions)
    if (rankRefreshNeedsRetry(ranks)) forget()
    else touchQuietEntry(statePath(), scopeKey, now(), wakeAtOf(found.payload), fs)
    return nothing({ unchanged: true, ...ranks })
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

  // Claim the head, then route `/boss-verify <pr> --claim <token>` into the PR's session.
  const spawn = deps.spawn ?? nodeSpawn
  const dispatch = (c, head, why) => {
    acted++
    const repoArgs = text(c.repo) ? ['--repo', c.repo] : []
    const expired = c.claim === 'stale'
    const claimed = takeOverExpiredClaim(gate, { pr: c.pr, head, repoArgs, dry, expired })
    if (!claimed.won) return record(c.pr, 'skipped', 'skipped: claim lost')
    const routed = routeCandidate(
      { ...c, token: claimed.token, headSha: head },
      {
        run,
        boss: resolved.path,
        sessions: dispatchState.sessions,
        dryRun,
        timeout,
        repoDir: text(env.REPO_DIR) ? env.REPO_DIR : cwd,
        trackerSource: routedTrackerSource(),
        spawnDetached: (cmd, args) =>
          spawnDetached(cmd, args, { env, cwd, logDir, now }, { spawn }),
      },
    )
    const { route, note, log, argv } = routed
    const detail = [why, note, log ? `log ${log}` : ''].filter(Boolean).join('; ')
    if (!routed.ok)
      return record(c.pr, 'skipped', `dispatch-unknown: ${route} (${routed.why ?? 'failed'})`, {
        route,
      })
    const line = `dispatched: ${route}${detail ? ` (${detail})` : ''}${dryRun ? ' (dry-run)' : ''}`
    record(c.pr, 'dispatched', line, { route, ...(dryRun && argv ? { argv } : {}) })
  }

  const skippedRemote = Array.isArray(found.payload.skipped) ? found.payload.skipped : []
  let discoverySkipped = null
  // An orphaned claim is replaced only where the fleet read named the PR's owner.
  for (const s of skippedRemote)
    if (s?.reason === REMOTE_SESSION_REASON && Number.isInteger(s.pr)) {
      const room = acted < batch && remaining() >= START_FLOOR_MS
      const writable = shared && !fleetUnreadable && room
      const o = remoteOrphan(s, { now: now(), gate, writable, dry })
      if (!o.quiet) acted++
      const line = `${REMOTE_SESSION_SKIP}${o.detail ? ` (${o.detail})` : ''}`
      record(s.pr, 'skipped', `${line}${!o.quiet && dryRun ? ' (dry-run)' : ''}`, {
        // An orphan held back by the batch or the start floor keeps the tick from being quiet.
        ...(o.quiet && !(o.due && !room) ? { remote: true } : { orphan: true }),
        ...(dryRun && o.argv ? { argv: o.argv } : {}),
      })
    }
  if (!eventPrUrl) {
    const counts = {}
    for (const s of skippedRemote)
      if (text(s?.reason) && s.reason !== REMOTE_SESSION_REASON)
        counts[s.reason] = (counts[s.reason] ?? 0) + 1
    discoverySkipped = counts
  }

  if (eventPrUrl) {
    for (const skip of skippedRemote)
      if (skip.reason !== REMOTE_SESSION_REASON)
        record(skip.pr, 'skipped', `skipped: ${skipReasonText(skip)}`, {
          discovery: true,
          quiet: !skip.reason.startsWith('unknown:'),
        })
    const present = [
      ...found.payload.candidates,
      ...(found.payload.mergeRetries ?? []),
      ...skippedRemote,
    ].some((entry) => entry.pr === eventPr)
    if (!present) {
      const answer = found.payload.admission?.find((entry) => entry.pr === eventPr)
      const reason = answer?.reason ?? 'unknown:admission'
      const why = skipReasonText({ ...answer, reason })
      record(eventPr, 'skipped', `skipped: not admitted (${why})`, {
        discovery: true,
        quiet: !reason.startsWith('unknown:'),
      })
    }
  }

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
    const judged = gate(
      ['judge', '--pr', String(pr), ...repoArgs, ...(text(c.waive) ? ['--waive', c.waive] : [])],
      judgeTimeout(),
    )
    if (!judged.ok) {
      // A failed judge says nothing about the head: its live session judges it, else it stays a
      // candidate. Nothing is claimed for a head no session will take.
      const d = dispatchable()
      const why = d.ok ? 'no live session' : d.why
      if (d.ok && liveSessionsFor(d.sessions, c.url).length > 0)
        dispatch(c, c.headSha, `judge failed: ${judged.error}`)
      else if (d.unreachable) needBoss(pr)
      else record(pr, 'deferred', `deferred: judge failed (${judged.error}), ${why}`)
      continue
    }
    const j = judged.payload
    const head = text(j.headSha) ? j.headSha : c.headSha
    const suffix = dryRun ? ' (dry-run)' : ''
    // A wait is only recorded, except over an expired claim, which it replaces under a new token.
    if (j.verdict === 'wait') {
      if (c.claim !== 'stale') {
        record(pr, 'skipped', `skipped: wait ${j.reason}`)
        continue
      }
      acted++
      const w = waitOverExpiredClaim(gate, { pr, head, repoArgs, dry, reason: j.reason })
      const how = !w.won
        ? null
        : w.posted
          ? `replaced expired claim${w.replaced ? ` ${w.replaced}` : ''}`
          : 'took the expired claim, wait post failed'
      record(
        pr,
        'skipped',
        how ? `skipped: wait ${j.reason} (${how})${suffix}` : 'skipped: claim lost',
      )
      continue
    }
    if (j.verdict === 'human' || j.verdict === 'blocked') {
      acted++
      const fileArgs = verdictFileArgs({ fs: FS, logDir, pr, head, judged: j })
      if (!fileArgs) {
        record(pr, 'skipped', 'skipped: invalid or unwritable blockers')
        continue
      }
      const took = claimForVerdict(c, gate, { pr, head, repoArgs, dry })
      if (!took) {
        record(pr, 'skipped', 'skipped: claim lost')
        continue
      }
      const posted = gate(
        postArgv({ pr, head, judged: j, fileArgs, repoArgs, dry, token: took.token }),
      )
      const postState = {
        fs,
        stateFile: dryRun ? null : statePath(),
        logDir,
        pr,
        head,
        judged: j,
        repo: c.repo,
        repoArgs,
        dry,
        gate,
        now,
      }
      if (posted.payload?.abandoned === 'claim-lost') record(pr, 'skipped', 'skipped: claim lost')
      else if (posted.payload?.trackerWrites === 'unavailable')
        record(pr, 'skipped', 'skipped: tracker writes unavailable')
      else if (posted.ok && (posted.payload?.posted || posted.payload?.skipped)) {
        failedPostRecord(postState, null)
        record(
          pr,
          'parked',
          `parked: ${j.verdict === 'blocked' ? 'blocked' : 'needs human'} (${j.reason})${posted.payload?.why === 'no-ticket' ? ' · no ticket' : ''}${suffix}`,
        )
      } else {
        const failure = failedPostRecord(postState, posted)
        record(pr, failure.outcome, failure.line, { stderr: failure.stderr })
      }
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
      const took = claimForVerdict(c, gate, { pr, head, repoArgs, dry })
      if (!took) {
        record(pr, 'skipped', 'skipped: claim lost')
        continue
      }
      const posted = gate(
        postArgv({
          pr,
          head,
          judged: j,
          fileArgs: verdictFileArgs({ fs: FS, logDir, pr, head, judged: j }),
          repoArgs,
          dry,
          token: took.token,
        }),
      )
      const postState = {
        fs,
        stateFile: dryRun ? null : statePath(),
        logDir,
        pr,
        head,
        judged: j,
        repo: c.repo,
        repoArgs,
        dry,
        gate,
        now,
      }
      if (posted.payload?.abandoned === 'claim-lost') {
        record(pr, 'skipped', 'skipped: claim lost')
        continue
      }
      if (!posted.ok || !(posted.payload?.posted || posted.payload?.skipped)) {
        const failure = failedPostRecord(postState, posted)
        record(pr, failure.outcome, failure.line, { stderr: failure.stderr })
        continue
      }
      failedPostRecord(postState, null)
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
    if (
      ['extensions-required', 'review-required', 'judge-required'].includes(j.verdict) ||
      sessionless
    ) {
      if (fleetUnreadable && liveSessionsFor(dispatchState?.sessions, c.url).length === 0) {
        record(pr, 'skipped', `skipped: unknown:fleet-sessions (${fleetUnreadable})`)
        continue
      }
      if (!needBoss(pr)) continue
      dispatch(c, head)
      continue
    }
    record(pr, 'skipped', `skipped: unexpected verdict ${j.verdict}`)
  }

  settleMerges(pending, records, { readFile, sleep, remaining, gate })
  const ranks = refreshRanks(found.payload.ranks, rankOptions)
  const rankRetry = rankRefreshNeedsRetry(ranks)
  if (persist || (eventPrUrl && !dryRun)) {
    if (!fleetUnreadable && !rankRetry && tickWasQuiet(found.payload, records, undispatchable)) {
      if (persist)
        storeQuietFingerprint(
          statePath(),
          scopeKey,
          found.payload.fingerprint,
          new Date(now()).toISOString(),
          fs,
          wakeAtOf(found.payload),
        )
    } else forget()
  }
  const summary = Object.fromEntries(OUTCOMES.map((o) => [o, 0]))
  for (const r of records) summary[r.outcome]++
  return {
    records,
    ...ranks,
    summary,
    undispatchable,
    dryRun,
    ...(discoverySkipped ? { discoverySkipped } : {}),
    ...(Number.isFinite(found.payload.claimExpiresAt)
      ? { claimExpiresAt: found.payload.claimExpiresAt }
      : {}),
    ...(fleetUnreadable ? { fleetUnreadable } : {}),
    ...(eventPrUrl
      ? { eventSettled: eventWasSettled(found.payload, records, undispatchable) }
      : {}),
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
      `${JSON.stringify({ summary: result.summary, rankRefreshes: result.rankRefreshes ?? [], ...(result.unreadable ? { unreadable: result.unreadable } : {}), dryRun: result.dryRun, ...(result.unchanged ? { unchanged: true } : {}) })}\n`,
    )
  } catch (error) {
    process.stderr.write(
      `verify-route: ${error instanceof Error ? error.message : String(error)}\n`,
    )
    process.exitCode = error instanceof UsageError ? 2 : 1
  }
}
