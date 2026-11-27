// Whether a standing park on a head holds when it is re-judged with no approval, send-back or new
// results. The judge reports a held park as it is, so nothing is re-posted.

import { capDescription } from '../commit-status.mjs'
import { TRIAL_POLICY_CODE } from './verify-policy-ref.mjs'
import {
  CLAIM_EXPIRED_REASON,
  CLAIM_RELEASE_REASONS,
  VERIFY_CONTEXT,
  VERIFY_DESCRIPTIONS,
  isCannotAssess,
} from './verify-vocabulary.mjs'

/**
 * The policy fingerprint carries this, so a change to when a park holds re-judges every park once.
 * 2: a park no longer holds while an applicable extension has no recorded run on its head; cores
 * before it re-recorded the fingerprint over such parks, which then never re-judged.
 * 3: an `extension-failed` park no longer holds while its head has extension reruns left.
 */
export const PARK_HOLD_REVISION = 3

// A timed-out or crashed extension usually means a busy host, not a fact about the head. A head
// gets the first malfunction plus two reruns before its `extension-failed` park holds for a person.
export const EXTENSION_ATTEMPTS = 3
// The wait a transient malfunction posts while reruns remain; the next tick reruns the extensions.
export const EXTENSION_RETRY_REASON = 'extension-retry'
// Prefix for an ordinary wait posted during an extension-retry / extension-failed rerun, so
// batched candidate discovery keeps admitting the head without counting it toward EXTENSION_ATTEMPTS.
export const EXTENSION_RETRY_WAIT_PREFIX = `${VERIFY_DESCRIPTIONS.waiting} ${EXTENSION_RETRY_REASON} · `
// Malfunctions a rerun can clear. A malformed or failed result says the same thing again.
const TRANSIENT_MALFUNCTIONS = new Set(['timed-out', 'crashed'])

/** Is every failed extension outcome one a rerun can clear? */
export function transientMalfunction(outcomes) {
  const failed = (Array.isArray(outcomes) ? outcomes : []).filter((o) => o?.outcome === 'failed')
  return failed.length > 0 && failed.every((o) => TRANSIENT_MALFUNCTIONS.has(o.reason))
}

export const namesExtensionFailed = (description) => {
  if (typeof description !== 'string' || !description.startsWith(VERIFY_DESCRIPTIONS.needsHuman))
    return false
  const codes = description.slice(VERIFY_DESCRIPTIONS.needsHuman.length).split(' · ')[0]
  return codes.split(',').some((code) => code.trim() === 'extension-failed')
}

/**
 * How many extension runs on this head malfunctioned: each retry wait and each distinct
 * `extension-failed` park episode in its `boss/verify` history. Re-posting an already-standing
 * `extension-failed` park (via `restore-park`, legacy `pending`→`success` upgrades, or pre-change
 * claim withdrawals) does not start a new episode until an intervening `waiting: extension-retry`
 * or a status that supersedes the park clears it. `nodes` is the raw status list; an unreadable
 * list (null) reads as exhausted, so no extension reruns on a guess.
 */
export function extensionMalfunctions(nodes) {
  if (!Array.isArray(nodes)) return EXTENSION_ATTEMPTS
  const retry = `${VERIFY_DESCRIPTIONS.waiting} ${EXTENSION_RETRY_REASON}`
  let count = 0
  let parked = false
  for (const n of newestVerifyFirst(nodes).reverse()) {
    const description = typeof n?.description === 'string' ? n.description : ''
    if (description === retry) {
      count++
      parked = false
    } else if (namesExtensionFailed(description)) {
      if (!parked) {
        count++
        parked = true
      }
    } else if (!replacesPark(n)) {
      parked = false
    }
  }
  return count
}

/**
 * Does the malfunction this run is judging leave a rerun after it? Then a transient one waits
 * instead of parking. `readNodes` returns the head's raw status list (null when unreadable).
 */
export function rerunsMalfunction(readNodes) {
  return extensionMalfunctions(readNodes()) + 1 < EXTENSION_ATTEMPTS
}

/** Is `status` the pending retry wait a transient extension malfunction leaves? */
export function isExtensionRetryWait(status) {
  return (
    status?.state === 'pending' &&
    status.description === `${VERIFY_DESCRIPTIONS.waiting} ${EXTENSION_RETRY_REASON}`
  )
}

/** Format an ordinary wait posted while an extension retry or `extension-failed` park is active. */
export function extensionRetryWaitDescription(reason) {
  return capDescription(`${EXTENSION_RETRY_WAIT_PREFIX}${reason}`)
}

