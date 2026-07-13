#!/usr/bin/env node
/**
 * proof-multisurface-t13.test.mjs — BOS-139 T13 (D15) mechanical acceptance.
 *
 * Unattended-run acceptance: a mixed TUI+web run must MECHANICALLY post exactly
 * ONE consolidated proof comment carrying BOTH a TUI section and a Web section
 * (each ✅ proven or an honest ⏸ deferral), with prior proof comments collapsed
 * exactly once. Drives finalizeAgentProof with a two-element surfaceRuns[] and
 * fully-stubbed external effects (no R2, no gh, no network), so it runs in CI
 * without any proof credentials — the same consolidated-comment machinery a real
 * cron run exercises, minus the live upload/post transport.
 */

import assert from 'node:assert/strict'
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
// in). The `deps` objects below are partial (no `judge`), so drop the key for
// this file's test process: judgeProof then short-circuits to
// { unjudged: true, reason: 'missing-key' } BEFORE constructing the SDK
// (unit-proven in proof-judge.test.mjs), keeping every current and future
// finalize call in this file network-free. node --test runs each file in its
// own process, so this cannot leak into other files.
delete process.env.PROOF_ANTHROPIC_API_KEY

const tuiRun = (extra) => ({
  surface: 'tui',
  brief: { title: 'Mixed-surface fixture', genAi: false },
  hasFailure: false,
  noSurface: false,
  reasonCode: null,
  agentResult: { passed: true, summary: 'Opened settings; Compact mode row visible', steps: 4 },
  captureShapes: [
    {
      recipeId: 'tui-agent',
      title: 'Mixed-surface fixture',
      surface: 'tui',
      privacy: 'fixture',
      status: 'passed',
      mediaType: 'mp4',
      fileName: 'tui-agent/tui-agent.mp4',
      posterFileName: 'tui-agent/tui-agent.png',
      stills: [{ fileName: 'tui-agent/frame-01.png', label: 'frame 01' }],
    },
  ],
  ...extra,
})

test('T13: mixed TUI+web run posts ONE consolidated two-section comment (web proven, TUI proven)', async () => {
  const localDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-t13-'))
  let collapseCalls = 0
  let postCalls = 0
  const savedExit = process.exitCode
  try {
    const webRun = {
      surface: 'web',
      brief: { title: 'Mixed-surface fixture', genAi: false },
      hasFailure: false,
      noSurface: false,
      reasonCode: null,
      agentResult: { passed: true, summary: 'Toggled Compact mode on /account', steps: 3 },
      captureShapes: [
        {
          recipeId: 'agent-proof',
          title: 'Mixed-surface fixture',
          surface: 'web',
          privacy: 'fixture',
          status: 'passed',
          mediaType: 'mp4',
          fileName: 'agent-proof/agent-proof.mp4',
          posterFileName: 'agent-proof/agent-proof.png',
          stills: [{ fileName: 'agent-proof/still-01.png', label: 's1' }],
        },
      ],
    }

    const { manifest, commentBody } = await finalizeAgentProof({
      surfaceRuns: [tuiRun(), webRun],
      prNumber: '4242',
      commit: 'abc1234',
      runId: 'RUN',
      token: 'tok',
      paths: { publicPrefix: 'proof/bossanova/pr-4242/abc1234/RUN/tok' },
      localDir,
      publicBaseUrl: 'https://proof.example/pr',
      shouldUpload: true,
      bucket: 'test-bucket',
      deps: {
        uploadBundle: () => {},
        publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
        uploadManifest: () => {},
        currentRepoIdentity: () => null,
        currentBranch: () => 'fixture-branch',
        collapsePriorProofComments: () => {
          collapseCalls += 1
        },
        postComment: () => {
          postCalls += 1
        },
      },
    })

    // Exactly one live comment: collapse prior once, post once.
    assert.equal(collapseCalls, 1, 'prior proof comments collapsed exactly once')
    assert.equal(postCalls, 1, 'exactly one consolidated comment posted')

    // The posted body IS the consolidated comment with BOTH surface sections.
    const marker = '<!-- bossanova-proof:pr-4242 -->'
    assert.equal(commentBody.split(marker).length - 1, 1, 'exactly one marker (one live comment)')
    assert.ok(commentBody.includes('#### TUI — ✅ proven'), 'TUI section present')
    assert.ok(commentBody.includes('#### Web — ✅ proven'), 'Web section present')

    // Hermeticity tripwire: this media-carrying run DOES reach the judge dep,
    // which must have short-circuited on the deleted key — any other value
    // here means a real (or attempted) Anthropic call leaked into a unit test.
    assert.deepEqual(manifest.judge, { unjudged: true, reason: 'missing-key' })
  } finally {
    process.exitCode = savedExit
    fs.rmSync(localDir, { recursive: true, force: true })
  }
})

