#!/usr/bin/env node

import { execFileSync } from 'node:child_process'

export const moduleRules = [
  { root: 'lib/bossalib/', target: 'test-bossalib' },
  { root: 'services/boss/', target: 'test-boss' },
  { root: 'services/bossd/', target: 'test-bossd' },
  { root: 'services/bosso/', target: 'test-bosso' },
  { root: 'services/mcp/', target: 'test-mcp' },
  { root: 'services/mcp-gateway/', target: 'test-mcp-gateway' },
  { root: 'plugins/bossd-plugin-claude/', target: 'test-claude' },
  { root: 'plugins/bossd-plugin-codex/', target: 'test-codex' },
  { root: 'plugins/bossd-plugin-dependabot/', target: 'test-dependabot' },
  { root: 'plugins/bossd-plugin-linear/', target: 'test-linear' },
  { root: 'plugins/bossd-plugin-opencode/', target: 'test-opencode' },
  { root: 'plugins/bossd-plugin-repair/', target: 'test-repair' },
  { root: 'plugins/bossd-plugin-sentry/', target: 'test-sentry' },
  { root: 'plugins/bossd-plugin-stub-runner/', target: 'test-stub-runner' },
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

// External inputs: files that live OUTSIDE any Go module but that a test INSIDE one
// reads at run time. They are the reason a path cannot be waved through as "irrelevant"
// just because it holds no Go code.
//
// This list is load-bearing and must stay exhaustive. The bazel half is mechanically
// derivable — a sandboxed test can only read what its target declares in `data` — and
// scripts/select-affected-external-inputs.test.mjs re-derives every cross-tree `data` dep
// from the BUILD files and fails if one is not covered here. The ledger half cannot be
// derived that way (those targets are `manual`, so they run unsandboxed and read straight
// from the worktree), so each entry below cites the test that does the reading.
//
// `ledgerModules` is listed separately from `patterns` because the two selections answer
// different questions: bazel needs the target pattern to test, the native ledger needs the
// `module` value of the row to run. A file can feed one and not the other.
export const externalInputRules = [
  {
    // //lib/bossalib/telemetry:telemetry_test `data` deps; client_test.go reads both files
    // to assert doc/web/Go analytics-event parity.
    match: (f) => f.startsWith('docs/analytics/') || f.startsWith('services/web/src/analytics/'),
    samplePath: 'docs/analytics/events.md',
    patterns: ['//lib/bossalib/...'],
    ledgerModules: [],
  },
  {
    // lib/bossalib/apiversion/webversion_test.go reads services/web/src/api.ts. That row is
    // bazel-`manual` (it escapes the module), so this feeds the LEDGER, not the graph.
    match: (f) => f === 'services/web/src/api.ts',
    samplePath: 'services/web/src/api.ts',
    patterns: [],
    ledgerModules: ['lib/bossalib'],
  },
  {
    // lib/bossalib/apiversion/openapiversion_test.go reads services/docs/openapi/*.yaml.
    // That row is bazel-`manual` (it escapes the module), so this feeds the LEDGER, not the graph.
    match: (f) => f.startsWith('services/docs/openapi/'),
    samplePath: 'services/docs/openapi/base.openapi.yaml',
    patterns: [],
    ledgerModules: ['lib/bossalib'],
  },
  {
    // //lib/bossalib/productparity:productparity_test `data` deps; trial_test.go reads these
    // cross-surface sources to keep checkout, web, marketing, and TUI trial copy in sync.
    match: (f) =>
      f.startsWith('services/boss/internal/views/') ||
      f.startsWith('services/bosso/internal/server/') ||
      f.startsWith('services/marketing/src/components/pricing/') ||
      f.startsWith('services/marketing/src/pages/') ||
      f.startsWith('services/web/src/pages/'),
    samplePath: 'services/web/src/pages/Billing.tsx',
    patterns: ['//lib/bossalib/...'],
    ledgerModules: [],
  },
  {
    // services/boss/internal/skillinstall reads the published skill trees and their prose
    // surfaces; //services/boss/internal/skillparity:skillparity_test declares the plugin
    // mirror as a `data` dep. Both the graph target and the manual row consume these.
    match: (f) =>
      f.startsWith('plugins/bossd-plugin-claude/skilldata/') ||
      f.startsWith('docs/skills/') ||
      f.startsWith('services/docs/docs/skills/'),
    samplePath: 'docs/skills/README.md',
    patterns: ['//services/boss/...'],
    ledgerModules: ['services/boss'],
  },
  {
    // services/bossd/internal/testharness spawns plugins/bossd-plugin-codex/testdata fakes.
    match: (f) => f.startsWith('plugins/bossd-plugin-codex/testdata/'),
    samplePath: 'plugins/bossd-plugin-codex/testdata/fake_codex_tui.sh',
    patterns: ['//services/bossd/...'],
    ledgerModules: ['services/bossd'],
  },
]

// Shared modules: a change here can break a dependent module's NATIVE ledger row, which no
// `//<module>/...` pattern would rerun. 13 modules require lib/bossalib, and the ledger's
// boss/bossd rows are integration tests that link it, so a bossalib change runs every row.
const sharedLedgerModules = new Set(['lib/bossalib'])

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
  'CONCEPTS.md',
  'TODOS.md',
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
//   - full:false with an EMPTY patterns list when every changed file is a known
//     Go-graph-irrelevant path (docs-only / web-only / workflow-only). This is the
//     same proof the mixed case already relies on: a file that is safe to IGNORE
//     next to a Go change is equally safe to ignore on its own, so there is no Go
//     target left to run. Callers MUST treat empty patterns as "run nothing" and
//     must never splice an empty list into `bazel test` (that would expand to the
//     whole graph). Only an unrecognized path still forces the FULL fail-safe.
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

    // External-input rules are ADDITIVE and are checked before the irrelevant list: a file
    // can both belong to a module (plugins/<p>/testdata) and feed a test in another one, and
    // a file can be Go-free (docs/analytics) yet still be a declared `data` dep. Missing
    // either case is what makes an empty selection unsound.
    let matched = false
    for (const rule of externalInputRules) {
      if (!rule.match(file)) continue
      for (const p of rule.patterns) patterns.add(p)
      // A rule with ledger-only consumers still counts as matched, so the file never falls
      // through to the `uncertain` FULL fail-safe below.
      matched = true
    }

    const moduleRule = bazelGoModuleRules.find(({ root }) => file.startsWith(root))
    if (moduleRule) {
      patterns.add(moduleRule.pattern)
      continue
    }

    if (matched || isBazelIrrelevant(file)) {
      continue
    }

    // Unrecognized file: cannot prove it is Go-graph-irrelevant → fail safe to FULL.
    return { full: true, patterns: ['//...'], reason: `uncertain: ${file}` }
  }

  if (patterns.size === 0) {
    // Only Go-graph-irrelevant files changed (web-only / docs-only / workflow-only).
    // Every one of them was matched by the explicit irrelevant lists above — none
    // reached the unrecognized fail-safe — so no Go target can be affected and the
    // correct selection is the EMPTY set, not the whole graph.
    return { full: false, patterns: [], reason: 'no go-relevant changes' }
  }

  return { full: false, patterns: [...patterns].sort(), reason: 'affected' }
}

