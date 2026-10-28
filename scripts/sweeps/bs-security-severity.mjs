// Shared severity, confidence and outcome vocabulary for security sweep records.

// Severity and confidence.

const SEVERITY_RANK = { info: 0, low: 1, medium: 2, high: 3, critical: 4 }
const SEVERITY_WORDS = {
  info: 'info',
  informational: 'info',
  low: 'low',
  medium: 'medium',
  moderate: 'medium',
  high: 'high',
  critical: 'critical',
}

const CONFIDENCE_RANK = {
  speculative: 0,
  plausible: 1,
  likely: 2,
  'confirmed-by-reading': 3,
  'confirmed-by-test': 4,
}
const CONFIDENCE_ALIASES = { confirmed: 'confirmed-by-reading' }

const stripMarkdown = (s) => String(s ?? '').replace(/[*_`]/g, '')

/**
 * Highest severity word before any parenthetical. An unparseable value becomes
 * `high` with severityUnparsed, so it reaches the verifier instead of slipping past.
 */
export function normaliseSeverity(raw) {
  const head = stripMarkdown(raw).toLowerCase().split('(')[0]
  let best = null
  for (const [word] of head.matchAll(/[a-z]+/g)) {
    const sev = SEVERITY_WORDS[word]
    if (sev && (best === null || SEVERITY_RANK[sev] > SEVERITY_RANK[best])) best = sev
  }
  return best === null
    ? { severity: 'high', severityUnparsed: true }
    : { severity: best, severityUnparsed: false }
}

/** Leading token of the confidence value; anything unknown is `speculative`. */
export function normaliseConfidence(raw) {
  const token =
    stripMarkdown(raw)
      .trim()
      .toLowerCase()
      .match(/^[a-z][a-z-]*/)?.[0] ?? ''
  const value = CONFIDENCE_ALIASES[token] ?? token
  return Object.hasOwn(CONFIDENCE_RANK, value) ? value : 'speculative'
}

export const severityRank = (s) => (s === null ? -1 : (SEVERITY_RANK[s] ?? SEVERITY_RANK.high))
export const confidenceRank = (c) => CONFIDENCE_RANK[c] ?? 0

/** Only an explicit lead opts out of the fileable finding outcome. */
export function normaliseOutcome(raw) {
  return String(raw ?? '')
    .trim()
    .toLowerCase() === 'needs_validation'
    ? 'needs_validation'
    : 'finding'
}
