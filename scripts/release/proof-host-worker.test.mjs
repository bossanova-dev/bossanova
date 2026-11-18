// node --test suite for the proof-host module's Worker (terraform/modules/proof-host/worker.mjs).
//
// The tests sign their own RS256 JWTs with a WebCrypto key pair, serve the JWKS through a
// recording fetch stub, and use a Map-backed fake bucket that counts its own calls: the fetch
// stub only ever sees JWKS fetches, so it cannot prove anything about the bucket.
import assert from 'node:assert/strict'
import { beforeEach, test } from 'node:test'

import proofHost, {
  contentSecurityPolicy,
  contentTypeFor,
  GATE_SCRIPT,
  handleRequest,
  JWKS_REFETCH_COOLDOWN_MS,
  parseRange,
  resetJwksCache,
  routePath,
  signinTarget,
  SWEEP_PAGE_BUDGET,
  SWEEP_PAGE_SIZE,
  sweep,
} from '../../terraform/modules/proof-host/worker.mjs'

const AUD = 'aud-tag-proof-pages'
const TEAM_DOMAIN = 'proof-team.cloudflareaccess.com'
const ISSUER = `https://${TEAM_DOMAIN}`
const CERTS_URL = `${ISSUER}/cdn-cgi/access/certs`
const PUBLISH_CLIENT_ID = 'abc123.access'
const NOW = Date.UTC(2026, 10, 5, 12, 0, 0)
const NOW_SECONDS = NOW / 1000
const BASE = 'https://proof.example.com'
const PAGE = '/acme/widgets/123/abcdef1/'
const PAGE_FILE = `${PAGE}index.html`
const PUBLISH = `/_publish${PAGE}`
const GATE_TITLE = 'Sign in to review this PR'
const SIGNIN_URL = `${BASE}/_signin?next=${encodeURIComponent(PAGE)}`
const RS256 = { hash: 'SHA-256', name: 'RSASSA-PKCS1-v1_5' }
const EMBEDDERS = 'https://app.bossanova.dev https://app-staging.bossanova.dev'
const policyFramedBy = (ancestors) => ({
  'base-uri': "'none'",
  'default-src': "'none'",
  'font-src': "'self'",
  'form-action': "'none'",
  'frame-ancestors': ancestors,
  'img-src': "'self' data:",
  'media-src': "'self'",
  'script-src': "'self'",
  'style-src': "'self' 'unsafe-inline'",
})

async function keyPair(kid) {
  const pair = await crypto.subtle.generateKey(
    {
      ...RS256,
      modulusLength: 2048,
      publicExponent: new Uint8Array([1, 0, 1]),
    },
    true,
    ['sign', 'verify'],
  )
  const jwk = await crypto.subtle.exportKey('jwk', pair.publicKey)
  return {
    jwk: { ...jwk, alg: 'RS256', kid, use: 'sig' },
    kid,
    privateKey: pair.privateKey,
  }
}

const primary = await keyPair('kid-primary')
const rotated = await keyPair('kid-rotated')
const stranger = await keyPair('kid-primary') // same kid, different key

const encode = (value) =>
  Buffer.from(typeof value === 'string' ? value : JSON.stringify(value)).toString('base64url')

async function sign(claims, { header = {}, key = primary } = {}) {
  const head = encode({ alg: 'RS256', kid: key.kid, typ: 'JWT', ...header })
  const body = encode(claims)
  const signature = await crypto.subtle.sign(
    RS256,
    key.privateKey,
    new TextEncoder().encode(`${head}.${body}`),
  )
  return `${head}.${body}.${Buffer.from(signature).toString('base64url')}`
}

const baseClaims = {
  aud: [AUD],
  exp: NOW_SECONDS + 600,
  iat: NOW_SECONDS - 60,
  iss: ISSUER,
  nbf: NOW_SECONDS - 60,
}
const ssoToken = (extra = {}, options) =>
  sign({ ...baseClaims, email: 'reviewer@example.com', ...extra }, options)
const serviceToken = (commonName = PUBLISH_CLIENT_ID) =>
  sign({ ...baseClaims, common_name: commonName })

function fakeBucket(objects = {}) {
  const store = new Map(
    Object.entries(objects).map(([key, { body, contentType }]) => [
      key,
      { bytes: Buffer.from(body), contentType },
    ]),
  )
  const counts = { get: 0, head: 0, put: 0 }
  const meta = (key) => {
    const entry = store.get(key)
    return entry
      ? {
          httpMetadata: entry.contentType ? { contentType: entry.contentType } : {},
          key,
          size: entry.bytes.length,
        }
      : null
  }
  return {
    counts,
    async get(key, options) {
      counts.get += 1
      const entry = store.get(key)
      if (!entry) return null
      const { length, offset } = options?.range ?? {
        length: entry.bytes.length,
        offset: 0,
      }
      return {
        ...meta(key),
        body: entry.bytes.subarray(offset, offset + length),
      }
    },
    async head(key) {
      counts.head += 1
      return meta(key)
    },
    async put(key, body, options) {
      counts.put += 1
      const bytes = Buffer.from(await new Response(body).arrayBuffer())
      store.set(key, { bytes, contentType: options?.httpMetadata?.contentType })
    },
    store,
  }
}

function jwksStub(keys = [primary]) {
  const stub = {
    calls: [],
    keys,
    fetchImpl: async (url) => {
      stub.calls.push(String(url))
      return Response.json({ keys: stub.keys.map((key) => key.jwk) })
    },
  }
  return stub
}

const HUNDRED_BYTES = 'x'.repeat(90) + '0123456789'

