#!/usr/bin/env node

const { execFileSync } = require('node:child_process')

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
exports.getCurrentBranch = getCurrentBranch
exports.isProtectedBranch = isProtectedBranch
exports.resolvePullRequestNumber = resolvePullRequestNumber
exports.validateCommitMessage = validateCommitMessage
