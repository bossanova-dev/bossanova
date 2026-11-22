export const TREND_WINDOW_MS = 10 * 60 * 1000

export const TRENDS = Object.freeze({
  CONVERGING: 'converging',
  FROZEN: 'frozen',
  UNKNOWN: 'unknown',
})

// trendOf reads the pending counts of the reads inside the last TREND_WINDOW_MS. A fall between
// any two consecutive reads in that window is `converging`; none is `frozen`; fewer than two
// countable reads is `unknown`, because one reading cannot show a direction. The window ends at the
// newest read of any kind: a run of unreadable reads ages an old fall out rather than preserving it.
export function trendOf(readings) {
  const all = (readings ?? []).filter((r) => Number.isFinite(r?.atMs))
  if (!Number.isFinite(all.at(-1)?.pending)) return TRENDS.UNKNOWN
  const countable = all.filter((r) => Number.isFinite(r?.pending))
  if (countable.length < 2) return TRENDS.UNKNOWN
  const lastAt = Math.max(...all.map((r) => r.atMs))
  const window = countable.filter((r) => r.atMs >= lastAt - TREND_WINDOW_MS)
  if (window.length < 2) return TRENDS.UNKNOWN
  for (let i = 1; i < window.length; i += 1) {
    if (window[i].pending < window[i - 1].pending) return TRENDS.CONVERGING
  }
  return TRENDS.FROZEN
}
