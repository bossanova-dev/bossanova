import assert from 'node:assert/strict'
import { before, beforeEach, test } from 'node:test'
import { webcrypto } from 'node:crypto'

import * as published from '../../terraform/modules/proof-host/worker.mjs'
import worker, {
  GATE_SCRIPT,
  JWKS_REFETCH_COOLDOWN_MS,
  gatePage,
  handleRequest,
  resetJwksCache,
  signinTarget,
} from '../../infra/modules/cf-proof-host/worker.mjs'

const BASE = 'https://proof.bossanova.dev'
const sha = 'a'.repeat(40)
const prefix = `owner/repo/7/${sha}/`
const PAGE_FILE = `/${prefix}index.html`
const EMAIL = 'reviewer@example.com'
const CERTS = 'https://proof-team.cloudflareaccess.com/cdn-cgi/access/certs'
const CSP =
  "default-src 'none'; script-src 'self'; img-src 'self'; media-src 'self'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors https://app.bossanova.dev https://app-staging.bossanova.dev"
const encode = (value) => Buffer.from(JSON.stringify(value)).toString('base64url')
const signinFor = (path) => `${BASE}/_signin?next=${encodeURIComponent(path)}`

let pair, jwk
let jwksCalls = 0
let clock = Date.now()
const deps = {
  fetchImpl: async (url) => {
    assert.equal(url, CERTS)
    jwksCalls++
    return Response.json({ keys: [jwk] })
  },
  now: () => clock,
}

before(async () => {
  pair = await webcrypto.subtle.generateKey(
    {
      name: 'RSASSA-PKCS1-v1_5',
      modulusLength: 2048,
      publicExponent: new Uint8Array([1, 0, 1]),
      hash: 'SHA-256',
    },
    true,
    ['sign', 'verify'],
  )
  jwk = {
    ...(await webcrypto.subtle.exportKey('jwk', pair.publicKey)),
    kid: 'test-key',
    alg: 'RS256',
  }
})
beforeEach(() => {
  resetJwksCache()
  jwksCalls = 0
  clock = Date.now()
})

async function token(claims = {}, header = {}) {
  const now = Math.floor(clock / 1000)
  const message = `${encode({ alg: 'RS256', kid: jwk.kid, ...header })}.${encode({ aud: ['proof-aud'], iss: 'https://proof-team.cloudflareaccess.com', exp: now + 300, email: EMAIL, ...claims })}`
  const signature = await webcrypto.subtle.sign(
    'RSASSA-PKCS1-v1_5',
    pair.privateKey,
    new TextEncoder().encode(message),
  )
  return `${message}.${Buffer.from(signature).toString('base64url')}`
}
function bucket(entries = {}) {
  return {
    gets: [],
    lists: [],
    deleted: [],
    async get(key, options) {
      this.gets.push(key)
      const content = entries[key]
      if (content === undefined) return null
      const bytes = new TextEncoder().encode(content)
      const metadata = {
        size: bytes.length,
        httpEtag: '"etag"',
        writeHttpMetadata(headers) {
          headers.set('Content-Type', key.endsWith('.html') ? 'text/html' : 'video/mp4')
        },
      }
      if (options.onlyIf.get('If-None-Match') === metadata.httpEtag) return metadata
      const ifMatch = options.onlyIf.get('If-Match')
      if (ifMatch && ifMatch !== '*' && ifMatch !== metadata.httpEtag) return metadata
      const match = /^bytes=(\d+)-(\d+)$/.exec(options.range.get('Range') ?? '')
      if (match) {
        const offset = Number(match[1]),
          length = Number(match[2]) - offset + 1
        return {
          ...metadata,
          body: bytes.slice(offset, offset + length),
          range: { offset, length },
        }
      }
      return { ...metadata, body: bytes }
    },
    async list(options) {
      this.lists.push(options)
      return { objects: [], truncated: false }
    },
    async delete(keys) {
      this.deleted.push(...keys)
    },
  }
}
function env(b = bucket()) {
  return {
    PROOF_BUCKET: b,
    ACCESS_AUD: 'proof-aud',
    ACCESS_TEAM_DOMAIN: 'proof-team.cloudflareaccess.com',
    ALLOWED_EMAILS: `someone@example.org, ${EMAIL.toUpperCase()}`,
    RETENTION_DAYS: '90',
  }
}
function request(path, { jwt, cookie, headers = {}, method = 'GET' } = {}) {
  return new Request(`${BASE}/${path.replace(/^\//, '')}`, {
    method,
    headers: {
      ...(jwt ? { 'Cf-Access-Jwt-Assertion': jwt } : {}),
      ...(cookie ? { Cookie: `CF_Authorization=${cookie}` } : {}),
      ...headers,
    },
  })
}
const call = (path, options, e = env()) => handleRequest(request(path, options), e, deps)
const framed = { 'Sec-Fetch-Dest': 'iframe' }
const tab = { 'Sec-Fetch-Dest': 'document' }

