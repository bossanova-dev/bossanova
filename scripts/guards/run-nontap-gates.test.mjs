#!/usr/bin/env node

import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test, { after } from 'node:test'
import { fileURLToPath } from 'node:url'
import { renderGateFailure } from './run-nontap-gates.mjs'

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url))
const runnerPath = path.join(scriptDirectory, 'run-nontap-gates.mjs')
const fixtureRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'run-nontap-gates-'))

after(() => {
  fs.rmSync(fixtureRoot, { recursive: true, force: true })
})

function writeGate(name, source) {
  const gatePath = path.join(fixtureRoot, name)
  fs.writeFileSync(gatePath, source)
  return gatePath
}

function escapeForRegExp(value) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

function runRunner(gates) {
  return spawnSync(process.execPath, [runnerPath, ...gates], { encoding: 'utf8' })
}

test('renderGateFailure includes not ok, gate path, and exit code', () => {
  const output = renderGateFailure({ gate: 'scripts/check-example.mjs', exitCode: 3 })

  assert.match(output, /not ok/)
  assert.match(output, /scripts\/check-example\.mjs/)
  assert.match(output, /exit code 3/)
})

test('passing fixture gates exit zero without emitting not ok', () => {
  const first = writeGate('pass-one.mjs', "process.stdout.write('pass-one\\n')\n")
  const second = writeGate('pass-two.mjs', "process.stdout.write('pass-two\\n')\n")

  const result = runRunner([first, second])
  const combined = `${result.stdout}${result.stderr}`

  assert.equal(result.status, 0)
  assert.doesNotMatch(combined, /not ok/)
  assert.match(combined, /pass-one/)
  assert.match(combined, /pass-two/)
})

test('failing fixture gate emits not ok and preserves exit code', () => {
  const pass = writeGate('before-fail.mjs', "process.stdout.write('before-fail\\n')\n")
  const fail = writeGate(
    'fail-three.mjs',
    "process.stderr.write('fixture failed\\n')\nprocess.exit(3)\n",
  )

  const result = runRunner([pass, fail])

  assert.equal(result.status, 3)
  assert.match(result.stderr, /not ok/)
  assert.match(result.stderr, new RegExp(escapeForRegExp(fail)))
})

// BOS-1276: the runner must NOT stop at the first failure. Gates after a failing one still run, so
// one CI round reports the whole set rather than peeling failures off one round at a time.
test('runner keeps going past a failing gate and still runs the rest', () => {
  const pass = writeGate('before-stop.mjs', "process.stdout.write('before-stop\\n')\n")
  const fail = writeGate('stop-fail.mjs', 'process.exit(3)\n')
  const afterFail = writeGate('after-fail.mjs', "process.stdout.write('after-fail-marker\\n')\n")

  const result = runRunner([pass, fail, afterFail])
  const combined = `${result.stdout}${result.stderr}`

  assert.equal(result.status, 3)
  assert.match(result.stderr, /not ok/)
  assert.match(combined, /after-fail-marker/)
})

test('two failing gates each emit a failure block and the first non-zero status is returned', () => {
  const first = writeGate('set-one-pass.mjs', "process.stdout.write('set-one\\n')\n")
  const second = writeGate('set-two-fail.mjs', 'process.exit(4)\n')
  const third = writeGate('set-three-pass.mjs', "process.stdout.write('set-three\\n')\n")
  const fourth = writeGate('set-four-fail.mjs', 'process.exit(9)\n')
  const fifth = writeGate('set-five-pass.mjs', "process.stdout.write('set-five-marker\\n')\n")

  const result = runRunner([first, second, third, fourth, fifth])
  const combined = `${result.stdout}${result.stderr}`

  // The FIRST non-zero status, not the last and not a synthesised 1.
  assert.equal(result.status, 4)

  const blocks = result.stderr.match(/not ok - non-TAP gate failed: /g) ?? []
  assert.equal(blocks.length, 2, `expected one block per failing gate, got: ${result.stderr}`)
  assert.match(result.stderr, new RegExp(escapeForRegExp(second)))
  assert.match(result.stderr, new RegExp(escapeForRegExp(fourth)))
  assert.match(result.stderr, /exit code 4/)
  assert.match(result.stderr, /exit code 9/)

  // Every gate in the list ran, including the one after the second failure.
  assert.match(combined, /set-three/)
  assert.match(combined, /set-five-marker/)
})

test('zero gates exit zero without output', () => {
  const result = runRunner([])

  assert.equal(result.status, 0)
  assert.equal(result.stdout, '')
  assert.equal(result.stderr, '')
})

test('signal-killed gate emits not ok and exits non-zero', () => {
  const killed = writeGate('sigterm.mjs', "process.kill(process.pid, 'SIGTERM')\n")

  const result = runRunner([killed])

  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /not ok/)
  assert.match(result.stderr, /SIGTERM/)
})
