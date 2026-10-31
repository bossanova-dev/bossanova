import assert from 'node:assert/strict'
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { test } from 'node:test'

import { describeProtos, findBuf } from './verify-describe-protos.mjs'
import { envelopeFor, gitFixture, runDescriberScript } from './verify-git-fixture.mjs'

const MODULE = new URL('./verify-describe-protos.mjs', import.meta.url)

/** A fake buf that records its arguments and prints `stdout` with exit `status`. */
function fakeBuf(t, dir, { stdout = '', status = 0 } = {}) {
  const bin = path.join(dir, 'buf')
  const log = path.join(dir, 'args.txt')
  writeFileSync(
    bin,
    `#!/bin/sh\nprintf '%s\\n' "$@" > '${log}'\nprintf '%s' '${stdout.replace(/'/g, `'\\''`)}'\nexit ${status}\n`,
  )
  chmodSync(bin, 0o755)
  return { bin, args: () => readFileSync(log, 'utf8').trim().split('\n') }
}
const scratch = (t) => {
  const dir = mkdtempSync(path.join(tmpdir(), 'fake-buf-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 5 }))
  return dir
}

function protoChange(t, { config = true } = {}) {
  const repo = gitFixture(t)
  repo.write({
    ...(config ? { 'proto/buf.yaml': 'version: v1\n' } : {}),
    'proto/acme/v1/user.proto':
      'syntax = "proto3";\nmessage User { string id = 1; string name = 2; }\n',
  })
  const base = repo.commit('base')
  repo.write({
    'proto/acme/v1/user.proto': 'syntax = "proto3";\nmessage User { string id = 1; }\n',
  })
  const head = repo.commit('head')
  return { repo, envelope: envelopeFor(repo.dir, base, head, 'api-contract') }
}

const violation = JSON.stringify({
  path: 'acme/v1/user.proto',
  start_line: 2,
  type: 'FIELD_NO_DELETE',
  message: 'Previously present field "2" with name "name" on message "User" was deleted.',
})

test('a buf violation is breaking and listed; the module is compared against its base copy', (t) => {
  const { repo, envelope } = protoChange(t)
  const buf = fakeBuf(t, scratch(t), { stdout: violation + '\n', status: 100 })
  const result = runDescriberScript(MODULE, envelope, repo.dir, { ...process.env, BUF: buf.bin })
  assert.equal(result.breaking, true)
  assert.equal(result.items.length, 2)
  assert.ok(result.items[1].includes('acme/v1/user.proto'))
  const args = buf.args()
  assert.equal(args[0], 'breaking')
  assert.ok(args[1].endsWith(path.join('head-0', 'proto')))
  assert.equal(args[2], '--against')
  assert.ok(args[3].endsWith(path.join('base-0', 'proto')))
  assert.deepEqual(args.slice(4), ['--error-format', 'json'])
})

test('empty buf output gives the no-breaking item', (t) => {
  const { repo, envelope } = protoChange(t)
  const buf = fakeBuf(t, scratch(t))
  const result = describeProtos(envelope, { cwd: repo.dir, env: { BUF: buf.bin } })
  assert.equal(result.breaking, false)
  assert.equal(result.items.length, 1)
})

test('no buf binary, no buf config or a buf error is not checked, ok and not breaking', (t) => {
  const missing = protoChange(t)
  const noBuf = runDescriberScript(MODULE, missing.envelope, missing.repo.dir, {
    ...process.env,
    BUF: '',
    PATH: scratch(t),
  })
  assert.equal(noBuf.ok, true)
  assert.equal(noBuf.breaking, false)
  assert.equal(noBuf.items.length, 1)

  const bare = protoChange(t, { config: false })
  const buf = fakeBuf(t, scratch(t), { stdout: violation, status: 100 })
  const noConfig = describeProtos(bare.envelope, { cwd: bare.repo.dir, env: { BUF: buf.bin } })
  assert.equal(noConfig.breaking, false)
  assert.equal(noConfig.items.length, 1)

  const broken = fakeBuf(t, scratch(t), { stdout: 'Failure: bad config', status: 1 })
  const failed = describeProtos(missing.envelope, {
    cwd: missing.repo.dir,
    env: { BUF: broken.bin },
  })
  assert.equal(failed.breaking, false)
  assert.equal(failed.items.length, 1)
})

test('a failing module keeps the violations found in the other modules', (t) => {
  const repo = gitFixture(t)
  const proto = (fields) => `syntax = "proto3";\nmessage M { ${fields} }\n`
  repo.write({
    'a/buf.yaml': 'version: v1\n',
    'a/m.proto': proto('string id = 1; string name = 2;'),
    'b/buf.yaml': 'version: v1\n',
    'b/m.proto': proto('string id = 1; string name = 2;'),
  })
  const base = repo.commit('base')
  repo.write({ 'a/m.proto': proto('string id = 1;'), 'b/m.proto': proto('string id = 1;') })
  const head = repo.commit('head')
  const envelope = envelopeFor(repo.dir, base, head, 'api-contract')
  for (const failing of ['a', 'b']) {
    // buf finds a violation in one module and fails with a usage error in the other.
    const bin = path.join(scratch(t), 'buf')
    writeFileSync(
      bin,
      `#!/bin/sh\ncase "$2" in\n  */${failing}) echo 'Failure: bad config'; exit 1 ;;\nesac\nprintf '%s\\n' '${violation.replace(/'/g, `'\\''`)}'\nexit 100\n`,
    )
    chmodSync(bin, 0o755)
    const result = describeProtos(envelope, { cwd: repo.dir, env: { BUF: bin } })
    assert.equal(result.breaking, true, failing)
    assert.equal(result.items.length, 3, `${failing}: ${JSON.stringify(result.items)}`)
  }
})

test('BUF takes precedence over PATH, and a non-executable BUF falls back to PATH', (t) => {
  const onPath = scratch(t)
  const pinned = scratch(t)
  fakeBuf(t, onPath)
  const { bin } = fakeBuf(t, pinned)
  assert.equal(findBuf({ BUF: bin, PATH: onPath }), bin)
  assert.equal(findBuf({ PATH: onPath }), path.join(onPath, 'buf'))
  const plain = path.join(pinned, 'not-executable')
  writeFileSync(plain, '')
  assert.equal(findBuf({ BUF: plain, PATH: onPath }), path.join(onPath, 'buf'))
  mkdirSync(path.join(pinned, 'empty'))
  assert.equal(findBuf({ PATH: path.join(pinned, 'empty') }), null)
})

test('an area without proto files has nothing to check', (t) => {
  const { repo, envelope } = protoChange(t)
  const result = describeProtos(
    { ...envelope, area: { ...envelope.area, paths: ['schema.graphql'] } },
    { cwd: repo.dir },
  )
  assert.deepEqual(result, { items: [], breaking: false })
})
