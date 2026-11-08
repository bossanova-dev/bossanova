import { hasLivePlanClaim } from '../linear-claim.mjs'

/** Select planning work using the same liveness rule as claim arbitration. */
export async function selectPlanCandidates(
  adapter,
  query,
  { now = new Date().toISOString() } = {},
) {
  if (adapter.planClaimSelection !== true) {
    return { candidates: await adapter.selectPlanned(query), excluded: [], claimAware: false }
  }
  const rows = await adapter.selectPlanned({ ...query, planClaims: true })
  const candidates = [],
    excluded = []
  for (const row of rows) {
    const { planClaimComments, ...candidate } = row
    if (!Array.isArray(planClaimComments)) {
      throw new Error(`planClaimSelection: ${row.identifier} is missing a planClaimComments array`)
    }
    const liveComments = planClaimComments.filter((comment) => hasLivePlanClaim([comment], { now }))
    if (liveComments.length > 0) {
      const claim = liveComments.sort(
        (a, b) => Date.parse(a.createdAt) - Date.parse(b.createdAt),
      )[0]
      excluded.push({ identifier: row.identifier, claimCreatedAt: claim.createdAt })
    } else candidates.push(candidate)
  }
  return { candidates, excluded, claimAware: true }
}

/** CLI boundary: keep candidate stdout free of exclusion diagnostics. */
export async function listPlanCandidates(adapter, query, errWrite) {
  const result = await selectPlanCandidates(adapter, query)
  for (const entry of result.excluded) {
    const age = Math.floor((Date.now() - Date.parse(entry.claimCreatedAt)) / 60000)
    errWrite(`list-unplanned: excluded ${entry.identifier} (live peer plan claim, ${age}m old)\n`)
  }
  if (!result.claimAware)
    errWrite(
      'list-unplanned: tracker adapter does not declare planClaimSelection; peer-claimed candidates are not excluded\n',
    )
  return result.candidates
}
