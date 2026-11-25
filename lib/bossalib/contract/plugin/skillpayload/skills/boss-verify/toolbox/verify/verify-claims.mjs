/**
 * Portable claim proofs for the `boss-proof/1` record. A claim is one `whatItDoes` bullet. A prover
 * names, per claim and by 1-based index only, a test or a change criterion that shows it; code then
 * accepts or downgrades each entry, so a prover can neither restate a claim nor make "proven" stand
 * without a check. Also: the confidence cap for unproven behaviour claims. Pure.
 */
import {
  CLAIM_PROOFS,
  MAX_BULLETS,
  MAX_LOWERED_BY,
  MAX_TEXT_CHARS,
  UNPROVEN_CLAIM,
} from './verify-proof.mjs'

export { CLAIM_PROOFS, UNPROVEN_CLAIM }
export const CLAIM_PROOFS_SCHEMA = 'boss-claim-proofs/1'
export const UNREADABLE_NOTE = 'claim proofs unreadable'

/**
 * @typedef {{claim: number, proof: string, behaviour?: boolean, test?: string, path?: string, criterion?: string, note?: string}} ClaimProof
 * @typedef {{proof: string, behaviour: boolean, test?: string, path?: string, chapter?: string, atMs?: number, note?: string}} Claim
 * @typedef {{criterion: string, title: string, verdict: string, sweep: boolean, rendered: boolean, published: boolean, atMs: number | null}} ClaimChapter
 * @typedef {{code: string, text: string}} Reason
 * @typedef {{level: string, asks: string[], loweredBy: Reason[]}} Confidence
 */

/** @param {unknown} value @returns {value is Record<string, unknown>} */
const object = (value) =>
  value !== null &&
  typeof value === 'object' &&
  !Array.isArray(value) &&
  (Object.getPrototypeOf(value) === Object.prototype || Object.getPrototypeOf(value) === null)

/** @param {unknown} value */
const text = (value) =>
  typeof value === 'string' && value.length > 0 && value.length <= MAX_TEXT_CHARS

const ENTRY_KEYS = Object.freeze({
  test: ['test', 'path'],
  clip: ['criterion'],
  unproven: [],
})
const COMMON_KEYS = Object.freeze(['claim', 'proof', 'behaviour', 'note'])

/**
 * Strictly validate a prover file `{schema: 'boss-claim-proofs/1', claims: [...]}`. An entry may
 * carry only its index, its proof kind, the keys that kind needs, `behaviour` and `note`; claim
 * text (`text`, `bullet`, anything else) is an unknown key. Never throws.
 * @param {unknown} input @param {number} bulletCount
 * @returns {{ok: boolean, errors: string[], claims: ClaimProof[]}}
 */
export function validateClaimProofs(input, bulletCount) {
  /** @type {string[]} */
  const errors = []
  if (!object(input)) return { ok: false, errors: ['claims file: expected object'], claims: [] }
  for (const key of Object.keys(input))
    if (key !== 'schema' && key !== 'claims') errors.push(`${key}: unknown key`)
  if (input.schema !== CLAIM_PROOFS_SCHEMA) errors.push(`schema: expected ${CLAIM_PROOFS_SCHEMA}`)
  const entries = input.claims
  if (!Array.isArray(entries) || entries.length > MAX_BULLETS) {
    errors.push(`claims: expected at most ${MAX_BULLETS} entries`)
    return { ok: false, errors, claims: [] }
  }
  const seen = new Set()
  entries.forEach((entry, i) => {
    const at = `claims[${i}]`
    if (!object(entry)) {
      errors.push(`${at}: expected object`)
      return
    }
    const { claim, proof } = entry
    if (!Number.isSafeInteger(claim) || Number(claim) < 1 || Number(claim) > bulletCount)
      errors.push(`${at}.claim: expected an index from 1 to ${bulletCount}`)
    else if (seen.has(claim)) errors.push(`${at}.claim: duplicate index`)
    seen.add(claim)
    if (typeof proof !== 'string' || !CLAIM_PROOFS.includes(proof)) {
      errors.push(`${at}.proof: expected one of ${CLAIM_PROOFS.join(', ')}`)
      return
    }
    const needed = ENTRY_KEYS[/** @type {keyof typeof ENTRY_KEYS} */ (proof)]
    for (const key of Object.keys(entry))
      if (!COMMON_KEYS.includes(key) && !needed.includes(key))
        errors.push(`${at}.${key}: unknown key`)
    for (const key of needed) if (!text(entry[key])) errors.push(`${at}.${key}: expected text`)
    if (Object.hasOwn(entry, 'note') && !text(entry.note)) errors.push(`${at}.note: expected text`)
    if (Object.hasOwn(entry, 'behaviour') && typeof entry.behaviour !== 'boolean')
      errors.push(`${at}.behaviour: expected boolean`)
  })
  const ok = errors.length === 0
  return { ok, errors, claims: ok ? /** @type {ClaimProof[]} */ (entries) : [] }
}

const escapeRegExp = (/** @type {string} */ value) => value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
const IDENT = 'A-Za-z0-9_$'

/** Whether `name` occurs in `haystack` with no identifier character on either side. */
function hasToken(/** @type {string} */ haystack, /** @type {string} */ name) {
  return new RegExp(`(?<![${IDENT}])${escapeRegExp(name)}(?![${IDENT}])`).test(haystack)
}

