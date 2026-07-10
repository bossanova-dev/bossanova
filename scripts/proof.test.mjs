import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

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
  tuiAgentUsable,
} from './proof.mjs'

const repoRootForTest = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
import { planSurfaceBudget, selectRecipes } from './proof-lib.mjs'
import { surfaceBudget } from './proof-surfaces.mjs'
import { aggregateExitCode, classifySurfaceOutcomes } from './proof-finalize-outcome.mjs'
import {
  builtinAgentDrivers,
  driverForSurface,
  validateSurfaceRun,
} from './proof-agent-drivers.mjs'

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
