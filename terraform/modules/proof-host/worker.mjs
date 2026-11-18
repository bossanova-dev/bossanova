// The proof host Worker: serves per-PR proof pages and their media out of the private R2 bucket
// bound as PROOF_BUCKET, accepts uploads from one Access service token, and deletes objects older
// than the retention window once a day.
//
// Cloudflare Access covers only /_signin and /_publish/. Page and media reads reach this Worker
// with no Access in front, so it authenticates them itself from the Access JWT in the
// CF_Authorization cookie (or the Cf-Access-Jwt-Assertion header), and a reader with no valid
// token gets a small sign-in gate instead of content. The gate is frameable, so a review app
// that embeds a page shows it instead of Access's login redirect, which refuses to be framed.
//
// Every token is checked here (RS256 against the team JWKS, aud, iss, exp/nbf, identity), on
// the Access-covered paths too: provider 4.x has no switch to turn off a script's workers.dev
// route, so the Access edge cannot be trusted to sit in front of any request.
//
// Keys are `<owner>/<repo>/<pr>/<sha>/<path>`, the prefix the publisher chooses.
// Bindings: PROOF_BUCKET (R2), and as plain text ACCESS_AUD, ACCESS_TEAM_DOMAIN,
// ALLOWED_EMAIL_DOMAINS (comma-separated), PUBLISH_CLIENT_ID, FRAME_ANCESTORS (space-separated
// https origins) and RETENTION_DAYS.
// No dependencies: Workers and Node 22 both provide Request, Response and crypto.subtle.

const FRAME_ANCESTOR = /^https:\/\/[a-z0-9.-]+(:[0-9]+)?$/

/**
 * The page CSP for a FRAME_ANCESTORS binding. A page with a recording loads its static player.js
 * beside it, and nothing inline; only that one key is ever served with a script type
 * (contentTypeFor), so 'self' admits no other. The listed origins may frame a page, and every
 * other site cannot. An empty binding, or any token that is not an https origin, frames from
 * nowhere: a typo fails closed instead of widening the list.
 */
export function contentSecurityPolicy(frameAncestors) {
  const origins = typeof frameAncestors === 'string' ? frameAncestors.split(/\s+/) : []
  const listed = origins.filter((origin) => origin !== '')
  const valid = listed.length > 0 && listed.every((origin) => FRAME_ANCESTOR.test(origin))
  const ancestors = valid ? listed.join(' ') : "'none'"
  return `default-src 'none'; script-src 'self'; img-src 'self' data:; media-src 'self'; style-src 'self' 'unsafe-inline'; font-src 'self'; frame-ancestors ${ancestors}; base-uri 'none'; form-action 'none'`
}

// Set on every response with the CSP, errors included, after any per-response header, so
// nothing overrides them.
export const SECURITY_HEADERS = Object.freeze({
  'Cache-Control': 'private, no-store',
  'Referrer-Policy': 'no-referrer',
  'X-Content-Type-Options': 'nosniff',
  'X-Robots-Tag': 'noindex, nofollow, noarchive',
})

export const JWT_HEADER = 'Cf-Access-Jwt-Assertion'
export const JWT_COOKIE = 'CF_Authorization'

// The two paths the module's Access application covers, and the gate's script. GitHub owner
// names cannot start with `_`, so none of them shadows a page.
export const SIGNIN_PATH = '/_signin'
export const PUBLISH_PREFIX = '/_publish'
export const GATE_SCRIPT_PATH = '/_gate.js'

// An unknown `kid` refetches the JWKS at most once per window, so a flood of forged tokens on the
// workers.dev route cannot turn into a flood of certificate fetches.
export const JWKS_REFETCH_COOLDOWN_MS = 30_000

const OWNER_OR_REPO = /^[A-Za-z0-9._-]+$/
const PR = /^[1-9][0-9]{0,6}$/
const SHA = /^[0-9a-f]{7,40}$/
const READ_METHODS = new Set(['GET', 'HEAD'])
// More CF_Authorization cookies than a browser holds for one host; the rest are ignored.
const MAX_COOKIE_TOKENS = 4

