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
import { failureDetail, safeDetail } from './verify-failure-detail.mjs'
import { PAGE_LIMIT_KINDS } from './verify-proof-fit.mjs'
import { PAGE_OBJECT, PUBLISH_HOOK_TIMEOUT_MS, PUBLISH_ROLE } from './verify-publish-hook.mjs'

// The daemon takes at most this many objects per call.
export const MAX_OBJECTS_PER_CALL = 64

/** The UploadEvidence destination each daemon publisher sends. */
export const DAEMON_DESTINATIONS = Object.freeze({
  daemon: 'EVIDENCE_DESTINATION_LOCAL_OR_HOSTED',
  hosted: 'EVIDENCE_DESTINATION_HOSTED',
})

export const SETUP_FAILURE_REASONS = Object.freeze([
  'credentials-missing',
  'not-configured',
  'host-unreachable',
])

/** Whether `reason` is a setup failure. @param {unknown} reason */
export const isSetupFailure = (reason) =>
  SETUP_FAILURE_REASONS.includes(/** @type {string} */ (reason))

const HOOK_SETUP_FAILURES = {
  'hook-failed:script-missing': 'not-configured',
  'hook-failed:not-configured': 'not-configured',
  'hook-failed:credentials-missing': 'credentials-missing',
}

/**
 * The setup failure an uploadProofPage refusal through `publisher` reports, as
 * `{kind, reason, detail?}`, else null. Only a hook or Access PUT publisher has one.
 * @param {{kind?: string} | null | undefined} publisher
 * @param {{ok?: boolean, reason?: string, detail?: string} | null | undefined} failed
 * @returns {{kind: string, reason: string, detail?: string} | null}
 */
export function setupFailure(publisher, failed) {
  if (failed?.ok !== false) return null
  const kind = publisher?.kind
  const reason =
    kind === 'hook'
      ? HOOK_SETUP_FAILURES[/** @type {keyof typeof HOOK_SETUP_FAILURES} */ (failed.reason)]
      : kind === 'access-put'
        ? failed.reason
        : undefined
  if (!kind || !isSetupFailure(reason)) return null
  return {
    kind,
    reason: /** @type {string} */ (reason),
    ...(failed.detail ? { detail: failed.detail } : {}),
  }
}

/**
 * The proof note line for a hosted fallback from `from` (setupFailure), given the `page` result:
 * the failed publisher, its reason and its safe detail, then where the page went. Null without one.
 * @param {{kind: string, reason: string, detail?: string} | null | undefined} from
 * @param {{published?: boolean, reason?: string}} [page]
 */
export function fallbackNotice(from, page) {
  if (!from) return null
  const detail = safeDetail(from.detail ?? '')
  const label = from.kind === 'hook' ? 'proof-publish hook' : from.kind
  const failed = `${label}: ${from.reason.replaceAll('-', ' ')}${detail ? ` (${detail})` : ''}`
  return page?.published
    ? `${failed}; published to Bossanova hosting instead.`
    : `${failed}; Bossanova hosting did not take the page either (${page?.reason ?? 'unknown'}).`
}

/**
 * @param {any} config
 */
function hostedFallbackFor(config) {
  if (!config || config.publishConfig?.proof === false) return null
  return config.publishConfig?.proof?.hostedFallback === false ? null : { kind: 'hosted' }
}

