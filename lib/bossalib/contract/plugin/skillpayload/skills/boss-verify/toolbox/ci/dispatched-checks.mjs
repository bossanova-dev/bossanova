// ci/dispatched-checks.mjs — the head commit's check runs from `workflow_dispatch` suites.
//
// A PR's `statusCheckRollup` leaves out check runs from `workflow_dispatch` suites. A repository
// whose draft `pull_request` runs skip the heavy jobs, and which dispatches those workflows on the
// head instead (ci/ci-request.mjs), shows its required checks in the rollup only as skipped copies.
// One GraphQL read of the head commit's check suites names each suite's event and commit, so a
// dispatched run counts for exactly that head and for no other.
//
// Pure; the caller owns spawning, quota labels and deadlines.

export const DISPATCH_EVENT = 'workflow_dispatch'

export function graphqlCheckSuitesArgs({ owner, name, headSha }) {
  return [
    'api',
    'graphql',
    '-f',
    'query=query($owner:String!,$name:String!,$head:GitObjectID!){repository(owner:$owner,name:$name){object(oid:$head){... on Commit{checkSuites(first:100){totalCount nodes{commit{oid} workflowRun{event workflow{name databaseId}} checkRuns(first:100){totalCount nodes{name status conclusion startedAt completedAt detailsUrl}}}}}}}}',
    '-f',
    `owner=${owner}`,
    '-f',
    `name=${name}`,
    '-f',
    `head=${headSha}`,
  ]
}

// checkRunsFromSuites flattens the read into rollup-shaped CheckRun nodes, each carrying its suite's
// `event` and commit (`headSha`). A malformed or truncated read throws: a partial list could hide
// the one failing run that matters.
export function checkRunsFromSuites(json) {
  const suites = json?.data?.repository?.object?.checkSuites
  if (json?.errors?.length || !Array.isArray(suites?.nodes) || !Number.isInteger(suites.totalCount))
    throw new Error('malformed check suites')
  if (suites.totalCount > suites.nodes.length) throw new Error('truncated check suites')
  return suites.nodes
    .filter((suite) => suite?.workflowRun)
    .flatMap((suite) => {
      const runs = suite.checkRuns
      if (!Array.isArray(runs?.nodes) || !Number.isInteger(runs.totalCount))
        throw new Error('malformed check runs')
      if (runs.totalCount > runs.nodes.length) throw new Error('truncated check runs')
      const workflow = suite.workflowRun.workflow ?? {}
      return runs.nodes.map((run) => ({
        __typename: 'CheckRun',
        name: run?.name ?? '',
        status: run?.status ?? '',
        conclusion: run?.conclusion ?? '',
        startedAt: run?.startedAt ?? '',
        completedAt: run?.completedAt ?? '',
        detailsUrl: run?.detailsUrl ?? '',
        workflowName: workflow.name ?? '',
        ...(workflow.databaseId == null ? {} : { workflowId: workflow.databaseId }),
        event: suite.workflowRun.event ?? '',
        headSha: suite.commit?.oid ?? '',
      }))
    })
}

// dispatchedCheckRuns keeps the rows from a `workflow_dispatch` suite on exactly `headSha`.
export function dispatchedCheckRuns(rows, headSha) {
  if (!Array.isArray(rows) || !headSha) return []
  return rows.filter((row) => row?.event === DISPATCH_EVENT && row.headSha === headSha)
}
