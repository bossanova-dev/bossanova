import assert from 'node:assert/strict'
import { test } from 'node:test'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { classifyEnvironmentFailure } from './env-failure-lib.mjs'
import {
  classifyPostgresProbe,
  worktreeIdentity,
  ensurePostgres,
  ensureTestPostgres,
  checkPostgres,
  downPostgres,
  reapPostgres,
} from './test-postgres.mjs'

const root = '/tmp/boss-test-worktree'
const identity = worktreeIdentity(root)
function fixture({ unhealthy = 0, race = false, portConflict = false, foreign = false } = {}) {
  let exists = false,
    probes = 0,
    runs = 0
  const calls = []
  const run = async (cmd, args) => {
    calls.push([cmd, ...args])
    if (cmd === 'psql') return { status: 0, stdout: 'ok' }
    if (args[0] === 'inspect')
      return exists
        ? {
            status: 0,
            stdout: JSON.stringify([
              {
                Config: {
                  Labels: { 'bossanova.test-postgres.worktree': foreign ? '/other' : root },
                },
                State: { Running: true },
              },
            ]),
          }
        : { status: 1, stderr: 'No such object' }
    if (args[0] === 'run') {
      runs++
      if (portConflict && runs === 1) return { status: 1, stderr: 'port is already allocated' }
      exists = true
      return race
        ? { status: 1, stderr: 'Conflict. The container name is already in use' }
        : { status: 0, stdout: 'id' }
    }
    if (args[0] === 'port') return { status: 0, stdout: '127.0.0.1:60123' }
    if (args[0] === 'exec')
      return ++probes <= unhealthy
        ? { status: 1, stderr: 'database system is in recovery mode' }
        : { status: 0, stdout: 'ok' }
    if (args[0] === 'rm') {
      exists = false
      return { status: 0 }
    }
    if (args[0] === 'ps') return { status: 0, stdout: '' }
    if (args[0] === 'info') return { status: 0, stdout: '24.0.6' }
    throw new Error(`unexpected ${cmd} ${args}`)
  }
  return {
    run,
    calls,
    options: { root, run, portAvailable: async () => true, waitSeconds: 0, sleep: async () => {} },
  }
}
test('identity is stable, isolated and avoids 5432', () => {
  assert.deepEqual(identity, worktreeIdentity(root))
  assert.notEqual(identity.name, worktreeIdentity('/other').name)
  assert.ok(identity.port > 10000 && identity.port !== 5432)
})
test('ensure publishes only loopback, reuses docker port, and configures test server', async () => {
  const f = fixture()
  const first = await ensurePostgres(f.options)
  assert.equal(
    first.bossoUrl,
    'postgres://postgres:postgres@127.0.0.1:60123/bosso_test?sslmode=disable',
  )
  assert.equal((await ensurePostgres(f.options)).bossoUrl, first.bossoUrl)
  const runs = f.calls.filter((c) => c[1] === 'run')
  assert.equal(runs.length, 1)
  assert.ok(runs[0].includes(`127.0.0.1:${identity.port}:5432`))
  assert.ok(runs[0].includes('max_connections=200'))
  assert.ok(runs[0].includes('fsync=off'))
})
test('foreign listener and docker allocated ports step to next port', async () => {
  for (const dockerConflict of [false, true]) {
    const f = fixture({ portConflict: dockerConflict })
    await ensurePostgres({
      ...f.options,
      portAvailable: async (p) => dockerConflict || p !== identity.port,
    })
    assert.ok(
      f.calls
        .filter((c) => c[1] === 'run')
        .at(-1)
        .includes(`127.0.0.1:${identity.port + 1}:5432`),
    )
  }
})
test('explicit name and port win; held explicit port fails', async () => {
  const f = fixture()
  await ensurePostgres({ ...f.options, name: 'custom', port: 62222 })
  const run = f.calls.find((c) => c[1] === 'run')
  assert.ok(run.includes('custom') && run.includes('127.0.0.1:62222:5432'))
  await assert.rejects(
    ensurePostgres({ ...fixture().options, port: 62222, portAvailable: async () => false }),
    (e) => e.kind === 'foreign-listener',
  )
})
test('concurrent create conflict adopts the newly owned container', async () => {
  const f = fixture({ race: true })
  assert.ok((await ensurePostgres(f.options)).adminUrl.endsWith('/postgres?sslmode=disable'))
})
test('unhealthy owned container recreates with volume once; persistent failure is classified', async () => {
  const f = fixture({ unhealthy: 1 })
  await ensurePostgres(f.options)
  assert.equal(f.calls.filter((c) => c[1] === 'rm' && c.includes('-v')).length, 1)
  const bad = fixture({ unhealthy: 99 })
  await assert.rejects(
    ensurePostgres(bad.options),
    (e) => e.kind === 'recovery-mode' && e.exitCode === 75,
  )
  assert.equal(bad.calls.filter((c) => c[1] === 'rm').length, 1)
})
test('foreign container is never removed or started', async () => {
  const f = fixture({ foreign: true })
  await f.run('docker', ['run'])
  await assert.rejects(ensurePostgres(f.options), /belongs/)
  assert.equal(f.calls.filter((c) => ['rm', 'start'].includes(c[1])).length, 0)
})
test('down removes owned volume and check never mutates servers', async () => {
  const f = fixture()
  await ensurePostgres(f.options)
  await downPostgres(f.options)
  assert.ok(f.calls.some((c) => c[1] === 'rm' && c.includes('-v')))
  f.calls.length = 0
  await checkPostgres('postgres://user:secret@localhost:1234/postgres', { run: f.run })
  assert.deepEqual(
    f.calls.map((c) => c[0]),
    ['psql'],
  )
})
test('check falls back to TCP only when psql is absent', async () => {
  let warning = ''
  await checkPostgres('postgres://u:secret@localhost:1234/postgres', {
    run: async () => ({ status: null, error: { code: 'ENOENT' } }),
    tcpReachable: async () => true,
    warn: (s) => {
      warning = s
    },
  })
  assert.match(warning, /deeper probes skipped/)
  assert.doesNotMatch(warning, /secret/)
})
test('reap touches only labelled containers with deleted worktree paths', async () => {
  const removed = []
  const run = async (_, args) => {
    if (args[0] === 'ps') return { status: 0, stdout: 'gone\nlive\nforeign' }
    if (args[0] === 'inspect')
      return {
        status: 0,
        stdout: JSON.stringify([
          {
            Config: {
              Labels:
                args[1] === 'foreign'
                  ? {}
                  : { 'bossanova.test-postgres.worktree': args[1] === 'gone' ? '/gone' : root },
            },
          },
        ]),
      }
    if (args[0] === 'rm') {
      removed.push(args)
      return { status: 0 }
    }
    throw new Error('unexpected')
  }
  await reapPostgres({ run, pathExists: (p) => p !== '/gone' })
  assert.deepEqual(removed, [['rm', '-f', '-v', 'gone']])
})
test('classification identifies verified infrastructure failures only', () => {
  for (const [text, kind] of [
    ['Cannot connect to the Docker daemon', 'docker-unavailable'],
    ['connection refused', 'connection-refused'],
    ['53300', 'too-many-clients'],
    ['57P03', 'recovery-mode'],
    ['read-only transaction', 'read-only'],
    ['No space left on device', 'disk-full'],
    ['failed to verify trust authentication', 'auth-rejected'],
    ['password authentication failed', 'auth-rejected'],
    [
      'connection to server at "localhost" (::1), port 5442 failed: Connection refused\n' +
        'connection to server at "localhost" (127.0.0.1), port 5442 failed: FATAL:  password authentication failed for user "u"',
      'auth-rejected',
    ],
    ['database "bosso_test" does not exist', 'database-missing'],
    ['port is already allocated', 'foreign-listener'],
  ])
    assert.equal(classifyPostgresProbe(text)?.kind, kind)
  assert.equal(classifyPostgresProbe('syntax error near SELECT'), null)
})

