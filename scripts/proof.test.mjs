import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { silenceConsole } from './quiet-test-console.mjs'

import {
  __resetTuiAgentBridgeCache,
  agentSurface,
  buildTuiAgentBridge,
  defaultBinFresh,
  evaluateRunPreflight,
  isDocsOnlyChange,
  newestSourceMtime,
  prefixStillFileNames,
  resolvePrebuiltTuiBins,
  resolveSurfacePlan,
  shouldPostDocsBuildCheck,
  shouldCleanupRunDir,
  tuiAgentBridgeEnv,
  tuiAgentCanCapture,
  tuiAgentUsable,
  uploadBundle,
} from './proof.mjs'

const repoRootForTest = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
import {
  DEFAULT_TOTAL_PROOF_BUDGET_MS,
  TUI_REPLAY_EXTRA_MS,
  planSurfaceBudget,
  planTuiLegs,
  selectRecipes,
} from './proof-lib.mjs'
import { classifyTuiSurface, committedScenarioPresent, surfaceBudget } from './proof-surfaces.mjs'
import { aggregateExitCode, classifySurfaceOutcomes } from './proof-finalize-outcome.mjs'
import {
  builtinAgentDrivers,
  driverForSurface,
  validateSurfaceRun,
} from './proof-agent-drivers.mjs'
import { runTuiWithReplayFallback } from './proof-tui-agent.mjs'

// Silence the code-under-test's console output (DEGRADED warnings + run-file
// manifest JSON dumps) so a passing run stays quiet. See quiet-test-console.mjs.
silenceConsole()

// ── evaluateRunPreflight (BOS-138 hard preflight) ─────────────────────────────

const PREFLIGHT_IDS = [
  'PROOF_ANTHROPIC_API_KEY',
  'CLOUDFLARE_API_TOKEN',
  'BOSS_PROOF_R2_BUCKET',
  'BOSS_PROOF_PUBLIC_BASE_URL',
  'CLOUDFLARE_ACCOUNT_ID',
  'agg',
  'ffmpeg',
  'chromium',
  'web-node-modules',
  'go-toolchain',
  'gh-auth',
  'git-credential',
]

function preflightLookups(present) {
  const has = (id) => present.has(id)
  return {
    env: (key) => (has(key) ? 'x' : undefined),
    hasBin: (bin) => has(bin),
    chromiumPresent: () => has('chromium'),
    webDepsPresent: () => has('web-node-modules'),
    goToolchainPresent: () => has('go-toolchain'),
    ghAuthOk: () => has('gh-auth'),
    gitCredentialOk: () => has('git-credential'),
  }
}

test('evaluateRunPreflight: all prerequisites present → null (proceed, run pipeline)', () => {
  const decision = evaluateRunPreflight({
    surface: 'web',
    shouldUpload: true,
    lookups: preflightLookups(new Set(PREFLIGHT_IDS)),
    env: {},
  })
  assert.equal(decision, null)
})

test('evaluateRunPreflight: a missing web prereq → env-unavailable naming exactly what is missing (no stage started)', () => {
  const present = new Set(PREFLIGHT_IDS)
  present.delete('ffmpeg')
  const decision = evaluateRunPreflight({
    surface: 'web',
    shouldUpload: true,
    lookups: preflightLookups(present),
    env: {},
  })
  assert.ok(decision, 'a missing prereq must defer the run before any pipeline stage')
  assert.equal(decision.reasonCode, 'env-unavailable')
  assert.deepEqual(decision.missing, ['ffmpeg'])
  assert.equal(decision.report.ok, false)
})

test('evaluateRunPreflight: BOSS_PROOF_MODE=agent satisfies the agent-key requirement', () => {
  const present = new Set(PREFLIGHT_IDS)
  present.delete('PROOF_ANTHROPIC_API_KEY')
  const decision = evaluateRunPreflight({
    surface: 'tui',
    shouldUpload: true,
    lookups: preflightLookups(present),
    env: { BOSS_PROOF_MODE: 'agent' },
  })
  assert.equal(decision, null, 'agent mode forces the key present; nothing else is missing')
})

test('evaluateRunPreflight: the recipe surface requires the recipe path deps, not the agent key/agg/Go', () => {
  // An explicit --recipe run takes the deterministic recipe path (main() passes
  // surface:'recipe'). It must NOT over-require the agent key / agg / Go (a TUI
  // surface would), and it MUST require chromium + web deps the recipe path uses.
  const recipeDepsPresent = new Set([
    'ffmpeg',
    'chromium',
    'web-node-modules',
    ...[
      'CLOUDFLARE_API_TOKEN',
      'BOSS_PROOF_R2_BUCKET',
      'BOSS_PROOF_PUBLIC_BASE_URL',
      'CLOUDFLARE_ACCOUNT_ID',
    ],
    'gh-auth',
    'git-credential',
  ])
  // Agent key / agg / Go deliberately absent — a recipe run must not care.
  assert.equal(
    evaluateRunPreflight({
      surface: 'recipe',
      shouldUpload: true,
      lookups: preflightLookups(recipeDepsPresent),
      env: {},
    }),
    null,
    'recipe preflight must pass without the agent key / agg / Go',
  )
  // Drop a real recipe-path dep → it must defer.
  const missingChromium = new Set(recipeDepsPresent)
  missingChromium.delete('chromium')
  const decision = evaluateRunPreflight({
    surface: 'recipe',
    shouldUpload: true,
    lookups: preflightLookups(missingChromium),
    env: {},
  })
  assert.ok(decision, 'a missing recipe-path dep must defer')
  assert.deepEqual(decision.missing, ['chromium'])
})

test('evaluateRunPreflight: dry-run drops upload/push creds from the required set', () => {
  const present = new Set([
    'PROOF_ANTHROPIC_API_KEY',
    'ffmpeg',
    'chromium',
    'web-node-modules',
    'agg',
    'go-toolchain',
  ])
  const decision = evaluateRunPreflight({
    surface: 'web',
    shouldUpload: false,
    lookups: preflightLookups(present),
    env: {},
  })
  assert.equal(decision, null, 'a dry-run must not defer on missing R2/gh credentials')
})

// ── agentSurface() routing ────────────────────────────────────────────────────

/** Minimal catalog fixture covering all three path-rule surfaces. */
const fixturecat = {
  recipes: [
    { id: 'tui-home', surface: 'tui' },
    { id: 'web-sessions', surface: 'web' },
    { id: 'marketing-home', surface: 'marketing' },
  ],
  pathRules: [
    {
      name: 'TUI views',
      patterns: ['services/boss/internal/views/'],
      recipeIds: ['tui-home'],
    },
    {
      name: 'Product web',
      patterns: ['services/web/'],
      recipeIds: ['web-sessions'],
    },
    {
      name: 'Marketing site',
      patterns: ['services/marketing/'],
      recipeIds: ['marketing-home'],
    },
  ],
}

const agentSurfaceEnvKeys = ['BOSS_PROOF_BRIEF', 'BOSS_PROOF_AGENT_SURFACE']

function withAgentSurfaceEnv(env, fn) {
  const previous = new Map(agentSurfaceEnvKeys.map((key) => [key, process.env[key]]))
  try {
    for (const key of agentSurfaceEnvKeys) {
      if (Object.hasOwn(env, key)) {
        const value = env[key]
        if (value === undefined) delete process.env[key]
        else process.env[key] = value
      } else {
        delete process.env[key]
      }
    }
    return fn()
  } finally {
    for (const [key, value] of previous) {
      if (value === undefined) delete process.env[key]
      else process.env[key] = value
    }
  }
}

function withTempBrief(brief, fn) {
  const briefPath = path.join(os.tmpdir(), `brief-${Date.now()}-${process.pid}.json`)
  fs.writeFileSync(briefPath, JSON.stringify(brief))
  try {
    return fn(briefPath)
  } finally {
    fs.rmSync(briefPath, { force: true })
  }
}

test('agentSurface: TUI-only changed files → tui', () => {
  withAgentSurfaceEnv({}, () => {
    assert.equal(
      agentSurface({ catalog: fixturecat, changedFiles: ['services/boss/internal/views/foo.go'] }),
      'tui',
    )
  })
})

test('agentSurface: web changed files → web', () => {
  withAgentSurfaceEnv({}, () => {
    assert.equal(
      agentSurface({ catalog: fixturecat, changedFiles: ['services/web/src/App.tsx'] }),
      'web',
    )
  })
})

// ── BOS-220 TUI scenario gate (runAgentSurfaces): committedScenarioPresent ────
// The dispatcher gates the TUI surface on `committedScenarioPresent(changedFiles)`:
// a TUI-classified diff with NO committed proof/scenarios/*.scenario.json defers
// with a single warn-only `scenario-missing` run; a diff that commits one skips the
// gate and proceeds. These pin the two real inputs the gate reads (the surface is
// TUI via classifyTuiSurface, and the scenario predicate) so the gate decision can't
// regress — e.g. the predicate matching a recipe, or the TUI surface losing its gate.
test('TUI gate: a TUI diff with no committed scenario is the scenario-missing case', () => {
  const changedFiles = ['services/boss/internal/views/home.go']
  assert.equal(classifyTuiSurface(changedFiles), true, 'diff classifies as the TUI surface')
  assert.equal(
    committedScenarioPresent(changedFiles),
    false,
    'no scenario present ⇒ dispatcher defers with scenario-missing',
  )
})

