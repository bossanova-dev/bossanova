/**
 * proof-tui-replay.mjs — Deterministic scenario replay engine (BOS-219).
 *
 * Replays a committed BOS-218 scenario (`loadScenario` output) over the existing
 * NDJSON bridge to produce a real TUI proof with ZERO LLM involvement. It is a
 * drop-in `loopRunner` for `runTuiAgentProof` (proof-tui-agent.mjs): `runReplayLoop`
 * returns the exact `runAgentLoop` return contract so the entire capture tail
 * (video, stills, chapters, finalize) is reused byte-for-byte.
 *
 * Three exports:
 *   - runReplayLoop({scenario, bridge, rawDir, readCastMs?, maxWallClockMs?,
 *       fileBasename?}) → {agentResult, finalScreen, captionTimings, sceneTimings,
 *       sceneForScreen, screens}  — same keys as runAgentLoop's contract.
 *   - synthesizeBrief(scenario, fileBasename) → a validateBrief-passing brief so
 *       the `brief` seam skips resolveBrief (and the Anthropic SDK) entirely (D4).
 *   - makeScenarioEvaluator(scenario) → the evaluateSceneEvidence-shaped gate that
 *       matches the scenario's STRUCTURED expectations (regex/anyOf/normalized-ci)
 *       and emits per-scene {id,title,passed,missing:string[]} (missing = displayText).
 *
 * Bridge op shape consumed (BOS-69 + BOS-217): observe()/key(name)/type(text)/
 * enter()/esc()/wait(ms)/daemon(obj) each → {screen}. `key:"enter"|"esc"` map to
 * the dedicated enter()/esc() ops; every other key → key(name). A daemon step
 * against a bridge lacking a daemon op fails loud (naming BOS-217).
 */

import fs from 'node:fs'
import path from 'node:path'

import { deriveScenes } from './proof-scenario.mjs'
import {
  displayText,
  evaluateExpectations,
  matchExpectation,
  normalizeExpectation,
} from './proof-evidence-matcher.mjs'
import { parseCastTailMs } from './proof-tui-agent.mjs'

// Default wall-clock cap mirrors TUI_BUDGETS.maxWallClockMs in proof-tui-agent.mjs
// (kept local to avoid a config import cycle). Overridable per call.
const DEFAULT_MAX_WALL_CLOCK_MS = 4 * 60 * 1000
// Default waitFor poll ceiling; matches the scenario schema's waitFor default.
const DEFAULT_WAIT_FOR_TIMEOUT_MS = 10000

/** Read the cast tail time (ms) from the live `.cast`; null when absent. */
function readCastTailMs(castPath) {
  try {
    return parseCastTailMs(fs.readFileSync(castPath, 'utf8'))
  } catch {
    return null
  }
}

/**
 * Client-side waitFor poll: observe → matchExpectation (matcher layer 1) → repeat
 * until a hit or the timeout. In production the bridge's ~350ms settle paces each
 * observe; the loop is bounded by wall-clock `timeoutMs`. On timeout throws an
 * Error carrying `.lastText` (the final settled screen) for debuggability.
 * @returns {Promise<string>} the settled screen that satisfied the expectation
 */
async function waitForExpectation(bridge, rawExpect, timeoutMs, pointer) {
  const expectation = normalizeExpectation(rawExpect, `${pointer}.waitFor`)
  const started = Date.now()
  let lastText = ''
  for (;;) {
    // eslint-disable-next-line no-await-in-loop -- polling is inherently sequential
    const r = await bridge.observe()
    lastText = r?.screen ?? ''
    if (matchExpectation(lastText, expectation)) return lastText
    if (Date.now() - started >= timeoutMs) {
      const err = new Error(
        `waitFor timed out after ${timeoutMs}ms waiting for ${displayText(expectation)}`,
      )
      err.lastText = lastText
      throw err
    }
  }
}

/** Loud failure for a daemon step the bridge can't service (old binary). Fires
 * both when the JS bridge lacks a daemon() method AND when a present daemon() is
 * rejected by an OLD Go bridge with `unknown op "daemon"` (BOS-217 not built in). */
function daemonDepError() {
  return new Error(
    'daemon step requires the BOS-217 bridge daemon op; the current proof-tui-agent ' +
      'bridge does not support it — rebuild the Go bridge (BOS-217)',
  )
}

