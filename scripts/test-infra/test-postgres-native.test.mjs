import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'

import {
  clearStalePostmasterPid,
  instancePaths,
  resolveNativeBinaries,
  withInstanceLock,
  worktreeIdentity,
} from './test-postgres-native.mjs'
import { downTestPostgres, ensureTestPostgres, reapPostgres } from './test-postgres.mjs'

// Every root below is a mkdtemp directory: the cache root, the socket root and the worktree
// roots. No test reaches the operator's ~/.cache, /tmp/bpg-* or a real server except the one
// real-native test, which also runs only inside its own temp roots and downs its instance.

const DEAD_PID = 2 ** 22 + 12345
// The simulated postmaster's pid: "alive" only through the injected process table.
const FAKE_POSTMASTER = 2 ** 22 + 777
const fakeProcs = {
  alive: (pid) => pid === process.pid || pid === FAKE_POSTMASTER,
  startTime: (pid) => (pid === process.pid ? 'me' : 'postmaster'),
}
const fastSleep = () => new Promise((resolve) => setTimeout(resolve, 1))

function tempDir(t, prefix, base = os.tmpdir()) {
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(base, prefix)))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }))
  return dir
}

function fakeBindir(t, version = '18.0') {
  const dir = tempDir(t, 'tpg-bin-')
  for (const name of ['initdb', 'pg_ctl', 'postgres', 'psql'])
    fs.writeFileSync(path.join(dir, name), '', { mode: 0o755 })
  fs.writeFileSync(path.join(dir, 'VERSION'), version)
  return dir
}

/**
 * A simulated native toolchain over the temp instance directory: initdb writes a cluster,
 * pg_ctl starts/stops a "server" bound to the port in boss-test.conf, and psql probes it.
 */
function nativeWorld(t, { probeFailures = 0, held = new Set(), bindFailures = new Set() } = {}) {
  const world = {
    running: new Map(), // data dir -> port
    calls: [],
    probeFailures,
    alwaysFail: false,
    held,
    bindFailures,
  }
  const portOf = (data) =>
    Number(/port = (\d+)/.exec(fs.readFileSync(path.join(data, 'boss-test.conf'), 'utf8'))[1])
  world.run = async (cmd, args, extra = {}) => {
    const name = path.basename(cmd)
    world.calls.push({ name, args, env: extra.env })
    const flag = (f) => args[args.indexOf(f) + 1]
    if (name === 'docker') return { status: 1, stderr: 'Cannot connect to the Docker daemon' }
    if (name === 'postgres') {
      const version = fs.readFileSync(path.join(path.dirname(cmd), 'VERSION'), 'utf8')
      return { status: 0, stdout: `postgres (PostgreSQL) ${version}\n` }
    }
    if (name === 'initdb') {
      const data = flag('-D')
      fs.mkdirSync(data, { recursive: true })
      fs.writeFileSync(path.join(data, 'PG_VERSION'), '18\n')
      fs.writeFileSync(path.join(data, 'postgresql.conf'), '# initdb\n')
      return { status: 0, stdout: '' }
    }
    if (name === 'pg_ctl') {
      const data = flag('-D')
      if (args[0] === 'status') return { status: world.running.has(data) ? 0 : 3 }
      if (args[0] === 'stop') {
        if (!world.running.has(data)) return { status: 1, stderr: 'no server running' }
        world.running.delete(data)
        fs.rmSync(path.join(data, 'postmaster.pid'), { force: true })
        return { status: 0 }
      }
      if (args[0] === 'start') {
        const port = portOf(data)
        if (fs.existsSync(path.join(data, 'postmaster.pid')))
          return { status: 1, stderr: 'lock file "postmaster.pid" already exists' }
        if (world.bindFailures.has(port)) {
          world.bindFailures.delete(port)
          fs.appendFileSync(
            flag('-l'),
            `LOG:  could not bind IPv4 address "127.0.0.1": Address already in use\n`,
          )
          return { status: 1, stderr: 'could not start server' }
        }
        world.running.set(data, port)
        fs.writeFileSync(path.join(data, 'postmaster.pid'), `${FAKE_POSTMASTER}\n${data}\n`)
        return { status: 0 }
      }
    }
    if (name === 'psql') {
      if (!extra.url) return { status: 0, stdout: '' } // CREATE DATABASE over the socket
      const port = Number(new URL(extra.url).port)
      if (![...world.running.values()].includes(port))
        return {
          status: 2,
          stderr: `connection to server at "127.0.0.1", port ${port} failed: Connection refused`,
        }
      if (world.alwaysFail || world.probeFailures > 0) {
        world.probeFailures--
        return { status: 2, stderr: 'FATAL:  the database system is starting up' }
      }
      return { status: 0, stdout: '' }
    }
    return { status: 127, error: { code: 'ENOENT' } }
  }
  world.portAvailable = async (port) => !world.held.has(port)
  // A clean shutdown of every simulated server: the postmaster removes its pid file.
  world.stopAll = () => {
    for (const data of world.running.keys())
      fs.rmSync(path.join(data, 'postmaster.pid'), { force: true })
    world.running.clear()
  }
  return world
}

