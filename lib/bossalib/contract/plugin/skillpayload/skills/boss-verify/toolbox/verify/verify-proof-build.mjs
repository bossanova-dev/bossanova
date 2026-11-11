// Build a boss-proof/1 record from one judge output. Everything comes from facts the judge already
// computed (its confidence, areas, blockers and notes) plus an optional verify-intent record, so the
// core proves a head with no evidence run of its own: areas carry describer items and no media.
import {
  CONFIDENCE_NOT_COMPUTED,
  INTENT_SOURCES,
  capText,
  LEVELS,
  MAX_AREAS,
  MAX_BLOCKERS,
  MAX_BULLETS,
  MAX_DESCRIBE_LINKS,
  MAX_HEALTH,
  MAX_ITEMS,
  MAX_LOWERED_BY,
  MAX_MODELS,
  MAX_NOTES,
  MAX_TEXT_CHARS,
  PROOF_SCHEMA,
  isProofUrl,
  validateProof,
  withDigest,
} from './verify-proof.mjs'

// The verdicts whose post records a proof; a wait or claim says nothing final about the head.
export const PROOF_VERDICTS = Object.freeze(['pass', 'blocked', 'defect', 'human'])

// What each confidence cap code means, for the page's "Why not high" list.
const LOWERED_TEXT = Object.freeze({
  'area-asks': 'It changes areas a person reviews.',
  'size-p90': 'It is larger than nine in ten merged PRs here.',
  'size-p99': 'It is larger than 99 in 100 merged PRs here.',
  'asks-over-3': 'It asks about more than three areas.',
  unproven: 'A UI or behavior change is unproven.',
  blocker: 'Blockers stand on this head.',
  'evidence-missing': 'Evidence needed to judge this head is missing.',
})

const SHA = /^[0-9a-f]{40}$/

/** @param {unknown} value @returns {value is Record<string, any>} */
const isObject = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)

/** One line of at most MAX_TEXT_CHARS characters, cut at a word and elided with U+2026; '' when nothing is left. */
export function boundText(value) {
  const line = String(value ?? '')
    .replace(/\s+/g, ' ')
    .trim()
  return capText(line, MAX_TEXT_CHARS)
}

/** @param {unknown} list @param {number} cap @returns {string[]} */
const boundList = (list, cap) =>
  [...new Set((Array.isArray(list) ? list : []).map(boundText).filter(Boolean))].slice(0, cap)

/** An identifier the proof contract and the daemon accept: lowercase, other runs one `-`. */
const slug = (value) =>
  String(value ?? '')
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 64)
    .replace(/-+$/, '')

/**
 * The proof-record name of a verify area id. Proof names and the daemon's asks allow no `:`, so
 * every run of characters outside `[a-z0-9]` becomes one `-`: `config:schema` is named
 * `config-schema`, `capped:other:docs` `capped-other-docs`. The verify id stays the area's
 * identity everywhere else (asks, park codes, caps); only the record carries this name.
 * @param {string} id
 */
export const proofAreaName = (id) => slug(id) || 'area'

/** A reason `{code, text}` from a cap code. @param {string} code */
const lowered = (code) => ({
  code: slug(code) || CONFIDENCE_NOT_COMPUTED,
  text: LOWERED_TEXT[/** @type {keyof typeof LOWERED_TEXT} */ (code)] ?? boundText(code),
})

/** A health entry, either a judge code string or a `{code, text}` reason. @param {unknown} entry */
function healthReason(entry) {
  if (typeof entry === 'string') {
    const code = slug(entry)
    return code ? { code, text: boundText(entry.replaceAll('-', ' ')) } : null
  }
  if (!isObject(entry)) return null
  const code = slug(entry.code)
  const text = boundText(entry.text)
  return code && text ? { code, text } : null
}

/** The intent record's `whatItDoes`, or the unknown intent when it is missing or malformed. */
export function whatItDoesFrom(intent) {
  const unknown = { source: 'unknown', bullets: [] }
  if (!isObject(intent) || !INTENT_SOURCES.includes(intent.source) || intent.source === 'unknown')
    return unknown
  const bullets = boundList(intent.bullets, MAX_BULLETS)
  if (!bullets.length) return unknown
  const ref = Array.isArray(intent.ref) ? intent.ref.join(', ') : intent.ref
  return {
    source: intent.source,
    bullets,
    ...(typeof ref === 'string' && boundText(ref) ? { ref: boundText(ref) } : {}),
  }
}

