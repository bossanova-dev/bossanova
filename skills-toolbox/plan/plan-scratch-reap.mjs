// plan-scratch-reap.mjs
//
// Age-based cleanup for boss-plan's self-ignoring plan scratch. It deliberately
// avoids tracker state: when in doubt, keep the file and let a later run try.

import { existsSync, lstatSync, readdirSync, rmSync } from 'node:fs'
import { join } from 'node:path'
import { isMainModule } from '../main-module.mjs'
import {
  PLAN_SCRATCH_IGNORE_FILE,
  PLAN_PIN_DIR_PREFIX,
  RUN_SCRATCH_DIR_PREFIX,
} from './plan-scratch-paths.mjs'

export const UNATTENDED_PLAN_PIN_MARKER = '.unattended'
export const COMPLETED_PLAN_PIN_MARKER = '.completed'
export const STALE_PLAN_PIN_TTL_MS = 6 * 60 * 60 * 1000
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
 * Unattended or completed pins can expire by age. Interactive and unknown
 * owners may resume before completion after an arbitrarily long pause, so keep
 * their pins and paired run scratch. Reapable pins use six idle hours; runner calls refresh
 * their root mtime. Other entries use the scratch TTL.
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
    if (name === PLAN_SCRATCH_IGNORE_FILE) continue
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
    const pinPath = name.startsWith(PLAN_PIN_DIR_PREFIX)
      ? path
      : name.startsWith(RUN_SCRATCH_DIR_PREFIX)
        ? join(dir, PLAN_PIN_DIR_PREFIX + name.slice(RUN_SCRATCH_DIR_PREFIX.length))
        : null
    if (
      isDir &&
      pinPath &&
      existsSync(pinPath) &&
      !existsSync(join(pinPath, UNATTENDED_PLAN_PIN_MARKER)) &&
      !existsSync(join(pinPath, COMPLETED_PLAN_PIN_MARKER))
    )
      continue
    const entryTtlMs = isDir && name.startsWith(PLAN_PIN_DIR_PREFIX) ? STALE_PLAN_PIN_TTL_MS : ttlMs
    if ((!st.isFile() && !isDir) || now - st.mtimeMs < entryTtlMs) continue
    reapEntry(path, isDir, removed, failed, opts.onReport)
  }
  return { removed, failed }
}

if (isMainModule(import.meta.url)) {
  const dir = process.argv[2] || '.linear-plans'
  reapPlanScratch(dir, { onReport: (msg) => process.stderr.write(`${msg}\n`) })
}
