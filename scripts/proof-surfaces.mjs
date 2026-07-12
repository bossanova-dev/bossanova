#!/usr/bin/env node

/**
 * Surface-descriptor registry (BOS-201): the single home for every piece of
 * concrete boss-proof surface knowledge. proof-lib.mjs is the abstract media
 * engine and knows only opaque surface-NAME strings; this module owns the
 * path-prefix maps, the surface→render-service-dir map, and the per-surface
 * budget values. Interim, extension-shaped: BOS-202/203 convert these entries
 * into discovered extension skills per the BOS-193 contract. Pure — no fs/env/Date.
 */

import { matchesAnyPrefix, normalizeChangedFiles, selectRecipes } from './proof-lib.mjs'

/**
 * Path prefixes that identify a TUI (boss) change. BOS-115 makes the TUI proof
 * surface agent-only and catalog-independent: surface detection keys off these
 * prefixes rather than the (now-removed) TUI recipe pathRules. Kept as a single
 * exported source of truth so the classifier and its test cannot drift.
 * Verbatim from the pre-BOS-201 proof-lib.mjs TUI_SURFACE_PREFIXES.
 * @type {readonly string[]}
 */
export const TUI_SURFACE_PREFIXES = [
  'services/boss/internal/views/',
  'services/boss/internal/tuitest/',
  'services/boss/internal/tuidriver/',
  'services/boss/internal/fixtures/',
  'services/boss/internal/client/',
  'services/boss/cmd/',
  'proto/',
]

/**
 * Path prefixes that hold the Vite web app's user-visible surface — the pages,
 * components, and assets the proof agent can actually drive in the running app.
 * Deliberately excludes `services/web/tests/` (the agent + specs themselves) and
 * everything under `scripts/`, the Go services, docs, and proto: a change there
 * has no surface to demonstrate. Single exported source of truth so the
 * classifier and its test cannot drift. Mirrors TUI_SURFACE_PREFIXES.
 * Verbatim from the pre-BOS-201 proof-lib.mjs WEB_UI_SURFACE_PREFIXES.
 * @type {readonly string[]}
 */
export const WEB_UI_SURFACE_PREFIXES = [
  'services/web/src/',
  'services/web/index.html',
  'services/web/public/',
]

/**
 * Every surface the boss-proof engine knows about. `renderServiceDir` is the
 * package whose Playwright context runs proof-render-* / proof-playwright-runner
 * for this surface (agent TUI intro cards render in services/web, matching the
 * old browserServiceDir default). `budget` is the shared-budget ladder input
 * (null for recipe surfaces, which the ladder never grants).
 * @type {ReadonlyArray<{name:string,kind:'agent'|'recipe',pathPrefixes:readonly string[],renderServiceDir:string,budget:{defaultMs:number,floorMs:number}|null}>}
 */
export const SURFACE_DESCRIPTORS = [
  {
    name: 'tui',
    kind: 'agent',
    pathPrefixes: TUI_SURFACE_PREFIXES,
    renderServiceDir: 'services/web',
    budget: { defaultMs: 4 * 60 * 1000, floorMs: 2 * 60 * 1000 },
  },
  {
    name: 'web',
    kind: 'agent',
    pathPrefixes: WEB_UI_SURFACE_PREFIXES,
    renderServiceDir: 'services/web',
    budget: { defaultMs: 12 * 60 * 1000, floorMs: 6 * 60 * 1000 },
  },
  {
    name: 'marketing',
    kind: 'recipe',
    pathPrefixes: [],
    renderServiceDir: 'services/marketing',
    budget: null,
  },
  {
    name: 'docs',
    kind: 'recipe',
    pathPrefixes: [],
    renderServiceDir: 'services/docs',
    budget: null,
  },
]

const BY_NAME = new Map(SURFACE_DESCRIPTORS.map((d) => [d.name, d]))

export function surfaceDescriptor(name) {
  return BY_NAME.get(name)
}

/**
 * Byte-identical replacement for the old proof-lib browserServiceDir: marketing
 * and docs map to their own package; every other surface (web, tui, default)
 * renders in services/web. Deliberately keeps the branch form (not a descriptor
 * lookup) so an unknown surface still falls through to services/web, exactly as
 * the old browserServiceDir did.
 * @param {string} name
 * @returns {string}
 */
export function surfaceRenderServiceDir(name) {
  if (name === 'marketing') return 'services/marketing'
  if (name === 'docs') return 'services/docs'
  return 'services/web'
}

export function surfaceBudget(name) {
  return BY_NAME.get(name)?.budget ?? null
}

export function isAgentSurface(name) {
  return BY_NAME.get(name)?.kind === 'agent'
}

