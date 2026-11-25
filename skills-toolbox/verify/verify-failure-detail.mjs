// A short, secret-safe, one-line account of why a proof part failed, for verify results and notes.
import os from 'node:os'
import { scanText } from '../plan/plan-secret-scan.mjs'

export const FAILURE_DETAIL_MAX = 160

// An environment value shorter than this is too common to redact by exact match.
const MIN_SECRET_LENGTH = 8
const SECRET_NAME =
  /(TOKEN|SECRET|PASSWORD|PASSWD|API_?KEY|ACCESS_KEY|PRIVATE_KEY|CREDENTIAL|_JWT$|_DSN$)/i
const URL_SHAPE = /\b[a-z][a-z0-9+.-]{1,30}:\/\/[^\s'"<>]{1,2048}/gi
// A long opaque run with letters and digits, unless it is a commit SHA.
const OPAQUE = /[A-Za-z0-9+=]{32,}/g
const SHA = /^[0-9a-f]{40}$/

/** A URL cut to its scheme and host: no user info, path, query or fragment. @param {string} url */
function bareUrl(url) {
  try {
    const parsed = new URL(url)
    return `${parsed.protocol}//${parsed.host}${parsed.pathname === '/' ? '' : '/…'}`
  } catch {
    return '[url]'
  }
}

/**
 * `text` as one line of at most FAILURE_DETAIL_MAX characters: URLs cut to their host, opaque
 * runs, credential shapes and credential-named `env` values redacted, the home directory as `~`,
 * and no backtick. Empty when nothing describable is left.
 * @param {unknown} text @param {Record<string, string | undefined>} [env]
 */
export function safeDetail(text, env = {}) {
  if (typeof text !== 'string') return ''
  let line = text.replace(/[\s\u0000-\u001f\u007f]+/g, ' ').trim()
  for (const [key, value] of Object.entries(env ?? {}))
    if (SECRET_NAME.test(key) && typeof value === 'string' && value.length >= MIN_SECRET_LENGTH)
      line = line.split(value).join('[redacted]')
  line = line
    .replace(URL_SHAPE, bareUrl)
    .replace(OPAQUE, (run) =>
      SHA.test(run) || !/[0-9]/.test(run) || !/[A-Za-z]/.test(run) ? run : '[redacted]',
    )
    .replaceAll('`', "'")
  const home = os.homedir()
  if (home.length > 1) line = line.split(home).join('~')
  // A shape the redactions above missed drops everything but the leading code or class.
  if (scanText(line).length) line = line.split(/[:\s]/)[0] || ''
  if (scanText(line).length) return ''
  return line.length > FAILURE_DETAIL_MAX ? `${line.slice(0, FAILURE_DETAIL_MAX - 1)}…` : line
}

/**
 * The detail for a thrown value: its code (a Node errno or a Connect code) or class, then its
 * message, made safe by safeDetail.
 * @param {unknown} error @param {Record<string, string | undefined>} [env]
 */
export function failureDetail(error, env = {}) {
  if (error === null || error === undefined || typeof error !== 'object')
    return safeDetail(String(error), env)
  const { code, message, name, cause } =
    /** @type {{code?: unknown, message?: unknown, name?: unknown, cause?: {code?: unknown}}} */ (
      error
    )
  const label =
    typeof code === 'string' || typeof code === 'number'
      ? String(code)
      : typeof name === 'string'
        ? name
        : 'Error'
  // fetch reports `fetch failed` and keeps the errno (ECONNREFUSED, ENOTFOUND) on its cause.
  const because = typeof cause?.code === 'string' ? ` (${cause.code})` : ''
  const said = typeof message === 'string' ? `${message.trim()}${because}` : ''
  if (!said) return safeDetail(label, env)
  return safeDetail(said.startsWith(label) ? said : `${label}: ${said}`, env)
}
