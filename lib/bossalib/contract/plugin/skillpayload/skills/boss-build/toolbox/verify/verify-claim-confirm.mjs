import { chatIsBusy } from '../stage-chain.mjs'
import { claimState, VERIFY_DESCRIPTIONS } from './verify-vocabulary.mjs'

export const CLAIM_CONFIRM_BACKOFF_MS = Object.freeze([250, 500, 1000])
export const DISPATCH_CONFIRM_MS = 6_000
export const DISPATCH_CONFIRM_POLL_MS = 1500
const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
const sameStatus = (a, b) =>
  ['state', 'description', 'updatedAt'].every((key) => a?.[key] === b?.[key])
// null means unreadable history; undefined means a readable history with no verify status.
const withdrawalAllowed = (latest, { token, prior = undefined }) =>
  latest === null ||
  claimState(latest, { token }).kind === 'mine' ||
  (prior !== undefined && sameStatus({ ...latest, updatedAt: latest?.updated_at }, prior))

/**
 * Confirms a posted claim against fresh reads. A readable change from the pre-write status is
 * superseding evidence; the pre-write status itself may be an expired claim being taken over.
 * @param {{read: () => {readable: boolean, status?: any}, prior?: any, token: string, sleep?: (ms: number) => any}} input
 * @returns {Promise<'won' | 'superseded' | 'unconfirmed'>}
 */
export async function confirmOwnClaim({ read, prior, token, sleep = pause }) {
  for (let attempt = 0; ; attempt++) {
    const back = read()
    if (back.readable) {
      if (claimState(back.status, { token }).kind === 'mine') return 'won'
      if (prior === undefined || !sameStatus(back.status, prior)) return 'superseded'
    }
    if (attempt === CLAIM_CONFIRM_BACKOFF_MS.length) return 'unconfirmed'
    await sleep(CLAIM_CONFIRM_BACKOFF_MS[attempt])
  }
}

/** @param {{park?: {state: string, description: string, targetUrl?: string} | null, reason: string}} input */
export function claimWithdrawal({ park, reason }) {
  return park
    ? {
        state: park.state,
        description: park.description,
        ...(park.targetUrl ? { targetUrl: park.targetUrl } : {}),
      }
    : { state: 'pending', description: `${VERIFY_DESCRIPTIONS.waiting} ${reason}` }
}

/**
 * The sweep is synchronous. Its injected clock and sleep bound polling while preserving the
 * separate release timeout. Only WORKING confirms that an unknown send started a turn.
 * @param {{run: Function, boss: string, sessionId?: string, chatId?: string, boundMs: number, sleep: (ms: number) => void, timeout: () => number, now?: () => number}} input
 */
export function confirmDispatchStarted({
  run,
  boss,
  sessionId,
  chatId,
  boundMs,
  sleep,
  timeout,
  now = Date.now,
}) {
  if (!sessionId || !chatId || boundMs <= 0) return false
  const deadline = now() + Math.min(DISPATCH_CONFIRM_MS, boundMs)
  while (now() < deadline) {
    const r = run(boss, ['chats', '--json', sessionId], {
      json: true,
      timeoutMs: Math.min(timeout(), Math.max(1, deadline - now())),
    })
    if (
      r?.ok &&
      Array.isArray(r.payload?.chats) &&
      r.payload.chats.some((chat) => chat.agent_session_id === chatId && chatIsBusy(chat))
    )
      return true
    const left = deadline - now()
    if (left <= 0) break
    sleep(Math.min(DISPATCH_CONFIRM_POLL_MS, left))
  }
  return false
}

/**
 * Withdrawal uses fresh park history; callers supply the status writer so this module remains
 * independent of the gate's reader and commit-status transport.
 * @param {{read: () => {readable: boolean, status?: any}, prior: any, token: string, sleep?: (ms: number) => any, readPark: () => any, post: (status: {state: string, description: string, targetUrl?: string}) => boolean, reason: string}} input
 */
