// verify-sweep-state.mjs — the unchanged-tick state: `{version: 1, entries: {<scopeKey>:
// {fingerprint, at, checkedAt, wakeAt?}}}` in the repo's git common dir. `checkedAt` is the ms-epoch
// last check and `wakeAt` the ms-epoch moment a claim the quiet tick saw expires. Anything
// unreadable, unparseable, of another version or missing reads as no previous fingerprint, and a
// write that fails is never an error: the next tick just runs in full.

import { mkdirSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs'
import { isAbsolute, join } from 'node:path'

import {
  readSweepEntries as readEntries,
  writeSweepEntries as writeEntries,
} from './verify-post-failures.mjs'

export { SWEEP_STATE_VERSION } from './verify-post-failures.mjs'
export { readEntries, writeEntries }

/** The default filesystem seam. */
export const FS = { readFileSync, writeFileSync, mkdirSync, renameSync, rmSync }
const text = (value) => typeof value === 'string' && value.trim() !== ''

/**
 * One sweep scope's key: its selection args and whether it acts on local sessions only.
 * @param {{selectionArgs?: string[], localSessionsOnly?: boolean, shared?: boolean, eventPrUrl?: string}} [scope]
 */
export function sweepScopeKey({
  selectionArgs = [],
  localSessionsOnly = false,
  shared = false,
  eventPrUrl,
} = {}) {
  return JSON.stringify({
    selection: selectionArgs,
    localSessionsOnly: localSessionsOnly === true,
    shared: shared === true,
    ...(eventPrUrl ? { eventPrUrl } : {}),
  })
}

/**
 * `<git common dir>/boss-verify/sweep-state.json`, or null when git cannot say.
 *
 * @param {(command: string, args: string[], options?: object) => {ok: boolean, output?: string}} run
 * @param {string} cwd
 * @param {number} timeoutMs
 */
export function sweepStatePath(run, cwd, timeoutMs) {
  const r = run('git', ['rev-parse', '--git-common-dir'], { timeoutMs })
  if (!r.ok || !text(r.output)) return null
  const common = String(r.output).trim()
  return join(isAbsolute(common) ? common : join(cwd, common), 'boss-verify', 'sweep-state.json')
}

/** `key`'s stored entry, or null. */
export function readQuietEntry(file, key, fs = FS) {
  if (!text(file)) return null
  return readEntries(fs, file)[key] ?? null
}

const wakeOf = (entry) => (Number.isFinite(entry?.wakeAt) ? entry.wakeAt : null)

/**
 * The quiet fingerprint stored for `key`, or null. Given `now`, a stored `wakeAt` at or before it
 * also reads as none: a claim the quiet tick saw has expired, so the tick runs in full.
 */
export function readQuietFingerprint(file, key, fs = FS, now = undefined) {
  const entry = readQuietEntry(file, key, fs)
  if (!text(entry?.fingerprint)) return null
  const wake = wakeOf(entry)
  if (Number.isFinite(now) && wake !== null && now >= wake) return null
  return entry.fingerprint
}

/** Stores `fingerprint` for `key`, with `wakeAt` when finite; false when it could not be written. */
export function storeQuietFingerprint(file, key, fingerprint, at, fs = FS, wakeAt = null) {
  if (!text(file)) return false
  return writeEntries(fs, file, {
    ...readEntries(fs, file),
    [key]: {
      fingerprint,
      at,
      checkedAt: Date.parse(at),
      ...(Number.isFinite(wakeAt) ? { wakeAt } : {}),
    },
  })
}

/** Refreshes an existing entry's `checkedAt` and `wakeAt` after an unchanged tick. */
export function touchQuietEntry(file, key, checkedAt, wakeAt, fs = FS) {
  if (!text(file)) return false
  const entries = readEntries(fs, file)
  if (!entries[key]) return false
  const { wakeAt: _old, ...kept } = entries[key]
  return writeEntries(fs, file, {
    ...entries,
    [key]: { ...kept, checkedAt, ...(Number.isFinite(wakeAt) ? { wakeAt } : {}) },
  })
}

/** Removes `key`'s entry; true when none is left. */
export function forgetQuietFingerprint(file, key, fs = FS) {
  if (!text(file)) return true
  const entries = readEntries(fs, file)
  if (!Object.hasOwn(entries, key)) return true
  delete entries[key]
  return writeEntries(fs, file, entries)
}

/**
 * Until when a webhook-healthy tick may stay idle on `entry`: the earlier of `checkedAt +
 * safetyNetMs` and a readable `wakeAt`, or null when the entry holds no quiet tick.
 */
export function safetyNetIdleUntil(entry, safetyNetMs) {
  const checkedAt = entry?.checkedAt ?? Date.parse(entry?.at)
  if (!text(entry?.fingerprint) || !Number.isFinite(checkedAt)) return null
  return Math.min(checkedAt + safetyNetMs, wakeOf(entry) ?? Infinity)
}
