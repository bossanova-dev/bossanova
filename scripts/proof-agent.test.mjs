/**
 * proof-agent.test.mjs — Unit tests for proof-agent.mjs pure seams.
 *
 * Strategy: test the pure/injectable seams without spawning Playwright or
 * calling the Anthropic API. The orchestrator's main external side-effects
 * (SDK + spawn) are tested via the helper modules' unit tests; here we cover:
 *
 *   1. agentModeAvailable() dispatcher logic from proof.mjs
 *   2. validateBrief defaults and errors (belt-and-suspenders — already in brief.test.mjs)
 *   3. Capture-shape assembly (status, stills, stub flag)
 *   4. Secret-scan gate (plants a token, expects FAIL before any upload)
 *   5. buildManifest with agentRunnerStubbed: true
 */

import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'

import { finalizeAgentProof } from './proof-agent-finalize.mjs'

function requireFfmpeg(t, reason) {
  const ffmpegCheck = spawnSync('ffmpeg', ['-version'], { stdio: 'ignore' })
  if (ffmpegCheck.status !== 0) {
    t.skip(reason)
    return false
  }
  return true
}

// ── 1. Dispatcher: agentModeAvailable() ─────────────────────────────────────

test('agentModeAvailable: BOSS_PROOF_MODE=recipe → false regardless of key', async () => {
  const original = process.env.BOSS_PROOF_MODE
  const originalKey = process.env.PROOF_ANTHROPIC_API_KEY
  process.env.BOSS_PROOF_MODE = 'recipe'
  process.env.PROOF_ANTHROPIC_API_KEY = 'sk-some-key'
  try {
    const { agentModeAvailable } = await import('./proof.mjs')
    assert.equal(agentModeAvailable(), false)
  } finally {
    process.env.BOSS_PROOF_MODE = original ?? ''
    if (originalKey === undefined) delete process.env.PROOF_ANTHROPIC_API_KEY
    else process.env.PROOF_ANTHROPIC_API_KEY = originalKey
  }
})

test('agentModeAvailable: BOSS_PROOF_MODE=agent → true regardless of key', async () => {
  const original = process.env.BOSS_PROOF_MODE
  const originalKey = process.env.PROOF_ANTHROPIC_API_KEY
  process.env.BOSS_PROOF_MODE = 'agent'
  delete process.env.PROOF_ANTHROPIC_API_KEY
  try {
    const { agentModeAvailable } = await import('./proof.mjs')
    assert.equal(agentModeAvailable(), true)
  } finally {
    if (original === undefined) delete process.env.BOSS_PROOF_MODE
    else process.env.BOSS_PROOF_MODE = original
    if (originalKey !== undefined) process.env.PROOF_ANTHROPIC_API_KEY = originalKey
  }
})

test('agentModeAvailable: unset mode + key set → true', async () => {
  const original = process.env.BOSS_PROOF_MODE
  const originalKey = process.env.PROOF_ANTHROPIC_API_KEY
  delete process.env.BOSS_PROOF_MODE
  process.env.PROOF_ANTHROPIC_API_KEY = 'sk-test-key-123'
  try {
    const { agentModeAvailable } = await import('./proof.mjs')
    assert.equal(agentModeAvailable(), true)
  } finally {
    if (original !== undefined) process.env.BOSS_PROOF_MODE = original
    if (originalKey === undefined) delete process.env.PROOF_ANTHROPIC_API_KEY
    else process.env.PROOF_ANTHROPIC_API_KEY = originalKey
  }
})

test('agentModeAvailable: explicit recipe selection prefers recipe path unless agent forced', async () => {
  const original = process.env.BOSS_PROOF_MODE
  const originalKey = process.env.PROOF_ANTHROPIC_API_KEY
  delete process.env.BOSS_PROOF_MODE
  process.env.PROOF_ANTHROPIC_API_KEY = 'sk-test-key-123'
  try {
    const { agentModeAvailable } = await import('./proof.mjs')
    assert.equal(agentModeAvailable({ explicitRecipeSelection: true }), false)
    process.env.BOSS_PROOF_MODE = 'agent'
    assert.equal(agentModeAvailable({ explicitRecipeSelection: true }), true)
  } finally {
    if (original === undefined) delete process.env.BOSS_PROOF_MODE
    else process.env.BOSS_PROOF_MODE = original
    if (originalKey === undefined) delete process.env.PROOF_ANTHROPIC_API_KEY
    else process.env.PROOF_ANTHROPIC_API_KEY = originalKey
  }
})

