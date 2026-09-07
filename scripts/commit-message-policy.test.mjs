#!/usr/bin/env node

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import { createRequire } from 'node:module'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'

import commitlintConfig from '../.commitlintrc.cjs'
import {
  getCurrentBranch,
  isProtectedBranch,
  resolvePendingCommitTrees,
  resolvePullRequestNumber,
  validateCommitMessage,
} from './commit-message-policy.cjs'

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

// ---------------------------------------------------------------------------
// BOS-1195: the empty bootstrap-commit exemption.
//
// The daemon's bootstrap commit is created EMPTY and deliberately never tagged, so
// before this exemption every daemon-bootstrapped branch was a closed loop: the
// placeholder could not satisfy the mandatory-tag rule and could not be rewritten to
// satisfy it either. The dangerous direction is the opposite one, so every positive
// case below is paired with the negative that bounds it.
// ---------------------------------------------------------------------------

const BOOTSTRAP = 'chore: [skip ci] create pull request'
const TREE = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
const OTHER_TREE = 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'

// The pending commit changes nothing: its tree is HEAD's tree.
const emptyTrees = () => ({ tree: TREE, parentTree: TREE })
// The pending commit changes something.
const nonEmptyTrees = () => ({ tree: OTHER_TREE, parentTree: TREE })

const onPrBranch = (extra) => ({
  branch: 'feature/status-check',
  prNumber: 445,
  ...extra,
})

test('THE REGRESSION: an untagged empty bootstrap commit passes on a branch with an open PR', () => {
  // This exact call returned { valid: false } before the exemption existed, which is
  // what made every daemon-bootstrapped branch unsatisfiable.
  assert.deepEqual(validateCommitMessage(BOOTSTRAP, onPrBranch({ resolveTrees: emptyTrees })), {
    valid: true,
  })
})

test('a tagged bootstrap commit still passes, including with a stale or stacked tag', () => {
  for (const subject of [
    'chore: [#445] [skip ci] create pull request',
    // Tags stack across injector runs for different PR numbers; the placeholder is
    // still the placeholder, and rewriting it is exactly what it must not require.
    'chore: [#444] [skip ci] create pull request',
    'chore: [#7] [#445] [skip ci] create pull request',
  ]) {
    assert.deepEqual(
      validateCommitMessage(subject, onPrBranch({ resolveTrees: emptyTrees })),
      { valid: true },
      subject,
    )
  }
})

test('an untagged NON-EMPTY commit is still rejected, with its message unchanged', () => {
  const result = validateCommitMessage(
    'fix(api): repair status check',
    onPrBranch({ resolveTrees: emptyTrees }),
  )

  assert.equal(result.valid, false)
  assert.equal(
    result.reason,
    'Branch maps to PR #445; commit header must be: type(scope): [#445] subject.',
  )
})

