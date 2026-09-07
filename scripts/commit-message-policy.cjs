#!/usr/bin/env node

const { execFileSync } = require('node:child_process')
const path = require('node:path')

const PROTECTED_BRANCHES = new Set(['main', 'staging', 'production'])
const NON_PROTECTED_HEADER_PATTERN = /^[a-z]+(\([^)]+\))?!?: \[#(\d+)\] .+$/

function getCommitHeader(message) {
  return String(message || '').split(/\r?\n/, 1)[0] || ''
}

function isProtectedBranch(branch) {
  return PROTECTED_BRANCHES.has(String(branch || '').trim())
}

function getCurrentBranch(execFile = execFileSync) {
  try {
    return execFile('git', ['branch', '--show-current'], {
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    }).trim()
  } catch (_error) {
    return ''
  }
}

function resolvePullRequestNumber(execFile = execFileSync) {
  try {
    const output = execFile('gh', ['pr', 'view', '--json', 'number', '--jq', '.number'], {
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    }).trim()
    const number = Number.parseInt(output, 10)

    return Number.isInteger(number) && number > 0 ? number : null
  } catch (_error) {
    return null
  }
}

// The shared non-empty-work-commit predicate. It is an ES module and this file is
// CommonJS, so it is loaded with require() rather than duplicated here: Node has
// supported require() of an ES module without top-level await since v22.12, and this
// repo's toolchain is well past that. Loading it is deliberately NOT fatal — a hook
// that crashes rejects every commit, which is far worse than the gap it would close.
// A failure degrades to "no exemption", i.e. exactly the behaviour that shipped before
// this exemption existed, which is also the safe direction: it can only ever reject a
// commit that should have passed, never accept untagged work.
let commitWorkPredicate

function loadCommitWorkPredicate(load) {
  // An injected loader is never memoized: the cache is for the one real module, and
  // sharing it would make the second caller's loader a no-op.
  if (load) {
    try {
      return load()
    } catch (_error) {
      return null
    }
  }

  if (commitWorkPredicate === undefined) {
    try {
      commitWorkPredicate = require(
        path.join(__dirname, '..', 'skills-toolbox', 'commit-work-predicate.mjs'),
      )
    } catch (_error) {
      commitWorkPredicate = null
    }
  }

  return commitWorkPredicate
}

// Resolve the trees the emptiness question compares, for a commit that DOES NOT EXIST
// YET. This runs from the commit-msg hook, before any commit object is written, so
// `git show -s --format=%T <sha>` has nothing to read: the pending commit's tree is the
// tree the current index would produce (`git write-tree`, which writes the tree object
// but no commit), and its parent is HEAD's tree.
//
// KNOWN GAP. On `git commit --amend` with an unchanged index this reports the amended
// commit as empty, because the pending tree equals HEAD's — and on an amend HEAD is the
// commit being REPLACED, not the pending commit's parent. The two states are
// indistinguishable from tree data alone, so the emptiness half cannot bound the amend
// path. What is left bounding it is the subject half by itself: an amend is exempted
// only when its new subject is set to the bootstrap placeholder verbatim, which no
// ordinary reword produces. That is a deliberate act on an advisory hook `--no-verify`
// already bypasses, not an accident — but it IS a path on which a commit carrying real
// content passes untagged, and it did not exist before this exemption. Do not read the
// subject test as making the amend path safe; closing it needs a source of amend-ness
// the commit-msg hook does not receive (prepare-commit-msg gets one).
//
// Returns null on any failure (no repository, an unmerged index, git absent). Null is
// "cannot establish emptiness", which the caller reads as "not exempt".
function resolvePendingCommitTrees(execFile = execFileSync) {
  const git = (args) =>
    execFile('git', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim()

  try {
    const tree = git(['write-tree'])
    let parentTree
    try {
      parentTree = git(['rev-parse', 'HEAD^{tree}'])
    } catch (_error) {
      // No HEAD yet: the pending commit is a root commit, so its parent tree is the
      // repository's empty tree. Only git can compute that — it depends on the
      // repository's hash algorithm — so never hard-code the SHA-1 value.
      parentTree = git(['hash-object', '-t', 'tree', '/dev/null'])
    }

    return tree && parentTree ? { tree, parentTree } : null
  } catch (_error) {
    return null
  }
}

// Whether this commit is the empty bootstrap placeholder a daemon creates to give a
// branch a diff so the forge will open a PR for it. That commit is created empty and
// deliberately never tagged, so without this exemption every daemon-bootstrapped branch
// is a closed loop: the placeholder cannot satisfy the mandatory-tag rule and cannot be
// rewritten to.
//
// Both halves are required. The subject test is a pure whole-subject equality (after
// stripping a run of injected tags), so a real commit cannot claim the exemption by
// embedding the placeholder wording or by having its tags stripped; the tree test is
// what stops a hand-written commit from claiming it by copying a string — except on the
// `--amend` path, where the tree test cannot discriminate (see resolvePendingCommitTrees). Ordering is
// cheapest-first on purpose: the string test rejects effectively every commit before
// the tree lookups shell out to git at all.
function isExemptBootstrapCommit(header, options = {}) {
  const predicate = loadCommitWorkPredicate(options.loadPredicate)
  if (!predicate || !predicate.isBootstrapSubject(header)) {
    return false
  }

  const resolveTrees = options.resolveTrees
  if (typeof resolveTrees !== 'function') {
    return false
  }

  let trees
  try {
    trees = resolveTrees()
  } catch (_error) {
    return false
  }

  return Boolean(
    trees &&
    predicate.isExemptBootstrapCommit({
      subject: header,
      tree: trees.tree,
      parentTree: trees.parentTree,
    }),
  )
}

function validateCommitMessage(message, options = {}) {
  const branch = options.branch || ''

  // Protected branches (main/staging/production) never carry a PR tag.
  if (isProtectedBranch(branch)) {
    return { valid: true }
  }

  const prNumber = options.prNumber ?? null

  // No PR maps to this branch yet. The [#PR] tag literally cannot exist before
  // the PR is opened — cron/agent commits precede PR creation, and bossd injects
  // the tag at finalize. Requiring it here is what forced cron worktrees to skip
  // the hook (and humans to guess a number). Allow the commit; commitlint's base
  // rules still validate the conventional type/scope/subject. Once the branch
  // maps to a PR (resolved via `gh` in .commitlintrc), the tag becomes mandatory.
  if (!(Number.isInteger(prNumber) && prNumber > 0)) {
    return { valid: true }
  }

  const header = getCommitHeader(message)

  // An empty bootstrap placeholder commit is exempt. Positioned after the
  // protected-branch and no-PR waivers so the cheap checks still short-circuit first,
  // and before the mandatory-tag branch because that branch is the one it must waive.
  if (isExemptBootstrapCommit(header, options)) {
    return { valid: true }
  }

  const match = header.match(NON_PROTECTED_HEADER_PATTERN)

  if (!match) {
    return {
      valid: false,
      reason: `Branch maps to PR #${prNumber}; commit header must be: type(scope): [#${prNumber}] subject.`,
    }
  }

  if (Number.parseInt(match[2], 10) !== prNumber) {
    return {
      valid: false,
      reason: `Current branch resolves to PR #${prNumber}; commit header must use [#${prNumber}].`,
    }
  }

  return { valid: true }
}

exports.getCommitHeader = getCommitHeader
exports.isExemptBootstrapCommit = isExemptBootstrapCommit
exports.resolvePendingCommitTrees = resolvePendingCommitTrees
exports.getCurrentBranch = getCurrentBranch
exports.isProtectedBranch = isProtectedBranch
exports.resolvePullRequestNumber = resolvePullRequestNumber
exports.validateCommitMessage = validateCommitMessage
