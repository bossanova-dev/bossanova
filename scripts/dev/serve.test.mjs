import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

import {
  ALL_SERVICES,
  ProcessSupervisor,
  availableServices,
  buildServiceEnv,
  meetsMinNodeVersion,
  parseServeArgs,
  parseTailscaleServePorts,
  parseTailscaleStatus,
  pickTailscalePort,
  writeServicesFileAtomic,
} from './serve.mjs'

test('meetsMinNodeVersion compares semver triples', () => {
  assert.equal(meetsMinNodeVersion('22.22.2', [22, 23, 4]), false)
  assert.equal(meetsMinNodeVersion('v22.23.4', [22, 23, 4]), true)
  assert.equal(meetsMinNodeVersion('v26.13.0', [22, 23, 4]), true)
  assert.equal(meetsMinNodeVersion('invalid', [22, 23, 4]), false)
})

test('parseServeArgs defaults and flags', () => {
  assert.deepEqual(parseServeArgs([]), {
    services: [...ALL_SERVICES],
    tunnel: 'auto',
    auth: 'dev',
    watch: true,
    help: false,
  })
  assert.deepEqual(
    parseServeArgs([
      '--services',
      'web, bosso,web',
      '--tunnel',
      'none',
      '--auth',
      'dev',
      '--no-watch',
    ]),
    {
      services: ['web', 'bosso'],
      tunnel: 'none',
      auth: 'dev',
      watch: false,
      help: false,
    },
  )
  assert.throws(() => parseServeArgs(['--services', 'unknown']), /unknown service/)
  assert.throws(() => parseServeArgs(['--tunnel', 'ngrok']), /invalid --tunnel/)
  assert.throws(() => parseServeArgs(['--auth', 'magic']), /invalid --auth/)
  assert.throws(() => parseServeArgs(['--bogus']), /Unknown option/)
})

test('availableServices filters to existing service directories', () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'serve-avail-'))
  try {
    fs.mkdirSync(path.join(tmp, 'services/web'), { recursive: true })
    fs.mkdirSync(path.join(tmp, 'services/docs'), { recursive: true })
    assert.deepEqual(availableServices(tmp, ['bosso', 'web', 'marketing', 'docs']), ['web', 'docs'])
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true, maxRetries: 3 })
  }
})

test('parseTailscaleStatus handles running, https enabled and certDomains null', () => {
  assert.deepEqual(
    parseTailscaleStatus(
      JSON.stringify({
        BackendState: 'Running',
        Self: { DNSName: 'delta.tail5662e6.ts.net.' },
        CertDomains: ['delta.tail5662e6.ts.net'],
      }),
    ),
    { running: true, dnsName: 'delta.tail5662e6.ts.net', httpsEnabled: true },
  )
  assert.deepEqual(
    parseTailscaleStatus(
      JSON.stringify({
        BackendState: 'Running',
        Self: { DNSName: 'delta.tail5662e6.ts.net.' },
        CertDomains: null,
      }),
    ),
    { running: true, dnsName: 'delta.tail5662e6.ts.net', httpsEnabled: false },
  )
  assert.deepEqual(parseTailscaleStatus('not-json'), {
    running: false,
    dnsName: '',
    httpsEnabled: false,
  })
})

test('parseTailscaleServePorts and pickTailscalePort avoid collisions', () => {
  const used = parseTailscaleServePorts(
    JSON.stringify({
      TCP: { 15173: { HTTPS: true } },
      Web: { 'delta.tail5662e6.ts.net:18080': {} },
    }),
  )
  assert.deepEqual(
    [...used].sort((a, b) => a - b),
    [15173, 18080],
  )
  assert.equal(pickTailscalePort(15173, used), 15183)
  assert.equal(pickTailscalePort(18080, used), 18090)
  assert.equal(pickTailscalePort(14321, used), 14321)
})

