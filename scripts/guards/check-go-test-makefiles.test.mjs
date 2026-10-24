#!/usr/bin/env node

import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

const repoRoot = path.resolve(path.dirname(new URL(import.meta.url).pathname), '../..')

const goModuleMakefiles = [
  'lib/bossalib/Makefile',
  'services/boss/Makefile',
  'services/bossd/Makefile',
  'services/bosso/Makefile',
  'plugins/bossd-plugin-claude/Makefile',
  'plugins/bossd-plugin-codex/Makefile',
  'plugins/bossd-plugin-dependabot/Makefile',
  'plugins/bossd-plugin-linear/Makefile',
  'plugins/bossd-plugin-opencode/Makefile',
  'plugins/bossd-plugin-repair/Makefile',
  'plugins/bossd-plugin-sentry/Makefile',
  'plugins/bossd-plugin-stub-runner/Makefile',
]

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

// The gate: each listed module Makefile, dry-run from `root` with -f, runs the shared Go test
// rules — coverage in `test-all`, `-short` with no coverage in `test`.
function assertMakefilesDryRunFromRoot(root, makefiles) {
  for (const file of makefiles) {
    const moduleDir = path.dirname(file)

    // BOS-373: the exhaustive coverage run moved from `test` to `test-all`.
    const fullOutput = execFileSync('make', ['-n', '-f', file, 'test-all'], {
      cwd: root,
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
      cwd: root,
      encoding: 'utf8',
    })
    assert.match(
      fastOutput,
      new RegExp(`cd ["']?${moduleDir}/?["']? && go test .* -short .* \\.\\/\\.\\.\\.`),
      file,
    )
    assert.doesNotMatch(fastOutput, /-coverprofile=coverage\.out/, file)
  }
}

test('go module Makefiles can be dry-run from the repo root with -f', () => {
  assertMakefilesDryRunFromRoot(repoRoot, goModuleMakefiles)
})

test('a module Makefile without the shared Go test rules fails the gate', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'go-test-makefiles-'))
  try {
    fs.mkdirSync(path.join(root, 'lib/shared'), { recursive: true })
    fs.mkdirSync(path.join(root, 'lib/missing'), { recursive: true })
    fs.writeFileSync(
      path.join(root, 'lib/shared/Makefile'),
      'test-all:\n\tcd lib/shared && go test -race -coverprofile=coverage.out ./...\n' +
        'test:\n\tcd lib/shared && go test -race -short -timeout 300s ./...\n',
    )
    fs.writeFileSync(
      path.join(root, 'lib/missing/Makefile'),
      'test-all:\n\tcd lib/missing && go test ./...\ntest:\n\tcd lib/missing && go test ./...\n',
    )
    assertMakefilesDryRunFromRoot(root, ['lib/shared/Makefile'])
    assert.throws(
      () => assertMakefilesDryRunFromRoot(root, ['lib/shared/Makefile', 'lib/missing/Makefile']),
      { message: /lib\/missing\/Makefile/ },
    )
  } finally {
    fs.rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  }
})

for (const file of goModuleMakefiles.filter((file) => file.startsWith('plugins/'))) {
  test(`${file} builds into the repository bin directory from either working directory`, () => {
    const moduleDir = path.dirname(file)
    const binary = path.join(repoRoot, 'bin', path.basename(moduleDir))
    for (const target of ['test', 'test-all']) {
      const output = execFileSync('make', ['-n', '-C', moduleDir, target], {
        cwd: repoRoot,
        encoding: 'utf8',
      })
      if (target === 'test') {
        assert.match(output, /go test .* -short /)
        assert.doesNotMatch(output, /-coverprofile=/)
      } else {
        assert.match(output, /go test .* -coverprofile=coverage\.out/)
        assert.doesNotMatch(output, /-short/)
      }
    }
    for (const args of [
      ['-C', moduleDir],
      ['-f', file],
    ]) {
      const output = execFileSync('make', ['-n', ...args, 'build'], {
        cwd: repoRoot,
        encoding: 'utf8',
      })
      assert.match(output, /go build -ldflags /)
      assert.ok(output.includes(`-o ${binary} .`), output)
      assert.match(output, /cd ".*" && go build/)
    }
  })
}
