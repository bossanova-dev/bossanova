#!/usr/bin/env node

import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import process from 'node:process'
import { spawn } from 'node:child_process'
import { fileURLToPath } from 'node:url'

import { isMainModule } from '../skills-toolbox/main-module.mjs'
import { ENV_FAILURE_EXIT_CODE, classifyGateFailure } from './env-failure-lib.mjs'

export const VERDICTS = Object.freeze({
  passed: 'GATE PASSED',
  failedPrefix: 'GATE FAILED (exit ',
  environmentPrefix: 'GATE ENVIRONMENT FAILURE (exit ',
  vanished: 'GATE UNKNOWN - vanished',
  stillRunning: 'GATE UNKNOWN - still running',
})

const DEFAULT_TIMEOUT_MS = 600_000
const POLL_MS = 250
// Same bounded window scripts/run-gate.mjs keeps over a streamed gate, reused here as a tail read.
const LOG_TAIL_LIMIT_BYTES = 256 * 1024

function usage(exitCode = 2) {
  const stream = exitCode === 0 ? process.stdout : process.stderr
  stream.write(
    [
      'usage:',
      '  node scripts/gate-run.mjs start -- <cmd...>',
      '  node scripts/gate-run.mjs wait [--timeout <ms>] <run-dir>',
      '  node scripts/gate-run.mjs stop <run-dir>',
      '',
    ].join('\n'),
  )
  process.exit(exitCode)
}

function writeFileAtomic(file, content) {
  const tmp = `${file}.${process.pid}.tmp`
  fs.writeFileSync(tmp, content)
  fs.renameSync(tmp, file)
}

function logPath(runDir) {
  return path.join(runDir, 'log')
}

// Every terminal outcome is emitted through here so it survives the poller's stdout: the first line
// is written to `<run-dir>/verdict` atomically before anything is printed, which is the only thing a
// reader can hold against a backgrounded notification that reports the launcher's exit code.
function emitVerdict(runDir, firstLine, exitCode, extraLines = []) {
  if (runDir) {
    try {
      writeFileAtomic(path.join(runDir, 'verdict'), `${firstLine}\n`)
    } catch {
      // A run dir that never existed (or is unwritable) still gets a printed verdict; the durable
      // copy is a bonus, never a precondition for reporting the outcome.
    }
  }
  process.stdout.write([firstLine, ...extraLines, ''].join('\n'))
  process.exit(exitCode)
}

// Bounded tail of the run dir's log, plus the stat facts an unknown verdict needs to name its cause.
function readLogTail(runDir) {
  const file = logPath(runDir)
  try {
    const stat = fs.statSync(file)
    const start = Math.max(0, stat.size - LOG_TAIL_LIMIT_BYTES)
    const length = stat.size - start
    const buffer = Buffer.alloc(length)
    const fd = fs.openSync(file, 'r')
    try {
      if (length > 0) fs.readSync(fd, buffer, 0, length, start)
    } finally {
      fs.closeSync(fd)
    }
    return { text: buffer.toString('utf8'), mtimeMs: stat.mtimeMs, size: stat.size }
  } catch {
    return null
  }
}

const LAST_LINE_LIMIT = 200

function lastNonBlankLine(text) {
  const lines = String(text ?? '').split('\n')
  for (let i = lines.length - 1; i >= 0; i -= 1) {
    const line = lines[i].trim()
    if (line) {
      return line.length > LAST_LINE_LIMIT ? `${line.slice(0, LAST_LINE_LIMIT)}...` : line
    }
  }
  return ''
}

function ageSeconds(fromMs) {
  if (!Number.isFinite(fromMs)) return null
  return Math.max(0, Math.round((Date.now() - fromMs) / 1000))
}

function mtimeMsOf(file) {
  try {
    return fs.statSync(file).mtimeMs
  } catch {
    return Number.NaN
  }
}

// The cause line that turns "still running" from a bare sentence into evidence: a gate wedged
// behind a sibling worktree's bazel lock has a log frozen for minutes, which is what distinguishes
// it from a gate that is simply slow.
function unknownCauseLine(runDir) {
  const tail = readLogTail(runDir)
  const elapsed = ageSeconds(mtimeMsOf(path.join(runDir, 'pid')))
  const logAge = tail ? ageSeconds(tail.mtimeMs) : null
  const parts = [
    `elapsed: ${elapsed === null ? 'unknown' : `${elapsed}s`}`,
    `log last written: ${logAge === null ? 'no log' : `${logAge}s ago`}`,
  ]
  const last = tail ? lastNonBlankLine(tail.text) : ''
  parts.push(`last log line: ${last || '(none)'}`)
  return parts.join(' - ')
}

function statusCodeFromExit(code, signal) {
  if (Number.isInteger(code)) return code
  if (!signal) return 1
  const signalNumber = os.constants.signals[signal]
  return Number.isInteger(signalNumber) ? 128 + signalNumber : 1
}

function isPidAlive(pid) {
  if (!Number.isInteger(pid) || pid <= 0) return false
  try {
    process.kill(pid, 0)
    return true
  } catch (err) {
    return err?.code === 'EPERM'
  }
}

