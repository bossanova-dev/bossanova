/**
 * The page model's review parts, built from the author's spec: the lead under the title, the
 * banner rows merged with the describers', the bug-fix account, what the tests prove, and the
 * guardrails, refactorings and review noise. With no spec lead, the lead is the PR body's first
 * paragraph.
 */
import { capText } from './verify-proof.mjs'
import { SIGNAL_KEYS } from './verify-proof-spec-review.mjs'

export const LEAD_PREFIX = Object.freeze(
  /** @type {Record<string, string>} */ ({
    'bug-fix': 'Bug fix.',
    feature: 'Feature.',
    dependency: 'Dependency update.',
    refactor: 'Refactor.',
    infrastructure: 'Infrastructure.',
    docs: 'Docs.',
    tests: 'Tests.',
    chore: 'Chore.',
  }),
)
export const PR_LEAD_CHARS = 400
export const PR_TITLE_CHARS = 110
/** The built-in area each author signal key speaks for, so its row replaces that area's. */
export const SIGNAL_AREAS = Object.freeze(
  /** @type {Record<string, string>} */ ({
    database: 'migrations',
    api: 'api-contract',
    security: 'security',
    dependencies: 'dependencies',
    config: 'infra',
    ui: 'ui',
    'agent-rules': 'agent-rules',
  }),
)
/** The signal key of an area's banner row: the author key that speaks for it, else its id. @param {string} area */
export const signalKeyOf = (area) =>
  Object.keys(SIGNAL_AREAS).find((key) => SIGNAL_AREAS[key] === area) ?? area
const SIGNAL_LABELS = Object.freeze(
  /** @type {Record<string, string>} */ ({
    database: 'Database',
    api: 'API',
    security: 'Security',
    dependencies: 'Dependencies',
    config: 'Config and deploy',
    ui: 'UI',
    'agent-rules': 'Agent rules',
    revert: 'Revert',
    'bug-fix': 'Bug fix',
    tests: 'Tests',
    checks: 'Checks',
  }),
)
/** The section a row with no area links to. */
const SIGNAL_SECTIONS = Object.freeze(
  /** @type {Record<string, string>} */ ({
    revert: 'after',
    'bug-fix': 'fix',
    tests: 'tests',
    checks: 'tests',
  }),
)
const PILL_TONES = Object.freeze(
  /** @type {Record<string, string>} */ ({
    safe: 'success',
    additive: 'success',
    proven: 'success',
    reported: 'info',
    breaking: 'danger',
  }),
)
const SEVERITY = ['muted', 'success', 'info', 'warning', 'danger']
const FIX_LABELS = Object.freeze(
  /** @type {Record<string, string>} */ ({
    symptom: 'Symptom',
    'root-cause': 'Root cause',
    reproduction: 'Reproduction',
    before: 'Before the fix',
    after: 'After the fix',
    'not-checked': 'Not checked',
    'left-open': 'Left open',
  }),
)
const FIX_TONES = Object.freeze(
  /** @type {Record<string, string>} */ ({
    before: 'info',
    after: 'success',
    'not-checked': 'warning',
    'left-open': 'warning',
  }),
)
const FIX_SOURCES = Object.freeze(
  /** @type {Record<string, string>} */ ({
    reported: "the author's report",
    diff: 'the diff',
    ci: 'CI',
  }),
)

/**
 * @typedef {import('./verify-proof-page-model.mjs').Pill} Pill
 * @typedef {import('./verify-proof-page-model.mjs').Signal} Signal
 * @typedef {import('./verify-proof-page-model.mjs').Link} Link
 * @typedef {import('./verify-proof-spec.mjs').ProofSpec} ProofSpec
 * @typedef {import('./verify-proof-spec-review.mjs').SpecSignal} SpecSignal
 * @typedef {{prefix?: string, text: string}} Lead
 * @typedef {{sub?: string, rows: {label: string, text: string, tone?: string}[]}} BugFix
 * @typedef {{id: string, ref: string, kind?: string, title: string, body: string, proposal?: string, evidence?: string}} ReviewNote
 * @typedef {ReviewNote & {specLinks: string[]}} DraftNote
 * @typedef {{guardrails: DraftNote[], refactoring: DraftNote[], noise: DraftNote[]}} DraftNotes
 */

/** `a`, `a and b`, `a, b and c`. @param {string[]} items */
const joinList = (items) =>
  items.length < 2 ? (items[0] ?? '') : `${items.slice(0, -1).join(', ')} and ${items.at(-1)}`

/** An author pill word as the page's pill. @param {string} word @returns {Pill} */
export const authorPill = (word) => ({ word, tone: PILL_TONES[word] ?? 'warning' })

/** @param {Pill} pill */
const severity = (pill) => SEVERITY.indexOf(pill.tone)

/**
 * The banner rows with the author's merged in. An author row replaces the summary of the row with
 * its key and keeps the more severe pill of the two, the author's on a tie; one with a key no row
 * has joins after them, in `SIGNAL_KEYS` order, linking the section that shows it when the page
 * has one.
 * @param {Signal[]} signals @param {SpecSignal[]} authored @param {Set<string>} anchors the page's section ids
 * @returns {Signal[]}
 */