// `bindings` overrides the Worker's env; `ancestors` is the frame-ancestors every response
// must then carry.
function fixture({ ancestors = EMBEDDERS, bindings = {} } = {}) {
  const bucket = fakeBucket({
    'acme/widgets/123/abcdef1/index.html': {
      body: '<h1>proof</h1>',
      contentType: 'text/html; charset=utf-8',
    },
    'acme/widgets/123/abcdef1/screenshots/a.png': {
      body: 'PNGDATA',
      contentType: 'image/png',
    },
    'acme/widgets/123/abcdef1/video.mp4': {
      body: HUNDRED_BYTES,
      contentType: 'video/mp4',
    },
    'acme/widgets/123/abcdef1/untyped.bin': { body: 'raw' },
    // Stored inline-typed, as an upload could declare it.
    'acme/widgets/123/abcdef1/media/pdf-statement.pdf': {
      body: HUNDRED_BYTES,
      contentType: 'text/html; charset=utf-8',
    },
    'acme/widgets/123/abcdef1/player.js': {
      body: 'PLAYER',
      contentType: 'text/javascript; charset=utf-8',
    },
    // Stored with types their keys do not have, as a bad upload would.
    'acme/widgets/123/abcdef1/media/head-0.mp4': {
      body: HUNDRED_BYTES,
      contentType: 'text/html; charset=utf-8',
    },
    'acme/widgets/123/abcdef1/media/player.js': {
      body: 'alert(1)',
      contentType: 'text/javascript; charset=utf-8',
    },
    'acme/widgets/123/abcdef1/shot.png': {
      body: 'PNG',
      contentType: 'text/javascript; charset=utf-8',
    },
  })
  const jwks = jwksStub()
  const env = {
    ACCESS_AUD: AUD,
    ACCESS_TEAM_DOMAIN: TEAM_DOMAIN,
    ALLOWED_EMAIL_DOMAINS: 'example.com',
    FRAME_ANCESTORS: EMBEDDERS,
    PROOF_BUCKET: bucket,
    PUBLISH_CLIENT_ID,
    RETENTION_DAYS: '90',
    ...bindings,
  }
  const call = async (
    path,
    { body, cookie, headers = {}, method = 'GET', now = NOW, token } = {},
  ) => {
    const request = new Request(`${BASE}${path}`, {
      body,
      duplex: body ? 'half' : undefined,
      headers: {
        ...headers,
        ...(token ? { 'Cf-Access-Jwt-Assertion': token } : {}),
        ...(cookie ? { Cookie: `CF_Authorization=${cookie}` } : {}),
      },
      method,
    })
    const response = await handleRequest(request, env, {
      fetchImpl: jwks.fetchImpl,
      now: () => now,
    })
    assertSecurityHeaders(response, ancestors)
    return response
  }
  return { bucket, call, env, jwks }
}

const directives = (policy) =>
  Object.fromEntries(
    policy.split(';').map((part) => {
      const [name, ...sources] = part.trim().split(/\s+/)
      return [name, sources.join(' ')]
    }),
  )

function assertSecurityHeaders(response, ancestors = EMBEDDERS) {
  const policy = response.headers.get('Content-Security-Policy') ?? ''
  const expected = policyFramedBy(ancestors)
  assert.deepEqual(directives(policy), expected)
  assert.equal(policy.split(';').length, Object.keys(expected).length)
  assert.equal(response.headers.get('X-Robots-Tag'), 'noindex, nofollow, noarchive')
  assert.equal(response.headers.get('X-Content-Type-Options'), 'nosniff')
  assert.equal(response.headers.get('Referrer-Policy'), 'no-referrer')
  assert.equal(response.headers.get('Cache-Control'), 'private, no-store')
}

async function assertGate(response, label) {
  assert.equal(response.status, 200, label)
  assert.equal(response.headers.get('Content-Type'), 'text/html; charset=utf-8', label)
  const body = await response.text()
  assert.ok(body.includes(GATE_TITLE), label)
  assert.ok(!body.includes('<h1>proof</h1>'), label)
  return body
}

beforeEach(() => resetJwksCache())

test('an SSO JWT on GET of the page, by either spelling, returns its index.html as html', async () => {
  const { call } = fixture()
  const token = await ssoToken()
  for (const path of [PAGE, PAGE_FILE]) {
    const response = await call(path, { token })
    assert.equal(response.status, 200, path)
    assert.equal(response.headers.get('Content-Type'), 'text/html; charset=utf-8', path)
    assert.equal(response.headers.get('Accept-Ranges'), 'bytes', path)
    assert.equal(await response.text(), '<h1>proof</h1>', path)
  }
})

test('an SSO JWT on GET of an asset returns it, and a missing key is 404', async () => {
  const { call } = fixture()
  const token = await ssoToken()
  const shot = await call(`${PAGE}screenshots/a.png`, { token })
  assert.equal(shot.status, 200)
  assert.equal(shot.headers.get('Content-Type'), 'image/png')
  assert.equal(await shot.text(), 'PNGDATA')

  const missing = await call(`${PAGE}screenshots/missing.png`, { token })
  assert.equal(missing.status, 404)
})

test('an object stored without a content type is served as application/octet-stream', async () => {
  const { call } = fixture()
  const response = await call(`${PAGE}untyped.bin`, { token: await ssoToken() })
  assert.equal(response.status, 200)
  assert.equal(response.headers.get('Content-Type'), 'application/octet-stream')
})

test('pages, assets and errors may be framed only by the configured origins', async () => {
  const { call } = fixture()
  const token = await ssoToken()
  const responses = {
    asset: await call(`${PAGE}screenshots/a.png`, { token }),
    forbidden: await call(`${PAGE}screenshots/a.png`),
    gate: await call(PAGE),
    missing: await call(`${PAGE}missing.png`, { token }),
    page: await call(PAGE, { token }),
    unroutable: await call('/nope', { token }),
    wrongMethod: await call(PAGE, { method: 'DELETE', token }),
  }
  const seen = Object.fromEntries(
    Object.entries(responses).map(([name, response]) => [
      name,
      [
        response.status,
        directives(response.headers.get('Content-Security-Policy'))['frame-ancestors'],
      ],
    ]),
  )
  assert.deepEqual(seen, {
    asset: [200, EMBEDDERS],
    forbidden: [403, EMBEDDERS],
    gate: [200, EMBEDDERS],
    missing: [404, EMBEDDERS],
    page: [200, EMBEDDERS],
    unroutable: [404, EMBEDDERS],
    wrongMethod: [405, EMBEDDERS],
  })
})

test('only the page-root player.js is served as a script, and every type comes from the key', async () => {
  const { call } = fixture()
  const token = await ssoToken()
  const served = async (path) =>
    (await call(`${PAGE}${path}`, { token })).headers.get('Content-Type')
  assert.equal(await served('player.js'), 'text/javascript; charset=utf-8')
  assert.equal(await served('media/player.js'), 'application/octet-stream')
  assert.equal(await served('media/head-0.mp4'), 'video/mp4')
  assert.equal(await served('shot.png'), 'image/png')
  assert.equal(contentTypeFor('a/b/1/abcdef1/x/player.js'), 'application/octet-stream')
  assert.equal(contentTypeFor('a/b/1/abcdef1/media/still.WEBP'), 'image/webp')
  assert.equal(contentTypeFor('a/b/1/abcdef1/.png'), 'application/octet-stream')
})

