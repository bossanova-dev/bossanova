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
    // BOS-354: raised 4→6 min (default) / 2→3 min (floor) so a legitimate
    // 4-scene Sonnet brief (slower per call since BOS-351) typically completes
    // rather than truncating mid-flight. Paired with the maxWallClockMs bump in
    // proof-tui-agent.mjs and the DEFAULT_TOTAL_PROOF_BUDGET_MS bump in proof-lib.mjs.
    budget: { defaultMs: 6 * 60 * 1000, floorMs: 3 * 60 * 1000 },
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

// ── Visibility tiers (BOS-1285) ──────────────────────────────────────────────
//
// The two surface classifiers below used to hand `matchesAnyPrefix` the RAW
// changed-file list, so ANY path under a surface prefix raised that surface —
// including files that compile but can never render. A changed file should
// raise a proof surface only when a reviewer could SEE its effect there, which
// splits the set into three tiers:
//
//   A. never demonstrable — a Go `_test.go` file. It compiles, it never
//      renders. Dropped from the set the classifier sees.
//   B. accompanying-only — a contract (`proto/`) or generated-client
//      (`services/boss/internal/client/`, `services/web/src/gen/`) edit has no
//      independent appearance. It raises a surface only when a RENDERING file
//      for that same surface also changed.
//   C. rendering — everything else under that surface's prefixes.
//
// The remedy is deliberately a FILTER in front of the prefix lists, never a
// deletion from them: `TUI_SURFACE_PREFIXES` / `WEB_UI_SURFACE_PREFIXES` stay
// the readable answer to "what IS this surface", and a mixed diff that touches
// both a `.proto` and a view still proves the view.

/**
 * Tier A — a Go test file. Anchored at a path segment boundary so
 * `views/home_test.go` matches while a hand-written `views/pretest_go.go` or a
 * directory named `_test.go/` does not.
 * @type {RegExp}
 */
export const NEVER_RENDERING_FILE_RE = /(?:^|\/)[^/]*_test\.go$/

/**
 * Tier B — path prefixes whose files accompany a visible change rather than
 * being one. `proto/` and `services/boss/internal/client/` are TUI prefixes
 * (a contract or client edit can alter what a view renders or receives);
 * `services/web/src/gen/` is the buf `protoc-gen-es` output under a web prefix
 * (`buf.gen.yaml`). Each stays in its surface's prefix list; this tier only
 * stops it raising the surface ON ITS OWN.
 * @type {readonly string[]}
 */
export const ACCOMPANYING_ONLY_PREFIXES = [
  'proto/',
  'services/boss/internal/client/',
  'services/web/src/gen/',
]

/**
 * Tier A membership for a single path. Pure; normalizes `./` and backslashes
 * the same way every other predicate in this file does.
 * @param {string|null|undefined} file
 * @returns {boolean}
 */
export function neverRenderingPath(file) {
  const [normalized] = normalizeChangedFiles([file])
  return normalized !== undefined && NEVER_RENDERING_FILE_RE.test(normalized)
}

/**
 * Tier B membership for a single path. Pure.
 * @param {string|null|undefined} file
 * @returns {boolean}
 */
export function accompanyingOnlyPath(file) {
  return matchesAnyPrefix([file], ACCOMPANYING_ONLY_PREFIXES)
}

/**
 * The tier-C (rendering) subset of `changedFiles` for one surface: every
 * changed file under `surfacePrefixes` that is neither tier A nor tier B.
 * Array in, array out; no fs/env/Date.
 * @param {string[]|null|undefined} changedFiles
 * @param {readonly string[]} surfacePrefixes
 * @returns {string[]}
 */
export function renderingFilesForSurface(changedFiles, surfacePrefixes) {
  return normalizeChangedFiles(changedFiles ?? []).filter(
    (file) =>
      !neverRenderingPath(file) &&
      !accompanyingOnlyPath(file) &&
      matchesAnyPrefix([file], surfacePrefixes),
  )
}

