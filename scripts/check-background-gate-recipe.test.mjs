#!/usr/bin/env node

import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import test, { after } from 'node:test'

import { VERDICTS } from './gate-run.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const GATE_RUN = path.join(REPO_ROOT, 'scripts/gate-run.mjs')
const CLAUDE_MD = path.join(REPO_ROOT, 'CLAUDE.md')
const DOC = path.join(REPO_ROOT, 'docs/testing/backgrounded-gate-runs.md')
const tempDirs = []

function mkTemp(prefix) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), prefix))
  tempDirs.push(dir)
  return dir
}

after(() => {
  for (const dir of tempDirs) fs.rmSync(dir, { recursive: true, force: true })
})

function nodeGate(args, options = {}) {
  const result = spawnSync(process.execPath, [GATE_RUN, ...args], {
    cwd: REPO_ROOT,
    encoding: 'utf8',
    ...options,
  })
  return {
    code: result.status ?? 1,
    stdout: result.stdout ?? '',
    stderr: result.stderr ?? '',
    signal: result.signal ?? null,
  }
}

function startGate(command) {
  const result = nodeGate(['start', '--', ...command])
  assert.equal(result.code, 0, result.stderr)
  const [runDir, pid] = result.stdout.trim().split('\n')
  assert.ok(fs.statSync(runDir).isDirectory())
  tempDirs.push(runDir)
  return { runDir, pid: Number.parseInt(pid, 10) }
}

function waitGate(runDir, timeout = 5_000) {
  return nodeGate(['wait', '--timeout', String(timeout), runDir])
}

function statusOf(runDir) {
  return fs.readFileSync(path.join(runDir, 'status'), 'utf8').trim()
}

function waitForFile(file, timeoutMs = 5_000) {
  const deadline = Date.now() + timeoutMs
  while (Date.now() <= deadline) {
    if (fs.existsSync(file)) return
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 25)
  }
  assert.fail(`timed out waiting for ${file}`)
}

function fixtureScript(body) {
  const dir = mkTemp('gate-run-fixture-')
  const file = path.join(dir, 'fixture.mjs')
  fs.writeFileSync(file, body)
  return file
}

const OLD_RECIPE = /echo\s+\$\?\s*>\s*"\$R\/status"\)\s*&/

test('passing and failing gates report the child status', () => {
  const passing = startGate([process.execPath, '-e', 'process.exit(0)'])
  const passed = waitGate(passing.runDir)
  assert.equal(statusOf(passing.runDir), '0')
  assert.equal(passed.stdout.trim(), VERDICTS.passed)
  assert.equal(passed.code, 0)

  const failing = startGate([process.execPath, '-e', 'process.exit(3)'])
  const failed = waitGate(failing.runDir)
  assert.equal(statusOf(failing.runDir), '3')
  assert.equal(failed.stdout.trim(), `${VERDICTS.failedPrefix}3)`)
  assert.equal(failed.code, 1)
})

test('start prints a parent-created run directory before child output exists', () => {
  const script = fixtureScript('setTimeout(() => {}, 2000)\n')
  const { runDir, pid } = startGate([process.execPath, script])
  assert.ok(Number.isInteger(pid))
  assert.ok(fs.statSync(runDir).isDirectory())
  assert.equal(fs.readFileSync(path.join(runDir, 'log'), 'utf8'), '')
})

test('dead pid with no status is vanished, not still running or passed', () => {
  const runDir = mkTemp('gate-run-vanished-')
  fs.writeFileSync(path.join(runDir, 'pid'), '99999999\n')
  const result = waitGate(runDir, 100)
  assert.equal(result.stdout.trim(), VERDICTS.vanished)
  assert.equal(result.code, 97)
})

test('missing run metadata is vanished without waiting for the full timeout', () => {
  const runDir = path.join(mkTemp('gate-run-missing-parent-'), 'missing-run')
  const result = waitGate(runDir, 60_000)
  assert.equal(result.stdout.trim(), VERDICTS.vanished)
  assert.equal(result.code, 97)
})

test('live pid with no status at timeout is still running', () => {
  const script = fixtureScript('setTimeout(() => {}, 5000)\n')
  const child = spawnSync(
    process.execPath,
    [
      '-e',
      `const {spawn}=require('node:child_process'); const c=spawn(process.execPath, ['${script}'], {detached:true, stdio:'ignore'}); c.unref(); console.log(c.pid)`,
    ],
    {
      encoding: 'utf8',
    },
  )
  const pid = Number.parseInt(child.stdout.trim(), 10)
  const runDir = mkTemp('gate-run-live-')
  fs.writeFileSync(path.join(runDir, 'pid'), `${pid}\n`)
  try {
    const result = waitGate(runDir, 100)
    assert.equal(result.stdout.trim(), VERDICTS.stillRunning)
    assert.equal(result.code, 98)
  } finally {
    try {
      process.kill(-pid, 'SIGTERM')
    } catch {}
  }
})