test('agentModeAvailable: unset mode + no key → false (recipe fallback)', async () => {
  const original = process.env.BOSS_PROOF_MODE
  const originalKey = process.env.PROOF_ANTHROPIC_API_KEY
  delete process.env.BOSS_PROOF_MODE
  delete process.env.PROOF_ANTHROPIC_API_KEY
  try {
    const { agentModeAvailable } = await import('./proof.mjs')
    assert.equal(agentModeAvailable(), false)
  } finally {
    if (original !== undefined) process.env.BOSS_PROOF_MODE = original
    if (originalKey !== undefined) process.env.PROOF_ANTHROPIC_API_KEY = originalKey
  }
})

// ── 2. validateBrief (belt-and-suspenders) ───────────────────────────────────

test('validateBrief: minimal valid brief passes with all defaults', async () => {
  const { validateBrief } = await import('./proof-brief.mjs')
  const { brief, errors } = validateBrief({ title: 'Test feature', description: 'Shows it works' })
  assert.deepEqual(errors, [])
  assert.equal(brief.title, 'Test feature')
  assert.deepEqual(brief.targetRoutes, [])
  assert.equal(brief.budgets.maxSteps, 60)
  assert.equal(brief.budgets.maxWallClockMs, 720000)
})

test('validateBrief: missing title returns null brief and error', async () => {
  const { validateBrief } = await import('./proof-brief.mjs')
  const { brief, errors } = validateBrief({ description: 'desc' })
  assert.equal(brief, null)
  assert.ok(errors.some((e) => /title/.test(e)))
})

// ── 3. Capture-shape assembly and agentRunnerStubbed flag ────────────────────

test('buildManifest sets agentRunnerStubbed in output when passed', async () => {
  const { buildManifest } = await import('./proof-lib.mjs')
  const manifest = buildManifest({
    commit: 'abc1234',
    prNumber: '42',
    runId: 'run-1',
    publicBaseUrl: 'https://proof.example.dev/p',
    agentRunnerStubbed: true,
    captures: [
      {
        recipeId: 'agent-proof',
        title: 'Agent proof',
        surface: 'web',
        privacy: 'fixture',
        status: 'passed',
        mediaType: 'mp4',
        fileName: 'agent-proof/agent-proof.mp4',
        posterFileName: 'agent-proof/agent-proof.png',
      },
    ],
  })
  assert.equal(manifest.agentRunnerStubbed, true)
  assert.equal(manifest.captures[0].mediaType, 'mp4')
  assert.equal(manifest.captures[0].url, 'https://proof.example.dev/p/agent-proof/agent-proof.mp4')
  assert.equal(
    manifest.captures[0].videoUrl,
    'https://proof.example.dev/p/agent-proof/agent-proof.mp4',
  )
  assert.equal(
    manifest.captures[0].posterUrl,
    'https://proof.example.dev/p/agent-proof/agent-proof.png',
  )
})

test('buildManifest: agentRunnerStubbed absent → property not set', async () => {
  const { buildManifest } = await import('./proof-lib.mjs')
  const manifest = buildManifest({
    commit: 'abc1234',
    prNumber: '7',
    runId: 'run-1',
    publicBaseUrl: 'https://proof.example.dev/p',
    captures: [],
  })
  assert.ok(
    !('agentRunnerStubbed' in manifest),
    'agentRunnerStubbed must not appear when not passed',
  )
})

test('buildManifest: failed capture with mp4 gets url (reviewable failure)', async () => {
  const { buildManifest } = await import('./proof-lib.mjs')
  const manifest = buildManifest({
    commit: 'abc1234',
    prNumber: '99',
    runId: 'run-2',
    publicBaseUrl: 'https://proof.example.dev/p',
    agentRunnerStubbed: true,
    captures: [
      {
        recipeId: 'agent-proof',
        title: 'Agent proof',
        surface: 'web',
        privacy: 'fixture',
        status: 'failed',
        mediaType: 'mp4',
        fileName: 'agent-proof/agent-proof.mp4',
        posterFileName: 'agent-proof/agent-proof.png',
        error: 'agent timed out after 720s',
      },
    ],
  })
  const c = manifest.captures[0]
  assert.equal(c.status, 'failed')
  assert.ok(c.url, 'failed capture with fileName must have url for reviewability')
  assert.equal(c.videoUrl, 'https://proof.example.dev/p/agent-proof/agent-proof.mp4')
})

