// skills-toolbox/linear-deps-lib.mjs
// Shared "is this ticket blocked?" rule + blocking-aware cron-gate query.
// node builtins only (the cron worktree is dependency-free). The blocker-clearing
// policy lives in the import-free tracker/blocker-clearance.mjs leaf.
//
// Strict scheduling clears only completed/canceled states or merged PRs. The build
// gate additionally admits green review PRs as possible selection work, without reading plans.
//
// A ticket C is "blocked by" X when Linear holds a relation {type:"blocks",
// issue:X, relatedIssue:C}. On C, that surfaces as an inverseRelation of type
// "blocks" whose `issue` is X. A blocker clears only when its PR is merged
// (state Done -> type "completed") or the work is dropped (Canceled -> "canceled").

import {
  assertNoLegacyGateKeys,
  buildIssueCountFilter,
  linearRequest,
  resolveGateSelectors,
} from './linear-gate-lib.mjs'

import {
  BLOCKER_CLEARED_STATE_TYPES,
  blockerVerdict as verdictOf,
  classifyBlocker,
  prUrlsOf,
} from './tracker/blocker-clearance.mjs'
export { BLOCKER_CLEARED_STATE_TYPES }
export const MAX_GATE_PR_LOOKUPS = 25

// Fetch candidate issues plus the relations + state needed to judge blocked-ness.
// `inverseRelations(first: 50)` bounds the relation page explicitly: Linear's
// default page cap (50) is shared across ALL inverse relation types (blocks,
// related, duplicate, ...), so without a stated size a real `blocks` relation
// could be paginated out and the ticket wrongly judged unblocked (the unsafe,
// fail-open direction). 50 is ample headroom for any realistic ticket.
export const BLOCKER_RELATIONS_SELECTION = `
        inverseRelations(first: 50) {
          nodes {
            type
            issue { id identifier state { name type } attachments(first: 20) { nodes { url } } }
          }
        }
`

export const BLOCKING_GATE_QUERY = `
  query UnblockedGate($filter: IssueFilter!, $first: Int!) {
    issues(first: $first, filter: $filter) {
      nodes {
        id
        identifier
        ${BLOCKER_RELATIONS_SELECTION}
      }
    }
  }
`

// Blockers of `issue`: the source issue of every inverse "blocks" relation.
export function extractBlockers(issue) {
  const nodes = issue?.inverseRelations?.nodes
  if (!Array.isArray(nodes)) return []
  return nodes.filter((r) => r?.type === 'blocks' && r?.issue).map((r) => r.issue)
}

// Blocker issues whose states have not cleared.
export function openBlockers(issue, { clearedStateTypes = BLOCKER_CLEARED_STATE_TYPES } = {}) {
  return extractBlockers(issue).filter((b) => !clearedStateTypes.has(b?.state?.type))
}

export function isUnblocked(issue, opts) {
  return verdictOf(extractBlockers(issue), { ...opts, mode: 'strict' }) === 'unblocked'
}

// Missing relation data cannot establish that a list candidate is unblocked.
export function blockerVerdict(issue, opts) {
  if (!Array.isArray(issue?.inverseRelations?.nodes)) return 'unknown'
  return verdictOf(extractBlockers(issue), opts)
}

export function countUnblocked(issues, opts) {
  if (!Array.isArray(issues)) return 0
  return issues.reduce((n, issue) => (isUnblocked(issue, opts) ? n + 1 : n), 0)
}

