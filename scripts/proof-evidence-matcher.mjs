/**
 * Shared assertion matcher for TUI proof evidence (BOS-218).
 *
 * Two-layer API (decision D9):
 *   layer 1: matchExpectation(screenText, expectation)  — pure, per-string
 *   layer 2: evaluateExpectations({expectations, texts}) — journey-wide
 * Consumers: the scenario replay engine (BOS-219) and the live-agent gate
 * (BOS-222) both call layer 2 verbatim; scene attribution stays with callers.
 *
 * `normalized` collapses whitespace on BOTH sides but stays CASE-SENSITIVE
 * (D4/D6): whitespace is rendering noise, letter case is content.
 * `normalized-ci` is the explicit case-insensitive opt-in for LLM-authored
 * expectations (Epic 3). Regex runs against the RAW screen text with /u.
 */
export const MATCH_MODES = ['literal', 'normalized', 'normalized-ci', 'regex']

export function normalizeWhitespace(s) {
  return String(s).replace(/\s+/g, ' ').trim()
}

function normalizeAlternative(raw, where) {
  if (typeof raw === 'string') {
    if (!raw.trim()) throw new Error(`${where}: empty text`)
    return { text: raw, match: 'normalized' }
  }
  if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) {
    throw new Error(`${where}: expected string or object`)
  }
  const { text, match = 'normalized', ...rest } = raw
  const unknown = Object.keys(rest)
  if (unknown.length) throw new Error(`${where}: unknown fields: ${unknown.join(', ')}`)
  if (!MATCH_MODES.includes(match)) throw new Error(`${where}: unknown match mode "${match}"`)
  if (typeof text !== 'string' || !text.trim()) throw new Error(`${where}: needs text or anyOf`)
  if (match === 'regex') compileRegex(text, where)
  return { text, match }
}

function compileRegex(source, where) {
  try {
    return new RegExp(source, 'u')
  } catch (err) {
    throw new Error(`${where}: invalid regex: ${err.message}`)
  }
}

/**
 * Canonicalize a raw expectation (bare string shorthand, object, or anyOf
 * object) into {text?, match, label?, anyOf?}. Throws Error with a
 * pointer-free reason; callers (the scenario validator) prefix the pointer.
 */
export function normalizeExpectation(raw, where = 'expect') {
  if (typeof raw === 'string') return normalizeAlternative(raw, where)
  if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) {
    throw new Error(`${where}: expected string or object`)
  }
  const { text, match, label, anyOf, ...rest } = raw
  const unknown = Object.keys(rest)
  if (unknown.length) throw new Error(`${where}: unknown fields: ${unknown.join(', ')}`)
  if (label !== undefined && (typeof label !== 'string' || !label.trim())) {
    throw new Error(`${where}: label must be a non-empty string when present`)
  }
  const hasText = text !== undefined
  const hasAnyOf = anyOf !== undefined
  if (hasText === hasAnyOf) throw new Error(`${where}: needs exactly one of text or anyOf`)
  if (hasAnyOf) {
    if (!Array.isArray(anyOf) || anyOf.length === 0) {
      throw new Error(`${where}: anyOf must be a non-empty array`)
    }
    if (match !== undefined) throw new Error(`${where}: match belongs on anyOf entries`)
    const alts = anyOf.map((a, i) => normalizeAlternative(a, `${where}.anyOf[${i}]`))
    return label ? { anyOf: alts, label } : { anyOf: alts }
  }
  const single = normalizeAlternative({ text, ...(match !== undefined ? { match } : {}) }, where)
  return label ? { ...single, label } : single
}

function matchAlternative(screenText, { text, match }) {
  if (match === 'literal') return screenText.includes(text)
  if (match === 'normalized')
    return normalizeWhitespace(screenText).includes(normalizeWhitespace(text))
  if (match === 'normalized-ci') {
    return normalizeWhitespace(screenText)
      .toLowerCase()
      .includes(normalizeWhitespace(text).toLowerCase())
  }
  return compileRegex(text, 'regex').test(screenText)
}

/** Layer 1: does one settled screen satisfy one canonical expectation? */
export function matchExpectation(screenText, expectation) {
  const alts = expectation.anyOf ?? [expectation]
  return alts.some((alt) => matchAlternative(String(screenText), alt))
}

/** Human/LLM-readable display string for downstream consumers (D8). */
export function displayText(expectation) {
  if (expectation.label) return expectation.label
  if (expectation.anyOf) return expectation.anyOf.map((a) => a.text).join(' | ')
  return expectation.text
}

/**
 * Layer 2: journey-wide evaluation over one scene's ordered settled screens.
 * Same semantics as today's evaluateSceneEvidence (proof-tui-agent.mjs:484)
 * generalized to matcher modes: an expectation passes if ANY screen matches.
 * On failure the record embeds the scene's FINAL settled screen (lastText)
 * for debuggability — deliberately not a similarity-picked screen.
 */
export function evaluateExpectations({ expectations, texts }) {
  const lastText = texts.length ? String(texts[texts.length - 1]) : null
  const missing = (expectations ?? [])
    .filter((exp) => !texts.some((t) => matchExpectation(t, exp)))
    .map((expectation) => ({ expectation, displayText: displayText(expectation) }))
  return { passed: missing.length === 0, missing, lastText }
}
