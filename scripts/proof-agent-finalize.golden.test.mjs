#!/usr/bin/env node
/**
 * proof-agent-finalize.golden.test.mjs — Golden tests pinning the EXACT comment
 * output of finalizeAgentProof.
 *
 * History: these were first written (BOS-139 T5/D7) to pin the pre-split output
 * and passed byte-identical across the behavior-preserving finalize split. In
 * Task 6 (P2b) they were DELIBERATELY regenerated once for the consolidated
 * per-surface (sectioned) layout — the single sanctioned golden change — and
 * extended with multi-surface shapes (both-passed / partial / both-deferred).
 *
 * Expected strings are machine-generated (never hand-written): each was produced
 * by running the shape once and capturing commentBody verbatim.
 */

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { test } from 'node:test'

import { finalizeAgentProof } from './proof-agent-finalize.mjs'

const stubDeps = () => ({
  uploadBundle: () => {},
  publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
  collapsePriorProofComments: () => {},
  postComment: () => {},
  uploadManifest: () => {},
  currentRepoIdentity: () => null,
  currentBranch: () => 'test-branch',
  // BOS-141 Task 4: the judge is advisory-only and never wired to the real
  // network in tests. The stubbed `unjudged` verdict drives the judge-led
  // render seam (D12): every consolidated comment carries the
  // `### 📸 Proof (unjudged)` headline, and every passed section's verdict
  // block gains the `_Self-graded (unjudged)_` label. Exit codes and the
  // per-surface ✅/⏸ markers stay untouched (D6 — the judge never grades them).
  judge: async () => ({ unjudged: true, reason: 'stubbed' }),
  // BOS-141 Task 5: these goldens all run with shouldUpload:false, so the
  // applyJudgeLabel call site is skipped — this stub exists only so a future
  // golden that flips shouldUpload:true never shells out to gh.
  applyJudgeLabel: async () => {},
})

// Runs finalize with fixed identity so output is deterministic. Restores
// process.exitCode around the call so a degraded shape does not leak exit 1
// into the test runner. Returns { manifest, commentBody, exitCode }.
async function run(overrides) {
  const localDir = fs.mkdtempSync(path.join(os.tmpdir(), 'finalize-golden-'))
  const savedExit = process.exitCode
  try {
    const r = await finalizeAgentProof({
      brief: { title: 'Golden brief', genAi: false },
      agentResult: { passed: true, summary: 'ok', evidence: [], steps: 3 },
      hasFailure: false,
      prNumber: '123',
      commit: 'abc1234',
      runId: 'RUN',
      token: 'tok-fixed',
      paths: { publicPrefix: 'proof/bossanova/pr-123/abc1234/RUN/tok-fixed' },
      localDir,
      publicBaseUrl: 'https://proof.example/pr',
      shouldUpload: false,
      bucket: null,
      deps: stubDeps(),
      ...overrides,
    })
    return { ...r, exitCode: process.exitCode ?? 0 }
  } finally {
    process.exitCode = savedExit
    fs.rmSync(localDir, { recursive: true, force: true })
  }
}

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
const tuiCap = (status) => ({
  recipeId: 'tui-agent',
  title: 't',
  surface: 'tui',
  privacy: 'fixture',
  status,
  mediaType: 'mp4',
  fileName: 'tui-agent/tui-agent.mp4',
  posterFileName: 'tui-agent/tui-agent.png',
  stills: [{ fileName: 'tui-agent/frame-01.png', label: 'frame 01' }],
})
// BOS-140 P3d: scene-carrying capture shapes for the two new chaptered
// goldens below. `scenes` mirrors the real captureShape.scenes produced by
// evaluateSceneEvidence (TUI)/the web tracker's finish() — {id, title,
// passed, missing, outputMs}.
const tuiCapTwoScenesPassed = () => ({
  recipeId: 'tui-agent',
  title: 't',
  surface: 'tui',
  privacy: 'fixture',
  status: 'passed',
  mediaType: 'mp4',
  fileName: 'tui-agent/tui-agent.mp4',
  posterFileName: 'tui-agent/tui-agent.png',
  stills: [
    {
      fileName: 'tui-agent/scene-01-frame-01.png',
      label: 'scene 01 frame 01',
      sceneId: 'scene-01',
    },
  ],
  scenes: [
    { id: 'scene-01', title: 'open repos', passed: true, missing: [], outputMs: 12000 },
    { id: 'scene-02', title: 'rename session', passed: true, missing: [], outputMs: 45000 },
  ],
})
// Reachable shape (fixed post-review, BOS-140 P3d fixup): a failed scene
// forces the WHOLE capture to status:'failed' and the run's hasFailure:true —
// the real pipeline (proof-agent.mjs) can never produce status:'passed' +
// hasFailure:false alongside a scene with passed:false.
const webCapTwoScenesOneFailed = () => ({
  recipeId: 'agent-proof',
  title: 't',
  surface: 'web',
  privacy: 'fixture',
  status: 'failed',
  mediaType: 'mp4',
  fileName: 'agent-proof/agent-proof.mp4',
  posterFileName: 'agent-proof/agent-proof.png',
  stills: [{ fileName: 'agent-proof/scene-01-01-a.png', label: 'a', sceneId: 'scene-01' }],
  scenes: [
    { id: 'scene-01', title: 'open sessions', passed: true, missing: [], outputMs: 8000 },
    {
      id: 'scene-02',
      title: 'rename a session',
      passed: false,
      missing: ['Renamed'],
      outputMs: 30000,
    },
  ],
})

