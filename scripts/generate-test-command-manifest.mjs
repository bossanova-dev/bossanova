#!/usr/bin/env node

import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(scriptDirectory, '..')
const moduleRoots = ['lib', 'services', 'plugins']

const defaultRootTargets = [
  'test',
  'test-all',
  'test-smoke',
  'test-affected',
  'test-full',
  'test-race',
  'test-profile',
  'test-scripts',
  'test-no-inline-stop-hooks',
  'test-readme',
  'test-public-mirror',
  'test-web-e2e',
]

// Hand-curated: the web suite is npm-script driven and cannot be derived from go.mod layout.
const defaultWebTargets = [
  {
    command: 'pnpm run typecheck',
    description:
      'TypeScript type-check (includes `tests` via `tsconfig.test.json` and `tests/e2e` via `tsconfig.e2e.json`)',
    ci: 'yes',
  },
  { command: 'pnpm run lint', description: 'Biome lint + format check', ci: 'yes' },
  { command: 'pnpm run test', description: 'Vitest unit tests', ci: 'yes' },
  { command: 'pnpm run build', description: 'Vite production build', ci: 'yes' },
  {
    command: 'pnpm run test:e2e',
    description:
      'Playwright Tier-1 faked suite (all specs under `tests/e2e/specs/` + `coverage.spec.ts`)',
    ci: 'yes',
  },
  {
    command: 'pnpm run test:e2e:real',
    description:
      'Playwright Tier-2 real-stack smoke (local only; requires Go toolchain + `BOSS_E2E_TEST_AUTH`; tests are `test.fixme` pending repo-seeding — see docs/plans/BOS-33)',
    ci: 'no',
  },
]

function listGoModPaths(root = repoRoot) {
  return moduleRoots.flatMap((moduleRoot) => {
    const absoluteModuleRoot = path.join(root, moduleRoot)
    if (!fs.existsSync(absoluteModuleRoot)) {
      return []
    }

    return fs
      .readdirSync(absoluteModuleRoot, { withFileTypes: true })
      .filter((entry) => entry.isDirectory())
      .map((entry) => path.posix.join(moduleRoot, entry.name, 'go.mod'))
      .filter((goModPath) => fs.existsSync(path.join(root, goModPath)))
  })
}

function targetForModule(modulePath) {
  const moduleName = path.posix.basename(modulePath)
  const targetName = modulePath.startsWith('plugins/')
    ? moduleName.replace(/^bossd-plugin-/, '')
    : moduleName

  return `test-${targetName}`
}

export function deriveModuleTargets(goModPaths) {
  const orderByRoot = new Map(moduleRoots.map((moduleRoot, index) => [moduleRoot, index]))

  return goModPaths
    .map((goModPath) => path.posix.dirname(goModPath))
    .sort((left, right) => {
      const [leftRoot] = left.split('/')
      const [rightRoot] = right.split('/')
      const rootOrder = (orderByRoot.get(leftRoot) ?? 99) - (orderByRoot.get(rightRoot) ?? 99)

      return rootOrder || left.localeCompare(right)
    })
    .map((modulePath) => ({
      path: modulePath,
      target: targetForModule(modulePath),
    }))
}

export function discoverModuleTargets(root = repoRoot) {
  return deriveModuleTargets(listGoModPaths(root))
}

function countTestFiles(modulePath) {
  const absoluteModulePath = path.join(repoRoot, modulePath)
  if (!fs.existsSync(absoluteModulePath)) {
    return 0
  }

  const output = execFileSync('find', [modulePath, '-name', '*_test.go'], {
    cwd: repoRoot,
    encoding: 'utf8',
  }).trim()

  if (output === '') {
    return 0
  }

  return output.split('\n').length
}

function renderCommand(target) {
  return `make ${target}`
}

function padCell(value, width, align = 'left') {
  return align === 'right' ? value.padStart(width) : value.padEnd(width)
}

function renderTable(headers, rows, alignments = []) {
  const widths = headers.map((header, index) =>
    Math.max(header.length, ...rows.map((row) => row[index].length)),
  )
  const separator = widths.map((width, index) => {
    const alignment = alignments[index] ?? 'left'
    return alignment === 'right' ? `${'-'.repeat(width - 1)}:` : '-'.repeat(width)
  })
  const renderRow = (row) =>
    `| ${row.map((cell, index) => padCell(cell, widths[index], alignments[index])).join(' | ')} |`

  return [renderRow(headers), renderRow(separator), ...rows.map(renderRow)]
}

