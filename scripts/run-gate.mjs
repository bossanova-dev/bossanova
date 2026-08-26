#!/usr/bin/env node

import { spawn } from 'node:child_process'
import {
  ENV_FAILURE_EXIT_CODE,
  classifyEnvironmentFailure,
  environmentFailureBanner,
} from './env-failure-lib.mjs'

const WINDOW_LIMIT_BYTES = 256 * 1024

function parseArgs(argv) {
  let label = ''
  const command = []
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i]
    if (arg === '--label') {
      label = argv[++i] ?? ''
    } else if (arg === '--') {
      command.push(...argv.slice(i + 1))
      break
    } else {
      throw new Error(`unknown argument: ${arg}`)
    }
  }
  if (command.length === 0 || !command[0]) {
    throw new Error('usage: node scripts/run-gate.mjs --label <name> -- <cmd> [args...]')
  }
  return { label, command }
}

function appendWindow(current, chunk) {
  const next = Buffer.concat([current, chunk])
  if (next.byteLength <= WINDOW_LIMIT_BYTES) return next
  return next.subarray(next.byteLength - WINDOW_LIMIT_BYTES)
}

async function main(argv = process.argv.slice(2)) {
  const { label, command } = parseArgs(argv)
  let outputWindow = Buffer.alloc(0)
  let observedClassification = null

  const child = spawn(command[0], command.slice(1), {
    stdio: ['inherit', 'pipe', 'pipe'],
    env: process.env,
  })

  child.stdout.on('data', (chunk) => {
    outputWindow = appendWindow(outputWindow, chunk)
    observedClassification ??= classifyEnvironmentFailure(outputWindow.toString('utf8'))
    process.stdout.write(chunk)
  })
  child.stderr.on('data', (chunk) => {
    outputWindow = appendWindow(outputWindow, chunk)
    observedClassification ??= classifyEnvironmentFailure(outputWindow.toString('utf8'))
    process.stderr.write(chunk)
  })

  const result = await new Promise((resolve) => {
    child.on('error', (error) => resolve({ error }))
    child.on('close', (status, signal) => resolve({ status, signal }))
  })

  if (result.error) {
    process.stderr.write(`${result.error.message}\n`)
    process.exitCode = 1
    return
  }

  if (result.status === 0) {
    process.exitCode = 0
    return
  }

  const classification =
    observedClassification ?? classifyEnvironmentFailure(outputWindow.toString('utf8'))
  if (classification) {
    process.stderr.write(
      `${environmentFailureBanner({
        kind: classification.kind,
        remedy: classification.remedy,
        label,
      })}\n`,
    )
    process.exitCode = ENV_FAILURE_EXIT_CODE
    return
  }

  if (typeof result.status === 'number') {
    process.exitCode = result.status
    return
  }

  process.stderr.write(`gate terminated by signal ${result.signal ?? 'unknown'}\n`)
  process.exitCode = 1
}

main().catch((error) => {
  process.stderr.write(`${error.stack ?? error.message}\n`)
  process.exitCode = 1
})
