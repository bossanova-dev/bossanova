// Pure, dependency-free core for the unattended bs-sweep-releases cron gate.
import { readFileSync } from 'node:fs'

import { isMainModule } from '../skills-toolbox/main-module.mjs'

export const REVIEW_BOT_LOGIN = 'chatgpt-codex-connector[bot]'
export const RELEASE_BASE_BRANCHES = ['staging', 'production']
export const RELEASE_MARKER_PREFIX = 'Release review: '
// Keep the single positional body argument comfortably below the 64 KiB limits
// used by Boss command transports. The marker and all dedupe semantics remain
// intact even when verbose review evidence is truncated.
export const MAX_NOTE_BODY_BYTES = 60 * 1024
const MAX_MARKER_BYTES = 2 * 1024
const MARKER_VERSION = 'v2:'

const LINE_TERMINATORS = /[\r\n\u2028\u2029]+/g
const RELEASE_MARKER_RE =
  /(?:^|[\r\n\u2028\u2029])(Release review: [^\r\n\u2028\u2029]+?)[ \t]*(?:\r\n|[\r\n\u2028\u2029])?$/
const RFC3339_RE =
  /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/

/** Collapse every JavaScript line separator in untrusted review text. */
export function sanitize(text) {
  return String(text ?? '').replace(LINE_TERMINATORS, ' ')
}

export function isReviewBotComment(comment) {
  return comment?.user?.login === REVIEW_BOT_LOGIN
}

export function parseFinding(comment) {
  const source = comment && typeof comment === 'object' ? comment : {}
  const body = typeof source.body === 'string' ? source.body : ''
  const firstLine =
    body
      .split(/[\r\n\u2028\u2029]/)
      .map((line) => line.trim())
      .find(Boolean) ?? ''
  const badge = firstLine.match(/!\[\s*(P\d+)\s+Badge\s*\]/)
  const headline = (
    badge
      ? firstLine.slice(firstLine.indexOf(badge[0]) + badge[0].length).replace(/^\([^)]*\)/, '')
      : firstLine
  )
    .replace(/<\/?sub>/g, '')
    .replace(/^[\s*_`]+|[\s*_`]+$/g, '')
    .trim()
  const line = Number(source.line ?? source.original_line)
  return {
    body,
    headline,
    line: Number.isFinite(line) ? line : null,
    path: typeof source.path === 'string' && source.path !== '' ? source.path : null,
    severity: badge?.[1] ?? null,
    side: typeof source.side === 'string' ? source.side : null,
    url: typeof source.html_url === 'string' ? source.html_url : null,
  }
}

export function clusterKey(comment) {
  const finding = parseFinding(comment)
  return finding.path === null ? null : `${finding.path}@${finding.line ?? 0}`
}

export function markerFor(cluster) {
  const key = typeof cluster === 'string' ? cluster : cluster?.key
  if (typeof key !== 'string' || key.length === 0)
    throw new Error('release marker requires an anchor')
  const marker = `${RELEASE_MARKER_PREFIX}${MARKER_VERSION}${Buffer.from(key).toString('base64url')}`
  if (Buffer.byteLength(marker) > MAX_MARKER_BYTES) {
    throw new Error('release marker exceeds the safe note-body budget')
  }
  return marker
}

// Old notes used their sanitized anchor as the marker. Preserve dedupe for
// anchors that were already lossless under that format; unsafe anchors cannot
// be matched safely because different raw paths can sanitize to the same text.
function legacyMarkerFor(cluster) {
  const key = typeof cluster === 'string' ? cluster : cluster?.key
  if (typeof key !== 'string' || key.length === 0 || sanitize(key) !== key) return null
  return `${RELEASE_MARKER_PREFIX}${key}`
}

/** Report whether a finding cluster is already represented in a note snapshot. */
export function isTrackedReleaseMarker(cluster, trackedMarkers) {
  const tracked = trackedMarkers instanceof Set ? trackedMarkers : new Set(trackedMarkers ?? [])
  const marker =
    typeof cluster === 'string' ? markerFor(cluster) : (cluster?.marker ?? markerFor(cluster))
  const legacyMarker = legacyMarkerFor(cluster)
  return tracked.has(marker) || (legacyMarker !== null && tracked.has(legacyMarker))
}