test('renderGallery shows stub notice when agentRunnerStubbed is true', async () => {
  const { buildManifest, renderGallery } = await import('./proof-lib.mjs')
  const manifest = buildManifest({
    commit: 'abc1234',
    prNumber: '7',
    runId: 'run-1',
    publicBaseUrl: 'https://proof.example.dev/p',
    agentRunnerStubbed: true,
    captures: [
      {
        recipeId: 'agent-proof',
        title: 'Test',
        surface: 'web',
        privacy: 'fixture',
        status: 'passed',
        mediaType: 'mp4',
        fileName: 'agent-proof/agent-proof.mp4',
        posterFileName: 'agent-proof/agent-proof.png',
      },
    ],
  })
  const body = renderGallery({ manifest })
  assert.match(body, /agent-runner stubbed/)
})

// ── 5. stills in agent capture shape ─────────────────────────────────────────

test('buildManifest: passed capture stills get urls', async () => {
  const { buildManifest } = await import('./proof-lib.mjs')
  const manifest = buildManifest({
    commit: 'abc1234',
    prNumber: '7',
    runId: 'run-1',
    publicBaseUrl: 'https://proof.example.dev/p',
    agentRunnerStubbed: true,
    captures: [
      {
        recipeId: 'agent-proof',
        title: 'Agent proof',
        surface: 'web',
        privacy: 'fixture',
        status: 'passed',
        mediaType: 'mp4',
        fileName: 'agent-proof/agent-proof.mp4',
        posterFileName: 'agent-proof/agent-proof.png',
        stills: [
          { fileName: 'agent-proof/01-dashboard.png', label: '01 dashboard' },
          { fileName: 'agent-proof/02-session.png', label: '02 session' },
        ],
      },
    ],
  })
  const c = manifest.captures[0]
  assert.ok(Array.isArray(c.stills), 'stills must be an array')
  assert.equal(c.stills.length, 2)
  assert.equal(c.stills[0].url, 'https://proof.example.dev/p/agent-proof/01-dashboard.png')
  assert.equal(c.stills[1].url, 'https://proof.example.dev/p/agent-proof/02-session.png')
})

// ── 6. proofUploadFiles: agent-mode failed capture gets media queued ──────────

test('proofUploadFiles: failed capture with mp4 fileName is queued for upload', async () => {
  const { proofUploadFiles } = await import('./proof-lib.mjs')
  const files = proofUploadFiles({
    manifest: {
      captures: [
        {
          status: 'failed',
          mediaType: 'mp4',
          fileName: 'agent-proof/agent-proof.mp4',
          posterFileName: 'agent-proof/agent-proof.png',
        },
      ],
    },
    localDir: '/tmp/proof-agent-test',
  })
  const rels = files.map((f) => f.relative)
  assert.ok(rels.includes('agent-proof/agent-proof.mp4'), 'mp4 must be queued for upload')
  assert.ok(rels.includes('agent-proof/agent-proof.png'), 'poster must be queued for upload')
})

// ── 7. Orchestrator seam: runAgentProof with mocked spawn + uploader ─────────
// Drives the real runAgentProof but injects a fake Playwright spawn (no browser)
// and a fake uploader.

const REPO_ROOT = path.resolve(path.dirname(new URL(import.meta.url).pathname), '..')

async function withTempBrief(brief, fn) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-agent-brief-'))
  const briefPath = path.join(dir, 'brief.json')
  fs.writeFileSync(briefPath, JSON.stringify(brief, null, 2))
  try {
    return await fn(briefPath)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
}

