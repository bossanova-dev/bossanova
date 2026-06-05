#!/usr/bin/env node

import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

import {
  checkDocMakeTargets,
  discoverSkillDocs,
  expandGeneratedTargets,
  extractDocumentedMakeInvocations,
  extractDocumentedTargets,
  extractMakefileTargets,
  findUndefinedDocumentedTargets,
} from './check-doc-make-targets.mjs';

test('extractDocumentedTargets pulls targets from fenced code blocks', () => {
  const doc = ['```bash', 'make deps', 'make build   # comment', '```'].join('\n');

  assert.deepEqual([...extractDocumentedTargets(doc)].sort(), ['build', 'deps']);
});

test('extractDocumentedTargets pulls every target from multi-goal make commands', () => {
  const doc = ['```bash', 'make lint test-boss build-codex', '```'].join('\n');

  assert.deepEqual([...extractDocumentedTargets(doc)].sort(), ['build-codex', 'lint', 'test-boss']);
});

test('extractDocumentedTargets pulls targets after make options', () => {
  const doc = ['```bash', 'make -j test-boss lint', 'make -C scripts lint-scripts', '```'].join(
    '\n',
  );

  assert.deepEqual([...extractDocumentedTargets(doc)].sort(), [
    'lint',
    'lint-scripts',
    'test-boss',
  ]);
});

test('extractDocumentedMakeInvocations records -C directories for following targets', () => {
  const doc = ['```bash', 'make -C scripts lint test', 'make --directory=docs build', '```'].join(
    '\n',
  );

  assert.deepEqual(extractDocumentedMakeInvocations(doc), [
    { directory: 'scripts', makefile: 'scripts/Makefile', target: 'lint' },
    { directory: 'scripts', makefile: 'scripts/Makefile', target: 'test' },
    { directory: 'docs', makefile: 'docs/Makefile', target: 'build' },
  ]);
});

test('extractDocumentedMakeInvocations records alternate makefiles for following targets', () => {
  const doc = [
    '```bash',
    'make -f scripts/Makefile lint',
    'make -C scripts -f Makefile test',
    '```',
  ].join('\n');

  assert.deepEqual(extractDocumentedMakeInvocations(doc), [
    { directory: '.', makefile: 'scripts/Makefile', target: 'lint' },
    { directory: 'scripts', makefile: 'scripts/Makefile', target: 'test' },
  ]);
});

test('extractDocumentedMakeInvocations records attached short option values', () => {
  const doc = ['```bash', 'make -Cscripts lint', 'make -fscripts/Makefile test', '```'].join('\n');

  assert.deepEqual(extractDocumentedMakeInvocations(doc), [
    { directory: 'scripts', makefile: 'scripts/Makefile', target: 'lint' },
    { directory: '.', makefile: 'scripts/Makefile', target: 'test' },
  ]);
});

test('extractDocumentedMakeInvocations preserves alternate makefiles when -C appears later', () => {
  const doc = [
    '```bash',
    'make -f Alt.mk -C scripts alt',
    'make --file=Alt.mk --directory=scripts lint',
    'make -fAlt.mk -Cscripts test',
    '```',
  ].join('\n');

  assert.deepEqual(extractDocumentedMakeInvocations(doc), [
    { directory: 'scripts', makefile: 'scripts/Alt.mk', target: 'alt' },
    { directory: 'scripts', makefile: 'scripts/Alt.mk', target: 'lint' },
    { directory: 'scripts', makefile: 'scripts/Alt.mk', target: 'test' },
  ]);
});

test('extractDocumentedMakeInvocations applies global make options to earlier targets', () => {
  const doc = [
    '```bash',
    'make lint -C scripts test',
    'make lint --file=scripts/Makefile test',
    '```',
  ].join('\n');

  assert.deepEqual(extractDocumentedMakeInvocations(doc), [
    { directory: 'scripts', makefile: 'scripts/Makefile', target: 'lint' },
    { directory: 'scripts', makefile: 'scripts/Makefile', target: 'test' },
    { directory: '.', makefile: 'scripts/Makefile', target: 'lint' },
    { directory: '.', makefile: 'scripts/Makefile', target: 'test' },
  ]);
});

test('extractDocumentedTargets pulls targets from inline code spans', () => {
  const doc = 'Per-module targets exist (e.g. `make test-boss`, `make lint-bossd`).';

  assert.deepEqual([...extractDocumentedTargets(doc)].sort(), ['lint-bossd', 'test-boss']);
});

test('extractDocumentedTargets ignores "make" in prose, not in code', () => {
  const doc = 'Be sure to make sure it works, then run `make test` to make certain.';

  assert.deepEqual([...extractDocumentedTargets(doc)], ['test']);
});

test('extractDocumentedTargets ignores bare `make` with no target', () => {
  const doc = 'Run `make` for the default build, or `make lint` to lint.';

  assert.deepEqual([...extractDocumentedTargets(doc)], ['lint']);
});