function secure(response, cache = 'private, no-store') {
  assert.equal(response.headers.get('X-Robots-Tag'), 'noindex, nofollow')
  assert.equal(response.headers.get('X-Content-Type-Options'), 'nosniff')
  assert.equal(response.headers.get('Referrer-Policy'), 'no-referrer')
  assert.equal(response.headers.get('Cache-Control'), cache)
  assert.equal(response.headers.get('Content-Security-Policy'), CSP)
}
async function assertGate(response, path) {
  assert.equal(response.status, 200)
  assert.equal(response.headers.get('Content-Type'), 'text/html; charset=utf-8')
  secure(response)
  const body = await response.text()
  assert.equal(body, gatePage(signinFor(path)))
  return body
}

test('a framed page read with no session gets the frameable gate and never reads R2', async () => {
  const b = bucket({ [`${prefix}index.html`]: '<html>proof</html>' })
  for (const path of [PAGE_FILE, `/${prefix}`]) {
    const body = await assertGate(await call(path, { headers: framed }, env(b)), path)
    assert.match(body, /<h1>Sign in to review this PR<\/h1>/)
    const links = [...body.matchAll(/<a\b[^>]*>/g)].map(([tag]) => tag)
    assert.deepEqual(links, [
      `<a class="signin" href="${signinFor(path)}" target="_blank" rel="noopener noreferrer">`,
    ])
    assert.deepEqual(
      [...body.matchAll(/<script\b[^>]*>/g)].map(([tag]) => tag),
      ['<script src="/_gate.js" defer>'],
    )
  }
  // A request with no Sec-Fetch-Dest (an older browser, curl) is treated as framed.
  await assertGate(await call(PAGE_FILE, {}, env(b)), PAGE_FILE)
  const head = await call(PAGE_FILE, { headers: framed, method: 'HEAD' }, env(b))
  assert.equal(head.status, 200)
  assert.equal(await head.text(), '')
  assert.deepEqual(b.gets, [])
})

test('the gate script is served unauthenticated as JavaScript the CSP admits', async () => {
  const response = await call('/_gate.js')
  assert.equal(response.status, 200)
  assert.equal(response.headers.get('Content-Type'), 'text/javascript; charset=utf-8')
  secure(response)
  assert.equal(await response.text(), GATE_SCRIPT)
})

test('the gate and its script match the published proof-host module', () => {
  const url = `${signinFor(PAGE_FILE)}&x="<'>`
  assert.equal(gatePage(url), published.gatePage(url))
  assert.equal(GATE_SCRIPT, published.GATE_SCRIPT)
})

test('a top-level tab with no session is redirected to /_signin with its path as next', async () => {
  const b = bucket()
  for (const path of [
    PAGE_FILE,
    `/${prefix}`,
    `/${prefix}video.mp4`,
    '/plans/bossanova/BOS-7/a.md',
  ]) {
    const response = await call(path, { headers: tab }, env(b))
    assert.equal(response.status, 302, path)
    assert.equal(response.headers.get('Location'), signinFor(path), path)
    secure(response)
  }
  assert.deepEqual(b.gets, [])
})

test('a signed-out asset or unreadable path is refused without reading R2', async () => {
  const b = bucket()
  for (const [path, headers] of [
    [`/${prefix}video.mp4`, framed],
    [`/${prefix}player.js`, {}],
    ['/plans/legacy.html', framed],
    ['/', tab],
    ['/a//b', tab],
    ['/_signinx', tab],
    ['/%zz', framed],
  ]) {
    const response = await call(path, { headers }, env(b))
    assert.equal(response.status, 403, path)
    secure(response)
  }
  assert.deepEqual(b.gets, [])
})

test('a valid Access JWT in the header or the cookie serves the page', async () => {
  const jwt = await token()
  const b = bucket({ [`${prefix}index.html`]: '<html>proof</html>' })
  for (const options of [{ jwt }, { cookie: jwt }]) {
    const response = await call(PAGE_FILE, { ...options, headers: framed }, env(b))
    assert.equal(response.status, 200)
    assert.equal(await response.text(), '<html>proof</html>')
    assert.equal(response.headers.get('Content-Type'), 'text/html')
    secure(response, 'private, max-age=300')
  }
  const stale = await token({ exp: Math.floor(clock / 1000) - 1 })
  const mixed = await call(
    PAGE_FILE,
    { headers: { Cookie: `CF_AppSession=x; CF_Authorization=${stale}; CF_Authorization=${jwt}` } },
    env(b),
  )
  assert.equal(mixed.status, 200, 'a stale duplicate cookie does not hide a valid one')
})