test('TUI gate: a TUI diff that commits a scenario skips the gate and proceeds', () => {
  const changedFiles = [
    'services/boss/internal/views/home.go',
    'proof/scenarios/home.scenario.json',
  ]
  assert.equal(classifyTuiSurface(changedFiles), true, 'still classifies as the TUI surface')
  assert.equal(
    committedScenarioPresent(changedFiles),
    true,
    'committed scenario present ⇒ gate skipped, agent runs normally',
  )
})

test('agentSurface: marketing changed files → recipe (deterministic capture, not the web agent)', () => {
  withAgentSurfaceEnv({}, () => {
    assert.equal(
      agentSurface({
        catalog: fixturecat,
        changedFiles: ['services/marketing/pages/index.astro'],
      }),
      'recipe',
    )
  })
})

test('agentSurface: no-match changed files → web', () => {
  withAgentSurfaceEnv({}, () => {
    assert.equal(agentSurface({ catalog: fixturecat, changedFiles: ['README.md'] }), 'web')
  })
})

test('agentSurface: an explicit brief surface wins over the catalog-derived default', () => {
  withTempBrief({ title: 't', description: 'd', surface: 'tui' }, (briefPath) => {
    withAgentSurfaceEnv({ BOSS_PROOF_BRIEF: briefPath }, () => {
      assert.equal(agentSurface({ catalog: fixturecat, changedFiles: [] }), 'tui')
    })
  })
})

test('agentSurface: BOSS_PROOF_AGENT_SURFACE still overrides the brief surface', () => {
  withTempBrief({ title: 't', description: 'd', surface: 'tui' }, (briefPath) => {
    withAgentSurfaceEnv({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_AGENT_SURFACE: 'web' }, () => {
      assert.equal(agentSurface({ catalog: fixturecat, changedFiles: [] }), 'web')
    })
  })
})

test('agentSurface: unsupported brief surface falls back to catalog-derived result', () => {
  withTempBrief({ title: 't', description: 'd', surface: 'cli' }, (briefPath) => {
    withAgentSurfaceEnv({ BOSS_PROOF_BRIEF: briefPath }, () => {
      assert.equal(
        agentSurface({ catalog: fixturecat, changedFiles: ['services/web/src/App.tsx'] }),
        'web',
      )
    })
  })
})

test('agentSurface: BOSS_PROOF_AGENT_SURFACE=web forces web even for TUI files', () => {
  withAgentSurfaceEnv({ BOSS_PROOF_AGENT_SURFACE: 'web' }, () => {
    assert.equal(
      agentSurface({ catalog: fixturecat, changedFiles: ['services/boss/internal/views/foo.go'] }),
      'web',
    )
  })
})

test('agentSurface: BOSS_PROOF_AGENT_SURFACE=tui forces tui even for web files', () => {
  withAgentSurfaceEnv({ BOSS_PROOF_AGENT_SURFACE: 'tui' }, () => {
    assert.equal(
      agentSurface({ catalog: fixturecat, changedFiles: ['services/web/src/App.tsx'] }),
      'tui',
    )
  })
})

test('tuiAgentBridgeEnv preserves a caller-provided boss binary', () => {
  assert.deepEqual(
    tuiAgentBridgeEnv({
      bridgeBin: '/tmp/proof-tui-bridge',
      bossBin: '/tmp/boss-e2e',
      existingBossBin: '/custom/boss',
    }),
    {
      BOSS_PROOF_TUI_BRIDGE_BIN: '/tmp/proof-tui-bridge',
      BOSS_PROOF_BOSS_BIN: '/custom/boss',
    },
  )
})

test('tuiAgentBridgeEnv uses the temp boss binary when no caller binary exists', () => {
  assert.deepEqual(
    tuiAgentBridgeEnv({
      bridgeBin: '/tmp/proof-tui-bridge',
      bossBin: '/tmp/boss-e2e',
    }),
    {
      BOSS_PROOF_TUI_BRIDGE_BIN: '/tmp/proof-tui-bridge',
      BOSS_PROOF_BOSS_BIN: '/tmp/boss-e2e',
    },
  )
})

// ── resolvePrebuiltTuiBins precedence (BOS-215) ───────────────────────────────

test('resolvePrebuiltTuiBins: env vars win when the files exist', () => {
  const got = resolvePrebuiltTuiBins({
    repoRoot: '/repo',
    env: { BOSS_PROOF_TUI_BRIDGE_BIN: '/x/bridge', BOSS_PROOF_BOSS_BIN: '/x/boss' },
    fileExists: (p) => p === '/x/bridge' || p === '/x/boss',
  })
  assert.deepEqual(got, { bridgeBin: '/x/bridge', bossBin: '/x/boss' })
})

test('resolvePrebuiltTuiBins: env var set but file missing falls back to null', () => {
  const got = resolvePrebuiltTuiBins({
    repoRoot: '/repo',
    env: { BOSS_PROOF_TUI_BRIDGE_BIN: '/gone/bridge' },
    fileExists: () => false,
  })
  assert.deepEqual(got, { bridgeBin: null, bossBin: null })
})

test('resolvePrebuiltTuiBins: default ./bin location used when both binaries exist', () => {
  const got = resolvePrebuiltTuiBins({
    repoRoot: '/repo',
    env: {},
    fileExists: (p) => p === '/repo/bin/proof-tui-bridge' || p === '/repo/bin/boss-e2e',
  })
  assert.deepEqual(got, {
    bridgeBin: '/repo/bin/proof-tui-bridge',
    bossBin: '/repo/bin/boss-e2e',
  })
})

test('resolvePrebuiltTuiBins: only one default binary present ⇒ that one resolves, other is null', () => {
  const got = resolvePrebuiltTuiBins({
    repoRoot: '/repo',
    env: {},
    fileExists: (p) => p === '/repo/bin/proof-tui-bridge',
  })
  assert.deepEqual(got, { bridgeBin: '/repo/bin/proof-tui-bridge', bossBin: null })
})

test('resolvePrebuiltTuiBins: stale default binaries fall back to null so the driver rebuilds', () => {
  const got = resolvePrebuiltTuiBins({
    repoRoot: '/repo',
    env: {},
    fileExists: () => true,
    binFresh: () => false,
  })
  assert.deepEqual(got, { bridgeBin: null, bossBin: null })
})

test('resolvePrebuiltTuiBins: fresh default binaries are reused', () => {
  const got = resolvePrebuiltTuiBins({
    repoRoot: '/repo',
    env: {},
    fileExists: () => true,
    binFresh: () => true,
  })
  assert.deepEqual(got, {
    bridgeBin: '/repo/bin/proof-tui-bridge',
    bossBin: '/repo/bin/boss-e2e',
  })
})

test('resolvePrebuiltTuiBins: env overrides bypass the freshness gate', () => {
  let freshChecks = 0
  const got = resolvePrebuiltTuiBins({
    repoRoot: '/repo',
    env: { BOSS_PROOF_TUI_BRIDGE_BIN: '/x/bridge', BOSS_PROOF_BOSS_BIN: '/x/boss' },
    fileExists: (p) => p === '/x/bridge' || p === '/x/boss',
    binFresh: () => {
      freshChecks += 1
      return false
    },
  })
  assert.deepEqual(got, { bridgeBin: '/x/bridge', bossBin: '/x/boss' })
  assert.equal(freshChecks, 0)
})

// ── defaultBinFresh / newestSourceMtime build-input scan (BOS-215) ────────────

// Build an injectable fake fs from a { dirs, mtimes } spec. `dirs` maps a
// directory path to its child entries ({ name, dir }); `mtimes` maps a file path
// to its mtimeMs. Unknown dirs/files throw, mirroring ENOENT on the real fs.
function fakeFs({ dirs = {}, mtimes = {} } = {}) {
  return {
    readdir: (dir) => {
      const entries = dirs[dir]
      if (!entries) throw new Error(`ENOENT: ${dir}`)
      return entries.map((e) => ({ name: e.name, isDirectory: () => Boolean(e.dir) }))
    },
    statSync: (p) => {
      if (!(p in mtimes)) throw new Error(`ENOENT: ${p}`)
      return { mtimeMs: mtimes[p] }
    },
  }
}

test('newestSourceMtime: embedded non-.go payloads count toward the newest mtime', () => {
  const { readdir, statSync } = fakeFs({
    dirs: {
      '/r/services/boss': [{ name: 'main.go' }, { name: 'skills', dir: true }],
      '/r/services/boss/skills': [{ name: 'boss-review.md' }],
    },
    mtimes: {
      '/r/services/boss/main.go': 100,
      '/r/services/boss/skills/boss-review.md': 300,
    },
  })
  // The .md payload (300) must win over the .go source (100); the old extension
  // allowlist would have ignored it and returned 100.
  assert.equal(newestSourceMtime(['/r/services/boss'], { statSync, readdir }), 300)
})

test('defaultBinFresh: a newer embedded skill payload marks the default bin stale', () => {
  const { readdir, statSync } = fakeFs({
    dirs: {
      '/r/services/boss': [{ name: 'skills', dir: true }],
      '/r/services/boss/skills': [{ name: 'boss-review.md' }],
      '/r/lib/bossalib': [],
    },
    mtimes: {
      '/r/bin/boss-e2e': 100,
      '/r/services/boss/skills/boss-review.md': 200,
    },
  })
  // bin mtime 100 < embedded payload mtime 200 ⇒ stale ⇒ rebuild.
  assert.equal(defaultBinFresh('/r/bin/boss-e2e', { root: '/r', statSync, readdir }), false)
})

