#!/usr/bin/env node

import { spawnSync } from 'node:child_process';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const repoRoot = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const runnerPath = path.join(repoRoot, 'scripts/proof-playwright-runner.mjs');

test('rejects recipe ids that are unsafe as screenshot filenames', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-runner-bad-id-'));
  const recipePath = path.join(dir, 'recipe.json');
  const outputDir = path.join(dir, 'out');
  fs.writeFileSync(
    recipePath,
    JSON.stringify({
      id: 'bad/id',
      route: '/',
      selector: 'main',
    }),
  );

  const result = runRunner(['--surface', 'web', '--recipe', recipePath, '--output-dir', outputDir]);

  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /invalid recipe id/);
  assert.equal(fs.existsSync(path.join(outputDir, 'bad')), false);
});

test('rejects unsupported browser proof surfaces before playwright starts', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-runner-bad-surface-'));
  const recipePath = path.join(dir, 'recipe.json');
  fs.writeFileSync(
    recipePath,
    JSON.stringify({
      id: 'marketing-home',
      route: '/',
      selector: 'main',
    }),
  );

  const result = runRunner([
    '--surface',
    'desktop',
    '--recipe',
    recipePath,
    '--output-dir',
    path.join(dir, 'out'),
  ]);

  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /invalid --surface/);
});

test('rejects external browser proof routes before playwright starts', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-runner-external-route-'));
  const recipePath = path.join(dir, 'recipe.json');
  fs.writeFileSync(
    recipePath,
    JSON.stringify({
      id: 'marketing-home',
      route: 'http://localhost:3000/admin',
      selector: 'main',
    }),
  );

  const result = runRunner([
    '--surface',
    'marketing',
    '--recipe',
    recipePath,
    '--output-dir',
    path.join(dir, 'out'),
  ]);

  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /proof browser route must be relative/);
});

test('rejects protocol-relative browser proof routes before playwright starts', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-runner-protocol-route-'));
  const recipePath = path.join(dir, 'recipe.json');
  fs.writeFileSync(
    recipePath,
    JSON.stringify({
      id: 'marketing-home',
      route: '//example.com/admin',
      selector: 'main',
    }),
  );

  const result = runRunner([
    '--surface',
    'marketing',
    '--recipe',
    recipePath,
    '--output-dir',
    path.join(dir, 'out'),
  ]);

  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /proof browser route must be relative/);
});

test('rejects backslash browser proof routes before playwright starts', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-runner-backslash-route-'));
  const recipePath = path.join(dir, 'recipe.json');
  fs.writeFileSync(
    recipePath,
    JSON.stringify({
      id: 'marketing-home',
      route: '/\\example.com/admin',
      selector: 'main',
    }),
  );

  const result = runRunner([
    '--surface',
    'marketing',
    '--recipe',
    recipePath,
    '--output-dir',
    path.join(dir, 'out'),
  ]);

  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /proof browser route must be relative/);
});

function runRunner(args) {
  return spawnSync(process.execPath, [runnerPath, ...args], {
    cwd: repoRoot,
    encoding: 'utf8',
    timeout: 5000,
  });
}
