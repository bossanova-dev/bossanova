/**
 * Portable proof contract. Canonical JSON sorts object keys recursively, preserves array order,
 * and has no whitespace. Only strings, booleans, null and safe integers are representable.
 * The digest hashes UTF-8 canonical JSON after removing the top-level digest field.
 */
import { createHash } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'

export const PROOF_SCHEMA = 'boss-proof/1'
export const PROOF_MARKER = '<!-- boss-proof:'
export const LEVELS = Object.freeze(['high', 'moderate', 'low', 'blocked', 'cannot-assess'])
export const REVIEW_OUTCOMES = Object.freeze(['ran', 'skipped', 'failed'])
export const MEDIA_KINDS = Object.freeze(['video', 'image'])
export const INTENT_SOURCES = Object.freeze([
  'acceptance-criteria',
  'issue',
  'pr-body',
  'commits',
  'unknown',
])
export const MAX_TEXT_CHARS = 300
export const MAX_URL_CHARS = 2048
export const MAX_BULLETS = 5
export const MAX_AREAS = 32
export const MAX_ITEMS = 20
export const MAX_DESCRIBE_LINKS = 5
export const MAX_MEDIA = 3
export const MAX_LOWERED_BY = 8
export const MAX_MODELS = 4
export const MAX_BLOCKERS = 50
export const MAX_NOTES = 20
export const MAX_HEALTH = 20
export const MAX_PROOF_BYTES = 64 * 1024

/**
 * @typedef {{code: string, text: string}} Reason
 * @typedef {{kind: string, url: string, poster?: string, caption?: string, durationMs?: number}} Media
 * @typedef {{name: string, label?: string, ask: boolean, items: string[], undisclosed: boolean, media: Media[], proven: boolean, links?: {text: string, url: string}[], breaking?: true}} ProofArea
 * @typedef {{models: {agent: string, outcome: string, detail?: string}[], blockers: {title: string, where?: string}[], notes: string[]}} ProofReview
 * @typedef {{schema: string, repo: string, pr: number, head: string, base: string,
 * confidence: {level: string, asks: string[], loweredBy: Reason[]},
 * whatItDoes: {source: string, ref?: string, bullets: string[]}, areas: ProofArea[], review: ProofReview | null,
 * health: Reason[], page: string | null, digest: string}} Proof
 * @typedef {(value: unknown, path: string, errors: string[]) => void} Check
 */

/** @param {unknown} value @returns {value is Record<string, unknown>} */
const object = (value) =>
  value !== null &&
  typeof value === 'object' &&
  !Array.isArray(value) &&
  (Object.getPrototypeOf(value) === Object.prototype || Object.getPrototypeOf(value) === null)

/** @param {unknown} value @returns {string} */
export function canonicalJson(value) {
  const active = new Set()
  /** @param {unknown} item @returns {string} */
  function encode(item) {
    if (item === null || typeof item === 'boolean' || typeof item === 'string')
      return JSON.stringify(item)
    if (typeof item === 'number' && Number.isSafeInteger(item)) return JSON.stringify(item)
    if (typeof item !== 'object' || item === null || (!Array.isArray(item) && !object(item))) {
      throw new TypeError(
        'Canonical JSON requires plain objects, arrays, strings, booleans, null and safe integers',
      )
    }
    if (active.has(item)) throw new TypeError('Canonical JSON cannot contain cycles')
    active.add(item)
    try {
      if (Array.isArray(item)) {
        return `[${Array.from(item, (entry) => encode(entry)).join(',')}]`
      }
      return `{${Object.keys(item)
        .sort()
        .map((key) => `${JSON.stringify(key)}:${encode(item[key])}`)
        .join(',')}}`
    } finally {
      active.delete(item)
    }
  }
  return encode(value)
}

/** @param {object} record */
export function proofDigest(record) {
  if (!object(record)) throw new TypeError('Proof must be an object')
  const { digest: _digest, ...content } = record
  return `sha256:${createHash('sha256').update(canonicalJson(content), 'utf8').digest('hex')}`
}

/**
 * @template {object} T
 * @param {T} record
 * @returns {T & {digest: string}}
 */
export function withDigest(record) {
  return Object.assign({}, record, { digest: proofDigest(record) })
}