test('defaultBinFresh: bin newer than every build input is fresh', () => {
  const { readdir, statSync } = fakeFs({
    dirs: {
      '/r/services/boss': [{ name: 'main.go' }, { name: 'skills', dir: true }],
      '/r/services/boss/skills': [{ name: 'boss-review.md' }],
      '/r/lib/bossalib': [],
    },
    mtimes: {
      '/r/bin/boss-e2e': 500,
      '/r/services/boss/main.go': 100,
      '/r/services/boss/skills/boss-review.md': 200,
    },
  })
  assert.equal(defaultBinFresh('/r/bin/boss-e2e', { root: '/r', statSync, readdir }), true)
})

test('defaultBinFresh: a source deletion bumps the directory mtime and marks the bin stale', () => {
  // Every surviving file is older than the bin, but a deleted/renamed source
  // bumped the containing directory's mtime past the bin's ⇒ must rebuild.
  const { readdir, statSync } = fakeFs({
    dirs: {
      '/r/services/boss': [{ name: 'main.go' }],
      '/r/lib/bossalib': [],
    },
    mtimes: {
      '/r/bin/boss-e2e': 500,
      '/r/services/boss': 600,
      '/r/services/boss/main.go': 100,
    },
  })
  assert.equal(defaultBinFresh('/r/bin/boss-e2e', { root: '/r', statSync, readdir }), false)
})

test('defaultBinFresh: a newer repo-root go.work marks the default bin stale', () => {
  // A workspace-only change (go.work) touches no file under the source roots but
  // still alters the built binary, so it must force a rebuild.
  const { readdir, statSync } = fakeFs({
    dirs: {
      '/r/services/boss': [{ name: 'main.go' }],
      '/r/lib/bossalib': [],
    },
    mtimes: {
      '/r/bin/boss-e2e': 500,
      '/r/services/boss/main.go': 100,
      '/r/go.work': 700,
    },
  })
  assert.equal(defaultBinFresh('/r/bin/boss-e2e', { root: '/r', statSync, readdir }), false)
})

test('defaultBinFresh: deleting a repo-root go.work bumps the repo-root mtime and marks the bin stale', () => {
  // The workspace file is gone (absent from mtimes ⇒ statSync throws), and every
  // surviving source file is older than the bin. Only the repo-root directory
  // mtime — bumped by unlinking go.work — is newer, so the bin must rebuild
  // instead of reusing one built under the old (go.work-present) workspace.
  const { readdir, statSync } = fakeFs({
    dirs: {
      '/r/services/boss': [{ name: 'main.go' }],
      '/r/lib/bossalib': [],
    },
    mtimes: {
      '/r/bin/boss-e2e': 500,
      '/r/services/boss/main.go': 100,
      // go.work / go.work.sum deleted — not present. Repo root mtime bumped past
      // the bin by the deletion.
      '/r': 700,
    },
  })
  assert.equal(defaultBinFresh('/r/bin/boss-e2e', { root: '/r', statSync, readdir }), false)
})

// ── buildTuiAgentBridge prebuilt short-circuit (BOS-215) ──────────────────────

test('buildTuiAgentBridge: both overrides ⇒ zero spawns, no tmp dir, returns overrides', () => {
  __resetTuiAgentBridgeCache()
  let spawnCalls = 0
  const got = buildTuiAgentBridge({
    bridgeBinOverride: '/pre/bridge',
    bossBinOverride: '/pre/boss',
    spawn: () => {
      spawnCalls += 1
      return { status: 0 }
    },
  })
  __resetTuiAgentBridgeCache()
  assert.equal(spawnCalls, 0)
  assert.equal(got.bridgeBin, '/pre/bridge')
  assert.equal(got.bossBin, '/pre/boss')
  assert.equal(got.dir, null)
})

// ── make proof-tui-prebuild target wiring (BOS-215) ───────────────────────────

test('Makefile proof-tui-prebuild target builds both bins into ./bin', () => {
  const mk = fs.readFileSync(path.join(repoRootForTest, 'Makefile'), 'utf8')
  assert.match(mk, /^proof-tui-prebuild:/m)
  assert.match(
    mk,
    /go build -tags e2e -o \$\(BIN_DIR\)\/proof-tui-bridge \.\/services\/boss\/cmd\/proof-tui-agent/,
  )
  assert.match(mk, /go build -tags e2e -o \$\(BIN_DIR\)\/boss-e2e \.\/services\/boss\/cmd/)
  assert.match(mk, /^\.PHONY:[\s\S]*proof-tui-prebuild/m)
})

test('prefixStillFileNames prefixes recipeId onto each fileName', () => {
  const result = prefixStillFileNames('web-flow', [{ fileName: '01-a.png', label: 'A' }])
  assert.deepEqual(result, [{ fileName: 'web-flow/01-a.png', label: 'A' }])
})

test('prefixStillFileNames returns [] for an empty array', () => {
  assert.deepEqual(prefixStillFileNames('x', []), [])
})

test('prefixStillFileNames returns [] when stills is undefined', () => {
  assert.deepEqual(prefixStillFileNames('x', undefined), [])
})

test('prefixStillFileNames returns [] when stills is null', () => {
  assert.deepEqual(prefixStillFileNames('x', null), [])
})

test('prefixStillFileNames handles multiple stills', () => {
  const stills = [
    { fileName: '01-open.png', label: 'Open' },
    { fileName: '02-close.png', label: 'Close' },
  ]
  const result = prefixStillFileNames('my-recipe', stills)
  assert.deepEqual(result, [
    { fileName: 'my-recipe/01-open.png', label: 'Open' },
    { fileName: 'my-recipe/02-close.png', label: 'Close' },
  ])
})

test('isDocsOnlyChange detects docs/markdown-only change sets', () => {
  assert.equal(isDocsOnlyChange(['services/docs/guides/mcp.md']), true)
  assert.equal(isDocsOnlyChange(['README.md', 'docs/cron.md']), true)
  assert.equal(isDocsOnlyChange(['services/web/src/App.tsx']), false)
  assert.equal(isDocsOnlyChange(['services/docs/x.md', 'services/boss/main.go']), false)
})

test('shouldPostDocsBuildCheck only applies when docs-only changes have no recipe', () => {
  assert.equal(
    shouldPostDocsBuildCheck({
      changedFiles: ['docs/cron.md'],
      selectedRecipes: [],
    }),
    true,
  )
  assert.equal(
    shouldPostDocsBuildCheck({
      changedFiles: ['services/docs/docs/guides/mcp.md'],
      selectedRecipes: [{ id: 'docs-home', surface: 'docs' }],
    }),
    false,
  )
  assert.equal(
    shouldPostDocsBuildCheck({
      changedFiles: ['services/docs/docs/guides/mcp.md', 'services/boss/main.go'],
      selectedRecipes: [],
    }),
    false,
  )
})

test('shouldCleanupRunDir: clean only on a real successful post', () => {
  const ok = { shouldUpload: true, hasFailure: false, prNumber: '788', keepWebm: false }
  assert.equal(shouldCleanupRunDir(ok), true)
  assert.equal(shouldCleanupRunDir({ ...ok, hasFailure: true }), false) // keep for debugging
  assert.equal(shouldCleanupRunDir({ ...ok, shouldUpload: false }), false) // dry-run keeps
  assert.equal(shouldCleanupRunDir({ ...ok, prNumber: 'local' }), false) // no PR, keep
  assert.equal(shouldCleanupRunDir({ ...ok, keepWebm: true }), false) // inspectable run
})

// ── selectRecipes path rules (default catalog) ────────────────────────────────

const defaultCatalog = JSON.parse(
  fs.readFileSync(new URL('../proof/recipes/default.json', import.meta.url), 'utf8'),
)

// BOS-115: TUI is agent-only. A boss/TUI diff matches ZERO recipes, yet
// agentSurface still routes it to the agentic TUI path via classifyTuiSurface
// (catalog-independent). This is the regression that would otherwise misroute
// a recipe-less TUI diff to the web agent.
test('a boss-only diff resolves to surface tui with zero matched recipes', () => {
  withAgentSurfaceEnv({}, () => {
    const changedFiles = ['services/boss/internal/client/cron.go']
    assert.deepEqual(
      selectRecipes(defaultCatalog, changedFiles),
      [],
      'a boss/TUI diff must match zero recipes (agent-only)',
    )
    assert.equal(agentSurface({ catalog: defaultCatalog, changedFiles }), 'tui')
  })
})

test('agentSurface routes every TUI prefix to tui against the real default catalog', () => {
  withAgentSurfaceEnv({}, () => {
    for (const file of [
      'services/boss/internal/views/home.go',
      'services/boss/internal/tuidriver/keybytes.go',
      'services/boss/cmd/root.go',
      'proto/boss.proto',
    ]) {
      assert.equal(
        agentSurface({ catalog: defaultCatalog, changedFiles: [file] }),
        'tui',
        `${file} should route to the agentic TUI surface`,
      )
    }
  })
})

test('tuiAgentUsable: key present or BOSS_PROOF_MODE=agent → usable; recipe mode or no key → not', () => {
  const keys = ['PROOF_ANTHROPIC_API_KEY', 'BOSS_PROOF_MODE']
  const prev = new Map(keys.map((k) => [k, process.env[k]]))
  const restore = () => {
    for (const [k, v] of prev) {
      if (v === undefined) delete process.env[k]
      else process.env[k] = v
    }
  }
  try {
    delete process.env.PROOF_ANTHROPIC_API_KEY
    delete process.env.BOSS_PROOF_MODE
    assert.equal(tuiAgentUsable(), false, 'no key and no mode → not usable')

    process.env.PROOF_ANTHROPIC_API_KEY = 'sk-test'
    assert.equal(tuiAgentUsable(), true, 'key present → usable')

    process.env.BOSS_PROOF_MODE = 'recipe'
    assert.equal(tuiAgentUsable(), false, 'recipe mode forces not-usable even with a key')

    delete process.env.PROOF_ANTHROPIC_API_KEY
    process.env.BOSS_PROOF_MODE = 'agent'
    assert.equal(tuiAgentUsable(), true, 'agent mode forces usable even without a key')
  } finally {
    restore()
  }
})

