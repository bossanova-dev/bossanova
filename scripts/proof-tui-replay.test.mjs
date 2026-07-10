/**
 * proof-tui-replay.test.mjs — Unit tests for the deterministic scenario replay
 * engine (BOS-219).
 *
 * The replay loop drives a scenario (BOS-218 loader shape) over an injected
 * bridge with NO LLM involvement. Every test uses a scripted fake bridge that
 * records the ops it received and returns canned screens — nothing spawns the Go
 * bridge, agg, ffmpeg, or hits the Anthropic API.
 */

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

import { loadScenario, deriveScenes } from './proof-scenario.mjs'
import { validateBrief } from './proof-brief.mjs'
import { runReplayLoop, synthesizeBrief, makeScenarioEvaluator } from './proof-tui-replay.mjs'

const FIXTURE_DIR = fileURLToPath(new URL('./testdata/scenario-fixtures/', import.meta.url))
const RICH = 'Session settings\nArchive delay: 45m\n45 minutes\nArchive delay'

/**
 * Scripted replay bridge. Records every op in `ops`; each op returns a screen.
 * `screen` (string) is the default screen for every op; `observeScreens`
 * overrides the observe() sequence (last value repeats); `throwOn` names an op
 * whose call throws to simulate a bridge crash; `omitDaemon` drops the daemon
 * method so the old-bridge loud-error path can be exercised.
 */
function replayBridge({
  screen = RICH,
  observeScreens = null,
  throwOn = null,
  omitDaemon = false,
  daemonThrow = null,
} = {}) {
  let obsIdx = 0
  const ops = []
  const bridge = {
    ops,
    observeCount: 0,
    async observe() {
      bridge.observeCount += 1
      ops.push(['observe'])
      if (throwOn === 'observe') throw new Error('observe boom')
      if (observeScreens) {
        const s = observeScreens[Math.min(obsIdx, observeScreens.length - 1)]
        obsIdx += 1
        return { screen: s ?? '' }
      }
      return { screen }
    },
    async key(k) {
      ops.push(['key', k])
      if (throwOn === 'key') throw new Error('key boom')
      return { screen }
    },
    async enter() {
      ops.push(['enter'])
      if (throwOn === 'enter') throw new Error('enter boom')
      return { screen }
    },
    async esc() {
      ops.push(['esc'])
      if (throwOn === 'esc') throw new Error('esc boom')
      return { screen }
    },
    async type(t) {
      ops.push(['type', t])
      if (throwOn === 'type') throw new Error('type boom')
      return { screen }
    },
  }
  // The REAL bridge (services/boss/cmd/proof-tui-agent) has no fixed-sleep op —
  // its `wait` op is wait-FOR-TEXT — so waitMs is a client-side sleep + observe.
  // `daemon` is dropped when omitDaemon to model an OLD bridge; `daemonThrow`
  // models a present method that a stale Go bridge rejects with `unknown op`.
  if (!omitDaemon) {
    bridge.daemon = async (d) => {
      ops.push(['daemon', d])
      if (throwOn === 'daemon') throw new Error(daemonThrow ?? 'daemon boom')
      return { screen }
    }
  }
  return bridge
}

function withTempRawDir(fn) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-replay-'))
  return Promise.resolve()
    .then(() => fn(dir))
    .finally(() => fs.rmSync(dir, { recursive: true, force: true }))
}

// ── Step→op mapping ─────────────────────────────────────────────────────────

