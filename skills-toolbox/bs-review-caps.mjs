// Single source of truth for the review-round cap and the byte-stable
// `bs-review clean:` / `bs-review capped:` sentinels shared by the `bs-review`
// skill and `bs-implement`'s Step 6 review loop.
//
// The round cap is env-configurable but **lower-only**: `BS_REVIEW_MAX_ROUNDS`
// may only *reduce* the cap below the hard default, never raise it — a
// pathological session must never be able to grant itself more review rounds.
// Any invalid / absent / too-high value falls back to the default. This mirrors
// wondercanvas' `WC_AUTO_REVIEW_MAX_ROUNDS` contract.
//
// The sentinel prefixes are the contract downstream callers route on
// (`bs-implement` Step 6c). They are pinned byte-for-byte by
// `bs-review-caps.test.mjs`; keep them byte-identical.
//
// Node built-ins only — cron worktrees are dependency-free.

// The hard default (and ceiling) for the review-round cap. The env may lower
// this but never raise it.
export const DEFAULT_REVIEW_MAX_ROUNDS = 3

/**
 * Resolve an effective round cap from a raw env value, clamped **lower-only**
 * against `defaultCap`: a strict positive integer in `[1, defaultCap]` is
 * honored; anything else — non-integer, `< 1`, `> defaultCap`, absent, empty —
 * falls back to `defaultCap`. The env can therefore only *lower* the cap.
 * @param {string|number|null|undefined} raw
 * @param {number} [defaultCap]
 * @returns {number}
 */
export function resolveMaxRounds(raw, defaultCap = DEFAULT_REVIEW_MAX_ROUNDS) {
  if (raw === null || raw === undefined) return defaultCap
  const trimmed = String(raw).trim()
  // Strict base-10 integer only: rejects '', signs, decimals, hex, exponents.
  if (!/^\d+$/.test(trimmed)) return defaultCap
  const n = Number.parseInt(trimmed, 10)
  // Lower-only clamp: never below 1, never above the default ceiling.
  if (n < 1 || n > defaultCap) return defaultCap
  return n
}

/**
 * The effective review-round cap, reading `BS_REVIEW_MAX_ROUNDS` from `env`.
 * @param {Record<string, string|undefined>} [env]
 * @returns {number}
 */
export function reviewMaxRounds(env = process.env) {
  return resolveMaxRounds(env.BS_REVIEW_MAX_ROUNDS, DEFAULT_REVIEW_MAX_ROUNDS)
}

// The hard default and ceiling for dispatched round-role review passes. This
// cap is separate from the fix-round cap above: it limits how many read-only
// round-role dispatches a review run may issue, while the guaranteed whole-
// branch round is always admitted.
export const DEFAULT_REVIEW_MAX_DISPATCHED_ROUNDS = 6

/**
 * Resolve an effective dispatched-round cap from a raw value, clamped
 * **lower-only** against `defaultCap`: a strict positive integer in
 * `[1, defaultCap]` is honored; anything else falls back to `defaultCap`.
 * @param {string|number|null|undefined} raw
 * @param {number} [defaultCap]
 * @returns {number}
 */
export function resolveMaxDispatchedRounds(raw, defaultCap = DEFAULT_REVIEW_MAX_DISPATCHED_ROUNDS) {
  if (raw === null || raw === undefined) return defaultCap
  const trimmed = String(raw).trim()
  if (!/^\d+$/.test(trimmed)) return defaultCap
  const n = Number.parseInt(trimmed, 10)
  if (n < 1 || n > defaultCap) return defaultCap
  return n
}

/**
 * The effective dispatched-round cap, reading
 * `BS_REVIEW_MAX_DISPATCHED_ROUNDS` from `env`.
 * @param {Record<string, string|undefined>} [env]
 * @returns {number}
 */
export function reviewMaxDispatchedRounds(env = process.env) {
  return resolveMaxDispatchedRounds(
    env.BS_REVIEW_MAX_DISPATCHED_ROUNDS,
    DEFAULT_REVIEW_MAX_DISPATCHED_ROUNDS,
  )
}

// Sentinel prefixes downstream callers route on — byte-identical contract.
export const CLEAN_PREFIX = 'bs-review clean:'
export const CAPPED_PREFIX = 'bs-review capped:'

/** The clean sentinel (no open must-fix findings). Byte-identical. */
export function cleanSentinel() {
  return `${CLEAN_PREFIX} no open must-fix findings.`
}

/**
 * The capped sentinel — a fixed prefix with only the round-count tail dynamic.
 *
 * The wording names BOTH things that cap a run. A run can cap with zero open
 * must-fix findings when unrepaired `invalid` entries are the only blocker, and
 * the old "open must-fix findings remain" text asserted the wrong reason there.
 * Routing is unaffected: callers match on `CAPPED_PREFIX` and `matchSentinel`
 * parses only the `after N rounds.` tail.
 *
 * @param {number} rounds
 * @returns {string}
 */
export function cappedSentinel(rounds) {
  return `${CAPPED_PREFIX} unresolved must-fix findings or invalid evidence remain after ${rounds} rounds.`
}

export function coverageCappedSentinel(rounds) {
  return `${CAPPED_PREFIX} review coverage completed zero discovered reviewers after ${rounds} rounds.`
}

/**
 * Classify a printed sentinel line. Routing matches on the fixed prefix; for a
 * capped line the trailing round count is extracted when present.
 * @param {string} line
 * @returns {{status:'clean'}|{status:'capped',rounds:number|null}|null}
 */
export function matchSentinel(line) {
  if (typeof line !== 'string') return null
  const s = line.trim()
  if (s.startsWith(CLEAN_PREFIX)) return { status: 'clean' }
  if (s.startsWith(CAPPED_PREFIX)) {
    const m = s.match(/after\s+([1-9]\d*)\s+rounds?\./)
    return m ? { status: 'capped', rounds: Number.parseInt(m[1], 10) } : null
  }
  return null
}

/**
 * Derive the terminal review verdict from report evidence. This intentionally
 * fails closed: a clean verdict requires readable counts for every blocker.
 * @param {unknown} evidence
 * @returns {{status:'clean'|'capped', reasons:string[]}}
 */
export function reviewVerdict(evidence = undefined) {
  if (!evidence || typeof evidence !== 'object' || Array.isArray(evidence)) {
    return { status: 'capped', reasons: ['unreadable-evidence'] }
  }
  const unresolved = evidence.mustfix?.unresolved
  const invalid = evidence.invalid
  if (!Number.isInteger(unresolved) || unresolved < 0 || !Array.isArray(invalid)) {
    return { status: 'capped', reasons: ['unreadable-evidence'] }
  }
  const ledger = evidence.ledger
  if (!validLedgerCoverage(ledger)) {
    return { status: 'capped', reasons: ['unreadable-ledger'] }
  }
  const reasons = []
  if (unresolved > 0) reasons.push('unresolved-mustfix')
  if (invalid.length > 0) reasons.push('invalid-evidence')
  if (ledger.discovered > 0 && ledger.completed === 0) reasons.push('no-coverage')
  return reasons.length ? { status: 'capped', reasons } : { status: 'clean', reasons: [] }
}

