import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { createServer } from 'node:http'
import { once } from 'node:events'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { putObjects, resolveAccessCredentials } from './verify-access-put.mjs'
import { uploadProofPage } from './verify-proof-publish.mjs'

const HEAD = 'a'.repeat(40)
const credentials = { clientId: 'client-id', clientSecret: 'client-secret-value' }
const env = {
  PROOF_ACCESS_CLIENT_ID: credentials.clientId,
  PROOF_ACCESS_CLIENT_SECRET: credentials.clientSecret,
}
const publisher = { kind: 'access-put', baseUrl: 'https://proof.example' }
const target = { repo: 'acme/widgets', pr: 7, headSha: HEAD }
const PAGE = 'index.html'

test('Access credentials prefer environment and use macOS keychain per missing account @sessions.AC256203', () => {
  const calls = []
  const readKeychain = ({ service, account }) => {
    calls.push([service, account])
    return account === 'PROOF_ACCESS_CLIENT_ID' ? credentials.clientId : credentials.clientSecret
  }
  assert.deepEqual(resolveAccessCredentials({ env, platform: 'darwin', readKeychain }), credentials)
  assert.deepEqual(calls, [])
  assert.deepEqual(
    resolveAccessCredentials({
      env: { PROOF_ACCESS_CLIENT_ID: credentials.clientId },
      platform: 'darwin',
      keychainService: 'my-proof-host',
      readKeychain,
    }),
    credentials,
  )
  assert.deepEqual(calls, [['my-proof-host', 'PROOF_ACCESS_CLIENT_SECRET']])
  calls.length = 0
  assert.deepEqual(
    resolveAccessCredentials({ env: {}, platform: 'darwin', readKeychain }),
    credentials,
  )
  assert.deepEqual(calls, [
    ['boss-proof-publish', 'PROOF_ACCESS_CLIENT_ID'],
    ['boss-proof-publish', 'PROOF_ACCESS_CLIENT_SECRET'],
  ])
})

test('missing and failed keychain credentials disclose names only', () => {
  let keychainCalls = 0
  assert.deepEqual(
    resolveAccessCredentials({
      env: { PROOF_ACCESS_CLIENT_ID: ' ' },
      platform: 'linux',
      readKeychain: () => {
        keychainCalls++
        throw new Error('must not read a non-macOS keychain')
      },
    }),
    {
      ok: false,
      reason: 'credentials-missing',
      missing: ['PROOF_ACCESS_CLIENT_ID', 'PROOF_ACCESS_CLIENT_SECRET'],
    },
  )
  assert.equal(keychainCalls, 0)
  const fail = () => {
    throw new Error(credentials.clientSecret)
  }
  for (const platform of ['linux', 'win32', 'darwin']) {
    const outcome = resolveAccessCredentials({ env: {}, platform, readKeychain: fail })
    assert.deepEqual(outcome, {
      ok: false,
      reason: 'credentials-missing',
      missing: ['PROOF_ACCESS_CLIENT_ID', 'PROOF_ACCESS_CLIENT_SECRET'],
    })
    assert.ok(!JSON.stringify(outcome).includes(credentials.clientSecret))
  }
  assert.deepEqual(
    resolveAccessCredentials({ env: { PROOF_ACCESS_CLIENT_ID: '  ' }, platform: 'linux' }).missing,
    ['PROOF_ACCESS_CLIENT_ID', 'PROOF_ACCESS_CLIENT_SECRET'],
  )
})

test('the page deadline aborts an HTTP upload that never returns', async (t) => {
  const server = (await import('node:http')).createServer(() => {})
  server.listen(0, '127.0.0.1')
  await once(server, 'listening')
  t.after(() => {
    server.closeAllConnections()
    server.close()
  })
  const { port } = server.address()
  const result = await uploadProofPage({
    publisher,
    target,
    html: '<p>page</p>',
    env,
    hookTimeoutMs: 25,
    fetchImpl: (url, options) =>
      fetch(String(url).replace('https://proof.example', `http://127.0.0.1:${port}`), options),
  })
  assert.deepEqual(result, { ok: false, reason: 'timeout' })
})

