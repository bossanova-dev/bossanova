// Settled-subscription upkeep for in-flight epic children.
//
// The daemon's broadcast subscription evaluator is level-triggered: any active `settled`
// subscription on a session already sitting in a trigger state fires on the next worker tick.
// Refreshing the stored record from the live list and gating new subscriptions on the session's
// machine state keeps a live watch honest without creating a wake loop on settled states.

/**
 * @typedef {{
 *   id?: string,
 *   owner_session_id?: string,
 *   ownerSessionId?: string,
 *   origin_chat_id?: string,
 *   originChatId?: string,
 *   trigger_event?: string,
 *   triggerEvent?: string,
 *   state?: string,
 *   fired_at?: string,
 *   firedAt?: string,
 *   expires_at?: string,
 *   expiresAt?: string,
 * }} SubscriptionRow
 */

/**
 * @typedef {{
 *   id: string,
 *   ownerSessionId: string,
 *   originChatId: string,
 *   triggerEvent: string,
 *   state: string,
 *   firedAt: string,
 *   expiresAt: string,
 * }} StoredSubscription
 */

/**
 * @typedef {{
 *   ticketId?: string,
 *   sessionId: string,
 * }} SessionRecord
 */

/**
 * @typedef {{
 *   listSessionSubscriptions: (args: { sessionId: string }) => Promise<SubscriptionRow[] | null | undefined>,
 *   subscribeSessionOutcome: (args: {
 *     ticketId: string,
 *     sessionId: string,
 *     message: string,
 *   }) => Promise<{ subscription?: SubscriptionRow | null, error?: string } | null | undefined>,
 * }} SubscriptionIo
 */

/**
 * CLI spellings of the session states that satisfy a `settled` broadcast subscription.
 * Mirrors `TriggerClassFor` in `services/bossd/internal/broadcast/subscription.go` (`boss show
 * --json` prints each proto enum name without its `SESSION_STATE_` prefix). Toolbox code may not
 * import host packages, so this set is maintained as a small standalone mirror.
 *
 * @type {ReadonlySet<string>}
 */
export const SETTLED_SESSION_STATES = Object.freeze(
  new Set(['GREEN_DRAFT', 'READY_FOR_REVIEW', 'MERGED', 'CLOSED', 'BLOCKED', 'ORPHANED']),
)

/**
 * Subset of `SETTLED_SESSION_STATES` where skipping a `settled` subscription leaves a coverage gap
 * that requires a fallback wake. In these states no PR callback can still fire to wake the driver
 * if the child idles or stalls; in `READY_FOR_REVIEW`, `MERGED` and `CLOSED`, PR callbacks remain.
 *
 * @type {ReadonlySet<string>}
 */
export const GAP_WHEN_SETTLED_STATES = Object.freeze(
  new Set(['GREEN_DRAFT', 'BLOCKED', 'ORPHANED']),
)

/**
 * @param {unknown} value
 * @returns {value is string}
 */
function isNonEmptyString(value) {
  return typeof value === 'string' && value.trim().length > 0
}

/**
 * Return true when `sessionState` names a daemon trigger state where a newly created `settled`
 * subscription would fire immediately. Empty, missing or unknown values return false so newly
 * launched children without a snapshot yet subscribe normally.
 *
 * @param {unknown} sessionState
 * @returns {boolean}
 */
export function sessionReadsSettled(sessionState) {
  if (typeof sessionState !== 'string') return false
  return SETTLED_SESSION_STATES.has(sessionState.trim().toUpperCase())
}

/**
 * Should a durable registration be re-armed while the child stays in flight? A one-shot watch is
 * CONSUMED when it fires, and a level-triggered `settled` subscription fires on any session
 * already in a trigger state (including mid-flight `GREEN_DRAFT`), burning the wake the driver
 * was relying on. A fired/expired/canceled registration on a still-in-flight child is therefore
 * not coverage; callers that re-arm a `settled` subscription must still check
 * `sessionReadsSettled` so they do not re-subscribe into a trigger state and loop.
 *
 * @param {{ registration?: { state?: unknown } | null, inFlight?: boolean }} [input]
 * @returns {boolean}
 */
export function needsRearm({ registration, inFlight } = {}) {
  if (!inFlight) return false
  if (!registration) return true
  const state = String(registration.state ?? '').toLowerCase()
  if (state === 'active') return false
  return ['fired', 'expired', 'canceled', 'cancelled', ''].includes(state)
}

/**
 * Verify a durable registration is OURS and live, from a list-read row. Ownership is
 * `owner_session_id` + `origin_chat_id`; liveness is `state` plus the absence of a `fired_at`.
 * A row that is not ours is not coverage, however active it looks.
 *
 * @param {SubscriptionRow | null | undefined} row
 * @param {{ ownerSessionId?: string, originChatId?: string }} [expected]
 * @returns {{ ok: boolean, reason: string }}
 */
export function verifySubscriptionRow(row, { ownerSessionId, originChatId } = {}) {
  const id = row?.id ?? ''
  const owner = row?.owner_session_id ?? row?.ownerSessionId ?? ''
  const origin = row?.origin_chat_id ?? row?.originChatId ?? ''
  const state = String(row?.state ?? '').toLowerCase()
  const firedAt = row?.fired_at ?? row?.firedAt ?? ''
  if (!isNonEmptyString(id)) return { ok: false, reason: 'subscription row has no id' }
  if (isNonEmptyString(ownerSessionId) && owner !== ownerSessionId) {
    return { ok: false, reason: 'subscription owner_session_id does not match the child session' }
  }
  if (isNonEmptyString(originChatId) && origin !== originChatId) {
    return { ok: false, reason: 'subscription origin_chat_id does not match the orchestrator chat' }
  }
  if (state !== 'active')
    return { ok: false, reason: `subscription state is ${state || 'unknown'}` }
  if (isNonEmptyString(firedAt)) return { ok: false, reason: 'subscription already fired' }
  return { ok: true, reason: '' }
}

