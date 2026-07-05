#!/usr/bin/env node
/**
 * proof-tui-agent.eval.mjs — Navigation-quality eval for the TUI agent proof orchestrator.
 *
 * Exercises runTuiAgentProof against three scenarios:
 *   - positive (#812): add-repo → repo-settings flow; expects verdict='passed'.
 *   - negative: unreachable evidence sentinel; expects verdict='failed' (anti false-positive).
 *   - multi-scene ordering (D9, BOS-140): a two-scene positive brief; expects verdict='passed'
 *     AND scene-marker ordering evidence (raw/scene-timings.json strictly increasing, both
 *     entries in manifest.captures[0].scenes passed).
 *
 * Key-gated (T2): skips cleanly with exit 0 when ANTHROPIC_API_KEY is unset.
 * Always dry-run: never uploads or posts (BOSS_PROOF_UPLOAD=0 enforced for own run).
 *
 * Run standalone: node scripts/proof-tui-agent.eval.mjs
 * Injectable: import { runEval } from './proof-tui-agent.eval.mjs' and pass stubs.
 */

import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { tuiAgentBridgeBuildCommand } from './proof-lib.mjs'
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
    'Navigate the boss TUI in two explicit scenes: first open the repos list, ' +
    "then open a repo's settings screen. Call begin_scene({id}) before starting " +
    'each scene, in order, as instructed.',
  targetRoutes: ['repos', 'settings'],
  scenes: [
    {
      id: 'scene-01',
      title: 'open repos list',
      stepsHints: [
        'Press s to open the Settings hub',
        'Press r to open the Repos list and confirm the repo table is visible',
      ],
      // 'PATH' is the repo-list table's column header; always rendered once
      // DemoWorld's repos are loaded (5 seeded repos, never empty).
      expectedEvidence: ['PATH'],
    },
    {
      id: 'scene-02',
      title: 'open repo settings',
      stepsHints: [
        "Call begin_scene({id: 'scene-02'}) to mark the start of this scene",
        'Press enter on a repo row to open its settings screen',
        'Confirm the settings screen text appears',
      ],
      // 'Settings' appears on the DemoWorld TUI settings screen; reachable in any run.
      expectedEvidence: ['Settings'],
    },
  ],
}

const SCENARIOS = [
  // Call order: first = positive, second = negative, third = multi-scene (D9).
  // Documented here and in test file.
  { name: 'positive (#812)', brief: POSITIVE_BRIEF, expectedVerdict: 'passed' },
  { name: 'negative', brief: NEGATIVE_BRIEF, expectedVerdict: 'failed' },
  { name: 'multi-scene ordering (D9)', brief: MULTISCENE_BRIEF, expectedVerdict: 'passed' },
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

// ── Main eval entry ────────────────────────────────────────────────────────────

/**
 * Run the navigation-quality eval.
 *
 * @param {object} [opts]
 * @param {object}   [opts.env=process.env]       - Environment variables (injected for tests).
 * @param {Function} [opts.runner=runTuiAgentProof] - Proof runner (injected for tests).
 * @param {Function} [opts.buildBridge]            - Bridge builder (injected for tests).
 * @param {Function} [opts.log=console.log]        - Log function (injected for tests).
 * @returns {Promise<{ok: boolean, skipped?: boolean, results?: Array}>}
 */
export async function runEval({
  env = process.env,
  runner = runTuiAgentProof,
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
        if (savedBrief === undefined) {
          delete process.env.BOSS_PROOF_BRIEF
        } else {
          process.env.BOSS_PROOF_BRIEF = savedBrief
        }
        try {
          fs.rmSync(tmpDir, { recursive: true, force: true })
        } catch {
          // ignore cleanup errors
        }
        if (sceneRunDir) {
          try {
            fs.rmSync(sceneRunDir, { recursive: true, force: true })
          } catch {
            // ignore cleanup errors
          }
        }
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
