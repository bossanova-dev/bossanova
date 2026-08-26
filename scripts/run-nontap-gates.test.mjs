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
  assert.match(result.stderr, new RegExp(fail.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')))
})

test('runner stops at the first failing gate', () => {
  const pass = writeGate('before-stop.mjs', "process.stdout.write('before-stop\\n')\n")
  const fail = writeGate('stop-fail.mjs', 'process.exit(3)\n')
  const afterFail = writeGate('after-fail.mjs', "process.stdout.write('after-fail-marker\\n')\n")

  const result = runRunner([pass, fail, afterFail])
  const combined = `${result.stdout}${result.stderr}`

  assert.equal(result.status, 3)
  assert.match(result.stderr, /not ok/)
  assert.doesNotMatch(combined, /after-fail-marker/)
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
