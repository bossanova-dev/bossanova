// Persistent counters share the sweep state with quiet fingerprints, under distinct keys.
import { writeAtomicJson } from './verify-admission.mjs'

export const SWEEP_STATE_VERSION = 1
export const POST_FAILURE_PARK_AFTER = 3
const keyFor = (repo, pr) => `post-failure:${repo}#${pr}`

export function notePostFailure(entries, repo, pr, head, why, at) {
  const key = keyFor(repo, pr)
  const prior = entries[key]
  return (entries[key] = { head, count: prior?.head === head ? prior.count + 1 : 1, why, at })
}

export function clearPostFailure(entries, repo, pr) {
  delete entries[keyFor(repo, pr)]
}

export function readSweepEntries(fs, file) {
  try {
    const state = JSON.parse(fs.readFileSync(file, 'utf8'))
    if (state?.version !== SWEEP_STATE_VERSION) return {}
    const entries = state.entries
    return entries && typeof entries === 'object' && !Array.isArray(entries) ? entries : {}
  } catch {
    return {}
  }
}

export function writeSweepEntries(fs, file, entries) {
  return (
    typeof file === 'string' &&
    file.length > 0 &&
    writeAtomicJson(fs, file, { version: SWEEP_STATE_VERSION, entries })
  )
}
