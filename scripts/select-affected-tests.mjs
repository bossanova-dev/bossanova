#!/usr/bin/env node

import { execFileSync } from 'node:child_process';

const moduleRules = [
  { root: 'lib/bossalib/', target: 'test-bossalib' },
  { root: 'services/boss/', target: 'test-boss' },
  { root: 'services/bossd/', target: 'test-bossd' },
  { root: 'services/bosso/', target: 'test-bosso' },
  { root: 'plugins/bossd-plugin-claude/', target: 'test-claude' },
  { root: 'plugins/bossd-plugin-codex/', target: 'test-codex' },
  { root: 'plugins/bossd-plugin-dependabot/', target: 'test-dependabot' },
  { root: 'plugins/bossd-plugin-linear/', target: 'test-linear' },
  { root: 'plugins/bossd-plugin-repair/', target: 'test-repair' },
];

const protoTargets = ['test-bossalib', 'test-boss', 'test-bossd', 'test-bosso'];

export function selectTargets(files) {
  const selections = new Map();

  for (const rawFile of files) {
    const file = normalizePath(rawFile);
    if (file === '') {
      continue;
    }

    if (file.startsWith('proto/') && file.endsWith('.proto')) {
      for (const target of protoTargets) {
        selectWholeTarget(selections, target);
      }
      continue;
    }

    if (file.startsWith('scripts/')) {
      selectWholeTarget(selections, 'test-scripts');
      continue;
    }

    if (file.startsWith('proof/')) {
      selectWholeTarget(selections, 'test-scripts');
      continue;
    }

    if (isManifestPath(file)) {
      selectWholeTarget(selections, 'test-manifest');
      continue;
    }

    const moduleRule = moduleRules.find(({ root }) => file.startsWith(root));
    if (moduleRule) {
      selectModuleTarget(selections, moduleRule, file);
    }
  }

  if (selections.size === 0) {
    selectWholeTarget(selections, 'test-smoke');
  }

  return [...selections.values()].map((selection) => ({
    kind: 'make',
    target: selection.target,
    env: selection.wholeModule
      ? {}
      : { GO_TEST_PACKAGES: [...selection.packages].sort().join(' ') },
  }));
}

export function renderMakeCommands(targets) {
  return targets.map(({ target, env }) => {
    const envPrefix = Object.entries(env ?? {})
      .map(([key, value]) => `${key}=${shellQuote(value)}`)
      .join(' ');
    return [envPrefix, 'make', target].filter(Boolean).join(' ');
  });
}

function selectModuleTarget(selections, moduleRule, file) {
  if (
    !file.endsWith('.go') ||
    file.endsWith('/Makefile') ||
    file === `${moduleRule.root}Makefile`
  ) {
    selectWholeTarget(selections, moduleRule.target);
    return;
  }

  const relativeDir = file.slice(moduleRule.root.length).split('/').slice(0, -1).join('/');
  selectPackageTarget(selections, moduleRule.target, `./${relativeDir}`);
}

function selectWholeTarget(selections, target) {
  selections.set(target, {
    target,
    wholeModule: true,
    packages: new Set(),
  });
}

function selectPackageTarget(selections, target, packagePath) {
  const existing = selections.get(target);
  if (existing?.wholeModule) {
    return;
  }

  const selection = existing ?? {
    target,
    wholeModule: false,
    packages: new Set(),
  };
  selection.packages.add(packagePath === './' ? '.' : packagePath);
  selections.set(target, selection);
}

function isManifestPath(file) {
  return (
    file === 'AGENTS.md' ||
    file === 'CLAUDE.md' ||
    file === '.claude/docs/testing.md' ||
    file.startsWith('.claude/docs/testing/') ||
    file.startsWith('.claude/skills/') ||
    file.startsWith('.codex/skills/') ||
    file.startsWith('docs/guidance/') ||
    file.startsWith('docs/testing/')
  );
}

function normalizePath(file) {
  return String(file ?? '')
    .trim()
    .replaceAll('\\', '/')
    .replace(/^\.\//, '');
}

function shellQuote(value) {
  return `'${String(value).replaceAll("'", "'\\''")}'`;
}

function changedFilesFromGit() {
  const baseRef = process.env.BASE_REF || 'origin/main';
  const output = execFileSync('git', ['diff', '--name-only', `${baseRef}...HEAD`], {
    encoding: 'utf8',
  });
  return output.split(/\r?\n/).filter(Boolean);
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const files = process.argv.slice(2).length > 0 ? process.argv.slice(2) : changedFilesFromGit();
  console.log(renderMakeCommands(selectTargets(files)).join('\n'));
}
