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
import { silenceConsole } from './quiet-test-console.mjs'

// Silence the code-under-test console output (finalize manifest JSON dumps +
// DEGRADED warnings) so a passing run stays quiet. See quiet-test-console.mjs.
silenceConsole()

// Hermeticity guard (BOS-141): finalizeAgentProof's default `judge` dep
// (judgeProof) makes a REAL Anthropic API call whenever PROOF_ANTHROPIC_API_KEY
// is set — and it IS set in every provisioned worktree (repo .env is copied
// in). runAgentProof forwards its merged deps verbatim into finalize, and the
// test deps here are partial (no `judge`), so drop the key for this file's
// test process: judgeProof then short-circuits to
// { unjudged: true, reason: 'missing-key' } BEFORE constructing the SDK
// (unit-proven in proof-judge.test.mjs). Tests that need the key present
// (agentModeAvailable) set and restore it around themselves, unaffected.
// node --test runs each file in its own process, so this cannot leak.
delete process.env.PROOF_ANTHROPIC_API_KEY

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

// ── BOS-142: per-scene agentRunnerStubbed disclosure → manifest flag ──────────
//
// The Task 5 web driver marks each AgentResult scene with `liveAgent` (true only
// for a scene that ran live against the real Claude plugin). proof-agent.mjs
// turns that into a per-scene `agentRunnerStubbed:false` on live scenes ONLY
// (absent ⇒ stubbed, byte-compat), and the run-level manifest flag is stubbed
// iff ANY scene is stub-backed.

