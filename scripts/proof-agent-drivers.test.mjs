import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  builtinAgentDrivers,
  discoverAgentDrivers,
  driverForSurface,
  validateSurfaceRun,
} from './proof-agent-drivers.mjs'
import { silenceConsole } from './quiet-test-console.mjs'

// Silence the code-under-test console output (finalize manifest JSON dumps +
// DEGRADED warnings) so a passing run stays quiet. See quiet-test-console.mjs.
silenceConsole()

const stubDeps = {
  tuiAgentUsable: () => true,
  buildTuiAgentBridge: () => ({ bridgeBin: '/tmp/b', bossBin: '/tmp/boss' }),
  resolvePrebuiltTuiBins: () => ({ bridgeBin: null, bossBin: null }),
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

test('builtinAgentDrivers: tui/web reference drivers with the driver interface', () => {
  const drivers = builtinAgentDrivers(stubDeps)
  assert.deepEqual(drivers.map((d) => d.surface).sort(), ['tui', 'web'])
  for (const d of drivers) {
    assert.equal(typeof d.usable, 'function')
    assert.equal(typeof d.preflightMissing, 'function')
    assert.equal(typeof d.run, 'function')
    assert.ok(['tui', 'web'].includes(d.budgetClass))
  }
  const tui = driverForSurface(drivers, 'tui')
  const web = driverForSurface(drivers, 'web')
  assert.equal(typeof tui.prebuild, 'function')
  assert.equal(web.prebuild, undefined)
})

test('tui driver prebuild: prefers prebuilt bins and skips the in-budget build (BOS-215)', () => {
  const calls = []
  const deps = {
    ...stubDeps,
    resolvePrebuiltTuiBins: ({ repoRoot, env }) => {
      calls.push(['resolve', repoRoot, env.BOSS_PROOF_BOSS_BIN])
      return { bridgeBin: '/pre/bridge', bossBin: '/pre/boss' }
    },
    buildTuiAgentBridge: (opts) => {
      calls.push(['build', opts])
      return { bridgeBin: opts.bridgeBinOverride, bossBin: opts.bossBinOverride }
    },
    tuiAgentBridgeEnv: ({ bridgeBin, bossBin }) => ({
      BOSS_PROOF_TUI_BRIDGE_BIN: bridgeBin,
      BOSS_PROOF_BOSS_BIN: bossBin,
    }),
  }
  const tui = driverForSurface(builtinAgentDrivers(deps), 'tui')
  const env = {}
  tui.prebuild({ repoRoot: '/repo', env })
  // Prebuilt pair resolved → buildTuiAgentBridge is handed both overrides so it
  // spawns nothing, and the resolved paths land on the env for the bridge.
  assert.deepEqual(calls[0], ['resolve', '/repo', undefined])
  assert.deepEqual(calls[1], [
    'build',
    { bridgeBinOverride: '/pre/bridge', bossBinOverride: '/pre/boss' },
  ])
  assert.equal(env.BOSS_PROOF_TUI_BRIDGE_BIN, '/pre/bridge')
  assert.equal(env.BOSS_PROOF_BOSS_BIN, '/pre/boss')
})

test('tui driver prebuild: no prebuilt bins → in-budget build fallback (BOS-215)', () => {
  const calls = []
  const deps = {
    ...stubDeps,
    resolvePrebuiltTuiBins: () => ({ bridgeBin: null, bossBin: null }),
    buildTuiAgentBridge: (opts) => {
      calls.push(opts)
      return { bridgeBin: '/tmp/bridge', bossBin: '/tmp/boss' }
    },
  }
  const tui = driverForSurface(builtinAgentDrivers(deps), 'tui')
  tui.prebuild({ repoRoot: '/repo', env: {} })
  // No override → buildTuiAgentBridge builds the old way (bridgeBinOverride undefined).
  assert.equal(calls.length, 1)
  assert.equal(calls[0].bridgeBinOverride, undefined)
})

test('discoverAgentDrivers: no extensions → only built-ins', async () => {
  const drivers = await discoverAgentDrivers({
    repoRoot: '/nowhere',
    env: {},
    deps: stubDeps,
    discover: async () => ({ extensions: [] }),
  })
  assert.deepEqual(drivers.map((d) => d.name).sort(), ['bossanova-tui', 'bossanova-web'])
})

test('discoverAgentDrivers: union + ordering, malformed extension skipped, no throw', async () => {
  const drivers = await discoverAgentDrivers({
    repoRoot: '/nowhere',
    env: {},
    deps: stubDeps,
    discover: async () => ({
      extensions: [
        {
          name: 'boss-proof-zeta',
          order: 1,
          driver: {
            surface: 'zeta',
            budgetClass: 'web',
            usable: () => true,
            preflightMissing: () => [],
            run: async () => ({ surface: 'zeta' }),
          },
        },
        { name: 'boss-proof-alpha', order: 1, driver: null },
      ],
    }),
    loadDriver: (e) => e.driver,
  })
  const zeta = drivers.filter((d) => d.surface === 'zeta')
  assert.equal(zeta.length, 1)
  assert.equal(zeta[0].name, 'boss-proof-zeta')
  assert.equal(driverForSurface(drivers, 'alpha'), null)
})

test('discoverAgentDrivers: extension missing budgetClass is skipped (not a silent perpetual defer)', async () => {
  const drivers = await discoverAgentDrivers({
    repoRoot: '/nowhere',
    deps: stubDeps,
    discover: async () => ({
      extensions: [
        {
          name: 'boss-proof-nobudget',
          order: 1,
          driver: {
            surface: 'nobudget',
            // budgetClass intentionally omitted — the dispatcher would resolve
            // surfaceBudget(undefined) → null and defer this surface forever.
            usable: () => true,
            preflightMissing: () => [],
            run: async () => ({ surface: 'nobudget' }),
          },
        },
      ],
    }),
    loadDriver: (e) => e.driver,
  })
  assert.equal(driverForSurface(drivers, 'nobudget'), null)
  assert.deepEqual(drivers.map((d) => d.name).sort(), ['bossanova-tui', 'bossanova-web'])
})

test('discoverAgentDrivers: discovery throw → built-ins, no throw', async () => {
  const drivers = await discoverAgentDrivers({
    repoRoot: '/nowhere',
    env: {},
    deps: stubDeps,
    discover: async () => {
      throw new Error('boom')
    },
  })
  assert.deepEqual(drivers.map((d) => d.name).sort(), ['bossanova-tui', 'bossanova-web'])
})

test('discoverAgentDrivers: surface-colliding extension deduped out, built-in wins', async () => {
  const drivers = await discoverAgentDrivers({
    repoRoot: '/nowhere',
    env: {},
    deps: stubDeps,
    discover: async () => ({
      extensions: [
        {
          name: 'boss-proof-tui-override',
          order: 5,
          driver: {
            surface: 'tui',
            budgetClass: 'tui',
            usable: () => true,
            preflightMissing: () => [],
            run: async () => ({ surface: 'tui' }),
          },
        },
      ],
    }),
    loadDriver: (e) => e.driver,
  })
  const tui = drivers.filter((d) => d.surface === 'tui')
  assert.equal(tui.length, 1)
  assert.equal(tui[0].name, 'bossanova-tui')
})

test('driverForSurface: unknown surface → null', () => {
  assert.equal(driverForSurface(builtinAgentDrivers(stubDeps), 'nope'), null)
})

test('validateSurfaceRun: shape + surface match', () => {
  assert.equal(validateSurfaceRun({ surface: 'tui' }, 'tui').ok, false)
  const full = {
    surface: 'tui',
    captureShapes: [],
    brief: {},
    agentResult: { passed: true, summary: '', evidence: [], steps: 0 },
    hasFailure: false,
    noSurface: false,
    scanTexts: [],
    elapsedMs: 0,
    reasonCode: null,
  }
  assert.equal(validateSurfaceRun(full, 'tui').ok, true)
  assert.equal(validateSurfaceRun(full, 'web').ok, false)
})
