/**
 * @typedef {{name: string, conclusion: string | null, steps?: {name: string, conclusion: string | null}[]}} Job
 * @typedef {{repo: {owner: string, repo: string}, serverUrl: string, runId: number, ref: string, workflow: string, eventName: string, sha: string}} RunContext
 * @typedef {{rest: {actions: {listJobsForWorkflowRun: unknown}}, paginate: (method: unknown, args: object) => Promise<Job[]>}} GitHub
 * @typedef {{warning: (message: string) => void, info: (message: string) => void}} Logger
 * @typedef {{
 *   teams?: {nodes: {id: string}[]},
 *   issueLabels?: {nodes: {id: string}[]},
 *   issueLabelCreate?: {success: boolean, issueLabel?: {id: string}},
 *   issues?: {nodes: {id: string, identifier: string}[]},
 *   commentCreate?: {success: boolean},
 *   issueCreate?: {success: boolean, issue?: {identifier: string}}
 * }} LinearData
 */

/**
 * Open or update one labelled Linear issue for a failed workflow run.
 * @param {{apiKey: string, teamKey: string, label: string, titlePrefix: string, context: RunContext, github: GitHub, core: Logger, fetch: typeof globalThis.fetch}} options
 */
export async function notifyWorkflowFailure(options) {
  const { apiKey, teamKey, label, titlePrefix, context, github, core, fetch } = options
  if (!apiKey?.trim()) throw new Error('Linear API key is required')
  const redact = (message) => String(message).split(apiKey).join('[redacted]')

  async function request(operation, query, variables) {
    let response
    try {
      response = await fetch('https://api.linear.app/graphql', {
        method: 'POST',
        headers: { authorization: apiKey, 'content-type': 'application/json' },
        body: JSON.stringify({ query, variables }),
        signal: AbortSignal.timeout(30_000),
      })
    } catch (error) {
      throw new Error(`Linear ${operation} failed: ${redact(error.message)}`)
    }
    if (!response.ok) throw new Error(`Linear ${operation} failed: HTTP ${response.status}`)
    const result = /** @type {{errors?: {message: string}[], data?: LinearData}} */ (
      await response.json()
    )
    if (result.errors?.length) {
      throw new Error(
        `Linear ${operation} failed: ${redact(result.errors.map((error) => error.message).join('; '))}`,
      )
    }
    if (!result.data) throw new Error(`Linear ${operation} failed: missing response data`)
    return result.data
  }

  const teamResult = await request(
    'NotifyTeam',
    `
    query NotifyTeam($key: String!) {
      teams(first: 1, filter: {key: {eq: $key}}) { nodes { id } }
    }`,
    { key: teamKey },
  )
  const teamId = teamResult.teams.nodes[0]?.id
  if (!teamId) throw new Error(`Linear team ${teamKey} not found`)

  const labelResult = await request(
    'NotifyLabel',
    `
    query NotifyLabel($filter: IssueLabelFilter!) {
      issueLabels(first: 1, filter: $filter) { nodes { id } }
    }`,
    {
      filter: {
        name: { eq: label },
        or: [{ team: { id: { eq: teamId } } }, { team: { null: true } }],
      },
    },
  )
  let labelId = labelResult.issueLabels.nodes[0]?.id
  if (!labelId) {
    const created = await request(
      'NotifyCreateLabel',
      `
      mutation NotifyCreateLabel($input: IssueLabelCreateInput!) {
        issueLabelCreate(input: $input) { success issueLabel { id } }
      }`,
      { input: { name: label, teamId, color: '#B60205' } },
    )
    labelId = created.issueLabelCreate.issueLabel?.id
    if (!created.issueLabelCreate.success || !labelId)
      throw new Error('Linear label creation failed')
  }

  const issueResult = await request(
    'NotifyOpenIssue',
    `
    query NotifyOpenIssue($filter: IssueFilter!) {
      issues(first: 1, filter: $filter) { nodes { id identifier } }
    }`,
    {
      filter: {
        team: { id: { eq: teamId } },
        labels: { some: { id: { eq: labelId } } },
        state: { type: { nin: ['completed', 'canceled'] } },
      },
    },
  )

  const bad = new Set(['failure', 'cancelled', 'timed_out'])
  let failedSteps = []
  try {
    const jobs = await github.paginate(github.rest.actions.listJobsForWorkflowRun, {
      ...context.repo,
      run_id: context.runId,
      filter: 'latest',
      per_page: 100,
    })
    failedSteps = jobs
      .filter((job) => bad.has(job.conclusion))
      .map((job) => {
        const named = (job.steps || [])
          .filter((step) => bad.has(step.conclusion))
          .map((step) => `\`${step.name}\` (${step.conclusion})`)
          .join(', ')
        return `- Job \`${job.name}\` (${job.conclusion}): ${named || 'no step reported a failure'}`
      })
  } catch (error) {
    core.warning(
      `Could not list the run's jobs, so the issue names no step: ${redact(error.message)}`,
    )
  }
  const runUrl = `${context.serverUrl}/${context.repo.owner}/${context.repo.repo}/actions/runs/${context.runId}`
  const body = [
    `Workflow **${context.workflow}** failed on \`${context.ref}\` (event: \`${context.eventName}\`).`,
    '',
    `- Run: ${runUrl}`,
    `- Commit: \`${context.sha}\``,
    ...(failedSteps.length > 0 ? ['', 'Failed:', ...failedSteps] : []),
    '',
    '_Opened automatically by the workflow failure notifier. Close this issue once the failure is resolved; the next failure will open a fresh one._',
  ].join('\n')

  const existing = issueResult.issues.nodes[0]
  if (existing) {
    const result = await request(
      'NotifyComment',
      `
      mutation NotifyComment($input: CommentCreateInput!) {
        commentCreate(input: $input) { success }
      }`,
      { input: { issueId: existing.id, body } },
    )
    if (!result.commentCreate.success) throw new Error('Linear comment creation failed')
    core.info(`Commented on existing issue ${existing.identifier}`)
  } else {
    const ref = context.ref.replace('refs/heads/', '')
    const result = await request(
      'NotifyCreateIssue',
      `
      mutation NotifyCreateIssue($input: IssueCreateInput!) {
        issueCreate(input: $input) { success issue { identifier } }
      }`,
      {
        input: {
          teamId,
          title: `${titlePrefix} on ${ref}`,
          description: body,
          labelIds: [labelId],
        },
      },
    )
    if (!result.issueCreate.success) throw new Error('Linear issue creation failed')
    core.info(`Opened issue ${result.issueCreate.issue.identifier}`)
  }
}