// Gate: true iff at least one matching (state+label) issue is unblocked.
// Fetches up to maxCandidates candidates with their blockers so an all-blocked
// head doesn't mask deeper unblocked work. The default mirrors the skill's own
// selection window (`list_issues ... limit=250` in bs-implement Step 2): scanning
// the same universe the skill would pick from is what lets the gate keep its
// "never a false-negative relative to the skill" contract for any plan-ready
// backlog of ≤250 matching candidates — a smaller cap could skip a run while an
// unblocked candidate sat just past it. (Beyond 250 the gate and the skill could
// in principle window different subsets; that backlog size is not realistic here.)
//
// The three identity selectors are optional and inert: each resolves to `undefined` when unset,
// contributing no clause, so this gate emits the byte-identical filter it emitted before they
// existed for every caller that passes only `state` and `label` — which is every caller in this
// tree today. They resolve through the shared `resolveGateSelectors` preamble rather than a local
// `me` check so the fail-closed identity contract, and the serial resolution order the request
// counts are pinned against, are written once for both gate runners.
//
// Round trips, as the code actually spends them: an unset selector costs zero, a selector holding
// a concrete id costs zero, and a selector set to the literal `me` costs one viewer lookup of its
// own — the resolver is not memoised, so two `me` selectors cost two lookups. That callers in this
// tree set at most one selector is an assumption about the callers, not a property of this gate.
//
// `filter` is the pre-rendered alternative to the legacy keys (the selection path); combining the
// two throws, exactly as `runLinearGate` does.
export async function runUnblockedGate({
  apiKey,
  state,
  label,
  assignee,
  creator,
  assigneeOrCreator,
  filter: rendered,
  maxCandidates = 250,
  fetchImpl = fetch,
  endpoint,
  prLookup,
  inReviewState,
}) {
  let filter = rendered
  if (filter !== undefined) {
    assertNoLegacyGateKeys('runUnblockedGate', {
      state,
      label,
      assignee,
      creator,
      assigneeOrCreator,
    })
  } else {
    const { assigneeId, creatorId, assigneeOrCreatorId } = await resolveGateSelectors({
      apiKey,
      assignee,
      creator,
      assigneeOrCreator,
      fetchImpl,
      endpoint,
    })
    filter = buildIssueCountFilter({
      state,
      label,
      assigneeId,
      creatorId,
      assigneeOrCreatorId,
    })
  }
  const data = await linearRequest({
    apiKey,
    query: BLOCKING_GATE_QUERY,
    variables: { filter, first: maxCandidates },
    fetchImpl,
    endpoint,
  })
  const nodes = data?.issues?.nodes
  if (!Array.isArray(nodes)) return false
  if (nodes.some((issue) => isUnblocked(issue))) return true
  if (!prLookup) return false
  const prFacts = new Map()
  for (const issue of nodes) {
    for (const blocker of extractBlockers(issue)) {
      if (classifyBlocker(blocker).clearance !== 'needs-pr-facts') continue
      for (const url of prUrlsOf(blocker)) {
        if (prFacts.has(url) || prFacts.size >= MAX_GATE_PR_LOOKUPS) continue
        try {
          prFacts.set(url, await prLookup(url))
        } catch {
          prFacts.set(url, { error: 'lookup failed' })
        }
      }
    }
    if (verdictOf(extractBlockers(issue), { prFacts, mode: 'gate', inReviewState }) === 'unblocked')
      return true
  }
  return false
}

// Resolve identifiers individually: issue(id:) accepts both UUIDs and human ids.
/** @param {string[]} ids
 * @param {{apiKey?: string, fetchImpl?: typeof fetch, endpoint?: string, mode?: string}} [options] */
export async function readBlockers(
  ids,
  { apiKey, fetchImpl = fetch, endpoint, mode = 'strict' } = {},
) {
  const results = []
  for (const id of ids) {
    const data = await linearRequest({
      apiKey,
      fetchImpl,
      endpoint,
      query: `query Blockers($id: String!) { issue(id: $id) { id identifier ${mode === 'selection' ? 'description' : ''} ${BLOCKER_RELATIONS_SELECTION} } }`,
      variables: { id },
    })
    if (!Array.isArray(data?.issue?.inverseRelations?.nodes))
      throw new Error(`cannot read blockers for ${id}`)
    results.push({
      id,
      ...(mode === 'selection' ? { description: data.issue.description } : {}),
      blockers: extractBlockers(data.issue).map((blocker) => ({
        ...blocker,
        id: blocker.identifier ?? blocker.id,
      })),
    })
  }
  return results
}