test('a services/web change still selects web recipes (unchanged by BOS-115)', () => {
  const recipes = selectRecipes(defaultCatalog, ['services/web/src/App.tsx'])
  assert.ok(recipes.length > 0, 'web change should match at least one web recipe')
  assert.ok(
    recipes.every((r) => r.surface === 'web'),
    'all selected recipes should have surface === web',
  )
})

test('a services/docs change selects docs recipes (incl. the MCP guide)', () => {
  const recipes = selectRecipes(defaultCatalog, ['services/docs/docs/guides/mcp.md'])
  assert.ok(recipes.length > 0, 'docs change should match at least one docs recipe')
  assert.ok(
    recipes.every((r) => r.surface === 'docs'),
    'all selected recipes should have surface === docs',
  )
  assert.ok(
    recipes.some((r) => r.id === 'docs-mcp-guide' && r.route === '/guides/mcp'),
    'docs selection should include the /guides/mcp recipe',
  )
})

test('a generic (non-MCP) docs change selects only docs-home, not the MCP-guide recipe', () => {
  const recipes = selectRecipes(defaultCatalog, ['services/docs/docs/guides/web.md'])
  const ids = recipes.map((r) => r.id)
  assert.ok(ids.includes('docs-home'), 'a docs change should still capture the docs home')
  assert.ok(
    !ids.includes('docs-mcp-guide'),
    'docs-mcp-guide (route /guides/mcp) must not be selected for an unrelated docs page',
  )
})

test('agentSurface: docs-only changes → recipe (deterministic capture)', () => {
  withAgentSurfaceEnv({}, () => {
    assert.equal(
      agentSurface({ catalog: defaultCatalog, changedFiles: ['services/docs/docs/guides/mcp.md'] }),
      'recipe',
    )
  })
})

test('agentSurface: mixed marketing + docs change → recipe', () => {
  withAgentSurfaceEnv({}, () => {
    assert.equal(
      agentSurface({
        catalog: defaultCatalog,
        changedFiles: [
          'services/marketing/src/pages/index.astro',
          'services/docs/docs/guides/mcp.md',
        ],
      }),
      'recipe',
    )
  })
})

// ── resolveSurfacePlan: multi-surface dispatch planning (BOS-139 D5/D13) ──────

const planCat = fixturecat

test('resolveSurfacePlan: mixed diff → both surfaces, cheap-first order', () => {
  const plan = resolveSurfacePlan({
    catalog: planCat,
    changedFiles: ['services/boss/internal/views/foo.go', 'services/web/src/App.tsx'],
    requiredProofBullets: [],
  })
  assert.deepEqual(plan.surfaces, { tui: true, web: true })
  assert.deepEqual(plan.order, ['tui', 'web'])
})

test('resolveSurfacePlan: web-scoped bullets order web first', () => {
  const plan = resolveSurfacePlan({
    catalog: planCat,
    changedFiles: ['services/boss/internal/views/foo.go', 'services/web/src/App.tsx'],
    requiredProofBullets: ['The /settings page shows the new toggle in the browser'],
  })
  assert.deepEqual(plan.order, ['web', 'tui'])
})

test('resolveSurfacePlan: BOSS_PROOF_AGENT_SURFACE=web narrows to web only', () => {
  const plan = resolveSurfacePlan({
    catalog: planCat,
    changedFiles: ['services/boss/internal/views/foo.go', 'services/web/src/App.tsx'],
    requiredProofBullets: [],
    env: { BOSS_PROOF_AGENT_SURFACE: 'web' },
  })
  assert.deepEqual(plan.order, ['web'])
  assert.deepEqual(plan.surfaces, { tui: false, web: true })
})

test('resolveSurfacePlan: brief surface narrows when no env override', () => {
  const plan = resolveSurfacePlan({
    catalog: planCat,
    changedFiles: ['services/boss/internal/views/foo.go', 'services/web/src/App.tsx'],
    requiredProofBullets: [],
    env: {},
    briefSurface: 'tui',
  })
  assert.deepEqual(plan.order, ['tui'])
})

test('resolveSurfacePlan: backend-only + web-scoped bullet FORCES web (D16 mitigation)', () => {
  const plan = resolveSurfacePlan({
    catalog: planCat,
    changedFiles: ['services/bossd/internal/server/server.go'],
    requiredProofBullets: ['The browser page reflects the new backend field'],
  })
  assert.equal(plan.surfaces.web, true)
  assert.deepEqual(plan.order, ['web'])
})

test('resolveSurfacePlan: backend-only + no bullets → empty order + recipes', () => {
  const plan = resolveSurfacePlan({
    catalog: planCat,
    changedFiles: ['services/bossd/internal/server/server.go'],
    requiredProofBullets: [],
  })
  assert.deepEqual(plan.order, [])
  assert.deepEqual(plan.recipes, [])
})

// ── BOS-356: harness-only diffs are exempt from a live agent surface ──────────

test('resolveSurfacePlan: harness-only diff with a TUI-scoped bullet → empty order (exemption beats force)', () => {
  const plan = resolveSurfacePlan({
    catalog: planCat,
    changedFiles: ['scripts/proof.mjs', 'docs/plans/BOS-356-x.md'],
    // A TUI-scoped `## Required proof` bullet would normally FORCE tui (D16), but
    // a harness-only diff has no product surface to demonstrate — exemption wins.
    requiredProofBullets: ['(TUI) settled screen shows the session list'],
  })
  assert.deepEqual(plan.surfaces, { tui: false, web: false })
  assert.deepEqual(plan.order, [])
})

test('resolveSurfacePlan: harness-only diff + committed scenario is NOT exempt (dogfood opt-in)', () => {
  const plan = resolveSurfacePlan({
    catalog: planCat,
    // A committed proof/scenarios/*.scenario.json makes the diff no longer
    // harness-only, so the TUI-scoped bullet forces tui and replay is reachable.
    changedFiles: ['scripts/proof.mjs', 'proof/scenarios/home.scenario.json'],
    requiredProofBullets: ['(TUI) settled screen shows the session list'],
  })
  assert.equal(plan.surfaces.tui, true)
  assert.ok(plan.order.includes('tui'))
})

// ── D5 shared-budget sequencing: TUI consuming the budget defers web ─────────

test('D5: TUI consuming the shared budget defers web with budget-exceeded (exit 0)', () => {
  const totalBudgetMs = 15 * 60 * 1000
  let elapsedMs = 0
  // TUI runs first (cheap-first) and consumes ~13 minutes of the shared budget.
  const tuiBudget = planSurfaceBudget({
    surface: 'tui',
    elapsedMs,
    totalBudgetMs,
    budget: surfaceBudget('tui'),
  })
  assert.equal(tuiBudget.run, true)
  elapsedMs += 13 * 60 * 1000
  // Web can no longer fit its 6-min floor in the ~2 min remaining → deferral.
  const webBudget = planSurfaceBudget({
    surface: 'web',
    elapsedMs,
    totalBudgetMs,
    budget: surfaceBudget('web'),
  })
  assert.deepEqual(webBudget, { run: false, reasonCode: 'budget-exceeded' })
  // The dispatcher records web as a synthetic deferred run; it classifies and
  // contributes a NEUTRAL exit code (partial success is not a failure).
  const perSurface = classifySurfaceOutcomes([
    { surface: 'tui', hasFailure: false, noSurface: false, captureShapes: [{ fileName: 't.mp4' }] },
    {
      surface: 'web',
      hasFailure: false,
      noSurface: false,
      reasonCode: 'budget-exceeded',
      captureShapes: [],
    },
  ])
  assert.equal(perSurface[1].outcome, 'deferred')
  assert.equal(perSurface[1].reasonCode, 'budget-exceeded')
  assert.equal(aggregateExitCode(perSurface), 0)
})

// BOS-203: anchor the dispatcher's driver-selection contract in this suite. The
// runAgentSurfaces dispatcher routes each surface through the agent-driver
// registry; these assertions pin the selection + validation behavior it relies
// on. (runAgentSurfaces itself is unexported and process/env-coupled, so it is
// not invoked directly here.)
const driverStubDeps = {
  tuiAgentUsable: () => true,
  buildTuiAgentBridge: () => ({ bridgeBin: '/tmp/b', bossBin: '/tmp/boss' }),
  tuiAgentBridgeEnv: () => ({
    BOSS_PROOF_TUI_BRIDGE_BIN: '/tmp/b',
    BOSS_PROOF_BOSS_BIN: '/tmp/boss',
  }),
  agentModeAvailable: () => true,
  webUiSurfacePresent: () => true,
  evaluateRunPreflight: () => null,
  runTuiAgentProof: async () => ({ surface: 'tui' }),
  runAgentProof: async () => ({ surface: 'web' }),
}

test('agent-driver registry: dispatcher selects tui/web drivers', () => {
  const drivers = builtinAgentDrivers(driverStubDeps)
  assert.deepEqual(drivers.map((d) => d.surface).sort(), ['tui', 'web'])
  // Unknown surface → no driver → the dispatcher's agent-unavailable path.
  assert.equal(driverForSurface(drivers, 'unknown'), null)
})

