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

//
// This file holds the ONE HTTP choke point every Linear GraphQL call in the skills tree
// reaches the network through — by direct call or by injection — so the bounded retry lives in
// `linearRequest` and nowhere else. Wrapping each caller instead would have produced several retry
// policies that drift apart; putting it here means the work gate, the blocked-work gate and every
// sweep dedupe fetch inherit one policy with no edit of their own and no way to opt out by accident.

import {
  TRACKER_RETRY_CAPS,
  TRACKER_VERDICTS,
  readEvidence,
  withBoundedRetry,
} from './tracker/outcome.mjs'

export const LINEAR_ENDPOINT = 'https://api.linear.app/graphql'

// The answer `issuesExist` gives when it could not READ the payload — distinct from `false`, which
// is the answer for a payload it read and which held no rows. Spelled as the shared `false-empty`
// verdict rather than a local sentinel so there is exactly one vocabulary for this outcome.
export const ISSUES_CANNOT_EVALUATE = TRACKER_VERDICTS.FALSE_EMPTY

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

// Smallest query that answers "who owns this API key?". A single `viewer { id }`
// selection: the gates need the id to build an equality clause and nothing else, and a
// narrower query is a smaller blast radius if the key is ever scoped down.
const VIEWER_QUERY = `
  query GateViewer {
    viewer { id }
  }
`

// Build a Linear IssueFilter for an existence check. `state` matches the status
// display name; `label` (optional) requires the issue to carry a label with that
// name, and may be a single name or an array of names matched as a disjunction —
// a candidate qualifies if it carries ANY of them. Clauses are omitted when their
// input is falsy (or an empty array) so Linear never rejects an empty sub-filter.
//
// `state` is `eq` ONLY, deliberately. A recorded field note warns that
// `state: { name: { in: [...] } }` returns 0 nodes with no error — a silent false-empty at the
// filter layer. No such filter is built anywhere in this tree and none should be: a multi-state
// existence check is two `eq` queries, which fail loudly when they fail. The note is preserved
// here, against the builder it warns about, rather than as code for a shape nobody writes.
//
// The LABEL array form uses `labels: { name: { in: [...] } }`, and that is not a contradiction of
// the note above — it is a DIFFERENT comparator on a different field, and it was settled by a live
// probe against the real API rather than by analogy: the membership query returned a matching node
// where the state-name equivalent returns none. Two reasons it is spelled this way rather than as
// a second OR group mirroring the identity disjunction below: it is one clause that reads as what
// it means, and — decisively — a filter object has exactly one top-level `or`, so an OR-group form
// would collide with `assigneeOrCreatorId` and force both under a nested `and`, changing a filter
// shape other tests pin. If a tracker ever disagrees with the probe, the fallback is that nested
// `and`, applied only when two groups are actually present.
//
// A single-element array is NOT rewritten to the `eq` form. The caller's spelling is preserved so
// the emitted filter is a function of the input shape alone, which is what makes the string path's
// output provably byte-identical to what it emitted before the array form existed.
//
// The three identity clauses are OPTIONAL and INERT by default: each contributes no key at all
// when its input is falsy, so every caller that passes only `state`/`label` — which is every
// caller in this tree today — emits the byte-identical filter it emitted before they existed.
// `assigneeOrCreatorId` is the one clause that cannot be spelled as a single field: the tracker
// expresses "assigned to X OR created by X" as a top-level `or` array, which ANDs with the
// sibling `state` and `labels` clauses rather than replacing them.
export function buildIssueCountFilter({
  state,
  label,
  assigneeId,
  creatorId,
  assigneeOrCreatorId,
} = {}) {
  const filter = {}
  if (state) filter.state = { name: { eq: state } }
  if (Array.isArray(label)) {
    if (label.length > 0) filter.labels = { name: { in: label } }
  } else if (label) {
    filter.labels = { name: { eq: label } }
  }
  if (assigneeId) filter.assignee = { id: { eq: assigneeId } }
  if (creatorId) filter.creator = { id: { eq: creatorId } }
  if (assigneeOrCreatorId) {
    filter.or = [
      { assignee: { id: { eq: assigneeOrCreatorId } } },
      { creator: { id: { eq: assigneeOrCreatorId } } },
    ]
  }
  return filter
}

