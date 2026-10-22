#!/usr/bin/env node

import { execFileSync } from 'node:child_process'
import * as fs from 'node:fs'
import { isAbsolute, join } from 'node:path'
import { isMainModule } from '../skills-toolbox/main-module.mjs'

export const GiB = 1024 ** 3
const DAY = 24 * 60 * 60 * 1000
export const GO_CACHE_SIGNATURE =
  'This directory holds cached build artifacts from the Go build system.'
// golangci-lint vendors Go's cache package, so its cache shares the layout under its own README.
export const GOLANGCI_CACHE_SIGNATURE =
  'This directory holds cached build artifacts from golangci-lint.'
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

function validateCache(dir, fsImpl, signature) {
  if (!isAbsolute(dir)) throw new Error('Go cache directory must be absolute')
  if (!lstat(fsImpl, dir)?.isDirectory()) {
    throw new Error('Go cache directory must be an ordinary directory')
  }
  const readme = join(dir, 'README')
  if (!lstat(fsImpl, readme)?.isFile()) throw new Error('Go cache README is missing or unsafe')
  if (fsImpl.readFileSync(readme, 'utf8').split('\n', 1)[0] !== signature) {
    throw new Error('Go cache README signature does not match')
  }
}

/** Total cache bytes; `visit` sees each recognized entry's stat without the walk keeping it. */
function scanCache(dir, fsImpl, signature = GO_CACHE_SIGNATURE, visit = undefined) {
  validateCache(dir, fsImpl, signature)
  let bytes = 0
  for (const name of fsImpl.readdirSync(dir)) {
    const path = join(dir, name)
    const stat = lstat(fsImpl, path)
    if (stat?.isFile()) bytes += stat.size
    if (!BUCKET.test(name) || !stat?.isDirectory()) continue
    for (const filename of fsImpl.readdirSync(path)) {
      const entryStat = lstat(fsImpl, join(path, filename))
      if (!entryStat?.isFile()) continue
      bytes += entryStat.size
      if (ENTRY.test(filename)) visit?.(entryStat)
    }
  }
  return bytes
}

/**
 * Evict only old Go build-cache entries; active builds refresh their mtimes. A cache can hold
 * millions of entries, so the first walk keeps only each entry's mtime and size to find the
 * cutoff that reaches the target, and a second walk removes entries no newer than it.
 */
export function evictGoCache({
  dir,
  targetBytes,
  minAgeMs = DAY,
  now = Date.now(),
  fsImpl = fs,
  signature = GO_CACHE_SIGNATURE,
}) {
  if (!Number.isFinite(targetBytes) || targetBytes < 0)
    throw new Error('targetBytes must be non-negative')
  if (!Number.isFinite(minAgeMs) || minAgeMs < 0 || !Number.isFinite(now)) {
    throw new Error('minAgeMs and now must be finite, with a non-negative minimum age')
  }
  const oldest = now - minAgeMs
  const mtimes = []
  const sizes = []
  const initialBytes = scanCache(dir, fsImpl, signature, (stat) => {
    if (stat.mtimeMs > oldest) return
    mtimes.push(stat.mtimeMs)
    sizes.push(stat.size)
  })
  const order = Uint32Array.from(mtimes.keys()).sort((a, b) => mtimes[a] - mtimes[b])
  let cutoff = -Infinity
  let projected = initialBytes
  for (const i of order) {
    if (projected <= targetBytes) break
    projected -= sizes[i]
    cutoff = mtimes[i]
  }
  let bytes = initialBytes
  let reclaimedBytes = 0
  let entriesRemoved = 0
  if (cutoff > -Infinity) {
    sweep: for (const name of fsImpl.readdirSync(dir)) {
      if (!BUCKET.test(name)) continue
      const bucket = join(dir, name)
      // Recheck the path after the walk: the cache may have been replaced meanwhile.
      validateCache(dir, fsImpl, signature)
      if (!lstat(fsImpl, bucket)?.isDirectory()) continue
      for (const filename of fsImpl.readdirSync(bucket)) {
        if (bytes <= targetBytes) break sweep
        if (!ENTRY.test(filename)) continue
        const path = join(bucket, filename)
        // Go refreshes an entry's mtime when a build reuses it, which lifts it past the cutoff.
        const current = lstat(fsImpl, path)
        if (!current?.isFile() || current.mtimeMs > cutoff || current.mtimeMs > oldest) continue
        try {
          fsImpl.unlinkSync(path)
        } catch (error) {
          if (error.code === 'ENOENT') continue
          throw error
        }
        bytes -= current.size
        reclaimedBytes += current.size
        entriesRemoved++
      }
    }
  }
  return {
    initialBytes,
    bytes: scanCache(dir, fsImpl, signature),
    reclaimedBytes,
    entriesRemoved,
  }
}

export function formatGiB(bytes) {
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
  const bytes = scanCache(directory, fsImpl)
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