test('simultaneous callers converge on one owned container', async () => {
  const f = fixture()
  const [a, b] = await Promise.all([ensurePostgres(f.options), ensurePostgres(f.options)])
  assert.equal(a.bossoUrl, b.bossoUrl)
})
test('all unhealthy server classes trigger one owned-volume replacement', async () => {
  for (const message of [
    'connection refused',
    'database system is in recovery mode',
    'database is read-only',
    'database "bosso_test" does not exist',
  ]) {
    const f = fixture()
    let first = true
    const run = async (command, args) => {
      if (args[0] === 'exec' && first) {
        first = false
        return { status: 1, stderr: message }
      }
      return f.run(command, args)
    }
    await ensurePostgres({ ...f.options, run })
    assert.equal(f.calls.filter((c) => c[1] === 'rm' && c.includes('-v')).length, 1, message)
  }
})

test('concurrent reapers tolerate an already removed stale container', async () => {
  let removed = false
  const run = async (_, args) => {
    if (args[0] === 'ps') return { status: 0, stdout: 'stale' }
    if (args[0] === 'inspect')
      return {
        status: 0,
        stdout: JSON.stringify([
          { Config: { Labels: { 'bossanova.test-postgres.worktree': '/gone' } } },
        ]),
      }
    if (args[0] === 'rm') {
      if (removed)
        return { status: 1, stderr: 'Error response from daemon: No such container: stale' }
      removed = true
      return { status: 0 }
    }
    throw new Error('unexpected command')
  }
  await Promise.all([
    reapPostgres({ run, pathExists: () => false }),
    reapPostgres({ run, pathExists: () => false }),
  ])
  assert.equal(removed, true)
})