const multiRun = (surface, extra) => ({
  surface,
  brief: { title: 'Multi brief', genAi: false },
  hasFailure: false,
  noSurface: false,
  reasonCode: null,
  ...extra,
})

test('golden: web passed → sectioned proven', async () => {
  const { commentBody, manifest, exitCode } = await run({
    captureShapes: [webCap('passed')],
    agentResult: { passed: true, summary: 'ok', evidence: [], steps: 3 },
    hasFailure: false,
  })
  assert.equal(
    commentBody,
    '<!-- bossanova-proof:pr-123 -->\n### 📸 Proof (unjudged)\n\n### [📸 Proof manifest](https://proof.example/pr/manifest.json)\n\n**Golden brief**\n\n**Commit:** `abc1234`  **Run:** RUN  **Gen-AI:** not live (UI-only demo)\n\n#### Web — ✅ proven\n\n<details><summary>Agent summary</summary>\n\nok\n\n</details>\n\n✅ **Evidence:** Satisfactory\n\n✅ **Confidence:** High\n\n_Self-graded (unjudged): judge unavailable — stubbed._\n',
  )
  assert.equal(manifest.deferred ?? false, false)
  assert.equal(exitCode, 0)
})

test('golden: web agent-incomplete → deferred (exit 1)', async () => {
  const { commentBody, manifest, exitCode } = await run({
    captureShapes: [webCap('failed')],
    agentResult: { passed: false, summary: 'agent stopped early', evidence: [], steps: 2 },
    hasFailure: true,
  })
  assert.equal(
    commentBody,
    '<!-- bossanova-proof:pr-123 -->\n### 📸 Proof (unjudged)\n\n### [📸 Proof manifest](https://proof.example/pr/manifest.json)\n\n**Golden brief**\n\n**Commit:** `abc1234`  **Run:** RUN  **Gen-AI:** not live (UI-only demo)\n\n#### Web — ⏸ deferred (agent-incomplete)\n\nThe bs-proof agent ran but did not produce a complete capture this run. See the run log / manifest below for what it produced. This is a proof-run shortfall to investigate, not a confirmed problem with the change.\n\nRe-capture from a dev environment:\n\n```bash\nBOSS_PROOF_AGENT_SURFACE=web node scripts/proof.mjs run\n```\n',
  )
  assert.equal(manifest.deferred ?? false, true)
  assert.equal(exitCode, 1)
})

test('golden: passed but no media → no-media deferred (exit 0)', async () => {
  const { commentBody, manifest, exitCode } = await run({
    captureShapes: [
      { recipeId: 'agent-proof', title: 't', surface: 'web', privacy: 'fixture', status: 'passed' },
    ],
    agentResult: { passed: true, summary: 'ok', evidence: [], steps: 3 },
    hasFailure: false,
  })
  assert.equal(
    commentBody,
    '<!-- bossanova-proof:pr-123 -->\n### 📸 Proof (unjudged)\n\n### [📸 Proof manifest](https://proof.example/pr/manifest.json)\n\n**Golden brief**\n\n**Commit:** `abc1234`  **Run:** RUN  **Gen-AI:** not live (UI-only demo)\n\n#### Web — ⏸ deferred (no-media)\n\nThe bs-proof run finished without producing any media to post. See the manifest below for details. This is a proof-run shortfall to investigate, not a confirmed problem with the change.\n\nRe-capture from a dev environment:\n\n```bash\nBOSS_PROOF_AGENT_SURFACE=web node scripts/proof.mjs run\n```\n',
  )
  assert.equal(manifest.deferred ?? false, true)
  assert.equal(exitCode, 0)
})