// Writes a tiny black webm + a proof-result.json carrying the given scenes so
// runAgentProof takes the passed video branch. Returns a spawnPlaywright stub.
function livePlaywrightStub(scenes) {
  return ({ localDir }) => {
    const rawDir = path.join(localDir, 'raw')
    fs.mkdirSync(rawDir, { recursive: true })
    fs.writeFileSync(
      path.join(rawDir, 'proof-result.json'),
      JSON.stringify({ passed: true, summary: 'ok', evidence: [], steps: 2, scenes }),
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
  }
}

async function runWithScenes(t, runId, scenes) {
  const { runAgentProof } = await import('./proof-agent.mjs')
  if (!requireFfmpeg(t, 'ffmpeg is required for per-scene stub-flag regression test')) return null
  const brief = { title: 'Scene flag feature', description: 'Proves per-scene stubbing' }
  let manifest
  await withTempBrief(brief, (briefPath) =>
    withEnv(
      {
        BOSS_PROOF_BRIEF: briefPath,
        BOSS_PROOF_UPLOAD: '0',
        BOSS_PROOF_RUN_ID: runId,
        BOSS_PROOF_RUN_TOKEN: 'tokscene1',
        BOSS_PROOF_PUBLIC_BASE_URL: 'https://proof.test.dev',
      },
      async () => {
        ;({ manifest } = await runAgentProof({
          prNumber: 'local',
          commit: 'abc1234',
          changedFiles: [],
          dryRun: true,
          deps: {
            spawnPlaywright: livePlaywrightStub(scenes),
            uploadBundle: () => {},
            uploadManifest: () => {},
            publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
            collapsePriorProofComments: () => {},
            postComment: () => {},
          },
        }))
      },
    ),
  )
  fs.rmSync(path.join(REPO_ROOT, '.proof', 'pr-local'), { recursive: true, force: true })
  return manifest
}

test('runAgentProof: all-stub scenes → manifest agentRunnerStubbed true, no per-scene field', async (t) => {
  const manifest = await runWithScenes(t, 'test-scene-allstub', [
    { id: 'scene-01', title: 'a', passed: true, missing: [], atMs: 50, liveAgent: false },
    { id: 'scene-02', title: 'b', passed: true, missing: [], atMs: 90, liveAgent: false },
  ])
  if (!manifest) return
  assert.equal(manifest.agentRunnerStubbed, true)
  const captureScenes = manifest.captures[0].scenes
  assert.equal(captureScenes.length, 2)
  for (const s of captureScenes) {
    assert.ok(!('agentRunnerStubbed' in s), 'stub scenes must not carry the field (byte-compat)')
  }
})

test('runAgentProof: mixed scenes → manifest true, only the live scene marked false', async (t) => {
  const manifest = await runWithScenes(t, 'test-scene-mixed', [
    { id: 'scene-01', title: 'live', passed: true, missing: [], atMs: 50, liveAgent: true },
    { id: 'scene-02', title: 'stub', passed: true, missing: [], atMs: 90, liveAgent: false },
  ])
  if (!manifest) return
  assert.equal(manifest.agentRunnerStubbed, true)
  const [live, stub] = manifest.captures[0].scenes
  assert.equal(live.agentRunnerStubbed, false)
  assert.ok(!('agentRunnerStubbed' in stub))
})

test('runAgentProof: all-live scenes → manifest flag absent (unstubbed), every scene false', async (t) => {
  const manifest = await runWithScenes(t, 'test-scene-alllive', [
    { id: 'scene-01', title: 'live1', passed: true, missing: [], atMs: 50, liveAgent: true },
    { id: 'scene-02', title: 'live2', passed: true, missing: [], atMs: 90, liveAgent: true },
  ])
  if (!manifest) return
  assert.ok(
    !('agentRunnerStubbed' in manifest),
    'an all-live capture must leave the manifest flag unset (unstubbed)',
  )
  for (const s of manifest.captures[0].scenes) {
    assert.equal(s.agentRunnerStubbed, false)
  }
})

test('runAgentProof: an honestly-deferred live scene beside a passing sibling → capture PASSED (not failed)', async (t) => {
  // BOS-142 graceful degradation: a liveAgent scene whose prereq was missing
  // defers (passed:false + liveDeferred marker). It must NOT drag the capture
  // red when a sibling passed — failedScenes excludes liveDeferred scenes,
  // mirroring the spec-side computeLivePassed.
  const manifest = await runWithScenes(t, 'test-scene-deferred', [
    {
      id: 'scene-01',
      title: 'live (deferred)',
      passed: false,
      missing: [],
      atMs: 0,
      liveAgent: false,
      liveDeferred: { missing: ['claude', 'tmux'] },
    },
    {
      id: 'scene-02',
      title: 'stub sibling',
      passed: true,
      missing: [],
      atMs: 90,
      liveAgent: false,
    },
  ])
  if (!manifest) return
  const capture = manifest.captures[0]
  assert.equal(capture.status, 'passed', 'a deferral must not fail a passing sibling capture')
  assert.ok(!capture.error, 'no scene(s)-failed error naming the deferral')
  // The run is still honestly stubbed (a deferred scene never ran live).
  assert.equal(manifest.agentRunnerStubbed, true)
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
    // Hermeticity tripwire: this media-carrying run reaches finalize's judge
    // dep, which must have short-circuited on the deleted
    // PROOF_ANTHROPIC_API_KEY — any other value means a real (or attempted)
    // Anthropic call leaked into a unit test.
    assert.deepEqual(manifest.judge, { unjudged: true, reason: 'missing-key' })
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

// ── BOS-140 P3b/P3c: per-scene stills + evidence gate ───────────────────────

test('runAgentProof: findRawStills accepts both legacy and scene-prefixed still names', async () => {
  const { runAgentProof } = await import('./proof-agent.mjs')

  const brief = { title: 'Scene stills feature', description: 'Agent took scene-prefixed shots' }
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        {
          BOSS_PROOF_BRIEF: briefPath,
          BOSS_PROOF_UPLOAD: '0',
          BOSS_PROOF_RUN_ID: 'test-run-scene-stills',
          BOSS_PROOF_RUN_TOKEN: 'tokscenestills1',
          BOSS_PROOF_PUBLIC_BASE_URL: 'https://proof.test.dev',
        },
        async () => {
          const { manifest } = await runAgentProof({
            prNumber: 'local',
            commit: 'cafed00d',
            changedFiles: [],
            dryRun: true,
            deps: {
              spawnPlaywright: ({ localDir }) => {
                const rawDir = path.join(localDir, 'raw')
                fs.mkdirSync(rawDir, { recursive: true })
                fs.writeFileSync(
                  path.join(rawDir, 'proof-result.json'),
                  JSON.stringify({ passed: true, summary: 'ok', evidence: [], steps: 2 }),
                )
                fs.writeFileSync(path.join(rawDir, 'session.webm'), 'fake-webm-data')
                // Legacy name (no scene prefix) and BOS-140 scene-prefixed name.
                fs.writeFileSync(path.join(rawDir, '01-legacy.png'), 'fake-png-1')
                fs.writeFileSync(path.join(rawDir, 'scene-01-02-scoped.png'), 'fake-png-2')
                return { status: 0 }
              },
              uploadBundle: () => {},
              uploadManifest: () => {},
              publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
              collapsePriorProofComments: () => {},
              postComment: () => {},
            },
          })

          const capture = manifest.captures[0]
          assert.equal(capture.stills.length, 2, 'both naming conventions must be picked up')
          const legacy = capture.stills.find((s) => s.fileName.endsWith('01-legacy.png'))
          const scoped = capture.stills.find((s) => s.fileName.endsWith('scene-01-02-scoped.png'))
          assert.ok(legacy, 'legacy NN-label.png still must be found')
          assert.ok(scoped, 'scene-SS-NN-label.png still must be found')
          assert.equal(legacy.sceneId, undefined, 'legacy still has no scene prefix to derive from')
          assert.equal(
            scoped.sceneId,
            'scene-01',
            'scene-01-02-*.png must be attributed to scene-01 (the scene-less brief’s synthetic scene)',
          )
        },
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    fs.rmSync(path.join(REPO_ROOT, '.proof', 'pr-local'), { recursive: true, force: true })
  }
})

test('runAgentProof: a failed scene forces captureShape.status to failed even when the agent passed', async (t) => {
  const { runAgentProof } = await import('./proof-agent.mjs')

  if (!requireFfmpeg(t, 'ffmpeg is required to produce a real mp4 for this regression test')) return

  const brief = { title: 'Failed scene feature', description: 'One of two scenes never proved out' }
  const originalExitCode = process.exitCode
  process.exitCode = undefined

  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        {
          BOSS_PROOF_BRIEF: briefPath,
          BOSS_PROOF_UPLOAD: '0',
          BOSS_PROOF_RUN_ID: 'test-run-scene-fail',
          BOSS_PROOF_RUN_TOKEN: 'tokscenefail1',
          BOSS_PROOF_PUBLIC_BASE_URL: 'https://proof.test.dev',
        },
        async () => {
          const { manifest } = await runAgentProof({
            prNumber: 'local',
            commit: 'deadc0de',
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
                    summary: 'agent believes it passed',
                    evidence: ['ok'],
                    steps: 4,
                    interactions: 2,
                    scenes: [
                      { id: 'scene-01', title: 'Open list', passed: true, missing: [], atMs: 0 },
                      {
                        id: 'scene-02',
                        title: 'Open settings',
                        passed: false,
                        missing: ['Deploy'],
                        atMs: 0,
                      },
                    ],
                  }),
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
                fs.writeFileSync(path.join(rawDir, 'scene-01-01-list.png'), 'fake-png-1')
                return { status: 0 }
              },
              uploadBundle: () => {},
              uploadManifest: () => {},
              publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
              collapsePriorProofComments: () => {},
              postComment: () => {},
            },
          })

          const capture = manifest.captures[0]
          assert.equal(
            capture.status,
            'failed',
            'a failed scene must fail the capture even though the agent said passed',
          )
          assert.match(capture.error, /scene\(s\) failed: scene-02/)
          assert.ok(Array.isArray(capture.scenes), 'capture must carry the per-scene outcomes')
          assert.equal(capture.scenes.length, 2)
          // BOS-140/P3b: a successful post-process (no intro card, so introMs is
          // 0) maps a wall-offset-0 marker to output-clock 0 — proves the
          // timeline produced by postprocessProofVideo threads through to each
          // scene's chapter timestamp.
          assert.equal(capture.scenes[0].outputMs, 0)
          assert.equal(capture.scenes[1].outputMs, 0)
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
  const { resolveAgentBackstopMs, VIDEO_SAVE_HEADROOM_MS, DEFAULT_INNER_BUDGET_MS } =
    await import('./proof-agent.mjs')
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

// ── BOS-139: collect mode (orchestrator owns identity + budget) ──────────────

test('runAgentProof: collect mode returns a SurfaceRun (no finalize), per-surface brief, clamped budget', async () => {
  const { runAgentProof } = await import('./proof-agent.mjs')
  const brief = {
    title: 'Collect web',
    description: 'collect-mode web run',
    budgets: { maxWallClockMs: 720000 },
  }
  const localDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-agent-collect-'))
  let uploadCalled = false
  let postCalled = false
  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv({ BOSS_PROOF_BRIEF: briefPath, BOSS_PROOF_UPLOAD: '0' }, async () => {
        const surfaceRun = await runAgentProof({
          prNumber: 'local',
          commit: 'abc1234',
          changedFiles: [],
          dryRun: true,
          runContext: {
            runId: 'RUN',
            token: 'tok',
            paths: { publicPrefix: 'proof/bossanova/pr-local/abc1234/RUN/tok' },
            localDir,
            briefFileName: 'brief-web.json',
            maxWallClockMs: 60_000,
            collect: true,
          },
          deps: {
            // No webm produced → no-media failed capture; enough to exercise collect.
            spawnPlaywright: () => ({ status: 1 }),
            uploadBundle: () => {
              uploadCalled = true
            },
            postComment: () => {
              postCalled = true
            },
          },
        })
        assert.equal(surfaceRun.surface, 'web')
        assert.ok(Array.isArray(surfaceRun.captureShapes))
        assert.equal(surfaceRun.brief.budgets.maxWallClockMs, 60_000, 'budget clamped to grant')
        assert.equal(surfaceRun.reasonCode, null)
        assert.equal(typeof surfaceRun.elapsedMs, 'number')
        assert.ok(fs.existsSync(path.join(localDir, 'brief-web.json')), 'per-surface brief written')
        assert.equal(uploadCalled, false, 'collect mode must not finalize/upload')
        assert.equal(postCalled, false, 'collect mode must not post a comment')
      }),
    )
  } finally {
    fs.rmSync(localDir, { recursive: true, force: true })
  }
})

