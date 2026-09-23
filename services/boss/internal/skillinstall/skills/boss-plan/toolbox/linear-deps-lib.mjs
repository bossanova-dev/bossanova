// skills-toolbox/linear-deps-lib.mjs
// Shared "is this ticket blocked?" rule + blocking-aware cron-gate query.
// node builtins only (the cron worktree is dependency-free). The blocker-clearing
// rule lives HERE once so the gate and the skills agree on a single definition.
//
// A ticket C is "blocked by" X when Linear holds a relation {type:"blocks",
// issue:X, relatedIssue:C}. On C, that surfaces as an inverseRelation of type
// "blocks" whose `issue` is X. A blocker clears only when its PR is merged
// (state Done -> type "completed") or the work is dropped (Canceled -> "canceled").

import { linearRequest, buildIssueCountFilter, resolveGateSelectors } from './linear-gate-lib.mjs'

// Linear state.type values that mean a blocker no longer blocks.
export const BLOCKER_CLEARED_STATE_TYPES = new Set(['completed', 'canceled'])

// Fetch candidate issues plus the relations + state needed to judge blocked-ness.
// `inverseRelations(first: 50)` bounds the relation page explicitly: Linear's
// default page cap (50) is shared across ALL inverse relation types (blocks,
// related, duplicate, ...), so without a stated size a real `blocks` relation
// could be paginated out and the ticket wrongly judged unblocked (the unsafe,
// fail-open direction). 50 is ample headroom for any realistic ticket.
export const BLOCKING_GATE_QUERY = `
  query UnblockedGate($filter: IssueFilter!, $first: Int!) {
    issues(first: $first, filter: $filter) {
      nodes {
        id
        identifier
        inverseRelations(first: 50) {
          nodes {
            type
            issue { id identifier state { name type } }
          }
        }
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

// True iff `issue` has no blocker whose state is still uncleared.
export function isUnblocked(issue, { clearedStateTypes = BLOCKER_CLEARED_STATE_TYPES } = {}) {
  return extractBlockers(issue).every((b) => clearedStateTypes.has(b?.state?.type))
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
export async function runUnblockedGate({
  apiKey,
  state,
  label,
  assignee,
  creator,
  assigneeOrCreator,
  maxCandidates = 250,
  fetchImpl = fetch,
  endpoint,
}) {
  const { assigneeId, creatorId, assigneeOrCreatorId } = await resolveGateSelectors({
    apiKey,
    assignee,
    creator,
    assigneeOrCreator,
    fetchImpl,
    endpoint,
  })
  const filter = buildIssueCountFilter({
    state,
    label,
    assigneeId,
    creatorId,
    assigneeOrCreatorId,
  })
  const data = await linearRequest({
    apiKey,
    query: BLOCKING_GATE_QUERY,
    variables: { filter, first: maxCandidates },
    fetchImpl,
    endpoint,
  })
  const nodes = data?.issues?.nodes
  return Array.isArray(nodes) && nodes.some((issue) => isUnblocked(issue))
}
