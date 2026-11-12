import { MERGE_POLICY_LIMITS } from './verify-merge-prose.mjs'
import { uiProof } from './verify-ui-proof.mjs'
import { withJudgeAreas } from './verify-judge.mjs'
import { capAreas, capReason } from './verify-area-caps.mjs'
import { canonicalAreaId, configAreaIds, detectAreas } from './verify-areas.mjs'
import {
  confidence,
  LEVELS,
  STATES,
  MAX_ASKS_BEFORE_LOW,
  MAX_REASONS,
} from './verify-confidence.mjs'

/** @typedef {import('./verify-confidence.mjs').ConfidenceResult} ConfidenceResult */
/** @typedef {import('./verify-confidence.mjs').ConfidenceFacts} ConfidenceFacts */
/** @typedef {{review?: {alwaysReview?: {name:string,parkCode:string}[]}, prosePolicy?: import('./verify-merge-prose.mjs').ProseFacts, dbtRoots?: string[], areaCaps?: {caps: import('./verify-area-caps.mjs').AreaCap[], health: string[]}, changedPaths?: string[]|null, areas?: ConfidenceFacts['areas'], changedLines?: number, judgeAreas?: ConfidenceFacts['areas'], judgeFailed?: boolean, sizeBaseline?: ConfidenceFacts['size']['baseline'] & {stale?: boolean}, range?: {kind: string}, policySurfaceHit?: boolean, ci?: {green?: boolean}, uiTests?: {covered?: boolean}, blockers?: {kind:string,summary:string}[]}} GateFacts */
export const COMPUTED_HUMAN_REVIEW_CODES = Object.freeze([
  'human-review:policy',
  'human-review:judge',
  'human-review:api-contract',
  'human-review:infra',
  'human-review:asks',
  'human-review:size',
])
/** Preserve same-head approvals recorded before computed asks had their own codes.
 * @param {readonly string[]} codes @returns {string[]}
 */
export function expandLegacyWaiver(codes) {
  return [
    ...new Set([
      ...codes,
      ...(codes.includes('always-human-path') ? COMPUTED_HUMAN_REVIEW_CODES : []),
    ]),
  ]
}

const SIGN_OFF_CODES = [
  'always-human-path',
  'human-review:database',
  'human-review:security',
  'human-review:ui',
  ...COMPUTED_HUMAN_REVIEW_CODES,
  'human-review:unproven',
]
/** @param {string} code */
export const levelSignOff = (code) => SIGN_OFF_CODES.includes(code)

/** @param {string} text */
export const compactParkCodeText = (text) => text.replaceAll('human-review:', '+')
/** @param {string} code */
export const expandParkCode = (code) =>
  code.startsWith('+') ? `human-review:${code.slice(1)}` : code

/** Build confidence inputs without allowing extension sign-offs to bypass the level.
 * @param {GateFacts} gate @param {{extensionReasons?: string[], outcomes?: import('./verify-ui-proof.mjs').ProofOutcome[], judgeAreas?: ConfidenceFacts['areas'], caps?: import('./verify-area-caps.mjs').AreaCap[]}} [options] @returns {ConfidenceFacts}
 */
export function levelFacts(
  gate,
  {
    extensionReasons = [],
    judgeAreas = gate.judgeAreas,
    caps = gate.areaCaps?.caps ?? [],
    outcomes = [],
  } = {},
) {
  const areas = [
    ...capAreas(
      [
        ...detectAreas(Array.isArray(gate.changedPaths) ? gate.changedPaths : [], {
          dbtRoots: gate.dbtRoots ?? [],
        }),
        ...(gate.areas ?? []).map((area) => ({ ...area, paths: [...(area.paths ?? [])] })),
      ],
      caps,
    ),
    ...(gate.areas ?? []),
  ]
  if (gate.policySurfaceHit)
    areas.push({ area: 'verify-policy', label: 'Verify policy', defaultAsk: 'always' })
  for (const code of extensionReasons.filter(levelSignOff)) {
    if (code !== 'human-review:unproven')
      areas.push({
        area: `extension:${code}`,
        label: code === 'always-human-path' ? 'Protected path' : code.slice('human-review:'.length),
        defaultAsk: 'always',
        source: 'extension',
      })
  }
  return {
    blockers: (gate.blockers ?? []).map((b) => ({ code: b.kind, label: b.summary })),
    evidence: {
      review: 'ran',
      collectors: [
        { name: 'changed-files', ok: Array.isArray(gate.changedPaths) },
        { name: 'size-baseline', ok: gate.sizeBaseline?.ok === true },
        ...(gate.judgeFailed ? [{ name: 'judge', ok: false }] : []),
      ],
    },
    areas: withJudgeAreas(areas, judgeAreas ?? []),
    ...(gate.range?.kind === 'delta'
      ? {}
      : { size: { changedLines: gate.changedLines, baseline: gate.sizeBaseline } }),
    proof: {
      ui: uiProof({ ciGreen: gate.ci?.green === true, coverage: gate.uiTests, outcomes }),
      ...(extensionReasons.includes('human-review:unproven') ? { behavior: 'unproven' } : {}),
    },
  }
}