// ── BOS-142: live-agent harness-mode decision ────────────────────────────────

test('buildPlaywrightEnv: liveAgent:true sets BOSS_PROOF_LIVE_AGENT and never the deferred key', async () => {
  const { buildPlaywrightEnv } = await import('./proof-agent.mjs')
  const env = buildPlaywrightEnv(
    {},
    { briefPath: 'b.json', localDir: '/out', model: 'claude-haiku-4-5', liveAgent: true },
  )
  assert.equal(env.BOSS_PROOF_LIVE_AGENT, '1')
  assert.equal(env.BOSS_PROOF_LIVE_AGENT_DEFERRED, undefined)
  // The fixed harness keys are still layered over base.
  assert.equal(env.E2E_REAL, '1')
  assert.equal(env.E2E_AGENT_PROOF, '1')
  assert.equal(env.BOSS_PROOF_BRIEF, 'b.json')
  assert.equal(env.BOSS_PROOF_OUT, '/out')
  assert.equal(env.BOSS_PROOF_MODEL, 'claude-haiku-4-5')
})

test('buildPlaywrightEnv: liveAgentDeferred sets the comma-joined deferred key, not the live flag', async () => {
  const { buildPlaywrightEnv } = await import('./proof-agent.mjs')
  const env = buildPlaywrightEnv(
    {},
    { briefPath: 'b.json', localDir: '/out', model: 'm', liveAgentDeferred: ['claude', 'tmux'] },
  )
  assert.equal(env.BOSS_PROOF_LIVE_AGENT_DEFERRED, 'claude,tmux')
  assert.equal(env.BOSS_PROOF_LIVE_AGENT, undefined)
})

