// Classify lost proof parts after a landed verdict and build head-bound missing/resolved notes.
import { createHash } from 'node:crypto'

/**
 * @typedef {'record' | 'page' | 'check-run'} ProofProblemPart
 * @typedef {{part: ProofProblemPart, reason: string}} ProofProblem
 * @typedef {{
 *   detached?: boolean,
 *   recorded?: boolean,
 *   reason?: unknown,
 *   note?: unknown,
 *   page?: {published?: boolean, reason?: unknown} | null,
 *   checkRun?: {posted?: boolean, reason?: unknown} | null,
 * }} ProofOutcome
 */

/** @type {readonly string[]} */
export const BENIGN_RECORD_REASONS = Object.freeze(['repo-proof-present'])

/** @type {readonly string[]} */
export const BENIGN_PAGE_REASONS = Object.freeze([
  'not-parked',
  'dry-run',
  'not-configured',
  'disabled',
  'opted-out',
])

/** @type {readonly string[]} */
export const BENIGN_CHECK_RUN_REASONS = Object.freeze([
  'fallback-disabled',
  'dry-run',
  'daemon-unsupported',
])

export const PROOF_MISSING_MARKER =
  /^<!-- boss-proof-missing head=([0-9a-fA-F]{40}) digest=([0-9a-f]{16})(?: state=(resolved))? -->$/

/**
 * @param {string} headSha
 * @param {string} digest
 * @param {boolean} [resolved=false]
 */
export const proofMissingMarker = (headSha, digest, resolved = false) =>
  `<!-- boss-proof-missing head=${headSha} digest=${digest}${resolved ? ' state=resolved' : ''} -->`

/** @param {unknown} value @returns {string} */
const nonEmpty = (value) => (typeof value === 'string' && value.trim() !== '' ? value : '')

/** @param {string} content */
const noteDigest = (content) => createHash('sha256').update(content).digest('hex').slice(0, 16)

/**
 * Classify non-benign proof losses from a head proof outcome.
 * @param {ProofOutcome | null | undefined} outcome
 * @returns {ProofProblem[]}
 */
export function proofProblems(outcome) {
  if (!outcome || typeof outcome !== 'object' || outcome.detached === true) return []
  /** @type {ProofProblem[]} */
  const problems = []
  if (outcome.recorded === false) {
    const reason =
      nonEmpty(outcome.reason) ||
      (outcome.note === 'failed' || outcome.note === 'unknown' ? `note-${outcome.note}` : 'unknown')
    if (!BENIGN_RECORD_REASONS.includes(reason)) problems.push({ part: 'record', reason })
  }
  if (outcome.page?.published === false) {
    const reason = nonEmpty(outcome.page.reason) || 'unknown'
    if (!BENIGN_PAGE_REASONS.includes(reason)) problems.push({ part: 'page', reason })
  }
  if (outcome.checkRun?.posted === false) {
    const reason = nonEmpty(outcome.checkRun.reason) || 'unknown'
    if (!BENIGN_CHECK_RUN_REASONS.includes(reason)) problems.push({ part: 'check-run', reason })
  }
  return problems
}

/**
 * Head-bound PR comment body reporting an incomplete proof after a landed verdict.
 * Never carries a `boss-proof` line, so it can never be parsed as a repository proof.
 * @param {{headSha: string, pr: number | string, repo?: string, problems: readonly ProofProblem[]}} input
 * @returns {{body: string, digest: string}}
 */
export function proofMissingNote({ headSha, pr, repo, problems }) {
  const bullets = problems.map((problem) => `- \`${problem.part}\`: \`${problem.reason}\``)
  const slug = nonEmpty(repo) || '<owner/name>'
  const republish = `verify-gate.mjs proof --pr ${pr} --head ${headSha} --judge <file> --repo ${slug}`
  const content = [
    `The verdict for \`${headSha.slice(0, 12)}\` stands, but the proof is incomplete:`,
    '',
    ...bullets,
    '',
    `Re-run verify on this head, or republish with \`${republish}\`.`,
  ].join('\n')
  const digest = noteDigest(content)
  return { body: `${content}\n\n${proofMissingMarker(headSha, digest)}`, digest }
}

/**
 * Replacement PR comment body once a later proof for the same head succeeds with no problems.
 * Never carries a `boss-proof` line.
 * @param {{headSha: string, pageUrl?: string | null}} input
 * @returns {{body: string, digest: string}}
 */
export function proofResolvedNote({ headSha, pageUrl }) {
  const url = nonEmpty(pageUrl)
  const content = `The proof for \`${headSha.slice(0, 12)}\` is now recorded${url ? `: ${url}` : '.'}`
  const digest = noteDigest(content)
  return { body: `${content}\n\n${proofMissingMarker(headSha, digest, true)}`, digest }
}

/**
 * One-line summary for `boss notes add` naming the PR, short head SHA, and each loss.
 * @param {{pr: number | string, headSha: string, problems: readonly ProofProblem[]}} input
 * @returns {string}
 */
export function proofMissingBossNote({ pr, headSha, problems }) {
  const summary = problems.map((problem) => `${problem.part}: ${problem.reason}`).join(', ')
  return `PR #${pr} (${headSha.slice(0, 12)}) proof incomplete: ${summary}`
}