function setup(t, worldOptions) {
  const cacheRoot = tempDir(t, 'tpg-cache-')
  const socketRoot = tempDir(t, 'tpg-sock-', '/tmp')
  const root = tempDir(t, 'tpg-root-')
  const bindir = fakeBindir(t)
  const world = nativeWorld(t, worldOptions)
  const logs = []
  const base = {
    root,
    cacheRoot,
    socketRoot,
    run: world.run,
    env: { PATH: bindir, HOME: os.tmpdir() },
    wellKnownDirs: async () => [],
    portAvailable: world.portAvailable,
    sleep: fastSleep,
    procs: fakeProcs,
    waitSeconds: 3,
    log: (line) => logs.push(line),
  }
  return {
    cacheRoot,
    socketRoot,
    root,
    bindir,
    world,
    logs,
    base,
    p: instancePaths(root, { cacheRoot, socketRoot }),
  }
}

test('binaries resolve from PATH, skip majors below 16, and fail as no-provider', async (t) => {
  const old = fakeBindir(t, '14.9')
  const fresh = fakeBindir(t, '17.2')
  const { run } = nativeWorld(t)
  const bins = await resolveNativeBinaries({
    env: { PATH: [old, fresh].join(path.delimiter) },
    run,
    wellKnownDirs: async () => [],
  })
  assert.equal(bins.bindir, fresh)
  assert.equal(bins.major, 17)
  const wellKnown = await resolveNativeBinaries({
    env: { PATH: '' },
    run,
    wellKnownDirs: async () => [fresh],
  })
  assert.equal(wellKnown.bindir, fresh)
  await assert.rejects(
    resolveNativeBinaries({ env: { PATH: old }, run, wellKnownDirs: async () => [] }),
    (err) => err.kind === 'no-provider' && err.exitCode === 75 && /Docker/.test(err.remedy),
  )
})

test('with Docker unavailable, ensure provisions a loopback native instance with a private socket and a secret password', async (t) => {
  const s = setup(t)
  const result = await ensureTestPostgres(s.base)
  assert.equal(result.provider, 'native')
  const identity = worktreeIdentity(s.root)
  assert.equal(result.server, `127.0.0.1:${identity.port}`)
  const url = new URL(result.bossoUrl)
  assert.equal(url.hostname, '127.0.0.1')
  assert.equal(url.pathname, '/bosso_test')
  const password = fs.readFileSync(s.p.password, 'utf8')
  assert.ok(password.length >= 32)
  assert.equal(decodeURIComponent(url.password), password)
  assert.equal(fs.statSync(s.p.password).mode & 0o777, 0o600)
  assert.equal(fs.statSync(s.p.socketDir).mode & 0o777, 0o700)
  assert.equal(path.dirname(s.p.socketDir), s.socketRoot)
  const conf = fs.readFileSync(path.join(s.p.data, 'boss-test.conf'), 'utf8')
  assert.match(conf, /listen_addresses = '127\.0\.0\.1'/)
  assert.ok(conf.includes(`unix_socket_directories = '${s.p.socketDir}'`))
  assert.match(conf, /fsync = off/)
  assert.match(conf, /max_connections = 200/)
  const initdb = s.world.calls.find((c) => c.name === 'initdb')
  assert.ok(initdb.args.includes('--auth-host=scram-sha-256'))
  assert.ok(initdb.args.includes(`--pwfile=${s.p.password}`))
  // The password reaches only the URL handed to the caller.
  for (const call of s.world.calls) assert.ok(!call.args.join(' ').includes(password), call.name)
  assert.ok(!fs.readFileSync(s.p.owner, 'utf8').includes(password))
  assert.ok(!s.logs.join('\n').includes(password))
  assert.deepEqual(JSON.parse(fs.readFileSync(s.p.owner, 'utf8')), {
    root: s.root,
    port: identity.port,
    provider: 'native',
    version: 18,
  })
  // Re-ensuring reuses the instance: same URL, no second initdb.
  assert.equal((await ensureTestPostgres(s.base)).bossoUrl, result.bossoUrl)
  assert.equal(s.world.calls.filter((c) => c.name === 'initdb').length, 1)
})

