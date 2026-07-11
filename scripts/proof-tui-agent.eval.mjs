#!/usr/bin/env node
/**
 * proof-tui-agent.eval.mjs — Navigation-quality eval for the TUI agent proof orchestrator.
 *
 * Exercises the TUI agent proof machinery against four scenarios:
 *   - positive (#812): add-repo → repo-settings flow; expects verdict='passed'.
 *   - negative: unreachable evidence sentinel; expects verdict='failed' (anti false-positive).
 *   - multi-scene ordering (D9, BOS-140): a two-scene positive brief; expects verdict='passed'
 *     AND scene-marker ordering evidence (raw/scene-timings.json strictly increasing, both
 *     entries in manifest.captures[0].scenes passed). Its briefs now drive the repo table with
 *     BOS-213 arrow keys and gate on BOS-222 matcher-shaped expectedEvidence (a normalized-vs-
 *     literal case and an anyOf case).
 *   - fallback (agent-fail → replay, BOS-223): forces the agent leg to gate-fail (unreachable
 *     sentinel) in a run that carries a committed scenario, and asserts the automatic replay
 *     fallback engages — proofSource='replay', the disclosure line in the comment body, and the
 *     replay leg's raw/scene-timings.json strictly increasing with exactly one entry per committed
 *     scene (so a dropped-scene regression can't pass on a partial artifact). It is the ONLY scenario routed
 *     through the BOS-223 surface dispatcher (the `dispatch` seam) rather than the single
 *     `runTuiAgentProof` runner the other three use, because the agent→replay fallback DECISION
 *     lives in the dispatcher (planTuiLegs/classifyTuiOutcome), not in runTuiAgentProof.
 *
 * Call order (documented here and pinned in the test file): 1=positive, 2=negative,
 * 3=multi-scene (D9) all via `runner`; 4=fallback via `dispatch`.
 *
 * Key-gated (T2): skips cleanly with exit 0 when ANTHROPIC_API_KEY is unset.
 * Always dry-run: never uploads or posts (BOSS_PROOF_UPLOAD=0 enforced for own run).
 *
 * Run standalone: node scripts/proof-tui-agent.eval.mjs
 * Injectable: import { runEval } from './proof-tui-agent.eval.mjs' and pass stubs
 * ({ runner, dispatch, buildBridge, env, log }).
 */

import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  renderConsolidatedComment,
  TUI_REPLAY_EXTRA_MS,
  tuiAgentBridgeBuildCommand,
} from './proof-lib.mjs'
import { runTuiAgentProof } from './proof-tui-agent.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

// ── Scenario briefs ───────────────────────────────────────────────────────────

/**
 * Positive scenario: drives the add-repo → repo-settings flow in DemoWorld.
 * expectedEvidence uses 'Settings' which appears on the settings screen.
 * Runner discriminated by call order: first call = positive.
 */
const POSITIVE_BRIEF = {
  title: 'TUI navigation eval — add-repo and repo-settings flow (#812)',
  description:
    'Navigate the boss TUI: open the repos list, add a repo using the Add repo dialog, ' +
    'then open Settings and confirm the settings screen is visible.',
  targetRoutes: ['repos', 'settings'],
  stepsHints: [
    'Press r to open the Repos list',
    'Press a to open Add repo and choose "Open project"',
    'Type a repo path into the path field and press Enter to confirm',
    'Observe the home screen update to include the new repo',
    'Press s to open Settings and confirm the settings screen text appears',
  ],
  // 'Settings' appears on the DemoWorld TUI settings screen; reachable in any run.
  expectedEvidence: ['Settings'],
}

/**
 * Negative scenario: unreachable evidence sentinel.
 * The T1 gate in runTuiAgentProof forces verdict='failed' when any expectedEvidence
 * substring is absent from the final settled screen. This string can never appear.
 */
const NEGATIVE_BRIEF = {
  title: 'TUI navigation eval — unreachable evidence sentinel',
  description:
    'Drives the TUI with an evidence string that can never appear on any screen. ' +
    'The eval must report failed (anti false-positive check). Navigate only with ' +
    'keystrokes — do NOT type or enter the sentinel string into any field, so it ' +
    'can never reach the rendered screen via free-text input.',
  targetRoutes: [],
  stepsHints: ['Press s to open Settings (navigation only; do not type any text)'],
  expectedEvidence: ['__UNREACHABLE_EVIDENCE_BOS71__'],
}