/**
 * The closed per-finding disposition vocabulary a capped verdict discloses.
 *
 * `repaired-unconfirmed` is the distinction the census exists for: a repair that
 * landed with every gate green but ran out of rounds to CONFIRM is not the same
 * news as a defect left standing, and a bare `capped` line cannot tell a reader
 * which one it is holding.
 */
export const DISPOSITIONS = Object.freeze(['fixed', 'refuted', 'repaired-unconfirmed', 'open'])

/**
 * The producer's spelling for each census bucket.
 *
 * `bs-review-report.mjs` and the report shape in `boss-review`'s Phase 7 both
 * write `'fixed' | 'verified' | 'unresolved'`, which overlaps the vocabulary
 * above on `fixed` ALONE. Without this map the fail-closed arm below folds
 * `verified` — a finding a confirming round positively settled — into `open`,
 * the one number a reader of a capped verdict treats as defects left standing,
 * so the census would over-report exactly the runs it exists to explain.
 *
 * Aliases only, never new buckets: the four names in `DISPOSITIONS` stay the
 * closed disclosure vocabulary, and a value outside both sets is still `open`.
 */
const DISPOSITION_ALIASES = Object.freeze({ verified: 'refuted', unresolved: 'open' })

/**
 * The finding records a report carries, wherever it keeps them. `mustfix.items`
 * is the shape `reviewConfidence` already reads; `findings` is accepted as the
 * flatter alternative so the census does not silently read zero against a report
 * that spells it the other way.
 */
function reportFindings(evidence) {
  if (!evidence || typeof evidence !== 'object' || Array.isArray(evidence)) return []
  if (Array.isArray(evidence.mustfix?.items)) return evidence.mustfix.items
  if (Array.isArray(evidence.findings)) return evidence.findings
  return []
}

/**
 * Count the report's own finding records by disposition.
 *
 * Fails closed in the same direction every other counter here does: a record
 * with no disposition field, or one carrying a value outside the closed set,
 * counts as `open`. An unreadable disposition is not evidence of a repair, and
 * a report whose findings carry no disposition at all yields an all-`open`
 * census — today's behaviour, which is what keeps this backward-compatible.
 *
 * @param {unknown} evidence
 * @returns {{fixed:number,refuted:number,'repaired-unconfirmed':number,open:number}}
 */
export function dispositionCensus(evidence = undefined) {
  const census = { fixed: 0, refuted: 0, 'repaired-unconfirmed': 0, open: 0 }
  for (const record of reportFindings(evidence)) {
    if (!record || typeof record !== 'object' || Array.isArray(record)) {
      census.open += 1
      continue
    }
    const raw = record.disposition ?? record.status
    const key = typeof raw === 'string' ? (DISPOSITION_ALIASES[raw] ?? raw) : raw
    census[typeof key === 'string' && DISPOSITIONS.includes(key) ? key : 'open'] += 1
  }
  return census
}

function validLedgerCoverage(ledger) {
  if (!ledger || typeof ledger !== 'object' || Array.isArray(ledger)) return false
  const keys = ['discovered', 'completed', 'skipped', 'timedOut', 'notReached']
  if (!keys.every((key) => Number.isInteger(ledger[key]) && ledger[key] >= 0)) return false
  return (
    ledger.discovered === ledger.completed + ledger.skipped + ledger.timedOut + ledger.notReached
  )
}

function asStringArray(value) {
  return Array.isArray(value) ? value.filter((item) => typeof item === 'string' && item.length) : []
}

function findingKey(finding) {
  if (!finding || typeof finding !== 'object' || Array.isArray(finding)) return ''
  if (typeof finding.id === 'string' && finding.id.length) return `id:${finding.id}`
  if (typeof finding.key === 'string' && finding.key.length) return `key:${finding.key}`
  const file = typeof finding.file === 'string' ? finding.file : ''
  const line = Number.isInteger(finding.line) ? finding.line : finding.line === null ? 'null' : ''
  const title = typeof finding.title === 'string' ? finding.title : ''
  return JSON.stringify([file, line, title])
}

function roundMustFix(round) {
  if (!round || typeof round !== 'object' || Array.isArray(round)) return null
  const candidates = [round.mustFix, round.mustfix, round.mustfix?.items, round.mustFix?.items]
  for (const candidate of candidates) {
    if (Array.isArray(candidate)) return candidate
  }
  return null
}

function dispositionSet(items) {
  const set = new Set()
  for (const item of Array.isArray(items) ? items : []) {
    const key = findingKey(item)
    if (key) set.add(key)
  }
  return set
}

function oscillationFindingKey(finding) {
  if (!finding || typeof finding !== 'object' || Array.isArray(finding)) return ''
  const file = typeof finding.file === 'string' ? finding.file : ''
  const title = typeof finding.title === 'string' ? finding.title : ''
  const line = Number.isInteger(finding.line) ? finding.line : finding.line === null ? 'null' : ''
  if (!file || !title || line === '') return ''
  return JSON.stringify([file, line, title])
}

function oscillationDispositionSet(dispositions, field) {
  const set = new Set()
  const reasons = []
  const add = (items) => {
    if (items === undefined) return
    if (!Array.isArray(items)) {
      reasons.push(`${field} dispositions must be an array`)
      return
    }
    for (const item of items) {
      const key = oscillationFindingKey(item)
      if (key) {
        set.add(key)
      } else {
        reasons.push(`malformed ${field} disposition`)
      }
    }
  }
  if (Array.isArray(dispositions)) {
    const matching = []
    for (const item of dispositions) {
      if (!item || typeof item !== 'object' || Array.isArray(item)) {
        reasons.push('malformed disposition')
        continue
      }
      const disposition = item.disposition ?? item.status
      if (typeof disposition !== 'string') {
        reasons.push('malformed disposition')
        continue
      }
      if (disposition === field) matching.push(item)
    }
    add(matching)
    return { set, reasons }
  }
  if (dispositions === null || dispositions === undefined) {
    return { set, reasons }
  }
  if (typeof dispositions !== 'object') {
    reasons.push('dispositions must be an object or array')
    return { set, reasons }
  }
  add(dispositions[field])
  return { set, reasons }
}

/**
 * Find must-fix findings that persisted across two consecutive rounds. Identity
 * is the review-loop ledger tuple encoded as JSON: `[file,line,title]`.
 * @param {object} input
 * @param {unknown} input.previousRound
 * @param {unknown} input.currentRound
 * @param {unknown} input.dispositions
 * @returns {{oscillating:string[],reasons:string[]}}
 */
export function classifyOscillation({
  previousRound = undefined,
  currentRound = undefined,
  dispositions = {},
} = {}) {
  const reasons = []
  const previous = roundMustFix(previousRound)
  const current = roundMustFix(currentRound)
  if (!previous || !current) {
    return { oscillating: [], reasons: ['rounds must be objects'] }
  }

  const currentKeys = new Set()
  for (const finding of current) {
    const key = oscillationFindingKey(finding)
    if (key) {
      currentKeys.add(key)
    } else if (!reasons.includes('malformed finding')) {
      reasons.push('malformed finding')
    }
  }
  const fixedResult = oscillationDispositionSet(dispositions, 'fixed')
  const verifiedResult = oscillationDispositionSet(dispositions, 'verified')
  reasons.push(...fixedResult.reasons, ...verifiedResult.reasons)
  const oscillating = []
  const seen = new Set()
  for (const finding of previous) {
    const key = oscillationFindingKey(finding)
    if (!key) {
      if (!reasons.includes('malformed finding')) reasons.push('malformed finding')
      continue
    }
    if (!currentKeys.has(key) || seen.has(key)) continue
    seen.add(key)
    oscillating.push(key)
  }
  return { oscillating, reasons: [...new Set(reasons)] }
}

