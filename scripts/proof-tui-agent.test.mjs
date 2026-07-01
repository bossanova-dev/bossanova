/**
 * proof-tui-agent.test.mjs — Unit tests for the TUI agent proof orchestrator.
 *
 * Strategy (mirrors proof-agent.test.mjs): drive the REAL runTuiAgentProof with
 * a fully stubbed dependency seam — a scripted NDJSON bridge, a scripted
 * Anthropic-shaped model, an injectable still renderer, and an injectable
 * cast→video converter. NOTHING here spawns the Go bridge, agg, ffmpeg, or hits
 * the Anthropic API / network. The Go `proof-tui-agent` bridge binary does not
 * exist on main yet (BOS-69 is in review) and there is no ANTHROPIC_API_KEY in
 * CI, so every test runs on stubs only.
 *
 * Required cases (from the Task 2 brief):
 *   1. brief → loop → frames → manifest (happy path)
 *   2. secret-scan abort (planted AWS-shaped key in done summary)
 *   3. budget stop (maxSteps / wall-clock / tokens variants)
 *   4. zero-frame fallback (no stills captured → exactly one fallback still)
 *   5. cast→video stubbed (mp4 present) AND null (stills-only still succeeds)
 *   6. T1 evidence gate (present → passed; absent → failed)
 */

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'

const REPO_ROOT = path.resolve(path.dirname(new URL(import.meta.url).pathname), '..')

// ── Test doubles ──────────────────────────────────────────────────────────────

/** Build an Anthropic-shaped tool_use response. */
function toolUse(name, input, { text = '', id, usage } = {}) {
  return {
    stop_reason: 'tool_use',
    usage: usage ?? { input_tokens: 10, output_tokens: 5 },
    content: [
      ...(text ? [{ type: 'text', text }] : []),
      { type: 'tool_use', id: id ?? `tu-${name}`, name, input },
    ],
  }
}

/**
 * Scripted model: returns the next queued response per createMessage call.
 * When `repeatLast` is set, keeps returning the final response forever (used to
 * drive the budget-stop tests where the model never calls done).
 */
function scriptedModel(responses, { repeatLast = false } = {}) {
  let i = 0
  const model = {
    calls: 0,
    async createMessage() {
      model.calls += 1
      const idx = repeatLast ? Math.min(i, responses.length - 1) : i
      const resp = responses[Math.min(idx, responses.length - 1)]
      if (i < responses.length - 1) i += 1
      return resp
    },
  }
  return model
}

/**
 * Scripted NDJSON bridge stub. `screens` is consumed one per bridge op; the last
 * screen repeats once exhausted. Tracks whether quit() was sent.
 */
function scriptedBridge({ screens = ['HOME SCREEN'] } = {}) {
  let i = 0
  const next = () => {
    const s = screens[Math.min(i, screens.length - 1)]
    i += 1
    return s
  }
  const bridge = {
    quitCalled: false,
    observeCount: 0,
    async observe() {
      bridge.observeCount += 1
      return { screen: next() }
    },
    async sendKeys() {
      return { screen: next() }
    },
    async typeText() {
      return { screen: next() }
    },
    async quit() {
      bridge.quitCalled = true
    },
  }
  return bridge
}

/** Still renderer stub: writes a fake PNG at the requested output path. */
function fakeRenderStill() {
  const calls = []
  const fn = async ({ input, output, title }) => {
    calls.push({ input, output, title })
    fs.writeFileSync(output, 'fake-png-bytes')
  }
  fn.calls = calls
  return fn
}

async function withTempBrief(brief, fn) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-brief-'))
  const briefPath = path.join(dir, 'brief.json')
  fs.writeFileSync(briefPath, JSON.stringify(brief, null, 2))
  try {
    return await fn(briefPath)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
}

function withEnv(overrides, fn) {
  const keys = [
    'BOSS_PROOF_BRIEF',
    'BOSS_PROOF_UPLOAD',
    'BOSS_PROOF_RUN_ID',
    'BOSS_PROOF_RUN_TOKEN',
    'BOSS_PROOF_R2_BUCKET',
    'BOSS_PROOF_PUBLIC_BASE_URL',
    'BOSS_PROOF_MODEL',
    'BOSS_PROOF_AGENT_TIMEOUT_MS',
  ]
  const saved = {}
  for (const k of keys) saved[k] = process.env[k]
  for (const [k, v] of Object.entries(overrides)) {
    if (v === undefined) delete process.env[k]
    else process.env[k] = v
  }
  return Promise.resolve()
    .then(fn)
    .finally(() => {
      for (const k of keys) {
        if (saved[k] === undefined) delete process.env[k]
        else process.env[k] = saved[k]
      }
    })
}

function cleanupPr(prNumber) {
  fs.rmSync(path.join(REPO_ROOT, '.proof', `pr-${prNumber}`), { recursive: true, force: true })
}

const BASE_ENV = (over = {}) => ({
  BOSS_PROOF_UPLOAD: '0',
  BOSS_PROOF_RUN_TOKEN: 'tok-tui-test',
  BOSS_PROOF_PUBLIC_BASE_URL: 'https://proof.test.dev',
  ...over,
})

// ── Anthropic SDK loading ────────────────────────────────────────────────────

test('SYSTEM_PROMPT: instructs the agent to finish all steps before done()', async () => {
  const mod = await import('./proof-tui-agent.mjs')
  assert.match(mod.SYSTEM_PROMPT, /complete every/i)
  assert.match(mod.SYSTEM_PROMPT, /before calling done/i)
  assert.match(mod.SYSTEM_PROMPT, /done\(passed=true\)/i)
  assert.match(mod.SYSTEM_PROMPT, /ALL.*expected-evidence strings/i)
  assert.match(mod.SYSTEM_PROMPT, /INJECTION GUARD/i)
  assert.match(mod.SYSTEM_PROMPT, /done\(passed=false\).*broken.*cannot complete/i)
})

