// ci/green-confirmation.mjs — when a green ci-wait reading may end the wait.
//
// A single green reading can be a partial check set caught between two workflows attaching their
// jobs, so a green settles only once a later reading of the same head confirms it. On the daemon
// path the readings come from a cache whose check set carries no SHA, so there only uncached
// (`fresh`) greens one interval apart confirm. Pure; Node builtins only.

import { CHECK_STATES } from '../pr-check-state.mjs'

// confirmsGreen: a green reading only ends the wait when the reading before it, on the same head,
// was green too and carried no more checks. One green read can be a partial set caught between two
// workflows attaching their jobs; two in a row with a set that did not shrink is the settled shape.
//
// On the daemon path (`fresh: true`) a cached reading can wake the wait but never confirms: the last
// reading must be a fresh green, and an earlier fresh green of the same head, with only greens of
// that head between them, must lie at least `intervalMs` before it. That is the gh path's own shape:
// two uncached greens one interval apart.
export function confirmsGreen(record, { fresh = false, intervalMs = 0 } = {}) {
  if (fresh) return confirmsFreshGreen(record.readings, intervalMs)
  const n = record.readings.length
  if (n < 2) return false
  const [prev, last] = [record.readings[n - 2], record.readings[n - 1]]
  return (
    prev.state === CHECK_STATES.GREEN &&
    last.state === CHECK_STATES.GREEN &&
    prev.headSha === last.headSha &&
    Number.isFinite(prev.total) &&
    Number.isFinite(last.total) &&
    prev.total <= last.total
  )
}

const isGreen = (r) => r.state === CHECK_STATES.GREEN

function confirmsFreshGreen(readings, intervalMs) {
  const last = readings[readings.length - 1]
  if (!last || !isGreen(last) || last.fresh !== true || !Number.isFinite(last.total)) return false
  for (let i = readings.length - 2; i >= 0; i--) {
    const r = readings[i]
    if (r.headSha !== last.headSha || !isGreen(r)) return false
    // A set that shrank since an earlier reading is not settled, whichever reading anchors it.
    if (!Number.isFinite(r.total) || r.total > last.total) return false
    if (r.fresh === true && last.atMs - r.atMs >= intervalMs) return true
  }
  return false
}

// freshGreenAnchor is the earliest fresh green in the trailing run of greens on the last reading's
// head whose set has not shrunk since: the reading a confirming fresh green must follow by one
// interval. Null when there is none.
export function freshGreenAnchor(readings) {
  const last = readings[readings.length - 1]
  let anchor = null
  for (let i = readings.length - 1; i >= 0; i--) {
    const r = readings[i]
    if (!last || r.headSha !== last.headSha || !isGreen(r)) break
    if (!Number.isFinite(r.total) || r.total > last.total) break
    if (r.fresh === true) anchor = r
  }
  return anchor
}