/**
 * Find must-fix findings that disappeared between consecutive rounds without
 * being recorded as fixed or leave-as-is.
 * @param {unknown} history
 * @returns {{ok:true, findings: object[]}|{ok:false, reason:string, findings: []}}
 */
export function vanishedFindings(history = {}) {
  if (!history || typeof history !== 'object' || Array.isArray(history)) {
    return { ok: false, reason: 'history must be an object', findings: [] }
  }
  if (!Array.isArray(history.rounds)) {
    return { ok: false, reason: 'history rounds must be an array', findings: [] }
  }
  const fixed = dispositionSet(history.fixed)
  const leaveAsIs = dispositionSet(history.leaveAsIs)
  const vanished = []
  const seen = new Set()
  for (let index = 0; index < history.rounds.length - 1; index += 1) {
    const current = roundMustFix(history.rounds[index])
    const next = roundMustFix(history.rounds[index + 1])
    if (!current || !next) {
      return { ok: false, reason: 'history round must be an object', findings: [] }
    }
    const nextKeys = new Set(next.map(findingKey).filter(Boolean))
    for (const finding of current) {
      const key = findingKey(finding)
      if (!key || nextKeys.has(key) || fixed.has(key) || leaveAsIs.has(key) || seen.has(key)) {
        continue
      }
      seen.add(key)
      vanished.push(finding)
    }
  }
  return { ok: true, findings: vanished }
}

function panelEvidence(panel = {}) {
  if (!panel || typeof panel !== 'object' || Array.isArray(panel)) return null
  const terminalPanel = asStringArray(panel.reviewers)
  if (!terminalPanel.length) return null
  const initialPanel = asStringArray(panel.initial ?? panel.initialReviewers)
  return {
    terminalPanel,
    initialPanel: initialPanel.length ? initialPanel : terminalPanel,
  }
}

/**
 * Derive panel agreement evidence from collected review output.
 * @param {unknown} evidence
 * @returns {object}
 */
export function reviewAgreement(evidence = {}) {
  const panel = panelEvidence(evidence?.panel)
  if (!panel) {
    return {
      ok: false,
      reason: 'unreadable-panel-evidence',
      panelSize: 0,
      initialPanelSize: 0,
      terminalPanel: [],
      initialPanel: [],
      panelShrank: false,
      uncorroboratedMustFixCount: 0,
      vanishedFindings: [],
    }
  }
  const items = Array.isArray(evidence?.mustfix?.items) ? evidence.mustfix.items : []
  const uncorroboratedMustFixCount =
    panel.terminalPanel.length >= 2
      ? items.filter((item) => Number.isInteger(item?.reviewerCount) && item.reviewerCount === 1)
          .length
      : 0
  const vanished = evidence?.history
    ? vanishedFindings(evidence.history)
    : { ok: true, findings: [] }
  if (!vanished.ok) {
    return {
      ok: false,
      reason: vanished.reason,
      panelSize: panel.terminalPanel.length,
      initialPanelSize: panel.initialPanel.length,
      terminalPanel: panel.terminalPanel,
      initialPanel: panel.initialPanel,
      panelShrank: panel.terminalPanel.length < panel.initialPanel.length,
      uncorroboratedMustFixCount,
      vanishedFindings: [],
    }
  }
  return {
    ok: true,
    panelSize: panel.terminalPanel.length,
    initialPanelSize: panel.initialPanel.length,
    terminalPanel: panel.terminalPanel,
    initialPanel: panel.initialPanel,
    panelShrank: panel.terminalPanel.length < panel.initialPanel.length,
    uncorroboratedMustFixCount,
    vanishedFindings: vanished.findings,
  }
}

function agreementEvidence(agreement) {
  if (!agreement || typeof agreement !== 'object' || Array.isArray(agreement)) return null
  if (!Number.isInteger(agreement.panelSize) || agreement.panelSize < 0) return null
  return {
    ok: agreement.ok !== false,
    reason: typeof agreement.reason === 'string' ? agreement.reason : undefined,
    panelSize: agreement.panelSize,
    initialPanelSize: Number.isInteger(agreement.initialPanelSize)
      ? agreement.initialPanelSize
      : agreement.panelSize,
    terminalPanel: asStringArray(agreement.terminalPanel),
    initialPanel: asStringArray(agreement.initialPanel),
    panelShrank: agreement.panelShrank === true,
    uncorroboratedMustFixCount: Number.isInteger(agreement.uncorroboratedMustFixCount)
      ? agreement.uncorroboratedMustFixCount
      : 0,
    vanishedFindings: Array.isArray(agreement.vanishedFindings) ? agreement.vanishedFindings : [],
  }
}

/**
 * Derive review confidence from panel and agreement evidence.
 * @param {unknown} evidence
 * @returns {{grade:'High'|'Medium'|'Low', reasons:string[]}}
 */
export function reviewConfidence(evidence = {}) {
  const reasons = []
  const agreement = agreementEvidence(evidence?.agreement) ?? reviewAgreement(evidence)
  if (!agreement.ok && agreement.reason === 'unreadable-panel-evidence') {
    reasons.push('unreadable-panel-evidence')
  } else if (!agreement.ok) {
    reasons.push('unreadable-vanished-history')
  }
  if (agreement.ok && agreement.panelSize < 2) reasons.push('single-sample-panel')
  if (evidence?.capped === true || evidence?.status === 'capped') reasons.push('round-cap-hit')
  if (Number.isInteger(evidence?.mustfix?.unresolved) && evidence.mustfix.unresolved > 0) {
    reasons.push('unresolved-mustfix')
  }
  if (Array.isArray(evidence?.invalid) && evidence.invalid.length > 0)
    reasons.push('invalid-evidence')
  const ledger = evidence?.ledger
  if (ledger && typeof ledger === 'object' && !Array.isArray(ledger)) {
    if (Number.isInteger(ledger.notReached) && ledger.notReached > 0)
      reasons.push('not-reached-reviewer')
    if (Number.isInteger(ledger.timedOut) && ledger.timedOut > 0) reasons.push('timed-out-reviewer')
  }
  if (agreement.ok && agreement.vanishedFindings.length > 0) reasons.push('vanished-finding')

  const lowReasons = new Set([
    'unreadable-panel-evidence',
    'unreadable-vanished-history',
    'single-sample-panel',
    'round-cap-hit',
    'unresolved-mustfix',
    'invalid-evidence',
    'not-reached-reviewer',
    'timed-out-reviewer',
    'vanished-finding',
  ])
  const low = reasons.filter((reason) => lowReasons.has(reason))
  if (low.length) return { grade: 'Low', reasons: low }
  if (agreement.panelShrank || agreement.uncorroboratedMustFixCount > 0) {
    const medium = []
    if (agreement.panelShrank) medium.push('panel-shrank')
    if (agreement.uncorroboratedMustFixCount > 0) medium.push('uncorroborated-mustfix')
    return { grade: 'Medium', reasons: medium }
  }
  return { grade: 'High', reasons: [] }
}