test('SYSTEM_PROMPT: requires a narration sentence alongside each action (caption source)', async () => {
  const mod = await import('./proof-tui-agent.mjs')
  // The TUI caption is sourced from the model's per-turn text block, so the
  // prompt must require narration or tool-only turns leave frames uncaptioned.
  assert.match(mod.SYSTEM_PROMPT, /CAPTION/)
  assert.match(mod.SYSTEM_PROMPT, /same message as each observe\/send_keys\/type_text/i)
  assert.match(mod.SYSTEM_PROMPT, /never call those tools without a leading narration/i)
})

test('loadAnthropic: missing SDK rejects with install hint', async () => {
  const { loadAnthropic } = await import('./proof-tui-agent.mjs')

  await assert.rejects(
    loadAnthropic(async () => {
      throw { code: 'ERR_MODULE_NOT_FOUND' }
    }),
    /pnpm install.*@anthropic-ai\/sdk/,
  )
})

test('loadAnthropic: returns default export function', async () => {
  const { loadAnthropic } = await import('./proof-tui-agent.mjs')

  function AnthropicStub() {}

  assert.equal(await loadAnthropic(async () => ({ default: AnthropicStub })), AnthropicStub)
})

test('runTuiAgentProof: generated brief missing SDK rejects with install hint', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')

  try {
    await withEnv(
      BASE_ENV({
        BOSS_PROOF_BRIEF: undefined,
        BOSS_PROOF_RUN_ID: 'tui-generated-brief-sdk-missing',
      }),
      async () => {
        await assert.rejects(
          runTuiAgentProof({
            prNumber: 'tuimissingbriefsdk',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              generateBriefFromDiff: async () => {
                throw { code: 'ERR_MODULE_NOT_FOUND' }
              },
            },
          }),
          /pnpm install.*@anthropic-ai\/sdk/,
        )
      },
    )
  } finally {
    cleanupPr('tuimissingbriefsdk')
  }
})

// ── 1. brief → loop → frames → manifest (happy path) ─────────────────────────

test('runTuiAgentProof: brief → loop → frames → manifest (happy path)', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'Open settings',
    description: 'Demonstrates the settings screen opens',
    expectedEvidence: ['Settings'],
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-happy' }),
        async () => {
          const renderStill = fakeRenderStill()
          const bridge = scriptedBridge({ screens: ['Settings panel open', 'Settings saved'] })
          const model = scriptedModel([
            toolUse('observe', {}),
            toolUse('send_keys', { keys: ['s'] }),
            toolUse('done', { summary: 'Opened settings successfully', passed: true }),
          ])

          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuihappy',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: { bridge, model, renderStill, castToVideo: async () => null },
          })

          assert.equal(manifest.verdict, 'passed')
          assert.equal(manifest.agentRunnerStubbed, true)
          const cap = manifest.captures[0]
          assert.equal(cap.surface, 'tui')
          assert.equal(cap.status, 'passed')
          assert.ok(Array.isArray(cap.stills) && cap.stills.length >= 1, 'frames must be present')
          assert.ok(bridge.quitCalled, 'bridge.quit() must always be sent')
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuihappy')
  }
})

// ── Regression: agent run must not abort on its own ISO-runId public URL ──────

test('runTuiAgentProof: ISO runId does not trip the manifest secret scan', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const brief = {
    title: 'Open settings',
    description: 'settings opens',
    expectedEvidence: ['Settings'],
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: '2026-06-26T02-16-36-825Z' }),
        async () => {
          const { manifest } = await runTuiAgentProof({
            prNumber: 'isorun',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              bridge: scriptedBridge({ screens: ['Settings panel open'] }),
              model: scriptedModel([
                toolUse('send_keys', { keys: ['s'] }),
                toolUse('done', { summary: 'Opened settings', passed: true }),
              ]),
              renderStill: fakeRenderStill(),
              castToVideo: async () => null,
            },
          })

          assert.equal(manifest.verdict, 'passed')
        },
      ),
    )
  } finally {
    cleanupPr('isorun')
  }
})

// ── 2. budget stops ───────────────────────────────────────────────────────────

test('runTuiAgentProof: model never calls done → halts at maxSteps, not passed', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'Runaway',
    description: 'Agent loops forever',
    budgets: { maxSteps: 3 },
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-maxsteps' }),
        async () => {
          const model = scriptedModel([toolUse('observe', {})], { repeatLast: true })
          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuimaxsteps',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              bridge: scriptedBridge({ screens: ['Home'] }),
              model,
              renderStill: fakeRenderStill(),
              castToVideo: async () => null,
            },
          })
          assert.equal(manifest.verdict, 'failed')
          assert.equal(manifest.captures[0].status, 'failed')
          assert.ok(
            model.calls <= 3,
            `model must not be called more than maxSteps (was ${model.calls})`,
          )
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuimaxsteps')
  }
})

test('runTuiAgentProof: token budget halts the loop with a non-passed result', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'Token burner',
    description: 'Agent burns the token budget',
    budgets: { maxSteps: 50, maxTokens: 10 },
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-tokens' }),
        async () => {
          // Each step reports usage that exceeds the 10-token budget, so the loop
          // must stop after the first step.
          const model = scriptedModel(
            [toolUse('observe', {}, { usage: { input_tokens: 80, output_tokens: 40 } })],
            { repeatLast: true },
          )
          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuitokens',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              bridge: scriptedBridge({ screens: ['Home'] }),
              model,
              renderStill: fakeRenderStill(),
              castToVideo: async () => null,
            },
          })
          assert.equal(manifest.verdict, 'failed')
          assert.ok(
            model.calls <= 2,
            `token budget must stop the loop early (calls=${model.calls})`,
          )
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuitokens')
  }
})

