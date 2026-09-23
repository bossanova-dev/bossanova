#!/usr/bin/env node

import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import test, { after } from 'node:test'

import { ENV_FAILURE_EXIT_CODE, classifyGateFailure } from './env-failure-lib.mjs'
import { VERDICTS } from './gate-run.mjs'
import { CACHE_STATES } from './gate-log-lib.mjs'

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
  const [runDir, pid, printedLog] = result.stdout.trim().split('\n')
  assert.ok(fs.statSync(runDir).isDirectory())
  tempDirs.push(runDir)
  return { runDir, pid: Number.parseInt(pid, 10), printedLog }
}

function waitGate(runDir, timeout = 5_000) {
  return nodeGate(['wait', '--timeout', String(timeout), runDir])
}

function statusOf(runDir) {
  // Never read `status` without waiting for it: the supervisor writes the file from its exit and
  // SIGTERM handlers, so a bare read races that write and throws ENOENT under parallel load.
  waitForFile(path.join(runDir, 'status'))
  return fs.readFileSync(path.join(runDir, 'status'), 'utf8').trim()
}

function firstLine(stdout) {
  return stdout.split('\n')[0].trim()
}

function escapeRegExp(value) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

function waitForFile(file, timeoutMs = 5_000) {
  const deadline = Date.now() + timeoutMs
  while (Date.now() <= deadline) {
    if (fs.existsSync(file)) return
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 25)
  }
  assert.fail(`timed out waiting for ${file}`)
}

function waitForNonEmptyFile(file, timeoutMs = 5_000) {
  waitForFile(file, timeoutMs)
  const deadline = Date.now() + timeoutMs
  while (Date.now() <= deadline) {
    if (fs.statSync(file).size > 0) return
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 25)
  }
  assert.fail(`timed out waiting for content in ${file}`)
}

function waitForDeadPid(pid, timeoutMs = 5_000) {
  const deadline = Date.now() + timeoutMs
  while (Date.now() <= deadline) {
    try {
      process.kill(pid, 0)
    } catch {
      return
    }
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 25)
  }
  assert.fail(`pid ${pid} still alive`)
}

function runDirSnapshot(runDir) {
  return fs
    .readdirSync(runDir)
    .sort()
    .map((entry) => {
      const stat = fs.statSync(path.join(runDir, entry))
      return `${entry}:${stat.size}:${stat.mtimeMs}`
    })
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
  assert.equal(firstLine(passed.stdout), VERDICTS.passed)
  assert.equal(passed.code, 0)

  const failing = startGate([process.execPath, '-e', 'process.exit(3)'])
  const failed = waitGate(failing.runDir)
  assert.equal(statusOf(failing.runDir), '3')
  assert.equal(firstLine(failed.stdout), `${VERDICTS.failedPrefix}3)`)
  assert.equal(failed.code, 1)
})

test('a non-zero gate whose log carries a transport signature is an environment failure', () => {
  const script = fixtureScript(
    'console.log(\'Post "https://api.github.com/graphql": read tcp 10.0.0.1:1->10.0.0.2:443: operation timed out\')\nprocess.exit(1)\n',
  )
  const { runDir } = startGate([process.execPath, script])
  const result = waitGate(runDir)
  assert.equal(statusOf(runDir), '1')
  assert.equal(firstLine(result.stdout), `${VERDICTS.environmentPrefix}1)`)
  assert.match(result.stdout, /kind: github-graphql-transport/)
  assert.equal(result.code, ENV_FAILURE_EXIT_CODE)
})

test('a non-zero gate with no classified signature stays a plain failure', () => {
  const script = fixtureScript("console.log('./main.go:12:3: undefined: Foo')\nprocess.exit(1)\n")
  const { runDir } = startGate([process.execPath, script])
  const result = waitGate(runDir)
  assert.equal(firstLine(result.stdout), `${VERDICTS.failedPrefix}1)`)
  assert.doesNotMatch(result.stdout, /ENVIRONMENT FAILURE/)
  assert.equal(result.code, 1)
})

test('start prints a parent-created run directory, pid and the literal log path', () => {
  const script = fixtureScript('setTimeout(() => {}, 2000)\n')
  const { runDir, pid, printedLog } = startGate([process.execPath, script])
  assert.ok(Number.isInteger(pid))
  assert.ok(fs.statSync(runDir).isDirectory())
  // The log file has no extension: `start` must print the path so no reader invents a `*.log` glob.
  assert.equal(printedLog, path.join(runDir, 'log'))
  assert.ok(fs.existsSync(printedLog))
  assert.equal(fs.readFileSync(path.join(runDir, 'log'), 'utf8'), '')
})