export async function confirmClaimOrWithdraw({ readPark, post, reason, ...confirmation }) {
  const result = await confirmOwnClaim(confirmation)
  if (result === 'won') return { won: true }
  if (result === 'superseded') return { won: false, reason: 'claim-held' }
  const { latest, park } = readPark()
  if (!withdrawalAllowed(latest, confirmation)) return { won: false, reason: 'claim-held' }
  return { won: false, reason, released: post(claimWithdrawal({ park, reason })) }
}

/** A failed route's release and audit record are kept together. */
export function releaseUnknownDispatch({ release, remember, routed }) {
  const released = release()
  const why = routed.why ?? 'failed'
  remember('dispatch-unknown', { why, released })
  return `dispatch-unknown: ${routed.route} (${why}${released ? '; released' : ''})`
}

/** @param {{current: {readable: boolean, status?: any}, read: () => {readable: boolean, status?: any}, readPark: () => any, token: string, now: number, sleep?: (ms: number) => any, post: Function, reason: string}} input */
export async function releaseOwnClaim({
  current,
  read,
  readPark,
  token,
  now,
  sleep,
  post,
  reason,
}) {
  if (!current.readable) {
    const result = await confirmOwnClaim({ read, token, sleep })
    if (result === 'superseded') return { released: false, reason: 'not-mine' }
  } else if (claimState(current.status, { token, now }).kind !== 'mine')
    return { released: false, reason: 'not-mine' }
  const { latest, park } = readPark()
  if (!withdrawalAllowed(latest, { token })) return { released: false, reason: 'not-mine' }
  return post(claimWithdrawal({ park, reason }))
    ? { released: true }
    : { released: false, reason: 'status-post-failed' }
}

/**
 * Confirm an unknown send before withdrawing it; never consume the release's own reserve.
 * @param {{routed: any, remaining: () => number, reserveMs: number, release: () => boolean, remember: Function, run: Function, boss: string, sleep: (ms: number) => void, timeout: () => number, now?: () => number}} input
 */
export function recoverUnknownDispatch({
  routed,
  remaining,
  reserveMs,
  release,
  remember,
  ...poll
}) {
  const { route, sessionId, chatId } = routed
  const confirmed =
    routed.why !== 'DELIVERY_NOT_SUBMITTED' &&
    ['existing-verify-chat', 'new-verify-chat'].includes(route) &&
    confirmDispatchStarted({
      ...poll,
      sessionId,
      chatId,
      boundMs: Math.max(0, remaining() - reserveMs),
    })
  return confirmed ? null : releaseUnknownDispatch({ routed, release, remember })
}

/** The distinction between a lost race and an unconfirmed, withdrawn claim is observable. */
export function claimFailureLine(claimed) {
  return claimed.reason === 'claim-unconfirmed'
    ? `skipped: claim unconfirmed (${claimed.released ? 'released' : 'release failed'})`
    : 'skipped: claim lost'
}

/**
 * Settle a dispatch before recording success; an unknown send must either start or release.
 * @param {Parameters<typeof recoverUnknownDispatch>[0] & {why?: string, dryRun: boolean}} input
 */
export function settleDispatch({ routed, why, dryRun, ...recovery }) {
  let confirmed = false
  if (!routed.ok) {
    const failure = recoverUnknownDispatch({ ...recovery, routed })
    if (failure) return { outcome: 'skipped', line: failure, extra: { route: routed.route } }
    confirmed = true
  }
  recovery.remember('dispatched')
  const { route, note, log, argv } = routed
  const detail = confirmed
    ? `confirmed after ${routed.why ?? 'failed'}`
    : [why, note, log ? `log ${log}` : ''].filter(Boolean).join('; ')
  return {
    outcome: 'dispatched',
    line: `dispatched: ${route}${detail ? ` (${detail})` : ''}${dryRun ? ' (dry-run)' : ''}`,
    extra: { route, ...(dryRun && argv ? { argv } : {}) },
  }
}
