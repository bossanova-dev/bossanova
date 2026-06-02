#!/usr/bin/env node

import assert from 'node:assert/strict';
import path from 'node:path';
import { test } from 'node:test';

import {
  extractLocalTargets,
  findMissingAssets,
  isLocalTarget,
  normalizeTarget,
} from './check-readme-assets.mjs';

test('isLocalTarget accepts relative paths and rejects remote/anchor targets', () => {
  assert.equal(isLocalTarget('docs/image.png'), true);
  assert.equal(isLocalTarget('./logo.svg'), true);

  assert.equal(isLocalTarget(''), false);
  assert.equal(isLocalTarget('#section'), false);
  assert.equal(isLocalTarget('https://example.com/a.png'), false);
  assert.equal(isLocalTarget('mailto:dev@example.com'), false);
  assert.equal(isLocalTarget('//cdn.example.com/a.png'), false);
});

test('normalizeTarget strips anchors and query strings and decodes escapes', () => {
  assert.equal(normalizeTarget('docs/page.md#heading'), 'docs/page.md');
  assert.equal(normalizeTarget('assets/a.png?v=2'), 'assets/a.png');
  assert.equal(normalizeTarget('docs/my%20file.png'), 'docs/my file.png');
});

test('extractLocalTargets pulls image, link, and <img> sources', () => {
  const readme = [
    '![alt](assets/hero.png)',
    'See [the guide](docs/guide.md) for details.',
    '<img src="assets/diagram.svg" width="40">',
  ].join('\n');

  const targets = extractLocalTargets(readme);

  assert.deepEqual(
    [...targets].sort(),
    ['assets/diagram.svg', 'assets/hero.png', 'docs/guide.md'].sort(),
  );
});

test('extractLocalTargets ignores remote URLs and bare anchors', () => {
  const readme = [
    '![remote](https://example.com/x.png)',
    '[external](https://example.com)',
    '[jump](#install)',
  ].join('\n');

  assert.equal(extractLocalTargets(readme).size, 0);
});

test('extractLocalTargets skips targets inside fenced code blocks', () => {
  const readme = [
    '```md',
    '![fenced](assets/should-be-ignored.png)',
    '```',
    '![real](assets/used.png)',
  ].join('\n');

  const targets = extractLocalTargets(readme);

  assert.deepEqual([...targets], ['assets/used.png']);
});

test('findMissingAssets reports targets that do not exist on disk', () => {
  const repoRoot = '/repo';
  const present = new Set([path.resolve(repoRoot, 'assets/used.png')]);
  const fileExists = (candidate) => present.has(candidate);

  const missing = findMissingAssets(['assets/used.png', 'assets/gone.png'], {
    repoRoot,
    fileExists,
  });

  assert.deepEqual(missing, ['assets/gone.png']);
});

test('findMissingAssets flags targets that escape the repository root', () => {
  const repoRoot = '/repo';
  // Pretend every resolved path exists so only the containment check can fail.
  const fileExists = () => true;

  const missing = findMissingAssets(['../outside-secret.png', 'assets/inside.png'], {
    repoRoot,
    fileExists,
  });

  assert.deepEqual(missing, ['../outside-secret.png']);
});