test('a forged, expired or otherwise invalid JWT is treated as signed out', async () => {
  const good = await token()
  const badSignature =
    good.slice(0, good.lastIndexOf('.') + 1) + Buffer.alloc(256).toString('base64url')
  const now = Math.floor(clock / 1000)
  for (const jwt of [
    'invalid',
    badSignature,
    await token({}, { alg: 'HS256' }),
    await token({}, { kid: 'unknown' }),
    await token({ exp: now - 1 }),
    await token({ exp: undefined }),
    await token({ aud: ['other'] }),
    await token({ iss: 'https://other' }),
    await token({ nbf: now + 300 }),
    await token({ email: 'stranger@example.com' }),
    await token({ email: undefined }),
  ]) {
    const b = bucket({ [`${prefix}index.html`]: 'proof' })
    for (const options of [{ jwt }, { cookie: jwt }]) {
      await assertGate(await call(PAGE_FILE, { ...options, headers: framed }, env(b)), PAGE_FILE)
      const asset = await call(`/${prefix}video.mp4`, options, env(b))
      assert.equal(asset.status, 403)
    }
    assert.deepEqual(b.gets, [])
  }
  for (const binding of ['ACCESS_AUD', 'ACCESS_TEAM_DOMAIN', 'ALLOWED_EMAILS']) {
    const e = env(bucket({ [`${prefix}index.html`]: 'proof' }))
    e[binding] = ''
    await assertGate(await call(PAGE_FILE, { jwt: good, headers: framed }, e), PAGE_FILE)
    assert.deepEqual(e.PROOF_BUCKET.gets, [])
  }
})

test('unknown kids refetch the JWKS at most once per cooldown window', async () => {
  for (let i = 0; i < 5; i++) {
    const jwt = await token({}, { kid: `forged-${i}` })
    assert.equal((await call(`/${prefix}x`, { jwt })).status, 403)
  }
  assert.equal(jwksCalls, 1)
  clock += JWKS_REFETCH_COOLDOWN_MS
  assert.equal((await call(`/${prefix}x`, { jwt: await token({}, { kid: 'forged' }) })).status, 403)
  assert.equal(jwksCalls, 2)
})

test('/_signin sends an allowed login in the Access header on to next', async () => {
  const jwt = await token()
  for (const next of [PAGE_FILE, `/${prefix}`, '/plans/bossanova/BOS-7/a.md']) {
    const response = await call(`/_signin?next=${encodeURIComponent(next)}`, { jwt })
    assert.equal(response.status, 302, next)
    assert.equal(response.headers.get('Location'), `${BASE}${next}`)
    secure(response)
  }
  const bare = await call('/_signin', { jwt })
  assert.equal(bare.status, 200)
  assert.equal(await bare.text(), 'Signed in. You can close this tab.')
  for (const options of [{}, { cookie: jwt }, { jwt: await token({ email: 'x@example.com' }) }]) {
    const response = await call(`/_signin?next=${encodeURIComponent(PAGE_FILE)}`, options)
    assert.equal(response.status, 403)
    assert.equal(response.headers.get('Location'), null)
  }
})

test('/_signin never redirects off this host or to its own paths', async () => {
  const jwt = await token()
  for (const next of [
    '',
    'owner/repo/7/x',
    'https://evil.example/',
    `${BASE}${PAGE_FILE}`,
    'http://proof.bossanova.dev/x',
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
    `${PAGE_FILE}?x=1`,
    `${PAGE_FILE}#x`,
    `/${prefix}../../../`,
    `/${prefix}%2e%2e/`,
    '/owner/repo/7/./x',
    '/_signin',
    '/_gate.js',
    '/',
  ]) {
    const response = await call(`/_signin?next=${encodeURIComponent(next)}`, { jwt })
    assert.equal(response.status, 400, JSON.stringify(next))
    assert.equal(response.headers.get('Location'), null, JSON.stringify(next))
    assert.equal(signinTarget(next, `${BASE}/_signin`), null, JSON.stringify(next))
  }
  const doubleEncoded = await call(
    `/_signin?next=${encodeURIComponent(encodeURIComponent('//evil.example/'))}`,
    { jwt },
  )
  assert.equal(doubleEncoded.status, 400)
})