// Three answers, not two: `true` (rows exist), `false` (the payload was READ and held none), and
// `ISSUES_CANNOT_EVALUATE` (the payload could not be read at all).
//
// This used to return a bare `false` for a payload it could not read, and its comment stated that
// tolerance as a deliberate intention. That intention WAS the defect: in an unattended gate a
// silent false negative is indistinguishable from a clean run, so a malformed or truncated payload
// skipped a whole cycle with nothing to show for it. The unanswerable case is now unrepresentable
// as `false`, and `runLinearGate` fails closed on it.
export function issuesExist(graphqlJson) {
  const issues = graphqlJson?.data?.issues
  if (!issues || typeof issues !== 'object') return ISSUES_CANNOT_EVALUATE
  const hasNextPage = issues.pageInfo?.hasNextPage
  const evidence = readEvidence(issues.nodes)
  if (evidence.verdict === TRACKER_VERDICTS.FALSE_EMPTY) {
    // `nodes` was unreadable. An explicit `hasNextPage: true` can still answer YES on its own — it
    // is positive evidence — but it can never answer NO, because a missing flag and a false one are
    // the same bytes here. Anything short of that explicit `true` is unanswerable.
    return hasNextPage === true ? true : ISSUES_CANNOT_EVALUATE
  }
  if (evidence.rows > 0) return true
  return hasNextPage === true
}

// Generic Linear GraphQL POST with the gate's auth + fail-closed error handling.
// Returns json.data. fetchImpl/endpoint injectable for tests. Throws on a missing
// key, an HTTP error, or a GraphQL error so callers can fail closed. Shared by
// every gate so the POST + auth + error plumbing is written (and tested) once.
//
// The one fetch runs under `withBoundedRetry`, so a transient transport failure no longer ends a
// run on its first occurrence. The signature is UNCHANGED for every existing caller: the retry is
// bounded by `TRACKER_RETRY_CAPS`, and a failure classified `permanent` — a missing key, a 401, a
// 403, a GraphQL validation error — still throws on the first attempt, so no fail-closed caller
// changes behaviour. `sleep` is injectable purely so tests never wait on wall-clock time.
export async function linearRequest({
  apiKey,
  query,
  variables,
  fetchImpl = fetch,
  endpoint = LINEAR_ENDPOINT,
  maxAttempts = TRACKER_RETRY_CAPS.maxAttempts,
  operation = 'read',
  sleep,
}) {
  // Checked ONCE, outside the retry: a missing credential is not a transport outcome and must never
  // reach the network, let alone reach it three times.
  if (!apiKey) throw new Error('LINEAR_API_KEY is not set')

  return withBoundedRetry(
    async () => {
      const response = await fetchImpl(endpoint, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Authorization: apiKey },
        body: JSON.stringify({ query, variables }),
      })

      // The status travels in the message AND on the error, so the classifier reads it from a
      // field rather than re-parsing prose — the text is a human affordance, not the signal.
      if (!response.ok) {
        const err = new Error(`Linear API HTTP ${response.status}`)
        err.status = response.status
        throw err
      }

      const json = await response.json()
      if (json?.errors?.length) {
        const messages = json.errors.map((e) => e?.message ?? String(e)).join('; ')
        const err = new Error(`Linear GraphQL error: ${messages}`)
        // Provenance travels on the error for the same reason the status above does: a GraphQL
        // rejection is server-side and deterministic whatever words it happens to contain, and
        // the server's free text is not this classifier's signal.
        err.kind = 'graphql'
        throw err
      }
      return json?.data ?? null
    },
    // Retry safety depends on what the caller SENT, so it is the caller's answer, not this
    // function's assumption. `read` is the default because the gates here issue queries, and a
    // caller that sends a mutation passes `operation: 'write'` — so a timed-out or socket-dropped
    // mutation classifies `indeterminate` and is verified by a read-back instead of blindly
    // re-sent. Naming a concrete caller here would be a repo-root path, which a published core's
    // installed tree does not contain.
    { maxAttempts, operation, ...(sleep ? { sleep } : {}) },
  )
}

// Resolve a gate's user selector to a concrete tracker user id. THE one identity contract in
// this tree, so a caller never re-derives it:
//
//   - falsy selector  -> `undefined`, ZERO requests. Callers may therefore pass an unset selector
//                        unconditionally, which is why `buildIssueCountFilter` omits the clause.
//   - the literal `me` -> one VIEWER_QUERY, returning the id of whoever owns the API key.
//   - anything else    -> returned unchanged, ZERO requests. A concrete id is already the answer.
//
// Fails CLOSED on a viewer payload carrying no id. The alternative — returning `undefined` — would
// silently drop the clause and widen a gate that was explicitly asked to narrow by identity, which
// is the direction that costs something: a gate that skips a cycle is cheap, a gate that fires on
// someone else's ticket is not.
export async function resolveLinearUserId({
  apiKey,
  user,
  fetchImpl = fetch,
  endpoint = LINEAR_ENDPOINT,
  sleep,
}) {
  if (!user) return undefined
  if (user !== 'me') return user
  const data = await linearRequest({
    apiKey,
    query: VIEWER_QUERY,
    fetchImpl,
    endpoint,
    sleep,
  })
  const id = data?.viewer?.id
  if (!id) {
    throw new Error(
      'Linear viewer lookup returned no user id, so "me" cannot be resolved; failing closed ' +
        'rather than widening the gate to every issue on the board',
    )
  }
  return id
}