/**
 * Multi-scene positive scenario (D9, BOS-140): extends the add-repo →
 * repo-settings flow into two explicitly marked scenes so the eval can assert
 * scene-marker ordering end-to-end, not just a single final verdict.
 *
 *   - scene-01 "open repos list": evidence token 'PATH' is the repo-list
 *     table's column header (services/boss/internal/views/repo_list.go
 *     buildTable(): `{Title: "PATH", ...}`). DemoWorld always seeds 5 repos
 *     (services/boss/internal/fixtures/fixtures.go Repos()), so the repo
 *     table — and its PATH header — always renders once the list loads; it is
 *     also the exact token services/boss/internal/tuitest/repolist_test.go
 *     already waits on to confirm the repo-list view opened.
 *   - scene-02 "open repo settings": reuses 'Settings', the same token the
 *     positive (#812) scenario above already relies on for the settings
 *     screen.
 */
const MULTISCENE_BRIEF = {
  title: 'TUI navigation eval — scene-marker ordering (D9)',
  description:
    'Navigate the boss TUI in two explicit scenes: first open the repos list and ' +
    "arrow-navigate its table, then open a repo's settings screen. Call " +
    'begin_scene({id}) before starting each scene, in order, as instructed.',
  targetRoutes: ['repos', 'settings'],
  scenes: [
    {
      id: 'scene-01',
      title: 'open repos list',
      // BOS-213 key vocabulary: the repos list is a bubbles `table` view
      // (services/boss/internal/views/repo_list.go — m.table.Update(msg) forwards
      // arrow keys to move the cursor; services/boss/internal/tuitest/repolist_test.go
      // opens it and waits on the 'PATH' header). Driving the selection with the new
      // `down`/`up` arrow keys — instead of single-char prose only — exercises the
      // key names BOS-213 added; the flow still reaches the same grounded tokens.
      stepsHints: [
        'Press s to open the Settings hub',
        'Press r to open the Repos list and confirm the repo table is visible',
        'Press the down arrow key to move the selection down one repo row',
        'Press the up arrow key to move the selection back to the first repo row',
      ],
      // BOS-222 matcher-shaped evidence. Two grounded expectations:
      //  (a) 'PATH' — plain string (normalized default); the repo-list column header,
      //      always rendered once DemoWorld's 5 seeded repos load (never empty).
      //  (b) normalized-vs-literal FLIP case: the header ROW renders the four columns
      //      ('', NAME, PATH, STATUS) separated by table padding — multiple spaces
      //      between labels. Under the pre-BOS-222 literal `includes()` gate the
      //      single-spaced string 'NAME PATH STATUS' would FAIL (the screen has
      //      multi-space runs). The normalized matcher collapses every whitespace run
      //      to one space on BOTH sides, so it PASSES. This is exactly the false
      //      negative BOS-222 exists to fix.
      expectedEvidence: [
        'PATH',
        { text: 'NAME PATH STATUS', match: 'normalized', label: 'repo-list column headers' },
      ],
    },
    {
      id: 'scene-02',
      title: 'open repo settings',
      stepsHints: [
        "Call begin_scene({id: 'scene-02'}) to mark the start of this scene",
        'Press enter on a repo row to open its settings screen',
        'Confirm the settings screen text appears',
      ],
      // BOS-222 matcher-shaped evidence: an `anyOf` group anchored to the DemoWorld
      // settings screen. 'Settings' (the same token the positive #812 scenario relies
      // on) matches via the normalized default; the case-insensitive 'settings'
      // alternative is a belt-and-suspenders variant. The group passes if ANY
      // alternative matches.
      expectedEvidence: [
        {
          anyOf: ['Settings', { text: 'settings', match: 'normalized-ci' }],
          label: 'settings screen',
        },
      ],
    },
  ],
}

