import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawn, spawnSync } from 'node:child_process'
import { once } from 'node:events'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'
import { ENV_FAILURE_EXIT_CODE } from './env-failure-lib.mjs'

const runGate = fileURLToPath(new URL('./run-gate.mjs', import.meta.url))

function runChild(source, env = {}) {
  const childEnv = { ...process.env, ...env }
  if (!('GOLANGCI_LINT_CACHE' in env)) delete childEnv.GOLANGCI_LINT_CACHE
  delete childEnv.BOSS_GATE_PROCESS_GROUP
  return spawnSync(
    process.execPath,
    [runGate, '--label', 'L', '--', process.execPath, '-e', source],
    {
      encoding: 'utf8',
      env: childEnv,
      timeout: 10000,
      maxBuffer: 8 * 1024 * 1024,
    },
  )
}

test('run-gate exits 0 and passes output through unchanged when the child succeeds', () => {
  const run = runChild(`
    process.stdout.write('out line\\n')
    process.stderr.write('err line\\n')
  `)
  assert.equal(run.status, 0)
  assert.equal(run.stdout, 'out line\n')
  assert.equal(run.stderr, 'err line\n')
})

test('run-gate prints a banner and exits 75 for disk exhaustion', () => {
  const run = runChild(`
    process.stdout.write('compile output\\n')
    process.stderr.write('write cache: No space left on device\\n')
    process.exitCode = 1
  `)
  assert.equal(run.status, ENV_FAILURE_EXIT_CODE)
  assert.match(run.stdout, /compile output/)
  assert.match(run.stderr, /No space left on device/)
  assert.match(run.stderr, /ENVIRONMENT FAILURE \(not a code defect\): disk-exhaustion during L/)
})

test('run-gate preserves a genuine failure status without adding a banner', () => {
  const run = runChild(`
    process.stderr.write('FAIL: assertion mismatch\\n')
    process.exitCode = 1
  `)
  assert.equal(run.status, 1)
  assert.equal(run.stdout, '')
  assert.match(run.stderr, /FAIL: assertion mismatch/)
  assert.doesNotMatch(run.stderr, /ENVIRONMENT FAILURE/)
})

test('run-gate does not relabel a later genuine failure after transient golangci contention', () => {
  const run = runChild(`
    process.stderr.write('Error: parallel golangci-lint is running\\n')
    process.stderr.write('main.go:1:1: undefined: foo\\n')
    process.exitCode = 1
  `)
  assert.equal(run.status, 1)
  assert.match(run.stderr, /parallel golangci-lint is running/)
  assert.match(run.stderr, /undefined: foo/)
  assert.doesNotMatch(run.stderr, /ENVIRONMENT FAILURE/)
})

test('run-gate banner reaches stderr after bulk output through a pipe', () => {
  const run = runChild(`
    process.stdout.write('x'.repeat(2 * 1024 * 1024))
    process.stderr.write('\\nNo space left on device\\n')
    process.exitCode = 1
  `)
  assert.equal(run.status, ENV_FAILURE_EXIT_CODE)
  assert.equal(run.stdout.length, 2 * 1024 * 1024)
  assert.match(run.stderr, /ENVIRONMENT FAILURE \(not a code defect\): disk-exhaustion/)
})

test('run-gate gives nested gates one private cache and removes it after completion', () => {
  const source = `
    const {spawnSync} = require('node:child_process');
    console.log(process.env.GOLANGCI_LINT_CACHE);
    if (process.env.GOLANGCI_LINT_CACHE !== process.env.BOSS_GATE_PRIVATE_LINT_CACHE) process.exit(99);
    const nested = spawnSync(process.execPath, [${JSON.stringify(runGate)}, '--', process.execPath, '-e', 'console.log(process.env.GOLANGCI_LINT_CACHE)'], {encoding: 'utf8'});
    process.stdout.write(nested.stdout);
    process.exitCode = nested.status;
  `
  const first = runChild(source)
  const second = runChild(source)
  assert.equal(first.status, 0, first.stderr)
  assert.equal(second.status, 0, second.stderr)
  const [cache, nested] = first.stdout.trim().split('\n')
  assert.notEqual(cache, 'undefined')
  assert.equal(nested, cache)
  assert.notEqual(second.stdout.trim().split('\n')[0], cache)
  assert.equal(fs.existsSync(cache), false)
})

