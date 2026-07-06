#!/usr/bin/env node

import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

import { renderMakeCommands, selectTargets } from './select-affected-tests.mjs'

const repoRoot = path.dirname(fileURLToPath(new URL('../Makefile', import.meta.url)))
const makefilePath = path.join(repoRoot, 'Makefile')

test('selectTargets scopes bossd Go changes to the changed package', () => {
  assert.deepEqual(selectTargets(['services/bossd/internal/session/lifecycle.go']), [
    {
      kind: 'make',
      target: 'test-bossd',
      env: { GO_TEST_PACKAGES: './internal/session' },
    },
  ])
})

test('selectTargets scopes sentry plugin Go changes to the changed package', () => {
  assert.deepEqual(selectTargets(['plugins/bossd-plugin-sentry/sentry.go']), [
    {
      kind: 'make',
      target: 'test-sentry',
      env: { GO_TEST_PACKAGES: '.' },
    },
  ])
})

test('selectTargets runs the whole module for module Makefile changes', () => {
  assert.deepEqual(selectTargets(['services/bossd/Makefile']), [
    { kind: 'make', target: 'test-bossd', env: {} },
  ])
})

test('selectTargets fans proto changes out to generated-code consumers', () => {
  assert.deepEqual(
    selectTargets(['proto/bossanova/v1/session.proto']).map(({ target }) => target),
    ['test-bossalib', 'test-boss', 'test-bossd', 'test-bosso'],
  )
  assert.deepEqual(
    selectTargets(['proto/bossanova/v1/session.proto']).map(({ env }) => env),
    [{}, {}, {}, {}],
  )
})

test('selectTargets maps script changes to script tests', () => {
  assert.deepEqual(selectTargets(['scripts/check-public-mirror-workflows.mjs']), [
    { kind: 'make', target: 'test-scripts', env: {} },
  ])
})

test('selects scripts tests for proof recipe changes', () => {
  assert.deepEqual(selectTargets(['proof/recipes/default.json']), [
    { kind: 'make', target: 'test-scripts', env: {} },
  ])
})

test('selectTargets maps skills-toolbox changes to script tests', () => {
  assert.deepEqual(selectTargets(['skills-toolbox/bs-epic-lib.mjs']), [
    { kind: 'make', target: 'test-scripts', env: {} },
  ])
})

test('selectTargets maps manifest and agent instruction changes to manifest checks', () => {
  assert.deepEqual(
    selectTargets(['AGENTS.md', 'CLAUDE.md', 'docs/testing/test-command-manifest.md']),
    [{ kind: 'make', target: 'test-manifest', env: {} }],
  )
})

test('selectTargets maps skill docs to manifest, Stop-hook guard, and skill content tests', () => {
  assert.deepEqual(selectTargets(['.claude/skills/agent-fast-testing/SKILL.md']), [
    { kind: 'make', target: 'test-manifest', env: {} },
    { kind: 'make', target: 'test-no-inline-stop-hooks', env: {} },
    { kind: 'make', target: 'test-scripts', env: {} },
  ])
})

test('selectTargets maps Codex skills to manifest, Stop-hook guard, and skill content tests', () => {
  assert.deepEqual(selectTargets(['.codex/skills/golang-pro/SKILL.md']), [
    { kind: 'make', target: 'test-manifest', env: {} },
    { kind: 'make', target: 'test-no-inline-stop-hooks', env: {} },
    { kind: 'make', target: 'test-scripts', env: {} },
  ])
})

test('selectTargets maps guidance docs to manifest checks', () => {
  assert.deepEqual(selectTargets(['docs/guidance/agent-fast-testing.md']), [
    { kind: 'make', target: 'test-manifest', env: {} },
  ])
})

test('selectTargets maps Claude testing docs to manifest checks', () => {
  assert.deepEqual(
    selectTargets(['.claude/docs/testing.md', '.claude/docs/testing/agent-fast-testing.md']),
    [{ kind: 'make', target: 'test-manifest', env: {} }],
  )
})

test('selectTargets falls back to smoke tests when no rule matches', () => {
  assert.deepEqual(selectTargets(['README.md']), [{ kind: 'make', target: 'test-smoke', env: {} }])
})

test('renderMakeCommands prefixes scoped environment variables', () => {
  assert.deepEqual(
    renderMakeCommands(selectTargets(['services/bossd/internal/session/lifecycle.go'])),
    ["GO_TEST_PACKAGES='./internal/session' make test-bossd"],
  )
})

test('test-affected propagates selector startup failures', () => {
  const fixture = createMakeFixture({
    nodeScript: '#!/bin/sh\nexit 33\n',
    makeScript: '#!/bin/sh\nexit 0\n',
  })

  const result = runMakeFixture(fixture)

  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /Error 33/)
})

