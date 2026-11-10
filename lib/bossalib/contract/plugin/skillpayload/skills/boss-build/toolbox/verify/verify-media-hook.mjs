// The `proof-media` extension role: a repo-owned script that gathers what a passing run recorded on
// the judged head (clips, stills, API transcripts; a CI artifact, say) into a directory the core
// names, and prints a `proof-media/1` manifest of it. The core validates the manifest, then
// publishes each file beside the proof page as `media/<basename>`. Like `proof-publish`, the
// script is discovered only on the trusted base tree, so a PR can never supply its own.
import {
  copyFileSync,
  mkdirSync,
  readFileSync,
  realpathSync,
  statSync,
  writeFileSync,
} from 'node:fs'
import path from 'node:path'
import { discoverExtensions } from '../skill-extensions.mjs'
import { PROOF_MEDIA_SCHEMA, mediaName, validateProofMedia } from './verify-proof-media.mjs'
import { PLAYER_OBJECT, runHookScript, scriptPath } from './verify-publish-hook.mjs'

export const MEDIA_ROLE = 'proof-media'
// Long enough to download one CI artifact; the sweep runs it detached, off its post budget.
export const MEDIA_HOOK_TIMEOUT_MS = 5 * 60 * 1000
export const MAX_MEDIA_OUTPUT_BYTES = 256 * 1024
// What `proof-bundle` writes and `post --proof-media <dir>` reads: the manifest beside its files.
export const MEDIA_MANIFEST = 'media.json'
export const MEDIA_DIR = 'media'

/**
 * @typedef {import('./verify-proof-media.mjs').ProofMedia} ProofMedia
 * @typedef {import('./verify-proof-spec.mjs').Dropped} Dropped
 * @typedef {{media: ProofMedia | null, dropped: Dropped[], dir: string, extension?: string, ignored?: string[], reason?: string}} MediaOutcome
 */

/**
 * `sizeOf` for the validator: a file's bytes when it is a regular file whose real path stays inside
 * `dir`, else null, so a symlink in a downloaded artifact can never publish a file from elsewhere.
 * @param {string} dir @returns {(file: string) => number | null}
 */
export function containedSize(dir) {
  return (file) => {
    try {
      const real = realpathSync(path.join(dir, file))
      const relative = path.relative(realpathSync(dir), real)
      if (!relative || relative.startsWith('..') || path.isAbsolute(relative)) return null
      const stat = statSync(real)
      return stat.isFile() ? stat.size : null
    } catch {
      return null
    }
  }
}

/** Validate `value` against `head`, with each file checked under `dir`. @param {unknown} value
 * @param {string} head @param {string} dir */
export const validateMediaIn = (value, head, dir) =>
  validateProofMedia(value, { head, sizeOf: containedSize(dir) })

/**
 * Run the first `proof-media` extension on the trusted base tree, by `order`, into `dir` and
 * validate the manifest it prints. Never throws, and never keeps the script's output beyond the
 * validated manifest. `reason` names why there is no media: `trusted-tree-unavailable`,
 * `not-configured`, `script-missing`, `timeout`, `oversize`, `signal`, `exit:<n>`, `not-json` or
 * `invalid:<why>`.
 * @returns {MediaOutcome}
 */
export function collectProofMedia({
  repo,
  pr,
  headSha,
  baseSha,
  dir,
  cwd,
  read,
  trustedTree,
  discover = discoverExtensions,
  env = process.env,
  run,
  timeoutMs = MEDIA_HOOK_TIMEOUT_MS,
}) {
  /** @param {string} reason @param {Partial<MediaOutcome>} [more] @returns {MediaOutcome} */
  const none = (reason, more = {}) => ({ media: null, dropped: [], dir, reason, ...more })
  try {
    const trusted = trustedTree({ baseSha, cwd, read })
    if (!trusted?.ok) return none('trusted-tree-unavailable')
    const { extensions = [] } = discover({
      core: 'boss-verify',
      role: MEDIA_ROLE,
      root: trusted.root,
    })
    const extension = extensions[0]
    if (!extension) return none('not-configured')
    const named = {
      extension: extension.name,
      ...(extensions.length > 1 ? { ignored: extensions.slice(1).map((e) => e.name) } : {}),
    }
    const script = scriptPath(extension, 'media')
    if (!script) return none('script-missing', named)
    mkdirSync(dir, { recursive: true })
    const envelope = { role: MEDIA_ROLE, extension: extension.name, repo, pr, headSha, baseSha }
    const ran = runHookScript({
      script,
      input: JSON.stringify({ ...envelope, evidenceDir: dir }),
      cwd,
      env,
      ...(run ? { run } : {}),
      timeoutMs,
      maxBytes: MAX_MEDIA_OUTPUT_BYTES,
    })
    if ('reason' in ran) return none(ran.reason, named)
    let parsed
    try {
      parsed = JSON.parse(ran.stdout)
    } catch {
      return none('not-json', named)
    }
    const { media, dropped } = validateMediaIn(parsed, headSha, dir)
    if (!media) return none(`invalid:${dropped[0]?.reason ?? 'not a manifest'}`, named)
    return { media, dropped, dir, ...named }
  } catch {
    return none('media-failed')
  }
}