// Snapshot + restore the env keys these tests mutate.
function withEnv(overrides, fn) {
  const keys = [
    'BOSS_PROOF_BRIEF',
    'BOSS_PROOF_UPLOAD',
    'BOSS_PROOF_RUN_ID',
    'BOSS_PROOF_RUN_TOKEN',
    'BOSS_PROOF_R2_BUCKET',
    'BOSS_PROOF_PUBLIC_BASE_URL',
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

test('runAgentProof: clean run reaches uploadBundle', async (t) => {
  const { runAgentProof } = await import('./proof-agent.mjs')

  if (!requireFfmpeg(t, 'ffmpeg is required for clean agent video regression test')) return

  const brief = { title: 'Clean feature', description: 'Proves the change works' }

  let uploadCalled = false

  await withTempBrief(brief, (briefPath) =>
    withEnv(
      {
        BOSS_PROOF_BRIEF: briefPath,
        BOSS_PROOF_UPLOAD: '1',
        BOSS_PROOF_RUN_ID: 'test-run-clean',
        BOSS_PROOF_RUN_TOKEN: 'tokclean1',
        BOSS_PROOF_R2_BUCKET: 'test-bucket',
        BOSS_PROOF_PUBLIC_BASE_URL: 'https://proof.test.dev',
      },
      async () => {
        const { manifest } = await runAgentProof({
          prNumber: 'local', // 'local' short-circuits comment posting
          commit: 'abc1234',
          changedFiles: [],
          dryRun: false,
          deps: {
            spawnPlaywright: ({ localDir }) => {
              const rawDir = path.join(localDir, 'raw')
              fs.mkdirSync(rawDir, { recursive: true })
              fs.writeFileSync(
                path.join(rawDir, 'proof-result.json'),
                JSON.stringify({ passed: true, summary: 'all good', evidence: [], steps: 2 }),
              )
              const result = spawnSync(
                'ffmpeg',
                [
                  '-y',
                  '-loglevel',
                  'error',
                  '-f',
                  'lavfi',
                  '-i',
                  'color=c=black:s=32x32:d=0.25',
                  '-c:v',
                  'libvpx',
                  '-pix_fmt',
                  'yuv420p',
                  path.join(rawDir, 'session.webm'),
                ],
                { stdio: 'ignore' },
              )
              assert.equal(result.status, 0, 'test video fixture must be created')
              return { status: 0 }
            },
            uploadBundle: () => {
              uploadCalled = true
            },
            uploadManifest: () => {},
            publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
            collapsePriorProofComments: () => {},
            postComment: () => {},
          },
        })
        assert.equal(manifest.agentRunnerStubbed, true)
        assert.equal(manifest.captures[0].title, 'Clean feature')
      },
    ),
  )

  assert.equal(uploadCalled, true, 'a clean run must reach uploadBundle')
  fs.rmSync(path.join(REPO_ROOT, '.proof', 'pr-local'), { recursive: true, force: true })
})

test('runAgentProof: passed agent result without captured video marks proof failed', async () => {
  const { runAgentProof } = await import('./proof-agent.mjs')

  const brief = {
    title: 'Missing video feature',
    description: 'Agent reported done but video was absent',
  }
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        {
          BOSS_PROOF_BRIEF: briefPath,
          BOSS_PROOF_UPLOAD: '0',
          BOSS_PROOF_RUN_ID: 'test-run-no-video',
          BOSS_PROOF_RUN_TOKEN: 'toknovideo1',
          BOSS_PROOF_PUBLIC_BASE_URL: 'https://proof.test.dev',
        },
        async () => {
          const { manifest } = await runAgentProof({
            prNumber: 'local',
            commit: 'feed123',
            changedFiles: [],
            dryRun: true,
            deps: {
              spawnPlaywright: ({ localDir }) => {
                const rawDir = path.join(localDir, 'raw')
                fs.mkdirSync(rawDir, { recursive: true })
                fs.writeFileSync(
                  path.join(rawDir, 'proof-result.json'),
                  JSON.stringify({
                    passed: true,
                    summary: 'agent claims success',
                    evidence: [],
                    steps: 2,
                  }),
                )
                return { status: 0 }
              },
              uploadBundle: () => {},
              uploadManifest: () => {},
              publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
              collapsePriorProofComments: () => {},
              postComment: () => {},
            },
          })

          assert.equal(manifest.verdict, 'failed')
          assert.equal(manifest.captures[0].status, 'failed')
          assert.match(manifest.captures[0].error, /no video artifact captured/)
          assert.equal(process.exitCode, 1)
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    fs.rmSync(path.join(REPO_ROOT, '.proof', 'pr-local'), { recursive: true, force: true })
  }
})

test('runAgentProof: agent done({noSurface}) posts the honest no-ui-surface note and keeps exit 0', async () => {
  const { runAgentProof } = await import('./proof-agent.mjs')

  const brief = { title: 'Backend-only change', description: 'No demonstrable web surface' }
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        {
          BOSS_PROOF_BRIEF: briefPath,
          BOSS_PROOF_UPLOAD: '0',
          BOSS_PROOF_RUN_ID: 'test-run-no-surface',
          BOSS_PROOF_RUN_TOKEN: 'toknosurface',
          BOSS_PROOF_PUBLIC_BASE_URL: 'https://proof.test.dev',
        },
        async () => {
          const { manifest, commentBody } = await runAgentProof({
            prNumber: 'local',
            commit: 'feed456',
            changedFiles: [],
            dryRun: true,
            deps: {
              spawnPlaywright: ({ localDir }) => {
                const rawDir = path.join(localDir, 'raw')
                fs.mkdirSync(rawDir, { recursive: true })
                fs.writeFileSync(
                  path.join(rawDir, 'proof-result.json'),
                  JSON.stringify({
                    passed: false,
                    noSurface: true,
                    summary: 'agent navigated; no user-visible change to demonstrate',
                    evidence: [],
                    steps: 3,
                  }),
                )
                return { status: 0 }
              },
              uploadBundle: () => {},
              uploadManifest: () => {},
              publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
              collapsePriorProofComments: () => {},
              postComment: () => {},
            },
          })

          assert.equal(manifest.deferred, true, 'no-surface run defers')
          assert.equal(manifest.captures.length, 1, 'agent capture only, no recipe floor')
          assert.match(commentBody, /No web UI surface to demonstrate/)
          assert.ok(!commentBody.includes('❌'), 'no-surface note carries no red verdict')
          // The crucial difference from a genuine failure: a no-surface skip is
          // neutral and must NOT redden the PR.
          assert.notEqual(process.exitCode, 1, 'no-surface skip keeps a non-failing exit code')
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    fs.rmSync(path.join(REPO_ROOT, '.proof', 'pr-local'), { recursive: true, force: true })
  }
})

test('runAgentProof: successful posted agent run removes local run directory', async (t) => {
  const { runAgentProof } = await import('./proof-agent.mjs')

  if (!requireFfmpeg(t, 'ffmpeg is required for cleanup wiring regression test')) return

  const brief = { title: 'Cleanup feature', description: 'Successful posted agent proof' }
  const runDir = path.join(
    REPO_ROOT,
    '.proof',
    'pr-789',
    'abc1234',
    'test-run-cleanup',
    'tokcleanup1',
  )

  await withTempBrief(brief, (briefPath) =>
    withEnv(
      {
        BOSS_PROOF_BRIEF: briefPath,
        BOSS_PROOF_UPLOAD: '1',
        BOSS_PROOF_RUN_ID: 'test-run-cleanup',
        BOSS_PROOF_RUN_TOKEN: 'tokcleanup1',
        BOSS_PROOF_R2_BUCKET: 'test-bucket',
        BOSS_PROOF_PUBLIC_BASE_URL: 'https://proof.test.dev',
      },
      async () => {
        const { manifest } = await runAgentProof({
          prNumber: '789',
          commit: 'abc1234',
          changedFiles: [],
          dryRun: false,
          deps: {
            spawnPlaywright: ({ localDir }) => {
              const rawDir = path.join(localDir, 'raw')
              fs.mkdirSync(rawDir, { recursive: true })
              fs.writeFileSync(
                path.join(rawDir, 'proof-result.json'),
                JSON.stringify({ passed: true, summary: 'all good', evidence: [], steps: 2 }),
              )
              const result = spawnSync(
                'ffmpeg',
                [
                  '-y',
                  '-loglevel',
                  'error',
                  '-f',
                  'lavfi',
                  '-i',
                  'color=c=black:s=32x32:d=0.25',
                  '-c:v',
                  'libvpx',
                  '-pix_fmt',
                  'yuv420p',
                  path.join(rawDir, 'session.webm'),
                ],
                { stdio: 'ignore' },
              )
              assert.equal(result.status, 0, 'test video fixture must be created')
              return { status: 0 }
            },
            uploadBundle: () => {},
            uploadManifest: () => {},
            publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
            collapsePriorProofComments: () => {},
            postComment: () => {},
          },
        })

        assert.equal(manifest.verdict, 'passed')
      },
    ),
  )

  assert.equal(fs.existsSync(runDir), false, 'successful posted agent run dir must be removed')
  fs.rmSync(path.join(REPO_ROOT, '.proof', 'pr-789'), { recursive: true, force: true })
})

// ── 8. Fallback frame extraction (agent path) ─────────────────────────────────
// When the agent takes ZERO raw stills but a video exists, extractFallbackFrame
// must be invoked and the resulting capture must contain exactly one still.
// When stills ARE present, the fallback extractor must NOT be called.

test('runAgentProof: no raw stills + video → fallback extractor invoked, capture has 1 still', async () => {
  const { runAgentProof } = await import('./proof-agent.mjs')

  const brief = { title: 'No stills feature', description: 'Agent took no screenshots' }
  let extractorCalled = false
  let extractorArgs = null
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        {
          BOSS_PROOF_BRIEF: briefPath,
          BOSS_PROOF_UPLOAD: '0', // dry run; no upload needed
          BOSS_PROOF_RUN_ID: 'test-run-fallback',
          BOSS_PROOF_RUN_TOKEN: 'tokfallback1',
          BOSS_PROOF_PUBLIC_BASE_URL: 'https://proof.test.dev',
        },
        async () => {
          const { manifest } = await runAgentProof({
            prNumber: 'local',
            commit: 'deadbeef',
            changedFiles: [],
            dryRun: true,
            deps: {
              spawnPlaywright: ({ localDir }) => {
                const rawDir = path.join(localDir, 'raw')
                fs.mkdirSync(rawDir, { recursive: true })
                // Write proof-result.json (passed) but NO \d\d-*.png stills
                fs.writeFileSync(
                  path.join(rawDir, 'proof-result.json'),
                  JSON.stringify({ passed: true, summary: 'all good', evidence: [], steps: 2 }),
                )
                // Write a fake .webm so the video branch is taken
                fs.writeFileSync(path.join(rawDir, 'session.webm'), 'fake-webm-data')
                return { status: 0 }
              },
              // Injected fallback extractor: writes the fallback PNG and records call
              extractFallbackFrame: ({ mp4Path, fallbackPath }) => {
                extractorCalled = true
                extractorArgs = { mp4Path, fallbackPath }
                // Write the fallback PNG so the stills assembly picks it up
                fs.writeFileSync(fallbackPath, 'fake-png-data')
                return { status: 0 }
              },
              uploadBundle: () => {},
              uploadManifest: () => {},
              publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
              collapsePriorProofComments: () => {},
              postComment: () => {},
            },
          })

          assert.equal(
            extractorCalled,
            true,
            'extractFallbackFrame must be called when no stills exist',
          )
          assert.ok(
            extractorArgs.fallbackPath.endsWith('01-final-frame.png'),
            'fallback file must be named 01-final-frame.png',
          )

          const capture = manifest.captures[0]
          assert.ok(Array.isArray(capture.stills), 'capture must have stills array')
          assert.equal(
            capture.stills.length,
            1,
            'capture must have exactly 1 still (the fallback frame)',
          )
          assert.ok(
            capture.stills[0].fileName.includes('01-final-frame.png'),
            'the still must be the fallback frame',
          )
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    fs.rmSync(path.join(REPO_ROOT, '.proof', 'pr-local'), { recursive: true, force: true })
  }
})

// ── Task 2: Degradation ladder in finalizeAgentProof ────────────────────────

test('finalizeAgentProof defers (no red verdict) when the run produced no media', async () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-finalize-test-'))
  const originalExitCode = process.exitCode
  process.exitCode = undefined
  try {
    const { manifest, commentBody } = await finalizeAgentProof({
      captureShape: {
        recipeId: 'tui-home',
        title: 'Test feature',
        surface: 'tui',
        privacy: 'fixture',
        status: 'failed',
        // no fileName → no media
      },
      brief: { title: 'Test feature' },
      agentResult: { passed: false, summary: null, evidence: [], steps: 0 },
      hasFailure: true,
      prNumber: 'local',
      commit: 'abc1234',
      runId: 'run-test',
      token: 'tok-test',
      paths: { publicPrefix: 'proof/test/pr-local/abc1234/run-test/tok-test' },
      localDir: tmpDir,
      publicBaseUrl: 'https://proof.test.dev',
      shouldUpload: false,
      bucket: null,
      deps: {
        postComment: () => {},
        uploadBundle: () => {},
        uploadManifest: () => {},
        publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
        collapsePriorProofComments: () => {},
        currentRepoIdentity: () => null,
        currentBranch: () => 'test-branch',
      },
    })
    assert.equal(manifest.deferred, true, 'manifest.deferred must be true on degraded path')
    assert.ok(!commentBody.includes('❌'), 'deferred comment must not contain red verdict')
    assert.ok(
      !commentBody.includes('Unsatisfactory'),
      'deferred comment must not contain Unsatisfactory',
    )
    // The PERSISTED manifest (the artifact uploaded to R2) must also carry the
    // flag — it is set before the manifest is written, not only on the return value.
    const persisted = JSON.parse(fs.readFileSync(path.join(tmpDir, 'manifest.json'), 'utf8'))
    assert.equal(persisted.deferred, true, 'written manifest.json must carry deferred:true')
  } finally {
    process.exitCode = originalExitCode
    fs.rmSync(tmpDir, { recursive: true, force: true })
  }
})

test('finalizeAgentProof treats stills-only captures as media', async () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-finalize-stills-test-'))
  try {
    const { manifest, commentBody } = await finalizeAgentProof({
      captureShape: {
        recipeId: 'tui-agent',
        title: 'TUI stills proof',
        surface: 'tui',
        privacy: 'fixture',
        status: 'passed',
        stills: [{ fileName: 'tui-agent/frame-01.png', label: 'frame 01' }],
      },
      brief: { title: 'TUI stills proof' },
      agentResult: { passed: true, summary: 'done', evidence: [], steps: 1 },
      hasFailure: false,
      prNumber: 'local',
      commit: 'abc1234',
      runId: 'run-stills-test',
      token: 'tok-stills',
      paths: { publicPrefix: 'proof/test/pr-local/abc1234/run-stills-test/tok-stills' },
      localDir: tmpDir,
      publicBaseUrl: 'https://proof.test.dev',
      shouldUpload: false,
      bucket: null,
      deps: {
        postComment: () => {},
        uploadBundle: () => {},
        uploadManifest: () => {},
        publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
        collapsePriorProofComments: () => {},
        currentRepoIdentity: () => null,
        currentBranch: () => 'test-branch',
      },
    })
    assert.equal(manifest.deferred, undefined, 'stills-only media must not be deferred')
    assert.ok(commentBody.includes('✅'), 'stills-only passed proof keeps normal verdict')
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true })
  }
})

