#!/usr/bin/env node
/**
 * proof-judge.eval.mjs — Judge eval suite (BOS-141 D9): five scenarios that
 * exercise `judgeProof` end-to-end against realistic manifest-shaped
 * captures, including an ADVERSARIAL fixture (wrong-feature stills presented
 * as proof of a different feature) that must fail to satisfy — the core
 * anti-rubber-stamp regression test for the fresh-context judge.
 *
 * DIVERGENCE FROM proof-tui-agent.eval.mjs (documented per plan): that eval
 * gates on `ANTHROPIC_API_KEY` (proof-tui-agent.eval.mjs ~L199) because the
 * TUI driver authenticates via the SDK's implicit env lookup. The JUDGE
 * authenticates with the EXPLICIT `PROOF_ANTHROPIC_API_KEY`
 * (proof-judge.mjs `createJudgeModel`: `new Anthropic({apiKey:
 * process.env.PROOF_ANTHROPIC_API_KEY})`), so THIS eval gates on
 * `PROOF_ANTHROPIC_API_KEY` instead — matching what the judge itself reads.
 *
 * Never uploads/posts (no R2, no `gh`) — pure judging, no finalize wiring.
 * Standalone entry: `node scripts/proof-judge.eval.mjs`
 * Injectable: `import { runEval, SCENARIOS, runScenario } from './proof-judge.eval.mjs'`
 */

import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { judgeProof } from './proof-judge.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const ASSETS_DIR = path.join(repoRoot, 'scripts/assets/proof-judge-eval')

// ── Fixture briefs ──────────────────────────────────────────────────────────

/** Single-scene brief: the RIGHT feature (session rename). */
const RENAME_BRIEF = {
  title: 'proof-judge eval — rename session (complete)',
  description:
    'Rename a boss TUI session from old-session-name to RENAMED-SESSION and confirm the ' +
    'session list shows the renamed name.',
  planRequiredProof: ['The TUI session list must show the renamed session as RENAMED-SESSION'],
  targetRoutes: ['sessions'],
  stepsHints: [
    'Open the session list',
    'Rename the session old-session-name to RENAMED-SESSION',
    'Confirm the session list shows RENAMED-SESSION',
  ],
  expectedEvidence: ['RENAMED-SESSION'],
}

/**
 * Two-scene brief: scene 1 (rename) is provable from the committed stills;
 * scene 2's expected evidence is a sentinel string that can never appear in
 * ANY still (mirrors the unreachable-evidence technique in
 * proof-tui-agent.eval.mjs's NEGATIVE_BRIEF) — this keeps the "missing
 * scene" grade genuinely unprovable rather than accidentally visible in the
 * scene-01 stills, which would make the live grade unstable.
 */
const RENAME_TWO_SCENE_BRIEF = {
  title: 'proof-judge eval — rename session (two scenes, one missing)',
  description:
    'Scene 1: rename a session and confirm the list shows RENAMED-SESSION. Scene 2: confirm ' +
    'the rename was recorded in the audit log.',
  planRequiredProof: [
    'The TUI session list must show the renamed session as RENAMED-SESSION',
    'The audit log must record the rename event',
  ],
  scenes: [
    {
      id: 'scene-01',
      title: 'rename session',
      stepsHints: ['Open the session list', 'Rename old-session-name to RENAMED-SESSION'],
      expectedEvidence: ['RENAMED-SESSION'],
    },
    {
      id: 'scene-02',
      title: 'confirm audit log entry',
      stepsHints: ["Open the audit log and confirm an entry reading 'RENAME-AUDIT-9F2K'"],
      expectedEvidence: ['RENAME-AUDIT-9F2K'],
    },
  ],
}

// ── Fixture capture-shape helpers ───────────────────────────────────────────

function stillsFor(fileNames, { sceneId } = {}) {
  return fileNames.map((fileName) => ({
    fileName,
    label: fileName.replace(/\.png$/, '').replaceAll('-', ' '),
    ...(sceneId ? { sceneId } : {}),
  }))
}

function captureShape({ surface = 'tui', status = 'passed', stills = [], scenes } = {}) {
  return {
    recipeId: 'proof-judge-eval',
    title: 'proof-judge eval capture',
    surface,
    privacy: 'private',
    status,
    mediaType: 'image',
    stills,
    ...(scenes ? { scenes } : {}),
  }
}

