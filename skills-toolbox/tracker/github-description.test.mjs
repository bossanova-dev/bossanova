import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createHash } from 'node:crypto'
import { mkdtempSync, writeFileSync, readFileSync, rmSync, existsSync, statSync } from 'node:fs'
import { join } from 'node:path'
import { tmpdir } from 'node:os'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { main } from './github.mjs'
import { classifyTrackerOutcome } from './outcome.mjs'

const config = {
  adapters: { tracker: 'github' },
  trackerConfig: { github: { repository: 'acme/tasks' } },
}
function setup(t, mismatch = false, fail = false) {
  const dir = mkdtempSync(join(tmpdir(), 'github-description-test-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  const writes = [],
    calls = [],
    files = []
  let stored
  const spawn = (_cmd, argv) => {
    calls.push(argv)
    assert.equal(argv[1], 'repos/acme/tasks/issues/42')
    if (argv.includes('--input')) {
      const file = argv[argv.indexOf('--input') + 1]
      files.push(file)
      assert.equal(statSync(file).mode & 0o777, 0o600)
      stored = JSON.parse(readFileSync(file, 'utf8')).body
      writes.push(stored)
      assert.ok(argv.includes('PATCH'))
      if (fail) return { status: 1, stderr: 'HTTP 504: timeout' }
    }
    return { status: 0, stdout: JSON.stringify({ body: mismatch ? 'different' : stored }) }
  }
  const invoke = async (description) => {
    const args = join(dir, 'args.json')
    writeFileSync(args, JSON.stringify({ id: 'GH-42', description }))
    let output = '',
      error = ''
    const status = await main(['write-description', '--args-file', args], {
      config,
      spawn,
      stdout: (s) => {
        output += s
      },
      stderr: (s) => {
        error += s
      },
    })
    return { status, output, error }
  }
  return { dir, invoke, writes, calls, files }
}

test('description PATCH uses private input, verifies bytes and cleans temporary input', async (t) => {
  const h = setup(t)
  const body = 'Résumé\r\n日本語\n'
  const result = await h.invoke(body)
  assert.equal(result.status, 0, result.error)
  assert.deepEqual(h.writes, [body])
  assert.equal(h.calls.length, 2)
  assert.deepEqual(JSON.parse(result.output), {
    id: 'GH-42',
    bytes: Buffer.byteLength(body),
    sha256: createHash('sha256').update(body).digest('hex'),
  })
  assert.ok(h.files.every((file) => !existsSync(file)))
})

test('invalid descriptions are rejected before any gh write', async (t) => {
  for (const body of ['', ' \n\t', 'x'.repeat(65537)]) {
    const h = setup(t)
    assert.notEqual((await h.invoke(body)).status, 0)
    assert.equal(h.calls.length, 0)
  }
  const h = setup(t)
  assert.equal((await h.invoke('x'.repeat(65536))).status, 0)
})

test('failed write and mismatching read-back preserve indeterminate provenance', async (t) => {
  for (const [mismatch, fail] of [
    [true, false],
    [false, true],
  ]) {
    const h = setup(t, mismatch, fail)
    const result = await h.invoke('private description')
    assert.notEqual(result.status, 0)
    assert.match(result.error, /may have applied/)
    assert.equal(
      classifyTrackerOutcome(result.error, { operation: 'write' }).verdict,
      'indeterminate',
    )
    assert.equal(result.error.includes('private description'), false)
    assert.ok(h.files.every((file) => !existsSync(file)))
  }
})

test('CLI GitHub operation discovery and write-description execute through fake gh', (t) => {
  const h = setup(t)
  writeFileSync(join(h.dir, '.boss-skills.json'), JSON.stringify(config))
  const state = join(h.dir, 'state.json'),
    bodyFile = join(h.dir, 'body.md'),
    log = join(h.dir, 'writes.json')
  writeFileSync(state, JSON.stringify({ body: 'old' }))
  writeFileSync(bodyFile, 'replaced description\n')
  writeFileSync(
    join(h.dir, 'gh'),
    `#!${process.execPath}
const fs = require('node:fs')
const args = process.argv.slice(2)
const state = process.env.TEST_ISSUE_STATE
if (args.includes('--input')) {
  const file = args[args.indexOf('--input') + 1]
  const value = fs.readFileSync(file, 'utf8')
  fs.writeFileSync(state, value)
  fs.appendFileSync(process.env.TEST_WRITE_LOG, value + '\\n')
}
const result = JSON.parse(fs.readFileSync(state, 'utf8'))
if (process.env.TEST_MISMATCH && !args.includes('--input')) result.body = 'mismatch'
process.stdout.write(JSON.stringify(result))
`,
    { mode: 0o700 },
  )
  const cli = fileURLToPath(new URL('./cli.mjs', import.meta.url))
  const invoke = (args, extra = {}) =>
    spawnSync(process.execPath, [cli, ...args], {
      cwd: h.dir,
      encoding: 'utf8',
      env: {
        ...process.env,
        TRACKER: 'github',
        PATH: `${h.dir}:${process.env.PATH}`,
        TEST_ISSUE_STATE: state,
        TEST_WRITE_LOG: log,
        ...extra,
      },
    })
  const operations = invoke(['operations', '--require', 'writeDescription'])
  assert.equal(operations.status, 0, operations.stderr)
  assert.equal(JSON.parse(operations.stdout).outcome, 'operations-present')
  const args = ['write-description', '--id', 'GH-42', '--body-file', bodyFile]
  const result = invoke(args)
  assert.equal(result.status, 0, result.stderr)
  assert.equal(JSON.parse(result.stdout).outcome, 'operation-executed')
  assert.equal(JSON.parse(readFileSync(state, 'utf8')).body, 'replaced description\n')
  const mismatch = invoke(args, { TEST_MISMATCH: '1' })
  assert.notEqual(mismatch.status, 0)
  assert.match(mismatch.stderr, /may have applied/)
  for (const body of ['', 'x'.repeat(65537)]) {
    const before = readFileSync(log, 'utf8')
    writeFileSync(bodyFile, body)
    assert.notEqual(invoke(args).status, 0)
    assert.equal(readFileSync(log, 'utf8'), before)
  }
})