/** @type {Check} */
const text = (value, path, errors) => {
  if (typeof value !== 'string' || value.length === 0 || value.length > MAX_TEXT_CHARS) {
    errors.push(`${path}: expected non-empty text of at most ${MAX_TEXT_CHARS} characters`)
  }
}
/** @type {Check} */
const id = (value, path, errors) => {
  text(value, path, errors)
  if (typeof value !== 'string' || !/^[a-z0-9][a-z0-9-]*$/.test(value))
    errors.push(`${path}: invalid identifier`)
}
/** @type {Check} */
const bool = (value, path, errors) => {
  if (typeof value !== 'boolean') errors.push(`${path}: expected boolean`)
}
/** @type {Check} */
const integer = (value, path, errors) => {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0)
    errors.push(`${path}: expected non-negative safe integer`)
}
/** @type {Check} */
export const checkProofUrl = (value, path, errors) => {
  if (
    typeof value !== 'string' ||
    value.length > MAX_URL_CHARS ||
    /\s/.test(value) ||
    !/^https:\/\//i.test(value)
  ) {
    errors.push(`${path}: expected absolute HTTPS URL`)
    return
  }
  try {
    const parsed = new URL(value)
    if (
      parsed.protocol !== 'https:' ||
      !parsed.hostname ||
      parsed.username ||
      parsed.password ||
      value.slice(8).split(/[/?#]/, 1)[0].includes('@')
    ) {
      errors.push(`${path}: expected HTTPS host without userinfo`)
    }
  } catch {
    errors.push(`${path}: invalid URL`)
  }
}
const url = checkProofUrl
export function isProofUrl(value) {
  const errors = []
  url(value, 'url', errors)
  return errors.length === 0
}
/** @param {readonly string[]} values @returns {Check} */
const enumeration = (values) => (value, path, errors) => {
  if (typeof value !== 'string' || !values.includes(value))
    errors.push(`${path}: unsupported value`)
}
/** @param {Check} check @returns {Check} */
const nullable = (check) => (value, path, errors) => {
  if (value !== null) check(value, path, errors)
}
/** @param {Check} check @param {number} cap @returns {Check} */
const array = (check, cap) => (value, path, errors) => {
  if (!Array.isArray(value)) {
    errors.push(`${path}: expected array`)
    return
  }
  if (value.length > cap) errors.push(`${path}: exceeds ${cap} entries`)
  value.forEach((entry, i) => check(entry, `${path}[${i}]`, errors))
}
/** @param {Record<string, Check>} required @param {Record<string, Check>} [optional] @returns {Check} */
const shape =
  (required, optional = {}) =>
  (value, path, errors) => {
    if (!object(value)) {
      errors.push(`${path}: expected object`)
      return
    }
    for (const key of Object.keys(value)) {
      if (!Object.hasOwn(required, key) && !Object.hasOwn(optional, key))
        errors.push(`${path}.${key}: unknown key`)
    }
    for (const [key, check] of Object.entries(required)) check(value[key], `${path}.${key}`, errors)
    for (const [key, check] of Object.entries(optional))
      if (Object.hasOwn(value, key)) check(value[key], `${path}.${key}`, errors)
  }
/** @type {Check} */
const sha = (value, path, errors) => {
  if (typeof value !== 'string' || !/^[a-fA-F0-9]{40}$/.test(value))
    errors.push(`${path}: expected 40-hex SHA`)
}
const reason = shape({ code: id, text })
const media = shape(
  { kind: enumeration(MEDIA_KINDS), url },
  { poster: url, caption: text, durationMs: integer },
)
const area = shape(
  {
    name: id,
    ask: bool,
    items: array(text, MAX_ITEMS),
    undisclosed: bool,
    media: array(media, MAX_MEDIA),
    proven: bool,
  },
  {
    label: text,
    links: array(shape({ text, url }), MAX_DESCRIBE_LINKS),
    breaking: (value, path, errors) => {
      if (value !== true) errors.push(`${path}: expected true when present`)
    },
  },
)
const review = shape({
  models: array(
    shape({ agent: text, outcome: enumeration(REVIEW_OUTCOMES) }, { detail: text }),
    MAX_MODELS,
  ),
  blockers: array(shape({ title: text }, { where: text }), MAX_BLOCKERS),
  notes: array(text, MAX_NOTES),
})
const proofShape = shape({
  schema: enumeration([PROOF_SCHEMA]),
  repo: (value, path, errors) => {
    text(value, path, errors)
    if (typeof value !== 'string' || !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(value))
      errors.push(`${path}: expected owner/name`)
  },
  pr: (value, path, errors) => {
    integer(value, path, errors)
    if (value === 0) errors.push(`${path}: expected positive integer`)
  },
  head: sha,
  base: sha,
  confidence: shape({
    level: enumeration(LEVELS),
    asks: array(id, MAX_AREAS),
    loweredBy: array(reason, MAX_LOWERED_BY),
  }),
  whatItDoes: shape(
    { source: enumeration(INTENT_SOURCES), bullets: array(text, MAX_BULLETS) },
    { ref: text },
  ),
  areas: array(area, MAX_AREAS),
  review: nullable(review),
  health: array(reason, MAX_HEALTH),
  page: nullable(url),
  digest: (value, path, errors) => {
    if (typeof value !== 'string' || !/^sha256:[a-f0-9]{64}$/.test(value))
      errors.push(`${path}: invalid digest`)
  },
})

/** @param {unknown} input @returns {{ok: boolean, errors: string[], proof: Proof | null}} */
export function validateProof(input) {
  /** @type {string[]} */
  const errors = []
  proofShape(input, 'proof', errors)
  try {
    if (Buffer.byteLength(canonicalJson(input), 'utf8') > MAX_PROOF_BYTES)
      errors.push('proof: exceeds byte limit')
  } catch (error) {
    errors.push(`proof: ${error instanceof Error ? error.message : String(error)}`)
  }
  if (errors.length) return { ok: false, errors, proof: null }
  const proof = /** @type {Proof} */ (input)
  const { level, asks, loweredBy } = proof.confidence
  const expectedAsks = proof.areas.filter((entry) => entry.ask).map((entry) => entry.name)
  if (JSON.stringify(asks) !== JSON.stringify(expectedAsks))
    errors.push('confidence.asks: must match asking areas in order')
  if (new Set(proof.areas.map((entry) => entry.name)).size !== proof.areas.length)
    errors.push('areas: duplicate names')
  if ((level === 'blocked') !== Boolean(proof.review?.blockers.length))
    errors.push('confidence.level: blocked must match review blockers')
  if (level === 'high' && (asks.length || loweredBy.length || proof.review === null))
    errors.push('confidence.level: high requires review and no asks or loweredBy')
  if (proof.review === null && level !== 'cannot-assess')
    errors.push('confidence.level: missing review requires cannot-assess')
  if (level !== 'high' && loweredBy.length === 0)
    errors.push('confidence.loweredBy: non-high level requires a reason')
  if ((proof.whatItDoes.source === 'unknown') !== (proof.whatItDoes.bullets.length === 0))
    errors.push('whatItDoes: unknown source must match empty bullets')
  if (proof.whatItDoes.source === 'unknown' && Object.hasOwn(proof.whatItDoes, 'ref'))
    errors.push('whatItDoes.ref: unknown source cannot carry a reference')
  if (proof.digest !== proofDigest(proof)) errors.push('digest: mismatch')
  return { ok: errors.length === 0, errors, proof: errors.length === 0 ? proof : null }
}

/** @param {unknown} record */
export function proofLine(record) {
  const result = validateProof(record)
  if (!result.ok || !result.proof) throw new TypeError(result.errors.join('\n'))
  const {
    schema,
    head,
    page,
    confidence: { level, asks },
    digest,
  } = result.proof
  const json = JSON.stringify({ schema, head, page, level, asks, digest }).replace(
    /[<>&]/g,
    (char) => `\\u${char.charCodeAt(0).toString(16).padStart(4, '0')}`,
  )
  return `${PROOF_MARKER} ${json} -->`
}

if (isMainModule(import.meta.url)) {
  try {
    const { positionals } = parseArgs({ strict: true, allowPositionals: true, options: {} })
    const [action, file] = positionals
    if (positionals.length !== 2 || !['validate', 'marker'].includes(action))
      throw new Error('Usage: verify-proof.mjs validate|marker <file>')
    const record = JSON.parse(readFileSync(file, 'utf8'))
    const result = validateProof(record)
    if (!result.ok) throw new Error(result.errors.join('\n'))
    if (action === 'marker') process.stdout.write(`${proofLine(record)}\n`)
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`)
    process.exitCode = 2
  }
}
