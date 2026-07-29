#!/usr/bin/env node

import { execFileSync } from 'node:child_process'

const moduleRules = [
  { root: 'lib/bossalib/', target: 'test-bossalib' },
  { root: 'services/boss/', target: 'test-boss' },
  { root: 'services/bossd/', target: 'test-bossd' },
  { root: 'services/bosso/', target: 'test-bosso' },
  { root: 'plugins/bossd-plugin-claude/', target: 'test-claude' },
  { root: 'plugins/bossd-plugin-codex/', target: 'test-codex' },
  { root: 'plugins/bossd-plugin-dependabot/', target: 'test-dependabot' },
  { root: 'plugins/bossd-plugin-linear/', target: 'test-linear' },
  { root: 'plugins/bossd-plugin-opencode/', target: 'test-opencode' },
  { root: 'plugins/bossd-plugin-repair/', target: 'test-repair' },
  { root: 'plugins/bossd-plugin-sentry/', target: 'test-sentry' },
]

const protoTargets = ['test-bossalib', 'test-boss', 'test-bossd', 'test-bosso']

// BOS-370: Go-bazel-target selection for the PR fast tier (bazel.yml go-test).
// This is a SEPARATE map from the make `moduleRules` above: it maps a changed
// file to the set of `//<module>/...` bazel patterns the PR CI job runs instead
// of the whole `//...` graph. It includes mcp / mcp-gateway / stub-runner (which
// the local make map omits) because all 14 Go modules have `go_test` targets, so
// `bazel test //<module>/...` is always a SAFE (never exit-4) selection.
//
// Fail-safe posture: err toward MORE testing, never less. Any file that cannot be
// confidently classified as Go-graph-irrelevant forces a FULL `//...` run. The
// release tier (main/staging/production) re-runs the whole `//...` graph (plain +
// race) as the safety net, so an under-selection here can never let a regression
// reach a release — it only affects PR latency.
const bazelGoModuleRules = [
  { root: 'lib/bossalib/', pattern: '//lib/bossalib/...' },
  { root: 'services/boss/', pattern: '//services/boss/...' },
  { root: 'services/bossd/', pattern: '//services/bossd/...' },
  { root: 'services/bosso/', pattern: '//services/bosso/...' },
  { root: 'services/mcp/', pattern: '//services/mcp/...' },
  { root: 'services/mcp-gateway/', pattern: '//services/mcp-gateway/...' },
  { root: 'plugins/bossd-plugin-claude/', pattern: '//plugins/bossd-plugin-claude/...' },
  { root: 'plugins/bossd-plugin-codex/', pattern: '//plugins/bossd-plugin-codex/...' },
  { root: 'plugins/bossd-plugin-dependabot/', pattern: '//plugins/bossd-plugin-dependabot/...' },
  { root: 'plugins/bossd-plugin-linear/', pattern: '//plugins/bossd-plugin-linear/...' },
  { root: 'plugins/bossd-plugin-opencode/', pattern: '//plugins/bossd-plugin-opencode/...' },
  { root: 'plugins/bossd-plugin-repair/', pattern: '//plugins/bossd-plugin-repair/...' },
  { root: 'plugins/bossd-plugin-sentry/', pattern: '//plugins/bossd-plugin-sentry/...' },
  { root: 'plugins/bossd-plugin-stub-runner/', pattern: '//plugins/bossd-plugin-stub-runner/...' },
]

// Graph-wide FULL triggers: a change to any of these can affect the entire Go
// bazel graph (proto codegen, workspace/module graph, bazel config, root make),
// so the only safe selection is the whole `//...` graph.
function isBazelGraphWideTrigger(file) {
  return (
    file.startsWith('proto/') ||
    file === 'go.work' ||
    file === 'go.work.sum' ||
    file === '.bazelrc' ||
    file === '.bazelversion' ||
    file === 'MODULE.bazel' ||
    file === 'MODULE.bazel.lock' ||
    file === 'Makefile'
  )
}

// Go-graph-irrelevant paths: files that provably CANNOT affect the Go bazel graph
// (no `//go:embed` reads from these dirs), so they contribute no pattern and never
// force a FULL run. Everything not matched here (and not a module or trigger) is
// treated as unrecognized → FULL fail-safe.
const bazelIrrelevantPrefixes = [
  'services/web/',
  'services/marketing/',
  'services/docs/',
  'lib/ui-tokens/',
  '.claude/',
  '.codex/',
  'docs/',
  '.github/',
  'scripts/',
  'proof/',
  'skills-toolbox/',
  'infra/',
]

