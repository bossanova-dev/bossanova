import { globToRegExp } from '../skill-config.mjs'

/**
 * @typedef {{path?: string, previousPath?: string, filename?: string, previous_filename?: string}} JudgeFile
 * @typedef {{label: string, path: string, reason: string}} JudgeEntry
 * @typedef {{area: string, label: string, paths: string[], defaultAsk: 'always', source: 'judge', reason: string}} JudgeArea
 * @typedef {{area: string, label: string, paths?: readonly string[], defaultAsk: string, source?: string}} EnvelopeArea
 */

export const JUDGE_LIMITS = Object.freeze({ maxAreas: 8, maxLabelLength: 60, maxReasonLength: 200 })

/** @param {unknown} value @returns {value is Record<string, unknown>} */
const object = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)
/** @param {unknown} value @param {number} limit @returns {value is string} */
const singleLine = (value, limit) =>
  typeof value === 'string' &&
  value.trim().length > 0 &&
  value.length <= limit &&
  !/[\r\n\u2028\u2029]/.test(value)
/** @param {unknown} value @returns {value is JudgeEntry} */
const validEntry = (value) =>
  object(value) &&
  singleLine(value.label, JUDGE_LIMITS.maxLabelLength) &&
  typeof value.path === 'string' &&
  value.path.length > 0 &&
  singleLine(value.reason, JUDGE_LIMITS.maxReasonLength)
/** @param {string} label */
const slug = (label) =>
  label
    .toLowerCase()
    .normalize('NFKD')
    .replace(/[\u0300-\u036f]/g, '')
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-|-$/g, '') || encodeURIComponent(label.toLowerCase())

/** Closed data envelope: no review transcript, build notes or arbitrary object keys cross it.
 * @param {{headSha: string, baseSha: string, files: readonly JudgeFile[], areas: readonly EnvelopeArea[], confidence: {level: string, asks: readonly {area: string, label: string, source?: string}[]}, pr: {title?: string, body?: string}}} input
 */
export function judgeEnvelope({ headSha, baseSha, files, areas, confidence, pr }) {
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
    areas: areas.map(({ area, label, paths, defaultAsk, source }) => ({
      area,
      label,
      paths: [...(paths ?? [])],
      defaultAsk,
      ...(source === undefined ? {} : { source }),
    })),
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
 * @param {unknown} output @param {{headSha: string, files: readonly JudgeFile[]}} context
 * @returns {{ok: true, areas: JudgeArea[], health: string[]}|{ok: false, error: string}}
 */
export function validateJudgeOutput(output, { headSha, files }) {
  if (!object(output)) return { ok: false, error: 'Judge output must be an object' }
  if (output.version !== 1) return { ok: false, error: 'Unsupported judge output version' }
  if (output.head !== headSha) return { ok: false, error: 'Judge output head does not match' }
  if (!Array.isArray(output.areas)) return { ok: false, error: 'Judge areas must be an array' }
  const changed = new Set(
    files.flatMap((f) => [f.path, f.filename, f.previousPath, f.previous_filename]).filter(Boolean),
  )
  const health = new Set()
  if (Object.keys(output).some((key) => !['version', 'head', 'areas'].includes(key)))
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
    if (!validEntry(value) || !changed.has(value.path)) {
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
  return { ok: true, areas, health: [...health] }
}

/** The append-only merge preserves deterministic objects and every describer field.
 * @template T, U @param {readonly T[]} areas @param {readonly U[]} judgeAreas @returns {(T|U)[]}
 */
export function withJudgeAreas(areas, judgeAreas) {
  return [...areas, ...judgeAreas]
}

/** @param {readonly JudgeArea[]} areas */
export function encodeJudgeRecord(areas) {
  return Buffer.from(
    JSON.stringify(areas.map(({ label, paths, reason }) => ({ label, path: paths[0], reason }))),
  ).toString('base64url')
}

/** Malformed records are rejected atomically, never partially recovered.
 * @param {unknown} value @returns {JudgeEntry[]|null}
 */
export function parseJudgeRecord(value) {
  if (typeof value !== 'string' || !/^[A-Za-z0-9_-]+$/.test(value)) return null
  try {
    const bytes = Buffer.from(value, 'base64url')
    if (bytes.toString('base64url') !== value) return null
    const entries = JSON.parse(bytes.toString('utf8'))
    if (
      !Array.isArray(entries) ||
      entries.length > JUDGE_LIMITS.maxAreas ||
      !entries.every(
        (entry) =>
          validEntry(entry) &&
          Object.keys(entry).every((key) => ['label', 'path', 'reason'].includes(key)),
      )
    )
      return null
    return entries
  } catch {
    return null
  }
}

/** @param {string} headSha @param {string} record */
export function judgeRecordMarker(headSha, record) {
  return `<!-- boss-judge-record head=${headSha} areas=${record} -->`
}

/** Union trusted, head-filtered records in comment order; a later empty record cannot retract.
 * @param {readonly string[]} records @param {{headSha: string, files: readonly JudgeFile[], neverReview?: readonly string[]}} context
 * @returns {JudgeArea[]|undefined}
 */
export function judgeAreasFor(records, context) {
  const never = (context.neverReview ?? []).map(globToRegExp)
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
  const seen = new Set()
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
      if (
        never.length > 0 &&
        area.paths.flatMap(pathsFor).every((path) => never.some((re) => re.test(path)))
      )
        continue
      if (seen.has(area.area)) continue
      seen.add(area.area)
      areas.push(area)
    }
  }
  return assessed ? areas : undefined
}
