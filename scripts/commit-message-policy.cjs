#!/usr/bin/env node

const { execFileSync } = require('node:child_process');

const PROTECTED_BRANCHES = new Set(['main', 'staging', 'production']);
const NON_PROTECTED_HEADER_PATTERN = /^[a-z]+(\([^)]+\))?!?: \[#(\d+)\] .+$/;

function getCommitHeader(message) {
  return String(message || '').split(/\r?\n/, 1)[0] || '';
}

function isProtectedBranch(branch) {
  return PROTECTED_BRANCHES.has(String(branch || '').trim());
}

function getCurrentBranch(execFile = execFileSync) {
  try {
    return execFile('git', ['branch', '--show-current'], {
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    }).trim();
  } catch (_error) {
    return '';
  }
}

function resolvePullRequestNumber(execFile = execFileSync) {
  try {
    const output = execFile('gh', ['pr', 'view', '--json', 'number', '--jq', '.number'], {
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    }).trim();
    const number = Number.parseInt(output, 10);

    return Number.isInteger(number) && number > 0 ? number : null;
  } catch (_error) {
    return null;
  }
}

function getRequiredPrTag(prNumber) {
  return Number.isInteger(prNumber) && prNumber > 0 ? `[#${prNumber}]` : '[#123]';
}

function validateCommitMessage(message, options = {}) {
  const branch = options.branch || '';

  if (isProtectedBranch(branch)) {
    return { valid: true };
  }

  const header = getCommitHeader(message);
  const match = header.match(NON_PROTECTED_HEADER_PATTERN);
  const prNumber = options.prNumber ?? null;

  if (!match) {
    return {
      valid: false,
      reason: `Expected commit header format on non-protected branches: type(scope): ${getRequiredPrTag(
        prNumber,
      )} subject.`,
    };
  }

  if (Number.isInteger(prNumber) && prNumber > 0 && Number.parseInt(match[2], 10) !== prNumber) {
    return {
      valid: false,
      reason: `Current branch resolves to PR #${prNumber}; commit header must use [#${prNumber}].`,
    };
  }

  return { valid: true };
}

exports.getCommitHeader = getCommitHeader;
exports.getCurrentBranch = getCurrentBranch;
exports.getRequiredPrTag = getRequiredPrTag;
exports.isProtectedBranch = isProtectedBranch;
exports.resolvePullRequestNumber = resolvePullRequestNumber;
exports.validateCommitMessage = validateCommitMessage;