// ── Browser-capture surface registry (BOS-202) ──────────────────────────────
//
// SURFACE_DESCRIPTORS above owns the CLASSIFICATION facets (agent-vs-recipe
// kind, path prefixes, render-service-dir, budget). BUILTIN_SURFACES below owns
// the BROWSER-CAPTURE facets: where a Playwright-driven surface's site is built
// and served (`serviceDir`), where the runner writes its spec (`specRoot`), the
// crop the video pipeline falls back to (`defaultCropToSelector`), and any extra
// env the runner exports before Playwright (`stageEnv`, web only today). These
// are a distinct concern from classification — `tui` is agent-only and has no
// browser-capture descriptor, so it is deliberately absent here.
//
// A consuming repo declares its own browser surfaces purely through its
// BOSS_PROOF_CATALOG file (a top-level `surfaces` array), overlaid onto these
// built-ins by resolveSurfaceRegistry — it never edits core. The `serviceDir`
// IS the "build + serve this site" hook: that package's playwright.config.ts
// `webServer` already declares the build+serve command, so core only names
// which config owns the surface and Playwright owns build/serve.
//
// @type {Record<string, {name:string,kind:string,serviceDir:string,specRoot:string,defaultCropToSelector:string,stageEnv?:Record<string,string>}>}
export const BUILTIN_SURFACES = {
  web: {
    name: 'web',
    kind: 'browser',
    serviceDir: 'services/web',
    specRoot: 'tests/e2e/specs',
    defaultCropToSelector: '#root',
    stageEnv: { VITE_E2E: '1' },
  },
  marketing: {
    name: 'marketing',
    kind: 'browser',
    serviceDir: 'services/marketing',
    specRoot: 'tests/e2e',
    defaultCropToSelector: 'main',
  },
  docs: {
    name: 'docs',
    kind: 'browser',
    serviceDir: 'services/docs',
    specRoot: 'tests/e2e',
    defaultCropToSelector: '#root',
  },
}

/**
 * Effective browser-surface registry: BUILTIN_SURFACES overlaid by the catalog's
 * optional top-level `surfaces` array. Each catalog entry is keyed by `name` and
 * shallow-merged over any built-in of the same name (catalog wins field-by-field,
 * so a consumer can override one field or declare a wholly new surface).
 * Nameless / non-string-name / null entries are ignored. Pure — no fs/env/Date.
 * @param {{surfaces?: object[]}} catalog
 * @returns {Record<string, object>}
 */
export function resolveSurfaceRegistry(catalog) {
  const registry = { ...BUILTIN_SURFACES }
  const declared = Array.isArray(catalog?.surfaces) ? catalog.surfaces : []
  for (const entry of declared) {
    if (!entry || typeof entry.name !== 'string' || entry.name.length === 0) continue
    registry[entry.name] = { ...(registry[entry.name] ?? {}), ...entry }
  }
  return registry
}

/**
 * Looks up a browser-capture descriptor in a resolved registry, throwing a clear
 * error for an unknown surface. Named distinctly from surfaceDescriptor(name)
 * (the classification lookup above) because it takes the resolved registry.
 * @param {Record<string, object>} registry
 * @param {string} surface
 * @returns {object}
 */
export function captureSurfaceDescriptor(registry, surface) {
  const descriptor = registry?.[surface]
  if (
    !descriptor ||
    typeof descriptor.serviceDir !== 'string' ||
    descriptor.serviceDir.length === 0
  ) {
    throw new Error(`unknown proof surface: ${surface}`)
  }
  return descriptor
}

/** Service dir that owns `surface` (its playwright.config.ts webServer builds+serves the site). */
export function surfaceServiceDir(registry, surface) {
  return captureSurfaceDescriptor(registry, surface).serviceDir
}

/**
 * Byte-compatible successor to the old proof-lib browserServiceDir: resolves a
 * browser surface to the package that owns its Playwright config, via the
 * registry. Defaults to the shipped BUILTIN_SURFACES; a caller with a loaded
 * catalog passes resolveSurfaceRegistry(catalog) so consumer surfaces resolve.
 * @param {string} surface
 * @param {Record<string, object>} [registry]
 * @returns {string}
 */
export function browserServiceDir(surface, registry = BUILTIN_SURFACES) {
  return surfaceServiceDir(registry, surface)
}

/** Names of all kind:'browser' surfaces in a resolved registry. */
export function browserSurfaceNames(registry) {
  return Object.values(registry ?? {})
    .filter((d) => d && d.kind === 'browser' && typeof d.name === 'string')
    .map((d) => d.name)
}

