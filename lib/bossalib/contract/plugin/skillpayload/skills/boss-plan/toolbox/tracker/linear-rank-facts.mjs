// skills-toolbox/tracker/linear-rank-facts.mjs
// The Linear issue fields rank-work.mjs orders candidates by: priority, due date and manual order up
// the epic chain, the project's priority, dates, status, manual order and the projects it must
// finish before, and the open tickets this one blocks with their own facts.

const ISSUE_FACTS = 'identifier priority dueDate'
const PARENT_CHAIN = `parent { ${ISSUE_FACTS} parent { ${ISSUE_FACTS} parent { ${ISSUE_FACTS} parent { ${ISSUE_FACTS} } } } }`

export const RANK_FACTS_SELECTION = `
        priority
        prioritySortOrder
        dueDate
        ${PARENT_CHAIN}
        project {
          id name priority prioritySortOrder startDate targetDate status { type }
          relations(first: 20) {
            nodes { type anchorType relatedAnchorType relatedProject { id name priority startDate targetDate status { type } } }
          }
        }
        relations(first: 50) {
          nodes {
            type
            relatedIssue {
              ${ISSUE_FACTS} state { type }
              parent { ${ISSUE_FACTS} }
              project { id name priority targetDate status { type } }
            }
          }
        }
`
