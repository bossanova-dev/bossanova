/**
 * `proof-spec/1`: what the proof-author agent wrote for one judged head. Validation keeps every
 * valid item and drops each invalid one with its reason, so bad agent output never fails a page.
 */

export const PROOF_SPEC_SCHEMA = 'proof-spec/1'
export const SPEC_TITLE_CHARS = 110
export const SPEC_TEXT_CHARS = 300
export const SPEC_FACT_CHARS = 90
export const MAX_SPEC_RISKS = 12
export const MAX_SPEC_FACTS = 6
export const MAX_SPEC_SNIPPETS = 3
export const MIN_SNIPPET_LINES = 4
export const MAX_SNIPPET_LINES = 16
export const MAX_SPEC_LIST = 20
export const MAX_SPEC_AREAS = MAX_SPEC_LIST
export const SPEC_LABEL_CHARS = 40
export const RISK_LEVELS = Object.freeze(['high', 'medium', 'low', 'deploy'])
export const EVIDENCE_KINDS = Object.freeze(['still', 'clip', 'transcript'])

/**
 * @typedef {{at: string, reason: string}} Dropped
 * @typedef {{title: string, detail?: string, area?: string}} SpecWaiting
 * @typedef {{level: string, title: string, body: string, ifItMatters?: string, label?: string, at: {path: string, line: number}}} SpecRisk
 * @typedef {{path: string, start: number, end: number, pinLine?: number, pinNote?: string}} SpecSnippet
 * @typedef {{id: string, name: string, summary: string, facts: {text: string, flag: boolean}[], snippets: SpecSnippet[]}} SpecArea
 * @typedef {{text: string, done: boolean}} SpecCheck
 * @typedef {{severity: string, at?: string, text: string}} SpecNote
 * @typedef {{claim: string, kind: string, how: string}} SpecPlan
 * @typedef {{schema: string, head: string, title?: string, waiting: SpecWaiting[], risks: SpecRisk[],
 *   areas: SpecArea[], checklist: {before: SpecCheck[], after: SpecCheck[]}, notes: SpecNote[],
 *   evidencePlan: SpecPlan[], uncertain: string[]}} ProofSpec
 * @typedef {{head: string, changedPaths?: Iterable<string> | null, areaIds?: Iterable<string> | null}} SpecContext
 */

/** @param {unknown} value @returns {value is Record<string, unknown>} */
export const isRecord = (value) =>
  value !== null && typeof value === 'object' && !Array.isArray(value)

/** Thrown inside one item's parser to drop that item; never escapes `validateProofSpec`. */
export class Drop extends Error {}

/** @param {unknown} value @param {number} max @param {string} field */
export function needText(value, max, field) {
  if (typeof value !== 'string' || !value.trim()) throw new Drop(`${field}: expected text`)
  if (value.length > max) throw new Drop(`${field}: longer than ${max} characters`)
  return value
}
/** @param {unknown} value @param {number} max @param {string} field */
export const optText = (value, max, field) =>
  value === undefined ? undefined : needText(value, max, field)
/** @param {unknown} value @param {string} field */
export function needLine(value, field) {
  if (!Number.isSafeInteger(value) || /** @type {number} */ (value) < 1)
    throw new Drop(`${field}: expected a line number`)
  return /** @type {number} */ (value)
}
/** @param {unknown} value @param {readonly string[]} keys @param {string} [field] */
export function onlyKeys(value, keys, field) {
  if (!isRecord(value)) throw new Drop(`${field ?? 'entry'}: expected object`)
  const extra = Object.keys(value).find((key) => !keys.includes(key))
  if (extra) throw new Drop(`${field ? `${field}.` : ''}${extra}: unknown key`)
  return value
}
/** Drop undefined optional fields so a kept item has only the keys it carries.
 * @template {Record<string, unknown>} T @param {T} item @returns {T} */
export const compact = (item) =>
  /** @type {T} */ (Object.fromEntries(Object.entries(item).filter(([, v]) => v !== undefined)))

/**
 * Parse each entry of `value` with `parse`; keep what parses, record each failure in `dropped`.
 * @template T
 * @param {unknown} value @param {string} at @param {number} cap
 * @param {(entry: unknown, at: string) => T} parse @param {Dropped[]} dropped
 * @returns {T[]}
 */
