#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  SURFACE_DESCRIPTORS,
  surfaceDescriptor,
  surfaceRenderServiceDir,
  surfaceBudget,
  isAgentSurface,
  classifyTuiSurface,
  webUiSurfacePresent,
  committedScenarioPresent,
  classifySurfaces,
  TUI_SURFACE_PREFIXES,
  WEB_UI_SURFACE_PREFIXES,
  BUILTIN_SURFACES,
  resolveSurfaceRegistry,
  captureSurfaceDescriptor,
  surfaceServiceDir,
  browserServiceDir,
  browserSurfaceNames,
} from './proof-surfaces.mjs'

const catalog = {
  version: 1,
  recipes: [
    { id: 'tui-home', surface: 'tui', title: 'TUI Home', privacy: 'fixture' },
    { id: 'web-sessions', surface: 'web', title: 'Web Sessions', privacy: 'fixture' },
    { id: 'marketing-home', surface: 'marketing', title: 'Marketing Home', privacy: 'fixture' },
  ],
  pathRules: [
    { name: 'TUI', patterns: ['services/boss/internal/views/'], recipeIds: ['tui-home'] },
    { name: 'Web', patterns: ['services/web/'], recipeIds: ['web-sessions'] },
    { name: 'Marketing', patterns: ['services/marketing/'], recipeIds: ['marketing-home'] },
  ],
}

test('surfaceRenderServiceDir maps marketing/docs, everything else to services/web', () => {
  assert.equal(surfaceRenderServiceDir('marketing'), 'services/marketing')
  assert.equal(surfaceRenderServiceDir('docs'), 'services/docs')
  assert.equal(surfaceRenderServiceDir('web'), 'services/web')
  assert.equal(surfaceRenderServiceDir('tui'), 'services/web')
  assert.equal(surfaceRenderServiceDir('anything-else'), 'services/web')
})

test('surfaceBudget returns tui/web specs and null for recipe surfaces', () => {
  assert.deepEqual(surfaceBudget('tui'), { defaultMs: 6 * 60 * 1000, floorMs: 3 * 60 * 1000 })
  assert.deepEqual(surfaceBudget('web'), { defaultMs: 12 * 60 * 1000, floorMs: 6 * 60 * 1000 })
  assert.equal(surfaceBudget('marketing'), null)
  assert.equal(surfaceBudget('unknown'), null)
})

test('isAgentSurface is true only for tui/web', () => {
  assert.equal(isAgentSurface('tui'), true)
  assert.equal(isAgentSurface('web'), true)
  assert.equal(isAgentSurface('marketing'), false)
  assert.equal(isAgentSurface('docs'), false)
})

test('TUI/WEB prefix lists are the canonical surface path prefixes', () => {
  assert.ok(TUI_SURFACE_PREFIXES.includes('services/boss/internal/views/'))
  assert.ok(WEB_UI_SURFACE_PREFIXES.includes('services/web/src/'))
  assert.equal(surfaceDescriptor('tui').pathPrefixes, TUI_SURFACE_PREFIXES)
})

test('SURFACE_DESCRIPTORS covers exactly tui, web, marketing, docs', () => {
  assert.deepEqual(SURFACE_DESCRIPTORS.map((d) => d.name).sort(), [
    'docs',
    'marketing',
    'tui',
    'web',
  ])
})

// ── classifyTuiSurface: catalog-independent TUI path detection (BOS-115) ──────

test('classifyTuiSurface is true for any changed file under a TUI prefix', () => {
  for (const prefix of TUI_SURFACE_PREFIXES) {
    assert.equal(
      classifyTuiSurface([`${prefix}some/file.go`]),
      true,
      `expected ${prefix}* to classify as TUI`,
    )
  }
})

test('classifyTuiSurface covers the documented TUI prefixes', () => {
  assert.deepEqual(TUI_SURFACE_PREFIXES, [
    'services/boss/internal/views/',
    'services/boss/internal/tuitest/',
    'services/boss/internal/tuidriver/',
    'services/boss/internal/fixtures/',
    'services/boss/internal/client/',
    'services/boss/cmd/',
    'proto/',
  ])
})

