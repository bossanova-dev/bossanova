#!/usr/bin/env node

import assert from 'node:assert/strict'
import * as fs from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import test from 'node:test'

import {
  GOLANGCI_CACHE_SIGNATURE,
  GiB,
  evictGoCache,
  parseThresholdGiB,
  runCleanup,
} from './clean-go-build-cache.mjs'

const NOW = Date.UTC(2026, 9, 20)
const DAY = 24 * 60 * 60 * 1000
const SIGNATURE = 'This directory holds cached build artifacts from the Go build system.'

function fixture(t) {
  const dir = fs.mkdtempSync(join(tmpdir(), 'go-cache-eviction-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }))
  fs.writeFileSync(join(dir, 'README'), `${SIGNATURE}\n`)
  return dir
}

function entry(dir, id, { age = 2 * DAY, size = 10, suffix = 'd' } = {}) {
  const bucket = join(dir, 'ab')
  fs.mkdirSync(bucket, { recursive: true })
  const path = join(bucket, `${id.toString(16).padStart(64, '0')}-${suffix}`)
  fs.writeFileSync(path, Buffer.alloc(size))
  fs.utimesSync(path, new Date(NOW - age), new Date(NOW - age))
  return path
}

test('parses positive cache thresholds', () => {
  assert.equal(parseThresholdGiB('100'), 100)
  assert.equal(parseThresholdGiB('1.5'), 1.5)
  assert.throws(() => parseThresholdGiB('0'), /positive number/)
  assert.throws(() => parseThresholdGiB('bad'), /positive number/)
})

test('evicts oldest entries first and stops at target', (t) => {
  const dir = fixture(t)
  const newer = entry(dir, 1)
  const oldest = entry(dir, 2, { age: 3 * DAY, suffix: 'a' })
  const protectedBytes = fs.statSync(join(dir, 'README')).size
  const result = evictGoCache({ dir, targetBytes: protectedBytes + 10, now: NOW })
  assert.equal(fs.existsSync(oldest), false)
  assert.equal(fs.existsSync(newer), true)
  assert.deepEqual(result, {
    initialBytes: protectedBytes + 20,
    bytes: protectedBytes + 10,
    reclaimedBytes: 10,
    entriesRemoved: 1,
  })
})

test('keeps young entries even when target cannot be reached', (t) => {
  const dir = fixture(t)
  const old = entry(dir, 1)
  const young = entry(dir, 2, { age: DAY - 1 })
  const result = evictGoCache({ dir, targetBytes: 0, now: NOW })
  assert.equal(fs.existsSync(old), false)
  assert.equal(fs.existsSync(young), true)
  assert.equal(result.entriesRemoved, 1)
  assert.ok(result.bytes > 0)
})

test('refuses invalid cache roots and invalid README signatures', (t) => {
  const dir = fixture(t)
  const old = entry(dir, 1)
  assert.throws(() => evictGoCache({ dir: 'relative', targetBytes: 0 }), /absolute/)
  fs.unlinkSync(join(dir, 'README'))
  assert.throws(() => evictGoCache({ dir, targetBytes: 0 }), /README/)
  fs.writeFileSync(join(dir, 'README'), `wrong\n${SIGNATURE}\n`)
  assert.throws(() => evictGoCache({ dir, targetBytes: 0 }), /README/)
  assert.equal(fs.existsSync(old), true)
  const alias = join(dir, 'alias')
  fs.symlinkSync(dir, alias)
  assert.throws(() => evictGoCache({ dir: alias, targetBytes: 0 }), /directory/)
  fs.unlinkSync(join(dir, 'README'))
  fs.writeFileSync(join(dir, 'signature'), SIGNATURE)
  fs.symlinkSync(join(dir, 'signature'), join(dir, 'README'))
  assert.throws(() => evictGoCache({ dir, targetBytes: 0 }), /README/)
})

test('evicts a golangci-lint cache only under its own signature', (t) => {
  const dir = fixture(t)
  const old = entry(dir, 1)
  const signature = GOLANGCI_CACHE_SIGNATURE
  assert.throws(() => evictGoCache({ dir, targetBytes: 0, now: NOW, signature }), /signature/)
  assert.equal(fs.existsSync(old), true)
  fs.writeFileSync(join(dir, 'README'), `${signature}\n`)
  assert.throws(() => evictGoCache({ dir, targetBytes: 0, now: NOW }), /signature/)
  assert.equal(evictGoCache({ dir, targetBytes: 0, now: NOW, signature }).entriesRemoved, 1)
  assert.equal(fs.existsSync(old), false)
})

test('only removes recognized files and never follows bucket or entry symlinks', (t) => {
  const dir = fixture(t)
  const old = entry(dir, 1)
  const external = fs.mkdtempSync(join(tmpdir(), 'go-cache-protected-'))
  t.after(() => fs.rmSync(external, { recursive: true, force: true }))
  const outside = join(external, `${'f'.repeat(64)}-d`)
  fs.writeFileSync(outside, 'outside')
  const protectedPaths = [join(dir, 'trim.txt'), join(dir, 'ab', 'not-an-entry')]
  for (const path of protectedPaths) fs.writeFileSync(path, 'keep')
  fs.mkdirSync(join(dir, 'fuzz'))
  fs.writeFileSync(join(dir, 'fuzz', `${'e'.repeat(64)}-d`), 'keep')
  fs.symlinkSync(external, join(dir, 'ff'))
  const symlink = join(dir, 'ab', `${'e'.repeat(64)}-d`)
  fs.symlinkSync(outside, symlink)
  evictGoCache({ dir, targetBytes: 0, now: NOW })
  assert.equal(fs.existsSync(old), false)
  for (const path of [...protectedPaths, outside, join(dir, 'README')])
    assert.ok(fs.existsSync(path))
  assert.ok(fs.lstatSync(symlink).isSymbolicLink())
  assert.ok(fs.lstatSync(join(dir, 'ff')).isSymbolicLink())
  assert.ok(fs.existsSync(join(dir, 'fuzz', `${'e'.repeat(64)}-d`)))
})

test('rechecks entry mtime before unlinking after a concurrent refresh', (t) => {
  const dir = fixture(t)
  const path = entry(dir, 1)
  let reads = 0
  const fsImpl = {
    ...fs,
    lstatSync(name) {
      if (name === path && ++reads === 2) fs.utimesSync(path, new Date(NOW), new Date(NOW))
      return fs.lstatSync(name)
    },
  }
  const result = evictGoCache({ dir, targetBytes: 0, now: NOW, fsImpl })
  assert.ok(fs.existsSync(path))
  assert.equal(result.entriesRemoved, 0)
})

test('tolerates an entry removed concurrently by another cleaner', (t) => {
  const dir = fixture(t)
  const path = entry(dir, 1)
  const result = evictGoCache({
    dir,
    targetBytes: 0,
    now: NOW,
    fsImpl: {
      ...fs,
      unlinkSync(name) {
        fs.unlinkSync(name)
        fs.unlinkSync(name)
      },
    },
  })
  assert.equal(fs.existsSync(path), false)
  assert.equal(result.entriesRemoved, 0)
})

test('runCleanup evicts an oversized cache without checking for active Go builds', (t) => {
  const dir = fixture(t)
  const old = entry(dir, 1, { size: 100 })
  const calls = []
  const lines = []
  const result = runCleanup({
    env: { GO_CACHE_MAX_GIB: String(150 / GiB) },
    now: NOW,
    execFileSync(command, args) {
      calls.push([command, args])
      if (command === 'go' && args.join(' ') === 'env GOCACHE') return dir
      throw new Error('Go build is active; no process probe or go clean may run')
    },
    write: (line) => lines.push(line),
  })
  assert.equal(result.action, 'clean')
  assert.equal(result.reclaimedBytes, 100)
  assert.equal(result.entriesRemoved, 1)
  assert.equal(fs.existsSync(old), false)
  assert.equal(calls.length, 1)
  assert.match(lines.join(''), /1 entr/)
})

test('runCleanup leaves a cache below threshold untouched with the default threshold', (t) => {
  const dir = fixture(t)
  const old = entry(dir, 1)
  const result = runCleanup({ env: {}, execFileSync: () => dir, write: () => {} })
  assert.equal(result.action, 'skip')
  assert.equal(result.thresholdBytes, 100 * GiB)
  assert.ok(fs.existsSync(old))
})

test('runCleanup stops at eighty percent instead of purging all old entries', (t) => {
  const dir = fixture(t)
  const oldest = entry(dir, 1, { size: 60, age: 3 * DAY })
  const newer = entry(dir, 2, { size: 60 })
  const result = runCleanup({
    env: { GO_CACHE_MAX_GIB: String(180 / GiB) },
    now: NOW,
    execFileSync: () => dir,
    write: () => {},
  })
  assert.equal(result.action, 'clean')
  assert.equal(result.entriesRemoved, 1)
  assert.ok(result.bytes <= 180 * 0.8)
  assert.ok(!fs.existsSync(oldest))
  assert.ok(fs.existsSync(newer))
})
