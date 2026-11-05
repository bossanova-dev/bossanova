/**
 * Portable claim proofs for the `boss-proof/1` record. A claim is one `whatItDoes` bullet. A prover
 * names, per claim and by 1-based index only, a test or a change criterion that shows it; code then
 * accepts or downgrades each entry, so a prover can neither restate a claim nor make "proven" stand
 * without a check. Also: the confidence cap for unproven behaviour claims, an added-API-symbol
 * extractor for proto and GraphQL contract files, and the "not in the description" mention check.
 * Pure.
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
function hasToken(/** @type {string} */ haystack, /** @type {string} */ name, flags = '') {
  return new RegExp(`(?<![${IDENT}])${escapeRegExp(name)}(?![${IDENT}])`, flags).test(haystack)
}

// A test file by the common conventions: `*.test.*`, `*.spec.*`, `*_test.*`, `test_*.py` or a
// test directory.
const TEST_FILE =
  /(?:^|\/)(?:tests?|__tests__|spec|e2e)\/|\.(?:test|spec)\.[^/]+$|_test\.[^/]+$|(?:^|\/)test_[^/]+\.py$/i

/**
 * @param {ClaimProof} entry
 * @param {{changedPaths: string[], readHeadFile: (path: string) => string | null, chapters: ClaimChapter[]}} context
 * @returns {Omit<Claim, 'behaviour'>}
 */
