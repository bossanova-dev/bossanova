#!/usr/bin/env node

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
  'check-race-budget',
  'test-profile',
  'test-scripts',
  'test-no-inline-stop-hooks',
  'post-rebase-check',
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
    ['Race wall-clock budgets', '`make check-race-budget`'],
    ['Slow-test profiling', '`make test-profile`'],
  ]
  const moduleRows = modules.map((module) => [
    `\`${module.path}\``,
    `\`${renderCommand(module.target)}\``,
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
    '`make test-smoke` runs `node --test scripts/bs-*-skill.test.mjs`. That glob does **not** match `scripts/boss-skill.test.mjs`, `scripts/boss-build-skill.test.mjs`, or `scripts/check-agent-test-guidance.test.mjs`, so those suites — including their exact-size ratchets — are not covered by a smoke run. `make test-scripts` (the `test` target in `scripts/Makefile`) runs every `scripts/*.test.mjs` and is the target that covers them, along with the `check-vacuous-regions.mjs` and `check-raw-size-ratchets.mjs` gates. Neither of those two is a whole-tree scan, and neither claims to be: `check-vacuous-regions.mjs` reads `scripts/` and `skills-toolbox/` only, and `check-raw-size-ratchets.mjs` reads `scripts/` files matching `skill*.test.mjs` plus one named extra. `make lint-scripts` reaches `scripts/Makefile` `lint`, including `node scripts/check-exec-waitdelay.mjs`, the BOS-927 full-tree Go exec captured-output gate. Each gate prints its own scope and residual with its success line — read that line, not this sentence, for what a given run actually covered.',
    '',
    '`codex-skills-check` (the `.codex` mirror staleness check) is a prerequisite of `make test-smoke` and `make test-all`, but **not** of `make test` / `make test-affected` — those run only the commands `scripts/select-affected-tests.mjs` picked, and reach `test-smoke` only when the selection is empty. A change that leaves a `.codex` mirror stale can therefore pass `make test`. The per-skill `assertMirrorRegenerated` checks in the `bs-sweep-*` suites close this for those skills by regenerating the mirror in memory and comparing exactly; size is never the discriminator, because the generated header makes a healthy mirror larger than its source.',
    '',
    '`scripts/select-affected-routing.test.mjs` compares workflow `paths:` filters with the local affected selector and keeps intentional divergences in its `workflowRouteExemptions` ledger. This is the mechanical guard for the false-green class described in `CLAUDE.md` § "Commands whose result lies"; update the ledger when a workflow path is intentionally broader than the local edit loop.',
    '',
    '### Where `-race` actually runs (BOS-1022)',
    '',
    // Same rule as the BOS-768 block above: this file is byte-for-byte generated, so
    // prose belongs here and nowhere else.
    "The race detector is **opt-in locally** (`make test-race`, or `RACE=1` on a module target) and **not** part of the per-PR gate: `bazel.yml`'s `go-test` job is deliberately plain. The only whole-graph plain+race pass in this repo is `bazel-linux-smoke.yml`, and it runs on the release tier only — `push` to `staging`/`production`, `pull_request` into those branches, and `workflow_dispatch`; `main` is deliberately absent. `bazel.yml`'s `release-gates` job adds `make test-native-ledger RACE=1`, which is the only race coverage the native ledger rows get. `ci.yml` is `workflow_dispatch:`-only here and survives as the public mirror's source; `test-go.yml` does set `RACE: \"1\"` on every non-`main` push, but is gated to `github.repository == 'bossanova-dev/bossanova'`, so on the private repo it is a no-op skip. Read those `on:` blocks before claiming a change is race-covered — a green feature PR has not run the race detector at all.",
    '',
    "`-race` also enables Go's `checkptr` instrumentation, which is brutal for the pure-Go SQLite driver (`modernc.org/sqlite`): a package that replays goose migrations once per test pays that tax on every replay, which is how `//services/bossd/internal/db` reached 512s and `//services/bossd/internal/server` 400s. `make check-race-budget` is the ratchet that keeps them down. It re-runs the targets in `scripts/race-budgets.json` under `--config=race` and scores each against its `budgetSeconds` via `scripts/check-race-budget.mjs`; CI runs the same pair as the final step of `bazel-linux-smoke.yml`'s race leg. Two rules decide whether a run means anything: shard durations are **summed, never maxed** (a sharded target that is slow everywhere must not read as fast), and any shard Bazel reports as cache-served is a hard failure, so `--nocache_test_results` is load-bearing rather than belt-and-braces — without it a cached result from an older commit scores a pass. Durations come from the Build Event Protocol stream (`--build_event_json_file`), not from JUnit `test.xml`: rules_go writes an empty `<testsuites></testsuites>` for a target that **passes**, so a `test.xml`-based gate could never score a green run.",
    '',
    '### Test gate cache and uncached misses (BOS-1021)',
    '',
    // Same rule as the BOS-768 block above: this file is byte-for-byte generated, so
    // prose belongs here and nowhere else.
    'Cache-eligible test gates are keyed on the whole working-tree content hash, the fully resolved command, the merge-base commit, and a fingerprint of toolchain versions plus expansion-affecting variables (`RACE`, `BOSS_NO_BAZEL`, `BAZEL_TEST_FLAGS`, `BAZEL_TEST_EXTRA_FLAGS`, `MAKEFLAGS`, `MAKEOVERRIDES`). A cache hit skips the gate and prints `cached` with a 12-character tree hash; a miss prints `fresh` and records a stamp only after a zero exit. Different commit messages over an identical tree therefore hit, while a changed command, changed base, race/plain switch, Make override, or Bazel flag switch misses. The cache is opt-in: undeclared gates, gates reading outside the whole-tree corpus, nondeterministic gates, unusable stamp directories, unresolvable bases, and failing git reads all fail open to a real run.',
    '',
    'When the branch adds or renames a file, including an uncommitted or untracked one, a cache miss forces an uncached runner path. For Bazel targets that means `--nocache_test_results`; for the native fallback it means `GO_TEST_COUNT=1`, which expands to `go test -count=1`. The repo opts into that behavior with `commands.testUncached` in `.boss-skills.json`; tools must consult that key rather than inventing a runner flag. The ordering is deliberate: check the exact gate cache first, then apply the uncached rule only on a miss.',
    '',
    '### Keeping a bossd package off the ratchet',
    '',
    'Two idioms came out of that work and are worth reaching for before adding a test to a slow package. **Build the schema once per binary**: `services/bossd/internal/dbtest` replays the migrations a single time, captures the resulting schema as a script, and builds every later database from it — `dbtest.New(t)` for an ordinary migrated database, `dbtest.NewMigrated(t)` for tests that assert on migration behaviour itself and so must not read back the output of the thing under test. It also owns the goose mutex: `migrate.Run`/`RunUpTo`/`RunDownTo` all write goose\'s package-level `SetBaseFS`/`SetDialect` globals, so two tests migrating concurrently race even on unrelated databases, and routing every goose entry point through `dbtest` gives callers that guarantee by construction. **Wait on the outcome you are about to assert**, never on a fixed settle sleep: a sleep tuned to be "long enough" without the race detector is either flaky or wasteful under it, and a poll against the same condition the assertion checks still fails when the outcome never arrives — just at a deadline instead of instantly.',
    '',
    '## Web Targets (`services/web`)',
    '',
    'Frontend Biome lint authoring traps are documented in',
    '[`docs/testing/frontend-lint-gates.md`](frontend-lint-gates.md).',
    '',
    ...renderTable(['Command', 'Description', 'CI?'], webRows),
    '',
    'Run from `services/web/`. `test:e2e:real` requires `E2E_REAL=1` and a running bossd+bosso stack; it is never set in CI.',
    '',
    "The repo-root `make test-web-e2e` target delegates to this module's Tier-1 `pnpm run test:e2e` (the Playwright faked suite, including the `smoke.spec.ts` harness-boot spec). It first does a best-effort `playwright install chromium-headless-shell` (non-fatal; fallback: `pnpm run test:e2e:install`), then runs `make -C services/web test-e2e`. It is opt-in / release-tier — NOT a per-feature-PR gate and NOT part of the default `make test` graph (the release-only `web-e2e` CI job invokes Playwright directly).",
    '',
    '## Go Module Targets',
    '',
    ...renderTable(['Module', 'Target'], moduleRows),
    '',
  ]

  return lines.join('\n')
}

export function buildManifest({ root = repoRoot } = {}) {
  return renderManifest({
    rootTargets: defaultRootTargets,
    modules: discoverModuleTargets(root),
  })
}

import { isMainModule } from '../skills-toolbox/main-module.mjs'

if (isMainModule(import.meta.url)) {
  process.stdout.write(buildManifest())
}
