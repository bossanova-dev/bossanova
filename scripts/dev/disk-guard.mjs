#!/usr/bin/env node

// Runs recover-disk-space when free space drops below DISK_GUARD_MIN_FREE_GIB (75) and six hours
// have passed since its last run, or a day has passed with space to spare, and raises a macOS
// notification when the run leaves space still low.
// launchd starts it every 30 minutes (scripts/dev/launchd/com.bossanova.disk-guard.plist.example)
// and never overlaps two runs of one job, so a slow cleanup is not started twice.

import { execFileSync } from 'node:child_process'
import * as fs from 'node:fs'
import { homedir, platform } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'

import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { GiB, formatGiB } from './clean-go-build-cache.mjs'

const HOUR = 60 * 60 * 1000
const DAY = 24 * HOUR
// A sweep that could not free enough space would otherwise rerun its package-manager cleans
// beside live sessions every 30 minutes.
const LOW_SPACE_INTERVAL = 6 * HOUR

/** Why a cleanup should run now, or null when it can wait. */
export function guardReason({ freeBytes, minFreeBytes, lastRunMs, now, intervalMs = DAY }) {
  const sinceLastRun = now - lastRunMs
  if (freeBytes < minFreeBytes && sinceLastRun >= LOW_SPACE_INTERVAL) return 'low-space'
  if (sinceLastRun >= intervalMs) return 'scheduled'
  return null
}

function freeBytes(path) {
  const { bavail, bsize } = fs.statfsSync(path)
  return Number(bavail) * Number(bsize)
}

function lastRunMs(stamp) {
  try {
    return fs.statSync(stamp).mtimeMs
  } catch {
    return 0
  }
}

function notify(message) {
  if (platform() !== 'darwin') return
  const script = `display notification ${JSON.stringify(message)} with title "Disk space low"`
  try {
    execFileSync('osascript', ['-e', script], { stdio: 'ignore' })
  } catch {
    // A notification is a courtesy; the log line below still records the shortfall.
  }
}

function main() {
  const { values } = parseArgs({
    options: { 'min-free-gib': { type: 'string' } },
    strict: true,
  })
  const minFreeGiB = Number(values['min-free-gib'] ?? process.env.DISK_GUARD_MIN_FREE_GIB ?? '75')
  if (!Number.isFinite(minFreeGiB) || minFreeGiB <= 0) {
    throw new Error('the minimum free space must be a positive number of GiB')
  }
  const home = homedir()
  const stamp = join(home, '.cache', 'bossanova', 'recover-disk-space.last')
  const now = Date.now()
  const before = freeBytes(home)
  const reason = guardReason({
    freeBytes: before,
    minFreeBytes: minFreeGiB * GiB,
    lastRunMs: lastRunMs(stamp),
    now,
  })
  const log = (line) => process.stdout.write(`${new Date().toISOString()} disk-guard: ${line}\n`)
  if (reason === null) return
  log(`${formatGiB(before)} free; running recover-disk-space (${reason})`)
  fs.mkdirSync(dirname(stamp), { recursive: true })
  fs.writeFileSync(stamp, '')
  const script = join(dirname(fileURLToPath(import.meta.url)), 'recover-disk-space.mjs')
  try {
    execFileSync(process.execPath, [script], { stdio: 'inherit' })
  } catch (error) {
    log(`recover-disk-space failed: ${error.message.split('\n')[0]}`)
  }
  const after = freeBytes(home)
  log(`${formatGiB(after)} free after cleanup`)
  if (after < minFreeGiB * GiB) {
    notify(`Only ${formatGiB(after)} free after cleanup. Run make recover-disk-space for more.`)
  }
}

if (isMainModule(import.meta.url)) main()