const bazelIrrelevantFiles = new Set([
  'README.md',
  'LICENSE',
  'AGENTS.md',
  'CLAUDE.md',
  '.gitignore',
  'pnpm-lock.yaml',
  'pnpm-workspace.yaml',
  'package.json',
  'turbo.json',
  '.prettierrc',
  '.prettierignore',
  'commitlint.config.cjs',
  'biome.json',
])

function isBazelIrrelevant(file) {
  return (
    bazelIrrelevantFiles.has(file) ||
    bazelIrrelevantPrefixes.some((prefix) => file.startsWith(prefix))
  )
}

// selectBazelAffected maps a changed-file list to the bazel target patterns the
// PR CI go-test job should run. Returns { full, patterns, reason }:
//   - full:true, patterns:['//...'] whenever the safe choice is the whole graph
//     (empty input, a graph-wide trigger, an unrecognized file, or no affected Go
//     module) — the fail-safe toward more testing.
//   - full:false with a sorted, deduped list of `//<module>/...` patterns when the
//     diff maps cleanly to one or more Go modules (irrelevant files are ignored).
export function selectBazelAffected(files) {
  if (!files || files.length === 0) {
    return { full: true, patterns: ['//...'], reason: 'empty' }
  }

  const patterns = new Set()

  for (const rawFile of files) {
    const file = normalizePath(rawFile)
    if (file === '') {
      continue
    }

    if (isBazelGraphWideTrigger(file)) {
      return { full: true, patterns: ['//...'], reason: `graph-wide: ${file}` }
    }

    const moduleRule = bazelGoModuleRules.find(({ root }) => file.startsWith(root))
    if (moduleRule) {
      patterns.add(moduleRule.pattern)
      continue
    }

    if (isBazelIrrelevant(file)) {
      continue
    }

    // Unrecognized file: cannot prove it is Go-graph-irrelevant → fail safe to FULL.
    return { full: true, patterns: ['//...'], reason: `uncertain: ${file}` }
  }

  if (patterns.size === 0) {
    // Only Go-graph-irrelevant files changed (web-only / docs-only PR).
    return { full: true, patterns: ['//...'], reason: 'no affected go targets' }
  }

  return { full: false, patterns: [...patterns].sort(), reason: 'affected' }
}

export function selectTargets(files) {
  const selections = new Map()

  for (const rawFile of files) {
    const file = normalizePath(rawFile)
    if (file === '') {
      continue
    }

    if (file.startsWith('proto/') && file.endsWith('.proto')) {
      for (const target of protoTargets) {
        selectWholeTarget(selections, target)
      }
      continue
    }

    if (file.startsWith('scripts/')) {
      selectWholeTarget(selections, 'test-scripts')
      continue
    }

    if (file.startsWith('proof/')) {
      selectWholeTarget(selections, 'test-scripts')
      continue
    }

    // The vendored skill toolbox helpers (skills-toolbox/*.mjs) ship their unit tests as
    // skills-toolbox/*.test.mjs, which `make test-scripts` globs. A change touching only
    // skills-toolbox/ must run those tests, not fall through to test-smoke.
    if (file.startsWith('skills-toolbox/')) {
      selectWholeTarget(selections, 'test-scripts')
      continue
    }

    if (isSkillPath(file)) {
      selectWholeTarget(selections, 'test-manifest')
      selectWholeTarget(selections, 'test-no-inline-stop-hooks')
      // A SKILL.md-only edit must also run the skill content tests (BOS-144), which pin
      // the byte-stable dispatch/sentinel contracts documented in the skill bodies.
      selectWholeTarget(selections, 'test-scripts')
      continue
    }

    if (isManifestPath(file)) {
      selectWholeTarget(selections, 'test-manifest')
      continue
    }

    // The TS workspace (web, marketing, ui-tokens) is tested via turbo behind
    // `make test-web`. lib/ui-tokens is an internal dependency of both apps, and
    // the lockfile / turbo.json are workspace-wide inputs, so any of them route
    // here. These are NOT Go modules, so select the whole target.
    if (isWebPath(file)) {
      selectWholeTarget(selections, 'test-web')
      continue
    }

    const moduleRule = moduleRules.find(({ root }) => file.startsWith(root))
    if (moduleRule) {
      selectModuleTarget(selections, moduleRule, file)
    }
  }

  if (selections.size === 0) {
    selectWholeTarget(selections, 'test-smoke')
  }

  return [...selections.values()].map((selection) => ({
    kind: 'make',
    target: selection.target,
    env: selection.wholeModule
      ? {}
      : { GO_TEST_PACKAGES: [...selection.packages].sort().join(' ') },
  }))
}

