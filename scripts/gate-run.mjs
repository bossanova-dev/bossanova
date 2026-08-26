#!/usr/bin/env node

import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import process from 'node:process'
import { spawn } from 'node:child_process'
import { fileURLToPath } from 'node:url'

import { isMainModule } from '../skills-toolbox/main-module.mjs'

export const VERDICTS = Object.freeze({
  passed: 'GATE PASSED',
  failedPrefix: 'GATE FAILED (exit ',
  vanished: 'GATE UNKNOWN - vanished',
  stillRunning: 'GATE UNKNOWN - still running',
})

const DEFAULT_TIMEOUT_MS = 600_000
const POLL_MS = 250

function usage(exitCode = 2) {
  const stream = exitCode === 0 ? process.stdout : process.stderr
  stream.write(
    [
      'usage:',
      '  node scripts/gate-run.mjs start -- <cmd...>',
      '  node scripts/gate-run.mjs wait [--timeout <ms>] <run-dir>',
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

  const logFd = fs.openSync(path.join(runDir, 'log'), 'a')
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

  process.stdout.write(`${runDir}\n${supervisor.pid}\n`)
}

function supervise(args) {
  const sep = args.indexOf('--')
  if (sep <= 0 || sep === args.length - 1) usage()

  const runDir = args[0]
  const command = args.slice(sep + 1)
  const logFd = fs.openSync(path.join(runDir, 'log'), 'a')
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
        process.stdout.write(`${VERDICTS.passed}\n`)
        process.exit(0)
      }
      process.stdout.write(`${VERDICTS.failedPrefix}${Number.isInteger(status) ? status : 1})\n`)
      process.exit(1)
    }

    const pid = readPid(runDir)
    if (!Number.isInteger(pid) || !isPidAlive(pid)) {
      process.stdout.write(`${VERDICTS.vanished}\n`)
      process.exit(97)
    }
    await new Promise((resolve) => setTimeout(resolve, POLL_MS))
  }

  process.stdout.write(`${VERDICTS.stillRunning}\n`)
  process.exit(98)
}

if (isMainModule(import.meta.url)) {
  const [subcommand, ...args] = process.argv.slice(2)
  if (subcommand === 'start') start(args)
  else if (subcommand === 'supervise') supervise(args)
  else if (subcommand === 'wait') await wait(args)
  else usage()
}