test('T13: missing R2 bucket defers env-unavailable — posts the comment, skips upload, exit 0', async () => {
  // Regression (Codex outside-voice P1): a missing BOSS_PROOF_R2_BUCKET is a
  // shared upload prerequisite. The dispatcher resolves the bucket lazily (null)
  // and the per-surface preflight defers every surface as env-unavailable, so
  // finalize is called with shouldUpload:true but bucket:null. It must still post
  // the honest consolidated deferral WITHOUT attempting an upload to a null
  // bucket (which would crash to a pipeline-error / exit 1).
  const localDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-t13-nobucket-'))
  let uploadCalls = 0
  let postCalls = 0
  const savedExit = process.exitCode
  try {
    const deferred = (surface) => ({
      surface,
      brief: {},
      hasFailure: false,
      noSurface: false,
      reasonCode: 'env-unavailable',
      missing: ['BOSS_PROOF_R2_BUCKET'],
      agentResult: { passed: false, summary: '', steps: 0 },
      captureShapes: [],
    })
    const { manifest, commentBody } = await finalizeAgentProof({
      surfaceRuns: [deferred('tui'), deferred('web')],
      prNumber: '4244',
      commit: 'abc1234',
      runId: 'RUN',
      token: 'tok',
      paths: { publicPrefix: 'proof/bossanova/pr-4244/abc1234/RUN/tok' },
      localDir,
      publicBaseUrl: 'https://proof.example/pr',
      shouldUpload: true,
      bucket: null,
      deps: {
        uploadBundle: () => {
          uploadCalls += 1
        },
        publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
        uploadManifest: () => {},
        currentRepoIdentity: () => null,
        currentBranch: () => 'fixture-branch',
        collapsePriorProofComments: () => {},
        postComment: () => {
          postCalls += 1
        },
      },
    })
    assert.equal(uploadCalls, 0, 'no upload attempted with a null bucket')
    assert.equal(postCalls, 1, 'honest consolidated deferral still posted')
    assert.ok(commentBody.includes('env-unavailable'), 'env-unavailable reason surfaced')
    assert.ok(!commentBody.includes('[📸 Proof manifest]'), 'no link to an unuploaded manifest')
    assert.equal(manifest.publicBaseUrl, undefined, 'manifest omits unuploaded public base URL')
    assert.notEqual(process.exitCode, 1, 'env-unavailable deferral exits 0')
  } finally {
    process.exitCode = savedExit
    fs.rmSync(localDir, { recursive: true, force: true })
  }
})

test('T13: partial mixed run posts ONE comment with a proven + a deferred section (exit 0)', async () => {
  const localDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-t13-partial-'))
  let postCalls = 0
  const savedExit = process.exitCode
  try {
    const { commentBody } = await finalizeAgentProof({
      surfaceRuns: [
        tuiRun(),
        // Web never ran — the shared budget was consumed by the TUI run.
        {
          surface: 'web',
          brief: { title: 'Mixed-surface fixture', genAi: false },
          hasFailure: false,
          noSurface: false,
          reasonCode: 'budget-exceeded',
          agentResult: { passed: false, summary: '', steps: 0 },
          captureShapes: [],
        },
      ],
      prNumber: '4243',
      commit: 'abc1234',
      runId: 'RUN',
      token: 'tok',
      paths: { publicPrefix: 'proof/bossanova/pr-4243/abc1234/RUN/tok' },
      localDir,
      publicBaseUrl: 'https://proof.example/pr',
      shouldUpload: true,
      bucket: 'test-bucket',
      deps: {
        uploadBundle: () => {},
        publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
        uploadManifest: () => {},
        currentRepoIdentity: () => null,
        currentBranch: () => 'fixture-branch',
        collapsePriorProofComments: () => {},
        postComment: () => {
          postCalls += 1
        },
      },
    })
    assert.equal(postCalls, 1, 'exactly one consolidated comment posted')
    assert.ok(commentBody.includes('#### TUI — ✅ proven'), 'TUI proven section present')
    assert.ok(
      commentBody.includes('#### Web — ⏸ deferred (budget-exceeded)'),
      'Web deferred section present',
    )
    assert.ok(!commentBody.includes('❌'), 'partial success carries no global red verdict')
    // Partial success is neutral: it must not redden the PR.
    assert.notEqual(process.exitCode, 1, 'partial success exits 0')
  } finally {
    process.exitCode = savedExit
    fs.rmSync(localDir, { recursive: true, force: true })
  }
})
