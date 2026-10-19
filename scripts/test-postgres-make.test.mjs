import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'

function recipe(t, target, { supplied = false, testStatus = 0, brokenAfter = false } = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'test-postgres-make-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  const log = path.join(dir, 'calls')
  const script = path.join(dir, 'probe')
  fs.writeFileSync(
    script,
    `#!/bin/sh
printf '%s\\n' "$*" >> "$PROBE_LOG"
if [ "$1" = ensure ]; then printf '%s\\n' 'postgres://postgres:postgres@127.0.0.1:23456/test'; exit 0; fi
if [ -f "$TEST_FINISHED" ] && [ "$BROKEN_AFTER" = 1 ]; then echo 'test-postgres: environment failure (connection-refused) - repair server' >&2; exit 75; fi
exit 0
`,
    { mode: 0o755 },
  )
  const fakeTest = `#!/bin/sh
printf 'test %s\\n' "$*" >> "$PROBE_LOG"
touch "$TEST_FINISHED"
exit "$TEST_STATUS"
`
  for (const command of ['run-tests', 'go', 'bash'])
    fs.writeFileSync(path.join(dir, command), fakeTest, { mode: 0o755 })
  const env = {
    ...process.env,
    PATH: `${dir}:${process.env.PATH}`,
    PROBE_LOG: log,
    TEST_FINISHED: path.join(dir, 'finished'),
    TEST_STATUS: String(testStatus),
    BROKEN_AFTER: brokenAfter ? '1' : '0',
  }
  delete env.BOSSO_TEST_DATABASE_URL
  delete env.WAREHOUSE_TEST_ADMIN_URL
  delete env.MAKEFLAGS
  const result = spawnSync(
    'make',
    [
      '--no-print-directory',
      '-s',
      target,
      `TEST_POSTGRES=${script}`,
      `MAKE=${path.join(dir, 'run-tests')}`,
      ...(supplied
        ? [
            'BOSSO_TEST_DATABASE_URL=postgres://u:secret@localhost:1234/external',
            'WAREHOUSE_TEST_ADMIN_URL=postgres://u:secret@localhost:1234/external',
          ]
        : []),
    ],
    { encoding: 'utf8', env },
  )
  return { ...result, calls: fs.readFileSync(log, 'utf8') }
}
for (const target of ['test-bosso-postgres', 'test-bosso-scale', 'test-warehouse']) {
  test(`${target} provisions when unset and checks supplied URLs`, (t) => {
    const own = recipe(t, target)
    assert.equal(own.status, 0, own.stderr)
    assert.match(own.calls, /ensure .*--print (?:bosso|admin)-url/)
    assert.match(own.calls, /test /)
    const supplied = recipe(t, target, { supplied: true })
    assert.equal(supplied.status, 0, supplied.stderr)
    assert.match(supplied.calls, /check --url postgres:\/\/u:secret@localhost:1234\/external/)
    assert.doesNotMatch(supplied.calls, /ensure/)
    assert.doesNotMatch(supplied.stdout + supplied.stderr, /secret/)
  })
}
test('failed tests preserve their failure with a healthy database', (t) => {
  const result = recipe(t, 'test-bosso-postgres', { testStatus: 19 })
  assert.equal(result.status, 2)
  assert.match(result.stderr, /Error 19/)
  assert.match(result.calls, /check --url/)
  assert.doesNotMatch(result.stderr, /environment failure/)
})
test('failed tests with a broken database emit the environment signature', (t) => {
  const result = recipe(t, 'test-bosso-postgres', { testStatus: 19, brokenAfter: true })
  assert.equal(result.status, 2)
  assert.match(result.stderr, /Error 75/)
  assert.match(result.stderr, /test-postgres: environment failure/)
})