/** Group findings by durable source anchor, retaining every comment at that anchor. */
export function clusterComments(comments) {
  const clusters = new Map()
  for (const comment of Array.isArray(comments) ? comments : []) {
    if (!comment || typeof comment !== 'object') continue
    const key = clusterKey(comment)
    if (key === null) continue
    if (!clusters.has(key))
      clusters.set(key, { key, marker: markerFor(key), findings: [], prUrls: [] })
    const cluster = clusters.get(key)
    const finding = parseFinding(comment)
    cluster.findings.push(finding)
    const prUrl = finding.url?.split('#')[0]
    if (prUrl && !cluster.prUrls.includes(prUrl)) cluster.prUrls.push(prUrl)
  }
  return [...clusters.values()]
}

function truncateUtf8(value, maxBytes) {
  const bytes = Buffer.from(String(value ?? ''), 'utf8')
  if (bytes.length <= maxBytes) return bytes.toString('utf8')
  let end = Math.max(0, maxBytes)
  while (end > 0 && (bytes[end] & 0xc0) === 0x80) end -= 1
  return bytes.subarray(0, end).toString('utf8')
}

/** Render a bounded note with the generated marker as its exact final line. */
export function bodyFor(cluster, marker = markerFor(cluster)) {
  const findings = Array.isArray(cluster?.findings) ? cluster.findings : []
  const anchor = findings[0] ?? {}
  const path = sanitize(anchor.path ?? '(unknown path)')
  const line = anchor.line ?? 0
  const side = sanitize(anchor.side ?? 'RIGHT')
  const suffix = `\n\n${marker}`
  const suffixBytes = Buffer.byteLength(suffix)
  if (suffixBytes >= MAX_NOTE_BODY_BYTES) throw new Error('release marker exceeds note-body limit')
  const budget = MAX_NOTE_BODY_BYTES - suffixBytes
  const truncation =
    '\n\n> [Finding details truncated to keep this note below the Boss CLI size limit.]'
  let body = ''
  let complete = true
  const append = (value) => {
    if (!complete) return false
    const text = String(value ?? '')
    const available = budget - Buffer.byteLength(body)
    if (Buffer.byteLength(text) <= available) {
      body += text
      return true
    }
    complete = false
    const noticeBytes = Buffer.byteLength(truncation)
    body += truncateUtf8(text, Math.max(0, available - noticeBytes))
    if (budget - Buffer.byteLength(body) >= noticeBytes) body += truncation
    return false
  }

  append(`Automated release review finding on \`${path}\` line \`${line}\`.\n\n## Findings`)
  for (const finding of findings) {
    const severity = finding.severity ? `[${sanitize(finding.severity)}] ` : ''
    const headline = sanitize(finding.headline).trim() || '(no headline)'
    if (!append(`\n\n### ${severity}${headline}\n\n> ${sanitize(finding.body).trim()}`)) break
    if (finding.url && !append(`\n\nSource: ${sanitize(finding.url)}`)) break
  }
  append(`\n\n## Context\n\n- anchor: \`${path}\` line \`${line}\` (\`${side}\` side)`)
  for (const url of cluster?.prUrls ?? []) {
    if (!append(`\n- seen on: ${sanitize(url)}`)) break
  }
  append('\n- filed by the `bs-sweep-releases` cron sweep; dedupe keys off the marker below')
  return `${body}${suffix}`
}

/**
 * Select every bot-authored, unmarked anchor cluster. The note snapshot is supplied
 * by the caller and never re-read here: one run decides from one immutable view.
 */
export function selectUnseenFindings({ comments, trackedMarkers } = {}) {
  const tracked = trackedMarkers instanceof Set ? trackedMarkers : new Set(trackedMarkers ?? [])
  return clusterComments((Array.isArray(comments) ? comments : []).filter(isReviewBotComment))
    .filter((cluster) => !isTrackedReleaseMarker(cluster, tracked))
    .sort((left, right) => left.key.localeCompare(right.key))
    .map((cluster) => ({
      key: cluster.key,
      marker: cluster.marker,
      body: bodyFor(cluster),
      count: cluster.findings.length,
    }))
}

function noteText(note) {
  if (!note || typeof note !== 'object') throw new Error('Boss note is malformed')
  for (const field of ['body', 'content', 'description']) {
    if (typeof note[field] === 'string') return note[field]
  }
  throw new Error('Boss note has no readable text')
}

/** Extract only exact marker lines from a completed Boss improvement-note snapshot. */
export async function fetchTrackedReleaseMarkers({ notes } = {}) {
  const list = Array.isArray(notes) ? notes : Array.isArray(notes?.notes) ? notes.notes : null
  if (list === null) throw new Error('Boss note snapshot must be an array')
  const markers = new Set()
  for (const note of list) {
    const marker = noteText(note).match(RELEASE_MARKER_RE)?.[1]
    if (marker) markers.add(marker.replace(/[ \t]+$/, ''))
  }
  return markers
}

