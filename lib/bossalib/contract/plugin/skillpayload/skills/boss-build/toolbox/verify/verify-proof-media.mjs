/**
 * `proof-media/1`: what a repo's proof-media extension captured for one judged head. Validation
 * keeps every valid clip, still, chapter and transcript and drops each invalid one with its reason.
 */
import {
  Drop,
  compact,
  isRecord,
  keepEach,
  needText,
  onlyKeys,
  optText,
} from './verify-proof-spec.mjs'

export const PROOF_MEDIA_SCHEMA = 'proof-media/1'
export const MAX_CLIPS = 6
export const MAX_STILLS = 12
export const MAX_CHAPTERS = 12
export const MAX_TRANSCRIPTS = 4
export const MAX_CLIP_MS = 60_000
export const MAX_FILE_BYTES = 15 * 1024 * 1024
export const MAX_TOTAL_BYTES = 60 * 1024 * 1024
export const MAX_CAPTION_CHARS = 120
export const MAX_TRANSCRIPT_JSON_CHARS = 32 * 1024
export const CLIP_SIDES = Object.freeze(['head', 'base'])
export const VIDEO_TYPES = Object.freeze(['.mp4'])
export const IMAGE_TYPES = Object.freeze(['.png', '.jpg', '.webp'])

/**
 * @typedef {import('./verify-proof-spec.mjs').Dropped} Dropped
 * @typedef {{atMs: number, caption: string}} Chapter
 * @typedef {{file: string, caption: string, atMs?: number}} Still
 * @typedef {{id: string, side: string, title: string, file: string, poster?: string, durationMs: number, chapters: Chapter[], stills: Still[]}} Clip
 * @typedef {{title: string, note?: string, status: number, ms: number, request: unknown, response: unknown, rows?: string}} Transcript
 * @typedef {{schema: string, head: string, source?: {kind: string, url: string}, clips: Clip[], transcripts: Transcript[]}} ProofMedia
 * @typedef {{head: string, sizeOf?: (file: string) => number | null}} MediaContext
 */

/** The published name of a media file: the page serves every file as `media/<basename>`.
 * @param {string} file */
export const mediaName = (file) => `media/${file.slice(file.lastIndexOf('/') + 1)}`

/** @param {unknown} value @param {string} field */
function needMs(value, field) {
  if (!Number.isSafeInteger(value) || /** @type {number} */ (value) < 0)
    throw new Drop(`${field}: expected milliseconds`)
  return /** @type {number} */ (value)
}

/**
 * Validate a `proof-media/1` manifest against the judged head. `sizeOf(file)` returns a file's
 * bytes, or null when it is missing; without it the size caps are not checked. Files count toward
 * the total in manifest order, so the first files to break the total are the ones dropped.
 * @param {unknown} value @param {MediaContext} context
 * @returns {{media: ProofMedia | null, dropped: Dropped[]}}
 */