// Resolve a gate's three identity selectors in one call, so the two gate runners share ONE
// preamble instead of each repeating the same three `resolveLinearUserId` awaits.
//
// Resolution is SERIAL and in this exact order — assignee, then creator, then assigneeOrCreator.
// That is not incidental: the request sequence a gate emits is pinned by tests that count and
// index into recorded bodies, so a concurrent or reordered resolution would be a behaviour change
// even though the resulting filter is identical.
//
// Cost, stated as what the code does rather than as what callers happen to pass:
//
//   - an UNSET selector costs zero requests (`resolveLinearUserId` returns early);
//   - a selector holding a CONCRETE id costs zero requests (it is already the answer);
//   - a selector set to the literal `me` costs ONE viewer lookup OF ITS OWN. The resolver is
//     deliberately not memoised, so two selectors both set to `me` cost two viewer lookups, and
//     three cost three.
//
// Callers in this tree today are ASSUMED to set at most one selector, which makes that worst case
// unreachable in practice — but that is an assumption about callers, not a property of this code,
// and a caller that breaks it pays one round trip per `me`.
export async function resolveGateSelectors({
  apiKey,
  assignee,
  creator,
  assigneeOrCreator,
  fetchImpl = fetch,
  endpoint = LINEAR_ENDPOINT,
  sleep,
}) {
  const assigneeId = await resolveLinearUserId({
    apiKey,
    user: assignee,
    fetchImpl,
    endpoint,
    sleep,
  })
  const creatorId = await resolveLinearUserId({ apiKey, user: creator, fetchImpl, endpoint, sleep })
  const assigneeOrCreatorId = await resolveLinearUserId({
    apiKey,
    user: assigneeOrCreator,
    fetchImpl,
    endpoint,
    sleep,
  })
  return { assigneeId, creatorId, assigneeOrCreatorId }
}

// Query Linear and resolve to whether matching work exists. fetchImpl is
// injectable for tests; defaults to the global fetch (Node 18+). Throws on a
// missing key, an HTTP error, or a GraphQL error so callers can fail closed.
//
// All three identity selectors are accepted here, not two: `hasWork` on the tracker adapter
// forwards whatever it is handed, and a selector this signature omits is one destructuring drops
// in silence — widening the gate to the whole board, which is the fail-open direction.
export async function runLinearGate({
  apiKey,
  state,
  label,
  assignee,
  creator,
  assigneeOrCreator,
  fetchImpl = fetch,
  endpoint = LINEAR_ENDPOINT,
  sleep,
}) {
  const { assigneeId, creatorId, assigneeOrCreatorId } = await resolveGateSelectors({
    apiKey,
    assignee,
    creator,
    assigneeOrCreator,
    fetchImpl,
    endpoint,
    sleep,
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
    query: GATE_QUERY,
    variables: { filter },
    fetchImpl,
    endpoint,
    sleep,
  })
  // issuesExist reads graphqlJson.data.issues, so wrap the returned data.
  const exists = issuesExist({ data })
  // Fail CLOSED, loudly. A gate that reports "no work" because it could not read its own answer is
  // indistinguishable from a gate that ran cleanly and found nothing; a visible skip costs the same
  // cycle and says so. This is a behaviour change to an unattended path, and the intended one.
  if (exists === ISSUES_CANNOT_EVALUATE) {
    throw new Error(
      'Linear gate could not evaluate the response: the issues payload was unreadable, so ' +
        '"no work" cannot be distinguished from "no answer"',
    )
  }
  return exists
}

// Terminal helper for gate entry scripts: write a one-line reason to stderr (only
// when failing) and exit with the gate convention — 0 = run, non-zero = skip.
// exit/stderr are injectable for tests.
export function gateExit(ok, reason, { exit = process.exit, stderr = process.stderr } = {}) {
  if (!ok && reason) stderr.write(`${reason}\n`)
  exit(ok ? 0 : 1)
}