test('runTuiAgentProof: wall-clock budget halts the loop with a non-passed result', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'Slowpoke',
    description: 'Agent exceeds the wall-clock budget',
    budgets: { maxSteps: 50, maxWallClockMs: -1 },
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-wallclock' }),
        async () => {
          const model = scriptedModel([toolUse('observe', {})], { repeatLast: true })
          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuiwallclock',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              bridge: scriptedBridge({ screens: ['Home'] }),
              model,
              renderStill: fakeRenderStill(),
              castToVideo: async () => null,
            },
          })
          assert.equal(manifest.verdict, 'failed')
          assert.equal(
            model.calls,
            0,
            'a negative wall-clock budget must stop before any model call',
          )
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuiwallclock')
  }
})

// ── 4. zero-frame fallback ────────────────────────────────────────────────────

test('runTuiAgentProof: no stills captured → exactly one fallback still', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'No screenshots',
    description: 'Agent calls done immediately',
    expectedEvidence: ['Home'],
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-zeroframe' }),
        async () => {
          const renderStill = fakeRenderStill()
          const bridge = scriptedBridge({ screens: ['Home dashboard'] })
          // Model calls done immediately — no observe/send_keys/type_text → 0 screens.
          const model = scriptedModel([
            toolUse('done', { summary: 'Nothing to capture', passed: true }),
          ])
          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuizeroframe',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: { bridge, model, renderStill, castToVideo: async () => null },
          })
          const cap = manifest.captures[0]
          assert.ok(Array.isArray(cap.stills), 'capture must have a stills array')
          assert.equal(cap.stills.length, 1, 'exactly one fallback still expected')
          assert.ok(bridge.observeCount >= 1, 'fallback must observe once for the still')
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuizeroframe')
  }
})

// ── 4b. planRequiredProof soft steering ───────────────────────────────────────

test('runTuiAgentProof: planRequiredProof steers the goal but never fails the hard evidence gate', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  // Long plan prose that never appears verbatim on a TUI screen — the failure
  // mode this fix prevents (feeding such prose into the hard substring gate).
  const planProse = 'Test output — updated scripts/ suite green; this never renders on a TUI screen'
  const brief = {
    title: 'Plan-steered proof',
    description: 'Demonstrate the change',
    expectedEvidence: ['Ready'], // short on-screen token (model-derived)
    planRequiredProof: [planProse], // soft steering only
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-plansteer' }),
        async () => {
          let capturedGoal = null
          const responses = [
            toolUse('observe', {}),
            toolUse('done', { summary: 'done', passed: true }),
          ]
          let i = 0
          // Capturing model: records the goal (first user message) it receives.
          const model = {
            async createMessage({ messages }) {
              if (capturedGoal === null) capturedGoal = messages[0].content
              const resp = responses[Math.min(i, responses.length - 1)]
              if (i < responses.length - 1) i += 1
              return resp
            },
          }
          // The settled screen shows the expectedEvidence token but NOT the plan prose.
          const bridge = scriptedBridge({ screens: ['Ready to go'] })
          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuiplansteer',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: { bridge, model, renderStill: fakeRenderStill(), castToVideo: async () => null },
          })

          // 1. Plan proof reaches the agent goal as SOFT guidance.
          assert.ok(
            capturedGoal.includes('steer your run to demonstrate it'),
            'goal must carry the plan-proof steering line',
          )
          assert.ok(capturedGoal.includes(planProse), 'goal must include the plan prose verbatim')

          // 2. The HARD evidence gate reads expectedEvidence only: the run passes
          //    even though the plan prose never appears on the final screen.
          const cap = manifest.captures[0]
          assert.equal(cap.status, 'passed', 'plan-prose absence must not fail the gate')
          assert.ok(
            !(cap.error && cap.error.includes('evidence gate failed')),
            'no evidence-gate failure from the unmatched plan prose',
          )
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuiplansteer')
  }
})

// ── 5. cast→video stubbed ─────────────────────────────────────────────────────

test('runTuiAgentProof: castToVideo returns mp4 → capture mediaType mp4', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'With video',
    description: 'Produces a video',
    expectedEvidence: ['Ready'],
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-video' }),
        async () => {
          const ghDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-gh-'))
          const ghPath = path.join(ghDir, 'gh')
          fs.writeFileSync(
            ghPath,
            [
              '#!/usr/bin/env node',
              'const args = process.argv.slice(2);',
              "if (args.join(' ') === 'repo view --json name -q .name') {",
              "  console.log('bossanova');",
              '  process.exit(0);',
              '}',
              "if (args[0] === 'pr' && args[1] === 'view' && args[2] === '123') {",
              '  console.log(JSON.stringify({ number: 123, title: "Targeted PR title" }));',
              '  process.exit(0);',
              '}',
              'process.exit(2);',
            ].join('\n'),
          )
          fs.chmodSync(ghPath, 0o755)
          const originalPath = process.env.PATH
          process.env.PATH = `${ghDir}${path.delimiter}${originalPath ?? ''}`
          let castArgs = null
          const castToVideo = async (args) => {
            castArgs = args
            const { captureDir, captureId } = args
            const mp4Path = path.join(captureDir, `${captureId}.mp4`)
            const posterPath = path.join(captureDir, `${captureId}.png`)
            fs.writeFileSync(mp4Path, 'fake-mp4')
            fs.writeFileSync(posterPath, 'fake-poster')
            return { mp4Path, posterPath }
          }
          try {
            const { manifest } = await runTuiAgentProof({
              prNumber: '123',
              commit: 'abc1234',
              changedFiles: [],
              dryRun: true,
              deps: {
                bridge: scriptedBridge({ screens: ['Ready to go'] }),
                model: scriptedModel([
                  toolUse('observe', {}),
                  toolUse('done', { summary: 'done', passed: true }),
                ]),
                renderStill: fakeRenderStill(),
                castToVideo,
              },
            })
            const cap = manifest.captures[0]
            assert.equal(cap.mediaType, 'mp4')
            assert.equal(cap.status, 'passed')
            assert.ok(cap.videoUrl, 'video capture must expose a videoUrl')
            assert.equal(castArgs.posterBasePng, path.join(castArgs.captureDir, 'frame-01.png'))
            assert.equal(castArgs.keepWebm, true)
            assert.equal(castArgs.label, 'bossanova#123')
            assert.equal(castArgs.cardTitle, 'Targeted PR title')
          } finally {
            if (originalPath === undefined) delete process.env.PATH
            else process.env.PATH = originalPath
            fs.rmSync(ghDir, { recursive: true, force: true })
          }
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('123')
  }
})