test('the bootstrap SUBJECT alone does not exempt: the commit must also be empty', () => {
  // The over-broad exemption this bounds: copying the placeholder string into a real
  // work commit must not buy a waiver.
  const result = validateCommitMessage(BOOTSTRAP, onPrBranch({ resolveTrees: nonEmptyTrees }))

  assert.equal(result.valid, false)
  assert.match(result.reason, /\[#445\]/)
})

test('EMPTINESS alone does not exempt: the subject must be the placeholder', () => {
  for (const subject of [
    'fix(api): repair status check',
    // Embeds the placeholder wording but is not equal to it.
    'revert: "chore: [skip ci] create pull request"',
    'chore: [skip ci] create pull request and tag it',
    // A real work commit with its tags stripped strips to real work, not a placeholder.
    'feat(boss): add X',
  ]) {
    assert.equal(
      validateCommitMessage(subject, onPrBranch({ resolveTrees: emptyTrees })).valid,
      false,
      subject,
    )
  }
})

test('emptiness that cannot be established is not an exemption', () => {
  // No resolver, a resolver that fails, and a resolver that cannot answer all mean
  // "cannot establish emptiness", which must read as "not exempt" — the direction that
  // can only ever reject a commit that should have passed.
  const cases = {
    'no resolver at all': undefined,
    'a resolver that throws': () => {
      throw new Error('not a git repository')
    },
    'a resolver that cannot answer': () => null,
  }

  for (const [label, resolveTrees] of Object.entries(cases)) {
    assert.equal(validateCommitMessage(BOOTSTRAP, onPrBranch({ resolveTrees })).valid, false, label)
  }
})

test('a predicate module that will not load degrades to the pre-exemption behaviour', () => {
  // A hook that crashes rejects every commit, which is worse than the gap it closes.
  assert.equal(
    validateCommitMessage(
      BOOTSTRAP,
      onPrBranch({
        resolveTrees: emptyTrees,
        loadPredicate: () => {
          throw new Error('ERR_REQUIRE_ESM')
        },
      }),
    ).valid,
    false,
  )
})

test('the exemption never fires where the cheaper waivers already pass a commit', () => {
  // The exemption is the only step here that shells out to git, so it must sit AFTER
  // the protected-branch and no-PR waivers. Count the resolver's calls rather than
  // throwing from it: isExemptBootstrapCommit swallows a throw, so an exception would
  // still return { valid: true } from the waiver below and prove nothing about order.
  // Every subject here IS the placeholder, so a hoisted exemption would genuinely
  // reach the resolver.
  let calls = 0
  const spy = () => {
    calls += 1
    return emptyTrees()
  }

  for (const branch of ['main', 'staging', 'production']) {
    assert.deepEqual(validateCommitMessage(BOOTSTRAP, { branch, resolveTrees: spy }), {
      valid: true,
    })
  }
  assert.deepEqual(
    validateCommitMessage(BOOTSTRAP, {
      branch: 'feature/status-check',
      prNumber: null,
      resolveTrees: spy,
    }),
    { valid: true },
  )
  assert.equal(calls, 0, 'the git-backed emptiness check must not run behind a cheaper waiver')

  // …and it does run once the cheap waivers no longer apply.
  assert.deepEqual(validateCommitMessage(BOOTSTRAP, onPrBranch({ resolveTrees: spy })), {
    valid: true,
  })
  assert.equal(calls, 1)
})

test('a tool outage while resolving the PR still degrades to the waived branch', () => {
  const outage = () => {
    throw new Error('gh: could not connect')
  }
  assert.equal(resolvePullRequestNumber(outage), null)
  assert.equal(getCurrentBranch(outage), '')
  // …and an unresolved PR number is the existing no-PR waiver, unchanged.
  assert.deepEqual(
    validateCommitMessage('fix(api): repair status check', {
      branch: 'feature/status-check',
      prNumber: resolvePullRequestNumber(outage),
    }),
    { valid: true },
  )
})

test('resolvePendingCommitTrees compares the PENDING tree, since no commit exists yet', () => {
  const repo = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'commit-policy-')))
  const env = { ...process.env, GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: '/dev/null' }
  const exec = (cmd, args, options) => execFileSync(cmd, args, { ...options, cwd: repo, env })
  const git = (...args) => execFileSync('git', args, { cwd: repo, env, encoding: 'utf8' }).trim()

  git('-c', 'init.defaultBranch=main', 'init', '-q', repo)
  git('config', 'user.email', 'test@example.com')
  git('config', 'user.name', 'Test')

  // No HEAD: the pending root commit compares against git's own empty tree, never a
  // hard-coded object id.
  const rootTrees = resolvePendingCommitTrees(exec)
  assert.equal(rootTrees.tree, git('hash-object', '-t', 'tree', '/dev/null'))
  assert.equal(rootTrees.parentTree, rootTrees.tree, 'an empty root commit is empty')

  fs.writeFileSync(path.join(repo, 'a.txt'), 'work\n')
  git('add', 'a.txt')
  const stagedTrees = resolvePendingCommitTrees(exec)
  assert.notEqual(stagedTrees.tree, stagedTrees.parentTree, 'staged work is not empty')

  git('commit', '-q', '--no-verify', '-m', 'chore(test): seed')

  // With a commit in place and nothing staged, the pending commit is empty — which is
  // exactly the shape the daemon's bootstrap commit has.
  const emptyPending = resolvePendingCommitTrees(exec)
  assert.equal(emptyPending.tree, emptyPending.parentTree)
  assert.equal(emptyPending.parentTree, git('rev-parse', 'HEAD^{tree}'))

  fs.rmSync(repo, { recursive: true, force: true })
})

test('the cron waiver short-circuits before any branch or PR resolution is attempted', () => {
  // Stub the module object .commitlintrc.cjs holds, so a resolution attempt is loud.
  const policy = createRequire(import.meta.url)('./commit-message-policy.cjs')
  const saved = {
    getCurrentBranch: policy.getCurrentBranch,
    resolvePullRequestNumber: policy.resolvePullRequestNumber,
    resolvePendingCommitTrees: policy.resolvePendingCommitTrees,
  }
  const explode = (name) => () => {
    throw new Error(`${name} must not run under the cron waiver`)
  }
  const savedCron = process.env.BOSS_CRON

  try {
    for (const name of Object.keys(saved)) policy[name] = explode(name)
    process.env.BOSS_CRON = 'true'
    const rule = commitlintConfig.plugins[0].rules['pr-tag']
    assert.deepEqual(rule({ raw: 'chore: untagged and unresolvable', header: '' }), [true])
  } finally {
    Object.assign(policy, saved)
    if (savedCron === undefined) delete process.env.BOSS_CRON
    else process.env.BOSS_CRON = savedCron
  }
})

test('without the cron waiver the rule consults the policy with the resolved context', () => {
  const policy = createRequire(import.meta.url)('./commit-message-policy.cjs')
  const saved = {
    getCurrentBranch: policy.getCurrentBranch,
    resolvePullRequestNumber: policy.resolvePullRequestNumber,
    resolvePendingCommitTrees: policy.resolvePendingCommitTrees,
  }
  const savedCron = process.env.BOSS_CRON

  try {
    policy.getCurrentBranch = () => 'feature/status-check'
    policy.resolvePullRequestNumber = () => 445
    policy.resolvePendingCommitTrees = () => emptyTrees()
    delete process.env.BOSS_CRON
    const rule = commitlintConfig.plugins[0].rules['pr-tag']
    // The bootstrap placeholder now reaches the exemption through the config's own
    // context resolution — the wiring, not just the predicate.
    assert.deepEqual(rule({ raw: BOOTSTRAP, header: BOOTSTRAP }), [true, ''])
  } finally {
    Object.assign(policy, saved)
    if (savedCron === undefined) delete process.env.BOSS_CRON
    else process.env.BOSS_CRON = savedCron
  }
})
