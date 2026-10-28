// The native (non-Docker) owned Postgres provider for scripts/test-infra/test-postgres.mjs, plus
// the pieces both owned providers share: the worktree identity, the infrastructure failure shape
// and the per-instance lock.
//
// A worktree owns at most one native instance, under <cacheRoot>/<hash12>/:
//   owner.json  {root, port, provider: 'native', version}   written before anything else
//   password    0600, the scram password; never in argv, diagnostics or owner.json
//   data/       the cluster; ready marks a completed initdb + first start + bosso_test
//   server.log  pg_ctl's log
// It listens on 127.0.0.1 only, with its unix socket in a private <socketRoot>/bpg-<hash12>.
// Nothing here touches a directory without owner.json, or one whose owner is another worktree.

import { spawnSync } from 'node:child_process'
import { createHash, randomBytes } from 'node:crypto'
import fs from 'node:fs'
import net from 'node:net'
import os from 'node:os'
import path from 'node:path'

import { ENV_FAILURE_EXIT_CODE, TEST_POSTGRES_FAILURE_SIGNATURE } from './env-failure-lib.mjs'

export const MIN_MAJOR_VERSION = 16
export const POSTGRES_APP_BINDIR = '/Applications/Postgres.app/Contents/Versions/latest/bin'

/** Class-specific remedies. None tells the caller to unset variables or run initdb by hand. */
export const REMEDIES = Object.freeze({
  'connection-refused':
    'the server is not accepting connections; start or repair it (in CI, the Postgres service container)',
  'recovery-mode': 'the server is still in recovery; let recovery finish or repair the server',
  'database-missing': 'the database named in the URL does not exist on that server; create it',
  'auth-rejected':
    'the server rejected the credentials in the supplied URL; correct them where the URL is configured',
  'read-only': 'the server is read-only; point the supplied URL at a writable primary',
  'disk-full': 'the database host is out of disk space; free space on it',
  'too-many-clients':
    'the server has no free connection slots; wait for other test runs or raise max_connections',
  'createdb-denied':
    'the role in the supplied URL cannot create databases; grant it CREATEDB on that server',
  'foreign-listener':
    'the requested port is held by another process; free it or choose another BOSSO_TEST_PG_PORT',
  'docker-unavailable':
    'BOSS_TEST_PG_PROVIDER=docker but Docker does not answer; start Docker or allow the native provider',
  'no-provider': `Docker does not answer and no PostgreSQL ${MIN_MAJOR_VERSION}+ server binaries were found; start Docker, or install PostgreSQL ${MIN_MAJOR_VERSION} or newer (Postgres.app or Homebrew postgresql@${MIN_MAJOR_VERSION}+)`,
  'lock-timeout':
    "another run held this worktree's test Postgres lock past the wait; retry once it finishes",
  'foreign-instance':
    'the instance directory for this worktree has no matching owner.json, so it was left untouched; set XDG_CACHE_HOME to use another cache root',
  'server-start-failed':
    'the owned native server failed to start twice; its server.log is in the instance directory under the test-postgres cache root',
  'probe-failed': 'the health probe failed for an unclassified reason; repair the server',
})

/**
 * The classified infrastructure failure both providers and the supplied-URL probe raise. The
 * message keeps TEST_POSTGRES_FAILURE_SIGNATURE so env-failure-lib still classifies it; nothing
 * on it carries a credential (`server` is host:port only).
 */
export function postgresFailure(kind, { provider = 'none', attempted = [] } = {}) {
  const remedy = REMEDIES[kind] ?? REMEDIES['probe-failed']
  return Object.assign(new Error(`${TEST_POSTGRES_FAILURE_SIGNATURE} (${kind}) - ${remedy}`), {
    category: 'infrastructure',
    kind,
    remedy,
    provider,
    attempted,
    exitCode: ENV_FAILURE_EXIT_CODE,
  })
}

export function worktreeIdentity(root) {
  const hash = createHash('sha256').update(root).digest('hex')
  return {
    name: `bossanova-test-postgres-${hash.slice(0, 12)}`,
    port: 20000 + (Number.parseInt(hash.slice(0, 8), 16) % 30000),
    hash: hash.slice(0, 12),
  }
}

/** Port `offset` steps after the worktree's hash-derived port, wrapping inside 20000-49999. */
export function steppedPort(identityPort, offset) {
  return 20000 + ((identityPort - 20000 + offset) % 30000)
}