test('a recording under media/ streams by range as video/mp4', async () => {
  const { call } = fixture()
  const response = await call(`${PAGE}media/head-0.mp4`, {
    headers: { Range: 'bytes=90-99' },
    token: await ssoToken(),
  })
  assert.equal(response.status, 206)
  assert.equal(response.headers.get('Content-Type'), 'video/mp4')
  assert.equal(response.headers.get('Accept-Ranges'), 'bytes')
  assert.equal(response.headers.get('Content-Range'), 'bytes 90-99/100')
  assert.equal(await response.text(), '0123456789')
})

test('a PDF is served as application/pdf for download on GET, HEAD and a range', async () => {
  const { call } = fixture()
  const token = await ssoToken()
  const path = `${PAGE}media/pdf-statement.pdf`
  const responses = [
    await call(path, { token }),
    await call(path, { method: 'HEAD', token }),
    await call(path, { headers: { Range: 'bytes=90-99' }, token }),
  ]
  assert.deepEqual(
    responses.map((response) => [
      response.status,
      response.headers.get('Content-Type'),
      response.headers.get('Content-Disposition'),
    ]),
    [
      [200, 'application/pdf', 'attachment'],
      [200, 'application/pdf', 'attachment'],
      [206, 'application/pdf', 'attachment'],
    ],
  )
  assert.equal(await responses[2].text(), '0123456789')
})

test('every type but a PDF is served with no Content-Disposition', async () => {
  const { call } = fixture()
  const token = await ssoToken()
  for (const path of [
    PAGE,
    `${PAGE}screenshots/a.png`,
    `${PAGE}media/head-0.mp4`,
    `${PAGE}player.js`,
  ]) {
    for (const method of ['GET', 'HEAD']) {
      const response = await call(path, { method, token })
      assert.equal(response.status, 200)
      assert.equal(response.headers.get('Content-Disposition'), null, path)
    }
  }
})

test('the page path without a trailing slash redirects 301 to it', async () => {
  const { bucket, call } = fixture()
  const response = await call(PAGE.slice(0, -1), { token: await ssoToken() })
  assert.equal(response.status, 301)
  assert.ok(response.headers.get('Location').endsWith(PAGE))
  assert.equal(bucket.counts.get + bucket.counts.head, 0)
})

test('HEAD returns the same status and headers as GET with an empty body', async () => {
  const { call } = fixture()
  const token = await ssoToken()
  for (const path of [PAGE, `${PAGE}screenshots/a.png`, `${PAGE}nope.png`]) {
    const get = await call(path, { token })
    const head = await call(path, { method: 'HEAD', token })
    assert.equal(head.status, get.status)
    assert.deepEqual([...head.headers], [...get.headers])
    assert.equal(await head.text(), '')
  }
})

test('a page read with no token gets the gate, an asset 403, and the bucket is never touched', async () => {
  // Page reads have no Access edge in front of them, so this is any anonymous browser.
  const { bucket, call, jwks } = fixture()
  for (const method of ['GET', 'HEAD']) {
    for (const path of [PAGE, PAGE.slice(0, -1), PAGE_FILE]) {
      const response = await call(path, {
        headers: { 'Sec-Fetch-Dest': 'iframe' },
        method,
      })
      assert.equal(response.status, 200, `${method} ${path}`)
    }
    for (const path of [
      `${PAGE}screenshots/a.png`,
      `${PAGE}video.mp4`,
      `${PAGE}player.js`,
      `${PAGE}media/pdf-statement.pdf`,
      `${PAGE}media/index.html`,
      '/',
    ]) {
      const response = await call(path, { method })
      assert.equal(response.status, 403, `${method} ${path}`)
    }
  }
  await assertGate(await call(PAGE), 'no Sec-Fetch-Dest')
  const gate = await assertGate(await call(PAGE_FILE), 'index.html, framed')
  assert.ok(
    gate.includes(`${BASE}/_signin?next=${encodeURIComponent(PAGE_FILE)}`),
    'the gate signs in back to index.html',
  )
  assert.deepEqual(bucket.counts, { get: 0, head: 0, put: 0 })
  assert.deepEqual(jwks.calls, [])
})

test('a top-level tab with no token is sent to /_signin with the page as next', async () => {
  const { bucket, call } = fixture()
  const expected = {
    [PAGE]: SIGNIN_URL,
    [PAGE_FILE]: `${BASE}/_signin?next=${encodeURIComponent(PAGE_FILE)}`,
  }
  for (const [path, location] of Object.entries(expected)) {
    const response = await call(path, {
      headers: { 'Sec-Fetch-Dest': 'document' },
    })
    assert.equal(response.status, 302, path)
    assert.equal(response.headers.get('Location'), location, path)
  }
  assert.equal(bucket.counts.get + bucket.counts.head, 0)
})

test('a valid CF_Authorization cookie serves the page, its assets and ranges', async () => {
  const { call } = fixture()
  const cookie = await ssoToken()
  const page = await call(PAGE, { cookie })
  assert.equal(page.status, 200)
  assert.equal(await page.text(), '<h1>proof</h1>')
  const shot = await call(`${PAGE}screenshots/a.png`, { cookie })
  assert.equal(await shot.text(), 'PNGDATA')
  const range = await call(`${PAGE}video.mp4`, {
    cookie,
    headers: { Range: 'bytes=90-99' },
  })
  assert.equal(range.status, 206)
  assert.equal(await range.text(), '0123456789')
  const pdf = await call(`${PAGE}media/pdf-statement.pdf`, { cookie })
  assert.equal(pdf.headers.get('Content-Type'), 'application/pdf')
  assert.equal(pdf.headers.get('Content-Disposition'), 'attachment')
})

test('the cookie is found among other cookies, and a stale duplicate does not hide a valid one', async () => {
  const { call } = fixture()
  const stale = await ssoToken({ exp: NOW_SECONDS - 1 })
  const valid = await ssoToken()
  const response = await call(PAGE, {
    headers: {
      Cookie: `CF_AppSession=abc; CF_Authorization=${stale}; theme=dark; CF_Authorization=${valid}`,
    },
  })
  assert.equal(response.status, 200)
  assert.equal(await response.text(), '<h1>proof</h1>')
})

