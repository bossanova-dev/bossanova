import * as nativeFs from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { writeAtomicJson } from './verify-admission.mjs'
import { areaCapsPath, CAP_STATE_MAX_AGE_MS } from './verify-area-caps-state.mjs'
import { spawnDetached, timedRunner } from './verify-route.mjs'

export const REFRESH_INTERVAL_MS = 6 * 60 * 60 * 1000
export const REFRESH_WAIT_MS = 5 * 60 * 1000
const SYNC = fileURLToPath(new URL('./verify-area-caps-sync.mjs', import.meta.url))

/** Prepare an active sweep before discovery. Idle ticks never call this helper.
 * A failed or abandoned refresh releases admission to the normal stale-state fail-open policy.
 * @param {Parameters<typeof refreshAreaCaps>[0]} [deps]
 * @returns {{started:boolean,health?:string,wait?:string}}
 */
export function prepareAreaCaps(deps = {}) {
  const refresh = refreshAreaCaps(deps)
  if (refresh.health) return refresh
  try {
    const now = (deps.now ?? Date.now)()
    const run = deps.run ?? timedRunner({ cwd: deps.cwd, env: deps.env })
    const fs = deps.fs ?? nativeFs
    const path = resolve(deps.cwd ?? process.cwd(), areaCapsPath(run))
    const state = JSON.parse(String(fs.readFileSync(path, 'utf8')))
    if (now - Date.parse(state.refreshedAt) <= CAP_STATE_MAX_AGE_MS) return refresh
    const started = Date.parse(state.refreshStartedAt)
    if (!Number.isFinite(started)) return refresh
    let completed = NaN
    try {
      completed = Date.parse(
        JSON.parse(String(fs.readFileSync(`${path}.refresh-result`, 'utf8'))).completedAt,
      )
    } catch {}
    if (completed >= started) return refresh
    if (now - started >= REFRESH_WAIT_MS) return { ...refresh, health: 'area-caps-refresh-timeout' }
    return { ...refresh, wait: 'area caps refresh pending' }
  } catch {
    return { ...refresh, health: 'area-caps-refresh-failed' }
  }
}

/** @param {{run?:import('./verify-area-caps-state.mjs').StateReader, now?:()=>number,
 * cwd?:string, env?:NodeJS.ProcessEnv, fs?:typeof nativeFs,
 * nodeBin?:string, logDir?:string, spawnDetached?:typeof spawnDetached,
 * spawn?:Parameters<typeof spawnDetached>[3]['spawn']}} [deps]
 * @returns {{started:boolean,health?:string}}
 */
export function refreshAreaCaps(deps = {}) {
  const fs = deps.fs ?? nativeFs
  const now = deps.now ?? Date.now
  const cwd = deps.cwd ?? process.cwd()
  const env = deps.env ?? process.env
  let lock
  let held = false
  try {
    const run = deps.run ?? timedRunner({ cwd, env })
    const path = resolve(cwd, areaCapsPath(run))
    lock = `${path}.refresh-lock`
    fs.mkdirSync(dirname(path), { recursive: true })
    try {
      if (now() - fs.statSync(lock).mtimeMs >= REFRESH_INTERVAL_MS) fs.rmSync(lock, { force: true })
    } catch {}
    try {
      fs.writeFileSync(lock, '', { flag: 'wx' })
      held = true
    } catch {
      return { started: false }
    }
    let state = {}
    try {
      const saved = JSON.parse(String(fs.readFileSync(path, 'utf8')))
      if (saved && typeof saved === 'object' && !Array.isArray(saved)) state = saved
    } catch {}
    const times = [state.refreshedAt, state.refreshStartedAt]
      .map((value) => Date.parse(value))
      .filter(Number.isFinite)
    const startedAt = now()
    if (times.some((time) => startedAt - time < REFRESH_INTERVAL_MS)) return { started: false }
    if (
      !writeAtomicJson(fs, path, { ...state, refreshStartedAt: new Date(startedAt).toISOString() })
    )
      return { started: false, health: 'area-caps-refresh-failed' }
    const spawn = deps.spawnDetached ?? spawnDetached
    const result = spawn(
      deps.nodeBin ?? process.execPath,
      [SYNC, 'refresh'],
      { cwd, env, now, label: 'area-caps', ...(deps.logDir ? { logDir: deps.logDir } : {}) },
      deps.spawn ? { spawn: deps.spawn } : {},
    )
    return result.ok ? { started: true } : { started: false, health: 'area-caps-refresh-failed' }
  } catch {
    return { started: false, health: 'area-caps-refresh-failed' }
  } finally {
    if (held) {
      try {
        fs.rmSync(lock, { force: true })
      } catch {}
    }
  }
}
