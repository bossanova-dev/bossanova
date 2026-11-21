// Who publishes a proof page, and how a page goes through the daemon's UploadEvidence.
//
// Publisher, first that applies, read only from the trusted base tree: its `proof-publish` hook;
// no page when its config is unreadable or sets `publishConfig.proof: false`; its configured
// Access PUT host; the daemon (its own bucket, else the daemon's hosted proof host) when it sets
// a `publishConfig.proof` block; else the daemon's hosted proof host alone.
//
// Both daemon publishers send the whole page in one call when it fits, `index.html` last, because
// the hosted host takes the whole manifest before any byte. A response `pageUrl` is the page.
import { loadSkillConfig, publishConfigFor } from '../skill-config.mjs'
import { discoverExtensions } from '../skill-extensions.mjs'
import { isAccessPublishUrl } from './verify-access-put.mjs'
import { daemonSocket } from './verify-daemon-rpc.mjs'
import { PAGE_LIMIT_KINDS } from './verify-proof-fit.mjs'
import { PAGE_OBJECT, PUBLISH_HOOK_TIMEOUT_MS, PUBLISH_ROLE } from './verify-publish-hook.mjs'

// The daemon takes at most this many objects per call.
export const MAX_OBJECTS_PER_CALL = 64

/** The UploadEvidence destination each daemon publisher sends. */
export const DAEMON_DESTINATIONS = Object.freeze({
  daemon: 'EVIDENCE_DESTINATION_LOCAL_OR_HOSTED',
  hosted: 'EVIDENCE_DESTINATION_HOSTED',
})

/**
 * The publisher for a page on `baseSha` (see the module comment), as `{publisher}` or
 * `{publisher: null, reason}`. Only the base tree is read, so a PR can neither supply a
 * publisher nor switch uploads on or off. Never throws.
 */
export function discoverProofPublisher({
  baseSha,
  cwd,
  read,
  trustedTree,
  discover = discoverExtensions,
  loadConfig = (root) => loadSkillConfig({ cwd: root }),
}) {
  try {
    const trusted = trustedTree({ baseSha, cwd, read })
    if (!trusted?.ok) return { publisher: null, reason: 'trusted-tree-unavailable' }
    const readConfig = () => {
      try {
        return loadConfig(trusted.root) ?? {}
      } catch {
        return null
      }
    }
    let extensions
    try {
      ;({ extensions } = discover({ core: 'boss-verify', role: PUBLISH_ROLE, root: trusted.root }))
    } catch {
      // Discovery reads the same config, so name it when it is what failed.
      return {
        publisher: null,
        reason: readConfig() ? 'publisher-discovery-failed' : 'config-unreadable',
      }
    }
    if (extensions?.[0]) return { publisher: { kind: 'hook', extension: extensions[0] } }
    // A broken opt-out must never become a hosted upload.
    const config = readConfig()
    if (!config) return { publisher: null, reason: 'config-unreadable' }
    // publishConfigFor maps a non-object block to null, so `false` is read raw.
    if (config.publishConfig?.proof === false) return { publisher: null, reason: 'opted-out' }
    const proof = /** @type {{kind?: string, baseUrl: string, keychainService?: string} | null} */ (
      publishConfigFor(config, 'proof')
    )
    if (proof?.kind === 'access-put') {
      if (!isAccessPublishUrl(proof.baseUrl))
        return { publisher: null, reason: 'invalid-publish-config' }
      return {
        publisher: {
          kind: 'access-put',
          baseUrl: proof.baseUrl,
          keychainService: proof.keychainService,
        },
      }
    }
    return { publisher: { kind: proof ? 'daemon' : 'hosted' } }
  } catch {
    return { publisher: null, reason: 'publisher-discovery-failed' }
  }
}

/** Whether `publisher` uploads through the daemon, from the workspace evidence directory. */
export const isDaemonPublisher = (/** @type {{kind?: string} | null | undefined} */ publisher) =>
  publisher?.kind === 'daemon' || publisher?.kind === 'hosted'

/**
 * The upload calls for a page whose assets are `names`, in order, `index.html` always in the last.
 * A daemon publisher sends everything in one call when it fits; otherwise assets go in calls of at
 * most MAX_OBJECTS_PER_CALL and the page alone follows.
 * @param {{kind?: string}} publisher @param {string[]} names @returns {string[][]}
 */
export function planUploadCalls(publisher, names) {
  if (isDaemonPublisher(publisher) && names.length < MAX_OBJECTS_PER_CALL)
    return [[...names, PAGE_OBJECT]]
  const calls = []
  for (let i = 0; i < names.length; i += MAX_OBJECTS_PER_CALL)
    calls.push(names.slice(i, i + MAX_OBJECTS_PER_CALL))
  calls.push([PAGE_OBJECT])
  return calls
}

/**
 * A call carrying anything but the page gets the media budget; the page alone gets `pageTimeoutMs`.
 * @param {string[]} names @param {number} pageTimeoutMs
 */
