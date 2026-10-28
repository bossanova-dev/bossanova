import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'

const OWNED = 'postgres://postgres:postgres@127.0.0.1:23456/test'
const SUPPLIED = 'postgres://u:secret@localhost:1234/external'

/** A fake test-postgres helper: logs its argv, prints the owned URL, fails the post-run probe on demand. */
function fakeHelper(dir) {
  const script = path.join(dir, 'probe')
  fs.writeFileSync(
    script,
    `#!/bin/sh
printf '%s\\n' "$*" >> "$PROBE_LOG"
if [ "$1" = ensure ]; then
  if [ "$ENSURE_FAILS" = 1 ]; then echo 'test-postgres: environment failure (no-provider) - start Docker' >&2; exit 75; fi
  if [ "$2" = --supplied ] && [ -n "$3" ]; then printf '%s\\n' "$3"; else printf '%s\\n' '${OWNED}'; fi
  exit 0
fi
if [ -f "$TEST_FINISHED" ] && [ "$BROKEN_AFTER" = 1 ]; then echo 'test-postgres: environment failure (connection-refused) - repair server' >&2; exit 75; fi
exit 0
`,
    { mode: 0o755 },
  )
  return script
}

function baseEnv(dir, log, extra = {}) {
  const env = { ...process.env, PATH: `${dir}:${process.env.PATH}`, PROBE_LOG: log, ...extra }
  delete env.BOSSO_TEST_DATABASE_URL
  delete env.WAREHOUSE_TEST_ADMIN_URL
  delete env.MAKEFLAGS
  delete env.CI
  return env
}

function recipe(t, target, { supplied = false, testStatus = 0, brokenAfter = false } = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'test-postgres-make-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }))
  const log = path.join(dir, 'calls')
  const script = fakeHelper(dir)
  const fakeTest = `#!/bin/sh
printf 'test %s\\n' "$*" >> "$PROBE_LOG"
printf 'url %s\\n' "\${BOSSO_TEST_DATABASE_URL:-}\${WAREHOUSE_TEST_ADMIN_URL:-}" >> "$PROBE_LOG"
touch "$TEST_FINISHED"
exit "$TEST_STATUS"
`
  for (const command of ['run-tests', 'go', 'bash'])
    fs.writeFileSync(path.join(dir, command), fakeTest, { mode: 0o755 })
  const env = baseEnv(dir, log, {
    TEST_FINISHED: path.join(dir, 'finished'),
    TEST_STATUS: String(testStatus),
    BROKEN_AFTER: brokenAfter ? '1' : '0',
  })
  const result = spawnSync(
    'make',
    [
      '--no-print-directory',
      '-s',
      target,
      `TEST_POSTGRES=${script}`,
      `MAKE=${path.join(dir, 'run-tests')}`,
      ...(supplied
        ? [`BOSSO_TEST_DATABASE_URL=${SUPPLIED}`, `WAREHOUSE_TEST_ADMIN_URL=${SUPPLIED}`]
        : []),
    ],
    { encoding: 'utf8', env },
  )
  return { ...result, calls: fs.readFileSync(log, 'utf8') }
}

/** The resolved URL reached the test command, through its environment or a make override. */
const reachedTest = (calls, url) =>
  calls.split('\n').some((line) => /^(?:test|url) /.test(line) && line.includes(url))

for (const target of ['test-bosso-postgres', 'test-bosso-scale', 'test-warehouse']) {
  test(`${target} resolves through ensure --supplied, owned or supplied`, (t) => {
    const own = recipe(t, target)
    assert.equal(own.status, 0, own.stderr)
    assert.match(own.calls, /^ensure --supplied {2}.*--print (?:bosso|admin)-url$/m)
    assert.ok(reachedTest(own.calls, OWNED), own.calls)
    const supplied = recipe(t, target, { supplied: true })
    assert.equal(supplied.status, 0, supplied.stderr)
    assert.match(
      supplied.calls,
      /^ensure --supplied postgres:\/\/u:secret@localhost:1234\/external /m,
    )
    assert.ok(reachedTest(supplied.calls, SUPPLIED), supplied.calls)
    assert.doesNotMatch(supplied.calls, /^check/m, 'no separate pre-check')
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

/** test-affected with a fake selector printing `selection`, each line run through a fake target. */
function affected(t, selection, extra = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'test-postgres-affected-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }))
  const log = path.join(dir, 'calls')
  fs.writeFileSync(log, '')
  const script = fakeHelper(dir)
  fs.writeFileSync(
    path.join(dir, 'selector'),
    `#!/bin/sh\nprintf '%s\\n' ${selection.map((line) => `'${line}'`).join(' ')}\n`,
    { mode: 0o755 },
  )
  fs.writeFileSync(
    path.join(dir, 'node'),
    `#!/bin/sh
if [ "$1" = scripts/test-infra/select-affected-tests.mjs ]; then exec "${path.join(dir, 'selector')}"; fi
exec "${process.execPath}" "$@"
`,
    { mode: 0o755 },
  )
  fs.writeFileSync(
    path.join(dir, 'selected'),
    `#!/bin/sh\nprintf 'selected %s bosso=%s warehouse=%s\\n' "$1" "\${BOSSO_TEST_DATABASE_URL:-}" "\${WAREHOUSE_TEST_ADMIN_URL:-}" >> "$PROBE_LOG"\n`,
    { mode: 0o755 },
  )
  const result = spawnSync(
    'make',
    ['--no-print-directory', '-s', 'test-affected', `TEST_POSTGRES=${script}`],
    {
      encoding: 'utf8',
      env: baseEnv(dir, log, { BOSS_GATE_STAMP_DIR: path.join(dir, 'gate-stamps'), ...extra }),
    },
  )
  return { ...result, calls: fs.readFileSync(log, 'utf8') }
}

test('test-affected resolves the database once per URL before the first selected Postgres command', (t) => {
  const result = affected(t, [
    'selected test-scripts',
    'selected test-bosso-postgres',
    'selected test-warehouse',
  ])
  assert.equal(result.status, 0, result.stderr)
  const lines = result.calls.trim().split('\n')
  assert.match(lines[0], /^ensure --supplied {2}.*--print bosso-url$/)
  assert.match(lines[1], /^ensure --supplied {2}.*--print admin-url$/)
  assert.equal(lines.length, 5)
  for (const line of lines.slice(2))
    assert.equal(line.endsWith(`bosso=${OWNED} warehouse=${OWNED}`), true, line)
})

test('test-affected fails fast with exit 75 when the database cannot be resolved, before any command', (t) => {
  const result = affected(t, ['selected test-bosso-scale'], { ENSURE_FAILS: '1' })
  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /Error 75/)
  assert.doesNotMatch(result.calls, /^selected/m)
})

test('test-affected without a Postgres target never calls the helper', (t) => {
  const result = affected(t, ['selected test-scripts'])
  assert.equal(result.status, 0, result.stderr)
  assert.doesNotMatch(result.calls, /^ensure/m)
  assert.match(result.calls, /^selected test-scripts bosso= warehouse=$/m)
})