test('classifyTuiSurface is false for web/marketing/docs and unrelated paths', () => {
  assert.equal(classifyTuiSurface(['services/web/src/App.tsx']), false)
  assert.equal(classifyTuiSurface(['services/marketing/src/pages/index.astro']), false)
  assert.equal(classifyTuiSurface(['services/docs/docs/guides/mcp.md']), false)
  assert.equal(classifyTuiSurface(['services/bossd/internal/server/server.go']), false)
  assert.equal(classifyTuiSurface(['README.md']), false)
  assert.equal(classifyTuiSurface([]), false)
  assert.equal(classifyTuiSurface(null), false)
})

test('classifyTuiSurface is true when ANY changed file is a TUI path (mixed diff)', () => {
  assert.equal(
    classifyTuiSurface(['services/web/src/App.tsx', 'services/boss/internal/views/home.go']),
    true,
  )
})

test('classifyTuiSurface normalizes leading ./ and backslashes', () => {
  assert.equal(classifyTuiSurface(['./services/boss/cmd/root.go']), true)
  assert.equal(classifyTuiSurface(['services\\boss\\cmd\\root.go']), true)
})

// ── webUiSurfacePresent: no-UI-surface pre-gate (BOS-118) ────────────────────

test('webUiSurfacePresent is true for any changed file under a web UI prefix', () => {
  for (const prefix of WEB_UI_SURFACE_PREFIXES) {
    assert.equal(
      webUiSurfacePresent([`${prefix}some/file.tsx`]),
      true,
      `expected ${prefix}* to count as a web UI surface`,
    )
  }
})

test('webUiSurfacePresent covers the documented web UI prefixes', () => {
  assert.deepEqual(WEB_UI_SURFACE_PREFIXES, [
    'services/web/src/',
    'services/web/index.html',
    'services/web/public/',
  ])
})

test('webUiSurfacePresent is false for changes with no demonstrable web surface', () => {
  // The proof scripts themselves (this very PR's shape).
  assert.equal(webUiSurfacePresent(['scripts/proof-agent.mjs', 'scripts/proof.mjs']), false)
  // The web agent/specs are tests, not app UI.
  assert.equal(webUiSurfacePresent(['services/web/tests/e2e/agent/runner.ts']), false)
  // Backend, docs, proto.
  assert.equal(webUiSurfacePresent(['services/bossd/internal/server/server.go']), false)
  assert.equal(webUiSurfacePresent(['docs/plans/BOS-118.md']), false)
  assert.equal(webUiSurfacePresent([]), false)
  assert.equal(webUiSurfacePresent(null), false)
})

test('webUiSurfacePresent is true when ANY changed file is a web UI path (mixed diff)', () => {
  assert.equal(
    webUiSurfacePresent(['scripts/proof.mjs', 'services/web/src/pages/SessionDetail.tsx']),
    true,
  )
})

// ── committedScenarioPresent: the BOS-220 TUI scenario-authoring gate ─────────

test('committedScenarioPresent is true when a proof/scenarios/*.scenario.json is in the diff', () => {
  assert.equal(committedScenarioPresent(['proof/scenarios/demo.scenario.json']), true)
  // A subdirectory scenario still counts (permissive suffix+dir match).
  assert.equal(committedScenarioPresent(['proof/scenarios/tui/home.scenario.json']), true)
  // Present anywhere in a mixed diff counts.
  assert.equal(
    committedScenarioPresent([
      'services/boss/internal/views/home.go',
      'proof/scenarios/home.scenario.json',
    ]),
    true,
  )
})

test('committedScenarioPresent is false for a TUI diff with no scenario file', () => {
  // A TUI change that ships without a scenario — the exact case that defers.
  assert.equal(committedScenarioPresent(['services/boss/internal/views/home.go']), false)
  // Wrong directory (a recipe, not a scenario).
  assert.equal(committedScenarioPresent(['proof/recipes/home.json']), false)
  // Right directory, wrong extension / suffix.
  assert.equal(committedScenarioPresent(['proof/scenarios/README.md']), false)
  assert.equal(committedScenarioPresent(['proof/scenarios/home.json']), false)
  // A scenario-shaped name outside proof/scenarios/ does not count.
  assert.equal(committedScenarioPresent(['scripts/home.scenario.json']), false)
  assert.equal(committedScenarioPresent([]), false)
  assert.equal(committedScenarioPresent(null), false)
})

test('committedScenarioPresent normalizes leading ./ and backslashes', () => {
  assert.equal(committedScenarioPresent(['./proof/scenarios/demo.scenario.json']), true)
  assert.equal(committedScenarioPresent(['proof\\scenarios\\demo.scenario.json']), true)
})