test('every non-passing verdict names the literal log path', () => {
  const { runDir } = startGate([process.execPath, '-e', 'process.exit(5)'])
  const failed = waitGate(runDir)
  assert.match(
    failed.stdout,
    new RegExp(`log: ${path.join(runDir, 'log').replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}`),
  )

  const missing = path.join(mkTemp('gate-run-vanished-log-'), 'missing-run')
  const vanished = waitGate(missing, 100)
  assert.equal(firstLine(vanished.stdout), VERDICTS.vanished)
  assert.match(vanished.stdout, /log: .*[/\\]log$/m)
})

test('wait writes its verdict first line durably to the run dir', () => {
  const passing = startGate([process.execPath, '-e', 'process.exit(0)'])
  const passed = waitGate(passing.runDir)
  assert.equal(
    fs.readFileSync(path.join(passing.runDir, 'verdict'), 'utf8').trim(),
    firstLine(passed.stdout),
  )
  assert.equal(
    fs.readFileSync(path.join(passing.runDir, 'verdict'), 'utf8').trim(),
    VERDICTS.passed,
  )

  const failing = startGate([process.execPath, '-e', 'process.exit(7)'])
  const failed = waitGate(failing.runDir)
  assert.equal(
    fs.readFileSync(path.join(failing.runDir, 'verdict'), 'utf8').trim(),
    firstLine(failed.stdout),
  )

  const script = fixtureScript('setTimeout(() => {}, 30000)\n')
  const running = startGate([process.execPath, script])
  try {
    waitForFile(path.join(running.runDir, 'child-pid'))
    const stillRunning = waitGate(running.runDir, 100)
    assert.equal(
      fs.readFileSync(path.join(running.runDir, 'verdict'), 'utf8').trim(),
      firstLine(stillRunning.stdout),
    )
    assert.equal(
      fs.readFileSync(path.join(running.runDir, 'verdict'), 'utf8').trim(),
      VERDICTS.stillRunning,
    )
  } finally {
    nodeGate(['stop', running.runDir])
  }
})

test('this suite never reads a status file without waiting for it first', () => {
  const source = fs.readFileSync(fileURLToPath(import.meta.url), 'utf8')
  const reads = source.match(/readFileSync\(path\.join\([^)]*'status'\)/g) ?? []
  assert.equal(reads.length, 1, 'status must only be read through the waiting statusOf helper')
  assert.match(
    source,
    /function statusOf\(runDir\) \{[\s\S]*?waitForFile\(path\.join\(runDir, 'status'\)\)[\s\S]*?readFileSync/,
  )
})

test('dead pid with no status is vanished, not still running or passed', () => {
  const runDir = mkTemp('gate-run-vanished-')
  fs.writeFileSync(path.join(runDir, 'pid'), '99999999\n')
  const result = waitGate(runDir, 100)
  assert.equal(firstLine(result.stdout), VERDICTS.vanished)
  assert.equal(result.code, 97)
})

test('missing run metadata is vanished without waiting for the full timeout', () => {
  const runDir = path.join(mkTemp('gate-run-missing-parent-'), 'missing-run')
  const result = waitGate(runDir, 60_000)
  assert.equal(firstLine(result.stdout), VERDICTS.vanished)
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
    assert.equal(firstLine(result.stdout), VERDICTS.stillRunning)
    assert.equal(result.code, 98)
  } finally {
    try {
      process.kill(-pid, 'SIGTERM')
    } catch {}
  }
})

