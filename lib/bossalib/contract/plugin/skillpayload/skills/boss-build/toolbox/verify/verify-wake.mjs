import * as nativeFs from 'node:fs'
import { createHash } from 'node:crypto'
import { isAbsolute, join } from 'node:path'
import { writeAtomicJson } from './verify-admission.mjs'
import { CHECK_REASONS, CHECK_STATES } from '../pr-check-state.mjs'

// A verify wait on something no callback announces (GitHub computes mergeability lazily) retries in
// the chat this many times, this far apart, then leaves the waiting head to the cron gate.
export const VERIFY_RETRY_ATTEMPTS = 3
export const VERIFY_RETRY_AFTER_MS = 60_000
export const VERIFY_RETRY_WINDOW_MS = 10 * VERIFY_RETRY_ATTEMPTS * VERIFY_RETRY_AFTER_MS

/** @typedef {{version: 1, head: string | null, count: number, at: number}} RetryRecord */
/** @typedef {{read: (key: string) => RetryRecord | null | undefined, write: (key: string, record: RetryRecord) => boolean}} RetryStore */

/**
 * A retry belongs to one head and one bounded wait episode; caller attempts can only shorten it.
 * @param {{record?: RetryRecord | null, head: string | null, now: number, floor?: number}} input
 */
export function retryAttempt({ record, head, now, floor = 1 }) {
  const count =
    record && record.head === head && now - record.at <= VERIFY_RETRY_WINDOW_MS ? record.count : 0
  return Math.max(floor, count + 1)
}

/**
 * One atomic JSON record per repo/PR/chat, shared across worktrees via the git common directory.
 * Missing or corrupt records can be replaced; other read failures must not grant fresh retries.
 * @param {{read: Function, cwd?: string, fs?: typeof nativeFs}} input
 * @returns {RetryStore}
 */
export function fileRetryStore({ read, cwd = process.cwd(), fs = nativeFs }) {
  const result = read('git', ['rev-parse', '--git-common-dir'])
  if (!result.ok || !result.output?.trim()) throw new Error('git common dir unavailable')
  const common = result.output.trim()
  const dir = join(isAbsolute(common) ? common : join(cwd, common), 'boss-verify', 'wake-retries')
  const path = (key) =>
    join(dir, `${createHash('sha256').update(key).digest('hex').slice(0, 32)}.json`)
  return {
    read(key) {
      try {
        const record = JSON.parse(fs.readFileSync(path(key), 'utf8'))
        return record?.version === 1 &&
          (record.head === null || typeof record.head === 'string') &&
          Number.isSafeInteger(record.count) &&
          record.count >= 0 &&
          Number.isFinite(record.at)
          ? record
          : null
      } catch (error) {
        if (error instanceof SyntaxError || error?.code === 'ENOENT') return null
        throw error
      }
    },
    write: (key, record) => writeAtomicJson(fs, path(key), record),
  }
}

/**
 * Spend a timed retry before sleeping, including interrupted sleeps. Callback and closed waits
 * never open the ledger. Unavailable storage leaves the head to the gate without a timer.
 * @param {Parameters<typeof wakePlan>[0]} input
 * @param {{pr: number, chat: string, repo: string, dryRun?: boolean, read: Function, retryStore?: RetryStore, now?: () => number}} io
 */
export function retryWakePlan(input, io) {
  const plan = wakePlan(input)
  if (plan.wake !== 'retry') return plan
  try {
    const store = io.retryStore ?? fileRetryStore(io)
    const key = `${io.repo}#${io.pr}:${io.chat}`
    const now = (io.now ?? Date.now)()
    const head = input.prView?.headRefOid ?? null
    const attempt = retryAttempt({
      record: store.read(key),
      head,
      now,
      floor: input.attempt,
    })
    const { retry, ...wait } = plan
    if (attempt > VERIFY_RETRY_ATTEMPTS) return { ...wait, wake: 'none', fallback: 'gate' }
    if (!io.dryRun && !store.write(key, { version: 1, head, count: attempt, at: now }))
      throw new Error('retry state write failed')
    return { ...plan, retry: { ...retry, attempt } }
  } catch {
    const { retry, ...wait } = plan
    return { ...wait, wake: 'none', fallback: 'gate', why: 'retry-state-unavailable' }
  }
}

const CI_REASONS = ['ci-not-settled', 'ci-not-green-on-head', 'ci-not-requested']
const REPAIR_WAITS = ['conflict', 'findings', 'blockers']