test('a held port and a bind failure both step to the next hash-derived port', async (t) => {
  const identity = (root) => worktreeIdentity(root).port
  const held = setup(t)
  held.world.held.add(identity(held.root))
  const first = await ensureTestPostgres(held.base)
  assert.equal(first.server, `127.0.0.1:${identity(held.root) + 1}`)
  assert.equal(JSON.parse(fs.readFileSync(held.p.owner, 'utf8')).port, identity(held.root) + 1)

  const bind = setup(t)
  bind.world.bindFailures.add(identity(bind.root))
  const second = await ensureTestPostgres(bind.base)
  assert.equal(second.server, `127.0.0.1:${identity(bind.root) + 1}`)
})

test('delayed readiness recovers within the wait without replacing the instance', async (t) => {
  const s = setup(t, { probeFailures: 2 })
  assert.equal((await ensureTestPostgres(s.base)).provider, 'native')
  assert.equal(s.world.calls.filter((c) => c.name === 'initdb').length, 1)
})

test('a stopped owned server and a stale or empty postmaster.pid are restarted in place', async (t) => {
  for (const pidFile of [null, `${DEAD_PID}\n`, '']) {
    const s = setup(t)
    const first = await ensureTestPostgres(s.base)
    s.world.stopAll()
    if (pidFile !== null) fs.writeFileSync(path.join(s.p.data, 'postmaster.pid'), pidFile)
    const again = await ensureTestPostgres(s.base)
    assert.equal(again.bossoUrl, first.bossoUrl, `pid file ${JSON.stringify(pidFile)}`)
    assert.equal(s.world.calls.filter((c) => c.name === 'initdb').length, 1)
    assert.equal(
      s.world.calls.filter((c) => c.name === 'pg_ctl' && c.args[0] === 'start').length,
      2,
    )
  }
})

test('a postmaster.pid is cleared only when empty or its pid is dead', (t) => {
  const s = setup(t)
  fs.mkdirSync(s.p.data, { recursive: true })
  const pidFile = path.join(s.p.data, 'postmaster.pid')
  for (const [content, cleared] of [
    [`${FAKE_POSTMASTER}\n${s.p.data}\n`, false],
    [`${DEAD_PID}\n${s.p.data}\n`, true],
    ['', true],
  ]) {
    fs.writeFileSync(pidFile, content)
    assert.equal(clearStalePostmasterPid(s.p, fakeProcs), cleared, JSON.stringify(content))
    assert.equal(fs.existsSync(pidFile), !cleared)
  }
})

test('an interrupted initdb (no ready marker) is wiped and initialised again', async (t) => {
  const s = setup(t)
  const identity = worktreeIdentity(s.root)
  fs.mkdirSync(path.join(s.p.data), { recursive: true })
  fs.writeFileSync(
    s.p.owner,
    JSON.stringify({ root: s.root, port: identity.port, provider: 'native', version: 18 }),
  )
  fs.writeFileSync(path.join(s.p.data, 'half-written'), 'x')
  await ensureTestPostgres(s.base)
  assert.equal(fs.existsSync(path.join(s.p.data, 'half-written')), false)
  assert.ok(fs.existsSync(s.p.ready))
})

test('a persistently unhealthy instance is replaced once, then fails classified', async (t) => {
  const s = setup(t)
  s.world.alwaysFail = true
  await assert.rejects(
    ensureTestPostgres(s.base),
    (err) =>
      err.kind === 'recovery-mode' &&
      err.exitCode === 75 &&
      err.provider === 'native' &&
      err.attempted.some((a) => a.provider === 'docker' && a.kind === 'docker-unavailable'),
  )
  assert.equal(s.world.calls.filter((c) => c.name === 'initdb').length, 2)
})

test('an instance directory without a matching owner.json is never touched', async (t) => {
  const s = setup(t)
  fs.mkdirSync(s.p.dir, { recursive: true })
  fs.writeFileSync(path.join(s.p.dir, 'precious'), 'keep')
  await assert.rejects(ensureTestPostgres(s.base), (err) => err.kind === 'foreign-instance')
  await downTestPostgres(s.base)
  await reapPostgres({
    cacheRoot: s.cacheRoot,
    socketRoot: s.socketRoot,
    docker: false,
    pathExists: () => false,
    run: s.world.run,
  })
  assert.equal(fs.readFileSync(path.join(s.p.dir, 'precious'), 'utf8'), 'keep')
  assert.equal(s.world.calls.filter((c) => ['initdb', 'pg_ctl'].includes(c.name)).length, 0)
})

