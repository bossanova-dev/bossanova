// Fitting a proof page to the hosted page limits. The limits reach the skill only through the
// daemon's over-limit refusal (`{kind, max}`), never from a constant here. After a refusal the page
// drops whole media units, largest first, until every limit the host has reported holds, is
// re-rendered with the number of files left out, re-scanned, and uploaded again. The loop retries
// once per new limit kind and every fit removes at least one unit, so it needs no attempt cap.
import { failureDetail } from './verify-failure-detail.mjs'
import { mediaFiles } from './verify-media-hook.mjs'
import { validateProofMedia } from './verify-proof-media.mjs'

/** The page-shape limits a hosted refusal can name. */
export const PAGE_LIMIT_KINDS = Object.freeze(['file-count', 'file-bytes', 'page-bytes'])

/**
 * @typedef {import('./verify-proof-media.mjs').ProofMedia} ProofMedia
 * @typedef {{kind: string, max: number}} PageLimit
 * @typedef {{count: number, bytes: number, largest: number}} PageSize
 * @typedef {{key: string, at: number, files: string[], bytes: number}} MediaUnit
 * @typedef {(file: string) => number | null} SizeOf
 */

/** @param {SizeOf} sizeOf @param {string} file */
const bytesOf = (sizeOf, file) => sizeOf(file) ?? 0

/**
 * The head clip whose stills pair by index with `clip`'s: its own id, or a base clip's pair.
 * @param {{id: string, side: string, pairs?: string}} clip
 */
const stillGroup = (clip) => (clip.side === 'base' && clip.pairs ? clip.pairs : clip.id)

/**
 * One unit per piece the fit can drop, in manifest order: each clip (its video and poster; its
 * stills go with it), each still index of a head clip and its base pair (both sides' stills at
 * that index, so the page's Before/After pairs stay aligned), and each document (its PDF and page
 * images). Transcripts carry no files and are never units.
 * @param {ProofMedia} media @param {SizeOf} sizeOf @returns {MediaUnit[]}
 */
export function mediaUnits(media, sizeOf) {
  /** @type {Omit<MediaUnit, 'at' | 'bytes'>[]} */
  const units = []
  const grouped = new Set()
  for (const clip of media.clips) {
    units.push({
      key: `clip:${clip.id}`,
      files: [clip.file, ...(clip.poster ? [clip.poster] : [])],
    })
    const group = stillGroup(clip)
    if (grouped.has(group)) continue
    grouped.add(group)
    const sides = media.clips.filter((c) => stillGroup(c) === group)
    for (let j = 0; j < Math.max(...sides.map((c) => c.stills.length)); j++)
      units.push({
        key: `still:${group}:${j}`,
        files: sides.flatMap((c) => (c.stills[j] ? [c.stills[j].file] : [])),
      })
  }
  for (const doc of media.documents ?? [])
    units.push({ key: `doc:${doc.id}`, files: [doc.file, ...doc.pages.map((page) => page.file)] })
  return units.map((unit, at) => {
    const files = [...new Set(unit.files)]
    return { ...unit, files, at, bytes: files.reduce((n, file) => n + bytesOf(sizeOf, file), 0) }
  })
}

/**
 * What the page sends: `fixed` (index.html and every non-media asset) plus each media file once.
 * @param {ProofMedia | null} media @param {SizeOf} sizeOf @param {PageSize} fixed @returns {PageSize}
 */
export function measure(media, sizeOf, fixed) {
  const sizes = (media ? mediaFiles(media) : []).map((file) => bytesOf(sizeOf, file))
  return {
    count: fixed.count + sizes.length,
    bytes: sizes.reduce((n, size) => n + size, fixed.bytes),
    largest: Math.max(fixed.largest, ...sizes),
  }
}

/**
 * The size of `html` as `index.html` plus the non-media `assets` (`[{text}]`, today the player).
 * @param {string} html @param {{text?: string}[]} assets @returns {PageSize}
 */
export function fixedSize(html, assets) {
  const sizes = [html, ...assets.map((asset) => asset.text ?? '')].map((t) => Buffer.byteLength(t))
  return {
    count: sizes.length,
    bytes: sizes.reduce((n, size) => n + size, 0),
    largest: Math.max(...sizes),
  }
}

/** Whether `size` meets every one of `limits`. @param {PageSize} size @param {PageLimit[]} limits */
export const withinLimits = (size, limits) =>
  limits.every(({ kind, max }) =>
    kind === 'file-count'
      ? size.count <= max
      : kind === 'page-bytes'
        ? size.bytes <= max
        : kind === 'file-bytes'
          ? size.largest <= max
          : false,
  )

/** `media` without `unit`. @param {ProofMedia} media @param {MediaUnit} unit @returns {ProofMedia} */
function withoutUnit(media, unit) {
  return {
    ...media,
    clips: media.clips
      .filter((clip) => unit.key !== `clip:${clip.id}`)
      .map((clip) => ({
        ...clip,
        stills: clip.stills.filter((_, j) => unit.key !== `still:${stillGroup(clip)}:${j}`),
      })),
    ...(media.documents
      ? { documents: media.documents.filter((doc) => unit.key !== `doc:${doc.id}`) }
      : {}),
  }
}

/**
 * The next unit to drop: one holding a file over the smallest `file-bytes` maximum first, else
 * the largest, ties to the later unit in the manifest.
 * @param {MediaUnit[]} units @param {PageLimit[]} limits @param {SizeOf} sizeOf
 */
