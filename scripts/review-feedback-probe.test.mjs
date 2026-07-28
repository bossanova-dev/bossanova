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
