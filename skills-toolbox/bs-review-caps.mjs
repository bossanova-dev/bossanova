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

export const FUNDING_STARVED = 'funding-starved'

/**
 * Price an allowance from same-sized dispatch legs.
 * @param {object} input
 * @param {number} input.legSeconds
 * @param {number} input.legs
 * @returns {number}
 */
export function stepAllowanceSeconds({ legSeconds, legs } = {}) {
  const leg =
    typeof legSeconds === 'number' && Number.isFinite(legSeconds) && legSeconds > 0
      ? Math.ceil(legSeconds)
      : 0
  const count = typeof legs === 'number' && Number.isInteger(legs) && legs > 0 ? legs : 0
  return leg * count
}

/**
 * Count how many full fix rounds an allowance can fund after initial legs.
 * @param {object} input
 * @param {number} input.allowanceSeconds
 * @param {number} input.legSeconds
 * @param {number} input.initialLegs
 * @param {number} input.fixRoundSeconds
 * @returns {number}
 */
export function fundedFixRounds({
  allowanceSeconds,
  legSeconds,
  initialLegs,
  fixRoundSeconds = DEFAULT_FIX_ROUND_SECONDS,
} = {}) {
  const allowance =
    typeof allowanceSeconds === 'number' &&
    Number.isFinite(allowanceSeconds) &&
    allowanceSeconds > 0
      ? Math.floor(allowanceSeconds)
      : 0
  const initial = stepAllowanceSeconds({ legSeconds, legs: initialLegs })
  const price =
    typeof fixRoundSeconds === 'number' && Number.isFinite(fixRoundSeconds) && fixRoundSeconds > 0
      ? Math.ceil(fixRoundSeconds)
      : DEFAULT_FIX_ROUND_SECONDS
  return Math.max(0, Math.floor((allowance - initial) / price))
}

/** The closed reason set `admitFixRound` returns over. */
export const ADMIT_FIX_ROUND_REASONS = Object.freeze([
  'within-budget',
  'mustfix-override',
  'no-open-mustfix',
  'all-attempted',
  'overrun-exhausted',
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
  if (overrunUsed >= MUSTFIX_OVERRUN_ROUNDS) return { admit: false, reason: 'overrun-exhausted' }
  return { admit: true, reason: 'mustfix-override' }
}

// Thin CLI (the surface the skill prose invokes):
//   node bs-review-caps.mjs rounds            → effective cap (reads BS_REVIEW_MAX_ROUNDS)
//   node bs-review-caps.mjs dispatched-rounds → effective dispatched cap
//   node bs-review-caps.mjs sentinel clean    → the clean sentinel line
//   node bs-review-caps.mjs sentinel capped N → the capped sentinel line for N rounds
//   node bs-review-caps.mjs match "<line>"    → JSON classification of a sentinel line
//   node bs-review-caps.mjs verdict --in <report.json> → sentinel derived from report evidence
//   node bs-review-caps.mjs confidence --in <report.json> → JSON derived confidence grade/reasons
//   node bs-review-caps.mjs classify --in <file> → JSON whole-text sentinel classification
//   node bs-review-caps.mjs oscillation --in <payload.json> → JSON {oscillating,reasons}
//   node bs-review-caps.mjs admit-fix-round '<json>'  → JSON {admit,reason} for one fix round
//   node bs-review-caps.mjs admit-dispatched-round '<json>' → JSON {admit,reason}
//   node bs-review-caps.mjs admit-confirming-round '<json>' → JSON {admit,reason}
import { readFileSync } from 'node:fs'
import { isMainModule } from './main-module.mjs'

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
    let report
    try {
      report = JSON.parse(readInputFile())
    } catch (err) {
      process.stderr.write(`unable to parse report JSON: ${err.message}\n`)
      process.exit(2)
    }
    const verdict = reviewVerdict(report)
    if (verdict.status === 'clean') {
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
    const raw = rest[0] ?? ''
    let input
    try {
      input = JSON.parse(raw)
    } catch {
      input = undefined
    }
    if (input === null || typeof input !== 'object' || Array.isArray(input)) {
      process.stderr.write('admit-fix-round requires one JSON object argument\n')
      process.exit(2)
    }
    process.stdout.write(`${JSON.stringify(admitFixRound(input))}\n`)
  } else if (cmd === 'admit-dispatched-round') {
    const raw = rest[0] ?? ''
    let input
    try {
      input = JSON.parse(raw)
    } catch {
      input = undefined
    }
    if (input === null || typeof input !== 'object' || Array.isArray(input)) {
      process.stderr.write('admit-dispatched-round requires one JSON object argument\n')
      process.exit(2)
    }
    process.stdout.write(`${JSON.stringify(admitDispatchedRound(input))}\n`)
  } else if (cmd === 'admit-confirming-round') {
    const raw = rest[0] ?? ''
    let input
    try {
      input = JSON.parse(raw)
    } catch {
      input = undefined
    }
    if (input === null || typeof input !== 'object' || Array.isArray(input)) {
      process.stderr.write('admit-confirming-round requires one JSON object argument\n')
      process.exit(2)
    }
    process.stdout.write(`${JSON.stringify(admitConfirmingRound(input))}\n`)
  } else {
    process.stderr.write(
      "usage: bs-review-caps.mjs <rounds | dispatched-rounds | sentinel clean | sentinel capped <N> | match \"<line>\" | verdict --in <report.json> | confidence --in <report.json> | classify --in <file> | oscillation --in <payload.json> | admit-fix-round '<json>' | admit-dispatched-round '<json>' | admit-confirming-round '<json>'>\n",
    )
    process.exit(2)
  }
}