function nextUnit(units, limits, sizeOf) {
  const caps = limits.filter((l) => l.kind === 'file-bytes').map((l) => l.max)
  const cap = caps.length ? Math.min(...caps) : Infinity
  const over = units.filter((unit) => unit.files.some((file) => bytesOf(sizeOf, file) > cap))
  return (over.length ? over : units).reduce((pick, unit) =>
    unit.bytes > pick.bytes || (unit.bytes === pick.bytes && unit.at > pick.at) ? unit : pick,
  )
}

/**
 * Drop whole units of `media` until the page meets every one of `limits`, revalidating after each
 * drop so a base clip or document whose head pair went is dropped too. `omitted` counts the
 * original files missing from the result; `media` is null when no clip, document or transcript is
 * left. `{ok: false}` when nothing is left to drop and a limit still fails. Pure.
 * @param {{media: ProofMedia, sizeOf: SizeOf, head: string, fixed: PageSize, limits: PageLimit[]}} input
 * @returns {{ok: true, media: ProofMedia | null, omitted: number} | {ok: false}}
 */
export function fitProofMedia({ media, sizeOf, head, fixed, limits }) {
  /** @type {ProofMedia | null} */
  let current = media
  while (!withinLimits(measure(current, sizeOf, fixed), limits)) {
    const units = current ? mediaUnits(current, sizeOf) : []
    if (!units.length) return { ok: false }
    current = validateProofMedia(withoutUnit(current, nextUnit(units, limits, sizeOf)), {
      head,
      sizeOf,
    }).media
  }
  const kept = new Set(current ? mediaFiles(current) : [])
  const omitted = mediaFiles(media).filter((file) => !kept.has(file)).length
  const empty =
    !current || (!current.clips.length && !current.transcripts.length && !current.documents?.length)
  return { ok: true, media: empty ? null : current, omitted }
}

/** The component-wise larger of two sizes. @param {PageSize} a @param {PageSize} b */
const larger = (a, b) => ({
  count: Math.max(a.count, b.count),
  bytes: Math.max(a.bytes, b.bytes),
  largest: Math.max(a.largest, b.largest),
})

/**
 * The page's extras for a fitted render: `omittedMedia` set, and `media` replaced, or removed with
 * `mediaSize` when the fit left none, so the page never reports a missing manifest.
 * @template {{media?: unknown, mediaSize?: unknown}} E
 * @param {E} extras @param {ProofMedia | null} media @param {number} omitted
 */
export function fittedExtras(extras, media, omitted) {
  const { media: _media, mediaSize: _size, ...rest } = extras
  return media ? { ...extras, media, omittedMedia: omitted } : { ...rest, omittedMedia: omitted }
}

/**
 * @typedef {{ok: boolean, reason?: string, detail?: string, limit?: PageLimit, url?: string}} UploadResult
 * @typedef {{
 *   upload: (html: string, media: ProofMedia | null) => Promise<UploadResult>,
 *   render: (media: ProofMedia | null, omitted: number) => string,
 *   scan: (html: string) => boolean,
 *   fixedFor: (html: string) => PageSize,
 *   html: string, media: ProofMedia, sizeOf: SizeOf, head: string,
 * }} FitInput
 */

/**
 * Upload `html` with `media`; on an over-limit refusal that names its limit, fit, render, scan
 * and upload again, once per new limit kind. Returns `{uploaded, html, omitted}`: the last upload
 * result (or the loop's own `{ok: false, reason}`), the html it carried, and how many media files
 * it left out. Stops with `over-limit` when nothing is left to drop or the host repeats a limit
 * the page already meets, and with `page-contains-secret`, uploading nothing, when the re-rendered
 * page fails the scan. Any other refusal is returned unchanged. Never throws.
 * @param {FitInput} input
 * @returns {Promise<{uploaded: UploadResult, html: string, omitted: number}>}
 */
export async function publishWithinLimits({
  upload,
  render,
  scan,
  html,
  media,
  sizeOf,
  head,
  fixedFor,
}) {
  let sent = { html, omitted: 0 }
  try {
    let uploaded = await upload(html, media)
    /** @type {PageLimit[]} */
    const limits = []
    while (uploaded.ok === false && uploaded.reason === 'over-limit' && uploaded.limit) {
      const limit = uploaded.limit
      const stop = { uploaded: { ok: false, reason: 'over-limit' }, ...sent }
      if (limits.some((seen) => seen.kind === limit.kind)) return stop
      limits.push(limit)
      // Fixed only grows, so each pass drops at least one more unit and the step ends.
      let fixed = fixedFor(sent.html)
      let page = null
      let fit = null
      while (page === null) {
        const next = fitProofMedia({ media, sizeOf, head, fixed, limits })
        if (!next.ok) return stop
        const rendered = render(next.media, next.omitted)
        const size = fixedFor(rendered)
        if (withinLimits(measure(next.media, sizeOf, size), limits)) {
          page = rendered
          fit = next
        } else fixed = larger(fixed, size)
      }
      if (scan(page)) return { uploaded: { ok: false, reason: 'page-contains-secret' }, ...sent }
      uploaded = await upload(page, fit.media)
      sent = { html: page, omitted: fit.omitted }
    }
    return { uploaded, ...sent }
  } catch (error) {
    return {
      uploaded: { ok: false, reason: 'upload-failed', detail: failureDetail(error) },
      ...sent,
    }
  }
}