export function renderMakeCommands(targets) {
  return targets.map(({ target, env }) => {
    const envPrefix = Object.entries(env ?? {})
      .map(([key, value]) => `${key}=${shellQuote(value)}`)
      .join(' ')
    return [envPrefix, 'make', target].filter(Boolean).join(' ')
  })
}

function selectModuleTarget(selections, moduleRule, file) {
  if (
    !file.endsWith('.go') ||
    file.endsWith('/Makefile') ||
    file === `${moduleRule.root}Makefile`
  ) {
    selectWholeTarget(selections, moduleRule.target)
    return
  }

  const relativeDir = file.slice(moduleRule.root.length).split('/').slice(0, -1).join('/')
  selectPackageTarget(selections, moduleRule.target, `./${relativeDir}`)
}

function selectWholeTarget(selections, target) {
  selections.set(target, {
    target,
    wholeModule: true,
    packages: new Set(),
  })
}

function selectPackageTarget(selections, target, packagePath) {
  const existing = selections.get(target)
  if (existing?.wholeModule) {
    return
  }

  const selection = existing ?? {
    target,
    wholeModule: false,
    packages: new Set(),
  }
  selection.packages.add(packagePath === './' ? '.' : packagePath)
  selections.set(target, selection)
}

function isManifestPath(file) {
  return (
    file === 'AGENTS.md' ||
    file === 'CLAUDE.md' ||
    file === '.claude/docs/testing.md' ||
    file.startsWith('.claude/docs/testing/') ||
    file.startsWith('docs/guidance/') ||
    file.startsWith('docs/testing/')
  )
}

function isSkillPath(file) {
  return file.startsWith('.claude/skills/') || file.startsWith('.codex/skills/')
}

function isWebPath(file) {
  return (
    file.startsWith('services/web/') ||
    file.startsWith('services/marketing/') ||
    file.startsWith('lib/ui-tokens/') ||
    file === 'pnpm-lock.yaml' ||
    file === 'turbo.json' ||
    // Marketing's prebuild copies infra/install.sh into public/, so it is a
    // marketing build input: turbo.json globalDependencies and
    // test-marketing.yml both treat it as one. Keep this in sync with those two.
    file === 'infra/install.sh'
  )
}

function normalizePath(file) {
  return String(file ?? '')
    .trim()
    .replaceAll('\\', '/')
    .replace(/^\.\//, '')
}

function shellQuote(value) {
  return `'${String(value).replaceAll("'", "'\\''")}'`
}

function changedFilesFromGit() {
  const baseRef = process.env.BASE_REF || 'origin/main'
  const output = execFileSync('git', ['diff', '--name-only', `${baseRef}...HEAD`], {
    encoding: 'utf8',
  })
  return output.split(/\r?\n/).filter(Boolean)
}

import { isMainModule } from '../skills-toolbox/main-module.mjs'

if (isMainModule(import.meta.url)) {
  const args = process.argv.slice(2)
  if (args[0] === '--bazel') {
    // BOS-370 PR CI path: print the space-joined bazel target patterns on ONE line
    // (either `//...` or e.g. `//services/bossd/... //lib/bossalib/...`). Explicit
    // files may follow `--bazel` for testability; otherwise diff against BASE_REF.
    const explicitFiles = args.slice(1)
    const files = explicitFiles.length > 0 ? explicitFiles : changedFilesFromGit()
    console.log(selectBazelAffected(files).patterns.join(' '))
  } else {
    const files = args.length > 0 ? args : changedFilesFromGit()
    console.log(renderMakeCommands(selectTargets(files)).join('\n'))
  }
}
