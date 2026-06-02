#!/usr/bin/env node

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

export function isLocalTarget(target) {
  if (!target || target.startsWith('#')) return false;
  if (/^[a-z][a-z0-9+.-]*:/i.test(target)) return false;
  if (target.startsWith('//')) return false;
  return true;
}

export function normalizeTarget(rawTarget) {
  const withoutAnchor = rawTarget.split('#')[0];
  const withoutQuery = withoutAnchor.split('?')[0];
  return decodeURIComponent(withoutQuery).trim();
}

export function extractLocalTargets(readme) {
  const localTargets = new Set();
  let inFence = false;

  for (const line of readme.split('\n')) {
    if (/^\s*```/.test(line)) {
      inFence = !inFence;
      continue;
    }
    if (inFence) continue;

    for (const match of line.matchAll(/!\[[^\]]*]\(([^)]+)\)/g)) {
      const target = normalizeTarget(match[1]);
      if (isLocalTarget(target)) localTargets.add(target);
    }

    for (const match of line.matchAll(/(?<!!)\[[^\]]+]\(([^)]+)\)/g)) {
      const target = normalizeTarget(match[1]);
      if (isLocalTarget(target)) localTargets.add(target);
    }

    for (const match of line.matchAll(/<img\b[^>]*\bsrc=["']([^"']+)["'][^>]*>/gi)) {
      const target = normalizeTarget(match[1]);
      if (isLocalTarget(target)) localTargets.add(target);
    }
  }

  return localTargets;
}

export function findMissingAssets(targets, { repoRoot, fileExists = fs.existsSync } = {}) {
  return [...targets].filter((target) => {
    const targetPath = path.resolve(repoRoot, target);
    return !targetPath.startsWith(repoRoot + path.sep) || !fileExists(targetPath);
  });
}

export function checkReadmeAssets(repoRoot = process.cwd()) {
  const readmePath = path.join(repoRoot, 'README.md');
  const readme = fs.readFileSync(readmePath, 'utf8');

  const localTargets = extractLocalTargets(readme);
  const missing = findMissingAssets(localTargets, { repoRoot });

  if (missing.length > 0) {
    console.error('README references missing local assets:');
    for (const target of missing) {
      console.error(`  - ${target}`);
    }
    process.exit(1);
  }

  console.log(`README local assets OK (${localTargets.size} checked)`);
}

const invokedDirectly =
  process.argv[1] &&
  fs.realpathSync(process.argv[1]) === fs.realpathSync(fileURLToPath(import.meta.url));

if (invokedDirectly) {
  checkReadmeAssets();
}
