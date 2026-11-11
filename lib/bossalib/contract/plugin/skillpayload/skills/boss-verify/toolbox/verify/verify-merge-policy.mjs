import { detectAreas } from './verify-areas.mjs'
import { legacyCodes } from './verify-level.mjs'
import { testPath } from './verify-ui-proof.mjs'

/**
 * A repo's merge policy: the structured `verifyDefaults.merge.auto` rules it reads from the base
 * tip, and the park codes a head is left with once those rules have covered what they can. With no
 * rule nothing merges without a person, so a high head parks `human-merge`.
 *
 * @typedef {{level?: 'high'|'moderate'|'low', area?: string, proof?: 'tests'|'recording', change?: 'additive'}} MergeRule
 * @typedef {import('./verify-confidence.mjs').ConfidenceResult} ConfidenceResult
 * @typedef {import('./verify-confidence.mjs').ConfidenceFacts} ConfidenceFacts
 * @typedef {ConfidenceResult['asks'][number]} Ask
 * @typedef {{alwaysReview?: {name: string, parkCode: string}[]}} Review
 * @typedef {{index: number, rule: MergeRule}} RuleHit
 * @typedef {import('./verify-merge-prose.mjs').PolicyDecision} PolicyDecision
 * @typedef {{area: string, label: string, source: string, code: string|null, floor: boolean, covered: boolean, rule: RuleHit|null, prose: PolicyDecision|null}} AskDecision
 * @typedef {{decisions: AskDecision[], residual: string[], levelRule: RuleHit|null, holds: string[], proseHolds: PolicyDecision[], policy: {state: string, path?: string, blob?: string, why?: string}}} MergeDecisions
 */

const RULE_KEYS = ['level', 'area', 'proof', 'change']
// Asks no structured rule covers: protected paths, extension sign-offs, capped areas, an edit to
// verify's own policy, and unproven behavior.
const FLOOR_AREAS = new Set(['verify-policy', 'unproven-behavior'])

/** @param {unknown} value @returns {value is Record<string, unknown>} */
const object = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)

/**
 * The rule list of a `verifyDefaults.merge` block: null when the block is absent, [] when it holds
 * no rule. Malformed rules are dropped; the schema rejects them before the gate reads the block.
 * @param {unknown} block @returns {MergeRule[]|null}
 */
export function mergeRules(block) {
  if (!object(block)) return null
  if (!Array.isArray(block.auto)) return []
  return block.auto
    .filter(
      (rule) =>
        object(rule) &&
        Object.keys(rule).length > 0 &&
        Object.entries(rule).every(
          ([key, value]) => RULE_KEYS.includes(key) && typeof value === 'string',
        ),
    )
    .map((rule) => /** @type {MergeRule} */ ({ ...rule }))
}

/** @param {Ask} ask @param {Review} review */
const codeOf = (ask, review) =>
  legacyCodes({ level: 'moderate', asks: [ask], reasons: [], loweredBy: [], health: [] }, review)[0]

/** @param {Ask} ask @param {Review} review */
const isFloor = (ask, review) =>
  ask.source === 'cap' ||
  ask.area.startsWith('extension:') ||
  FLOOR_AREAS.has(ask.area) ||
  codeOf(ask, review) === 'always-human-path'

/** The area ids an ask counts as: its built-in area, or the category of a legacy config ask.
 * @param {Ask} ask @param {Review} review @returns {string|null}
 */
function askArea(ask, review) {
  if (ask.source === 'builtin') return ask.area
  if (ask.source !== 'config') return null
  const code = codeOf(ask, review)
  return code?.startsWith('human-review:') ? code.slice('human-review:'.length) : null
}

/** The `change` fact of an area: breaking when any entry says so, missing when none carries one.
 * @param {ConfidenceFacts} facts @param {string} area
 */
function changeOf(facts, area) {
  const changes = (facts.areas ?? [])
    .filter((entry) => entry.area === area)
    .map((entry) => /** @type {{change?: string}} */ (entry).change)
    .filter(Boolean)
  return changes.includes('breaking') ? 'breaking' : changes[0]
}

/** Whether a UI proof speaks to every path of an ask. The proof facts look only at built-in UI
 * paths (and, for `tests`, the tests that cover them), while a config UI ask can hold any path.
 * @param {ConfidenceFacts} facts @param {Ask} ask @param {MergeRule['proof']} proof
 */
function proofCovers(facts, ask, proof) {
  const paths = (facts.areas ?? [])
    .filter((entry) => entry.area === ask.area)
    .flatMap((entry) => entry.paths ?? [])
  const ui = new Set(detectAreas(paths).find((area) => area.area === 'ui')?.paths ?? [])
  return (
    paths.length > 0 &&
    paths.every((path) => ui.has(path) || (proof === 'tests' && testPath.test(path)))
  )
}

const NO_PROSE = Object.freeze({ state: 'none', decisions: [], stale: false })