/**
 * Execute one bridge-touching (or wait) step and return its settled screen.
 * `expect` steps are handled by the caller (they touch no bridge). Throws on a
 * bridge crash, a waitFor timeout, or a daemon step against an old bridge.
 * `waitMs` is a CLIENT-SIDE fixed dwell (the Go `wait` op is wait-FOR-TEXT, wrong
 * semantics), then one observe captures the settled frame; `sleep` is injectable
 * so tests stay instant.
 * @returns {Promise<string>}
 */
async function runBridgeStep(bridge, step, pointer, sleep) {
  if ('waitFor' in step) {
    return waitForExpectation(
      bridge,
      step.waitFor,
      step.timeoutMs ?? DEFAULT_WAIT_FOR_TIMEOUT_MS,
      pointer,
    )
  }
  if ('waitMs' in step) {
    await sleep(step.waitMs)
    return (await bridge.observe())?.screen ?? ''
  }
  if ('key' in step) {
    const k = step.key
    if (k === 'enter') return (await bridge.enter())?.screen ?? ''
    if (k === 'esc') return (await bridge.esc())?.screen ?? ''
    return (await bridge.key(k))?.screen ?? ''
  }
  if ('type' in step) {
    return (await bridge.type(step.type))?.screen ?? ''
  }
  if ('daemon' in step) {
    if (typeof bridge.daemon !== 'function') throw daemonDepError()
    try {
      return (await bridge.daemon(step.daemon))?.screen ?? ''
    } catch (err) {
      // An OLD Go bridge answers `unknown op "daemon"`; makeStdioBridge's request()
      // surfaces that as a thrown Error. Map it to the same loud BOS-217 failure.
      if (/unknown op/i.test(err?.message ?? '')) {
        const e = daemonDepError()
        e.cause = err
        throw e
      }
      throw err
    }
  }
  // Unreachable for a validated scenario (validateStep guarantees one known op).
  throw new Error(`unknown step op at ${pointer}`)
}

/**
 * Replay a validated scenario over an injected bridge. Never throws: a bridge
 * crash, waitFor timeout, budget overrun, or evidence miss is folded into a
 * `passed:false` agentResult whose summary names the failing step/scene, so the
 * capture tail still runs (stills/degraded-marker) exactly as the agent path.
 *
 * @param {{ scenario: object, bridge: object, rawDir: string,
 *   readCastMs?: () => (number|null), maxWallClockMs?: number,
 *   fileBasename?: string, sleep?: (ms:number) => Promise<void> }} opts
 * @returns {Promise<{ agentResult: object, finalScreen: string,
 *   captionTimings: Array, sceneTimings: Array, sceneForScreen: object,
 *   screens: Array }>}
 */
export async function runReplayLoop({
  scenario,
  bridge,
  rawDir,
  readCastMs = () => readCastTailMs(path.join(rawDir, 'session.cast')),
  maxWallClockMs = DEFAULT_MAX_WALL_CLOCK_MS,
  fileBasename = 'scenario',
  sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms)),
}) {
  const derived = deriveScenes(scenario)
  const scenes = Array.isArray(scenario?.scenes) ? scenario.scenes : []

  const screens = []
  const sceneForScreen = {}
  const captionTimings = []
  const sceneTimings = []
  let finalScreen = ''
  let screenN = 0
  let stepsRun = 0
  const started = Date.now()

  let thrownError = null
  let failingPointer = null
  const sceneFailures = []

  outer: for (let si = 0; si < scenes.length; si += 1) {
    const scene = scenes[si]
    const sceneId = derived[si]?.id ?? `scene-${String(si + 1).padStart(2, '0')}`
    const sceneTitle = derived[si]?.title ?? String(scene.title ?? `scene ${si + 1}`)
    // Scene 1 is anchored at 0 (matching runAgentLoop); later scenes read the
    // live cast clock when they begin.
    const sceneStartMs = si === 0 ? 0 : readCastMs()
    sceneTimings.push({ id: sceneId, title: sceneTitle, startMs: sceneStartMs })

    const sceneScreens = []
    const sceneExpectations = []
    const steps = Array.isArray(scene.steps) ? scene.steps : []

    for (let sj = 0; sj < steps.length; sj += 1) {
      const step = steps[sj]
      const pointer = `scenes[${si}].steps[${sj}]`
      if (Date.now() - started > maxWallClockMs) {
        thrownError = new Error(`exceeded wall-clock budget of ${maxWallClockMs}ms`)
        failingPointer = pointer
        break outer
      }
      stepsRun += 1

      // `expect` steps assert evidence at scene end; they touch no bridge.
      if ('expect' in step) {
        const list = Array.isArray(step.expect) ? step.expect : [step.expect]
        for (const e of list) sceneExpectations.push(normalizeExpectation(e, `${pointer}.expect`))
        continue
      }

      let screen
      try {
        // eslint-disable-next-line no-await-in-loop -- steps replay sequentially
        screen = await runBridgeStep(bridge, step, pointer, sleep)
      } catch (err) {
        thrownError = err
        failingPointer = pointer
        if (err.lastText) finalScreen = err.lastText
        break outer
      }

      finalScreen = screen
      screenN += 1
      const seq = String(screenN).padStart(2, '0')
      fs.writeFileSync(path.join(rawDir, `screen-${seq}.txt`), screen)
      fs.writeFileSync(path.join(rawDir, `caption-${seq}.txt`), step.caption ?? '')
      const castMs = readCastMs()
      screens.push({ seq: screenN, text: screen, castMs })
      sceneForScreen[screenN] = sceneId
      sceneScreens.push(screen)
      if (step.caption)
        captionTimings.push({ seq: screenN, caption: step.caption, startMs: castMs })
    }

    // Scene-end evidence gate over this scene's settled screens.
    const { missing } = evaluateExpectations({
      expectations: sceneExpectations,
      texts: sceneScreens,
    })
    if (missing.length) {
      sceneFailures.push({ id: sceneId, missing: missing.map((m) => m.displayText) })
    }
  }

  const base = `deterministic scenario replay of ${fileBasename}`
  let passed
  let summary
  if (thrownError) {
    passed = false
    summary = failingPointer
      ? `${base}: failed at ${failingPointer}: ${thrownError.message}`
      : `${base}: ${thrownError.message}`
  } else if (sceneFailures.length) {
    passed = false
    summary = `${base}: ${sceneFailures
      .map((f) => `scene ${f.id} missing ${f.missing.join(', ')}`)
      .join('; ')}`
  } else {
    passed = true
    summary = base
  }

  return {
    agentResult: { passed, summary, evidence: [], steps: stepsRun },
    finalScreen,
    captionTimings,
    sceneTimings,
    sceneForScreen,
    screens,
  }
}