test('down stops and removes the owned instance and is idempotent', async (t) => {
  const s = setup(t)
  await ensureTestPostgres(s.base)
  assert.deepEqual(await downTestPostgres(s.base), { removed: ['native'] })
  assert.equal(fs.existsSync(s.p.dir), false)
  assert.equal(fs.existsSync(s.p.socketDir), false)
  assert.equal(s.world.running.size, 0)
  assert.deepEqual(await downTestPostgres(s.base), { removed: [] })
})

test('a postmaster.pid whose pid another process reused is stale, and that process is never signalled', async (t) => {
  const s = setup(t)
  await ensureTestPostgres(s.base)
  // An unclean stop: the server is gone but postmaster.pid stays, and its pid now belongs to an
  // unrelated live process. pg_ctl status trusts kill(pid, 0), so it reports a running server.
  s.world.running.delete(s.p.data)
  const run = async (cmd, args, extra) =>
    path.basename(cmd) === 'pg_ctl' && args[0] === 'status'
      ? { status: fs.existsSync(path.join(s.p.data, 'postmaster.pid')) ? 0 : 3 }
      : s.world.run(cmd, args, extra)
  const procs = { ...fakeProcs, runsCluster: () => false }
  s.world.calls.length = 0
  const result = await ensureTestPostgres({ ...s.base, run, procs })
  assert.equal(result.provider, 'native')
  const pgCtl = s.world.calls.filter((c) => c.name === 'pg_ctl').map((c) => c.args[0])
  assert.deepEqual(pgCtl, ['start'])
  assert.equal(s.world.calls.filter((c) => c.name === 'initdb').length, 0)
  assert.equal(clearStalePostmasterPid(s.p, { ...fakeProcs, runsCluster: () => null }), false)
  assert.deepEqual(await downTestPostgres({ ...s.base, run, procs }), { removed: ['native'] })
  assert.equal(
    s.world.calls.filter((c) => c.name === 'pg_ctl' && c.args[0] === 'stop').length,
    0,
    'down never signals a pid that is not this cluster',
  )
})

test('a Docker CLI whose daemon is gone counts as unavailable for ensure and down', async (t) => {
  // With its daemon socket missing, `docker info --format '{{.ServerVersion}}'` exits 0 and
  // prints an empty version (Docker CLI 24 on delta); every other docker call fails.
  const s = setup(t)
  const run = async (cmd, args, extra) =>
    path.basename(cmd) === 'docker' && args[0] === 'info'
      ? { status: 0, stdout: '\n', stderr: 'Cannot connect to the Docker daemon' }
      : s.world.run(cmd, args, extra)
  const options = { ...s.base, run }
  assert.equal((await ensureTestPostgres(options)).provider, 'native')
  assert.deepEqual(await downTestPostgres(options), { removed: ['native'] })
  assert.deepEqual(await downTestPostgres(options), { removed: [] })
})

test('two concurrent ensure calls for one worktree converge on one instance', async (t) => {
  const s = setup(t)
  const [a, b] = await Promise.all([ensureTestPostgres(s.base), ensureTestPostgres(s.base)])
  assert.equal(a.bossoUrl, b.bossoUrl)
  assert.equal(s.world.calls.filter((c) => c.name === 'initdb').length, 1)
  assert.equal(fs.existsSync(s.p.lock), false, 'lock released')
})

test('a dead-holder lock is taken over; a live holder times out as lock-timeout', async (t) => {
  const s = setup(t)
  fs.mkdirSync(s.p.lock, { recursive: true })
  fs.writeFileSync(
    path.join(s.p.lock, 'owner.json'),
    JSON.stringify({ pid: DEAD_PID, startedAt: 'then' }),
  )
  assert.equal((await ensureTestPostgres(s.base)).provider, 'native')
  assert.equal(fs.existsSync(s.p.lock), false)

  const lockDir = path.join(tempDir(t, 'tpg-lock-'), 'x.lock')
  fs.mkdirSync(lockDir)
  fs.writeFileSync(
    path.join(lockDir, 'owner.json'),
    JSON.stringify({ pid: 4242, startedAt: 'live' }),
  )
  let clock = 0
  let ran = false
  await assert.rejects(
    withInstanceLock(lockDir, async () => (ran = true), {
      timeoutMs: 5000,
      procs: {
        alive: (pid) => pid === 4242 || pid === process.pid,
        startTime: (pid) => (pid === 4242 ? 'live' : 'me'),
      },
      now: () => clock,
      sleep: async (ms) => {
        clock += ms
      },
    }),
    (err) => err.kind === 'lock-timeout' && err.exitCode === 75,
  )
  assert.equal(ran, false)
  // A holder whose pid now belongs to a different process (start time differs) is stale.
  await withInstanceLock(lockDir, async () => (ran = true), {
    procs: { alive: () => true, startTime: (pid) => (pid === 4242 ? 'reused' : 'me') },
  })
  assert.equal(ran, true)
})

