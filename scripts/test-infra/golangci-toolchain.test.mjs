import assert from 'node:assert/strict'
import test from 'node:test'
import fs from 'node:fs'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { tempDir } from './test-tmpdir.mjs'
import { golangciGoEnv } from './golangci-toolchain.mjs'

function resolve(local, toolchain = 'go1.26.5', failProbe = false, persisted = 'auto') {
  const env = {}
  const calls = []
  const exec = (command, args, options) => {
    calls.push({ command, args, env: options.env })
    if (args[0] === 'work') {
      if (toolchain === 'off') throw new Error('no workspace')
      return JSON.stringify({ Toolchain: toolchain })
    }
    if (args[1] === 'GOTOOLCHAIN') return persisted
    if (options.env.GOTOOLCHAIN && failProbe) throw new Error('download denied')
    return options.env.GOTOOLCHAIN || local
  }
  return { run: () => golangciGoEnv({ env, cwd: '/workspace', exec }), env, calls }
}

test('newer minor pins and probes the workspace toolchain including release candidates', () => {
  for (const local of ['go1.28.1', 'go1.28rc1']) {
    const f = resolve(local)
    const result = f.run()
    assert.equal(result.env.GOTOOLCHAIN, 'go1.26.5')
    assert.equal(result.goVersion, 'go1.26.5')
    assert.equal(result.pinnedFrom, local)
    assert.equal(f.calls.length, 4)
  }
})

test('patch, equal, older, unparseable and absent directives preserve env', () => {
  for (const [local, directive] of [
    ['go1.26.7', 'go1.26.5'],
    ['go1.26.5', 'go1.26.5'],
    ['go1.25.9', 'go1.26.5'],
    ['devel unknown', 'go1.26.5'],
    ['go1.28.1', ''],
    ['go1.28.1', 'off'],
    ['go1.28.1', 'unknown'],
  ]) {
    const f = resolve(local, directive)
    assert.equal(f.run().env, f.env)
  }
})

test('explicit toolchain wins without invoking go', () => {
  for (const value of ['local', 'go1.28.1']) {
    const env = { GOTOOLCHAIN: value }
    assert.equal(
      golangciGoEnv({
        env,
        exec() {
          assert.fail('exec called')
        },
      }).env,
      env,
    )
  }
})

test('toolchain persisted with go env -w wins over the workspace pin', () => {
  for (const persisted of ['local', 'go1.28.1', 'go1.26.5+auto', 'path']) {
    const f = resolve('go1.28.1', 'go1.26.5', false, persisted)
    const result = f.run()
    assert.equal(result.env, f.env)
    assert.equal(result.pinnedFrom, null)
    assert.ok(
      f.calls.every((call) => !call.env.GOTOOLCHAIN),
      persisted,
    )
  }
})

test('unavailable pinned toolchain names versions, cause and recovery', () => {
  assert.throws(
    resolve('go1.28.1', 'go1.26.5', true).run,
    /go1\.26\.5.*go1\.28\.1.*download denied.*GOTOOLCHAIN=local/s,
  )
})

test('goversion CLI prints the effective lint version', (t) => {
  const dir = tempDir('golangci-toolchain-', t)
  fs.writeFileSync(
    path.join(dir, 'go'),
    '#!/bin/sh\nif [ "$1" = work ]; then echo \'{"Toolchain":"go1.26.5"}\'; elif [ "$2" = GOTOOLCHAIN ]; then echo auto; else echo "${GOTOOLCHAIN:-go1.28.1}"; fi\n',
    { mode: 0o755 },
  )
  const result = spawnSync(
    process.execPath,
    [new URL('./golangci-toolchain.mjs', import.meta.url).pathname, 'goversion'],
    {
      env: { ...process.env, GOTOOLCHAIN: '', PATH: dir + path.delimiter + process.env.PATH },
      encoding: 'utf8',
    },
  )
  assert.equal(result.status, 0, result.stderr)
  assert.equal(result.stdout.trim(), 'go1.26.5')
})
