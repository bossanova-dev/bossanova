#!/usr/bin/env node

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'

const repoRoot = path.resolve(path.dirname(new URL(import.meta.url).pathname), '..')

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
]

test('go module Makefiles include the shared Go test rules', () => {
  for (const file of goModuleMakefiles) {
    const text = fs.readFileSync(path.join(repoRoot, file), 'utf8')
    assert.match(
      text,
      /^include \$\(dir \$\(lastword \$\(MAKEFILE_LIST\)\)\)\.\.\/\.\.\/mk\/go-test\.mk$/m,
      file,
    )
  }
})

test('go module Makefiles do not duplicate the shared go test command', () => {
  for (const file of goModuleMakefiles) {
    const text = fs.readFileSync(path.join(repoRoot, file), 'utf8')
    assert.doesNotMatch(
      text,
      /go test \$\(RACE_FLAG\) -timeout 300s -coverprofile=coverage\.out \.\/\.\.\./,
      file,
    )
  }
})

test('shared Go test rules keep coverage in test-all but out of the fast default test', () => {
  const moduleDir = path.join(repoRoot, 'lib/bossalib')
  // BOS-373: `test` is now the fast default (-short, no coverage); `test-all` is the
  // exhaustive coverage run (the old `test`); `test-fast` stays a fast alias.
  const fullTest = execFileSync('make', ['-n', '-C', moduleDir, 'test-all'], { encoding: 'utf8' })
  const fastTest = execFileSync('make', ['-n', '-C', moduleDir, 'test'], { encoding: 'utf8' })
  const fastAlias = execFileSync('make', ['-n', '-C', moduleDir, 'test-fast'], { encoding: 'utf8' })

  assert.match(fullTest, /go test .* -coverprofile=coverage\.out \.\/\.\.\./)

  assert.match(fastTest, /go test .* -short .* \.\/\.\.\./)
  assert.doesNotMatch(fastTest, /-coverprofile=coverage\.out/)

  assert.match(fastAlias, /go test .* -short .* \.\/\.\.\./)
  assert.doesNotMatch(fastAlias, /-coverprofile=coverage\.out/)
})

test('go module Makefiles can be dry-run from the repo root with -f', () => {
  const makefiles = [
    'lib/bossalib/Makefile',
    'services/bossd/Makefile',
    'plugins/bossd-plugin-claude/Makefile',
  ]

  for (const file of makefiles) {
    const moduleDir = path.dirname(file)

    // BOS-373: the exhaustive coverage run moved from `test` to `test-all`.
    const fullOutput = execFileSync('make', ['-n', '-f', file, 'test-all'], {
      cwd: repoRoot,
      encoding: 'utf8',
    })
    assert.match(
      fullOutput,
      new RegExp(
        `cd ["']?${moduleDir}/?["']? && go test .* -coverprofile=coverage\\.out \\.\\/\\.\\.\\.`,
      ),
      file,
    )

    // The fast default `test` runs `-short` with no coverage profile.
    const fastOutput = execFileSync('make', ['-n', '-f', file, 'test'], {
      cwd: repoRoot,
      encoding: 'utf8',
    })
    assert.match(
      fastOutput,
      new RegExp(`cd ["']?${moduleDir}/?["']? && go test .* -short .* \\.\\/\\.\\.\\.`),
      file,
    )
    assert.doesNotMatch(fastOutput, /-coverprofile=coverage\.out/, file)
  }
})