test('agent-driver registry: SurfaceRun validation happy + missing-key', () => {
  const full = {
    surface: 'web',
    captureShapes: [],
    brief: {},
    agentResult: { passed: true, summary: '', evidence: [], steps: 0 },
    hasFailure: false,
    noSurface: false,
    scanTexts: [],
    elapsedMs: 0,
    reasonCode: null,
  }
  assert.equal(validateSurfaceRun(full, 'web').ok, true)
  assert.equal(validateSurfaceRun({ surface: 'web' }, 'web').ok, false)
})

// ── BOS-223: agent-first TUI dispatch with scenario-replay fallback ───────────
// The orchestration helper (runTuiWithReplayFallback) runs the agent leg first
// and, on gate-fail / crash / not-attempted, replays each committed scenario.
// These stub the leg runner entirely (no bridge/agent/Anthropic) and assert the
// two-leg dispatch, the surfaceRun proofSource/fallbackReason threading, the
// budget reserve split, and web-dispatch isolation.

/** A spy leg runner: records each call's args; `behaviors(args, i)` returns the
 *  SurfaceRun to resolve with, or `{ __throw: msg }` to simulate a machinery crash. */
function fakeTuiLeg(behaviors) {
  const calls = []
  const fn = async (args) => {
    const b = behaviors(args, calls.length)
    calls.push(args)
    if (b && b.__throw) throw new Error(b.__throw)
    return b
  }
  fn.calls = calls
  return fn
}

/** A minimal collect-mode TUI SurfaceRun the leg runner resolves with. */
function tuiLegRun({ hasFailure = false, error = null, elapsedMs = 1000 } = {}) {
  const captureShapes = [
    hasFailure
      ? { surface: 'tui', status: 'failed', error: error ?? 'gate failed' }
      : { surface: 'tui', status: 'passed', fileName: 'tui/tui.mp4' },
  ]
  return {
    surface: 'tui',
    captureShapes,
    brief: { title: 'brief' },
    agentResult: { passed: !hasFailure, summary: error ?? 'summary', evidence: [], steps: 1 },
    hasFailure,
    noSurface: false,
    scanTexts: ['screen'],
    elapsedMs,
    reasonCode: null,
  }
}

function tuiDispatchCtx(overrides = {}) {
  return {
    prNumber: '1',
    commit: 'abc1234',
    changedFiles: ['proof/scenarios/home.scenario.json'],
    dryRun: true,
    planRequiredProof: [],
    runContext: { collect: true, runId: 'r', token: 't', maxWallClockMs: 600000 },
    ...overrides,
  }
}

/** Replay seams + reserve for the orchestration; the leg runner is stubbed so
 *  runReplayLoop itself never executes — it is only wrapped into `loopRunner`. */
function replayDeps(overrides = {}) {
  return {
    runReplayLoop: () => ({}),
    synthesizeBrief: () => ({ title: 'replay-brief' }),
    makeScenarioEvaluator: () => () => [],
    loadScenario: () => ({ scenario: { title: 'scn' }, scenes: [] }),
    replayReserveMs: 120000,
    repoRoot: '/tmp',
    ...overrides,
  }
}

test('BOS-223 (a): agent pass ⇒ agent captureShape, replay leg NOT run, proofSource agent', async () => {
  const leg = fakeTuiLeg(() => tuiLegRun({ hasFailure: false }))
  const run = await runTuiWithReplayFallback(
    tuiDispatchCtx(),
    replayDeps({ runTuiAgentProof: leg, agentUsable: true }),
  )
  assert.equal(leg.calls.length, 1, 'only the agent leg ran')
  assert.equal(leg.calls[0].brief, undefined, 'agent leg is the default path (no injected brief)')
  assert.equal(leg.calls[0].loopRunner, undefined, 'agent leg has no replay loopRunner')
  assert.equal(run.proofSource, 'agent')
  assert.equal(run.fallbackReason, null)
  assert.equal(run.hasFailure, false)
  assert.equal(run.captureShapes[0].proofSource, 'agent')
})

test('BOS-223 (b): agent gate-fail + scenario ⇒ replay leg runs w/ BOS-219 injections, proofSource replay', async () => {
  const leg = fakeTuiLeg((_args, i) => tuiLegRun({ hasFailure: i === 0 }))
  const run = await runTuiWithReplayFallback(
    tuiDispatchCtx(),
    replayDeps({ runTuiAgentProof: leg, agentUsable: true }),
  )
  assert.equal(leg.calls.length, 2, 'agent leg then replay leg')
  assert.equal(leg.calls[0].brief, undefined, 'first call is the bare agent leg')
  assert.ok(leg.calls[1].brief, 'replay leg is handed a synthesized brief')
  assert.equal(typeof leg.calls[1].loopRunner, 'function', 'replay leg gets loopRunner')
  assert.equal(typeof leg.calls[1].evaluateEvidence, 'function', 'replay leg gets evaluateEvidence')
  assert.equal(run.proofSource, 'replay')
  assert.equal(run.fallbackReason, 'agent-failed')
  assert.equal(run.hasFailure, false)
  assert.equal(run.captureShapes[0].proofSource, 'replay')
})

test('BOS-223 (c): keyless (agent unusable) + scenario ⇒ agent leg NEVER invoked, replay proof', async () => {
  const leg = fakeTuiLeg(() => tuiLegRun({ hasFailure: false }))
  const drivers = builtinAgentDrivers({
    ...driverStubDeps,
    tuiAgentUsable: () => false,
    runTuiAgentProof: leg,
    runTuiWithReplayFallback,
    runReplayLoop: () => ({}),
    synthesizeBrief: () => ({ title: 'replay-brief' }),
    makeScenarioEvaluator: () => () => [],
    loadScenario: () => ({ scenario: { title: 'scn' }, scenes: [] }),
    tuiReplayReserveMs: 120000,
  })
  const tui = driverForSurface(drivers, 'tui')
  const run = await tui.run(tuiDispatchCtx())
  assert.ok(leg.calls.length >= 1, 'the replay leg ran')
  assert.ok(
    leg.calls.every((c) => c.brief && typeof c.loopRunner === 'function'),
    'every leg call is a replay call — the agent leg is never invoked when keyless',
  )
  assert.equal(run.proofSource, 'replay')
  assert.equal(run.fallbackReason, 'agent-unavailable')
  assert.equal(run.hasFailure, false)
})

test('BOS-223 (e2): double gate-fail ⇒ merged captureShapes never reference the agent artifacts the replay leg deleted', async () => {
  // Regression: the replay leg wipes `localDir/<CAPTURE_ID>` before it runs (the
  // two legs share one fixed-name capture dir). On a DOUBLE gate-fail the
  // agent-incomplete branch used to concatenate the agent leg's captureShapes with
  // the replay's — but the agent leg's PNGs are gone by then, so every frame the
  // (shorter) replay did not re-create became a dangling manifest reference and
  // uploadBundle died with an opaque `wrangler ... exited 1` (ENOENT), turning an
  // honest, reviewable agent-incomplete into a pipeline-error.
  const localDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-two-leg-'))
  const captureDir = path.join(localDir, 'tui-agent')
  // Each leg writes `frames` stills + the fixed-name video, then gate-fails.
  const runLegWritingFrames = (frames) => {
    fs.mkdirSync(captureDir, { recursive: true })
    fs.writeFileSync(path.join(captureDir, 'tui.mp4'), 'mp4')
    const stills = []
    for (let i = 1; i <= frames; i++) {
      const rel = `tui-agent/scene-01-frame-0${i}.png`
      fs.writeFileSync(path.join(localDir, rel), 'png')
      stills.push({ fileName: rel, label: `scene 1 frame 0${i}` })
    }
    return {
      ...tuiLegRun({ hasFailure: true }),
      captureShapes: [
        {
          surface: 'tui',
          status: 'failed',
          error: 'gate failed',
          fileName: 'tui-agent/tui.mp4',
          stills,
        },
      ],
    }
  }
  try {
    // Agent leg captures 3 frames; the shorter replay leg re-creates only 1.
    const leg = fakeTuiLeg((_args, i) => runLegWritingFrames(i === 0 ? 3 : 1))
    const run = await runTuiWithReplayFallback(
      tuiDispatchCtx({
        runContext: { collect: true, runId: 'r', token: 't', maxWallClockMs: 600000, localDir },
      }),
      replayDeps({ runTuiAgentProof: leg, agentUsable: true }),
    )
    assert.equal(leg.calls.length, 2, 'both legs ran')
    assert.equal(run.hasFailure, true, 'double gate-fail stays a fatal agent-incomplete')
    const referenced = run.captureShapes.flatMap((c) =>
      [c.fileName, c.posterFileName, ...(c.stills ?? []).map((s) => s.fileName)].filter(Boolean),
    )
    const missing = referenced.filter((rel) => !fs.existsSync(path.join(localDir, rel)))
    assert.deepEqual(
      missing,
      [],
      `captureShapes reference artifacts absent from disk: ${missing.join(', ')}`,
    )
  } finally {
    fs.rmSync(localDir, { recursive: true, force: true })
  }
})

