#!/usr/bin/env node

const { execFileSync } = require('child_process')
const { createHash } = require('crypto')
const {
  existsSync,
  lstatSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  renameSync,
  unlinkSync,
  writeFileSync,
} = require('fs')
const os = require('os')
const path = require('path')

const INLINE_COMMENT_DISPLAY_LIMIT = 100
const REVIEW_THREAD_DISPLAY_LIMIT = 100
const DEFAULT_HOST = 'github.com'
const CONTRACT_VERSION = 'review-feedback-probe/v3'

// Exit statuses. A degraded observation gets its own value so a caller that only inspects the exit
// status cannot mistake it for a clean run, and cannot confuse it with either a hard failure or the
// suspicious-zero case.
const EXIT_PROBE_FAILED = 1
const EXIT_SUSPICIOUS_ZERO = 2
const EXIT_DEGRADED = 3

// DEGRADED_READ_TOKEN is emitted once per degraded read, in a single uniform shape, so every relaxed
// read in a transcript is findable with one search. A relaxed gate whose failures are no longer
// visible anywhere has been deleted, not relaxed.
const DEGRADED_READ_TOKEN = 'DEGRADED_READ'

let contractPrinted = false

function printContract() {
  if (!contractPrinted) {
    console.log(`probe_contract=${CONTRACT_VERSION}`)
    contractPrinted = true
  }
}

function runGh(args, options = {}) {
  let out
  try {
    // stderr stays on its own pipe rather than merged into stdout. The merge would let unrelated
    // load-time noise on stderr become the text the failure classifier branches on.
    out = execFileSync('gh', args, {
      encoding: 'utf8',
      maxBuffer: 20 * 1024 * 1024,
      stdio: ['ignore', 'pipe', 'pipe'],
    })
  } catch (error) {
    if (options.hint) {
      // Appended rather than substituted: the original signature must survive for the classifier,
      // and the hint must survive redaction, which runs over this same value.
      error.stderr = `${error.stderr ? error.stderr.toString() : error.message}\n${options.hint}`
    }
    throw error
  }

  if (!out.trim()) {
    throw new Error(`gh ${args.join(' ')} produced empty stdout`)
  }

  return out
}

function redactCredentials(value) {
  return String(value || '')
    .replace(/\bgh[os]_[A-Za-z0-9_]+\b/g, '[redacted]')
    .replace(/x-access-token:[^@\s]+@/g, 'x-access-token:[redacted]@')
}

function compact(value, max = 360) {
  return String(value || '')
    .replace(/\s+/g, ' ')
    .trim()
    .slice(0, max)
}

function failReason(error) {
  const stderr = error?.stderr ? error.stderr.toString() : ''
  return compact(redactCredentials(stderr || error?.message || error))
}

// FAILURE_CLASS_* are the vocabulary the printed `probe_failure_class` field carries. A consumer
// routes a transient class to a reported residual and a permanent class to a true stop by reading
// that field, rather than by pattern-matching free-text stderr.
const FAILURE_CLASS_RATE_LIMITED = 'rate_limited'
const FAILURE_CLASS_AUTH = 'auth'
const FAILURE_CLASS_NOT_FOUND = 'not_found'
const FAILURE_CLASS_ENVIRONMENT = 'environment'
const FAILURE_CLASS_OTHER = 'other'
const FAILURE_CLASS_NONE = 'none'

// DEGRADABLE_FAILURE_CLASSES are the classes a caller may continue past with a degraded, explicitly
// unobserved reading. `other` is included deliberately: an unrecognised signature is the
// conservative default, and a degraded reading can never reach `clean`, so admitting it here cannot
// manufacture a pass. `auth`, `not_found`, and `environment` are absent because none of them is
// transient — continuing past one would report a PR nobody observed.
const DEGRADABLE_FAILURE_CLASSES = new Set([FAILURE_CLASS_RATE_LIMITED, FAILURE_CLASS_OTHER])

const RATE_LIMIT_TEXT =
  /api rate limit exceeded|secondary rate limit|rate limit exceeded|was submitted too quickly|exceeded a secondary rate limit/

function graphqlErrorTypes(error) {
  const errors = Array.isArray(error?.graphqlErrors) ? error.graphqlErrors : []
  return errors.map((entry) => String(entry?.type || '').toUpperCase()).filter(Boolean)
}

function httpStatusFrom(error) {
  if (Number.isInteger(error?.httpStatus)) {
    return error.httpStatus
  }
  const match = `${error?.stderr || ''}\n${error?.message || ''}`.match(/\bHTTP\s+(\d{3})\b/)
  return match ? Number(match[1]) : 0
}

// retryHorizonFrom reads a reset epoch or a retry delay out of whatever the failure carried. It is
// only the fallback: the degraded path prefers a REST rate-limit read, which answers authoritatively
// and costs no GraphQL budget.
function retryHorizonFrom(error) {
  const text = `${error?.stderr || ''}\n${error?.message || ''}`
  const reset = text.match(/x-ratelimit-reset:\s*(\d+)/i)
  if (reset) {
    return reset[1]
  }
  const retryAfter = text.match(/retry-after:\s*(\d+)/i)
  if (retryAfter) {
    return retryAfter[1]
  }
  return ''
}