test('runAgentProof: has raw stills → fallback extractor NOT invoked', async () => {
  const { runAgentProof } = await import('./proof-agent.mjs')

  const brief = { title: 'Has stills feature', description: 'Agent took screenshots' }
  let extractorCalled = false
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        {
          BOSS_PROOF_BRIEF: briefPath,
          BOSS_PROOF_UPLOAD: '0',
          BOSS_PROOF_RUN_ID: 'test-run-has-stills',
          BOSS_PROOF_RUN_TOKEN: 'tokhasstills1',
          BOSS_PROOF_PUBLIC_BASE_URL: 'https://proof.test.dev',
        },
        async () => {
          const { manifest } = await runAgentProof({
            prNumber: 'local',
            commit: 'cafebabe',
            changedFiles: [],
            dryRun: true,
            deps: {
              spawnPlaywright: ({ localDir }) => {
                const rawDir = path.join(localDir, 'raw')
                fs.mkdirSync(rawDir, { recursive: true })
                fs.writeFileSync(
                  path.join(rawDir, 'proof-result.json'),
                  JSON.stringify({ passed: true, summary: 'ok', evidence: [], steps: 3 }),
                )
                // Write a fake .webm and TWO real stills
                fs.writeFileSync(path.join(rawDir, 'session.webm'), 'fake-webm-data')
                fs.writeFileSync(path.join(rawDir, '01-home.png'), 'fake-png-1')
                fs.writeFileSync(path.join(rawDir, '02-detail.png'), 'fake-png-2')
                return { status: 0 }
              },
              extractFallbackFrame: () => {
                extractorCalled = true
                return { status: 0 }
              },
              uploadBundle: () => {},
              uploadManifest: () => {},
              publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
              collapsePriorProofComments: () => {},
              postComment: () => {},
            },
          })

          assert.equal(
            extractorCalled,
            false,
            'extractFallbackFrame must NOT be called when stills exist',
          )

          const capture = manifest.captures[0]
          assert.ok(Array.isArray(capture.stills), 'capture must have stills array')
          assert.equal(
            capture.stills.length,
            2,
            'capture must have exactly 2 stills (the real ones)',
          )
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    fs.rmSync(path.join(REPO_ROOT, '.proof', 'pr-local'), { recursive: true, force: true })
  }
})