test('uploadBundle: a manifest naming absent media throws by NAME before any wrangler call', () => {
  // Defense in depth for the regression above: wrangler surfaces a missing --file
  // as a bare `exited 1` (runCommand inherits stdio, so the ENOENT never reaches
  // the thrown Error), which made dangling-reference bugs unreadable in CI logs.
  // Still fail-loud — just diagnosable, and without a half-uploaded bundle.
  const localDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-upload-'))
  try {
    fs.writeFileSync(path.join(localDir, 'manifest.json'), '{}')
    fs.mkdirSync(path.join(localDir, 'tui-agent'), { recursive: true })
    fs.writeFileSync(path.join(localDir, 'tui-agent', 'scene-01-frame-01.png'), 'png')
    const manifest = {
      captures: [
        {
          stills: [
            { fileName: 'tui-agent/scene-01-frame-01.png' },
            { fileName: 'tui-agent/scene-01-frame-03.png' },
          ],
        },
      ],
    }
    assert.throws(
      () => uploadBundle({ localDir, publicPrefix: 'p', manifest, bucket: 'b' }),
      (err) => {
        assert.match(err.message, /missing from/)
        assert.match(err.message, /tui-agent\/scene-01-frame-03\.png/)
        assert.doesNotMatch(
          err.message,
          /scene-01-frame-01\.png/,
          'only the absent artifact is named',
        )
        return true
      },
      'a dangling manifest reference throws before shelling out to wrangler',
    )
  } finally {
    fs.rmSync(localDir, { recursive: true, force: true })
  }
})

test('BOS-223 (d): keyless + scenario-less is resolved by the BOS-220 scenario-missing gate upstream', () => {
  // A KEYLESS scenario-less TUI diff never reaches the usable gate: the dispatcher
  // defers it as `scenario-missing` first (BOS-220, reordered by BOS-350 to fire only
  // when keyless). The pure {F,F} planTuiLegs row below therefore stays a
  // self-contained decision the live path never exercises.
  assert.equal(committedScenarioPresent(['services/boss/internal/views/home.go']), false)
  assert.deepEqual(planTuiLegs({ agentUsable: false, scenarioPresent: false }), {
    runAgent: false,
    runReplay: false,
    deferReason: 'agent-unavailable',
  })
})

// ── BOS-350: TUI scenario-missing gate defers unless the agent can ACTUALLY capture ──
// The dispatcher's TUI `scenario-missing` gate is
//   `surface === 'tui' && !committedScenarioPresent(changedFiles) && !tuiAgentCanCapture()`.
// `tuiAgentCanCapture()` is the load-bearing gate predicate (exported so these pin it
// directly rather than reconstructing the boolean): it is `tuiAgentUsable()` AND a real
// PROOF_ANTHROPIC_API_KEY, because the TUI agent leg hard-requires that key. These pin the
// three env seams the gate composes (matching the BOS-223 (d) proxy style: runAgentSurfaces
// is unexported/env-coupled), plus a driver-level run that exercises the agent-only leg the
// key-present path now falls through to.
test('BOS-350: key-present scenario-less TUI is NOT deferred scenario-missing (falls through to agent-only leg)', () => {
  const changedFiles = ['services/boss/internal/views/home.go']
  assert.equal(classifyTuiSurface(changedFiles), true, 'diff classifies as the TUI surface')
  assert.equal(committedScenarioPresent(changedFiles), false, 'no committed scenario')
  const prevKey = process.env.PROOF_ANTHROPIC_API_KEY
  const prevMode = process.env.BOSS_PROOF_MODE
  try {
    delete process.env.BOSS_PROOF_MODE
    process.env.PROOF_ANTHROPIC_API_KEY = 'sk-test'
    assert.equal(tuiAgentCanCapture(), true, 'real key present ⇒ TUI agent can capture')
    // The gate defers only when the scenario is absent AND the agent cannot capture.
    const gateDefers = !committedScenarioPresent(changedFiles) && !tuiAgentCanCapture()
    assert.equal(gateDefers, false, 'key present ⇒ gate does NOT defer; control falls through')
    assert.deepEqual(
      planTuiLegs({ agentUsable: true, scenarioPresent: false }),
      { runAgent: true, runReplay: false },
      'the fall-through path plans the agent-only leg',
    )
  } finally {
    if (prevKey === undefined) delete process.env.PROOF_ANTHROPIC_API_KEY
    else process.env.PROOF_ANTHROPIC_API_KEY = prevKey
    if (prevMode === undefined) delete process.env.BOSS_PROOF_MODE
    else process.env.BOSS_PROOF_MODE = prevMode
  }
})

test('BOS-350: keyless scenario-less TUI still defers scenario-missing (gate holds)', () => {
  const changedFiles = ['services/boss/internal/views/home.go']
  const prevKey = process.env.PROOF_ANTHROPIC_API_KEY
  const prevMode = process.env.BOSS_PROOF_MODE
  try {
    delete process.env.PROOF_ANTHROPIC_API_KEY
    delete process.env.BOSS_PROOF_MODE
    assert.equal(tuiAgentCanCapture(), false, 'keyless ⇒ TUI agent cannot capture')
    const gateDefers = !committedScenarioPresent(changedFiles) && !tuiAgentCanCapture()
    assert.equal(gateDefers, true, 'keyless + scenario-less ⇒ still deferred scenario-missing')
  } finally {
    if (prevKey === undefined) delete process.env.PROOF_ANTHROPIC_API_KEY
    else process.env.PROOF_ANTHROPIC_API_KEY = prevKey
    if (prevMode === undefined) delete process.env.BOSS_PROOF_MODE
    else process.env.BOSS_PROOF_MODE = prevMode
  }
})

// Regression pin: BOSS_PROOF_MODE=agent WITHOUT a real key is not a capturable agent.
// tuiAgentUsable() trusts the mode assertion (returns true — correct for driver.usable,
// where the key is daemon-injected in cron), but the TUI agent leg builds its Anthropic
// client from PROOF_ANTHROPIC_API_KEY explicitly, so with no key it would throw and — with
// no committed scenario to replay — surface as a `pipeline-error` (exit 1). The gate uses
// tuiAgentCanCapture() so this doomed config keeps deferring the warn-only `scenario-missing`
// nudge (exit 0), preserving the change's exit-code-neutral invariant.
test('BOS-350: BOSS_PROOF_MODE=agent WITHOUT a key still defers scenario-missing (no exit-1 crash)', () => {
  const changedFiles = ['services/boss/internal/views/home.go']
  const prevKey = process.env.PROOF_ANTHROPIC_API_KEY
  const prevMode = process.env.BOSS_PROOF_MODE
  try {
    delete process.env.PROOF_ANTHROPIC_API_KEY
    process.env.BOSS_PROOF_MODE = 'agent'
    assert.equal(tuiAgentUsable(), true, 'mode=agent ⇒ usable (trusts the mode assertion)')
    assert.equal(
      tuiAgentCanCapture(),
      false,
      'mode=agent but no real key ⇒ the TUI agent cannot actually capture',
    )
    const gateDefers = !committedScenarioPresent(changedFiles) && !tuiAgentCanCapture()
    assert.equal(
      gateDefers,
      true,
      'mode=agent without a key + scenario-less ⇒ defers scenario-missing, not a doomed agent leg',
    )
  } finally {
    if (prevKey === undefined) delete process.env.PROOF_ANTHROPIC_API_KEY
    else process.env.PROOF_ANTHROPIC_API_KEY = prevKey
    if (prevMode === undefined) delete process.env.BOSS_PROOF_MODE
    else process.env.BOSS_PROOF_MODE = prevMode
  }
})

test('BOS-350: key-present scenario-less driver run ⇒ agent leg only, proofSource agent, scenarioOwed', async () => {
  const leg = fakeTuiLeg(() => tuiLegRun({ hasFailure: false }))
  const drivers = builtinAgentDrivers({
    ...driverStubDeps,
    tuiAgentUsable: () => true,
    runTuiAgentProof: leg,
    runTuiWithReplayFallback,
    runReplayLoop: () => ({}),
    synthesizeBrief: () => ({ title: 'replay-brief' }),
    makeScenarioEvaluator: () => () => [],
    loadScenario: () => ({ scenario: { title: 'scn' }, scenes: [] }),
    tuiReplayReserveMs: 120000,
  })
  const tui = driverForSurface(drivers, 'tui')
  // A scenario-less TUI diff: no proof/scenarios/*.scenario.json in changedFiles.
  const run = await tui.run(
    tuiDispatchCtx({ changedFiles: ['services/boss/internal/views/home.go'] }),
  )
  assert.equal(leg.calls.length, 1, 'only the agent leg ran (no replay — there is no scenario)')
  assert.equal(leg.calls[0].brief, undefined, 'the single call is the bare agent leg')
  assert.equal(run.proofSource, 'agent', 'proof came from the agent, not replay')
  assert.equal(run.fallbackReason, null)
  assert.equal(run.scenarioOwed, true, 'a committed scenario is still owed (author nudge)')
})

test('BOS-223 (e): both legs gate-fail ⇒ agent-incomplete with combined two-leg errorDetail', async () => {
  const leg = fakeTuiLeg((_args, i) =>
    tuiLegRun({ hasFailure: true, error: i === 0 ? 'agentERR' : 'replayERR' }),
  )
  const run = await runTuiWithReplayFallback(
    tuiDispatchCtx(),
    replayDeps({ runTuiAgentProof: leg, agentUsable: true }),
  )
  assert.equal(run.proofSource, null)
  assert.equal(run.hasFailure, true)
  assert.equal(run.reasonCode, null, 'reasonCode stays null so finalize derives agent-incomplete')
  const perSurface = classifySurfaceOutcomes([run])
  assert.equal(perSurface[0].reasonCode, 'agent-incomplete')
  assert.equal(perSurface[0].error, 'agent: agentERR; replay: replayERR')
  assert.equal(
    aggregateExitCode(perSurface),
    1,
    'TUI agent-incomplete is fail-loud exit 1 (BOS-226)',
  )
})

