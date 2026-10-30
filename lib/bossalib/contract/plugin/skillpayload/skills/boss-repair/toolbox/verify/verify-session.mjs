import { readSessionRow } from './verify-admission.mjs'

const text = (value) => typeof value === 'string' && value.trim() !== ''

export const REPAIR_RECEIPT_WAIT_MS = 60 * 60 * 1000

export function headFirstCiStartMs(statusCheckRollup) {
  const starts = (Array.isArray(statusCheckRollup) ? statusCheckRollup : [])
    .filter((entry) => !String(entry?.name ?? entry?.context ?? '').startsWith('boss/'))
    .map((entry) => (typeof entry?.startedAt === 'string' ? Date.parse(entry.startedAt) : NaN))
    .filter(Number.isFinite)
  return starts.length ? Math.min(...starts) : null
}

export function readRepairInFlight(read, prUrl, rollup, now) {
  const session = readSessionRow(read, prUrl)
  const idle = (why) => ({ inFlight: false, why })
  if (!session.readable) return idle('session-unreadable')
  if (!session.row || !text(session.row.id)) return idle('no-session')
  const show = read('boss', ['show', session.row.id, '--json'], { json: true })
  if (!show.ok) return idle('session-unreadable')
  const active = show.payload?.session?.repair_active
  if (typeof active !== 'boolean') return idle('repair-active-absent')
  if (!active) return idle('repair-idle')
  const firstStart = headFirstCiStartMs(rollup)
  if (firstStart === null || !Number.isFinite(now)) return idle('head-age-unknown')
  if (now - firstStart > REPAIR_RECEIPT_WAIT_MS) return idle('wait-expired')
  return { inFlight: true, why: 'repair-active' }
}
