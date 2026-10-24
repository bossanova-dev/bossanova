#!/usr/bin/env node
import { spawnSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import { existsSync } from 'node:fs'
import net from 'node:net'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { ENV_FAILURE_EXIT_CODE, TEST_POSTGRES_FAILURE_SIGNATURE } from './env-failure-lib.mjs'

const LABEL = 'bossanova.test-postgres.worktree'
const suppliedRemedy =
  'unset BOSSO_TEST_DATABASE_URL / WAREHOUSE_TEST_ADMIN_URL to provision this worktree’s own container, or repair the supplied server'
const SQL =
  "DO $$ BEGIN IF pg_is_in_recovery() THEN RAISE EXCEPTION 'database system is in recovery mode'; END IF; IF current_setting('transaction_read_only') = 'on' THEN RAISE EXCEPTION 'database is read-only'; END IF; END $$; BEGIN; CREATE TEMP TABLE boss_health (id int); INSERT INTO boss_health VALUES (1); ROLLBACK;"
const defaultRun = async (command, args, extra) => {
  let env = process.env
  if (extra?.url) {
    // PGDATABASE treats a URI as a literal database name. Pass the connection
    // explicitly, keeping passwords in the environment rather than process argv.
    const connection = new URL(extra.url)
    const password =
      connection.searchParams.get('password') ?? decodeURIComponent(connection.password)
    connection.password = ''
    connection.searchParams.delete('password')
    args = [...args, '--dbname', connection.toString()]
    env = { ...process.env, PGPASSWORD: password, PGCONNECT_TIMEOUT: '5' }
  }
  return spawnSync(command, args, { encoding: 'utf8', timeout: 15000, env })
}
const output = (r) => `${r.stdout || ''}\n${r.stderr || ''}`
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

export function classifyPostgresProbe(text) {
  const rules = [
    [
      'docker-unavailable',
      /Cannot connect to the Docker daemon|docker daemon is not running|docker.*ENOENT/i,
    ],
    ['connection-refused', /connection refused|could not connect to server/i],
    ['too-many-clients', /53300|too many (?:clients|connections)|remaining connection slots/i],
    ['recovery-mode', /57P03|in recovery|starting up|shutting down/i],
    ['read-only', /read.only/i],
    ['disk-full', /No space left on device|ENOSPC|disk full/i],
    [
      'auth-rejected',
      /failed to verify trust authentication|password authentication failed|no pg_hba.conf entry/i,
    ],
    ['database-missing', /database ".*" does not exist/i],
    ['foreign-listener', /port is already allocated|address already in use/i],
  ]
  const kind = rules.find(([, pattern]) => pattern.test(String(text)))?.[0]
  return kind
    ? {
        kind,
        remedy:
          kind === 'docker-unavailable'
            ? 'start Docker, or supply a Postgres you may create and drop schemas on via BOSSO_TEST_DATABASE_URL / WAREHOUSE_TEST_ADMIN_URL'
            : suppliedRemedy,
      }
    : null
}
function failure(text, fallback = 'probe-failed') {
  const classified = classifyPostgresProbe(text) || { kind: fallback, remedy: suppliedRemedy }
  return Object.assign(
    new Error(`${TEST_POSTGRES_FAILURE_SIGNATURE} (${classified.kind}) - ${classified.remedy}`),
    classified,
    { exitCode: ENV_FAILURE_EXIT_CODE },
  )
}
export function worktreeIdentity(root) {
  const hash = createHash('sha256').update(root).digest('hex')
  return {
    name: `bossanova-test-postgres-${hash.slice(0, 12)}`,
    port: 20000 + (Number.parseInt(hash.slice(0, 8), 16) % 30000),
  }
}
function portAvailable(port) {
  return new Promise((resolve) => {
    const server = net.createServer()
    server.once('error', () => resolve(false))
    server.listen(port, '127.0.0.1', () => server.close(() => resolve(true)))
  })
}
function tcpReachable(host, port) {
  return new Promise((resolve) => {
    const socket = net.createConnection({ host, port })
    const finish = (ok) => {
      socket.destroy()
      resolve(ok)
    }
    socket.setTimeout(3000)
    socket.once('connect', () => finish(true))
    socket.once('error', () => finish(false))
    socket.once('timeout', () => finish(false))
  })
}
async function inspect(name, run) {
  const result = await run('docker', ['inspect', name])
  if (result.status !== 0) {
    if (/No such (?:object|container)/i.test(output(result))) return null
    throw failure(output(result) + (result.error?.code || ''), 'docker-unavailable')
  }
  return JSON.parse(result.stdout)[0]
}
function assertOwned(info, root) {
  if (info?.Config?.Labels?.[LABEL] !== root)
    throw new Error(
      'container belongs to another worktree or is unlabelled; choose another BOSSO_TEST_PG_CONTAINER',
    )
}
async function removeContainer(name, run) {
  const result = await run('docker', ['rm', '-f', '-v', name])
  if (result.status !== 0 && !/No such (?:object|container)/i.test(output(result))) {
    throw failure(output(result))
  }
}
export async function reapPostgres({ run = defaultRun, pathExists = existsSync } = {}) {
  const list = await run('docker', ['ps', '-aq', '--filter', `label=${LABEL}`])
  if (list.status !== 0) throw failure(output(list), 'docker-unavailable')
  for (const id of (list.stdout || '').trim().split(/\s+/).filter(Boolean)) {
    const info = await inspect(id, run)
    const root = info?.Config?.Labels?.[LABEL]
    if (root && !pathExists(root)) {
      await removeContainer(id, run)
    }
  }
}
async function ownedHealth(name, run) {
  const result = await run('docker', [
    'exec',
    name,
    'psql',
    '-U',
    'postgres',
    '-d',
    'bosso_test',
    '-v',
    'ON_ERROR_STOP=1',
    '-Atc',
    SQL,
  ])
  if (result.status !== 0) return failure(output(result))
  // Connecting to bosso_test proves its existence; postgres is required for warehouse maintenance.
  const admin = await run('docker', [
    'exec',
    name,
    'psql',
    '-U',
    'postgres',
    '-d',
    'postgres',
    '-v',
    'ON_ERROR_STOP=1',
    '-Atc',
    SQL,
  ])
  return admin.status === 0 ? null : failure(output(admin))
}
export async function ensurePostgres(options = {}) {
  const {
    root,
    run = defaultRun,
    waitSeconds = 60,
    sleep: pause = sleep,
    portAvailable: available = portAvailable,
  } = options
  const identity = worktreeIdentity(root)
  const name = options.name || identity.name
  await reapPostgres({ run, pathExists: options.pathExists })
  let lastError
  for (let generation = 0; generation < 2; generation++) {
    let info = await inspect(name, run)
    if (info) assertOwned(info, root)
    if (!info) {
      let created = false
      for (let offset = 0; offset < 100; offset++) {
        const port = options.port ?? 20000 + ((identity.port - 20000 + offset) % 30000)
        if (!(await available(port))) {
          if (options.port) throw failure('address already in use')
          continue
        }
        const createdResult = await run('docker', [
          'run',
          '-d',
          '--name',
          name,
          '--label',
          `${LABEL}=${root}`,
          '-e',
          'POSTGRES_USER=postgres',
          '-e',
          'POSTGRES_PASSWORD=postgres',
          '-e',
          'POSTGRES_DB=bosso_test',
          '-p',
          `127.0.0.1:${port}:5432`,
          'postgres:16',
          '-c',
          'fsync=off',
          '-c',
          'synchronous_commit=off',
          '-c',
          'full_page_writes=off',
          '-c',
          'max_connections=200',
        ])
        if (
          createdResult.status === 0 ||
          /container name.*already in use/i.test(output(createdResult))
        ) {
          created = true
          break
        }
        if (/port is already allocated|address already in use/i.test(output(createdResult))) {
          // A failed Docker port binding can leave the named container behind. Remove only our label.
          const failed = await inspect(name, run)
          if (failed) {
            assertOwned(failed, root)
            await removeContainer(name, run)
          }
          if (options.port) throw failure(output(createdResult))
          continue
        }
        throw failure(
          output(createdResult) + (createdResult.error?.code || ''),
          'docker-unavailable',
        )
      }
      if (!created) throw failure('address already in use')
      info = await inspect(name, run)
      if (!info) throw failure('container disappeared during creation')
      assertOwned(info, root)
    }
    if (!info.State?.Running) {
      const started = await run('docker', ['start', name])
      if (started.status !== 0) lastError = failure(output(started))
    }
    for (let attempt = 0; attempt <= waitSeconds; attempt++) {
      lastError = await ownedHealth(name, run)
      if (!lastError) {
        const published = await run('docker', ['port', name, '5432/tcp'])
        const match = /^127\.0\.0\.1:(\d+)\s*$/m.exec(published.stdout || '')
        if (published.status !== 0 || !match) throw failure('owned container has no loopback port')
        const prefix = `postgres://postgres:postgres@127.0.0.1:${match[1]}`
        return {
          bossoUrl: `${prefix}/bosso_test?sslmode=disable`,
          adminUrl: `${prefix}/postgres?sslmode=disable`,
        }
      }
      if (attempt < waitSeconds) await pause(1000)
    }
    if (generation === 0) {
      const current = await inspect(name, run)
      if (current) {
        assertOwned(current, root)
        await removeContainer(name, run)
      }
    }
  }
  throw lastError
}
export async function downPostgres({ root, name, run = defaultRun } = {}) {
  name ||= worktreeIdentity(root).name
  const info = await inspect(name, run)
  if (!info) return
  assertOwned(info, root)
  await removeContainer(name, run)
}
export async function checkPostgres(
  url,
  { run = defaultRun, tcpReachable: reachable = tcpReachable, warn = (s) => console.error(s) } = {},
) {
  const parsed = new URL(url)
  // libpq reads the credential from the child environment rather than exposing it in process argv.
  const result = await run('psql', ['-v', 'ON_ERROR_STOP=1', '-Atc', SQL], { url })
  if (result.error?.code === 'ENOENT') {
    if (!(await reachable(parsed.hostname, Number(parsed.port || 5432))))
      throw failure('connection refused')
    warn(
      `test-postgres: ${parsed.hostname}:${parsed.port || 5432}${parsed.pathname}: psql unavailable; deeper probes skipped`,
    )
    return
  }
  if (result.status !== 0) throw failure(output(result))
}
export async function main(argv = process.argv.slice(2)) {
  const { values, positionals } = parseArgs({
    args: argv,
    allowPositionals: true,
    strict: true,
    options: {
      print: { type: 'string' },
      name: { type: 'string' },
      port: { type: 'string' },
      url: { type: 'string' },
      'wait-seconds': { type: 'string' },
      help: { type: 'boolean' },
    },
  })
  if (values.help) {
    console.log(
      'test-postgres ensure [--print bosso-url|admin-url] [--name NAME] [--port PORT] [--wait-seconds N]\ntest-postgres check --url URL (read-only; psql or TCP)\ntest-postgres down (owned container and volume only)\nDefault ensure output: export lines for eval "$(make -s postgres-test-up)".',
    )
    return
  }
  if (positionals.length !== 1 || !['ensure', 'check', 'down'].includes(positionals[0]))
    throw new Error('expected ensure, check or down; see --help')
  const run = defaultRun
  if (positionals[0] === 'check') {
    if (!values.url) throw new Error('check requires --url')
    await checkPostgres(values.url, { run })
    return
  }
  const rootResult = await run('git', ['rev-parse', '--show-toplevel'])
  if (rootResult.status !== 0) throw new Error('cannot resolve worktree root')
  const options = { root: rootResult.stdout.trim(), name: values.name, run }
  for (const [key, value] of [
    ['port', values.port],
    ['waitSeconds', values['wait-seconds']],
  ]) {
    if (value !== undefined) {
      const n = Number(value)
      if (
        !Number.isInteger(n) ||
        n < (key === 'port' ? 1 : 0) ||
        n > (key === 'port' ? 65535 : 3600)
      )
        throw new Error(`invalid ${key}`)
      options[key] = n
    }
  }
  if (positionals[0] === 'down') {
    await downPostgres(options)
    return
  }
  if (values.print && !['bosso-url', 'admin-url'].includes(values.print))
    throw new Error('unknown --print value')
  const urls = await ensurePostgres(options)
  if (values.print) console.log(values.print === 'bosso-url' ? urls.bossoUrl : urls.adminUrl)
  else
    console.log(
      `export BOSSO_TEST_DATABASE_URL='${urls.bossoUrl}'\nexport WAREHOUSE_TEST_ADMIN_URL='${urls.adminUrl}'`,
    )
}
if (isMainModule(import.meta.url))
  main().catch((error) => {
    console.error(
      error.exitCode === 75
        ? error.message
        : 'test-postgres: operation failed; see --help and verify container ownership/options',
    )
    process.exitCode = error.exitCode || 1
  })
