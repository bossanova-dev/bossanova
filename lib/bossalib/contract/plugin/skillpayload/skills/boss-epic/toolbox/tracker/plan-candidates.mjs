import { hasLivePlanClaim } from '../linear-claim.mjs'
import { unmodelledReferenceTokens } from './reference-forms.mjs'

/**
 * Select planning work using the same liveness rule as claim arbitration.
 * @param adapter
 * @param query
 * @param {{now?: string, tolerated?: Set<string>}} [options]
 */
export async function selectPlanCandidates(
  adapter,
  query,
  { now = new Date().toISOString(), tolerated } = {},
) {
  const claimAware = adapter.planClaimSelection === true
  const rows = await adapter.selectPlanned(claimAware ? { ...query, planClaims: true } : query)
  const candidates = [],
    excluded = []
  for (const row of rows) {
    const { planClaimComments, ...candidate } = row
    if (claimAware && !Array.isArray(planClaimComments)) {
      throw new Error(`planClaimSelection: ${row.identifier} is missing a planClaimComments array`)
    }
    const liveComments = claimAware
      ? planClaimComments.filter((comment) => hasLivePlanClaim([comment], { now }))
      : []
    if (liveComments.length > 0) {
      const claim = liveComments.sort(
        (a, b) => Date.parse(a.createdAt) - Date.parse(b.createdAt),
      )[0]
      excluded.push({
        identifier: row.identifier,
        reason: 'live-plan-claim',
        claimCreatedAt: claim.createdAt,
      })
      continue
    }
    const tokens =
      tolerated === undefined
        ? []
        : unmodelledReferenceTokens(candidate.description, {
            rewrites: adapter.referenceRewrites ?? [],
            tolerated,
          })
    if (tokens.length > 0)
      excluded.push({ identifier: row.identifier, reason: 'unmodelled-references', tokens })
    else candidates.push(claimAware ? candidate : row)
  }
  return { candidates, excluded, claimAware }
}

/** A bounded explanation shared by worker diagnostics and the cron gate. */
export function describePlanExclusion(entry, now = Date.now()) {
  if (entry.reason === 'unmodelled-references') {
    const tokens = [...new Set(entry.tokens.map(({ token }) => token))].slice(0, 5)
    const transforms = [...new Set(entry.tokens.map(({ transform }) => transform))]
    return `${entry.identifier} (the tracker rewrites ${tokens.join(', ')} and the tolerated set lacks ${transforms.join(', ')}; declare it under descriptionNormalization.tolerated or edit the description)`
  }
  const age = Math.floor((now - Date.parse(entry.claimCreatedAt)) / 60000)
  return `${entry.identifier} (live peer plan claim, ${age}m old)`
}

/** CLI boundary: keep candidate stdout free of exclusion diagnostics. */
export async function listPlanCandidates(adapter, query, errWrite, options = {}) {
  const result = await selectPlanCandidates(adapter, query, options)
  for (const entry of result.excluded) {
    errWrite(`list-unplanned: excluded ${describePlanExclusion(entry)}\n`)
  }
  if (!result.claimAware)
    errWrite(
      'list-unplanned: tracker adapter does not declare planClaimSelection; peer-claimed candidates are not excluded\n',
    )
  return result.candidates
}