test('runReplayLoop: maps each step op to the matching bridge call', async () => {
  const scenario = {
    version: 1,
    title: 'Mapping',
    fixture: { preset: 'demo' },
    scenes: [
      {
        id: 'scene-01',
        title: 'Ops',
        steps: [
          { key: 'down' },
          { key: 'enter' },
          { key: 'esc' },
          { type: '45' },
          { waitMs: 500 },
          { daemon: { action: 'push_output', sessionId: 'sess-1' } },
        ],
      },
    ],
  }
  await withTempRawDir(async (rawDir) => {
    const bridge = replayBridge()
    const sleeps = []
    const sleep = async (ms) => {
      sleeps.push(ms)
    }
    const result = await runReplayLoop({ scenario, bridge, rawDir, sleep })
    // {key:"enter"|"esc"} → dedicated ops; other keys → key(); {type} → type();
    // {waitMs} → client-side sleep() then observe() (the Go `wait` op is
    // wait-for-text, not a fixed dwell); {daemon} → daemon(params).
    assert.deepEqual(bridge.ops, [
      ['key', 'down'],
      ['enter'],
      ['esc'],
      ['type', '45'],
      ['observe'],
      ['daemon', { action: 'push_output', sessionId: 'sess-1' }],
    ])
    assert.deepEqual(sleeps, [500], 'waitMs must be a client-side fixed sleep')
    // Every bridge-touching step writes raw/screen-NN.txt and appends to screens.
    assert.equal(result.screens.length, 6)
    for (let i = 1; i <= 6; i += 1) {
      const p = path.join(rawDir, `screen-${String(i).padStart(2, '0')}.txt`)
      assert.ok(fs.existsSync(p), `screen-${i} raw file must exist`)
    }
    for (const s of result.screens) assert.equal(result.sceneForScreen[s.seq], 'scene-01')
  })
})

// ── Full scenario pass (valid-full fixture) ─────────────────────────────────

test('runReplayLoop: valid-full fixture replays to a passing agentResult', async () => {
  const { scenario } = loadScenario(path.join(FIXTURE_DIR, 'valid-full.json'))
  await withTempRawDir(async (rawDir) => {
    const bridge = replayBridge()
    const result = await runReplayLoop({
      scenario,
      bridge,
      rawDir,
      fileBasename: 'valid-full.scenario.json',
      sleep: async () => {},
    })
    // key down, key enter, waitFor(observe), type, waitMs, daemon → 6 screens; expect has no frame.
    assert.equal(result.screens.length, 6)
    assert.equal(result.agentResult.passed, true)
    assert.equal(
      result.agentResult.summary,
      'deterministic scenario replay of valid-full.scenario.json',
    )
    assert.deepEqual(result.agentResult.evidence, [])
    // Only the captioned frame-producing step emits a captionTiming (the key-down
    // step's "Move to the session"). The expect step's caption has no frame.
    assert.equal(result.captionTimings.length, 1)
    assert.equal(result.captionTimings[0].caption, 'Move to the session')
    assert.equal(result.sceneTimings.length, 1)
    assert.equal(result.sceneTimings[0].startMs, 0)
  })
})

// ── Return-contract parity with runAgentLoop ────────────────────────────────

test('runReplayLoop: return shape is key-identical to the runAgentLoop contract', async () => {
  const { scenario } = loadScenario(path.join(FIXTURE_DIR, 'valid-minimal.json'))
  await withTempRawDir(async (rawDir) => {
    const bridge = replayBridge({ screen: 'something on screen' })
    const result = await runReplayLoop({ scenario, bridge, rawDir })
    assert.deepEqual(Object.keys(result).sort(), [
      'agentResult',
      'captionTimings',
      'finalScreen',
      'sceneForScreen',
      'sceneTimings',
      'screens',
    ])
  })
})

// ── Timings from the injected cast clock ────────────────────────────────────

test('runReplayLoop: timings come from the injected readCastMs clock', async () => {
  const scenario = {
    version: 1,
    title: 'Timings',
    fixture: { preset: 'demo' },
    scenes: [
      { id: 'scene-01', title: 'One', steps: [{ key: 'a', caption: 'first' }] },
      { id: 'scene-02', title: 'Two', steps: [{ key: 'b', caption: 'second' }] },
    ],
  }
  await withTempRawDir(async (rawDir) => {
    let n = 0
    const readCastMs = () => {
      n += 1
      return n * 1000
    }
    const bridge = replayBridge({ screen: 'S' })
    const result = await runReplayLoop({ scenario, bridge, rawDir, readCastMs })
    // Scene 1 anchored at 0; scene 2 anchored at the readCastMs read taken when
    // it begins.
    assert.equal(result.sceneTimings[0].startMs, 0)
    assert.equal(result.sceneTimings[0].id, 'scene-01')
    assert.equal(result.sceneTimings[1].id, 'scene-02')
    assert.ok(Number.isFinite(result.sceneTimings[1].startMs))
    // Each captured screen and caption carries a cast-clock ms.
    assert.ok(result.screens.every((s) => Number.isFinite(s.castMs)))
    assert.ok(result.captionTimings.every((c) => Number.isFinite(c.startMs)))
  })
})

