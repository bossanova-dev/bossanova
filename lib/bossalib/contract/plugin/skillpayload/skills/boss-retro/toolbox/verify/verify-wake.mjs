import { CHECK_REASONS, CHECK_STATES } from '../pr-check-state.mjs'

// A verify wait on something no callback announces (GitHub computes mergeability lazily) retries in
// the chat this many times, this far apart, then leaves the waiting head to the cron gate.
export const VERIFY_RETRY_ATTEMPTS = 3
export const VERIFY_RETRY_AFTER_MS = 60_000

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
