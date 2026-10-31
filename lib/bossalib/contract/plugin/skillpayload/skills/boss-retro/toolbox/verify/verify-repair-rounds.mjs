// PR-scoped repair history survives pushes and restarts only at an operator's decision.
export const REPAIR_ROUND_CAP = 2
export const REPAIR_EXHAUSTED_AFTER = 5

const order = (a, b) => a.at - b.at || a.id - b.id

/** Trusted operatorMarkers only; repeated publications update a round without adding one. */
export function repairRounds(markers) {
  const sorted = [...markers].sort(order)
  const reset = sorted.findLast((m) => m.kind === 'approve' || m.kind === 'send-back')
  const counted = sorted.filter(
    (m) => m.kind === 'repair-outcomes' && (!reset || order(m, reset) > 0),
  )
  const rounds = new Map()
  const latest = new Map()
  const notFixableSends = new Map()
  for (const marker of counted) {
    if (!rounds.has(marker.blockersHead)) rounds.set(marker.blockersHead, rounds.size + 1)
    const report = sorted.findLast(
      (m) =>
        m.kind === 'blockers' &&
        m.head === marker.blockersHead &&
        m.at <= marker.at &&
        (!reset || order(m, reset) > 0),
    )
    for (const outcome of marker.outcomes) {
      const blocker = report?.blockerFile.blockers.find((b) => b.id === outcome.id)
      const details = {
        ...outcome,
        summary: blocker?.summary ?? outcome.id,
        severity: blocker?.severity ?? null,
        file: blocker?.file ?? null,
        line: blocker?.line ?? null,
        round: rounds.get(marker.blockersHead),
      }
      latest.set(outcome.id, details)
      if (outcome.outcome === 'not-fixable' && report) {
        const sends = notFixableSends.get(outcome.id) ?? new Map()
        sends.delete(report.id)
        sends.set(report.id, details)
        notFixableSends.set(outcome.id, sends)
      }
    }
  }
  return {
    rounds: rounds.size,
    repeatNotFixable: [...notFixableSends.values()]
      .filter((sends) => sends.size >= 2)
      .map((sends) => {
        const { outcome, ...details } = [...sends.values()].at(-1)
        return { ...details, count: sends.size }
      }),
    notFixable: [...latest.values()]
      .filter((o) => o.outcome === 'not-fixable')
      .map(({ id, summary, severity, file, line, reason, round }) => ({
        id,
        summary,
        severity,
        file,
        line,
        reason,
        round,
      })),
  }
}

export function resolveEffectiveVerdict({
  verdict,
  humanCodes,
  reason,
  repairAttempts,
  repairRounds = null,
  repeatNotFixable = null,
  blockers = [],
  exhaustedAfter = REPAIR_EXHAUSTED_AFTER,
}) {
  if (
    verdict === 'blocked' &&
    ((repairRounds !== null && repairRounds >= REPAIR_ROUND_CAP) ||
      blockers.some((b) => repeatNotFixable?.includes(b.id)))
  )
    return { effective: 'human', reason: 'repair-exhausted' }
  if (
    ['defect', 'blocked'].includes(verdict) &&
    repairAttempts !== null &&
    repairAttempts >= exhaustedAfter
  )
    return { effective: 'human', reason: 'repair-exhausted' }
  return { effective: verdict, reason: humanCodes ? humanCodes.join(',') : reason }
}