test('extractMakefileTargets collects .PHONY entries across line continuations', () => {
  const makefile = [
    '.PHONY: all build \\',
    '\tlint test',
    '',
    'something-else: dep',
    '\techo hi',
  ].join('\n');

  const targets = extractMakefileTargets(makefile);

  for (const name of ['all', 'build', 'lint', 'test', 'something-else']) {
    assert.ok(targets.has(name), `expected target ${name}`);
  }
});

test('extractMakefileTargets collects explicit rule heads', () => {
  const makefile = [
    'test-boss:',
    '\t$(MAKE) -C services/boss test',
    '',
    'lint-bossd: lint-check-version',
    '\techo lint',
  ].join('\n');

  const targets = extractMakefileTargets(makefile);

  assert.ok(targets.has('test-boss'));
  assert.ok(targets.has('lint-bossd'));
});

test('extractMakefileTargets ignores variable assignments and pattern/computed rules', () => {
  const makefile = [
    'BIN_DIR := bin',
    'GOLANGCI_LINT_VERSION := v2.11.4',
    '$(BIN_DIR)/boss: $(GEN_STAMP)',
    '\tgo build',
    '$(BIN_DIR)/bossd-plugin-%: $(GEN_STAMP)',
    '\tgo build',
  ].join('\n');

  const targets = extractMakefileTargets(makefile);

  assert.equal(targets.has('BIN_DIR'), false);
  assert.equal(targets.has('GOLANGCI_LINT_VERSION'), false);
  assert.equal(targets.size, 0);
});

test('extractMakefileTargets skips rule heads inside inactive wildcard conditionals', () => {
  const repoRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'check-doc-make-targets-'));
  const makefile = [
    'always:',
    '\t@true',
    'ifneq ($(wildcard optional/go.mod),)',
    'optional-target:',
    '\t@true',
    'endif',
  ].join('\n');

  const targets = extractMakefileTargets(makefile, repoRoot);

  assert.ok(targets.has('always'));
  assert.equal(targets.has('optional-target'), false);
});

test('extractMakefileTargets keeps rule heads inside active wildcard conditionals', () => {
  const repoRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'check-doc-make-targets-'));
  fs.mkdirSync(path.join(repoRoot, 'optional'));
  fs.writeFileSync(path.join(repoRoot, 'optional', 'go.mod'), 'module optional\n');
  const makefile = [
    'ifneq ($(wildcard optional/go.mod),)',
    'optional-target:',
    '\t@true',
    'endif',
  ].join('\n');

  const targets = extractMakefileTargets(makefile, repoRoot);

  assert.ok(targets.has('optional-target'));
});

test('expandGeneratedTargets adds per-plugin targets for present define-plugin macros', () => {
  const makefile = [
    'define define-plugin-test',
    'test-$(2):',
    '\t$$(MAKE) -C $(1) test',
    'endef',
    'define define-plugin-lint',
    'lint-$(2): lint-check-version',
    '\tgolangci-lint run',
    'endef',
  ].join('\n');

  const generated = expandGeneratedTargets(makefile, ['claude', 'codex']);

  for (const name of ['test-claude', 'test-codex', 'lint-claude', 'lint-codex']) {
    assert.ok(generated.has(name), `expected generated target ${name}`);
  }
  // No define-plugin-build macro present, so build-* must not be synthesized.
  assert.equal(generated.has('build-claude'), false);
});

test('findUndefinedDocumentedTargets returns documented targets missing from the Makefile', () => {
  const documented = new Set(['test', 'lint', 'ghost']);
  const defined = new Set(['test', 'lint', 'build']);

  assert.deepEqual(findUndefinedDocumentedTargets(documented, defined), ['ghost']);
});

test('discoverSkillDocs finds checked-in skill SKILL.md files recursively', () => {
  const repoRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'check-doc-make-targets-'));
  fs.mkdirSync(path.join(repoRoot, '.claude', 'skills', 'alpha'), { recursive: true });
  fs.mkdirSync(path.join(repoRoot, '.claude', 'skills', 'beta', 'nested'), { recursive: true });
  fs.writeFileSync(path.join(repoRoot, '.claude', 'skills', 'alpha', 'SKILL.md'), '# alpha\n');
  fs.writeFileSync(path.join(repoRoot, '.claude', 'skills', 'beta', 'nested', 'SKILL.md'), '# b\n');
  // Non-SKILL files are ignored.
  fs.writeFileSync(path.join(repoRoot, '.claude', 'skills', 'beta', 'NOTES.md'), '# notes\n');

  assert.deepEqual(
    discoverSkillDocs(repoRoot).map((p) => path.relative(repoRoot, p)),
    [
      path.join('.claude', 'skills', 'alpha', 'SKILL.md'),
      path.join('.claude', 'skills', 'beta', 'nested', 'SKILL.md'),
    ],
  );
});

test('discoverSkillDocs returns empty when no skills directory exists', () => {
  const repoRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'check-doc-make-targets-'));

  assert.deepEqual(discoverSkillDocs(repoRoot), []);
});