// ── waitFor: observe-poll semantics ─────────────────────────────────────────

test('runReplayLoop: waitFor resolves on the observe where the matcher hits', async () => {
  const scenario = {
    version: 1,
    title: 'Wait',
    fixture: { preset: 'demo' },
    scenes: [{ id: 'scene-01', title: 'W', steps: [{ waitFor: 'READY', timeoutMs: 10000 }] }],
  }
  await withTempRawDir(async (rawDir) => {
    const bridge = replayBridge({ observeScreens: ['loading', 'still loading', 'READY now'] })
    const result = await runReplayLoop({ scenario, bridge, rawDir })
    assert.equal(bridge.observeCount, 3)
    assert.equal(result.screens.length, 1)
    assert.equal(result.screens[0].text, 'READY now')
    assert.equal(result.agentResult.passed, true)
  })
})

test('runReplayLoop: waitFor timeout returns (never throws) with a step-pointered failure', async () => {
  const scenario = {
    version: 1,
    title: 'WaitTimeout',
    fixture: { preset: 'demo' },
    scenes: [{ id: 'scene-01', title: 'W', steps: [{ waitFor: 'NEVER', timeoutMs: 5 }] }],
  }
  await withTempRawDir(async (rawDir) => {
    const bridge = replayBridge({ observeScreens: ['nothing here'] })
    const result = await runReplayLoop({ scenario, bridge, rawDir })
    assert.equal(result.agentResult.passed, false)
    assert.match(result.agentResult.summary, /scenes\[0\]\.steps\[0\]/)
    assert.match(result.agentResult.summary, /waitFor|timed out/i)
  })
})

// ── Bridge crash mid-scenario ───────────────────────────────────────────────

test('runReplayLoop: bridge crash returns a failed result naming the failing step', async () => {
  const scenario = {
    version: 1,
    title: 'Crash',
    fixture: { preset: 'demo' },
    scenes: [{ id: 'scene-01', title: 'C', steps: [{ key: 'a' }, { type: 'boom' }] }],
  }
  await withTempRawDir(async (rawDir) => {
    const bridge = replayBridge({ throwOn: 'type' })
    const result = await runReplayLoop({ scenario, bridge, rawDir })
    assert.equal(result.agentResult.passed, false)
    assert.match(result.agentResult.summary, /scenes\[0\]\.steps\[1\]/)
    // The screen captured before the crash is preserved.
    assert.equal(result.screens.length, 1)
  })
})

// ── Wall-clock budget ───────────────────────────────────────────────────────

test('runReplayLoop: exceeding the wall-clock budget aborts with a budget summary', async () => {
  const scenario = {
    version: 1,
    title: 'Budget',
    fixture: { preset: 'demo' },
    scenes: [{ id: 'scene-01', title: 'B', steps: [{ key: 'a' }] }],
  }
  await withTempRawDir(async (rawDir) => {
    const bridge = replayBridge()
    const result = await runReplayLoop({ scenario, bridge, rawDir, maxWallClockMs: -1 })
    assert.equal(result.agentResult.passed, false)
    assert.match(result.agentResult.summary, /budget/i)
  })
})

// ── daemon op against an old bridge ─────────────────────────────────────────