test('golden: noSurface → no-ui-surface deferred (exit 0)', async () => {
  const { commentBody, manifest, exitCode } = await run({
    captureShapes: [
      { recipeId: 'agent-proof', title: 't', surface: 'web', privacy: 'fixture', status: 'failed' },
    ],
    agentResult: { passed: false, summary: 'no surface', evidence: [], steps: 1 },
    hasFailure: true,
    noSurface: true,
  })
  assert.equal(
    commentBody,
    "<!-- bossanova-proof:pr-123 -->\n### 📸 Proof (unjudged)\n\n### [📸 Proof manifest](https://proof.example/pr/manifest.json)\n\n**Golden brief**\n\n**Commit:** `abc1234`  **Run:** RUN  **Gen-AI:** not live (UI-only demo)\n\n#### Web — ⏸ deferred (no-ui-surface)\n\nNo web UI surface to demonstrate for this change. The web proof agent drives the running app to show a user-visible change (a page, component, or interaction); this PR changes code with no such surface, so there is nothing to capture in a video. See the PR's diff and tests for the change. This is expected, not a problem with the change.\n",
  )
  assert.equal(manifest.deferred ?? false, true)
  assert.equal(exitCode, 0)
})

test('golden: TUI passed (stills + mp4) → sectioned proven', async () => {
  const { commentBody, manifest, exitCode } = await run({
    captureShapes: [tuiCap('passed')],
    agentResult: { passed: true, summary: 'ok', evidence: [], steps: 5 },
    hasFailure: false,
  })
  assert.equal(
    commentBody,
    '<!-- bossanova-proof:pr-123 -->\n### 📸 Proof (unjudged)\n\n### [📸 Proof manifest](https://proof.example/pr/manifest.json)\n\n**Golden brief**\n\n**Commit:** `abc1234`  **Run:** RUN  **Gen-AI:** not live (UI-only demo)\n\n#### TUI — ✅ proven\n\n<details><summary>Agent summary</summary>\n\nok\n\n</details>\n\n✅ **Evidence:** Satisfactory\n\n✅ **Confidence:** High\n\n_Self-graded (unjudged): judge unavailable — stubbed._\n',
  )
  assert.equal(manifest.deferred ?? false, false)
  assert.equal(exitCode, 0)
})

// BOS-140 P3d: two new goldens locking the chaptered comment shape. Both
// generated by running finalizeAgentProof once and pasting commentBody
// verbatim — never hand-written (per the plan's Step 1).

test('golden: 2-scene TUI passed → sectioned proven with chapter list', async () => {
  const { commentBody, manifest, exitCode } = await run({
    captureShapes: [tuiCapTwoScenesPassed()],
    agentResult: { passed: true, summary: 'ok', evidence: [], steps: 5 },
    hasFailure: false,
  })
  assert.equal(
    commentBody,
    '<!-- bossanova-proof:pr-123 -->\n### 📸 Proof (unjudged)\n\n### [📸 Proof manifest](https://proof.example/pr/manifest.json)\n\n**Golden brief**\n\n**Commit:** `abc1234`  **Run:** RUN  **Gen-AI:** not live (UI-only demo)\n\n#### TUI — ✅ proven\n\n<details><summary>Agent summary</summary>\n\nok\n\n</details>\n\n✅ **Evidence:** Satisfactory\n\n✅ **Confidence:** High\n\n_Self-graded (unjudged): judge unavailable — stubbed._\n\n**Scenes:**\n- [0:12] ✅ [Scene 1 — open repos](https://proof.example/pr/tui-agent/tui-agent.mp4#t=12)\n- [0:45] ✅ [Scene 2 — rename session](https://proof.example/pr/tui-agent/tui-agent.mp4#t=45)\n',
  )
  assert.equal(manifest.deferred ?? false, false)
  assert.equal(exitCode, 0)
})

