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
  assert.match(result.stdout, /probe_contract=review-feedback-probe\/v2/)
  assert.match(result.stdout, /probe_status=failed/)
  assert.match(result.stdout, /repair_status=not_evaluated/)
  assert.doesNotMatch(result.stdout, /repair_status=unknown/)
  assert.match(result.stdout, /fatal auth \[redacted\] failed/)
  assert.doesNotMatch(result.stdout, /ghs_secret123/)
  assert.doesNotMatch(result.stdout, /UNRESOLVED_THREADS/)
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