function parseWaitArgs(args) {
  let timeoutMs = DEFAULT_TIMEOUT_MS
  const rest = []
  for (let i = 0; i < args.length; i += 1) {
    const arg = args[i]
    if (arg === '--timeout') {
      const raw = args[i + 1]
      if (!raw) usage()
      timeoutMs = Number(raw)
      i += 1
      continue
    }
    rest.push(arg)
  }
  if (rest.length !== 1 || !Number.isFinite(timeoutMs) || timeoutMs < 0) usage()
  return { timeoutMs, runDir: rest[0] }
}

function readPid(runDir) {
  try {
    const raw = fs.readFileSync(path.join(runDir, 'pid'), 'utf8').trim()
    return Number.parseInt(raw, 10)
  } catch {
    return Number.NaN
  }
}

function start(args) {
  const sep = args.indexOf('--')
  const command = sep >= 0 ? args.slice(sep + 1) : args
  if (command.length === 0) usage()

  const runDir = fs.mkdtempSync(path.join(os.tmpdir(), 'boss-gate-'))
  fs.writeFileSync(path.join(runDir, 'cmd'), `${command.join('\0')}\n`)

  const logFd = fs.openSync(logPath(runDir), 'a')
  const supervisor = spawn(
    process.execPath,
    [fileURLToPath(import.meta.url), 'supervise', runDir, '--', ...command],
    {
      detached: true,
      stdio: ['ignore', logFd, logFd],
    },
  )
  fs.closeSync(logFd)
  fs.writeFileSync(path.join(runDir, 'pid'), `${supervisor.pid}\n`)
  supervisor.unref()

  // Third line is the literal log path: the run dir's log file has no extension, so a reader that
  // guesses `"$D"/*.log` gets a glob abort that looks exactly like a gate which logged nothing.
  process.stdout.write(`${runDir}\n${supervisor.pid}\n${logPath(runDir)}\n`)
}

function supervise(args) {
  const sep = args.indexOf('--')
  if (sep <= 0 || sep === args.length - 1) usage()

  const runDir = args[0]
  const command = args.slice(sep + 1)
  const logFd = fs.openSync(logPath(runDir), 'a')
  const child = spawn(command[0], command.slice(1), {
    detached: true,
    stdio: ['ignore', logFd, logFd],
  })
  fs.writeFileSync(path.join(runDir, 'child-pid'), `${child.pid}\n`)

  let finished = false
  const finish = (code, signal) => {
    if (finished) return
    finished = true
    fs.closeSync(logFd)
    writeFileAtomic(path.join(runDir, 'status'), `${statusCodeFromExit(code, signal)}\n`)
    process.exit(0)
  }

  for (const signal of ['SIGINT', 'SIGTERM']) {
    process.on(signal, () => {
      try {
        process.kill(-child.pid, signal)
      } catch {}
      finish(null, signal)
    })
  }

  child.on('error', () => finish(127, null))
  child.on('exit', finish)
}

async function wait(args) {
  const { timeoutMs, runDir } = parseWaitArgs(args)
  const deadline = Date.now() + timeoutMs
  const statusPath = path.join(runDir, 'status')

  while (Date.now() <= deadline) {
    if (fs.existsSync(statusPath)) {
      const status = Number.parseInt(fs.readFileSync(statusPath, 'utf8').trim(), 10)
      if (status === 0) {
        emitVerdict(runDir, VERDICTS.passed, 0)
      }
      const exitStatus = Number.isInteger(status) ? status : 1
      // Only a NON-ZERO status is classified, and only over the bounded tail, so a green gate is
      // never re-read and a test whose own output mentions a timeout cannot flip a pass.
      const classification = classifyGateFailure(readLogTail(runDir)?.text ?? '')
      if (classification) {
        emitVerdict(runDir, `${VERDICTS.environmentPrefix}${exitStatus})`, ENV_FAILURE_EXIT_CODE, [
          `kind: ${classification.kind}${classification.remedy ? ` - ${classification.remedy}` : ''}`,
          `log: ${logPath(runDir)}`,
        ])
      }
      emitVerdict(runDir, `${VERDICTS.failedPrefix}${exitStatus})`, 1, [`log: ${logPath(runDir)}`])
    }

    const pid = readPid(runDir)
    if (!Number.isInteger(pid) || !isPidAlive(pid)) {
      const vanishedTail = readLogTail(runDir)
      emitVerdict(runDir, VERDICTS.vanished, 97, [
        ...(vanishedTail ? [unknownCauseLine(runDir)] : []),
        `log: ${logPath(runDir)}`,
      ])
    }
    await new Promise((resolve) => setTimeout(resolve, POLL_MS))
  }

  emitVerdict(runDir, VERDICTS.stillRunning, 98, [
    unknownCauseLine(runDir),
    `log: ${logPath(runDir)}`,
  ])
}