test('golden: 2-scene web with one failed scene → deferred section carries a ✗ chapter', async () => {
  const { commentBody, manifest, exitCode } = await run({
    captureShapes: [webCapTwoScenesOneFailed()],
    agentResult: { passed: false, summary: 'ok', evidence: [], steps: 4 },
    hasFailure: true,
  })
  assert.equal(
    commentBody,
    '<!-- bossanova-proof:pr-123 -->\n### 📸 Proof (unjudged)\n\n### [📸 Proof manifest](https://proof.example/pr/manifest.json)\n\n**Golden brief**\n\n**Commit:** `abc1234`  **Run:** RUN  **Gen-AI:** not live (UI-only demo)\n\n#### Web — ⏸ deferred (agent-incomplete)\n\nThe bs-proof agent ran but did not produce a complete capture this run. See the run log / manifest below for what it produced. This is a proof-run shortfall to investigate, not a confirmed problem with the change.\n\nRe-capture from a dev environment:\n\n```bash\nBOSS_PROOF_AGENT_SURFACE=web node scripts/proof.mjs run\n```\n\n**Scenes:**\n- [0:08] ✅ [Scene 1 — open sessions](https://proof.example/pr/agent-proof/agent-proof.mp4#t=8)\n- [0:30] ✗ [Scene 2 — rename a session](https://proof.example/pr/agent-proof/agent-proof.mp4#t=30) (missing: Renamed)\n',
  )
  assert.equal(manifest.deferred ?? false, true)
  assert.equal(exitCode, 1)
})

test('golden: pipeline-error → pipeline-error deferred (exit 1)', async () => {
  const { commentBody, manifest, exitCode } = await run({
    captureShapes: [
      { recipeId: 'agent-proof', title: 't', surface: 'web', privacy: 'fixture', status: 'failed' },
    ],
    agentResult: { passed: false, summary: 'crashed', evidence: [], steps: 0 },
    hasFailure: true,
    pipelineError: { stage: 'render', message: 'boom', stderrTail: 'Error: boom\n  at x' },
  })
  assert.equal(
    commentBody,
    '<!-- bossanova-proof:pr-123 -->\n### 📸 Proof (unjudged)\n\n### [📸 Proof manifest](https://proof.example/pr/manifest.json)\n\n**Golden brief**\n\n**Commit:** `abc1234`  **Run:** RUN  **Gen-AI:** not live (UI-only demo)\n\n#### Web — ⏸ deferred (pipeline-error)\n\nThe bs-proof pipeline hit an internal error while capturing this change. This is a defect in the proof tooling that must be fixed — the failed stage and error output are shown below. It is not a problem with the change itself.\n\n**Failed stage:** `render`\n\nError output (tail):\n\n```\nError: boom\n  at x\n```\n\nRe-capture from a dev environment:\n\n```bash\nBOSS_PROOF_AGENT_SURFACE=web node scripts/proof.mjs run\n```\n',
  )
  assert.equal(manifest.deferred ?? false, true)
  assert.equal(exitCode, 1)
})

test('golden: two-surface both passed → two proven sections', async () => {
  const { commentBody, manifest, exitCode } = await run({
    surfaceRuns: [
      multiRun('tui', {
        captureShapes: [tuiCap('passed')],
        agentResult: { passed: true, summary: 'tui ok', steps: 5 },
      }),
      multiRun('web', {
        captureShapes: [webCap('passed')],
        agentResult: { passed: true, summary: 'web ok', steps: 3 },
      }),
    ],
  })
  assert.equal(
    commentBody,
    '<!-- bossanova-proof:pr-123 -->\n### 📸 Proof (unjudged)\n\n### [📸 Proof manifest](https://proof.example/pr/manifest.json)\n\n**Multi brief**\n\n**Commit:** `abc1234`  **Run:** RUN  **Gen-AI:** not live (UI-only demo)\n\n#### TUI — ✅ proven\n\n<details><summary>Agent summary</summary>\n\ntui ok\n\n</details>\n\n✅ **Evidence:** Satisfactory\n\n✅ **Confidence:** High\n\n_Self-graded (unjudged): judge unavailable — stubbed._\n\n#### Web — ✅ proven\n\n<details><summary>Agent summary</summary>\n\nweb ok\n\n</details>\n\n✅ **Evidence:** Satisfactory\n\n✅ **Confidence:** High\n\n_Self-graded (unjudged): judge unavailable — stubbed._\n',
  )
  assert.equal(manifest.deferred ?? false, false)
  assert.equal(exitCode, 0)
})

