/**
 * `proof-spec/1`: what the proof-author agent wrote for one judged head. Validation keeps every
 * valid item and drops each invalid one with its reason, so bad agent output never fails a page.
 * Every location must be in the diff, every link must name an item the page shows, and a
 * criterion is done only when its evidence resolves to something the head really has.
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
export const SPEC_REASON_CHARS = 200
export const MAX_SPEC_DECISIONS = 4
export const MAX_SPEC_LINKS = 8
export const RISK_LEVELS = Object.freeze(['high', 'medium', 'low'])
export const EVIDENCE_KINDS = Object.freeze(['still', 'clip', 'transcript', 'document'])
export const CRITERION_STATUSES = Object.freeze(['done', 'unverified', 'missed', 'skipped'])
/** The kinds of criterion evidence: all but `code` can prove a criterion done. */
export const PROOF_KINDS = Object.freeze(['test', 'ci', 'acceptance', 'finding', 'code'])
export const TEST_PATH = /(_test\.go$|\.test\.|\.spec\.|__tests__\/|(^|\/)e2e\/)/
/** A test file by its name, which criterion evidence must cite: a fixture under a test folder is not one. */
const TEST_FILE = /(_test\.go$|\.(test|spec)\.[^/]+$|__tests__\/[^/]+\.[^/]+$)/
/** The pipeline's own words, which a reader-facing reason never uses. */
export const PIPELINE_WORDS =
  /\b(verif\w*|judg\w*|areas?|ask(s|ed|ing)?|polic(y|ies)|never-?review|(no-?)?sign-?off|boss-\w+)\b/i
const SPEC_ID = /^[A-Za-z][\w.:-]{0,79}$/
export const SEVERITY_CHARS = 12
/** Severity words reviewers use, as the P-level the page shows. */
export const SEVERITY_LEVELS = Object.freeze(
  /** @type {Record<string, string>} */ ({
    critical: 'P0',
    blocker: 'P0',
    high: 'P1',
    important: 'P1',
    major: 'P1',
    medium: 'P2',
    moderate: 'P2',
    low: 'P3',
    minor: 'P3',
    nit: 'P3',
    nitpick: 'P3',
    suggestion: 'P3',
  }),
)

/**
 * @typedef {{at: string, reason: string}} Dropped
 * @typedef {{title: string, detail?: string, area?: string}} SpecWaiting
 * @typedef {{path: string, line: number}} SpecAt
 * @typedef {{id?: string, level: string, title: string, impact: string, guard?: string, at?: SpecAt, links: string[]}} SpecRisk
 * @typedef {{id: string, question: string, why: string, suggestion: string, at: SpecAt, links: string[]}} SpecDecision
 * @typedef {{kind: string, label: string, ref: string}} SpecEvidence
 * @typedef {{id: string, text: string, status: string, note?: string, evidence: SpecEvidence[]}} SpecCriterion
 * @typedef {{text: string, why: string, followUp?: string}} SpecSkipped
 * @typedef {'complete' | 'partial' | 'incomplete'} TicketVerdict
 * @typedef {{verdict: TicketVerdict | null, summary?: string, criteria: SpecCriterion[], skipped: SpecSkipped[], contradictions: string[]}} SpecTicketFit
 * @typedef {{path: string, start: number, end: number, pinLine?: number, pinNote?: string}} SpecSnippet
 * @typedef {{id: string, name: string, summary: string, facts: {text: string, flag: boolean}[], snippets: SpecSnippet[]}} SpecArea
 * @typedef {{text: string, done: boolean}} SpecCheck
 * @typedef {{severity: string, at?: string, text: string}} SpecNote
 * @typedef {{claim: string, kind: string, how: string}} SpecPlan
 * @typedef {{schema: string, head: string, title?: string, reason?: string, waiting: SpecWaiting[],
 *   risks: SpecRisk[], decisions: SpecDecision[], ticketFit?: SpecTicketFit, areas: SpecArea[],
 *   checklist: {after: SpecCheck[]}, notes: SpecNote[], evidencePlan: SpecPlan[],
 *   uncertain: string[]}} ProofSpec
 * @typedef {{head: string, changedPaths?: Iterable<string> | null, areaIds?: Iterable<string> | null,
 *   hasLine?: (path: string, line: number) => boolean | null, checks?: Iterable<string> | null,
 *   acceptance?: {id: string, result: string, head: string}[], findingIds?: Iterable<string>,
 *   addedPaths?: Iterable<string> | null}} SpecContext
 *   `checks` are the names of the checks that passed on the head, `acceptance` the rows review
 *   certified and `findingIds` the ids of review's findings; criterion evidence resolves to them.
 *   `addedPaths` are the changed files that gained lines; a cited test must be one when given.
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
 * Parse each entry of `value` with `parse`, which also gets the entry's index in `value`; keep
 * what parses, record each failure in `dropped`.
 * @template T
 * @param {unknown} value @param {string} at @param {number} cap
 * @param {(entry: unknown, at: string, index: number) => T} parse @param {Dropped[]} dropped
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
      kept.push(parse(entry, where, i))
    } catch (error) {
      if (!(error instanceof Drop)) throw error
      dropped.push({ at: where, reason: error.message })
    }
  })
  return kept
}

/** A note's severity: optional, a known word as its P-level, and dropped alone (keeping the note)
 * when it is not short text. @param {unknown} value @param {string} at @param {Dropped[]} dropped */
