#!/usr/bin/env node

import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { afterEach, test } from 'node:test';
import { fileURLToPath } from 'node:url';

const scriptPath = path.join(path.dirname(fileURLToPath(import.meta.url)), 'trim-cast.mjs');
const tempDirs = [];

afterEach(() => {
  for (const dir of tempDirs.splice(0)) {
    fs.rmSync(dir, { force: true, recursive: true });
  }
});

function makeTempDir() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'trim-cast-'));
  tempDirs.push(dir);
  return dir;
}

function writeCast(filePath, header, events) {
  const lines = [JSON.stringify(header), ...events.map((event) => JSON.stringify(event))];
  fs.writeFileSync(filePath, `${lines.join('\n')}\n`, 'utf8');
}

function readCast(filePath) {
  return fs
    .readFileSync(filePath, 'utf8')
    .trimEnd()
    .split('\n')
    .map((line) => JSON.parse(line));
}

function runTrim(inputPath, outputPath, needle) {
  return spawnSync(process.execPath, [scriptPath, inputPath, outputPath, needle], {
    encoding: 'utf8',
  });
}

test('trims v2 asciicast events from first matching terminal output', () => {
  const dir = makeTempDir();
  const inputPath = path.join(dir, 'input.cast');
  const outputPath = path.join(dir, 'output.cast');

  writeCast(inputPath, { version: 2, width: 80, height: 24 }, [
    [0.5, 'o', 'boot'],
    [2.5, 'o', 'ready prompt'],
    [4.0, 'i', 'input'],
    [5.0, 'o', 'done'],
  ]);

  const result = runTrim(inputPath, outputPath, 'ready');

  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(readCast(outputPath), [
    { version: 2, width: 80, height: 24 },
    [0, 'o', 'ready prompt'],
    [1.5, 'i', 'input'],
    [2.5, 'o', 'done'],
  ]);
});

test('trims v3 asciicast events from first matching terminal output', () => {
  const dir = makeTempDir();
  const inputPath = path.join(dir, 'input.cast');
  const outputPath = path.join(dir, 'output.cast');

  writeCast(inputPath, { version: 3, width: 80, height: 24 }, [
    [0.5, 'o', 'boot'],
    [2.0, 'o', 'ready prompt'],
    [1.5, 'i', 'input'],
    [1.0, 'o', 'done'],
  ]);

  const result = runTrim(inputPath, outputPath, 'ready');

  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(readCast(outputPath), [
    { version: 3, width: 80, height: 24 },
    [0, 'o', 'ready prompt'],
    [1.5, 'i', 'input'],
    [1.0, 'o', 'done'],
  ]);
});

test('fails when the target text is absent', () => {
  const dir = makeTempDir();
  const inputPath = path.join(dir, 'input.cast');
  const outputPath = path.join(dir, 'output.cast');

  writeCast(inputPath, { version: 2 }, [[0.5, 'o', 'boot']]);

  const result = runTrim(inputPath, outputPath, 'missing');

  assert.equal(result.status, 1);
  assert.match(result.stderr, /Could not find text: 'missing'/);
  assert.equal(fs.existsSync(outputPath), false);
});