export function defaultCacheRoot(env = process.env) {
  const base = env.XDG_CACHE_HOME || path.join(env.HOME || os.homedir(), '.cache')
  return path.join(base, 'bossanova', 'test-postgres')
}

export function instancePaths(root, { cacheRoot, socketRoot = '/tmp' }) {
  const { hash } = worktreeIdentity(root)
  const dir = path.join(cacheRoot, hash)
  return {
    hash,
    dir,
    lock: `${dir}.lock`,
    owner: path.join(dir, 'owner.json'),
    password: path.join(dir, 'password'),
    ready: path.join(dir, 'ready'),
    log: path.join(dir, 'server.log'),
    data: path.join(dir, 'data'),
    socketDir: path.join(socketRoot, `bpg-${hash}`),
  }
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

/** True when nothing holds `port` on 127.0.0.1. */
export function loopbackPortFree(port) {
  return new Promise((resolve) => {
    const server = net.createServer()
    server.once('error', () => resolve(false))
    server.listen(port, '127.0.0.1', () => server.close(() => resolve(true)))
  })
}

function pidAlive(pid) {
  if (!Number.isInteger(pid) || pid <= 0) return false
  try {
    process.kill(pid, 0)
    return true
  } catch (err) {
    return err.code === 'EPERM'
  }
}

function pidStartTime(pid) {
  if (!Number.isInteger(pid) || pid <= 0) return null
  const result = spawnSync('ps', ['-o', 'lstart=', '-p', String(pid)], {
    encoding: 'utf8',
    env: { PATH: process.env.PATH ?? '/usr/bin:/bin', LANG: 'C', LC_ALL: 'C' },
  })
  const out = (result.stdout || '').trim()
  return result.status === 0 && out ? out : null
}

/** true when the live pid's argv names dataDir, false when it does not, null when unknown. */
function pidRunsCluster(pid, dataDir) {
  if (!Number.isInteger(pid) || pid <= 0) return null
  const result = spawnSync('ps', ['-o', 'command=', '-p', String(pid)], {
    encoding: 'utf8',
    env: { PATH: process.env.PATH ?? '/usr/bin:/bin', LANG: 'C', LC_ALL: 'C' },
  })
  const out = (result.stdout || '').trim()
  if (result.status !== 0 || !out) return null
  return out.includes(dataDir)
}

export const realProcesses = Object.freeze({
  alive: pidAlive,
  startTime: pidStartTime,
  runsCluster: pidRunsCluster,
})

/** Child env for initdb/pg_ctl: no inherited PG* variable may redirect them. */
function toolEnv(env = process.env) {
  const keep = {}
  for (const key of ['PATH', 'HOME', 'TMPDIR', 'USER']) if (env[key]) keep[key] = env[key]
  return { ...keep, LANG: 'C', LC_ALL: 'C' }
}

const defaultRun = async (command, args, extra = {}) =>
  spawnSync(command, args, {
    encoding: 'utf8',
    timeout: extra.timeout ?? 15000,
    env: extra.env ?? process.env,
    stdio: ['ignore', 'pipe', 'pipe'],
  })

function readJSON(file) {
  try {
    return JSON.parse(fs.readFileSync(file, 'utf8'))
  } catch {
    return null
  }
}

// ---------------------------------------------------------------------------------------------
// Instance lock: <instance dir>.lock, created with mkdir, owner.json {pid, startedAt}.

function lockOwnerStale(lockDir, procs, now) {
  const owner = readJSON(path.join(lockDir, 'owner.json'))
  if (!owner) {
    // A holder between mkdir and its owner write; stale only once that window has long passed.
    let stat
    try {
      stat = fs.statSync(lockDir)
    } catch {
      return null
    }
    return now() - stat.mtimeMs > 30_000 ? {} : null
  }
  if (!procs.alive(owner.pid)) return owner
  const live = procs.startTime(owner.pid)
  if (owner.startedAt != null && live != null && live !== owner.startedAt) return owner
  return null
}

function takeOver(lockDir, stale) {
  const tomb = `${lockDir}.stale-${process.pid}-${randomBytes(4).toString('hex')}`
  try {
    fs.renameSync(lockDir, tomb)
  } catch {
    return // another caller moved or released it first; retry the mkdir
  }
  const moved = readJSON(path.join(tomb, 'owner.json')) ?? {}
  if (moved.pid !== stale.pid || moved.startedAt !== stale.startedAt) {
    // A fresh holder replaced the stale one between the read and the rename: hand it back.
    try {
      fs.renameSync(tomb, lockDir)
    } catch {
      // Its slot is already taken again; leave the tombstone rather than delete a live record.
    }
    return
  }
  fs.rmSync(tomb, { recursive: true, force: true, maxRetries: 3 })
}

/**
 * Run `fn` holding the instance lock. A dead holder, or one whose start time no longer matches
 * its pid, is stale and taken over. The wait is bounded; on timeout the class is lock-timeout.
 */
export async function withInstanceLock(
  lockDir,
  fn,
  {
    timeoutMs = 300_000,
    pollMs = 200,
    procs = realProcesses,
    sleep: pause = sleep,
    now = Date.now,
    provider = 'none',
  } = {},
) {
  fs.mkdirSync(path.dirname(lockDir), { recursive: true, mode: 0o700 })
  const me = { pid: process.pid, startedAt: procs.startTime(process.pid) }
  const deadline = now() + timeoutMs
  for (;;) {
    try {
      fs.mkdirSync(lockDir, { mode: 0o700 })
      fs.writeFileSync(path.join(lockDir, 'owner.json'), JSON.stringify(me), { mode: 0o600 })
      break
    } catch (err) {
      if (err.code !== 'EEXIST') throw err
    }
    const stale = lockOwnerStale(lockDir, procs, now)
    if (stale) {
      takeOver(lockDir, stale)
      continue
    }
    if (now() >= deadline) throw postgresFailure('lock-timeout', { provider })
    await pause(pollMs)
  }
  try {
    return await fn()
  } finally {
    const owner = readJSON(path.join(lockDir, 'owner.json'))
    if (owner?.pid === me.pid && owner?.startedAt === me.startedAt) {
      fs.rmSync(lockDir, { recursive: true, force: true, maxRetries: 3 })
    }
  }
}

// ---------------------------------------------------------------------------------------------
// Binaries.

async function defaultWellKnownDirs({ run }) {
  const dirs = [POSTGRES_APP_BINDIR]
  const brew = await run('brew', ['--prefix'], { timeout: 10000 })
  if (brew.status === 0 && brew.stdout?.trim()) {
    const opt = path.join(brew.stdout.trim(), 'opt')
    let names = []
    try {
      names = fs.readdirSync(opt).filter((name) => /^postgresql/.test(name))
    } catch {
      // No Homebrew opt directory.
    }
    const version = (name) => Number(/@(\d+)/.exec(name)?.[1] ?? 0)
    names.sort((a, b) => version(b) - version(a))
    dirs.push(...names.map((name) => path.join(opt, name, 'bin')))
  }
  return dirs
}

/**
 * initdb, pg_ctl, postgres and psql from one bin directory, searched on PATH, then
 * `pg_config --bindir`, then Postgres.app, then Homebrew. Below MIN_MAJOR_VERSION is skipped;
 * none found is the class no-provider.
 */
export async function resolveNativeBinaries({
  env = process.env,
  run = defaultRun,
  exists = fs.existsSync,
  wellKnownDirs = defaultWellKnownDirs,
} = {}) {
  const pathDirs = (env.PATH ?? '').split(path.delimiter).filter(Boolean)
  const sources = [
    async () => pathDirs.filter((dir) => exists(path.join(dir, 'initdb'))),
    async () => {
      const pgConfig = pathDirs.map((dir) => path.join(dir, 'pg_config')).find(exists)
      if (!pgConfig) return []
      const out = await run(pgConfig, ['--bindir'])
      return out.status === 0 && out.stdout?.trim() ? [out.stdout.trim()] : []
    },
    async () => wellKnownDirs({ run }),
  ]
  const seen = new Set()
  for (const source of sources) {
    for (const bindir of await source()) {
      if (seen.has(bindir)) continue
      seen.add(bindir)
      const bins = {
        initdb: path.join(bindir, 'initdb'),
        pgCtl: path.join(bindir, 'pg_ctl'),
        postgres: path.join(bindir, 'postgres'),
        psql: path.join(bindir, 'psql'),
      }
      if (!Object.values(bins).every((file) => exists(file))) continue
      const version = await run(bins.postgres, ['--version'])
      const major = Number(/(\d+)(?:\.\d+)*/.exec(version.stdout || '')?.[1])
      if (version.status === 0 && major >= MIN_MAJOR_VERSION) return { bindir, ...bins, major }
    }
  }
  throw postgresFailure('no-provider', { provider: 'native' })
}

// ---------------------------------------------------------------------------------------------
// The instance.

export function readInstanceOwner(dir) {
  const owner = readJSON(path.join(dir, 'owner.json'))
  return owner && typeof owner.root === 'string' && owner.provider === 'native' ? owner : null
}

function writeOwner(p, owner) {
  const tmp = `${p.owner}.tmp-${process.pid}`
  fs.writeFileSync(tmp, `${JSON.stringify(owner)}\n`, { mode: 0o600 })
  fs.renameSync(tmp, p.owner)
}

/** Create the instance directory atomically, so it never exists without owner.json. */
function createInstanceDir(p, owner) {
  fs.mkdirSync(path.dirname(p.dir), { recursive: true, mode: 0o700 })
  const staging = fs.mkdtempSync(`${p.dir}.new-`)
  fs.writeFileSync(path.join(staging, 'owner.json'), `${JSON.stringify(owner)}\n`, { mode: 0o600 })
  fs.renameSync(staging, p.dir)
}

/** The private socket directory: ours, a real directory, mode 0700. */
function ensureSocketDir(dir) {
  try {
    fs.mkdirSync(dir, { mode: 0o700 })
  } catch (err) {
    if (err.code !== 'EEXIST') throw err
  }
  const stat = fs.lstatSync(dir)
  if (!stat.isDirectory() || (process.getuid && stat.uid !== process.getuid()))
    throw postgresFailure('foreign-instance', { provider: 'native' })
  fs.chmodSync(dir, 0o700)
}

function serverConf(port, socketDir) {
  const quote = (value) => `'${String(value).replaceAll("'", "''")}'`
  return [
    `listen_addresses = '127.0.0.1'`,
    `port = ${port}`,
    `unix_socket_directories = ${quote(socketDir)}`,
    'fsync = off',
    'synchronous_commit = off',
    'full_page_writes = off',
    'max_connections = 200',
    '',
  ].join('\n')
}

function postmasterPid(p) {
  let text
  try {
    text = fs.readFileSync(path.join(p.data, 'postmaster.pid'), 'utf8')
  } catch {
    return undefined
  }
  const [pidLine = '', dataLine = ''] = text.split('\n')
  return { text, pid: Number.parseInt(pidLine, 10), dataDir: dataLine.trim() }
}

/**
 * Whether postmaster.pid names this cluster's live postmaster: true, false (empty, dead, or a
 * reused pid whose process is not this cluster's postgres), or null when ps cannot tell.
 */
function postmasterIsOurs(record, p, procs) {
  if (!record || record.text.trim() === '' || !Number.isInteger(record.pid)) return false
  if (!procs.alive(record.pid)) return false
  if (!procs.runsCluster) return true
  return procs.runsCluster(record.pid, record.dataDir || p.data)
}

/** Remove postmaster.pid only inside the owned data dir, and only when it names no live server of this cluster. */
export function clearStalePostmasterPid(p, procs = realProcesses) {
  const record = postmasterPid(p)
  if (!record) return false
  if (postmasterIsOurs(record, p, procs) !== false) return false
  fs.rmSync(path.join(p.data, 'postmaster.pid'), { force: true })
  return true
}

async function serverRunning(p, bins, run, env, procs = realProcesses) {
  if (!fs.existsSync(p.data)) return false
  // pg_ctl status trusts kill(pid, 0), so a reused pid would read as this server running.
  if (postmasterIsOurs(postmasterPid(p), p, procs) === false) return false
  const status = await run(bins.pgCtl, ['status', '-D', p.data], { env: toolEnv(env) })
  return status.status === 0
}

/** Stop the owned cluster through pg_ctl. Nothing else is ever signalled. */
async function stopServer(p, bins, { run, env, procs = realProcesses }, mode) {
  if (postmasterIsOurs(postmasterPid(p), p, procs) !== true) return
  await run(bins.pgCtl, ['stop', '-D', p.data, '-m', mode, '-w', '-t', '60'], {
    env: toolEnv(env),
    timeout: 70_000,
  })
}

async function wipeCluster(p, bins, o) {
  await stopServer(p, bins, o, 'immediate')
  for (const file of [p.ready, p.password]) fs.rmSync(file, { force: true })
  fs.rmSync(p.data, { recursive: true, force: true, maxRetries: 3 })
}

async function initCluster(p, bins, run, env) {
  fs.writeFileSync(p.password, randomBytes(24).toString('hex'), { mode: 0o600 })
  fs.chmodSync(p.password, 0o600)
  const init = await run(
    bins.initdb,
    [
      '-D',
      p.data,
      '-U',
      'postgres',
      '--auth-local=trust',
      '--auth-host=scram-sha-256',
      `--pwfile=${p.password}`,
      '-E',
      'UTF8',
      '--no-locale',
      '--no-sync',
      '--no-instructions',
    ],
    { env: toolEnv(env), timeout: 120_000 },
  )
  if (init.status !== 0) throw postgresFailure('server-start-failed', { provider: 'native' })
  fs.appendFileSync(
    path.join(p.data, 'postgresql.conf'),
    "\ninclude_if_exists = 'boss-test.conf'\n",
  )
}

function logTail(p) {
  try {
    return fs.readFileSync(p.log, 'utf8').slice(-4096)
  } catch {
    return ''
  }
}

/** Start the cluster, stepping past a held or unbindable port. Returns the port it listens on. */
async function startServer(p, bins, owner, o) {
  const identity = worktreeIdentity(owner.root)
  let offset = (owner.port - identity.port + 30000) % 30000
  const waitSeconds = Math.max(1, o.waitSeconds)
  for (let tries = 0; tries < 100; tries++, offset++) {
    const port = o.port ?? steppedPort(identity.port, offset)
    if (!(await o.portAvailable(port))) {
      if (o.port) throw postgresFailure('foreign-listener', { provider: 'native' })
      continue
    }
    if (port !== owner.port) {
      owner.port = port
      writeOwner(p, owner)
    }
    fs.writeFileSync(path.join(p.data, 'boss-test.conf'), serverConf(port, p.socketDir))
    const logStart = logTail(p).length
    const started = await o.run(
      bins.pgCtl,
      ['start', '-D', p.data, '-l', p.log, '-w', '-t', String(waitSeconds), '-s'],
      { env: toolEnv(o.env), timeout: (waitSeconds + 10) * 1000 },
    )
    if (started.status === 0) return port
    const tail = logTail(p).slice(logStart) + (started.stderr || '')
    if (!/could not bind|address already in use|could not create any TCP\/IP sockets/i.test(tail))
      throw postgresFailure('server-start-failed', { provider: 'native' })
    if (o.port) throw postgresFailure('foreign-listener', { provider: 'native' })
    // Another process bound the port between the check and the start: step to the next one.
  }
  throw postgresFailure('foreign-listener', { provider: 'native' })
}

export function nativeUrls(p, port) {
  const password = fs.readFileSync(p.password, 'utf8').trim()
  const prefix = `postgres://postgres:${encodeURIComponent(password)}@127.0.0.1:${port}`
  return {
    bossoUrl: `${prefix}/bosso_test?sslmode=disable`,
    adminUrl: `${prefix}/postgres?sslmode=disable`,
  }
}

async function createBossoTest(p, bins, port, run, env) {
  const result = await run(
    bins.psql,
    [
      '-X',
      '-h',
      p.socketDir,
      '-p',
      String(port),
      '-U',
      'postgres',
      '-d',
      'postgres',
      '-v',
      'ON_ERROR_STOP=1',
      '-Atc',
      'CREATE DATABASE bosso_test',
    ],
    { env: toolEnv(env) },
  )
  if (result.status !== 0 && !/already exists/.test(`${result.stdout}${result.stderr}`))
    throw postgresFailure('server-start-failed', { provider: 'native' })
}

/**
 * Ensure this worktree's native instance is initialised, running and healthy. `probe(url)` is the
 * caller's health probe; it resolves null when healthy or a classified error. An instance without
 * its ready marker, or still unhealthy after the bounded wait, is wiped and initialised once more.
 * Callers hold the instance lock.
 */
export async function ensureNative(options) {
  const {
    root,
    cacheRoot,
    socketRoot = '/tmp',
    run = defaultRun,
    env = process.env,
    waitSeconds = 60,
    sleep: pause = sleep,
    procs = realProcesses,
    probe,
  } = options
  const o = {
    ...options,
    run,
    env,
    procs,
    waitSeconds,
    portAvailable: options.portAvailable ?? loopbackPortFree,
  }
  const p = instancePaths(root, { cacheRoot, socketRoot })
  const bins = options.binaries ?? (await resolveNativeBinaries({ env, run }))
  let owner = readInstanceOwner(p.dir)
  if (fs.existsSync(p.dir) && (!owner || owner.root !== root))
    throw postgresFailure('foreign-instance', { provider: 'native' })
  if (!owner) {
    owner = {
      root,
      port: options.port ?? worktreeIdentity(root).port,
      provider: 'native',
      version: bins.major,
    }
    createInstanceDir(p, owner)
  }
  ensureSocketDir(p.socketDir)
  let lastError
  for (let generation = 0; generation < 2; generation++) {
    try {
      if (!fs.existsSync(p.ready)) {
        // No marker: a fresh instance or an interrupted startup. Start from an empty cluster.
        await wipeCluster(p, bins, o)
        await initCluster(p, bins, run, env)
      }
      let port = owner.port
      if (!(await serverRunning(p, bins, run, env, procs))) {
        clearStalePostmasterPid(p, procs)
        port = await startServer(p, bins, owner, o)
      }
      if (!fs.existsSync(p.ready)) {
        await createBossoTest(p, bins, port, run, env)
        fs.writeFileSync(p.ready, `${new Date().toISOString()}\n`)
      }
      const urls = nativeUrls(p, port)
      for (let attempt = 0; attempt <= waitSeconds; attempt++) {
        lastError = (await probe(urls.bossoUrl)) || (await probe(urls.adminUrl))
        if (!lastError) return { ...urls, port }
        if (attempt < waitSeconds) await pause(1000)
      }
    } catch (err) {
      if (!['server-start-failed'].includes(err.kind)) throw err
      lastError = err
    }
    if (generation === 0) await wipeCluster(p, bins, o)
  }
  throw lastError
}

function removeSocketDir(dir) {
  try {
    const stat = fs.lstatSync(dir)
    if (stat.isDirectory() && (!process.getuid || stat.uid === process.getuid()))
      fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  } catch {
    // Absent.
  }
}

/** Stop and remove an owned instance. Idempotent; never touches a dir without matching owner. */
export async function downNative({
  root,
  cacheRoot,
  socketRoot = '/tmp',
  run = defaultRun,
  env = process.env,
  binaries,
  procs = realProcesses,
}) {
  const p = instancePaths(root, { cacheRoot, socketRoot })
  const owner = readInstanceOwner(p.dir)
  if (!owner) return false
  if (owner.root !== root) throw postgresFailure('foreign-instance', { provider: 'native' })
  let bins = binaries
  if (!bins) {
    try {
      bins = await resolveNativeBinaries({ env, run })
    } catch {
      bins = { pgCtl: 'pg_ctl' }
    }
  }
  await stopServer(p, bins, { run, env, procs }, 'fast')
  fs.rmSync(p.dir, { recursive: true, force: true, maxRetries: 3 })
  removeSocketDir(p.socketDir)
  return true
}

/** Remove native instances whose recorded worktree root no longer exists, each under its lock. */
export async function reapNative({
  cacheRoot,
  socketRoot = '/tmp',
  pathExists = fs.existsSync,
  run = defaultRun,
  env = process.env,
  procs = realProcesses,
  binaries,
}) {
  let entries = []
  try {
    entries = fs.readdirSync(cacheRoot, { withFileTypes: true })
  } catch {
    return []
  }
  const reaped = []
  for (const entry of entries) {
    if (!entry.isDirectory() || !/^[0-9a-f]{12}$/.test(entry.name)) continue
    const dir = path.join(cacheRoot, entry.name)
    const owner = readInstanceOwner(dir)
    if (!owner || pathExists(owner.root)) continue
    try {
      await withInstanceLock(
        `${dir}.lock`,
        async () => {
          const current = readInstanceOwner(dir)
          if (!current || pathExists(current.root)) return
          if (instancePaths(current.root, { cacheRoot, socketRoot }).dir !== dir) return
          await downNative({
            root: current.root,
            cacheRoot,
            socketRoot,
            run,
            env,
            binaries,
            procs,
          })
          reaped.push(dir)
        },
        { timeoutMs: 0, procs },
      )
    } catch (err) {
      if (err.kind !== 'lock-timeout') throw err
    }
  }
  return reaped
}