// ── Scenario table (D9) ─────────────────────────────────────────────────────
//
// Each scenario builds real manifest-shaped inputs and (unless it overrides
// `deps`) lets `judgeProof` default to the REAL `createJudgeModel()` (live
// SDK call, PROOF_ANTHROPIC_API_KEY) and the REAL `prepareJudgeImages` (real
// ffmpeg downscale to 1024px, D10) — so scenarios 1-4 are genuinely
// end-to-end live grades, and the adversarial scenario (3) validates the
// downscale cap doesn't blind grading. Only scenario 5 injects a stub model
// (and a stub env key) to deterministically exercise the error path without
// ever making a network call.

export const SCENARIOS = [
  {
    name: 'complete',
    fixtureFiles: ['rename-session-before.png', 'rename-session-after.png'],
    retryOnRateLimit: true,
    buildInputs: () => ({
      surfaceRuns: [
        {
          surface: 'tui',
          brief: RENAME_BRIEF,
          agentResult: {
            summary:
              'Renamed session old-session-name to RENAMED-SESSION; session list confirms ' +
              'the new name.',
          },
          hasFailure: false,
          captureShapes: [
            captureShape({
              stills: stillsFor(['rename-session-before.png', 'rename-session-after.png']),
              scenes: [
                {
                  id: 'scene-01',
                  title: 'rename session',
                  passed: true,
                  missing: [],
                  outputMs: 1200,
                },
              ],
            }),
          ],
        },
      ],
      manifest: { agentRunnerStubbed: false, surfaces: [{ surface: 'tui', outcome: 'passed' }] },
    }),
    assert: (verdict) => ({
      ok: !verdict.unjudged && verdict.evidence === 'satisfactory',
      note: verdict.unjudged
        ? `unjudged: ${verdict.reason}`
        : `evidence=${verdict.evidence} confidence=${verdict.confidence}`,
    }),
  },
  {
    name: 'missing scene',
    fixtureFiles: ['rename-session-before.png', 'rename-session-after.png'],
    retryOnRateLimit: true,
    buildInputs: () => ({
      surfaceRuns: [
        {
          surface: 'tui',
          brief: RENAME_TWO_SCENE_BRIEF,
          agentResult: {
            summary:
              'Renamed session old-session-name to RENAMED-SESSION and confirmed the new ' +
              'name; the run stopped before the audit log could be checked.',
          },
          hasFailure: true,
          captureShapes: [
            captureShape({
              stills: stillsFor(['rename-session-before.png', 'rename-session-after.png'], {
                sceneId: 'scene-01',
              }),
              scenes: [
                {
                  id: 'scene-01',
                  title: 'rename session',
                  passed: true,
                  missing: [],
                  outputMs: 1200,
                },
                {
                  id: 'scene-02',
                  title: 'confirm audit log entry',
                  passed: false,
                  missing: ['RENAME-AUDIT-9F2K'],
                  outputMs: null,
                },
              ],
            }),
          ],
        },
      ],
      manifest: { agentRunnerStubbed: false, surfaces: [{ surface: 'tui', outcome: 'deferred' }] },
    }),
    assert: (verdict) => {
      if (verdict.unjudged) return { ok: false, note: `unjudged: ${verdict.reason}` }
      const scene2 = verdict.perScene?.find((s) => s?.id === 'scene-02')
      const scene2Failed = scene2 ? scene2.verdict !== 'passed' : false
      return {
        ok: verdict.evidence === 'partial' && scene2Failed,
        note: `evidence=${verdict.evidence} scene-02 verdict=${scene2?.verdict ?? '(missing)'}`,
      }
    },
  },
  {
    name: 'adversarial',
    fixtureFiles: ['settings-screen-1.png', 'settings-screen-2.png'],
    retryOnRateLimit: true,
    buildInputs: () => ({
      surfaceRuns: [
        {
          surface: 'tui',
          // Adversarial: the WRONG feature's stills presented against the
          // rename brief's required proof, with the mechanical scene gate
          // (falsely) marked passed — the mechanical clamp does NOT apply
          // here, so this scenario tests ONLY whether the judge's own
          // vision-based grading catches the mismatch (anti-rubber-stamp,
          // D9's core assertion).
          brief: RENAME_BRIEF,
          agentResult: {
            summary: 'Opened Settings and updated notification preferences.',
          },
          hasFailure: false,
          captureShapes: [
            captureShape({
              stills: stillsFor(['settings-screen-1.png', 'settings-screen-2.png']),
              scenes: [
                {
                  id: 'scene-01',
                  title: 'rename session',
                  passed: true,
                  missing: [],
                  outputMs: 900,
                },
              ],
            }),
          ],
        },
      ],
      manifest: { agentRunnerStubbed: false, surfaces: [{ surface: 'tui', outcome: 'passed' }] },
    }),
    assert: (verdict) => ({
      ok: !verdict.unjudged && verdict.evidence !== 'satisfactory',
      note: verdict.unjudged
        ? `unjudged: ${verdict.reason}`
        : `evidence=${verdict.evidence} confidence=${verdict.confidence}`,
    }),
  },
  {
    name: 'stub without disclosure',
    fixtureFiles: ['rename-session-before.png', 'rename-session-after.png'],
    retryOnRateLimit: true,
    buildInputs: () => ({
      surfaceRuns: [
        {
          surface: 'tui',
          brief: RENAME_BRIEF,
          agentResult: {
            summary:
              'Renamed session old-session-name to RENAMED-SESSION; session list confirms ' +
              'the new name (run against a stubbed agent-runner daemon).',
          },
          hasFailure: false,
          captureShapes: [
            captureShape({
              stills: stillsFor(['rename-session-before.png', 'rename-session-after.png']),
              scenes: [
                {
                  id: 'scene-01',
                  title: 'rename session',
                  passed: true,
                  missing: [],
                  outputMs: 1200,
                },
              ],
            }),
          ],
        },
      ],
      // agentRunnerStubbed:true WITHOUT the run itself disclosing it in the
      // transcript summary — clampJudgeVerdict (Task 1, pure/unit-tested) must
      // append the mandatory stub caveat deterministically; this scenario
      // proves that clamp is actually wired into the live judgeProof path.
      manifest: { agentRunnerStubbed: true, surfaces: [{ surface: 'tui', outcome: 'passed' }] },
    }),
    assert: (verdict) => {
      if (verdict.unjudged) return { ok: false, note: `unjudged: ${verdict.reason}` }
      const hasStubCaveat = (verdict.caveats ?? []).some(
        (c) => typeof c === 'string' && c.toLowerCase().includes('stub'),
      )
      // Policy (clampJudgeVerdict rule 3): 'high' confidence is only valid when
      // every scene passed AND the stub is disclosed. The stub caveat is always
      // appended (rule 2), so a violation here would mean 'high' with a failed
      // scene — confirm the live output never does that.
      const allScenesPassed = (verdict.perScene ?? []).every((s) => s?.verdict === 'passed')
      const confidencePolicyOk = verdict.confidence !== 'high' || allScenesPassed
      return {
        ok: hasStubCaveat && confidencePolicyOk,
        note: `caveats=${JSON.stringify(verdict.caveats)} confidence=${verdict.confidence}`,
      }
    },
  },
  {
    name: 'API error',
    fixtureFiles: [],
    retryOnRateLimit: false,
    buildInputs: () => ({
      surfaceRuns: [
        {
          surface: 'tui',
          brief: RENAME_BRIEF,
          agentResult: { summary: 'n/a' },
          hasFailure: false,
          captureShapes: [captureShape({ stills: [] })],
        },
      ],
      manifest: { agentRunnerStubbed: false },
      // No real key/network involved: a fake env key satisfies judgeProof's
      // missing-key short-circuit, and the injected model throws before any
      // SDK/network call would happen — createJudgeModel() is never even
      // constructed because deps.model is supplied.
      deps: {
        env: { PROOF_ANTHROPIC_API_KEY: 'fake-key-for-error-test' },
        model: {
          async createMessage() {
            throw new Error('simulated-api-error')
          },
        },
      },
    }),
    assert: (verdict) => ({
      ok: verdict.unjudged === true,
      note: verdict.unjudged
        ? `unjudged: ${verdict.reason}`
        : `NOT unjudged: ${JSON.stringify(verdict)}`,
    }),
  },
]