test('CLI probes the supplied URI with connection options and keeps its password off argv', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'postgres-probe-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }))
  const capture = path.join(dir, 'capture.json')
  fs.writeFileSync(
    path.join(dir, 'psql'),
    `#!${process.execPath}
const fs = require('node:fs')
fs.writeFileSync(process.env.PROBE_CAPTURE, JSON.stringify({args: process.argv.slice(2), password: process.env.PGPASSWORD}))
`,
    { mode: 0o755 },
  )
  for (const suffix of ['', '&password=query%40secret']) {
    const uri =
      'postgres://test%20user:encoded%40secret@127.0.0.1:5442/test%20db?sslmode=disable&options=-c%20statement_timeout%3D1000' +
      suffix
    const result = spawnSync(
      process.execPath,
      [fileURLToPath(new URL('./test-postgres.mjs', import.meta.url)), 'check', '--url', uri],
      {
        encoding: 'utf8',
        env: {
          ...process.env,
          PATH: dir + path.delimiter + process.env.PATH,
          PROBE_CAPTURE: capture,
        },
      },
    )
    assert.equal(result.status, 0, result.stderr)
    const { args, password } = JSON.parse(fs.readFileSync(capture, 'utf8'))
    assert.ok(args.includes('--dbname'), 'psql must receive a connection URI explicitly')
    const connection = new URL(args[args.indexOf('--dbname') + 1])
    assert.equal(connection.hostname, '127.0.0.1')
    assert.equal(connection.port, '5442')
    assert.equal(decodeURIComponent(connection.username), 'test user')
    assert.equal(decodeURIComponent(connection.pathname), '/test db')
    assert.equal(connection.searchParams.get('sslmode'), 'disable')
    assert.equal(connection.searchParams.get('options'), '-c statement_timeout=1000')
    assert.equal(connection.password, '')
    assert.equal(connection.searchParams.has('password'), false)
    assert.equal(password, suffix ? 'query@secret' : 'encoded@secret')
    assert.ok(args.every((arg) => !/secret/.test(arg)))
  }
})

// ---------------------------------------------------------------------------------------------
// Supplied-URL resolution. Owned fallbacks run against the Docker fixture above with temp lock
// and cache roots; nothing here reaches a real server or the operator's cache.

