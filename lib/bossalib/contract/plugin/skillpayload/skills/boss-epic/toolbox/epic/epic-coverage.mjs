// Pure coverage rules for re-arming epic PR watches.
//
// A state-matching trigger whose condition the PR already satisfies is not re-armed (re-arming
// would fire at once). `ready_for_review`, `checks_failed`, and `checks_passed_ready` on a green
// child count as covered for the cycle. Three cases keep a fallback wake instead:
//   - `checks_passed_ready` on a held child (no PR trigger reports the hold clearing),
//   - `checks_passed_ready` on a green child that did not merge in the cycle,
//   - `merged` or `closed` while the child is still in flight (no further PR trigger will fire to
//     reconcile the terminal provider outcome).

export const HELD_CHECKS_PASSED_READY_REASON =
  'checks_passed_ready holds but the child is held; no trigger reports the hold clearing'

export const UNMERGED_CHECKS_PASSED_READY_REASON =
  'checks_passed_ready holds and the child did not merge this cycle; no trigger will wake the next merge attempt'

const TERMINAL_TRIGGERS = new Set(['merged', 'closed'])

/**
 * Format the action token for a fallback or unwatched watch failure.
 *
 * @param {string} ticketId
 * @param {{ mechanism?: string } | null | undefined} fallback
 * @param {string} reason
 * @returns {string}
 */
export function fallbackAction(ticketId, fallback, reason) {
  return fallback ? `fallback:${ticketId}:${fallback.mechanism}` : `unwatched:${ticketId}:${reason}`
}

/**
 * Partition `rearmNeeded` against live `verifiedTriggers` and I/O-reported `satisfied` triggers.
 *
 * @param {{
 *   rearmNeeded?: readonly string[],
 *   satisfied?: unknown,
 *   verifiedTriggers?: ReadonlySet<string> | readonly string[],
 *   held?: boolean,
 * }} [input]
 * @returns {{ covered: string[], stillMissing: string[], reasons: string[] }}
 */
export function rearmCoverage({
  rearmNeeded = [],
  satisfied,
  verifiedTriggers,
  held = false,
} = {}) {
  const requested = [...new Set(Array.isArray(rearmNeeded) ? rearmNeeded : [])]
  const requestedSet = new Set(requested)
  /** @type {ReadonlySet<string>} */
  const verified =
    verifiedTriggers instanceof Set
      ? verifiedTriggers
      : new Set(Array.isArray(verifiedTriggers) ? verifiedTriggers : [])
  const claimedSet = new Set(
    Array.isArray(satisfied)
      ? satisfied.filter((trigger) => typeof trigger === 'string' && requestedSet.has(trigger))
      : [],
  )
  /** @type {string[]} */
  const covered = []
  /** @type {string[]} */
  const stillMissing = []
  /** @type {string[]} */
  const reasons = []

  for (const trigger of requested) {
    if (verified.has(trigger)) continue
    if (claimedSet.has(trigger)) {
      if (held && trigger === 'checks_passed_ready') {
        stillMissing.push(trigger)
        reasons.push(HELD_CHECKS_PASSED_READY_REASON)
      } else if (TERMINAL_TRIGGERS.has(trigger)) {
        stillMissing.push(trigger)
        reasons.push(
          `${trigger} already holds while the child is still in flight; fallback wake required to reconcile provider outcome`,
        )
      } else {
        covered.push(trigger)
      }
      continue
    }
    stillMissing.push(trigger)
  }

  return { covered, stillMissing, reasons }
}

/**
 * Combine an I/O arm error with `rearmCoverage` gaps and specific reasons.
 *
 * @param {{
 *   armError?: string,
 *   stillMissing?: readonly string[],
 *   reasons?: readonly string[],
 * }} [input]
 * @returns {string}
 */
export function coverageFailureReason({ armError = '', stillMissing = [], reasons = [] } = {}) {
  if (stillMissing.length === 0) return armError
  /** @type {string[]} */
  const parts = []
  if (armError) parts.push(armError)
  if (reasons.length > 0) parts.push(...reasons)
  const unreasoned = stillMissing.filter(
    (trigger) => !reasons.some((reason) => reason.startsWith(`${trigger} `)),
  )
  if (!armError && unreasoned.length > 0) {
    parts.push(`list verification found no watch for ${unreasoned.join(',')}`)
  }
  return parts.join('; ')
}