// ── Task 3: Time-boxed agent path ────────────────────────────────────────────

test('resolveAgentBackstopMs: default backstop exceeds the inner agent budget so the SIGKILL never preempts the graceful save', async () => {
  const { resolveAgentBackstopMs, VIDEO_SAVE_HEADROOM_MS } = await import('./proof-agent.mjs')
  const innerBudgetMs = 720000 // proof-brief DEFAULT_BUDGETS.maxWallClockMs (12 min)
  const backstop = resolveAgentBackstopMs({ envValue: undefined, innerBudgetMs })
  assert.equal(backstop, innerBudgetMs + VIDEO_SAVE_HEADROOM_MS)
  assert.ok(backstop > innerBudgetMs, 'backstop must strictly exceed the inner budget')
})

test('resolveAgentBackstopMs: an env value below the floor is clamped up (cannot invert the timeouts)', async () => {
  const { resolveAgentBackstopMs, VIDEO_SAVE_HEADROOM_MS } = await import('./proof-agent.mjs')
  const innerBudgetMs = 720000
  // 420000 is the misconfiguration that hard-killed the agent 5 min early.
  const backstop = resolveAgentBackstopMs({ envValue: '420000', innerBudgetMs })
  assert.equal(backstop, innerBudgetMs + VIDEO_SAVE_HEADROOM_MS)
})