/** Is `status` a pending ordinary wait preserving an extension-retry or `extension-failed` rerun? */
export function isExtensionRetryFollowUpWait(status) {
  return (
    status?.state === 'pending' &&
    typeof status.description === 'string' &&
    status.description.startsWith(EXTENSION_RETRY_WAIT_PREFIX)
  )
}

/** Extract the wait reason from a `waiting:` description, stripping any extension-retry prefix. */
export function waitingReason(description) {
  if (typeof description !== 'string') return null
  if (description.startsWith(EXTENSION_RETRY_WAIT_PREFIX))
    return description.slice(EXTENSION_RETRY_WAIT_PREFIX.length)
  return description.startsWith(`${VERIFY_DESCRIPTIONS.waiting} `)
    ? description.slice(VERIFY_DESCRIPTIONS.waiting.length + 1)
    : null
}

/** Is `status` a pending `waiting:` status? */
export const isWaiting = (status) =>
  status?.state === 'pending' &&
  typeof status.description === 'string' &&
  status.description.startsWith(VERIFY_DESCRIPTIONS.waiting)

const CLAIM_RELEASE_WAITS = new Set([
  `${VERIFY_DESCRIPTIONS.waiting} ${CLAIM_RELEASE_REASONS.unconfirmed}`,
  `${VERIFY_DESCRIPTIONS.waiting} ${CLAIM_RELEASE_REASONS.dispatch}`,
  `${VERIFY_DESCRIPTIONS.waiting} ${CLAIM_EXPIRED_REASON}`,
])

/** Is `status` a pending wait left by withdrawing or expiring a claim? */
export function isClaimReleaseWait(status) {
  return status?.state === 'pending' && CLAIM_RELEASE_WAITS.has(status.description)
}

/** Does `status` keep any standing park beneath it alive for candidate discovery? */
export function retainsStandingPark(status) {
  return (
    isCannotAssess(status) ||
    isExtensionRetryWait(status) ||
    isExtensionRetryFollowUpWait(status) ||
    isClaimReleaseWait(status)
  )
}

/** A head's `boss/verify` statuses, newest first; ties and unreadable times preserve API order. */
export function newestVerifyFirst(nodes) {
  return (Array.isArray(nodes) ? nodes : [])
    .filter((n) => n?.context === VERIFY_CONTEXT)
    .map((n, i) => ({ n, i, at: Date.parse(n.updated_at ?? n.created_at ?? '') }))
    .sort((a, b) => (Number.isFinite(b.at) && Number.isFinite(a.at) ? b.at - a.at : 0) || a.i - b.i)
    .map(({ n }) => n)
}

/** Is `n` a pending `verifying…` claim status? */
export const isClaim = (n) =>
  n?.state === 'pending' && String(n.description ?? '').startsWith(VERIFY_DESCRIPTIONS.verifying)

/** A wait, cannot-assess hold or claim replaces a park while a run works on it. */
export const replacesPark = (status) =>
  isCannotAssess(status) || isWaiting(status) || isClaim(status)

/** The standing park beneath any claims, waits or cannot-assess holds, or null if none. */
export function standingParkOf(nodes, isPark) {
  for (const n of newestVerifyFirst(nodes)) {
    const description = typeof n.description === 'string' ? n.description : ''
    const targetUrl = typeof n.target_url === 'string' ? n.target_url : ''
    if (isPark(n)) return { state: n.state, description, ...(targetUrl && { targetUrl }) }
    if (!replacesPark(n)) return null
  }
  return null
}

/**
 * Does `status` keep an off-selection head judged (standing park or active extension-retry)?
 * Returns `true`, `false`, or `null` when status history had to be read and was unreadable.
 */
export function retainsJudgedHead(status, readNodes, isPark) {
  if (isExtensionRetryWait(status)) return true
  if (!retainsStandingPark(status)) return false
  const nodes = readNodes()
  if (!nodes) return null
  return Boolean(standingParkOf(nodes, isPark)) || hasActiveExtensionRetry(nodes)
}

/**
 * Did a verify run claim this head before its newest park? Only a run that consults extensions
 * claims, so a park with no claim beneath it was decided without them. `nodes` is the raw status
 * list; an unreadable list (null) is a yes, so no extension-raised code is released on a guess.
 */
export function extensionRunBeforePark(nodes, isPark) {
  if (!Array.isArray(nodes)) return true
  const verify = newestVerifyFirst(nodes)
  const at = verify.findIndex((n) => isPark(n))
  return at === -1 || verify.slice(at + 1).some(isClaim)
}

/**
 * Is an `extension-retry` wait still active beneath any claims and later waits on this head?
 * Stops at any non-claim, non-wait status (a park, cannot-assess hold, or other verdict).
 */
