/**
 * `proof-media/1`: what a repo's proof-media extension captured for one judged head. Validation
 * keeps every valid clip, still, chapter, transcript, document and page, and drops each invalid
 * one with its reason.
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
import { capText } from './verify-proof.mjs'

export const PROOF_MEDIA_SCHEMA = 'proof-media/1'
export const MAX_CLIPS = 6
export const MAX_STILLS = 12
export const MAX_CHAPTERS = 12
export const MAX_TRANSCRIPTS = 4
export const MAX_DOCUMENTS = 6
export const MAX_DOCUMENT_PAGES = 8
export const MAX_CLIP_MS = 60_000
export const MAX_FILE_BYTES = 15 * 1024 * 1024
export const MAX_TOTAL_BYTES = 60 * 1024 * 1024
export const MAX_PDF_BYTES = 10 * 1024 * 1024
export const MAX_CAPTION_CHARS = 120
export const MAX_TRANSCRIPT_JSON_CHARS = 32 * 1024
/** Bytes as MiB for a reader. @param {number} bytes */
const mib = (bytes) => `${Number((bytes / 1024 / 1024).toFixed(1))} MiB`
export const CLIP_SIDES = Object.freeze(['head', 'base'])
export const VIDEO_TYPES = Object.freeze(['.mp4'])
export const IMAGE_TYPES = Object.freeze(['.png', '.jpg', '.webp'])
export const DOCUMENT_TYPES = Object.freeze(['.pdf'])

/**
 * @typedef {import('./verify-proof-spec.mjs').Dropped} Dropped
 * @typedef {{atMs: number, caption: string}} Chapter
 * @typedef {{file: string, caption: string, atMs?: number}} Still
 * @typedef {{id: string, side: string, pairs?: string, title: string, file: string, poster?: string, durationMs: number, chapters: Chapter[], stills: Still[]}} Clip
 * @typedef {{title: string, note?: string, status: number, ms: number, request: unknown, response: unknown, rows?: string}} Transcript
 * @typedef {{page: number, file: string, caption: string}} DocumentPage `page` as given, or the
 *   page's 1-based position in a list that numbers no page
 * @typedef {{id: string, side: string, pairs?: string, title: string, file: string, pageCount: number, pages: DocumentPage[]}} ProofDocument
 * @typedef {{schema: string, head: string, source?: {kind: string, url: string}, clips: Clip[], transcripts: Transcript[], documents?: ProofDocument[]}} ProofMedia
 * @typedef {{head: string, sizeOf?: (file: string) => number | null}} MediaContext
 */

/** The published name of a media file: the page serves every file as `media/<basename>`.
 * @param {string} file */
export const mediaName = (file) => `media/${file.slice(file.lastIndexOf('/') + 1)}`

/** `m:ss`. @param {number} ms */
const clock = (ms) =>
  `${Math.floor(ms / 60_000)}:${String(Math.floor(ms / 1000) % 60).padStart(2, '0')}`

/** @param {unknown} value @param {string} field */
function needMs(value, field) {
  if (!Number.isSafeInteger(value) || /** @type {number} */ (value) < 0)
    throw new Drop(`${field}: expected milliseconds`)
  return /** @type {number} */ (value)
}

/**
 * An item's `id`, `side` and, for a base item, the head item it `pairs` with. The caller adds the
 * id to `ids` once the whole item is kept, so a dropped item never claims its id.
 * @param {Record<string, unknown>} item @param {Set<string>} ids @param {string} noun
 */
function identity(item, ids, noun) {
  const id = needText(item.id, 40, 'id')
  if (!/^[a-z0-9][a-z0-9-]*$/.test(id)) throw new Drop('id: expected a lower-case identifier')
  if (ids.has(id)) throw new Drop(`id: ${id} appears twice`)
  if (typeof item.side !== 'string' || !CLIP_SIDES.includes(item.side))
    throw new Drop('side: expected head or base')
  if (item.side === 'head' && item.pairs !== undefined)
    throw new Drop(`pairs: only a base ${noun} names the head ${noun} it pairs with`)
  const pairs = item.side === 'base' ? needText(item.pairs, 40, 'pairs') : undefined
  return { id, side: item.side, pairs }
}