test('runTuiAgentProof: castToVideo returns null → stills-only run still succeeds', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'Stills only',
    description: 'No agg available',
    expectedEvidence: ['Ready'],
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-stills' }),
        async () => {
          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuistills',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              bridge: scriptedBridge({ screens: ['Ready to go'] }),
              model: scriptedModel([
                toolUse('observe', {}),
                toolUse('done', { summary: 'done', passed: true }),
              ]),
              renderStill: fakeRenderStill(),
              castToVideo: async () => null, // agg missing
            },
          })
          const cap = manifest.captures[0]
          assert.notEqual(cap.status, 'failed', 'missing video alone must not fail the run')
          assert.equal(manifest.verdict, 'passed')
          assert.notEqual(cap.mediaType, 'mp4')
          assert.ok(cap.stills.length >= 1, 'stills-only capture must still carry stills')
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuistills')
  }
})

test('defaultCastToVideo: returns null (stills-only) when the cast file is absent', async () => {
  const { defaultCastToVideo } = await import('./proof-tui-agent.mjs')
  const warnings = []
  const originalWarn = console.warn
  console.warn = (message) => warnings.push(String(message))
  try {
    const out = defaultCastToVideo({
      castPath: '/nonexistent/session.cast',
      captureDir: '/tmp',
      captureId: 'tui-agent',
    })
    assert.equal(out, null)
  } finally {
    console.warn = originalWarn
  }
  assert.ok(
    warnings.some((message) => /cast/i.test(message) && /stills-only/i.test(message)),
    'missing cast should warn that the proof is falling back to stills-only',
  )
})

// ── 6. T1 evidence gate ───────────────────────────────────────────────────────

test('runTuiAgentProof: T1 gate — evidence present + done(passed) → verdict passed', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'Gate pass',
    description: 'Evidence present',
    expectedEvidence: ['Session created'],
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-gate-pass' }),
        async () => {
          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuigatepass',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              bridge: scriptedBridge({ screens: ['Session created OK'] }),
              model: scriptedModel([
                toolUse('observe', {}),
                toolUse('done', { summary: 'created', passed: true }),
              ]),
              renderStill: fakeRenderStill(),
              castToVideo: async () => null,
            },
          })
          assert.equal(manifest.verdict, 'passed')
          assert.equal(manifest.captures[0].status, 'passed')
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuigatepass')
  }
})

// ── 7. makeStdioBridge unit tests (real pipes, fake binary) ──────────────────

test('bridgeSpawnArgs: includes only cast path when bossBin is absent', async () => {
  const { bridgeSpawnArgs } = await import('./proof-tui-agent.mjs')
  assert.deepEqual(bridgeSpawnArgs({ castPath: '/tmp/session.cast' }), [
    '--cast',
    '/tmp/session.cast',
  ])
})

test('bridgeSpawnArgs: forwards boss binary when bossBin is set', async () => {
  const { bridgeSpawnArgs } = await import('./proof-tui-agent.mjs')
  assert.deepEqual(bridgeSpawnArgs({ castPath: '/tmp/session.cast', bossBin: '/tmp/boss-e2e' }), [
    '--cast',
    '/tmp/session.cast',
    '--boss-bin',
    '/tmp/boss-e2e',
  ])
})

// Write a tiny fake-bridge executable once; all bridge tests share it.
// It reads NDJSON lines from stdin, responds with { id, ok, screen }, and exits
// on op=quit. Setting FAKE_BRIDGE_EXIT_AFTER=N causes it to exit (code 0) after
// N successful responses, without responding to the N+1-th request.
const _fakeBridgeDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-bridge-'))
const FAKE_BRIDGE_BIN = path.join(_fakeBridgeDir, 'fake-bridge.js')
fs.writeFileSync(
  FAKE_BRIDGE_BIN,
  [
    '#!/usr/bin/env node',
    "'use strict';",
    "const readline = require('node:readline');",
    'const rl = readline.createInterface({ input: process.stdin, terminal: false });',
    'const exitAfter = process.env.FAKE_BRIDGE_EXIT_AFTER !== undefined',
    '  ? Number(process.env.FAKE_BRIDGE_EXIT_AFTER) : Infinity;',
    "const baseScreen = process.env.FAKE_BRIDGE_SCREEN || 'FAKE SCREEN';",
    'let count = 0;',
    "rl.on('line', (line) => {",
    '  const t = line.trim();',
    '  if (!t) return;',
    '  let msg;',
    '  try { msg = JSON.parse(t); } catch { return; }',
    '  if (count >= exitAfter) { process.exit(0); }',
    '  count++;',
    "  process.stdout.write(JSON.stringify({ id: msg.id, ok: true, screen: baseScreen + ' ' + count }) + '\\n');",
    "  if (msg.op === 'quit') { process.exit(0); }",
    '});',
  ].join('\n'),
)
fs.chmodSync(FAKE_BRIDGE_BIN, 0o755)
process.on('exit', () => {
  try {
    fs.rmSync(_fakeBridgeDir, { recursive: true, force: true })
  } catch {
    // ignore cleanup errors
  }
})

