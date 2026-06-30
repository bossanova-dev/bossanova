#!/usr/bin/env node

import assert from 'node:assert/strict'
import { test } from 'node:test'

import { isProtectedBranch, validateCommitMessage } from './commit-message-policy.cjs'

test('protected branches allow conventional commits without PR tags', () => {
  for (const branch of ['main', 'staging', 'production']) {
    assert.equal(isProtectedBranch(branch), true)
    assert.deepEqual(validateCommitMessage('fix(api): repair status check', { branch }), {
      valid: true,
    })
  }
})

test('feature branches WITHOUT a resolved PR accept tagless conventional commits', () => {
  // The [#PR] tag cannot exist before the PR is opened (cron/agent commits
  // precede PR creation; bossd injects the tag at finalize). With no PR mapped
  // to the branch, the commit-message hook must not require the tag — that is
  // what let the hook run unconditionally in cron worktrees.
  assert.deepEqual(
    validateCommitMessage('fix(api): repair status check', {
      branch: 'feature/status-check',
      prNumber: null,
    }),
    { valid: true },
  )
})

test('feature branches WITH a resolved PR reject tagless commits', () => {
  const result = validateCommitMessage('fix(api): repair status check', {
    branch: 'feature/status-check',
    prNumber: 445,
  })

  assert.equal(result.valid, false)
  assert.match(result.reason, /\[#445\]/)
})

test('feature branches WITH a resolved PR reject a tag in the wrong position', () => {
  const result = validateCommitMessage('fix(api): repair status check [#445]', {
    branch: 'feature/status-check',
    prNumber: 445,
  })

  assert.equal(result.valid, false)
  assert.match(result.reason, /\[#445\]/)
})

test('feature branches WITH a resolved PR accept the matching tag after the header colon', () => {
  assert.deepEqual(
    validateCommitMessage('fix(api): [#445] repair status check', {
      branch: 'feature/status-check',
      prNumber: 445,
    }),
    { valid: true },
  )
})

test('branches with a resolved PR reject mismatched tags', () => {
  const result = validateCommitMessage('fix(api): [#444] repair status check', {
    branch: 'feature/status-check',
    prNumber: 445,
  })

  assert.equal(result.valid, false)
  assert.match(result.reason, /must use \[#445\]/)
})

test('branches without a resolved PR accept any syntactically valid PR tag', () => {
  assert.deepEqual(
    validateCommitMessage('fix(api): [#123] repair status check', {
      branch: 'feature/status-check',
      prNumber: null,
    }),
    { valid: true },
  )
})

test('production-candidate branches are non-protected and require tags once a PR exists', () => {
  assert.equal(isProtectedBranch('production-candidate-release'), false)
  // No PR yet → tagless is fine.
  assert.equal(
    validateCommitMessage('fix(api): repair status check', {
      branch: 'production-candidate-release',
      prNumber: null,
    }).valid,
    true,
  )
  // PR mapped → tag required.
  assert.equal(
    validateCommitMessage('fix(api): repair status check', {
      branch: 'production-candidate-release',
      prNumber: 77,
    }).valid,
    false,
  )
})