test('buildPlaywrightEnv: neither flag → both live-agent env keys unset', async () => {
  const { buildPlaywrightEnv } = await import('./proof-agent.mjs')
  const env = buildPlaywrightEnv({}, { briefPath: 'b.json', localDir: '/out', model: 'm' })
  assert.equal(env.BOSS_PROOF_LIVE_AGENT, undefined)
  assert.equal(env.BOSS_PROOF_LIVE_AGENT_DEFERRED, undefined)
  // An empty deferred array must NOT set the deferred key either.
  const env2 = buildPlaywrightEnv(
    {},
    { briefPath: 'b.json', localDir: '/out', model: 'm', liveAgentDeferred: [] },
  )
  assert.equal(env2.BOSS_PROOF_LIVE_AGENT_DEFERRED, undefined)
})

test('buildPlaywrightEnv: hermetic — an ambient live flag in base can NEVER leak into a non-live run', async () => {
  const { buildPlaywrightEnv } = await import('./proof-agent.mjs')
  // A stray operator/CI export (or a nested run) sets both live-agent keys in
  // the ambient env. A non-live decision (no liveAgent, no deferred ids) must
  // strip them so the child spec never boots live mode / the real runner.
  const env = buildPlaywrightEnv(
    { BOSS_PROOF_LIVE_AGENT: '1', BOSS_PROOF_LIVE_AGENT_DEFERRED: 'stale' },
    { briefPath: 'b.json', localDir: '/out', model: 'm' },
  )
  assert.equal(env.BOSS_PROOF_LIVE_AGENT, undefined, 'ambient live flag must be cleared')
  assert.equal(
    env.BOSS_PROOF_LIVE_AGENT_DEFERRED,
    undefined,
    'ambient deferred key must be cleared',
  )

  // A deferred decision over the same polluted base clears the stale live flag.
  const deferred = buildPlaywrightEnv(
    { BOSS_PROOF_LIVE_AGENT: '1' },
    { briefPath: 'b.json', localDir: '/out', model: 'm', liveAgentDeferred: ['claude'] },
  )
  assert.equal(deferred.BOSS_PROOF_LIVE_AGENT, undefined, 'a deferral must not inherit a live flag')
  assert.equal(deferred.BOSS_PROOF_LIVE_AGENT_DEFERRED, 'claude')
})

