#!/usr/bin/env node

import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import process from 'node:process'
import { spawn, execFileSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'

import { isMainModule } from '../skills-toolbox/main-module.mjs'
import { ENV_FAILURE_EXIT_CODE, classifyGateFailure } from './test-infra/env-failure-lib.mjs'
import { evictGoCache } from './dev/clean-go-build-cache.mjs'
import {
  DISK_FLOOR_SIGNATURE,
  GiB,
  measureHeadroom,
  decideHeadroom,
  parseFloorGiB,
} from './test-infra/disk-headroom-lib.mjs'
import { gateEnvironment, gateTimeoutMs, spawnGate } from './test-infra/gate-process.mjs'
import { gateEvidenceLine } from './test-infra/gate-log-lib.mjs'
import {
  extractFailedTargets,
  isolationCommandFor,
  decideIsolationVerdict,
  renderIsolationLines,
  renderLoadLine,
} from './test-infra/gate-isolation-lib.mjs'

export const VERDICTS = Object.freeze({
  passed: 'GATE PASSED',
  flakyPrefix: 'GATE FLAKY-UNDER-LOAD (exit ',
  failedPrefix: 'GATE FAILED (exit ',
  environmentPrefix: 'GATE ENVIRONMENT FAILURE (exit ',
  stoppedPrefix: 'GATE STOPPED (exit ',
  vanished: 'GATE UNKNOWN - vanished',
  stillRunning: 'GATE UNKNOWN - still running',
})

// `wait`'s exit code for a run a caller deliberately tore down with `stop`: distinct from 0 (passed),
// 1 (failed), 75 (environment), 97 (vanished) and 98 (still running), so a teardown can never be
// read as a red gate by exit code any more than by its first line.
export const STOPPED_EXIT_CODE = 96
export const FLAKY_EXIT_CODE = 95

const DEFAULT_TIMEOUT_MS = 600_000
const POLL_MS = 250
// Same bounded window scripts/test-infra/run-gate.mjs keeps over a streamed gate, reused here as a tail read.
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

// `stop` leaves this marker BEFORE its first signal, so the non-zero status that signal produces
// renders as a deliberate teardown rather than a failure. A status of 0 still wins (the gate passed
// before the stop took effect), and a non-zero status with no marker is still a genuine red gate.
function stoppedMarkerPath(runDir) {
  return path.join(runDir, 'stopped')
}

function wasStopped(runDir) {
  return fs.existsSync(stoppedMarkerPath(runDir))
}

// Every terminal outcome is emitted through here so it survives the poller's stdout: the first line
// is written to `<run-dir>/verdict` atomically before anything is printed, which is the only thing a
// reader can hold against a backgrounded notification that reports the launcher's exit code.
//
// The evidence line is appended LAST and never touches the first line or the durable `verdict`
// bytes, so every documented consumer contract — `VERDICTS`, `cat <run-dir>/verdict` — is byte-for
// -byte what it was before BOS-1276.
function emitVerdict(runDir, firstLine, exitCode, extraLines = []) {
  if (runDir) {
    try {
      writeFileAtomic(path.join(runDir, 'verdict'), `${firstLine}\n`)
    } catch {
      // A run dir that never existed (or is unwritable) still gets a printed verdict; the durable
      // copy is a bonus, never a precondition for reporting the outcome.
    }
  }
  process.stdout.write([firstLine, ...extraLines, evidenceLine(runDir), ''].join('\n'))
  process.exit(exitCode)
}

// Bounded tail of the run dir's log, plus the stat facts an unknown verdict needs to name its cause.
function readLogTail(runDir, limit = LOG_TAIL_LIMIT_BYTES) {
  const file = logPath(runDir)
  try {
    const stat = fs.statSync(file)
    const start = Math.max(0, stat.size - limit)
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

// Every verdict names the evidence behind it, on the PASSING path as well as the failing ones: the
// single most-cited misreading in this class is a green that proves nothing about whether anything
// re-ran. Rendered by scripts/test-infra/gate-log-lib.mjs from the same bounded 256 KB tail this file already
// reads, so there is no new I/O and no new process.
//
// It can never throw. A defect in the reader must degrade to a line that says so, not swallow the
// verdict the caller is waiting on.
function evidenceLine(runDir) {
  try {
    const tail = runDir ? readLogTail(runDir) : null
    const line = gateEvidenceLine(tail?.text ?? '')
    // Name the window when the log outran it. The counts are read from a bounded tail, so on a
    // longer log `post-summary failures: 0` means "none in the last 256 KB", not "checked the whole
    // run and found none" - and reporting a tail-scoped count in whole-run shape is the same
    // under-determination this line exists to remove.
    if (tail && tail.size > LOG_TAIL_LIMIT_BYTES) {
      const kb = (bytes) => Math.round(bytes / 1024)
      return `${line} - counts read from the last ${kb(LOG_TAIL_LIMIT_BYTES)} KB of a ${kb(tail.size)} KB log, not the whole run`
    }
    return line
  } catch {
    return 'evidence: unavailable - the gate log could not be read'
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
  const isolationDir = path.join(runDir, 'isolation')
  if (fs.existsSync(isolationDir) && !fs.existsSync(path.join(runDir, 'isolation.json'))) {
    const newest = fs
      .readdirSync(isolationDir)
      .reduce((latest, name) => Math.max(latest, mtimeMsOf(path.join(isolationDir, name))), 0)
    parts.push(`phase: isolation - newest isolation log written ${ageSeconds(newest)}s ago`)
  }
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

function readJson(runDir, name) {
  try {
    return JSON.parse(fs.readFileSync(path.join(runDir, name), 'utf8'))
  } catch {
    return null
  }
}

function positiveEnv(name, fallback) {
  const value = Number(process.env[name])
  return Number.isFinite(value) && value > 0 ? value : fallback
}

function loadSampler(load, phase, runDir) {
  let count = 0
  let sum = 0
  let peak = 0
  const sample = () => {
    const value = os.loadavg()[0]
    count += 1
    sum += value
    peak = Math.max(peak, value)
  }
  sample()
  const timer = setInterval(sample, 5000)
  return () => {
    clearInterval(timer)
    sample()
    load[phase] = {
      samples: count,
      peak,
      mean: sum / count,
    }
    writeFileAtomic(path.join(runDir, 'load.json'), JSON.stringify(load))
  }
}

function diskPreflight(logFd) {
  const floorBytes =
    parseFloorGiB(process.env.BOSS_GATE_MIN_FREE_GIB, (line) => fs.writeSync(logFd, `${line}\n`)) *
    GiB
  if (floorBytes === 0) return null
  let cacheDir
  try {
    cacheDir =
      process.env.GOCACHE || execFileSync('go', ['env', 'GOCACHE'], { encoding: 'utf8' }).trim()
  } catch (error) {
    fs.writeSync(logFd, `gate-run preflight: Go cache unavailable: ${error.message}\n`)
  }
  const paths = [
    os.tmpdir(),
    process.cwd(),
    ...(cacheDir ? [cacheDir] : []),
    path.join(os.homedir(), '.cache'),
  ]
  const measure = () => measureHeadroom(paths)
  const record = { floorBytes, preflight: measure(), reclaimedBytes: 0, refused: false, atExit: [] }
  const reclaim = (entries) => {
    const decision = decideHeadroom({ entries, floorBytes })
    if (decision.verdict !== 'below-floor') return
    try {
      const { bytes } = evictGoCache({ dir: cacheDir, targetBytes: Number.MAX_SAFE_INTEGER })
      const targetBytes = Math.max(0, bytes - (floorBytes - decision.lowest.freeBytes) * 1.1)
      record.reclaimedBytes += evictGoCache({ dir: cacheDir, targetBytes }).reclaimedBytes
    } catch (error) {
      fs.writeSync(logFd, `gate-run preflight: reclaim unavailable: ${error.message}\n`)
    }
    fs.writeSync(
      logFd,
      `gate-run preflight: reclaimed ${(record.reclaimedBytes / GiB).toFixed(2)} GiB from Go build cache\n`,
    )
  }
  reclaim(record.preflight)
  const decision = decideHeadroom({ entries: measure(), floorBytes })
  record.refused = decision.verdict === 'below-floor'
  if (record.refused) {
    fs.writeSync(
      logFd,
      `${DISK_FLOOR_SIGNATURE}: ${(decision.lowest.freeBytes / GiB).toFixed(2)} GiB free on ${decision.lowest.path}, floor ${(floorBytes / GiB).toFixed(2)} GiB (BOSS_GATE_MIN_FREE_GIB)\n`,
    )
  }
  return { record, measure, reclaim }
}

function diskEvidenceLines(runDir, classification) {
  if (classification.kind !== 'disk-exhaustion') return []
  const disk = readJson(runDir, 'disk.json')
  const before = disk?.preflight?.reduce((a, b) => (!a || b.freeBytes < a.freeBytes ? b : a), null)
  const after = disk?.atExit?.find((entry) => entry.path === before?.path)
  if (!before || !after) return []
  return [
    `disk: ${(before.freeBytes / GiB).toFixed(2)} GiB free on ${before.path} at preflight, ${(after.freeBytes / GiB).toFixed(2)} GiB at exit; reclaimed ${(disk.reclaimedBytes / GiB).toFixed(2)} GiB`,
  ]
}

async function supervise(args) {
  const sep = args.indexOf('--')
  if (sep <= 0 || sep === args.length - 1) usage()
  const runDir = args[0]
  const command = args.slice(sep + 1)
  const logFd = fs.openSync(logPath(runDir), 'a')
  let currentChild = null
  let interruptedSignal = null
  const environment = gateEnvironment(process.env, runDir)
  if (environment.owned) {
    writeFileAtomic(path.join(runDir, 'owned-cache'), path.basename(environment.owned))
  }
  for (const signal of ['SIGINT', 'SIGTERM']) {
    process.on(signal, () => {
      interruptedSignal = signal
      currentChild?.terminate(signal)
    })
  }
  const load = { cpus: os.availableParallelism() }
  const runChild = async (argv, fd, timeoutMs = gateTimeoutMs()) => {
    const started = Date.now()
    const managed = spawnGate(argv, {
      stdio: ['ignore', fd, fd],
      env: { ...environment.env, BOSS_GATE_PROCESS_GROUP: '' },
      timeoutMs,
    })
    currentChild = managed
    if (managed.child.pid) writeFileAtomic(path.join(runDir, 'child-pid'), `${managed.child.pid}\n`)
    const result = await managed.result
    currentChild = null
    return {
      exit: result.error
        ? 127
        : wasStopped(runDir) && result.status === 0
          ? 128 + os.constants.signals.SIGTERM
          : statusCodeFromExit(result.status, result.signal),
      durationMs: Date.now() - started,
      timedOut: result.timedOut,
    }
  }
  let disk = null
  try {
    disk = diskPreflight(logFd)
  } catch (error) {
    fs.writeSync(logFd, `gate-run preflight unavailable: ${error.message}\n`)
  }
  const finishGateLoad = loadSampler(load, 'gate', runDir)
  const original =
    wasStopped(runDir) || interruptedSignal
      ? { exit: 128 + (os.constants.signals[interruptedSignal ?? 'SIGTERM'] ?? 15) }
      : disk?.record.refused
        ? { exit: ENV_FAILURE_EXIT_CODE }
        : await runChild(command, logFd)
  finishGateLoad()
  try {
    if (disk) {
      if (
        !disk.record.refused &&
        original.exit !== 0 &&
        classifyGateFailure(readLogTail(runDir)?.text ?? '')?.kind === 'disk-exhaustion'
      ) {
        disk.reclaim(disk.measure())
      }
    }
  } catch (error) {
    fs.writeSync(logFd, `gate-run preflight unavailable: ${error.message}\n`)
  }
  try {
    if (
      original.exit !== 0 &&
      !wasStopped(runDir) &&
      !interruptedSignal &&
      process.env.BOSS_GATE_ISOLATE !== '0' &&
      !classifyGateFailure(readLogTail(runDir)?.text ?? '')
    ) {
      const { targets, unattributed } = extractFailedTargets(
        readLogTail(runDir, 1024 * 1024)?.text ?? '',
        { cwd: process.cwd() },
      )
      const maxTargets = positiveEnv('BOSS_GATE_ISOLATE_MAX_TARGETS', 6)
      const record = { targets, unattributed, results: [], maxTargets, budgetExhausted: false }
      if (targets.length > 0 && targets.length <= maxTargets) {
        const finishIsolationLoad = loadSampler(load, 'isolation', runDir)
        try {
          const deadline = Date.now() + positiveEnv('BOSS_GATE_ISOLATE_BUDGET_MS', 1800000)
          fs.mkdirSync(path.join(runDir, 'isolation'))
          for (const [index, target] of targets.entries()) {
            if (wasStopped(runDir) || interruptedSignal) break
            const remaining = deadline - Date.now()
            if (remaining <= 0) {
              record.budgetExhausted = true
              break
            }
            const argv = isolationCommandFor(target, process.env)
            const log = path.join(runDir, 'isolation', `${index + 1}.log`)
            const fd = fs.openSync(log, 'a')
            fs.writeSync(
              logFd,
              `gate-run isolation: re-running ${target.label ?? target.file} alone (${index + 1} of ${targets.length})\n`,
            )
            try {
              const result = await runChild(
                argv,
                fd,
                Math.min(remaining, positiveEnv('BOSS_GATE_ISOLATE_TARGET_TIMEOUT_MS', 900000)),
              )
              record.results.push({ target, argv, ...result, log })
              if (Date.now() >= deadline) record.budgetExhausted = true
            } finally {
              fs.closeSync(fd)
            }
          }
        } finally {
          finishIsolationLoad()
        }
      }
      record.decision = decideIsolationVerdict(record)
      writeFileAtomic(path.join(runDir, 'isolation.json'), JSON.stringify(record))
    }
  } catch (err) {
    fs.writeSync(logFd, `gate-run isolation unavailable: ${err.message}\n`)
  } finally {
    try {
      if (disk) {
        disk.record.atExit = disk.measure()
        writeFileAtomic(path.join(runDir, 'disk.json'), JSON.stringify(disk.record))
      }
    } catch (error) {
      fs.writeSync(logFd, `gate-run preflight unavailable: ${error.message}\n`)
    }
    environment.cleanup()
    fs.closeSync(logFd)
    writeFileAtomic(path.join(runDir, 'status'), `${original.exit}\n`)
  }
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
      if (wasStopped(runDir)) {
        emitVerdict(runDir, `${VERDICTS.stoppedPrefix}${exitStatus})`, STOPPED_EXIT_CODE, [
          `log: ${logPath(runDir)}`,
        ])
      }
      // Only a NON-ZERO status is classified, and only over the bounded tail, so a green gate is
      // never re-read and a test whose own output mentions a timeout cannot flip a pass.
      const classification = classifyGateFailure(readLogTail(runDir)?.text ?? '')
      if (classification) {
        emitVerdict(runDir, `${VERDICTS.environmentPrefix}${exitStatus})`, ENV_FAILURE_EXIT_CODE, [
          renderLoadLine(readJson(runDir, 'load.json')),
          `kind: ${classification.kind}${classification.remedy ? ` - ${classification.remedy}` : ''}`,
          ...diskEvidenceLines(runDir, classification),
          `log: ${logPath(runDir)}`,
        ])
      }
      const isolation = readJson(runDir, 'isolation.json')
      const flaky = isolation?.decision === 'flaky-under-load'
      emitVerdict(
        runDir,
        `${flaky ? VERDICTS.flakyPrefix : VERDICTS.failedPrefix}${exitStatus})`,
        flaky ? FLAKY_EXIT_CODE : 1,
        [
          renderIsolationLines(isolation),
          renderLoadLine(readJson(runDir, 'load.json')),
          `log: ${logPath(runDir)}`,
        ],
      )
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

function verdictLineForStatus(status, stopped = false) {
  if (status === 0) return VERDICTS.passed
  return `${stopped ? VERDICTS.stoppedPrefix : VERDICTS.failedPrefix}${status})`
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

// The recorded pid identifies the child's detached process group until terminal status. A leader
// can exit while descendants retain cache files or log descriptors, so signal the group directly.
function signalChildGroup(childPid, signal) {
  if (!Number.isInteger(childPid) || childPid <= 0) return false
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
    if (
      Number.isInteger(childPid) &&
      childPid > 0 &&
      !isPidAlive(childPid) &&
      !isPidAlive(readPid(runDir))
    ) {
      await new Promise((resolve) => setTimeout(resolve, POLL_MS))
      return readStatus(runDir)
    }
    await new Promise((resolve) => setTimeout(resolve, POLL_MS))
  }
}

// Reclaim an orphan: a child spawned `detached` outlives a SIGKILLed supervisor holding the
// worktree/bazel lock, and nothing could reach it or stop the run dir reading as still-running.
// Idempotent, so an unattended caller can always run it in a cleanup step.
function cleanupOrphanCache(runDir) {
  let name
  try {
    name = fs.readFileSync(path.join(runDir, 'owned-cache'), 'utf8')
  } catch {
    return
  }
  // The marker contains only a run-local directory name; an external caller cache has no marker.
  if (name !== path.basename(name) || !name.startsWith('boss-golangci-')) return
  fs.rmSync(path.join(runDir, name), { recursive: true, force: true })
}

async function stop(args) {
  if (args.length !== 1) usage()
  const runDir = args[0]

  const existing = readStatus(runDir)
  if (existing !== null) {
    // Already terminal: change no file (not even `verdict`), report what is there, exit 0. The
    // evidence line is a read of the log, so it stays inside that no-write guarantee.
    const stopped = wasStopped(runDir)
    const flaky =
      existing !== 0 &&
      !stopped &&
      readJson(runDir, 'isolation.json')?.decision === 'flaky-under-load'
    const firstLine = flaky
      ? `${VERDICTS.flakyPrefix}${existing})`
      : verdictLineForStatus(existing, stopped)
    process.stdout.write(`${firstLine}\n${evidenceLine(runDir)}\n`)
    process.exit(0)
  }

  let childPid = readChildPid(runDir)
  const stopExtras = () => [`log: ${logPath(runDir)}`]

  // Mark the teardown before anything is signalled, so every status this stop provokes - whether
  // the supervisor writes it or this function forces it below - renders as GATE STOPPED.
  try {
    writeFileAtomic(stoppedMarkerPath(runDir), `${new Date().toISOString()}\n`)
  } catch (err) {
    process.stderr.write(`gate-run stop: cannot write stop marker in ${runDir}: ${err.message}\n`)
    process.exit(1)
  }

  childPid = readChildPid(runDir)
  signalChildGroup(childPid, 'SIGTERM')
  const termStatus = await pollForStatus(runDir, childPid, Date.now() + STOP_TERM_GRACE_MS)
  if (termStatus !== null)
    emitVerdict(runDir, verdictLineForStatus(termStatus, true), 0, stopExtras())

  // The child ignored or outlived SIGTERM. Escalate before writing anything: a terminal status
  // written over a LIVE child is the worst outcome this subcommand has, because the orphan goes on
  // holding the worktree/bazel lock while the run dir now reads terminal, hiding it from the very
  // caller that asked for it to be reclaimed. SIGKILL is uncatchable, so confirming death after it
  // is what makes "the held lock is released" a true statement rather than a hopeful one.
  childPid = readChildPid(runDir)
  signalChildGroup(childPid, 'SIGKILL')
  const killStatus = await pollForStatus(runDir, childPid, Date.now() + STOP_KILL_GRACE_MS)
  if (killStatus !== null)
    emitVerdict(runDir, verdictLineForStatus(killStatus, true), 0, stopExtras())

  const survived = Number.isInteger(childPid) && childPid > 0 && isPidAlive(childPid)
  const stoppedStatus = 128 + (os.constants.signals.SIGTERM ?? 15)
  try {
    if (!survived) cleanupOrphanCache(runDir)
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
    emitVerdict(runDir, verdictLineForStatus(stoppedStatus, true), 1, stopExtras())
  }
  emitVerdict(runDir, verdictLineForStatus(stoppedStatus, true), 0, stopExtras())
}

if (isMainModule(import.meta.url)) {
  const [subcommand, ...args] = process.argv.slice(2)
  if (subcommand === 'start') start(args)
  else if (subcommand === 'supervise') await supervise(args)
  else if (subcommand === 'wait') await wait(args)
  else if (subcommand === 'stop') await stop(args)
  else usage()
}