/**
 * Fallback scenario (BOS-223): forces the AGENT leg to fail its evidence gate
 * (the unreachable-sentinel technique NEGATIVE_BRIEF uses) in a run that carries a
 * committed scenario, so the automatic scenario-replay fallback engages. Unlike the
 * three navigation scenarios — which drive `runTuiAgentProof` directly — this one is
 * routed through the BOS-223 surface dispatcher (`dispatch`), because the
 * agent→replay fallback DECISION lives there (planTuiLegs/classifyTuiOutcome +
 * runTuiWithReplayFallback), not in the single runner.
 */
const FALLBACK_BRIEF = {
  title: 'TUI navigation eval — agent-fail → scenario replay fallback (BOS-223)',
  description:
    'Drives the agent leg with an evidence string that can never appear on any ' +
    'screen so the agent leg gate-fails; a committed scenario then replays as the ' +
    'automatic fallback. Navigate only with keystrokes — do NOT type or enter the ' +
    'sentinel string into any field, so it can never reach the rendered screen.',
  targetRoutes: [],
  stepsHints: ['Press s to open Settings (navigation only; do not type any text)'],
  expectedEvidence: ['__UNREACHABLE_EVIDENCE_BOS224__'],
}

// Committed scenario the replay leg consumes (BOS-219 format, DemoWorld-grounded
// tokens: PATH / NAME / STATUS / Settings). The eval feeds this path to the
// dispatcher's diff-only discovery (discoverChangedScenarios) via the changed-files
// list it controls, so the fallback run finds exactly one committed scenario.
const FALLBACK_SCENARIO_FILE = 'proof/scenarios/bos224-tui-navigation-fallback.scenario.json'

const SCENARIOS = [
  // Call order: 1 = positive, 2 = negative, 3 = multi-scene (D9) — all via `runner`;
  // 4 = fallback (BOS-223) via `dispatch`. Documented here and pinned in the test file.
  { name: 'positive (#812)', brief: POSITIVE_BRIEF, expectedVerdict: 'passed' },
  { name: 'negative', brief: NEGATIVE_BRIEF, expectedVerdict: 'failed' },
  { name: 'multi-scene ordering (D9)', brief: MULTISCENE_BRIEF, expectedVerdict: 'passed' },
  // The fallback scenario is discriminated by `viaDispatch`: the loop routes it
  // through the injectable `dispatch` seam and gates on proofSource/disclosure/
  // timings instead of a verdict. `scenarioFile` is the committed scenario the
  // replay leg consumes (fed to discoverChangedScenarios via changedFiles).
  {
    name: 'fallback (agent-fail → replay, BOS-223)',
    brief: FALLBACK_BRIEF,
    viaDispatch: true,
    scenarioFile: FALLBACK_SCENARIO_FILE,
    expectedProofSource: 'replay',
  },
]

// ── Scene-ordering assertion helpers (D9, pure) ────────────────────────────

/**
 * True when every entry's `startMs` is strictly greater than the previous
 * entry's — i.e. scenes began in order with no ties or regressions. Fewer
 * than 2 entries vacuously pass (nothing to compare). Non-array input fails
 * closed. Pure.
 * @param {Array<{startMs: number}>} sceneTimings
 * @returns {boolean}
 */
export function sceneTimingsStrictlyIncreasing(sceneTimings) {
  if (!Array.isArray(sceneTimings)) return false
  for (let i = 1; i < sceneTimings.length; i++) {
    if (!(sceneTimings[i].startMs > sceneTimings[i - 1].startMs)) return false
  }
  return true
}

/**
 * True when `scenes` is a non-empty array and every entry passed its
 * per-scene evidence gate (the shape `evaluateSceneEvidence` /
 * `captureShape.scenes` produce: `{ id, title, passed, missing }`). An empty
 * or non-array input fails closed (no scenes is not "all scenes passed").
 * Pure.
 * @param {Array<{passed: boolean}>} scenes
 * @returns {boolean}
 */
export function allScenesPassed(scenes) {
  return Array.isArray(scenes) && scenes.length > 0 && scenes.every((s) => s?.passed === true)
}