// ── classifySurfaces: surface SET classifier (BOS-139 / D5) ──────────────────

test('classifySurfaces: tui-only diff → tui true, web false, no recipes', () => {
  const s = classifySurfaces({ changedFiles: ['services/boss/internal/views/home.go'], catalog })
  assert.equal(s.tui, true)
  assert.equal(s.web, false)
  assert.deepEqual(s.recipes, [])
})

test('classifySurfaces: web-only diff → web true, tui false', () => {
  const s = classifySurfaces({ changedFiles: ['services/web/src/App.tsx'], catalog })
  assert.equal(s.tui, false)
  assert.equal(s.web, true)
  assert.deepEqual(s.recipes, [])
})

test('classifySurfaces: mixed diff → BOTH true (any-match-wins single-select gone)', () => {
  const s = classifySurfaces({
    changedFiles: ['services/boss/internal/views/home.go', 'services/web/src/App.tsx'],
    catalog,
  })
  assert.equal(s.tui, true)
  assert.equal(s.web, true)
})

test('classifySurfaces: neither (backend-only) → both false, no recipes', () => {
  const s = classifySurfaces({
    changedFiles: ['services/bossd/internal/server/server.go'],
    catalog,
  })
  assert.equal(s.tui, false)
  assert.equal(s.web, false)
  assert.deepEqual(s.recipes, [])
})

test('classifySurfaces: marketing + web → web true and marketing recipe surfaced', () => {
  const s = classifySurfaces({
    changedFiles: ['services/web/src/App.tsx', 'services/marketing/src/pages/index.astro'],
    catalog,
  })
  assert.equal(s.web, true)
  assert.ok(s.recipes.some((r) => r.surface === 'marketing'))
})

test('classifySurfaces: forcedSurfaces forces tui onto a backend-only diff (D16 mitigation)', () => {
  const s = classifySurfaces({
    changedFiles: ['services/bossd/internal/server/server.go'],
    catalog,
    forcedSurfaces: ['tui'],
  })
  assert.equal(s.tui, true)
  assert.equal(s.web, false)
})

// ── Browser-capture surface registry (BOS-202) ──────────────────────────────

test('BUILTIN_SURFACES reproduces the shipped web/marketing/docs mapping', () => {
  assert.equal(BUILTIN_SURFACES.web.serviceDir, 'services/web')
  assert.equal(BUILTIN_SURFACES.web.specRoot, 'tests/e2e/specs')
  assert.deepEqual(BUILTIN_SURFACES.web.stageEnv, { VITE_E2E: '1' })
  assert.equal(BUILTIN_SURFACES.web.defaultCropToSelector, '#root')
  assert.equal(BUILTIN_SURFACES.marketing.serviceDir, 'services/marketing')
  assert.equal(BUILTIN_SURFACES.marketing.specRoot, 'tests/e2e')
  assert.equal(BUILTIN_SURFACES.marketing.defaultCropToSelector, 'main')
  assert.equal(BUILTIN_SURFACES.docs.serviceDir, 'services/docs')
  assert.equal(BUILTIN_SURFACES.docs.specRoot, 'tests/e2e')
  assert.equal(BUILTIN_SURFACES.docs.defaultCropToSelector, '#root')
})

test('resolveSurfaceRegistry with no catalog surfaces returns the built-ins', () => {
  const reg = resolveSurfaceRegistry({})
  assert.equal(reg.web.serviceDir, 'services/web')
  assert.deepEqual(Object.keys(reg).sort(), ['docs', 'marketing', 'web'])
})

test('resolveSurfaceRegistry adds a consumer-declared surface without touching built-ins', () => {
  const reg = resolveSurfaceRegistry({
    surfaces: [
      {
        name: 'portal',
        kind: 'browser',
        serviceDir: 'apps/portal',
        specRoot: 'e2e',
        defaultCropToSelector: 'main',
      },
    ],
  })
  assert.equal(reg.portal.serviceDir, 'apps/portal')
  assert.equal(reg.web.serviceDir, 'services/web') // built-in intact
})

test('resolveSurfaceRegistry field-merges an override onto a built-in', () => {
  const reg = resolveSurfaceRegistry({ surfaces: [{ name: 'docs', serviceDir: 'sites/docs' }] })
  assert.equal(reg.docs.serviceDir, 'sites/docs') // overridden
  assert.equal(reg.docs.specRoot, 'tests/e2e') // built-in field preserved
})

