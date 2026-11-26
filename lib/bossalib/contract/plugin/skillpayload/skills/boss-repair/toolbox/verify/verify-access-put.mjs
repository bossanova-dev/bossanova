// Publish proof objects to an Access-gated host using credentials owned by the verify host.
import { spawnSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { failureDetail } from './verify-failure-detail.mjs'
import { isProofUrl } from './verify-proof.mjs'
import { publishContentType } from './verify-publish-hook.mjs'

const CLIENT_ID = 'PROOF_ACCESS_CLIENT_ID'
const CLIENT_SECRET = 'PROOF_ACCESS_CLIENT_SECRET'

/** @typedef {{clientId: string, clientSecret: string}} AccessCredentials */
/** @typedef {{service: string, account: string}} KeychainAccount */

/** @param {KeychainAccount} input @returns {string | null} */
function readAccessKeychain({ service, account }) {
  try {
    const result = spawnSync(
      '/usr/bin/security',
      ['find-generic-password', '-s', service, '-a', account, '-w'],
      {
        encoding: 'utf8',
        timeout: 5000,
        killSignal: 'SIGKILL',
        maxBuffer: 16 * 1024,
        stdio: ['ignore', 'pipe', 'ignore'],
      },
    )
    return result.status === 0 ? result.stdout.trim() : null
  } catch {
    return null
  }
}

/**
 * Resolve each Access credential from the environment, then the macOS login keychain.
 * Missing results name only the environment variable, never a value or keychain error.
 * @param {{env?: NodeJS.ProcessEnv, keychainService?: string, platform?: string,
 * readKeychain?: (input: KeychainAccount) => string | null | undefined}} [options]
 * @returns {AccessCredentials | {ok: false, reason: 'credentials-missing', missing: string[]}}
 */
export function resolveAccessCredentials({
  env = process.env,
  keychainService = 'boss-proof-publish',
  platform = process.platform,
  readKeychain = readAccessKeychain,
} = {}) {
  const values = [CLIENT_ID, CLIENT_SECRET].map((account) => {
    if (env[account]?.trim()) return env[account].trim()
    if (platform !== 'darwin') return ''
    try {
      return readKeychain({ service: keychainService, account })?.trim() ?? ''
    } catch {
      return ''
    }
  })
  const missing = [CLIENT_ID, CLIENT_SECRET].filter((_, index) => !values[index])
  return missing.length
    ? { ok: false, reason: 'credentials-missing', missing }
    : { clientId: values[0], clientSecret: values[1] }
}

const UNREACHABLE_CODES = new Set([
  'ENOTFOUND',
  'EAI_AGAIN',
  'ECONNREFUSED',
  'EHOSTUNREACH',
  'ENETUNREACH',
  'ETIMEDOUT',
  'UND_ERR_CONNECT_TIMEOUT',
  'UNABLE_TO_VERIFY_LEAF_SIGNATURE',
  'UNABLE_TO_GET_ISSUER_CERT_LOCALLY',
  'DEPTH_ZERO_SELF_SIGNED_CERT',
  'SELF_SIGNED_CERT_IN_CHAIN',
])
const TLS_CODE = /^(ERR_TLS_|ERR_SSL_|CERT_)/

/** Whether a thrown fetch error says the host was never reached. fetch keeps the errno on its
 * cause. @param {unknown} error */
export function isUnreachableError(error) {
  const { code, cause } = /** @type {{code?: unknown, cause?: {code?: unknown}}} */ (error ?? {})
  const found = typeof cause?.code === 'string' ? cause.code : code
  return typeof found === 'string' && (UNREACHABLE_CODES.has(found) || TLS_CODE.test(found))
}

/** @param {unknown} baseUrl @returns {baseUrl is string} */
export const isAccessPublishUrl = (baseUrl) =>
  typeof baseUrl === 'string' && isProofUrl(baseUrl) && !/[?#]/.test(baseUrl)

/**
 * PUT one batch sequentially under its shared deadline. Refuse redirects so Access credentials
 * cannot follow a response to another host. A DNS, connect or TLS failure before any response is
 * `host-unreachable`. Response bodies and errors never enter outcomes.
 * @param {{baseUrl: string, prefix: string, dir: string, names: string[],
 * credentials: AccessCredentials, fetchImpl?: typeof fetch, timeoutMs?: number}} options
 * @returns {Promise<{ok: true, base: string} | {ok: false, reason: string, detail?: string}>}
 */
export async function putObjects({
  baseUrl,
  prefix,
  dir,
  names,
  credentials,
  fetchImpl = fetch,
  timeoutMs = 120000,
}) {
  if (!isAccessPublishUrl(baseUrl)) return { ok: false, reason: 'invalid-publish-config' }
  const base = baseUrl.replace(/\/+$/, '')
  let answered = false
  try {
    const signal = AbortSignal.timeout(timeoutMs)
    for (const name of names) {
      const contentType = publishContentType(name)
      if (!contentType) return { ok: false, reason: 'upload-failed' }
      const response = await fetchImpl(`${base}/_publish/${prefix}${name}`, {
        method: 'PUT',
        redirect: 'manual',
        signal,
        headers: {
          'Content-Type': contentType,
          'CF-Access-Client-Id': credentials.clientId,
          'CF-Access-Client-Secret': credentials.clientSecret,
        },
        body: readFileSync(join(dir, name)),
      })
      answered = true
      // Do not retain a response payload or leave its stream holding a socket open.
      try {
        await response.body?.cancel()
      } catch {
        /* A refused response still reports its status when the peer closes the stream. */
      }
      if (response.status === 401 || response.status === 403)
        return { ok: false, reason: 'upload-refused' }
      if (response.status < 200 || response.status >= 300)
        return { ok: false, reason: 'upload-failed', detail: `HTTP ${response.status}` }
    }
    return { ok: true, base }
  } catch (error) {
    if (/** @type {{name?: string}} */ (error)?.name === 'TimeoutError')
      return { ok: false, reason: 'timeout' }
    const secrets = {
      ACCESS_CLIENT_SECRET: credentials.clientSecret,
      ACCESS_CLIENT_ID: credentials.clientId,
    }
    const reason = !answered && isUnreachableError(error) ? 'host-unreachable' : 'upload-failed'
    return { ok: false, reason, detail: failureDetail(error, secrets) }
  }
}
