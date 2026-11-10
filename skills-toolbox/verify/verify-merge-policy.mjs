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

/**
 * The park codes a head is left with after its repo's merge rules.
 * @param {ConfidenceResult} result the head's confidence
 * @param {ConfidenceFacts} facts `levelFacts(...)` for the same head
 * @param {Review} [review] the base tip's review policy
 * @param {unknown} [merge] the base tip's raw `verifyDefaults.merge` block
 * @param {import('./verify-merge-prose.mjs').ProseFacts} [prose]
 * @returns {string[]}
 */
export function policyCodes(
  result,
  facts,
  review = {},
  merge = null,
  prose = { state: 'none', decisions: [], stale: false },
) {
  if (result.level === 'cannot-assess' || result.level === 'blocked')
    return legacyCodes(result, review)
  const holds =
    prose.state === 'unreadable' ||
    prose.decisions.some((decision) => decision.decision === 'human')
      ? ['human-review:policy']
      : []
  const rules = mergeRules(merge) ?? []
  const levelOnly = rules.some((rule) => rule.area === undefined && rule.level === result.level)
  /** @param {MergeRule} rule @param {Ask} ask */
  const matches = (rule, ask) => {
    if (rule.level !== undefined && rule.level !== result.level) return false
    if (rule.area === undefined) return true
    if (askArea(ask, review) !== rule.area) return false
    if (rule.proof !== undefined && facts.proof?.ui !== rule.proof) return false
    if (rule.proof !== undefined && !proofCovers(facts, ask, rule.proof)) return false
    return rule.change === undefined || changeOf(facts, rule.area) === rule.change
  }
  const uncovered = result.asks.filter(
    (ask) =>
      isFloor(ask, review) ||
      !(
        (ask.source !== 'judge' && rules.some((rule) => matches(rule, ask))) ||
        (prose.decisions.some(
          (decision) =>
            decision.ask === ask.area && decision.decision === 'auto' && decision.verified,
        ) &&
          !prose.decisions.some(
            (decision) => decision.ask === ask.area && decision.decision === 'human',
          ))
      ),
  )
  const uiAsks = result.asks.filter((ask) => askArea(ask, review) === 'ui')
  const uiCovered =
    facts.proof?.behavior !== 'unproven' &&
    uiAsks.length > 0 &&
    uiAsks.every((ask) => !uncovered.includes(ask))
  const residual = result.loweredBy.filter(
    (cause) => cause !== 'area-asks' && !(cause === 'unproven' && uiCovered),
  )
  if (result.level === 'high') return [...holds, ...(levelOnly ? [] : ['human-merge'])]
  if (uncovered.length === 0 && (residual.length === 0 || levelOnly)) return holds
  return [...holds, ...legacyCodes({ ...result, asks: uncovered }, review)]
}