/** @param {{verdict?: string, reason?: string, prView?: any}} input */
function waitingOnFor({ verdict, reason, prView }) {
  if (prView?.mergeStateStatus === 'DIRTY' || reason === 'merge-state-dirty') return 'conflict'
  if (verdict === 'defect') return 'findings'
  if (verdict === 'blocked') return 'blockers'
  if (reason === 'pr-draft') return 'draft'
  if (reason === 'pr-not-open') return 'closed'
  if (typeof reason !== 'string' || !reason || CI_REASONS.includes(reason)) return 'ci'
  return reason.startsWith('merge-state-') ? 'merge-state' : 'verify'
}

/**
 * Which wake can still fire for this wait. An on-transition callback is armed only while its
 * transition is still to come: repair pushes a new head, a draft has not left draft, or CI on the
 * head is not green (`ciGreen: false`). Any other verify-owned wait retries on a timer, at most
 * VERIFY_RETRY_ATTEMPTS times, then falls back to the gate; a closed PR has nothing to wait for.
 * @param {{verdict?: string, reason?: string, prView?: any, ciGreen?: boolean | null, attempt?: number}} input
 */
export function wakePlan({ verdict, reason, prView, ciGreen = null, attempt = 1 }) {
  const waitingOn = waitingOnFor({ verdict, reason, prView })
  const owner = REPAIR_WAITS.includes(waitingOn)
    ? 'repair'
    : waitingOn === 'draft'
      ? 'person'
      : 'verify'
  if (owner !== 'verify' || (waitingOn === 'ci' && ciGreen === false))
    return {
      owner,
      waitingOn,
      wake: 'callback',
      trigger:
        prView?.isDraft === true && waitingOn !== 'draft' ? 'checks_passed' : 'checks_passed_ready',
      onTransition: true,
    }
  if (waitingOn === 'closed') return { owner, waitingOn, wake: 'none' }
  if (attempt > VERIFY_RETRY_ATTEMPTS) return { owner, waitingOn, wake: 'none', fallback: 'gate' }
  return {
    owner,
    waitingOn,
    wake: 'retry',
    retry: { attempt, of: VERIFY_RETRY_ATTEMPTS, afterMs: VERIFY_RETRY_AFTER_MS },
  }
}

/**
 * CI on the head as wakePlan reads it, from a judgeCiOnHead result: `true` green, `false` failing,
 * still pending or missing a required run (a CI callback can still fire), `null` unknown or unread.
 * @param {any} ci
 */
export function ciGreenOf(ci) {
  if (ci?.green) return true
  if (ci?.verdict?.reason === CHECK_REASONS.NOT_REQUESTED) return false
  return [CHECK_STATES.FAILING, CHECK_STATES.PENDING].includes(ci?.verdict?.state) ? false : null
}

/** @param {number} ms */
function sleepSync(ms) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, Math.max(0, ms))
}

/**
 * Carries out a wakePlan: arms its callback through `read`, sleeps out its retry, or arms nothing.
 * A dry run never writes and never sleeps.
 * @param {any} plan
 * @param {{pr: number, chat: string, repo?: string, dryRun?: boolean, read: Function, sleep?: Function}} io
 */
export async function applyWake(plan, { pr, chat, repo, dryRun = false, read, sleep = sleepSync }) {
  if (plan.wake === 'callback') {
    const argv = rearmArgv({ pr, chat, repo, trigger: plan.trigger })
    const writes = [{ kind: 'callback', argv }]
    if (dryRun) return { dryRun: true, ...plan, writes }
    const r = read('boss', argv, { json: true })
    return {
      armed: r.ok,
      ...plan,
      ...(r.ok ? { callback: r.payload } : { reason: r.error }),
      writes,
    }
  }
  const retry = plan.wake === 'retry'
  if (retry && !dryRun) await sleep(plan.retry.afterMs)
  return {
    ...(dryRun ? { dryRun: true } : {}),
    armed: false,
    ...plan,
    ...(retry ? { slept: dryRun ? 0 : plan.retry.afterMs } : {}),
    writes: [],
  }
}

/** @param {{pr: number | string, chat: string, repo?: string, trigger?: string}} input */
export function rearmArgv({ pr, chat, repo, trigger = 'checks_passed_ready' }) {
  return [
    'callback',
    'add',
    String(pr),
    trigger,
    '--on-transition',
    '--chat',
    chat,
    ...(typeof repo === 'string' && repo.trim() ? ['--repo', repo] : []),
    '--message',
    `/boss-verify ${pr}`,
    '--json',
  ]
}
