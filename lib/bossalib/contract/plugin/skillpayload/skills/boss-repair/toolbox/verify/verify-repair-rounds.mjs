// PR-scoped repair history survives pushes and restarts only at an operator's decision.
export const REPAIR_ROUND_CAP = 2

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
  for (const marker of counted) {
    if (!rounds.has(marker.blockersHead)) rounds.set(marker.blockersHead, rounds.size + 1)
    const report = sorted.findLast((m) => m.kind === 'blockers' && m.head === marker.blockersHead)
    for (const outcome of marker.outcomes) {
      const blocker = report?.blockerFile.blockers.find((b) => b.id === outcome.id)
      latest.set(outcome.id, {
        ...outcome,
        summary: blocker?.summary ?? outcome.id,
        severity: blocker?.severity ?? null,
        file: blocker?.file ?? null,
        line: blocker?.line ?? null,
        round: rounds.get(marker.blockersHead),
      })
    }
  }
  return {
    rounds: rounds.size,
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