test('a still-running gate names its elapsed time, log staleness and last log line', () => {
  const script = fixtureScript(
    "console.log('INFO: Waiting for another command to complete')\nsetTimeout(() => {}, 10000)\n",
  )
  const { runDir, pid } = startGate([process.execPath, script])
  try {
    waitForNonEmptyFile(path.join(runDir, 'log'))
    const result = waitGate(runDir, 100)
    const lines = result.stdout.trim().split('\n')
    assert.equal(lines[0], VERDICTS.stillRunning)
    assert.match(lines[1], /^elapsed: \d+s - log last written: \d+s ago - last log line: /)
    assert.match(lines[1], /INFO: Waiting for another command to complete/)
    assert.equal(lines[2], `log: ${path.join(runDir, 'log')}`)
    assert.equal(result.code, 98)
  } finally {
    try {
      process.kill(pid, 'SIGTERM')
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
  assert.notEqual(firstLine(result.stdout), VERDICTS.passed)
})

test('stop reclaims a live gate and guarantees a terminal non-zero status', () => {
  const script = fixtureScript('setTimeout(() => {}, 30000)\n')
  const { runDir } = startGate([process.execPath, script])
  waitForFile(path.join(runDir, 'child-pid'))
  const childPid = Number.parseInt(
    fs.readFileSync(path.join(runDir, 'child-pid'), 'utf8').trim(),
    10,
  )

  const stopped = nodeGate(['stop', runDir])
  assert.equal(stopped.code, 0, stopped.stderr)
  waitForDeadPid(childPid)
  assert.notEqual(statusOf(runDir), '0')

  const after = waitGate(runDir, 2_000)
  assert.notEqual(firstLine(after.stdout), VERDICTS.stillRunning)
  assert.equal(after.code, 1)
})

// The motivating case for `stop`: the supervisor is gone and the child ignores SIGTERM, so nothing
// but an escalation can release the worktree/bazel lock. Without the SIGKILL escalation this test
// fails on the liveness assertion while the status assertion still passes — which is exactly how a
// terminal status over a live orphan can look like a successful reclamation.
test('stop escalates past a TERM-ignoring orphan whose supervisor is gone', () => {
  const { runDir, pid } = startGate(['/bin/sh', '-c', 'trap "" TERM; sleep 60'])
  waitForFile(path.join(runDir, 'child-pid'))
  const childPid = Number.parseInt(
    fs.readFileSync(path.join(runDir, 'child-pid'), 'utf8').trim(),
    10,
  )
  process.kill(pid, 'SIGKILL')
  waitForDeadPid(pid)

  const stopped = nodeGate(['stop', runDir])
  assert.equal(stopped.code, 0, stopped.stderr)
  // The lock is only released if the child is actually dead, not merely signalled.
  waitForDeadPid(childPid)
  assert.notEqual(statusOf(runDir), '0')

  const after = waitGate(runDir, 2_000)
  assert.notEqual(firstLine(after.stdout), VERDICTS.stillRunning)
})

test('stop writes a durable verdict on the path where it forces the status itself', () => {
  const { runDir, pid } = startGate(['/bin/sh', '-c', 'trap "" TERM; sleep 60'])
  waitForFile(path.join(runDir, 'child-pid'))
  const childPid = Number.parseInt(
    fs.readFileSync(path.join(runDir, 'child-pid'), 'utf8').trim(),
    10,
  )
  process.kill(pid, 'SIGKILL')
  waitForDeadPid(pid)

  const stopped = nodeGate(['stop', runDir])
  assert.equal(stopped.code, 0, stopped.stderr)
  waitForDeadPid(childPid)
  assert.equal(
    fs.readFileSync(path.join(runDir, 'verdict'), 'utf8').trim(),
    firstLine(stopped.stdout),
  )
  assert.match(stopped.stdout, new RegExp(`log: ${escapeRegExp(path.join(runDir, 'log'))}`))
})

test('a transport phrase inside a reported test failure stays a red gate', () => {
  // Real fixtures in this repo carry the literal string, so an unbounded transport scan would
  // reclassify a genuine red as an environment failure whose remedy is "re-run it, not a red gate".
  const goFailure = [
    '=== RUN   TestStreamReconnect',
    '    terminal_stream_test.go:1269: got "connection reset by peer", want nil',
    '--- FAIL: TestStreamReconnect (0.02s)',
    'FAIL\tgithub.com/example/pkg\t0.031s',
  ].join('\n')
  assert.equal(classifyGateFailure(goFailure), null)

  // A bare transport failure with no test-failure marker is still classified.
  const transportOnly =
    'Post "https://api.github.com/graphql": read tcp 10.0.0.1:1->10.0.0.2:443: operation timed out'
  assert.equal(classifyGateFailure(transportOnly)?.kind, 'github-graphql-transport')

  // A host-environment signature is NOT withheld by a test-failure marker: a disk that filled up
  // during a test run really is a host failure, which is run-gate's shipped behaviour.
  const diskDuringTests = ['--- FAIL: TestWrite (0.01s)', 'No space left on device'].join('\n')
  assert.equal(classifyGateFailure(diskDuringTests)?.kind, 'disk-exhaustion')
})

test('stop on an already-terminal run dir changes no file and prints the existing verdict', () => {
  const { runDir } = startGate([process.execPath, '-e', 'process.exit(4)'])
  const waited = waitGate(runDir)
  assert.equal(firstLine(waited.stdout), `${VERDICTS.failedPrefix}4)`)

  const before = runDirSnapshot(runDir)
  const stopped = nodeGate(['stop', runDir])
  assert.equal(stopped.code, 0, stopped.stderr)
  assert.equal(firstLine(stopped.stdout), `${VERDICTS.failedPrefix}4)`)
  assert.deepEqual(runDirSnapshot(runDir), before)
})

test('self-relaunch works from a helper path containing spaces', () => {
  const root = mkTemp('gate-run path with spaces ')
  const scriptsDir = path.join(root, 'scripts')
  const toolboxDir = path.join(root, 'skills-toolbox')
  fs.mkdirSync(scriptsDir)
  fs.mkdirSync(toolboxDir)
  fs.copyFileSync(GATE_RUN, path.join(scriptsDir, 'gate-run.mjs'))
  fs.copyFileSync(
    path.join(REPO_ROOT, 'scripts/env-failure-lib.mjs'),
    path.join(scriptsDir, 'env-failure-lib.mjs'),
  )
  fs.copyFileSync(
    path.join(REPO_ROOT, 'scripts/gate-log-lib.mjs'),
    path.join(scriptsDir, 'gate-log-lib.mjs'),
  )
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
  assert.equal(firstLine(waited.stdout), VERDICTS.passed)
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
  assert.equal(firstLine(result.stdout), VERDICTS.passed)
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
    `${VERDICTS.environmentPrefix}N)`,
    VERDICTS.vanished,
    VERDICTS.stillRunning,
  ]) {
    assert.match(
      text,
      new RegExp(token.replace(/[.*+?^${}()|[\]\\]/g, '\\$&').replace(/\\ /g, '\\s+')),
    )
  }
  assert.match(text, /absent\s+status\s+file\s+is\s+unknown\s+and\s+never\s+a\s+pass/)
  assert.match(text, /<run-dir>\/verdict/)
  assert.match(text, /node\s+scripts\/gate-run\.mjs\s+stop\s+<run-dir>/)
  assert.match(text, /docs\/testing\/backgrounded-gate-runs\.md/)
  assert.ok(fs.existsSync(DOC))
  assert.doesNotMatch(text, OLD_RECIPE)
})

