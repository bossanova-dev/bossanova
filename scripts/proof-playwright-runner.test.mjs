#!/usr/bin/env node

import { spawnSync } from 'node:child_process';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

import { buildSpec, validateRecipe } from './proof-playwright-runner.mjs';

const repoRoot = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const runnerPath = path.join(repoRoot, 'scripts/proof-playwright-runner.mjs');

test('buildSpec video branch records webm via its own context and screenshots a poster', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      surface: 'web',
      capture: 'video',
      steps: [
        { action: 'goto', route: '/' },
        { action: 'click', selector: '[data-testid="row"]' },
        { action: 'type', selector: 'input[name="q"]', value: 'hello' },
        { action: 'wait', timeoutMs: 500 },
      ],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  });
  assert.match(spec, /recordVideo/);
  assert.match(spec, /newContext/);
  assert.match(spec, /web-flow\.webm/);
  assert.match(spec, /web-flow\.png/); // poster
  assert.match(spec, /\.click\(\)/);
  assert.match(spec, /pressSequentially\('hello'\)/);
  assert.match(spec, /context\.close\(\)/); // finalizes the video before rename
});

test('buildSpec video branch preserves playwright baseURL for relative goto steps', () => {
  const spec = buildSpec({
    recipe: {
      id: 'web-flow',
      surface: 'web',
      capture: 'video',
      steps: [{ action: 'goto', route: '/' }],
    },
    outputDir: '/tmp/out',
    surface: 'web',
  });

  assert.match(spec, /async \(\{ browser, baseURL \}\)/);
  assert.match(spec, /baseURL,/);
  assert.doesNotMatch(spec, /chromium\.launch/);
});

test('validateRecipe accepts a video recipe and rejects an unknown action', () => {
  assert.doesNotThrow(() =>
    validateRecipe({
      id: 'v',
      surface: 'web',
      capture: 'video',
      steps: [{ action: 'goto', route: '/' }],
    }),
  );
  assert.throws(
    () =>
      validateRecipe({ id: 'v', surface: 'web', capture: 'video', steps: [{ action: 'nope' }] }),
    /unsupported video step action/,
  );
});

test('validateRecipe rejects incomplete video steps before playwright starts', () => {
  assert.throws(
    () =>
      validateRecipe({ id: 'v', surface: 'web', capture: 'video', steps: [{ action: 'goto' }] }),
    /video goto step requires route/,
  );
  assert.throws(
    () =>
      validateRecipe({ id: 'v', surface: 'web', capture: 'video', steps: [{ action: 'click' }] }),
    /video click step requires selector/,
  );
  assert.throws(
    () =>
      validateRecipe({
        id: 'v',
        surface: 'web',
        capture: 'video',
        steps: [{ action: 'type', selector: 'input' }],
      }),
    /video type step requires value/,
  );
});

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
