#!/usr/bin/env node

import assert from 'node:assert/strict';
import test from 'node:test';

import { buildTape } from './proof-vhs.mjs';

test('buildTape emits Output, launcher, and per-step directives', () => {
  const tape = buildTape({
    recipe: {
      id: 'tui-flow',
      terminal: { width: 140, height: 36 },
      frameDelayMs: 400,
      steps: [
        { keys: ['n'], waitForText: 'New Session' },
        { keys: ['enter'] },
        { keys: ['ctrl+b'] },
      ],
    },
    launcherCmd: 'proof/tui/run-fixture.sh demo',
    outputPath: '/tmp/out/tui-flow.webm',
  });
  assert.match(tape, /Output "\/tmp\/out\/tui-flow\.webm"/);
  assert.match(tape, /proof\/tui\/run-fixture\.sh demo/);
  assert.match(tape, /Type "n"/);
  assert.match(tape, /\nEnter\n/);
  assert.match(tape, /Ctrl\+B/);
  assert.match(tape, /Sleep 400ms/);
});

test('buildTape throws on an unknown key token', () => {
  assert.throws(
    () =>
      buildTape({
        recipe: { id: 'x', terminal: { width: 140, height: 36 }, steps: [{ keys: ['f13'] }] },
        launcherCmd: 'x',
        outputPath: '/tmp/x.webm',
      }),
    /unsupported tape key/,
  );
});

test('buildTape rejects an invalid recipe id', () => {
  assert.throws(
    () =>
      buildTape({
        recipe: { id: 'Bad Id', terminal: { width: 140, height: 36 }, steps: [] },
        launcherCmd: 'x',
        outputPath: '/tmp/x.webm',
      }),
    /invalid recipe id/,
  );
});
