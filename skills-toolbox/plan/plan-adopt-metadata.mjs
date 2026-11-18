/** @param {unknown} value @returns {string} */
export function canonical(value) {
  if (Array.isArray(value)) return `[${value.map(canonical).join(',')}]`
  if (value === null || typeof value !== 'object') return JSON.stringify(value) ?? 'null'
  const object = /** @type {Record<string, unknown>} */ (value)
  const keys = Object.keys(object).sort()
  return `{${keys.map((key) => `${JSON.stringify(key)}:${canonical(object[key])}`).join(',')}}`
}

/** @param {string} code @param {string} field @param {string} message */
function entry(code, field, message) {
  return { code, field, message: `plan-run-guards: ${message}` }
}

const escapedCharacters = /&(?:lt|gt|amp|quot|apos|#(?:60|62|38|34|39)|#[xX](?:3[cCeE]|26|22|27));/g
/** @type {Record<string, string>} */
const namedCharacters = { lt: '<', gt: '>', amp: '&', quot: '"', apos: "'" }

/**
 * Decode one transport escape round in string values, preserving keys and literal entities.
 * @param {unknown} value
 * @returns {{value: unknown, decoded: string[]}}
 */
export function decodeReturnTransport(value) {
  /** @type {string[]} */
  const decoded = []
  /** @param {unknown} current @param {string} field @returns {unknown} */
  function walk(current, field) {
    if (typeof current === 'string') {
      const text = current.replace(escapedCharacters, (entity) => {
        const reference = entity.slice(1, -1)
        if (!reference.startsWith('#')) return namedCharacters[reference]
        const hex = reference[1].toLowerCase() === 'x'
        return String.fromCharCode(Number.parseInt(reference.slice(hex ? 2 : 1), hex ? 16 : 10))
      })
      if (text !== current) decoded.push(field)
      return text
    }
    if (Array.isArray(current))
      return current.map((item, index) => walk(item, `${field}[${index}]`))
    if (
      current !== null &&
      typeof current === 'object' &&
      (Object.getPrototypeOf(current) === Object.prototype ||
        Object.getPrototypeOf(current) === null)
    ) {
      return Object.fromEntries(
        Object.entries(current).map(([key, item]) => [
          key,
          walk(item, field ? `${field}.${key}` : key),
        ]),
      )
    }
    return current
  }
  return { value: walk(value, ''), decoded }
}

/**
 * Always validate the object the dispatch returned, never a same-named worker-written file.
 * A diverged file is replaced and reported as a warning.
 * @param {{returned?: unknown, onDisk?: unknown}} input
 * @returns {{ok: boolean, adopted: unknown, state: 'absent'|'identical'|'diverged', violations: object[], warnings?: object[]}}
 */
export function adoptReturnedMetadata({ returned, onDisk } = {}) {
  if (returned === undefined || returned === null || typeof returned !== 'object') {
    return {
      ok: false,
      adopted: returned,
      state: 'absent',
      violations: [
        entry(
          'metadata-not-returned',
          'returned',
          'the dispatch returned no metadata object — artifact readiness is not message ' +
            'readiness, so there is nothing here to validate',
        ),
      ],
    }
  }
  const { value: adopted, decoded } = decodeReturnTransport(returned)
  const warnings = decoded.length
    ? [
        entry(
          'returned-entities-decoded',
          'returned',
          `decoded transport escaping in ${decoded.join(', ')}`,
        ),
      ]
    : []
  const state =
    onDisk === undefined
      ? 'absent'
      : canonical(onDisk) === canonical(adopted)
        ? 'identical'
        : 'diverged'
  if (state === 'diverged') {
    warnings.push(
      entry(
        'metadata-file-overwritten',
        'draft-metadata',
        'the draft-metadata file held bytes that were not the returned object; replaced with ' +
          'the returned object',
      ),
    )
  }
  return { ok: true, adopted, state, violations: [], ...(warnings.length ? { warnings } : {}) }
}