function parseRfc3339(value, field) {
  const match = typeof value === 'string' ? RFC3339_RE.exec(value) : null
  if (!match) throw new Error(`invalid ${field} timestamp`)
  const [, yearText, monthText, dayText, hourText, minuteText, secondText] = match
  const [year, month, day, hour, minute, second] = [
    yearText,
    monthText,
    dayText,
    hourText,
    minuteText,
    secondText,
  ].map(Number)
  const daysInMonth = [
    31,
    year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0) ? 29 : 28,
    31,
    30,
    31,
    30,
    31,
    31,
    30,
    31,
    30,
    31,
  ]
  if (
    month < 1 ||
    month > 12 ||
    day < 1 ||
    day > daysInMonth[month - 1] ||
    hour > 23 ||
    minute > 59 ||
    second > 59
  ) {
    throw new Error(`invalid ${field} timestamp`)
  }
  const millis = Date.parse(value)
  if (!Number.isFinite(millis)) throw new Error(`invalid ${field} timestamp`)
  return millis
}

function resolveNow(now) {
  const value = typeof now === 'function' ? now() : now
  const millis = value instanceof Date ? value.getTime() : Date.parse(value)
  if (!Number.isFinite(millis)) throw new Error('invalid now clock')
  return millis
}

/**
 * Fetch every staging/production PR through the injected all-state pager. A bad
 * response, timestamp, or incomplete page sequence throws so callers skip safely.
 */
export async function collectReleasePullRequests({
  listPullRequests,
  now = () => new Date(),
  maxPages = 20,
} = {}) {
  if (typeof listPullRequests !== 'function') throw new Error('listPullRequests is required')
  if (!Number.isInteger(maxPages) || maxPages < 1)
    throw new Error('maxPages must be a positive integer')
  const cutoff = resolveNow(now) - 7 * 24 * 60 * 60 * 1000
  const releases = []
  const seenNumbers = new Set()
  for (const base of RELEASE_BASE_BRANCHES) {
    for (let page = 1; page <= maxPages; page += 1) {
      const response = await listPullRequests({ base, state: 'all', page, perPage: 100 })
      if (
        !response ||
        typeof response !== 'object' ||
        !Array.isArray(response.items) ||
        typeof response.hasNextPage !== 'boolean'
      ) {
        throw new Error(`invalid ${base} release PR page`)
      }
      for (const pullRequest of response.items) {
        if (
          !pullRequest ||
          typeof pullRequest !== 'object' ||
          !Number.isFinite(pullRequest.number)
        ) {
          throw new Error(`invalid ${base} release PR`)
        }
        const createdAt = parseRfc3339(pullRequest.createdAt, 'createdAt')
        const updatedAt = parseRfc3339(pullRequest.updatedAt, 'updatedAt')
        if ((createdAt >= cutoff || updatedAt >= cutoff) && !seenNumbers.has(pullRequest.number)) {
          seenNumbers.add(pullRequest.number)
          releases.push(pullRequest)
        }
      }
      if (!response.hasNextPage) break
      if (page === maxPages) throw new Error(`incomplete ${base} release PR pagination`)
    }
  }
  return releases
}

/** Small shell-out seam for skills: select <comments.json> <notes.json>. */
export function runCli(argv, { readFile = (file) => readFileSync(file, 'utf8') } = {}) {
  const [command, commentsFile, notesFile] = argv
  if (command !== 'select') throw new Error(`unknown subcommand: ${command}`)
  const comments = JSON.parse(readFile(commentsFile))
  const notes = JSON.parse(readFile(notesFile))
  const markers = new Set()
  const list = Array.isArray(notes) ? notes : Array.isArray(notes?.notes) ? notes.notes : null
  if (list === null) throw new Error('Boss note snapshot must be an array')
  for (const note of list) {
    const marker = noteText(note).match(RELEASE_MARKER_RE)?.[1]
    if (marker) markers.add(marker.replace(/[ \t]+$/, ''))
  }
  return JSON.stringify(selectUnseenFindings({ comments, trackedMarkers: markers }))
}

if (isMainModule(import.meta.url)) {
  try {
    process.stdout.write(`${runCli(process.argv.slice(2))}\n`)
  } catch (error) {
    process.stderr.write(`sweep-releases-gate: ${error.message}\n`)
    process.exitCode = 1
  }
}