/**
 * Scene count of a committed replay scenario file — the number of
 * `raw/scene-timings.json` entries a well-formed replay of it must emit. The
 * fallback gate compares the artifact's length against this so a regression that
 * drops a scene's timing (e.g. only `scene-01` survives while `scene-02` stops
 * emitting) fails the eval instead of passing on a non-empty-but-incomplete
 * artifact. Reads `scenes.length` from the committed JSON so it tracks the file
 * automatically. Returns 0 on any read/parse error or a missing/non-array
 * `scenes`, which the caller treats as a closed-fail (0 can never equal a
 * real timing count). Impure only in the single file read.
 * @param {string} scenarioFile - repo-relative path to the committed scenario JSON.
 * @returns {number}
 */
export function committedScenarioSceneCount(scenarioFile) {
  try {
    const parsed = JSON.parse(fs.readFileSync(path.resolve(repoRoot, scenarioFile), 'utf8'))
    return Array.isArray(parsed.scenes) ? parsed.scenes.length : 0
  } catch {
    return 0
  }
}

/**
 * Per-scenario teardown shared by both loop branches (verdict runner + dispatch):
 * restore BOSS_PROOF_BRIEF to its pre-run value and remove the run's temp dirs.
 * Both branches must keep this in lockstep, so it lives in one place rather than
 * being copied into each `finally`. `savedBrief === undefined` means the var was
 * unset before the run, so we delete rather than restore. Falsy dirs are skipped
 * (e.g. a single-scene run has no sceneRunDir); cleanup errors are swallowed.
 * @param {string|undefined} savedBrief
 * @param {Array<string|null|undefined>} dirs
 */
function restoreBriefAndCleanup(savedBrief, dirs) {
  if (savedBrief === undefined) {
    delete process.env.BOSS_PROOF_BRIEF
  } else {
    process.env.BOSS_PROOF_BRIEF = savedBrief
  }
  for (const dir of dirs) {
    if (!dir) continue
    try {
      fs.rmSync(dir, { recursive: true, force: true })
    } catch {
      // ignore cleanup errors
    }
  }
}

// ── Default bridge builder ─────────────────────────────────────────────────────

/**
 * Build the BOS-69 proof-tui-agent Go bridge binary into a temp path and return
 * the binary path. Injected via the `buildBridge` seam so unit tests never spawn
 * a Go build.
 */
export async function defaultBuildBridge() {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-agent-eval-'))
  const outBin = path.join(tmpDir, 'proof-tui-agent-bridge')
  const [cmd, args, opts] = tuiAgentBridgeBuildCommand({ outBin })
  const result = spawnSync(cmd, args, {
    cwd: path.resolve(repoRoot, opts.cwd ?? '.'),
    stdio: 'inherit',
  })
  if (result.status !== 0) {
    throw new Error(`TUI bridge build failed (go exited ${result.status})`)
  }
  return outBin
}

// ── Default fallback dispatcher (BOS-223) ──────────────────────────────────────

/**
 * Default `dispatch` seam for the fallback scenario. Drives the REAL BOS-223
 * agent-first→replay routing (`runTuiWithReplayFallback`) with the live replay
 * seams, then renders the consolidated comment body via the real renderer so the
 * eval can assert the disclosure line end-to-end. Injected as a stub in the no-key
 * structural tests; only exercised for real in a key-gated run.
 *
 * The agent leg reads the sentinel brief from BOSS_PROOF_BRIEF (set by runEval,
 * exactly as the runner path does) and gate-fails; the replay leg loads the
 * committed scenario discovered in `changedFiles` and replays it, so the classified
 * outcome is `proofSource:'replay'`. The replay leg persists raw/scene-timings.json
 * into `runContext.localDir`, which runEval reads back for the strictly-increasing
 * ordering gate.
 *
 * @param {object} opts
 * @param {string}   opts.localDir     - Known run dir (runContext.localDir seam).
 * @param {string[]} opts.changedFiles - Diff list carrying the committed scenario path.
 * @param {boolean}  opts.dryRun       - Always true for the eval.
 * @returns {Promise<{proofSource: string|null, fallbackReason: string|null, commentBody: string}>}
 */