/** Every file a manifest names, once each, in manifest order. @param {ProofMedia} media */
export function mediaFiles(media) {
  const files = media.clips.flatMap((clip) => [
    clip.file,
    ...(clip.poster ? [clip.poster] : []),
    ...clip.stills.map((still) => still.file),
  ])
  return [...new Set(files)]
}

/**
 * The objects the page needs beside `index.html`, in upload order: each media file as
 * `media/<basename>`, then the player when a head clip exists for it to play.
 * @param {ProofMedia | null} media @param {string} dir @param {string} player
 * @returns {{name: string, from?: string, text?: string}[]}
 */
export function pageAssets(media, dir, player) {
  if (!media) return []
  return [
    ...mediaFiles(media).map((file) => ({ name: mediaName(file), from: path.join(dir, file) })),
    ...(media.clips.some((clip) => clip.side === 'head')
      ? [{ name: PLAYER_OBJECT, text: player }]
      : []),
  ]
}

/**
 * Write `outcome` under `out` as `proof-bundle` hands it on: the validated manifest as
 * `media.json` with each path under `media/`, which the files already are. Returns the manifest
 * written, or null when there is none.
 * @param {MediaOutcome} outcome @param {string} out
 */
export function writeMediaManifest(outcome, out) {
  if (!outcome.media) return null
  const under = (/** @type {string} */ file) => `${MEDIA_DIR}/${file}`
  const manifest = {
    ...outcome.media,
    clips: outcome.media.clips.map((clip) => ({
      ...clip,
      file: under(clip.file),
      ...(clip.poster ? { poster: under(clip.poster) } : {}),
      stills: clip.stills.map((still) => ({ ...still, file: under(still.file) })),
    })),
  }
  writeFileSync(path.join(out, MEDIA_MANIFEST), `${JSON.stringify(manifest, null, 2)}\n`)
  return manifest
}

/**
 * The media `proof-bundle` left in `dir`, revalidated against `head`: `{media, dropped, dir}`,
 * with `media` null when `dir` holds no valid manifest. Never throws.
 * @param {string} dir @param {string} head @returns {MediaOutcome}
 */
export function readMediaDir(dir, head) {
  try {
    const raw = JSON.parse(readFileSync(path.join(dir, MEDIA_MANIFEST), 'utf8'))
    const { media, dropped } = validateMediaIn(raw, head, dir)
    return media
      ? { media, dropped, dir }
      : { media: null, dropped, dir, reason: `invalid:${dropped[0]?.reason}` }
  } catch {
    return { media: null, dropped: [], dir, reason: 'no-manifest' }
  }
}

/** Copy `assets` into `dir` under their published names. @param {{name: string, from?: string,
 * text?: string}[]} assets @param {string} dir */
export function stageAssets(assets, dir) {
  for (const asset of assets) {
    const to = path.join(dir, asset.name)
    mkdirSync(path.dirname(to), { recursive: true })
    if (asset.from) copyFileSync(realpathSync(asset.from), to)
    else writeFileSync(to, asset.text ?? '')
  }
}

/** The bundle's summary of `outcome`: titles, chapters and captions, never a file or binary.
 * @param {MediaOutcome} outcome */
export function mediaSummary(outcome) {
  const media = outcome.media
  return {
    schema: PROOF_MEDIA_SCHEMA,
    ...(outcome.extension ? { extension: outcome.extension } : {}),
    ...(outcome.reason ? { reason: outcome.reason } : {}),
    dropped: outcome.dropped,
    clips: (media?.clips ?? []).map((clip) => ({
      id: clip.id,
      side: clip.side,
      ...(clip.pairs ? { pairs: clip.pairs } : {}),
      title: clip.title,
      durationMs: clip.durationMs,
      chapters: clip.chapters,
      stills: clip.stills.map((still) => ({
        caption: still.caption,
        ...(still.atMs === undefined ? {} : { atMs: still.atMs }),
      })),
    })),
    transcripts: (media?.transcripts ?? []).map((t) => ({
      title: t.title,
      status: t.status,
      ...(t.note ? { note: t.note } : {}),
    })),
  }
}
