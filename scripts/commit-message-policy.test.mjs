#!/usr/bin/env node

import assert from 'node:assert/strict';
import { test } from 'node:test';

import {
  getRequiredPrTag,
  isProtectedBranch,
  validateCommitMessage,
} from './commit-message-policy.cjs';

test('protected branches allow conventional commits without PR tags', () => {
  for (const branch of ['main', 'staging', 'production']) {
    assert.equal(isProtectedBranch(branch), true);
    assert.deepEqual(validateCommitMessage('fix(api): repair status check', { branch }), {
      valid: true,
    });
  }
});

test('feature branches reject conventional commits without PR tags', () => {
  const result = validateCommitMessage('fix(api): repair status check', {
    branch: 'feature/status-check',
  });

  assert.equal(result.valid, false);
  assert.match(result.reason, /type\(scope\): \[#123\] subject/);
});

test('feature branches reject PR tags in the wrong position', () => {
  const result = validateCommitMessage('fix(api): repair status check [#445]', {
    branch: 'feature/status-check',
  });

  assert.equal(result.valid, false);
  assert.match(result.reason, /type\(scope\): \[#123\] subject/);
});

test('feature branches accept PR tags immediately after the header colon', () => {
  assert.deepEqual(
    validateCommitMessage('fix(api): [#445] repair status check', {
      branch: 'feature/status-check',
    }),
    { valid: true },
  );
});

test('branches with a resolved PR reject mismatched tags', () => {
  const result = validateCommitMessage('fix(api): [#444] repair status check', {
    branch: 'feature/status-check',
    prNumber: 445,
  });

  assert.equal(result.valid, false);
  assert.match(result.reason, /must use \[#445\]/);
});

test('branches without a resolved PR accept any syntactically valid PR tag', () => {
  assert.deepEqual(
    validateCommitMessage('fix(api): [#123] repair status check', {
      branch: 'feature/status-check',
      prNumber: null,
    }),
    { valid: true },
  );
});

test('production-candidate branches are non-protected and require tags', () => {
  assert.equal(isProtectedBranch('production-candidate-release'), false);
  assert.equal(
    validateCommitMessage('fix(api): repair status check', {
      branch: 'production-candidate-release',
    }).valid,
    false,
  );
});

test('required PR tag uses resolved PR number when available', () => {
  assert.equal(getRequiredPrTag(445), '[#445]');
  assert.equal(getRequiredPrTag(null), '[#123]');
});
