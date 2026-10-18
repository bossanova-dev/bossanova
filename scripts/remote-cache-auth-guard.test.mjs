import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

const guard = fileURLToPath(new URL('./remote-cache-auth-guard.mjs', import.meta.url))
const KEY = 'test-key-must-never-be-printed'
const WARNING = 'WARNING: Remote Cache: UNAUTHENTICATED: Invalid API key\\n'

function runGuarded(source, opts = {}) {
  const key = Object.hasOwn(opts, 'key') ? opts.key : KEY
  const env = { ...process.env }
  delete env.BUILDBUDDY_API_KEY
  if (key !== undefined) env.BUILDBUDDY_API_KEY = key
  return spawnSync(process.execPath, [guard, '--', process.execPath, '-e', source], {
    encoding: 'utf8',
    env,
  })
}

function authLines(stderr) {
  return stderr.split('\n').filter((line) => line.startsWith('remote cache auth failed:'))
}

test('an UNAUTHENTICATED warning fails an otherwise green step with one line', () => {
  const run = runGuarded(`process.stdout.write('build ok\\n'); process.stderr.write('${WARNING}')`)
  assert.equal(run.status, 1)
  assert.equal(run.stdout, 'build ok\n')
  assert.match(run.stderr, /Invalid API key/)
  assert.deepEqual(authLines(run.stderr), [
    'remote cache auth failed: Bazel printed "Remote Cache: UNAUTHENTICATED", so the BUILDBUDDY_API_KEY secret is invalid or lacks permission and this run was uncached',
  ])
  assert.ok(!run.stdout.includes(KEY) && !run.stderr.includes(KEY), 'key value never echoed')
})

test('a PERMISSION_DENIED remote-cache warning fails the step', () => {
  const run = runGuarded(
    `process.stderr.write('WARNING: Remote Cache: PERMISSION_DENIED: write not allowed\\n')`,
  )
  assert.equal(run.status, 1)
  assert.equal(authLines(run.stderr).length, 1)
})

test('a warning split across output chunks is still caught', () => {
  const run = runGuarded(
    `process.stderr.write('WARNING: Remote Ca'); setTimeout(() => process.stderr.write('che: UNAUTHENTICATED: Invalid API key\\n'), 50)`,
  )
  assert.equal(run.status, 1)
  assert.equal(authLines(run.stderr).length, 1)
})

test('clean output passes through byte-for-byte and exits 0', () => {
  const run = runGuarded(
    `process.stdout.write('INFO: 2683 processes: 1780 remote cache hit\\n'); process.stderr.write('Executed 20 out of 172 tests\\n')`,
  )
  assert.equal(run.status, 0)
  assert.equal(run.stdout, 'INFO: 2683 processes: 1780 remote cache hit\n')
  assert.equal(run.stderr, 'Executed 20 out of 172 tests\n')
})

test('a Bazel failure keeps its exit code, with or without the warning', () => {
  assert.equal(runGuarded(`process.exitCode = 3`).status, 3)
  const run = runGuarded(`process.stderr.write('${WARNING}'); process.exitCode = 3`)
  assert.equal(run.status, 3)
  assert.equal(authLines(run.stderr).length, 1)
})

test('without the key (forks, keyless runs) the warning is ignored', () => {
  for (const key of [undefined, '']) {
    const run = runGuarded(`process.stderr.write('${WARNING}')`, { key })
    assert.equal(run.status, 0, `key=${JSON.stringify(key)}`)
    assert.deepEqual(authLines(run.stderr), [])
  }
})

test('a command that cannot start exits 127', () => {
  const run = spawnSync(process.execPath, [guard, '--', '/nonexistent/bazel'], {
    encoding: 'utf8',
  })
  assert.equal(run.status, 127)
})
