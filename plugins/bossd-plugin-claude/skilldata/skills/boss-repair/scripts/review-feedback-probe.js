#!/usr/bin/env node

const { execFileSync } = require('child_process')
const { createHash } = require('crypto')
const {
  existsSync,
  lstatSync,
  mkdirSync,
  readFileSync,
  renameSync,
  unlinkSync,
  writeFileSync,
} = require('fs')
const os = require('os')
const path = require('path')

const INLINE_COMMENT_DISPLAY_LIMIT = 100
const REVIEW_THREAD_DISPLAY_LIMIT = 100
const DEFAULT_HOST = 'github.com'
const CONTRACT_VERSION = 'review-feedback-probe/v2'
let contractPrinted = false

function printContract() {
  if (!contractPrinted) {
    console.log(`probe_contract=${CONTRACT_VERSION}`)
    contractPrinted = true
  }
}

function runGh(args) {
  const out = execFileSync('gh', args, {
    encoding: 'utf8',
    maxBuffer: 20 * 1024 * 1024,
    stdio: ['ignore', 'pipe', 'pipe'],
  })

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
  }

  const host = resolveHost({ requestedHost: hostFlag })
  const prView = JSON.parse(runGh(['pr', 'view', '--json', 'number,latestReviews,url']))
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

function main(args = process.argv.slice(2)) {
  printContract()
  const mark = markArguments(args)
  const identity = resolveIdentity(args, { skipPrView: mark?.disposition === 'open' })
  const { owner, name, pr, host, prView } = identity
  const context = { host, owner, name, pr }

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

  const threads = fetchReviewThreads(context)
  const unresolved = threads.filter((thread) => !thread.isResolved)
  const reconciliation = reconcileReviewThreads(context, unresolved)
  const latestCommented = (prView.latestReviews || []).some(
    (review) => review.state === 'COMMENTED',
  )
  const suspiciousZero = latestCommented && comments.length === 0 && threads.length === 0

  console.log(`repo=${repo} pr=${pr}`)
  console.log(`host=${context.host}`)
  console.log(`inline_comments=${comments.length}`)
  console.log(`review_threads=${threads.length} unresolved_threads=${unresolved.length}`)
  console.log(`latest_review_commented=${latestCommented}`)
  console.log(`probe_status=${suspiciousZero ? 'suspicious_zero' : 'ok'}`)
  const repair = repairStatusFromReviewProbe({
    suspiciousZero,
    unresolvedCount: unresolved.length,
    actionableCount: reconciliation.actionable.length,
    inlineCommentCount: comments.length,
    reviewThreadCount: threads.length,
    latestCommented,
  })
  console.log(`repair_status=${repair.status}`)
  console.log(`repair_reason=${repair.reason}`)
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
    process.exit(2)
  }
}

module.exports = {
  clearThreadDisposition,
  journalKey,
  markThreadDisposition,
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
    console.log('probe_status=failed')
    console.log('repair_status=not_evaluated')
    console.log(`repair_reason=${failReason(error)}`)
    console.log(`ERROR ${failReason(error)}`)
    process.exit(1)
  }
}
