import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import { webcrypto } from 'node:crypto'

let worker, pair, jwk
const originalFetch = globalThis.fetch
const sha = 'a'.repeat(40)
const prefix = `owner/repo/7/${sha}/`
const encode = (value) => Buffer.from(JSON.stringify(value)).toString('base64url')
let jwksCalls = 0
before(async () => {
  worker = (await import('../../infra/modules/cf-proof-host/worker.mjs')).default
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
  globalThis.fetch = async (url) => {
    assert.equal(url, 'https://proof-team.cloudflareaccess.com/cdn-cgi/access/certs')
    jwksCalls++
    return Response.json({ keys: [jwk] })
  }
})
after(() => {
  globalThis.fetch = originalFetch
})
async function token(claims = {}, header = {}) {
  const now = Math.floor(Date.now() / 1000)
  const message = `${encode({ alg: 'RS256', kid: jwk.kid, ...header })}.${encode({ aud: ['proof-aud'], iss: 'https://proof-team.cloudflareaccess.com', exp: now + 300, ...claims })}`
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
    RETENTION_DAYS: '90',
  }
}
async function request(path, jwt, headers = {}, method = 'GET') {
  return new Request(`https://proof.bossanova.dev/${path}`, {
    method,
    headers: { ...(jwt ? { 'Cf-Access-Jwt-Assertion': jwt } : {}), ...headers },
  })
}
function secure(response) {
  assert.equal(response.headers.get('X-Robots-Tag'), 'noindex, nofollow')
  assert.equal(response.headers.get('X-Content-Type-Options'), 'nosniff')
  assert.equal(response.headers.get('Referrer-Policy'), 'no-referrer')
  assert.equal(response.headers.get('Cache-Control'), 'private, max-age=300')
  assert.equal(
    response.headers.get('Content-Security-Policy'),
    "default-src 'none'; img-src 'self'; media-src 'self'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
  )
}
test('Access rejects missing, malformed, bad signature, algorithm, unknown kid and invalid claims without reading R2', async () => {
  const good = await token()
  const badSignature =
    good.slice(0, good.lastIndexOf('.') + 1) + Buffer.alloc(256).toString('base64url')
  for (const jwt of [
    null,
    'invalid',
    badSignature,
    await token({}, { alg: 'HS256' }),
    await token({}, { kid: 'unknown' }),
    await token({ exp: 0 }),
    await token({ aud: ['other'] }),
    await token({ iss: 'https://other' }),
    await token({ nbf: Date.now() / 1000 + 300 }),
  ]) {
    const b = bucket()
    const response = await worker.fetch(await request(`${prefix}video.mp4`, jwt), env(b))
    assert.equal(response.status, 403)
    secure(response)
    assert.deepEqual(b.gets, [])
  }
  for (const binding of ['ACCESS_AUD', 'ACCESS_TEAM_DOMAIN']) {
    const e = env()
    e[binding] = ''
    assert.equal((await worker.fetch(await request('x', good), e)).status, 403)
    assert.deepEqual(e.PROOF_BUCKET.gets, [])
  }
  assert.ok(jwksCalls >= 2, 'unknown kid refetches JWKS')
})
test('authenticated exact objects, HEAD, ranges and conditional reads preserve security headers', async () => {
  const jwt = await token(),
    key = `${prefix}video.mp4`,
    b = bucket({ [key]: '0123456789abcdefghij' }),
    e = env(b)
  let response = await worker.fetch(await request(key, jwt), e)
  assert.equal(response.status, 200)
  assert.equal(await response.text(), '0123456789abcdefghij')
  assert.equal(response.headers.get('Content-Type'), 'video/mp4')
  secure(response)
  response = await worker.fetch(await request(key, jwt, { Range: 'bytes=0-9' }), e)
  assert.equal(response.status, 206)
  assert.equal(response.headers.get('Content-Range'), 'bytes 0-9/20')
  assert.equal(await response.text(), '0123456789')
  secure(response)
  response = await worker.fetch(await request(key, jwt, {}, 'HEAD'), e)
  assert.equal(response.status, 200)
  assert.equal(await response.text(), '')
  for (const [headers, status] of [
    [{ 'If-None-Match': '"etag"' }, 304],
    [{ 'If-Match': '"other"' }, 412],
    [{ 'If-Match': '"etag"', 'If-None-Match': '"etag"' }, 304],
  ]) {
    response = await worker.fetch(await request(key, jwt, headers), e)
    assert.equal(response.status, status)
    secure(response)
  }
  assert.deepEqual(b.lists, [])
})
test('only full proof prefixes map to index.html; unsafe keys and directories never list', async () => {
  const jwt = await token(),
    b = bucket({ [`${prefix}index.html`]: '<html>proof</html>', 'plans/legacy.html': 'plan' }),
    e = env(b)
  let response = await worker.fetch(await request(prefix, jwt), e)
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
    response = await worker.fetch(await request(path, jwt), e)
    assert.equal(response.status, 404, path)
    secure(response)
  }
  assert.equal((await worker.fetch(await request('plans/legacy.html', jwt), e)).status, 200)
  response = await worker.fetch(await request(prefix, jwt, {}, 'POST'), e)
  assert.equal(response.status, 405)
  secure(response)
  assert.deepEqual(b.lists, [])
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