/**
 * True when the diff holds at least one rendering file for `surfacePrefixes`.
 * This is the single replacement for the bare `matchesAnyPrefix(changedFiles,
 * …)` the two classifiers below used to call. Array in, boolean out, pure —
 * same shape as `proofHarnessOnlyDiff` / `committedScenarioPresent`.
 * @param {string[]|null|undefined} changedFiles
 * @param {readonly string[]} surfacePrefixes
 * @returns {boolean}
 */
export function renderingFilePresent(changedFiles, surfacePrefixes) {
  return renderingFilesForSurface(changedFiles, surfacePrefixes).length > 0
}

/**
 * Pure path classifier: true when a RENDERING changed file lives under a TUI
 * prefix. Independent of the recipe catalog, so a boss-only diff routes to the
 * agentic TUI proof path even though the catalog no longer holds any TUI
 * recipes. (Moved from proof-lib, BOS-201.)
 *
 * BOS-1285: "rendering" is the tier-C filter above, not a bare prefix match — a
 * diff whose only TUI-prefixed files are `_test.go` compile fallout, `proto/`
 * contracts, or `services/boss/internal/client/` transport does NOT raise the
 * surface, because there is nothing a reviewer could watch. Pair any of those
 * with a file under `services/boss/internal/views/` and the surface is back.
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
  return renderingFilePresent(changedFiles, TUI_SURFACE_PREFIXES)
}

/**
 * Pure path pre-gate: true when a RENDERING changed file lives under a web UI
 * surface prefix (BOS-1285: buf's generated `services/web/src/gen/` output is
 * tier B and never raises the surface alone). Used to skip the (expensive, ~12-minute) web agent run entirely for a
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
  return renderingFilePresent(changedFiles, WEB_UI_SURFACE_PREFIXES)
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
 * BOS-356: matches a proof-harness script — `scripts/proof*.{mjs,js,mts,cjs}`.
 * Anchored so it catches the whole harness family (`scripts/proof.mjs`,
 * `scripts/proof-lib.mjs`, `scripts/proof-*.test.mjs`, `scripts/proof-brief.d.mts`,
 * `scripts/proof-*.eval.mjs`) while never catching a non-proof script such as
 * `scripts/skill-extensions.mjs`. Verified against the live `scripts/` listing:
 * every `scripts/proof*` file matches, and nothing outside that family does.
 * @type {RegExp}
 */
export const PROOF_HARNESS_FILE_RE = /^scripts\/proof[\w.-]*\.(?:mjs|js|mts|cjs)$/

/**
 * BOS-356: matches a TOP-LEVEL plan doc (`docs/plans/<name>.md`, no subdir),
 * mirroring loadPlanEvidence's no-subdir rule (scripts/proof-brief.mjs:79-86).
 * A harness PR commits its plan alongside the harness edits, so a plan doc must
 * count as "still harness-only" — but only at the top level, matching the run
 * path that actually reads these bullets.
 * @type {RegExp}
 */
export const PLAN_DOC_RE = /^docs\/plans\/[^/]+\.md$/

/**
 * BOS-356: pure predicate — true when a diff is ENTIRELY the proof harness
 * itself (`scripts/proof*` scripts) plus optionally its top-level plan doc, with
 * at least one harness file present. Such a diff changes no product surface, so
 * running a live TUI/web agent captures a stock demo unrelated to the change
 * (useless proof) and — post-BOS-226 — can fail fatally on an `agent-incomplete`
 * flake. resolveSurfacePlan uses this to zero the surface set and defer to the
 * honest `no-ui-surface` note (exit 0).
 *
 * The "≥1 harness file" clause keeps a pure docs-only diff (`docs/plans/*.md`
 * with no harness script) from being mislabeled "harness-only". A committed
 * `proof/scenarios/*.scenario.json` is neither a harness script nor a plan doc,
 * so a scenario-bearing diff is (by construction) NOT harness-only — that is the
 * deliberate opt-in to the existing deterministic replay (BOS-223).
 * Modeled on committedScenarioPresent: array-in → bool-out, no fs/env/Date.
 * @param {string[]|null|undefined} changedFiles
 * @returns {boolean}
 */