function readStatus(runDir) {
  try {
    const raw = fs.readFileSync(path.join(runDir, 'status'), 'utf8').trim()
    const parsed = Number.parseInt(raw, 10)
    return Number.isInteger(parsed) ? parsed : 1
  } catch {
    return null
  }
}

function verdictLineForStatus(status) {
  return status === 0 ? VERDICTS.passed : `${VERDICTS.failedPrefix}${status})`
}

function readChildPid(runDir) {
  try {
    const raw = fs.readFileSync(path.join(runDir, 'child-pid'), 'utf8').trim()
    return Number.parseInt(raw, 10)
  } catch {
    return Number.NaN
  }
}

const STOP_TERM_GRACE_MS = 2_000
const STOP_KILL_GRACE_MS = 2_000

// Signal the recorded child's process group. `supervise` spawns the child `detached`, so on POSIX it
// is always a process-group leader and `-pid` is the correct and sufficient target. There is
// deliberately no bare-pid fallback: once the group is gone the leader is gone with it, so a raw
// `kill(childPid)` could only ever reach a *different* process that has since been assigned that
// recycled pid — and run dirs live in os.tmpdir() and are never collected, so stopping a stale run
// dir from a previous boot is a realistic way to reach one.
function signalChildGroup(childPid, signal) {
  if (!Number.isInteger(childPid) || childPid <= 0) return false
  if (!isPidAlive(childPid)) return false
  try {
    process.kill(-childPid, signal)
    return true
  } catch {
    return false
  }
}

// Poll for the supervisor's own status write, stopping early once the child is known dead (with the
// supervisor gone, no status will ever appear, so there is nothing left to wait for).
async function pollForStatus(runDir, childPid, untilMs) {
  for (;;) {
    const status = readStatus(runDir)
    if (status !== null) return status
    if (Date.now() > untilMs) return null
    if (Number.isInteger(childPid) && childPid > 0 && !isPidAlive(childPid)) {
      await new Promise((resolve) => setTimeout(resolve, POLL_MS))
      return readStatus(runDir)
    }
    await new Promise((resolve) => setTimeout(resolve, POLL_MS))
  }
}

// Reclaim an orphan: a child spawned `detached` outlives a SIGKILLed supervisor holding the
// worktree/bazel lock, and nothing could reach it or stop the run dir reading as still-running.
// Idempotent, so an unattended caller can always run it in a cleanup step.
async function stop(args) {
  if (args.length !== 1) usage()
  const runDir = args[0]

  const existing = readStatus(runDir)
  if (existing !== null) {
    // Already terminal: change no file (not even `verdict`), report what is there, exit 0.
    process.stdout.write(`${verdictLineForStatus(existing)}\n`)
    process.exit(0)
  }

  const childPid = readChildPid(runDir)
  const stopExtras = () => [`log: ${logPath(runDir)}`]

  signalChildGroup(childPid, 'SIGTERM')
  const termStatus = await pollForStatus(runDir, childPid, Date.now() + STOP_TERM_GRACE_MS)
  if (termStatus !== null) emitVerdict(runDir, verdictLineForStatus(termStatus), 0, stopExtras())

  // The child ignored or outlived SIGTERM. Escalate before writing anything: a terminal status
  // written over a LIVE child is the worst outcome this subcommand has, because the orphan goes on
  // holding the worktree/bazel lock while the run dir now reads terminal, hiding it from the very
  // caller that asked for it to be reclaimed. SIGKILL is uncatchable, so confirming death after it
  // is what makes "the held lock is released" a true statement rather than a hopeful one.
  signalChildGroup(childPid, 'SIGKILL')
  const killStatus = await pollForStatus(runDir, childPid, Date.now() + STOP_KILL_GRACE_MS)
  if (killStatus !== null) emitVerdict(runDir, verdictLineForStatus(killStatus), 0, stopExtras())

  const survived = Number.isInteger(childPid) && childPid > 0 && isPidAlive(childPid)
  const stoppedStatus = 128 + (os.constants.signals.SIGTERM ?? 15)
  try {
    writeFileAtomic(path.join(runDir, 'status'), `${stoppedStatus}\n`)
  } catch (err) {
    process.stderr.write(`gate-run stop: cannot write status in ${runDir}: ${err.message}\n`)
    process.exit(1)
  }
  if (survived) {
    // Terminal status written so the run dir stops reading as still-running, but say plainly that
    // the lock was NOT released — never report this as an ordinary clean stop.
    process.stderr.write(
      `gate-run stop: child pid ${childPid} survived SIGKILL and may still hold its lock\n`,
    )
    emitVerdict(runDir, verdictLineForStatus(stoppedStatus), 1, stopExtras())
  }
  emitVerdict(runDir, verdictLineForStatus(stoppedStatus), 0, stopExtras())
}

if (isMainModule(import.meta.url)) {
  const [subcommand, ...args] = process.argv.slice(2)
  if (subcommand === 'start') start(args)
  else if (subcommand === 'supervise') supervise(args)
  else if (subcommand === 'wait') await wait(args)
  else if (subcommand === 'stop') await stop(args)
  else usage()
}
