#!/usr/bin/env node
/** proof-agent-drivers.mjs — agent-driver (bespoke surface) registry for boss-proof.
 *  See docs/skills/agent-driver-contract.md. Built-in tui/web drivers are REFERENCE
 *  implementations registered through the same interface a repo uses to contribute its
 *  own bespoke drivers via BOS-193 skill extensions. */
import path from 'node:path'
import { pathToFileURL } from 'node:url'
import { discoverExtensions } from './skill-extensions.mjs'

const DEFAULT_ORDER = 100
const PROOF_CORE = 'boss-proof'
const SURFACE_RUN_KEYS = [
  'surface',
  'captureShapes',
  'brief',
  'agentResult',
  'hasFailure',
  'noSurface',
  'scanTexts',
  'elapsedMs',
  'reasonCode',
]

export function validateSurfaceRun(run, surface) {
  const errors = []
  if (!run || typeof run !== 'object') return { ok: false, errors: ['run is not an object'] }
  for (const k of SURFACE_RUN_KEYS) if (!(k in run)) errors.push(`missing ${k}`)
  if (run.surface !== surface) errors.push(`surface mismatch: ${run.surface} !== ${surface}`)
  return { ok: errors.length === 0, errors }
}

function isValidDriverRecord(d) {
  return (
    !!d &&
    typeof d === 'object' &&
    typeof d.surface === 'string' &&
    d.surface !== '' &&
    // budgetClass is dereferenced by the dispatcher (surfaceBudget(budgetClass));
    // an omitted/blank one resolves to a null budget and would silently defer the
    // surface as budget-exceeded forever. Require it so a misconfigured extension
    // is skipped with a warn instead of degrading to a silent perpetual defer.
    typeof d.budgetClass === 'string' &&
    d.budgetClass !== '' &&
    typeof d.usable === 'function' &&
    typeof d.preflightMissing === 'function' &&
    (d.prebuild === undefined || typeof d.prebuild === 'function') &&
    typeof d.run === 'function'
  )
}

/** The two built-in reference drivers, closing over proof.mjs's own already-imported helpers. */
export function builtinAgentDrivers(deps) {
  const tui = {
    surface: 'tui',
    budgetClass: 'tui',
    source: 'builtin',
    order: 0,
    name: 'bossanova-tui',
    usable: () => deps.tuiAgentUsable(),
    preflightMissing: ({ shouldUpload, repoRoot }) =>
      deps.evaluateRunPreflight({ surface: 'tui', shouldUpload, repoRoot })?.missing ?? [],
    prebuild: ({ repoRoot, env }) => {
      if (env.BOSS_PROOF_TUI_BRIDGE_BIN) return
      // Prefer prebuilt bins (BOS-215) so the go builds never run inside the
      // capture budget; fall back byte-identically to an in-budget build when
      // nothing is prebuilt. The log line makes the choice observable.
      const prebuilt = deps.resolvePrebuiltTuiBins({ repoRoot, env })
      const usingPrebuilt = Boolean(prebuilt.bridgeBin && prebuilt.bossBin)
      console.error(
        usingPrebuilt
          ? '[proof] tui: using prebuilt bridge + boss binaries (0 go builds in budget)'
          : '[proof] tui: building bridge/boss at proof time (no prebuilt binaries found)',
      )
      const existingBossBin = env.BOSS_PROOF_BOSS_BIN
      const { bridgeBin, bossBin } = deps.buildTuiAgentBridge({
        bridgeBinOverride: prebuilt.bridgeBin ?? undefined,
        bossBinOverride: prebuilt.bossBin ?? existingBossBin,
      })
      const bridgeEnv = deps.tuiAgentBridgeEnv({ bridgeBin, bossBin, existingBossBin })
      env.BOSS_PROOF_TUI_BRIDGE_BIN = bridgeEnv.BOSS_PROOF_TUI_BRIDGE_BIN
      env.BOSS_PROOF_BOSS_BIN = bridgeEnv.BOSS_PROOF_BOSS_BIN
    },
    // BOS-223: agent-first dispatch with an automatic scenario-replay fallback.
    // The orchestration runs the agent leg first, then replays each committed
    // scenario (BOS-219 seams) on gate-fail / crash / keyless. tuiAgentUsable()
    // is resolved HERE and passed as the leg-planning input.
    run: (ctx) =>
      deps.runTuiWithReplayFallback(ctx, {
        runTuiAgentProof: deps.runTuiAgentProof,
        agentUsable: deps.tuiAgentUsable(),
        runReplayLoop: deps.runReplayLoop,
        synthesizeBrief: deps.synthesizeBrief,
        makeScenarioEvaluator: deps.makeScenarioEvaluator,
        loadScenario: deps.loadScenario,
        replayReserveMs: deps.tuiReplayReserveMs,
      }),
  }
  const web = {
    surface: 'web',
    budgetClass: 'web',
    source: 'builtin',
    order: 0,
    name: 'bossanova-web',
    usable: () => deps.agentModeAvailable({ explicitRecipeSelection: false }),
    preflightMissing: ({ shouldUpload, repoRoot }) =>
      deps.evaluateRunPreflight({ surface: 'web', shouldUpload, repoRoot })?.missing ?? [],
    // web needs no prebuild; the no-ui-surface gate stays in the dispatcher.
    run: (ctx) => deps.runAgentProof(ctx),
  }
  return [tui, web]
}

