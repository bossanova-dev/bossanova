#!/usr/bin/env node

import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

import {
  BOOTSTRAP_COMMIT_SUBJECT,
  classifyPR,
  implementedState,
  isBootstrapSubject,
  isOwnedPR,
  realAheadSubjects,
} from './pr-ownership.mjs';

const SCRIPT_PATH = fileURLToPath(new URL('./pr-ownership.mjs', import.meta.url));

const TICKET = 'BOS-23';
const BRANCH = 'dave/bos-23-implement-sentry-integration';
const ISSUE_URL = 'https://linear.app/bossanova-dev/issue/BOS-23/implement-sentry-integration';
const REAL = ['feat(proto): add Sentry repo credentials', 'feat(boss): add Sentry flow'];

function run(args, input) {
  return spawnSync(process.execPath, [SCRIPT_PATH, ...args], { encoding: 'utf8', input });
}

test('isBootstrapSubject matches the bootstrap placeholder, tagged or not', () => {
  assert.equal(isBootstrapSubject(BOOTSTRAP_COMMIT_SUBJECT), true);
  assert.equal(isBootstrapSubject('chore: [#640] [skip ci] create pull request'), true);
  assert.equal(isBootstrapSubject('feat(proto): add Sentry repo credentials'), false);
  assert.equal(isBootstrapSubject(''), false);
  assert.equal(isBootstrapSubject(undefined), false);
});

test('realAheadSubjects drops the bootstrap commit and blanks', () => {
  assert.deepEqual(realAheadSubjects([BOOTSTRAP_COMMIT_SUBJECT]), []);
  assert.deepEqual(realAheadSubjects(['', '  ', BOOTSTRAP_COMMIT_SUBJECT]), []);
  assert.deepEqual(realAheadSubjects([...REAL, BOOTSTRAP_COMMIT_SUBJECT]), REAL);
  assert.deepEqual(realAheadSubjects(null), []);
});

test('isOwnedPR — branch name is the primary signal', () => {
  assert.equal(
    isOwnedPR({
      ticketId: TICKET,
      issueBranch: BRANCH,
      pr: { headBranch: BRANCH, title: 'whatever' },
    }),
    true,
  );
});

test('isOwnedPR — [BOS-NN] title substring is the fallback', () => {
  assert.equal(
    isOwnedPR({
      ticketId: TICKET,
      issueBranch: 'other',
      pr: { headBranch: 'mismatch', title: '[BOS-23] Sentry' },
    }),
    true,
  );
});

test('isOwnedPR — "Linear issue: <url>" body first line is a signal', () => {
  assert.equal(
    isOwnedPR({
      ticketId: TICKET,
      issueUrl: ISSUE_URL,
      pr: { headBranch: 'x', title: 'y', body: `Linear issue: ${ISSUE_URL}\n\nPlan: ...` },
    }),
    true,
  );
});

test('isOwnedPR — none of the signals match -> not owned', () => {
  assert.equal(
    isOwnedPR({
      ticketId: TICKET,
      issueBranch: BRANCH,
      issueUrl: ISSUE_URL,
      pr: {
        headBranch: 'feature/x',
        title: '[BOS-99] other',
        body: 'Linear issue: https://example.com/other',
      },
    }),
    false,
  );
  assert.equal(isOwnedPR({ ticketId: TICKET, pr: null }), false);
});

test('isOwnedPR — url match requires an exact first-line url', () => {
  // A url mentioned later in the body must not count; only the first line does.
  assert.equal(
    isOwnedPR({
      ticketId: TICKET,
      issueUrl: ISSUE_URL,
      pr: { headBranch: 'x', title: 'y', body: `something\nLinear issue: ${ISSUE_URL}` },
    }),
    false,
  );
});

test('implementedState — empty vs populated', () => {
  assert.equal(implementedState({ aheadSubjects: [] }), 'empty');
  assert.equal(implementedState({ aheadSubjects: [BOOTSTRAP_COMMIT_SUBJECT] }), 'empty');
  assert.equal(implementedState({ aheadSubjects: REAL }), 'populated');
});

test('classifyPR — none when there is no open PR and no real work ahead', () => {
  assert.equal(classifyPR({ ticketId: TICKET, pr: null }), 'none');
});

test('classifyPR — owned when the issue branch has real work ahead but no open PR', () => {
  assert.equal(
    classifyPR({
      ticketId: TICKET,
      sessionBranch: BRANCH,
      issueBranch: BRANCH,
      pr: null,
      aheadSubjects: REAL,
    }),
    'owned',
  );
});