test('golden: two-surface partial (web proven, TUI deferred) → exit 0', async () => {
  const { commentBody, manifest, exitCode } = await run({
    surfaceRuns: [
      multiRun('web', {
        captureShapes: [webCap('passed')],
        agentResult: { passed: true, summary: 'web ok', steps: 3 },
      }),
      multiRun('tui', {
        captureShapes: [],
        agentResult: { passed: false, summary: '', steps: 0 },
        reasonCode: 'budget-exceeded',
      }),
    ],
  })
  assert.equal(
    commentBody,
    '<!-- bossanova-proof:pr-123 -->\n### 📸 Proof (unjudged)\n\n### [📸 Proof manifest](https://proof.example/pr/manifest.json)\n\n**Multi brief**\n\n**Commit:** `abc1234`  **Run:** RUN  **Gen-AI:** not live (UI-only demo)\n\n#### Web — ✅ proven\n\n<details><summary>Agent summary</summary>\n\nweb ok\n\n</details>\n\n✅ **Evidence:** Satisfactory\n\n✅ **Confidence:** High\n\n_Self-graded (unjudged): judge unavailable — stubbed._\n\n#### TUI — ⏸ deferred (budget-exceeded)\n\nThis surface was deferred because the shared proof budget (~15min per run) was consumed by an earlier surface in this multi-surface PR. The change itself is fine; re-run proof for this surface alone to capture it.\n\nRe-capture from a dev environment:\n\n```bash\nBOSS_PROOF_AGENT_SURFACE=tui node scripts/proof.mjs run\n```\n',
  )
  assert.equal(manifest.deferred ?? false, true)
  assert.equal(exitCode, 0)
})

test('golden: two-surface both deferred → two deferred sections (exit 0)', async () => {
  const { commentBody, manifest, exitCode } = await run({
    surfaceRuns: [
      multiRun('tui', {
        captureShapes: [],
        agentResult: { passed: false, summary: '', steps: 0 },
        reasonCode: 'env-unavailable',
      }),
      multiRun('web', {
        captureShapes: [],
        agentResult: { passed: false, summary: '', steps: 0 },
        reasonCode: 'budget-exceeded',
      }),
    ],
  })
  assert.equal(
    commentBody,
    '<!-- bossanova-proof:pr-123 -->\n### 📸 Proof (unjudged)\n\n### [📸 Proof manifest](https://proof.example/pr/manifest.json)\n\n**Multi brief**\n\n**Commit:** `abc1234`  **Run:** RUN  **Gen-AI:** not live (UI-only demo)\n\n#### TUI — ⏸ deferred (env-unavailable)\n\nbs-proof could not run because required prerequisites are missing in this environment: one or more prerequisites. Provision the missing keys/toolchain (run `node scripts/proof.mjs doctor` to see the full report) and re-run. This is a provisioning gap, not a problem with the change.\n\nRe-capture from a dev environment:\n\n```bash\nBOSS_PROOF_AGENT_SURFACE=tui node scripts/proof.mjs run\n```\n\n#### Web — ⏸ deferred (budget-exceeded)\n\nThis surface was deferred because the shared proof budget (~15min per run) was consumed by an earlier surface in this multi-surface PR. The change itself is fine; re-run proof for this surface alone to capture it.\n\nRe-capture from a dev environment:\n\n```bash\nBOSS_PROOF_AGENT_SURFACE=web node scripts/proof.mjs run\n```\n',
  )
  assert.equal(manifest.deferred ?? false, true)
  assert.equal(exitCode, 0)
})

// ── BOS-141 Task 4: judge-led headline + judge verdict block (P4b/D12) ──────
//
// A JUDGED run (real judge stub, not the default unjudged) drives the top
// headline from manifest.judge and replaces the mechanical Evidence/Confidence
// block with the labeled `**Fresh-context judge (<model>):** …` block. The
// per-surface ✅/⏸ markers and the exit code are UNCHANGED (D6 — the advisory
// judge never grades outcomes). Strings generated by running the shape once.