export function renderManifest({ rootTargets, modules, webTargets = defaultWebTargets }) {
  const ladderRows = [
    ['Fast local confidence', '`make test-smoke`'],
    ['Default edit-loop (affected)', '`make test`'],
    ['Changed-file selection', '`make test-affected`'],
    ['Full/exhaustive suite', '`make test-all`'],
    ['Race detector pass', '`make test-race`'],
    ['Slow-test profiling', '`make test-profile`'],
  ]
  const moduleRows = modules.map((module) => [
    `\`${module.path}\``,
    `\`${renderCommand(module.target)}\``,
    String(module.testFiles),
  ])
  const webRows = webTargets.map((target) => [
    `\`${target.command}\``,
    target.description,
    target.ci,
  ])
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
    '### Coverage gaps worth knowing (BOS-768)',
    '',
    // This manifest is byte-for-byte generated and `scripts/check-test-command-manifest.mjs`
    // fails if the checked-in file differs, so prose belongs HERE and nowhere else. Editing
    // docs/testing/test-command-manifest.md by hand turns that gate red on the next run.
    '`make test-smoke` runs `node --test scripts/bs-*-skill.test.mjs`. That glob does **not** match `scripts/boss-skill.test.mjs`, `scripts/boss-build-skill.test.mjs`, or `scripts/check-agent-test-guidance.test.mjs`, so those suites — including their exact-size ratchets — are not covered by a smoke run. `make test-scripts` (the `test` target in `scripts/Makefile`) runs every `scripts/*.test.mjs` and is the target that covers them, along with the `check-vacuous-regions.mjs` and `check-raw-size-ratchets.mjs` gates. Neither of those two is a whole-tree scan, and neither claims to be: `check-vacuous-regions.mjs` reads `scripts/` and `skills-toolbox/` only, and `check-raw-size-ratchets.mjs` reads `scripts/` files matching `skill*.test.mjs` plus one named extra. Each prints its own scope and residual with its success line — read that line, not this sentence, for what a given run actually covered.',
    '',
    '`codex-skills-check` (the `.codex` mirror staleness check) is a prerequisite of `make test-smoke` and `make test-all`, but **not** of `make test` / `make test-affected` — those run only the commands `scripts/select-affected-tests.mjs` picked, and reach `test-smoke` only when the selection is empty. A change that leaves a `.codex` mirror stale can therefore pass `make test`. The per-skill `assertMirrorRegenerated` checks in the `bs-sweep-*` suites close this for those skills by regenerating the mirror in memory and comparing exactly; size is never the discriminator, because the generated header makes a healthy mirror larger than its source.',
    '',
    '## Web Targets (`services/web`)',
    '',
    ...renderTable(['Command', 'Description', 'CI?'], webRows),
    '',
    'Run from `services/web/`. `test:e2e:real` requires `E2E_REAL=1` and a running bossd+bosso stack; it is never set in CI.',
    '',
    "The repo-root `make test-web-e2e` target delegates to this module's Tier-1 `pnpm run test:e2e` (the Playwright faked suite, including the `smoke.spec.ts` harness-boot spec). It first does a best-effort `playwright install chromium` (non-fatal; fallback: `pnpm run test:e2e:install`), then runs `make -C services/web test-e2e`. It is opt-in / release-tier — NOT a per-feature-PR gate and NOT part of the default `make test` graph (the release-only `web-e2e` CI job invokes Playwright directly).",
    '',
    '## Go Module Targets',
    '',
    ...renderTable(['Module', 'Target', 'Test files'], moduleRows, ['left', 'left', 'right']),
    '',
  ]

  return lines.join('\n')
}

function buildManifest() {
  return renderManifest({
    rootTargets: defaultRootTargets,
    modules: discoverModuleTargets().map((module) => ({
      ...module,
      testFiles: countTestFiles(module.path),
    })),
  })
}

import { isMainModule } from '../skills-toolbox/main-module.mjs'

if (isMainModule(import.meta.url)) {
  process.stdout.write(buildManifest())
}