// A test file by the common conventions: `*.test.*`, `*.spec.*`, `*_test.*`, `test_*.py` or a
// test directory.
const TEST_FILE =
  /(?:^|\/)(?:tests?|__tests__|spec|e2e)\/|\.(?:test|spec)\.[^/]+$|_test\.[^/]+$|(?:^|\/)test_[^/]+\.py$/i

/**
 * Verify a `test` claim against PR-changed paths and the head file contents. Shared by
 * `acceptClaimProofs` and `buildProofRecord` so sidecar test claims are never trusted on shape.
 * @param {{test: string, path: string}} entry
 * @param {{changedPaths: string[], readHeadFile: (path: string) => string | null}} context
 * @returns {Omit<Claim, 'behaviour'>}
 */
export function verifyTestClaim({ test, path }, { changedPaths, readHeadFile }) {
  const unproven = (/** @type {string} */ note) => ({ proof: 'unproven', note })
  if (!changedPaths.includes(path)) return unproven('the named test file is not changed by this PR')
  if (!TEST_FILE.test(path)) return unproven('the named file is not a test file')
  let source = null
  try {
    source = readHeadFile(path)
  } catch {
    source = null
  }
  if (typeof source !== 'string') return unproven('the named test file is unreadable at the head')
  if (!hasToken(source, test)) return unproven('the named test is not in its file at the head')
  return { proof: 'test', test, path }
}

/**
 * @param {ClaimProof} entry
 * @param {{changedPaths: string[], readHeadFile: (path: string) => string | null, chapters: ClaimChapter[]}} context
 * @returns {Omit<Claim, 'behaviour'>}
 */
function check(entry, { changedPaths, readHeadFile, chapters }) {
  const unproven = (/** @type {string} */ note) => ({ proof: 'unproven', note })
  if (entry.proof === 'test') {
    const { test, path } = /** @type {{test: string, path: string}} */ (entry)
    return verifyTestClaim({ test, path }, { changedPaths, readHeadFile })
  }
  if (entry.proof === 'clip') {
    const chapter = chapters.find((c) => c.criterion === entry.criterion)
    if (!chapter) return unproven('the named criterion has no chapter')
    if (chapter.sweep) return unproven('the named criterion is a sweep chapter, not in the video')
    if (chapter.verdict !== 'proven') return unproven('the named criterion is not proven')
    if (!chapter.rendered || !Number.isSafeInteger(chapter.atMs))
      return unproven('the named chapter is not rendered in the video')
    if (!chapter.published) return unproven('the video is not published')
    return {
      proof: 'clip',
      chapter: chapter.title.slice(0, MAX_TEXT_CHARS),
      atMs: /** @type {number} */ (chapter.atMs),
    }
  }
  return entry.note ? unproven(entry.note) : { proof: 'unproven' }
}

/**
 * The record claims, one per bullet in bullet order, from a prover file. A test stands only when
 * its path is changed by the PR, is a test file, is readable at the head and contains the test name
 * as a whole token; a clip only for a rendered, non-sweep, proven chapter of a published video.
 * Everything else, a missing entry included, is `unproven` with a note. `behaviour` defaults to
 * true. `proofs` is the parsed prover file, or null when none was supplied.
 * @param {{bullets: string[], proofs: unknown, changedPaths?: string[], readHeadFile?: (path: string) => string | null, chapters?: ClaimChapter[]}} input
 * @returns {Claim[]}
 */
export function acceptClaimProofs({
  bullets,
  proofs,
  changedPaths = [],
  readHeadFile = () => null,
  chapters = [],
}) {
  if (proofs === null || proofs === undefined)
    return bullets.map(() => ({
      proof: 'unproven',
      behaviour: true,
      note: 'no claim proofs were supplied',
    }))
  const valid = validateClaimProofs(proofs, bullets.length)
  if (!valid.ok)
    return bullets.map(() => ({ proof: 'unproven', behaviour: true, note: UNREADABLE_NOTE }))
  const byIndex = new Map(valid.claims.map((entry) => [entry.claim, entry]))
  return bullets.map((_, i) => {
    const entry = byIndex.get(i + 1)
    if (!entry) return { proof: 'unproven', behaviour: true, note: 'no proof was named' }
    return {
      behaviour: entry.behaviour !== false,
      ...check(entry, { changedPaths, readHeadFile, chapters }),
    }
  })
}

/** @param {Claim[] | undefined} claims */
const unprovenBehaviour = (claims) =>
  (claims ?? []).filter((claim) => claim.behaviour !== false && claim.proof === 'unproven').length

/**
 * Cap a confidence for unproven behaviour claims: `high` becomes `moderate`, and every level gains
 * one `unproven-claim` reason (replacing the last reason when the list is full). A claim without
 * `behaviour` counts as behaviour. Wraps any confidence function's output; never mutates it.
 * @template {Confidence} T
 * @param {T} confidence @param {Claim[] | undefined} claims @returns {T}
 */
export function capForClaims(confidence, claims) {
  const count = unprovenBehaviour(claims)
  if (count === 0) return confidence
  const reason = { code: UNPROVEN_CLAIM, text: `${count} behaviour claim(s) unproven.` }
  const kept = confidence.loweredBy.filter((r) => r.code !== UNPROVEN_CLAIM)
  return {
    ...confidence,
    level: confidence.level === 'high' ? 'moderate' : confidence.level,
    loweredBy: [...kept.slice(0, MAX_LOWERED_BY - 1), reason],
  }
}