function ownedRoots(t) {
  const cacheRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'tpg-cache-'))
  const socketRoot = fs.mkdtempSync(path.join('/tmp', 'tpg-sock-'))
  t.after(() => {
    fs.rmSync(cacheRoot, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
    fs.rmSync(socketRoot, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  })
  return { cacheRoot, socketRoot }
}
/** The Docker fixture, with the supplied URL's psql probe answering `suppliedError`. */
function suppliedWorld(t, supplied, suppliedError, { errors = Infinity } = {}) {
  const f = fixture()
  let remaining = errors
  const run = async (cmd, args, extra = {}) => {
    if (cmd === 'psql' && extra.url === supplied) {
      f.calls.push(['psql-supplied'])
      if (suppliedError && remaining-- > 0) return { status: 2, stderr: suppliedError }
      return { status: 0, stdout: '' }
    }
    return f.run(cmd, args)
  }
  const logs = []
  return {
    calls: f.calls,
    logs,
    options: {
      ...f.options,
      ...ownedRoots(t),
      run,
      supplied,
      env: {},
      log: (line) => logs.push(line),
    },
  }
}
const REFUSED = 'connection to server at "127.0.0.1", port 5442 failed: Connection refused'
const LOOPBACK = 'postgres://u:secret@127.0.0.1:5442/bosso_test?sslmode=disable'

test('a refused loopback supplied URL falls back to an owned instance with one redacted line', async (t) => {
  for (const host of ['127.0.0.1', 'localhost', '[::1]']) {
    const supplied = `postgres://u:secret@${host}:5442/bosso_test`
    const w = suppliedWorld(t, supplied, REFUSED)
    const result = await ensureTestPostgres(w.options)
    assert.equal(result.provider, 'docker')
    assert.match(result.bossoUrl, /^postgres:\/\/postgres:postgres@127\.0\.0\.1:60123\/bosso_test/)
    assert.deepEqual(w.logs, [
      `test-postgres: supplied ${host}:5442 is unavailable (connection-refused); using an owned instance`,
    ])
    assert.ok(!w.logs.join('').includes('secret'))
  }
})

test('availability classes fall back; recovery that clears within the short wait keeps the supplied URL', async (t) => {
  for (const message of [
    'FATAL:  database "bosso_test" does not exist',
    'server closed the connection unexpectedly',
  ]) {
    const w = suppliedWorld(t, LOOPBACK, message)
    assert.equal((await ensureTestPostgres(w.options)).provider, 'docker', message)
  }
  const persistent = suppliedWorld(t, LOOPBACK, 'FATAL:  the database system is starting up')
  assert.equal((await ensureTestPostgres(persistent.options)).provider, 'docker')
  assert.ok(persistent.calls.filter((c) => c[0] === 'psql-supplied').length > 1, 'waited first')
  const brief = suppliedWorld(t, LOOPBACK, 'FATAL:  the database system is starting up', {
    errors: 2,
  })
  const kept = await ensureTestPostgres(brief.options)
  assert.equal(kept.provider, 'supplied')
  assert.equal(kept.bossoUrl, LOOPBACK)
})

test('a healthy supplied URL is returned unchanged and nothing is provisioned', async (t) => {
  const w = suppliedWorld(t, LOOPBACK, null)
  assert.deepEqual(await ensureTestPostgres(w.options), {
    ok: true,
    provider: 'supplied',
    server: '127.0.0.1:5442',
    bossoUrl: LOOPBACK,
    adminUrl: LOOPBACK,
  })
  assert.deepEqual(w.calls, [['psql-supplied']])
})

test('auth-rejected, read-only, createdb-denied, a non-loopback host and CI all fail closed with no fallback', async (t) => {
  const cases = [
    [LOOPBACK, 'FATAL:  password authentication failed for user "u"', {}, 'auth-rejected'],
    [LOOPBACK, 'ERROR:  database is read-only', {}, 'read-only'],
    [LOOPBACK, 'ERROR:  role cannot create databases', {}, 'createdb-denied'],
    ['postgres://u:secret@db.internal:5442/bosso_test', REFUSED, {}, 'connection-refused'],
    [LOOPBACK, REFUSED, { CI: '1' }, 'connection-refused'],
  ]
  for (const [supplied, message, env, kind] of cases) {
    const w = suppliedWorld(t, supplied, message)
    await assert.rejects(
      ensureTestPostgres({ ...w.options, env, requireCreatedb: true }),
      (err) =>
        err.kind === kind &&
        err.exitCode === 75 &&
        err.category === 'infrastructure' &&
        err.provider === 'supplied' &&
        err.attempted.length === 1 &&
        !JSON.stringify(err.attempted).includes('secret'),
      `${kind} ${JSON.stringify(env)}`,
    )
    assert.deepEqual(
      w.calls.filter((c) => c[0] === 'docker'),
      [],
      `${kind}: no owned instance`,
    )
    assert.deepEqual(w.logs, [])
  }
})

test('--require createdb adds the role capability probe to the health SQL', async () => {
  const seen = []
  const run = async (_cmd, args) => {
    seen.push(args.join(' '))
    return { status: 0 }
  }
  await checkPostgres(LOOPBACK, { run, requireCreatedb: true })
  await checkPostgres(LOOPBACK, { run })
  assert.match(seen[0], /rolcreatedb OR rolsuper/)
  assert.doesNotMatch(seen[1], /rolcreatedb/)
})

test('forcing an unavailable Docker provider fails as docker-unavailable', async (t) => {
  const w = suppliedWorld(t, undefined, null)
  const run = async (cmd, args, extra) =>
    cmd === 'docker' && args[0] === 'info'
      ? { status: 1, stderr: 'Cannot connect to the Docker daemon' }
      : w.options.run(cmd, args, extra)
  await assert.rejects(
    ensureTestPostgres({ ...w.options, run, env: { BOSS_TEST_PG_PROVIDER: 'docker' } }),
    (err) => err.kind === 'docker-unavailable' && err.provider === 'docker' && err.exitCode === 75,
  )
})

test('--json failures are one credential-free infrastructure object; the human line keeps the env-failure signature', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'postgres-json-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }))
  fs.writeFileSync(
    path.join(dir, 'psql'),
    `#!/bin/sh\necho 'psql: error: connection to server at "db.internal" (10.0.0.9), port 5442 failed: FATAL:  password authentication failed for user "u"' >&2\nexit 2\n`,
    { mode: 0o755 },
  )
  const script = fileURLToPath(new URL('./test-postgres.mjs', import.meta.url))
  const supplied = 'postgres://u:s3cr3t@db.internal:5442/bosso_test'
  const env = { PATH: dir + path.delimiter + process.env.PATH, HOME: dir }
  for (const json of [true, false]) {
    const result = spawnSync(
      process.execPath,
      [
        script,
        'ensure',
        '--supplied',
        supplied,
        '--require',
        'createdb',
        ...(json ? ['--json'] : []),
      ],
      { encoding: 'utf8', env },
    )
    assert.equal(result.status, 75, result.stderr)
    assert.ok(!`${result.stdout}${result.stderr}`.includes('s3cr3t'))
    assert.equal(classifyEnvironmentFailure(result.stderr)?.kind, 'test-postgres')
    if (!json) continue
    const lines = result.stdout.trim().split('\n')
    assert.equal(lines.length, 1)
    const body = JSON.parse(lines[0])
    assert.deepEqual(Object.keys(body).sort(), [
      'attempted',
      'category',
      'kind',
      'ok',
      'provider',
      'remedy',
    ])
    assert.equal(body.ok, false)
    assert.equal(body.category, 'infrastructure')
    assert.equal(body.kind, 'auth-rejected')
    assert.equal(body.provider, 'supplied')
    assert.deepEqual(body.attempted, [
      { provider: 'supplied', kind: 'auth-rejected', server: 'db.internal:5442' },
    ])
    assert.doesNotMatch(body.remedy, /unset|initdb|pg_ctl/)
  }
})