test('test-affected stops on the first failing selected command', () => {
  const fixture = createMakeFixture({
    nodeScript: "#!/bin/sh\nprintf '%s\\n' 'make fail-selected' 'make success-selected'\n",
    makeScript: [
      '#!/bin/sh',
      'echo "$*" >> "$FAKE_MAKE_LOG"',
      'case "$1" in',
      '  fail-selected) exit 42 ;;',
      '  success-selected) exit 0 ;;',
      '  *) exit 0 ;;',
      'esac',
      '',
    ].join('\n'),
  })

  const result = runMakeFixture(fixture)
  const log = fs.readFileSync(fixture.logPath, 'utf8')

  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /Error 42/)
  assert.match(log, /fail-selected/)
  assert.doesNotMatch(log, /success-selected/)
})

test('test-affected runs manifest commands directly when selected', () => {
  const fixture = createMakeFixture({
    nodeScript: "#!/bin/sh\nprintf '%s\\n' 'make test-manifest'\n",
    makeScript: [
      '#!/bin/sh',
      'echo "$*" >> "$FAKE_MAKE_LOG"',
      'case "$1" in',
      '  test-manifest) exit 0 ;;',
      '  test-scripts) exit 99 ;;',
      '  *) exit 0 ;;',
      'esac',
      '',
    ].join('\n'),
  })

  const result = runMakeFixture(fixture)
  const log = fs.readFileSync(fixture.logPath, 'utf8')

  assert.equal(result.status, 0)
  assert.match(log, /test-manifest/)
  assert.doesNotMatch(log, /test-scripts/)
})

test('test-affected runs smoke tests when selector emits no commands', () => {
  const fixture = createMakeFixture({
    nodeScript: '#!/bin/sh\nexit 0\n',
    makeScript: [
      '#!/bin/sh',
      'echo "$*" >> "$FAKE_MAKE_LOG"',
      'case "$1" in',
      '  test-smoke) exit 0 ;;',
      '  test-scripts) exit 99 ;;',
      '  *) exit 0 ;;',
      'esac',
      '',
    ].join('\n'),
  })

  const result = runMakeFixture(fixture)
  const log = fs.readFileSync(fixture.logPath, 'utf8')

  assert.equal(result.status, 0)
  assert.match(log, /test-smoke/)
  assert.doesNotMatch(log, /test-scripts/)
})

test('Task 3 publishes smoke target', () => {
  const makefile = fs.readFileSync(makefilePath, 'utf8')
  const phonyBlock = makefile.match(/^\.PHONY:[\s\S]*?\n\n/)?.[0] ?? ''

  assert.match(makefile, /^test-smoke:/m)
  assert.match(phonyBlock, /\btest-smoke\b/)
})

function createMakeFixture({ nodeScript, makeScript, makefileText }) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'select-affected-tests-'))
  const binDirectory = path.join(directory, 'bin')
  const fixtureMakefilePath = path.join(directory, 'Makefile')
  fs.mkdirSync(binDirectory)

  const nodePath = path.join(binDirectory, 'node')
  const makePath = path.join(binDirectory, 'make')
  const logPath = path.join(directory, 'make.log')

  fs.writeFileSync(nodePath, nodeScript, { mode: 0o755 })
  fs.writeFileSync(makePath, makeScript, { mode: 0o755 })
  fs.writeFileSync(logPath, '')
  if (makefileText) {
    fs.writeFileSync(fixtureMakefilePath, makefileText)
  }

  return {
    binDirectory,
    directory,
    logPath,
    makefilePath: makefileText ? fixtureMakefilePath : makefilePath,
  }
}

function runMakeFixture(fixture) {
  return spawnSync(realMakePath(), ['-f', fixture.makefilePath, 'test-affected', 'MAKE=make'], {
    cwd: repoRoot,
    encoding: 'utf8',
    env: {
      ...process.env,
      FAKE_MAKE_LOG: fixture.logPath,
      PATH: `${fixture.binDirectory}${path.delimiter}${process.env.PATH}`,
    },
  })
}

function realMakePath() {
  return execFileSync('which', ['make'], { encoding: 'utf8' }).trim()
}

function makefileWithoutTarget(target) {
  const makefile = fs.readFileSync(makefilePath, 'utf8')
  const targetPattern = target.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  return makefile.replace(
    new RegExp(`^${targetPattern}:.*\\n(?:\\t.*\\n|\\s*#.*\\n|\\s*\\n)*`, 'm'),
    '',
  )
}