/**
 * Pure path classifier: true when ANY changed file lives under a TUI prefix.
 * Independent of the recipe catalog, so a boss-only diff routes to the agentic
 * TUI proof path even though the catalog no longer holds any TUI recipes.
 * (Moved from proof-lib, BOS-201.)
 *
 * Note: a mixed diff that touches BOTH a TUI prefix and a web/marketing/docs
 * surface classifies as TUI (any-match wins). This differs from the old
 * recipe-surface rule (which only inferred TUI when every matched recipe was
 * TUI); it is intentional — a PR that changes a TUI view should prove that view
 * via the agent rather than fall through to the web/recipe path.
 * @param {string[]|null|undefined} changedFiles
 * @returns {boolean}
 */
export function classifyTuiSurface(changedFiles) {
  return matchesAnyPrefix(changedFiles, TUI_SURFACE_PREFIXES)
}

/**
 * Pure path pre-gate: true when ANY changed file lives under a web UI surface
 * prefix. Used to skip the (expensive, ~12-minute) web agent run entirely for a
 * change with no demonstrable web surface (e.g. a scripts-only or backend-only
 * PR), so it posts an honest "no UI surface" note instead of running the agent,
 * having it decline, and posting useless filler. Biased slightly broad: a
 * non-visual change under `services/web/src/` still passes the gate and lets the
 * agent decide (and honestly defer), which is cheaper than a wrongful skip that
 * hides a real change. (Moved from proof-lib, BOS-201.)
 * @param {string[]|null|undefined} changedFiles
 * @returns {boolean}
 */
export function webUiSurfacePresent(changedFiles) {
  return matchesAnyPrefix(changedFiles, WEB_UI_SURFACE_PREFIXES)
}

/**
 * Matches a committed deterministic TUI-proof scenario file
 * (`proof/scenarios/*.scenario.json`, BOS-219 schema). Anchored so only files
 * under `proof/scenarios/` ending in `.scenario.json` count — never a bare
 * `proof/scenarios/README.md` or a recipe.
 * @type {RegExp}
 */
export const SCENARIO_FILE_RE = /^proof\/scenarios\/.+\.scenario\.json$/

/**
 * Pure predicate: true when the PR's changed-files list adds or modifies a
 * committed `proof/scenarios/*.scenario.json`. BOS-220 gates the TUI proof
 * surface on this — a TUI change that ships without a scenario gets a
 * `scenario-missing` deferral (exit 1 since BOS-226) nudging the author to commit one, so the
 * deterministic TUI proof (BOS-219) actually gets authored. Modeled on
 * webUiSurfacePresent (pure, array-in → bool-out); a suffix+dir match rather
 * than a bare prefix because only `*.scenario.json` files count. Detection is
 * changed-files-only — never a catalog or directory scan.
 * @param {string[]|null|undefined} changedFiles
 * @returns {boolean}
 */
export function committedScenarioPresent(changedFiles) {
  return normalizeChangedFiles(changedFiles ?? []).some((f) => SCENARIO_FILE_RE.test(f))
}

/**
 * Surface SET classifier (BOS-139 / epic D5). Replaces the single-select
 * dispatch (agentSurface) so a mixed diff proves BOTH the TUI and the web app.
 * Relocated to the surface registry (BOS-201).
 *
 * KNOWN LIMITATION (epic D16 / outside-voice #9): classification is by changed
 * FILE PATH, so a backend-only change with UI-visible effects (e.g. a bossd
 * handler that alters what the web app renders) classifies as no surface.
 * Mitigation: a plan `## Required proof` bullet that names a surface FORCES it
 * into the set via `forcedSurfaces` — required-proof bullets are the primary
 * brief source (D13) precisely because file paths cannot see behavior. Keep
 * this note in sync with the bs-proof SKILL.md "Known limitation" section.
 *
 * @param {{ changedFiles: string[], catalog: object, forcedSurfaces?: string[] }} opts
 * @returns {{ tui: boolean, web: boolean, recipes: object[] }}
 */
export function classifySurfaces({ changedFiles, catalog, forcedSurfaces = [] }) {
  const matched = selectRecipes(catalog, changedFiles)
  // Derive the browser-recipe surface set from the registry so a consumer-declared
  // browser surface's recipes are classified as browser recipes, not dropped. The
  // `!== 'web'` guard preserves today's semantics: `web` is an agent-driven surface
  // (proved live by the web agent, not by recipe capture), so `recipes` continues to
  // mean the marketing/docs-style recipe surfaces (+ any consumer browser surface).
  const browserSurfaces = new Set(browserSurfaceNames(resolveSurfaceRegistry(catalog)))
  return {
    tui: classifyTuiSurface(changedFiles) || forcedSurfaces.includes('tui'),
    web: webUiSurfacePresent(changedFiles) || forcedSurfaces.includes('web'),
    recipes: matched.filter((r) => browserSurfaces.has(r.surface) && r.surface !== 'web'),
  }
}
