import assert from 'node:assert/strict'
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import test from 'node:test'

const helper = new URL('./skills-check-isolated.mjs', import.meta.url)

function runFixture(t, { installStatus = 0, checkStatus = 0, output = '  installed: yes\n' } = {}) {
  const dir = mkdtempSync(path.join(tmpdir(), 'skills-check-test-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const boss = path.join(dir, 'boss')
  const log = path.join(dir, 'calls.jsonl')
  writeFileSync(
    boss,
    `#!${process.execPath}
const fs = require('node:fs')
const path = require('node:path')
fs.appendFileSync(process.env.TEST_CALL_LOG, JSON.stringify({
  argv: process.argv.slice(2), home: process.env.HOME,
  hermes: process.env.HERMES_HOME ?? null, trust: process.env.BOSS_TRUST_CHECKOUT_SKILLS,
  firstPath: process.env.PATH.split(path.delimiter)[0],
  stubs: ['claude', 'codex'].map(name => require('node:child_process').spawnSync(name, ['--version']).status),
}) + '\\n')
if (process.argv[3] === 'install') process.exit(${installStatus})
process.stdout.write(${JSON.stringify(output)})
process.exit(${checkStatus})
`,
    { mode: 0o755 },
  )
  const result = spawnSync(process.execPath, [helper.pathname, '--boss', boss], {
    env: {
      ...process.env,
      HOME: dir,
      HERMES_HOME: path.join(dir, 'host-hermes'),
      TEST_CALL_LOG: log,
    },
    encoding: 'utf8',
  })
  const calls = existsSync(log)
    ? readFileSync(log, 'utf8')
        .trim()
        .split('\n')
        .map((line) => JSON.parse(line))
    : []
  return { result, calls, dir }
}

test('installs then checks in a disposable home with deterministic agents', (t) => {
  const { result, calls, dir } = runFixture(t)
  assert.equal(result.status, 0, result.stderr)
  assert.deepEqual(
    calls.map((call) => call.argv),
    [
      ['skills', 'install'],
      ['skills', 'check'],
    ],
  )
  for (const call of calls) {
    assert.notEqual(call.home, dir)
    assert.equal(call.home, calls[0].home)
    assert.equal(call.firstPath, path.join(call.home, 'bin'))
    assert.deepEqual(call.stubs, [0, 0])
    assert.equal(call.hermes, null)
    assert.equal(call.trust, '1')
    assert.equal(existsSync(call.home), false)
  }
  assert.match(result.stdout, /installed: yes/)
})

for (const [step, installStatus, checkStatus, expectedCalls] of [
  ['install', 17, 0, 1],
  ['check', 0, 23, 2],
]) {
  test(`propagates ${step} failure and cleans up`, (t) => {
    const { result, calls } = runFixture(t, { installStatus, checkStatus })
    assert.equal(result.status, installStatus || checkStatus, result.stderr)
    assert.equal(calls.length, expectedCalls)
    assert.equal(existsSync(calls[0].home), false)
  })
}

for (const output of [
  'installed: no\n',
  'no supported agents found on PATH\ninstalled: yes\n',
  'installed: yesterday\n',
]) {
  test(`rejects vacuous check output ${JSON.stringify(output)}`, (t) => {
    const { result, calls } = runFixture(t, { output })
    assert.equal(result.status, 1, result.stderr)
    assert.equal(calls.length, 2)
    assert.equal(existsSync(calls[0].home), false)
  })
}

test('rejects unknown CLI arguments', () => {
  const result = spawnSync(process.execPath, [helper.pathname, '--unknown'], { encoding: 'utf8' })
  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /Unknown option/)
})
