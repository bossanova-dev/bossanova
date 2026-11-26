/**
 * The parts of `proof-spec/1` that say what kind of change this is and what would help next time:
 * the lead paragraph, the author's banner rows, the bug-fix account, what the tests prove, and the
 * guardrails, refactorings and review noise the change suggests. Like the rest of the spec, each
 * invalid item is dropped alone with its reason.
 */
import { Drop, compact, keepEach, needText, onlyKeys, optText } from './verify-proof-spec-parse.mjs'

export const LEAD_KINDS = Object.freeze([
  'bug-fix',
  'feature',
  'refactor',
  'dependency',
  'infrastructure',
  'docs',
  'tests',
  'chore',
])
/** The banner rows an author may write, in the order new ones join the banner. */
export const SIGNAL_KEYS = Object.freeze([
  'database',
  'api',
  'security',
  'dependencies',
  'config',
  'ui',
  'agent-rules',
  'revert',
  'bug-fix',
  'tests',
  'checks',
])
export const SIGNAL_PILLS = Object.freeze([
  'safe',
  'additive',
  'to review',
  'behaviour change',
  'breaking',
  'manual step',
  'not recorded',
  'proven',
  'reported',
  'unproven',
  'risky',
])
export const BUG_FIX_LABELS = Object.freeze([
  'symptom',
  'root-cause',
  'reproduction',
  'before',
  'after',
  'not-checked',
  'left-open',
])
export const BUG_FIX_SOURCES = Object.freeze(['reported', 'diff', 'ci'])
export const GUARDRAIL_KINDS = Object.freeze(['lint', 'check', 'config', 'rule'])
export const REVIEW_KEYS = Object.freeze([
  'lead',
  'signals',
  'bugFix',
  'testing',
  'guardrails',
  'refactoring',
  'noise',
])
export const LEAD_CHARS = 600
export const SIGNAL_CHARS = 160
export const MAX_BUG_FIX_ROWS = 8
export const MAX_REVIEW_ITEMS = 5
const TEXT_CHARS = 300
const TITLE_CHARS = 110
const TESTING_SUMMARY_CHARS = 500
const TESTING_ITEM_CHARS = 160
const TESTING_ALSO_CHARS = 200
const MAX_TESTING_ITEMS = 8
const MAX_TESTING_ALSO = 4

/**
 * @typedef {{kind: string, text: string}} SpecLead
 * @typedef {{key: string, summary: string, pill: string}} SpecSignal
 * @typedef {{label: string, text: string, source?: string}} SpecBugFixRow
 * @typedef {{summary?: string, proves: string[], gaps: string[], also: string[]}} SpecTesting
 * @typedef {{id: string, kind?: string, title: string, body: string, proposal?: string, evidence?: string, links: string[]}} SpecReviewItem
 * @typedef {{lead?: SpecLead, signals?: SpecSignal[], bugFix?: {rows: SpecBugFixRow[]}, testing?: SpecTesting,
 *   guardrails?: SpecReviewItem[], refactoring?: SpecReviewItem[], noise?: SpecReviewItem[]}} SpecReview
 */

/** @param {unknown} value @param {readonly string[]} allowed @param {string} field */
function oneOf(value, allowed, field) {
  if (typeof value !== 'string' || !allowed.includes(value))
    throw new Drop(`${field}: expected one of ${allowed.join(', ')}`)
  return value
}

/** @template T @param {T[]} list @returns {T[] | undefined} */
const nonEmpty = (list) => (list.length ? list : undefined)

/**
 * The review parts of a spec. Guardrail, refactoring and noise ids (G1, F1, N1) are claimed
 * page-wide like every other item id; the caller filters their links once every id is claimed.
 * @param {Record<string, unknown>} value the spec
 * @param {import('./verify-proof-spec.mjs').PartContext} ctx
 * @returns {SpecReview}
 */
