#!/usr/bin/env node

import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  externalInputRules,
  moduleRules,
  nativeModuleRootsForExternalInputRule,
  renderMakeCommands,
  selectBazelAffected,
  selectLedgerModules,
  selectTargets,
} from './select-affected-tests.mjs'

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
    ['test-web', 'test-bossalib', 'test-boss', 'test-bossd', 'test-bosso'],
  )
  assert.deepEqual(
    selectTargets(['proto/bossanova/v1/session.proto']).map(({ env }) => env),
    [{}, {}, {}, {}, {}],
  )
})

test('selectTargets maps proto support files to web generated-code consumers', () => {
  assert.deepEqual(selectTargets(['proto/bossanova/v1/session.md']), [
    { kind: 'make', target: 'test-web', env: {} },
  ])
})

test('selectTargets maps script changes to script tests', () => {
  assert.deepEqual(selectTargets(['scripts/check-public-mirror-workflows.mjs']), [
    { kind: 'make', target: 'test-scripts', env: {} },
  ])
})

test('selectTargets maps script workflow and codegen inputs to script tests', () => {
  for (const file of [
    '.github/workflows/test-bosso-production-deployment.yml',
    '.github/workflows/test-docs.yml',
    '.github/workflows/test-marketing.yml',
    '.github/workflows/test-plugin-distribution.yml',
    '.github/workflows/test-proto.yml',
    '.github/workflows/test-scripts.yml',
    '.github/workflows/test-web.yml',
    'buf.yaml',
    'buf.gen.yaml',
  ]) {
    assert.deepEqual(selectTargets([file]), [{ kind: 'make', target: 'test-scripts', env: {} }])
  }
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

test('selectTargets routes declared external inputs to native module targets', () => {
  assert.deepEqual(selectTargets(['docs/analytics/events.md']), [
    { kind: 'make', target: 'test-bossalib', env: {} },
    { kind: 'make', target: 'test-smoke', env: {} },
  ])
  assert.deepEqual(selectTargets(['docs/skills/README.md']), [
    { kind: 'make', target: 'test-boss', env: {} },
    { kind: 'make', target: 'test-smoke', env: {} },
  ])
  assert.deepEqual(selectTargets(['services/web/src/api.ts']), [
    { kind: 'make', target: 'test-bossalib', env: {} },
    { kind: 'make', target: 'test-scripts', env: {} },
    { kind: 'make', target: 'test-web', env: {} },
  ])
  assert.deepEqual(selectTargets(['plugins/bossd-plugin-codex/testdata/fake_codex_tui.sh']), [
    { kind: 'make', target: 'test-bossd', env: {} },
    { kind: 'make', target: 'test-codex', env: {} },
  ])
})

test('selectTargets maps every Go module on disk to a native make target', () => {
  const roots = moduleRootsFromDisk()
  const missing = roots.filter(
    (root) => !moduleRules.some((rule) => trimSlash(rule.root) === trimSlash(root)),
  )

  assert.deepEqual(missing, [])
  for (const root of roots) {
    const selected = selectTargets([`${root}/main.go`]).map(({ target }) => target)
    assert.notDeepEqual(selected, ['test-smoke'], `${root} must not fall back to smoke only`)
  }
})

test('moduleRootsFromDisk fails closed when git returns no modules', () => {
  assert.throws(
    () => moduleRootsFromDisk({ execFile: () => '' }),
    /moduleRootsFromDisk found no module roots; the module-rule coverage assertion would pass vacuously/,
  )
})

test('each external input rule selects every derivable native target from its sample path', () => {
  for (const rule of externalInputRules) {
    assert.equal(typeof rule.samplePath, 'string', 'external input rules need a samplePath')
    assert.ok(rule.match(rule.samplePath), `${rule.samplePath} must satisfy its rule`)

    const selected = new Set(selectTargets([rule.samplePath]).map(({ target }) => target))
    for (const moduleRoot of nativeModuleRootsForExternalInputRule(rule)) {
      const moduleRule = moduleRules.find((candidate) => trimSlash(candidate.root) === moduleRoot)
      if (!moduleRule) continue
      assert.ok(
        selected.has(moduleRule.target),
        `${rule.samplePath} must select ${moduleRule.target}`,
      )
    }
  }
})

test('selectTargets maps the build-and-ci reference doc to script tests', () => {
  assert.deepEqual(selectTargets(['docs/build-and-ci.md']), [
    { kind: 'make', target: 'test-scripts', env: {} },
  ])
})

test('selectTargets maps manifest and agent instruction changes to manifest checks', () => {
  assert.deepEqual(selectTargets(['AGENTS.md', 'CLAUDE.md']), [
    { kind: 'make', target: 'test-manifest', env: {} },
  ])
  assert.deepEqual(selectTargets(['docs/testing/test-command-manifest.md']), [
    { kind: 'make', target: 'test-scripts', env: {} },
    { kind: 'make', target: 'test-manifest', env: {} },
  ])
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

test('selectTargets maps every prettier-pinning input to the script tests', () => {
  // scripts/prettier-pin-drift.test.mjs compares the root manifest's prettier range
  // against the standalone services/docs package's, and the version each tree's
  // lockfile resolves. A bump to any of the four touches no scripts/** path, so the
  // gate must be selected from those files themselves.
  assert.deepEqual(selectTargets(['package.json']), [
    { kind: 'make', target: 'test-scripts', env: {} },
  ])
  assert.deepEqual(selectTargets(['services/docs/package.json']), [
    { kind: 'make', target: 'test-scripts', env: {} },
  ])
  assert.deepEqual(selectTargets(['services/docs/pnpm-lock.yaml']), [
    { kind: 'make', target: 'test-scripts', env: {} },
  ])
  // The root lockfile is also a web-tree input: adding the gate must not cost it the
  // web targets it has always selected.
  assert.deepEqual(selectTargets(['pnpm-lock.yaml']), [
    { kind: 'make', target: 'test-scripts', env: {} },
    { kind: 'make', target: 'test-web', env: {} },
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

test('selectTargets routes the skill manifest test to test-scripts as well as test-boss', () => {
  // scripts/check-email-course.mjs parses its published-skill allowlist out of this file's
  // `want` slice, so dropping a core from the publish set can leave the trial email course
  // naming a skill nobody receives — the boss-proof failure that gate exists to catch. The
  // file sits one directory ABOVE skillinstall/skills/, so the payload rule does not cover
  // it, and it touches neither docs/email-course/ nor scripts/. test-boss must survive: it
  // is the manifest test's own package.
  assert.deepEqual(selectTargets(['services/boss/internal/skillinstall/skills_manifest_test.go']), [
    { kind: 'make', target: 'test-scripts', env: {} },
    { kind: 'make', target: 'test-boss', env: { GO_TEST_PACKAGES: './internal/skillinstall' } },
  ])
})

test('selectTargets routes the trial-enrolment source to test-scripts as well as test-bosso', () => {
  // scripts/check-email-course.mjs parses the event name out of this file's
  // `stripeTrialStartedEvent` constant, so renaming it is a script-gate input even
  // though the Go build stays green. The file is under neither scripts/ nor
  // docs/email-course/, so no other rule selects test-scripts for it. test-bosso must
  // survive: the wire-contract test the course cites lives in this package.
  assert.deepEqual(selectTargets(['services/bosso/cmd/trial_enrollment.go']), [
    { kind: 'make', target: 'test-scripts', env: {} },
    { kind: 'make', target: 'test-bosso', env: { GO_TEST_PACKAGES: './cmd' } },
  ])
})

test('selectTargets adds script tests to plugin skilldata WITHOUT dropping plugin and boss readers', () => {
  assert.deepEqual(
    selectTargets(['plugins/bossd-plugin-claude/skilldata/skills/boss-build/SKILL.md']),
    [
      { kind: 'make', target: 'test-boss', env: {} },
      { kind: 'make', target: 'test-scripts', env: {} },
      { kind: 'make', target: 'test-claude', env: {} },
    ],
  )
})

test('selectTargets adds script tests to docs MCP prop inputs WITHOUT dropping module readers', () => {
  assert.deepEqual(selectTargets(['services/docs/docs/commands.md']), [
    { kind: 'make', target: 'test-scripts', env: {} },
  ])
  assert.deepEqual(selectTargets(['lib/bossalib/bossmcp/tools.go']), [
    { kind: 'make', target: 'test-scripts', env: {} },
    { kind: 'make', target: 'test-bossalib', env: { GO_TEST_PACKAGES: './bossmcp' } },
  ])
})

test('selectTargets adds script tests to readiness figure inputs WITHOUT dropping modules', () => {
  assert.deepEqual(selectTargets(['lib/bossalib/config/config.go']), [
    { kind: 'make', target: 'test-scripts', env: {} },
    { kind: 'make', target: 'test-bossalib', env: { GO_TEST_PACKAGES: './config' } },
  ])
  assert.deepEqual(selectTargets(['services/bossd/internal/tmux/tmux.go']), [
    { kind: 'make', target: 'test-scripts', env: {} },
    { kind: 'make', target: 'test-bossd', env: { GO_TEST_PACKAGES: './internal/tmux' } },
  ])
})

test('selectTargets adds script tests to TUI key vocabulary inputs WITHOUT dropping test-boss', () => {
  assert.deepEqual(selectTargets(['services/boss/internal/tuidriver/keybytes.go']), [
    { kind: 'make', target: 'test-scripts', env: {} },
    { kind: 'make', target: 'test-boss', env: { GO_TEST_PACKAGES: './internal/tuidriver' } },
  ])
  assert.deepEqual(selectTargets(['services/boss/internal/tuidriver/testdata/key-vocab.json']), [
    { kind: 'make', target: 'test-scripts', env: {} },
    { kind: 'make', target: 'test-boss', env: {} },
  ])
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
  // test-scripts rides along because scripts/check-price-parity.mjs scans this tree for
  // stray price literals and its own test asserts against the real repository tree.
  assert.deepEqual(selectTargets(['services/web/src/App.tsx']), [
    { kind: 'make', target: 'test-scripts', env: {} },
    { kind: 'make', target: 'test-web', env: {} },
  ])
})

test('selectTargets does not route generated web code to the price gate', () => {
  // check-price-parity.mjs skips `gen/`, so routing it here would run the gate on behalf
  // of files it never reads and make test-proto.yml's exemption stale.
  assert.deepEqual(selectTargets(['services/web/src/gen/bossanova/v1/session_pb.ts']), [
    { kind: 'make', target: 'test-web', env: {} },
  ])
})

test('selectTargets maps marketing changes to the turbo web target', () => {
  assert.deepEqual(selectTargets(['services/marketing/src/pages/index.astro']), [
    { kind: 'make', target: 'test-bossalib', env: {} },
    { kind: 'make', target: 'test-scripts', env: {} },
    { kind: 'make', target: 'test-web', env: {} },
  ])
})

test('selectTargets maps ui-tokens changes to the turbo web target', () => {
  assert.deepEqual(selectTargets(['lib/ui-tokens/index.css']), [
    { kind: 'make', target: 'test-web', env: {} },
  ])
})

test('selectTargets maps the lockfile and turbo.json to the turbo web target', () => {
  // The lockfile keeps its web target and additionally selects the script tests, since
  // the prettier pin-drift gate reads the version it resolves (see the pin-drift case).
  assert.deepEqual(selectTargets(['pnpm-lock.yaml']), [
    { kind: 'make', target: 'test-scripts', env: {} },
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
  assert.deepEqual(selectTargets(['TODOS.md']), [{ kind: 'make', target: 'test-smoke', env: {} }])
})

test('selectTargets maps README to script tests', () => {
  assert.deepEqual(selectTargets(['README.md']), [
    { kind: 'make', target: 'test-scripts', env: {} },
  ])
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
  assert.equal(result.full, false)
  assert.deepEqual(result.patterns, [])
  assert.equal(result.reason, 'no go-relevant changes')
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

// A diff made up ENTIRELY of proven Go-graph-irrelevant paths selects NOTHING.
// This is the same proof the mixed case above relies on: a file safe to ignore
// beside a Go change is safe to ignore alone. Selecting `//...` here (the old
// behavior) meant a docs-only or workflow-only commit ran the whole graph.
test('selectBazelAffected selects NOTHING for a web-only change', () => {
  const result = selectBazelAffected(['services/web/src/App.tsx'])
  assert.equal(result.full, false)
  assert.deepEqual(result.patterns, [])
  assert.equal(result.reason, 'no go-relevant changes')
})

test('selectBazelAffected selects NOTHING for a docs-only change', () => {
  const result = selectBazelAffected(['docs/solutions/build-systems/bazel-sandbox-patterns.md'])
  assert.equal(result.full, false)
  assert.deepEqual(result.patterns, [])
})

test('selectBazelAffected selects NOTHING for a workflow-only change', () => {
  const result = selectBazelAffected(['.github/workflows/ci.yml', 'TODOS.md'])
  assert.equal(result.full, false)
  assert.deepEqual(result.patterns, [])
})

// The fail-safe must survive the change above: an UNRECOGNIZED path still forces
// FULL even when every other file in the diff is irrelevant.
test('selectBazelAffected still fails safe to FULL when an unrecognized file joins irrelevant ones', () => {
  const result = selectBazelAffected(['docs/a.md', 'weird-new-file.xyz'])
  assert.equal(result.full, true)
  assert.deepEqual(result.patterns, ['//...'])
  assert.match(result.reason, /uncertain/)
})

// selectLedgerModules — which native (bazel-`manual`) ledger rows a diff needs.
test('selectLedgerModules maps a module change to that module root', () => {
  assert.deepEqual(selectLedgerModules(['services/boss/internal/tuitest/x.go']), {
    all: false,
    modules: ['services/boss'],
  })
})

test('selectLedgerModules returns the union for a multi-module change', () => {
  assert.deepEqual(selectLedgerModules(['services/bossd/a.go', 'services/boss/b.go']), {
    all: false,
    modules: ['services/boss', 'services/bossd'],
  })
})

// lib/bossalib is a SHARED module: the boss/bossd native rows are integration tests that
// link it, and no `//lib/bossalib/...` pattern reruns them. Scoping a bossalib change to
// its own row would skip exactly the rows it is most likely to break.
test('selectLedgerModules widens a shared-module change to every row', () => {
  assert.deepEqual(selectLedgerModules(['lib/bossalib/b.go']), { all: true, modules: [] })
  assert.deepEqual(selectLedgerModules(['services/bossd/a.go', 'lib/bossalib/b.go']), {
    all: true,
    modules: [],
  })
})

test('selectLedgerModules selects no module for a docs-only change', () => {
  assert.deepEqual(selectLedgerModules(['docs/a.md']), { all: false, modules: [] })
})

test('selectLedgerModules runs EVERY row on a graph-wide change', () => {
  assert.deepEqual(selectLedgerModules(['proto/bossanova/v1/x.proto']), { all: true, modules: [] })
})

test('selectLedgerModules runs EVERY row on an unrecognized file (fail-safe)', () => {
  assert.deepEqual(selectLedgerModules(['weird-new-file.xyz']), { all: true, modules: [] })
})

// Every module root the selector can emit must be a real ledger `module` value or
// a module with no ledger rows — a typo here would silently skip native tests.
test('selectLedgerModules roots match the ledger module vocabulary', () => {
  const ledger = JSON.parse(
    fs.readFileSync(path.join(repoRoot, 'scripts/bazel/ledger.json'), 'utf8'),
  )
  const ledgerModules = new Set(ledger.map((row) => row.module))
  for (const mod of ledgerModules) {
    // A change inside a ledger module must reach that module's rows: either by selecting
    // exactly its root (so `run-ledger.mjs --module <root>` matches), or by widening to
    // every row when the module is shared.
    const { all, modules } = selectLedgerModules([`${mod}/some_file.go`])
    assert.ok(
      all || modules.includes(mod),
      `a change in ${mod} must still run that module's ledger rows, got ${JSON.stringify({ all, modules })}`,
    )
  }
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
  const selectorPath = path.join(binDirectory, 'select-affected-tests')
  const makePath = path.join(binDirectory, 'make')
  const logPath = path.join(directory, 'make.log')

  fs.writeFileSync(selectorPath, nodeScript, { mode: 0o755 })
  fs.writeFileSync(
    nodePath,
    [
      '#!/bin/sh',
      'if [ "$1" = "scripts/select-affected-tests.mjs" ]; then',
      `  exec ${JSON.stringify(selectorPath)}`,
      'fi',
      `exec ${JSON.stringify(process.execPath)} "$@"`,
      '',
    ].join('\n'),
    { mode: 0o755 },
  )
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
      BOSS_GATE_STAMP_DIR: path.join(fixture.directory, 'gate-stamps'),
      FAKE_MAKE_LOG: fixture.logPath,
      PATH: `${fixture.binDirectory}${path.delimiter}${process.env.PATH}`,
    },
  })
}

function realMakePath() {
  return execFileSync('which', ['make'], { encoding: 'utf8' }).trim()
}

function moduleRootsFromDisk({ execFile = execFileSync } = {}) {
  const roots = execFile(
    'git',
    ['ls-files', '--', 'lib/*/go.mod', 'services/*/go.mod', 'plugins/*/go.mod'],
    {
      cwd: repoRoot,
      encoding: 'utf8',
    },
  )
    .trim()
    .split('\n')
    .filter(Boolean)
    .map((file) => path.posix.dirname(file))
    .sort()
  assert.ok(
    roots.length > 0,
    'moduleRootsFromDisk found no module roots; the module-rule coverage assertion would pass vacuously',
  )
  return roots
}

function trimSlash(value) {
  return value.replace(/\/+$/, '')
}

function makefileWithoutTarget(target) {
  const makefile = fs.readFileSync(makefilePath, 'utf8')
  const targetPattern = target.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  return makefile.replace(
    new RegExp(`^${targetPattern}:.*\\n(?:\\t.*\\n|\\s*#.*\\n|\\s*\\n)*`, 'm'),
    '',
  )
}