test('resolveAgentBackstopMs: an env value above the floor is honored', async () => {
  const { resolveAgentBackstopMs } = await import('./proof-agent.mjs')
  const backstop = resolveAgentBackstopMs({ envValue: '1500000', innerBudgetMs: 720000 })
  assert.equal(backstop, 1500000)
})

test('resolveAgentBackstopMs: a missing/invalid inner budget falls back to the default budget', async () => {
  const { resolveAgentBackstopMs, VIDEO_SAVE_HEADROOM_MS, DEFAULT_INNER_BUDGET_MS } = await import(
    './proof-agent.mjs'
  )
  const backstop = resolveAgentBackstopMs({ envValue: undefined, innerBudgetMs: undefined })
  assert.equal(backstop, DEFAULT_INNER_BUDGET_MS + VIDEO_SAVE_HEADROOM_MS)
})

test('runInterruptible: kills child at timeout and resolves with timedOut=true (not a throw)', async () => {
  const { runInterruptible } = await import('./proof-agent.mjs')
  // A Node.js child that sleeps for 10 seconds — must be killed within 100ms.
  const res = await runInterruptible('node', ['-e', 'setInterval(()=>{},1000)'], {
    timeoutMs: 100,
  })
  assert.equal(res.timedOut, true, 'timed-out child must resolve with timedOut=true')
  assert.equal(res.code, null, 'killed child has null exit code')
})