// Served types by extension. The type an upload declared is never served, so no object becomes a
// script, or a page, by its PUT header. The publisher's own table, minus js.
const CONTENT_TYPES = {
  css: 'text/css; charset=utf-8',
  gif: 'image/gif',
  html: 'text/html; charset=utf-8',
  jpeg: 'image/jpeg',
  jpg: 'image/jpeg',
  json: 'application/json',
  mp4: 'video/mp4',
  ndjson: 'application/x-ndjson',
  pdf: 'application/pdf',
  png: 'image/png',
  svg: 'image/svg+xml',
  txt: 'text/plain; charset=utf-8',
  webm: 'video/webm',
  webp: 'image/webp',
  woff2: 'font/woff2',
}
const PLAYER = /^[^/]+\/[^/]+\/[^/]+\/[^/]+\/player\.js$/
const SCRIPT_TYPE = 'text/javascript; charset=utf-8'
const PDF_TYPE = 'application/pdf'

/** The type a key is served with: a script only for `<prefix>player.js`, else by extension. */
export function contentTypeFor(key) {
  if (PLAYER.test(key)) return SCRIPT_TYPE
  const name = key.slice(key.lastIndexOf('/') + 1)
  const dot = name.lastIndexOf('.')
  const extension = dot > 0 ? name.slice(dot + 1).toLowerCase() : ''
  return CONTENT_TYPES[extension] ?? 'application/octet-stream'
}
const RS256 = { hash: 'SHA-256', name: 'RSASSA-PKCS1-v1_5' }

// Module scope, so it survives across requests on one isolate. Keyed by certs URL.
const jwksCache = new Map()

/** Forget every cached JWKS; for tests. */
export function resetJwksCache() {
  jwksCache.clear()
}

// A text body gets an explicit type, so a HEAD (which drops the body) keeps the same headers.
// handleRequest adds the security headers to every response last.
function respond(status, body = null, headers = {}) {
  const typed =
    typeof body === 'string' ? { 'Content-Type': 'text/plain; charset=utf-8', ...headers } : headers
  return new Response(body, { headers: typed, status })
}

function base64UrlBytes(text) {
  if (typeof text !== 'string' || !/^[A-Za-z0-9_-]*$/.test(text)) return null
  const padded = text.replace(/-/g, '+').replace(/_/g, '/')
  const binary = atob(padded + '='.repeat((4 - (padded.length % 4)) % 4))
  return Uint8Array.from(binary, (char) => char.charCodeAt(0))
}

function base64UrlJson(text) {
  try {
    const bytes = base64UrlBytes(text)
    if (!bytes) return null
    const value = JSON.parse(new TextDecoder().decode(bytes))
    return value !== null && typeof value === 'object' && !Array.isArray(value) ? value : null
  } catch {
    return null
  }
}

async function loadJwks(entry, certsUrl, fetchImpl) {
  try {
    const response = await fetchImpl(certsUrl)
    if (!response.ok) return
    const body = await response.json()
    const keys = new Map()
    for (const jwk of Array.isArray(body?.keys) ? body.keys : []) {
      if (typeof jwk?.kid === 'string' && jwk.kty === 'RSA') keys.set(jwk.kid, jwk)
    }
    entry.keys = keys
  } catch {
    // An unreachable or malformed JWKS keeps the previous keys; the token fails closed below.
  }
}

// Concurrent callers share one in-flight fetch: a request that arrives while the first fetch on a
// cold isolate is still pending awaits it, instead of seeing the empty entry and answering 403.
async function fetchJwks(certsUrl, fetchImpl, now) {
  const entry = jwksCache.get(certsUrl) ?? {
    fetchedAt: -Infinity,
    inFlight: null,
    keys: new Map(),
  }
  jwksCache.set(certsUrl, entry)
  if (!entry.inFlight) {
    entry.fetchedAt = now
    entry.inFlight = loadJwks(entry, certsUrl, fetchImpl).finally(() => {
      entry.inFlight = null
    })
  }
  await entry.inFlight
  return entry
}