/**
 * Refresh a stored subscription record against live rows returned by `listSessionSubscriptions`.
 * Returns `null` when `stored` has no id or its id is absent from `rows` (removed, expired or
 * swept). A row with a non-empty `fired_at` / `firedAt` always reads as `fired`, regardless of
 * its `state` column, matching `verifySubscriptionRow`.
 *
 * @param {StoredSubscription | null | undefined} stored
 * @param {readonly SubscriptionRow[] | null | undefined} rows
 * @returns {StoredSubscription | null}
 */
export function refreshStoredSubscription(stored, rows) {
  if (!stored || !isNonEmptyString(stored.id)) return null
  const list = Array.isArray(rows) ? rows : []
  const row = list.find((candidate) => (candidate?.id ?? '') === stored.id) ?? null
  if (!row) return null
  const firedAt = isNonEmptyString(row.fired_at)
    ? row.fired_at
    : isNonEmptyString(row.firedAt)
      ? row.firedAt
      : ''
  const expiresAt = row.expires_at ?? row.expiresAt ?? ''
  const state = isNonEmptyString(firedAt) ? 'fired' : (row.state ?? '')
  return {
    ...stored,
    state,
    firedAt,
    expiresAt,
  }
}

/**
 * Keep one in-flight child's `settled` subscription honest across reconciliation cycles:
 *   1. Refresh any stored subscription from the live list (fail closed without re-subscribing if
 *      the list read throws).
 *   2. Keep a live subscription without subscribing again.
 *   3. Skip subscribing when the session already reads a level-triggered settled state, reporting
 *      a coverage gap only for `GAP_WHEN_SETTLED_STATES`.
 *   4. Otherwise subscribe, list-read and verify ownership.
 *
 * @param {{
 *   io: SubscriptionIo,
 *   ticketId?: string,
 *   record: SessionRecord,
 *   stored?: StoredSubscription | null,
 *   sessionState?: string,
 *   message: string,
 *   actions?: string[],
 * }} input
 * @returns {Promise<{ subscription: StoredSubscription | null, error: string }>}
 */
export async function ensureSettleSubscription({
  io,
  ticketId,
  record,
  stored = null,
  sessionState,
  message,
  actions = [],
}) {
  const resolvedTicketId = isNonEmptyString(ticketId) ? ticketId : (record?.ticketId ?? '')
  const sessionId = record?.sessionId ?? ''

  /** @type {StoredSubscription | null} */
  let subscription = stored ?? null

  // 1. Refresh any stored subscription against the live list.
  if (isNonEmptyString(stored?.id)) {
    let liveRows
    try {
      liveRows = (await io.listSessionSubscriptions({ sessionId })) ?? []
    } catch (err) {
      const errMessage = err instanceof Error ? err.message : String(err)
      actions.push(`subscribe:${resolvedTicketId}:unverified`)
      return {
        subscription: stored,
        error: `subscription list read failed: ${errMessage}`,
      }
    }
    subscription = refreshStoredSubscription(stored, liveRows)
  }

  // 2. Keep a live subscription.
  if (!needsRearm({ registration: subscription, inFlight: true })) {
    return { subscription, error: '' }
  }

  // 3. Settled-state gate: do not subscribe into a level-triggered state that would fire at once.
  if (sessionReadsSettled(sessionState)) {
    const normalizedState = String(sessionState).trim().toUpperCase()
    actions.push(`subscribe:${resolvedTicketId}:skipped:${normalizedState}`)
    const error = GAP_WHEN_SETTLED_STATES.has(normalizedState)
      ? `session reads ${normalizedState}, so a settled subscription would fire at once; the fallback wake covers it`
      : ''
    return { subscription, error }
  }

  // 4. Subscribe and verify ownership from a fresh list read.
  const created =
    (await io.subscribeSessionOutcome({
      ticketId: resolvedTicketId,
      sessionId,
      message,
    })) ?? {}
  let subError = created.error ?? ''
  const rows = (await io.listSessionSubscriptions({ sessionId })) ?? []
  const wantedId = created.subscription?.id ?? ''
  const row = rows.find((candidate) => (candidate?.id ?? '') === wantedId) ?? null
  const verdict = row
    ? verifySubscriptionRow(row, { ownerSessionId: sessionId })
    : { ok: false, reason: 'subscription absent from the list read' }
  if (verdict.ok && row) {
    subscription = {
      id: row.id ?? '',
      ownerSessionId: row.owner_session_id ?? row.ownerSessionId ?? '',
      originChatId: row.origin_chat_id ?? row.originChatId ?? '',
      triggerEvent: row.trigger_event ?? row.triggerEvent ?? '',
      state: row.state ?? '',
      firedAt: row.fired_at ?? row.firedAt ?? '',
      expiresAt: row.expires_at ?? row.expiresAt ?? '',
    }
    subError = ''
  } else {
    subscription = null
    subError = subError || verdict.reason
  }
  actions.push(`subscribe:${resolvedTicketId}${subError ? `:failed:${subError}` : ':verified'}`)
  return { subscription, error: subError }
}
