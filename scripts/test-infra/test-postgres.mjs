#!/usr/bin/env node
// The test Postgres helper: one entry point that resolves a healthy server for tests and
// verification. A supplied URL is probed first and returned unchanged when healthy; an
// unavailable loopback supplied URL (outside CI) falls back to this worktree's owned instance,
// provisioned through Docker or, when Docker does not answer, natively
// (test-postgres-native.mjs). Every failure is a classified infrastructure result (exit 75).
// Nothing here mutates a server it does not own.
import { spawnSync } from 'node:child_process'
import { existsSync } from 'node:fs'
import net from 'node:net'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import {
  REMEDIES,
  defaultCacheRoot,
  downNative,
  ensureNative,
  instancePaths,
  nativeUrls,
  postgresFailure,
  readInstanceOwner,
  reapNative,
  resolveNativeBinaries,
  steppedPort,
  withInstanceLock,
  worktreeIdentity,
} from './test-postgres-native.mjs'

const LABEL = 'bossanova.test-postgres.worktree'
const SQL =
  "DO $$ BEGIN IF pg_is_in_recovery() THEN RAISE EXCEPTION 'database system is in recovery mode'; END IF; IF current_setting('transaction_read_only') = 'on' THEN RAISE EXCEPTION 'database is read-only'; END IF; END $$; BEGIN; CREATE TEMP TABLE boss_health (id int); INSERT INTO boss_health VALUES (1); ROLLBACK;"
// --require createdb: the role must be able to create databases (control-boss's per-run database).
const CREATEDB_SQL =
  "DO $$ BEGIN IF NOT (SELECT rolcreatedb OR rolsuper FROM pg_roles WHERE rolname = current_user) THEN RAISE EXCEPTION 'role cannot create databases'; END IF; END $$;"
const healthSQL = (requireCreatedb) => (requireCreatedb ? `${CREATEDB_SQL} ${SQL}` : SQL)
// A supplied loopback URL falls back to an owned instance only on these availability classes.
const AVAILABILITY_CLASSES = new Set(['connection-refused', 'recovery-mode', 'database-missing'])
const SUPPLIED_RECOVERY_WAIT_SECONDS = 5
const defaultRun = async (command, args, extra = {}) => {
  let env = extra.env ?? process.env
  if (extra.url) {
    // PGDATABASE treats a URI as a literal database name. Pass the connection
    // explicitly, keeping passwords in the environment rather than process argv.
    const connection = new URL(extra.url)
    const password =
      connection.searchParams.get('password') ?? decodeURIComponent(connection.password)
    connection.password = ''
    connection.searchParams.delete('password')
    args = [...args, '--dbname', connection.toString()]
    env = { ...env, PGPASSWORD: password, PGCONNECT_TIMEOUT: '5' }
  }
  return spawnSync(command, args, { encoding: 'utf8', timeout: extra.timeout ?? 15000, env })
}
const output = (r) => `${r.stdout || ''}\n${r.stderr || ''}`
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

export { worktreeIdentity }