function noteSeverity(value, at, dropped) {
  if (value === undefined || (typeof value === 'string' && !value.trim())) return ''
  if (typeof value === 'string' && SEVERITY_LEVELS[value.trim().toLowerCase()])
    return SEVERITY_LEVELS[value.trim().toLowerCase()]
  if (typeof value === 'string' && value.trim().length <= SEVERITY_CHARS) return value.trim()
  dropped.push({ at, reason: `expected at most ${SEVERITY_CHARS} characters, severity left out` })
  return ''
}

const TOP_KEYS = Object.freeze([
  'schema',
  'head',
  'title',
  'reason',
  'waiting',
  'risks',
  'decisions',
  'ticketFit',
  'areas',
  'checklist',
  'notes',
  'evidencePlan',
  'uncertain',
])

/**
 * A ticket's verdict from its criteria: complete when every one is done, partial when some are,
 * incomplete when none is; null with no criteria.
 * @param {{status: string}[]} criteria @returns {TicketVerdict | null}
 */
export function ticketVerdict(criteria) {
  if (!criteria.length) return null
  const done = criteria.filter((c) => c.status === 'done').length
  return done === criteria.length ? 'complete' : done ? 'partial' : 'incomplete'
}

/** `path` or `path:line`. @param {string} ref */
const splitRef = (ref) => {
  const m = /^(.+?)(?::(\d+))?$/.exec(ref)
  return { path: m?.[1] ?? ref, line: m?.[2] ? Number(m[2]) : null }
}

/**
 * Validate an agent's `proof-spec/1` against the judged head. A spec for another head, or one
 * that is not a spec at all, is ignored whole; otherwise each invalid item is dropped alone.
 * A risk's `at`, a snippet's path and criterion `code` evidence must cite a file in
 * `changedPaths`, and a line must exist at head when `hasLine` can tell; a decision's `at` must be
 * a line the diff shows, so the page can quote the code around it. An area or a waiting reason's
 * `area` must be one of `areaIds` when given. The `reason` is one sentence in the reader's words,
 * never the pipeline's. A criterion stays done only when one of its evidence refs resolves: a
 * changed test file, a check in `checks`, an acceptance row met at this head, or a finding id;
 * evidence that resolves to nothing is dropped, and the ticket's verdict is computed from the
 * criteria. A link must name another kept item's id or a finding id, else it is dropped.
 * Line-level snippet checks need git and happen when the page quotes them. A note's severity is
 * optional; a known word becomes its P-level, and one longer than 12 characters is left out.
 * @param {unknown} value @param {SpecContext} context
 * @returns {{spec: ProofSpec | null, dropped: Dropped[]}}
 */
