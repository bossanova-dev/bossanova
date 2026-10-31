/**
 * @typedef {{area: string, label: string, defaultAsk: 'always'|'unless-proven'|'never', paths?: readonly string[], source?: string, bumps?: readonly {name: string, from: string, to: string}[]}} ConfidenceArea
 * @typedef {{blockers?: readonly {code: string, label: string}[], evidence?: {review?: 'ran'|'missing', collectors?: readonly {name: string, ok: boolean}[]}, areas?: readonly ConfidenceArea[], size?: {changedLines: number, baseline: {ok: true, thresholds: {p90: number, p99: number}|null}|{ok: false, error: string}}, proof?: {ui?: 'proven'|'unproven'|'none', behavior?: 'proven'|'unproven'|'none'}, secondModel?: {ran: boolean, reason?: string}}} ConfidenceFacts
 * @typedef {'high'|'moderate'|'low'|'blocked'|'cannot-assess'} ConfidenceLevel
 * @typedef {{level: ConfidenceLevel, asks: {area: string, label: string, source: string}[], reasons: string[], loweredBy: string[], health: string[]}} ConfidenceResult
 */

export const LEVELS = Object.freeze(['high', 'moderate', 'low'])
export const STATES = Object.freeze(['blocked', 'cannot-assess'])
export const MAX_REASONS = 3
export const MAX_ASKS_BEFORE_LOW = 3

/** Nearest-rank percentiles of merged PR changed-line counts.
 * @param {readonly unknown[]} changedLineCounts
 * @param {{minSample?: number}} [options]
 * @returns {{p90: number, p99: number, sample: number}|null}
 */
export function sizeThresholds(changedLineCounts, { minSample = 30 } = {}) {
  const values = changedLineCounts
    .filter(
      /** @returns {value is number} */
      (value) => typeof value === 'number' && Number.isInteger(value) && value >= 0,
    )
    .sort((a, b) => a - b)
  if (values.length === 0 || values.length < minSample) return null
  return {
    p90: values[Math.ceil(values.length * 0.9) - 1],
    p99: values[Math.ceil(values.length * 0.99) - 1],
    sample: values.length,
  }
}

/** Explain a head's confidence from facts; no approval or model self-rating participates.
 * @param {ConfidenceFacts} facts
 * @returns {ConfidenceResult}
 */
export function confidence(facts) {
  const areas = facts.areas ?? []
  /** @type {ConfidenceResult['asks']} */
  const asks = []
  const seen = new Set()
  /** @type {Map<string, ConfidenceArea>} */
  const areaById = new Map()
  for (const entry of areas) {
    if (
      entry.defaultAsk !== 'always' &&
      !(entry.defaultAsk === 'unless-proven' && facts.proof?.ui !== 'proven')
    )
      continue
    if (seen.has(entry.area)) continue
    seen.add(entry.area)
    areaById.set(entry.area, entry)
    asks.push({ area: entry.area, label: entry.label, source: entry.source ?? 'builtin' })
  }
  const areaAskCount = asks.length
  const uiUnproven = areas.some((entry) => entry.area === 'ui') && facts.proof?.ui !== 'proven'
  const behaviorUnproven = facts.proof?.behavior === 'unproven'
  if (behaviorUnproven && !seen.has('unproven-behavior')) {
    asks.push({ area: 'unproven-behavior', label: 'Unproven behavior', source: 'proof' })
  }
  /** @type {string[]} */
  const health = []
  const baseline = facts.size?.baseline
  if (baseline?.ok === true && baseline.thresholds === null) health.push('size-baseline-thin')
  if (facts.secondModel?.ran === false) health.push('second-model-skipped')
  /** @param {ConfidenceLevel} level @param {string[]} loweredBy @param {string[]} reasons @returns {ConfidenceResult} */
  const result = (level, loweredBy, reasons) => ({
    level,
    asks,
    loweredBy,
    reasons: [...new Set(reasons)].slice(0, MAX_REASONS),
    health,
  })
  if (facts.blockers?.length) {
    return result(
      'blocked',
      ['blocker'],
      facts.blockers.map((b) => `${b.label}. Resolving it would raise this.`),
    )
  }
  const missing = []
  if (!facts.evidence) missing.push('Evidence is missing. Collecting it would raise this.')
  if (facts.evidence && facts.evidence.review !== 'ran')
    missing.push('The review did not run. Rerunning it would raise this.')
  for (const collector of facts.evidence?.collectors ?? []) {
    if (collector.ok !== true)
      missing.push(`${collector.name} could not be collected. Collecting it would raise this.`)
  }
  const changedLines = facts.size?.changedLines
  if (!Number.isSafeInteger(changedLines) || changedLines < 0)
    missing.push('The changed-line count is missing. Counting it would raise this.')
  if (baseline?.ok !== true)
    missing.push('The size baseline could not be read. Reading it would raise this.')
  if (missing.length) return result('cannot-assess', ['evidence-missing'], missing)

  /** @type {{code: string, rank: number, reasons: string[]}[]} */
  const caps = []
  if (areaAskCount) {
    caps.push({
      code: 'area-asks',
      rank: 1,
      reasons: asks.slice(0, areaAskCount).map((ask) => {
        const entry = areaById.get(ask.area)
        const bump = entry?.bumps?.[0]
        if (bump)
          return `Bumps ${bump.name} ${bump.from} → ${bump.to}, a major version. Reviewing the bump would raise this.`
        return entry?.source === 'config'
          ? `Matches your review area "${ask.label}". Reviewing this area would raise this.`
          : `Changes ${ask.label}. Reviewing this area would raise this.`
      }),
    })
  }
  const thresholds = baseline.ok ? baseline.thresholds : null
  const lines = changedLines.toLocaleString('en-US')
  if (thresholds && changedLines > thresholds.p90)
    caps.push({
      code: 'size-p90',
      rank: 1,
      reasons: [
        `Changes ${lines} lines, more than 90% of merged PRs here. Splitting it would raise this.`,
      ],
    })
  if (thresholds && changedLines > thresholds.p99)
    caps.push({
      code: 'size-p99',
      rank: 2,
      reasons: [
        `Changes ${lines} lines, more than 99% of merged PRs here. Splitting it would raise this.`,
      ],
    })
  if (asks.length > MAX_ASKS_BEFORE_LOW)
    caps.push({
      code: 'asks-over-3',
      rank: 2,
      reasons: [`Asks about ${asks.length} areas. Splitting the change would raise this.`],
    })
  if (uiUnproven || behaviorUnproven)
    caps.push({
      code: 'unproven',
      rank: 1,
      reasons: [
        ...(uiUnproven ? ['The UI change is unproven. A proof recording would raise this.'] : []),
        ...(behaviorUnproven
          ? ['The behavior change is unproven. Proving it would raise this.']
          : []),
      ],
    })
  const rank = Math.max(0, ...caps.map((cap) => cap.rank))
  const reasons = [
    ...caps.filter((cap) => cap.rank === rank),
    ...caps.filter((cap) => cap.rank !== rank),
  ].flatMap((cap) => cap.reasons)
  return result(
    rank === 2 ? 'low' : rank === 1 ? 'moderate' : 'high',
    caps.map((cap) => cap.code),
    reasons,
  )
}