// classifyGhFailure turns a `gh` failure into one of the FAILURE_CLASS_* values. It is pure — error
// in, class out — so the whole decision ladder is unit-testable without a network, in the same shape
// as repairStatusFromReviewProbe below.
//
// The rungs are ordered structural-first, text-last, and that order is load-bearing. A structured
// GraphQL `errors[].type` and an HTTP status cannot be reworded by the provider; stderr text can,
// and a retryable failure whose wording reads permanent is precisely the
// transient-misclassified-as-permanent symptom this classifier exists to prevent. Unrecognised text
// falls to `other`, which routes conservatively rather than optimistically.
function classifyGhFailure(error) {
  const stderr = String(error?.stderr || '')
  const message = String(error?.message || '')
  const detail = compact(redactCredentials(stderr || message || error))
  const retryAfter = retryHorizonFrom(error)
  const classified = (failureClass) => ({ failureClass, retryAfter, detail })

  // Rung 1: the structured GraphQL error types, when a response body carried any.
  const types = graphqlErrorTypes(error)
  if (types.length > 0) {
    if (types.includes('RATE_LIMITED')) {
      return classified(FAILURE_CLASS_RATE_LIMITED)
    }
    if (types.includes('UNAUTHORIZED') || types.includes('FORBIDDEN')) {
      return classified(FAILURE_CLASS_AUTH)
    }
    if (types.includes('NOT_FOUND')) {
      return classified(FAILURE_CLASS_NOT_FOUND)
    }
    return classified(FAILURE_CLASS_OTHER)
  }

  const haystack = `${stderr}\n${message}`.toLowerCase()
  const rateLimited = RATE_LIMIT_TEXT.test(haystack)

  // Rung 2: the HTTP status the failure carries.
  const status = httpStatusFrom(error)
  if (status === 429 || (status === 403 && rateLimited)) {
    return classified(FAILURE_CLASS_RATE_LIMITED)
  }
  if (status === 401) {
    return classified(FAILURE_CLASS_AUTH)
  }
  if (status === 404) {
    return classified(FAILURE_CLASS_NOT_FOUND)
  }

  // Rung 3: the stderr text, last because it is the only rung a provider rewording can break.
  if (rateLimited) {
    return classified(FAILURE_CLASS_RATE_LIMITED)
  }
  if (/bad credentials/.test(haystack)) {
    return classified(FAILURE_CLASS_AUTH)
  }
  if (error?.code === 'ENOENT' || /spawnsync gh enoent|executable file not found/.test(haystack)) {
    return classified(FAILURE_CLASS_ENVIRONMENT)
  }
  if (/not a git repository/.test(haystack)) {
    return classified(FAILURE_CLASS_ENVIRONMENT)
  }
  if (/could not resolve to a/.test(haystack)) {
    return classified(FAILURE_CLASS_NOT_FOUND)
  }
  return classified(FAILURE_CLASS_OTHER)
}

// graphqlResponseError builds the error raised when a GraphQL call returns a zero exit status and a
// body carrying `errors[]` instead of `data`. Attaching the array is what lets classifyGhFailure see
// a `RATE_LIMITED` type on this path; replacing the body with a generic message would discard the
// only signature the classifier could have learned anything from.
function graphqlResponseError(errors) {
  const messages = errors.map((entry) => String(entry?.message || '')).filter(Boolean)
  const types = errors.map((entry) => String(entry?.type || '')).filter(Boolean)
  const summary = messages.join('; ') || types.join(', ') || 'unspecified GraphQL error'
  const error = new Error(`GraphQL response carried errors: ${compact(redactCredentials(summary))}`)
  error.graphqlErrors = errors
  return error
}

function parsePaginatedArray(raw) {
  const parsed = JSON.parse(raw)
  if (!Array.isArray(parsed)) {
    return []
  }

  if (parsed.every((page) => Array.isArray(page))) {
    return parsed.flat()
  }

  return parsed
}

function withPrivateUmask(work) {
  const previous = process.umask(0o077)
  try {
    return work()
  } finally {
    process.umask(previous)
  }
}

function requireSafeDirectory(dir) {
  if (!existsSync(dir)) {
    withPrivateUmask(() => mkdirSync(dir, { recursive: true, mode: 0o700 }))
  }
  const stat = lstatSync(dir)
  if (stat.isSymbolicLink()) {
    throw new Error(`refusing symlinked review disposition state root: ${dir}`)
  }
  if (!stat.isDirectory()) {
    throw new Error(`review disposition state root is not a directory: ${dir}`)
  }
  if (typeof process.getuid === 'function' && stat.uid !== process.getuid()) {
    throw new Error(`refusing non-owned review disposition state root: ${dir}`)
  }
}

function journalKey({ host, owner, name, pr }) {
  return `${host}-${owner}-${name}-${pr}`.toLowerCase().replace(/[^a-z0-9._-]/g, '_')
}

