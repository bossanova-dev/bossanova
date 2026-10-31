// The `proof-publish` extension role: a repo-owned script that stores a verify run's evidence and
// proof page with the repo's own tooling and credentials, then reports the https base URL that
// serves them. The core chooses every object key (`<owner>/<repo>/<pr>/<sha>/<name>`) and composes
// every public URL as `<baseUrl>/<key>`, so a hook and the built-in uploader produce URLs the same
// way and the page and its media share one origin.
import { spawnSync } from 'node:child_process'
import { realpathSync, statSync } from 'node:fs'
import path from 'node:path'
import { isProofUrl } from './verify-proof.mjs'

export const PUBLISH_ROLE = 'proof-publish'
// Matches the daemon's upload ceiling: a hook uploading a long recording gets the same budget.
export const PUBLISH_HOOK_TIMEOUT_MS = 10 * 60 * 1000
export const MAX_PUBLISH_OUTPUT_BYTES = 64 * 1024
// The proof page object; it is uploaded beside the media under the same prefix.
export const PAGE_OBJECT = 'index.html'

const RESULT_KEYS = new Set(['ok', 'extension', 'role', 'baseUrl', 'error'])
const SLUG = /^[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+$/
const SHA = /^[0-9a-f]{40}$/
const CONTENT_TYPES = {
  'video.mp4': 'video/mp4',
  'poster.png': 'image/png',
  'manifest.json': 'application/json',
  'request-log.ndjson': 'application/x-ndjson',
  [PAGE_OBJECT]: 'text/html; charset=utf-8',
}
const SCREENSHOT_TYPES = {
  png: 'image/png',
  jpg: 'image/jpeg',
  jpeg: 'image/jpeg',
  webp: 'image/webp',
}

/** The content type an evidence object is stored with, or null for a name no publisher accepts. */
export function publishContentType(name) {
  if (typeof name !== 'string') return null
  if (Object.hasOwn(CONTENT_TYPES, name)) return CONTENT_TYPES[name]
  const shot = /^screenshots\/[A-Za-z0-9._-]+\.(png|jpg|jpeg|webp)$/.exec(name)
  return shot ? SCREENSHOT_TYPES[shot[1]] : null
}

/**
 * The deterministic key prefix `<owner>/<repo>/<pr>/<sha>/`, or null when any part is malformed:
 * the slug must be exactly `owner/repo` with no dot segment, `pr` a positive integer and `headSha`
 * 40 lowercase hex characters.
 *
 * @param {{repo?: unknown, pr?: unknown, headSha?: unknown}} [target]
 */
export function publishPrefix({ repo, pr, headSha } = {}) {
  if (typeof repo !== 'string' || !SLUG.test(repo)) return null
  if (repo.split('/').some((part) => part === '.' || part === '..')) return null
  if (typeof pr !== 'number' || !Number.isSafeInteger(pr) || pr < 1) return null
  if (typeof headSha !== 'string' || !SHA.test(headSha)) return null
  return `${repo}/${pr}/${headSha}/`
}

/** Parse the marker's required extension-relative `publish` script path; null when unusable. */
export function parsePublishMarker(marker) {
  if (typeof marker?.publish !== 'string') return null
  const raw = marker.publish.trim().replace(/\\/g, '/')
  if (!raw || raw.startsWith('/') || /^[A-Za-z]:/.test(raw)) return null
  const segments = raw.split('/').filter((segment) => segment && segment !== '.')
  if (!segments.length || segments.includes('..')) return null
  return { publish: segments.join('/') }
}

/** Validate a hook result: a closed key set, `ok: true` and an https base URL. */
export function refineProofPublish(result) {
  if (!result || typeof result !== 'object' || Array.isArray(result))
    return ['expected publish result object']
  const errors = []
  // The key name is hook output, so the error never repeats it.
  if (Object.keys(result).some((key) => !RESULT_KEYS.has(key))) errors.push('unknown key')
  if (result.ok !== true) errors.push('ok must be true')
  if (typeof result.extension !== 'string' || result.extension.trim() === '')
    errors.push('extension must be non-empty text')
  if (result.role !== PUBLISH_ROLE) errors.push(`role must be ${PUBLISH_ROLE}`)
  if (!('baseUrl' in result)) errors.push('missing baseUrl')
  else if (!isProofUrl(result.baseUrl) || /[?#]/.test(result.baseUrl))
    errors.push('baseUrl must be an absolute HTTPS URL without userinfo, query or fragment')
  return errors
}

function scriptPath(extension) {
  try {
    const script = realpathSync(path.resolve(extension.dir, extension.publish))
    const relative = path.relative(realpathSync(extension.dir), script)
    if (relative.startsWith('..') || path.isAbsolute(relative) || !statSync(script).isFile())
      return null
    return script
  } catch {
    return null
  }
}

/**
 * Run one discovered hook over `envelope` (`{role, extension, repo, pr, headSha, prefix,
 * evidenceDir, objects}`) from `cwd` with the caller's environment. Never throws, and never puts the
 * hook's stdout or stderr into the outcome: they may carry its credential.
 *
 * @returns {{ok: true, baseUrl: string} | {ok: false, reason: string}}
 */
export function runProofPublish({
  extension,
  envelope,
  cwd,
  env = process.env,
  run = spawnSync,
  timeoutMs = PUBLISH_HOOK_TIMEOUT_MS,
}) {
  /** @param {string} reason @returns {{ok: false, reason: string}} */
  const fail = (reason) => ({ ok: false, reason })
  try {
    const script = scriptPath(extension)
    if (!script) return fail('script-missing')
    const result = run(process.execPath, [script], {
      input: JSON.stringify({ ...envelope, role: PUBLISH_ROLE, extension: extension.name }),
      cwd,
      env,
      timeout: timeoutMs,
      // A hook that traps SIGTERM must not outlive its timeout.
      killSignal: 'SIGKILL',
      maxBuffer: MAX_PUBLISH_OUTPUT_BYTES,
      encoding: 'utf8',
      // stderr is discarded rather than captured, so upload-tool chatter never counts against the
      // stdout bound and never reaches anything the core keeps.
      stdio: ['pipe', 'pipe', 'ignore'],
    })
    const errorCode = result.error && 'code' in result.error ? result.error.code : undefined
    if (errorCode === 'ETIMEDOUT') return fail('timeout')
    if (
      errorCode === 'ENOBUFS' ||
      Buffer.byteLength(result.stdout ?? '') > MAX_PUBLISH_OUTPUT_BYTES
    )
      return fail('oversize')
    if (result.signal) return fail('signal')
    if (result.status !== 0) return fail(`exit:${result.status ?? 1}`)
    let parsed
    try {
      parsed = JSON.parse(String(result.stdout))
    } catch {
      return fail('not-json')
    }
    const errors = refineProofPublish(parsed)
    if (!errors.length && parsed.extension !== extension.name) errors.push('extension mismatch')
    if (errors.length) return fail(`invalid:${errors[0]}`)
    return { ok: true, baseUrl: parsed.baseUrl.replace(/\/+$/, '') }
  } catch (error) {
    const code = /** @type {{code?: string}} */ (error)?.code
    return fail(code === 'ETIMEDOUT' ? 'timeout' : code === 'ENOBUFS' ? 'oversize' : 'exit:1')
  }
}
