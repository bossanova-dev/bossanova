import { globToRegExp } from '../skill-config.mjs'
import {
  slug,
  validateDecisions,
  decodePolicyRecord,
  validJudgeEntry,
  JUDGE_LIMITS,
} from './verify-merge-prose.mjs'

/**
 * @typedef {{path?: string, previousPath?: string, filename?: string, previous_filename?: string}} JudgeFile
 * @typedef {{label: string, path: string, reason: string}} JudgeEntry
 * @typedef {{area: string, label: string, paths: string[], defaultAsk: 'always', source: 'judge', reason: string}} JudgeArea
 * @typedef {{area: string, label: string, paths?: readonly string[], defaultAsk: string, source?: string, items?: readonly string[], change?: string}} EnvelopeArea
 * @typedef {{headSha: string, files: readonly JudgeFile[], noSignOff?: readonly string[]}} JudgeContext
 */

export { JUDGE_LIMITS } from './verify-merge-prose.mjs'

/** @param {unknown} value @returns {value is Record<string, unknown>} */
const object = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)

/** Closed data envelope: no review transcript, build notes or arbitrary object keys cross it.
 * @param {{headSha: string, baseSha: string, files: readonly JudgeFile[], areas: readonly EnvelopeArea[], confidence: {level: string, asks: readonly {area: string, label: string, source?: string}[]}, pr: {title?: string, body?: string}, mergePolicy?: import('./verify-merge-prose.mjs').MergePolicy}} input
 */
export function judgeEnvelope({ headSha, baseSha, files, areas, confidence, pr, mergePolicy }) {
  return {
    version: 1,
    headSha,
    baseSha,
    files: files.map((file) => ({
      path: file.path ?? file.filename ?? '',
      ...((file.previousPath ?? file.previous_filename)
        ? { previousPath: file.previousPath ?? file.previous_filename }
        : {}),
    })),
    areas: areas.map(({ area, label, paths, defaultAsk, source, items, change }) => ({
      area,
      label,
      paths: [...(paths ?? [])],
      defaultAsk,
      ...(source === undefined ? {} : { source }),
      ...(items === undefined ? {} : { items: [...items] }),
      ...(change === undefined ? {} : { change }),
    })),
    ...(mergePolicy?.state === 'ok'
      ? { mergePolicy: { path: mergePolicy.path, blob: mergePolicy.blob, text: mergePolicy.text } }
      : {}),
    confidence: {
      level: confidence.level,
      asks: confidence.asks.map(({ area, label, source }) => ({
        area,
        label,
        ...(source === undefined ? {} : { source }),
      })),
    },
    pr: { title: pr.title ?? '', body: pr.body ?? '' },
  }
}

/** Validate additions using changed paths independently collected by the gate.
 * @param {unknown} output @param {{headSha: string, files: readonly JudgeFile[], mergePolicy?: import('./verify-merge-prose.mjs').MergePolicy}} context
 * @returns {{ok: true, areas: JudgeArea[], decisions: import('./verify-merge-prose.mjs').PolicyDecision[], health: string[] }|{ok: false, error: string}}
 */
export function validateJudgeOutput(output, { headSha, files, mergePolicy = { state: 'none' } }) {
  if (!object(output)) return { ok: false, error: 'Judge output must be an object' }
  if (output.version !== 1) return { ok: false, error: 'Unsupported judge output version' }
  if (output.head !== headSha) return { ok: false, error: 'Judge output head does not match' }
  if (mergePolicy.state === 'ok' && output.policyBlob !== mergePolicy.blob)
    return { ok: false, error: 'Judge output policy blob does not match' }
  if (!Array.isArray(output.areas)) return { ok: false, error: 'Judge areas must be an array' }
  const changed = new Set(
    files.flatMap((f) => [f.path, f.filename, f.previousPath, f.previous_filename]).filter(Boolean),
  )
  const health = new Set()
  if (
    Object.keys(output).some(
      (key) => !['version', 'head', 'policyBlob', 'areas', 'decisions'].includes(key),
    )
  )
    health.add('judge-keys-ignored')
  /** @type {JudgeArea[]} */
  const areas = []
  const seen = new Set()
  for (const value of output.areas) {
    if (
      object(value) &&
      Object.keys(value).some((key) => !['label', 'path', 'reason'].includes(key))
    )
      health.add('judge-keys-ignored')
    if (!validJudgeEntry(value) || !changed.has(value.path)) {
      health.add('judge-area-rejected')
      continue
    }
    const area = `judge:${slug(value.label)}`
    if (seen.has(area)) continue
    seen.add(area)
    if (areas.length >= JUDGE_LIMITS.maxAreas) {
      health.add('judge-areas-truncated')
      continue
    }
    areas.push({
      area,
      label: value.label,
      paths: [value.path],
      defaultAsk: 'always',
      source: 'judge',
      reason: value.reason,
    })
  }
  const prose = validateDecisions(output.decisions, {
    policy: mergePolicy,
    files,
    judgeLabels: areas.map((area) => area.label),
  })
  return {
    ok: true,
    areas,
    decisions: prose.decisions,
    health: [...new Set([...health, ...prose.health])],
  }
}