// ── BOS-142 T7: consolidated "never launches real runner by default" guarantee ─
// (d) An orchestrator run with no liveAgent scene never sets BOSS_PROOF_LIVE_AGENT.
//     buildPlaywrightEnv is the single gate that maps the internal opts.liveAgent
//     flag onto the BOSS_PROOF_LIVE_AGENT env var that the real-stack harness
//     (services/web/tests/e2e/real/global-setup.ts) reads to decide 'stub' vs
//     'live'. No live scene ⇒ opts has no liveAgent ⇒ the var stays unset ⇒
//     global-setup boots the stub harness, never the real bossd-plugin-claude.
//     The runAgentProof spawnPlaywright-spy test above ('runAgentProof: non-live
//     brief never calls liveAgentPreflight and passes no live flags') proves the
//     upstream half: a non-live run never even puts liveAgent on the opts.
//     Grep tag: "never launches real runner by default".
test('never launches real runner by default (d): no live scene → BOSS_PROOF_LIVE_AGENT stays unset', async () => {
  const { buildPlaywrightEnv } = await import('./proof-agent.mjs')
  const env = buildPlaywrightEnv({}, { briefPath: 'b.json', localDir: '/out', model: 'm' })
  assert.equal(env.BOSS_PROOF_LIVE_AGENT, undefined)
  assert.equal(env.BOSS_PROOF_LIVE_AGENT_DEFERRED, undefined)
})

async function runWithSpies(brief, deps, extraEnv = {}, { planRequiredProof } = {}) {
  const { runAgentProof } = await import('./proof-agent.mjs')
  const originalExitCode = process.exitCode
  process.exitCode = undefined
  try {
    await withTempBrief(brief, (briefPath) =>
      withEnv(
        {
          BOSS_PROOF_BRIEF: briefPath,
          BOSS_PROOF_UPLOAD: '0',
          BOSS_PROOF_RUN_ID: 'test-run-live',
          BOSS_PROOF_RUN_TOKEN: 'toklive1',
          BOSS_PROOF_PUBLIC_BASE_URL: 'https://proof.test.dev',
          ...extraEnv,
        },
        () =>
          runAgentProof({
            prNumber: 'local',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: true,
            planRequiredProof,
            deps: {
              uploadBundle: () => {},
              uploadManifest: () => {},
              publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
              collapsePriorProofComments: () => {},
              postComment: () => {},
              ...deps,
            },
          }),
      ),
    )
  } finally {
    process.exitCode = originalExitCode
    fs.rmSync(path.join(REPO_ROOT, '.proof', 'pr-local'), { recursive: true, force: true })
  }
}