test('BOS-223 (f): a leg crash with no good other leg ⇒ throws (dispatcher crash-catch → pipeline-error)', async () => {
  const leg = fakeTuiLeg((_args, i) =>
    i === 0 ? { __throw: 'boom' } : tuiLegRun({ hasFailure: true }),
  )
  await assert.rejects(
    () =>
      runTuiWithReplayFallback(
        tuiDispatchCtx(),
        replayDeps({ runTuiAgentProof: leg, agentUsable: true }),
      ),
    /boom/,
    'pipeline-error is surfaced by rethrow so proof-finalize-outcome stays untouched',
  )
})

test('BOS-223 (g): budget reserve split — agent leg = slice − reserve, replay leg = reserve', async () => {
  const seen = []
  const leg = fakeTuiLeg((args, i) => {
    seen.push(args.runContext.maxWallClockMs)
    return tuiLegRun({ hasFailure: i === 0 })
  })
  await runTuiWithReplayFallback(
    tuiDispatchCtx({ runContext: { collect: true, maxWallClockMs: 600000 } }),
    replayDeps({ runTuiAgentProof: leg, agentUsable: true, replayReserveMs: 120000 }),
  )
  assert.equal(seen[0], 480000, 'agent leg capped at slice − reserve')
  assert.equal(seen[1], 120000, 'replay leg gets its reserve')

  // 120s-floor edge: a slice equal to the reserve still leaves the agent leg ≥ 60s.
  const seen2 = []
  const leg2 = fakeTuiLeg((args, i) => {
    seen2.push(args.runContext.maxWallClockMs)
    return tuiLegRun({ hasFailure: i === 0 })
  })
  await runTuiWithReplayFallback(
    tuiDispatchCtx({ runContext: { collect: true, maxWallClockMs: 120000 } }),
    replayDeps({ runTuiAgentProof: leg2, agentUsable: true, replayReserveMs: 120000 }),
  )
  assert.equal(seen2[0], 60000, 'agent leg floored at 60s')
  assert.equal(seen2[1], 120000, 'replay leg still gets its reserve')
})

test('BOS-223 (g2): multiple committed scenarios ⇒ replay only the FIRST (single-bundle safety)', async () => {
  // The replay leg captures into one per-surface bundle dir (constant CAPTURE_ID),
  // so replaying several scenarios would clobber all but the last on disk while
  // emitting N captureShapes resolving to that one file. Until per-scenario capture
  // namespacing lands, only the first committed scenario replays; the second is not
  // invoked (a note is logged). The agent leg gets slice − reserve, the single
  // replay leg the full reserve.
  const twoScenarioCtx = tuiDispatchCtx({
    changedFiles: ['proof/scenarios/home.scenario.json', 'proof/scenarios/detail.scenario.json'],
    runContext: { collect: true, maxWallClockMs: 600000 },
  })
  const seen = []
  const briefs = []
  // call 0 = agent (gate-fail), call 1 = the FIRST replay scenario (passes).
  const leg = fakeTuiLeg((args, i) => {
    seen.push(args.runContext.maxWallClockMs)
    briefs.push(args.runContext.briefFileName)
    return tuiLegRun({ hasFailure: i === 0 })
  })
  const run = await runTuiWithReplayFallback(
    twoScenarioCtx,
    replayDeps({ runTuiAgentProof: leg, agentUsable: true, replayReserveMs: 120000 }),
  )
  assert.equal(leg.calls.length, 2, 'agent leg + exactly ONE replay leg (second scenario skipped)')
  assert.equal(seen[0], 480000, 'agent leg capped at slice − reserve')
  assert.equal(seen[1], 120000, 'the single replay leg gets the full reserve')
  assert.equal(
    briefs[1],
    'brief-replay-home.scenario.json.json',
    'the replayed scenario is the FIRST changed one',
  )
  assert.equal(run.proofSource, 'replay')
  assert.equal(run.hasFailure, false)
})

test('BOS-223 (g3): the replay leg clears stale agent raw/ + capture artifacts before running', async () => {
  // The replay leg reuses the shared collect-mode localDir, so without isolation it
  // would inherit the failed agent leg's `raw/screen-*.txt` frames (renderFrames scans
  // ALL of them) and its fixed-name capture media. Pre-seed both, then assert they are
  // gone by the time the replay leg is invoked.
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'tui-replay-isolate-'))
  fs.mkdirSync(path.join(tmp, 'raw'), { recursive: true })
  fs.writeFileSync(path.join(tmp, 'raw', 'screen-000.txt'), 'stale agent frame')
  fs.mkdirSync(path.join(tmp, 'tui-agent'), { recursive: true })
  fs.writeFileSync(path.join(tmp, 'tui-agent', 'tui-agent.mp4'), 'stale agent media')

  let staleAtReplay = null
  const leg = fakeTuiLeg((_args, i) => {
    if (i === 1) {
      staleAtReplay = {
        rawFrame: fs.existsSync(path.join(tmp, 'raw', 'screen-000.txt')),
        media: fs.existsSync(path.join(tmp, 'tui-agent', 'tui-agent.mp4')),
      }
    }
    return tuiLegRun({ hasFailure: i === 0 })
  })
  try {
    await runTuiWithReplayFallback(
      tuiDispatchCtx({ runContext: { collect: true, localDir: tmp, maxWallClockMs: 600000 } }),
      replayDeps({ runTuiAgentProof: leg, agentUsable: true }),
    )
    assert.equal(leg.calls.length, 2, 'agent leg then replay leg')
    assert.deepEqual(
      staleAtReplay,
      { rawFrame: false, media: false },
      'stale agent raw frames AND capture media are cleared before the replay leg runs',
    )
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true })
  }
})

test('BOS-223 budget: a committed-scenario TUI run extends the shared pool by TUI_REPLAY_EXTRA_MS', () => {
  // Mirrors the runAgentSurfaces pool-extend line (the LIVE_AGENT_EXTRA_MS precedent).
  const withScenario = ['services/boss/x.go', 'proof/scenarios/home.scenario.json']
  const reserve = Number(process.env.TUI_REPLAY_EXTRA_MS) || TUI_REPLAY_EXTRA_MS
  const total =
    DEFAULT_TOTAL_PROOF_BUDGET_MS + (committedScenarioPresent(withScenario) ? reserve : 0)
  assert.equal(total, DEFAULT_TOTAL_PROOF_BUDGET_MS + TUI_REPLAY_EXTRA_MS)
  // A scenario-less TUI diff never triggers the extension.
  assert.equal(committedScenarioPresent(['services/boss/x.go']), false)
})

test('BOS-223 (h): web driver dispatch is byte-identical — no TUI disclosure fields leak', async () => {
  const webRun = {
    surface: 'web',
    captureShapes: [],
    brief: {},
    agentResult: { passed: true, summary: '', evidence: [], steps: 0 },
    hasFailure: false,
    noSurface: false,
    scanTexts: [],
    elapsedMs: 5,
    reasonCode: null,
  }
  const spy = async (ctx) => ({ ...webRun, echoed: ctx.prNumber })
  const drivers = builtinAgentDrivers({ ...driverStubDeps, runAgentProof: spy })
  const web = driverForSurface(drivers, 'web')
  const out = await web.run({
    prNumber: 'PR9',
    commit: 'c',
    changedFiles: [],
    dryRun: true,
    planRequiredProof: [],
    runContext: {},
  })
  assert.deepEqual(out, { ...webRun, echoed: 'PR9' })
  assert.equal(out.proofSource, undefined, 'the web path carries no TUI proofSource/fallbackReason')
})

// ── BOS-219: `scenario` dispatch (validate/run) via runScenarioCommand ────────
// Drives the REAL CLI dispatch path (not runReplayLoop in isolation) with a fake
// bridge, so no Go binary, agg, ffmpeg, or Anthropic SDK is ever touched.

const SCENARIO_FIXTURE_DIR = path.join(repoRootForTest, 'scripts/testdata/scenario-fixtures')

const DEMO_SCENARIO = {
  version: 1,
  title: 'Replay demo',
  fixture: { preset: 'demo' },
  scenes: [
    {
      id: 'scene-01',
      title: 'Home',
      steps: [{ key: 'down', caption: 'Move selection' }, { expect: ['REPO', 'STATUS'] }],
    },
  ],
}

/** Fake replay bridge exposing the real op set the replay loop drives. */
function scenarioReplayBridge(screen) {
  const ops = []
  const bridge = {
    ops,
    quitCalled: false,
    async observe() {
      ops.push('observe')
      return { screen }
    },
    async key(k) {
      ops.push(['key', k])
      return { screen }
    },
    async enter() {
      ops.push('enter')
      return { screen }
    },
    async esc() {
      ops.push('esc')
      return { screen }
    },
    async type(t) {
      ops.push(['type', t])
      return { screen }
    },
    async daemon(d) {
      ops.push(['daemon', d])
      return { screen }
    },
    async quit() {
      bridge.quitCalled = true
    },
  }
  return bridge
}

function scenarioRenderStill() {
  return async ({ output }) => {
    fs.writeFileSync(output, 'fake-png')
  }
}

function withScenarioFile(obj, fn) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'scenario-cli-'))
  const p = path.join(dir, 'demo.scenario.json')
  fs.writeFileSync(p, JSON.stringify(obj))
  return Promise.resolve()
    .then(() => fn(p))
    .finally(() => fs.rmSync(dir, { recursive: true, force: true }))
}

