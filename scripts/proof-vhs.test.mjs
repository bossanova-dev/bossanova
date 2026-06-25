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

test('buildTape hides the launcher preamble and slows playback', () => {
  const tape = buildTape({
    recipe: { id: 'tui-x', steps: [{ keys: ['n'] }] },
    launcherCmd: 'proof/tui/run-fixture.sh demo',
    outputPath: '/tmp/out.webm',
  });
  const lines = tape.split('\n');
  const hideIdx = lines.indexOf('Hide');
  const typeIdx = lines.findIndex((l) => l.startsWith('Type "proof/tui/run-fixture.sh demo"'));
  const showIdx = lines.indexOf('Show');
  assert.ok(hideIdx >= 0 && typeIdx > hideIdx, 'Hide precedes the launcher Type');
  assert.ok(showIdx > typeIdx, 'Show comes after the launcher boot');
  assert.match(tape, /Set PlaybackSpeed 0\.65/);
});

test('buildTape default frameDelay is the slower 650ms', () => {
  const tape = buildTape({
    recipe: { id: 'tui-x', steps: [{ keys: ['n'] }] },
    launcherCmd: 'x',
    outputPath: '/tmp/o.webm',
  });
  assert.match(tape, /Sleep 650ms/);
});