export async function defaultDispatch({ localDir, changedFiles, dryRun }) {
  const { runTuiAgentProof: runLeg, runTuiWithReplayFallback } =
    await import('./proof-tui-agent.mjs')
  const { runReplayLoop, synthesizeBrief, makeScenarioEvaluator } =
    await import('./proof-tui-replay.mjs')
  const { loadScenario } = await import('./proof-scenario.mjs')

  // Give both legs generous headroom; runTuiWithReplayFallback splits this slice
  // into an agent cap + the replay reserve. Exact size only matters for real runs
  // (this path never executes in the no-key structural tests, which stub dispatch).
  const slice = TUI_REPLAY_EXTRA_MS * 3
  const surfaceRun = await runTuiWithReplayFallback(
    {
      prNumber: 'eval',
      commit: 'eval',
      changedFiles,
      dryRun,
      runContext: { localDir, maxWallClockMs: slice },
    },
    {
      runTuiAgentProof: runLeg,
      agentUsable: true,
      runReplayLoop,
      synthesizeBrief,
      makeScenarioEvaluator,
      loadScenario,
      replayReserveMs: TUI_REPLAY_EXTRA_MS,
    },
  )

  // Render the consolidated comment body through the REAL renderer so the
  // disclosure line ("… scenario replay shown") is produced exactly as a posted
  // proof comment would render it, rather than hand-assembled here.
  const section = {
    outcome: surfaceRun.hasFailure ? 'failed' : 'passed',
    label: 'TUI',
    reasonCode: surfaceRun.reasonCode ?? null,
    proofSource: surfaceRun.proofSource ?? null,
    fallbackReason: surfaceRun.fallbackReason ?? null,
    summary: '',
    captures: [],
  }
  const commentBody = renderConsolidatedComment({
    marker: '<!-- proof-tui-agent-eval -->',
    manifest: { title: FALLBACK_BRIEF.title, commit: 'eval', runId: 'eval' },
    sections: [section],
  })

  return {
    proofSource: surfaceRun.proofSource ?? null,
    fallbackReason: surfaceRun.fallbackReason ?? null,
    commentBody,
  }
}

/**
 * The exact BOS-223 disclosure substring the fallback comment must carry. Pinned
 * here (kept in sync with proof-lib.mjs `surfaceSectionLines`) so both the eval's
 * live gate and the structural tests assert against one source of truth.
 */
export const FALLBACK_DISCLOSURE_SUBSTRING = 'scenario replay shown'

// ── Main eval entry ────────────────────────────────────────────────────────────

/**
 * Run the navigation-quality eval.
 *
 * @param {object} [opts]
 * @param {object}   [opts.env=process.env]       - Environment variables (injected for tests).
 * @param {Function} [opts.runner=runTuiAgentProof] - Proof runner (injected for tests).
 * @param {Function} [opts.dispatch=defaultDispatch] - BOS-223 surface dispatcher for the
 *   fallback scenario (injected as a stub for tests). Only the fallback scenario
 *   (viaDispatch:true) uses it; the three navigation scenarios stay on `runner`.
 * @param {Function} [opts.buildBridge]            - Bridge builder (injected for tests).
 * @param {Function} [opts.log=console.log]        - Log function (injected for tests).
 * @returns {Promise<{ok: boolean, skipped?: boolean, results?: Array}>}
 */