/**
 * Classify the sentinel evidence present in a whole report/transcript.
 * Missing and ambiguous are distinct non-clean outcomes for callers that need
 * to fail closed without changing the byte-stable single-line matcher.
 * @param {unknown} text
 * @returns {{status:'clean'}|{status:'capped',rounds:number|null}|{status:'missing'}|{status:'ambiguous'}}
 */
export function classifySentinels(text) {
  if (typeof text !== 'string' || text.length === 0) return { status: 'missing' }
  const matches = []
  for (const line of text.split(/\r?\n/)) {
    const hasClean = line.includes(CLEAN_PREFIX)
    const hasCapped = line.includes(CAPPED_PREFIX)
    if (hasClean && hasCapped) return { status: 'ambiguous' }
    if (!hasClean && !hasCapped) continue
    const classified = matchSentinel(line)
    if (!classified) return { status: 'ambiguous' }
    matches.push(classified)
  }
  if (matches.length === 0) return { status: 'missing' }
  if (matches.length !== 1) return { status: 'ambiguous' }
  return matches[0]
}

// ---------------------------------------------------------------------------
// Fix-round admission — the deadline gate plus the must-fix override.
// ---------------------------------------------------------------------------
//
// A caller's wall-clock deadline is priced per leg: a fix round is admitted only
// when its WHOLE allowance still remains. That is the right overrun policy and
// the wrong value policy — it refuses a round that would have closed a located,
// never-attempted must-fix, and the run's only honest terminal state then names
// the clock rather than the finding.
//
// `admitFixRound` is the decision table for that admission. It is **pure**: it
// reads no clock and no env, so the caller passes the remainder it re-derived
// from `date +%s` at the boundary. The override is bounded twice over — by
// `MUSTFIX_OVERRUN_ROUNDS` (one extra round, never more) and by the round cap,
// which is evaluated FIRST and is never overridden. Overriding the cap would
// break `resolveMaxRounds`' lower-only contract above: a pathological session
// must never be able to grant itself more review rounds.

/** The price of one fix→confirm round in seconds — `FIX_ROUND_MINUTES * 60` in the skill prose. */
export const DEFAULT_FIX_ROUND_SECONDS = 1200

/**
 * How many rounds the must-fix override may admit past the deadline, in total.
 * One: the whole allowance then sits inside the caller's post-review reserve,
 * so an override round is self-sufficient and needs no second absolute bound
 * threaded across the skill boundary.
 */
export const MUSTFIX_OVERRUN_ROUNDS = 1

/**
 * The overrun allowance expressed in seconds — the REPORTED total for the run's
 * overrun ledger field, not a gate input. The gate compares round counts.
 */
export const MUSTFIX_OVERRUN_SECONDS = MUSTFIX_OVERRUN_ROUNDS * DEFAULT_FIX_ROUND_SECONDS

/**
 * How many rounds are reserved for a must-fix the pass ITSELF caused, in total.
 *
 * Distinct from `MUSTFIX_OVERRUN_ROUNDS` and bounded independently of it. A
 * `Critical` raised in round 2 against a fix round 1 landed competes for the
 * same single general overrun that round 1 already spent, so without a reserve
 * the pass cannot lawfully repair the regression it introduced and returns
 * `overrun-exhausted` — a lawful-looking terminal state over a defect the
 * review itself created.
 *
 * One, for the same reason the general overrun is one: the allowance sits
 * inside the caller's post-review reserve, so it needs no second absolute bound
 * threaded across the skill boundary. The bound is also what keeps
 * over-reporting `selfInflictedMustFix` non-catastrophic — it can buy one round
 * for the whole pass, never one per round.
 */
export const RESERVED_REGRESSION_ROUNDS = 1

export const FUNDING_STARVED = 'funding-starved'

/**
 * The pricing call itself failed or its output did not parse. Distinct from
 * `FUNDING_STARVED` on purpose: a caller that could not price its allowance must
 * not be byte-identical to one that priced it and found it adequate, and it must
 * not be byte-identical to one that priced it and found it starved either.
 */
export const FUNDING_UNPRICED = 'funding-unpriced'

/** The closed reason set a sentinel payload's `funding.reason` ranges over. */
export const FUNDING_REASONS = Object.freeze([FUNDING_STARVED, FUNDING_UNPRICED])

/** A positive finite seconds value rounded UP; anything else is `fallback`. */
function ceiledSeconds(value, fallback = 0) {
  return typeof value === 'number' && Number.isFinite(value) && value > 0
    ? Math.ceil(value)
    : fallback
}

/**
 * Price an allowance from same-sized dispatch legs.
 * @param {object} input
 * @param {number} input.legSeconds
 * @param {number} input.legs
 * @returns {number}
 */
export function stepAllowanceSeconds({ legSeconds, legs } = {}) {
  const leg = ceiledSeconds(legSeconds)
  const count = typeof legs === 'number' && Number.isInteger(legs) && legs > 0 ? legs : 0
  return leg * count
}

/**
 * Count how many full fix rounds an allowance can fund after initial legs.
 *
 * This is a projection of `priceAllowance`, not a second normalisation of the same
 * arithmetic. It used to be its own lenient one, with the OPPOSITE contract: `{}`
 * priced a starved step from nothing, a `legSeconds` of `0` funded a round the caller
 * could not afford, and a negative allowance floored to `0` and reported as starved —
 * the exact shapes `priceAllowance` exists to refuse. Two normalisations agree until
 * they do not, and gates that priced through the lenient one were asserting something
 * production never computes.
 *
 * @param {object} input same shape as `priceAllowance`
 * @returns {number}
 * @throws {TypeError} on any missing, non-finite, or non-positive term
 */
export function fundedFixRounds(input = {}) {
  return priceAllowance(input).fundedFixRounds
}

/** A positive finite number, for the reject-rather-than-fabricate gate below. */
function isPositiveFinite(value) {
  return typeof value === 'number' && Number.isFinite(value) && value > 0
}

/**
 * Price one per-step allowance ONCE, and refuse to price a malformed one.
 *
 * This is the single normalisation `fundingDisclosure` and the `funding` verb both
 * read. Deriving the echoed numbers separately from the count is the very defect the
 * disclosure exists to end — two independent normalisations agree until they do not,
 * and the narrated number then drifts from the arithmetic it claims to report.
 *
 * It **throws** rather than pricing a guess. Every rejected shape below used to
 * return a confident verdict: `{}` priced a starved step from nothing, a `legSeconds`
 * of `0` funded a round the caller could not afford, and a negative allowance was
 * silently floored to `0` and reported as starved. A caller cannot tell any of those
 * from a real price, so they must not be expressible.
 *
 * `fixRoundSeconds` alone is optional — omitted, the step is priced at
 * `DEFAULT_FIX_ROUND_SECONDS` and the value used is echoed back. Present but
 * malformed is still a rejection.
 *
 * Pure: no clock and no env, so the same input always prices the same way and an
 * allowance can never select an outcome by timing.
 *
 * @param {object} input
 * @param {number} input.allowanceSeconds
 * @param {number} input.legSeconds
 * @param {number} input.initialLegs positive integer
 * @param {number} [input.fixRoundSeconds]
 * @returns {{fundedFixRounds:number,allowanceSeconds:number,fixRoundSeconds:number}}
 * @throws {TypeError} on any missing, non-finite, or non-positive term
 */
