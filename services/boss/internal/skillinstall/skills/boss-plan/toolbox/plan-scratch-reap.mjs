// plan-scratch-reap.mjs
//
// Age-based cleanup for boss-plan's gitignored plan scratch. It deliberately
// avoids tracker state: when in doubt, keep the file and let a later run try.

import { existsSync, readdirSync, rmSync, statSync } from 'node:fs'
import { join } from 'node:path'
import { isMainModule } from './main-module.mjs'

export const STALE_PLAN_SCRATCH_TTL_MS = 24 * 60 * 60 * 1000

function report(onReport, msg) {
  if (onReport) onReport(msg)
}

/**
 * Reap stale regular files directly under the plan scratch directory.
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
      st = statSync(path)
    } catch {
      continue
    }
    if (!st.isFile() || now - st.mtimeMs < ttlMs) continue
    try {
      rmSync(path, { force: true })
      if (existsSync(path)) {
        const msg = `warning: stale plan scratch survived removal: ${path}`
        failed.push(msg)
        report(opts.onReport, msg)
        continue
      }
      removed.push(path)
      report(opts.onReport, `removed stale plan scratch: ${path}`)
    } catch (err) {
      const msg = `warning: failed to reap stale plan scratch ${path}: ${err.message}`
      failed.push(msg)
      report(opts.onReport, msg)
    }
  }
  return { removed, failed }
}

if (isMainModule(import.meta.url)) {
  const dir = process.argv[2] || '.linear-plans'
  reapPlanScratch(dir, { onReport: (msg) => process.stderr.write(`${msg}\n`) })
}