test('run-gate preserves a caller-owned cache on failure', (t) => {
  const cache = fs.mkdtempSync(path.join(os.tmpdir(), 'caller-golangci-'))
  t.after(() => fs.rmSync(cache, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }))
  fs.writeFileSync(path.join(cache, 'keep'), 'caller data')
  const run = runChild('console.log(process.env.GOLANGCI_LINT_CACHE); process.exitCode = 7', {
    GOLANGCI_LINT_CACHE: cache,
  })
  assert.equal(run.status, 7)
  assert.equal(run.stdout.trim(), cache)
  assert.equal(fs.readFileSync(path.join(cache, 'keep'), 'utf8'), 'caller data')
})

test('run-gate deadline kills resistant descendants after their leader exits', (t) => {
  const run = runChild(
    `
    const {spawn} = require('node:child_process');
    const child = spawn(process.execPath, ['-e', 'process.on("SIGTERM", () => {}); console.log("ready"); setInterval(() => {}, 1000)'], {stdio: ['ignore', 'pipe', 'inherit']});
    child.stdout.once('data', () => {
      console.log(JSON.stringify({cache: process.env.GOLANGCI_LINT_CACHE, pid: child.pid}));
      process.exit(0);
    });
  `,
    { BOSS_GATE_TIMEOUT_MS: '3000' },
  )
  const { cache, pid } = JSON.parse(run.stdout.trim())
  t.after(() => {
    try {
      process.kill(pid, 'SIGKILL')
    } catch {}
  })
  assert.equal(run.error, undefined, run.error?.message)
  assert.equal(run.status, 124, run.stderr)
  assert.match(run.stderr, /wall-clock deadline/)
  assert.equal(fs.existsSync(cache), false)
  const state = spawnSync('ps', ['-o', 'stat=', '-p', String(pid)], { encoding: 'utf8' })
  assert.ok(state.status !== 0 || state.stdout.trim().startsWith('Z'), state.stdout)
})

test('run-gate removes its private cache after a failing child exits', () => {
  const run = runChild('console.log(process.env.GOLANGCI_LINT_CACHE); process.exitCode = 7')
  assert.equal(run.status, 7)
  const cache = run.stdout.trim()
  assert.notEqual(cache, 'undefined')
  assert.equal(fs.existsSync(cache), false)
})

for (const [signal, expected] of [
  ['SIGINT', 130],
  ['SIGTERM', 143],
]) {
  test(
    `run-gate preserves ${signal} cancellation when its child exits successfully`,
    { timeout: 15000 },
    async (t) => {
      const child = spawn(
        process.execPath,
        [
          runGate,
          '--',
          process.execPath,
          '-e',
          `
      process.on('${signal}', () => process.exit(0));
      console.log(process.pid);
      setInterval(() => {}, 1000);
    `,
        ],
        { env: { ...process.env, BOSS_GATE_PROCESS_GROUP: '' }, stdio: ['ignore', 'pipe', 'pipe'] },
      )
      let groupPid
      t.after(() => {
        child.kill('SIGKILL')
        if (groupPid) {
          try {
            process.kill(-groupPid, 'SIGKILL')
          } catch {}
        }
      })
      const completion = once(child, 'close')
      const [ready] = await once(child.stdout, 'data')
      groupPid = Number(String(ready).trim())
      assert.ok(groupPid > 0)
      child.kill(signal)
      const [status] = await completion
      assert.equal(status, expected)
    },
  )
}

test('run-gate maps a golangci host admission deadline to exit 75', () => {
  const run = runChild(
    `process.stderr.write('golangci-lint: host admission wait exceeded after 10ms (not a lint finding)'); process.exitCode=1`,
  )
  assert.equal(run.status, ENV_FAILURE_EXIT_CODE)
  assert.match(run.stderr, /golangci-admission-timeout/)
})