export function priceAllowance(input = {}) {
  if (input === null || typeof input !== 'object' || Array.isArray(input)) {
    throw new TypeError('funding input must be a JSON object')
  }
  const { allowanceSeconds, legSeconds, initialLegs, fixRoundSeconds } = input
  if (!isPositiveFinite(allowanceSeconds)) {
    throw new TypeError('allowanceSeconds must be a positive finite number of seconds')
  }
  if (!isPositiveFinite(legSeconds)) {
    throw new TypeError('legSeconds must be a positive finite number of seconds')
  }
  if (!isPositiveFinite(initialLegs) || !Number.isInteger(initialLegs)) {
    throw new TypeError('initialLegs must be a positive integer')
  }
  if (fixRoundSeconds !== undefined && !isPositiveFinite(fixRoundSeconds)) {
    throw new TypeError(
      'fixRoundSeconds, when supplied, must be a positive finite number of seconds',
    )
  }
  const allowance = Math.floor(allowanceSeconds)
  const initial = Math.ceil(legSeconds) * initialLegs
  const price = Math.ceil(fixRoundSeconds ?? DEFAULT_FIX_ROUND_SECONDS)
  return {
    fundedFixRounds: Math.max(0, Math.floor((allowance - initial) / price)),
    allowanceSeconds: allowance,
    fixRoundSeconds: price,
  }
}

/**
 * The funding disclosure for one per-step allowance: the funded fix-round count plus
 * the two numbers a decline must name — the allowance that declined the work and the
 * price of the work it declined — as EFFECTIVE values, so a caller reports what was
 * actually priced rather than what it passed. All three come from ONE `priceAllowance`
 * call, so the echoed numbers cannot disagree with the count they explain.
 *
 * `reason` is `FUNDING_STARVED` exactly when no ordinary fix round is funded, and
 * `null` otherwise, so a funded step names no reason at all rather than an empty-string
 * one. A malformed input is not priced at all — `priceAllowance` throws.
 *
 * @param {object} input same shape as `priceAllowance`
 * @returns {{fundedFixRounds:number,reason:(string|null),allowanceSeconds:number,fixRoundSeconds:number}}
 * @throws {TypeError} on any missing, non-finite, or non-positive term
 */
export function fundingDisclosure(input = {}) {
  const priced = priceAllowance(input)
  return {
    fundedFixRounds: priced.fundedFixRounds,
    reason: priced.fundedFixRounds === 0 ? FUNDING_STARVED : null,
    allowanceSeconds: priced.allowanceSeconds,
    fixRoundSeconds: priced.fixRoundSeconds,
  }
}

/**
 * Build the run-file sentinel payload every terminal write carries, so no site
 * hand-builds escaped JSON inside double quotes and no site can interpolate a reason
 * into the byte-stable sentinel LINE by accident.
 *
 * **Total by design — it never throws.** Every call site is an UNCHECKED shell command
 * substitution inside a `bs-run-sentinel.mjs write` argument list, so a rejection does
 * not stop the write: the substitution collapses to the empty string, the writer stores
 * `{}`, and the verdict loses `provisional:false` as well as the reason — reading back
 * as the caller's own pessimistic seed. A rejected reason must therefore still yield a
 * VALID payload. The reason SET stays closed for consumers: an unrecognised value is
 * disclosed as `FUNDING_UNPRICED`, which is what it means — this run's funding state
 * could not be established — and never silently as the funded case. Callers that want
 * the rejection can compare `reason` against `FUNDING_REASONS` themselves; the CLI
 * warns on stderr.
 *
 * `reason` is empty (or absent) for a step that was priced and funded, or one of
 * `FUNDING_REASONS`.
 *
 * `census`, when supplied, rides the payload BESIDE the sentinel line and never
 * inside it. The line is a byte-stable external contract — `matchSentinel`, the
 * Step 6 routing block and `bs-dispatch-await.mjs` all match on `CAPPED_PREFIX`
 * — so disclosure that a reader needs but a router must not parse belongs here.
 * Omitted, the payload is byte-identical to what every existing site writes.
 *
 * @param {string} [reason]
 * @param {object} [extra]
 * @param {object|null} [extra.census] per-finding disposition counts, capped runs only
 * @returns {{provisional:false,funding?:{reason:string},census?:object}}
 */
export function sentinelPayload(reason = '', { census = null } = {}) {
  const disclosed =
    census !== null && typeof census === 'object' && !Array.isArray(census) ? { census } : {}
  if (reason === undefined || reason === null || reason === '') {
    return { provisional: false, ...disclosed }
  }
  if (typeof reason !== 'string' || !FUNDING_REASONS.includes(reason)) {
    return { provisional: false, funding: { reason: FUNDING_UNPRICED }, ...disclosed }
  }
  return { provisional: false, funding: { reason }, ...disclosed }
}

/** True when `reason` is a value `sentinelPayload` carries through unchanged. */
export function isFundingReason(reason) {
  return reason === '' || (typeof reason === 'string' && FUNDING_REASONS.includes(reason))
}

/** The closed reason set `admitFixRound` returns over. */
export const ADMIT_FIX_ROUND_REASONS = Object.freeze([
  'within-budget',
  'mustfix-override',
  'no-open-mustfix',
  'all-attempted',
  'overrun-exhausted',
  'regression-reserved',
  'round-cap',
])

export const ADMIT_DISPATCHED_ROUND_REASONS = Object.freeze([
  'guaranteed',
  'below-cap',
  'round-cap',
])

/**
 * Decide whether to admit one more dispatched round-role review pass.
 *
 * This fails closed in the admitting direction: an unreadable dispatched count
 * is treated as already at the cap so malformed evidence cannot widen the
 * allowance. Guaranteed whole-branch passes are evaluated first and always
 * admitted, so the cap cannot reduce Phase R coverage.
 * @param {object} input
 * @param {boolean} [input.guaranteed]
 * @param {number} [input.dispatchedRoundsUsed]
 * @param {number} [input.maxDispatchedRounds]
 * @returns {{admit: boolean, reason: string}}
 */
export function admitDispatchedRound({
  guaranteed = false,
  dispatchedRoundsUsed = 0,
  maxDispatchedRounds = DEFAULT_REVIEW_MAX_DISPATCHED_ROUNDS,
} = {}) {
  if (guaranteed === true) return { admit: true, reason: 'guaranteed' }
  const cap = resolveMaxDispatchedRounds(maxDispatchedRounds)
  const used = safeCount(dispatchedRoundsUsed, cap)
  if (used >= cap) return { admit: false, reason: 'round-cap' }
  return { admit: true, reason: 'below-cap' }
}

export const ADMIT_CONFIRMING_ROUND_REASONS = Object.freeze([
  'unchanged-tip',
  'tip-changed',
  'fixed',
  'verified',
  'carried-claim',
  'invalid-open',
])