test('resolveSurfaceRegistry ignores nameless/invalid entries', () => {
  const reg = resolveSurfaceRegistry({ surfaces: [null, {}, { name: '' }] })
  assert.deepEqual(Object.keys(reg).sort(), ['docs', 'marketing', 'web'])
})

test('captureSurfaceDescriptor throws on an unknown surface', () => {
  assert.throws(
    () => captureSurfaceDescriptor(resolveSurfaceRegistry({}), 'nope'),
    /unknown proof surface: nope/,
  )
})

test('surfaceServiceDir resolves shipped and consumer surfaces', () => {
  const reg = resolveSurfaceRegistry({
    surfaces: [
      {
        name: 'portal',
        kind: 'browser',
        serviceDir: 'apps/portal',
        specRoot: 'e2e',
        defaultCropToSelector: 'main',
      },
    ],
  })
  assert.equal(surfaceServiceDir(reg, 'marketing'), 'services/marketing')
  assert.equal(surfaceServiceDir(reg, 'portal'), 'apps/portal')
})

test('browserServiceDir resolves shipped surfaces via the default registry', () => {
  assert.equal(browserServiceDir('web'), 'services/web')
  assert.equal(browserServiceDir('marketing'), 'services/marketing')
  assert.equal(browserServiceDir('docs'), 'services/docs')
})

test('browserServiceDir honors a consumer registry', () => {
  const reg = resolveSurfaceRegistry({
    surfaces: [
      {
        name: 'portal',
        kind: 'browser',
        serviceDir: 'apps/portal',
        specRoot: 'e2e',
        defaultCropToSelector: 'main',
      },
    ],
  })
  assert.equal(browserServiceDir('portal', reg), 'apps/portal')
})

test('browserSurfaceNames lists kind:browser surfaces', () => {
  assert.deepEqual(browserSurfaceNames(resolveSurfaceRegistry({})).sort(), [
    'docs',
    'marketing',
    'web',
  ])
})

test('classifySurfaces treats a consumer-declared browser surface recipe as a browser recipe', () => {
  const consumerCatalog = {
    surfaces: [
      {
        name: 'portal',
        kind: 'browser',
        serviceDir: 'apps/portal',
        specRoot: 'e2e',
        defaultCropToSelector: 'main',
      },
    ],
    recipes: [
      { id: 'portal-home', surface: 'portal', route: '/', viewport: { width: 1440, height: 1000 } },
    ],
    pathRules: [{ name: 'Portal', patterns: ['apps/portal/'], recipeIds: ['portal-home'] }],
  }
  const out = classifySurfaces({
    changedFiles: ['apps/portal/src/App.tsx'],
    catalog: consumerCatalog,
  })
  assert.deepEqual(
    out.recipes.map((r) => r.id),
    ['portal-home'],
  )
})

test('classifySurfaces still selects the shipped marketing/docs recipes', () => {
  // Regression: existing behavior preserved for built-in surfaces (no `surfaces` block).
  const builtinCatalog = {
    recipes: [
      { id: 'm', surface: 'marketing', route: '/', viewport: { width: 1440, height: 1000 } },
    ],
    pathRules: [{ name: 'M', patterns: ['services/marketing/'], recipeIds: ['m'] }],
  }
  const out = classifySurfaces({
    changedFiles: ['services/marketing/x.astro'],
    catalog: builtinCatalog,
  })
  assert.deepEqual(
    out.recipes.map((r) => r.id),
    ['m'],
  )
})

test('the shipped default.json surfaces block matches BUILTIN_SURFACES', () => {
  const defaultPath = fileURLToPath(new URL('../proof/recipes/default.json', import.meta.url))
  const defaultCatalog = JSON.parse(fs.readFileSync(defaultPath, 'utf8'))
  assert.ok(Array.isArray(defaultCatalog.surfaces) && defaultCatalog.surfaces.length === 3)
  const reg = resolveSurfaceRegistry(defaultCatalog)
  for (const name of ['web', 'marketing', 'docs']) {
    assert.equal(reg[name].serviceDir, BUILTIN_SURFACES[name].serviceDir)
    assert.equal(reg[name].specRoot, BUILTIN_SURFACES[name].specRoot)
    assert.equal(reg[name].defaultCropToSelector, BUILTIN_SURFACES[name].defaultCropToSelector)
    assert.deepEqual(reg[name].stageEnv, BUILTIN_SURFACES[name].stageEnv)
  }
})