// Ledger module roots, derived from the same bazel module rules so the two
// selections can never drift: `//services/boss/...` -> `services/boss`.
function patternToModuleRoot(pattern) {
  return pattern.replace(/^\/\//, '').replace(/\/\.\.\.$/, '')
}

// selectLedgerModules maps a changed-file list to the ledger `module` values whose
// native (bazel-`manual`) rows still need to run. Returns { all, modules }:
//   - all:true  — run every ledger row (graph-wide/uncertain change, i.e. the same
//     FULL fail-safe selectBazelAffected uses). modules is empty and MUST be ignored.
//   - all:false — run only rows whose `module` is in `modules` (possibly empty,
//     meaning no native row needs to run at all).
export function selectLedgerModules(files) {
  const affected = selectBazelAffected(files)
  if (affected.full) {
    return { all: true, modules: [] }
  }

  const modules = new Set(affected.patterns.map(patternToModuleRoot))

  // The bazel patterns alone are NOT the ledger's answer. Ledger rows are bazel-`manual`:
  // they run unsandboxed and read files no target pattern covers, so a file can need a
  // native row without contributing any pattern (services/web/src/api.ts is the case that
  // proves it — apiversion's row reads it, and the graph never touches it).
  for (const rawFile of files || []) {
    const file = normalizePath(rawFile)
    if (file === '') continue
    for (const rule of externalInputRules) {
      if (rule.match(file)) {
        for (const m of rule.ledgerModules) modules.add(m)
      }
    }
  }

  // A shared module's own pattern reruns only its own rows, but its dependents' native
  // integration rows link it too — so widen to every row rather than under-select.
  if ([...modules].some((m) => sharedLedgerModules.has(m))) {
    return { all: true, modules: [] }
  }

  return { all: false, modules: [...modules].sort() }
}

export function selectTargets(files) {
  const selections = new Map()
  let selectedPrimaryTarget = false
  let selectedExternalInputTarget = false

  for (const rawFile of files) {
    const file = normalizePath(rawFile)
    if (file === '') {
      continue
    }

    for (const rule of externalInputRules) {
      if (!rule.match(file)) continue
      for (const moduleRoot of nativeModuleRootsForExternalInputRule(rule)) {
        const moduleRule = findModuleRule(moduleRoot)
        if (moduleRule) {
          selectWholeTarget(selections, moduleRule.target)
          selectedExternalInputTarget = true
        }
      }
    }

    if (file.startsWith('proto/') && file.endsWith('.proto')) {
      for (const target of protoTargets) {
        selectWholeTarget(selections, target)
      }
      selectedPrimaryTarget = true
      continue
    }

    if (file.startsWith('scripts/')) {
      selectWholeTarget(selections, 'test-scripts')
      selectedPrimaryTarget = true
      continue
    }

    if (file.startsWith('proof/')) {
      selectWholeTarget(selections, 'test-scripts')
      selectedPrimaryTarget = true
      continue
    }

    // docs/build-and-ci.md carries the build-system reference lifted out of CLAUDE.md. It
    // cites real Make targets and is scanned by scripts/check-doc-make-targets.mjs, so an
    // edit must run the script tests rather than falling through to test-smoke.
    if (file === 'docs/build-and-ci.md') {
      selectWholeTarget(selections, 'test-scripts')
      selectedPrimaryTarget = true
      continue
    }

    // The skill config carries the tracker role tables that scripts/check-skill-symbols.mjs
    // resolves skill prose against, so a config-only edit can turn a role citation red with
    // no scripts/** change at all.
    if (file === '.boss-skills.json') {
      selectWholeTarget(selections, 'test-scripts')
      selectedPrimaryTarget = true
      continue
    }

    // The vendored skill toolbox helpers (skills-toolbox/*.mjs) ship their unit tests as
    // skills-toolbox/*.test.mjs, which `make test-scripts` globs. A change touching only
    // skills-toolbox/ must run those tests, not fall through to test-smoke.
    if (file.startsWith('skills-toolbox/')) {
      selectWholeTarget(selections, 'test-scripts')
      selectedPrimaryTarget = true
      continue
    }

    // The prettier pin-drift gate (scripts/prettier-pin-drift.test.mjs) compares the
    // prettier range the root manifest declares against the one the standalone
    // services/docs package declares, AND the version each tree's lockfile actually
    // resolves (matching caret ranges still admit two different installed engines). A
    // bump to any of those four files touches no scripts/** path, so without this rule
    // the exact change class that gate exists to catch would select only test-smoke and
    // merge with the gate never having run.
    // Terminal on purpose for the three non-web inputs: they trade the whole-repo
    // test-smoke fallback for the gate that actually reads them, the same trade the
    // docs/build-and-ci.md and .boss-skills.json rules above already make.
    if (PRETTIER_PIN_INPUTS.has(file)) {
      selectWholeTarget(selections, 'test-scripts')
      selectedPrimaryTarget = true
      // Deliberately no `continue` for the root lockfile: isWebPath() claims it too, and
      // swallowing it here would silently drop the web targets it has always selected.
      if (file !== 'pnpm-lock.yaml') continue
    }

    if (isSkillPath(file)) {
      selectWholeTarget(selections, 'test-manifest')
      selectWholeTarget(selections, 'test-no-inline-stop-hooks')
      // A SKILL.md-only edit must also run the skill content tests (BOS-144), which pin
      // the byte-stable dispatch/sentinel contracts documented in the skill bodies.
      selectWholeTarget(selections, 'test-scripts')
      selectedPrimaryTarget = true
      continue
    }

    if (isManifestPath(file)) {
      selectWholeTarget(selections, 'test-manifest')
      selectedPrimaryTarget = true
      continue
    }

    // The TS workspace (web, marketing, ui-tokens) is tested via turbo behind
    // `make test-web`. lib/ui-tokens is an internal dependency of both apps, and
    // the lockfile / turbo.json are workspace-wide inputs, so any of them route
    // here. These are NOT Go modules, so select the whole target.
    if (isWebPath(file)) {
      selectWholeTarget(selections, 'test-web')
      selectedPrimaryTarget = true
      continue
    }

    // The published skill sources live INSIDE services/boss, so a payload edit there is
    // also a test-boss input (the skillinstall manifest test embeds them). Both the prose
    // (.md) and the shipped shell helpers (.sh, covered by scripts/*.test.mjs) route to the
    // script gates. Select test-scripts WITHOUT `continue`, so the moduleRules lookup below
    // still adds test-boss — dropping it would silence the manifest gate.
    if (
      file.startsWith('services/boss/internal/skillinstall/skills/') &&
      (file.endsWith('.md') || file.endsWith('.sh'))
    ) {
      selectWholeTarget(selections, 'test-scripts')
      selectedPrimaryTarget = true
    }

    const moduleRule = moduleRules.find(({ root }) => file.startsWith(root))
    if (moduleRule) {
      selectModuleTarget(selections, moduleRule, file)
      selectedPrimaryTarget = true
    }
  }

  if (selections.size === 0) {
    selectWholeTarget(selections, 'test-smoke')
  } else if (selectedExternalInputTarget && !selectedPrimaryTarget) {
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

export function nativeModuleRootsForExternalInputRule(rule) {
  return [...(rule.ledgerModules ?? []), ...(rule.patterns ?? []).map(patternToModuleRoot)].map(
    normalizeModuleRoot,
  )
}

function normalizeModuleRoot(root) {
  return String(root ?? '')
    .replaceAll('\\', '/')
    .replace(/\/+$/, '')
}

function findModuleRule(moduleRoot) {
  const normalized = normalizeModuleRoot(moduleRoot)
  return moduleRules.find(({ root }) => normalizeModuleRoot(root) === normalized)
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

// Inputs to the prettier pin-drift gate: the two manifests it compares declared ranges
// from, and the two lockfiles it compares resolved versions from.
const PRETTIER_PIN_INPUTS = new Set([
  'package.json',
  'services/docs/package.json',
  'pnpm-lock.yaml',
  'services/docs/pnpm-lock.yaml',
])

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
    //
    // When nothing Go-relevant changed the pattern list is EMPTY, and printing an
    // empty line would be indistinguishable from the script having crashed — whose
    // caller-side fail-safe is a FULL `//...` run. Print the explicit `NONE`
    // sentinel instead so the caller can tell "provably nothing to test" apart from
    // "selector failed, test everything".
    const explicitFiles = args.slice(1)
    const files = explicitFiles.length > 0 ? explicitFiles : changedFilesFromGit()
    const { patterns } = selectBazelAffected(files)
    console.log(patterns.length === 0 ? 'NONE' : patterns.join(' '))
  } else if (args[0] === '--ledger-modules') {
    // Native-ledger selection: print `ALL` (run every row) or a space-joined list
    // of module roots, or `NONE` when no native row is affected.
    const explicitFiles = args.slice(1)
    const files = explicitFiles.length > 0 ? explicitFiles : changedFilesFromGit()
    const { all, modules } = selectLedgerModules(files)
    console.log(all ? 'ALL' : modules.length === 0 ? 'NONE' : modules.join(' '))
  } else {
    const files = args.length > 0 ? args : changedFilesFromGit()
    console.log(renderMakeCommands(selectTargets(files)).join('\n'))
  }
}
