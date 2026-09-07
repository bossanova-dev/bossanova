// plan-scratch-reap.mjs
//
// Age-based cleanup for boss-plan's gitignored plan scratch. It deliberately
// avoids tracker state: when in doubt, keep the file and let a later run try.

import { existsSync, lstatSync, readdirSync, rmSync } from 'node:fs'
import { join } from 'node:path'
import { isMainModule } from './main-module.mjs'

export const STALE_PLAN_SCRATCH_TTL_MS = 24 * 60 * 60 * 1000

function report(onReport, msg) {
  if (onReport) onReport(msg)
}

/**
 * Reap one stale entry, file or directory. Mirrors `reapStaleRunDirs`' discipline
 * in bs-run-sentinel.mjs: remove, then re-check existence, and report rather than
 * throw so one unremovable entry never aborts the sweep.
 * @param {string} path
 * @param {boolean} isDir
 * @param {string[]} removed
 * @param {string[]} failed
 * @param {((msg: string) => void)|undefined} onReport
 */
function reapEntry(path, isDir, removed, failed, onReport) {
  const kind = isDir ? 'directory' : 'file'
  try {
    rmSync(path, { recursive: isDir, force: true })
    if (existsSync(path)) {
      const msg = `warning: stale plan scratch ${kind} survived removal: ${path}`
      failed.push(msg)
      report(onReport, msg)
      return
    }
    removed.push(path)
    report(onReport, `removed stale plan scratch ${kind}: ${path}`)
  } catch (err) {
    const msg = `warning: failed to reap stale plan scratch ${kind} ${path}: ${err.message}`
    failed.push(msg)
    report(onReport, msg)
  }
}

/**
 * Reap stale entries directly under the plan scratch directory — regular files
 * and directories alike.
 *
 * Directories are in scope because the contract can create them: a run's scratch
 * lives in one `run-<RUN-ID>/` directory (see plan-scratch-paths.mjs), so a run
 * that dies before its own cleanup leaves a directory behind. A file-only sweep
 * could never reach it, which is exactly how the legacy `children/` directory
 * became permanently unreachable.
 *
 * TTL semantics are unchanged and apply to the entry itself: a directory whose
 * own mtime is younger than the TTL is kept, so a live run's scratch is never
 * removed out from under it.
 * @param {string} dir
 * @param {{now?: number, ttlMs?: number, onReport?: (msg: string) => void}} [opts]
 * @returns {{removed: string[], failed: string[]}}
 */
export function reapPlanScratch(dir = '.linear-plans', opts = {}) {
  const removed = []
  const failed = []
  const now = opts.now ?? Date.now()
  const ttlMs = opts.ttlMs ?? STALE_PLAN_SCRATCH_TTL_MS
  if (!existsSync(dir)) return { removed, failed }
  let entries
  try {
    entries = readdirSync(dir)
  } catch (err) {
    const msg = `warning: unable to scan plan scratch ${dir}: ${err.message}`
    failed.push(msg)
    report(opts.onReport, msg)
    return { removed, failed }
  }
  for (const name of entries) {
    const path = join(dir, name)
    let st
    try {
      // lstatSync, never statSync: statSync FOLLOWS a symlink, so a link would be
      // classified and TTL-judged by its target — a link created seconds ago whose
      // target is old would be reaped immediately. lstatSync describes the link
      // itself, which reports neither isFile() nor isDirectory(), so the skip below
      // actually holds.
      st = lstatSync(path)
    } catch {
      continue
    }
    const isDir = st.isDirectory()
    // Anything that is neither a regular file nor a directory (a socket, a
    // symlink to elsewhere) is left alone: it is not something this contract
    // creates, so removing it would be acting outside the sweep's remit.
    if ((!st.isFile() && !isDir) || now - st.mtimeMs < ttlMs) continue
    reapEntry(path, isDir, removed, failed, opts.onReport)
  }
  return { removed, failed }
}

if (isMainModule(import.meta.url)) {
  const dir = process.argv[2] || '.linear-plans'
  reapPlanScratch(dir, { onReport: (msg) => process.stderr.write(`${msg}\n`) })
}
