import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  builtinAgentDrivers,
  discoverAgentDrivers,
  driverForSurface,
  validateSurfaceRun,
} from './proof-agent-drivers.mjs'

const stubDeps = {
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
