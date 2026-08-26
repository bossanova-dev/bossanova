import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'
import { ENV_FAILURE_EXIT_CODE } from './env-failure-lib.mjs'

const runGate = fileURLToPath(new URL('./run-gate.mjs', import.meta.url))

function runChild(source) {
  return spawnSync(
    process.execPath,
    [runGate, '--label', 'L', '--', process.execPath, '-e', source],
    {
      encoding: 'utf8',
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
