// biome-ignore lint/performance/noNamespaceImport: Sentry SDK namespace import required by integration spec.
import * as Sentry from '@sentry/react'

const dsn =
  // biome-ignore lint/security/noSecrets: Public Sentry DSN required by deployment spec.
  'https://f2047aedfb788b237eaa08d0a692fc3d@o4511396716871680.ingest.de.sentry.io/4511396756062288'
const redacted = '[REDACTED]'
const messageCap = 2000
const truncMarker = '...[truncated]'

const reGitHubToken = /(?:github_pat_[A-Za-z0-9_]{20,}|(?:ghs|gho|ghp|ghu|ghr)_[A-Za-z0-9]{30,})/gi
const reJwt = /eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{20,}/g
// reAuthHeader matches "Authorization: <scheme> <value>" for single-token
// schemes (Basic, Bearer, Token). reAuthHeaderMulti handles multi-parameter
// schemes (Digest, MAC, AWS4-HMAC-SHA256) whose credential contains commas
// and spaces. reBearer is the fallback for bare "Bearer <token>" without
// the Authorization: prefix.
const reAuthHeader = /(\bAuthorization:\s*(?:Basic|Bearer|Token)\s+)\S+/gi
const reAuthHeaderMulti = /(\bAuthorization:\s*(?:Digest|MAC|AWS4-HMAC-SHA256)\s+)[^\r\n]+/gi
const reBearer = /(\bBearer\s+)[A-Za-z0-9._~+/-]{20,}/gi
const reEmail = /[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/g
const reEnvSecret = /((?:api[_-]?key|secret|token|password|passwd)\s*[=:]\s*)\S+/gi

// Third-party denylist: drop Cloudflare Web Analytics beacon noise (BOS-352).
// The beacon is auto-injected at the Cloudflare Pages edge and is not our code.
const reBeaconScript = /beacon\.min\.js/i
const reCloudflareInsights = /static\.cloudflareinsights\.com/i
const thirdPartyNoisePatterns = [reBeaconScript, reCloudflareInsights]

// Unactionable parse-noise: a `SyntaxError: Unexpected token …` reaching
// production Sentry cannot be a first-party bundle defect, because a
// syntactically invalid bundle is caught before it ships — `docusaurus build`
// fails on invalid JS and the Playwright smoke loads the built site in
// Chromium. Any such error that still reaches Sentry is client-environment
// noise (legacy browser / injected extension script / corrupted partial
// download) we cannot control (BOS-383).
//
// Scope note (deliberate over-match, BOS-383): keyed to message text, not
// frame origin, this pattern also drops two adjacent first-party classes —
// `JSON.parse` failures (`Unexpected token 'o', "…" is not valid JSON`) and
// the `Unexpected token '<'` "HTML served where JS was expected" signature of
// a stale-asset / bad-deploy / CDN-routing miss. That trade-off is accepted,
// not overlooked: (a) `services/docs/src` does NO first-party `JSON.parse`
// (grep-verified), so no first-party data-parse SyntaxError exists to lose;
// (b) a bad deploy serving HTML-as-JS is caught by the docs Playwright smoke
// on the next deploy, Cloudflare Pages deploy status, and user reports — not by
// this one-event Sentry issue; and (c) this filter is a single, trivially
// reversible `beforeSend` guard. Frame-origin scoping was rejected because the
// originating event's culprit `?(assets/js/common)` is itself a first-party
// bundle frame, so an "external-frames-only" filter would fail to drop it.
const reUnexpectedToken = /Unexpected token/i

export function scrub(input: string): string {
  if (input === '') {
    return input
  }

  return input
    .replace(reGitHubToken, redacted)
    .replace(reJwt, redacted)
    .replace(reAuthHeader, `$1${redacted}`)
    .replace(reAuthHeaderMulti, `$1${redacted}`)
    .replace(reBearer, `$1${redacted}`)
    .replace(reEmail, redacted)
    .replace(reEnvSecret, `$1${redacted}`)
}

export function initSentry(opts: { env?: string; release?: string } = {}): void {
  // A dev-server HMR crash created a production issue (BOS-1170). Keep local
  // browser errors on the developer's machine instead of sending them to Sentry.
  if (process.env.NODE_ENV === 'development') {
    return
  }

  Sentry.init({
    dsn,
    environment: opts.env ?? 'production',
    release: opts.release,
    sampleRate: 1.0,
    tracesSampleRate: 0,
    sendDefaultPii: false,
    integrations: [],
    beforeSend(event): Sentry.ErrorEvent | null {
      if (isThirdPartyNoise(event) || isUnactionableParseNoise(event)) {
        return null
      }
      scrubEvent(event)
      return event
    },
  })
  Sentry.setTag('app', 'docs')
}

// isThirdPartyNoise reports whether the event's crashing (deepest/most-recent)
// stack frame originates from a denylisted third-party bundle. Only the
// primary exception's deepest frame is matched, mirroring Sentry denyUrls
// semantics: exception.values is ordered oldest-cause-first with the crashing
// exception last, and matching chained causes or shallower frames could drop
// a first-party error that merely passes through a third-party callback.
// Fails open: events without a parseable stacktrace are kept.
function isThirdPartyNoise(event: unknown): boolean {
  const eventRecord = asRecord(event)
  if (!eventRecord) {
    return false
  }

  const values = asArray(asRecord(eventRecord.exception)?.values)
  const primary = asRecord(values[values.length - 1])
  const frames = asArray(asRecord(primary?.stacktrace)?.frames)
  const deepest = asRecord(frames[frames.length - 1])
  if (!deepest) {
    return false
  }
  // Match filename OR abs_path: browser SDKs may relativize filename (or
  // emit '<anonymous>') while abs_path keeps the full third-party URL.
  const locations = [deepest.filename, deepest.abs_path].filter(
    (candidate): candidate is string => typeof candidate === 'string',
  )
  return locations.some((location) =>
    thirdPartyNoisePatterns.some((pattern) => pattern.test(location)),
  )
}

// isUnactionableParseNoise reports whether the event's primary (crashing)
// exception is a `SyntaxError` whose value matches `Unexpected token`. The
// match is keyed to BOTH the type and the message so first-party
// TypeError/ReferenceError with real stacks — and SyntaxErrors that are not
// parse-token failures — are never suppressed. The primary exception is the
// last element of exception.values, matching the isThirdPartyNoise ordering
// convention. Fails open: any unparseable shape is kept. See BOS-383.
function isUnactionableParseNoise(event: unknown): boolean {
  const eventRecord = asRecord(event)
  if (!eventRecord) {
    return false
  }

  const values = asArray(asRecord(eventRecord.exception)?.values)
  const primary = asRecord(values[values.length - 1])
  if (!primary) {
    return false
  }
  return (
    primary.type === 'SyntaxError' &&
    typeof primary.value === 'string' &&
    reUnexpectedToken.test(primary.value)
  )
}

function scrubEvent(event: unknown): void {
  const eventRecord = asRecord(event)
  if (!eventRecord) {
    return
  }

  eventRecord.message = capMessage(scrubString(eventRecord.message))
  eventRecord.transaction = scrubString(eventRecord.transaction)
  eventRecord.server_name = scrubString(eventRecord.server_name)
  eventRecord.user = undefined

  const exception = asRecord(eventRecord.exception)
  const values = asArray(exception?.values)
  for (const value of values) {
    const exceptionValue = asRecord(value)
    if (!exceptionValue) {
      continue
    }
    exceptionValue.type = scrubString(exceptionValue.type)
    exceptionValue.value = scrubString(exceptionValue.value)
    scrubStacktrace(exceptionValue.stacktrace)
  }

  for (const thread of asArray(eventRecord.threads)) {
    const threadRecord = asRecord(thread)
    scrubStacktrace(threadRecord?.stacktrace)
  }

  for (const breadcrumb of asArray(eventRecord.breadcrumbs)) {
    const crumb = asRecord(breadcrumb)
    if (!crumb) {
      continue
    }
    crumb.message = scrubString(crumb.message)
    if (crumb.data !== undefined) {
      crumb.data = scrubValue(crumb.data)
    }
  }

  const request = asRecord(eventRecord.request)
  if (request) {
    request.url = scrubString(request.url)
    request.query_string = scrubString(request.query_string)
    request.data = undefined
    request.cookies = undefined
    request.headers = undefined
    request.env = undefined
  }

  eventRecord.tags = scrubValue(eventRecord.tags)
  eventRecord.contexts = scrubValue(eventRecord.contexts)
  eventRecord.fingerprint = scrubValue(eventRecord.fingerprint)
}

function scrubStacktrace(stacktrace: unknown): void {
  const stacktraceRecord = asRecord(stacktrace)
  if (!stacktraceRecord) {
    return
  }

  for (const frameValue of asArray(stacktraceRecord.frames)) {
    const frame = asRecord(frameValue)
    if (!frame) {
      continue
    }
    frame.filename = scrubString(frame.filename)
    frame.abs_path = scrubString(frame.abs_path)
    frame.function = scrubString(frame.function)
    frame.module = scrubString(frame.module)
    frame.package = scrubString(frame.package)
    frame.context_line = scrubString(frame.context_line)
    frame.pre_context = scrubValue(frame.pre_context)
    frame.post_context = scrubValue(frame.post_context)
    frame.vars = undefined
  }
}

function scrubValue(value: unknown): unknown {
  if (typeof value === 'string') {
    return scrub(value)
  }

  if (Array.isArray(value)) {
    return value.map(scrubValue)
  }

  const record = asRecord(value)
  if (!record) {
    return value
  }

  const scrubbed: Record<string, unknown> = {}
  for (const [key, nestedValue] of Object.entries(record)) {
    scrubbed[scrub(key)] = scrubValue(nestedValue)
  }
  return scrubbed
}

function scrubString(value: unknown): string {
  return typeof value === 'string' ? scrub(value) : ''
}

function capMessage(message: string): string {
  if (message.length <= messageCap) {
    return message
  }
  return `${message.slice(0, messageCap)}${truncMarker}`
}

function asArray(value: unknown): unknown[] {
  return Array.isArray(value) ? value : []
}

function asRecord(value: unknown): Record<string, unknown> | undefined {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    return
  }
  return value as Record<string, unknown>
}