test('signed-in exact objects, HEAD, ranges and conditional reads keep their headers', async () => {
  const jwt = await token(),
    key = `${prefix}video.mp4`,
    b = bucket({ [key]: '0123456789abcdefghij' }),
    e = env(b)
  let response = await call(key, { jwt }, e)
  assert.equal(response.status, 200)
  assert.equal(await response.text(), '0123456789abcdefghij')
  assert.equal(response.headers.get('Content-Type'), 'video/mp4')
  secure(response, 'private, max-age=300')
  response = await call(key, { cookie: jwt, headers: { Range: 'bytes=0-9' } }, e)
  assert.equal(response.status, 206)
  assert.equal(response.headers.get('Content-Range'), 'bytes 0-9/20')
  assert.equal(await response.text(), '0123456789')
  secure(response, 'private, max-age=300')
  response = await call(key, { jwt, method: 'HEAD' }, e)
  assert.equal(response.status, 200)
  assert.equal(response.headers.get('Content-Length'), '20')
  assert.equal(await response.text(), '')
  for (const [headers, status] of [
    [{ 'If-None-Match': '"etag"' }, 304],
    [{ 'If-Match': '"other"' }, 412],
    [{ 'If-Match': '"etag"', 'If-None-Match': '"etag"' }, 304],
  ]) {
    response = await call(key, { jwt, headers }, e)
    assert.equal(response.status, status)
    secure(response, 'private, max-age=300')
  }
  assert.deepEqual(b.lists, [])
})

test('only full proof prefixes map to index.html; unsafe keys and directories never list', async () => {
  const jwt = await token(),
    b = bucket({ [`${prefix}index.html`]: '<html>proof</html>', 'plans/legacy.html': 'plan' }),
    e = env(b)
  let response = await call(prefix, { jwt }, e)
  assert.equal(response.status, 200)
  assert.equal(response.headers.get('Content-Type'), 'text/html')
  for (const path of [
    '',
    'owner/',
    'owner/repo/',
    'owner/repo/7/',
    'a/%2e%2e%2fb',
    'a//b',
    'a/%5cb',
    '%zz',
    'missing',
  ]) {
    response = await call(path, { jwt }, e)
    assert.equal(response.status, 404, path)
    secure(response, 'private, max-age=300')
  }
  assert.equal((await call('plans/legacy.html', { jwt }, e)).status, 200)
  response = await call(prefix, { jwt, method: 'POST' }, e)
  assert.equal(response.status, 405)
  secure(response)
  assert.deepEqual(b.lists, [])
})

test('a bucket error answers 500 with the security headers', async () => {
  const b = bucket()
  b.get = async () => {
    throw new Error('r2 down')
  }
  const response = await call(PAGE_FILE, { jwt: await token() }, env(b))
  assert.equal(response.status, 500)
  secure(response)
})

test('the default export serves through handleRequest', async () => {
  const response = await worker.fetch(request('/_gate.js'), env())
  assert.equal(response.status, 200)
  assert.equal(await response.text(), GATE_SCRIPT)
})

test('daily sweep follows cursors, deletes old evidence, and exempts plans and recent objects', async () => {
  const now = Date.now(),
    old = new Date(now - 91 * 86400000),
    recent = new Date(now - 89 * 86400000),
    b = bucket()
  b.list = async function (options) {
    this.lists.push(options)
    return options.cursor
      ? { objects: [{ key: 'other/old', uploaded: old }], truncated: false }
      : {
          objects: [
            { key: 'evidence/old', uploaded: old },
            { key: 'evidence/new', uploaded: recent },
            { key: 'plans/history', uploaded: old },
          ],
          truncated: true,
          cursor: 'next',
        }
  }
  await worker.scheduled({ scheduledTime: now }, env(b))
  assert.deepEqual(b.deleted, ['evidence/old', 'other/old'])
  assert.equal(b.lists[1].cursor, 'next')
})

test('invalid retention is fail safe and every sweep has a finite page budget', async () => {
  for (const retention of [undefined, '0', 'abc', '1.5', '-1']) {
    const b = bucket()
    await worker.scheduled({ scheduledTime: Date.now() }, { ...env(b), RETENTION_DAYS: retention })
    assert.deepEqual(b.lists, [])
    assert.deepEqual(b.deleted, [])
  }
  const b = bucket()
  b.list = async function (options) {
    this.lists.push(options)
    return { objects: [], truncated: true, cursor: String(this.lists.length) }
  }
  await worker.scheduled({ scheduledTime: Date.now() }, env(b))
  assert.ok(b.lists.length > 0 && b.lists.length <= 100)
})