function stateDirectory({ stateRoot, host, owner, name, pr }) {
  const root =
    stateRoot || process.env.BOSS_REPAIR_STATE_DIR || path.join(os.tmpdir(), 'boss-repair-state')
  requireSafeDirectory(root)
  const child = journalKey({ host, owner, name, pr })
  const dir = path.join(root, child)
  withPrivateUmask(() => mkdirSync(dir, { recursive: true, mode: 0o700 }))
  requireSafeDirectory(dir)
  return dir
}

function threadIdentity(thread) {
  const comments = thread?.comments?.nodes || []
  const last = comments[comments.length - 1] || {}
  return createHash('sha256')
    .update(`${last.author?.login || ''}\0${last.databaseId || last.id || ''}`)
    .digest('hex')
}

function threadRecordPath(context, threadId) {
  const key = createHash('sha256').update(String(threadId)).digest('hex')
  return path.join(stateDirectory(context), `${key}.json`)
}

function readThreadDisposition(context, threadId) {
  const recordPath = threadRecordPath(context, threadId)
  if (!existsSync(recordPath)) {
    return null
  }
  const record = JSON.parse(readFileSync(recordPath, 'utf8'))
  return record?.threadId === threadId ? record : null
}

function markThreadDisposition(context, thread, disposition) {
  if (!['dispatched', 'needs-human'].includes(disposition)) {
    throw new Error(`unsupported review thread disposition: ${disposition}`)
  }
  const recordPath = threadRecordPath(context, thread.id)
  const record = {
    threadId: thread.id,
    disposition,
    actedIdentity: threadIdentity(thread),
  }
  const temporary = `${recordPath}.${process.pid}.tmp`
  withPrivateUmask(() => writeFileSync(temporary, `${JSON.stringify(record)}\n`, { mode: 0o600 }))
  renameSync(temporary, recordPath)
  return record
}

function clearThreadDisposition(context, threadId) {
  const recordPath = threadRecordPath(context, threadId)
  if (existsSync(recordPath)) {
    unlinkSync(recordPath)
  }
}

// The deferred-resolution queue: a thread whose REST reply landed but whose GraphQL resolve was
// rate-limited. It is a SIBLING record kind of the disposition journal, not a third disposition
// value: markThreadDisposition hard-validates against ['dispatched', 'needs-human'] and
// reconcileReviewThreads partitions purely from those two, so a third value would silently change
// repair routing. It records work owed, not who owns triage.
//
// It lives under the same stateDirectory root so it inherits that root's hardening —
// requireSafeDirectory fails closed on a symlinked or non-owned root, and withPrivateUmask keeps
// records at 0o700/0o600 — rather than re-implementing those guards for a second store.
const DEFERRED_RECORD_SUFFIX = '.deferred.json'

function deferredRecordPath(context, threadId) {
  const key = createHash('sha256').update(String(threadId)).digest('hex')
  return path.join(stateDirectory(context), `${key}${DEFERRED_RECORD_SUFFIX}`)
}

function readDeferredResolution(context, threadId) {
  const recordPath = deferredRecordPath(context, threadId)
  if (!existsSync(recordPath)) {
    return null
  }
  const record = JSON.parse(readFileSync(recordPath, 'utf8'))
  return record?.threadId === threadId ? record : null
}

// markDeferredResolution stores the thread id and the already-landed reply's identity, and NOTHING
// ELSE. No reply body, no reply endpoint. By the time a record exists the REST reply has already
// posted, so re-running it on quota recovery would double-post a review comment; storing nothing the
// drain could re-submit makes the narrow retry span a property of the data rather than a rule a
// later refactor could widen.
function markDeferredResolution(context, threadId, replyUrl = '') {
  if (!threadId) {
    throw new Error('deferred resolution requires a thread id')
  }
  const recordPath = deferredRecordPath(context, threadId)
  const record = {
    threadId,
    kind: 'deferred-resolution',
    repliedUrl: String(replyUrl || ''),
    deferredAt: new Date().toISOString(),
  }
  const temporary = `${recordPath}.${process.pid}.tmp`
  withPrivateUmask(() => writeFileSync(temporary, `${JSON.stringify(record)}\n`, { mode: 0o600 }))
  renameSync(temporary, recordPath)
  return record
}

function clearDeferredResolution(context, threadId) {
  const recordPath = deferredRecordPath(context, threadId)
  if (existsSync(recordPath)) {
    unlinkSync(recordPath)
  }
}

function listDeferredResolutions(context) {
  const dir = stateDirectory(context)
  return readdirSync(dir)
    .filter((entry) => entry.endsWith(DEFERRED_RECORD_SUFFIX))
    .sort()
    .map((entry) => {
      try {
        return JSON.parse(readFileSync(path.join(dir, entry), 'utf8'))
      } catch {
        return null
      }
    })
    .filter((record) => record?.threadId)
}

