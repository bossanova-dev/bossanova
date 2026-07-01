#!/usr/bin/env node

const { execFileSync } = require('child_process')

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

function repairStatusFromReviewProbe({
  suspiciousZero,
  unresolvedCount,
  inlineCommentCount,
  reviewThreadCount,
  latestCommented,
}) {
  if (suspiciousZero) {
    return { status: 'unknown', reason: 'commented review but no comments found' }
  }
  if (unresolvedCount > 0) {
    return { status: 'needs_repair', reason: 'unresolved review threads' }
  }
  if (reviewThreadCount === 0 && (inlineCommentCount > 0 || latestCommented)) {
    return { status: 'unknown', reason: 'inline comments without review thread state' }
  }
  return { status: 'clean', reason: 'no unresolved review threads' }
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

function main() {
  const prView = JSON.parse(runGh(['pr', 'view', '--json', 'number,latestReviews']))
  const repoView = JSON.parse(runGh(['repo', 'view', '--json', 'owner,name']))
  const owner = repoView.owner.login
  const name = repoView.name
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
    inlineCommentCount: comments.length,
    reviewThreadCount: threads.length,
    latestCommented,
  })
  console.log(`repair_status=${repair.status}`)
  console.log(`repair_reason=${repair.reason}`)

  if (suspiciousZero) {
    console.log(
      'ERROR latestReviews contains COMMENTED, but REST and GraphQL found zero comments. Retry with explicit repo/pr before concluding no feedback exists.',
    )
    process.exit(2)
  }

  if (unresolved.length > 0) {
    console.log('')
    console.log('UNRESOLVED_THREADS')
    unresolved.forEach((thread, index) => {
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

try {
  main()
} catch (error) {
  console.log('probe_status=failed')
  console.log('repair_status=unknown')
  console.log('repair_reason=review probe failed')
  console.log(`ERROR ${error.message}`)
  process.exit(1)
}
