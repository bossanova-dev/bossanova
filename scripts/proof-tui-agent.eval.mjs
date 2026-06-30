#!/usr/bin/env node
/**
 * proof-tui-agent.eval.mjs — Navigation-quality eval for the TUI agent proof orchestrator.
 *
 * Exercises runTuiAgentProof against two scenarios:
 *   - positive (#812): add-repo → repo-settings flow; expects verdict='passed'.
 *   - negative: unreachable evidence sentinel; expects verdict='failed' (anti false-positive).
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

const SCENARIOS = [
  // Call order: first = positive, second = negative. Documented here and in test file.
  { name: 'positive (#812)', brief: POSITIVE_BRIEF, expectedVerdict: 'passed' },
  { name: 'negative', brief: NEGATIVE_BRIEF, expectedVerdict: 'failed' },
]

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

      let verdict = 'error'
      let ok = false
      let errorMsg = null

      try {
        const { manifest } = await runner({
          prNumber: 'eval',
          commit: 'eval',
          changedFiles: [],
          dryRun: true,
        })
        verdict = manifest.verdict
        ok = verdict === scenario.expectedVerdict
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
      }

      const statusLabel = ok ? 'PASS' : 'FAIL'
      const verdictInfo = errorMsg
        ? `error: ${errorMsg}`
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