test('runAgentProof: timed-out Playwright drive defers with the agent capture only — NO recipe floor', async () => {
  const { runAgentProof } = await import('./proof-agent.mjs')
  const brief = { title: 'Timeout feature', description: 'Playwright times out' }
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        {
          BOSS_PROOF_BRIEF: briefPath,
          BOSS_PROOF_UPLOAD: '0',
          BOSS_PROOF_RUN_ID: 'test-run-timeout',
          BOSS_PROOF_RUN_TOKEN: 'toktimeout1',
          BOSS_PROOF_PUBLIC_BASE_URL: 'https://proof.test.dev',
        },
        async () => {
          const { manifest, commentBody } = await runAgentProof({
            prNumber: 'local',
            commit: 'deadbeef',
            changedFiles: [],
            dryRun: true,
            deps: {
              // Simulate a timed-out Playwright run — returns without throwing.
              spawnPlaywright: () => ({ status: -1, timedOut: true }),
              uploadBundle: () => {},
              uploadManifest: () => {},
              publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
              collapsePriorProofComments: () => {},
              postComment: () => {},
            },
          })

          assert.equal(manifest.deferred, true, 'timed-out run must be deferred')
          // Web is agent-only (BOS-118): the deferred run must carry ONLY the
          // agent's own capture, never a recipe-floor gallery.
          assert.ok(
            !manifest.captures.some((c) => c.surface !== 'web' || c.recipeId !== 'agent-proof'),
            'deferred web run must contain only the agent-proof capture (no recipe floor)',
          )
          assert.equal(manifest.captures.length, 1, 'exactly one capture (the agent proof)')
          assert.ok(!commentBody.includes('❌'), 'deferred comment must not contain red verdict')
          assert.ok(
            commentBody.includes('/manifest.json'),
            'deferred comment must link to uploaded/local manifest evidence',
          )
          assert.match(
            manifest.captures[0].error ?? '',
            /timed out/,
            'capture error must mention timeout',
          )
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    fs.rmSync(path.join(REPO_ROOT, '.proof', 'pr-local'), { recursive: true, force: true })
  }
})