test('Range: bytes=0-9 on a 100-byte object returns 206 with 10 bytes', async () => {
  const { call } = fixture()
  const response = await call(`${PAGE}video.mp4`, {
    headers: { Range: 'bytes=0-9' },
    token: await ssoToken(),
  })
  assert.equal(response.status, 206)
  assert.equal(response.headers.get('Content-Range'), 'bytes 0-9/100')
  assert.equal(response.headers.get('Content-Length'), '10')
  assert.equal(await response.text(), 'xxxxxxxxxx')
})

test('open-ended and suffix ranges return the tail, and a multi-range is a plain 200', async () => {
  const { call } = fixture()
  const token = await ssoToken()
  const open = await call(`${PAGE}video.mp4`, {
    headers: { Range: 'bytes=90-' },
    token,
  })
  assert.equal(open.status, 206)
  assert.equal(open.headers.get('Content-Range'), 'bytes 90-99/100')
  assert.equal(await open.text(), '0123456789')

  const suffix = await call(`${PAGE}video.mp4`, {
    headers: { Range: 'bytes=-5' },
    token,
  })
  assert.equal(suffix.status, 206)
  assert.equal(await suffix.text(), '56789')

  const multi = await call(`${PAGE}video.mp4`, {
    headers: { Range: 'bytes=0-1,5-6' },
    token,
  })
  assert.equal(multi.status, 200)
  assert.equal((await multi.text()).length, 100)
})

test('a range past the end of the object returns 416', async () => {
  const { call } = fixture()
  const response = await call(`${PAGE}video.mp4`, {
    headers: { Range: 'bytes=100-200' },
    token: await ssoToken(),
  })
  assert.equal(response.status, 416)
  assert.equal(response.headers.get('Content-Range'), 'bytes */100')
})

test('tokens with a bad signature, audience, issuer, time, algorithm or identity get the gate', async () => {
  const { bucket, call, env } = fixture()
  const header = encode({ alg: 'none', kid: primary.kid, typ: 'JWT' })
  const cases = {
    'alg none': `${header}.${encode({ ...baseClaims, email: 'e' })}.`,
    'alg HS256 with the RS key id': await ssoToken({}, { header: { alg: 'HS256' } }),
    'email outside the allowed domain': await ssoToken({
      email: 'mallory@example.com.evil.com',
    }),
    'email with the domain as a prefix only': await ssoToken({
      email: 'mallory@evil.example.com',
    }),
    'exp in the past': await ssoToken({ exp: NOW_SECONDS - 1 }),
    'missing exp': await ssoToken({ exp: undefined }),
    'nbf in the future': await ssoToken({ nbf: NOW_SECONDS + 600 }),
    'neither an email nor the publisher': await sign({
      ...baseClaims,
      common_name: 'someone-else.access',
    }),
    'not a JWT': 'garbage',
    'undecodable signature': `${(await ssoToken()).split('.').slice(0, 2).join('.')}.a`,
    'signed by a different key': await ssoToken({}, { key: stranger }),
    'wrong aud': await ssoToken({ aud: ['some-other-app'] }),
    'wrong aud string': await ssoToken({ aud: 'some-other-app' }),
    'wrong iss': await ssoToken({ iss: 'https://evil.cloudflareaccess.com' }),
  }
  for (const [name, token] of Object.entries(cases)) {
    await assertGate(await call(PAGE, { token }), `header: ${name}`)
    await assertGate(await call(PAGE, { cookie: token }), `cookie: ${name}`)
    const asset = await call(`${PAGE}video.mp4`, { cookie: token })
    assert.equal(asset.status, 403, `asset: ${name}`)
  }
  assert.equal(bucket.counts.get + bucket.counts.head, 0)

  const upper = await call(PAGE, {
    cookie: await ssoToken({ email: 'Reviewer@Example.com' }),
  })
  assert.equal(upper.status, 200, 'the domain compares case-insensitively')
  delete env.ALLOWED_EMAIL_DOMAINS
  await assertGate(
    await call(PAGE, { cookie: await ssoToken() }),
    'no ALLOWED_EMAIL_DOMAINS binding',
  )
})

test('an aud claim given as a plain string is accepted', async () => {
  const { call } = fixture()
  const response = await call(PAGE, { token: await ssoToken({ aud: AUD }) })
  assert.equal(response.status, 200)
})

test('a Worker missing its Access bindings fails closed', async () => {
  const { call, env } = fixture()
  const token = await ssoToken()
  delete env.ACCESS_AUD
  await assertGate(await call(PAGE, { cookie: token }))
  assert.equal((await call('/_signin', { token })).status, 403, 'no sign-in redirect')
})

test('a failing JWKS fetch fails closed on reads, sign-in and publishing', async () => {
  for (const failure of [
    async () => {
      throw new Error('network down')
    },
    async () => new Response('oops', { status: 500 }),
    async () => new Response('not json', { status: 200 }),
  ]) {
    resetJwksCache()
    const { bucket, call, jwks } = fixture()
    jwks.fetchImpl = failure
    const sso = await ssoToken()
    await assertGate(await call(PAGE, { cookie: sso }))
    assert.equal((await call(`${PAGE}video.mp4`, { cookie: sso })).status, 403)
    assert.equal(
      (await call(`/_signin?next=${encodeURIComponent(PAGE)}`, { token: sso })).status,
      403,
    )
    const put = await call(`${PUBLISH}x.png`, {
      body: 'x',
      method: 'PUT',
      token: await serviceToken(),
    })
    assert.equal(put.status, 403)
    assert.deepEqual(bucket.counts, { get: 0, head: 0, put: 0 })
  }
})

test('an unknown kid triggers exactly one JWKS refetch, and the cached key serves the next request', async () => {
  const { call, jwks } = fixture()
  // Warm the cache with the primary key only.
  assert.equal((await call(PAGE, { token: await ssoToken() })).status, 200)
  assert.equal(jwks.calls.length, 1)
  assert.equal(jwks.calls[0], CERTS_URL)

  // Access rotates its signing key: the new kid is unknown to the cache.
  jwks.keys = [primary, rotated]
  const token = await ssoToken({}, { key: rotated })
  const later = NOW + JWKS_REFETCH_COOLDOWN_MS
  assert.equal((await call(PAGE, { now: later, token })).status, 200)
  assert.equal(jwks.calls.length, 2)

  assert.equal((await call(PAGE, { now: later, token })).status, 200)
  assert.equal(jwks.calls.length, 2)
})