test('classifyPR — foreign when a different branch has real work ahead but no open PR', () => {
  assert.equal(
    classifyPR({
      ticketId: TICKET,
      sessionBranch: 'dave/other-work',
      issueBranch: BRANCH,
      pr: null,
      aheadSubjects: REAL,
    }),
    'foreign',
  );
});

test('classifyPR — foreign when an open PR with real work matches no ownership signal', () => {
  assert.equal(
    classifyPR({
      ticketId: TICKET,
      issueBranch: BRANCH,
      issueUrl: ISSUE_URL,
      pr: { headBranch: 'feature/x', title: '[BOS-99]' },
      aheadSubjects: REAL,
    }),
    'foreign',
  );
});

test('classifyPR — bootstrap-only when an unowned PR carries only the bootstrap commit', () => {
  // A fresh PR with no real file changes (just bossd's placeholder commit) holds
  // nothing to clobber, so it is adoptable as a reusable bootstrap PR regardless
  // of whether its branch/title/body name the ticket. Foreign protection only
  // kicks in once the PR carries real work. Reproduces the PR #676 state: a
  // generic "security-review" branch + "Security review" PR with an empty body.
  assert.equal(
    classifyPR({
      ticketId: TICKET,
      sessionBranch: 'security-review',
      issueBranch: BRANCH,
      issueUrl: ISSUE_URL,
      pr: { headBranch: 'security-review', title: 'Security review', body: '' },
      aheadSubjects: [BOOTSTRAP_COMMIT_SUBJECT],
    }),
    'bootstrap-only',
  );
});

test('classifyPR — bootstrap-only when owned with only the placeholder ahead', () => {
  assert.equal(
    classifyPR({
      ticketId: TICKET,
      issueBranch: BRANCH,
      pr: { headBranch: BRANCH, title: 'x' },
      aheadSubjects: [BOOTSTRAP_COMMIT_SUBJECT],
    }),
    'bootstrap-only',
  );
});

test('classifyPR — owned when owned with real work ahead (the BOS-23 misfire)', () => {
  assert.equal(
    classifyPR({
      ticketId: TICKET,
      issueBranch: BRANCH,
      pr: { headBranch: BRANCH, title: '[BOS-23] Sentry' },
      aheadSubjects: [...REAL, BOOTSTRAP_COMMIT_SUBJECT],
    }),
    'owned',
  );
});

test('CLI classify — owned (reproduces PR #640 state)', () => {
  const prJson = JSON.stringify({
    number: 640,
    title: '[BOS-23] Implement Sentry integration',
    body: `Linear issue: ${ISSUE_URL}`,
    headRefName: BRANCH,
  });
  const result = run([
    'classify',
    '--ticket',
    TICKET,
    '--issue-branch',
    BRANCH,
    '--issue-url',
    ISSUE_URL,
    '--pr-json',
    prJson,
    '--ahead-subjects-json',
    JSON.stringify([...REAL, BOOTSTRAP_COMMIT_SUBJECT]),
  ]);
  assert.equal(result.status, 0);
  assert.equal(result.stdout.trim(), 'owned');
});

test('CLI classify — foreign PR with real work on the branch', () => {
  const prJson = JSON.stringify({
    number: 5,
    title: 'unrelated',
    body: '',
    headRefName: 'feature/x',
  });
  const result = run([
    'classify',
    '--ticket',
    TICKET,
    '--issue-branch',
    BRANCH,
    '--pr-json',
    prJson,
    '--ahead-subjects-json',
    JSON.stringify(REAL),
  ]);
  assert.equal(result.status, 0);
  assert.equal(result.stdout.trim(), 'foreign');
});

test('CLI classify — empty unowned bootstrap PR is adoptable bootstrap-only', () => {
  // PR #676 repro: a human-named "security-review" branch with a "Security
  // review" draft PR and only bossd's placeholder commit ahead is a reusable
  // bootstrap PR, not foreign.
  const prJson = JSON.stringify({
    number: 676,
    title: 'Security review',
    body: '',
    headRefName: 'security-review',
    state: 'OPEN',
  });
  const result = run([
    'classify',
    '--ticket',
    TICKET,
    '--session-branch',
    'security-review',
    '--issue-branch',
    BRANCH,
    '--issue-url',
    ISSUE_URL,
    '--pr-json',
    prJson,
    '--ahead-subjects-json',
    JSON.stringify([BOOTSTRAP_COMMIT_SUBJECT]),
  ]);
  assert.equal(result.status, 0);
  assert.equal(result.stdout.trim(), 'bootstrap-only');
});