/**
 * The publisher for a page on `baseSha` (see the module comment), as `{publisher, fallback?}` or
 * `{publisher: null, reason}`. A hook or Access PUT publisher carries `fallback: {kind: 'hosted'}`
 * unless its config forbids one. Only the base tree is read, so a PR can neither supply a
 * publisher nor switch uploads or the fallback on or off. Never throws.
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
    // A broken opt-out must never become a hosted upload.
    const config = readConfig()
    const fallback = hostedFallbackFor(config)
    if (extensions?.[0])
      return {
        publisher: { kind: 'hook', extension: extensions[0] },
        ...(fallback ? { fallback } : {}),
      }
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
        ...(fallback ? { fallback } : {}),
      }
    }
    // A block holding only `hostedFallback` names no publisher of its own.
    const fallbackOnly = proof && Object.keys(proof).join() === 'hostedFallback'
    return { publisher: { kind: proof && !fallbackOnly ? 'daemon' : 'hosted' } }
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
// The refusals that name no cause of their own, so they carry the error's detail.
const OPAQUE_REASONS = ['upload-failed', 'daemon-unavailable']
// Plain words for a daemon stage an operator must act on, put before the detail. The daemon's
// own message is never reflected, so the words live here.
const STAGE_EXPLANATIONS = {
  'server-not-configured': "Hosted review pages aren't set up on this server",
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
 * fitted to a guessed limit. An `upload-failed` or `daemon-unavailable` refusal carries the error's
 * failureDetail as `detail`, with the daemon's stage reason after its code and, for a stage an
 * operator must act on, plain words before it.
 * @param {{kind?: string}} publisher @param {unknown} error
 * @returns {{ok: false, reason: string, detail?: string, limit?: {kind: string, max: number}}}
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
  if (OPAQUE_REASONS.includes(reason)) {
    // The daemon names the stage that failed in its reason header; its message is never reflected.
    const stage = typeof header === 'string' && /^[a-z][a-z-]{0,39}$/.test(header) ? header : ''
    const detail = failureDetail(error)
    const at = detail.indexOf(':')
    const staged = stage && at > 0 ? `${detail.slice(0, at)} (${stage})${detail.slice(at)}` : detail
    const explanation = STAGE_EXPLANATIONS[/** @type {keyof typeof STAGE_EXPLANATIONS} */ (stage)]
    return { ok: false, reason, detail: explanation ? `${explanation}; ${staged}` : staged }
  }
  return { ok: false, reason }
}

/**
 * Send `names` from `dir` to UploadEvidence for `target`: `{ok: true, url}` when the daemon returns
 * the page URL, `{ok: true, base}` for a local bucket keyed by this PR head, or `{ok: false, reason}`.
 * @param {{publisher: {kind?: string}, names: string[], dir: string | null,
 *   target: {pr: number, headSha: string}, env: Record<string, string | undefined>,
 *   callDaemon: Function, timeoutMs: number}} input
 * @returns {Promise<{ok: true, url: string} | {ok: true, base: string} | {ok: false, reason: string, detail?: string, limit?: {kind: string, max: number}}>}
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

const HOSTED_LIMIT_FIELDS = Object.freeze({
  'file-count': 'maxFileCount',
  'file-bytes': 'maxFileBytes',
  'page-bytes': 'maxPageBytes',
})

/**
 * The page limits the daemon last saw bosso return for `workspacePath`'s repository, as
 * `[{kind, max}]`. Empty when the daemon has none fresh, predates the read, or the call fails, so
 * the caller keeps fitting on refusal. Never throws.
 * @param {{workspacePath: string, env: Record<string, string | undefined>, callDaemon: Function,
 *   timeoutMs?: number}} input
 * @returns {Promise<{kind: string, max: number}[]>}
 */
export async function hostedPageLimits({ workspacePath, env, callDaemon, timeoutMs = 5000 }) {
  let result
  try {
    result = await callDaemon(
      daemonSocket(env),
      'GetHostedProofLimits',
      { workspacePath },
      { timeoutMs },
    )
  } catch {
    return []
  }
  const limits = /** @type {Record<string, unknown>} */ (result?.limits ?? {})
  return Object.entries(HOSTED_LIMIT_FIELDS).flatMap(([kind, field]) => {
    // Connect JSON carries int64 as a decimal string.
    const raw = limits[field]
    const max = typeof raw === 'string' && /^[1-9][0-9]*$/.test(raw) ? Number(raw) : raw
    return typeof max === 'number' && Number.isSafeInteger(max) && max > 0 ? [{ kind, max }] : []
  })
}
