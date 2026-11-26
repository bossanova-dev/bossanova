/**
 * The parsers every part of a proof spec shares. Each throws `Drop` to drop the one item it
 * parses; `keepEach` keeps what parses and records each drop with its reason.
 */

/** @typedef {import('./verify-proof-spec.mjs').Dropped} Dropped */

/** @param {unknown} value @returns {value is Record<string, unknown>} */
export const isRecord = (value) =>
  value !== null && typeof value === 'object' && !Array.isArray(value)

/** Thrown inside one item's parser to drop that item; never escapes `validateProofSpec`. */
export class Drop extends Error {}

/** @param {unknown} value @param {number} max @param {string} field */
export function needText(value, max, field) {
  if (typeof value !== 'string' || !value.trim()) throw new Drop(`${field}: expected text`)
  if (value.length > max) throw new Drop(`${field}: longer than ${max} characters`)
  return value
}
/** @param {unknown} value @param {number} max @param {string} field */
export const optText = (value, max, field) =>
  value === undefined ? undefined : needText(value, max, field)
/** @param {unknown} value @param {string} field */
export function needLine(value, field) {
  if (!Number.isSafeInteger(value) || /** @type {number} */ (value) < 1)
    throw new Drop(`${field}: expected a line number`)
  return /** @type {number} */ (value)
}
/** @param {unknown} value @param {readonly string[]} keys @param {string} [field] */
export function onlyKeys(value, keys, field) {
  if (!isRecord(value)) throw new Drop(`${field ?? 'entry'}: expected object`)
  const extra = Object.keys(value).find((key) => !keys.includes(key))
  if (extra) throw new Drop(`${field ? `${field}.` : ''}${extra}: unknown key`)
  return value
}
/** Drop undefined optional fields so a kept item has only the keys it carries.
 * @template {Record<string, unknown>} T @param {T} item @returns {T} */
export const compact = (item) =>
  /** @type {T} */ (Object.fromEntries(Object.entries(item).filter(([, v]) => v !== undefined)))

/**
 * Parse each entry of `value` with `parse`, which also gets the entry's index in `value`; keep
 * what parses, record each failure in `dropped`.
 * @template T
 * @param {unknown} value @param {string} at @param {number} cap
 * @param {(entry: unknown, at: string, index: number) => T} parse @param {Dropped[]} dropped
 * @returns {T[]}
 */
export function keepEach(value, at, cap, parse, dropped) {
  if (value === undefined) return []
  if (!Array.isArray(value)) {
    dropped.push({ at, reason: 'expected a list' })
    return []
  }
  /** @type {T[]} */
  const kept = []
  value.forEach((entry, i) => {
    const where = `${at}[${i}]`
    if (kept.length >= cap) {
      dropped.push({ at: where, reason: `over the cap of ${cap}` })
      return
    }
    try {
      kept.push(parse(entry, where, i))
    } catch (error) {
      if (!(error instanceof Drop)) throw error
      dropped.push({ at: where, reason: error.message })
    }
  })
  return kept
}