/** Default loader: honor an inline `ext.driver` (test injection) else import `<dir>/driver.mjs`. */
async function defaultLoadDriver(ext) {
  if (ext && ext.driver !== undefined) return ext.driver
  const mod = await import(pathToFileURL(path.join(ext.dir, 'driver.mjs')).href)
  return mod.default ?? mod.driver ?? null
}

/** Built-ins unioned with role:agent-driver extensions, sorted by (order,name), deduped by
 *  surface (first-after-sort wins). Never throws: a malformed/failed extension is skipped with a warn. */
export async function discoverAgentDrivers({
  repoRoot,
  deps,
  discover = discoverExtensions,
  loadDriver = defaultLoadDriver,
}) {
  const builtins = builtinAgentDrivers(deps)
  let descriptors = []
  try {
    const res = (await discover({ core: PROOF_CORE, root: repoRoot, role: 'agent-driver' })) ?? {}
    descriptors = res.extensions ?? []
  } catch (err) {
    console.warn(`[proof-agent-drivers] extension discovery skipped: ${err.message}`)
  }
  const extras = []
  for (const ext of descriptors) {
    try {
      const driver = await loadDriver(ext)
      if (!isValidDriverRecord(driver)) {
        console.warn(`[proof-agent-drivers] extension "${ext.name}" skipped: invalid driver record`)
        continue
      }
      extras.push({ order: ext.order ?? DEFAULT_ORDER, name: ext.name, ...driver })
    } catch (err) {
      console.warn(`[proof-agent-drivers] extension "${ext?.name}" skipped: ${err.message}`)
    }
  }
  // Built-ins are authoritative: seed the seen-set with their surfaces so a
  // same-surface extension can never shadow a reference driver regardless of the
  // (order, name) it declares (docs/skills/agent-driver-contract.md: "a built-in
  // always wins"). Extensions are then deduped among themselves by (order, name).
  const seen = new Set(builtins.map((d) => d.surface))
  const dedupedExtras = extras
    .sort((a, b) => a.order - b.order || String(a.name).localeCompare(String(b.name)))
    .filter((d) => (seen.has(d.surface) ? false : (seen.add(d.surface), true)))
  return [...builtins, ...dedupedExtras]
}

export function driverForSurface(drivers, surface) {
  return drivers.find((d) => d.surface === surface) ?? null
}