test('the backgrounded-gate doc carries the verdicts, stop, and the live hazards', () => {
  const doc = fs.readFileSync(DOC, 'utf8')
  for (const token of [
    VERDICTS.passed,
    `${VERDICTS.failedPrefix}N)`,
    `${VERDICTS.environmentPrefix}N)`,
    VERDICTS.vanished,
    VERDICTS.stillRunning,
  ]) {
    assert.match(
      doc,
      new RegExp(token.replace(/[.*+?^${}()|[\]\\]/g, '\\$&').replace(/\\ /g, '\\s+')),
    )
  }
  assert.match(doc, /node\s+scripts\/gate-run\.mjs\s+stop\s+<run-dir>/)
  assert.match(doc, /<run-dir>\/verdict/)
  // The three new live hazards.
  assert.match(doc, /no\s+matches\s+found/)
  assert.match(doc, /nothing\s+to\s+run/)
  assert.match(doc, /launcher/)
  // The retired-hazard table is still there and still labelled retired.
  assert.match(doc, /These are the retired hazards/)
})

test('old recipe matcher catches an inline retired fixture', () => {
  assert.match('R=$(mktemp -d) && (make test >"$R/log" 2>&1; echo $? >"$R/status") &', OLD_RECIPE)
})

// --- BOS-1276: every verdict names its evidence, and nothing else about the verdict moves -------

function evidenceLinesOf(stdout) {
  return stdout.split('\n').filter((line) => line.startsWith('evidence: '))
}

function assertVerdictShape(label, stdout, runDir, expectedFirstLine) {
  assert.equal(firstLine(stdout), expectedFirstLine, `${label}: first line must not move`)
  const evidence = evidenceLinesOf(stdout)
  assert.equal(evidence.length, 1, `${label}: exactly one evidence line, got ${evidence.length}`)
  assert.match(evidence[0], /cache: /, `${label}: evidence must name a cache state`)
  assert.match(evidence[0], /failure lines: \d+/, `${label}: evidence must count failure lines`)
  assert.match(evidence[0], /post-summary failures: \d+/, `${label}: post-summary count`)
  assert.match(evidence[0], /shard-denominated failures: \d+/, `${label}: shard count`)
  if (runDir && fs.existsSync(path.join(runDir, 'verdict'))) {
    // The durable copy is the first line and NOTHING else: no evidence, byte-for-byte as before.
    assert.equal(
      fs.readFileSync(path.join(runDir, 'verdict'), 'utf8'),
      `${expectedFirstLine}\n`,
      `${label}: durable verdict bytes must be unchanged`,
    )
  }
}