/**
 * Decide whether a confirming round should run after a fix loop iteration.
 *
 * This also fails closed in the admitting direction: unreadable counts are
 * treated as non-zero so malformed evidence does not skip review. The only
 * refusal is the no-op case where the tip is unchanged and the review ledger
 * gained no fixed, verified, carried-claim, or unrepaired-invalid evidence.
 * @param {object} input
 * @param {boolean} [input.tipUnchanged]
 * @param {number} [input.fixedCount]
 * @param {number} [input.verifiedCount]
 * @param {number} [input.carriedClaimCount]
 * @param {number} [input.invalidCount]
 * @returns {{admit: boolean, reason: string}}
 */
export function admitConfirmingRound({
  tipUnchanged = false,
  fixedCount = 0,
  verifiedCount = 0,
  carriedClaimCount = 0,
  invalidCount = 0,
} = {}) {
  if (tipUnchanged !== true) return { admit: true, reason: 'tip-changed' }
  const fixed = safeCount(fixedCount, 1)
  if (fixed > 0) return { admit: true, reason: 'fixed' }
  const verified = safeCount(verifiedCount, 1)
  if (verified > 0) return { admit: true, reason: 'verified' }
  const carried = safeCount(carriedClaimCount, 1)
  if (carried > 0) return { admit: true, reason: 'carried-claim' }
  const invalid = safeCount(invalidCount, 1)
  if (invalid > 0) return { admit: true, reason: 'invalid-open' }
  return { admit: false, reason: 'unchanged-tip' }
}

/**
 * A non-negative integer count, or `fallback`. Strings are rejected outright
 * rather than coerced: every caller of this helper falls back in the REFUSING
 * direction, so a malformed count can never widen the allowance.
 * @param {unknown} raw
 * @param {number} fallback
 * @returns {number}
 */
function safeCount(raw, fallback) {
  return typeof raw === 'number' && Number.isInteger(raw) && raw >= 0 ? raw : fallback
}

/**
 * Decide whether to admit one more fix→confirm round. Pure — no clock, no env.
 *
 * Order is the contract, not an implementation detail:
 *   1. round cap  — evaluated first, never overridden (lower-only contract)
 *   2. no open must-fix — the override buys nothing, so today's refusal stands
 *   3. whole allowance remains (or no deadline at all) — ordinary admission
 *   4. below the allowance — only an UNATTEMPTED must-fix overrides, and only
 *      while overrun allowance remains
 *   5. general overrun spent — a must-fix the pass ITSELF caused draws on a
 *      separate, independently bounded reserve before `overrun-exhausted`
 *
 * `remainingSeconds: null` (or absent) means **no deadline was supplied**, never
 * a deadline of `0` — the distinction the skill's gate turns on. An unreadable
 * remainder falls back to `0`, which routes through the bounded override rather
 * than granting free admission.
 *
 * @param {object} input
 * @param {number|null} [input.remainingSeconds] seconds left against the caller's deadline; `null` = no deadline
 * @param {number} [input.fixRoundSeconds] the price of one round
 * @param {boolean} [input.openMustFix] any must-fix finding still open
 * @param {boolean} [input.unattemptedMustFix] at least one open must-fix no fix round has dispatched against
 * @param {number} [input.roundsUsed] fix rounds already run
 * @param {number} [input.maxRounds] the effective round cap (clamped lower-only)
 * @param {number} [input.overrunRoundsUsed] override rounds already spent
 * @param {boolean} [input.selfInflictedMustFix] an open must-fix whose cited site a fix commit THIS pass landed touched
 * @param {number} [input.regressionRoundsUsed] reserved regression rounds already spent
 * @returns {{admit: boolean, reason: string}}
 */
export function admitFixRound({
  remainingSeconds = null,
  fixRoundSeconds = DEFAULT_FIX_ROUND_SECONDS,
  openMustFix = false,
  unattemptedMustFix = false,
  roundsUsed = 0,
  maxRounds = DEFAULT_REVIEW_MAX_ROUNDS,
  overrunRoundsUsed = 0,
  selfInflictedMustFix = false,
  regressionRoundsUsed = 0,
} = {}) {
  // 1. The round cap bounds ATTEMPT COUNT and is never overridden. A run that
  //    exhausts it has attempted the finding up to `maxRounds` times, which is
  //    already a lawful terminal state.
  const cap = resolveMaxRounds(maxRounds)
  // Fail closed: an unreadable round count is treated as already at the cap.
  const used = safeCount(roundsUsed, cap)
  if (used >= cap) return { admit: false, reason: 'round-cap' }

  // 2. Nothing open to fix — the override exists only to close a must-fix, so a
  //    budget large enough to admit a round cannot reach `within-budget` here.
  if (!openMustFix) return { admit: false, reason: 'no-open-mustfix' }

  // 3. The ordinary gate: the WHOLE allowance must remain. No deadline = no cap.
  const price =
    typeof fixRoundSeconds === 'number' && Number.isFinite(fixRoundSeconds) && fixRoundSeconds > 0
      ? fixRoundSeconds
      : DEFAULT_FIX_ROUND_SECONDS
  if (remainingSeconds === null || remainingSeconds === undefined) {
    return { admit: true, reason: 'within-budget' }
  }
  // Fail closed: an unreadable remainder is treated as spent, not as unbounded.
  const remaining =
    typeof remainingSeconds === 'number' &&
    Number.isFinite(remainingSeconds) &&
    remainingSeconds > 0
      ? remainingSeconds
      : 0
  if (remaining >= price) return { admit: true, reason: 'within-budget' }

  // 4. Below the allowance. Only a located must-fix that no round has yet been
  //    dispatched against justifies the overrun; an attempted-and-failed finding
  //    is a lawful terminal state and must not spend a second allowance.
  if (!unattemptedMustFix) return { admit: false, reason: 'all-attempted' }
  // Fail closed: an unreadable overrun count is treated as already spent.
  const overrunUsed = safeCount(overrunRoundsUsed, MUSTFIX_OVERRUN_ROUNDS)
  if (overrunUsed >= MUSTFIX_OVERRUN_ROUNDS) {
    // 5. The general overrun is spent. A must-fix the pass ITSELF caused draws
    //    on its own reserve instead, so the pass can repair the regression it
    //    introduced rather than reporting a lawful-looking `overrun-exhausted`
    //    over its own damage. Evaluated HERE, not earlier: while the general
    //    allowance remains it is spent first, so the reserve is not consumed by
    //    a round the ordinary path could already fund, and the answer for every
    //    caller that does not supply the flag is byte-identical to today's.
    //
    //    Attribution is the CALLER's: `selfInflictedMustFix` is a supplied
    //    boolean, never computed here, so this stays pure. Absent input is
    //    `false` and the change is strictly additive.
    if (selfInflictedMustFix === true) {
      // Fail closed: an unreadable reserve count is treated as already spent.
      const regressionUsed = safeCount(regressionRoundsUsed, RESERVED_REGRESSION_ROUNDS)
      if (regressionUsed < RESERVED_REGRESSION_ROUNDS) {
        return { admit: true, reason: 'regression-reserved' }
      }
    }
    return { admit: false, reason: 'overrun-exhausted' }
  }
  return { admit: true, reason: 'mustfix-override' }
}