async function signingKey(kid, certsUrl, { fetchImpl, now }) {
  let entry = jwksCache.get(certsUrl)
  if (entry?.inFlight) await entry.inFlight
  if (!entry || (!entry.keys.has(kid) && now - entry.fetchedAt >= JWKS_REFETCH_COOLDOWN_MS)) {
    entry = await fetchJwks(certsUrl, fetchImpl, now)
  }
  const jwk = entry.keys.get(kid)
  if (!jwk) return null
  return crypto.subtle.importKey('jwk', jwk, RS256, false, ['verify'])
}

/**
 * Verify the Access JWT: RS256 signature against the team's JWKS, `aud` containing ACCESS_AUD,
 * `iss` the team domain, and the `exp`/`nbf` times. Returns the claims, or null on any failure,
 * including one that throws (bad base64, an unimportable key): a bad token is turned away, never a 500.
 */
export async function verifyAccessJwt(token, env, deps) {
  try {
    return await checkAccessJwt(token, env, deps)
  } catch {
    return null
  }
}

async function checkAccessJwt(token, env, deps) {
  if (typeof token !== 'string' || !env?.ACCESS_AUD || !env?.ACCESS_TEAM_DOMAIN) return null
  const parts = token.split('.')
  if (parts.length !== 3) return null
  const [encodedHeader, encodedPayload, encodedSignature] = parts
  const header = base64UrlJson(encodedHeader)
  const claims = base64UrlJson(encodedPayload)
  const signature = base64UrlBytes(encodedSignature)
  if (!header || !claims || !signature || signature.length === 0) return null
  if (header.alg !== 'RS256' || typeof header.kid !== 'string') return null

  const issuer = `https://${env.ACCESS_TEAM_DOMAIN}`
  const key = await signingKey(header.kid, `${issuer}/cdn-cgi/access/certs`, deps)
  if (!key) return null
  const signed = new TextEncoder().encode(`${encodedHeader}.${encodedPayload}`)
  if (!(await crypto.subtle.verify(RS256, key, signature, signed))) return null

  const audiences = Array.isArray(claims.aud) ? claims.aud : [claims.aud]
  if (!audiences.includes(env.ACCESS_AUD)) return null
  if (claims.iss !== issuer) return null
  const nowSeconds = deps.now / 1000
  if (typeof claims.exp !== 'number' || claims.exp <= nowSeconds) return null
  if (claims.nbf !== undefined && (typeof claims.nbf !== 'number' || claims.nbf > nowSeconds))
    return null
  return claims
}

function decodeSegment(raw) {
  if (raw === '') return null
  let decoded
  try {
    decoded = decodeURIComponent(raw)
  } catch {
    return null
  }
  if (decoded === '' || decoded === '.' || decoded === '..') return null
  if (decoded.includes('/') || decoded.includes('\\')) return null
  return decoded
}

/**
 * Map a URL path onto the page grammar: `{redirect}` for `/<owner>/<repo>/<pr>/<sha>`,
 * `{key, page}` for its page (spelled `/` or `/index.html`) or an asset under it, or null for
 * anything else.
 */
export function routePath(pathname) {
  const segments = pathname.split('/').slice(1)
  if (segments.length < 4) return null
  const [owner, repo, pr, sha, ...rest] = segments
  for (const name of [owner, repo]) {
    if (!OWNER_OR_REPO.test(name) || name === '.' || name === '..') return null
  }
  if (!PR.test(pr) || !SHA.test(sha)) return null
  const prefix = `${owner}/${repo}/${pr}/${sha}/`
  if (rest.length === 0) return { redirect: `/${prefix}` }
  if (rest.length === 1 && (rest[0] === '' || rest[0] === 'index.html'))
    return { key: `${prefix}index.html`, page: true }
  const asset = rest.map(decodeSegment)
  if (asset.includes(null)) return null
  return { key: `${prefix}${asset.join('/')}`, page: false }
}

// A single `bytes=` range against an object of `size` bytes: null to serve the whole object,
// `{unsatisfiable}` for a 416, otherwise the inclusive `{start, end}`. A multi-range or malformed
// header is ignored and served as a plain 200.
export function parseRange(header, size) {
  const match = /^bytes=(\d*)-(\d*)$/.exec(header ?? '')
  if (!match || (match[1] === '' && match[2] === '')) return null
  if (match[1] === '') {
    const suffix = Number(match[2])
    if (suffix === 0 || size === 0) return { unsatisfiable: true }
    return { end: size - 1, start: Math.max(0, size - suffix) }
  }
  const start = Number(match[1])
  // `bytes=9-3` is malformed (ignored); an open-ended `bytes=500-` past the end is a 416.
  if (match[2] !== '' && Number(match[2]) < start) return null
  if (start >= size) return { unsatisfiable: true }
  const end = match[2] === '' ? size - 1 : Number(match[2])
  return { end: Math.min(end, size - 1), start }
}