export function validateProofSpec(
  value,
  {
    head,
    changedPaths = null,
    areaIds = null,
    hasLine,
    checks = null,
    acceptance = [],
    findingIds = [],
    addedPaths = null,
  },
) {
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
  const passed = checks ? new Set(checks) : null
  const met = new Set(
    acceptance
      .filter((row) => row.result === 'met' && row.head.toLowerCase() === head.toLowerCase())
      .map((row) => row.id),
  )
  const findings = new Set(findingIds)
  const added = addedPaths ? new Set(addedPaths) : null
  /** @param {unknown} id @param {string} field */
  const knownArea = (id, field) => {
    const text = needText(id, SPEC_TEXT_CHARS, field)
    if (areas && !areas.has(text)) throw new Drop(`${field}: ${text} is not a judged area`)
    return text
  }
  /** A changed file and, when `hasLine` can tell, a line it has at head.
   * @param {string} path @param {number | null} line @param {string} field @param {string} [lineField] */
  const inDiff = (path, line, field, lineField = field) => {
    if (!changed) throw new Drop(`${field}: the changed files are unknown, so it cannot be checked`)
    if (!changed.has(path)) throw new Drop(`${field}: ${path} is not changed by this PR`)
    if (line !== null && hasLine?.(path, line) === false)
      throw new Drop(`${lineField}: ${path} has no line ${line} at head`)
  }
  /** @param {unknown} value @param {string} field @returns {SpecAt} */
  const location = (value, field) => {
    const at = onlyKeys(value, ['path', 'line'], field)
    const path = needText(at.path, SPEC_TEXT_CHARS, `${field}.path`)
    const line = needLine(at.line, `${field}.line`)
    inDiff(path, line, `${field}.path`, `${field}.line`)
    return { path, line }
  }
  /** Ids each kept item claims, and their anchor forms, so a second item with one is dropped. */
  const ids = new Set()
  const anchors = new Set()
  /** @param {string} id */
  const anchorOf = (id) => id.toLowerCase().replace(/[^a-z0-9]+/g, '-')
  /** @param {unknown} value @param {string} field */
  const checkId = (value, field) => {
    const id = needText(value, SPEC_TEXT_CHARS, field)
    if (!SPEC_ID.test(id)) throw new Drop(`${field}: expected a short id such as D1`)
    if (anchors.has(anchorOf(id)) || findings.has(id))
      throw new Drop(`${field}: ${id} appears twice`)
    return id
  }
  /** Claim a kept item's id, once the whole item has parsed. @param {string | undefined} id */
  const claim = (id) => {
    if (id === undefined) return
    ids.add(id)
    anchors.add(anchorOf(id))
  }
  /** The link ids that are text; any other entry is dropped alone.
   * @param {unknown} value @param {string} at @returns {string[]} */
  const linkList = (value, at) =>
    keepEach(
      value,
      `${at}.links`,
      MAX_SPEC_LINKS,
      (id) => needText(id, SPEC_TEXT_CHARS, 'id'),
      dropped,
    )
  let title
  try {
    title = optText(value.title, SPEC_TITLE_CHARS, 'title')
  } catch (error) {
    if (!(error instanceof Drop)) throw error
    dropped.push({ at: 'spec.title', reason: error.message })
  }
  let reason
  try {
    reason = optText(value.reason, SPEC_REASON_CHARS, 'reason')
    if (reason !== undefined) {
      if (/[\r\n]/.test(reason) || /[.!?]\s+\S/.test(reason.trim()))
        throw new Drop('reason: expected one sentence')
      const word = PIPELINE_WORDS.exec(reason)?.[0]
      if (word) throw new Drop(`reason: says "${word}", a word only the pipeline uses`)
    }
  } catch (error) {
    if (!(error instanceof Drop)) throw error
    dropped.push({ at: 'spec.reason', reason: error.message })
    reason = undefined
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
    (entry, where) => {
      const r = onlyKeys(entry, ['id', 'level', 'title', 'impact', 'guard', 'at', 'links'])
      if (typeof r.level !== 'string' || !RISK_LEVELS.includes(r.level))
        throw new Drop(`level: expected one of ${RISK_LEVELS.join(', ')}`)
      const risk = compact({
        id: r.id === undefined ? undefined : checkId(r.id, 'id'),
        level: r.level,
        title: needText(r.title, SPEC_TEXT_CHARS, 'title'),
        impact: needText(r.impact, SPEC_TEXT_CHARS, 'impact'),
        guard: optText(r.guard, SPEC_TEXT_CHARS, 'guard'),
        at: r.at === undefined ? undefined : location(r.at, 'at'),
        links: linkList(r.links, where),
      })
      claim(risk.id)
      return risk
    },
    dropped,
  )
  const decisions = keepEach(
    value.decisions,
    'decisions',
    MAX_SPEC_DECISIONS,
    (entry, where) => {
      const d = onlyKeys(entry, ['id', 'question', 'why', 'suggestion', 'at', 'links'])
      const id = checkId(d.id, 'id')
      const at = location(d.at, 'at')
      if (hasLine?.(at.path, at.line) !== true)
        throw new Drop(`at: ${at.path}:${at.line} is not a line the diff shows`)
      const decision = {
        id,
        question: needText(d.question, SPEC_TEXT_CHARS, 'question'),
        why: needText(d.why, SPEC_TEXT_CHARS, 'why'),
        suggestion: needText(d.suggestion, SPEC_TEXT_CHARS, 'suggestion'),
        at,
        links: linkList(d.links, where),
      }
      claim(id)
      return decision
    },
    dropped,
  )
  /** @param {SpecEvidence} ev @returns {boolean} whether it proves its criterion */
  const resolve = (ev) => {
    if (ev.kind === 'ci') {
      if (!passed?.has(ev.ref)) throw new Drop(`ref: ${ev.ref} is not a check that passed on head`)
      return true
    }
    if (ev.kind === 'acceptance') {
      if (!met.has(ev.ref)) throw new Drop(`ref: ${ev.ref} is not a criterion review met on head`)
      return true
    }
    if (ev.kind === 'finding') {
      if (!findings.has(ev.ref)) throw new Drop(`ref: ${ev.ref} is not a review finding`)
      return true
    }
    const { path, line } = splitRef(ev.ref)
    inDiff(path, line, 'ref')
    if (ev.kind === 'test' && !TEST_FILE.test(path))
      throw new Drop(`ref: ${path} is not a test file`)
    if (ev.kind === 'test' && added && !added.has(path))
      throw new Drop(`ref: ${path} gains no lines in this PR`)
    return ev.kind === 'test'
  }
  /** @type {SpecTicketFit | undefined} */
  let ticketFit
  if (value.ticketFit !== undefined) {
    try {
      const t = onlyKeys(
        value.ticketFit,
        ['verdict', 'summary', 'criteria', 'skipped', 'contradictions'],
        'ticketFit',
      )
      const criteria = keepEach(
        t.criteria,
        'ticketFit.criteria',
        MAX_SPEC_LIST,
        (entry, at) => {
          const c = onlyKeys(entry, ['id', 'text', 'status', 'note', 'evidence'])
          if (typeof c.status !== 'string' || !CRITERION_STATUSES.includes(c.status))
            throw new Drop(`status: expected one of ${CRITERION_STATUSES.join(', ')}`)
          const id = checkId(c.id, 'id')
          const text = needText(c.text, SPEC_TEXT_CHARS, 'text')
          const note = optText(c.note, SPEC_TEXT_CHARS, 'note')
          let proven = false
          const evidence = keepEach(
            c.evidence,
            `${at}.evidence`,
            MAX_SPEC_LINKS,
            (item) => {
              const e = onlyKeys(item, ['kind', 'label', 'ref'])
              if (typeof e.kind !== 'string' || !PROOF_KINDS.includes(e.kind))
                throw new Drop(`kind: expected one of ${PROOF_KINDS.join(', ')}`)
              const ev = {
                kind: e.kind,
                label: needText(e.label, SPEC_TITLE_CHARS, 'label'),
                ref: needText(e.ref, SPEC_TEXT_CHARS, 'ref'),
              }
              proven = resolve(ev) || proven
              return ev
            },
            dropped,
          )
          let status = c.status
          if (status === 'done' && !proven) {
            dropped.push({
              at: `${at}.status`,
              reason: 'done, but no test, passing check, met criterion or finding shows it',
            })
            status = 'unverified'
          }
          claim(id)
          return compact({ id, text, status, note, evidence })
        },
        dropped,
      )
      const verdict = ticketVerdict(criteria)
      if (t.verdict !== undefined && t.verdict !== verdict)
        dropped.push({
          at: 'ticketFit.verdict',
          reason: `${String(t.verdict)} replaced by ${verdict ?? 'none'}, worked out from the criteria`,
        })
      let summary
      try {
        summary = optText(t.summary, SPEC_TEXT_CHARS, 'summary')
      } catch (error) {
        if (!(error instanceof Drop)) throw error
        dropped.push({ at: 'ticketFit.summary', reason: error.message })
      }
      ticketFit = compact({
        verdict,
        summary,
        criteria,
        skipped: keepEach(
          t.skipped,
          'ticketFit.skipped',
          MAX_SPEC_LIST,
          (entry) => {
            const k = onlyKeys(entry, ['text', 'why', 'followUp'])
            return compact({
              text: needText(k.text, SPEC_TEXT_CHARS, 'text'),
              why: needText(k.why, SPEC_TEXT_CHARS, 'why'),
              followUp:
                k.followUp === undefined || k.followUp === null
                  ? undefined
                  : needText(k.followUp, SPEC_LABEL_CHARS, 'followUp'),
            })
          },
          dropped,
        ),
        contradictions: keepEach(
          t.contradictions,
          'ticketFit.contradictions',
          MAX_SPEC_LIST,
          (entry) => needText(entry, SPEC_TEXT_CHARS, 'text'),
          dropped,
        ),
      })
    } catch (error) {
      if (!(error instanceof Drop)) throw error
      dropped.push({ at: 'ticketFit', reason: error.message })
    }
  }
  // A link names a kept item or a finding, never its own item.
  /** @template {{id?: string, links: string[]}} T @param {T} item @param {string} at @returns {T} */
  const keepLinks = (item, at) => {
    const links = [...new Set(item.links)].filter((id) => {
      if (id !== item.id && (ids.has(id) || findings.has(id))) return true
      dropped.push({ at: `${at}.links`, reason: `${id} names nothing on this page` })
      return false
    })
    return { ...item, links }
  }
  const linkedRisks = risks.map((risk, i) => keepLinks(risk, `risks[${i}]`))
  const linkedDecisions = decisions.map((decision, i) => keepLinks(decision, `decisions[${i}]`))
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
          inDiff(path, null, 'path')
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
  if (value.checklist !== undefined) {
    if (!isRecord(value.checklist)) dropped.push({ at: 'checklist', reason: 'expected object' })
    else
      for (const key of Object.keys(value.checklist))
        if (key !== 'after') dropped.push({ at: `checklist.${key}`, reason: 'unknown key' })
  }
  const after = keepEach(
    isRecord(value.checklist) ? value.checklist.after : undefined,
    'checklist.after',
    MAX_SPEC_LIST,
    (entry) => {
      const c = onlyKeys(entry, ['text', 'done'])
      if (c.done !== undefined && typeof c.done !== 'boolean')
        throw new Drop('done: expected boolean')
      return { text: needText(c.text, SPEC_TEXT_CHARS, 'text'), done: c.done === true }
    },
    dropped,
  )
  const notes = keepEach(
    value.notes,
    'notes',
    MAX_SPEC_LIST,
    (entry, at) => {
      const n = onlyKeys(entry, ['severity', 'at', 'text'])
      return compact({
        severity: noteSeverity(n.severity, `${at}.severity`, dropped),
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
      reason,
      waiting,
      risks: linkedRisks,
      decisions: linkedDecisions,
      ticketFit,
      areas: specAreas,
      checklist: { after },
      notes,
      evidencePlan,
      uncertain,
    }),
    dropped,
  }
}