test('every wait verdict carries an evidence line without moving the first line or verdict file', () => {
  const passing = startGate([process.execPath, '-e', 'process.exit(0)'])
  assertVerdictShape('passed', waitGate(passing.runDir).stdout, passing.runDir, VERDICTS.passed)

  const failing = startGate([process.execPath, '-e', 'process.exit(3)'])
  assertVerdictShape(
    'failed',
    waitGate(failing.runDir).stdout,
    failing.runDir,
    `${VERDICTS.failedPrefix}3)`,
  )

  const envScript = fixtureScript(
    'console.log(\'Post "https://api.github.com/graphql": operation timed out\')\nprocess.exit(1)\n',
  )
  const env = startGate([process.execPath, envScript])
  const envResult = waitGate(env.runDir)
  assertVerdictShape(
    'environment failure',
    envResult.stdout,
    env.runDir,
    `${VERDICTS.environmentPrefix}1)`,
  )
  assert.equal(envResult.code, ENV_FAILURE_EXIT_CODE)

  const vanishedDir = mkTemp('gate-run-evidence-vanished-')
  fs.writeFileSync(path.join(vanishedDir, 'pid'), '99999999\n')
  assertVerdictShape('vanished', waitGate(vanishedDir, 100).stdout, vanishedDir, VERDICTS.vanished)

  const runningScript = fixtureScript('setTimeout(() => {}, 30000)\n')
  const running = startGate([process.execPath, runningScript])
  try {
    waitForFile(path.join(running.runDir, 'child-pid'))
    assertVerdictShape(
      'still running',
      waitGate(running.runDir, 100).stdout,
      running.runDir,
      VERDICTS.stillRunning,
    )
  } finally {
    nodeGate(['stop', running.runDir])
  }
})

test('every verdict stop writes carries an evidence line too', () => {
  // The path where `stop` forces the status itself, after escalating past a TERM-ignoring child.
  const { runDir, pid } = startGate(['/bin/sh', '-c', 'trap "" TERM; sleep 60'])
  waitForFile(path.join(runDir, 'child-pid'))
  const childPid = Number.parseInt(
    fs.readFileSync(path.join(runDir, 'child-pid'), 'utf8').trim(),
    10,
  )
  process.kill(pid, 'SIGKILL')
  waitForDeadPid(pid)
  const stopped = nodeGate(['stop', runDir])
  assert.equal(stopped.code, 0, stopped.stderr)
  waitForDeadPid(childPid)
  assertVerdictShape('stop forced status', stopped.stdout, runDir, firstLine(stopped.stdout))
  assert.notEqual(firstLine(stopped.stdout), VERDICTS.passed)

  // And the already-terminal path, which must still change no file in the run dir.
  const terminal = startGate([process.execPath, '-e', 'process.exit(4)'])
  waitGate(terminal.runDir)
  const before = runDirSnapshot(terminal.runDir)
  const again = nodeGate(['stop', terminal.runDir])
  assertVerdictShape(
    'stop already terminal',
    again.stdout,
    terminal.runDir,
    `${VERDICTS.failedPrefix}4)`,
  )
  assert.deepEqual(runDirSnapshot(terminal.runDir), before)
})

test('a passing gate whose log shows nothing re-ran says so in its evidence', () => {
  // The dominant misreading this line exists to remove: GATE PASSED over a fully cached run.
  const cached = fixtureScript(
    "console.log('Executed 0 out of 60 tests: 60 tests pass.')\nprocess.exit(0)\n",
  )
  const { runDir } = startGate([process.execPath, cached])
  const result = waitGate(runDir)
  assert.equal(firstLine(result.stdout), VERDICTS.passed)
  assert.match(result.stdout, /evidence: cache: fully cached \(executed 0 of 60 tests/)
  assert.equal(result.code, 0)

  // And a passing gate with no bazel leg reports unknown, never anything pass-equivalent.
  const bare = startGate([process.execPath, '-e', 'process.exit(0)'])
  const bareResult = waitGate(bare.runDir)
  assert.match(bareResult.stdout, /evidence: cache: unknown \(no Bazel executed-count line/)
  assert.equal(CACHE_STATES.unknown, 'unknown')
})
