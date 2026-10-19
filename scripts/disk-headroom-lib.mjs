import fs from 'node:fs'

export const DISK_FLOOR_SIGNATURE = 'gate-run preflight: free disk below floor'
export const GiB = 1024 ** 3

export function measureHeadroom(paths, { statfs = fs.statfsSync, stat = fs.statSync } = {}) {
  const seen = new Set()
  const entries = []
  for (const path of paths) {
    let device
    try {
      device = stat(path).dev
    } catch (error) {
      if (error.code === 'ENOENT') continue
      throw error
    }
    if (seen.has(device)) continue
    const { bavail, bsize } = statfs(path)
    entries.push({ path, freeBytes: Number(bavail) * Number(bsize) })
    seen.add(device)
  }
  return entries
}

export function decideHeadroom({ entries, floorBytes }) {
  const lowest = entries.reduce((a, b) => (!a || b.freeBytes < a.freeBytes ? b : a), null)
  return {
    verdict:
      floorBytes === 0
        ? 'disabled'
        : lowest && lowest.freeBytes < floorBytes
          ? 'below-floor'
          : 'ok',
    lowest,
  }
}

export function parseFloorGiB(value, warn = (line) => process.stderr.write(`${line}\n`)) {
  if (value === undefined) return 20
  const number = Number(value)
  if (String(value).trim() !== '' && Number.isFinite(number) && number >= 0) return number
  warn('gate-run preflight: invalid BOSS_GATE_MIN_FREE_GIB; using 20 GiB')
  return 20
}