/** Save/restore the proof env keys a scenario run touches. */
function withScenarioEnv(overrides, fn) {
  const KEYS = [
    'BOSS_PROOF_UPLOAD',
    'BOSS_PROOF_RUN_ID',
    'BOSS_PROOF_RUN_TOKEN',
    'BOSS_PROOF_PUBLIC_BASE_URL',
    'PROOF_ANTHROPIC_API_KEY',
    'BOSS_PROOF_TUI_BRIDGE_BIN',
    'BOSS_PROOF_BOSS_BIN',
  ]
  const saved = {}
  for (const k of KEYS) saved[k] = process.env[k]
  for (const [k, v] of Object.entries(overrides)) {
    if (v === undefined) delete process.env[k]
    else process.env[k] = v
  }
  return Promise.resolve()
    .then(fn)
    .finally(() => {
      for (const k of KEYS) {
        if (saved[k] === undefined) delete process.env[k]
        else process.env[k] = saved[k]
      }
    })
}

test('runScenarioCommand validate: a valid scenario prints OK and never sets exit 1', async () => {
  const { runScenarioCommand } = await import('./proof.mjs')
  const logs = []
  let exit = null
  await runScenarioCommand(
    { sub: 'validate', file: path.join(SCENARIO_FIXTURE_DIR, 'valid-minimal.json') },
    { log: (m) => logs.push(m), errorLog: () => {}, setExitCode: (n) => (exit = n) },
  )
  assert.match(logs.join('\n'), /scenario valid/)
  assert.equal(exit, null)
})

test('runScenarioCommand validate: an invalid scenario prints pointerful errors and sets exit 1', async () => {
  const { runScenarioCommand } = await import('./proof.mjs')
  const errors = []
  let exit = null
  await runScenarioCommand(
    { sub: 'validate', file: path.join(SCENARIO_FIXTURE_DIR, 'invalid-no-title.json') },
    { log: () => {}, errorLog: (m) => errors.push(m), setExitCode: (n) => (exit = n) },
  )
  assert.equal(exit, 1)
  assert.match(errors.join('\n'), /title/)
})

test('runScenarioCommand run --dry-run: replays via the three seams with zero SDK calls', async () => {
  const { runScenarioCommand } = await import('./proof.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined
  let genCalls = 0
  try {
    await withScenarioEnv(
      {
        BOSS_PROOF_UPLOAD: '0',
        BOSS_PROOF_RUN_ID: 'scenario-dispatch',
        BOSS_PROOF_RUN_TOKEN: 'tok-scenario',
        BOSS_PROOF_PUBLIC_BASE_URL: 'https://proof.test.dev',
        PROOF_ANTHROPIC_API_KEY: undefined,
      },
      async () => {
        await withScenarioFile(DEMO_SCENARIO, async (file) => {
          const bridge = scenarioReplayBridge('REPO list\nSTATUS: ok')
          const result = await runScenarioCommand(
            { sub: 'run', file, dryRun: true, pr: null },
            {
              prNumber: 'scenariorun',
              commit: 'abc1234',
              bridge,
              proofDeps: {
                renderStill: scenarioRenderStill(),
                castToVideo: async () => null,
                generateBriefFromDiff: async () => {
                  genCalls += 1
                  return { title: 'never', description: 'used' }
                },
              },
              log: () => {},
            },
          )
          assert.ok(result?.manifest, 'run returns a manifest')
          // Brief seam (D4): resolveBrief → generateBriefFromDiff is never reached.
          assert.equal(genCalls, 0, 'no brief generation → no Anthropic SDK path')
          // Hermeticity tripwire: judge short-circuits on the deleted key.
          assert.deepEqual(result.manifest.judge, { unjudged: true, reason: 'missing-key' })
          const cap = result.manifest.captures[0]
          assert.equal(cap.surface, 'tui')
          assert.equal(cap.status, 'passed')
          assert.equal(cap.scenes[0].passed, true, 'evaluator gate result flows to finalize')
          assert.deepEqual(cap.scenes[0].missing, [])
          assert.ok(bridge.quitCalled, 'bridge.quit() is always sent')
        })
      },
    )
  } finally {
    process.exitCode = originalExitCode
    fs.rmSync(path.join(repoRootForTest, '.proof', 'pr-scenariorun'), {
      recursive: true,
      force: true,
    })
  }
})

test("runScenarioCommand run: threads the scenario's terminal into the TUI leg deps (BOS-571)", async () => {
  const { runScenarioCommand } = await import('./proof.mjs')
  /** Spy standing in for runTuiAgentProof; records the deps it was handed. */
  const capture = []
  const runTuiAgentProof = async ({ deps }) => {
    capture.push(deps)
    return {}
  }
  const overrides = {
    prNumber: 'scenarioterm',
    commit: 'abc1234',
    runTuiAgentProof,
    runReplayLoop: async () => ({}),
    synthesizeBrief: () => ({ title: 't', description: 'd' }),
    makeScenarioEvaluator: () => () => ({ passed: true }),
    bridge: scenarioReplayBridge('REPO'),
    log: () => {},
  }

  // A scenario declaring `terminal` reaches the bridge factory's dep bag.
  await withScenarioFile({ ...DEMO_SCENARIO, terminal: { cols: 72, rows: 30 } }, (file) =>
    runScenarioCommand({ sub: 'run', file, dryRun: true, pr: null }, overrides),
  )
  assert.deepEqual(capture.at(-1).terminal, { cols: 72, rows: 30 })

  // Without one, no terminal is invented (default argv stays byte-identical).
  await withScenarioFile(DEMO_SCENARIO, (file) =>
    runScenarioCommand({ sub: 'run', file, dryRun: true, pr: null }, overrides),
  )
  assert.equal('terminal' in capture.at(-1), false, 'absent terminal stays absent')
})

// ── BOS-219 Task 4: full-run integration + end-to-end no-API-key guarantee ─────
// Drives the WHOLE `scenario run --dry-run` CLI/dispatch path (not runReplayLoop
// in isolation) with a fake bridge, asserting the captureShape handed to finalize
// carries the keys finalizeAgentProof accepts today, that PROOF_ANTHROPIC_API_KEY
// is deleted for the whole test with zero Anthropic SDK calls, and that local
// artifacts are produced.

const TWO_SCENE_SCENARIO = {
  version: 1,
  title: 'Two scene replay',
  fixture: { preset: 'demo' },
  scenes: [
    {
      id: 'repos',
      title: 'Repos',
      steps: [{ key: 'r', caption: 'Open repos' }, { expect: ['REPO'] }],
    },
    {
      id: 'status',
      title: 'Status',
      steps: [{ key: 's', caption: 'Open status' }, { expect: ['STATUS'] }],
    },
  ],
}

test('scenario run --dry-run: full pipeline feeds finalize the right captureShape with no API key', async () => {
  const { runScenarioCommand } = await import('./proof.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined
  let genCalls = 0
  try {
    await withScenarioEnv(
      {
        BOSS_PROOF_UPLOAD: '0',
        BOSS_PROOF_RUN_ID: 'scenario-integ',
        BOSS_PROOF_RUN_TOKEN: 'tok-scenario-integ',
        BOSS_PROOF_PUBLIC_BASE_URL: 'https://proof.test.dev',
        // Deleted for the WHOLE test: any SDK use (brief gen or judge) would trip.
        PROOF_ANTHROPIC_API_KEY: undefined,
      },
      async () => {
        await withScenarioFile(TWO_SCENE_SCENARIO, async (file) => {
          const bridge = scenarioReplayBridge('REPO list — STATUS: ok')
          const result = await runScenarioCommand(
            { sub: 'run', file, dryRun: true, pr: null },
            {
              prNumber: 'scenariointeg',
              commit: 'abc1234',
              bridge,
              proofDeps: {
                renderStill: scenarioRenderStill(),
                castToVideo: async () => null,
                // Spy proving the brief seam keeps the SDK path unreachable.
                generateBriefFromDiff: async () => {
                  genCalls += 1
                  return { title: 'never', description: 'used' }
                },
              },
              log: () => {},
            },
          )

          // (b) zero SDK calls: brief gen never reached + judge short-circuited.
          assert.equal(genCalls, 0, 'brief seam keeps the Anthropic SDK path unreachable')
          assert.deepEqual(result.manifest.judge, { unjudged: true, reason: 'missing-key' })
          assert.equal(process.env.PROOF_ANTHROPIC_API_KEY, undefined, 'key stays unset all test')

          // (a) captureShape keys finalizeAgentProof accepts today.
          assert.equal(result.manifest.verdict, 'passed')
          const cap = result.manifest.captures[0]
          assert.equal(cap.surface, 'tui')
          assert.equal(cap.status, 'passed')
          assert.ok('mediaType' in cap, 'capture carries a mediaType')
          assert.equal(cap.scenes.length, 2, 'both scenes flow through to finalize')
          for (const scene of cap.scenes) {
            assert.deepEqual(
              Object.keys(scene).sort(),
              ['id', 'missing', 'outputMs', 'passed', 'title'],
              'each scene carries id/title/passed/missing/outputMs',
            )
            assert.equal(scene.passed, true)
          }
          // (c) local artifacts produced: fallback stills referencing the tui-agent dir.
          assert.ok(Array.isArray(cap.stills) && cap.stills.length >= 1, 'stills produced')
          assert.match(cap.stills[0].fileName, /tui-agent\//)
          assert.ok(bridge.quitCalled)
        })
      },
    )
  } finally {
    process.exitCode = originalExitCode
    fs.rmSync(path.join(repoRootForTest, '.proof', 'pr-scenariointeg'), {
      recursive: true,
      force: true,
    })
  }
})
