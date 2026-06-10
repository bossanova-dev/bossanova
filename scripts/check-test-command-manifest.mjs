#!/usr/bin/env node

import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(scriptDirectory, '..');
const manifestPath = path.join(repoRoot, 'docs/testing/test-command-manifest.md');

const expected = execFileSync(process.execPath, ['scripts/generate-test-command-manifest.mjs'], {
  cwd: repoRoot,
  encoding: 'utf8',
});

const actual = fs.existsSync(manifestPath) ? fs.readFileSync(manifestPath, 'utf8') : '';

if (actual !== expected) {
  console.error('docs/testing/test-command-manifest.md is stale.');
  console.error('Run: make test-manifest-update');
  process.exit(1);
}