// resolveReviewThread issues the GraphQL resolution mutation. There is no REST equivalent — REST
// exposes neither a thread's resolution state nor a way to change it — which is why the fallback for
// resolution is deferral rather than substitution.
function resolveReviewThread(context, threadId) {
  const query = `
    mutation($threadId: ID!) {
      resolveReviewThread(input: {threadId: $threadId}) {
        thread { isResolved }
      }
    }`
  const raw = runGh([
    'api',
    ...hostnameArgs(context.host),
    'graphql',
    '-f',
    `threadId=${threadId}`,
    '-f',
    `query=${query}`,
  ])
  const parsed = JSON.parse(raw)
  if (Array.isArray(parsed?.errors) && parsed.errors.length > 0) {
    throw graphqlResponseError(parsed.errors)
  }
  return parsed?.data?.resolveReviewThread?.thread?.isResolved === true
}

// drainDeferredResolutions retries the RESOLVE and nothing else. It never touches a reply endpoint,
// and it cannot: the records carry nothing to post. It also retries once per pass and leaves an
// unresolved record for the next pass rather than looping internally — nesting a bounded retry
// inside the outer repair loop's bounded retry would make both bounds a lie.
function drainDeferredResolutions(context) {
  const pending = listDeferredResolutions(context)
  console.log(`deferred_pending=${pending.length}`)

  let resolved = 0
  const remaining = []
  for (const record of pending) {
    try {
      if (!resolveReviewThread(context, record.threadId)) {
        // A zero-exit body carrying no `errors[]` that nonetheless did not report `isResolved` is
        // an UNOBSERVED resolve, not a successful one. Keep the record so a later pass retries it,
        // rather than erasing the only evidence that a reply landed on an unresolved thread.
        throw new Error(`resolveReviewThread did not report isResolved for ${record.threadId}`)
      }
      clearDeferredResolution(context, record.threadId)
      resolved += 1
      console.log(`deferred_resolved=${record.threadId}`)
    } catch (error) {
      const classified = classifyGhFailure(error)
      remaining.push({ record, classified })
      // The record stays in place on every failure class. A transient class will succeed on a later
      // pass; a permanent one is a condition a human has to see, and deleting the record would erase
      // the only evidence that a reply landed on a thread nobody resolved.
      console.log(`deferred_remaining_thread=${record.threadId} class=${classified.failureClass}`)
      printDegradedReadLine('deferred_resolution', classified)
    }
  }

  console.log(`deferred_resolved_count=${resolved}`)
  console.log(`deferred_remaining_count=${remaining.length}`)
  printProbeFailureFields(remaining.length > 0 ? remaining[0].classified : null)
  if (remaining.length > 0) {
    process.exit(EXIT_DEGRADED)
  }
}

function reconcileReviewThreads(context, threads) {
  const actionable = []
  const parked = []
  for (const thread of threads) {
    const record = readThreadDisposition(context, thread.id)
    if (record?.disposition === 'needs-human' && record.actedIdentity === threadIdentity(thread)) {
      parked.push(thread)
      continue
    }
    if (record?.disposition === 'needs-human') {
      clearThreadDisposition(context, thread.id)
    }
    actionable.push(thread)
  }
  return { actionable, parked }
}

function repairStatusFromReviewProbe({
  suspiciousZero = false,
  unresolvedCount = 0,
  actionableCount = unresolvedCount,
  inlineCommentCount = 0,
  reviewThreadCount = 0,
  latestCommented = false,
}) {
  if (suspiciousZero) {
    return { status: 'unknown', reason: 'commented review but no comments found' }
  }
  if (actionableCount > 0) {
    return { status: 'needs_repair', reason: 'unresolved review threads' }
  }
  if (unresolvedCount > 0) {
    return { status: 'parked', reason: 'unresolved review threads are parked' }
  }
  if (reviewThreadCount === 0 && (inlineCommentCount > 0 || latestCommented)) {
    return { status: 'unknown', reason: 'inline comments without review thread state' }
  }
  return { status: 'clean', reason: 'no unresolved review threads' }
}

function parseFlag(args, flag) {
  const index = args.indexOf(flag)
  return index >= 0 ? args[index + 1] || '' : ''
}

