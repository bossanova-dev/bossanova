// Behaviour tests for the untrusted proof host Worker (infra/modules/cf-user-uploads/worker.mjs).
// URLs are signed here with node:crypto over the documented message, independently of the Worker,
// so these tests pin the view-signature contract bosso mints against (BOS-2604).
import assert from 'node:assert/strict'
import { createHmac } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import worker, {
  contentSecurityPolicy,
  handleRequest,
} from '../../infra/modules/cf-user-uploads/worker.mjs'

const DOMAIN = 'bossanova-user-uploads.com'
const KEY = 'ab'.repeat(32)
const STAGING_KEY = '0f'.repeat(40)
const TOKEN = 'Q2xpcFRva2VuMTIzNDU2Nzg'
const OTHER_TOKEN = 'b3RoZXJQYWdlVG9rZW4xMjM'
const NOW = 1_800_000_000_000
const NOW_S = NOW / 1000
const WEEK_S = 7 * 24 * 60 * 60
const PRODUCTION_ANCESTORS = 'https://app.bossanova.dev https://app-staging.bossanova.dev'
const STAGING_ANCESTORS = 'https://app-staging.bossanova.dev'
const CSP_PREFIX =
  "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; media-src 'self'; font-src 'self'; connect-src 'none'; worker-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors "
const PRODUCTION_CSP = `${CSP_PREFIX}https://app.bossanova.dev https://app-staging.bossanova.dev`

// The allow-list, restated here so a Worker change to it fails a test.
const TYPES = {
  html: 'text/html; charset=utf-8',
  css: 'text/css; charset=utf-8',
  js: 'text/javascript; charset=utf-8',
  mjs: 'text/javascript; charset=utf-8',
  json: 'application/json',
  txt: 'text/plain; charset=utf-8',
  ndjson: 'application/x-ndjson',
  png: 'image/png',
  jpg: 'image/jpeg',
  webp: 'image/webp',
  gif: 'image/gif',
  avif: 'image/avif',
  svg: 'image/svg+xml',
  mp4: 'video/mp4',
  webm: 'video/webm',
  pdf: 'application/pdf',
  woff2: 'font/woff2',
}

function signature({ org = 'acme', token = TOKEN, exp = NOW_S + 3600, key = KEY } = {}) {
  const sig = createHmac('sha256', Buffer.from(key, 'hex'))
    .update(`bossanova-user-uploads/v1\n${org}\n${token}\n${exp}`)
    .digest('base64url')
  return `v1.${exp}.${sig}`
}

function url({
  host = `acme.${DOMAIN}`,
  segment = signature(),
  token = TOKEN,
  path = 'index.html',
} = {}) {
  return `https://${host}/${segment}/${token}/${path}`
}

