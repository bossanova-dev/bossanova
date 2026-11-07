// skills-toolbox/tracker/github-rank-facts.mjs
// The facts rankWork orders GitHub candidates by, read in one GraphQL request per batch: each
// candidate's milestone due date, its sub-issue parent chain, and the open issues it blocks
// (issue dependencies), each with its `boss:priority-N` label. They are mapped into the node shape
// rank-work.mjs documents. GitHub has no project priority, schedule or manual order, so those
// facts stay absent and rank neutrally.

export const RANK_FACTS_BATCH = 50

const ISSUE_FACTS =
  'number url state repository { nameWithOwner } labels(first: 50) { nodes { name } } milestone { dueOn }'

function query({ owner, repo }, numbers) {
  const issues = numbers.map(
    (number) => `i${number}: issue(number: ${number}) {
      milestone { dueOn }
      parent { ${ISSUE_FACTS} parent { ${ISSUE_FACTS} parent { ${ISSUE_FACTS} } } }
      blocking(first: 50) { nodes { ${ISSUE_FACTS} parent { ${ISSUE_FACTS} } } }
    }`,
  )
  return `query { repository(owner: ${JSON.stringify(owner)}, name: ${JSON.stringify(repo)}) {
    ${issues.join('\n    ')}
  } }`
}

/** The `boss:priority-N` label's N (1 Urgent .. 4 Low), or 0 for none. */
export function labelPriority(labels) {
  for (const label of labels ?? []) {
    const match = /^boss:priority-([1-4])$/.exec(typeof label === 'string' ? label : label?.name)
    if (match) return Number(match[1])
  }
  return 0
}

function referenceOf(issue, local) {
  if (!issue) return null
  const own = String(issue.repository?.nameWithOwner ?? '').toLowerCase() === local.toLowerCase()
  return {
    identifier: own ? `GH-${issue.number}` : issue.url,
    priority: labelPriority(issue.labels?.nodes),
    dueDate: issue.milestone?.dueOn ?? null,
    state: { type: issue.state === 'CLOSED' ? 'completed' : 'unstarted' },
    ...('parent' in issue ? { parent: referenceOf(issue.parent, local) } : {}),
  }
}

/**
 * `rows` (normalized candidates, `id` like `GH-12`) with `dueDate`, `parent` and `relations` added
 * from GitHub. A candidate GitHub returns nothing for keeps neutral facts; a failed request throws,
 * because an order built on facts that could not be read is not the tracker's order.
 */
export function withGithubRankFacts(rows, { api, identity }) {
  const local = `${identity.owner}/${identity.repo}`
  const facts = new Map()
  const numbers = rows.map((row) => String(row.id).replace(/^GH-/, ''))
  for (let i = 0; i < numbers.length; i += RANK_FACTS_BATCH) {
    const batch = numbers.slice(i, i + RANK_FACTS_BATCH)
    const result = api('graphql', ['-f', `query=${query(identity, batch)}`])
    const repository = result?.data?.repository
    if (!repository || typeof repository !== 'object')
      throw new Error('github tracker: rank facts unreadable (no repository in the GraphQL answer)')
    for (const number of batch) facts.set(number, repository[`i${number}`] ?? null)
  }
  return rows.map((row, i) => {
    const issue = facts.get(numbers[i])
    if (!issue) return row
    return {
      ...row,
      dueDate: issue.milestone?.dueOn ?? null,
      parent: referenceOf(issue.parent, local),
      relations: {
        nodes: (issue.blocking?.nodes ?? []).map((blocked) => ({
          type: 'blocks',
          relatedIssue: referenceOf(blocked, local),
        })),
      },
    }
  })
}
