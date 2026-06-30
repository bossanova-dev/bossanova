import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'

import {
  agentSurface,
  isDocsOnlyChange,
  prefixStillFileNames,
  shouldPostDocsBuildCheck,
  shouldCleanupRunDir,
  tuiAgentBridgeEnv,
  tuiAgentUsable,
} from './proof.mjs'
import { selectRecipes } from './proof-lib.mjs'

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