test('golden: judge satisfactory → ✅ headline + Fresh-context judge block (exit 0)', async () => {
  const { commentBody, manifest, exitCode } = await run({
    captureShapes: [tuiCapTwoScenesPassed()],
    agentResult: { passed: true, summary: 'ok', evidence: [], steps: 5 },
    hasFailure: false,
    deps: {
      ...stubDeps(),
      judge: async () => ({
        evidence: 'satisfactory',
        confidence: 'high',
        perScene: [{ id: 'scene-01', verdict: 'passed', reason: 'evidence clear' }],
        caveats: ['agent-runner stubbed: UI + orchestration exercised against a stubbed daemon'],
        model: 'claude-haiku-4-5',
        clamped: ['stub-caveat-appended'],
      }),
    },
  })
  assert.equal(
    commentBody,
    '<!-- bossanova-proof:pr-123 -->\n### ✅ Proof — judged convincing\n\n### [📸 Proof manifest](https://proof.example/pr/manifest.json)\n\n**Golden brief**\n\n**Commit:** `abc1234`  **Run:** RUN  **Gen-AI:** not live (UI-only demo)\n\n#### TUI — ✅ proven\n\n<details><summary>Agent summary</summary>\n\nok\n\n</details>\n\n**Fresh-context judge (claude-haiku-4-5):** Evidence: Satisfactory · Confidence: High\n- Caveat: agent-runner stubbed: UI + orchestration exercised against a stubbed daemon\n\n**Scenes:**\n- [0:12] ✅ [Scene 1 — open repos](https://proof.example/pr/tui-agent/tui-agent.mp4#t=12)\n- [0:45] ✅ [Scene 2 — rename session](https://proof.example/pr/tui-agent/tui-agent.mp4#t=45)\n',
  )
  assert.equal(manifest.judge.evidence, 'satisfactory')
  assert.equal(manifest.deferred ?? false, false)
  assert.equal(exitCode, 0)
})

test('golden: judge unsatisfactory → ⚠️ headline + per-scene ✗ reason (exit 0)', async () => {
  const { commentBody, manifest, exitCode } = await run({
    captureShapes: [tuiCapTwoScenesPassed()],
    agentResult: { passed: true, summary: 'ok', evidence: [], steps: 5 },
    hasFailure: false,
    deps: {
      ...stubDeps(),
      judge: async () => ({
        evidence: 'unsatisfactory',
        confidence: 'low',
        perScene: [
          { id: 'scene-01', verdict: 'passed', reason: 'open repos shown' },
          { id: 'scene-02', verdict: 'failed', reason: 'rename never appears in the stills' },
        ],
        caveats: ['agent-runner stubbed: UI + orchestration exercised against a stubbed daemon'],
        model: 'claude-haiku-4-5',
        clamped: ['evidence-downgraded-scene-failure'],
      }),
    },
  })
  assert.equal(
    commentBody,
    '<!-- bossanova-proof:pr-123 -->\n### ⚠️ Proof produced but not convincing\n\n### [📸 Proof manifest](https://proof.example/pr/manifest.json)\n\n**Golden brief**\n\n**Commit:** `abc1234`  **Run:** RUN  **Gen-AI:** not live (UI-only demo)\n\n#### TUI — ✅ proven\n\n<details><summary>Agent summary</summary>\n\nok\n\n</details>\n\n**Fresh-context judge (claude-haiku-4-5):** Evidence: Unsatisfactory · Confidence: Low\n- Scene scene-02 — rename session: ✗ rename never appears in the stills\n- Caveat: agent-runner stubbed: UI + orchestration exercised against a stubbed daemon\n\n**Scenes:**\n- [0:12] ✅ [Scene 1 — open repos](https://proof.example/pr/tui-agent/tui-agent.mp4#t=12)\n- [0:45] ✅ [Scene 2 — rename session](https://proof.example/pr/tui-agent/tui-agent.mp4#t=45)\n',
  )
  assert.equal(manifest.judge.evidence, 'unsatisfactory')
  // D6: a not-convincing advisory grade does NOT flip the mechanically-passed
  // section marker or the exit code.
  assert.ok(commentBody.includes('#### TUI — ✅ proven'))
  assert.equal(exitCode, 0)
})

test('golden: unjudged → 📸 headline + legacy Evidence block + Self-graded label (exit 0)', async () => {
  const { commentBody, manifest, exitCode } = await run({
    captureShapes: [tuiCap('passed')],
    agentResult: { passed: true, summary: 'ok', evidence: [], steps: 5 },
    hasFailure: false,
    // Default stubDeps() judge returns { unjudged: true, reason: 'stubbed' }.
  })
  assert.equal(
    commentBody,
    '<!-- bossanova-proof:pr-123 -->\n### 📸 Proof (unjudged)\n\n### [📸 Proof manifest](https://proof.example/pr/manifest.json)\n\n**Golden brief**\n\n**Commit:** `abc1234`  **Run:** RUN  **Gen-AI:** not live (UI-only demo)\n\n#### TUI — ✅ proven\n\n<details><summary>Agent summary</summary>\n\nok\n\n</details>\n\n✅ **Evidence:** Satisfactory\n\n✅ **Confidence:** High\n\n_Self-graded (unjudged): judge unavailable — stubbed._\n',
  )
  assert.deepEqual(manifest.judge, { unjudged: true, reason: 'stubbed' })
  assert.equal(exitCode, 0)
})

