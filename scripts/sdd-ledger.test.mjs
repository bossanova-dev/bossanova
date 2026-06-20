#!/usr/bin/env node
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

import {
  hashPath,
  ledgerPathFor,
  parseTaskRanges,
  renderHeader,
  renderTaskLine,
  upsertTaskLine,
} from './sdd-ledger.mjs';

const SCRIPT = fileURLToPath(new URL('./sdd-ledger.mjs', import.meta.url));
const SHA_A = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa';
const SHA_B = 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb';
const SHA_C = 'cccccccccccccccccccccccccccccccccccccccc';

test('hashPath is deterministic and 12 hex chars', () => {
  assert.equal(hashPath('/a/b'), hashPath('/a/b'));
  assert.match(hashPath('/a/b'), /^[0-9a-f]{12}$/);
  assert.notEqual(hashPath('/a/b'), hashPath('/a/c'));
});

test('ledgerPathFor keys on worktree path under the home dir', () => {
  const p = ledgerPathFor('/work/myrepo', '/home/state');
  assert.ok(p.startsWith('/home/state/linear-implement/ledger/myrepo/'));
  assert.ok(p.endsWith(`myrepo-${hashPath('/work/myrepo')}.md`));
});

test('renderTaskLine: complete vs in-progress', () => {
  assert.equal(
    renderTaskLine({ task: 3, base: SHA_A, head: SHA_B, status: 'complete', note: 'review clean' }),
    `Task 3: complete (commits ${SHA_A}..${SHA_B}, review clean)`,
  );
  assert.equal(
    renderTaskLine({ task: 4, base: SHA_C, status: 'in-progress' }),
    `Task 4: in-progress (base ${SHA_C})`,
  );
});

test('upsertTaskLine replaces an existing task line and is idempotent', () => {
  const h = renderHeader({ ticket: 'BOS-1', branch: 'b', base: 'base000' });
  const c1 = upsertTaskLine(h, 1, 'Task 1: in-progress (base base000)');
  const c2 = upsertTaskLine(c1, 1, 'Task 1: complete (commits base000..head111, review clean)');
  assert.ok(c2.includes('Task 1: complete (commits base000..head111, review clean)'));
  assert.ok(!c2.includes('Task 1: in-progress'));
  // second identical upsert changes nothing
  assert.equal(
    upsertTaskLine(c2, 1, 'Task 1: complete (commits base000..head111, review clean)'),
    c2,
  );
});

test('upsertTaskLine appends a new task line', () => {
  const h = renderHeader({ ticket: 'BOS-1', branch: 'b', base: 'base000' });
  const c = upsertTaskLine(h, 2, 'Task 2: in-progress (base xyz0000)');
  assert.ok(c.includes('Task 2: in-progress (base xyz0000)'));
});

test('parseTaskRanges extracts completed ranges only', () => {
  let c = renderHeader({ ticket: 'BOS-1', branch: 'b', base: 'base000' });
  c = upsertTaskLine(
    c,
    1,
    renderTaskLine({
      task: 1,
      base: 'aaaaaaa',
      head: 'bbbbbbb',
      status: 'complete',
      note: 'review clean',
    }),
  );
  c = upsertTaskLine(c, 2, renderTaskLine({ task: 2, base: 'ccccccc', status: 'in-progress' }));
  assert.deepEqual(parseTaskRanges(c), [{ task: 1, base: 'aaaaaaa', head: 'bbbbbbb' }]);
});

test('upsertTaskLine with a string task key is idempotent (non-numeric review-round key)', () => {
  const h = renderHeader({ ticket: 'BOS-1', branch: 'b', base: 'base000' });
  const line = 'Task review-r1: complete (commits aaaaaaa..bbbbbbb)';
  const c1 = upsertTaskLine(h, 'review-r1', line);
  // key must appear exactly once
  assert.ok(c1.includes(line));
  assert.equal(c1.split('Task review-r1:').length - 1, 1);
  // second identical upsert must be idempotent
  const c2 = upsertTaskLine(c1, 'review-r1', line);
  assert.equal(c2, c1);
});

test('parseTaskRanges extracts completed ranges for non-numeric task keys', () => {
  let c = renderHeader({ ticket: 'BOS-1', branch: 'b', base: 'base000' });
  c = upsertTaskLine(
    c,
    'review-r1',
    renderTaskLine({ task: 'review-r1', base: 'aaaaaaa', head: 'bbbbbbb', status: 'complete' }),
  );
  const ranges = parseTaskRanges(c);
  assert.deepEqual(ranges, [{ task: 'review-r1', base: 'aaaaaaa', head: 'bbbbbbb' }]);
});

