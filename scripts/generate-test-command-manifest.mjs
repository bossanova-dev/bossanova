#!/usr/bin/env node

import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(scriptDirectory, '..');
const moduleRoots = ['lib', 'services', 'plugins'];

const defaultRootTargets = [
  'test-smoke',
  'test-affected',
  'test-full',
  'test-race',
  'test-profile',
  'test-scripts',
  'test-readme',
  'test-public-mirror',
];

function listGoModPaths(root = repoRoot) {
  return moduleRoots.flatMap((moduleRoot) => {
    const absoluteModuleRoot = path.join(root, moduleRoot);
    if (!fs.existsSync(absoluteModuleRoot)) {
      return [];
    }

    return fs
      .readdirSync(absoluteModuleRoot, { withFileTypes: true })
      .filter((entry) => entry.isDirectory())
      .map((entry) => path.posix.join(moduleRoot, entry.name, 'go.mod'))
      .filter((goModPath) => fs.existsSync(path.join(root, goModPath)));
  });
}

function targetForModule(modulePath) {
  const moduleName = path.posix.basename(modulePath);
  const targetName = modulePath.startsWith('plugins/')
    ? moduleName.replace(/^bossd-plugin-/, '')
    : moduleName;

  return `test-${targetName}`;
}

export function deriveModuleTargets(goModPaths) {
  const orderByRoot = new Map(moduleRoots.map((moduleRoot, index) => [moduleRoot, index]));

  return goModPaths
    .map((goModPath) => path.posix.dirname(goModPath))
    .sort((left, right) => {
      const [leftRoot] = left.split('/');
      const [rightRoot] = right.split('/');
      const rootOrder = (orderByRoot.get(leftRoot) ?? 99) - (orderByRoot.get(rightRoot) ?? 99);

      return rootOrder || left.localeCompare(right);
    })
    .map((modulePath) => ({
      path: modulePath,
      target: targetForModule(modulePath),
    }));
}

export function discoverModuleTargets(root = repoRoot) {
  return deriveModuleTargets(listGoModPaths(root));
}

function countTestFiles(modulePath) {
  const absoluteModulePath = path.join(repoRoot, modulePath);
  if (!fs.existsSync(absoluteModulePath)) {
    return 0;
  }

  const output = execFileSync('find', [modulePath, '-name', '*_test.go'], {
    cwd: repoRoot,
    encoding: 'utf8',
  }).trim();

  if (output === '') {
    return 0;
  }

  return output.split('\n').length;
}

function renderCommand(target) {
  return `make ${target}`;
}

function padCell(value, width, align = 'left') {
  return align === 'right' ? value.padStart(width) : value.padEnd(width);
}

function renderTable(headers, rows, alignments = []) {
  const widths = headers.map((header, index) =>
    Math.max(header.length, ...rows.map((row) => row[index].length)),
  );
  const separator = widths.map((width, index) => {
    const alignment = alignments[index] ?? 'left';
    return alignment === 'right' ? `${'-'.repeat(width - 1)}:` : '-'.repeat(width);
  });
  const renderRow = (row) =>
    `| ${row.map((cell, index) => padCell(cell, widths[index], alignments[index])).join(' | ')} |`;

  return [renderRow(headers), renderRow(separator), ...rows.map(renderRow)];
}

export function renderManifest({ rootTargets, modules }) {
  const ladderRows = [
    ['Fast local confidence', '`make test-smoke`'],
    ['Changed-file selection', '`make test-affected`'],
    ['Full test suite', '`make test-full`'],
    ['Race detector pass', '`make test-race`'],
    ['Slow-test profiling', '`make test-profile`'],
  ];
  const moduleRows = modules.map((module) => [
    `\`${module.path}\``,
    `\`${renderCommand(module.target)}\``,
    String(module.testFiles),
  ]);
  const lines = [
    '# Test Command Manifest',
    '',
    'Generated from repository Make targets and module layout. Update with `make test-manifest-update`.',
    '',
    '## Agent Command Ladder',
    '',
    ...renderTable(['Use case', 'Command'], ladderRows),
    '',
    '## Root Test Targets',
    '',
    ...rootTargets.map((target) => `- \`${renderCommand(target)}\``),
    '',
    '## Go Module Targets',
    '',
    ...renderTable(['Module', 'Target', 'Test files'], moduleRows, ['left', 'left', 'right']),
    '',
  ];

  return lines.join('\n');
}

function buildManifest() {
  return renderManifest({
    rootTargets: defaultRootTargets,
    modules: discoverModuleTargets().map((module) => ({
      ...module,
      testFiles: countTestFiles(module.path),
    })),
  });
}

if (import.meta.url === `file://${process.argv[1]}`) {
  process.stdout.write(buildManifest());
}
