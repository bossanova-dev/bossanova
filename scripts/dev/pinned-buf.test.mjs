import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'
import { resolvePinnedBuf, withoutMakeOverrides } from './pinned-buf.mjs'

const repoRoot = fileURLToPath(new URL('../..', import.meta.url))
function fixture(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'pinned-buf-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 5 }))
  const bin = path.join(dir, 'bin')
  fs.mkdirSync(bin)
  const buf = path.join(dir, 'buf-1.66.1/buf')
  const env = {
    ...withoutMakeOverrides(process.env),
    BUF_CACHE_DIR: dir,
    PATH: `${bin}:${process.env.PATH}`,
  }
  function executable(file, body) {
    fs.mkdirSync(path.dirname(file), { recursive: true })
    fs.writeFileSync(file, `#!/bin/sh\n${body}\n`, { mode: 0o755 })
  }
  return { dir, bin, buf, env, executable }
}

test('a host buf never shadows the pin', (t) => {
  const f = fixture(t)
  const hostLog = path.join(f.dir, 'host-calls')
  f.executable(path.join(f.bin, 'buf'), `echo called >> '${hostLog}'; echo 1.68.2`)
  f.executable(f.buf, 'echo 1.66.1')
  assert.deepEqual(resolvePinnedBuf({ repoRoot, env: f.env }), { buf: f.buf })
  assert.equal(fs.existsSync(hostLog), false)
})

test('a wrong-version BUF override is an error even when tools are optional', (t) => {
  const f = fixture(t)
  f.executable(f.buf, 'echo 1.68.2')
  delete f.env.GUARD_PROOF_REQUIRE_TOOLS
  const result = resolvePinnedBuf({ repoRoot, env: { ...f.env, BUF: f.buf } })
  assert.ok('error' in result)
  assert.match(result.error, /want 1\.66\.1/)
})

test('an absent pin offline is a skip and leaves no downloaded files', (t) => {
  const f = fixture(t)
  f.executable(path.join(f.bin, 'curl'), 'exit 22')
  f.executable(path.join(f.bin, 'sleep'), 'exit 0')
  const result = resolvePinnedBuf({ repoRoot, env: f.env })
  assert.ok('skip' in result)
  assert.match(result.skip, /make buf-check-version/)
  assert.deepEqual(fs.readdirSync(path.dirname(f.buf)), [])
})

test('scrubbing inherited overrides leaves fixture make values in control', (t) => {
  const f = fixture(t)
  const env = { ...process.env, BUF: '/poison/buf', MAKEFLAGS: 'BUF=/poison/buf' }
  const run = (env) =>
    spawnSync('make', ['-s', '-n', 'lint-proto', `BUF_CACHE_DIR=${f.dir}`], {
      cwd: repoRoot,
      env,
      encoding: 'utf8',
    })
  const raw = run(env)
  assert.equal(raw.status, 0, raw.stderr)
  assert.match(raw.stdout, /\/poison\/buf/)
  const isolated = run(withoutMakeOverrides(env))
  assert.equal(isolated.status, 0, isolated.stderr)
  assert.ok(isolated.stdout.includes(f.buf), isolated.stdout)
  assert.doesNotMatch(isolated.stdout, /\/poison/)
})

test('proto proofs skip an offline pin locally and fail when tools are required', (t) => {
  const f = fixture(t)
  f.executable(path.join(f.bin, 'curl'), 'exit 22')
  f.executable(path.join(f.bin, 'sleep'), 'exit 0')
  delete f.env.NODE_TEST_CONTEXT
  for (const required of ['0', '1']) {
    const result = spawnSync(
      process.execPath,
      ['--test', '--test-reporter=tap', 'scripts/guards/proto-rpc-naming.test.mjs'],
      {
        cwd: repoRoot,
        env: { ...f.env, GUARD_PROOF_REQUIRE_TOOLS: required },
        encoding: 'utf8',
      },
    )
    assert.equal(result.status, required === '1' ? 1 : 0, result.stdout + result.stderr)
    assert.match(result.stdout, required === '1' ? /# fail 3/ : /# skipped 3/)
    assert.match(result.stdout, /make buf-check-version/)
  }
})
