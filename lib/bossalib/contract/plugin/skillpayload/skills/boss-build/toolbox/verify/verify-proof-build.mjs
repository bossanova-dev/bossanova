// Build a boss-proof/1 record from one judge output. Everything comes from facts the judge already
// computed (its confidence, areas, blockers and notes) plus an optional verify-intent record, so the
// core proves a head with no evidence run of its own: areas carry describer items and no media.
import { readFileSync } from 'node:fs'
import path from 'node:path'
import {
  CLAIM_PROOFS,
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
import { capForClaims, verifyTestClaim } from './verify-claims.mjs'
import { findingHash, isNoteFinding } from './verify-blockers.mjs'
import { DESCRIBED_KEYS } from './verify-describe-blocks.mjs'

// The verdicts whose post records a proof; a wait or claim says nothing final about the head.
export const PROOF_VERDICTS = Object.freeze(['pass', 'blocked', 'defect', 'human'])
// Head-pinned sidecar written by a repo verify extension so the core proof page carries its
// claims and health without the extension publishing a second, shadowing proof page.
export const PROOF_EVIDENCE_SCHEMA = 'boss-proof-evidence/1'

// What each confidence cap code means, for the page's "Why not high" list.
const LOWERED_TEXT = Object.freeze({
  'area-asks': 'It changes areas a person reviews.',
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
  const changeReasons = boundList(area.changeReasons, MAX_ITEMS)
  return {
    items: boundList(area.items, MAX_ITEMS),
    ...(links.length ? { links } : {}),
    ...(area.breaking === true ? { breaking: /** @type {true} */ (true) } : {}),
    ...(['additive', 'breaking'].includes(area.change)
      ? { change: area.change, ...(changeReasons.length ? { changeReasons } : {}) }
      : {}),
  }
}

/** The area's describer blocks, signals and risks; they ride the page sidecar, not the record.
 * @param {Record<string, any>} area */
const describedOf = (area) =>
  Object.fromEntries(
    DESCRIBED_KEYS.filter((key) => Array.isArray(area[key]) && area[key].length).map((key) => [
      key,
      area[key],
    ]),
  )

/**
 * The record's areas: one per judge area, then one per ask no judge area carries (a judge-added
 * area, a capped area, an extension sign-off), each asking when its id is one of the confidence's
 * asks. Names that collide after mapping take a `-<n>` suffix. Over MAX_AREAS, asking areas are
 * kept first. Each carries its verify `id`, `paths` and `described` facts for the page sidecar;
 * build strips them.
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
      described: describedOf(area),
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

/** The finding notes, judge notes on no-sign-off paths, not-applicable notes and defect findings, bounded, in that order. */
function reviewNotes(judged) {
  const titles = (list) =>
    (Array.isArray(list) ? list : []).filter(isObject).map((f) => f.title ?? f.summary)
  return boundList(
    [
      ...titles(judged.findingNotes),
      ...(Array.isArray(judged.judgeNotes) ? judged.judgeNotes : [])
        .filter(isObject)
        .map((note) => `${note.label} (${note.path}): ${note.reason}`),
      ...(Array.isArray(judged.notApplicableNotes) ? judged.notApplicableNotes : [])
        .filter(isObject)
        .map((entry) => `${entry.extension}: ${entry.note}`),
      ...titles(judged.findings),
    ],
    MAX_NOTES,
  )
}

export const MAX_REVIEW_FINDINGS = 50
export const MAX_ACCEPTANCE = 50
const SEVERITY = /^(P[0-3]|style)$/i
const ACCEPTANCE_RESULTS = ['met', 'unmet', 'unchecked']
/** @type {FindingStatus[]} */
const STATUS_ORDER = ['open', 'note', 'fixed']

/**
 * @typedef {'open' | 'fixed' | 'note'} FindingStatus
 * @typedef {{id: string, severity: string, status: FindingStatus, title: string, detail?: string, file?: string, line?: number}} ReviewFinding
 * @typedef {{id: string, text: string, result: string, check?: string, head: string}} AcceptanceRow
 */

/**
 * One review finding as the page shows it. Its id is the blocker id buildBlockers gives the same
 * file, line and title, so one finding read from several judge fields shows once.
 * @param {unknown} f @param {FindingStatus} status @param {string} [blockerId] @returns {ReviewFinding | null}
 */
function reviewFinding(f, status, blockerId) {
  if (!isObject(f)) return null
  const id = blockerId ?? `review:${findingHash(f)}`
  const title = boundText(f.title ?? f.summary)
  if (!title) return null
  const file = typeof f.file === 'string' ? boundText(f.file) : ''
  const line = file && Number.isSafeInteger(f.line) && f.line > 0 ? f.line : null
  const detail = boundText(f.evidence ?? f.text)
  const severity =
    typeof f.severity === 'string' && SEVERITY.test(f.severity.trim()) ? f.severity.trim() : ''
  return {
    id,
    severity: severity.toLowerCase() === 'style' ? 'style' : severity.toUpperCase(),
    status,
    title,
    ...(detail && detail !== title ? { detail } : {}),
    ...(file ? { file } : {}),
    ...(line ? { line } : {}),
  }
}

/**
 * What review found, for the page sidecar: each finding with its severity, status, location and
 * one line of evidence, open ones first, then notes, then those fixed in review, once per id; and
 * the acceptance rows review certified. The open ones include pre-existing and accepted findings,
 * which block nothing but are still unfixed. Never throws.
 * @param {Record<string, any>} judged @returns {{findings: ReviewFinding[], acceptance: AcceptanceRow[]}}
 */
export function reviewSidecar(judged) {
  const list = (/** @type {unknown} */ value) => (Array.isArray(value) ? value : [])
  const blockers = list(judged?.blockers).filter((b) => isObject(b) && b.kind === 'review-finding')
  const found = [
    ...blockers.map((b) => reviewFinding(b, 'open', b.id)),
    ...list(judged?.findings).map((f) => reviewFinding(f, 'open')),
    ...list(judged?.reviewUnfixed).map((f) => reviewFinding(f, isNoteFinding(f) ? 'note' : 'open')),
    ...list(judged?.findingNotes).map((f) => reviewFinding(f, 'note')),
    ...list(judged?.reviewFixed).map((f) => reviewFinding(f, 'fixed')),
  ]
  const byId = new Map()
  for (const finding of found) if (finding && !byId.has(finding.id)) byId.set(finding.id, finding)
  const acceptance = list(judged?.reviewAcceptance)
    .filter(
      (row) =>
        isObject(row) &&
        boundText(row.id) &&
        ACCEPTANCE_RESULTS.includes(row.result) &&
        SHA.test(String(row.head ?? '').toLowerCase()),
    )
    .map((row) => ({
      id: boundText(row.id),
      text: boundText(row.text) || boundText(row.id),
      result: row.result,
      ...(boundText(row.check) ? { check: boundText(row.check) } : {}),
      head: String(row.head).toLowerCase(),
    }))
  return {
    findings: [...byId.values()]
      .sort((a, b) => STATUS_ORDER.indexOf(a.status) - STATUS_ORDER.indexOf(b.status))
      .slice(0, MAX_REVIEW_FINDINGS),
    acceptance: [...new Map(acceptance.map((row) => [row.id, row])).values()].slice(
      0,
      MAX_ACCEPTANCE,
    ),
  }
}

/**
 * The proof record's `review` section from a judge output: the review models, bounded blockers and
 * notes.
 * @param {Record<string, any>} judged
 */
export function proofReview(judged) {
  const blockers = (Array.isArray(judged?.blockers) ? judged.blockers : [])
    .filter(isObject)
    .map(blockerEntry)
    .filter(Boolean)
    .slice(0, MAX_BLOCKERS)
  return { models: reviewModels(judged), blockers, notes: reviewNotes(judged) }
}

/**
 * The proof record's health reasons from a judge output's `confidence.health`.
 * @param {Record<string, any>} judged
 */
export function proofHealth(judged) {
  const confidence = isObject(judged?.confidence) ? judged.confidence : {}
  return (Array.isArray(confidence.health) ? confidence.health : [])
    .map(healthReason)
    .filter(Boolean)
    .slice(0, MAX_HEALTH)
}

/**
 * The proof record's confidence (`{level, asks, loweredBy}`) from a judge output, the resolved
 * proof `areas`, and `blockers`. Shared by the core proof builder and repo evidence extensions.
 *
 * Level rules the contract fixes: blockers make the record `blocked` and only blockers do (a
 * `blocked` confidence without them reads `cannot-assess`); `high` requires no asks and no
 * `loweredBy` (otherwise `moderate`); a level below high always names a reason: `area-asks` when
 * the judge gave none and an area asks, else `confidence-not-computed`.
 *
 * @param {{judged: Record<string, any>, areas: {name: string, ask: boolean}[], blockers?: unknown[]}} input
 */
export function proofConfidence({ judged, areas, blockers = [] }) {
  const confidence = isObject(judged?.confidence) ? judged.confidence : {}
  let level = LEVELS.includes(confidence.level) ? confidence.level : 'cannot-assess'
  const loweredBy = (Array.isArray(confidence.loweredBy) ? confidence.loweredBy : [])
    .filter((code) => typeof code === 'string')
    .map(lowered)
  if (blockers.length) level = 'blocked'
  else if (level === 'blocked') level = 'cannot-assess'
  if (level === 'blocked' && !loweredBy.some((r) => r.code === 'blocker'))
    loweredBy.unshift(lowered('blocker'))
  // High means nothing asks and nothing lowered it; a confidence claiming otherwise is moderate.
  if (level === 'high' && (loweredBy.length || areas.some((area) => area.ask))) level = 'moderate'
  if (level !== 'high' && loweredBy.length === 0)
    loweredBy.push(
      level !== 'cannot-assess' && areas.some((area) => area.ask)
        ? lowered('area-asks')
        : { code: CONFIDENCE_NOT_COMPUTED, text: 'Confidence is not computed yet.' },
    )
  return {
    level,
    asks: areas.filter((area) => area.ask).map((area) => area.name),
    loweredBy: [...new Map(loweredBy.map((r) => [r.code, r])).values()].slice(0, MAX_LOWERED_BY),
  }
}

/**
 * Path to the optional head-pinned evidence sidecar a repo verify extension wrote for `headSha`.
 * @param {string} workspaceRoot @param {string} headSha
 */
export const proofEvidenceFile = (workspaceRoot, headSha) =>
  path.join(workspaceRoot, '.verify-evidence', `proof-evidence-${headSha.toLowerCase()}.json`)

/**
 * @param {unknown} entry
 * @param {{changedPaths: string[], readHeadFile: (path: string) => string | null, hasHeadClip: boolean}} context
 */
function validClaimEntry(entry, { changedPaths, readHeadFile, hasHeadClip }) {
  if (!isObject(entry)) return null
  if (typeof entry.proof !== 'string' || !CLAIM_PROOFS.includes(entry.proof)) return null
  const behaviour = entry.behaviour !== false
  const note =
    typeof entry.note === 'string' && boundText(entry.note) ? boundText(entry.note) : null
  if (entry.proof === 'test') {
    const test = boundText(entry.test)
    const file = boundText(entry.path)
    if (!test || !file) return null
    const verified = verifyTestClaim({ test, path: file }, { changedPaths, readHeadFile })
    if (verified.proof === 'test')
      return { proof: 'test', behaviour, test, path: file, ...(note ? { note } : {}) }
    return { proof: 'unproven', behaviour, ...(verified.note ? { note: verified.note } : {}) }
  }
  if (entry.proof === 'clip') {
    const chapter = boundText(entry.chapter)
    if (!chapter) return null
    if (!hasHeadClip) return { proof: 'unproven', behaviour, note: 'no verified head recording' }
    const atMs = Number.isSafeInteger(entry.atMs) && entry.atMs >= 0 ? entry.atMs : null
    return {
      proof: 'clip',
      behaviour,
      chapter,
      ...(atMs !== null ? { atMs } : {}),
      ...(note ? { note } : {}),
    }
  }
  return { proof: 'unproven', behaviour, ...(note ? { note } : {}) }
}

/**
 * Read the optional `boss-proof-evidence/1` sidecar for `headSha` from `workspaceRoot`. Returns
 * `null` when absent, unreadable, malformed or written for another head. Never throws.
 * @param {{workspaceRoot?: string | null, headSha?: string | null}} input
 */
export function readProofEvidence({ workspaceRoot, headSha } = {}) {
  try {
    const head = String(headSha ?? '').toLowerCase()
    if (!workspaceRoot || !SHA.test(head)) return null
    const raw = JSON.parse(readFileSync(proofEvidenceFile(workspaceRoot, head), 'utf8'))
    if (!isObject(raw) || raw.schema !== PROOF_EVIDENCE_SCHEMA) return null
    if (String(raw.head ?? '').toLowerCase() !== head) return null
    return raw
  } catch {
    return null
  }
}

/**
 * Read git context (`readHeadFile` and `changedPaths`) needed to verify `boss-proof-evidence/1`
 * test claims for `headSha`. Runs `ensureCommit` only when the sidecar actually carries test claims.
 * @param {{read?: ((cmd: string, args: string[], opts?: Record<string, unknown>) => {ok?: boolean, output?: unknown} | null) | null, judged: Record<string, any>, headSha: string, evidence: Record<string, any> | null, ensureCommit?: ((read: any, sha: string) => void) | null}} input
 */
export function evidenceClaimContext({ read, judged, headSha, evidence, ensureCommit = null }) {
  const readHeadFile = (/** @type {string} */ file) => {
    try {
      const out =
        typeof read === 'function'
          ? read('git', ['show', `${headSha}:${file}`], { raw: true })
          : null
      return out?.ok ? String(out.output ?? '') : null
    } catch {
      return null
    }
  }
  let changedPaths = null
  const needsTestDiff =
    evidence !== null &&
    Array.isArray(evidence.claims) &&
    evidence.claims.some((claim) => claim?.proof === 'test')
  const baseForDiff = String(judged?.baseSha ?? judged?.facts?.base?.sha ?? '').toLowerCase()
  if (needsTestDiff && baseForDiff && typeof read === 'function') {
    if (typeof ensureCommit === 'function')
      for (const sha of [baseForDiff, headSha]) ensureCommit(read, sha)
    try {
      const diffOut = read('git', ['diff', '--name-only', `${baseForDiff}...${headSha}`], {
        raw: true,
      })
      if (diffOut?.ok)
        changedPaths = String(diffOut.output ?? '')
          .split('\n')
          .map((line) => line.trim())
          .filter(Boolean)
    } catch {
      changedPaths = null
    }
  }
  return { readHeadFile, changedPaths }
}

/**
 * The boss-proof/1 record for judge output `judged` on `repo`#`pr`, with `page` null, plus each
 * area's verify id, paths and described facts: `{ok: true, proof, areas: [{id, name, paths, ask,
 * described}]}`, or `{ok: false, reason, errors?}` when the judge output names no terminal
 * verdict for a head or the record would not validate. Never throws.
 *
 * Level rules the contract fixes: blockers make the record `blocked` and only blockers do (a
 * `blocked` confidence without them reads `cannot-assess`); a level below high always names a
 * reason: `area-asks` when the judge gave none and an area asks, else `confidence-not-computed`.
 *
 * @param {{judged: Record<string, any>, intent?: unknown, evidence?: Record<string, any> | null, repo: string, pr: number, readHeadFile?: (path: string) => string | null, changedPaths?: string[] | null, hasHeadClip?: boolean}} input
 */
export function buildProofRecord({
  judged,
  intent = null,
  evidence = null,
  repo,
  pr,
  readHeadFile = () => null,
  changedPaths = null,
  hasHeadClip = false,
}) {
  try {
    if (!isObject(judged) || !PROOF_VERDICTS.includes(judged.verdict))
      return { ok: false, reason: 'judge-not-terminal' }
    const head = String(judged.headSha ?? '').toLowerCase()
    const base = String(judged.baseSha ?? judged.facts?.base?.sha ?? '').toLowerCase()
    if (!SHA.test(head) || !SHA.test(base)) return { ok: false, reason: 'judge-without-range' }
    const sidecar =
      isObject(evidence) &&
      evidence.schema === PROOF_EVIDENCE_SCHEMA &&
      String(evidence.head ?? '').toLowerCase() === head
        ? evidence
        : null
    const confidence = isObject(judged.confidence) ? judged.confidence : {}
    const asks = (Array.isArray(confidence.asks) ? confidence.asks : []).filter(
      (ask) => isObject(ask) && typeof ask.area === 'string',
    )
    const named = proofAreas(Array.isArray(judged.facts?.areas) ? judged.facts.areas : [], asks)
    const areas = named.map((entry) => entry.record)
    const review = proofReview(judged)
    const callerWhat = whatItDoesFrom(intent)
    const sidecarWhat = whatItDoesFrom(sidecar?.intent ?? null)
    const baseWhatItDoes = callerWhat.source !== 'unknown' ? callerWhat : sidecarWhat
    const bulletsMatch =
      sidecarWhat.source !== 'unknown' &&
      sidecarWhat.bullets.length === baseWhatItDoes.bullets.length &&
      sidecarWhat.bullets.every((bullet, i) => bullet === baseWhatItDoes.bullets[i])
    const effectiveChangedPaths = Array.isArray(changedPaths)
      ? changedPaths
      : [
          ...new Set([
            ...(Array.isArray(judged.extensionEnvelope?.changedPaths)
              ? judged.extensionEnvelope.changedPaths
              : []),
            ...(Array.isArray(judged.facts?.areas)
              ? judged.facts.areas.flatMap((area) => (Array.isArray(area?.paths) ? area.paths : []))
              : []),
          ]),
        ]
    const rawClaims =
      bulletsMatch && Array.isArray(sidecar?.claims)
        ? sidecar.claims.map((entry) =>
            validClaimEntry(entry, {
              changedPaths: effectiveChangedPaths,
              readHeadFile,
              hasHeadClip,
            }),
          )
        : null
    const claims =
      baseWhatItDoes.source !== 'unknown' &&
      rawClaims &&
      rawClaims.length === baseWhatItDoes.bullets.length &&
      rawClaims.every(Boolean)
        ? /** @type {NonNullable<ReturnType<typeof validClaimEntry>>[]} */ (rawClaims)
        : null
    const whatItDoes = claims ? { ...baseWhatItDoes, claims } : baseWhatItDoes
    const sidecarHealth = (Array.isArray(sidecar?.health) ? sidecar.health : [])
      .map(healthReason)
      .filter(Boolean)
    const judgeHealth = proofHealth(judged)
    const judgeCodes = new Set(judgeHealth.map((entry) => entry.code))
    const seenHealth = new Set()
    /** @type {{code: string, text: string}[]} */
    const mergedHealth = []
    const sidecarBudget = Math.max(0, MAX_HEALTH - judgeHealth.length)
    let nonJudgeSidecar = 0
    for (const entry of sidecarHealth) {
      if (seenHealth.has(entry.code)) continue
      if (!judgeCodes.has(entry.code)) {
        if (nonJudgeSidecar >= sidecarBudget) continue
        nonJudgeSidecar++
      }
      seenHealth.add(entry.code)
      mergedHealth.push(entry)
    }
    for (const entry of judgeHealth) {
      if (seenHealth.has(entry.code)) continue
      seenHealth.add(entry.code)
      mergedHealth.push(entry)
    }
    const health = mergedHealth.slice(0, MAX_HEALTH)
    const record = withDigest({
      schema: PROOF_SCHEMA,
      repo,
      pr,
      head,
      base,
      confidence: capForClaims(
        proofConfidence({ judged, areas, blockers: review.blockers }),
        claims ?? undefined,
      ),
      whatItDoes,
      areas,
      review,
      health,
      page: null,
    })
    const checked = validateProof(record)
    if (!checked.ok) return { ok: false, reason: 'record-invalid', errors: checked.errors }
    return {
      ok: true,
      proof: record,
      areas: named.map(({ id, paths, described, record: area }) => ({
        id,
        name: area.name,
        paths,
        ask: area.ask,
        described,
      })),
    }
  } catch (error) {
    return { ok: false, reason: 'record-failed', errors: [String(error?.message ?? error)] }
  }
}
