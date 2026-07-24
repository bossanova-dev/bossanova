// scripts/sweep-sentry-gate.mjs
// Pure-core decision logic for the bs-sweep-sentry skill. NO side effects,
// NO imports beyond node builtins — the cron worktree is dependency-free.

// Sentry `level` → weight. Missing or unrecognized levels fall back to the
// "unknown" weight of 10 (LEVEL_WEIGHT[level] ?? 10 below handles both cases).
const LEVEL_WEIGHT = { fatal: 40, error: 30, warning: 15, info: 5, debug: 0 }

const HOUR_MS = 60 * 60 * 1000
const DAY_MS = 24 * HOUR_MS

// Event-volume tiers. `issue.count` is a STRING on the live API — Number()-coerce it.
// n >= 1000 → 20, >= 100 → 15, >= 10 → 10, >= 2 → 5, else (incl. NaN/negative) → 0.
function volumeScore(count) {
  const n = Number(count)
  if (!Number.isFinite(n) || n < 0) return 0
  if (n >= 1000) return 20
  if (n >= 100) return 15
  if (n >= 10) return 10
  if (n >= 2) return 5
  return 0
}

// Users-affected tiers.
// u >= 100 → 20, >= 10 → 15, >= 2 → 10, >= 1 → 5, else (incl. NaN/negative) → 0.
function usersScore(userCount) {
  const u = Number(userCount)
  if (!Number.isFinite(u) || u < 0) return 0
  if (u >= 100) return 20
  if (u >= 10) return 15
  if (u >= 2) return 10
  if (u >= 1) return 5
  return 0
}

// Recency of `lastSeen` relative to an injected `now`. Missing/unparseable
// lastSeen → 0. within 24h → 10, within 7 days → 5, else → 0.
function recencyScore(lastSeen, now) {
  const t = Date.parse(lastSeen ?? '')
  if (!Number.isFinite(t)) return 0
  const age = Math.abs(now - t)
  if (age <= DAY_MS) return 10
  if (age <= 7 * DAY_MS) return 5
  return 0
}

/**
 * Score a Sentry issue 0–100-ish. Pure and deterministic given `now` (the
 * recency signal needs a reference time — tests always inject `now`).
 *
 * Weight table:
 *   level:        fatal 40, error 30, warning 15, info 5, debug 0, unknown/missing 10
 *   event volume: n=Number(count); >=1000 20, >=100 15, >=10 10, >=2 5, else 0
 *   users:        u=Number(userCount); >=100 20, >=10 15, >=2 10, >=1 5, else 0
 *   isUnhandled:  === true → +10
 *   recency:      lastSeen within 24h +10, within 7d +5, else 0 (missing → 0)
 *
 * Tolerates malformed input (null, missing fields) without throwing.
 */
export function scoreIssue(issue, { now = Date.now() } = {}) {
  const levelWeight = LEVEL_WEIGHT[issue?.level] ?? 10
  const volumeWeight = volumeScore(issue?.count)
  const usersWeight = usersScore(issue?.userCount)
  const unhandledWeight = issue?.isUnhandled === true ? 10 : 0
  const recencyWeight = recencyScore(issue?.lastSeen, now)
  return levelWeight + volumeWeight + usersWeight + unhandledWeight + recencyWeight
}