/** Save/restore env keys around a bridge test. */
function withBridgeEnv(overrides, fn) {
  const KEYS = ['BOSS_PROOF_TUI_BRIDGE_BIN', 'FAKE_BRIDGE_EXIT_AFTER', 'FAKE_BRIDGE_SCREEN']
  const saved = {}
  for (const k of KEYS) saved[k] = process.env[k]
  for (const [k, v] of Object.entries(overrides)) {
    if (v === undefined) delete process.env[k]
    else process.env[k] = String(v)
  }
  return Promise.resolve()
    .then(fn)
    .finally(() => {
      for (const k of KEYS) {
        if (saved[k] === undefined) delete process.env[k]
        else process.env[k] = saved[k]
      }
    })
}

/** Create throw-away localDir + rawDir under os.tmpdir(). Returns { localDir, rawDir, cleanup }. */
function makeBridgeDirs(label) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), `bridge-${label}-`))
  const localDir = path.join(root, 'local')
  const rawDir = path.join(root, 'raw')
  fs.mkdirSync(localDir, { recursive: true })
  fs.mkdirSync(rawDir, { recursive: true })
  return { localDir, rawDir, cleanup: () => fs.rmSync(root, { recursive: true, force: true }) }
}

test('makeStdioBridge: observe/sendKeys/typeText round-trip returns expected screen', async () => {
  const { makeStdioBridge } = await import('./proof-tui-agent.mjs')
  const { localDir, rawDir, cleanup } = makeBridgeDirs('roundtrip')
  try {
    await withBridgeEnv({ BOSS_PROOF_TUI_BRIDGE_BIN: FAKE_BRIDGE_BIN }, async () => {
      const bridge = makeStdioBridge({ localDir, rawDir })
      try {
        const r1 = await bridge.observe()
        const r2 = await bridge.sendKeys(['s'])
        const r3 = await bridge.typeText('hello')

        assert.ok(
          typeof r1.screen === 'string' && r1.screen.includes('FAKE SCREEN'),
          'observe returns screen',
        )
        assert.ok(
          typeof r2.screen === 'string' && r2.screen.includes('FAKE SCREEN'),
          'sendKeys returns screen',
        )
        assert.ok(
          typeof r3.screen === 'string' && r3.screen.includes('FAKE SCREEN'),
          'typeText returns screen',
        )
        // Responses arrive in order: count increments 1, 2, 3
        assert.ok(r1.screen !== r2.screen, 'screens must differ (monotonic counter)')
        assert.ok(r2.screen !== r3.screen, 'screens must differ (monotonic counter)')
      } finally {
        try {
          await bridge.quit()
        } catch {
          // best-effort
        }
      }
    })
  } finally {
    cleanup()
  }
})

test('makeStdioBridge: multiple requests complete in order with correct id dispatch', async () => {
  const { makeStdioBridge } = await import('./proof-tui-agent.mjs')
  const { localDir, rawDir, cleanup } = makeBridgeDirs('ordering')
  try {
    await withBridgeEnv({ BOSS_PROOF_TUI_BRIDGE_BIN: FAKE_BRIDGE_BIN }, async () => {
      const bridge = makeStdioBridge({ localDir, rawDir })
      try {
        // Fire 4 requests sequentially (bridge serialises anyway); each must
        // resolve with a distinct screen, proving id-based dispatch is correct.
        const screens = []
        for (let i = 0; i < 4; i++) {
          // eslint-disable-next-line no-await-in-loop
          const r = await bridge.observe()
          screens.push(r.screen)
        }
        assert.equal(screens.length, 4)
        // All screens contain the fixture marker
        assert.ok(
          screens.every((s) => s.includes('FAKE SCREEN')),
          'all responses must carry a screen',
        )
        // Each response is unique (counter increments per request)
        const unique = new Set(screens)
        assert.equal(unique.size, 4, 'each response must be distinct (monotonic ids)')
      } finally {
        try {
          await bridge.quit()
        } catch {
          // best-effort
        }
      }
    })
  } finally {
    cleanup()
  }
})

test('makeStdioBridge: bridge exit/closed-pipe fails pending waiters promptly (not after 30s)', async () => {
  const { makeStdioBridge } = await import('./proof-tui-agent.mjs')
  const { localDir, rawDir, cleanup } = makeBridgeDirs('crash')
  try {
    await withBridgeEnv(
      { BOSS_PROOF_TUI_BRIDGE_BIN: FAKE_BRIDGE_BIN, FAKE_BRIDGE_EXIT_AFTER: '0' },
      async () => {
        const bridge = makeStdioBridge({ localDir, rawDir })

        // First request: bridge exits before responding → must reject promptly
        await assert.rejects(bridge.observe(), (err) => {
          assert.match(err.message, /bridge/i, 'error must identify the bridge failure')
          return true
        })

        // Second request: crashed is latched → also rejects immediately
        await assert.rejects(bridge.observe(), /bridge/i)
      },
    )
  } finally {
    cleanup()
  }
})

test('makeStdioBridge: missing binary throws bridge-not-found error synchronously', async () => {
  const { makeStdioBridge } = await import('./proof-tui-agent.mjs')
  const { localDir, rawDir, cleanup } = makeBridgeDirs('nobin')
  try {
    await withBridgeEnv({ BOSS_PROOF_TUI_BRIDGE_BIN: undefined }, () => {
      assert.throws(
        () => makeStdioBridge({ localDir, rawDir }),
        /bridge binary not found/i,
        'constructor must throw immediately when binary is absent',
      )
    })
  } finally {
    cleanup()
  }
})