export async function runEval({
  env = process.env,
  runner = runTuiAgentProof,
  dispatch = defaultDispatch,
  buildBridge = defaultBuildBridge,
  log = console.log,
} = {}) {
  // T2: Key gate — skip without ANTHROPIC_API_KEY.
  if (!env.ANTHROPIC_API_KEY) {
    log('skipped: no ANTHROPIC_API_KEY — set it to run the TUI navigation eval')
    return { skipped: true, ok: true }
  }

  // Snapshot env we mutate BEFORE the try so the finally restores it even when
  // the bridge build below throws.
  const savedUpload = process.env.BOSS_PROOF_UPLOAD
  const savedBridgeBin = process.env.BOSS_PROOF_TUI_BRIDGE_BIN
  // Temp dir we created for a freshly-built bridge (cleaned up in finally). Null
  // when the caller supplied BOSS_PROOF_TUI_BRIDGE_BIN — we never delete theirs.
  let builtBridgeDir = null

  try {
    // Enforce dryRun semantics: ensure upload is off for this eval's process.
    process.env.BOSS_PROOF_UPLOAD = '0'

    // Bridge: use provided bin or build one via the Go build helper.
    let bridgeBin = env.BOSS_PROOF_TUI_BRIDGE_BIN
    if (!bridgeBin) {
      log('Building TUI bridge binary...')
      bridgeBin = await buildBridge()
      builtBridgeDir = path.dirname(bridgeBin)
      log(`Bridge built: ${bridgeBin}`)
    }
    process.env.BOSS_PROOF_TUI_BRIDGE_BIN = bridgeBin

    const results = []

    for (const scenario of SCENARIOS) {
      // Write brief to a temp file and point BOSS_PROOF_BRIEF at it for the run.
      const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-eval-brief-'))
      const briefPath = path.join(tmpDir, 'brief.json')
      fs.writeFileSync(briefPath, JSON.stringify(scenario.brief, null, 2))

      const savedBrief = process.env.BOSS_PROOF_BRIEF
      process.env.BOSS_PROOF_BRIEF = briefPath

      // BOS-223 fallback scenario: routed through the `dispatch` seam (the agent→
      // replay fallback DECISION lives in the dispatcher, not runTuiAgentProof).
      // Gated on proofSource='replay' + the disclosure line in the comment body +
      // the replay leg's scene-timings: one strictly-increasing entry per committed
      // scene (exact count, not merely non-empty) — not a verdict.
      if (scenario.viaDispatch) {
        const dispatchRunDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-eval-dispatch-'))
        let ok = false
        let proofSource = null
        let dispatchNote = null
        let errorMsg = null
        try {
          const { proofSource: source, commentBody } = await dispatch({
            scenario,
            brief: scenario.brief,
            briefPath,
            scenarioFile: scenario.scenarioFile,
            localDir: dispatchRunDir,
            changedFiles: [scenario.scenarioFile],
            env,
            dryRun: true,
            log,
          })
          proofSource = source ?? null
          const sourceOk = proofSource === (scenario.expectedProofSource ?? 'replay')
          const disclosureOk = String(commentBody ?? '').includes(FALLBACK_DISCLOSURE_SUBSTRING)
          const timingsPath = path.join(dispatchRunDir, 'raw', 'scene-timings.json')
          const timings = fs.existsSync(timingsPath)
            ? JSON.parse(fs.readFileSync(timingsPath, 'utf8'))
            : null
          // Require exactly one marker per committed scene, not just ≥1: an
          // empty array passes sceneTimingsStrictlyIncreasing([]) vacuously, and
          // a partial array (e.g. only scene-01 because a regression dropped
          // scene-02's timing emission) is still strictly increasing yet no
          // longer proves ordered replay timings for the FULL scenario. Compare
          // against the committed replay scenario's scene count so the gate stays
          // in lockstep with the file the replay leg actually consumes — the same
          // exact-length discipline the D9 gate applies below. The `>= 1` guard
          // fails closed if the scenario file is unreadable (count 0).
          const expectedSceneCount = committedScenarioSceneCount(scenario.scenarioFile)
          const timingsOk =
            Array.isArray(timings) &&
            expectedSceneCount >= 1 &&
            timings.length === expectedSceneCount &&
            sceneTimingsStrictlyIncreasing(timings)
          ok = sourceOk && disclosureOk && timingsOk
          if (!ok) {
            const timingCount = Array.isArray(timings) ? timings.length : 'none'
            dispatchNote =
              `sourceOk=${sourceOk} disclosureOk=${disclosureOk} timingsOk=${timingsOk} ` +
              `(scene-timings=${timingCount}, expected=${expectedSceneCount})`
          }
        } catch (err) {
          errorMsg = err.message
          ok = false
        } finally {
          restoreBriefAndCleanup(savedBrief, [tmpDir, dispatchRunDir])
        }

        const statusLabel = ok ? 'PASS' : 'FAIL'
        const detail = errorMsg
          ? `error: ${errorMsg}`
          : dispatchNote
            ? `expected proofSource=${scenario.expectedProofSource} actual=${proofSource}; ${dispatchNote}`
            : `expected proofSource=${scenario.expectedProofSource} actual=${proofSource}`
        log(`[${statusLabel}] ${scenario.name}: ${detail}`)
        results.push({
          name: scenario.name,
          expectedProofSource: scenario.expectedProofSource ?? 'replay',
          proofSource,
          ok,
        })
        continue
      }

      // D9: a multi-scene brief additionally asserts scene-marker ordering.
      // Give the run its own known localDir (via runContext) so
      // raw/scene-timings.json can be read back afterwards — runTuiAgentProof
      // never returns its localDir, so this is the only seam that lets the
      // eval find the artifact without guessing at a random run id/token.
      const isMultiScene = (scenario.brief.scenes?.length ?? 0) > 1
      const sceneRunDir = isMultiScene
        ? fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-eval-scenes-'))
        : null

      let verdict = 'error'
      let ok = false
      let errorMsg = null
      let sceneNote = null

      try {
        const runnerOpts = {
          prNumber: 'eval',
          commit: 'eval',
          changedFiles: [],
          dryRun: true,
        }
        if (sceneRunDir) runnerOpts.runContext = { localDir: sceneRunDir }

        const { manifest } = await runner(runnerOpts)
        verdict = manifest.verdict
        ok = verdict === scenario.expectedVerdict

        // Only enforced when the returned manifest actually carries a
        // `captures` array — real runTuiAgentProof runs always do, but the
        // generic verdict-only stubs the other scenarios/tests use do not, so
        // this stays a no-op for them (verdict match alone still governs ok).
        if (ok && isMultiScene && manifest.captures) {
          const scenesOk = allScenesPassed(manifest.captures[0]?.scenes)
          const timingsPath = path.join(sceneRunDir, 'raw', 'scene-timings.json')
          const timings = fs.existsSync(timingsPath)
            ? JSON.parse(fs.readFileSync(timingsPath, 'utf8'))
            : null
          const timingsOk =
            Array.isArray(timings) &&
            timings.length === scenario.brief.scenes.length &&
            sceneTimingsStrictlyIncreasing(timings)
          ok = scenesOk && timingsOk
          if (!ok) {
            sceneNote = `scene-order gate failed (scenesOk=${scenesOk} timingsOk=${timingsOk})`
          }
        }
      } catch (err) {
        errorMsg = err.message
        ok = false
      } finally {
        // Restore BOSS_PROOF_BRIEF before cleanup to avoid any ordering issues.
        restoreBriefAndCleanup(savedBrief, [tmpDir, sceneRunDir])
      }

      const statusLabel = ok ? 'PASS' : 'FAIL'
      const verdictInfo = errorMsg
        ? `error: ${errorMsg}`
        : sceneNote
          ? `expected=${scenario.expectedVerdict} actual=${verdict}; ${sceneNote}`
          : `expected=${scenario.expectedVerdict} actual=${verdict}`
      log(`[${statusLabel}] ${scenario.name}: ${verdictInfo}`)
      results.push({ name: scenario.name, expectedVerdict: scenario.expectedVerdict, verdict, ok })
    }

    const allOk = results.every((r) => r.ok)
    const passCount = results.filter((r) => r.ok).length
    log(`\nEval summary: ${passCount}/${results.length} scenarios passed`)

    return { ok: allOk, results }
  } finally {
    // Restore mutated env vars.
    if (savedUpload === undefined) {
      delete process.env.BOSS_PROOF_UPLOAD
    } else {
      process.env.BOSS_PROOF_UPLOAD = savedUpload
    }
    if (savedBridgeBin === undefined) {
      delete process.env.BOSS_PROOF_TUI_BRIDGE_BIN
    } else {
      process.env.BOSS_PROOF_TUI_BRIDGE_BIN = savedBridgeBin
    }
    // Remove a bridge dir we built (never one the caller supplied).
    if (builtBridgeDir) {
      try {
        fs.rmSync(builtBridgeDir, { recursive: true, force: true })
      } catch {
        // ignore cleanup errors
      }
    }
  }
}

// ── CLI entry ──────────────────────────────────────────────────────────────────

const invokedDirectly =
  process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)

if (invokedDirectly) {
  runEval()
    .then(({ ok }) => process.exit(ok ? 0 : 1))
    .catch((err) => {
      console.error(err)
      process.exit(1)
    })
}