/** @param {Record<string, any>} area */
function describedFields(area) {
  const links = (Array.isArray(area.links) ? area.links : [])
    .filter((link) => isObject(link) && boundText(link.text) && isProofUrl(link.url))
    .map((link) => ({ text: boundText(link.text), url: link.url }))
    .slice(0, MAX_DESCRIBE_LINKS)
  const unmentioned = boundList(area.undisclosed, MAX_ITEMS)
  const changeReasons = boundList(area.changeReasons, MAX_ITEMS)
  return {
    items: boundList(area.items, MAX_ITEMS),
    ...(links.length ? { links } : {}),
    ...(area.breaking === true ? { breaking: /** @type {true} */ (true) } : {}),
    ...(['additive', 'breaking'].includes(area.change)
      ? { change: area.change, ...(changeReasons.length ? { changeReasons } : {}) }
      : {}),
    undisclosed: unmentioned.length > 0,
    ...(unmentioned.length ? { unmentioned } : {}),
  }
}

/**
 * The record's areas: one per judge area, then one per ask no judge area carries (a judge-added
 * area, a capped area, an extension sign-off), each asking when its id is one of the confidence's
 * asks. Names that collide after mapping take a `-<n>` suffix. Over MAX_AREAS, asking areas are
 * kept first. Each carries its verify `id` and `paths` for the page sidecar; build strips them.
 * @param {Record<string, any>[]} areas @param {{area: string, label?: string}[]} asks
 */
export function proofAreas(areas, asks) {
  const asked = new Set(asks.map((ask) => ask.area))
  const covered = new Set(areas.map((area) => area.area))
  const entries = [
    ...areas.map((area) => ({ id: String(area.area), label: area.label, area })),
    ...asks
      .filter((ask) => !covered.has(ask.area))
      .map((ask) => ({
        id: String(ask.area),
        label: ask.label,
        area: /** @type {Record<string, any>} */ ({}),
      })),
  ]
  const used = new Set()
  const named = entries.map(({ id, label, area }) => {
    const base = proofAreaName(id)
    let name = base
    for (let n = 2; used.has(name); n++) name = `${base}-${n}`
    used.add(name)
    const text = boundText(label)
    return {
      id,
      paths: Array.isArray(area.paths) ? area.paths.filter((p) => typeof p === 'string') : [],
      record: {
        name,
        ...(text ? { label: text } : {}),
        ask: asked.has(id),
        ...describedFields(area),
        media: [],
        proven: false,
      },
    }
  })
  if (named.length <= MAX_AREAS) return named
  const keep = new Set(
    [...named.filter((e) => e.record.ask), ...named.filter((e) => !e.record.ask)].slice(
      0,
      MAX_AREAS,
    ),
  )
  return named.filter((entry) => keep.has(entry))
}

/** The models that reviewed the head: the build review receipt, then each verify extension. */
function reviewModels(judged) {
  const receipt = judged.facts?.receipt
  const models = [
    { agent: 'review', outcome: receipt?.state === 'success' ? 'ran' : 'skipped' },
    ...(Array.isArray(judged.extensions?.outcomes) ? judged.extensions.outcomes : [])
      .filter((entry) => isObject(entry) && boundText(entry.extension))
      .map((entry) => ({
        agent: boundText(entry.extension),
        outcome: entry.outcome === 'ok' ? 'ran' : entry.outcome === 'failed' ? 'failed' : 'skipped',
      })),
  ]
  return models.slice(0, MAX_MODELS)
}

/** @param {Record<string, any>} blocker */
function blockerEntry(blocker) {
  const title = boundText(blocker.summary ?? blocker.title)
  if (!title) return null
  const file = typeof blocker.file === 'string' && blocker.file ? blocker.file : null
  const where = file
    ? boundText(Number.isSafeInteger(blocker.line) ? `${file}:${blocker.line}` : file)
    : ''
  return { title, ...(where ? { where } : {}) }
}