test('Access PUT publishes bytes, headers and deterministic paths through an HTTP server @sessions.AC256202', async (t) => {
  const requests = []
  const server = createServer(async (req, res) => {
    const chunks = []
    for await (const chunk of req) chunks.push(chunk)
    requests.push({
      method: req.method,
      url: req.url,
      headers: req.headers,
      body: Buffer.concat(chunks),
    })
    res.writeHead(201).end()
  })
  server.listen(0, '127.0.0.1')
  await once(server, 'listening')
  t.after(() => new Promise((resolve) => server.close(resolve)))
  const { port } = server.address()
  const dir = tempDir('access-media-', t)
  const video = path.join(dir, 'video.mp4')
  fs.writeFileSync(video, Buffer.from([0, 1, 2, 255]))
  const signals = []
  const result = await uploadProofPage({
    publisher,
    html: '<p>proof page</p>',
    assets: [
      { name: 'video.mp4', from: video },
      { name: 'player.js', text: 'play()' },
    ],
    target,
    env,
    fetchImpl: (url, options) => {
      signals.push(options.signal)
      assert.equal(options.redirect, 'manual')
      return fetch(
        String(url).replace('https://proof.example', `http://127.0.0.1:${port}`),
        options,
      )
    },
  })
  assert.deepEqual(result, {
    ok: true,
    url: `https://proof.example/acme/widgets/7/${HEAD}/index.html`,
  })
  assert.deepEqual(
    requests.map((r) => r.url),
    ['video.mp4', 'player.js', 'index.html'].map(
      (name) => `/_publish/acme/widgets/7/${HEAD}/${name}`,
    ),
  )
  assert.deepEqual(
    requests.map((r) => r.body),
    [Buffer.from([0, 1, 2, 255]), Buffer.from('play()'), Buffer.from('<p>proof page</p>')],
  )
  assert.deepEqual(
    requests.map((r) => r.headers['content-type']),
    ['video/mp4', 'text/javascript; charset=utf-8', 'text/html; charset=utf-8'],
  )
  for (const r of requests) {
    assert.equal(r.method, 'PUT')
    assert.equal(r.headers['cf-access-client-id'], credentials.clientId)
    assert.equal(r.headers['cf-access-client-secret'], credentials.clientSecret)
  }
  assert.equal(signals[0], signals[1], 'one deadline covers the media batch')
  assert.notEqual(signals[1], signals[2], 'page receives its own short deadline')
})

test('Access PUT failures stop before index and never return credential or response text', async (t) => {
  const dir = tempDir('access-put-', t)
  fs.writeFileSync(path.join(dir, 'player.js'), 'play()')
  const input = {
    ...publisher,
    prefix: `acme/widgets/7/${HEAD}/`,
    dir,
    names: ['player.js'],
    credentials,
  }
  for (const [status, reason, detail] of [
    [401, 'upload-refused'],
    [403, 'upload-refused'],
    [500, 'upload-failed', 'HTTP 500'],
    [302, 'upload-failed', 'HTTP 302'],
  ]) {
    const result = await putObjects({
      ...input,
      fetchImpl: async () => ({
        status,
        text: () => {
          throw new Error('body read')
        },
      }),
    })
    assert.deepEqual(result, { ok: false, reason, ...(detail ? { detail } : {}) })
  }
  assert.deepEqual(
    await putObjects({
      ...input,
      fetchImpl: async () => {
        throw new Error(`refused ${credentials.clientSecret}`)
      },
    }),
    { ok: false, reason: 'upload-failed', detail: 'Error: refused [redacted]' },
  )
  const unreachable = Object.assign(new TypeError('fetch failed'), {
    cause: { code: 'ECONNREFUSED' },
  })
  assert.deepEqual(
    await putObjects({
      ...input,
      fetchImpl: async () => {
        throw unreachable
      },
    }),
    { ok: false, reason: 'host-unreachable', detail: 'TypeError: fetch failed (ECONNREFUSED)' },
  )
  assert.deepEqual(
    await putObjects({
      ...input,
      fetchImpl: async () => {
        throw Object.assign(new Error(credentials.clientSecret), { name: 'TimeoutError' })
      },
    }),
    { ok: false, reason: 'timeout' },
  )
  const calls = []
  const failure = await uploadProofPage({
    publisher,
    target,
    html: '<p>page</p>',
    assets: [{ name: 'player.js', text: 'play()' }],
    env,
    fetchImpl: async (url) => {
      calls.push(url)
      return { status: 403 }
    },
  })
  assert.deepEqual(failure, { ok: false, reason: 'upload-refused' })
  assert.equal(calls.length, 1)
  assert.ok(calls[0].endsWith('/player.js'))
})