/** The merged confidence contract requires size, including when an approved delta skips its cap.
 * @param {GateFacts} gate @param {{extensionReasons?: string[], outcomes?: import('./verify-ui-proof.mjs').ProofOutcome[], judgeAreas?: ConfidenceFacts['areas'], caps?: import('./verify-area-caps.mjs').AreaCap[]}} [options] @returns {ConfidenceResult}
 */
export function levelConfidence(gate, options) {
  const facts = levelFacts(gate, options)
  if (gate.range?.kind === 'delta') facts.size = { changedLines: 0, baseline: gate.sizeBaseline }
  const result = confidence(facts)
  const capped = facts.areas.filter((area) => area.source === 'cap')
  for (const ask of result.asks) {
    const area = capped.find((area) => area.area === ask.area)
    if (area) ask.cap = area.cap
  }
  result.reasons = [...capped.map((area) => capReason(area.cap)), ...result.reasons].slice(
    0,
    MAX_REASONS,
  )
  result.health.push(...(gate.areaCaps?.health ?? []))
  if (gate.sizeBaseline?.ok && gate.sizeBaseline.stale) result.health.push('size-baseline-stale')
  const holds = holdFacts(gate, result)
  if (holds) result.holds = holds
  return result
}

/**
 * @typedef {{area: string, label: string, code?: string, extension: string, outcome: 'breaking'|'failed', items?: string[], more?: number, reason?: string}} DescriberHold
 * @typedef {{policy: ({kind: 'unreadable', path: string, why: string}|({kind: 'rule'} & Omit<import('./verify-merge-prose.mjs').PolicyDecision, 'decision'>))[], describers: DescriberHold[]}} HoldFacts
 */

/** The bounded facts explaining only policy and describer holds.
 * @param {GateFacts} gate @param {ConfidenceResult} result @returns {HoldFacts|null}
 */
export function holdFacts(gate, result) {
  const prose = gate.prosePolicy
  /** @type {HoldFacts['policy']} */
  const policy =
    prose?.state === 'unreadable'
      ? [{ kind: 'unreadable', path: prose.path ?? '', why: prose.why ?? '' }]
      : (prose?.decisions ?? [])
          .filter((entry) => entry.decision === 'human')
          .slice(0, MERGE_POLICY_LIMITS.maxDecisions)
          .map(({ decision: _decision, ...entry }) => ({ kind: 'rule', ...entry }))
  /** @type {DescriberHold[]} */
  const describers = []
  for (const area of gate.areas ?? []) {
    const ask = result.asks.find((ask) => ask.area === area.area)
    if (!ask) continue
    const code = legacyCodes({ ...result, asks: [ask] }, gate.review)[0]
    for (const entry of area.describers ?? []) {
      const items = entry.items ?? []
      describers.push({
        area: area.area,
        label: area.label,
        ...(code ? { code } : {}),
        extension: entry.extension,
        outcome: entry.outcome,
        ...(entry.outcome === 'failed'
          ? { reason: entry.reason }
          : {
              items: items.slice(0, 3),
              ...(items.length > 3 ? { more: items.length - 3 } : {}),
            }),
      })
      if (describers.length === 8) break
    }
    if (describers.length === 8) break
  }
  return policy.length || describers.length ? { policy, describers } : null
}

