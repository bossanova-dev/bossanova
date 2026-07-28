#!/usr/bin/env node

import { execFileSync } from 'node:child_process'
import { isAbsolute } from 'node:path'
import { pathToFileURL } from 'node:url'

export const GiB = 1024 ** 3

const GO_BUILD_PROCESS_NAMES = ['go', 'compile', 'link']

export function parseThresholdGiB(value) {
  const thresholdGiB = Number(value)
  if (!Number.isFinite(thresholdGiB) || thresholdGiB <= 0) {
    throw new Error('GO_CACHE_MAX_GIB must be a positive number')
  }
  return thresholdGiB
}

export function shouldClean({ bytes, thresholdBytes, goBuildActive }) {
  if (bytes < thresholdBytes) {
    return { action: 'skip', reason: 'below-threshold' }
  }
  if (goBuildActive) {
    return { action: 'skip', reason: 'go-build-active' }
  }
  return { action: 'clean' }
}

function commandOutput(executor, command, args) {
  return String(executor(command, args, { encoding: 'utf8' })).trim()
}

function cacheDirectory(executor) {
  const directory = commandOutput(executor, 'go', ['env', 'GOCACHE'])
  if (!isAbsolute(directory)) {
    throw new Error(`go env GOCACHE returned an invalid directory: ${directory || '(empty)'}`)
  }
  return directory
}

function cacheBytes(executor, directory) {
  const output = commandOutput(executor, 'du', ['-sk', directory])
  const kibibytes = Number(output.split(/\s+/, 1)[0])
  if (!Number.isSafeInteger(kibibytes) || kibibytes < 0) {
    throw new Error(`could not measure Go build cache: ${output || '(empty output)'}`)
  }
  return kibibytes * 1024
}

function goBuildActive(executor) {
  for (const processName of GO_BUILD_PROCESS_NAMES) {
    try {
      executor('pgrep', ['-x', processName], { stdio: 'ignore' })
      return true
    } catch (error) {
      if (error?.status === 1) {
        continue
      }
      throw new Error(`could not check for active ${processName} processes: ${error.message}`)
    }
  }
  return false
}

function formatGiB(bytes) {
  return `${(bytes / GiB).toFixed(2)} GiB`
}

function reportSkip({ directory, bytes, thresholdBytes, reason, write }) {
  if (reason === 'go-build-active') {
    write('clean-cache: Go build process is active; skipping cache cleanup.\n')
    return
  }

  write(
    `clean-cache: ${directory} is ${formatGiB(bytes)}, below threshold ${formatGiB(thresholdBytes)}; skipping.\n`,
  )
}

export function runCleanup({
  env = process.env,
  execFileSync: executor = execFileSync,
  write = (line) => process.stdout.write(line),
} = {}) {
  const thresholdGiB = parseThresholdGiB(env.GO_CACHE_MAX_GIB ?? '100')
  const thresholdBytes = thresholdGiB * GiB
  const directory = cacheDirectory(executor)
  const initialBytes = cacheBytes(executor, directory)

  let decision = shouldClean({
    bytes: initialBytes,
    thresholdBytes,
    goBuildActive: initialBytes >= thresholdBytes && goBuildActive(executor),
  })
  if (decision.action === 'skip') {
    reportSkip({ directory, bytes: initialBytes, thresholdBytes, reason: decision.reason, write })
    return { ...decision, bytes: initialBytes, thresholdBytes }
  }

  const recheckedBytes = cacheBytes(executor, directory)
  decision = shouldClean({
    bytes: recheckedBytes,
    thresholdBytes,
    goBuildActive: recheckedBytes >= thresholdBytes && goBuildActive(executor),
  })
  if (decision.action === 'skip') {
    reportSkip({ directory, bytes: recheckedBytes, thresholdBytes, reason: decision.reason, write })
    return { ...decision, bytes: recheckedBytes, thresholdBytes }
  }

  executor('go', ['clean', '-cache'], { stdio: 'inherit' })
  const remainingBytes = cacheBytes(executor, directory)
  const reclaimedBytes = Math.max(0, initialBytes - remainingBytes)
  write(
    `clean-cache: cleaned ${directory}: ${formatGiB(initialBytes)} -> ${formatGiB(remainingBytes)} (reclaimed ${formatGiB(reclaimedBytes)}).\n`,
  )

  return {
    action: 'clean',
    bytes: remainingBytes,
    reclaimedBytes,
    thresholdBytes,
  }
}

function main() {
  try {
    runCleanup()
  } catch (error) {
    process.stderr.write(`clean-cache: ${error.message}\n`)
    process.exitCode = 1
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main()
}