// Thin CLI (the surface the skill prose invokes):
//   node bs-review-caps.mjs rounds            → effective cap (reads BS_REVIEW_MAX_ROUNDS)
//   node bs-review-caps.mjs dispatched-rounds → effective dispatched cap
//   node bs-review-caps.mjs sentinel clean --in <report.json> → the clean line, DERIVED
//   node bs-review-caps.mjs sentinel capped N → the capped sentinel line for N rounds
//   node bs-review-caps.mjs match "<line>"    → JSON classification of a sentinel line
//   node bs-review-caps.mjs verdict --in <report.json> → sentinel derived from report evidence
//   node bs-review-caps.mjs verdict --in <report.json> --payload [<reason>] → the payload beside it
//   node bs-review-caps.mjs confidence --in <report.json> → JSON derived confidence grade/reasons
//   node bs-review-caps.mjs classify --in <file> → JSON whole-text sentinel classification
//   node bs-review-caps.mjs oscillation --in <payload.json> → JSON {oscillating,reasons}
//   node bs-review-caps.mjs admit-fix-round '<json>'  → JSON {admit,reason} for one fix round
//   node bs-review-caps.mjs admit-dispatched-round '<json>' → JSON {admit,reason}
//   node bs-review-caps.mjs admit-confirming-round '<json>' → JSON {admit,reason}
//   node bs-review-caps.mjs funding '<json>' → JSON {fundedFixRounds,reason,allowanceSeconds,fixRoundSeconds}
//   node bs-review-caps.mjs sentinel-payload [<reason>] → the run-file sentinel payload JSON
import { readFileSync } from 'node:fs'
import { createGateRecorder } from './gate-outcome.mjs'
import { isMainModule } from './main-module.mjs'