export function validateProofMedia(value, { head, sizeOf }) {
  /** @type {Dropped[]} */
  const dropped = []
  if (!isRecord(value) || value.schema !== PROOF_MEDIA_SCHEMA)
    return {
      media: null,
      dropped: [{ at: 'media', reason: `not a ${PROOF_MEDIA_SCHEMA} manifest` }],
    }
  if (typeof value.head !== 'string' || value.head.toLowerCase() !== head.toLowerCase())
    return { media: null, dropped: [{ at: 'media.head', reason: 'captured for another head' }] }
  for (const key of Object.keys(value))
    if (!['schema', 'head', 'source', 'clips', 'transcripts'].includes(key))
      dropped.push({ at: `media.${key}`, reason: 'unknown key' })
  let total = 0
  /** @type {Map<string, string>} */
  const names = new Map()
  /** A relative file of one of `types`, under the per-file and total caps, with a basename no
   * other file publishes. @param {unknown} raw @param {readonly string[]} types @param {string} field */
  const file = (raw, types, field) => {
    const path = needText(raw, 300, field)
    if (
      path.startsWith('/') ||
      path.includes('\\') ||
      path.split('/').some((p) => p === '..' || p === '')
    )
      throw new Drop(`${field}: expected a relative path inside the evidence directory`)
    if (!/^[A-Za-z0-9._-]+$/.test(path.slice(path.lastIndexOf('/') + 1)))
      throw new Drop(`${field}: a file name uses only letters, digits, dot, dash and underscore`)
    const dot = path.lastIndexOf('.')
    if (dot < 0 || !types.includes(path.slice(dot).toLowerCase()))
      throw new Drop(`${field}: expected one of ${types.join(', ')}`)
    const name = mediaName(path)
    if (names.has(name) && names.get(name) !== path)
      throw new Drop(`${field}: ${name} is already published from ${names.get(name)}`)
    if (sizeOf && !names.has(name)) {
      const bytes = sizeOf(path)
      if (bytes === null) throw new Drop(`${field}: ${path} is missing`)
      if (bytes > MAX_FILE_BYTES)
        throw new Drop(`${field}: ${path} is over ${MAX_FILE_BYTES} bytes`)
      if (total + bytes > MAX_TOTAL_BYTES)
        throw new Drop(`${field}: ${path} takes the media over ${MAX_TOTAL_BYTES} bytes`)
      total += bytes
    }
    names.set(name, path)
    return path
  }
  let source
  if (value.source !== undefined) {
    try {
      const s = onlyKeys(value.source, ['kind', 'url'], 'source')
      const url = needText(s.url, 2048, 'source.url')
      if (!/^https:\/\/[^\s/@]+\//.test(url)) throw new Drop('source.url: expected an HTTPS URL')
      source = { kind: needText(s.kind, 40, 'source.kind'), url }
    } catch (error) {
      if (!(error instanceof Drop)) throw error
      dropped.push({ at: 'media.source', reason: error.message })
    }
  }
  const ids = new Set()
  const clips = keepEach(
    value.clips,
    'clips',
    MAX_CLIPS,
    (entry, at) => {
      const c = onlyKeys(entry, [
        'id',
        'side',
        'title',
        'file',
        'poster',
        'durationMs',
        'chapters',
        'stills',
      ])
      const id = needText(c.id, 40, 'id')
      if (!/^[a-z0-9][a-z0-9-]*$/.test(id)) throw new Drop('id: expected a lower-case identifier')
      if (ids.has(id)) throw new Drop(`id: ${id} appears twice`)
      if (typeof c.side !== 'string' || !CLIP_SIDES.includes(c.side))
        throw new Drop('side: expected head or base')
      const durationMs = needMs(c.durationMs, 'durationMs')
      if (durationMs === 0 || durationMs >= MAX_CLIP_MS)
        throw new Drop(`durationMs: a clip runs under ${MAX_CLIP_MS} ms`)
      const title = needText(c.title, MAX_CAPTION_CHARS, 'title')
      const video = file(c.file, VIDEO_TYPES, 'file')
      let poster
      try {
        poster = c.poster === undefined ? undefined : file(c.poster, IMAGE_TYPES, 'poster')
      } catch (error) {
        if (!(error instanceof Drop)) throw error
        dropped.push({ at: `${at}.poster`, reason: error.message })
      }
      ids.add(id)
      let last = -1
      const chapters = keepEach(
        c.chapters,
        `${at}.chapters`,
        MAX_CHAPTERS,
        (chapter) => {
          const ch = onlyKeys(chapter, ['atMs', 'caption'])
          const atMs = needMs(ch.atMs, 'atMs')
          if (atMs >= durationMs) throw new Drop('atMs: after the end of the clip')
          if (atMs <= last) throw new Drop('atMs: chapters must be in time order')
          const caption = needText(ch.caption, MAX_CAPTION_CHARS, 'caption')
          last = atMs
          return { atMs, caption }
        },
        dropped,
      )
      const stills = keepEach(
        c.stills,
        `${at}.stills`,
        MAX_STILLS,
        (still) => {
          const s = onlyKeys(still, ['file', 'caption', 'atMs'])
          const atMs = s.atMs === undefined ? undefined : needMs(s.atMs, 'atMs')
          if (atMs !== undefined && atMs >= durationMs)
            throw new Drop('atMs: after the end of the clip')
          const caption = needText(s.caption, MAX_CAPTION_CHARS, 'caption')
          return compact({ file: file(s.file, IMAGE_TYPES, 'file'), caption, atMs })
        },
        dropped,
      )
      return compact({ id, side: c.side, title, file: video, poster, durationMs, chapters, stills })
    },
    dropped,
  )
  /** @param {unknown} json @param {string} field */
  const bounded = (json, field) => {
    let text
    try {
      text = JSON.stringify(json)
    } catch {
      text = undefined
    }
    if (text === undefined) throw new Drop(`${field}: expected JSON`)
    if (text.length > MAX_TRANSCRIPT_JSON_CHARS)
      throw new Drop(`${field}: longer than ${MAX_TRANSCRIPT_JSON_CHARS} characters`)
    return json
  }
  const transcripts = keepEach(
    value.transcripts,
    'transcripts',
    MAX_TRANSCRIPTS,
    (entry) => {
      const t = onlyKeys(entry, ['title', 'note', 'status', 'ms', 'request', 'response', 'rows'])
      const status = t.status
      if (!Number.isSafeInteger(status) || Number(status) < 100 || Number(status) > 599)
        throw new Drop('status: expected an HTTP status')
      return compact({
        title: needText(t.title, MAX_CAPTION_CHARS, 'title'),
        note: optText(t.note, 300, 'note'),
        status: Number(status),
        ms: needMs(t.ms, 'ms'),
        request: bounded(t.request ?? {}, 'request'),
        response: bounded(t.response ?? {}, 'response'),
        rows: optText(t.rows, MAX_TRANSCRIPT_JSON_CHARS, 'rows'),
      })
    },
    dropped,
  )
  return {
    media: compact({
      schema: PROOF_MEDIA_SCHEMA,
      head: value.head.toLowerCase(),
      source,
      clips,
      transcripts,
    }),
    dropped,
  }
}
