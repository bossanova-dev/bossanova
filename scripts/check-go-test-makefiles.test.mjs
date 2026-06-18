#!/usr/bin/env node

import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';

const repoRoot = path.resolve(path.dirname(new URL(import.meta.url).pathname), '..');

const goModuleMakefiles = [
  'lib/bossalib/Makefile',
  'services/boss/Makefile',
  'services/bossd/Makefile',
  'services/bosso/Makefile',
  'plugins/bossd-plugin-claude/Makefile',
  'plugins/bossd-plugin-codex/Makefile',
  'plugins/bossd-plugin-dependabot/Makefile',
  'plugins/bossd-plugin-linear/Makefile',
  'plugins/bossd-plugin-repair/Makefile',
  'plugins/bossd-plugin-sentry/Makefile',
];

test('go module Makefiles include the shared Go test rules', () => {
  for (const file of goModuleMakefiles) {
    const text = fs.readFileSync(path.join(repoRoot, file), 'utf8');
    assert.match(
      text,
      /^include \$\(dir \$\(lastword \$\(MAKEFILE_LIST\)\)\)\.\.\/\.\.\/mk\/go-test\.mk$/m,
      file,
    );
  }
});

test('go module Makefiles do not duplicate the shared go test command', () => {
  for (const file of goModuleMakefiles) {
    const text = fs.readFileSync(path.join(repoRoot, file), 'utf8');
    assert.doesNotMatch(
      text,
      /go test \$\(RACE_FLAG\) -timeout 300s -coverprofile=coverage\.out \.\/\.\.\./,
      file,
    );
  }
});

test('shared Go test rules keep coverage out of fast test dry runs', () => {
  const moduleDir = path.join(repoRoot, 'lib/bossalib');
  const fullTest = execFileSync('make', ['-n', '-C', moduleDir, 'test'], { encoding: 'utf8' });
  const fastTest = execFileSync('make', ['-n', '-C', moduleDir, 'test-fast'], { encoding: 'utf8' });

  assert.match(fullTest, /go test .* -coverprofile=coverage\.out \.\/\.\.\./);
  assert.match(fastTest, /go test .* -short .* \.\/\.\.\./);
  assert.doesNotMatch(fastTest, /-coverprofile=coverage\.out/);
});

test('go module Makefiles can be dry-run from the repo root with -f', () => {
  const makefiles = [
    'lib/bossalib/Makefile',
    'services/bossd/Makefile',
    'plugins/bossd-plugin-claude/Makefile',
  ];

  for (const file of makefiles) {
    const output = execFileSync('make', ['-n', '-f', file, 'test'], {
      cwd: repoRoot,
      encoding: 'utf8',
    });

    const moduleDir = path.dirname(file);
    assert.match(
      output,
      new RegExp(
        `cd ["']?${moduleDir}/?["']? && go test .* -coverprofile=coverage\\.out \\.\\/\\.\\.\\.`,
      ),
      file,
    );
  }
});
