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

function compact(value, max = 360) {
  return String(value || '')
    .replace(/\s+/g, ' ')
    .trim()
    .slice(0, max)
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

function stateDirectory({ stateRoot, host, owner, name, pr }) {
  const root =
    stateRoot || process.env.BOSS_REPAIR_STATE_DIR || path.join(os.tmpdir(), 'boss-repair-state')
  requireSafeDirectory(root)
  const child = `${host}-${owner}-${name}-${pr}`.replace(/[^A-Za-z0-9._-]/g, '_')
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

function reviewContext(prView, owner, name) {
  const host = new URL(prView.url || 'https://github.com').hostname
  return { host, owner, name, pr: prView.number }
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
  const mark = markArguments(args)
  const prView = JSON.parse(runGh(['pr', 'view', '--json', 'number,latestReviews,url']))
  const repoView = JSON.parse(runGh(['repo', 'view', '--json', 'owner,name']))
  const owner = repoView.owner.login
  const name = repoView.name
  const context = reviewContext(prView, owner, name)

  if (mark) {
    if (mark.disposition === 'open') {
      clearThreadDisposition(context, mark.threadId)
      console.log(`marked_thread=${mark.threadId} disposition=open`)
      return
    }
    const thread = fetchReviewThreads(owner, name, prView.number).find(
      (item) => item.id === mark.threadId,
    )
    if (!thread) {
      throw new Error(`review thread not found: ${mark.threadId}`)
    }
    markThreadDisposition(context, thread, mark.disposition)
    console.log(`marked_thread=${mark.threadId} disposition=${mark.disposition}`)
    return
  }

  return probe(context, prView, owner, name)
}

function fetchReviewThreads(owner, name, pr) {
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

function probe(context, prView, owner, name) {
  const repo = `${owner}/${name}`
  const pr = prView.number

  const comments = parsePaginatedArray(
    runGh([
      'api',
      `repos/${repo}/pulls/${pr}/comments`,
      '--method',
      'GET',
      '--paginate',
      '--slurp',
      '-f',
      'per_page=100',
    ]),
  )

  const threads = fetchReviewThreads(owner, name, pr)
  const unresolved = threads.filter((thread) => !thread.isResolved)
  const reconciliation = reconcileReviewThreads(context, unresolved)
  const latestCommented = (prView.latestReviews || []).some(
    (review) => review.state === 'COMMENTED',
  )
  const suspiciousZero = latestCommented && comments.length === 0 && threads.length === 0

  console.log(`repo=${repo} pr=${pr}`)
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
      'ERROR latestReviews contains COMMENTED, but REST and GraphQL found zero comments. Retry with explicit repo/pr before concluding no feedback exists.',
    )
    process.exit(2)
  }

  if (reconciliation.actionable.length > 0) {
    console.log('')
    console.log('UNRESOLVED_THREADS')
    reconciliation.actionable.forEach((thread, index) => {
      const first = thread.comments.nodes[0] || {}
      const last = thread.comments.nodes[thread.comments.nodes.length - 1] || first
      console.log(
        `#${index + 1} thread=${thread.id} comment_id=${first.databaseId || ''} path=${first.path || last.path || ''} line=${first.line || last.line || ''}`,
      )
      console.log(`author=${first.author?.login || last.author?.login || ''}`)
      console.log(`url=${first.url || last.url || ''}`)
      console.log(`body=${compact(first.body || last.body)}`)
    })
    return
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
}

module.exports = {
  clearThreadDisposition,
  markThreadDisposition,
  reconcileReviewThreads,
  repairStatusFromReviewProbe,
  readThreadDisposition,
  stateDirectory,
  threadIdentity,
}

if (require.main === module) {
  try {
    main()
  } catch (error) {
    console.log('probe_status=failed')
    console.log('repair_status=unknown')
    console.log('repair_reason=review probe failed')
    console.log(`ERROR ${error.message}`)
    process.exit(1)
  }
}