// ── Scenario runner ──────────────────────────────────────────────────────────

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

/** True when an unjudged reason looks like a transient rate-limit (HTTP 429). */
function looksLikeRateLimit(reason) {
  return typeof reason === 'string' && /429|rate.?limit/i.test(reason)
}

/**
 * Runs one scenario against the REAL `judgeProof` (default deps, unless the
 * scenario supplies overrides): copies its fixture PNGs into a fresh temp
 * `localDir` (so ffmpeg downscale output from `prepareJudgeImages` never
 * pollutes the committed `scripts/assets/proof-judge-eval/` fixtures), calls
 * `judgeProof`, retries transient 429s with a short backoff (serialized —
 * scenarios never run concurrently), applies the scenario's assertion, and
 * always cleans up the temp dir.
 * @param {object} scenario one entry of `SCENARIOS`
 * @returns {Promise<{name: string, ok: boolean, note: string, verdict: object}>}
 */
export async function runScenario(scenario) {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-judge-eval-'))
  try {
    for (const fileName of scenario.fixtureFiles ?? []) {
      fs.copyFileSync(path.join(ASSETS_DIR, fileName), path.join(tmpDir, fileName))
    }
    const { surfaceRuns, manifest, deps } = scenario.buildInputs()

    const delaysMs = scenario.retryOnRateLimit ? [2000, 5000] : []
    let verdict = await judgeProof({ surfaceRuns, manifest, localDir: tmpDir, deps })
    let attempt = 0
    while (verdict.unjudged && looksLikeRateLimit(verdict.reason) && attempt < delaysMs.length) {
      await sleep(delaysMs[attempt])
      attempt += 1
      verdict = await judgeProof({ surfaceRuns, manifest, localDir: tmpDir, deps })
    }

    const { ok, note } = scenario.assert(verdict)
    return { name: scenario.name, ok, note, verdict }
  } finally {
    try {
      fs.rmSync(tmpDir, { recursive: true, force: true })
    } catch {
      // ignore cleanup errors
    }
  }
}

