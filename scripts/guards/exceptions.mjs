// The inline exception format for registered guards:
//
//   guard-exception(<guard-id>): <reason>; expires YYYY-MM-DD
//
// It may sit inside any comment syntax (`//`, `#`, `<!-- -->`, `/* */`). A marker that shares its
// line with code covers that line; a marker alone on its line covers the next non-blank line.
// There is no approval field: raising an exception is a plain edit reviewed in the PR.
//
// Per-PR guards call parseExceptions and isExcepted only. expiredExceptions exists for the daily
// sweep, so an unrelated PR can never go red because a date passed.

const MARKER = /guard-exception\(([^)]*)\):(.*)$/
const EXPIRY = /^(.*?);\s*expires\b\s*(\S*)/
const COMMENT_OPENERS = /^(?:\/\/+|#+|<!--|\/\*+|\*+|--)$/
const COMMENT_CLOSERS = /(?:-->|\*\/)$/

function isValidDate(text) {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(text)) return false
  const date = new Date(`${text}T00:00:00Z`)
  return !Number.isNaN(date.getTime()) && date.toISOString().slice(0, 10) === text
}

/**
 * Parse every inline exception marker in `text`.
 *
 * @param {string} text File contents.
 * @param {{guardId?: string}} [options] Only markers for this guard id; all markers when omitted.
 * @returns {{exceptions: {guardId: string, line: number, coversLine: number, reason: string,
 *   expires: string}[], errors: string[]}}
 */
export function parseExceptions(text, { guardId } = {}) {
  const lines = text.split('\n')
  const exceptions = []
  const errors = []
  lines.forEach((raw, index) => {
    const match = MARKER.exec(raw)
    if (!match) return
    const markerId = match[1].trim()
    if (guardId !== undefined && markerId !== guardId) return
    const lineNumber = index + 1
    const where = `line ${lineNumber}: guard-exception(${markerId})`
    const expiry = EXPIRY.exec(match[2])
    if (!expiry) {
      errors.push(`${where} must read \`<reason>; expires YYYY-MM-DD\``)
      return
    }
    const reason = expiry[1].trim()
    const expires = expiry[2].replace(COMMENT_CLOSERS, '')
    if (reason === '') {
      errors.push(`${where} has an empty reason`)
      return
    }
    if (!isValidDate(expires)) {
      errors.push(`${where} expiry ${JSON.stringify(expires)} is not a valid YYYY-MM-DD date`)
      return
    }
    const before = raw.slice(0, match.index).trim()
    let coversLine = lineNumber
    if (before === '' || COMMENT_OPENERS.test(before)) {
      const next = lines.findIndex((line, at) => at > index && line.trim() !== '')
      if (next === -1) {
        errors.push(`${where} sits alone on its line and covers no line after it`)
        return
      }
      coversLine = next + 1
    }
    exceptions.push({ guardId: markerId, line: lineNumber, coversLine, reason, expires })
  })
  return { exceptions, errors }
}

/**
 * Whether a parsed exception covers `lineNumber` (1-based).
 *
 * @param {{exceptions: {coversLine: number}[]}} parsed
 * @param {number} lineNumber
 */
export function isExcepted(parsed, lineNumber) {
  return parsed.exceptions.some((exception) => exception.coversLine === lineNumber)
}

/**
 * The exceptions whose expiry date is before `today`. For the daily sweep only — per-PR guards
 * must never call this.
 *
 * @param {{exceptions: {expires: string}[]}} parsed
 * @param {string | Date} today `YYYY-MM-DD` or a Date (its UTC day is used).
 */
export function expiredExceptions(parsed, today) {
  const day = today instanceof Date ? today.toISOString().slice(0, 10) : today
  return parsed.exceptions.filter((exception) => exception.expires < day)
}