/** The append-only merge preserves deterministic objects and every describer field.
 * @template T, U @param {readonly T[]} areas @param {readonly U[]} judgeAreas @returns {(T|U)[]}
 */
export function withJudgeAreas(areas, judgeAreas) {
  return [...areas, ...judgeAreas]
}

/** Judge areas as record entries, one path each.
 * @param {readonly JudgeArea[]} areas @returns {JudgeEntry[]}
 */
export const judgeEntries = (areas) =>
  areas.map(({ label, paths, reason }) => ({ label, path: paths[0], reason }))

/** @param {readonly JudgeArea[]} areas @param {{policy: import('./verify-merge-prose.mjs').MergePolicy, decisions: import('./verify-merge-prose.mjs').PolicyDecision[]}} [prose] */
export function encodeJudgeRecord(areas, prose) {
  const entries = judgeEntries(areas)
  const record =
    !prose || prose.policy.state === 'none'
      ? entries
      : {
          areas: entries,
          policyBlob:
            prose.policy.state === 'ok' ? prose.policy.blob : `unreadable:${prose.policy.why}`,
          decisions: prose.decisions,
        }
  return Buffer.from(JSON.stringify(record)).toString('base64url')
}

/** Malformed records are rejected atomically, never partially recovered.
 * @param {unknown} value @returns {JudgeEntry[]|null}
 */
export function parseJudgeRecord(value) {
  return decodePolicyRecord(value)?.areas ?? null
}

/** @param {unknown} value */
export function parseJudgeDecisions(value) {
  const record = decodePolicyRecord(value)
  return record?.policyBlob === undefined
    ? null
    : { policyBlob: record.policyBlob, decisions: record.decisions ?? [] }
}

/** @param {string} last */
export const matchJudgeRecordMarker = (last) =>
  last.match(/^<!-- boss-judge-record head=([0-9a-fA-F]{40}) areas=(\S+) -->$/)

/** @param {string} headSha @param {string} record @param {readonly JudgeEntry[]} areas */
export function judgeRecordComment(headSha, record, areas) {
  return [
    areas.length
      ? `Review added areas: ${areas.map((area) => `${area.label} (\`${area.path}\`): ${area.reason}`).join('; ')}`
      : 'The judge found no additional review areas.',
    judgeRecordMarker(headSha, record),
  ].join('\n\n')
}

/** @param {string} headSha @param {string} record */
export function judgeRecordMarker(headSha, record) {
  return `<!-- boss-judge-record head=${headSha} areas=${record} -->`
}

/**
 * Union trusted, head-filtered records in comment order; a later empty record cannot retract.
 * `areas` ask; `notes` are the findings whose every path (all rename sides) falls under
 * `context.noSignOff`, kept for the proof page instead of asking. Undefined when no record was
 * assessed.
 * @param {readonly string[]} records @param {JudgeContext} context
 * @returns {{areas: JudgeArea[], notes: JudgeArea[]}|undefined}
 */
export function judgeReview(records, context) {
  const noSignOff = (context.noSignOff ?? []).map(globToRegExp)
  /** @param {string} path */
  const pathsFor = (path) => {
    const paths = new Set([path])
    for (const file of context.files) {
      const pair = [file.path ?? file.filename, file.previousPath ?? file.previous_filename].filter(
        Boolean,
      )
      if (pair.includes(path)) for (const side of pair) paths.add(side)
    }
    return [...paths]
  }
  /** @type {JudgeArea[]} */
  const areas = []
  /** @type {JudgeArea[]} */
  const notes = []
  const seen = new Set()
  const noted = new Set()
  let assessed = false
  for (const record of records) {
    const entries = parseJudgeRecord(record)
    if (entries === null) continue
    assessed = true
    const result = validateJudgeOutput(
      { version: 1, head: context.headSha, areas: entries },
      context,
    )
    if (!result.ok) continue
    for (const area of result.areas) {
      const quiet =
        noSignOff.length > 0 &&
        area.paths.flatMap(pathsFor).every((path) => noSignOff.some((re) => re.test(path)))
      const [list, ids] = quiet ? [notes, noted] : [areas, seen]
      if (ids.has(area.area)) continue
      ids.add(area.area)
      list.push(area)
    }
  }
  return assessed ? { areas, notes } : undefined
}

/** The asking judge areas alone.
 * @param {readonly string[]} records @param {JudgeContext} context
 * @returns {JudgeArea[]|undefined}
 */
export function judgeAreasFor(records, context) {
  return judgeReview(records, context)?.areas
}