test('runTuiAgentProof: T1 gate — evidence ABSENT + done(passed) → verdict failed', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'Gate fail',
    description: 'Evidence missing',
    expectedEvidence: ['Session created'],
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-gate-fail' }),
        async () => {
          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuigatefail',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            fallbackRecipeCaptures: () => [
              {
                recipeId: 'tui-home',
                title: 'Boss TUI Home',
                surface: 'tui',
                privacy: 'fixture',
                status: 'passed',
                mediaType: 'png',
                fileName: 'tui-home/tui-home.png',
              },
            ],
            deps: {
              // Screen never shows the expected evidence substring.
              bridge: scriptedBridge({ screens: ['Some other screen'] }),
              model: scriptedModel([
                toolUse('observe', {}),
                toolUse('done', { summary: 'claims success', passed: true }),
              ]),
              renderStill: fakeRenderStill(),
              castToVideo: async () => null,
            },
          })
          assert.equal(
            manifest.verdict,
            'failed',
            'model done(passed=true) must be overridden when evidence is absent',
          )
          assert.equal(manifest.captures[0].status, 'failed')
          assert.match(manifest.captures[0].error, /evidence/i)
          assert.equal(
            manifest.captures.some((c) => c.recipeId === 'tui-home' && c.status === 'passed'),
            true,
            'degraded TUI agent run must include recipe-floor capture evidence',
          )
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuigatefail')
  }
})

test('runTuiAgentProof: no recipe floor — degraded agent defers with only agent captures (Q1)', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  const brief = {
    title: 'No floor',
    description: 'Evidence missing, no recipe fallback',
    expectedEvidence: ['Session created'],
  }

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-no-floor' }),
        async () => {
          // No fallbackRecipeCaptures: the TUI surface never falls back to recipe
          // media. A degraded agent run yields only the agent's own capture(s)
          // and defers to a neutral comment.
          const { manifest } = await runTuiAgentProof({
            prNumber: 'tuinofloor',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              bridge: scriptedBridge({ screens: ['Some other screen'] }),
              model: scriptedModel([
                toolUse('observe', {}),
                toolUse('done', { summary: 'claims success', passed: true }),
              ]),
              renderStill: fakeRenderStill(),
              castToVideo: async () => null,
            },
          })
          assert.equal(manifest.verdict, 'failed', 'absent evidence overrides done(passed=true)')
          assert.equal(
            manifest.deferred,
            true,
            'a degraded TUI run defers to a neutral comment (no gallery)',
          )
          assert.equal(
            manifest.captures.some((c) => c.recipeId === 'tui-home'),
            false,
            'TUI surface must NOT include any recipe-floor capture',
          )
          assert.equal(
            manifest.captures.every((c) => c.surface === 'tui' && c.recipeId === 'tui-agent'),
            true,
            'manifest carries only the agent’s own captures, no recipe floor',
          )
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('tuinofloor')
  }
})

// ── Task 3: TUI SDK abort guard ───────────────────────────────────────────────

test('createSdkModel: aborted SDK call yields degraded result (not a throw)', async () => {
  const { createSdkModel } = await import('./proof-tui-agent.mjs')
  const origTimeout = process.env.BOSS_PROOF_AGENT_TIMEOUT_MS
  // Abort after 50ms so the test is fast.
  process.env.BOSS_PROOF_AGENT_TIMEOUT_MS = '50'

  try {
    // Mock Anthropic client whose create() hangs forever but respects the abort signal.
    class HangingAnthropic {
      constructor() {
        this.messages = {
          // `signal` is passed as the second (request-options) arg, mirroring the
          // real SDK contract — not as a body param.
          create: (_body, { signal } = {}) =>
            new Promise((_, reject) => {
              if (signal?.aborted) {
                reject(new Error('aborted'))
                return
              }
              signal?.addEventListener('abort', () => reject(new Error('aborted')))
            }),
        }
      }
    }

    const model = createSdkModel({ importer: async () => ({ default: HangingAnthropic }) })
    const result = await model.createMessage({
      model: 'claude-sonnet-4-6',
      system: 'test',
      tools: [],
      messages: [{ role: 'user', content: 'hi' }],
      maxTokens: 100,
    })

    assert.equal(result.stop_reason, 'end_turn', 'aborted call must return degraded stop_reason')
    assert.deepEqual(result.content, [], 'aborted call must return empty content')
  } finally {
    if (origTimeout === undefined) delete process.env.BOSS_PROOF_AGENT_TIMEOUT_MS
    else process.env.BOSS_PROOF_AGENT_TIMEOUT_MS = origTimeout
  }
})

test('createSdkModel: forwards PROOF_ANTHROPIC_API_KEY and passes signal as a request option', async () => {
  const { createSdkModel } = await import('./proof-tui-agent.mjs')
  const origKey = process.env.PROOF_ANTHROPIC_API_KEY
  process.env.PROOF_ANTHROPIC_API_KEY = 'sk-proof-test-key'

  try {
    let ctorOpts = null
    let createArgs = null
    class RecordingAnthropic {
      constructor(opts) {
        ctorOpts = opts
        this.messages = {
          create: (body, options) => {
            createArgs = { body, options }
            return Promise.resolve({
              stop_reason: 'end_turn',
              content: [],
              usage: { input_tokens: 1, output_tokens: 1 },
            })
          },
        }
      }
    }

    const model = createSdkModel({ importer: async () => ({ default: RecordingAnthropic }) })
    await model.createMessage({
      model: 'claude-sonnet-4-6',
      system: 'test',
      tools: [],
      messages: [{ role: 'user', content: 'hi' }],
      maxTokens: 100,
    })

    // Bug 1: the proof key must reach the SDK constructor, not the implicit lookup.
    assert.equal(ctorOpts?.apiKey, 'sk-proof-test-key', 'apiKey must be forwarded to the client')
    // Bug 2: signal belongs in the request-options arg, never in the body.
    assert.ok(createArgs, 'create() must have been called')
    assert.equal(createArgs.body.signal, undefined, 'signal must not appear in the request body')
    assert.ok(createArgs.options?.signal instanceof AbortSignal, 'signal must be a request option')
  } finally {
    if (origKey === undefined) delete process.env.PROOF_ANTHROPIC_API_KEY
    else process.env.PROOF_ANTHROPIC_API_KEY = origKey
  }
})

