import { readFileSync } from 'node:fs'
import { join } from 'node:path'

export const CAP_STATE_MAX_AGE_MS = 24 * 60 * 60 * 1000
/** @typedef {import('./verify-area-caps.mjs').AreaCap} AreaCap */
/** @typedef {(command:string,args:string[])=>{ok:boolean,output?:string}} StateReader */

/** @param {StateReader} read */
export function areaCapsPath(read) {
  const common = read('git', ['rev-parse', '--git-common-dir'])
  if (!common.ok || !common.output?.trim()) throw Error('git common directory unavailable')
  return join(common.output.trim(), 'boss-verify', 'area-caps.json')
}

/** @param {unknown} value @returns {value is AreaCap} */
function validCap(value) {
  const cap = /** @type {AreaCap} */ (value)
  return (
    !!cap &&
    typeof cap.key === 'string' &&
    typeof cap.label === 'string' &&
    Number.isFinite(Date.parse(cap.cappedAt)) &&
    Array.isArray(cap.escapes) &&
    cap.escapes.length > 0 &&
    cap.escapes.every(
      (e) =>
        Number.isInteger(e.original) &&
        e.original > 0 &&
        Number.isInteger(e.fix) &&
        e.fix > 0 &&
        Number.isFinite(Date.parse(e.fixMergedAt)),
    )
  )
}

/** Read only the clone's state; feedback availability does not judge the head.
 * @param {{read:StateReader,repo:string,now?:number,fs?:Pick<typeof import('node:fs'),'readFileSync'>}} options
 * @returns {{caps:AreaCap[],health:string[]}}
 */
export function readAreaCaps({ read, repo, now = Date.now(), fs = { readFileSync } }) {
  try {
    const state = JSON.parse(String(fs.readFileSync(areaCapsPath(read), 'utf8')))
    if (
      state?.v !== 1 ||
      typeof state.repo !== 'string' ||
      state.repo.toLowerCase() !== repo.toLowerCase() ||
      !Number.isFinite(Date.parse(state.refreshedAt)) ||
      !Array.isArray(state.caps) ||
      !state.caps.every(validCap)
    )
      throw Error('invalid cap state')
    if (now - Date.parse(state.refreshedAt) > CAP_STATE_MAX_AGE_MS)
      return { caps: [], health: ['area-caps-stale'] }
    return { caps: state.caps, health: [] }
  } catch {
    return { caps: [], health: ['area-caps-unknown'] }
  }
}

/** Effective caps alone affect admission; a successful refresh with the same caps changes nothing.
 * @param {Parameters<typeof readAreaCaps>[0]} options
 */
export function readAreaCapKeys(options) {
  return [...new Set(readAreaCaps(options).caps.map((cap) => cap.key))].sort()
}