test('runAgentProof: non-live brief never calls liveAgentPreflight and passes no live flags', async () => {
  let preflightCalled = false
  let capturedOpts = null
  await runWithSpies(
    { title: 'Plain feature', description: 'no live scene, no live-agent bullet' },
    {
      spawnPlaywright: (opts) => {
        capturedOpts = opts
        return { status: 1 } // no webm → no-media capture; enough to capture opts
      },
      liveAgentPreflight: () => {
        preflightCalled = true
        return []
      },
    },
  )
  assert.equal(preflightCalled, false, 'preflight must not run when no live scene is requested')
  assert.ok(capturedOpts, 'spawnPlaywright must have been called')
  assert.ok(!('liveAgent' in capturedOpts), 'no liveAgent flag on a non-live run')
  assert.ok(!('liveAgentDeferred' in capturedOpts), 'no liveAgentDeferred flag on a non-live run')
})

test('runAgentProof: a plan `live-agent` bullet ALONE (no live scene) never boots live mode', async () => {
  // Marker-only opt-in must NOT enable live MODE: a generated brief has its
  // scene.liveAgent stripped, so proof.spec.ts would fall through to the plain
  // LLM/stub path — booting the real runner with no live scene to drive would
  // spend without a deterministic gate or disclosure. Live mode is keyed solely
  // off an actual live scene in the validated brief.
  let preflightCalled = false
  let capturedOpts = null
  await runWithSpies(
    { title: 'Plain feature', description: 'no live scene; opt-in only via a plan bullet' },
    {
      spawnPlaywright: (opts) => {
        capturedOpts = opts
        return { status: 1 }
      },
      liveAgentPreflight: () => {
        preflightCalled = true
        return []
      },
    },
    {},
    { planRequiredProof: ['demonstrate a live-agent Claude session end to end'] },
  )
  assert.equal(preflightCalled, false, 'a plan bullet alone must not trigger the live preflight')
  assert.ok(capturedOpts, 'spawnPlaywright must have been called')
  assert.ok(!('liveAgent' in capturedOpts), 'no liveAgent flag from a marker-only brief')
  assert.ok(!('liveAgentDeferred' in capturedOpts), 'no deferred flag from a marker-only brief')
})

test('runAgentProof: live scene + all prereqs present → spawnPlaywright gets liveAgent:true', async () => {
  let capturedOpts = null
  await runWithSpies(
    {
      title: 'Live feature',
      description: 'has a live scene',
      scenes: [{ id: 'live', title: 'live flow', expectedEvidence: ['X'], liveAgent: true }],
    },
    {
      spawnPlaywright: (opts) => {
        capturedOpts = opts
        return { status: 1 }
      },
      liveAgentPreflight: () => [], // all prereqs present
    },
  )
  assert.ok(capturedOpts, 'spawnPlaywright must have been called')
  assert.equal(capturedOpts.liveAgent, true, 'a satisfied live scene runs genuinely live')
  assert.ok(!('liveAgentDeferred' in capturedOpts), 'no deferred ids when nothing is missing')
})

test('runAgentProof: live scene + a missing prereq → spawnPlaywright gets liveAgentDeferred, not liveAgent', async () => {
  let capturedOpts = null
  await runWithSpies(
    {
      title: 'Live feature',
      description: 'has a live scene but claude is missing',
      scenes: [{ id: 'live', title: 'live flow', expectedEvidence: ['X'], liveAgent: true }],
    },
    {
      spawnPlaywright: (opts) => {
        capturedOpts = opts
        return { status: 1 }
      },
      liveAgentPreflight: () => ['claude'], // one prereq missing → defer
    },
  )
  assert.ok(capturedOpts, 'spawnPlaywright must have been called')
  assert.ok(Array.isArray(capturedOpts.liveAgentDeferred), 'deferred ids must be passed')
  assert.ok(capturedOpts.liveAgentDeferred.includes('claude'), 'the missing id is named')
  assert.ok(!('liveAgent' in capturedOpts), 'a deferred live run must NOT set liveAgent:true')
})