function validateRepo(value) {
  if (!/^[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+$/.test(value)) {
    throw new Error(`invalid --repo: ${value}`)
  }
  const [owner, name] = value.split('/')
  return { owner, name }
}

function validatePr(value) {
  if (!/^[1-9][0-9]*$/.test(value)) {
    throw new Error(`invalid --pr: ${value}`)
  }
  return Number(value)
}

function validateHost(value) {
  if (!/^[A-Za-z0-9.-]+$/.test(value)) {
    throw new Error(`invalid --host: ${value}`)
  }
  return value
}

function hostnameFromUrl(value) {
  if (!value) {
    return ''
  }
  try {
    return new URL(value).hostname
  } catch {
    return ''
  }
}

function hostnameArgs(host) {
  return host && host !== DEFAULT_HOST ? ['--hostname', host] : []
}

function repoArgForHost(repo, host) {
  return host && host !== DEFAULT_HOST ? `${host}/${repo}` : repo
}

function resolveHost({ requestedHost, repoForHost }) {
  if (requestedHost) {
    return validateHost(requestedHost)
  }
  if (process.env.GH_HOST) {
    return validateHost(process.env.GH_HOST)
  }
  try {
    const args = ['repo', 'view', '--json', 'url']
    if (repoForHost) {
      args.splice(2, 0, repoForHost)
    }
    const repoView = JSON.parse(runGh(args))
    return hostnameFromUrl(repoView.url) || DEFAULT_HOST
  } catch {
    return DEFAULT_HOST
  }
}

function resolveIdentity(args, options = {}) {
  const repoFlag = parseFlag(args, '--repo')
  const prFlag = parseFlag(args, '--pr')
  const hostFlag = parseFlag(args, '--host')
  if ((repoFlag && !prFlag) || (!repoFlag && prFlag)) {
    throw new Error('--repo and --pr must be supplied together')
  }

  if (repoFlag || prFlag) {
    const repo = validateRepo(repoFlag)
    const pr = validatePr(prFlag)
    const host = resolveHost({ requestedHost: hostFlag, repoForHost: repoFlag })
    if (options.skipPrView) {
      return { ...repo, pr, host, prView: { number: pr, latestReviews: [] } }
    }
    try {
      const prView = JSON.parse(
        runGh([
          'pr',
          'view',
          String(pr),
          '--repo',
          repoArgForHost(`${repo.owner}/${repo.name}`, host),
          '--json',
          'number,latestReviews,url',
        ]),
      )
      return { ...repo, pr: prView.number || pr, host, prView }
    } catch (error) {
      // `gh pr view` is GraphQL-backed and runs before any REST read, so a limit here would strand
      // the probe before the REST path it already has could help. The explicit flags already carry
      // the whole subject identity, so the read is degradable: continue without `latestReviews`,
      // and record the degradation so no output path from here can report a content verdict.
      const classified = classifyGhFailure(error)
      if (!DEGRADABLE_FAILURE_CLASSES.has(classified.failureClass)) {
        throw error
      }
      return {
        ...repo,
        pr,
        host,
        prView: { number: pr, latestReviews: [] },
        degradedIdentity: classified,
      }
    }
  }

  const host = resolveHost({ requestedHost: hostFlag })
  const prView = JSON.parse(
    runGh(['pr', 'view', '--json', 'number,latestReviews,url'], {
      // Without `--repo`/`--pr` there is no GraphQL-free way to discover WHICH pull request this is,
      // so this read is a capability, not an observation that can be degraded. Name the flags that
      // make it degradable rather than failing with a bare quota message.
      hint: 'pass --repo OWNER/REPO --pr PR_NUM to run the probe without this GraphQL identity read',
    }),
  )
  const repoView = JSON.parse(runGh(['repo', 'view', '--json', 'owner,name']))
  return {
    owner: repoView.owner.login,
    name: repoView.name,
    pr: prView.number,
    host: host || hostnameFromUrl(prView.url) || DEFAULT_HOST,
    prView,
  }
}

function markArguments(args) {
  if (args[0] !== 'mark') {
    return null
  }
  const threadIndex = args.indexOf('--thread')
  const dispositionIndex = args.indexOf('--disposition')
  const threadId = threadIndex >= 0 ? args[threadIndex + 1] : ''
  const disposition = dispositionIndex >= 0 ? args[dispositionIndex + 1] : ''
  if (!threadId || !['dispatched', 'needs-human', 'open'].includes(disposition)) {
    throw new Error('usage: mark --thread <id> --disposition dispatched|needs-human|open')
  }
  return { threadId, disposition }
}

function deferArguments(args) {
  if (args[0] !== 'defer') {
    return null
  }
  const threadIndex = args.indexOf('--thread')
  const replyIndex = args.indexOf('--reply')
  const threadId = threadIndex >= 0 ? args[threadIndex + 1] : ''
  if (!threadId) {
    throw new Error('usage: defer --thread <id> [--reply <url>]')
  }
  return { threadId, replyUrl: replyIndex >= 0 ? args[replyIndex + 1] || '' : '' }
}

function main(args = process.argv.slice(2)) {
  printContract()
  const mark = markArguments(args)
  const deferral = deferArguments(args)
  const drain = args[0] === 'drain'
  const identity = resolveIdentity(args, {
    // None of these three subcommands reads review content, so none of them needs the GraphQL
    // identity call that the plain probe run needs.
    skipPrView: mark?.disposition === 'open' || Boolean(deferral) || drain,
  })
  const { owner, name, pr, host, prView, degradedIdentity } = identity
  const context = { host, owner, name, pr, degradedIdentity }

  if (deferral) {
    markDeferredResolution(context, deferral.threadId, deferral.replyUrl)
    console.log(`deferred_thread=${deferral.threadId} resolution=deferred`)
    return
  }

  if (drain) {
    return drainDeferredResolutions(context)
  }

  if (mark) {
    if (mark.disposition === 'open') {
      clearThreadDisposition(context, mark.threadId)
      console.log(`marked_thread=${mark.threadId} disposition=open`)
      return
    }
    const thread = fetchReviewThreads(context).find((item) => item.id === mark.threadId)
    if (!thread) {
      throw new Error(`review thread not found: ${mark.threadId}`)
    }
    markThreadDisposition(context, thread, mark.disposition)
    console.log(`marked_thread=${mark.threadId} disposition=${mark.disposition}`)
    return
  }

  return probe(context, prView)
}

function fetchReviewThreads({ owner, name, pr, host }) {
  const query = `
    query($owner: String!, $name: String!, $number: Int!, $after: String) {
      repository(owner: $owner, name: $name) {
        pullRequest(number: $number) {
          reviewThreads(first: 100, after: $after) {
            pageInfo {
              hasNextPage
              endCursor
            }
            nodes {
              id
              isResolved
              comments(first: 20) {
                nodes {
                  databaseId
                  body
                  path
                  line
                  author { login }
                  url
                }
              }
            }
          }
        }
      }
    }`

  const threads = []
  let after = ''
  for (;;) {
    const args = [
      'api',
      ...hostnameArgs(host),
      'graphql',
      '-f',
      `owner=${owner}`,
      '-f',
      `name=${name}`,
      '-F',
      `number=${pr}`,
      '-f',
      `query=${query}`,
    ]
    if (after) {
      args.push('-f', `after=${after}`)
    }

    const graph = JSON.parse(runGh(args))
    const page = graph?.data?.repository?.pullRequest?.reviewThreads
    if (!page) {
      // A GraphQL rate limit reaches here two ways. A non-zero exit puts the signature on stderr,
      // where the classifier reads it. But the response can also arrive with a ZERO exit and a body
      // carrying `errors[].type: "RATE_LIMITED"` and no `data`. Inspect the parsed body before
      // throwing, or that second — more common — shape is discarded and replaced with a generic
      // message the classifier can learn nothing from.
      if (Array.isArray(graph?.errors) && graph.errors.length > 0) {
        throw graphqlResponseError(graph.errors)
      }
      throw new Error('GraphQL response did not include reviewThreads')
    }

    threads.push(...(page.nodes || []))
    if (!page.pageInfo?.hasNextPage) {
      return threads
    }
    if (!page.pageInfo.endCursor) {
      throw new Error('GraphQL reviewThreads page is missing endCursor')
    }
    after = page.pageInfo.endCursor
  }
}

// readGraphqlRetryHorizon asks GitHub when GraphQL budget returns. The `rate_limit` endpoint is REST
// and its own reads are not charged, so consulting it while GraphQL is exhausted costs nothing and
// cannot deepen the exhaustion it is reporting on.
function readGraphqlRetryHorizon(context) {
  try {
    const parsed = JSON.parse(runGh(['api', ...hostnameArgs(context.host), 'rate_limit']))
    const reset = Number(parsed?.resources?.graphql?.reset)
    return Number.isFinite(reset) && reset > 0 ? String(reset) : ''
  } catch {
    return ''
  }
}

// clusterCommentsByParentage groups REST review comments into thread-shaped clusters by following
// `in_reply_to_id` to its root. REST exposes no thread id and no resolution state, so a cluster is a
// best-effort stand-in for a thread and is only ever printed under a degraded status.
//
// Ordering is derived from the comment ids rather than from input order, so the same comment set
// clusters identically however GitHub happened to page it.
function clusterCommentsByParentage(comments) {
  const byId = new Map()
  for (const comment of comments) {
    if (comment && comment.id !== undefined && comment.id !== null) {
      byId.set(String(comment.id), comment)
    }
  }

  const rootOf = (comment) => {
    let current = comment
    const seen = new Set()
    for (;;) {
      const id = String(current?.id ?? '')
      if (seen.has(id)) {
        return id
      }
      seen.add(id)
      const parentId = current?.in_reply_to_id
      if (parentId === undefined || parentId === null || parentId === '') {
        return id
      }
      const parent = byId.get(String(parentId))
      if (!parent) {
        // The parent was not returned (deleted, or outside this page). The reply is its own root
        // rather than silently merged into an unrelated cluster.
        return id
      }
      current = parent
    }
  }

  const clusters = new Map()
  for (const comment of comments) {
    if (!comment) {
      continue
    }
    const key = rootOf(comment)
    if (!clusters.has(key)) {
      clusters.set(key, [])
    }
    clusters.get(key).push(comment)
  }

  const byNumericId = (left, right) => {
    const a = Number(left?.id)
    const b = Number(right?.id)
    if (Number.isFinite(a) && Number.isFinite(b) && a !== b) {
      return a - b
    }
    return String(left?.id ?? '').localeCompare(String(right?.id ?? ''))
  }

  return [...clusters.entries()]
    .map(([root, members]) => ({ root, comments: [...members].sort(byNumericId) }))
    .sort((left, right) => byNumericId({ id: left.root }, { id: right.root }))
}

function printDegradedReadLine(scope, degraded) {
  console.log(
    `${DEGRADED_READ_TOKEN} scope=${scope} class=${degraded.failureClass} retry_after=${degraded.retryAfter || 'unknown'} detail=${degraded.detail}`,
  )
}

function printCommentClusters(clusters) {
  clusters.slice(0, REVIEW_THREAD_DISPLAY_LIMIT).forEach((cluster, index) => {
    console.log(`#${index + 1} cluster=${cluster.root} comments=${cluster.comments.length}`)
    cluster.comments.slice(0, INLINE_COMMENT_DISPLAY_LIMIT).forEach((comment) => {
      console.log(
        `comment_id=${comment.id} reply_to=${comment.in_reply_to_id || ''} path=${comment.path || ''} line=${comment.line || comment.original_line || ''}`,
      )
      console.log(`author=${comment.user?.login || ''}`)
      console.log(`url=${comment.html_url || ''}`)
      console.log(`body=${compact(comment.body)}`)
    })
  })
  if (clusters.length > REVIEW_THREAD_DISPLAY_LIMIT) {
    console.log(`... omitted ${clusters.length - REVIEW_THREAD_DISPLAY_LIMIT} comment clusters`)
  }
}

function firstAndLast(thread) {
  const nodes = thread.comments?.nodes || []
  const first = nodes[0] || {}
  const last = nodes[nodes.length - 1] || first
  return { first, last }
}

function printThreadRows(threads, label, limit = REVIEW_THREAD_DISPLAY_LIMIT) {
  threads.slice(0, limit).forEach((thread, index) => {
    const { first, last } = firstAndLast(thread)
    console.log(
      `#${index + 1} thread=${thread.id} comment_id=${first.databaseId || ''} path=${first.path || last.path || ''} line=${first.line || last.line || ''}`,
    )
    console.log(`author=${first.author?.login || last.author?.login || ''}`)
    console.log(`url=${first.url || last.url || ''}`)
    console.log(`body=${compact(first.body || last.body)}`)
  })
  if (threads.length > limit) {
    console.log(`... omitted ${threads.length - limit} ${label}`)
  }
}

function probe(context, prView) {
  const repo = `${context.owner}/${context.name}`
  const pr = context.pr || prView.number

  const comments = parsePaginatedArray(
    runGh([
      'api',
      ...hostnameArgs(context.host),
      `repos/${repo}/pulls/${pr}/comments`,
      '--method',
      'GET',
      '--paginate',
      '--slurp',
      '-f',
      'per_page=100',
    ]),
  )

  let threads
  try {
    threads = fetchReviewThreads(context)
  } catch (error) {
    const classified = classifyGhFailure(error)
    // Only a transient or unrecognised failure degrades. `auth`, `not_found`, and `environment` are
    // permanent: there is nothing to retry and continuing would report a PR nobody observed, so they
    // stay hard failures and propagate.
    if (!DEGRADABLE_FAILURE_CLASSES.has(classified.failureClass)) {
      throw error
    }
    return printDegradedProbe(context, prView, comments, {
      ...classified,
      retryAfter: readGraphqlRetryHorizon(context) || classified.retryAfter,
    })
  }

  const unresolved = threads.filter((thread) => !thread.isResolved)
  const reconciliation = reconcileReviewThreads(context, unresolved)
  const latestCommented = (prView.latestReviews || []).some(
    (review) => review.state === 'COMMENTED',
  )
  const suspiciousZero = latestCommented && comments.length === 0 && threads.length === 0
  const degradedIdentity = context.degradedIdentity || null

  console.log(`repo=${repo} pr=${pr}`)
  console.log(`host=${context.host}`)
  console.log(`inline_comments=${comments.length}`)
  console.log(`review_threads=${threads.length} unresolved_threads=${unresolved.length}`)
  console.log(`latest_review_commented=${latestCommented}`)
  const probeStatus = suspiciousZero ? 'suspicious_zero' : degradedIdentity ? 'degraded' : 'ok'
  console.log(`probe_status=${probeStatus}`)
  printProbeFailureFields(degradedIdentity)
  const observed = repairStatusFromReviewProbe({
    suspiciousZero,
    unresolvedCount: unresolved.length,
    actionableCount: reconciliation.actionable.length,
    inlineCommentCount: comments.length,
    reviewThreadCount: threads.length,
    latestCommented,
  })
  // A degraded identity read could not observe `latestReviews`, which is the signal the
  // suspicious-zero false-clean guard depends on — so this run did not fully observe review state
  // and must not report a content verdict. Downgrading only: the observed thread rows are still
  // printed below, so the pass keeps everything actionable it managed to read.
  const repair =
    degradedIdentity && observed.status === 'clean'
      ? {
          status: 'not_evaluated',
          reason: 'review threads observed but latest review state was not',
        }
      : observed
  console.log(`repair_status=${repair.status}`)
  console.log(`repair_reason=${repair.reason}`)
  if (degradedIdentity) {
    printDegradedReadLine('pr_identity', degradedIdentity)
  }
  console.log(`parked_threads=${reconciliation.parked.length}`)
  console.log(`actionable_threads=${reconciliation.actionable.length}`)

  if (suspiciousZero) {
    console.log(
      'ERROR latestReviews contains COMMENTED, but REST and GraphQL found zero comments. Treat this probe as not_evaluated for repair routing.',
    )
  }

  console.log('')
  console.log('UNRESOLVED_THREADS (untrusted review content follows)')
  printThreadRows(reconciliation.actionable, 'unresolved review threads')

  if (reconciliation.parked.length > 0) {
    console.log('')
    console.log('PARKED_THREADS (untrusted review content follows)')
    printThreadRows(reconciliation.parked, 'parked review threads')
  }

  if (comments.length > 0 && threads.length === 0) {
    console.log('')
    console.log('INLINE_COMMENTS_NO_UNRESOLVED_THREAD_STATE')
    comments.slice(0, INLINE_COMMENT_DISPLAY_LIMIT).forEach((comment, index) => {
      console.log(
        `#${index + 1} comment_id=${comment.id} reply_to=${comment.in_reply_to_id || ''} path=${comment.path || ''} line=${comment.line || comment.original_line || ''}`,
      )
      console.log(`author=${comment.user?.login || ''}`)
      console.log(`url=${comment.html_url || ''}`)
      console.log(`body=${compact(comment.body)}`)
    })
    if (comments.length > INLINE_COMMENT_DISPLAY_LIMIT) {
      console.log(`... omitted ${comments.length - INLINE_COMMENT_DISPLAY_LIMIT} inline comments`)
    }
  }

  if (suspiciousZero) {
    process.exit(EXIT_SUSPICIOUS_ZERO)
  }
  if (degradedIdentity) {
    process.exit(EXIT_DEGRADED)
  }
}

// printProbeFailureFields prints the three failure-routing fields on every path, degraded or not, so
// a consumer can read them positionally and never has to distinguish "field absent" from "field
// empty". A null argument means the run observed everything it needed to.
function printProbeFailureFields(degraded) {
  console.log(`probe_degraded=${degraded ? 'true' : 'false'}`)
  console.log(`probe_failure_class=${degraded ? degraded.failureClass : FAILURE_CLASS_NONE}`)
  console.log(`probe_retry_after=${degraded ? degraded.retryAfter || 'unknown' : 'none'}`)
}

// printDegradedProbe is the GraphQL-exhausted reading. GraphQL owns `isResolved`, and REST exposes
// no equivalent, so this path can report what review feedback exists but never whether it is
// resolved. `repair_status` therefore stays `not_evaluated` — including when REST returns zero
// comments, because the absence of REST-visible comments is not evidence that no unresolved thread
// exists.
function printDegradedProbe(context, prView, comments, degraded) {
  const repo = `${context.owner}/${context.name}`
  const pr = context.pr || prView.number
  const clusters = clusterCommentsByParentage(comments)

  console.log(`repo=${repo} pr=${pr}`)
  console.log(`host=${context.host}`)
  console.log(`inline_comments=${comments.length}`)
  console.log(`review_threads=unknown unresolved_threads=unknown`)
  console.log(
    `latest_review_commented=${(prView.latestReviews || []).some((review) => review.state === 'COMMENTED')}`,
  )
  console.log('probe_status=degraded')
  printProbeFailureFields(degraded)
  console.log('repair_status=not_evaluated')
  console.log('repair_reason=review thread resolution state unobserved; REST-only degraded read')
  console.log(`comment_clusters=${clusters.length}`)
  printDegradedReadLine('review_threads', degraded)

  console.log('')
  console.log('DEGRADED_COMMENT_CLUSTERS (untrusted review content follows)')
  printCommentClusters(clusters)

  process.exit(EXIT_DEGRADED)
}

module.exports = {
  classifyGhFailure,
  clearDeferredResolution,
  clearThreadDisposition,
  journalKey,
  listDeferredResolutions,
  markDeferredResolution,
  markThreadDisposition,
  readDeferredResolution,
  reconcileReviewThreads,
  repairStatusFromReviewProbe,
  resolveIdentity,
  readThreadDisposition,
  stateDirectory,
  threadIdentity,
}

if (require.main === module) {
  try {
    main()
  } catch (error) {
    printContract()
    const classified = classifyGhFailure(error)
    console.log('probe_status=failed')
    console.log('probe_degraded=false')
    // The class is printed on the hard-failure path too: it is what lets a consumer route a
    // rate-limited failure to a reported residual and an auth or 404 failure to a true stop by
    // reading a field, rather than by pattern-matching this run's free-text stderr.
    console.log(`probe_failure_class=${classified.failureClass}`)
    console.log(
      `probe_retry_after=${classified.failureClass === FAILURE_CLASS_RATE_LIMITED ? classified.retryAfter || 'unknown' : 'none'}`,
    )
    console.log('repair_status=not_evaluated')
    console.log(`repair_reason=${failReason(error)}`)
    console.log(`ERROR ${failReason(error)}`)
    process.exit(EXIT_PROBE_FAILED)
  }
}