test('two worktrees get distinct directories and ports; reap removes only gone worktrees', async (t) => {
  const s = setup(t)
  const otherRoot = tempDir(t, 'tpg-root-other-')
  const other = { ...s.base, root: otherRoot }
  const a = await ensureTestPostgres(s.base)
  const b = await ensureTestPostgres(other)
  const pb = instancePaths(otherRoot, s)
  assert.notEqual(pb.dir, s.p.dir)
  assert.notEqual(a.server, b.server)
  fs.rmSync(otherRoot, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  await reapPostgres({ ...s.base, docker: false })
  assert.equal(fs.existsSync(pb.dir), false, 'gone worktree reaped')
  assert.ok(fs.existsSync(s.p.dir), 'live worktree kept')
  assert.equal(s.world.running.size, 1)
})

// ---------------------------------------------------------------------------------------------
// The one real-native test. It needs PostgreSQL 16+ server binaries and a non-root user.

const realBins = await resolveNativeBinaries().catch(() => null)
const realSkip = !realBins
  ? 'no PostgreSQL 16+ initdb/pg_ctl/postgres/psql resolvable on this host'
  : process.getuid?.() === 0
    ? 'initdb refuses to run as root'
    : false

test(
  'a real native instance starts, allows CREATE DATABASE, survives an immediate stop and is gone after down',
  { skip: realSkip, timeout: 240_000 },
  async (t) => {
    // One teardown, in order: down the instance first (it takes its lock under the cache root),
    // then remove the temp roots.
    const mk = (prefix, base = os.tmpdir()) =>
      fs.realpathSync(fs.mkdtempSync(path.join(base, prefix)))
    const cacheRoot = mk('tpg-real-cache-')
    const socketRoot = mk('tpgs-', '/tmp')
    const root = mk('tpg-real-root-')
    const env = { ...process.env }
    delete env.BOSSO_TEST_DATABASE_URL
    delete env.WAREHOUSE_TEST_ADMIN_URL
    const options = {
      root,
      cacheRoot,
      socketRoot,
      env,
      provider: 'native',
      requireCreatedb: true,
      waitSeconds: 30,
      log: () => {},
    }
    const p = instancePaths(root, { cacheRoot, socketRoot })
    t.after(async () => {
      try {
        await downTestPostgres(options)
      } finally {
        for (const dir of [cacheRoot, socketRoot, root])
          fs.rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
      }
    })
    const psql = (url, sql) => {
      const u = new URL(url)
      const password = decodeURIComponent(u.password)
      u.password = ''
      return spawnSync(realBins.psql, ['-X', '-Atc', sql, '--dbname', u.toString()], {
        encoding: 'utf8',
        env: { PATH: process.env.PATH, PGPASSWORD: password },
      })
    }
    const first = await ensureTestPostgres(options)
    assert.equal(first.provider, 'native')
    const created = psql(first.adminUrl, 'CREATE DATABASE tpg_real_probe')
    assert.equal(created.status, 0, created.stderr)

    const stop = spawnSync(realBins.pgCtl, ['stop', '-D', p.data, '-m', 'immediate', '-w'], {
      encoding: 'utf8',
    })
    assert.equal(stop.status, 0, stop.stderr)
    fs.writeFileSync(path.join(p.data, 'postmaster.pid'), '')
    const again = await ensureTestPostgres(options)
    assert.equal(again.bossoUrl, first.bossoUrl)
    const kept = psql(again.adminUrl, "SELECT 1 FROM pg_database WHERE datname = 'tpg_real_probe'")
    assert.equal(kept.stdout.trim(), '1', kept.stderr)

    assert.deepEqual(await downTestPostgres(options), { removed: ['native'] })
    assert.equal(fs.existsSync(p.dir), false)
    assert.notEqual(psql(first.adminUrl, 'SELECT 1').status, 0, 'server is gone')
  },
)