/** The finding notes, not-applicable notes and defect findings, bounded, in that order. */
function reviewNotes(judged) {
  const titles = (list) =>
    (Array.isArray(list) ? list : []).filter(isObject).map((f) => f.title ?? f.summary)
  return boundList(
    [
      ...titles(judged.findingNotes),
      ...(Array.isArray(judged.notApplicableNotes) ? judged.notApplicableNotes : [])
        .filter(isObject)
        .map((entry) => `${entry.extension}: ${entry.note}`),
      ...titles(judged.findings),
    ],
    MAX_NOTES,
  )
}

/**
 * The boss-proof/1 record for judge output `judged` on `repo`#`pr`, with `page` null, plus each
 * area's verify id and paths: `{ok: true, proof, areas: [{id, name, paths, ask}]}`, or
 * `{ok: false, reason, errors?}` when the judge output names no terminal verdict for a head or
 * the record would not validate. Never throws.
 *
 * Level rules the contract fixes: blockers make the record `blocked` and only blockers do (a
 * `blocked` confidence without them reads `cannot-assess`); a level below high always names a
 * reason, `confidence-not-computed` when the judge gave none.
 *
 * @param {{judged: Record<string, any>, intent?: unknown, repo: string, pr: number}} input
 */
export function buildProofRecord({ judged, intent = null, repo, pr }) {
  try {
    if (!isObject(judged) || !PROOF_VERDICTS.includes(judged.verdict))
      return { ok: false, reason: 'judge-not-terminal' }
    const head = String(judged.headSha ?? '').toLowerCase()
    const base = String(judged.baseSha ?? judged.facts?.base?.sha ?? '').toLowerCase()
    if (!SHA.test(head) || !SHA.test(base)) return { ok: false, reason: 'judge-without-range' }
    const confidence = isObject(judged.confidence) ? judged.confidence : {}
    const asks = (Array.isArray(confidence.asks) ? confidence.asks : []).filter(
      (ask) => isObject(ask) && typeof ask.area === 'string',
    )
    const named = proofAreas(Array.isArray(judged.facts?.areas) ? judged.facts.areas : [], asks)
    const blockers = (Array.isArray(judged.blockers) ? judged.blockers : [])
      .filter(isObject)
      .map(blockerEntry)
      .filter(Boolean)
      .slice(0, MAX_BLOCKERS)
    let level = LEVELS.includes(confidence.level) ? confidence.level : 'cannot-assess'
    const loweredBy = (Array.isArray(confidence.loweredBy) ? confidence.loweredBy : [])
      .filter((code) => typeof code === 'string')
      .map(lowered)
    if (blockers.length) level = 'blocked'
    else if (level === 'blocked') level = 'cannot-assess'
    if (level === 'blocked' && !loweredBy.some((r) => r.code === 'blocker'))
      loweredBy.unshift(lowered('blocker'))
    const areas = named.map((entry) => entry.record)
    // High means nothing asks and nothing lowered it; a confidence claiming otherwise is moderate.
    if (level === 'high' && (loweredBy.length || areas.some((area) => area.ask))) level = 'moderate'
    if (level !== 'high' && loweredBy.length === 0)
      loweredBy.push({ code: CONFIDENCE_NOT_COMPUTED, text: 'Confidence is not computed yet.' })
    const record = withDigest({
      schema: PROOF_SCHEMA,
      repo,
      pr,
      head,
      base,
      confidence: {
        level,
        asks: areas.filter((area) => area.ask).map((area) => area.name),
        loweredBy: [...new Map(loweredBy.map((r) => [r.code, r])).values()].slice(
          0,
          MAX_LOWERED_BY,
        ),
      },
      whatItDoes: whatItDoesFrom(intent),
      areas,
      review: { models: reviewModels(judged), blockers, notes: reviewNotes(judged) },
      health: (Array.isArray(confidence.health) ? confidence.health : [])
        .map(healthReason)
        .filter(Boolean)
        .slice(0, MAX_HEALTH),
      page: null,
    })
    const checked = validateProof(record)
    if (!checked.ok) return { ok: false, reason: 'record-invalid', errors: checked.errors }
    return {
      ok: true,
      proof: record,
      areas: named.map(({ id, paths, record: area }) => ({
        id,
        name: area.name,
        paths,
        ask: area.ask,
      })),
    }
  } catch (error) {
    return { ok: false, reason: 'record-failed', errors: [String(error?.message ?? error)] }
  }
}