// Multi-surface + REAL judge: the run-level judge's per-scene ✗ lines must be
// SCOPED to the surface whose captures own that scene id — a Web-scene failure
// must not surface under the TUI section's proven block (review fix). scene-tui
// belongs to TUI, scene-web (judge-failed) to Web. Both surfaces stay
// mechanically ✅ proven (D6: the advisory judge never flips markers/exit).
const tuiCapSceneA = () => ({
  recipeId: 'tui-agent',
  title: 't',
  surface: 'tui',
  privacy: 'fixture',
  status: 'passed',
  mediaType: 'mp4',
  fileName: 'tui-agent/tui-agent.mp4',
  posterFileName: 'tui-agent/tui-agent.png',
  stills: [{ fileName: 'tui-agent/scene-tui-01.png', label: 'tui 01', sceneId: 'scene-tui' }],
  scenes: [{ id: 'scene-tui', title: 'open repos', passed: true, missing: [], outputMs: 12000 }],
})
const webCapSceneB = () => ({
  recipeId: 'agent-proof',
  title: 't',
  surface: 'web',
  privacy: 'fixture',
  status: 'passed',
  mediaType: 'mp4',
  fileName: 'agent-proof/agent-proof.mp4',
  posterFileName: 'agent-proof/agent-proof.png',
  stills: [{ fileName: 'agent-proof/scene-web-01.png', label: 'web 01', sceneId: 'scene-web' }],
  scenes: [{ id: 'scene-web', title: 'rename session', passed: true, missing: [], outputMs: 8000 }],
})

test('golden: multi-surface judged → per-scene ✗ stays in its own surface section', async () => {
  const { commentBody, manifest, exitCode } = await run({
    surfaceRuns: [
      multiRun('tui', {
        captureShapes: [tuiCapSceneA()],
        agentResult: { passed: true, summary: 'tui ok', steps: 5 },
      }),
      multiRun('web', {
        captureShapes: [webCapSceneB()],
        agentResult: { passed: true, summary: 'web ok', steps: 3 },
      }),
    ],
    deps: {
      ...stubDeps(),
      judge: async () => ({
        evidence: 'partial',
        confidence: 'medium',
        perScene: [
          { id: 'scene-tui', verdict: 'passed', reason: 'repos list is visible' },
          {
            id: 'scene-web',
            verdict: 'failed',
            reason: 'rename result never appears in the stills',
          },
        ],
        caveats: ['agent-runner stubbed: UI + orchestration exercised against a stubbed daemon'],
        model: 'claude-haiku-4-5',
        clamped: ['evidence-downgraded-scene-failure'],
      }),
    },
  })
  assert.equal(
    commentBody,
    '<!-- bossanova-proof:pr-123 -->\n### ⚠️ Proof produced — partially convincing\n\n### [📸 Proof manifest](https://proof.example/pr/manifest.json)\n\n**Multi brief**\n\n**Commit:** `abc1234`  **Run:** RUN  **Gen-AI:** not live (UI-only demo)\n\n#### TUI — ✅ proven\n\n<details><summary>Agent summary</summary>\n\ntui ok\n\n</details>\n\n**Fresh-context judge (claude-haiku-4-5):** Evidence: Partial · Confidence: Medium\n- Caveat: agent-runner stubbed: UI + orchestration exercised against a stubbed daemon\n\n#### Web — ✅ proven\n\n<details><summary>Agent summary</summary>\n\nweb ok\n\n</details>\n\n**Fresh-context judge (claude-haiku-4-5):** Evidence: Partial · Confidence: Medium\n- Scene scene-web — rename session: ✗ rename result never appears in the stills\n- Caveat: agent-runner stubbed: UI + orchestration exercised against a stubbed daemon\n',
  )
  // Explicit attribution asserts (belt-and-suspenders on top of the byte-exact match).
  const tuiSection = commentBody.slice(
    commentBody.indexOf('#### TUI'),
    commentBody.indexOf('#### Web'),
  )
  const webSection = commentBody.slice(commentBody.indexOf('#### Web'))
  assert.ok(!tuiSection.includes('scene-web'), 'TUI section must not carry the Web scene failure')
  assert.ok(!tuiSection.includes('✗'), 'TUI section carries no ✗ per-scene line')
  assert.ok(
    webSection.includes('- Scene scene-web — rename session: ✗'),
    'Web section shows its own ✗',
  )
  assert.equal(manifest.deferred ?? false, false)
  assert.equal(exitCode, 0)
})