test('SIGTERM-killed gate writes a non-passing status', () => {
  const script = fixtureScript('setTimeout(() => {}, 10000)\n')
  const { runDir, pid } = startGate([process.execPath, script])
  waitForFile(path.join(runDir, 'child-pid'))
  process.kill(pid, 'SIGTERM')
  const result = waitGate(runDir)
  assert.notEqual(statusOf(runDir), '0')
  assert.notEqual(result.stdout.trim(), VERDICTS.passed)
})

test('self-relaunch works from a helper path containing spaces', () => {
  const root = mkTemp('gate-run path with spaces ')
  const scriptsDir = path.join(root, 'scripts')
  const toolboxDir = path.join(root, 'skills-toolbox')
  fs.mkdirSync(scriptsDir)
  fs.mkdirSync(toolboxDir)
  fs.copyFileSync(GATE_RUN, path.join(scriptsDir, 'gate-run.mjs'))
  fs.copyFileSync(
    path.join(REPO_ROOT, 'skills-toolbox/main-module.mjs'),
    path.join(toolboxDir, 'main-module.mjs'),
  )

  const copiedGateRun = path.join(scriptsDir, 'gate-run.mjs')
  const result = spawnSync(
    process.execPath,
    [copiedGateRun, 'start', '--', process.execPath, '-e', ''],
    {
      cwd: root,
      encoding: 'utf8',
    },
  )
  assert.equal(result.status, 0, result.stderr)
  const [runDir] = result.stdout.trim().split('\n')
  tempDirs.push(runDir)

  const waited = spawnSync(process.execPath, [copiedGateRun, 'wait', '--timeout', '5000', runDir], {
    cwd: root,
    encoding: 'utf8',
  })
  assert.equal(waited.stdout.trim(), VERDICTS.passed)
  assert.equal(waited.status, 0)
})

test('start survives the launching shell exiting', () => {
  const script = fixtureScript('setTimeout(() => process.exit(0), 250)\n')
  const output = execFileSync(
    'sh',
    ['-c', `${process.execPath} ${GATE_RUN} start -- ${process.execPath} ${script}`],
    {
      encoding: 'utf8',
    },
  )
  const [runDir] = output.trim().split('\n')
  tempDirs.push(runDir)
  const result = waitGate(runDir)
  assert.equal(result.stdout.trim(), VERDICTS.passed)
})

test('documented invocation is dialect-free', () => {
  const doc = fs.readFileSync(CLAUDE_MD, 'utf8')
  const invocation = 'node scripts/gate-run.mjs start -- make test'
  assert.match(doc, /node\s+scripts\/gate-run\.mjs\s+start\s+--\s+make\s+test/)
  assert.equal(invocation.includes('$('), false)
  assert.equal(invocation.includes('&&'), false)
  assert.equal(invocation.includes('&'), false)
})

test('CLAUDE.md documents the gate-run verdict contract and retires the old recipe', () => {
  const text = fs.readFileSync(CLAUDE_MD, 'utf8')
  assert.match(text, /### Commands whose result lies/)
  assert.match(text, /node\s+scripts\/gate-run\.mjs\s+start\s+--\s+make\s+test/)
  assert.match(text, /node\s+scripts\/gate-run\.mjs\s+wait\s+<run-dir>/)
  for (const token of [
    VERDICTS.passed,
    `${VERDICTS.failedPrefix}N)`,
    VERDICTS.vanished,
    VERDICTS.stillRunning,
  ]) {
    assert.match(
      text,
      new RegExp(token.replace(/[.*+?^${}()|[\]\\]/g, '\\$&').replace(/\\ /g, '\\s+')),
    )
  }
  assert.match(text, /absent\s+status\s+file\s+is\s+unknown\s+and\s+never\s+a\s+pass/)
  assert.match(text, /docs\/testing\/backgrounded-gate-runs\.md/)
  assert.ok(fs.existsSync(DOC))
  assert.doesNotMatch(text, OLD_RECIPE)
})

test('old recipe matcher catches an inline retired fixture', () => {
  assert.match('R=$(mktemp -d) && (make test >"$R/log" 2>&1; echo $? >"$R/status") &', OLD_RECIPE)
})
