#!/usr/bin/env node

import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

import { GiB, parseThresholdGiB, runCleanup, shouldClean } from './clean-go-build-cache.mjs'

const KiB_PER_GIB = GiB / 1024

function commandError(status) {
  const error = new Error(`command exited with ${status}`)
  error.status = status
  return error
}

function createExecutor({ cacheDir = '/tmp/go-build', sizesKiB = [], activeChecks = [] } = {}) {
  const calls = []

  return {
    calls,
    execFileSync(command, args) {
      calls.push([command, args])

      if (command === 'go' && args[0] === 'env') {
        return `${cacheDir}\n`
      }

      if (command === 'du') {
        return `${sizesKiB.shift()}\t${cacheDir}\n`
      }

      if (command === 'pgrep') {
        if (activeChecks.shift()) {
          return '123\n'
        }
        throw commandError(1)
      }

      if (command === 'go' && args[0] === 'clean') {
        return ''
      }

      throw new Error(`unexpected command: ${command} ${args.join(' ')}`)
    },
  }
}

test('parses a positive cache threshold in GiB', () => {
  assert.equal(parseThresholdGiB('100'), 100)
  assert.equal(parseThresholdGiB('1.5'), 1.5)
  assert.throws(() => parseThresholdGiB('0'), /positive number/)
  assert.throws(() => parseThresholdGiB('not-a-number'), /positive number/)
})

test('only cleans a cache at or above the threshold when Go is idle', () => {
  assert.equal(
    shouldClean({ bytes: 99 * GiB, thresholdBytes: 100 * GiB, goBuildActive: false }).action,
    'skip',
  )
  assert.equal(
    shouldClean({ bytes: 100 * GiB, thresholdBytes: 100 * GiB, goBuildActive: false }).action,
    'clean',
  )
  assert.equal(
    shouldClean({ bytes: 101 * GiB, thresholdBytes: 100 * GiB, goBuildActive: true }).action,
    'skip',
  )
})

test('does not invoke Go cleanup below the configured threshold', () => {
  const executor = createExecutor({ sizesKiB: [99 * KiB_PER_GIB] })
  const lines = []

  const result = runCleanup({
    env: { GO_CACHE_MAX_GIB: '100' },
    execFileSync: executor.execFileSync,
    write: (line) => lines.push(line),
  })

  assert.equal(result.action, 'skip')
  assert.equal(result.reason, 'below-threshold')
  assert.equal(
    executor.calls.some(([command, args]) => command === 'go' && args[0] === 'clean'),
    false,
  )
  assert.match(lines.join(''), /below threshold/)
})

test('does not invoke Go cleanup while a Go build process is active', () => {
  const executor = createExecutor({ sizesKiB: [101 * KiB_PER_GIB], activeChecks: [true] })
  const lines = []

  const result = runCleanup({
    env: { GO_CACHE_MAX_GIB: '100' },
    execFileSync: executor.execFileSync,
    write: (line) => lines.push(line),
  })

  assert.equal(result.action, 'skip')
  assert.equal(result.reason, 'go-build-active')
  assert.equal(
    executor.calls.some(([command, args]) => command === 'go' && args[0] === 'clean'),
    false,
  )
  assert.match(lines.join(''), /Go build process is active/)
})

test('rechecks for active Go builds immediately before cleanup', () => {
  const executor = createExecutor({
    sizesKiB: [101 * KiB_PER_GIB, 101 * KiB_PER_GIB],
    activeChecks: [false, false, false, true],
  })

  const result = runCleanup({
    env: { GO_CACHE_MAX_GIB: '100' },
    execFileSync: executor.execFileSync,
    write: () => {},
  })

  assert.equal(result.action, 'skip')
  assert.equal(result.reason, 'go-build-active')
  assert.equal(
    executor.calls.some(([command, args]) => command === 'go' && args[0] === 'clean'),
    false,
  )
})

test('cleans an oversized idle cache and reports reclaimed space', () => {
  const executor = createExecutor({
    sizesKiB: [101 * KiB_PER_GIB, 101 * KiB_PER_GIB, KiB_PER_GIB],
    activeChecks: [false, false, false, false, false, false],
  })
  const lines = []

  const result = runCleanup({
    env: { GO_CACHE_MAX_GIB: '100' },
    execFileSync: executor.execFileSync,
    write: (line) => lines.push(line),
  })

  assert.equal(result.action, 'clean')
  assert.equal(result.reclaimedBytes, 100 * GiB)
  assert.equal(
    executor.calls.some(([command, args]) => command === 'go' && args.join(' ') === 'clean -cache'),
    true,
  )
  assert.match(lines.join(''), /reclaimed 100.00 GiB/)
})

test('launchd template schedules the guarded Make target daily', () => {
  const template = readFileSync(
    'scripts/launchd/com.bossanova.clean-go-build-cache.plist.example',
    'utf8',
  )

  assert.match(template, /__REPO_ROOT__/)
  assert.match(template, /make clean-cache/)
  assert.match(template, /<key>Hour<\/key>\s*<integer>4<\/integer>/)
  assert.match(template, /<key>Minute<\/key>\s*<integer>15<\/integer>/)
})
