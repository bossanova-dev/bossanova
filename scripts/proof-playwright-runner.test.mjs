#!/usr/bin/env node

import { spawnSync } from 'node:child_process';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

import { buildSpec, validateRecipe, slugify } from './proof-playwright-runner.mjs';

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

// ── slugify ───────────────────────────────────────────────────────────────────

test('slugify: basic cases', () => {
  assert.equal(slugify('Open home'), 'open-home');
  assert.equal(slugify('  A/B  '), 'a-b');
  assert.equal(slugify(''), 'step');
  assert.equal(slugify('goto'), 'goto');
});

// ── buildSpec video: per-step stills + video-meta.json ───────────────────────

const labelledVideoRecipe = {
  id: 'web-new-session-flow',
  surface: 'web',
  capture: 'video',
  cropToSelector: '.data-table-wrap',
  viewport: { width: 1440, height: 1000 },
  steps: [
    { action: 'goto', route: '/', label: 'Open home' },
    { action: 'wait', selector: '.data-table-wrap', label: 'Sessions list' },
    { action: 'click', selector: '.data-table-wrap .row-clickable', label: 'Open session' },
    { action: 'wait', timeoutMs: 800, label: 'Session view' },
  ],
};

test('buildSpec video: emits captureStill call after each step', () => {
  const spec = buildSpec({ recipe: labelledVideoRecipe, outputDir: '/tmp/out', surface: 'web' });
  // Should have exactly 4 captureStill calls
  const matches = spec.match(/captureStill\(/g) ?? [];
  assert.equal(matches.length, 4 + 1); // 4 calls + 1 helper definition
});

test('buildSpec video: still filenames use 2-digit NN and slug', () => {
  const spec = buildSpec({ recipe: labelledVideoRecipe, outputDir: '/tmp/out', surface: 'web' });
  assert.match(spec, /01-open-home\.png/);
  assert.match(spec, /02-sessions-list\.png/);
  assert.match(spec, /03-open-session\.png/);
  assert.match(spec, /04-session-view\.png/);
});

test('buildSpec video: emits __stills.push with fileName and label', () => {
  const spec = buildSpec({ recipe: labelledVideoRecipe, outputDir: '/tmp/out', surface: 'web' });
  assert.match(spec, /__stills\.push\(\{ fileName: '01-open-home\.png', label: 'Open home' \}\)/);
  assert.match(
    spec,
    /__stills\.push\(\{ fileName: '02-sessions-list\.png', label: 'Sessions list' \}\)/,
  );
  assert.match(
    spec,
    /__stills\.push\(\{ fileName: '03-open-session\.png', label: 'Open session' \}\)/,
  );
  assert.match(
    spec,
    /__stills\.push\(\{ fileName: '04-session-view\.png', label: 'Session view' \}\)/,
  );
});

test('buildSpec video: writes video-meta.json with cropHeight and stills', () => {
  const spec = buildSpec({ recipe: labelledVideoRecipe, outputDir: '/tmp/out', surface: 'web' });
  assert.match(spec, /video-meta\.json/);
  assert.match(spec, /cropHeight/);
  assert.match(spec, /__stills/);
});

test('buildSpec video: preserves the tallest measured crop height across stills', () => {
  const spec = buildSpec({ recipe: labelledVideoRecipe, outputDir: '/tmp/out', surface: 'web' });
  assert.match(spec, /__cropHeight = Math\.max\(__cropHeight \?\? 0, __h\)/);
  assert.doesNotMatch(spec, /__cropHeight = __h;/);
});

test('buildSpec video: disables crop metadata after any full-frame still fallback', () => {
  const spec = buildSpec({ recipe: labelledVideoRecipe, outputDir: '/tmp/out', surface: 'web' });
  assert.match(spec, /let __disableCrop = false;/);
  assert.match(spec, /if \(__h === null\) __disableCrop = true;/);
  assert.match(spec, /cropHeight: __disableCrop \? null : __cropHeight/);
});

test('buildSpec video: uses recipe cropToSelector when present', () => {
  const spec = buildSpec({ recipe: labelledVideoRecipe, outputDir: '/tmp/out', surface: 'web' });
  assert.match(spec, /\.data-table-wrap/);
});

test('buildSpec video: uses #root default for web surface without cropToSelector', () => {
  const recipe = {
    id: 'web-flow',
    surface: 'web',
    capture: 'video',
    viewport: { width: 1440, height: 1000 },
    steps: [{ action: 'goto', route: '/', label: 'Open home' }],
  };
  const spec = buildSpec({ recipe, outputDir: '/tmp/out', surface: 'web' });
  assert.match(spec, /'#root'/);
});

test('buildSpec video: uses main default for marketing surface without cropToSelector', () => {
  const recipe = {
    id: 'mkt-flow',
    surface: 'marketing',
    capture: 'video',
    viewport: { width: 1440, height: 1000 },
    steps: [{ action: 'goto', route: '/', label: 'Open home' }],
  };
  const spec = buildSpec({ recipe, outputDir: '/tmp/out', surface: 'marketing' });
  assert.match(spec, /'main'/);
});

// ── validateRecipe: label field ───────────────────────────────────────────────

test('validateRecipe: video step with label: "" throws', () => {
  assert.throws(
    () =>
      validateRecipe({
        id: 'v',
        surface: 'web',
        capture: 'video',
        steps: [{ action: 'goto', route: '/', label: '' }],
      }),
    /video step label must be a non-empty string/,
  );
});

test('validateRecipe: video step with label: "X" passes', () => {
  assert.doesNotThrow(() =>
    validateRecipe({
      id: 'v',
      surface: 'web',
      capture: 'video',
      steps: [{ action: 'goto', route: '/', label: 'X' }],
    }),
  );
});

test('validateRecipe: video step without label passes', () => {
  assert.doesNotThrow(() =>
    validateRecipe({
      id: 'v',
      surface: 'web',
      capture: 'video',
      steps: [{ action: 'goto', route: '/' }],
    }),
  );
});

function runRunner(args) {
  return spawnSync(process.execPath, [runnerPath, ...args], {
    cwd: repoRoot,
    encoding: 'utf8',
    timeout: 5000,
  });
}
