import assert from 'node:assert/strict';
import { test } from 'node:test';

import { prefixStillFileNames, shouldCleanupRunDir } from './proof.mjs';

test('prefixStillFileNames prefixes recipeId onto each fileName', () => {
  const result = prefixStillFileNames('web-flow', [{ fileName: '01-a.png', label: 'A' }]);
  assert.deepEqual(result, [{ fileName: 'web-flow/01-a.png', label: 'A' }]);
});

test('prefixStillFileNames returns [] for an empty array', () => {
  assert.deepEqual(prefixStillFileNames('x', []), []);
});

test('prefixStillFileNames returns [] when stills is undefined', () => {
  assert.deepEqual(prefixStillFileNames('x', undefined), []);
});

test('prefixStillFileNames returns [] when stills is null', () => {
  assert.deepEqual(prefixStillFileNames('x', null), []);
});

test('prefixStillFileNames handles multiple stills', () => {
  const stills = [
    { fileName: '01-open.png', label: 'Open' },
    { fileName: '02-close.png', label: 'Close' },
  ];
  const result = prefixStillFileNames('my-recipe', stills);
  assert.deepEqual(result, [
    { fileName: 'my-recipe/01-open.png', label: 'Open' },
    { fileName: 'my-recipe/02-close.png', label: 'Close' },
  ]);
});

test('shouldCleanupRunDir: clean only on a real successful post', () => {
  const ok = { shouldUpload: true, hasFailure: false, prNumber: '788', keepWebm: false };
  assert.equal(shouldCleanupRunDir(ok), true);
  assert.equal(shouldCleanupRunDir({ ...ok, hasFailure: true }), false); // keep for debugging
  assert.equal(shouldCleanupRunDir({ ...ok, shouldUpload: false }), false); // dry-run keeps
  assert.equal(shouldCleanupRunDir({ ...ok, prNumber: 'local' }), false); // no PR, keep
  assert.equal(shouldCleanupRunDir({ ...ok, keepWebm: true }), false); // inspectable run
});