/** @param {ConfidenceResult} result @param {{alwaysReview?: {name:string,parkCode:string}[]}} [review] */
export function legacyCodes(result, review = {}) {
  if (result.level === 'cannot-assess') return ['cannot-assess']
  if (result.level === 'high' || result.level === 'blocked') return []
  const codes = result.asks.map((entry) => {
    const ask =
      entry.source === 'cap'
        ? {
            ...entry,
            area: entry.cap.key,
            source: entry.cap.key.startsWith('config:') ? 'config' : 'builtin',
          }
        : entry
    if (ask.source === 'config') {
      const entries = review.alwaysReview ?? []
      const entry = entries[configAreaIds(entries).indexOf(canonicalAreaId(ask.area))]
      return entry?.parkCode ?? 'always-human-path'
    }
    if (ask.area.startsWith('extension:')) return ask.area.slice('extension:'.length)
    if (ask.source === 'judge') return 'human-review:judge'
    return (
      {
        migrations: 'human-review:database',
        'api-contract': 'human-review:api-contract',
        infra: 'human-review:infra',
        ui: 'human-review:ui',
        'unproven-behavior': 'human-review:unproven',
      }[ask.area] ?? 'human-review:asks'
    )
  })
  if (codes.length === 0) codes.push('human-review:size')
  return SIGN_OFF_CODES.filter((code) => codes.includes(code))
}

/** @param {ConfidenceResult} result */
export function levelLabel(result) {
  const labels = result.asks.map((ask) => ask.label)
  const named =
    labels.slice(0, MAX_ASKS_BEFORE_LOW).join(', ') +
    (labels.length > MAX_ASKS_BEFORE_LOW ? ` +${labels.length - MAX_ASKS_BEFORE_LOW}` : '')
  const size = result.reasons
    .map((reason) => /Changes ([\d,]+) lines/.exec(reason)?.[1])
    .find(Boolean)
  return `${result.level}: ${named || (size ? `${size} lines` : result.reasons.join(' ')) || 'Review evidence'}`
}

/** @param {{codes:string[]|null,label?:string}|null} park @returns {ConfidenceResult['level']|null} */
export function parkLevel(park) {
  const labelled = /^(high|moderate|low):/.exec(park?.label ?? '')?.[1]
  if (labelled === 'high' || labelled === 'moderate' || labelled === 'low') return labelled
  if (park?.codes?.includes('cannot-assess')) return 'cannot-assess'
  return park?.codes?.length &&
    park.codes.every((code) => levelSignOff(code) || ['no-receipt', 'human-merge'].includes(code))
    ? 'moderate'
    : null
}

/** @param {import('./verify-area-caps.mjs').AreaCap} cap */
function capBanner(cap) {
  const escapes = cap.escapes
    .map((escape) => `#${escape.original} (fixed by #${escape.fix})`)
    .join(', ')
  const key = /^[\w./:-]+$/.test(cap.key)
    ? cap.key
    : `"${cap.key.replaceAll('\\', '\\\\').replaceAll('"', '\\"').replaceAll('$', '\\$').replaceAll('`', '\\`')}"`
  return `Area cap: ${cap.label} since ${cap.cappedAt.slice(0, 10)} after ${escapes}. Clear: \`node "$BOSS_VERIFY_TOOLBOX/verify/verify-area-caps-sync.mjs" clear --area ${key} --reason "<reason>"\`.`
}

/** @param {ConfidenceResult} result */
export function levelNoteLead(result) {
  return [
    ...result.asks.filter((ask) => ask.cap).map((ask) => capBanner(ask.cap)),
    levelLabel(result),
    '',
    ...result.reasons.slice(0, MAX_REASONS).map((reason) => `- ${reason}`),
    ...(result.level === 'low'
      ? ['', ...result.asks.map((ask) => `- [ ] Review ${ask.label}`)]
      : []),
  ]
}

/** @param {unknown} value @returns {value is ConfidenceResult} */
export function validConfidence(value) {
  if (!value || typeof value !== 'object') return false
  const v = /** @type {ConfidenceResult} */ (value)
  return (
    [...LEVELS, ...STATES].includes(v.level) &&
    Array.isArray(v.asks) &&
    v.asks.every(
      (ask) =>
        ask &&
        typeof ask.area === 'string' &&
        typeof ask.label === 'string' &&
        typeof ask.source === 'string',
    ) &&
    Array.isArray(v.reasons) &&
    v.reasons.length <= MAX_REASONS &&
    v.reasons.every((reason) => typeof reason === 'string')
  )
}
