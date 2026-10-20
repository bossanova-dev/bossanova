#!/usr/bin/env node

import { spawn } from 'node:child_process'
import { constants } from 'node:os'
import { parseArgs } from 'node:util'

import { isMainModule } from '../skills-toolbox/main-module.mjs'

// BOS-1690: Bazel treats a rejected remote-cache key as a WARNING and carries on uncached, so a
// placeholder BUILDBUDDY_API_KEY ran every CI build cold for months with every step green.
const AUTH_FAILURE = /Remote Cache:[^\n]*\b(UNAUTHENTICATED|PERMISSION_DENIED)\b/
const MAX_CARRY = 4096

export function findAuthFailure(line) {
  return AUTH_FAILURE.exec(line)?.[1] ?? null
}

function lineScanner(onFailure) {
  let carry = ''
  const check = (line) => {
    const status = findAuthFailure(line)
    if (status) onFailure(status)
  }
  return {
    write(text) {
      const lines = (carry + text).split('\n')
      carry = lines.pop().slice(-MAX_CARRY)
      lines.forEach(check)
    },
    end() {
      check(carry)
    },
  }
}

function exitCodeFor({ error, code, signal }) {
  if (error) return 127
  if (typeof code === 'number') return code
  return 128 + (constants.signals[signal] ?? 0)
}

async function main(argv = process.argv.slice(2)) {
  const { positionals: command } = parseArgs({
    args: argv,
    options: {},
    strict: true,
    allowPositionals: true,
  })
  if (command.length === 0) {
    process.stderr.write('usage: node scripts/remote-cache-auth-guard.mjs -- <cmd> [args...]\n')
    process.exitCode = 2
    return
  }

  const guarded = Boolean(process.env.BUILDBUDDY_API_KEY)
  let failure = null
  const child = spawn(command[0], command.slice(1), { stdio: ['inherit', 'pipe', 'pipe'] })
  /** @type {[import('node:stream').Readable, NodeJS.WritableStream][]} */
  const pipes = [
    [child.stdout, process.stdout],
    [child.stderr, process.stderr],
  ]
  for (const [source, sink] of pipes) {
    const scanner = lineScanner((status) => {
      failure ??= status
    })
    source.on('data', (chunk) => {
      sink.write(chunk)
      if (guarded) scanner.write(chunk.toString('latin1'))
    })
    source.on('end', () => {
      if (guarded) scanner.end()
    })
  }

  const result = await new Promise((resolve) => {
    child.on('error', (error) => resolve({ error }))
    child.on('close', (code, signal) => resolve({ code, signal }))
  })
  if (result.error) process.stderr.write(`${command[0]}: ${result.error.message}\n`)

  const status = exitCodeFor(result)
  if (failure) {
    process.stderr.write(
      `remote cache auth failed: Bazel printed "Remote Cache: ${failure}", so the BUILDBUDDY_API_KEY secret is invalid or lacks permission and this run was uncached\n`,
    )
  }
  process.exitCode = status === 0 && failure ? 1 : status
}

if (isMainModule(import.meta.url)) await main()
