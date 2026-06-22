#!/usr/bin/env node

import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const SCRIPT_PATH = fileURLToPath(new URL('./pr-ownership.mjs', import.meta.url));

const BRANCH = 'dave/bos-23-implement-sentry-integration';

function run(args, input) {
  return spawnSync(process.execPath, [SCRIPT_PATH, ...args], { encoding: 'utf8', input });
}

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