test('checkDocMakeTargets flags dead make targets referenced in skill docs', () => {
  const repoRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'check-doc-make-targets-'));
  fs.mkdirSync(path.join(repoRoot, '.claude', 'skills', 'demo'), { recursive: true });
  fs.writeFileSync(path.join(repoRoot, 'Makefile'), 'test:\n\t@true\n');
  fs.writeFileSync(path.join(repoRoot, 'CLAUDE.md'), '');
  fs.writeFileSync(path.join(repoRoot, 'AGENTS.md'), '');
  fs.writeFileSync(path.join(repoRoot, 'README.md'), '');
  fs.writeFileSync(
    path.join(repoRoot, '.claude', 'skills', 'demo', 'SKILL.md'),
    'Run `make test`, then `make ghost`.\n',
  );

  assert.equal(checkDocMakeTargets(repoRoot), false);
});

test('checkDocMakeTargets passes when skill docs only use defined targets', () => {
  const repoRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'check-doc-make-targets-'));
  fs.mkdirSync(path.join(repoRoot, '.claude', 'skills', 'demo'), { recursive: true });
  fs.writeFileSync(path.join(repoRoot, 'Makefile'), 'test:\n\t@true\nlint:\n\t@true\n');
  fs.writeFileSync(path.join(repoRoot, 'CLAUDE.md'), '');
  fs.writeFileSync(path.join(repoRoot, 'AGENTS.md'), '');
  fs.writeFileSync(path.join(repoRoot, 'README.md'), '');
  fs.writeFileSync(
    path.join(repoRoot, '.claude', 'skills', 'demo', 'SKILL.md'),
    'Run `make test` and `make lint`.\n',
  );

  assert.equal(checkDocMakeTargets(repoRoot), true);
});

test('repository docs only reference make targets the Makefile defines', () => {
  const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

  assert.equal(checkDocMakeTargets(repoRoot), true);
});

test('script workflow runs the doc-target guard when checked docs change', () => {
  const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
  const workflow = fs.readFileSync(
    path.join(repoRoot, '.github/workflows/test-scripts.yml'),
    'utf8',
  );

  for (const doc of ['README.md', 'CLAUDE.md', 'AGENTS.md']) {
    assert.equal(workflow.match(new RegExp(`- ${doc}`, 'g'))?.length, 2);
  }

  // Skill docs are scanned too, so a skill edit must also trigger the guard
  // (once in the push paths filter, once in the dorny/paths-filter block).
  assert.equal(workflow.split('- .claude/skills/**').length - 1, 2);
});

test('checkDocMakeTargets validates -C targets against that directory Makefile', () => {
  const repoRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'check-doc-make-targets-'));
  fs.mkdirSync(path.join(repoRoot, 'scripts'));
  fs.mkdirSync(path.join(repoRoot, 'plugins'));
  fs.writeFileSync(path.join(repoRoot, 'Makefile'), 'build:\n\t@true\n');
  fs.writeFileSync(path.join(repoRoot, 'scripts', 'Makefile'), 'lint:\n\t@true\n');
  fs.writeFileSync(path.join(repoRoot, 'CLAUDE.md'), 'Run `make -C scripts lint`.\n');
  fs.writeFileSync(path.join(repoRoot, 'AGENTS.md'), '');
  fs.writeFileSync(path.join(repoRoot, 'README.md'), '');

  assert.equal(checkDocMakeTargets(repoRoot), true);
});

test('checkDocMakeTargets validates -f targets against the selected Makefile', () => {
  const repoRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'check-doc-make-targets-'));
  fs.mkdirSync(path.join(repoRoot, 'scripts'));
  fs.mkdirSync(path.join(repoRoot, 'plugins'));
  fs.writeFileSync(path.join(repoRoot, 'Makefile'), 'build:\n\t@true\n');
  fs.writeFileSync(path.join(repoRoot, 'scripts', 'Makefile'), 'lint:\n\t@true\n');
  fs.writeFileSync(path.join(repoRoot, 'CLAUDE.md'), 'Run `make -f scripts/Makefile lint`.\n');
  fs.writeFileSync(path.join(repoRoot, 'AGENTS.md'), '');
  fs.writeFileSync(path.join(repoRoot, 'README.md'), '');

  assert.equal(checkDocMakeTargets(repoRoot), true);
});

test('checkDocMakeTargets only synthesizes targets for plugin modules', () => {
  const repoRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'check-doc-make-targets-'));
  fs.mkdirSync(path.join(repoRoot, 'plugins', 'bossd-plugin-real'), { recursive: true });
  fs.mkdirSync(path.join(repoRoot, 'plugins', 'bossd-plugin-ghost'), { recursive: true });
  fs.writeFileSync(path.join(repoRoot, 'plugins', 'bossd-plugin-real', 'go.mod'), 'module real\n');
  fs.writeFileSync(
    path.join(repoRoot, 'Makefile'),
    ['define define-plugin-test', 'test-$(2):', '\t@true', 'endef', 'test:', '\t@true'].join('\n'),
  );
  fs.writeFileSync(path.join(repoRoot, 'CLAUDE.md'), 'Run `make test-ghost`.\n');
  fs.writeFileSync(path.join(repoRoot, 'AGENTS.md'), '');
  fs.writeFileSync(path.join(repoRoot, 'README.md'), '');

  assert.equal(checkDocMakeTargets(repoRoot), false);
});