export function mergeSignals(signals, authored, anchors) {
  const mine = new Map(authored.map((s) => [s.key, s]))
  const merged = signals.map((signal) => {
    const own = mine.get(signal.key)
    if (!own) return signal
    mine.delete(signal.key)
    const pill = authorPill(own.pill)
    return {
      ...signal,
      summary: own.summary,
      pill: severity(pill) >= severity(signal.pill) ? pill : signal.pill,
    }
  })
  const added = SIGNAL_KEYS.flatMap((key) => {
    const own = mine.get(key)
    if (!own) return []
    const anchor = SIGNAL_AREAS[key] ? `a-${SIGNAL_AREAS[key]}` : SIGNAL_SECTIONS[key]
    return [
      {
        key,
        label: SIGNAL_LABELS[key],
        summary: own.summary,
        pill: authorPill(own.pill),
        href: anchors.has(anchor) ? `#${anchor}` : null,
      },
    ]
  })
  return [...merged, ...added]
}

/**
 * The first paragraph of a PR body as plain prose: comments, code blocks, headings, rules and
 * images left out, links as their text, emphasis and list markers removed, code spans kept.
 * Null when the body has no prose.
 * @param {string | undefined} body
 */
export function bodyLead(body) {
  const paragraphs = (body ?? '')
    .replace(/<!--[\s\S]*?(-->|$)/g, '\n\n')
    .replace(/^(```|~~~)[\s\S]*?(^\1|(?![\s\S]))/gm, '\n\n')
    .replace(/\r/g, '')
    .split(/\n\s*\n|\n(?=#{1,6}\s)/)
  for (const paragraph of paragraphs) {
    const text = paragraph
      .split('\n')
      .filter((line) => !/^\s*(#{1,6}\s|[-*_]{3,}\s*$|\|)/.test(line))
      .map((line) => line.replace(/^\s*(>\s*)*([-*+]|\d+[.)])?\s+/, ''))
      .join(' ')
      .replace(/!\[[^\]]*\]\([^)]*\)/g, '')
      .replace(/\[([^\]]+)\]\([^)]*\)/g, '$1')
      .replace(/<[^>]+>/g, '')
      .replace(/(\*\*|__)(.+?)\1/g, '$2')
      .replace(/(^|[\s(])[*_]([^*_\s][^*_]*?)[*_](?=[\s).,;:!?]|$)/g, '$1$2')
      .replace(/\s+/g, ' ')
      .trim()
    if (/[\p{L}\p{N}]/u.test(text)) return capText(text, PR_LEAD_CHARS)
  }
  return null
}

/**
 * The header's lead: the author's, opened by its kind, else the PR body's first paragraph.
 * @param {ProofSpec | null} spec @param {string | undefined} body @returns {Lead | null}
 */
export function pageLead(spec, body) {
  if (spec?.lead) {
    const prefix = LEAD_PREFIX[spec.lead.kind]
    const text = spec.lead.text.trim()
    return {
      prefix,
      text: text.toLowerCase().startsWith(prefix.toLowerCase())
        ? text.slice(prefix.length).trim()
        : text,
    }
  }
  const text = bodyLead(body)
  return text ? { text } : null
}

/** The page's title: the author's, else the PR's cut to 110 characters. @param {ProofSpec | null} spec @param {string | undefined} title */
export const pageTitle = (spec, title) =>
  spec?.title ?? (title?.trim() ? capText(title.trim(), PR_TITLE_CHARS) : null)

/**
 * The guardrail, refactoring and noise items, each anchored by its lower-case id, with the ids
 * it links to still to resolve.
 * @param {ProofSpec | null} spec @returns {DraftNotes}
 */
export function reviewNotes(spec) {
  /** @param {import('./verify-proof-spec-review.mjs').SpecReviewItem[] | undefined} items @returns {DraftNote[]} */
  const draft = (items) =>
    (items ?? []).map(({ id, links, ...item }) => ({
      id: id.toLowerCase(),
      ref: id,
      ...item,
      specLinks: links,
    }))
  return {
    guardrails: draft(spec?.guardrails),
    refactoring: draft(spec?.refactoring),
    noise: draft(spec?.noise),
  }
}

/**
 * The review sections of the model, their items' links resolved by `linksOf`.
 * @param {ProofSpec | null} spec @param {DraftNotes} notes @param {(id: string) => Link[]} linksOf
 */
export function reviewSections(spec, notes, linksOf) {
  /** @param {DraftNote[]} items */
  const linked = (items) =>
    items.map(({ specLinks, ...item }) => ({ ...item, links: linksOf(item.id) }))
  const rows = spec?.bugFix?.rows ?? []
  const sources = [...new Set(rows.flatMap((row) => (row.source ? [FIX_SOURCES[row.source]] : [])))]
  /** @type {BugFix | null} */
  const bugFix = rows.length
    ? {
        ...(sources.length ? { sub: `from ${joinList(sources)}` } : {}),
        rows: rows.map((row) => ({
          label: FIX_LABELS[row.label],
          text: row.text,
          ...(FIX_TONES[row.label] ? { tone: FIX_TONES[row.label] } : {}),
        })),
      }
    : null
  return {
    bugFix,
    testing: spec?.testing ?? null,
    guardrails: linked(notes.guardrails),
    refactoring: linked(notes.refactoring),
    noise: linked(notes.noise),
  }
}