// ── BOS-141 Task 3: judge wiring (advisory-only, D6) ────────────────────
//
// The judge runs AFTER outcome classification and BEFORE the manifest.json
// write, so manifest.judge is present on the FIRST (and normally only)
// write/upload. It is advisory: it never affects process.exitCode, which
// stays governed solely by aggregateExitCode(perSurface) as before.

test('judge wiring: manifest carries judge in the written manifest.json (media present)', async () => {
  const localDir = fs.mkdtempSync(path.join(os.tmpdir(), 'finalize-judge-'))
  const savedExit = process.exitCode
  try {
    let judgeCalls = 0
    const result = await finalizeAgentProof({
      captureShapes: [webCap('passed')],
      brief: { title: 'Judge brief', genAi: false },
      agentResult: { passed: true, summary: 'ok', evidence: [], steps: 3 },
      hasFailure: false,
      prNumber: '123',
      commit: 'abc1234',
      runId: 'RUN',
      token: 'tok-fixed',
      paths: { publicPrefix: 'proof/bossanova/pr-123/abc1234/RUN/tok-fixed' },
      localDir,
      publicBaseUrl: 'https://proof.example/pr',
      shouldUpload: false,
      bucket: null,
      deps: {
        ...stubDeps(),
        judge: async () => {
          judgeCalls += 1
          return {
            evidence: 'satisfactory',
            confidence: 'high',
            perScene: [],
            caveats: [],
            model: 'claude-haiku-4-5',
            clamped: [],
          }
        },
      },
    })
    assert.equal(judgeCalls, 1, 'the judge dep must be called once when media is present')
    const written = JSON.parse(fs.readFileSync(path.join(localDir, 'manifest.json'), 'utf8'))
    assert.ok(written.judge, 'the WRITTEN manifest.json must carry .judge')
    assert.equal(written.judge.evidence, 'satisfactory')
    assert.equal(result.manifest.judge.evidence, 'satisfactory')
  } finally {
    process.exitCode = savedExit
    fs.rmSync(localDir, { recursive: true, force: true })
  }
})

test('judge wiring: an unsatisfactory judge grade on a mechanically-passed run leaves process.exitCode untouched', async () => {
  const { manifest, exitCode } = await run({
    captureShapes: [webCap('passed')],
    agentResult: { passed: true, summary: 'ok', evidence: [], steps: 3 },
    hasFailure: false,
    deps: {
      ...stubDeps(),
      judge: async () => ({
        evidence: 'unsatisfactory',
        confidence: 'low',
        perScene: [],
        caveats: [],
        model: 'claude-haiku-4-5',
        clamped: [],
      }),
    },
  })
  assert.equal(manifest.judge.evidence, 'unsatisfactory')
  assert.equal(
    exitCode,
    0,
    'the judge is advisory-only and must never flip a mechanically-passed exit code',
  )
})

test('judge wiring: a no-media deferral short-circuits to unjudged/not-applicable without calling the judge dep', async () => {
  let judgeCalls = 0
  const { manifest, exitCode } = await run({
    captureShapes: [
      { recipeId: 'agent-proof', title: 't', surface: 'web', privacy: 'fixture', status: 'passed' },
    ],
    agentResult: { passed: true, summary: 'ok', evidence: [], steps: 3 },
    hasFailure: false,
    deps: {
      ...stubDeps(),
      judge: async () => {
        judgeCalls += 1
        return { evidence: 'satisfactory', confidence: 'high', perScene: [], caveats: [] }
      },
    },
  })
  assert.equal(
    judgeCalls,
    0,
    'a no-media deferral has nothing to grade; the judge dep must not be called',
  )
  assert.deepEqual(manifest.judge, { unjudged: true, reason: 'not-applicable' })
  assert.equal(exitCode, 0)
})

test('judge wiring: a judge dep that throws is caught into an unjudged manifest field, never propagated', async () => {
  const { manifest, exitCode } = await run({
    captureShapes: [webCap('passed')],
    agentResult: { passed: true, summary: 'ok', evidence: [], steps: 3 },
    hasFailure: false,
    deps: {
      ...stubDeps(),
      judge: async () => {
        throw new Error('boom')
      },
    },
  })
  assert.deepEqual(manifest.judge, { unjudged: true, reason: 'boom' })
  assert.equal(exitCode, 0)
})
