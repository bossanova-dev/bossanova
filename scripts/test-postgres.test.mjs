import assert from 'node:assert/strict'
import { test } from 'node:test'
import {
  classifyPostgresProbe,
  worktreeIdentity,
  ensurePostgres,
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