test('buildRenderArgs forwards the caption to proof-render-terminal', async () => {
  const { buildRenderArgs } = await import('./proof-tui-agent.mjs')
  const args = buildRenderArgs({
    outPath: '/tmp/f.png',
    title: 'boss',
    caption: 'Opening cron list',
  })
  assert.ok(args.includes('--caption'))
  assert.ok(args.includes('Opening cron list'))
})

test('buildRenderArgs omits --caption when narration is empty', async () => {
  const { buildRenderArgs } = await import('./proof-tui-agent.mjs')
  const args = buildRenderArgs({ outPath: '/tmp/f.png', title: 'boss', caption: '' })
  assert.ok(!args.includes('--caption'))
})

test('renderFrames reads the sidecar caption-NN.txt and forwards it to renderStill', async () => {
  const { renderFrames } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  const captureDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-cap-'))
  try {
    fs.writeFileSync(path.join(rawDir, 'screen-01.txt'), 'a TUI screen')
    fs.writeFileSync(path.join(rawDir, 'caption-01.txt'), 'Opening cron list')
    const seen = []
    const renderStill = async ({ output, caption }) => {
      seen.push({ caption })
      fs.writeFileSync(output, 'fake-png')
    }
    await renderFrames({ rawDir, captureDir, title: 'boss', renderStill })
    assert.equal(seen.length, 1)
    assert.equal(seen[0].caption, 'Opening cron list')
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
    fs.rmSync(captureDir, { recursive: true, force: true })
  }
})

test('renderFrames batches all frames through renderStills in one call (single browser)', async () => {
  const { renderFrames } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  const captureDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-cap-'))
  try {
    fs.writeFileSync(path.join(rawDir, 'screen-01.txt'), 'screen one')
    fs.writeFileSync(path.join(rawDir, 'caption-01.txt'), 'Home')
    fs.writeFileSync(path.join(rawDir, 'screen-02.txt'), 'screen two')
    let batchCalls = 0
    let jobsSeen = null
    // One batch call for ALL frames — not one call per frame.
    const renderStills = async (jobs) => {
      batchCalls += 1
      jobsSeen = jobs
      for (const j of jobs) fs.writeFileSync(j.output, 'fake-png')
    }
    const stills = await renderFrames({ rawDir, captureDir, title: 'boss', renderStills })
    assert.equal(batchCalls, 1, 'all frames render in a single batch invocation')
    assert.equal(jobsSeen.length, 2)
    assert.equal(jobsSeen[0].caption, 'Home')
    assert.equal(jobsSeen[1].caption, '') // no sidecar → empty
    assert.deepEqual(
      stills.map((s) => s.label),
      ['frame 01', 'frame 02'],
    )
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
    fs.rmSync(captureDir, { recursive: true, force: true })
  }
})

test('renderFrames batch: only frames whose PNG was written are returned', async () => {
  const { renderFrames } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  const captureDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-cap-'))
  try {
    fs.writeFileSync(path.join(rawDir, 'screen-01.txt'), 'one')
    fs.writeFileSync(path.join(rawDir, 'screen-02.txt'), 'two')
    // Simulate a partial batch: only frame 02 gets written.
    const renderStills = async (jobs) => {
      const j = jobs.find((x) => x.output.endsWith('frame-02.png'))
      fs.writeFileSync(j.output, 'fake-png')
    }
    const stills = await renderFrames({ rawDir, captureDir, title: 'boss', renderStills })
    assert.deepEqual(
      stills.map((s) => s.label),
      ['frame 02'],
    )
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
    fs.rmSync(captureDir, { recursive: true, force: true })
  }
})

test('renderFrames defaults caption to empty string when no sidecar exists', async () => {
  const { renderFrames } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  const captureDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-cap-'))
  try {
    fs.writeFileSync(path.join(rawDir, 'screen-01.txt'), 'a TUI screen')
    const seen = []
    const renderStill = async ({ output, caption }) => {
      seen.push({ caption })
      fs.writeFileSync(output, 'fake-png')
    }
    await renderFrames({ rawDir, captureDir, title: 'boss', renderStill })
    assert.equal(seen.length, 1)
    assert.equal(seen[0].caption, '')
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
    fs.rmSync(captureDir, { recursive: true, force: true })
  }
})

test('runAgentLoop binds each frame caption to the narration preceding its tool call', async () => {
  const { runAgentLoop } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  try {
    // A SINGLE response carries two actions, each preceded by its own narration.
    // The turn-wide text join would caption both frames identically; the fix
    // must write each tool call's preceding text to its own sidecar.
    const firstTurn = {
      stop_reason: 'tool_use',
      usage: { input_tokens: 10, output_tokens: 5 },
      content: [
        { type: 'text', text: 'Opening the cron list' },
        { type: 'tool_use', id: 'a', name: 'observe', input: {} },
        { type: 'text', text: 'Selecting the first row' },
        { type: 'tool_use', id: 'b', name: 'send_keys', input: { keys: ['down'] } },
      ],
    }
    const model = scriptedModel([
      firstTurn,
      toolUse('done', { passed: true, summary: 'done', evidence: [] }, { text: 'wrapping up' }),
    ])
    const bridge = scriptedBridge({ screens: ['SCREEN ONE', 'SCREEN TWO'] })
    await runAgentLoop({
      brief: {
        description: 'prove it',
        budgets: { maxSteps: 5, maxWallClockMs: 60_000, maxTokens: 100_000 },
      },
      model: 'test-model',
      modelDep: model,
      bridge,
      rawDir,
    })
    assert.equal(
      fs.readFileSync(path.join(rawDir, 'caption-01.txt'), 'utf8'),
      'Opening the cron list',
    )
    assert.equal(
      fs.readFileSync(path.join(rawDir, 'caption-02.txt'), 'utf8'),
      'Selecting the first row',
    )
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
  }
})

// ── parseCastTailMs (caption clock, BOS-121) ─────────────────────────────────