test('CLI classify — empty --pr-json is none', () => {
  const result = run([
    'classify',
    '--ticket',
    TICKET,
    '--pr-json',
    '',
    '--ahead-subjects-json',
    '[]',
  ]);
  assert.equal(result.status, 0);
  assert.equal(result.stdout.trim(), 'none');
});

test('CLI classify — real work ahead without an open PR is owned on the issue branch', () => {
  const result = run([
    'classify',
    '--ticket',
    TICKET,
    '--session-branch',
    BRANCH,
    '--issue-branch',
    BRANCH,
    '--pr-json',
    '',
    '--ahead-subjects-json',
    JSON.stringify(REAL),
  ]);
  assert.equal(result.status, 0);
  assert.equal(result.stdout.trim(), 'owned');
});

test('CLI classify — real work ahead without an open PR is foreign on another branch', () => {
  const result = run([
    'classify',
    '--ticket',
    TICKET,
    '--session-branch',
    'dave/other-work',
    '--issue-branch',
    BRANCH,
    '--pr-json',
    '',
    '--ahead-subjects-json',
    JSON.stringify(REAL),
  ]);
  assert.equal(result.status, 0);
  assert.equal(result.stdout.trim(), 'foreign');
});

test('CLI classify — accepts a gh-list array and skips closed PRs', () => {
  const arr = JSON.stringify([
    { number: 1, title: 'closed', state: 'CLOSED', headRefName: BRANCH },
    { number: 640, title: '[BOS-23] Sentry', state: 'OPEN', headRefName: BRANCH },
  ]);
  const result = run([
    'classify',
    '--ticket',
    TICKET,
    '--issue-branch',
    BRANCH,
    '--pr-json',
    arr,
    '--ahead-subjects-json',
    JSON.stringify(REAL),
  ]);
  assert.equal(result.status, 0);
  assert.equal(result.stdout.trim(), 'owned');
});

test('CLI classify — with two open PRs on the head, the first is used', () => {
  // gh can return more than one open PR for a head (e.g. a duplicate); .find
  // takes the first non-closed entry. Here the first is foreign, so -> foreign.
  const arr = JSON.stringify([
    { number: 700, title: 'unrelated', state: 'OPEN', headRefName: 'feature/x' },
    { number: 640, title: '[BOS-23] Sentry', state: 'OPEN', headRefName: BRANCH },
  ]);
  const result = run([
    'classify',
    '--ticket',
    TICKET,
    '--issue-branch',
    BRANCH,
    '--pr-json',
    arr,
    '--ahead-subjects-json',
    JSON.stringify(REAL),
  ]);
  assert.equal(result.status, 0);
  assert.equal(result.stdout.trim(), 'foreign');
});

test('CLI classify — reads newline-delimited ahead subjects from stdin', () => {
  const prJson = JSON.stringify({ number: 640, title: '[BOS-23] Sentry', headRefName: BRANCH });
  const stdin = `${REAL.join('\n')}\n${BOOTSTRAP_COMMIT_SUBJECT}\n`;
  const result = run(
    [
      'classify',
      '--ticket',
      TICKET,
      '--issue-branch',
      BRANCH,
      '--pr-json',
      prJson,
      '--ahead-subjects-stdin',
    ],
    stdin,
  );
  assert.equal(result.status, 0);
  assert.equal(result.stdout.trim(), 'owned');
});

test('CLI classify — stdin with only the bootstrap commit is bootstrap-only', () => {
  const prJson = JSON.stringify({ number: 640, title: '[BOS-23] Sentry', headRefName: BRANCH });
  const result = run(
    [
      'classify',
      '--ticket',
      TICKET,
      '--issue-branch',
      BRANCH,
      '--pr-json',
      prJson,
      '--ahead-subjects-stdin',
    ],
    `${BOOTSTRAP_COMMIT_SUBJECT}\n`,
  );
  assert.equal(result.status, 0);
  assert.equal(result.stdout.trim(), 'bootstrap-only');
});

test('CLI number — extracts the first open PR number from gh-list JSON', () => {
  const arr = JSON.stringify([
    { number: 1, title: 'closed', state: 'CLOSED', headRefName: BRANCH },
    { number: 640, title: '[BOS-23] Sentry', state: 'OPEN', headRefName: BRANCH },
  ]);
  const result = run(['number', '--pr-json', arr]);
  assert.equal(result.status, 0);
  assert.equal(result.stdout.trim(), '640');
});

test('CLI number — empty when there is no open PR', () => {
  const result = run(['number', '--pr-json', '']);
  assert.equal(result.status, 0);
  assert.equal(result.stdout.trim(), '');
});

test('CLI — unknown command exits 1', () => {
  const result = run(['bogus']);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /unknown command/);
});