test('requests that arrive while the first JWKS fetch is in flight share it and get 200', async () => {
  const { call, jwks } = fixture()
  const served = jwks.fetchImpl
  let release
  const gate = new Promise((resolve) => {
    release = resolve
  })
  jwks.fetchImpl = async (url) => {
    await gate
    return served(url)
  }
  const token = await ssoToken()
  const first = call(PAGE, { token })
  const second = call(PAGE, { token })
  // Let both requests reach the cold cache before the certs fetch resolves.
  await new Promise((resolve) => setTimeout(resolve, 20))
  release()
  const statuses = (await Promise.all([first, second])).map((r) => r.status)
  assert.deepEqual(statuses, [200, 200])
  assert.equal(jwks.calls.length, 1)
})

test('a kid the JWKS never publishes gets the gate and refetches at most once per cooldown', async () => {
  const { call, jwks } = fixture()
  const token = await ssoToken({}, { key: { ...rotated, kid: 'kid-unknown' } })
  await assertGate(await call(PAGE, { token }))
  await assertGate(await call(PAGE, { cookie: token }))
  assert.equal(jwks.calls.length, 1)
})

test('a service-token JWT on PUT stores the object with the request content type and returns 201', async () => {
  const { bucket, call } = fixture()
  const response = await call(`${PUBLISH}screenshots/new.png`, {
    body: 'NEWPNG',
    headers: { 'Content-Type': 'image/png' },
    method: 'PUT',
    token: await serviceToken(),
  })
  assert.equal(response.status, 201)
  const stored = bucket.store.get('acme/widgets/123/abcdef1/screenshots/new.png')
  assert.equal(stored.bytes.toString(), 'NEWPNG')
  assert.equal(stored.contentType, 'image/png')
})

test('a service-token JWT on PUT overwrites an existing object, so a re-publish is safe', async () => {
  const { bucket, call } = fixture()
  const response = await call(`${PUBLISH}index.html`, {
    body: '<h1>again</h1>',
    headers: { 'Content-Type': 'text/html; charset=utf-8' },
    method: 'PUT',
    token: await serviceToken(),
  })
  assert.equal(response.status, 201)
  assert.equal(
    bucket.store.get('acme/widgets/123/abcdef1/index.html').bytes.toString(),
    '<h1>again</h1>',
  )
})

test('an SSO JWT on PUT gets 403 and stores nothing', async () => {
  const { bucket, call } = fixture()
  const response = await call(`${PUBLISH}index.html`, {
    body: 'defaced',
    method: 'PUT',
    token: await ssoToken(),
  })
  assert.equal(response.status, 403)
  assert.equal(bucket.counts.put, 0)
})

test('a service-token JWT with a different common_name gets 403 on PUT', async () => {
  const { bucket, call } = fixture()
  const response = await call(`${PUBLISH}index.html`, {
    body: 'defaced',
    method: 'PUT',
    token: await serviceToken('someone-else.access'),
  })
  assert.equal(response.status, 403)
  assert.equal(bucket.counts.put, 0)
})

test('a PUT to the page path itself or its redirect form under /_publish is 404', async () => {
  const { bucket, call } = fixture()
  const token = await serviceToken()
  for (const path of [PUBLISH, PUBLISH.slice(0, -1), '/_publish/x']) {
    const response = await call(path, { body: 'x', method: 'PUT', token })
    assert.equal(response.status, 404, path)
  }
  assert.equal(bucket.counts.put, 0)
})

test('a PUT needs the publisher identity in the Access header, never a cookie or no token', async () => {
  const { bucket, call } = fixture()
  const attempts = {
    'no token': {},
    'publisher token as a cookie': { cookie: await serviceToken() },
    'SSO cookie': { cookie: await ssoToken() },
  }
  for (const [name, options] of Object.entries(attempts)) {
    const response = await call(`${PUBLISH}index.html`, {
      body: 'defaced',
      method: 'PUT',
      ...options,
    })
    assert.equal(response.status, 403, name)
  }
  assert.equal(bucket.counts.put, 0)
})

test('writes happen only under /_publish, and /_publish is never read', async () => {
  const { bucket, call } = fixture()
  const publisher = await serviceToken()
  const put = await call(`${PAGE}index.html`, {
    body: 'defaced',
    method: 'PUT',
    token: publisher,
  })
  assert.equal(put.status, 405)
  assert.equal(put.headers.get('Allow'), 'GET, HEAD')
  for (const options of [{ token: publisher }, { cookie: await ssoToken() }]) {
    const get = await call(`${PUBLISH}index.html`, options)
    assert.equal(get.status, 405)
    assert.equal(get.headers.get('Allow'), 'PUT')
  }
  assert.deepEqual(bucket.counts, { get: 0, head: 0, put: 0 })
})