test('parseTaskRanges still returns Number for numeric task keys', () => {
  let c = renderHeader({ ticket: 'BOS-1', branch: 'b', base: 'base000' });
  c = upsertTaskLine(
    c,
    1,
    renderTaskLine({ task: 1, base: 'aaaaaaa', head: 'bbbbbbb', status: 'complete' }),
  );
  const ranges = parseTaskRanges(c);
  assert.deepEqual(ranges, [{ task: 1, base: 'aaaaaaa', head: 'bbbbbbb' }]);
  assert.equal(typeof ranges[0].task, 'number');
});

test('CLI init → record → show round-trips against a temp ledger home', () => {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'bli-ledger-'));
  const env = { ...process.env, BLI_LEDGER_HOME: home };
  const run = (...args) => spawnSync('node', [SCRIPT, ...args], { encoding: 'utf8', env });

  assert.equal(
    run('init', '--ticket', 'pending', '--branch', 'x', '--base', SHA_A).stdout.trim(),
    'INIT',
  );
  // second init reconciles a pending ticket in the header instead of leaving stale state
  assert.equal(
    run('init', '--ticket', 'BOS-9', '--branch', 'x', '--base', SHA_A).stdout.trim(),
    'RESUME',
  );
  assert.equal(
    run(
      'record',
      '--task',
      '1',
      '--base',
      SHA_A,
      '--head',
      SHA_B,
      '--status',
      'complete',
      '--note',
      'review clean',
    ).stdout.trim(),
    'RECORDED',
  );

  const show = run('show').stdout;
  assert.ok(show.includes('ticket: BOS-9'));
  assert.ok(show.includes(`Task 1: complete (commits ${SHA_A}..${SHA_B}, review clean)`));
});

test('CLI pending init resumes an existing real-ticket ledger without rewriting header', () => {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'bli-ledger-'));
  const env = { ...process.env, BLI_LEDGER_HOME: home };
  const run = (...args) => spawnSync('node', [SCRIPT, ...args], { encoding: 'utf8', env });

  assert.equal(
    run('init', '--ticket', 'pending', '--branch', 'x', '--base', SHA_A).stdout.trim(),
    'INIT',
  );
  assert.equal(
    run('init', '--ticket', 'BOS-9', '--branch', 'x', '--base', SHA_A).stdout.trim(),
    'RESUME',
  );
  assert.equal(
    run(
      'record',
      '--task',
      '1',
      '--base',
      SHA_A,
      '--head',
      SHA_B,
      '--status',
      'complete',
    ).stdout.trim(),
    'RECORDED',
  );

  const resume = run('init', '--ticket', 'pending', '--branch', 'x', '--base', SHA_C);
  assert.equal(resume.status, 0);
  assert.equal(resume.stdout.trim(), 'RESUME');

  const show = run('show').stdout;
  assert.ok(show.includes('ticket: BOS-9'));
  assert.ok(show.includes(`run base: ${SHA_A}`));
  assert.doesNotMatch(show, new RegExp(`run base: ${SHA_C}`));
});

test('CLI record rejects malformed inputs without writing a task line', () => {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'bli-ledger-'));
  const env = { ...process.env, BLI_LEDGER_HOME: home };
  const run = (...args) => spawnSync('node', [SCRIPT, ...args], { encoding: 'utf8', env });

  assert.equal(
    run('init', '--ticket', 'BOS-9', '--branch', 'x', '--base', SHA_A).stdout.trim(),
    'INIT',
  );

  const missingHead = run('record', '--task', '1', '--base', SHA_A, '--status', 'complete');
  assert.notEqual(missingHead.status, 0);
  assert.match(missingHead.stderr, /missing required flag: --head/);

  const badStatus = run(
    'record',
    '--task',
    '2',
    '--base',
    SHA_A,
    '--head',
    SHA_B,
    '--status',
    'done',
  );
  assert.notEqual(badStatus.status, 0);
  assert.match(badStatus.stderr, /invalid --status/);

  const show = run('show').stdout;
  assert.doesNotMatch(show, /Task 1/);
  assert.doesNotMatch(show, /Task 2/);
});

test('CLI init rejects an existing ledger for a different ticket', () => {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'bli-ledger-'));
  const env = { ...process.env, BLI_LEDGER_HOME: home };
  const run = (...args) => spawnSync('node', [SCRIPT, ...args], { encoding: 'utf8', env });

  assert.equal(
    run('init', '--ticket', 'BOS-9', '--branch', 'x', '--base', SHA_A).stdout.trim(),
    'INIT',
  );
  const result = run('init', '--ticket', 'BOS-10', '--branch', 'x', '--base', SHA_A);

  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /ledger header mismatch: ticket/);
});