// Gate-outcome recording covers the three `admit-*` verbs and nothing else. Those three
// ARE the gate — one is invoked per review round and each one either admits the round or refuses
// it, which is exactly a pass/fire outcome. Every other verb here (rounds, sentinel, match, verdict,
// confidence, classify, oscillation, funding, sentinel-payload) is pure computation or a renderer:
// it has no verdict to record, so recording one would fabricate a firing rate. This mirrors the
// plan's own adjudication of bs-run-sentinel as "a verdict router, not a gate".
if (isMainModule(import.meta.url)) {
  const [cmd, ...rest] = process.argv.slice(2)
  const readInputFile = () => {
    if (rest.length !== 2 || rest[0] !== '--in' || !rest[1]) {
      process.stderr.write(`${cmd} requires --in <path>\n`)
      process.exit(2)
    }
    try {
      return readFileSync(rest[1], 'utf8')
    } catch (err) {
      process.stderr.write(`unable to read ${rest[1]}: ${err.message}\n`)
      process.exit(2)
    }
  }
  if (cmd === 'rounds') {
    process.stdout.write(`${reviewMaxRounds()}\n`)
  } else if (cmd === 'dispatched-rounds') {
    process.stdout.write(`${reviewMaxDispatchedRounds()}\n`)
  } else if (cmd === 'sentinel' && rest[0] === 'clean') {
    // DERIVED CLEAN ONLY. A clean terminal sentinel is a function of report evidence,
    // never an unconditional literal. The evidence-free form this replaces let a pass
    // that had repaired nothing print a contract-valid clean line, die before its repair
    // pass re-categorized, and publish a false green over an open must-fix — while
    // `reviewVerdict`, the owner that already refuses clean on unrepaired `invalid`
    // evidence, was never consulted. Both refusals below fail CLOSED (no line on stdout,
    // non-zero exit), and the two exit codes are distinct so the caller can tell "you
    // supplied no evidence" from "your evidence says otherwise".
    //
    // `sentinel capped <N>` deliberately keeps its evidence-free form: the pre-dispatch
    // seed and the decline routes legitimately hold no report, and those routes are
    // pessimistic, so an evidence-free capped line can only ever under-claim.
    if (rest[1] !== '--in' || !rest[2]) {
      process.stderr.write(
        'sentinel clean requires report evidence: sentinel clean --in <report.json>\n',
      )
      process.exit(2)
    }
    let cleanReport
    try {
      cleanReport = JSON.parse(readFileSync(rest[2], 'utf8'))
    } catch (err) {
      process.stderr.write(`unable to read report JSON ${rest[2]}: ${err.message}\n`)
      process.exit(2)
    }
    const cleanVerdict = reviewVerdict(cleanReport)
    if (cleanVerdict.status !== 'clean') {
      process.stderr.write(
        `refusing a clean sentinel: derived verdict is ${cleanVerdict.status} (${cleanVerdict.reasons.join(', ')})\n`,
      )
      process.exit(3)
    }
    process.stdout.write(`${cleanSentinel()}\n`)
  } else if (cmd === 'sentinel' && rest[0] === 'capped') {
    // rest[1] is the actual round count reached; require a positive integer.
    // `0` is not a valid round count because the loop counter starts at 1.
    const raw = String(rest[1] ?? '').trim()
    if (!/^[1-9]\d*$/.test(raw)) {
      process.stderr.write('sentinel capped requires a positive integer round count\n')
      process.exit(2)
    }
    const rounds = Number.parseInt(raw, 10)
    process.stdout.write(`${cappedSentinel(rounds)}\n`)
  } else if (cmd === 'match') {
    process.stdout.write(`${JSON.stringify(matchSentinel(rest[0] ?? ''))}\n`)
  } else if (cmd === 'verdict') {
    // `--payload [<reason>]` is ADDITIVE. Without it this verb's stdout is the
    // same sentinel LINE it has always printed, byte for byte, which is what
    // routing matches on; with it the verb prints the payload that rides beside
    // that line instead, carrying the disposition census a capped run discloses.
    // Splitting the two keeps the census reachable without ever widening the
    // line, whose prefix is an external contract three consumers match on.
    //
    // STAGED: no `SKILL.md` write site calls this route yet — every terminal
    // sentinel write still spells `sentinel-payload "${STEP_6C_FUNDING_REASON:-}"`,
    // which carries no census. The census is reachable only through this flag
    // until a caller is wired to it.
    const payloadAt = rest.indexOf('--payload')
    // The reason is OPTIONAL, so the next token is only a reason when it is not
    // itself a flag. Splicing just this flag out — rather than truncating
    // everything after it — keeps `readInputFile`'s exact `--in <path>` arity
    // whatever ORDER the two are given in; truncating made `--payload` the one
    // position-sensitive flag here, so `--payload --in x` exited 2 claiming
    // `--in` was missing when it had been supplied.
    const nextArg = payloadAt === -1 ? undefined : rest[payloadAt + 1]
    const hasReason = nextArg !== undefined && !nextArg.startsWith('--')
    const payloadReason = payloadAt === -1 ? null : hasReason ? nextArg : ''
    if (payloadAt !== -1) rest.splice(payloadAt, hasReason ? 2 : 1)
    let report
    try {
      report = JSON.parse(readInputFile())
    } catch (err) {
      process.stderr.write(`unable to parse report JSON: ${err.message}\n`)
      process.exit(2)
    }
    const verdict = reviewVerdict(report)
    if (payloadAt !== -1) {
      // Same total-never-throws contract as `sentinel-payload`: this is an
      // unchecked command substitution at every write site, so an unrecognised
      // reason is disclosed as `funding-unpriced` and named on stderr rather
      // than emptying the argument and dropping `provisional:false` with it.
      if (!isFundingReason(payloadReason)) {
        process.stderr.write(
          `verdict --payload: unrecognised funding reason ${JSON.stringify(payloadReason)}; ` +
            `disclosing ${FUNDING_UNPRICED} instead (expected empty or one of: ${FUNDING_REASONS.join(', ')})\n`,
        )
      }
      // A clean run has nothing capped to disclose, so it carries no census.
      const census = verdict.status === 'capped' ? dispositionCensus(report) : null
      process.stdout.write(`${JSON.stringify(sentinelPayload(payloadReason, { census }))}\n`)
    } else if (verdict.status === 'clean') {
      process.stdout.write(`${cleanSentinel()}\n`)
    } else {
      const rounds = Number.isInteger(report?.rounds) && report.rounds > 0 ? report.rounds : 1
      if (verdict.reasons.includes('no-coverage')) {
        process.stdout.write(`${coverageCappedSentinel(rounds)}\n`)
      } else {
        process.stdout.write(`${cappedSentinel(rounds)}\n`)
      }
    }
  } else if (cmd === 'confidence') {
    let report
    try {
      report = JSON.parse(readInputFile())
    } catch (err) {
      process.stderr.write(`unable to parse report JSON: ${err.message}\n`)
      process.exit(2)
    }
    process.stdout.write(`${JSON.stringify(reviewConfidence(report))}\n`)
  } else if (cmd === 'classify') {
    process.stdout.write(`${JSON.stringify(classifySentinels(readInputFile()))}\n`)
  } else if (cmd === 'oscillation') {
    const raw = rest[0] === '--in' ? readInputFile() : (rest[0] ?? '')
    let input
    try {
      input = JSON.parse(raw)
    } catch {
      input = undefined
    }
    if (input === null || typeof input !== 'object' || Array.isArray(input)) {
      process.stderr.write('oscillation requires one JSON object argument\n')
      process.exit(2)
    }
    const result = classifyOscillation(input)
    if (result.reasons.length > 0) {
      process.stderr.write(`oscillation payload is malformed: ${result.reasons.join(', ')}\n`)
      process.exit(2)
    }
    process.stdout.write(`${JSON.stringify(result)}\n`)
  } else if (cmd === 'admit-fix-round') {
    // One JSON object argument, printed back as the same `{admit,reason}` the
    // function returns — so the invocation the skill prose cites cannot drift
    // from the surface the decision table tests.
    // The admission decision's own `reason` is already a closed slug vocabulary
    // (ADMIT_FIX_ROUND_REASONS), so the outcome line reuses it verbatim rather than inventing a
    // parallel set that could drift from the decision table.
    const recorder = createGateRecorder('bs-review-caps.admit-fix-round')
    const raw = rest[0] ?? ''
    let input
    try {
      input = JSON.parse(raw)
    } catch {
      input = undefined
    }
    if (input === null || typeof input !== 'object' || Array.isArray(input)) {
      recorder.record('fire', 'malformed-input')
      process.stderr.write('admit-fix-round requires one JSON object argument\n')
      process.exit(2)
    }
    const decision = admitFixRound(input)
    recorder.record(decision.admit ? 'pass' : 'fire', decision.reason)
    process.stdout.write(`${JSON.stringify(decision)}\n`)
  } else if (cmd === 'admit-dispatched-round') {
    const recorder = createGateRecorder('bs-review-caps.admit-dispatched-round')
    const raw = rest[0] ?? ''
    let input
    try {
      input = JSON.parse(raw)
    } catch {
      input = undefined
    }
    if (input === null || typeof input !== 'object' || Array.isArray(input)) {
      recorder.record('fire', 'malformed-input')
      process.stderr.write('admit-dispatched-round requires one JSON object argument\n')
      process.exit(2)
    }
    const decision = admitDispatchedRound(input)
    recorder.record(decision.admit ? 'pass' : 'fire', decision.reason)
    process.stdout.write(`${JSON.stringify(decision)}\n`)
  } else if (cmd === 'admit-confirming-round') {
    const recorder = createGateRecorder('bs-review-caps.admit-confirming-round')
    const raw = rest[0] ?? ''
    let input
    try {
      input = JSON.parse(raw)
    } catch {
      input = undefined
    }
    if (input === null || typeof input !== 'object' || Array.isArray(input)) {
      recorder.record('fire', 'malformed-input')
      process.stderr.write('admit-confirming-round requires one JSON object argument\n')
      process.exit(2)
    }
    const decision = admitConfirmingRound(input)
    recorder.record(decision.admit ? 'pass' : 'fire', decision.reason)
    process.stdout.write(`${JSON.stringify(decision)}\n`)
  } else if (cmd === 'funding') {
    // One JSON object argument, printed back as the disclosure the function returns —
    // so a skill body COMPUTES the funded-round count it used to narrate, and the
    // number in the prose cannot drift from the arithmetic the tests pin.
    const raw = rest[0] ?? ''
    let input
    try {
      input = JSON.parse(raw)
    } catch {
      input = undefined
    }
    if (input === null || typeof input !== 'object' || Array.isArray(input)) {
      process.stderr.write('funding requires one JSON object argument\n')
      process.exit(2)
    }
    // Reject rather than fabricate: a malformed term prices nothing and exits
    // non-zero, so a caller's `if` cannot read a failed pricing call as a funded step.
    let disclosure
    try {
      disclosure = fundingDisclosure(input)
    } catch (err) {
      process.stderr.write(`funding cannot price this allowance: ${err.message}\n`)
      process.exit(2)
    }
    process.stdout.write(`${JSON.stringify(disclosure)}\n`)
  } else if (cmd === 'sentinel-payload') {
    // The whole payload, built here rather than hand-escaped at each write site, so a
    // caller states a reason instead of quoting JSON inside a double-quoted shell string.
    //
    // This verb NEVER exits non-zero. Every call site spells it as an unchecked command
    // substitution in a `bs-run-sentinel.mjs write` argument list, so a non-zero exit
    // does not stop the write — it empties the argument and persists `{}`, dropping
    // `provisional:false` along with the reason. An unrecognised reason is disclosed as
    // `funding-unpriced` and named on stderr, so the operator sees the mistake while the
    // run file still carries a verdict a consumer can read.
    const requested = rest[0] ?? ''
    if (!isFundingReason(requested)) {
      process.stderr.write(
        `sentinel-payload: unrecognised funding reason ${JSON.stringify(requested)}; ` +
          `disclosing ${FUNDING_UNPRICED} instead (expected empty or one of: ${FUNDING_REASONS.join(', ')})\n`,
      )
    }
    process.stdout.write(`${JSON.stringify(sentinelPayload(requested))}\n`)
  } else {
    process.stderr.write(
      "usage: bs-review-caps.mjs <rounds | dispatched-rounds | sentinel clean --in <report.json> | sentinel capped <N> | match \"<line>\" | verdict --in <report.json> [--payload [<reason>]] | confidence --in <report.json> | classify --in <file> | oscillation --in <payload.json> | admit-fix-round '<json>' | admit-dispatched-round '<json>' | admit-confirming-round '<json>' | funding '<json>' | sentinel-payload [<reason>]>\n",
    )
    process.exit(2)
  }
}
