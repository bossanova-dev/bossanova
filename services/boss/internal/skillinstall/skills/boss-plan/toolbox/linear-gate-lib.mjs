// skills-toolbox/linear-gate-lib.mjs
// Shared, pure-ish helpers for the cron "is there work?" gates that query Linear.
// NO imports beyond node builtins — the cron worktree is dependency-free. Each
// skill's gate/gate.mjs is a thin I/O entry that imports these so the query +
// auth + count logic is written once and unit-tested here.
//
// Linear access mirrors plugins/bossd-plugin-linear/linear.go: POST to
// https://api.linear.app/graphql with the personal API key sent DIRECTLY in the
// Authorization header (no "Bearer" prefix). Gates filter by state *name*
// (e.g. an unplanned versus a planned state name) because those statuses share the same stable
// state.type ("unstarted"), so a type filter cannot distinguish them.

export const LINEAR_ENDPOINT = 'https://api.linear.app/graphql'

// Smallest query that answers "does at least one matching issue exist?": ask for
// one node plus the hasNextPage flag.
const GATE_QUERY = `
  query GateIssues($filter: IssueFilter!) {
    issues(first: 1, filter: $filter) {
      nodes { id }
      pageInfo { hasNextPage }
    }
  }
`

// Build a Linear IssueFilter for an existence check. `state` matches the status
// display name; `label` (optional) requires the issue to carry a label with that
// name. Clauses are omitted when their input is falsy so Linear never rejects an
// empty sub-filter.
export function buildIssueCountFilter({ state, label } = {}) {
  const filter = {}
  if (state) filter.state = { name: { eq: state } }
  if (label) filter.labels = { name: { eq: label } }
  return filter
}

// True when a GraphQL response indicates at least one matching issue. Tolerates
// malformed payloads (returns false) so callers can treat "no usable data" as
// "no work" — error payloads are surfaced separately by runLinearGate.
export function issuesExist(graphqlJson) {
  const issues = graphqlJson?.data?.issues
  if (!issues) return false
  if (Array.isArray(issues.nodes) && issues.nodes.length > 0) return true
  return Boolean(issues.pageInfo?.hasNextPage)
}

// Generic Linear GraphQL POST with the gate's auth + fail-closed error handling.
// Returns json.data. fetchImpl/endpoint injectable for tests. Throws on a missing
// key, an HTTP error, or a GraphQL error so callers can fail closed. Shared by
// every gate so the POST + auth + error plumbing is written (and tested) once.
export async function linearRequest({
  apiKey,
  query,
  variables,
  fetchImpl = fetch,
  endpoint = LINEAR_ENDPOINT,
}) {
  if (!apiKey) throw new Error('LINEAR_API_KEY is not set')

  const response = await fetchImpl(endpoint, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: apiKey },
    body: JSON.stringify({ query, variables }),
  })

  if (!response.ok) throw new Error(`Linear API HTTP ${response.status}`)

  const json = await response.json()
  if (json?.errors?.length) {
    const messages = json.errors.map((e) => e?.message ?? String(e)).join('; ')
    throw new Error(`Linear GraphQL error: ${messages}`)
  }
  return json?.data ?? null
}

// Query Linear and resolve to whether matching work exists. fetchImpl is
// injectable for tests; defaults to the global fetch (Node 18+). Throws on a
// missing key, an HTTP error, or a GraphQL error so callers can fail closed.
export async function runLinearGate({
  apiKey,
  state,
  label,
  fetchImpl = fetch,
  endpoint = LINEAR_ENDPOINT,
}) {
  const filter = buildIssueCountFilter({ state, label })
  const data = await linearRequest({
    apiKey,
    query: GATE_QUERY,
    variables: { filter },
    fetchImpl,
    endpoint,
  })
  // issuesExist reads graphqlJson.data.issues, so wrap the returned data.
  return issuesExist({ data })
}

// Terminal helper for gate entry scripts: write a one-line reason to stderr (only
// when failing) and exit with the gate convention — 0 = run, non-zero = skip.
// exit/stderr are injectable for tests.
export function gateExit(ok, reason, { exit = process.exit, stderr = process.stderr } = {}) {
  if (!ok && reason) stderr.write(`${reason}\n`)
  exit(ok ? 0 : 1)
}