/**
 * How a repo's merge rules decided each ask of a head: for every ask in `result.asks` (same order)
 * its own park code, whether it is a floor ask no rule can cover, the first structured rule that
 * covers it (`{index, rule}`, or null) and the prose decision for it; the level
 * causes left once the asks are settled (`residual`); the level-only rule allowing the head's level
 * (`levelRule`, or null); and the prose policy's own hold codes.
 * @param {ConfidenceResult} result the head's confidence
 * @param {ConfidenceFacts} facts `levelFacts(...)` for the same head
 * @param {Review} [review] the base tip's review policy
 * @param {unknown} [merge] the base tip's raw `verifyDefaults.merge` block
 * @param {import('./verify-merge-prose.mjs').ProseFacts} [prose]
 * @returns {MergeDecisions}
 */
export function mergeDecisions(result, facts, review = {}, merge = null, prose = NO_PROSE) {
  const assessable = result.level !== 'cannot-assess' && result.level !== 'blocked'
  const rules = assessable ? (mergeRules(merge) ?? []) : []
  const holds =
    assessable &&
    (prose.state === 'unreadable' ||
      prose.decisions.some((decision) => decision.decision === 'human'))
      ? ['human-review:policy']
      : []
  const levelIndex = rules.findIndex(
    (rule) => rule.area === undefined && rule.level === result.level,
  )
  /** @param {MergeRule} rule @param {Ask} ask */
  const matches = (rule, ask) => {
    if (rule.level !== undefined && rule.level !== result.level) return false
    if (rule.area === undefined) return true
    if (askArea(ask, review) !== rule.area) return false
    if (rule.proof !== undefined && facts.proof?.ui !== rule.proof) return false
    if (rule.proof !== undefined && !proofCovers(facts, ask, rule.proof)) return false
    return rule.change === undefined || changeOf(facts, rule.area) === rule.change
  }
  /** @param {Ask} ask @param {boolean} floor */
  const proseFor = (ask, floor) => {
    if (!assessable) return null
    const matching = prose.decisions.filter((decision) => decision.ask === ask.area)
    return (
      matching.find((decision) => decision.decision === 'human') ??
      (floor ? null : (matching[0] ?? null))
    )
  }
  const decisions = result.asks.map((ask) => {
    const floor = isFloor(ask, review)
    const index = floor || ask.source === 'judge' ? -1 : rules.findIndex((r) => matches(r, ask))
    const proseDecision = proseFor(ask, floor)
    const byProse = proseDecision?.decision === 'auto' && proseDecision.verified
    return {
      area: ask.area,
      label: ask.label,
      source: ask.source,
      code: codeOf(ask, review) ?? null,
      floor,
      covered: !floor && (index !== -1 || byProse),
      rule: index === -1 ? null : { index, rule: rules[index] },
      prose: proseDecision,
    }
  })
  const uiCovered =
    facts.proof?.behavior !== 'unproven' &&
    result.asks.some((ask) => askArea(ask, review) === 'ui') &&
    result.asks.every((ask, i) => askArea(ask, review) !== 'ui' || decisions[i].covered)
  const residual = result.loweredBy.filter(
    (cause) => cause !== 'area-asks' && !(cause === 'unproven' && uiCovered),
  )
  const { state, path, blob, why } = prose
  return {
    decisions,
    residual,
    levelRule: levelIndex === -1 ? null : { index: levelIndex, rule: rules[levelIndex] },
    holds,
    proseHolds: prose.decisions.filter(
      (decision) => decision.decision === 'human' && (!decision.ask || !decision.verified),
    ),
    policy: { state, ...(path && { path }), ...(blob && { blob }), ...(why && { why }) },
  }
}

/**
 * The park codes a head is left with after its repo's merge rules: the projection of
 * mergeDecisions, which it also records on `result.merge` so a reader of the judge's confidence
 * (the merge-policy replay) sees how each ask was decided.
 * @param {ConfidenceResult & {merge?: MergeDecisions}} result the head's confidence
 * @param {ConfidenceFacts} facts `levelFacts(...)` for the same head
 * @param {Review} [review] the base tip's review policy
 * @param {unknown} [merge] the base tip's raw `verifyDefaults.merge` block
 * @param {import('./verify-merge-prose.mjs').ProseFacts} [prose]
 * @returns {string[]}
 */
export function policyCodes(result, facts, review = {}, merge = null, prose = NO_PROSE) {
  const decided = mergeDecisions(result, facts, review, merge, prose)
  result.merge = decided
  if (result.level === 'cannot-assess' || result.level === 'blocked')
    return legacyCodes(result, review)
  const { holds, residual, levelRule } = decided
  if (result.level === 'high') return [...holds, ...(levelRule ? [] : ['human-merge'])]
  const uncovered = result.asks.filter((_, i) => !decided.decisions[i].covered)
  if (uncovered.length === 0 && (residual.length === 0 || levelRule)) return holds
  return [...holds, ...legacyCodes({ ...result, asks: uncovered }, review)]
}
