#!/usr/bin/env node

// On every wake, first removes landed Claude Code agent worktrees (agent-worktree-reaper.mjs).
// Runs recover-disk-space when free space drops below DISK_GUARD_MIN_FREE_GIB (75) and six hours
// have passed since its last run, or a day has passed with space to spare, and raises a macOS
// notification when the run leaves space still low.
// launchd starts it every 30 minutes (scripts/dev/launchd/com.bossanova.disk-guard.plist.example)
// and never overlaps two runs of one job, so a slow cleanup is not started twice.

import { execFileSync } from 'node:child_process'
import * as fs from 'node:fs'
import { homedir, platform } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'

import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { GiB, formatGiB } from './clean-go-build-cache.mjs'

const HOUR = 60 * 60 * 1000
const DAY = 24 * HOUR
// A sweep that could not free enough space would otherwise rerun its package-manager cleans
// beside live sessions every 30 minutes.
const LOW_SPACE_INTERVAL = 6 * HOUR
const AGENT_PASS_TIMEOUT = 10 * 60 * 1000

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

/**
 * One launchd wake: the landed-agent-worktree pass, which runs every time, then the free-space
 * check. A failing pass is logged and never stops the check. Returns the cleanup reason, if any.
 * @param {{
 *   runAgentPass: () => void,
 *   freeBytes: () => number,
 *   minFreeBytes: number,
 *   lastRunMs: number,
 *   now: number,
 *   runCleanup: (reason: string, before: number) => void,
 *   log: (line: string) => void,
 * }} steps
 */
export function wake({ runAgentPass, freeBytes, minFreeBytes, lastRunMs, now, runCleanup, log }) {
  try {
    runAgentPass()
  } catch (error) {
    log(`agent-worktree-reaper failed: ${String(error?.message ?? error).split('\n')[0]}`)
  }
  const before = freeBytes()
  const reason = guardReason({ freeBytes: before, minFreeBytes, lastRunMs, now })
  if (reason !== null) runCleanup(reason, before)
  return reason
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
  const here = dirname(fileURLToPath(import.meta.url))
  // launchd starts the guard in /, and the scripts find their repository from their cwd.
  const repoRoot = resolve(here, '..', '..')
  const log = (line) => process.stdout.write(`${new Date().toISOString()} disk-guard: ${line}\n`)
  wake({
    runAgentPass: () =>
      execFileSync(process.execPath, [join(here, 'agent-worktree-reaper.mjs')], {
        stdio: 'inherit',
        cwd: repoRoot,
        timeout: AGENT_PASS_TIMEOUT,
      }),
    freeBytes: () => freeBytes(home),
    minFreeBytes: minFreeGiB * GiB,
    lastRunMs: lastRunMs(stamp),
    now: Date.now(),
    runCleanup: (reason, before) => {
      log(`${formatGiB(before)} free; running recover-disk-space (${reason})`)
      fs.mkdirSync(dirname(stamp), { recursive: true })
      fs.writeFileSync(stamp, '')
      try {
        execFileSync(process.execPath, [join(here, 'recover-disk-space.mjs')], {
          stdio: 'inherit',
          cwd: repoRoot,
        })
      } catch (error) {
        log(`recover-disk-space failed: ${error.message.split('\n')[0]}`)
      }
      const after = freeBytes(home)
      log(`${formatGiB(after)} free after cleanup`)
      if (after < minFreeGiB * GiB) {
        notify(`Only ${formatGiB(after)} free after cleanup. Run make recover-disk-space for more.`)
      }
    },
    log,
  })
}

if (isMainModule(import.meta.url)) main()
