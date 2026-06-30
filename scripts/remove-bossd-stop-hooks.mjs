#!/usr/bin/env node

import fs from 'node:fs'
import { fileURLToPath } from 'node:url'

export const DEFAULT_SETTINGS_PATH = '.claude/settings.local.json'

// True when the Stop-hook entry is owned by bossd (its finalizer or an
// agent-run hook), i.e. the entries a self-finalizing skill must remove so
// bossd does not run FinalizeSession again against an already-ready PR.
export function isBossdStopMatcher(matcher) {
  const m = typeof matcher === 'string' ? matcher : ''
  return m === 'bossd-finalize' || m.startsWith('bossd-agent-run-')
}

// Pure: given parsed settings, return [nextData, changed]. Returns the SAME
// object reference (and changed=false) when there is nothing to remove, so
// callers can skip the write.
export function pruneBossdStopHooks(data) {
  const stop = Array.isArray(data?.hooks?.Stop) ? data.hooks.Stop : null
  if (!stop) return [data, false]
  const kept = stop.filter((entry) => !isBossdStopMatcher(entry?.matcher))
  if (kept.length === stop.length) return [data, false]
  return [{ ...data, hooks: { ...data.hooks, Stop: kept } }, true]
}

// Side-effecting: read the settings file, prune bossd Stop entries, write back
// only when something changed. Returns true if the file was modified. Missing
// file is a no-op (false). Malformed JSON throws (fail loud), matching the
// inline snippet this replaced.
export function removeBossdStopHooks(settingsPath = DEFAULT_SETTINGS_PATH, deps = {}) {
  const fsImpl = deps.fs || fs
  if (!fsImpl.existsSync(settingsPath)) return false
  const data = JSON.parse(fsImpl.readFileSync(settingsPath, 'utf8'))
  const [next, changed] = pruneBossdStopHooks(data)
  if (!changed) return false
  fsImpl.writeFileSync(settingsPath, `${JSON.stringify(next, null, 2)}\n`)
  return true
}

const invokedDirectly =
  process.argv[1] &&
  fs.realpathSync(process.argv[1]) === fs.realpathSync(fileURLToPath(import.meta.url))

if (invokedDirectly) {
  try {
    removeBossdStopHooks(process.argv[2] || DEFAULT_SETTINGS_PATH)
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error))
    process.exitCode = 1
  }
}
