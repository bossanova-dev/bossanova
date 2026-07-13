#!/usr/bin/env node
/**
 * proof-agent-finalize.judge-label.test.mjs — Finalize-orchestrator wiring for
 * the `proof-invalid` label (BOS-141 D12, Task 5).
 *
 * The pure decision (`labelActionForJudge`) and argv builders
 * (ensureLabelCommand/addLabelCommand/removeLabelCommand) are unit-tested in
 * proof-judge.test.mjs. This file only tests that finalizeAgentProof calls
 * the `applyJudgeLabel` dep with the right action at the right time, and that
 * a throwing dep never affects the returned manifest or process.exitCode.
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

const webCap = (status) => ({
  recipeId: 'agent-proof',
  title: 't',
  surface: 'web',
  privacy: 'fixture',
  status,
  mediaType: 'mp4',
  fileName: 'agent-proof/agent-proof.mp4',
  posterFileName: 'agent-proof/agent-proof.png',
  stills: [{ fileName: 'agent-proof/still-01.png', label: 's1' }],
})

// Posting deps (postComment/collapsePriorProofComments/uploadBundle) must be
// stubbed whenever a test sets shouldUpload:true, or finalize would try to
// shell out to gh/wrangler for real.
function postingStubDeps({ judge, applyJudgeLabel }) {
  return {
    uploadBundle: () => {},
    publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
    collapsePriorProofComments: () => {},
    postComment: () => {},
    uploadManifest: () => {},
    currentRepoIdentity: () => null,
    currentBranch: () => 'test-branch',
    judge,
    applyJudgeLabel,
  }
}

async function run({ deps, shouldUpload, bucket = null, prNumber = '123' }) {
  const localDir = fs.mkdtempSync(path.join(os.tmpdir(), 'finalize-judge-label-'))
  const savedExit = process.exitCode
  try {
    const r = await finalizeAgentProof({
      brief: { title: 'Label wiring brief', genAi: false },
      agentResult: { passed: true, summary: 'ok', evidence: [], steps: 1 },
      hasFailure: false,
      prNumber,
      commit: 'abc1234',
      runId: 'RUN',
      token: 'tok-fixed',
      paths: { publicPrefix: 'proof/bossanova/pr-123/abc1234/RUN/tok-fixed' },
      localDir,
      publicBaseUrl: 'https://proof.example/pr',
      shouldUpload,
      bucket,
      captureShapes: [webCap('passed')],
      deps,
    })
    return { ...r, exitCode: process.exitCode ?? 0 }
  } finally {
    process.exitCode = savedExit
    fs.rmSync(localDir, { recursive: true, force: true })
  }
}

test('finalize: unsatisfactory judge + posting -> applyJudgeLabel called with add', async () => {
  const calls = []
  const deps = postingStubDeps({
    judge: async () => ({ evidence: 'unsatisfactory', confidence: 'high' }),
    applyJudgeLabel: async (opts) => calls.push(opts),
  })
  await run({ deps, shouldUpload: true, bucket: 'test-bucket' })
  assert.deepEqual(calls, [{ action: 'add', prNumber: '123' }])
})

test('finalize: satisfactory judge + posting -> applyJudgeLabel called with remove', async () => {
  const calls = []
  const deps = postingStubDeps({
    judge: async () => ({ evidence: 'satisfactory', confidence: 'high' }),
    applyJudgeLabel: async (opts) => calls.push(opts),
  })
  await run({ deps, shouldUpload: true, bucket: 'test-bucket' })
  assert.deepEqual(calls, [{ action: 'remove', prNumber: '123' }])
})

test('finalize: prNumber "local" -> applyJudgeLabel not called', async () => {
  const calls = []
  const deps = postingStubDeps({
    judge: async () => ({ evidence: 'unsatisfactory', confidence: 'high' }),
    applyJudgeLabel: async (opts) => calls.push(opts),
  })
  await run({ deps, shouldUpload: true, bucket: 'test-bucket', prNumber: 'local' })
  assert.deepEqual(calls, [])
})

test('finalize: shouldUpload false -> applyJudgeLabel not called', async () => {
  const calls = []
  const deps = postingStubDeps({
    judge: async () => ({ evidence: 'unsatisfactory', confidence: 'high' }),
    applyJudgeLabel: async (opts) => calls.push(opts),
  })
  await run({ deps, shouldUpload: false })
  assert.deepEqual(calls, [])
})

test('finalize: a throwing applyJudgeLabel dep does not fail finalize (manifest + exit code untouched)', async () => {
  const deps = postingStubDeps({
    judge: async () => ({ evidence: 'unsatisfactory', confidence: 'high' }),
    applyJudgeLabel: async () => {
      throw new Error('gh exploded')
    },
  })
  const { manifest, exitCode } = await run({ deps, shouldUpload: true, bucket: 'test-bucket' })
  assert.equal(exitCode, 0)
  assert.equal(manifest.judge.evidence, 'unsatisfactory')
})