/**
 * Parse each entry of `value` with `parse`, keeping at most `cap`: every head before any base, and
 * a base only when it pairs with a kept head no other base pairs with. A base is checked before it
 * is parsed, so one whose head is missing or dropped never counts its files toward the total.
 * Kept items stay in manifest order.
 * @template {{id: string}} T
 * @param {unknown} value @param {string} at @param {number} cap @param {string} noun
 * @param {(entry: unknown, at: string) => T} parse @param {Dropped[]} dropped @returns {T[]}
 */
function keepPairedEach(value, at, cap, noun, parse, dropped) {
  if (value === undefined) return []
  if (!Array.isArray(value)) {
    dropped.push({ at, reason: 'expected a list' })
    return []
  }
  const heads = new Set()
  const paired = new Set()
  /** @type {{i: number, item: T}[]} */
  const kept = []
  for (const bases of [false, true])
    value.forEach((entry, i) => {
      if ((isRecord(entry) && entry.side === 'base') !== bases) return
      const where = `${at}[${i}]`
      if (kept.length >= cap) {
        dropped.push({ at: where, reason: `over the cap of ${cap}` })
        return
      }
      try {
        const pairs = bases ? /** @type {Record<string, unknown>} */ (entry).pairs : undefined
        if (typeof pairs === 'string' && !heads.has(pairs))
          throw new Drop(`pairs: ${pairs} is not a head ${noun}`)
        if (typeof pairs === 'string' && paired.has(pairs))
          throw new Drop(`pairs: ${pairs} already has a base ${noun}`)
        const item = parse(entry, where)
        if (bases) paired.add(pairs)
        else heads.add(item.id)
        kept.push({ i, item })
      } catch (error) {
        if (!(error instanceof Drop)) throw error
        dropped.push({ at: where, reason: error.message })
      }
    })
  return kept.sort((x, y) => x.i - y.i).map(({ item }) => item)
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
    if (!['schema', 'head', 'source', 'clips', 'transcripts', 'documents'].includes(key))
      dropped.push({ at: `media.${key}`, reason: 'unknown key' })
  let total = 0
  /** @type {Map<string, string>} */
  const names = new Map()
  /** A relative file of one of `types`, under `cap` bytes and the total cap, with a basename no
   * other file publishes. @param {unknown} raw @param {readonly string[]} types @param {string} field */
  const file = (raw, types, field, cap = MAX_FILE_BYTES) => {
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
    const type = dot < 0 ? '' : path.slice(dot)
    if (!types.includes(type.toLowerCase()))
      throw new Drop(`${field}: expected one of ${types.join(', ')}`)
    // The upload accepts only lower-case extensions, so one upper-case file would fail them all.
    if (type !== type.toLowerCase())
      throw new Drop(`${field}: ${path} needs a lower-case extension, ${type.toLowerCase()}`)
    const name = mediaName(path)
    if (names.has(name) && names.get(name) !== path)
      throw new Drop(`${field}: ${name} is already published from ${names.get(name)}`)
    if (sizeOf && !names.has(name)) {
      const bytes = sizeOf(path)
      if (bytes === null) throw new Drop(`${field}: ${path} is missing`)
      if (bytes > cap) throw new Drop(`${field}: ${path} is over ${mib(cap)}`)
      if (total + bytes > MAX_TOTAL_BYTES)
        throw new Drop(`${field}: ${path} takes the media over ${mib(MAX_TOTAL_BYTES)}`)
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
  const clips = keepPairedEach(
    value.clips,
    'clips',
    MAX_CLIPS,
    'clip',
    (entry, at) => {
      const c = onlyKeys(entry, [
        'id',
        'side',
        'pairs',
        'title',
        'file',
        'poster',
        'durationMs',
        'chapters',
        'stills',
      ])
      const { id, side, pairs } = identity(c, ids, 'clip')
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
          // A still with no caption keeps its place, named by its clip and time.
          const blank =
            s.caption === undefined || (typeof s.caption === 'string' && !s.caption.trim())
          const caption = blank
            ? capText(
                `${title}${atMs === undefined ? '' : ` at ${clock(atMs)}`}`,
                MAX_CAPTION_CHARS,
              )
            : needText(s.caption, MAX_CAPTION_CHARS, 'caption')
          return compact({ file: file(s.file, IMAGE_TYPES, 'file'), caption, atMs })
        },
        dropped,
      )
      return compact({
        id,
        side,
        pairs,
        title,
        file: video,
        poster,
        durationMs,
        chapters,
        stills,
      })
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
        request: bounded(t.request === undefined ? {} : t.request, 'request'),
        response: bounded(t.response === undefined ? {} : t.response, 'response'),
        rows: optText(t.rows, MAX_TRANSCRIPT_JSON_CHARS, 'rows'),
      })
    },
    dropped,
  )
  const docIds = new Set()
  // Absent stays absent, so a manifest without documents validates to exactly what it did before.
  const documents =
    value.documents === undefined
      ? undefined
      : keepPairedEach(
          value.documents,
          'documents',
          MAX_DOCUMENTS,
          'document',
          (entry, at) => {
            const d = onlyKeys(entry, [
              'id',
              'side',
              'pairs',
              'title',
              'file',
              'pageCount',
              'pages',
            ])
            const { id, side, pairs } = identity(d, docIds, 'document')
            if (!Number.isSafeInteger(d.pageCount) || Number(d.pageCount) < 1)
              throw new Drop('pageCount: expected a whole number of pages')
            const pageCount = Number(d.pageCount)
            const title = needText(d.title, MAX_CAPTION_CHARS, 'title')
            const pdf = file(d.file, DOCUMENT_TYPES, 'file', MAX_PDF_BYTES)
            docIds.add(id)
            // Pages pair on `page`. A list that numbers no page takes each page's position
            // instead, so a page dropped below leaves its place empty rather than shifting the rest.
            const raw = Array.isArray(d.pages) ? d.pages.filter(isRecord) : []
            const numbered = raw.filter((p) => p.page !== undefined).length
            const mixed = numbered > 0 && numbered < raw.length
            if (mixed)
              dropped.push({
                at: `${at}.pages`,
                reason: 'page: either every page names its page number or none does',
              })
            const numbers = new Set()
            const pages = keepEach(
              mixed ? [] : d.pages,
              `${at}.pages`,
              MAX_DOCUMENT_PAGES,
              (page, _, index) => {
                const p = onlyKeys(page, ['page', 'file', 'caption'])
                if (numbered && (!Number.isSafeInteger(p.page) || Number(p.page) < 1))
                  throw new Drop('page: expected a page number')
                const n = numbered ? Number(p.page) : index + 1
                if (n > pageCount) throw new Drop(`page: ${n} is past pageCount ${pageCount}`)
                if (numbers.has(n)) throw new Drop(`page: ${n} appears twice`)
                // A page with no caption keeps its place, named by its document and number.
                const blank =
                  p.caption === undefined || (typeof p.caption === 'string' && !p.caption.trim())
                const caption = blank
                  ? capText(`${title} page ${n}`, MAX_CAPTION_CHARS)
                  : needText(p.caption, MAX_CAPTION_CHARS, 'caption')
                const image = file(p.file, IMAGE_TYPES, 'file')
                numbers.add(n)
                return { page: n, file: image, caption }
              },
              dropped,
            ).sort((x, y) => x.page - y.page)
            return compact({ id, side, pairs, title, file: pdf, pageCount, pages })
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
      documents,
    }),
    dropped,
  }
}