test('runReplayLoop: daemon step against a bridge without a daemon op fails loud (BOS-217)', async () => {
  const scenario = {
    version: 1,
    title: 'Daemon',
    fixture: { preset: 'demo' },
    scenes: [{ id: 'scene-01', title: 'D', steps: [{ daemon: { action: 'x' } }] }],
  }
  await withTempRawDir(async (rawDir) => {
    const bridge = replayBridge({ omitDaemon: true })
    const result = await runReplayLoop({ scenario, bridge, rawDir })
    assert.equal(result.agentResult.passed, false)
    assert.match(result.agentResult.summary, /BOS-217/)
  })
})

test('runReplayLoop: daemon op rejected with "unknown op" by a stale Go bridge fails loud (BOS-217)', async () => {
  const scenario = {
    version: 1,
    title: 'DaemonUnknownOp',
    fixture: { preset: 'demo' },
    scenes: [{ id: 'scene-01', title: 'D', steps: [{ daemon: { action: 'x' } }] }],
  }
  await withTempRawDir(async (rawDir) => {
    // makeStdioBridge now always exposes daemon(), so an OLD Go bridge surfaces as
    // a thrown `bridge op 'daemon' failed: unknown op "daemon"` from request().
    const bridge = replayBridge({
      daemonThrow: `bridge op 'daemon' failed: unknown op "daemon"`,
      throwOn: 'daemon',
    })
    const result = await runReplayLoop({ scenario, bridge, rawDir })
    assert.equal(result.agentResult.passed, false)
    assert.match(result.agentResult.summary, /BOS-217/)
  })
})

// ── synthesizeBrief ─────────────────────────────────────────────────────────

test('synthesizeBrief: produces a brief that passes validateBrief', () => {
  const { scenario } = loadScenario(path.join(FIXTURE_DIR, 'valid-full.json'))
  const brief = synthesizeBrief(scenario, 'valid-full.scenario.json')
  const { brief: validated, errors } = validateBrief(brief)
  assert.deepEqual(errors, [])
  assert.notEqual(validated, null)
  assert.equal(brief.title, scenario.title)
  assert.match(brief.description, /valid-full\.scenario\.json/)
  assert.deepEqual(brief.scenes, deriveScenes(scenario))
})

// ── makeScenarioEvaluator (Task 2) ──────────────────────────────────────────

test('makeScenarioEvaluator: matches structured expectations and reports missing displayTexts', () => {
  const scenario = {
    version: 1,
    title: 'Eval',
    fixture: { preset: 'demo' },
    scenes: [
      {
        id: 'scene-01',
        title: 'Scene one',
        steps: [
          {
            expect: [
              { text: 'STATUS', match: 'normalized-ci' },
              { text: '\\bREPO\\b', match: 'regex' },
              { anyOf: ['missing-a', 'missing-b'], label: 'never shown' },
            ],
          },
        ],
      },
    ],
  }
  const evaluate = makeScenarioEvaluator(scenario)
  const screens = [{ seq: 1, text: 'status: ok\nREPO list' }]
  const sceneForScreen = { 1: 'scene-01' }
  const out = evaluate({ scenes: deriveScenes(scenario), screens, sceneForScreen })
  assert.equal(out.length, 1)
  assert.deepEqual(Object.keys(out[0]).sort(), ['id', 'missing', 'passed', 'title'])
  assert.equal(out[0].id, 'scene-01')
  assert.equal(out[0].title, 'Scene one')
  assert.equal(out[0].passed, false)
  assert.deepEqual(out[0].missing, ['never shown'])
  assert.ok(out[0].missing.every((m) => typeof m === 'string'))
})

test('makeScenarioEvaluator: a fully-satisfied scene passes with no missing', () => {
  const scenario = {
    version: 1,
    title: 'EvalPass',
    fixture: { preset: 'demo' },
    scenes: [{ id: 'scene-01', title: 'Only', steps: [{ expect: ['REPO', 'STATUS'] }] }],
  }
  const evaluate = makeScenarioEvaluator(scenario)
  const out = evaluate({
    scenes: deriveScenes(scenario),
    screens: [{ seq: 1, text: 'REPO STATUS ready' }],
    sceneForScreen: { 1: 'scene-01' },
  })
  assert.equal(out[0].passed, true)
  assert.deepEqual(out[0].missing, [])
})
