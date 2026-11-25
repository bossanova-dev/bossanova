// Shared case- and separator-insensitive label identity helpers.
// Dependency-free: vendored into published skill cores alongside skill-config.mjs.

/**
 * Normalise a label or state name to its comparison key by lowercasing and stripping
 * whitespace, `-` and `_`. Null-safe (`null` and `undefined` yield `''`).
 *
 * @param {unknown} value
 * @returns {string}
 */
export function labelKey(value) {
  return String(value ?? '')
    .toLowerCase()
    .replace(/[\s_-]+/g, '')
}

/**
 * Find the first label in `existing` whose `labelKey` matches `name`, returning its exact
 * existing spelling, or `null` when `name` is non-string/blank or no existing label matches.
 * Each `existing` entry may be a label string or a `{ name: string }` object.
 *
 * @param {unknown} name
 * @param {readonly (string | { name?: unknown } | null | undefined)[] | null | undefined} existing
 * @returns {string | null}
 */
export function resolveLabelIdentity(name, existing) {
  if (typeof name !== 'string') return null
  const target = labelKey(name)
  if (target === '') return null
  if (!Array.isArray(existing)) return null
  for (const entry of existing) {
    const candidate =
      typeof entry === 'string'
        ? entry
        : entry && typeof entry === 'object' && typeof entry.name === 'string'
          ? entry.name
          : null
    if (candidate !== null && labelKey(candidate) === target) return candidate
  }
  return null
}
