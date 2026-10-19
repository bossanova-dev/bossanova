#!/usr/bin/env node

import { execFileSync } from 'node:child_process'
import * as fs from 'node:fs'
import { isAbsolute, join } from 'node:path'
import { isMainModule } from '../skills-toolbox/main-module.mjs'

export const GiB = 1024 ** 3
const DAY = 24 * 60 * 60 * 1000
const CACHE_SIGNATURE = 'This directory holds cached build artifacts from the Go build system.'
const BUCKET = /^[0-9a-f]{2}$/
const ENTRY = /^[0-9a-f]{64}-[ad]$/

export function parseThresholdGiB(value) {
  const thresholdGiB = Number(value)
  if (!Number.isFinite(thresholdGiB) || thresholdGiB <= 0) {
    throw new Error('GO_CACHE_MAX_GIB must be a positive number')
  }
  return thresholdGiB
}

function lstat(fsImpl, path) {
  try {
    return fsImpl.lstatSync(path)
  } catch (error) {
    if (error.code === 'ENOENT') return null
    throw error
  }
}

function validateCache(dir, fsImpl) {
  if (!isAbsolute(dir)) throw new Error('Go cache directory must be absolute')
  if (!lstat(fsImpl, dir)?.isDirectory()) {
    throw new Error('Go cache directory must be an ordinary directory')
  }
  const readme = join(dir, 'README')
  if (!lstat(fsImpl, readme)?.isFile()) throw new Error('Go cache README is missing or unsafe')
  if (fsImpl.readFileSync(readme, 'utf8').split('\n', 1)[0] !== CACHE_SIGNATURE) {
    throw new Error('Go cache README signature does not match')
  }
}

function scanCache(dir, fsImpl) {
  validateCache(dir, fsImpl)
  let bytes = 0
  const entries = []
  for (const name of fsImpl.readdirSync(dir)) {
    const path = join(dir, name)
    const stat = lstat(fsImpl, path)
    if (stat?.isFile()) bytes += stat.size
    if (!BUCKET.test(name) || !stat?.isDirectory()) continue
    for (const filename of fsImpl.readdirSync(path)) {
      const entryPath = join(path, filename)
      const entryStat = lstat(fsImpl, entryPath)
      if (!entryStat?.isFile()) continue
      bytes += entryStat.size
      if (ENTRY.test(filename)) entries.push({ path: entryPath, bucket: path, stat: entryStat })
    }
  }
  return { bytes, entries }
}

/** Evict only old Go build-cache entries; active builds refresh their mtimes. */
export function evictGoCache({ dir, targetBytes, minAgeMs = DAY, now = Date.now(), fsImpl = fs }) {
  if (!Number.isFinite(targetBytes) || targetBytes < 0)
    throw new Error('targetBytes must be non-negative')
  if (!Number.isFinite(minAgeMs) || minAgeMs < 0 || !Number.isFinite(now)) {
    throw new Error('minAgeMs and now must be finite, with a non-negative minimum age')
  }
  const cache = scanCache(dir, fsImpl)
  const initialBytes = cache.bytes
  let bytes = initialBytes
  let reclaimedBytes = 0
  let entriesRemoved = 0
  cache.entries.sort((a, b) => a.stat.mtimeMs - b.stat.mtimeMs || a.path.localeCompare(b.path))
  for (const entry of cache.entries) {
    if (bytes <= targetBytes) break
    if (entry.stat.mtimeMs > now - minAgeMs) continue
    // Recheck the path and entry after the walk: Go may have refreshed it meanwhile.
    validateCache(dir, fsImpl)
    if (!lstat(fsImpl, entry.bucket)?.isDirectory()) continue
    const current = lstat(fsImpl, entry.path)
    if (
      !current?.isFile() ||
      current.mtimeMs > now - minAgeMs ||
      current.mtimeMs !== entry.stat.mtimeMs ||
      current.ino !== entry.stat.ino ||
      current.dev !== entry.stat.dev ||
      current.size !== entry.stat.size
    )
      continue
    try {
      fsImpl.unlinkSync(entry.path)
    } catch (error) {
      if (error.code === 'ENOENT') continue
      throw error
    }
    bytes -= current.size
    reclaimedBytes += current.size
    entriesRemoved++
  }
  return { initialBytes, bytes: scanCache(dir, fsImpl).bytes, reclaimedBytes, entriesRemoved }
}

function formatGiB(bytes) {
  return `${(bytes / GiB).toFixed(2)} GiB`
}

export function runCleanup({
  env = process.env,
  execFileSync: executor = execFileSync,
  fsImpl = fs,
  now = Date.now(),
  write = (line) => process.stdout.write(line),
} = {}) {
  const thresholdBytes = parseThresholdGiB(env.GO_CACHE_MAX_GIB ?? '100') * GiB
  const directory = String(executor('go', ['env', 'GOCACHE'], { encoding: 'utf8' })).trim()
  const { bytes } = scanCache(directory, fsImpl)
  if (bytes < thresholdBytes) {
    write(
      `clean-cache: ${directory} is ${formatGiB(bytes)}, below threshold ${formatGiB(thresholdBytes)}; skipping.\n`,
    )
    return { action: 'skip', reason: 'below-threshold', bytes, thresholdBytes }
  }
  const result = evictGoCache({ dir: directory, targetBytes: thresholdBytes * 0.8, now, fsImpl })
  write(
    `clean-cache: cleaned ${directory}: ${formatGiB(result.initialBytes)} -> ${formatGiB(result.bytes)} (reclaimed ${formatGiB(result.reclaimedBytes)}, ${result.entriesRemoved} entries).\n`,
  )
  return { action: 'clean', ...result, thresholdBytes }
}

function main() {
  try {
    runCleanup()
  } catch (error) {
    process.stderr.write(`clean-cache: ${error.message}\n`)
    process.exitCode = 1
  }
}

if (isMainModule(import.meta.url)) main()