// A fake R2 bucket. Stored objects carry hostile HTTP metadata the Worker must never copy.
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
        uploaded: new Date(NOW - 1000),
        httpMetadata: { contentType: 'text/html', contentDisposition: 'inline' },
        writeHttpMetadata(headers) {
          headers.set('Content-Type', 'text/html')
          headers.set('Content-Disposition', 'inline')
          headers.set('Set-Cookie', 'session=stolen')
        },
      }
      if (options.onlyIf.get('If-None-Match') === metadata.httpEtag) return metadata
      const ifMatch = options.onlyIf.get('If-Match')
      if (ifMatch && ifMatch !== '*' && ifMatch !== metadata.httpEtag) return metadata
      const match = /^bytes=(\d+)-(\d+)$/.exec(options.range.get('Range') ?? '')
      if (match) {
        const offset = Number(match[1])
        const length = Number(match[2]) - offset + 1
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

function production(b = bucket(), overrides = {}) {
  return {
    UPLOADS_BUCKET: b,
    UPLOADS_DOMAIN: DOMAIN,
    UPLOADS_ENV: 'production',
    UPLOADS_VIEW_KEY: KEY,
    FRAME_ANCESTORS: PRODUCTION_ANCESTORS,
    RETENTION_DAYS: '90',
    ...overrides,
  }
}

function staging(b = bucket(), overrides = {}) {
  return production(b, {
    UPLOADS_ENV: 'staging',
    UPLOADS_VIEW_KEY: STAGING_KEY,
    FRAME_ANCESTORS: STAGING_ANCESTORS,
    ...overrides,
  })
}

function fetch(target, env, { method = 'GET', headers = {} } = {}) {
  return handleRequest(new Request(target, { method, headers }), env, { now: NOW })
}

function secure(response, csp = PRODUCTION_CSP) {
  assert.equal(response.headers.get('Content-Security-Policy'), csp)
  assert.equal(response.headers.get('X-Content-Type-Options'), 'nosniff')
  assert.equal(response.headers.get('Referrer-Policy'), 'no-referrer')
  assert.equal(response.headers.get('X-Robots-Tag'), 'noindex, nofollow')
  assert.equal(response.headers.get('Cache-Control'), 'private, max-age=300')
  assert.equal(response.headers.get('Set-Cookie'), null)
  assert.equal(response.headers.get('Content-Disposition'), null)
}

async function refused(target, env, label) {
  const response = await fetch(target, env)
  assert.equal(response.status, 404, label)
  secure(response, contentSecurityPolicy(env.FRAME_ANCESTORS))
  assert.deepEqual(env.UPLOADS_BUCKET.gets, [], `${label}: no R2 read`)
  assert.deepEqual(env.UPLOADS_BUCKET.lists, [], `${label}: no R2 list`)
}

const pageBucket = () =>
  bucket({
    [`acme/${TOKEN}/index.html`]: '<html>proof</html>',
    [`acme/${TOKEN}/player.js`]: 'play()',
    [`acme/${TOKEN}/media/clip.mp4`]: 'mp4-bytes',
  })

test('a signed page, its root and its relative assets are served with the pinned headers', async () => {
  const b = pageBucket()
  const env = production(b)
  for (const [path, body, type] of [
    ['index.html', '<html>proof</html>', TYPES.html],
    ['', '<html>proof</html>', TYPES.html],
    ['player.js', 'play()', TYPES.js],
    ['media/clip.mp4', 'mp4-bytes', TYPES.mp4],
  ]) {
    const response = await fetch(url({ path }), env)
    assert.equal(response.status, 200, path)
    assert.equal(await response.text(), body)
    assert.equal(response.headers.get('Content-Type'), type)
    secure(response)
  }
  assert.deepEqual(b.gets, [
    `acme/${TOKEN}/index.html`,
    `acme/${TOKEN}/index.html`,
    `acme/${TOKEN}/player.js`,
    `acme/${TOKEN}/media/clip.mp4`,
  ])
  // The default export runs on the real clock.
  const live = signature({ exp: Math.floor(Date.now() / 1000) + 600 })
  const response = await worker.fetch(new Request(url({ segment: live })), env)
  assert.equal(response.status, 200)
  // The longest link lifetime is accepted at its boundary.
  assert.equal((await fetch(url({ segment: signature({ exp: NOW_S + WEEK_S }) }), env)).status, 200)
})

test('missing, malformed, wrong, expired, over-long and cross-org or cross-page signatures are 404 without reading R2', async () => {
  const good = signature()
  const [, exp, sig] = good.split('.')
  for (const path of ['index.html', 'media/clip.mp4']) {
    const cases = {
      'no signature segment': `https://acme.${DOMAIN}/${TOKEN}/${path}`,
      'version 2': url({ path, segment: `v2.${exp}.${sig}` }),
      'leading zero expiry': url({ path, segment: `v1.0${exp}.${sig}` }),
      'short signature': url({ path, segment: `v1.${exp}.${sig.slice(1)}` }),
      'padded signature': url({ path, segment: `v1.${exp}.${sig}=` }),
      'empty segment': url({ path, segment: '' }),
      'wrong signature': url({ path, segment: signature({ key: 'cd'.repeat(32) }) }),
      'exp edited after signing': url({ path, segment: `v1.${Number(exp) + 1}.${sig}` }),
      expired: url({ path, segment: signature({ exp: NOW_S - 1 }) }),
      'expires now': url({ path, segment: signature({ exp: NOW_S }) }),
      'past the maximum lifetime': url({ path, segment: signature({ exp: NOW_S + WEEK_S + 1 }) }),
      'minted for another org': url({ path, segment: signature({ org: 'globex' }) }),
      'minted for another token': url({ path, segment: signature({ token: OTHER_TOKEN }) }),
    }
    for (const [label, target] of Object.entries(cases))
      await refused(target, production(pageBucket()), `${path}: ${label}`)
  }
})

test('a missing, empty or non-hex view key fails closed for a correctly signed request', async () => {
  for (const key of [undefined, '', 'AB'.repeat(32), 'ab'.repeat(31), `${KEY}a`, 'zz'.repeat(32)]) {
    await refused(url(), production(pageBucket(), { UPLOADS_VIEW_KEY: key }), `key ${key}`)
  }
})

test('the signed path is 404 on another org, the apex, a deeper host, workers.dev or an empty domain', async () => {
  for (const [label, host, overrides] of [
    ['other org', `globex.${DOMAIN}`, {}],
    ['apex', DOMAIN, {}],
    ['two levels', `www.acme.${DOMAIN}`, {}],
    ['workers.dev', 'bossanova-user-uploads-production.example.workers.dev', {}],
    ['bad label', `-acme.${DOMAIN}`, {}],
    ['empty domain', `acme.${DOMAIN}`, { UPLOADS_DOMAIN: '' }],
    ['missing domain', `acme.${DOMAIN}`, { UPLOADS_DOMAIN: undefined }],
  ])
    await refused(url({ host }), production(pageBucket(), overrides), label)
  // A signature minted for another org reads only that org's prefix, never acme's object.
  const b = pageBucket()
  const response = await fetch(
    url({ host: `globex.${DOMAIN}`, segment: signature({ org: 'globex' }) }),
    production(b),
  )
  assert.equal(response.status, 404)
  assert.deepEqual(b.gets, [`globex/${TOKEN}/index.html`])
})

test('allow-listed extensions get their table type whatever the stored metadata says; others are 404 unread', async () => {
  const entries = {}
  for (const extension of Object.keys(TYPES)) entries[`acme/${TOKEN}/file.${extension}`] = 'x'
  entries[`acme/${TOKEN}/LOUD.HTML`] = 'x'
  const b = bucket(entries)
  for (const [extension, type] of Object.entries(TYPES)) {
    const response = await fetch(url({ path: `file.${extension}` }), production(b))
    assert.equal(response.status, 200, extension)
    assert.equal(response.headers.get('Content-Type'), type, extension)
    secure(response)
  }
  assert.equal(
    (await fetch(url({ path: 'LOUD.HTML' }), production(b))).headers.get('Content-Type'),
    TYPES.html,
  )
  for (const path of ['file.exe', 'file.jpeg', 'file.html.bak', 'README', '.html', 'media/clip'])
    await refused(url({ path }), production(bucket(entries)), path)
})

test('every status carries the security headers and never a Set-Cookie', async () => {
  const b = pageBucket()
  const env = production(b)
  const statuses = []
  for (const [target, init] of [
    [url(), {}],
    [url(), { headers: { 'If-None-Match': '"etag"' } }],
    [url({ path: 'missing.html' }), {}],
    [url(), { method: 'POST' }],
    [url(), { headers: { 'If-Match': '"other"' } }],
  ]) {
    const response = await fetch(target, env, init)
    statuses.push(response.status)
    secure(response)
  }
  assert.deepEqual(statuses, [200, 304, 404, 405, 412])
  const broken = production({
    gets: [],
    async get() {
      throw new Error('R2 down')
    },
  })
  const response = await fetch(url(), broken)
  assert.equal(response.status, 500)
  secure(response)
})

test('frame-ancestors lists exactly the configured https origins and fails closed otherwise', async () => {
  assert.equal(contentSecurityPolicy(PRODUCTION_ANCESTORS), PRODUCTION_CSP)
  assert.equal(contentSecurityPolicy(STAGING_ANCESTORS), `${CSP_PREFIX}${STAGING_ANCESTORS}`)
  for (const value of [
    undefined,
    '',
    '   ',
    'https://app.bossanova.dev http://evil.example',
    "'self'",
  ])
    assert.equal(contentSecurityPolicy(value), `${CSP_PREFIX}'none'`, String(value))
  const response = await fetch(url(), production(pageBucket(), { FRAME_ANCESTORS: '' }))
  assert.equal(response.status, 200)
  assert.equal(response.headers.get('Content-Security-Policy'), `${CSP_PREFIX}'none'`)
})

test('signed ranges, HEAD and conditional reads use R2 native handling', async () => {
  const key = `acme/${TOKEN}/media/clip.mp4`
  const b = bucket({ [key]: '0123456789abcdefghij' })
  const env = production(b)
  const target = url({ path: 'media/clip.mp4' })
  let response = await fetch(target, env, { headers: { Range: 'bytes=0-9' } })
  assert.equal(response.status, 206)
  assert.equal(response.headers.get('Content-Range'), 'bytes 0-9/20')
  assert.equal(response.headers.get('Content-Length'), '10')
  assert.equal(await response.text(), '0123456789')
  secure(response)
  response = await fetch(target, env, { method: 'HEAD' })
  assert.equal(response.status, 200)
  assert.equal(response.headers.get('Content-Length'), '20')
  assert.equal(response.headers.get('Content-Type'), TYPES.mp4)
  assert.equal(await response.text(), '')
  response = await fetch(target, env, { headers: { 'If-None-Match': '"etag"' } })
  assert.equal(response.status, 304)
  assert.equal(response.headers.get('ETag'), '"etag"')
  response = await fetch(target, env, { headers: { 'If-Match': '"other"' } })
  assert.equal(response.status, 412)
  assert.deepEqual(b.lists, [])
})

test('unsafe paths are 404, other methods are 405, and nothing lists the bucket', async () => {
  for (const [label, target] of [
    ['encoded traversal', url({ path: '%2e%2e/index.html' })],
    ['encoded traversal into another page', url({ path: `%2e%2e%2f${OTHER_TOKEN}%2findex.html` })],
    ['empty segment', url({ path: 'media//clip.mp4' })],
    ['trailing directory', url({ path: 'media/' })],
    ['encoded slash', url({ path: 'media%2fclip.mp4' })],
    ['backslash', url({ path: 'media%5cclip.mp4' })],
    ['undecodable', url({ path: '%zz.html' })],
    ['short token', url({ token: 'short' })],
    ['token with a dot', url({ token: `${TOKEN}.x` })],
    ['no trailing slash', `https://acme.${DOMAIN}/${signature()}/${TOKEN}`],
  ])
    await refused(target, production(pageBucket()), label)
  const b = pageBucket()
  for (const method of ['POST', 'PUT', 'DELETE', 'PATCH']) {
    const response = await fetch(url(), production(b), { method })
    assert.equal(response.status, 405, method)
    assert.equal(response.headers.get('Allow'), 'GET, HEAD')
    secure(response)
  }
  assert.deepEqual(b.gets, [])
  assert.deepEqual(b.lists, [])
})

test('the sweep deletes old objects in every prefix and follows cursors', async () => {
  const old = new Date(NOW - 91 * 86400000)
  const recent = new Date(NOW - 89 * 86400000)
  const b = bucket()
  b.list = async function (options) {
    this.lists.push(options)
    return options.cursor
      ? { objects: [{ key: 'globex/t/old.html', uploaded: old }], truncated: false }
      : {
          objects: [
            { key: 'acme/t/old.html', uploaded: old },
            { key: 'acme/t/new.html', uploaded: recent },
            { key: 'plans/history.html', uploaded: old },
          ],
          truncated: true,
          cursor: 'next',
        }
  }
  await worker.scheduled({ scheduledTime: NOW }, production(b))
  assert.deepEqual(b.deleted, ['acme/t/old.html', 'plans/history.html', 'globex/t/old.html'])
  assert.deepEqual(
    b.lists.map((options) => options.cursor),
    [undefined, 'next'],
  )
})

test('an invalid retention deletes nothing and a sweep stops at its page budget', async () => {
  for (const retention of [undefined, '', '0', '-1', '1.5', 'abc']) {
    const b = bucket()
    await worker.scheduled({ scheduledTime: NOW }, production(b, { RETENTION_DAYS: retention }))
    assert.deepEqual(b.lists, [], String(retention))
    assert.deepEqual(b.deleted, [])
  }
  const b = bucket()
  b.list = async function (options) {
    this.lists.push(options)
    return {
      objects: [{ key: `k${this.lists.length}`, uploaded: new Date(0) }],
      truncated: true,
      cursor: String(this.lists.length),
    }
  }
  await worker.scheduled({ scheduledTime: NOW }, production(b))
  assert.equal(b.lists.length, 100)
  assert.equal(b.deleted.length, 100)
})

test('staging serves <org>-staging hosts with its own key and frame origin; each env refuses the other', async () => {
  const stagingSignature = signature({ key: STAGING_KEY })
  const stagingHost = `acme-staging.${DOMAIN}`

  const b = pageBucket()
  const response = await fetch(url({ host: stagingHost, segment: stagingSignature }), staging(b))
  assert.equal(response.status, 200)
  assert.equal(await response.text(), '<html>proof</html>')
  secure(response, `${CSP_PREFIX}https://app-staging.bossanova.dev`)
  assert.deepEqual(b.gets, [`acme/${TOKEN}/index.html`])

  for (const [label, target, env] of [
    ['production refuses a staging host', url({ host: stagingHost }), production(pageBucket())],
    [
      'production refuses a staging host signed over its full label',
      url({ host: stagingHost, segment: signature({ org: 'acme-staging' }) }),
      production(pageBucket()),
    ],
    [
      'staging refuses a production host',
      url({ segment: stagingSignature }),
      staging(pageBucket()),
    ],
    [
      'a production key does not open staging',
      url({ host: stagingHost, segment: signature() }),
      staging(pageBucket()),
    ],
  ])
    await refused(target, env, label)

  for (const value of [undefined, '', 'Production', 'dev'])
    await refused(url(), production(pageBucket(), { UPLOADS_ENV: value }), `UPLOADS_ENV ${value}`)

  for (const label of ['-staging', 'acme--staging', 'staging', '-acme-staging', 'acme-stagingx'])
    await refused(
      url({ host: `${label}.${DOMAIN}`, segment: stagingSignature }),
      staging(pageBucket()),
      `staging label ${label}`,
    )
})

test('a deleted object is 404 on the next request through the same signed URL, and no response is shared-cacheable', async () => {
  const entries = { [`acme/${TOKEN}/index.html`]: '<html>proof</html>' }
  const b = bucket(entries)
  const env = production(b)
  const target = url()
  const served = await fetch(target, env)
  assert.equal(served.status, 200)
  assert.match(served.headers.get('Cache-Control'), /^private, max-age=(\d+)$/)
  assert.ok(Number(/max-age=(\d+)/.exec(served.headers.get('Cache-Control'))[1]) <= 300)
  // bosso's proof page deleter removes the object from R2 (BOS-2603).
  delete entries[`acme/${TOKEN}/index.html`]
  const gone = await fetch(target, env)
  assert.equal(gone.status, 404)
  assert.match(gone.headers.get('Cache-Control'), /^private\b/)
  assert.deepEqual(b.gets, [`acme/${TOKEN}/index.html`, `acme/${TOKEN}/index.html`])
})

// bosso's signer (services/bosso/internal/proofview) reproduces the same file, so a drift from the
// contract on either side fails both suites.
const VECTORS = JSON.parse(
  readFileSync(
    new URL(
      '../../services/bosso/internal/proofview/testdata/view-signature-vectors.json',
      import.meta.url,
    ),
    'utf8',
  ),
)

test('the shared view-signature vectors open their page and the tampered ones are 404', async () => {
  assert.ok(VECTORS.valid.length > 0 && VECTORS.refused.length > 0)
  const vectorBucket = () =>
    bucket(
      Object.fromEntries(
        [...VECTORS.valid, ...VECTORS.refused].map((v) => [
          `${v.label}/${v.token}/index.html`,
          'page',
        ]),
      ),
    )
  const at = (v, env) =>
    handleRequest(new Request(`https://${v.label}.${DOMAIN}/${v.segment}/${v.token}/`), env, {
      now: v.now * 1000,
    })
  for (const v of VECTORS.valid) {
    const response = await at(v, production(vectorBucket(), { UPLOADS_VIEW_KEY: VECTORS.key }))
    assert.equal(response.status, 200, v.name)
    assert.equal(await response.text(), 'page', v.name)
  }
  for (const v of VECTORS.refused) {
    const env = production(vectorBucket(), { UPLOADS_VIEW_KEY: VECTORS.key })
    const response = await at(v, env)
    assert.equal(response.status, 404, v.name)
    assert.deepEqual(env.UPLOADS_BUCKET.gets, [], `${v.name}: no R2 read`)
  }
})