export function keepEach(value, at, cap, parse, dropped) {
  if (value === undefined) return []
  if (!Array.isArray(value)) {
    dropped.push({ at, reason: 'expected a list' })
    return []
  }
  /** @type {T[]} */
  const kept = []
  value.forEach((entry, i) => {
    const where = `${at}[${i}]`
    if (kept.length >= cap) {
      dropped.push({ at: where, reason: `over the cap of ${cap}` })
      return
    }
    try {
      kept.push(parse(entry, where))
    } catch (error) {
      if (!(error instanceof Drop)) throw error
      dropped.push({ at: where, reason: error.message })
    }
  })
  return kept
}

const TOP_KEYS = Object.freeze([
  'schema',
  'head',
  'title',
  'waiting',
  'risks',
  'areas',
  'checklist',
  'notes',
  'evidencePlan',
  'uncertain',
])

/**
 * Validate an agent's `proof-spec/1` against the judged head. A spec for another head, or one
 * that is not a spec at all, is ignored whole; otherwise each invalid item is dropped alone.
 * A risk must cite a file in `changedPaths`; an area or a waiting reason's `area` must be one of
 * `areaIds` when given. Line-level snippet checks need git and happen when the page quotes them.
 * @param {unknown} value @param {SpecContext} context
 * @returns {{spec: ProofSpec | null, dropped: Dropped[]}}
 */