/**
 * Synthesize a validateBrief-passing brief from a scenario (D4). Skipping
 * resolveBrief with this pre-resolved brief makes replay structurally free of any
 * Anthropic SDK call. `scenes` is the derived (displayText) scene list; the
 * structured expectations used for matching live in `makeScenarioEvaluator`.
 * @param {object} scenario validated scenario (loadScenario/validateScenario output)
 * @param {string} fileBasename scenario file basename, echoed in the description
 */
export function synthesizeBrief(scenario, fileBasename) {
  return {
    title: scenario.title,
    description: `Deterministic replay of proof scenario ${fileBasename}`,
    scenes: deriveScenes(scenario),
  }
}

/**
 * Build the evidence gate for a scenario. Same call/return signature as
 * `evaluateSceneEvidence` ({scenes, screens, sceneForScreen} →
 * Array<{id,title,passed,missing:string[]}>) so runTuiAgentProof's `.map` chapter
 * decoration works unchanged. It CLOSES OVER the scenario's canonical structured
 * expectations (regex/anyOf/normalized-ci survive) rather than the display
 * strings; `missing` entries are displayText strings for the comment/gallery.
 * The `scenes` argument is accepted for signature parity but ignored — the
 * per-scene expectations come from the closed-over scenario.
 * @param {object} scenario validated scenario
 */
export function makeScenarioEvaluator(scenario) {
  const derived = deriveScenes(scenario)
  const scenes = Array.isArray(scenario?.scenes) ? scenario.scenes : []
  const specs = scenes.map((scene, i) => {
    const expectations = []
    for (const step of Array.isArray(scene.steps) ? scene.steps : []) {
      if (!('expect' in step)) continue
      const list = Array.isArray(step.expect) ? step.expect : [step.expect]
      for (const e of list) expectations.push(normalizeExpectation(e))
    }
    return {
      id: derived[i]?.id ?? `scene-${String(i + 1).padStart(2, '0')}`,
      title: derived[i]?.title ?? String(scene.title ?? `scene ${i + 1}`),
      expectations,
    }
  })
  return ({ screens = [], sceneForScreen = {} } = {}) =>
    specs.map((spec) => {
      const texts = screens.filter((s) => sceneForScreen[s.seq] === spec.id).map((s) => s.text)
      const { missing } = evaluateExpectations({ expectations: spec.expectations, texts })
      return {
        id: spec.id,
        title: spec.title,
        passed: missing.length === 0,
        missing: missing.map((m) => m.displayText),
      }
    })
}
