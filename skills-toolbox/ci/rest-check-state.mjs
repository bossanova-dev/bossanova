// GitHub's REST check snapshot in the same shapes gh's GraphQL view and run list expose.
// The caller owns spawning, quota labels and deadlines.
const upper = (value) => (typeof value === 'string' ? value.toUpperCase() : '')

export function rollupFromRest({ checkRuns = [], statuses = [], workflowRuns = [] }) {
  const workflows = new Map(workflowRuns.map((run) => [run.check_suite_id, run]))
  return [
    ...checkRuns.map((check) => {
      const workflow = workflows.get(check.check_suite?.id)
      return {
        __typename: 'CheckRun',
        name: check.name,
        status: upper(check.status),
        conclusion: check.status?.toLowerCase() === 'completed' ? upper(check.conclusion) : '',
        startedAt: check.started_at,
        completedAt: check.completed_at,
        ...(workflow ? { workflowName: workflow.name, workflowId: workflow.workflow_id } : {}),
      }
    }),
    ...statuses.map((status) => ({
      __typename: 'StatusContext',
      context: status.context,
      state: upper(status.state),
      description: status.description,
      startedAt: status.created_at,
    })),
  ]
}

function collected(pages, key) {
  if (
    !Array.isArray(pages) ||
    pages.length === 0 ||
    pages.some((page) => !Array.isArray(page?.[key]) || !Number.isInteger(page.total_count))
  )
    throw new Error(`malformed ${key} pages`)
  const rows = pages.flatMap((page) => page[key])
  if (pages.some((page) => page.total_count > rows.length)) throw new Error(`truncated ${key}`)
  return rows
}

export function runsFromRest(pages) {
  return runRows(collected(pages, 'workflow_runs'))
}

function runRows(workflowRuns) {
  return workflowRuns.map((run) => ({
    name: run.name,
    workflowName: run.name,
    status: run.status,
    conclusion: run.conclusion,
    headSha: run.head_sha,
    event: run.event,
    workflowDatabaseId: run.workflow_id,
  }))
}

export function runsFromCheckSuites(json, headSha) {
  const suites = json?.data?.repository?.object?.checkSuites
  if (json?.errors?.length || !Array.isArray(suites?.nodes) || !Number.isInteger(suites.totalCount))
    throw new Error('malformed check suites')
  if (suites.totalCount > suites.nodes.length) throw new Error('truncated check suites')
  return suites.nodes
    .filter((suite) => suite.workflowRun)
    .map((suite) => ({
      name: suite.workflowRun.workflow?.name ?? '',
      workflowName: suite.workflowRun.workflow?.name ?? '',
      status: suite.status,
      conclusion: suite.conclusion,
      headSha,
      event: suite.workflowRun.event,
      workflowDatabaseId: suite.workflowRun.workflow?.databaseId,
    }))
}

export function viewFromRestPull(pull) {
  if (!pull?.head?.sha || !pull?.base?.ref || !['open', 'closed'].includes(pull.state))
    throw new Error('malformed pull response')
  return {
    number: pull.number,
    state: pull.merged_at ? 'MERGED' : upper(pull.state),
    isDraft: pull.draft === true,
    headRefOid: pull.head.sha,
    headRefName: pull.head.ref,
    baseRefName: pull.base.ref,
    mergeable:
      pull.mergeable === true ? 'MERGEABLE' : pull.mergeable === false ? 'CONFLICTING' : 'UNKNOWN',
    mergeStateStatus: upper(pull.mergeable_state) || 'UNKNOWN',
    reviewDecision: null,
  }
}

// Host-qualified repositories must pin every API read, including when GH_HOST names another host.
function apiArgs(repo, endpoint) {
  const parts = repo.split('/')
  const host = parts.length === 3 ? parts.shift() : null
  return ['api', `repos/${parts.join('/')}/${endpoint}`, ...(host ? ['--hostname', host] : [])]
}

export function restCheckSnapshot({ pr, repo, gh }) {
  const read = (endpoint, paginate = false) => {
    const result = gh([...apiArgs(repo, endpoint), ...(paginate ? ['--paginate', '--slurp'] : [])])
    if (!result.ok)
      throw Object.assign(new Error(result.error || 'REST read failed'), {
        code: result.refused ? 'RATE_LIMITED' : 'UNREADABLE',
      })
    return result.json
  }
  try {
    const view = viewFromRestPull(read(`pulls/${pr}`))
    const head = encodeURIComponent(view.headRefOid)
    const checkRuns = collected(
      read(`commits/${head}/check-runs?filter=latest&per_page=100`, true),
      'check_runs',
    )
    const statuses = collected(read(`commits/${head}/status?per_page=100`, true), 'statuses')
    const pages = read(`actions/runs?head_sha=${head}&per_page=100`, true)
    const workflowRuns = collected(pages, 'workflow_runs')
    return {
      ok: /** @type {const} */ (true),
      value: {
        view: { ...view, statusCheckRollup: rollupFromRest({ checkRuns, statuses, workflowRuns }) },
        runs: runRows(workflowRuns),
      },
    }
  } catch (error) {
    return {
      ok: /** @type {const} */ (false),
      error: { code: error.code || 'UNREADABLE', message: error.message },
    }
  }
}

export function graphqlRunsArgs({ owner, name, headSha }) {
  return [
    'api',
    'graphql',
    '-f',
    'query=query($owner:String!,$name:String!,$head:GitObjectID!){repository(owner:$owner,name:$name){object(oid:$head){... on Commit{checkSuites(first:100){totalCount nodes{status conclusion workflowRun{databaseId event workflow{name databaseId}}}}}}}}',
    '-f',
    `owner=${owner}`,
    '-f',
    `name=${name}`,
    '-f',
    `head=${headSha}`,
  ]
}