test('parseCastTailMs: returns the last event time in ms (header skipped)', async () => {
  const { parseCastTailMs } = await import('./proof-tui-agent.mjs')
  const cast = [
    '{"version":2,"width":80,"height":24}',
    '[0.5, "o", "boot"]',
    '[2.25, "o", "settings"]',
    '[3.0, "o", "done"]',
  ].join('\n')
  assert.equal(parseCastTailMs(cast), 3000)
})

test('parseCastTailMs: ignores trailing blank lines and partial last lines', async () => {
  const { parseCastTailMs } = await import('./proof-tui-agent.mjs')
  const cast = '{"version":2}\n[1.0, "o", "a"]\n[2.5, "o", "b"]\n\n  \n'
  assert.equal(parseCastTailMs(cast), 2500)
})

test('parseCastTailMs: empty / header-only / non-string → 0 (graceful degrade)', async () => {
  const { parseCastTailMs } = await import('./proof-tui-agent.mjs')
  assert.equal(parseCastTailMs(''), 0)
  assert.equal(parseCastTailMs('{"version":2,"width":80}'), 0)
  assert.equal(parseCastTailMs(undefined), 0)
  assert.equal(parseCastTailMs(null), 0)
})

test('parseCastTailMs: rounds to the nearest ms and never goes negative', async () => {
  const { parseCastTailMs } = await import('./proof-tui-agent.mjs')
  assert.equal(parseCastTailMs('{"v":2}\n[1.2345, "o", "x"]'), 1235)
  assert.equal(parseCastTailMs('{"v":2}\n[-0.4, "o", "x"]'), 0)
})

test('runAgentLoop: records per-step caption timings from the injected cast clock', async () => {
  const { runAgentLoop } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  try {
    const firstTurn = {
      stop_reason: 'tool_use',
      usage: { input_tokens: 10, output_tokens: 5 },
      content: [
        { type: 'text', text: 'Opening the cron list' },
        { type: 'tool_use', id: 'a', name: 'observe', input: {} },
        { type: 'text', text: 'Selecting the first row' },
        { type: 'tool_use', id: 'b', name: 'send_keys', input: { keys: ['down'] } },
      ],
    }
    const model = scriptedModel([
      firstTurn,
      toolUse('done', { passed: true, summary: 'done', evidence: [] }),
    ])
    const bridge = scriptedBridge({ screens: ['SCREEN ONE', 'SCREEN TWO'] })
    // Injected cast clock advances per settled screen — no real bridge/cast.
    const stamps = [1000, 3500]
    let i = 0
    const { captionTimings } = await runAgentLoop({
      brief: {
        description: 'prove it',
        budgets: { maxSteps: 5, maxWallClockMs: 60_000, maxTokens: 100_000 },
      },
      model: 'test-model',
      modelDep: model,
      bridge,
      rawDir,
      readCastMs: () => stamps[Math.min(i++, stamps.length - 1)],
    })
    assert.deepEqual(captionTimings, [
      { seq: 1, caption: 'Opening the cron list', startMs: 1000 },
      { seq: 2, caption: 'Selecting the first row', startMs: 3500 },
    ])
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
  }
})

test('runAgentLoop: default cast clock degrades to startMs 0 when no .cast exists', async () => {
  const { runAgentLoop } = await import('./proof-tui-agent.mjs')
  const rawDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-raw-'))
  try {
    const model = scriptedModel([
      toolUse('observe', {}, { text: 'Looking at the home screen' }),
      toolUse('done', { passed: true, summary: 'done', evidence: [] }),
    ])
    const bridge = scriptedBridge({ screens: ['HOME'] })
    // No readCastMs injected and no session.cast on disk → reader returns 0.
    const { captionTimings } = await runAgentLoop({
      brief: {
        description: 'prove it',
        budgets: { maxSteps: 5, maxWallClockMs: 60_000, maxTokens: 100_000 },
      },
      model: 'test-model',
      modelDep: model,
      bridge,
      rawDir,
    })
    assert.deepEqual(captionTimings, [
      { seq: 1, caption: 'Looking at the home screen', startMs: 0 },
    ])
  } finally {
    fs.rmSync(rawDir, { recursive: true, force: true })
  }
})

test('runTuiAgentProof: persists raw/caption-timings.json and forwards timings to castToVideo', async () => {
  const { runTuiAgentProof } = await import('./proof-tui-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined
  const brief = {
    title: 'Captioned video',
    description: 'Produces a captioned video',
    expectedEvidence: ['Ready'],
  }
  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        BASE_ENV({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_RUN_ID: 'tui-captions' }),
        async () => {
          let castArgs = null
          const castToVideo = async (args) => {
            castArgs = args
            return null // stills-only; we only assert the timings were threaded
          }
          const { manifest } = await runTuiAgentProof({
            prNumber: '0',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            deps: {
              bridge: scriptedBridge({ screens: ['Ready to go'] }),
              model: scriptedModel([
                toolUse('observe', {}, { text: 'Observing the ready screen' }),
                toolUse('done', { summary: 'done', passed: true }),
              ]),
              renderStill: fakeRenderStill(),
              castToVideo,
            },
          })
          assert.ok(manifest, 'run completes')
          assert.ok(
            Array.isArray(castArgs.captionTimings),
            'captionTimings forwarded to castToVideo',
          )
          assert.equal(castArgs.captionTimings[0].caption, 'Observing the ready screen')
          // raw/caption-timings.json persisted next to the run's screens. The
          // path is deterministic from the env (commit/run-id/run-token in BASE_ENV).
          const rawTimingsPath = path.join(
            REPO_ROOT,
            '.proof',
            'pr-0',
            'abc1234',
            'tui-captions',
            'tok-tui-test',
            'raw',
            'caption-timings.json',
          )
          const persisted = JSON.parse(fs.readFileSync(rawTimingsPath, 'utf8'))
          assert.equal(persisted[0].caption, 'Observing the ready screen')
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    cleanupPr('0')
  }
})
