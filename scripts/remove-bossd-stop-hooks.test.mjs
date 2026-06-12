#!/usr/bin/env node

import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';

import {
  isBossdStopMatcher,
  pruneBossdStopHooks,
  removeBossdStopHooks,
} from './remove-bossd-stop-hooks.mjs';

function withTempSettings(contents, run) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'stop-hooks-'));
  const file = path.join(dir, 'settings.local.json');
  if (contents !== null) fs.writeFileSync(file, contents);
  try {
    return run(file);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

test('isBossdStopMatcher matches finalize and agent-run prefixes only', () => {
  assert.equal(isBossdStopMatcher('bossd-finalize'), true);
  assert.equal(isBossdStopMatcher('bossd-agent-run-abc123'), true);
  assert.equal(isBossdStopMatcher('user-hook'), false);
  assert.equal(isBossdStopMatcher(undefined), false);
});

test('pruneBossdStopHooks drops bossd entries and keeps the rest', () => {
  const data = {
    hooks: {
      Stop: [
        { matcher: 'bossd-finalize' },
        { matcher: 'bossd-agent-run-xyz' },
        { matcher: 'user-hook' },
      ],
    },
  };
  const [next, changed] = pruneBossdStopHooks(data);
  assert.equal(changed, true);
  assert.deepEqual(next.hooks.Stop, [{ matcher: 'user-hook' }]);
});

test('pruneBossdStopHooks is a no-op when no bossd entries present', () => {
  const data = { hooks: { Stop: [{ matcher: 'user-hook' }] } };
  const [next, changed] = pruneBossdStopHooks(data);
  assert.equal(changed, false);
  assert.equal(next, data);
});

test('pruneBossdStopHooks is a no-op when hooks.Stop is absent', () => {
  const data = { hooks: {} };
  const [, changed] = pruneBossdStopHooks(data);
  assert.equal(changed, false);
});

test('removeBossdStopHooks returns false when the file is missing', () => {
  withTempSettings(null, (file) => {
    assert.equal(removeBossdStopHooks(file), false);
  });
});

test('removeBossdStopHooks removes bossd entries and rewrites the file', () => {
  const input = JSON.stringify(
    { hooks: { Stop: [{ matcher: 'bossd-finalize' }, { matcher: 'keep' }] } },
    null,
    2,
  );
  withTempSettings(input, (file) => {
    assert.equal(removeBossdStopHooks(file), true);
    const out = JSON.parse(fs.readFileSync(file, 'utf8'));
    assert.deepEqual(out.hooks.Stop, [{ matcher: 'keep' }]);
    assert.equal(fs.readFileSync(file, 'utf8').endsWith('}\n'), true);
  });
});

test('removeBossdStopHooks is idempotent (second run makes no change)', () => {
  const input = JSON.stringify({ hooks: { Stop: [{ matcher: 'bossd-finalize' }] } }, null, 2);
  withTempSettings(input, (file) => {
    assert.equal(removeBossdStopHooks(file), true);
    assert.equal(removeBossdStopHooks(file), false);
  });
});

test('removeBossdStopHooks throws on malformed JSON (fail loud)', () => {
  withTempSettings('{ not json', (file) => {
    assert.throws(() => removeBossdStopHooks(file), SyntaxError);
  });
});
