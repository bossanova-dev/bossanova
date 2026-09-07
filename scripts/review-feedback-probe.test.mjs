import assert from 'node:assert/strict'
import {
  chmodSync,
  lstatSync,
  mkdtempSync,
  mkdirSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { createRequire } from 'node:module'
import { spawnSync } from 'node:child_process'

const require = createRequire(import.meta.url)
const probe = require('../services/boss/internal/skillinstall/skills/boss-repair/scripts/review-feedback-probe.js')

function withStateRoot(fn) {
  const root = mkdtempSync(path.join(os.tmpdir(), 'review-feedback-probe-test-'))
  try {
    return fn(root)
  } finally {
    rmSync(root, { recursive: true, force: true })
  }
}

function context(root, overrides = {}) {
  return {
    stateRoot: root,
    host: 'github.com',
    owner: 'octo',
    name: 'repo',
    pr: 42,
    ...overrides,
  }
}

function thread(id, commentId, login = 'reviewer') {
  return {
    id,
    comments: { nodes: [{ databaseId: commentId, author: { login } }] },
  }
}

function writeExecutable(file, body) {
  writeFileSync(file, body)
  chmodSync(file, 0o755)
}

function runProbe(args, { ghBody, env = {} } = {}) {
  return withStateRoot((root) => {
    const bin = path.join(root, 'bin')
    mkdirSync(bin)
    writeExecutable(
      path.join(bin, 'gh'),
      ghBody ||
        `#!/bin/sh
echo 'unexpected gh invocation' >&2
exit 99
`,
    )
    const script = path.resolve(
      'services/boss/internal/skillinstall/skills/boss-repair/scripts/review-feedback-probe.js',
    )
    return spawnSync(process.execPath, [script, ...args], {
      encoding: 'utf8',
      env: {
        ...process.env,
        ...env,
        BOSS_REPAIR_STATE_DIR: root,
        PATH: `${bin}:${process.env.PATH}`,
      },
    })
  })
}

test('repairStatusFromReviewProbe preserves old branches and adds parked', () => {
  assert.deepEqual(
    probe.repairStatusFromReviewProbe({ suspiciousZero: true, unresolvedCount: 3 }),
    { status: 'unknown', reason: 'commented review but no comments found' },
  )
  assert.deepEqual(probe.repairStatusFromReviewProbe({ unresolvedCount: 2, actionableCount: 1 }), {
    status: 'needs_repair',
    reason: 'unresolved review threads',
  })
  assert.deepEqual(probe.repairStatusFromReviewProbe({ unresolvedCount: 2, actionableCount: 0 }), {
    status: 'parked',
    reason: 'unresolved review threads are parked',
  })
  assert.deepEqual(
    probe.repairStatusFromReviewProbe({ reviewThreadCount: 0, inlineCommentCount: 1 }),
    { status: 'unknown', reason: 'inline comments without review thread state' },
  )
  assert.deepEqual(probe.repairStatusFromReviewProbe({}), {
    status: 'clean',
    reason: 'no unresolved review threads',
  })
})

test('classifyGhFailure names every rate-limit signature transient', () => {
  const cases = [
    { stderr: 'gh: HTTP 429 too many requests' },
    { stderr: 'gh: HTTP 403: API rate limit exceeded for user ID 1234' },
    { stderr: 'You have exceeded a secondary rate limit. Please wait a few minutes.' },
    { stderr: 'gh: Your token has exceeded a secondary rate limit' },
    { graphqlErrors: [{ type: 'RATE_LIMITED', message: 'API rate limit exceeded' }] },
  ]
  for (const error of cases) {
    assert.equal(probe.classifyGhFailure(error).failureClass, 'rate_limited', JSON.stringify(error))
  }
})

test('classifyGhFailure names auth, not-found, and environment failures permanent', () => {
  assert.equal(
    probe.classifyGhFailure({ stderr: 'gh: HTTP 401 Unauthorized' }).failureClass,
    'auth',
  )
  assert.equal(probe.classifyGhFailure({ stderr: 'gh: Bad credentials' }).failureClass, 'auth')
  assert.equal(
    probe.classifyGhFailure({ graphqlErrors: [{ type: 'UNAUTHORIZED' }] }).failureClass,
    'auth',
  )
  assert.equal(
    probe.classifyGhFailure({ stderr: 'gh: HTTP 404 Not Found' }).failureClass,
    'not_found',
  )
  assert.equal(
    probe.classifyGhFailure({
      stderr: 'Could not resolve to a Repository with the name octo/repo.',
    }).failureClass,
    'not_found',
  )
  assert.equal(
    probe.classifyGhFailure({ code: 'ENOENT', message: 'spawnSync gh ENOENT' }).failureClass,
    'environment',
  )
  assert.equal(
    probe.classifyGhFailure({
      stderr: 'fatal: not a git repository (or any of the parent directories)',
    }).failureClass,
    'environment',
  )
})

test('classifyGhFailure defaults unrecognised failures to the conservative class', () => {
  assert.equal(probe.classifyGhFailure({ stderr: '' }).failureClass, 'other')
  assert.equal(probe.classifyGhFailure({}).failureClass, 'other')
  assert.equal(probe.classifyGhFailure(new Error('something unexpected')).failureClass, 'other')
  assert.equal(
    probe.classifyGhFailure({ graphqlErrors: [{ type: 'INTERNAL' }] }).failureClass,
    'other',
  )
  // A plain 403 with no rate-limit wording is not evidence of a quota reset, so it must not be
  // classified transient — but it still falls to the conservative default rather than to a class
  // that could reach a clean verdict.
  assert.equal(probe.classifyGhFailure({ stderr: 'gh: HTTP 403 Forbidden' }).failureClass, 'other')
})

test('classifyGhFailure classifies structurally before it reads stderr text', () => {
  // A retryable failure whose wording reads permanent: the structured type must win, or the
  // transient-misclassified-as-permanent symptom survives the classifier.
  const classified = probe.classifyGhFailure({
    graphqlErrors: [{ type: 'RATE_LIMITED', message: 'API rate limit exceeded' }],
    stderr: 'gh: Bad credentials',
  })
  assert.equal(classified.failureClass, 'rate_limited')
  // The HTTP status outranks the text too.
  assert.equal(
    probe.classifyGhFailure({ stderr: 'gh: HTTP 429\nnot a git repository' }).failureClass,
    'rate_limited',
  )
})

test('classifyGhFailure redacts credential-shaped stderr in its detail', () => {
  const classified = probe.classifyGhFailure({
    stderr: 'gh: HTTP 401 Bad credentials for ghs_secret123',
  })
  assert.equal(classified.failureClass, 'auth')
  assert.match(classified.detail, /\[redacted\]/)
  assert.doesNotMatch(classified.detail, /ghs_secret123/)
})

test('classifyGhFailure reports a retry horizon when the failure carries one', () => {
  assert.equal(
    probe.classifyGhFailure({ stderr: 'HTTP 429\nx-ratelimit-reset: 1788750000' }).retryAfter,
    '1788750000',
  )
  assert.equal(probe.classifyGhFailure({ stderr: 'HTTP 429\nRetry-After: 60' }).retryAfter, '60')
  assert.equal(probe.classifyGhFailure({ stderr: 'HTTP 429' }).retryAfter, '')
})

test('deferred resolutions round-trip through the state directory', () => {
  withStateRoot((root) => {
    const ctx = context(root)
    assert.equal(probe.readDeferredResolution(ctx, 'thread-1'), null)
    assert.deepEqual(probe.listDeferredResolutions(ctx), [])

    probe.markDeferredResolution(ctx, 'thread-1', 'https://example.test/reply/1')
    const record = probe.readDeferredResolution(ctx, 'thread-1')
    assert.equal(record.threadId, 'thread-1')
    assert.equal(record.repliedUrl, 'https://example.test/reply/1')
    assert.equal(probe.listDeferredResolutions(ctx).length, 1)

    // R10: the record must carry nothing a drain could re-submit as a reply.
    assert.equal(record.body, undefined)
    assert.equal(record.endpoint, undefined)
    assert.equal(record.replyEndpoint, undefined)
    assert.ok(!JSON.stringify(record).includes('/replies'))

    probe.clearDeferredResolution(ctx, 'thread-1')
    assert.equal(probe.readDeferredResolution(ctx, 'thread-1'), null)
    assert.deepEqual(probe.listDeferredResolutions(ctx), [])
  })
})

test('a deferred record coexists with a disposition record and does not change partitioning', () => {
  withStateRoot((root) => {
    const ctx = context(root)
    const item = thread('thread-1', 10)
    probe.markThreadDisposition(ctx, item, 'needs-human')
    const before = probe.reconcileReviewThreads(ctx, [item])

    probe.markDeferredResolution(ctx, 'thread-1', 'https://example.test/reply/1')

    // Both records exist, keyed independently, and neither overwrote the other.
    assert.equal(probe.readThreadDisposition(ctx, 'thread-1').disposition, 'needs-human')
    assert.equal(probe.readDeferredResolution(ctx, 'thread-1').threadId, 'thread-1')
    // The disposition journal partitions identically before and after the deferred record exists.
    assert.deepEqual(probe.reconcileReviewThreads(ctx, [item]), before)
    assert.deepEqual(probe.reconcileReviewThreads(ctx, [item]), { actionable: [], parked: [item] })
    // Listing deferred records must not pick up the disposition record.
    assert.equal(probe.listDeferredResolutions(ctx).length, 1)
  })
})

test('deferred records fail closed on symlinked and non-owned state roots', () => {
  withStateRoot((root) => {
    const target = path.join(root, 'target')
    const linked = path.join(root, 'linked')
    mkdirSync(target)
    symlinkSync(target, linked)
    assert.throws(() => probe.markDeferredResolution(context(linked), 'thread-1'), /symlink/)
    assert.throws(() => probe.readDeferredResolution(context(linked), 'thread-1'), /symlink/)
    assert.throws(() => probe.listDeferredResolutions(context(linked)), /symlink/)
  })

  withStateRoot((root) => {
    if (typeof process.getuid !== 'function') return
    const original = process.getuid
    process.getuid = () => original() + 1
    try {
      assert.throws(() => probe.markDeferredResolution(context(root), 'thread-1'), /non-owned/)
      assert.throws(() => probe.listDeferredResolutions(context(root)), /non-owned/)
    } finally {
      process.getuid = original
    }
  })
})

test('needs-human disposition parks unchanged thread and reactivates changed identity', () => {
  withStateRoot((root) => {
    const ctx = context(root)
    const initial = thread('thread-1', 10)
    probe.markThreadDisposition(ctx, initial, 'needs-human')

    assert.deepEqual(probe.reconcileReviewThreads(ctx, [initial]), {
      actionable: [],
      parked: [initial],
    })

    const replied = thread('thread-1', 11)
    assert.deepEqual(probe.reconcileReviewThreads(ctx, [replied]), {
      actionable: [replied],
      parked: [],
    })
    assert.equal(probe.readThreadDisposition(ctx, 'thread-1'), null)
  })
})

test('dispatched stays actionable and open clears the journal record', () => {
  withStateRoot((root) => {
    const ctx = context(root)
    const item = thread('thread-1', 10)
    probe.markThreadDisposition(ctx, item, 'dispatched')
    assert.deepEqual(probe.reconcileReviewThreads(ctx, [item]), {
      actionable: [item],
      parked: [],
    })
    probe.clearThreadDisposition(ctx, item.id)
    assert.equal(probe.readThreadDisposition(ctx, item.id), null)
  })
})

test('journal is isolated by host and pull request and never uses the worktree', () => {
  withStateRoot((root) => {
    const item = thread('thread-1', 10)
    const first = context(root)
    const second = context(root, { pr: 43 })
    const third = context(root, { host: 'github.example.test' })
    probe.markThreadDisposition(first, item, 'needs-human')

    assert.equal(probe.readThreadDisposition(second, item.id), null)
    assert.equal(probe.readThreadDisposition(third, item.id), null)
    const stateDir = probe.stateDirectory(first)
    assert.equal(path.dirname(stateDir), root)
    assert.equal(lstatSync(stateDir).isDirectory(), true)
  })
})

test('symlinked state roots fail closed', () => {
  withStateRoot((root) => {
    const target = path.join(root, 'target')
    const linked = path.join(root, 'linked')
    mkdirSync(target)
    symlinkSync(target, linked)
    assert.throws(() => probe.stateDirectory(context(linked)), /symlink/)
  })
})

test('non-owned state roots fail closed', () => {
  withStateRoot((root) => {
    if (typeof process.getuid !== 'function') return
    const original = process.getuid
    process.getuid = () => original() + 1
    try {
      assert.throws(() => probe.stateDirectory(context(root)), /non-owned/)
    } finally {
      process.getuid = original
    }
  })
})

test('mark CLI records and opens a disposition without network access', () => {
  withStateRoot((root) => {
    const bin = path.join(root, 'bin')
    mkdirSync(bin)
    const gh = path.join(bin, 'gh')
    writeFileSync(
      gh,
      `#!/bin/sh
if [ "$1" = "pr" ]; then echo '{"number":42,"latestReviews":[],"url":"https://github.com/octo/repo/pull/42"}'; exit 0; fi
if [ "$1" = "repo" ]; then echo '{"owner":{"login":"octo"},"name":"repo"}'; exit 0; fi
if [ "$1" = "api" ]; then echo '{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"thread-1","isResolved":false,"comments":{"nodes":[{"databaseId":10,"author":{"login":"reviewer"}}]}}]}}}}}'; exit 0; fi
exit 1
`,
    )
    chmodSync(gh, 0o755)
    const script = path.resolve(
      'services/boss/internal/skillinstall/skills/boss-repair/scripts/review-feedback-probe.js',
    )
    const env = { ...process.env, BOSS_REPAIR_STATE_DIR: root, PATH: `${bin}:${process.env.PATH}` }
    const mark = spawnSync(
      process.execPath,
      [script, 'mark', '--thread', 'thread-1', '--disposition', 'needs-human'],
      { encoding: 'utf8', env },
    )
    assert.equal(mark.status, 0, mark.stderr)
    assert.match(mark.stdout, /marked_thread=thread-1 disposition=needs-human/)
    const open = spawnSync(
      process.execPath,
      [script, 'mark', '--thread', 'thread-1', '--disposition', 'open'],
      { encoding: 'utf8', env },
    )
    assert.equal(open.status, 0, open.stderr)
    assert.match(open.stdout, /marked_thread=thread-1 disposition=open/)
  })
})

test('non-zero gh is not_evaluated and redacts credential-shaped stderr', () => {
  const result = runProbe([], {
    ghBody: `#!/bin/sh
echo 'fatal auth ghs_secret123 failed' >&2
exit 1
`,
  })
  assert.equal(result.status, 1)
  assert.match(result.stdout, /probe_contract=review-feedback-probe\/v3/)
  assert.match(result.stdout, /probe_status=failed/)
  assert.match(result.stdout, /repair_status=not_evaluated/)
  assert.doesNotMatch(result.stdout, /repair_status=unknown/)
  assert.match(result.stdout, /fatal auth \[redacted\] failed/)
  assert.doesNotMatch(result.stdout, /ghs_secret123/)
  assert.doesNotMatch(result.stdout, /UNRESOLVED_THREADS/)
  // The hard-failure path prints the routing fields too, so a consumer never has to guess whether
  // an unclassified failure was transient.
  assert.match(result.stdout, /probe_degraded=false/)
  assert.match(result.stdout, /probe_failure_class=other/)
  assert.match(result.stdout, /probe_retry_after=none/)
})

// degradedGh serves the REST comment read and the REST rate_limit read, and rate-limits every
// GraphQL call, which is the exact asymmetry the ticket records: separate quotas, REST still funded.
function degradedGh({ comments = '[]', graphqlBody = '', graphqlExit = 1 } = {}) {
  return `#!/bin/sh
if [ "$1" = "pr" ]; then echo '{"number":42,"latestReviews":[],"url":"https://github.com/octo/repo/pull/42"}'; exit 0; fi
if [ "$1" = "api" ] && [ "$2" = "repos/octo/repo/pulls/42/comments" ]; then echo '${comments}'; exit 0; fi
if [ "$1" = "api" ] && [ "$2" = "rate_limit" ]; then echo '{"resources":{"graphql":{"reset":1788750000}}}'; exit 0; fi
if [ "$1" = "api" ] && [ "$2" = "graphql" ]; then
  ${graphqlBody ? `echo '${graphqlBody}'` : `echo 'gh: HTTP 429 API rate limit exceeded' >&2`}
  exit ${graphqlExit}
fi
echo "unexpected $*" >&2
exit 99
`
}

const DEGRADED_ARGS = ['--repo', 'octo/repo', '--pr', '42', '--host', 'github.com']

test('a GraphQL-rate-limited probe prints REST comment clusters under a degraded status', () => {
  const comments = JSON.stringify([
    { id: 20, in_reply_to_id: 10, body: 'and another', user: { login: 'reviewer' } },
    {
      id: 10,
      body: 'please inspect',
      path: 'file.go',
      line: 7,
      user: { login: 'reviewer' },
      html_url: 'https://example.test/c10',
    },
    { id: 30, body: 'separate finding', path: 'other.go', line: 3, user: { login: 'second' } },
  ])
  const result = runProbe(DEGRADED_ARGS, { ghBody: degradedGh({ comments }) })

  assert.equal(result.status, 3, result.stdout)
  assert.match(result.stdout, /probe_contract=review-feedback-probe\/v3/)
  assert.match(result.stdout, /probe_status=degraded/)
  assert.doesNotMatch(result.stdout, /probe_status=ok/)
  assert.doesNotMatch(result.stdout, /probe_status=failed/)
  assert.match(result.stdout, /repair_status=not_evaluated/)
  assert.doesNotMatch(result.stdout, /repair_status=clean/)
  assert.match(result.stdout, /probe_degraded=true/)
  assert.match(result.stdout, /probe_failure_class=rate_limited/)
  assert.match(result.stdout, /probe_retry_after=1788750000/)
  assert.match(result.stdout, /^DEGRADED_READ scope=review_threads class=rate_limited /m)
  assert.match(result.stdout, /DEGRADED_COMMENT_CLUSTERS \(untrusted review content follows\)/)
  assert.match(result.stdout, /comment_clusters=2/)
  assert.match(result.stdout, /#1 cluster=10 comments=2/)
  assert.match(result.stdout, /#2 cluster=30 comments=1/)
  assert.match(result.stdout, /body=please inspect/)
  assert.match(result.stdout, /body=separate finding/)
})

test('a zero-exit GraphQL body carrying RATE_LIMITED degrades rather than reporting a generic error', () => {
  const result = runProbe(DEGRADED_ARGS, {
    ghBody: degradedGh({
      graphqlExit: 0,
      graphqlBody: '{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}',
    }),
  })

  assert.equal(result.status, 3, result.stdout)
  assert.match(result.stdout, /probe_status=degraded/)
  assert.match(result.stdout, /probe_failure_class=rate_limited/)
  assert.doesNotMatch(result.stdout, /did not include reviewThreads/)
})

test('a zero-exit GraphQL body with another error type still fails and is not an empty observation', () => {
  const result = runProbe(DEGRADED_ARGS, {
    ghBody: degradedGh({
      graphqlExit: 0,
      graphqlBody: '{"errors":[{"type":"INTERNAL","message":"server blew up"}]}',
    }),
  })

  // `other` is degradable and conservative, so this reports a degraded, unobserved reading — never
  // a successful empty thread list.
  assert.equal(result.status, 3, result.stdout)
  assert.match(result.stdout, /probe_status=degraded/)
  assert.match(result.stdout, /probe_failure_class=other/)
  assert.doesNotMatch(result.stdout, /repair_status=clean/)
  assert.match(result.stdout, /review_threads=unknown unresolved_threads=unknown/)
})

test('degraded mode with zero REST comments still refuses to report clean', () => {
  const result = runProbe(DEGRADED_ARGS, { ghBody: degradedGh({ comments: '[]' }) })

  assert.equal(result.status, 3, result.stdout)
  assert.match(result.stdout, /probe_status=degraded/)
  assert.match(result.stdout, /repair_status=not_evaluated/)
  assert.doesNotMatch(result.stdout, /repair_status=clean/)
  assert.match(result.stdout, /comment_clusters=0/)
  assert.match(result.stdout, /^DEGRADED_READ /m)
})

test('comment clustering is stable under input reordering', () => {
  const members = [
    { id: 10, body: 'root a', user: { login: 'reviewer' } },
    { id: 11, in_reply_to_id: 10, body: 'reply a', user: { login: 'reviewer' } },
    { id: 12, body: 'root b', user: { login: 'reviewer' } },
    { id: 13, in_reply_to_id: 12, body: 'reply b', user: { login: 'reviewer' } },
  ]
  const forward = runProbe(DEGRADED_ARGS, {
    ghBody: degradedGh({ comments: JSON.stringify(members) }),
  })
  const reversed = runProbe(DEGRADED_ARGS, {
    ghBody: degradedGh({ comments: JSON.stringify([...members].reverse()) }),
  })

  assert.equal(forward.status, 3, forward.stdout)
  assert.equal(reversed.status, 3, reversed.stdout)
  const clusterLines = (stdout) => stdout.match(/^#\d+ cluster=\d+ comments=\d+$/gm)
  assert.deepEqual(clusterLines(forward.stdout), [
    '#1 cluster=10 comments=2',
    '#2 cluster=12 comments=2',
  ])
  assert.deepEqual(clusterLines(reversed.stdout), clusterLines(forward.stdout))
})

test('a permanent failure class does not take the degraded path', () => {
  for (const stderr of ['gh: HTTP 401 Bad credentials', 'gh: HTTP 404 Not Found']) {
    const result = runProbe(DEGRADED_ARGS, {
      ghBody: `#!/bin/sh
if [ "$1" = "pr" ]; then echo '{"number":42,"latestReviews":[],"url":"https://github.com/octo/repo/pull/42"}'; exit 0; fi
if [ "$1" = "api" ] && [ "$2" = "repos/octo/repo/pulls/42/comments" ]; then echo '[]'; exit 0; fi
if [ "$1" = "api" ] && [ "$2" = "graphql" ]; then echo '${stderr}' >&2; exit 1; fi
echo "unexpected $*" >&2
exit 99
`,
    })
    assert.equal(result.status, 1, result.stdout)
    assert.match(result.stdout, /probe_status=failed/)
    assert.doesNotMatch(result.stdout, /probe_status=degraded/)
    assert.doesNotMatch(result.stdout, /repair_status=clean/)
    assert.match(result.stdout, /probe_failure_class=(auth|not_found)/)
  }
})

test('a rate limit on the REST comment read is a classified failure, not an empty cluster list', () => {
  const result = runProbe(DEGRADED_ARGS, {
    ghBody: `#!/bin/sh
if [ "$1" = "pr" ]; then echo '{"number":42,"latestReviews":[],"url":"https://github.com/octo/repo/pull/42"}'; exit 0; fi
echo 'gh: HTTP 429 API rate limit exceeded' >&2
exit 1
`,
  })

  assert.equal(result.status, 1, result.stdout)
  assert.match(result.stdout, /probe_status=failed/)
  assert.match(result.stdout, /probe_failure_class=rate_limited/)
  assert.match(result.stdout, /repair_status=not_evaluated/)
  assert.doesNotMatch(result.stdout, /DEGRADED_COMMENT_CLUSTERS/)
  assert.doesNotMatch(result.stdout, /comment_clusters=/)
})

test('a rate-limited identity read degrades instead of stranding, and cannot report clean', () => {
  const result = runProbe(DEGRADED_ARGS, {
    ghBody: `#!/bin/sh
if [ "$1" = "pr" ]; then echo 'gh: HTTP 429 API rate limit exceeded' >&2; exit 1; fi
if [ "$1" = "api" ] && [ "$2" = "repos/octo/repo/pulls/42/comments" ]; then echo '[]'; exit 0; fi
if [ "$1" = "api" ] && [ "$2" = "rate_limit" ]; then echo '{"resources":{"graphql":{"reset":1788750000}}}'; exit 0; fi
if [ "$1" = "api" ] && [ "$2" = "graphql" ]; then echo '{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}}'; exit 0; fi
echo "unexpected $*" >&2
exit 99
`,
  })

  // Review threads WERE observed here, but `latestReviews` was not — and that is the signal the
  // suspicious-zero false-clean guard depends on, so the verdict is downgraded, never upgraded.
  assert.equal(result.status, 3, result.stdout)
  assert.match(result.stdout, /probe_status=degraded/)
  assert.match(result.stdout, /repair_status=not_evaluated/)
  assert.doesNotMatch(result.stdout, /repair_status=clean/)
  assert.match(result.stdout, /probe_failure_class=rate_limited/)
  assert.match(result.stdout, /^DEGRADED_READ scope=pr_identity class=rate_limited /m)
})

test('an implicit-identity rate limit names the GraphQL-free flags rather than only the quota', () => {
  const result = runProbe([], {
    ghBody: `#!/bin/sh
echo 'gh: HTTP 429 API rate limit exceeded' >&2
exit 1
`,
  })

  assert.equal(result.status, 1, result.stdout)
  assert.match(result.stdout, /probe_failure_class=rate_limited/)
  assert.match(result.stdout, /pass --repo OWNER\/REPO --pr PR_NUM/)
})

// withProbeCli keeps ONE state root across several spawned runs, which the drain tests need: they
// defer in one process and drain in the next.
function withProbeCli(fn) {
  return withStateRoot((root) => {
    const bin = path.join(root, 'bin')
    mkdirSync(bin)
    const script = path.resolve(
      'services/boss/internal/skillinstall/skills/boss-repair/scripts/review-feedback-probe.js',
    )
    const run = (args, ghBody) => {
      writeExecutable(path.join(bin, 'gh'), ghBody || '#!/bin/sh\nexit 99\n')
      return spawnSync(process.execPath, [script, ...args], {
        encoding: 'utf8',
        env: {
          ...process.env,
          BOSS_REPAIR_STATE_DIR: root,
          PATH: `${bin}:${process.env.PATH}`,
        },
      })
    }
    return fn(run, root)
  })
}

// GH_DRAIN_MIXED resolves thread-1 and rate-limits thread-2, and fails loudly on ANY reply-posting
// call — the direct pin against a drain double-posting a review comment on quota recovery.
const GH_DRAIN_MIXED = `#!/bin/sh
case " $* " in *"/replies"*) echo 'FORBIDDEN reply call during drain' >&2; exit 42 ;; esac
if [ "$1" = "api" ] && [ "$2" = "graphql" ]; then
  case " $* " in
    *"thread-1"*) echo '{"data":{"resolveReviewThread":{"thread":{"isResolved":true}}}}'; exit 0 ;;
    *"thread-2"*) echo 'gh: HTTP 429 API rate limit exceeded' >&2; exit 1 ;;
  esac
fi
echo "unexpected $*" >&2
exit 99
`

test('drain clears resolved records, keeps rate-limited ones, and never posts a reply', () => {
  withProbeCli((run) => {
    for (const threadId of ['thread-1', 'thread-2']) {
      const deferred = run([
        'defer',
        '--thread',
        threadId,
        '--reply',
        `https://example.test/reply/${threadId}`,
        ...DEGRADED_ARGS,
      ])
      assert.equal(deferred.status, 0, deferred.stderr || deferred.stdout)
      assert.match(deferred.stdout, new RegExp(`deferred_thread=${threadId} resolution=deferred`))
    }

    const drained = run(['drain', ...DEGRADED_ARGS], GH_DRAIN_MIXED)
    assert.equal(drained.status, 3, drained.stdout)
    // The fake exits 42 on any `.../replies` call. Assert on BOTH streams: a reply attempt that
    // failed inside runGh surfaces as the classified detail on stdout rather than on stderr.
    assert.doesNotMatch(drained.stderr, /FORBIDDEN reply call during drain/)
    assert.doesNotMatch(drained.stdout, /FORBIDDEN reply call during drain/)
    assert.match(drained.stdout, /deferred_pending=2/)
    assert.match(drained.stdout, /deferred_resolved=thread-1/)
    assert.match(drained.stdout, /deferred_remaining_thread=thread-2 class=rate_limited/)
    assert.match(drained.stdout, /deferred_resolved_count=1/)
    assert.match(drained.stdout, /deferred_remaining_count=1/)
    assert.match(drained.stdout, /^DEGRADED_READ scope=deferred_resolution class=rate_limited /m)
    assert.match(drained.stdout, /probe_failure_class=rate_limited/)

    // The next pass sees exactly the record whose resolve did not land.
    const second = run(['drain', ...DEGRADED_ARGS], GH_DRAIN_MIXED)
    assert.equal(second.status, 3, second.stdout)
    assert.match(second.stdout, /deferred_pending=1/)
    assert.match(second.stdout, /deferred_remaining_thread=thread-2/)
    assert.doesNotMatch(second.stdout, /deferred_resolved=thread-1/)
  })
})

// GH_DRAIN_SILENT returns a ZERO-exit body with no `errors[]` that nonetheless never reports
// isResolved. That is an UNOBSERVED resolve, not a successful one: the drain must keep the record
// rather than delete the only evidence that a reply landed on a thread nobody resolved.
const GH_DRAIN_SILENT = `#!/bin/sh
case " $* " in *"/replies"*) echo 'FORBIDDEN reply call during drain' >&2; exit 42 ;; esac
if [ "$1" = "api" ] && [ "$2" = "graphql" ]; then
  echo '{"data":{"resolveReviewThread":null}}'
  exit 0
fi
echo "unexpected $*" >&2
exit 99
`

test('drain keeps a record whose resolve returned a zero-exit body that never reported isResolved', () => {
  withProbeCli((run) => {
    const deferred = run([
      'defer',
      '--thread',
      'thread-silent',
      '--reply',
      'https://example.test/reply/thread-silent',
      ...DEGRADED_ARGS,
    ])
    assert.equal(deferred.status, 0, deferred.stderr || deferred.stdout)

    const drained = run(['drain', ...DEGRADED_ARGS], GH_DRAIN_SILENT)
    assert.equal(drained.status, 3, drained.stdout)
    assert.doesNotMatch(drained.stdout, /deferred_resolved=thread-silent/)
    assert.match(drained.stdout, /deferred_resolved_count=0/)
    assert.match(drained.stdout, /deferred_remaining_thread=thread-silent class=other/)
    assert.match(drained.stdout, /deferred_remaining_count=1/)
    assert.match(drained.stdout, /^DEGRADED_READ scope=deferred_resolution class=other /m)
    assert.doesNotMatch(drained.stderr, /FORBIDDEN reply call during drain/)
    assert.doesNotMatch(drained.stdout, /FORBIDDEN reply call during drain/)

    // The record survives to the next pass — the whole point of refusing to clear it.
    const second = run(['drain', ...DEGRADED_ARGS], GH_DRAIN_SILENT)
    assert.match(second.stdout, /deferred_pending=1/)
  })
})

test('draining an empty queue reports zero pending and succeeds', () => {
  withProbeCli((run) => {
    const drained = run(['drain', ...DEGRADED_ARGS], GH_DRAIN_MIXED)
    assert.equal(drained.status, 0, drained.stdout)
    assert.match(drained.stdout, /deferred_pending=0/)
    assert.match(drained.stdout, /deferred_resolved_count=0/)
    assert.match(drained.stdout, /deferred_remaining_count=0/)
    assert.match(drained.stdout, /probe_failure_class=none/)
    assert.doesNotMatch(drained.stdout, /DEGRADED_READ/)
  })
})

test('the undegraded path keeps its pre-existing statuses so the contract bump is additive', () => {
  const result = runProbe(DEGRADED_ARGS, {
    ghBody: `#!/bin/sh
if [ "$1" = "pr" ]; then echo '{"number":42,"latestReviews":[],"url":"https://github.com/octo/repo/pull/42"}'; exit 0; fi
if [ "$1" = "api" ] && [ "$2" = "repos/octo/repo/pulls/42/comments" ]; then echo '[]'; exit 0; fi
if [ "$1" = "api" ] && [ "$2" = "graphql" ]; then echo '{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}}'; exit 0; fi
echo "unexpected $*" >&2
exit 99
`,
  })

  assert.equal(result.status, 0, result.stdout)
  assert.match(result.stdout, /probe_status=ok/)
  assert.match(result.stdout, /repair_status=clean/)
  assert.match(result.stdout, /probe_degraded=false/)
  assert.match(result.stdout, /probe_failure_class=none/)
  assert.match(result.stdout, /probe_retry_after=none/)
  assert.doesNotMatch(result.stdout, /DEGRADED_READ/)
})

test('malformed explicit identity flags fail as probe failures', () => {
  for (const args of [
    ['--repo', 'not a repo', '--pr', '1'],
    ['--repo', 'Owner/Repo', '--pr', '0'],
    ['--repo', 'Owner/Repo', '--pr', '1', '--host', 'https://github.com'],
    ['--repo', 'Owner/Repo'],
    ['--pr', '1'],
  ]) {
    const result = runProbe(args)
    assert.equal(result.status, 1, `${args.join(' ')}\n${result.stdout}`)
    assert.match(result.stdout, /probe_status=failed/)
    assert.match(result.stdout, /repair_status=not_evaluated/)
    assert.doesNotMatch(result.stdout, /probe_status=ok/)
  }
})

test('non-default host qualifies gh pr repo argument and keeps gh api hostname', () => {
  const result = runProbe(
    ['--repo', 'octo/repo', '--pr', '42', '--host', 'github.enterprise.test'],
    {
      ghBody: `#!/bin/sh
printf '%s\\n' "$*" >> "$BOSS_REPAIR_STATE_DIR/gh-args"
if [ "$1" = "pr" ]; then
  test "$2" = "view" || exit 98
  test "$4" = "--repo" || exit 97
  test "$5" = "github.enterprise.test/octo/repo" || exit 96
  case " $* " in *" --hostname "*) exit 95 ;; esac
  echo '{"number":42,"latestReviews":[],"url":"https://github.enterprise.test/octo/repo/pull/42"}'
  exit 0
fi
if [ "$1" = "api" ] && [ "$2" = "--hostname" ] && [ "$3" = "github.enterprise.test" ] && [ "$4" = "repos/octo/repo/pulls/42/comments" ]; then
  echo '[]'
  exit 0
fi
if [ "$1" = "api" ] && [ "$2" = "--hostname" ] && [ "$3" = "github.enterprise.test" ] && [ "$4" = "graphql" ]; then
  echo '{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}}'
  exit 0
fi
echo "unexpected $*" >&2
exit 99
`,
    },
  )
  assert.equal(result.status, 0, result.stderr || result.stdout)
  assert.match(result.stdout, /host=github\.enterprise\.test/)
  assert.match(result.stdout, /probe_status=ok/)
})

test('explicit open mark is network-free and uses case-folded journal key', () => {
  withStateRoot((root) => {
    const first = context(root, { owner: 'Owner', name: 'Repo' })
    const second = context(root, { owner: 'owner', name: 'repo' })
    const otherHost = context(root, { host: 'github.example.test' })
    assert.equal(probe.stateDirectory(first), probe.stateDirectory(second))
    assert.notEqual(probe.stateDirectory(first), probe.stateDirectory(otherHost))

    const bin = path.join(root, 'bin')
    mkdirSync(bin)
    writeExecutable(
      path.join(bin, 'gh'),
      `#!/bin/sh
echo 'gh should not be called' >&2
exit 99
`,
    )
    probe.markThreadDisposition(first, thread('thread-1', 10), 'needs-human')
    const script = path.resolve(
      'services/boss/internal/skillinstall/skills/boss-repair/scripts/review-feedback-probe.js',
    )
    const result = spawnSync(
      process.execPath,
      [
        script,
        'mark',
        '--thread',
        'thread-1',
        '--disposition',
        'open',
        '--repo',
        'owner/repo',
        '--pr',
        '42',
        '--host',
        'github.com',
      ],
      {
        encoding: 'utf8',
        env: {
          ...process.env,
          BOSS_REPAIR_STATE_DIR: root,
          PATH: `${bin}:${process.env.PATH}`,
        },
      },
    )
    assert.equal(result.status, 0, result.stderr || result.stdout)
    assert.match(result.stdout, /marked_thread=thread-1 disposition=open/)
    assert.equal(probe.readThreadDisposition(second, 'thread-1'), null)
  })
})

test('parked probe prints bounded parked accounting and unresolved header', () => {
  withStateRoot((root) => {
    const ctx = context(root)
    probe.markThreadDisposition(ctx, thread('thread-1', 10), 'needs-human')

    const bin = path.join(root, 'bin')
    mkdirSync(bin)
    writeExecutable(
      path.join(bin, 'gh'),
      `#!/bin/sh
if [ "$1" = "pr" ]; then echo '{"number":42,"latestReviews":[],"url":"https://github.com/octo/repo/pull/42"}'; exit 0; fi
if [ "$1" = "api" ] && [ "$2" = "repos/octo/repo/pulls/42/comments" ]; then echo '[]'; exit 0; fi
if [ "$1" = "api" ] && [ "$2" = "graphql" ]; then echo '{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"thread-1","isResolved":false,"comments":{"nodes":[{"databaseId":10,"body":"please inspect","path":"file.go","line":7,"author":{"login":"reviewer"},"url":"https://example.test/thread"}]}}]}}}}}'; exit 0; fi
echo "unexpected $*" >&2
exit 99
`,
    )
    const script = path.resolve(
      'services/boss/internal/skillinstall/skills/boss-repair/scripts/review-feedback-probe.js',
    )
    const result = spawnSync(
      process.execPath,
      [script, '--repo', 'octo/repo', '--pr', '42', '--host', 'github.com'],
      {
        encoding: 'utf8',
        env: {
          ...process.env,
          BOSS_REPAIR_STATE_DIR: root,
          PATH: `${bin}:${process.env.PATH}`,
        },
      },
    )
    assert.equal(result.status, 0, result.stderr || result.stdout)
    assert.match(result.stdout, /probe_status=ok/)
    assert.match(result.stdout, /repair_status=parked/)
    assert.match(result.stdout, /UNRESOLVED_THREADS \(untrusted review content follows\)/)
    assert.match(result.stdout, /PARKED_THREADS \(untrusted review content follows\)/)
    assert.match(result.stdout, /path=file.go line=7/)
    assert.match(result.stdout, /body=please inspect/)
  })
})