// ── Main eval entry ──────────────────────────────────────────────────────────

/**
 * Runs the full judge eval suite. Key-gated (see divergence note above):
 * skips cleanly with `{skipped:true, ok:true}` when `PROOF_ANTHROPIC_API_KEY`
 * is unset from `env` — `runScenario` is never invoked in that case, so no
 * judge model is ever constructed. Never uploads/posts.
 * @param {object} [opts]
 * @param {object}   [opts.env=process.env]        - Environment variables (injected for tests).
 * @param {Function} [opts.runScenario=runScenario] - Per-scenario runner (injected for tests).
 * @param {Function} [opts.log=console.log]         - Log function (injected for tests).
 * @returns {Promise<{ok: boolean, skipped?: boolean, results?: Array}>}
 */
export async function runEval({
  env = process.env,
  runScenario: run = runScenario,
  log = console.log,
} = {}) {
  if (!env.PROOF_ANTHROPIC_API_KEY) {
    log('skipped: no PROOF_ANTHROPIC_API_KEY — set it to run the judge eval suite')
    return { skipped: true, ok: true }
  }

  const results = []
  for (const scenario of SCENARIOS) {
    let result
    try {
      result = await run(scenario)
    } catch (err) {
      result = { name: scenario.name, ok: false, note: `error: ${err.message}` }
    }
    const statusLabel = result.ok ? 'PASS' : 'FAIL'
    log(`[${statusLabel}] ${scenario.name}: ${result.note ?? ''}`)
    results.push(result)
  }

  const allOk = results.every((r) => r.ok)
  const passCount = results.filter((r) => r.ok).length
  log(`\nEval summary: ${passCount}/${results.length} scenarios passed`)

  return { ok: allOk, results }
}

// ── CLI entry ──────────────────────────────────────────────────────────────

import { isMainModule } from '../skills-toolbox/main-module.mjs'

const invokedDirectly = isMainModule(import.meta.url)

if (invokedDirectly) {
  runEval()
    .then(({ ok }) => process.exit(ok ? 0 : 1))
    .catch((err) => {
      console.error(err)
      process.exit(1)
    })
}