async function serve(request, env, key) {
  const meta = await env.PROOF_BUCKET.head(key)
  if (!meta) return respond(404, 'Not found')
  const type = contentTypeFor(key)
  const headers = { 'Accept-Ranges': 'bytes', 'Content-Type': type }
  // A page links a PDF and never embeds it, so the browser downloads it instead of opening its
  // own viewer on this origin.
  if (type === PDF_TYPE) headers['Content-Disposition'] = 'attachment'
  const range = parseRange(request.headers.get('Range'), meta.size)
  if (range?.unsatisfiable) {
    return respond(416, null, {
      ...headers,
      'Content-Range': `bytes */${meta.size}`,
    })
  }
  const status = range ? 206 : 200
  if (range) headers['Content-Range'] = `bytes ${range.start}-${range.end}/${meta.size}`
  headers['Content-Length'] = String(range ? range.end - range.start + 1 : meta.size)
  if (request.method === 'HEAD') return respond(status, null, headers)
  const object = await env.PROOF_BUCKET.get(
    key,
    range ? { range: { length: range.end - range.start + 1, offset: range.start } } : undefined,
  )
  if (!object) return respond(404, 'Not found')
  return respond(status, object.body, headers)
}

const escapeHtml = (text) =>
  text.replace(
    /[&<>"']/g,
    (char) => ({ '"': '&quot;', '&': '&amp;', "'": '&#39;', '<': '&lt;', '>': '&gt;' })[char],
  )

/**
 * The page a reader with no valid token gets in place of a proof page. It is the same for a page
 * that exists and one that does not, and its only request-derived text is the sign-in link for
 * the path the reader asked for.
 */
export function gatePage(signinUrl) {
  const href = escapeHtml(signinUrl)
  return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow, noarchive, nosnippet, noimageindex">
<title>Sign in</title>
<style>
body { margin: 0; font: 15px/1.5 system-ui, sans-serif; color: #1f2328; background: #fff; }
main { max-width: 28rem; margin: 15vh auto 0; padding: 0 16px; text-align: center; }
h1 { font-size: 1.15rem; font-weight: 600; margin: 0 0 1.25rem; }
a.signin { display: inline-block; padding: 0.5rem 1.25rem; border-radius: 6px; background: #1f6feb; color: #fff; text-decoration: none; font-weight: 600; }
p { margin: 0 0 1rem; color: #59636e; font-size: 0.875rem; }
code { overflow-wrap: anywhere; user-select: all; }
@media (prefers-color-scheme: dark) {
  body { color: #e6edf3; background: #0d1117; }
  p { color: #9198a1; }
}
</style>
<script src="${GATE_SCRIPT_PATH}" defer></script>
</head>
<body>
<main>
<h1>Sign in to review this PR</h1>
<p><a class="signin" href="${href}" target="_blank" rel="noopener noreferrer">Sign in</a></p>
<p>Sign-in opens in a new tab. The review summary shows here when you come back.</p>
<p>If no tab opens, open <code>${href}</code> yourself.</p>
</main>
</body>
</html>
`
}

// Reload once the reader comes back from the sign-in tab: the tab turning visible again, or the
// frame regaining focus, after it was hidden or lost focus. Reloading on any focus would race the
// click on the sign-in link, which focuses the frame first.
export const GATE_SCRIPT = `let away = false
const comeBack = () => {
  if (away && document.visibilityState === 'visible') location.reload()
}
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'hidden') away = true
  else comeBack()
})
window.addEventListener('blur', () => {
  away = true
})
window.addEventListener('focus', comeBack)
`

/**
 * Where `/_signin` sends a signed-in reader: the absolute URL of a proof page on this host, or
 * null for anything else. `next` must already be the normalised path of a page, so a scheme, a
 * host, `//`, a backslash, whitespace, a query, a fragment, dot segments or percent-encoded
 * separators all fail one of the three checks.
 */
export function signinTarget(next, requestUrl) {
  let parsed
  try {
    parsed = new URL(next, requestUrl)
  } catch {
    return null
  }
  if (parsed.origin !== new URL(requestUrl).origin || parsed.pathname !== next) return null
  const route = routePath(parsed.pathname)
  return route?.page || route?.redirect ? parsed.href : null
}

function cookieTokens(header) {
  if (typeof header !== 'string') return []
  const prefix = `${JWT_COOKIE}=`
  return header
    .split(';')
    .map((part) => part.trim())
    .filter((part) => part.startsWith(prefix))
    .map((part) => part.slice(prefix.length))
    .slice(0, MAX_COOKIE_TOKENS)
}

// The ALLOWED_EMAIL_DOMAINS binding as lower-case domains; empty or missing admits no staff.
function allowedDomains(binding) {
  if (typeof binding !== 'string') return []
  return binding
    .split(',')
    .map((domain) => domain.trim().toLowerCase())
    .filter((domain) => domain !== '')
}

/**
 * Who a verified token speaks for: `publisher` for the publish service token (Access puts its
 * client id in `common_name`), `staff` for an SSO login whose email's domain is one of
 * ALLOWED_EMAIL_DOMAINS, else null.
 */
function identityOf(claims, env) {
  if (
    typeof env.PUBLISH_CLIENT_ID === 'string' &&
    env.PUBLISH_CLIENT_ID !== '' &&
    claims.common_name === env.PUBLISH_CLIENT_ID
  )
    return 'publisher'
  const email = claims.email
  if (typeof email !== 'string') return null
  const at = email.lastIndexOf('@')
  if (at <= 0) return null
  const domain = email.slice(at + 1).toLowerCase()
  return allowedDomains(env.ALLOWED_EMAIL_DOMAINS).includes(domain) ? 'staff' : null
}

async function identify(tokens, env, deps) {
  for (const token of tokens) {
    const claims = await verifyAccessJwt(token, env, deps)
    const identity = claims ? identityOf(claims, env) : null
    if (identity) return identity
  }
  return null
}

// A top-level tab can follow Access's login redirect itself, as it did before the gate; a frame
// cannot, because the login page refuses to be framed, so a frame gets the gate instead.
function signInFirst(request, url) {
  const signin = `${SIGNIN_PATH}?next=${encodeURIComponent(url.pathname)}`
  if (request.headers.get('Sec-Fetch-Dest') === 'document') {
    return respond(302, null, { Location: new URL(signin, url).href })
  }
  return respond(200, gatePage(new URL(signin, url).href), {
    'Content-Type': 'text/html; charset=utf-8',
  })
}

/** Page and media reads: no Access in front, so the token comes from the cookie or the header. */
async function read({ deps, env, request, url }) {
  const route = routePath(url.pathname)
  const identity = await identify(
    [request.headers.get(JWT_HEADER), ...cookieTokens(request.headers.get('Cookie'))],
    env,
    deps,
  )
  if (!identity) {
    return route?.page || route?.redirect ? signInFirst(request, url) : respond(403, 'Forbidden')
  }
  if (!route) return respond(404, 'Not found')
  if (route.redirect) {
    return respond(301, null, { Location: new URL(route.redirect, url).href })
  }
  return serve(request, env, route.key)
}

/** `/_signin` sits behind Access, so a staff login arrives here and goes on to `next`. */
async function signIn({ deps, env, request, url }) {
  const identity = await identify([request.headers.get(JWT_HEADER)], env, deps)
  if (identity !== 'staff') return respond(403, 'Forbidden')
  const next = url.searchParams.get('next')
  if (next === null) return respond(200, 'Signed in. You can close this tab.')
  const target = signinTarget(next, url.href)
  if (!target) return respond(400, 'Bad request')
  return respond(302, null, { Location: target })
}

/** `/_publish/<owner>/<repo>/<pr>/<sha>/<path>` sits behind Access; only the publish token writes. */
async function publish({ deps, env, request, url }) {
  const identity = await identify([request.headers.get(JWT_HEADER)], env, deps)
  if (identity !== 'publisher') return respond(403, 'Forbidden')
  const route = routePath(url.pathname.slice(PUBLISH_PREFIX.length))
  // A write names its object, so the page goes up as `index.html`, never as the bare `/`.
  if (!route?.key || url.pathname.endsWith('/')) return respond(404, 'Not found')
  await env.PROOF_BUCKET.put(route.key, request.body, {
    httpMetadata: {
      contentType: request.headers.get('Content-Type') ?? 'application/octet-stream',
    },
  })
  return respond(201)
}

function gateScript() {
  return respond(200, GATE_SCRIPT, { 'Content-Type': SCRIPT_TYPE })
}

// Every path is exactly one of these, chosen by path alone before any token is read.
const ENDPOINTS = {
  gateScript: { handle: gateScript, methods: READ_METHODS },
  publish: { handle: publish, methods: new Set(['PUT']) },
  read: { handle: read, methods: READ_METHODS },
  signIn: { handle: signIn, methods: READ_METHODS },
}

function endpointFor(pathname) {
  if (pathname === GATE_SCRIPT_PATH) return ENDPOINTS.gateScript
  if (pathname === SIGNIN_PATH) return ENDPOINTS.signIn
  if (pathname.startsWith(`${PUBLISH_PREFIX}/`)) return ENDPOINTS.publish
  return ENDPOINTS.read
}

/**
 * Handle one request. `deps` is injectable for tests: `fetchImpl` fetches the JWKS and `now()`
 * returns the clock in milliseconds.
 */
export async function handleRequest(request, env, deps = {}) {
  const resolved = {
    fetchImpl: deps.fetchImpl ?? ((input, init) => fetch(input, init)),
    now: (deps.now ?? Date.now)(),
  }
  const response = await decide(request, env, resolved)
  const headers = new Headers(response.headers)
  headers.set('Content-Security-Policy', contentSecurityPolicy(env?.FRAME_ANCESTORS))
  for (const [name, value] of Object.entries(SECURITY_HEADERS)) headers.set(name, value)
  // HEAD answers exactly as GET would, minus the body.
  return new Response(request.method === 'HEAD' ? null : response.body, {
    headers,
    status: response.status,
  })
}

async function decide(request, env, deps) {
  try {
    const url = new URL(request.url)
    const endpoint = endpointFor(url.pathname)
    if (!endpoint.methods.has(request.method)) {
      return respond(405, 'Method not allowed', {
        Allow: [...endpoint.methods].join(', '),
      })
    }
    return await endpoint.handle({ deps, env, request, url })
  } catch {
    return respond(500, 'Internal error')
  }
}

// List one page of objects per call.
export const SWEEP_PAGE_SIZE = 1000
// Pages listed per run at most, so one run stays finite on any bucket. Each run starts from the
// first key again, so a bucket past this many pages is swept only in its first pages.
export const SWEEP_PAGE_BUDGET = 100
const DAY_MS = 86_400_000

/**
 * The daily retention sweep: delete every object uploaded before `scheduledTime` minus
 * RETENTION_DAYS days. A missing, non-integer or non-positive RETENTION_DAYS deletes nothing.
 * There are no exempt prefixes: every key this host stores is a proof page's.
 */
export async function sweep(event, env) {
  const days = Number(env.RETENTION_DAYS)
  if (!Number.isInteger(days) || days < 1) return
  const cutoff = event.scheduledTime - days * DAY_MS
  let cursor
  for (let page = 0; page < SWEEP_PAGE_BUDGET; page++) {
    const listed = await env.PROOF_BUCKET.list({ cursor, limit: SWEEP_PAGE_SIZE })
    const expired = listed.objects
      .filter((object) => object.uploaded.getTime() < cutoff)
      .map((object) => object.key)
    if (expired.length > 0) await env.PROOF_BUCKET.delete(expired)
    if (!listed.truncated) return
    cursor = listed.cursor
  }
}

// Workers module syntax needs a default export; handleRequest and sweep stay the named, testable
// entry points.
export default {
  fetch(request, env) {
    return handleRequest(request, env)
  },
  scheduled: sweep,
}
