// verify-leg-marker.mjs — the two `x-boss-extension` keys that shape a `role: verify` leg:
// `timeout: <minutes>` replaces the default leg timeout, and `writes: ignored` lets the leg create
// files git reports as ignored. Discovery reads them here; verify-extension-leg.mjs enforces them.

export const MAX_LEG_TIMEOUT_MINUTES = 45
export const LEG_WRITES = Object.freeze(['ignored'])

const MINUTES = /^[0-9]+$/

/** A whole number of minutes from 1 to MAX_LEG_TIMEOUT_MINUTES, else null. */
function parseTimeout(value) {
  const raw = typeof value === 'string' ? value.trim() : value
  const minutes = typeof raw === 'string' && MINUTES.test(raw) ? Number(raw) : raw
  return Number.isInteger(minutes) && minutes >= 1 && minutes <= MAX_LEG_TIMEOUT_MINUTES
    ? minutes
    : null
}

/**
 * The usable leg keys of a verify marker block, as descriptor fields. An absent or unusable key is
 * omitted, so an undeclared descriptor's JSON is unchanged; `legMarkerSkip` reports unusable ones.
 * @param {Record<string, unknown>} block
 * @returns {{timeoutMinutes?: number, writes?: string}}
 */
export function parseLegMarker(block) {
  /** @type {{timeoutMinutes?: number, writes?: string}} */
  const out = {}
  const timeoutMinutes = parseTimeout(block.timeout)
  if (timeoutMinutes !== null) out.timeoutMinutes = timeoutMinutes
  const writes = /** @type {string} */ (block.writes)
  if (LEG_WRITES.includes(writes)) out.writes = writes
  return out
}

/**
 * The skip a present but unusable leg key earns, or null. A bad value is a failed declaration,
 * never read as the default: a typo'd timeout would otherwise cut a long leg at five minutes.
 * @param {Record<string, unknown>} block
 * @returns {{code: string, reason: string} | null}
 */
export function legMarkerSkip(block) {
  if (block.timeout !== undefined && parseTimeout(block.timeout) === null)
    return {
      code: 'invalid-timeout',
      reason: `invalid "timeout" (expected whole minutes from 1 to ${MAX_LEG_TIMEOUT_MINUTES})`,
    }
  if (block.writes !== undefined && !LEG_WRITES.includes(/** @type {string} */ (block.writes)))
    return {
      code: 'invalid-writes',
      reason: `invalid "writes" (expected ${LEG_WRITES.join(', ')})`,
    }
  return null
}