test('only a DNS, connect or TLS failure before any response is host-unreachable @sessions.AC278701', async (t) => {
  const dir = tempDir('access-unreachable-', t)
  for (const name of ['player.js', PAGE]) fs.writeFileSync(path.join(dir, name), 'x')
  const input = { ...publisher, prefix: `acme/widgets/7/${HEAD}/`, dir, credentials }
  const thrown = (code) => Object.assign(new TypeError('fetch failed'), { cause: { code } })
  for (const code of [
    'ENOTFOUND',
    'EAI_AGAIN',
    'ECONNREFUSED',
    'EHOSTUNREACH',
    'UND_ERR_CONNECT_TIMEOUT',
    'CERT_HAS_EXPIRED',
    'ERR_TLS_CERT_ALTNAME_INVALID',
    'SELF_SIGNED_CERT_IN_CHAIN',
  ]) {
    const result = await putObjects({
      ...input,
      names: ['player.js'],
      fetchImpl: async () => {
        throw thrown(code)
      },
    })
    assert.equal(result.reason, 'host-unreachable', code)
  }
  for (const code of ['ECONNRESET', 'EPIPE', undefined]) {
    const result = await putObjects({
      ...input,
      names: ['player.js'],
      fetchImpl: async () => {
        throw thrown(code)
      },
    })
    assert.equal(result.reason, 'upload-failed', String(code))
  }
  let calls = 0
  const partial = await putObjects({
    ...input,
    names: ['player.js', PAGE],
    fetchImpl: async () => {
      if (calls++ === 0) return { status: 201 }
      throw thrown('ECONNREFUSED')
    },
  })
  assert.equal(partial.reason, 'upload-failed')
})

test('missing credentials and invalid configured hosts issue no HTTP requests', async () => {
  const fetchImpl = () => {
    throw new Error('must not upload')
  }
  assert.equal(
    (
      await uploadProofPage({
        publisher,
        target,
        html: '',
        env: {},
        readKeychain: () => null,
        fetchImpl,
      })
    ).reason,
    'credentials-missing',
  )
  const named = await uploadProofPage({
    publisher,
    target,
    html: '',
    env: { PROOF_ACCESS_CLIENT_ID: credentials.clientId },
    platform: 'linux',
    fetchImpl,
  })
  assert.deepEqual(named, {
    ok: false,
    reason: 'credentials-missing',
    detail: 'PROOF_ACCESS_CLIENT_SECRET',
  })
  for (const baseUrl of [
    'http://proof.example',
    'https://proof.example/?token=value',
    'https://user:pass@proof.example',
  ]) {
    assert.equal(
      (
        await uploadProofPage({
          publisher: { ...publisher, baseUrl },
          target,
          html: '',
          env,
          fetchImpl,
        })
      ).reason,
      'invalid-publish-config',
    )
  }
})

test('keychain secrets echoed in a page are refused before upload @sessions.AC256203', async () => {
  let calls = 0
  const result = await uploadProofPage({
    publisher,
    target,
    html: `<p>${credentials.clientSecret}</p>`,
    env: { PROOF_ACCESS_CLIENT_ID: credentials.clientId },
    platform: 'darwin',
    readKeychain: () => credentials.clientSecret,
    fetchImpl: async () => {
      calls++
      return { status: 201 }
    },
  })
  assert.deepEqual(result, { ok: false, reason: 'page-contains-secret' })
  assert.equal(calls, 0)
})