export function validateProofSpec(value, { head, changedPaths = null, areaIds = null }) {
  /** @type {Dropped[]} */
  const dropped = []
  if (!isRecord(value) || value.schema !== PROOF_SPEC_SCHEMA)
    return { spec: null, dropped: [{ at: 'spec', reason: `not a ${PROOF_SPEC_SCHEMA} document` }] }
  if (typeof value.head !== 'string' || value.head.toLowerCase() !== head.toLowerCase())
    return { spec: null, dropped: [{ at: 'spec.head', reason: 'written for another head' }] }
  for (const key of Object.keys(value))
    if (!TOP_KEYS.includes(key)) dropped.push({ at: `spec.${key}`, reason: 'unknown key' })
  const changed = changedPaths ? new Set(changedPaths) : null
  const areas = areaIds ? new Set(areaIds) : null
  /** @param {unknown} id @param {string} field */
  const knownArea = (id, field) => {
    const text = needText(id, SPEC_TEXT_CHARS, field)
    if (areas && !areas.has(text)) throw new Drop(`${field}: ${text} is not a judged area`)
    return text
  }
  let title
  try {
    title = optText(value.title, SPEC_TITLE_CHARS, 'title')
  } catch (error) {
    if (!(error instanceof Drop)) throw error
    dropped.push({ at: 'spec.title', reason: error.message })
  }
  const waiting = keepEach(
    value.waiting,
    'waiting',
    MAX_SPEC_LIST,
    (entry) => {
      const w = onlyKeys(entry, ['title', 'detail', 'area'])
      return compact({
        title: needText(w.title, SPEC_TEXT_CHARS, 'title'),
        detail: optText(w.detail, SPEC_TEXT_CHARS, 'detail'),
        area: w.area === undefined ? undefined : knownArea(w.area, 'area'),
      })
    },
    dropped,
  )
  const risks = keepEach(
    value.risks,
    'risks',
    MAX_SPEC_RISKS,
    (entry) => {
      const r = onlyKeys(entry, ['level', 'title', 'body', 'ifItMatters', 'label', 'at'])
      if (typeof r.level !== 'string' || !RISK_LEVELS.includes(r.level))
        throw new Drop(`level: expected one of ${RISK_LEVELS.join(', ')}`)
      const at = onlyKeys(r.at, ['path', 'line'], 'at')
      const path = needText(at.path, SPEC_TEXT_CHARS, 'at.path')
      if (!changed)
        throw new Drop('at.path: the changed files are unknown, so it cannot be checked')
      if (!changed.has(path)) throw new Drop(`at.path: ${path} is not changed by this PR`)
      return compact({
        level: r.level,
        title: needText(r.title, SPEC_TEXT_CHARS, 'title'),
        body: needText(r.body, SPEC_TEXT_CHARS, 'body'),
        ifItMatters: optText(r.ifItMatters, SPEC_TEXT_CHARS, 'ifItMatters'),
        label: optText(r.label, SPEC_LABEL_CHARS, 'label'),
        at: { path, line: needLine(at.line, 'at.line') },
      })
    },
    dropped,
  )
  const seenAreas = new Set()
  const specAreas = keepEach(
    value.areas,
    'areas',
    MAX_SPEC_AREAS,
    (entry, at) => {
      const a = onlyKeys(entry, ['id', 'name', 'summary', 'facts', 'snippets'])
      const id = knownArea(a.id, 'id')
      if (seenAreas.has(id)) throw new Drop(`id: ${id} appears twice`)
      seenAreas.add(id)
      const facts = keepEach(
        a.facts,
        `${at}.facts`,
        MAX_SPEC_FACTS,
        (fact) => {
          const f = onlyKeys(fact, ['text', 'flag'])
          if (f.flag !== undefined && typeof f.flag !== 'boolean')
            throw new Drop('flag: expected boolean')
          return { text: needText(f.text, SPEC_FACT_CHARS, 'text'), flag: f.flag === true }
        },
        dropped,
      )
      const snippets = keepEach(
        a.snippets,
        `${at}.snippets`,
        MAX_SPEC_SNIPPETS,
        (snippet) => {
          const s = onlyKeys(snippet, ['path', 'start', 'end', 'pinLine', 'pinNote'])
          const path = needText(s.path, SPEC_TEXT_CHARS, 'path')
          if (!changed)
            throw new Drop('path: the changed files are unknown, so it cannot be checked')
          if (!changed.has(path)) throw new Drop(`path: ${path} is not changed by this PR`)
          const start = needLine(s.start, 'start')
          const end = needLine(s.end, 'end')
          const lines = end - start + 1
          if (lines < MIN_SNIPPET_LINES || lines > MAX_SNIPPET_LINES)
            throw new Drop(
              `start..end: ${lines} lines, expected ${MIN_SNIPPET_LINES} to ${MAX_SNIPPET_LINES}`,
            )
          return compact({
            path,
            start,
            end,
            pinLine: s.pinLine === undefined ? undefined : needLine(s.pinLine, 'pinLine'),
            pinNote: optText(s.pinNote, SPEC_TEXT_CHARS, 'pinNote'),
          })
        },
        dropped,
      )
      return {
        id,
        name: needText(a.name, SPEC_LABEL_CHARS, 'name'),
        summary: needText(a.summary, SPEC_TEXT_CHARS, 'summary'),
        facts,
        snippets,
      }
    },
    dropped,
  )
  /** @param {string} at @returns {SpecCheck[]} */
  const checks = (at) =>
    keepEach(
      isRecord(value.checklist) ? value.checklist[at] : undefined,
      `checklist.${at}`,
      MAX_SPEC_LIST,
      (entry) => {
        const c = onlyKeys(entry, ['text', 'done'])
        if (c.done !== undefined && typeof c.done !== 'boolean')
          throw new Drop('done: expected boolean')
        return { text: needText(c.text, SPEC_TEXT_CHARS, 'text'), done: c.done === true }
      },
      dropped,
    )
  if (value.checklist !== undefined) {
    if (!isRecord(value.checklist)) dropped.push({ at: 'checklist', reason: 'expected object' })
    else
      for (const key of Object.keys(value.checklist))
        if (key !== 'before' && key !== 'after')
          dropped.push({ at: `checklist.${key}`, reason: 'unknown key' })
  }
  const checklist = { before: checks('before'), after: checks('after') }
  const notes = keepEach(
    value.notes,
    'notes',
    MAX_SPEC_LIST,
    (entry) => {
      const n = onlyKeys(entry, ['severity', 'at', 'text'])
      return compact({
        severity: needText(n.severity, 8, 'severity'),
        at: optText(n.at, SPEC_TEXT_CHARS, 'at'),
        text: needText(n.text, SPEC_TEXT_CHARS, 'text'),
      })
    },
    dropped,
  )
  const evidencePlan = keepEach(
    value.evidencePlan,
    'evidencePlan',
    MAX_SPEC_LIST,
    (entry) => {
      const e = onlyKeys(entry, ['claim', 'kind', 'how'])
      if (typeof e.kind !== 'string' || !EVIDENCE_KINDS.includes(e.kind))
        throw new Drop(`kind: expected one of ${EVIDENCE_KINDS.join(', ')}`)
      return {
        claim: needText(e.claim, SPEC_TEXT_CHARS, 'claim'),
        kind: e.kind,
        how: needText(e.how, SPEC_TEXT_CHARS, 'how'),
      }
    },
    dropped,
  )
  const uncertain = keepEach(
    value.uncertain,
    'uncertain',
    MAX_SPEC_LIST,
    (entry) => needText(entry, SPEC_TEXT_CHARS, 'text'),
    dropped,
  )
  return {
    spec: compact({
      schema: PROOF_SPEC_SCHEMA,
      head: value.head.toLowerCase(),
      title,
      waiting,
      risks,
      areas: specAreas,
      checklist,
      notes,
      evidencePlan,
      uncertain,
    }),
    dropped,
  }
}