export function proofHarnessOnlyDiff(changedFiles) {
  const files = normalizeChangedFiles(changedFiles ?? [])
  if (files.length === 0) return false
  let sawHarnessFile = false
  for (const f of files) {
    if (PROOF_HARNESS_FILE_RE.test(f)) {
      sawHarnessFile = true
      continue
    }
    if (!PLAN_DOC_RE.test(f)) return false
  }
  return sawHarnessFile
}

/**
 * BOS-1285: path prefixes that hold no product source at all — documentation,
 * the proof/build harness, the agent-skill payload. A change confined to them
 * cannot alter what ANY surface renders, directly or behaviourally.
 * @type {readonly string[]}
 */
export const NON_PRODUCT_PREFIXES = ['docs/', 'scripts/', 'skills-toolbox/', '.claude/', '.codex/']

/**
 * BOS-1285: the tier-B prefixes whose contents are MACHINE-GENERATED, and so
 * carry no hand-authored behaviour of their own.
 *
 * Deliberately NOT `ACCOMPANYING_ONLY_PREFIXES`. Tier B answers the narrow
 * question "does this raise a surface ON ITS OWN" — for which a hand-written
 * RPC client belongs — while `productSourcePresent` below asks the much
 * stronger "could this change what a user experiences at all". Reusing tier B
 * for both conflated the two and silently neutralised the R6 escape hatch:
 * `services/boss/internal/client/` is hand-written Go compiled into the boss
 * binary (client.go, local.go, remote.go; no `Code generated` marker), so a
 * client-only change that alters what a view receives read as "no product
 * source", and the `## Required proof` bullet forcing the TUI surface for it
 * was discarded as `forced-no-surface`. `proto/` and `services/web/src/gen/`
 * ARE generated, so they stay non-product here.
 * @type {readonly string[]}
 */
export const GENERATED_ARTIFACT_PREFIXES = ['proto/', 'services/web/src/gen/']

/** Markdown/MDX anywhere in the tree is prose, never product source. */
export const PROSE_FILE_RE = /\.(?:md|mdx)$/

/**
 * BOS-1285: pure predicate — true when the diff holds at least one file that
 * could change what a user experiences, directly or behaviourally.
 *
 * This is the guard that keeps the `forcedSurfaces` escape hatch alive. A
 * behaviour-only change (say a bossd handler that alters what the web app
 * renders) classifies to no surface by path, and a plan's `## Required proof`
 * bullet is the documented way to force one back (the D16 mitigation on
 * `classifySurfaces`). That force must keep working, so the
 * forced-but-undemonstrable deferral in proof.mjs fires ONLY when the diff is
 * entirely non-product: prose, harness scripts, the skills payload, tier-A test
 * files and the GENERATED artifacts in `GENERATED_ARTIFACT_PREFIXES`. Anything
 * else — a Go handler, a hand-written RPC client, a `.tsx`, a migration — is
 * product source and keeps its forced surface.
 *
 * Array in, boolean out; no fs/env/Date.
 * @param {string[]|null|undefined} changedFiles
 * @returns {boolean}
 */
export function productSourcePresent(changedFiles) {
  return normalizeChangedFiles(changedFiles ?? []).some(
    (file) =>
      !neverRenderingPath(file) &&
      !matchesAnyPrefix([file], GENERATED_ARTIFACT_PREFIXES) &&
      !PROSE_FILE_RE.test(file) &&
      !matchesAnyPrefix([file], NON_PRODUCT_PREFIXES),
  )
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
