import { PLAN_CLAIM_MARKER } from '../linear-claim.mjs'
import { RANK_FACTS_SELECTION } from './linear-rank-facts.mjs'
import { BLOCKER_RELATIONS_SELECTION } from '../linear-deps-lib.mjs'

// The candidate read behind the OPTIONAL executable `selectPlanned` capability: the fields Step 2's
// eligibility walk reads plus the rank facts rankWork orders by (linear-rank-facts.mjs).
export const LIST_PLANNED_QUERY = `
  query ListPlanned($first: Int!, $filter: IssueFilter!) {
    issues(first: $first, filter: $filter) {
      nodes {
        identifier
        title
        estimate
        createdAt
        state { name type }
        ${RANK_FACTS_SELECTION}
        labels { nodes { name } }
        attachments { nodes { id title url createdAt } }
        ${BLOCKER_RELATIONS_SELECTION}
      }
    }
  }
`

export const LIST_PLAN_CANDIDATES_QUERY = LIST_PLANNED_QUERY.replace(
  'attachments {',
  `comments(first: 50, filter: { body: { contains: "${PLAN_CLAIM_MARKER}:" } }) { nodes { id body createdAt } }
        attachments {`,
)