export function reviewParts(value, { checkId, claim, linkList, dropped }) {
  /** One optional object key, dropped whole when it does not parse.
   * @template T @param {string} key @param {(entry: unknown) => T} parse @returns {T | undefined} */
  const one = (key, parse) => {
    if (value[key] === undefined) return undefined
    try {
      return parse(value[key])
    } catch (error) {
      if (!(error instanceof Drop)) throw error
      dropped.push({ at: key, reason: error.message })
      return undefined
    }
  }
  const lead = one('lead', (entry) => {
    const l = onlyKeys(entry, ['kind', 'text'], 'lead')
    return {
      kind: oneOf(l.kind, LEAD_KINDS, 'lead.kind'),
      text: needText(l.text, LEAD_CHARS, 'lead.text'),
    }
  })
  const keys = new Set()
  const signals = keepEach(
    value.signals,
    'signals',
    SIGNAL_KEYS.length,
    (entry) => {
      const s = onlyKeys(entry, ['key', 'summary', 'pill'])
      const key = oneOf(s.key, SIGNAL_KEYS, 'key')
      if (keys.has(key)) throw new Drop(`key: ${key} appears twice`)
      const signal = {
        key,
        summary: needText(s.summary, SIGNAL_CHARS, 'summary'),
        pill: oneOf(s.pill, SIGNAL_PILLS, 'pill'),
      }
      keys.add(key)
      return signal
    },
    dropped,
  )
  const bugFix = one('bugFix', (entry) => {
    const b = onlyKeys(entry, ['rows'], 'bugFix')
    const rows = keepEach(
      b.rows,
      'bugFix.rows',
      MAX_BUG_FIX_ROWS,
      (row) => {
        const r = onlyKeys(row, ['label', 'text', 'source'])
        return compact({
          label: oneOf(r.label, BUG_FIX_LABELS, 'label'),
          text: needText(r.text, TEXT_CHARS, 'text'),
          source: r.source === undefined ? undefined : oneOf(r.source, BUG_FIX_SOURCES, 'source'),
        })
      },
      dropped,
    )
    if (!rows.length) throw new Drop('bugFix.rows: nothing to show')
    return { rows }
  })
  const testing = one('testing', (entry) => {
    const t = onlyKeys(entry, ['summary', 'proves', 'gaps', 'also'], 'testing')
    /** @param {string} key @param {number} cap @param {number} max */
    const list = (key, cap, max) =>
      keepEach(t[key], `testing.${key}`, cap, (item) => needText(item, max, 'text'), dropped)
    let summary
    try {
      summary = optText(t.summary, TESTING_SUMMARY_CHARS, 'summary')
    } catch (error) {
      if (!(error instanceof Drop)) throw error
      dropped.push({ at: 'testing.summary', reason: error.message })
    }
    const parts = {
      proves: list('proves', MAX_TESTING_ITEMS, TESTING_ITEM_CHARS),
      gaps: list('gaps', MAX_TESTING_ITEMS, TESTING_ITEM_CHARS),
      also: list('also', MAX_TESTING_ALSO, TESTING_ALSO_CHARS),
    }
    if (!summary && !parts.proves.length && !parts.gaps.length && !parts.also.length)
      throw new Drop('testing: nothing to show')
    return compact({ summary, ...parts })
  })
  /** @param {string} key @param {string} prefix @param {{kinded?: boolean, linked?: boolean}} [shape] */
  const items = (key, prefix, { kinded = false, linked = true } = {}) =>
    keepEach(
      value[key],
      key,
      MAX_REVIEW_ITEMS,
      (entry, where) => {
        const i = onlyKeys(entry, [
          'id',
          ...(kinded ? ['kind'] : []),
          'title',
          'body',
          'proposal',
          'evidence',
          ...(linked ? ['links'] : []),
        ])
        const id = checkId(i.id, 'id')
        if (!new RegExp(`^${prefix}\\d{1,3}$`).test(id))
          throw new Drop(`id: expected ${prefix}1, ${prefix}2 and so on`)
        /** @type {SpecReviewItem} */
        const item = compact({
          id,
          kind: kinded ? oneOf(i.kind, GUARDRAIL_KINDS, 'kind') : undefined,
          title: needText(i.title, TITLE_CHARS, 'title'),
          body: needText(i.body, TEXT_CHARS, 'body'),
          proposal: optText(i.proposal, TEXT_CHARS, 'proposal'),
          evidence: optText(i.evidence, TEXT_CHARS, 'evidence'),
          links: linked ? linkList(i.links, where) : [],
        })
        claim(id)
        return item
      },
      dropped,
    )
  return compact({
    lead,
    signals: nonEmpty(signals),
    bugFix,
    testing,
    guardrails: nonEmpty(items('guardrails', 'G', { kinded: true })),
    refactoring: nonEmpty(items('refactoring', 'RF')),
    noise: nonEmpty(items('noise', 'N', { linked: false })),
  })
}
