// The shared baseline format every registered guard uses (scripts/guards/registry/).
//
// A baseline is a JSON array of unique strings, sorted with the default JavaScript `<` order, and
// serialised as `JSON.stringify(entries, null, 2) + '\n'`. That is byte-identical to
// scripts/guards/cli-flag-parsers-baseline.json, so existing baselines move onto this helper without a
// reformatting diff. The file sits next to the guard script that owns it.
//
// An entry may carry its reason inline, written `<key> — <reason>`. A guard compares only the key
// (see splitBaselineEntry), so rewording a reason never reads as a stale entry.

import fs from 'node:fs'
import path from 'node:path'

/**
 * Read and validate a shared-format baseline.
 *
 * @param {string} repoRoot Absolute repository root.
 * @param {string} relPath Repo-relative path of the baseline file.
 * @returns {{entries: string[], errors: string[]}}
 */
export function readBaseline(repoRoot, relPath) {
  let parsed
  try {
    parsed = JSON.parse(fs.readFileSync(path.join(repoRoot, relPath), 'utf8'))
  } catch (error) {
    return { entries: [], errors: [`cannot read ${relPath}: ${error.message}`] }
  }
  if (!Array.isArray(parsed) || parsed.some((entry) => typeof entry !== 'string')) {
    return { entries: [], errors: [`${relPath} must be a JSON array of strings`] }
  }
  const errors = []
  if (parsed.some((entry, index) => index > 0 && parsed[index - 1] > entry)) {
    errors.push(`${relPath} must stay sorted`)
  }
  if (new Set(parsed).size !== parsed.length) errors.push(`${relPath} has duplicate entries`)
  return { entries: parsed, errors }
}

const REASON_SEPARATOR = ' — '

/**
 * Split a baseline entry into the key a guard compares and the reason written after ` — `.
 *
 * @param {string} entry
 * @returns {{key: string, reason: string}} `reason` is '' when the entry carries none.
 */
export function splitBaselineEntry(entry) {
  const at = entry.indexOf(REASON_SEPARATOR)
  if (at === -1) return { key: entry, reason: '' }
  return { key: entry.slice(0, at), reason: entry.slice(at + REASON_SEPARATOR.length).trim() }
}

/**
 * Compare a guard's current offenders against its baseline. The list only shrinks: an offender
 * missing from the baseline fails with the guard's remedy, and an entry that no longer occurs
 * fails until it is removed.
 *
 * @param {{baselinePath: string, entries: Iterable<string>, offenders: Iterable<string>,
 *   remedy: string | ((offender: string) => string)}} input
 * @returns {{failures: string[], newViolations: string[], staleEntries: string[]}}
 */
export function compareBaseline({ baselinePath, entries, offenders, remedy }) {
  const baseline = new Set(entries)
  const current = new Set(offenders)
  const newViolations = [...current].filter((offender) => !baseline.has(offender)).sort()
  const staleEntries = [...baseline].filter((entry) => !current.has(entry)).sort()
  const failures = [
    ...newViolations.map(
      (offender) => `${offender}: ${typeof remedy === 'function' ? remedy(offender) : remedy}`,
    ),
    ...staleEntries.map((entry) => `${baselinePath}: ${entry} no longer occurs — remove it`),
  ]
  return { failures, newViolations, staleEntries }
}

/**
 * Serialise entries in the shared baseline format (sorted, de-duplicated, trailing newline).
 *
 * @param {Iterable<string>} entries
 * @returns {string}
 */
export function formatBaseline(entries) {
  return JSON.stringify([...new Set(entries)].sort(), null, 2) + '\n'
}
