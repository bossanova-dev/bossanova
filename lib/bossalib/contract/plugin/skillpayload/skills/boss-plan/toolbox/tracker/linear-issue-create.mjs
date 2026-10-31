import { linearRequest } from '../linear-gate-lib.mjs'
import { TRACKER_CREDENTIALS_MISSING } from './adapter-core.mjs'
import { classifyTrackerOutcome } from './outcome.mjs'

const TEAM_QUERY = `query CreateTeam($team: String!) {
  teams(filter: {name: {eq: $team}}) {
    nodes { id states(first: 250) { nodes { id name } pageInfo { hasNextPage } } }
  }
}`
const LABEL_QUERY = `query CreateLabels($names: [String!]!, $after: String) {
  issueLabels(first: 250, filter: {name: {in: $names}}, after: $after) {
    nodes { id name team { id } } pageInfo { hasNextPage endCursor }
  }
}`
const MARKER_QUERY = `query CreateMarker($filter: IssueFilter!, $after: String) {
  issues(first: 250, filter: $filter, includeArchived: true, after: $after) {
    nodes { id identifier url description } pageInfo { hasNextPage endCursor }
  }
}`
const CREATE_MUTATION = `mutation IssueCreate($input: IssueCreateInput!) {
  issueCreate(input: $input) { success issue { id identifier url } }
}`
const nonEmpty = (value) => typeof value === 'string' && value.trim().length > 0
const connectionNodes = (value) => {
  if (!Array.isArray(value?.nodes)) throw new Error('unreadable connection')
  return value.nodes
}
const summary = ({ id, identifier, url }) => ({ id, identifier, url })
const readableIssue = (issue) => [issue?.id, issue?.identifier, issue?.url].every(nonEmpty)
const result = (outcome, extra = {}) => ({ ok: outcome === 'ok', outcome, ...extra })

/**
 * Create once after resolving tracker names; settle uncertain writes by marker reads only.
 * @param {{apiKey?: string, fetchImpl?: typeof fetch, endpoint?: string,
 * sleep?: (ms: number) => Promise<void>, team?: string, title?: string,
 * description?: string, stateName?: string, labels?: string[], marker?: string}} [input]
 */
export async function linearApplyIssueCreate({
  apiKey,
  fetchImpl,
  endpoint,
  sleep,
  team,
  title,
  description,
  stateName,
  labels = [],
  marker,
  ...unknown
} = {}) {
  if (Object.keys(unknown).length) throw new Error('tracker/linear applyIssueCreate: unknown keys')
  for (const [key, value] of Object.entries({ team, title, description, stateName })) {
    if (!nonEmpty(value)) throw new Error(`tracker/linear applyIssueCreate: ${key} is required`)
  }
  if (!Array.isArray(labels) || labels.some((name) => !nonEmpty(name)))
    throw new Error('tracker/linear applyIssueCreate: labels must be non-empty strings')
  if (marker !== undefined && (!nonEmpty(marker) || /[\r\n]/.test(marker)))
    throw new Error('tracker/linear applyIssueCreate: marker must be a non-empty single line')
  if (!apiKey)
    throw Object.assign(new Error('LINEAR_API_KEY is not set'), {
      code: TRACKER_CREDENTIALS_MISSING,
    })
  const transport = { apiKey, fetchImpl, endpoint, ...(sleep ? { sleep } : {}) }
  const read = (query, variables) => linearRequest({ ...transport, query, variables })
  const pages = async (query, variables, field) => {
    const nodes = []
    let after
    for (let page = 0; page < 20; page++) {
      const connection = (await read(query, { ...variables, ...(after ? { after } : {}) }))?.[field]
      nodes.push(...connectionNodes(connection))
      if (connection.pageInfo?.hasNextPage === false) return nodes
      if (!nonEmpty(connection.pageInfo?.endCursor) || after === connection.pageInfo.endCursor)
        throw new Error('incomplete connection')
      after = connection.pageInfo.endCursor
    }
    throw new Error('marker or label snapshot incomplete')
  }
  const lookup = async () => {
    const nodes = await pages(
      MARKER_QUERY,
      { filter: { team: { id: { eq: teamId } }, description: { contains: marker } } },
      'issues',
    )
    const matches = nodes.filter(
      (node) => typeof node?.description === 'string' && node.description.includes(marker),
    )
    if (matches.length > 1) return result('failed', { reason: 'marker-ambiguous' })
    if (!matches.length) return null
    if (!readableIssue(matches[0])) throw new Error('unreadable marked issue')
    return result('ok', { existing: true, issue: summary(matches[0]) })
  }
  let teamId
  let stateId
  const labelIds = []
  try {
    const teams = connectionNodes((await read(TEAM_QUERY, { team: team.trim() }))?.teams)
    if (teams.length !== 1 || !nonEmpty(teams[0]?.id))
      return result('failed', { reason: 'team-not-found', permanent: true })
    teamId = teams[0].id
    if (teams[0].states?.pageInfo?.hasNextPage === true)
      throw new Error('incomplete state snapshot')
    const state = connectionNodes(teams[0].states).find((state) => state?.name === stateName.trim())
    if (!nonEmpty(state?.id))
      return result('failed', { reason: 'state-not-found', permanent: true })
    stateId = state.id
    if (labels.length) {
      const nodes = await pages(LABEL_QUERY, { names: [...new Set(labels)] }, 'issueLabels')
      for (const name of new Set(labels)) {
        const matches = nodes.filter((label) => label?.name === name && nonEmpty(label?.id))
        const pick =
          matches.find((label) => label.team?.id === teamId) ?? matches.find((label) => !label.team)
        if (!pick) return result('failed', { reason: `label-not-found: ${name}`, permanent: true })
        labelIds.push(pick.id)
      }
    }
    if (marker) {
      const existing = await lookup()
      if (existing) return existing
    }
  } catch (error) {
    return result('failed', { reason: `create-read-failed: ${error.message}` })
  }
  const body = marker && !description.includes(marker) ? `${description}\n${marker}` : description
  let outcome
  try {
    const data = await linearRequest({
      ...transport,
      query: CREATE_MUTATION,
      variables: { input: { teamId, title, description: body, stateId, labelIds } },
      operation: 'write',
    })
    if (data?.issueCreate?.success === false)
      return result('failed', { reason: 'issue-create-rejected' })
    if (data?.issueCreate?.success === true && readableIssue(data.issueCreate.issue))
      return result('ok', { issue: summary(data.issueCreate.issue) })
    outcome = result('indeterminate', { reason: 'issue-create-unreadable' })
  } catch (error) {
    const verdict = classifyTrackerOutcome(error, { operation: 'write' }).verdict
    outcome = result(verdict === 'indeterminate' ? 'indeterminate' : 'failed', {
      reason: `issue-create: ${error.message}`,
    })
  }
  if (outcome.outcome === 'indeterminate' && marker) {
    try {
      const settled = await lookup()
      if (settled) return settled
    } catch {
      /* An unsuccessful settle read cannot disprove the write. */
    }
  }
  return outcome
}
