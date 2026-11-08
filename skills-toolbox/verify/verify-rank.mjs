// Review ranks can differ from build ranks at hand-over: one refresh is expected, then steady.
// This leaf shares rank comparison with selection, without depending on a tracker or transport.
import { compareRank } from '../rank-work.mjs'

const text = (value) => typeof value === 'string' && value.trim().length > 0
const keys = ['tier', 'tierFrom', 'deadline', 'deadlineFrom', 'scheduled', 'leverage']
const valid = (r) =>
  r &&
  Number.isInteger(r.tier) &&
  r.tier >= 0 &&
  r.tier <= 4 &&
  text(r.tierFrom) &&
  (r.deadline === null ||
    (typeof r.deadline === 'string' && /^\d{4}-\d{2}-\d{2}$/.test(r.deadline))) &&
  (r.deadlineFrom === null || typeof r.deadlineFrom === 'string') &&
  typeof r.scheduled === 'boolean' &&
  Number.isInteger(r.leverage) &&
  r.leverage >= 0
export const rankOf = (ticket) =>
  valid(ticket?.rank) ? Object.fromEntries(keys.map((k) => [k, ticket.rank[k]])) : null

export function rankOrder(entries) {
  return [...entries].sort((a, b) => {
    const ar = rankOf(a.ticket),
      br = rankOf(b.ticket)
    return (ar && br ? compareRank(ar, br) : ar ? -1 : br ? 1 : 0) || a.pr.number - b.pr.number
  })
}

export function rankFacts(linked, included) {
  return linked
    .filter((e) => rankOf(e.ticket) && included(e.pr))
    .map(({ ticket, pr }) => ({
      ticket: String(ticket.identifier).toUpperCase(),
      pr: pr.number,
      url: pr.url,
      rank: rankOf(ticket),
    }))
    .sort((a, b) => a.pr - b.pr)
}

/** Normalize the trackerRank JSON served by the session CLI; rankedAt is deliberately ignored. */
export function storedRankOf(row) {
  const r = row?.trackerRank
  if (!r || typeof r.deadlineSource !== 'string') return null
  return rankOf({
    rank: {
      tier: r.tier,
      tierFrom: r.tierSource,
      deadline: r.deadlineOn ?? null,
      deadlineFrom: r.deadlineSource || null,
      scheduled: r.isScheduled,
      leverage: r.unblockedTicketCount,
    },
  })
}

export function rankRefreshes(ranks, sessions) {
  const out = []
  for (const fact of ranks) {
    const fresh = rankOf({ rank: fact.rank })
    if (!fresh) continue
    for (const row of sessions) {
      if (
        !text(row?.id) ||
        text(row.archived_at) ||
        !text(row.tracker_id) ||
        row.tracker_id.toUpperCase() !== fact.ticket.toUpperCase()
      )
        continue
      const stored = storedRankOf(row)
      if (stored && keys.every((k) => stored[k] === fresh[k])) continue
      out.push({
        session: row.id,
        ticket: fact.ticket,
        pr: fact.pr,
        argv: ['session', 'set-rank', row.id, fact.ticket, '--rank-json', JSON.stringify(fresh)],
      })
    }
  }
  return out
}

export function rankRefreshNeedsRetry(result) {
  return (
    result.unreadable ||
    result.rankRefreshes.some((r) => ['failed', 'deferred'].includes(r.outcome))
  )
}

/** Best-effort sweep writes; failures leave the quiet fingerprint invalid so the next tick retries. */
export function refreshRanks(
  ranks,
  { eventPrUrl, dispatchable, run, boss, timeout, remaining, startFloor, dryRun },
) {
  const scoped = (Array.isArray(ranks) ? ranks : []).filter(
    (r) => !eventPrUrl || r.url === eventPrUrl,
  )
  if (!scoped.length) return { rankRefreshes: [] }
  const state = dispatchable()
  if (!state.ok) return { rankRefreshes: [], refreshed: 0, unreadable: state.why }
  const records = rankRefreshes(scoped, state.sessions).map((call) => {
    const { pr, ticket, session, argv } = call
    const record = { pr, ticket, session }
    if (remaining() < startFloor) return { ...record, outcome: 'deferred' }
    if (dryRun) return { ...record, outcome: 'dry-run', argv }
    try {
      const result = run(boss, argv, { timeoutMs: timeout() })
      return result.ok
        ? { ...record, outcome: 'refreshed' }
        : { ...record, outcome: 'failed', error: result.error ?? 'unknown' }
    } catch (error) {
      return { ...record, outcome: 'failed', error: String(error) }
    }
  })
  return { rankRefreshes: records }
}

// The live rows bound to a PR URL, newest first.
export function liveSessionsFor(sessions, url) {
  return (Array.isArray(sessions) ? sessions : [])
    .filter((row) => text(url) && row?.pr_url === url && !text(row?.archived_at) && text(row?.id))
    .sort((a, b) => String(b.updated_at ?? '').localeCompare(String(a.updated_at ?? '')))
}