// shortIds contain `-` and could contain other regex metacharacters — escape them
// before building the per-shortId marker regex.
function escapeRegExp(s) {
  return String(s).replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

/**
 * Returns the subset of `sentryIssues` NOT already tracked in Linear. A Sentry
 * issue is "already tracked" iff any Linear issue's `description` contains the
 * exact marker line `Sentry: <shortId>` — line-anchored, case-sensitive,
 * allowing trailing whitespace only. Sentry issues without a string `shortId`
 * are never treated as tracked (nothing to match against).
 */
export function dedupeAgainstIssues(sentryIssues, linearIssues = []) {
  const descriptions = linearIssues.map((li) => String(li?.description ?? ''))
  return sentryIssues.filter((issue) => {
    const shortId = issue?.shortId
    if (typeof shortId !== 'string') return true
    const marker = new RegExp('^Sentry: ' + escapeRegExp(shortId) + '[ \\t]*$', 'm')
    return !descriptions.some((desc) => marker.test(desc))
  })
}

/**
 * The single selection entry point — the skill and the cron gate both call
 * this; neither re-implements triage.
 *
 * 1. Sanitize: a non-array `sentryIssues` → treated as []. Entries that are
 *    not objects or lack a string `shortId` are excluded (payload
 *    sanitization, not a reasoned drop — they never appear in the output).
 * 2. Score every remaining issue once via scoreIssue (now threaded through).
 * 3. Partition (every non-top issue carries a reason — no silent caps):
 *    - already tracked in Linear → dropped, reason 'already-tracked'
 *    - score < threshold (and not tracked) → dropped, reason 'below-threshold'
 *    - remaining eligible issues, sorted score desc (tie-break shortId asc):
 *      first → top; next up-to-cap → deferred, reason 'over-cap' (stay
 *      eligible for later runs); beyond 1+cap → dropped, reason 'over-cap'.
 */
export function selectTop(
  sentryIssues,
  linearIssues = [],
  { threshold = 40, cap = 25, now = Date.now() } = {},
) {
  const candidates = Array.isArray(sentryIssues)
    ? sentryIssues.filter((it) => it && typeof it === 'object' && typeof it.shortId === 'string')
    : []

  const notTrackedIds = new Set(dedupeAgainstIssues(candidates, linearIssues).map((i) => i.shortId))

  const dropped = []
  const eligible = []
  for (const issue of candidates) {
    const score = scoreIssue(issue, { now })
    if (!notTrackedIds.has(issue.shortId)) {
      dropped.push({ shortId: issue.shortId, title: issue.title, score, reason: 'already-tracked' })
      continue
    }
    if (score < threshold) {
      dropped.push({ shortId: issue.shortId, title: issue.title, score, reason: 'below-threshold' })
      continue
    }
    eligible.push({ issue, score })
  }

  eligible.sort((a, b) => {
    if (b.score !== a.score) return b.score - a.score
    return a.issue.shortId < b.issue.shortId ? -1 : a.issue.shortId > b.issue.shortId ? 1 : 0
  })

  const top = eligible.length > 0 ? { issue: eligible[0].issue, score: eligible[0].score } : null
  const deferred = eligible.slice(1, 1 + cap).map(({ issue, score }) => ({
    shortId: issue.shortId,
    title: issue.title,
    score,
    reason: 'over-cap',
  }))
  for (const { issue, score } of eligible.slice(1 + cap)) {
    dropped.push({ shortId: issue.shortId, title: issue.title, score, reason: 'over-cap' })
  }

  return { top, deferred, dropped }
}

// Parse the rel="next" target out of a Sentry `Link` response header. Sentry
// marks a real next page with results="true"; absent header, rel="next" with
// results="false", or a malformed header all mean "no more pages".
function nextPageUrl(linkHeader) {
  if (typeof linkHeader !== 'string') return null
  for (const part of linkHeader.split(',')) {
    if (!part.includes('rel="next"')) continue
    if (!part.includes('results="true"')) return null
    const m = part.match(/<([^>]+)>/)
    return m ? m[1] : null
  }
  return null
}

/**
 * Thin async I/O wrapper, kept out of the pure core. Fetches unresolved issues
 * at Sentry's max page size (limit=100) and follows `Link: rel="next"`
 * pagination up to `maxPages` pages (default 20 → a 2000-issue window), so the
 * triage window is not silently capped at the API's 25-issue default page.
 * The window must stay generous: already-tracked issues remain unresolved in
 * Sentry (filing a Linear stub does not resolve them), so they keep occupying
 * window slots until actually fixed — a tight window would let them starve
 * actionable issues beyond it.
 */
export async function fetchSentryIssues({
  token,
  org,
  query = 'is:unresolved',
  fetchImpl = fetch,
  maxPages = 20,
} = {}) {
  if (!token || !String(token).trim()) throw new Error('SENTRY_API_KEY is not set')
  if (!org || !String(org).trim()) throw new Error('SENTRY_ORG is not set')
  let url = `https://sentry.io/api/0/organizations/${encodeURIComponent(org)}/issues/?query=${encodeURIComponent(query)}&limit=100`
  const issues = []
  for (let page = 0; page < maxPages && url; page++) {
    const response = await fetchImpl(url, { headers: { Authorization: `Bearer ${token}` } })
    if (!response.ok) {
      const hint =
        response.status === 401 || response.status === 403
          ? 'check the API token / event:read scope'
          : response.status === 404
            ? 'check the org slug'
            : null
      const err = new Error(`Sentry API HTTP ${response.status}${hint ? ` — ${hint}` : ''}`)
      err.status = response.status
      throw err
    }
    const body = await response.json()
    issues.push(...(Array.isArray(body) ? body : []))
    url = nextPageUrl(response.headers?.get?.('link'))
  }
  return issues
}

// GraphQL for the Linear marker scan, owned here so the cron gate and the
// SKILL.md Phase 1 fetch share ONE copy — query drift between them would let
// dedupe snapshots disagree and file duplicate stubs. Scans ALL states
// including ARCHIVED tickets (includeArchived: true — Linear excludes them by
// default, and an archived marker-carrier would otherwise make the gate fire
// forever for an issue the write path refuses to re-file).
export const MARKED_ISSUES_QUERY = `query Marked($filter: IssueFilter!, $after: String) {
  issues(first: 250, filter: $filter, after: $after, includeArchived: true) {
    nodes { identifier description }
    pageInfo { hasNextPage endCursor }
  }
}`

/**
 * Fetch every Linear issue whose description carries a marker with the given
 * `markerPrefix` (default "Sentry: "; bs-sweep-security's main-gate probe passes
 * "MainGate: " to reuse this same paginated, fail-closed scan rather than
 * re-implementing it). `linearRequest` is injected (scripts/linear-gate-lib.mjs's
 * export in production) so this module keeps its no-local-imports contract and the
 * loop is unit-testable. Bounded pagination (default 20 pages) so a pathological
 * workspace cannot hang the gate — but a TRUNCATED scan is an error, not a
 * result: triaging a partial dedupe snapshot could select an already-tracked
 * issue, so hitting the bound with pages left throws (fail-closed).
 */
export async function fetchMarkedLinearIssues({
  apiKey,
  linearRequest,
  maxPages = 20,
  markerPrefix = 'Sentry: ',
} = {}) {
  if (typeof linearRequest !== 'function') {
    throw new Error('fetchMarkedLinearIssues requires a linearRequest implementation')
  }
  const filter = { description: { contains: markerPrefix } }
  const nodes = []
  let after = null
  for (let page = 0; page < maxPages; page++) {
    const data = await linearRequest({
      apiKey,
      query: MARKED_ISSUES_QUERY,
      variables: { filter, after },
    })
    const conn = data?.issues
    if (!conn) throw new Error('Linear marker scan returned no issues connection')
    nodes.push(...(conn.nodes ?? []))
    if (!conn.pageInfo?.hasNextPage) return nodes
    after = conn.pageInfo.endCursor
  }
  throw new Error(`Linear marker scan exceeded ${maxPages} pages — dedupe snapshot incomplete`)
}

// append to scripts/sweep-sentry-gate.mjs
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

export function runCli(argv, { readFile = (f) => readFileSync(f, 'utf8') } = {}) {
  const [cmd, ...rest] = argv
  const json = (f) => JSON.parse(readFile(f))
  switch (cmd) {
    case 'score':
      return JSON.stringify(
        json(rest[0]).map((issue) => ({ shortId: issue.shortId, score: scoreIssue(issue) })),
      )
    case 'select-top':
      return JSON.stringify(
        selectTop(json(rest[0]), json(rest[1]), {
          threshold: rest[2] !== undefined ? Number(rest[2]) : undefined,
          cap: rest[3] !== undefined ? Number(rest[3]) : undefined,
        }),
      )
    case 'dedupe':
      return JSON.stringify(dedupeAgainstIssues(json(rest[0]), json(rest[1])).map((i) => i.shortId))
    default:
      throw new Error(`unknown subcommand: ${cmd}`)
  }
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  try {
    process.stdout.write(runCli(process.argv.slice(2)))
    process.stdout.write('\n')
  } catch (err) {
    process.stderr.write(`sweep-sentry-gate: ${err.message}\n`)
    process.exit(1)
  }
}