export function hasActiveExtensionRetry(nodes) {
  if (!Array.isArray(nodes)) return false
  for (const n of newestVerifyFirst(nodes)) {
    if (isExtensionRetryWait(n)) return true
    if (!isClaim(n) && !isWaiting(n)) return false
  }
  return false
}

/**
 * Park to restore when withdrawing a claim. Reposting an `extension-failed` park (or writing a
 * park over an active `waiting: extension-retry`) would append a phantom malfunction to the commit
 * status history and hide the active retry wait; returning `null` withdraws to `waiting: <reason>`
 * instead, which `retainsStandingPark` and `hasActiveExtensionRetry` look through.
 */
export function withdrawalPark(nodes, park) {
  if (!park) return null
  if (hasActiveExtensionRetry(nodes) || namesExtensionFailed(park.description)) return null
  return park
}

/**
 * Does an ordinary wait posted over `nodes` need the extension-retry prefix so off-selection
 * candidate discovery keeps admitting the head across `fallback: 'gate'`?
 */
export function preservesExtensionRetryWait(nodes, park) {
  return hasActiveExtensionRetry(nodes) || namesExtensionFailed(park?.description)
}

/** Does a parked head sitting at a wait or claim still have an active extension retry to run? */
export function rerunsRetryWait(status, codes, readNodes) {
  if (!isWaiting(status) && !isClaim(status)) return false
  if (!Array.isArray(codes) || !codes.includes('extension-failed')) return false
  const nodes = readNodes()
  return hasActiveExtensionRetry(nodes) && rerunsPark(codes, () => nodes, { retryWaiting: true })
}

/**
 * Does a standing `extension-failed` park rerun its extensions instead of holding?
 * - Only a judge verdict that would run the extensions (`extensions-required`) reruns them,
 *   so an unusable review's park (`review-required`) still holds.
 * - A park already recorded under the current policy (`policyRecorded: true`) holds — transient
 *   malfunctions wait (`waiting: extension-retry`) rather than park while attempts remain, so a
 *   park recorded under the current policy is final (non-transient or exhausted) — unless an
 *   active `waiting: extension-retry` (`retryWaiting: true`) sits over the park awaiting a rerun.
 * - An unrecorded or older-policy `extension-failed` park reruns while attempts remain.
 */
export function rerunsPark(
  codes,
  readNodes,
  {
    verdict = { verdict: 'extensions-required' },
    policyRecorded = false,
    retryWaiting = false,
  } = {},
) {
  return (
    verdict?.verdict === 'extensions-required' &&
    Array.isArray(codes) &&
    codes.includes('extension-failed') &&
    (retryWaiting || !policyRecorded) &&
    extensionMalfunctions(readNodes()) < EXTENSION_ATTEMPTS
  )
}

// Merge failures stand independently of verify policy.
const NON_POLICY_CODES = new Set(['no-session', 'worktree-diverged', 'worktree-unsynced'])

/**
 * The applicable extensions with no recorded run on this head: those the newest extension record
 * (`[{extension, outcome}]`, or null when none) does not name. A malfunction park is itself the
 * record that its extensions ran, and a park naming a merge failure stands whatever they say, so
 * both name none.
 */
export function extensionsNotRun(codes, { applicable = [], record = null } = {}) {
  if (codes.some((code) => code === 'extension-failed' || NON_POLICY_CODES.has(code))) return []
  const ran = new Set(Array.isArray(record) ? record.map((entry) => entry.extension) : [])
  return applicable.filter((name) => !ran.has(name))
}

/**
 * Does the current base still raise every code of a standing park? `fresh` comes from the zero-token
 * judge; collector-only `cannot-assess` stands only while that judge still raises it. Other stale
 * codes stand when an extension that ran before the park (`extensionsRan`) may still raise them.
 * The clean-run `record` names which `applicable` extension raised each code; a malfunction park,
 * or a park without a record, conservatively stands while any extension still applies.
 */
export function parkStillRaised(
  codes,
  { fresh = [], repairing = false, extensionsRan = true, applicable = [], record = null } = {},
) {
  const live = new Set(applicable)
  const judged = codes.includes('extension-failed') ? null : record
  const byExtension = (code) =>
    extensionsRan &&
    code !== 'cannot-assess' &&
    // Only a trial run raises a trial park, and it parks every verdict afresh; held, it would keep
    // an ordinary judge from re-judging the head under base policy.
    code !== TRIAL_POLICY_CODE &&
    (Array.isArray(judged)
      ? judged.some((e) => e.outcome === code && live.has(e.extension))
      : live.size > 0)
  return codes.every(
    (code) =>
      fresh.includes(code) ||
      (code === 'repair-exhausted' ? repairing : NON_POLICY_CODES.has(code) || byExtension(code)),
  )
}
