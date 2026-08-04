#!/usr/bin/env node

import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

import { renderMakeCommands, selectBazelAffected, selectTargets } from './select-affected-tests.mjs'

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

test('selectTargets maps the build-and-ci reference doc to script tests', () => {
  assert.deepEqual(selectTargets(['docs/build-and-ci.md']), [
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

test('selectTargets maps the skill config to the script tests', () => {
  // scripts/check-skill-symbols.mjs resolves skill role citations through this file, so a
  // config-only edit can turn the gate red with no scripts/** change.
  assert.deepEqual(selectTargets(['.boss-skills.json']), [
    { kind: 'make', target: 'test-scripts', env: {} },
  ])
})

test('selectTargets adds the script tests to published skill sources WITHOUT dropping test-boss', () => {
  // The published skill bodies live inside services/boss, so they are inputs to BOTH the
  // prose gates and the skillinstall manifest test. Selecting test-scripts terminally here
  // would silence the latter.
  assert.deepEqual(
    selectTargets(['services/boss/internal/skillinstall/skills/boss-build/SKILL.md']),
    [
      { kind: 'make', target: 'test-scripts', env: {} },
      { kind: 'make', target: 'test-boss', env: {} },
    ],
  )
})

test('selectTargets adds the script tests to published skill SHELL payloads too', () => {
  // The published payload also ships executable helpers (*.sh) whose node:test coverage
  // lives under scripts/. A shell-only payload change must still reach test-scripts, and
  // must not drop test-boss (the skillinstall manifest test embeds the payload).
  assert.deepEqual(
    selectTargets(['services/boss/internal/skillinstall/skills/boss-finalize/add-pr-numbers.sh']),
    [
      { kind: 'make', target: 'test-scripts', env: {} },
      { kind: 'make', target: 'test-boss', env: {} },
    ],
  )
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

test('selectTargets maps web changes to the turbo web target', () => {
  assert.deepEqual(selectTargets(['services/web/src/App.tsx']), [
    { kind: 'make', target: 'test-web', env: {} },
  ])
})

test('selectTargets maps marketing changes to the turbo web target', () => {
  assert.deepEqual(selectTargets(['services/marketing/src/pages/index.astro']), [
    { kind: 'make', target: 'test-web', env: {} },
  ])
})

test('selectTargets maps ui-tokens changes to the turbo web target', () => {
  assert.deepEqual(selectTargets(['lib/ui-tokens/index.css']), [
    { kind: 'make', target: 'test-web', env: {} },
  ])
})

test('selectTargets maps the lockfile and turbo.json to the turbo web target', () => {
  assert.deepEqual(selectTargets(['pnpm-lock.yaml']), [
    { kind: 'make', target: 'test-web', env: {} },
  ])
  assert.deepEqual(selectTargets(['turbo.json']), [{ kind: 'make', target: 'test-web', env: {} }])
})

test('selectTargets maps infra/install.sh to the turbo web target', () => {
  // Marketing's prebuild copies infra/install.sh into public/, so it is a
  // marketing build input (turbo.json globalDependencies + test-marketing.yml).
  assert.deepEqual(selectTargets(['infra/install.sh']), [
    { kind: 'make', target: 'test-web', env: {} },
  ])
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

test('--bazel CLI prints the sorted space-joined patterns on one line', () => {
  const scriptPath = fileURLToPath(new URL('./select-affected-tests.mjs', import.meta.url))
  const affected = spawnSync(
    process.execPath,
    [scriptPath, '--bazel', 'services/bossd/x.go', 'lib/bossalib/y.go'],
    { encoding: 'utf8' },
  )
  assert.equal(affected.status, 0)
  assert.equal(affected.stdout, '//lib/bossalib/... //services/bossd/...\n')

  const full = spawnSync(process.execPath, [scriptPath, '--bazel', 'go.work'], { encoding: 'utf8' })
  assert.equal(full.status, 0)
  assert.equal(full.stdout, '//...\n')
})

test('selectBazelAffected maps one Go module to its bazel pattern', () => {
  assert.deepEqual(selectBazelAffected(['services/bossd/internal/session/lifecycle.go']), {
    full: false,
    patterns: ['//services/bossd/...'],
    reason: 'affected',
  })
})

test('selectBazelAffected sorts and dedupes two affected modules', () => {
  assert.deepEqual(
    selectBazelAffected([
      'services/bossd/internal/session/lifecycle.go',
      'lib/bossalib/safego/safego.go',
      'services/bossd/internal/db/db.go',
    ]),
    {
      full: false,
      patterns: ['//lib/bossalib/...', '//services/bossd/...'],
      reason: 'affected',
    },
  )
})

test('selectBazelAffected maps a plugin change to its bazel pattern', () => {
  assert.deepEqual(selectBazelAffected(['plugins/bossd-plugin-sentry/sentry.go']), {
    full: false,
    patterns: ['//plugins/bossd-plugin-sentry/...'],
    reason: 'affected',
  })
})

for (const graphWide of [
  'proto/bossanova/v1/session.proto',
  'go.work',
  'go.work.sum',
  '.bazelrc',
  '.bazelversion',
  'MODULE.bazel',
  'MODULE.bazel.lock',
  'Makefile',
]) {
  test(`selectBazelAffected falls back to FULL for graph-wide change ${graphWide}`, () => {
    const result = selectBazelAffected([graphWide])
    assert.equal(result.full, true)
    assert.deepEqual(result.patterns, ['//...'])
    // Pin the diagnostic reason so a mislabeled/dropped trigger is caught, not
    // just silently absorbed by the `uncertain` fail-safe branch.
    assert.match(result.reason, /graph-wide/)
  })
}

// Lock the three Go modules the bazel map intentionally includes but the local
// make `moduleRules` omits (mcp / mcp-gateway / stub-runner). These are the
// deliberate divergence between the two maps; a typo or dropped entry here would
// silently un-optimize (fall to FULL) or, worse, point bazel at a wrong target.
for (const [file, pattern] of [
  ['services/mcp/internal/serve/serve.go', '//services/mcp/...'],
  ['services/mcp-gateway/internal/auth/middleware.go', '//services/mcp-gateway/...'],
  ['plugins/bossd-plugin-stub-runner/server.go', '//plugins/bossd-plugin-stub-runner/...'],
]) {
  test(`selectBazelAffected maps bazel-only module ${pattern}`, () => {
    assert.deepEqual(selectBazelAffected([file]), {
      full: false,
      patterns: [pattern],
      reason: 'affected',
    })
  })
}

test('selectBazelAffected treats the docusaurus site (services/docs/) as irrelevant', () => {
  const result = selectBazelAffected(['services/docs/docs/api.md'])
  assert.equal(result.full, true)
  assert.deepEqual(result.patterns, ['//...'])
  assert.equal(result.reason, 'no affected go targets')
})

test('selectBazelAffected ignores Go-graph-irrelevant files mixed with a code change', () => {
  // The common PR case: a Go change alongside a README/doc edit must select only
  // the affected module, NOT fall back to FULL.
  assert.deepEqual(selectBazelAffected(['services/bossd/x.go', 'README.md', 'docs/foo.md']), {
    full: false,
    patterns: ['//services/bossd/...'],
    reason: 'affected',
  })
})

test('selectBazelAffected falls back to FULL for a web-only change', () => {
  const result = selectBazelAffected(['services/web/src/App.tsx'])
  assert.equal(result.full, true)
  assert.deepEqual(result.patterns, ['//...'])
})

test('selectBazelAffected falls back to FULL for a docs-only change', () => {
  const result = selectBazelAffected(['docs/solutions/build-systems/bazel-sandbox-patterns.md'])
  assert.equal(result.full, true)
  assert.deepEqual(result.patterns, ['//...'])
})

test('selectBazelAffected fails safe to FULL for an unrecognized file', () => {
  const result = selectBazelAffected(['weird-new-file.xyz'])
  assert.equal(result.full, true)
  assert.deepEqual(result.patterns, ['//...'])
  assert.match(result.reason, /uncertain/)
})

test('selectBazelAffected falls back to FULL for empty input', () => {
  assert.deepEqual(selectBazelAffected([]), {
    full: true,
    patterns: ['//...'],
    reason: 'empty',
  })
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