test('buildServiceEnv wires web to bosso and configures CORS + dev auth', () => {
  const localPorts = { bosso: 8080, web: 5173, marketing: 4321, docs: 3001 }
  const publicUrls = {
    bosso: 'https://delta.tail5662e6.ts.net:18080',
    web: 'https://delta.tail5662e6.ts.net:15173',
    marketing: 'https://delta.tail5662e6.ts.net:14321',
    docs: 'https://delta.tail5662e6.ts.net:13001',
  }

  const bossoEnv = buildServiceEnv({
    service: 'bosso',
    localPorts,
    publicUrls,
    authMode: 'dev',
    databaseUrl: 'postgres://postgres:postgres@127.0.0.1:5432/bosso_test?sslmode=disable',
    tailscaleHost: 'delta.tail5662e6.ts.net',
    baseEnv: {
      PATH: '/usr/bin',
      BOSSO_WORKOS_API_KEY: 'sk_live_ambient',
      BOSSO_WORKOS_CLIENT_ID: 'client_ambient',
    },
  })
  assert.equal(bossoEnv.BOSSO_ADDR, '127.0.0.1:8080')
  assert.equal(bossoEnv.BOSS_ENV, 'development')
  assert.equal(bossoEnv.BOSS_E2E_TEST_AUTH, '1')
  assert.equal(bossoEnv.BOSSO_WORKOS_API_KEY, '')
  assert.equal(bossoEnv.BOSSO_WORKOS_CLIENT_ID, '')
  assert.equal(bossoEnv.BOSSO_WORKOS_WEBHOOK_SECRET, '')
  assert.equal(bossoEnv.BOSSO_STRIPE_SECRET_KEY, '')
  assert.equal(bossoEnv.BOSSO_STRIPE_CLOUD_PRICE_ID, '')
  assert.equal(bossoEnv.BOSSO_DB_DRIVER, 'postgres')
  assert.equal(bossoEnv.BOSSO_DATABASE_URL, bossoEnv.BOSSO_MIGRATIONS_DATABASE_URL)
  assert.equal(
    bossoEnv.BOSSO_CORS_ORIGINS,
    'http://127.0.0.1:5173,http://localhost:5173,https://delta.tail5662e6.ts.net:15173',
  )
  assert.equal(
    bossoEnv.BOSSO_WS_ALLOWED_ORIGINS,
    '127.0.0.1:5173,localhost:5173,delta.tail5662e6.ts.net:15173',
  )

  const webEnv = buildServiceEnv({
    service: 'web',
    localPorts,
    publicUrls,
    authMode: 'dev',
    tailscaleHost: 'delta.tail5662e6.ts.net',
    baseEnv: { PATH: '/usr/bin' },
  })
  assert.equal(webEnv.BOSS_WEB_PORT, '5173')
  assert.equal(webEnv.VITE_API_BASE_URL, 'https://delta.tail5662e6.ts.net:18080')
  assert.equal(webEnv.VITE_E2E_REAL, '1')
  assert.equal(webEnv.BOSS_WEB_ALLOWED_HOSTS, '.ts.net,delta.tail5662e6.ts.net')

  const workosWebEnv = buildServiceEnv({
    service: 'web',
    localPorts,
    publicUrls,
    authMode: 'workos',
    baseEnv: { VITE_E2E_REAL: '1' },
  })
  assert.equal(workosWebEnv.VITE_E2E_REAL, undefined)

  const marketingEnv = buildServiceEnv({
    service: 'marketing',
    localPorts,
    publicUrls,
    authMode: 'dev',
    tailscaleHost: 'delta.tail5662e6.ts.net',
    baseEnv: {},
  })
  assert.equal(marketingEnv.BOSS_MARKETING_ALLOWED_HOSTS, '.ts.net,delta.tail5662e6.ts.net')
  assert.equal(marketingEnv.PUBLIC_SITE_URL, 'https://delta.tail5662e6.ts.net:14321')
})

test('writeServicesFileAtomic writes sorted https-only JSON without temp residue', () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'serve-links-'))
  try {
    const file = path.join(tmp, 'services.json')
    writeServicesFileAtomic(file, {
      web: 'https://delta.tail5662e6.ts.net:15173',
      bosso: 'https://delta.tail5662e6.ts.net:18080',
      ignoredHttp: 'http://127.0.0.1:9999',
    })
    assert.deepEqual(JSON.parse(fs.readFileSync(file, 'utf8')), {
      bosso: 'https://delta.tail5662e6.ts.net:18080',
      web: 'https://delta.tail5662e6.ts.net:15173',
    })
    assert.deepEqual(fs.readdirSync(tmp), ['services.json'])
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true, maxRetries: 3 })
  }
})

test('ProcessSupervisor stops entire child process group including grandchildren', async () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'serve-pgid-'))
  const grandchildPidFile = path.join(tmp, 'grandchild.pid')
  const logs = []
  const supervisor = new ProcessSupervisor({ write: (line) => logs.push(line) })

  try {
    const script = [
      'const { spawn } = require("node:child_process");',
      'const fs = require("node:fs");',
      'const child = spawn(process.execPath, ["-e", "setInterval(() => {}, 60000)"]);',
      `fs.writeFileSync(${JSON.stringify(grandchildPidFile)}, String(child.pid));`,
      'console.log("ready");',
      'setInterval(() => {}, 60000);',
    ].join(' ')

    supervisor.spawn('demo', 'demo', process.execPath, ['-e', script])

    const deadline = Date.now() + 30_000
    while (!fs.existsSync(grandchildPidFile) && Date.now() < deadline) {
      await new Promise((r) => setTimeout(r, 50))
    }
    const grandchildPid = Number.parseInt(fs.readFileSync(grandchildPidFile, 'utf8').trim(), 10)
    assert.ok(Number.isInteger(grandchildPid) && grandchildPid > 0)

    await supervisor.stopAll()

    // Confirm the grandchild was also terminated via process-group signaling.
    let alive = true
    const killDeadline = Date.now() + 30_000
    while (alive && Date.now() < killDeadline) {
      try {
        process.kill(grandchildPid, 0)
        await new Promise((r) => setTimeout(r, 50))
      } catch {
        alive = false
      }
    }
    assert.equal(alive, false, 'grandchild process should be terminated with its process group')
    assert.ok(logs.some((line) => line.includes('[demo] ready')))
  } finally {
    await supervisor.stopAll()
    fs.rmSync(tmp, { recursive: true, force: true, maxRetries: 3 })
  }
})