export const callTimeoutMs = (names, pageTimeoutMs) =>
  names.some((name) => name !== PAGE_OBJECT) ? PUBLISH_HOOK_TIMEOUT_MS : pageTimeoutMs

const DAEMON_REASONS = {
  unimplemented: 'daemon-unsupported',
  failed_precondition: 'page-refused',
  permission_denied: 'upload-refused',
  invalid_argument: 'upload-refused',
  unavailable: 'upload-failed',
  resource_exhausted: 'over-limit',
}
// The hosted refusals each publisher records verbatim. The daemon publisher's own
// `not-configured` (no bucket) stays `page-refused`.
const HOSTED_REASONS = {
  hosted: ['not-configured', 'quota-exhausted', 'over-limit'],
  daemon: ['quota-exhausted', 'over-limit'],
}

/**
 * The page reason for an UploadEvidence refusal through `publisher`.
 * @param {{kind?: string}} publisher @param {unknown} error
 */
export function daemonRefusalReason(publisher, error) {
  const { code, reason } = /** @type {{code?: string, reason?: string}} */ (error ?? {})
  const passed = HOSTED_REASONS[/** @type {keyof typeof HOSTED_REASONS} */ (publisher?.kind)]
  if (reason && passed?.includes(reason)) return reason
  return DAEMON_REASONS[/** @type {keyof typeof DAEMON_REASONS} */ (code)] ?? 'daemon-unavailable'
}

/**
 * The refusal result for an UploadEvidence error through `publisher`. A ResourceExhausted
 * `over-limit` refusal also carries the hosted page limit it broke, `limit: {kind, max}`, from the
 * daemon's limit headers; with either header missing or malformed it carries none, so nothing is
 * fitted to a guessed limit.
 * @param {{kind?: string}} publisher @param {unknown} error
 * @returns {{ok: false, reason: string, limit?: {kind: string, max: number}}}
 */
export function daemonRefusal(publisher, error) {
  const reason = daemonRefusalReason(publisher, error)
  const { code, limit, limitMax } =
    /** @type {{code?: string, limit?: unknown, limitMax?: unknown}} */ (error ?? {})
  const header = /** @type {{reason?: string}} */ (error ?? {}).reason
  const max = typeof limitMax === 'string' && /^[1-9][0-9]*$/.test(limitMax) ? Number(limitMax) : 0
  if (
    reason === 'over-limit' &&
    code === 'resource_exhausted' &&
    header === 'over-limit' &&
    typeof limit === 'string' &&
    PAGE_LIMIT_KINDS.includes(limit) &&
    Number.isSafeInteger(max) &&
    max > 0
  )
    return { ok: false, reason, limit: { kind: limit, max } }
  return { ok: false, reason }
}

/**
 * Send `names` from `dir` to UploadEvidence for `target`: `{ok: true, url}` when the daemon returns
 * the page URL, `{ok: true, base}` for a local bucket keyed by this PR head, or `{ok: false, reason}`.
 * @param {{publisher: {kind?: string}, names: string[], dir: string | null,
 *   target: {pr: number, headSha: string}, env: Record<string, string | undefined>,
 *   callDaemon: Function, timeoutMs: number}} input
 * @returns {Promise<{ok: true, url: string} | {ok: true, base: string} | {ok: false, reason: string, limit?: {kind: string, max: number}}>}
 */
export async function sendDaemonCall({
  publisher,
  names,
  dir,
  target,
  env,
  callDaemon,
  timeoutMs,
}) {
  let result
  try {
    result = await callDaemon(
      daemonSocket(env),
      'UploadEvidence',
      {
        evidenceDir: dir,
        objects: names.map((name) => ({ name, path: name })),
        prNumber: target.pr,
        headSha: target.headSha,
        destination: DAEMON_DESTINATIONS[/** @type {'daemon' | 'hosted'} */ (publisher.kind)],
      },
      { timeoutMs },
    )
  } catch (error) {
    return daemonRefusal(publisher, error)
  }
  const pageUrl = typeof result?.pageUrl === 'string' ? result.pageUrl : ''
  // A daemon that predates destination ignores it and answers as a local upload.
  if (publisher.kind === 'hosted')
    return result?.destination === DAEMON_DESTINATIONS.hosted && pageUrl
      ? { ok: true, url: pageUrl }
      : { ok: false, reason: 'daemon-unsupported' }
  if (pageUrl) return { ok: true, url: pageUrl }
  // The daemon derives the slug from the workspace's origin; the page is keyed by this PR head.
  const keyed = new RegExp(`^[^/]+/[^/]+/${target.pr}/${target.headSha}/$`)
  if (!keyed.test(String(result?.keyPrefix ?? '')))
    return { ok: false, reason: 'page-url-mismatch' }
  return { ok: true, base: String(result.publicBaseUrl ?? '').replace(/\/+$/, '') }
}