test('the gate links /_signin for this page in a new tab and loads only its own script', async () => {
  const { call } = fixture()
  const body = await assertGate(await call(PAGE))
  const links = [...body.matchAll(/<a\b[^>]*>/g)].map(([tag]) => tag)
  assert.deepEqual(links, [
    `<a class="signin" href="${SIGNIN_URL}" target="_blank" rel="noopener noreferrer">`,
  ])
  assert.deepEqual(
    [...body.matchAll(/<script\b[^>]*>/g)].map(([tag]) => tag),
    ['<script src="/_gate.js" defer>'],
  )
  assert.ok(!/<script>/.test(body), 'no inline script')
  const origins = new Set([...body.matchAll(/https?:\/\/[^/"'\s<]+/g)].map(([origin]) => origin))
  assert.deepEqual([...origins], [BASE])
  const redirectForm = await assertGate(await call(PAGE.slice(0, -1)))
  assert.ok(
    redirectForm.includes(`href="${BASE}/_signin?next=${encodeURIComponent(PAGE.slice(0, -1))}"`),
  )
})

test('the gate is the same for a page that exists and one that does not', async () => {
  const { bucket, call } = fixture()
  const missing = '/acme/widgets/999/abcdef1/'
  const gate = (path) =>
    call(path).then(async (response) =>
      (await response.text()).replaceAll(encodeURIComponent(path), 'PATH'),
    )
  assert.equal(await gate(PAGE), await gate(missing))
  assert.equal(bucket.counts.get + bucket.counts.head, 0)
})

test('the gate script is served to anyone as a script', async () => {
  const { call } = fixture()
  const response = await call('/_gate.js')
  assert.equal(response.status, 200)
  assert.equal(response.headers.get('Content-Type'), 'text/javascript; charset=utf-8')
  assert.equal(await response.text(), GATE_SCRIPT)
})

function runGateScript() {
  const listeners = { document: {}, window: {} }
  const page = { reloads: 0, visibilityState: 'visible' }
  const target = (name) => ({
    addEventListener: (type, listener) => {
      listeners[name][type] = listener
    },
  })
  const document = {
    ...target('document'),
    get visibilityState() {
      return page.visibilityState
    },
  }
  const location = {
    reload: () => {
      page.reloads += 1
    },
  }
  new Function('document', 'window', 'location', GATE_SCRIPT)(document, target('window'), location)
  const fire = (name, type, visibilityState = page.visibilityState) => {
    page.visibilityState = visibilityState
    listeners[name][type]()
  }
  return { fire, page }
}

test('the gate script reloads when the reader comes back, not on the click that leaves', async () => {
  const focusFirst = runGateScript()
  focusFirst.fire('window', 'focus')
  assert.equal(focusFirst.page.reloads, 0, 'the click focusing the frame')

  const tabbed = runGateScript()
  tabbed.fire('document', 'visibilitychange', 'hidden')
  assert.equal(tabbed.page.reloads, 0, 'leaving for the sign-in tab')
  tabbed.fire('document', 'visibilitychange', 'visible')
  assert.equal(tabbed.page.reloads, 1, 'tab visible again')

  const sideBySide = runGateScript()
  sideBySide.fire('window', 'blur')
  sideBySide.fire('window', 'focus')
  assert.equal(sideBySide.page.reloads, 1, 'frame focused again')
})

test('/_signin sends a staff login on to its next page', async () => {
  const { call } = fixture()
  const token = await ssoToken()
  for (const next of [PAGE, PAGE.slice(0, -1), PAGE_FILE]) {
    const response = await call(`/_signin?next=${encodeURIComponent(next)}`, {
      token,
    })
    assert.equal(response.status, 302, next)
    assert.equal(response.headers.get('Location'), `${BASE}${next}`)
  }
  const bare = await call('/_signin', { token })
  assert.equal(bare.status, 200)
  assert.equal(bare.headers.get('Location'), null)
})

test('/_signin answers only a staff login in the Access header', async () => {
  const { call } = fixture()
  const path = `/_signin?next=${encodeURIComponent(PAGE)}`
  const attempts = {
    'no token': {},
    'publisher token': { token: await serviceToken() },
    'SSO cookie but no Access header': { cookie: await ssoToken() },
    'expired SSO token': { token: await ssoToken({ exp: NOW_SECONDS - 1 }) },
  }
  for (const [name, options] of Object.entries(attempts)) {
    const response = await call(path, options)
    assert.equal(response.status, 403, name)
    assert.equal(response.headers.get('Location'), null, name)
  }
})

test('dot-segment spellings of /_signin and /_publish still need the Access header', async () => {
  // The URL parser folds these into /_signin and /_publish, but Access may match the raw path.
  const { bucket, call } = fixture()
  const cookie = await ssoToken()
  const signin = await call(`/x/../_signin?next=${encodeURIComponent(PAGE)}`, {
    cookie,
  })
  assert.equal(signin.status, 403)
  const put = await call(`/./_publish/x/..${PAGE}index.html`, {
    body: 'defaced',
    cookie: await serviceToken(),
    method: 'PUT',
  })
  assert.equal(put.status, 403)
  assert.equal(bucket.counts.put, 0)
})

test('/_signin refuses any next that is not a proof page on this host', async () => {
  const { call } = fixture()
  const token = await ssoToken()
  const rejected = [
    '',
    'acme/widgets/123/abcdef1/',
    'https://evil.example/',
    'http://proof.example.com/acme/widgets/123/abcdef1/',
    `${BASE}${PAGE}`,
    '//evil.example/',
    '///evil.example/',
    '/\\evil.example/',
    '/\\/evil.example/',
    '/%2F%2Fevil.example/',
    '/%5Cevil.example/',
    '%2F%2Fevil.example',
    '/\t/evil.example/',
    '/\n/evil.example/',
    'javascript:alert(1)',
    `${PAGE}?x=1`,
    `${PAGE}#x`,
    `${PAGE}../../../`,
    `${PAGE}%2e%2e/`,
    '/acme/widgets/123/abcdef1/./',
    `${PAGE}index.html/`,
    `${PAGE}index.htm`,
    `${PAGE}index%2Ehtml`,
    `${PAGE}media/index.html`,
    `${PAGE}screenshots/a.png`,
    `${PAGE}player.js`,
    '/_signin?next=%2F%2Fevil.example',
    '/',
  ]
  for (const next of rejected) {
    const response = await call(`/_signin?next=${encodeURIComponent(next)}`, {
      token,
    })
    assert.equal(response.status, 400, JSON.stringify(next))
    assert.equal(response.headers.get('Location'), null, JSON.stringify(next))
    assert.equal(signinTarget(next, `${BASE}/_signin`), null)
  }
  const doubleEncoded = await call(
    `/_signin?next=${encodeURIComponent(encodeURIComponent('//evil.example/'))}`,
    { token },
  )
  assert.equal(doubleEncoded.status, 400)
})

test('paths outside the grammar are 404 with a valid JWT and never reach the bucket', async () => {
  const { bucket, call } = fixture()
  const token = await ssoToken()
  const paths = [
    '/',
    '/acme/widgets/123/',
    '/acme/widgets/0/abcdef1/',
    '/acme/widgets/01/abcdef1/',
    '/acme/widgets/12345678/abcdef1/',
    '/acme/widgets/123/ABCDEF1/',
    '/acme/widgets/123/abc/',
    `/acme/widgets/123/${'a'.repeat(41)}/`,
    '/ac%20me/widgets/123/abcdef1/',
    '/acme/../123/abcdef1/x',
    '/acme/widgets/123/abcdef1/../x',
    '/acme/widgets/123/abcdef1/%2e%2e/x',
    '/acme/widgets/123/abcdef1/a//b',
    '/acme/widgets/123/abcdef1/a/',
    '/acme/widgets/123/abcdef1/a%5Cb',
    '/acme/widgets/123/abcdef1/a%2Fb',
    '/acme/widgets/123/abcdef1/%E0%A4%A',
  ]
  for (const path of paths) {
    const response = await call(path, { token })
    assert.equal(response.status, 404, path)
  }
  assert.equal(bucket.counts.get + bucket.counts.head, 0)
})

test('routePath rejects dot, empty and backslash segments after percent-decoding', () => {
  // The URL parser already folds `..`, `%2e%2e` and backslashes out of request.url, so this checks the
  // grammar itself on raw paths the parser never normalised.
  const prefix = '/acme/widgets/123/abcdef1'
  for (const asset of ['..', '%2e%2e', '%2E', '.', 'a\\b', 'a%5cb', 'a%2fb', '', 'a/']) {
    assert.equal(routePath(`${prefix}/x/${asset}`), null, asset)
  }
  for (const segment of ['.', '..']) {
    assert.equal(routePath(`/${segment}/widgets/123/abcdef1/`), null, segment)
    assert.equal(routePath(`/acme/${segment}/123/abcdef1/`), null, segment)
  }
  assert.deepEqual(routePath(`${prefix}/screenshots/a%20b.png`), {
    key: 'acme/widgets/123/abcdef1/screenshots/a b.png',
    page: false,
  })
  for (const page of [`${prefix}/`, `${prefix}/index.html`]) {
    assert.deepEqual(routePath(page), {
      key: 'acme/widgets/123/abcdef1/index.html',
      page: true,
    })
  }
  assert.deepEqual(routePath(prefix), { redirect: `${prefix}/` })
})

test('parseRange handles the single-range forms against the object size', () => {
  assert.deepEqual(parseRange('bytes=0-9', 100), { end: 9, start: 0 })
  assert.deepEqual(parseRange('bytes=90-500', 100), { end: 99, start: 90 })
  assert.deepEqual(parseRange('bytes=-500', 100), { end: 99, start: 0 })
  assert.deepEqual(parseRange('bytes=100-', 100), { unsatisfiable: true })
  assert.deepEqual(parseRange('bytes=-0', 100), { unsatisfiable: true })
  assert.deepEqual(parseRange('bytes=0-', 0), { unsatisfiable: true })
  for (const ignored of [null, '', 'bytes=-', 'bytes=9-3', 'bytes=0-1,4-5', 'items=0-1']) {
    assert.equal(parseRange(ignored, 100), null, String(ignored))
  }
})

test('a raw backslash in the asset path never reaches the bucket as a backslash key', async () => {
  // The URL parser turns `\` into `/` before the Worker sees it, so `a\b` can only ever name
  // `a/b`; nothing with a backslash is ever looked up.
  const { bucket, call } = fixture()
  const response = await call('/acme/widgets/123/abcdef1/a\\b', {
    token: await ssoToken(),
  })
  assert.equal(response.status, 404)
  assert.ok(![...bucket.store.keys()].some((key) => key.includes('\\')))
})

test('DELETE with a valid JWT returns 405', async () => {
  const { bucket, call } = fixture()
  for (const token of [await ssoToken(), await serviceToken()]) {
    const response = await call(`${PAGE}index.html`, {
      method: 'DELETE',
      token,
    })
    assert.equal(response.status, 405)
  }
  assert.ok(bucket.store.has('acme/widgets/123/abcdef1/index.html'))
})

test('every response carries the security headers, errors included', async () => {
  const { call } = fixture()
  const sso = await ssoToken()
  const responses = {
    200: await call(PAGE, { token: sso }),
    '200 gate': await call(PAGE),
    201: await call(`${PUBLISH}a.txt`, {
      body: 'a',
      method: 'PUT',
      token: await serviceToken(),
    }),
    206: await call(`${PAGE}video.mp4`, {
      headers: { Range: 'bytes=0-9' },
      token: sso,
    }),
    301: await call(PAGE.slice(0, -1), { token: sso }),
    302: await call(`/_signin?next=${encodeURIComponent(PAGE)}`, {
      token: sso,
    }),
    400: await call('/_signin?next=%2F%2Fevil.example', { token: sso }),
    403: await call(`${PAGE}video.mp4`),
    404: await call('/nope', { token: sso }),
    405: await call(PAGE, { method: 'DELETE', token: sso }),
    416: await call(`${PAGE}video.mp4`, {
      headers: { Range: 'bytes=500-' },
      token: sso,
    }),
  }
  for (const [status, response] of Object.entries(responses)) {
    assert.equal(response.status, Number.parseInt(status, 10))
    assertSecurityHeaders(response)
  }
})

test('an unexpected bucket failure is a 500 that still carries the security headers', async () => {
  const { call, env } = fixture()
  env.PROOF_BUCKET.head = async () => {
    throw new Error('r2 down')
  }
  const response = await call(PAGE, { token: await ssoToken() })
  assert.equal(response.status, 500)
})

test('a configured FRAME_ANCESTORS list frames pages, the gate and errors verbatim', async () => {
  const origins = 'https://review.example.com https://localhost:8443'
  const { call } = fixture({ ancestors: origins, bindings: { FRAME_ANCESTORS: origins } })
  const token = await ssoToken()
  const statuses = [
    (await call(PAGE, { token })).status,
    (await call(PAGE)).status,
    (await call('/_gate.js')).status,
    (await call(`${PAGE}video.mp4`)).status,
    (await call('/nope', { token })).status,
    (await call(PAGE, { method: 'DELETE', token })).status,
  ]
  assert.deepEqual(statuses, [200, 200, 200, 403, 404, 405])
})

test('an empty FRAME_ANCESTORS, or any token that is not an https origin, frames from nowhere', async () => {
  const bindings = [
    undefined,
    '',
    '   ',
    'http://app.example.com',
    'https://app.example.com/path',
    'https://app.example.com/',
    'https://APP.example.com',
    'https://ok.example.com *',
    "https://ok.example.com 'self'",
    'https://ok.example.com;script-src *',
    'https://ok.example.com:port',
  ]
  for (const binding of bindings) {
    const { call } = fixture({ ancestors: "'none'", bindings: { FRAME_ANCESTORS: binding } })
    const token = await ssoToken()
    assert.equal((await call(PAGE, { token })).status, 200, String(binding))
    assert.equal((await call(PAGE)).status, 200, String(binding))
    assert.equal((await call('/nope', { token })).status, 404, String(binding))
    assert.match(contentSecurityPolicy(binding), /frame-ancestors 'none';/, String(binding))
  }
})

test("script-src 'self' is on pages, the gate, its script and every error", async () => {
  const { call, env } = fixture()
  const token = await ssoToken()
  const responses = [
    await call(PAGE, { token }),
    await call(PAGE),
    await call('/_gate.js'),
    await call(`${PAGE}video.mp4`),
    await call('/nope', { token }),
  ]
  env.PROOF_BUCKET.head = async () => {
    throw new Error('r2 down')
  }
  responses.push(await call(PAGE, { token }))
  assert.deepEqual(
    responses.map((response) => [
      response.status,
      directives(response.headers.get('Content-Security-Policy'))['script-src'],
    ]),
    [
      [200, "'self'"],
      [200, "'self'"],
      [200, "'self'"],
      [403, "'self'"],
      [404, "'self'"],
      [500, "'self'"],
    ],
  )
})

test('staff may sign in from any listed email domain, compared case-insensitively', async () => {
  const { call } = fixture({
    bindings: { ALLOWED_EMAIL_DOMAINS: 'example.com, Example.ORG ,partner.dev' },
  })
  for (const email of ['a@example.com', 'b@example.org', 'c@PARTNER.dev']) {
    const cookie = await ssoToken({ email })
    const page = await call(PAGE, { cookie })
    assert.equal(page.status, 200, email)
    assert.equal(await page.text(), '<h1>proof</h1>', email)
    const signin = await call(`/_signin?next=${encodeURIComponent(PAGE)}`, {
      token: await ssoToken({ email }),
    })
    assert.equal(signin.status, 302, email)
  }
  for (const email of [
    'd@other.com',
    'e@example.com.evil.com',
    'f@sub.example.org',
    'g@partner.dev.evil.com',
    'partner.dev',
    '@example.com',
  ]) {
    await assertGate(await call(PAGE, { cookie: await ssoToken({ email }) }), email)
    const signin = await call(`/_signin?next=${encodeURIComponent(PAGE)}`, {
      token: await ssoToken({ email }),
    })
    assert.equal(signin.status, 403, email)
  }
})

test('an empty ALLOWED_EMAIL_DOMAINS list admits no staff', async () => {
  for (const binding of ['', ' , ', ',']) {
    const { bucket, call } = fixture({ bindings: { ALLOWED_EMAIL_DOMAINS: binding } })
    await assertGate(await call(PAGE, { cookie: await ssoToken() }), JSON.stringify(binding))
    assert.equal(bucket.counts.get + bucket.counts.head, 0)
  }
})

function sweepBucket(pages) {
  return {
    deleted: [],
    lists: [],
    async delete(keys) {
      this.deleted.push(...keys)
    },
    async list(options) {
      this.lists.push(options)
      return pages(options, this.lists.length)
    },
  }
}

const DAY = 86_400_000
const sweepEnv = (bucket, retention = '90') => ({ PROOF_BUCKET: bucket, RETENTION_DAYS: retention })

test('the sweep deletes objects older than the retention window across cursor pages', async () => {
  const old = new Date(NOW - 91 * DAY)
  const recent = new Date(NOW - 89 * DAY)
  const cutoff = new Date(NOW - 90 * DAY)
  const bucket = sweepBucket((options) =>
    options.cursor
      ? {
          objects: [
            { key: 'acme/widgets/1/abcdef1/index.html', uploaded: old },
            { key: 'acme/widgets/2/abcdef2/index.html', uploaded: recent },
          ],
          truncated: false,
        }
      : {
          cursor: 'page-2',
          objects: [
            { key: 'acme/widgets/123/abcdef1/index.html', uploaded: old },
            { key: 'acme/widgets/123/abcdef1/player.js', uploaded: old },
            { key: 'acme/widgets/124/abcdef9/index.html', uploaded: recent },
            { key: 'acme/widgets/125/abcdef8/index.html', uploaded: cutoff },
          ],
          truncated: true,
        },
  )
  await proofHost.scheduled({ scheduledTime: NOW }, sweepEnv(bucket))
  assert.deepEqual(bucket.deleted, [
    'acme/widgets/123/abcdef1/index.html',
    'acme/widgets/123/abcdef1/player.js',
    'acme/widgets/1/abcdef1/index.html',
  ])
  assert.deepEqual(bucket.lists, [
    { cursor: undefined, limit: SWEEP_PAGE_SIZE },
    { cursor: 'page-2', limit: SWEEP_PAGE_SIZE },
  ])
})

test('a page with nothing expired deletes nothing', async () => {
  const bucket = sweepBucket(() => ({
    objects: [{ key: 'acme/widgets/1/abcdef1/index.html', uploaded: new Date(NOW - DAY) }],
    truncated: false,
  }))
  await sweep({ scheduledTime: NOW }, sweepEnv(bucket, 7))
  assert.deepEqual(bucket.deleted, [])
  assert.equal(bucket.lists.length, 1)
})

test('the sweep stops at its page budget on a bucket that never ends', async () => {
  const bucket = sweepBucket((_options, count) => ({
    cursor: String(count),
    objects: [{ key: `acme/widgets/${count}/abcdef1/index.html`, uploaded: new Date(0) }],
    truncated: true,
  }))
  await sweep({ scheduledTime: NOW }, sweepEnv(bucket))
  assert.equal(bucket.lists.length, SWEEP_PAGE_BUDGET)
  assert.equal(bucket.deleted.length, SWEEP_PAGE_BUDGET)
})

test('a missing, non-integer or non-positive RETENTION_DAYS sweeps nothing', async () => {
  for (const retention of [undefined, '', ' ', '0', '-1', '1.5', 'abc', 'Infinity', 'NaN']) {
    const bucket = sweepBucket(() => ({
      objects: [{ key: 'acme/widgets/1/abcdef1/index.html', uploaded: new Date(0) }],
      truncated: false,
    }))
    await sweep({ scheduledTime: NOW }, { PROOF_BUCKET: bucket, RETENTION_DAYS: retention })
    assert.deepEqual(bucket.lists, [], String(retention))
    assert.deepEqual(bucket.deleted, [], String(retention))
  }
})