export function classifyPostgresProbe(text) {
  const rules = [
    [
      'docker-unavailable',
      /Cannot connect to the Docker daemon|docker daemon is not running|docker.*ENOENT/i,
    ],
    ['createdb-denied', /role cannot create databases/i],
    // A server that answered and rejected the credentials outranks another address refusing:
    // libpq joins per-address errors, so `localhost` can report ::1 refused then 127.0.0.1 auth.
    [
      'auth-rejected',
      /failed to verify trust authentication|password authentication failed|no pg_hba.conf entry/i,
    ],
    [
      'connection-refused',
      /connection refused|could not connect to server|server closed the connection unexpectedly|connection reset by peer/i,
    ],
    ['too-many-clients', /53300|too many (?:clients|connections)|remaining connection slots/i],
    ['recovery-mode', /57P03|in recovery|starting up|shutting down/i],
    ['read-only', /read.only/i],
    ['disk-full', /No space left on device|ENOSPC|disk full/i],
    ['database-missing', /database ".*" does not exist/i],
    ['foreign-listener', /port is already allocated|address already in use/i],
  ]
  const kind = rules.find(([, pattern]) => pattern.test(String(text)))?.[0]
  return kind ? { kind, remedy: REMEDIES[kind] } : null
}
function failure(text, fallback = 'probe-failed') {
  return postgresFailure(classifyPostgresProbe(text)?.kind ?? fallback)
}
/** `host:port` of a URL, with no credentials. */
export function redactedServer(url) {
  try {
    const u = new URL(url)
    return `${u.hostname}:${u.port || '5432'}`
  } catch {
    return 'unparseable-url'
  }
}
export function isLoopbackHost(hostname) {
  const host = String(hostname).replace(/^\[|\]$/g, '')
  return host === '127.0.0.1' || host === '::1' || host === 'localhost'
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
async function reapDocker({ run = defaultRun, pathExists = existsSync } = {}) {
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
/**
 * Remove owned instances whose worktree is gone: labelled containers (unless `docker` is false)
 * and, when `cacheRoot` is given, native instance directories carrying owner.json.
 */
export async function reapPostgres(options = {}) {
  if (options.docker !== false) await reapDocker(options)
  if (options.cacheRoot) await reapNative({ ...options, cacheRoot: options.cacheRoot })
}
async function ownedHealth(name, run, sql = SQL) {
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
    sql,
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
    sql,
  ])
  return admin.status === 0 ? null : failure(output(admin))
}
async function publishedPort(name, run) {
  const published = await run('docker', ['port', name, '5432/tcp'])
  const match = /^127\.0\.0\.1:(\d+)\s*$/m.exec(published.stdout || '')
  if (published.status !== 0 || !match) throw failure('owned container has no loopback port')
  return match[1]
}
/** The Docker provider: this worktree's labelled container and volume. */
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
  const sql = healthSQL(options.requireCreatedb)
  await reapDocker({ run, pathExists: options.pathExists })
  let lastError
  for (let generation = 0; generation < 2; generation++) {
    let info = await inspect(name, run)
    if (info) assertOwned(info, root)
    if (!info) {
      let created = false
      for (let offset = 0; offset < 100; offset++) {
        const port = options.port ?? steppedPort(identity.port, offset)
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
      lastError = await ownedHealth(name, run, sql)
      if (!lastError) {
        const prefix = `postgres://postgres:postgres@127.0.0.1:${await publishedPort(name, run)}`
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
  if (!info) return false
  assertOwned(info, root)
  await removeContainer(name, run)
  return true
}
/** Read-only probe of a URL: psql when present, else TCP reachability. Never mutates a server. */
export async function checkPostgres(
  url,
  {
    run = defaultRun,
    tcpReachable: reachable = tcpReachable,
    warn = (s) => console.error(s),
    requireCreatedb = false,
    psql = 'psql',
  } = {},
) {
  const parsed = new URL(url)
  // libpq reads the credential from the child environment rather than exposing it in process argv.
  const result = await run(
    psql,
    ['-X', '-v', 'ON_ERROR_STOP=1', '-Atc', healthSQL(requireCreatedb)],
    {
      url,
    },
  )
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

// ---------------------------------------------------------------------------------------------
// Resolution: supplied URL first, then this worktree's owned instance.

function resolved(options) {
  const env = options.env ?? process.env
  const forced = options.provider ?? (env.BOSS_TEST_PG_PROVIDER || undefined)
  if (forced !== undefined && !['docker', 'native'].includes(forced))
    throw new Error('BOSS_TEST_PG_PROVIDER must be docker or native')
  return {
    ...options,
    env,
    forced,
    run: options.run ?? defaultRun,
    cacheRoot: options.cacheRoot ?? defaultCacheRoot(env),
    socketRoot: options.socketRoot ?? '/tmp',
    waitSeconds: options.waitSeconds ?? 60,
    sleep: options.sleep ?? sleep,
    log: options.log ?? ((line) => console.error(line)),
  }
}

/**
 * Docker is used when `docker info` names a server version within a bounded timeout. The CLI
 * exits 0 with an empty version when its daemon socket is gone, so the status alone is not enough.
 */
async function dockerAnswers(run) {
  const info = await run('docker', ['info', '--format', '{{.ServerVersion}}'], { timeout: 10000 })
  return info.status === 0 && Boolean((info.stdout || '').trim())
}

async function probeURL(url, o, psql) {
  try {
    new URL(url)
  } catch {
    return postgresFailure('probe-failed')
  }
  try {
    await checkPostgres(url, {
      run: o.run,
      requireCreatedb: o.requireCreatedb,
      tcpReachable: o.tcpReachable,
      warn: o.log,
      psql,
    })
    return null
  } catch (err) {
    return err.kind ? err : postgresFailure('probe-failed')
  }
}

/** The healthy supplied result, or null to fall back; throws when the failure is fail-closed. */
async function resolveSupplied(o, attempted) {
  const url = o.supplied
  if (!url) return null
  const server = redactedServer(url)
  let err
  for (let attempt = 0; ; attempt++) {
    err = await probeURL(url, o)
    const waits = o.recoveryWaitSeconds ?? SUPPLIED_RECOVERY_WAIT_SECONDS
    if (err?.kind !== 'recovery-mode' || attempt >= waits) break
    await o.sleep(1000)
  }
  if (!err) return { ok: true, provider: 'supplied', server, bossoUrl: url, adminUrl: url }
  attempted.push({ provider: 'supplied', kind: err.kind, server })
  let host = ''
  try {
    host = new URL(url).hostname
  } catch {
    // Unparseable: stays fail-closed below.
  }
  const ci = o.env.CI !== undefined && o.env.CI !== ''
  if (!AVAILABILITY_CLASSES.has(err.kind) || !isLoopbackHost(host) || ci)
    throw postgresFailure(err.kind, { provider: 'supplied', attempted })
  o.log(`test-postgres: supplied ${server} is unavailable (${err.kind}); using an owned instance`)
  return null
}

function ownedResult(provider, urls) {
  return {
    ok: true,
    provider,
    server: redactedServer(urls.bossoUrl),
    bossoUrl: urls.bossoUrl,
    adminUrl: urls.adminUrl,
  }
}

/** Re-raise a classified provider failure carrying the provider and the attempts so far. */
function annotate(err, provider, attempted) {
  if (!err.kind) return err
  return postgresFailure(err.kind, {
    provider,
    attempted: [...attempted, { provider, kind: err.kind }],
  })
}

function lockOptions(o) {
  return {
    timeoutMs: o.lockTimeoutMs ?? (2 * o.waitSeconds + 180) * 1000,
    procs: o.procs,
    sleep: o.sleep,
    now: o.now,
    provider: o.forced ?? 'none',
  }
}

async function ensureOwned(o, attempted) {
  await reapNative({ ...o, cacheRoot: o.cacheRoot })
  return withInstanceLock(
    instancePaths(o.root, o).lock,
    async () => {
      if (o.forced !== 'native') {
        if (await dockerAnswers(o.run)) {
          try {
            return ownedResult('docker', await ensurePostgres(o))
          } catch (err) {
            if (err.kind !== 'docker-unavailable' || o.forced === 'docker')
              throw annotate(err, 'docker', attempted)
          }
        } else if (o.forced === 'docker') {
          throw annotate(postgresFailure('docker-unavailable'), 'docker', attempted)
        }
        // docker-unavailable means "try native", not "fail".
        attempted.push({ provider: 'docker', kind: 'docker-unavailable' })
      }
      try {
        const binaries =
          o.binaries ??
          (await resolveNativeBinaries({ env: o.env, run: o.run, wellKnownDirs: o.wellKnownDirs }))
        const urls = await ensureNative({
          ...o,
          binaries,
          probe: (url) => probeURL(url, o, binaries.psql),
        })
        return ownedResult('native', urls)
      } catch (err) {
        throw annotate(err, 'native', attempted)
      }
    },
    lockOptions(o),
  )
}

/**
 * ensure: a URL only after a probe proved the server reachable, the credentials accepted, a write
 * transaction working and (with requireCreatedb) the role able to create databases. Otherwise a
 * classified infrastructure failure (exit 75).
 */
export async function ensureTestPostgres(options) {
  const o = resolved(options)
  const attempted = []
  try {
    return (await resolveSupplied(o, attempted)) ?? (await ensureOwned(o, attempted))
  } catch (err) {
    if (err.kind && !err.attempted?.length && attempted.length)
      throw postgresFailure(err.kind, { provider: err.provider, attempted })
    throw err
  }
}

/**
 * The read-only form of ensure, for doctor: probes the supplied URL and any existing owned
 * instance, never starting or creating one. `state` is `healthy`, or `provisionable` when no
 * owned instance is up but ensure has a provider that could start one.
 */
export async function probeTestPostgres(options) {
  const o = resolved(options)
  const attempted = []
  const supplied = await resolveSupplied(o, attempted)
  if (supplied) return { ...supplied, state: 'healthy' }
  let docker = false
  if (o.forced !== 'native' && (docker = await dockerAnswers(o.run))) {
    const name = o.name || worktreeIdentity(o.root).name
    const info = await inspect(name, o.run).catch(() => null)
    if (
      info?.Config?.Labels?.[LABEL] === o.root &&
      info.State?.Running &&
      !(await ownedHealth(name, o.run, healthSQL(o.requireCreatedb)))
    ) {
      const port = await publishedPort(name, o.run)
      return { ok: true, provider: 'docker', server: `127.0.0.1:${port}`, state: 'healthy' }
    }
  }
  let binaries = null
  if (o.forced !== 'docker') {
    binaries = await resolveNativeBinaries({
      env: o.env,
      run: o.run,
      wellKnownDirs: o.wellKnownDirs,
    }).catch(() => null)
    const p = instancePaths(o.root, o)
    const owner = readInstanceOwner(p.dir)
    if (binaries && owner?.root === o.root && existsSync(p.ready) && existsSync(p.password)) {
      const urls = nativeUrls(p, owner.port)
      const err =
        (await probeURL(urls.bossoUrl, o, binaries.psql)) ||
        (await probeURL(urls.adminUrl, o, binaries.psql))
      if (!err)
        return { ok: true, provider: 'native', server: `127.0.0.1:${owner.port}`, state: 'healthy' }
    }
  }
  if (docker) return { ok: true, provider: 'docker', server: null, state: 'provisionable' }
  if (binaries) return { ok: true, provider: 'native', server: null, state: 'provisionable' }
  const kind = o.forced === 'docker' ? 'docker-unavailable' : 'no-provider'
  throw postgresFailure(kind, {
    provider: o.forced ?? 'none',
    attempted: [...attempted, { provider: o.forced ?? 'none', kind }],
  })
}

/** Remove this worktree's owned instances (native and Docker). Succeeds when nothing exists. */
export async function downTestPostgres(options) {
  const o = resolved(options)
  return withInstanceLock(
    instancePaths(o.root, o).lock,
    async () => {
      const removed = []
      if (o.forced !== 'docker' && (await downNative(o))) removed.push('native')
      if (o.forced !== 'native') {
        if (await dockerAnswers(o.run)) {
          if (await downPostgres(o)) removed.push('docker')
        } else if (o.forced === 'docker') {
          throw postgresFailure('docker-unavailable', { provider: 'docker' })
        } else {
          o.log('test-postgres: Docker does not answer; no container was checked')
        }
      }
      return { removed }
    },
    lockOptions(o),
  )
}

/** The structured, credential-free form of a classified failure. */
export function infrastructureResult(err) {
  return {
    ok: false,
    category: 'infrastructure',
    kind: err.kind,
    provider: err.provider ?? 'none',
    attempted: (err.attempted ?? []).map(({ provider, kind, server }) =>
      server ? { provider, kind, server } : { provider, kind },
    ),
    remedy: err.remedy ?? REMEDIES[err.kind] ?? REMEDIES['probe-failed'],
  }
}

const HELP = `test-postgres ensure [--supplied URL] [--require createdb] [--json | --print bosso-url|admin-url]
                     [--name NAME] [--port PORT] [--wait-seconds N]
  Probe the supplied URL and return it when healthy; when a loopback supplied URL is unavailable
  (outside CI), or none is supplied, ensure this worktree's owned instance (Docker, else native;
  BOSS_TEST_PG_PROVIDER=docker|native forces one). Default output: export lines for
  eval "$(make -s postgres-test-up)".
test-postgres check --url URL [--require createdb] [--json]   (read-only; psql or TCP)
test-postgres down   (this worktree's owned instances only)
Exit 75 is a classified infrastructure failure; --json prints it as one object on stdout.`

export async function main(
  argv = process.argv.slice(2),
  { env = process.env, run = defaultRun } = {},
) {
  const { values, positionals } = parseArgs({
    args: argv,
    allowPositionals: true,
    strict: true,
    options: {
      print: { type: 'string' },
      name: { type: 'string' },
      port: { type: 'string' },
      url: { type: 'string' },
      supplied: { type: 'string' },
      require: { type: 'string' },
      json: { type: 'boolean' },
      'wait-seconds': { type: 'string' },
      help: { type: 'boolean' },
    },
  })
  if (values.help) {
    console.log(HELP)
    return
  }
  if (positionals.length !== 1 || !['ensure', 'check', 'down'].includes(positionals[0]))
    throw new Error('expected ensure, check or down; see --help')
  if (values.require !== undefined && values.require !== 'createdb')
    throw new Error('--require accepts only createdb')
  if (values.print && !['bosso-url', 'admin-url'].includes(values.print))
    throw new Error('unknown --print value')
  if (values.print && values.json) throw new Error('--print and --json are exclusive')
  const requireCreatedb = values.require === 'createdb'
  try {
    if (positionals[0] === 'check') {
      if (!values.url) throw new Error('check requires --url')
      try {
        await checkPostgres(values.url, { run, requireCreatedb })
      } catch (err) {
        throw err.kind
          ? postgresFailure(err.kind, {
              provider: 'supplied',
              attempted: [
                { provider: 'supplied', kind: err.kind, server: redactedServer(values.url) },
              ],
            })
          : err
      }
      if (values.json) console.log(JSON.stringify({ ok: true, server: redactedServer(values.url) }))
      return
    }
    const rootResult = await run('git', ['rev-parse', '--show-toplevel'])
    if (rootResult.status !== 0) throw new Error('cannot resolve worktree root')
    const options = { root: rootResult.stdout.trim(), name: values.name, run, env, requireCreatedb }
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
      const result = await downTestPostgres(options)
      if (values.json) console.log(JSON.stringify({ ok: true, ...result }))
      return
    }
    const result = await ensureTestPostgres({ ...options, supplied: values.supplied || undefined })
    if (values.json) {
      const { ok, provider, server, bossoUrl, adminUrl } = result
      console.log(JSON.stringify({ ok, provider, server, bossoUrl, adminUrl }))
    } else if (values.print) {
      console.log(values.print === 'bosso-url' ? result.bossoUrl : result.adminUrl)
    } else {
      console.log(
        `export BOSSO_TEST_DATABASE_URL='${result.bossoUrl}'\nexport WAREHOUSE_TEST_ADMIN_URL='${result.adminUrl}'`,
      )
    }
  } catch (err) {
    if (values.json && err.category === 'infrastructure') {
      console.log(JSON.stringify(infrastructureResult(err)))
      // The human line stays on stderr so env-failure classification still sees the signature.
      console.error(err.message)
      process.exitCode = err.exitCode
      return
    }
    throw err
  }
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