function check(entry, { changedPaths, readHeadFile, chapters }) {
  const unproven = (/** @type {string} */ note) => ({ proof: 'unproven', note })
  if (entry.proof === 'test') {
    const { test, path } = /** @type {{test: string, path: string}} */ (entry)
    if (!changedPaths.includes(path))
      return unproven('the named test file is not changed by this PR')
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

// ---------------------------------------------------------------------------------------------
// Added API symbols

const GRAPHQL = /\.(?:graphqls?|gql)$/i
const PROTO = /\.proto$/i
const GRAPHQL_DECL = /^\s*(extend\s+)?(type|input|interface|enum)\s+([A-Za-z_]\w*)/
const GRAPHQL_FIELD = /^\s*([A-Za-z_]\w*)\s*[(:]/
const GRAPHQL_ENUM_VALUE = /^\s*([A-Za-z_]\w*)\s*(?:@.*)?$/
const PROTO_DECL = /^\s*(message|enum|service|oneof)\s+([A-Za-z_]\w*)/
const PROTO_RPC = /^\s*rpc\s+([A-Za-z_]\w*)\s*\(/
const PROTO_FIELD =
  /^\s*(?:(?:optional|repeated|required)\s+)?(?:map\s*<[^>]*>|[A-Za-z_][\w.]*)\s+([A-Za-z_]\w*)\s*=\s*\d+/
const PROTO_ENUM_VALUE = /^\s*([A-Za-z_]\w*)\s*=\s*-?\d+/
const PROTO_SKIP = /^\s*(?:option|reserved|extensions|import|package|syntax)\b/

/** A line without its comment and string contents, so their braces do not count. */
const code = (/** @type {string} */ line, /** @type {boolean} */ graphql) =>
  line.replace(/"(?:[^"\\]|\\.)*"/g, '""').replace(graphql ? /#.*$/ : /\/\/.*$/, '')

/**
 * The block enclosing line `index` (0-based): the nearest unclosed `{` above it, with the
 * declaration on that line (or the line before), as `{index, kind, name, extend}` or null.
 * @param {string[]} lines @param {number} index @param {RegExp} decl @param {boolean} graphql
 */
function enclosing(lines, index, decl, graphql) {
  let depth = 0
  for (let i = index - 1; i >= 0; i--) {
    const line = code(lines[i], graphql)
    for (let j = line.length - 1; j >= 0; j--) {
      if (line[j] === '}') depth++
      else if (line[j] === '{' && depth-- === 0) {
        const match = decl.exec(line) ?? (i > 0 ? decl.exec(code(lines[i - 1], graphql)) : null)
        if (!match) return null
        const [, extend, kind, name] = graphql ? match : [match[0], '', match[1], match[2]]
        return { index: decl.test(line) ? i : i - 1, kind, name, extend: Boolean(extend) }
      }
    }
  }
  return null
}

/**
 * Declarations a diff adds to a proto or GraphQL file, read with a line grammar. GraphQL reports
 * `type`, `input`, `interface` and `enum` declarations and their fields or values qualified by the
 * enclosing type (`Hospital.state`); proto reports `message`, `enum` and `service` declarations,
 * `rpc` names, and fields or enum values qualified by the enclosing message or enum (`Order.note`,
 * through any `oneof`). A field of a declaration the same diff adds is covered by the declaration.
 * Other formats, OpenAPI included, return nothing.
 * @param {{path: string, headText: string, addedLines: number[]}} input
 * @returns {string[]}
 */
export function addedApiSymbols({ path, headText, addedLines }) {
  const graphql = GRAPHQL.test(path)
  if (!graphql && !PROTO.test(path)) return []
  const lines = String(headText).split(/\r\n|\n|\r/)
  const added = new Set(addedLines)
  const decl = graphql ? GRAPHQL_DECL : PROTO_DECL
  /** @type {string[]} */
  const symbols = []
  const push = (/** @type {string} */ symbol) => {
    if (!symbols.includes(symbol)) symbols.push(symbol)
  }
  for (const number of [...added].sort((a, b) => a - b)) {
    const index = number - 1
    const line = code(lines[index] ?? '', graphql)
    if (!line.trim() || (!graphql && PROTO_SKIP.test(line))) continue
    let parent = enclosing(lines, index, decl, graphql)
    // A oneof is not a namespace: its fields belong to the message around it.
    while (!graphql && parent?.kind === 'oneof')
      parent = enclosing(lines, parent.index, decl, graphql)
    const declared = decl.exec(line)
    if (declared) {
      const [kind, name, extend] = graphql
        ? [declared[2], declared[3], declared[1]]
        : [declared[1], declared[2], '']
      if (extend || kind === 'oneof') continue
      if (parent && added.has(parent.index + 1)) continue
      push(parent ? `${parent.name}.${name}` : name)
      continue
    }
    if (!parent || added.has(parent.index + 1)) continue
    if (graphql) {
      const field =
        parent.kind === 'enum' ? GRAPHQL_ENUM_VALUE.exec(line) : GRAPHQL_FIELD.exec(line)
      if (field) push(`${parent.name}.${field[1]}`)
      continue
    }
    const rpc = PROTO_RPC.exec(line)
    if (rpc && parent.kind === 'service') push(rpc[1])
    const field =
      parent.kind === 'enum'
        ? PROTO_ENUM_VALUE.exec(line)
        : parent.kind === 'message'
          ? PROTO_FIELD.exec(line)
          : null
    if (field) push(`${parent.name}.${field[1]}`)
  }
  return symbols
}

// ---------------------------------------------------------------------------------------------
// Mentions

const SYMBOL = /^[A-Za-z_][\w]*(?:\.[A-Za-z_][\w]*)+$/
const IDENTIFIER = /[A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)*/g

/**
 * Whether a name is identifier-shaped and salient: dotted, underscored or lower camelCase
 * (`retryCount`). A capitalised word with inner capitals (`GraphQL`, `TypeScript`) is usually a
 * proper noun, so it is not salient unless backticked.
 */
const salient = (/** @type {string} */ token) =>
  token.includes('.') || token.includes('_') || /^[a-z][a-z0-9]*[A-Z]/.test(token)

/** The tokens that make an item judgeable: backticked spans and salient identifiers. */
function salientTokens(/** @type {string} */ item) {
  const spans = [...item.matchAll(/`([^`]+)`/g)].map((match) => match[1].trim()).filter(Boolean)
  const rest = item.replace(/`[^`]*`/g, ' ')
  return [...spans, ...(rest.match(IDENTIFIER) ?? []).filter(salient)]
}

/** Whether `token` is mentioned: present whole, or for `A.b` both `A` and `b` as whole words. */
function mentioned(/** @type {string} */ token, /** @type {string} */ haystack) {
  if (hasToken(haystack, token, 'i')) return true
  if (!SYMBOL.test(token)) return false
  return token.split('.').every((part) => hasToken(haystack, part, 'i'))
}

/**
 * The facts and symbols no text mentions, in input order (facts first). A symbol, bare (`Order`,
 * `GetOrder`) or qualified (`A.b`), is judged by its own name: `A.b` counts as mentioned when
 * `A.b` appears or both `A` and `b` appear as whole words (case-insensitive). A describer item in
 * `facts` counts as mentioned when any of its salient tokens does, and an item with no salient
 * token cannot be judged and is never flagged; a dotted fact is judged as a symbol.
 * @param {{facts?: string[], symbols?: string[], texts: string[]}} input @returns {string[]}
 */
export function unmentioned({ facts = [], symbols = [], texts }) {
  const haystack = texts.filter((value) => typeof value === 'string').join('\n')
  const judged = [
    ...facts.map((fact) => ({ fact, tokens: SYMBOL.test(fact) ? [fact] : salientTokens(fact) })),
    ...symbols.map((symbol) => ({ fact: symbol, tokens: [symbol] })),
  ]
  const seen = new Set()
  return judged
    .filter(({ fact }) => !seen.has(fact) && Boolean(seen.add(fact)))
    .filter(({ tokens }) => tokens.length > 0 && !tokens.some((t) => mentioned(t, haystack)))
    .map(({ fact }) => fact)
}
