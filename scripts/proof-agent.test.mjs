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

import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test } from 'node:test';

function requireFfmpeg(t, reason) {
  const ffmpegCheck = spawnSync('ffmpeg', ['-version'], { stdio: 'ignore' });
  if (ffmpegCheck.status !== 0) {
    t.skip(reason);
    return false;
  }
  return true;
}

// ── 1. Dispatcher: agentModeAvailable() ─────────────────────────────────────

test('agentModeAvailable: BOSS_PROOF_MODE=recipe → false regardless of key', async () => {
  const original = process.env.BOSS_PROOF_MODE;
  const originalKey = process.env.PROOF_ANTHROPIC_API_KEY;
  process.env.BOSS_PROOF_MODE = 'recipe';
  process.env.PROOF_ANTHROPIC_API_KEY = 'sk-some-key';
  try {
    const { agentModeAvailable } = await import('./proof.mjs');
    assert.equal(agentModeAvailable(), false);
  } finally {
    process.env.BOSS_PROOF_MODE = original ?? '';
    if (originalKey === undefined) delete process.env.PROOF_ANTHROPIC_API_KEY;
    else process.env.PROOF_ANTHROPIC_API_KEY = originalKey;
  }
});

test('agentModeAvailable: BOSS_PROOF_MODE=agent → true regardless of key', async () => {
  const original = process.env.BOSS_PROOF_MODE;
  const originalKey = process.env.PROOF_ANTHROPIC_API_KEY;
  process.env.BOSS_PROOF_MODE = 'agent';
  delete process.env.PROOF_ANTHROPIC_API_KEY;
  try {
    const { agentModeAvailable } = await import('./proof.mjs');
    assert.equal(agentModeAvailable(), true);
  } finally {
    if (original === undefined) delete process.env.BOSS_PROOF_MODE;
    else process.env.BOSS_PROOF_MODE = original;
    if (originalKey !== undefined) process.env.PROOF_ANTHROPIC_API_KEY = originalKey;
  }
});

test('agentModeAvailable: unset mode + key set → true', async () => {
  const original = process.env.BOSS_PROOF_MODE;
  const originalKey = process.env.PROOF_ANTHROPIC_API_KEY;
  delete process.env.BOSS_PROOF_MODE;
  process.env.PROOF_ANTHROPIC_API_KEY = 'sk-test-key-123';
  try {
    const { agentModeAvailable } = await import('./proof.mjs');
    assert.equal(agentModeAvailable(), true);
  } finally {
    if (original !== undefined) process.env.BOSS_PROOF_MODE = original;
    if (originalKey === undefined) delete process.env.PROOF_ANTHROPIC_API_KEY;
    else process.env.PROOF_ANTHROPIC_API_KEY = originalKey;
  }
});

test('agentModeAvailable: explicit recipe selection prefers recipe path unless agent forced', async () => {
  const original = process.env.BOSS_PROOF_MODE;
  const originalKey = process.env.PROOF_ANTHROPIC_API_KEY;
  delete process.env.BOSS_PROOF_MODE;
  process.env.PROOF_ANTHROPIC_API_KEY = 'sk-test-key-123';
  try {
    const { agentModeAvailable } = await import('./proof.mjs');
    assert.equal(agentModeAvailable({ explicitRecipeSelection: true }), false);
    process.env.BOSS_PROOF_MODE = 'agent';
    assert.equal(agentModeAvailable({ explicitRecipeSelection: true }), true);
  } finally {
    if (original === undefined) delete process.env.BOSS_PROOF_MODE;
    else process.env.BOSS_PROOF_MODE = original;
    if (originalKey === undefined) delete process.env.PROOF_ANTHROPIC_API_KEY;
    else process.env.PROOF_ANTHROPIC_API_KEY = originalKey;
  }
});

test('agentModeAvailable: unset mode + no key → false (recipe fallback)', async () => {
  const original = process.env.BOSS_PROOF_MODE;
  const originalKey = process.env.PROOF_ANTHROPIC_API_KEY;
  delete process.env.BOSS_PROOF_MODE;
  delete process.env.PROOF_ANTHROPIC_API_KEY;
  try {
    const { agentModeAvailable } = await import('./proof.mjs');
    assert.equal(agentModeAvailable(), false);
  } finally {
    if (original !== undefined) process.env.BOSS_PROOF_MODE = original;
    if (originalKey !== undefined) process.env.PROOF_ANTHROPIC_API_KEY = originalKey;
  }
});

// ── 2. validateBrief (belt-and-suspenders) ───────────────────────────────────

test('validateBrief: minimal valid brief passes with all defaults', async () => {
  const { validateBrief } = await import('./proof-brief.mjs');
  const { brief, errors } = validateBrief({ title: 'Test feature', description: 'Shows it works' });
  assert.deepEqual(errors, []);
  assert.equal(brief.title, 'Test feature');
  assert.deepEqual(brief.targetRoutes, []);
  assert.equal(brief.budgets.maxSteps, 60);
  assert.equal(brief.budgets.maxWallClockMs, 720000);
});

test('validateBrief: missing title returns null brief and error', async () => {
  const { validateBrief } = await import('./proof-brief.mjs');
  const { brief, errors } = validateBrief({ description: 'desc' });
  assert.equal(brief, null);
  assert.ok(errors.some((e) => /title/.test(e)));
});

// ── 3. Capture-shape assembly and agentRunnerStubbed flag ────────────────────

test('buildManifest sets agentRunnerStubbed in output when passed', async () => {
  const { buildManifest } = await import('./proof-lib.mjs');
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
  });
  assert.equal(manifest.agentRunnerStubbed, true);
  assert.equal(manifest.captures[0].mediaType, 'mp4');
  assert.equal(manifest.captures[0].url, 'https://proof.example.dev/p/agent-proof/agent-proof.mp4');
  assert.equal(
    manifest.captures[0].videoUrl,
    'https://proof.example.dev/p/agent-proof/agent-proof.mp4',
  );
  assert.equal(
    manifest.captures[0].posterUrl,
    'https://proof.example.dev/p/agent-proof/agent-proof.png',
  );
});

test('buildManifest: agentRunnerStubbed absent → property not set', async () => {
  const { buildManifest } = await import('./proof-lib.mjs');
  const manifest = buildManifest({
    commit: 'abc1234',
    prNumber: '7',
    runId: 'run-1',
    publicBaseUrl: 'https://proof.example.dev/p',
    captures: [],
  });
  assert.ok(
    !('agentRunnerStubbed' in manifest),
    'agentRunnerStubbed must not appear when not passed',
  );
});

test('buildManifest: failed capture with mp4 gets url (reviewable failure)', async () => {
  const { buildManifest } = await import('./proof-lib.mjs');
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
  });
  const c = manifest.captures[0];
  assert.equal(c.status, 'failed');
  assert.ok(c.url, 'failed capture with fileName must have url for reviewability');
  assert.equal(c.videoUrl, 'https://proof.example.dev/p/agent-proof/agent-proof.mp4');
});

test('renderGallery shows stub notice when agentRunnerStubbed is true', async () => {
  const { buildManifest, renderGallery } = await import('./proof-lib.mjs');
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
  });
  const body = renderGallery({ manifest });
  assert.match(body, /agent-runner stubbed/);
});

// ── 4. Secret-scan gate ───────────────────────────────────────────────────────

test('classifySecretRisk: planted GitHub token → high risk', async () => {
  const { classifySecretRisk } = await import('./proof-lib.mjs');
  // A planted token in the form a GitHub PAT would take
  const plantedToken = 'ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA';
  const result = classifySecretRisk(`agent navigated to dashboard. Token found: ${plantedToken}`);
  assert.equal(result.risk, 'high');
  assert.equal(result.reason, 'credential-pattern');
});

test('classifySecretRisk: normal agent summary → no risk', async () => {
  const { classifySecretRisk } = await import('./proof-lib.mjs');
  const result = classifySecretRisk(
    'Navigated to /dashboard. Clicked New Session. Verified the session list updated.',
  );
  assert.equal(result.risk, 'none');
});

test('classifySecretRisk: OpenAI-style key in evidence → high risk', async () => {
  const { classifySecretRisk } = await import('./proof-lib.mjs');
  const plantedKey = 'sk-proj-ABCDEFGHIJKLMNOPQRSTUVWXYZ01234567';
  const result = classifySecretRisk(`API key: ${plantedKey}`);
  assert.equal(result.risk, 'high');
});

// ── 5. stills in agent capture shape ─────────────────────────────────────────

test('buildManifest: passed capture stills get urls', async () => {
  const { buildManifest } = await import('./proof-lib.mjs');
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
  });
  const c = manifest.captures[0];
  assert.ok(Array.isArray(c.stills), 'stills must be an array');
  assert.equal(c.stills.length, 2);
  assert.equal(c.stills[0].url, 'https://proof.example.dev/p/agent-proof/01-dashboard.png');
  assert.equal(c.stills[1].url, 'https://proof.example.dev/p/agent-proof/02-session.png');
});

// ── 6. proofUploadFiles: agent-mode failed capture gets media queued ──────────

test('proofUploadFiles: failed capture with mp4 fileName is queued for upload', async () => {
  const { proofUploadFiles } = await import('./proof-lib.mjs');
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
  });
  const rels = files.map((f) => f.relative);
  assert.ok(rels.includes('agent-proof/agent-proof.mp4'), 'mp4 must be queued for upload');
  assert.ok(rels.includes('agent-proof/agent-proof.png'), 'poster must be queued for upload');
});

// ── 7. Orchestrator seam: runAgentProof with mocked spawn + uploader ─────────
// Drives the real runAgentProof but injects a fake Playwright spawn (no browser)
// and a fake uploader. Asserts that a planted secret in the LLM-generated
// brief.title (which flows into the assembled manifest) makes the run THROW
// before any upload command runs.

const REPO_ROOT = path.resolve(path.dirname(new URL(import.meta.url).pathname), '..');

async function withTempBrief(brief, fn) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-agent-brief-'));
  const briefPath = path.join(dir, 'brief.json');
  fs.writeFileSync(briefPath, JSON.stringify(brief, null, 2));
  try {
    return await fn(briefPath);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
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
  ];
  const saved = {};
  for (const k of keys) saved[k] = process.env[k];
  for (const [k, v] of Object.entries(overrides)) {
    if (v === undefined) delete process.env[k];
    else process.env[k] = v;
  }
  return Promise.resolve()
    .then(fn)
    .finally(() => {
      for (const k of keys) {
        if (saved[k] === undefined) delete process.env[k];
        else process.env[k] = saved[k];
      }
    });
}

test('runAgentProof: planted secret in brief.title FAILS before any upload', async () => {
  const { runAgentProof } = await import('./proof-agent.mjs');

  // Planted GitHub-PAT-shaped token embedded in the brief title; it flows into
  // captureShape.title → manifest → (would be) the published comment.
  const plantedToken = 'ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA';
  const brief = {
    title: `Demo feature ${plantedToken}`,
    description: 'Proves the change works',
  };

  let uploadCalled = false;
  let commentPosted = false;
  let manifestUploaded = false;

  await withTempBrief(brief, (briefPath) =>
    withEnv(
      {
        BOSS_PROOF_BRIEF: briefPath,
        BOSS_PROOF_UPLOAD: '1', // upload enabled so we can prove it is NOT reached
        BOSS_PROOF_RUN_ID: 'test-run-secret',
        BOSS_PROOF_RUN_TOKEN: 'tok12345',
        BOSS_PROOF_R2_BUCKET: 'test-bucket',
        BOSS_PROOF_PUBLIC_BASE_URL: 'https://proof.test.dev',
      },
      async () => {
        await assert.rejects(
          runAgentProof({
            prNumber: '123',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: false,
            deps: {
              // Fake spawn: write a benign result, return success; no browser.
              spawnPlaywright: ({ localDir }) => {
                fs.mkdirSync(path.join(localDir, 'raw'), { recursive: true });
                fs.writeFileSync(
                  path.join(localDir, 'raw', 'proof-result.json'),
                  JSON.stringify({ passed: true, summary: 'ok', evidence: [], steps: 3 }),
                );
                return { status: 0 };
              },
              uploadBundle: () => {
                uploadCalled = true;
              },
              uploadManifest: () => {
                manifestUploaded = true;
              },
              publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
              collapsePriorProofComments: () => {},
              postComment: () => {
                commentPosted = true;
              },
            },
          }),
          /secret-like content detected/,
        );
      },
    ),
  );

  assert.equal(uploadCalled, false, 'uploadBundle must NOT be called when a secret is detected');
  assert.equal(manifestUploaded, false, 'manifest must NOT be uploaded when a secret is detected');
  assert.equal(commentPosted, false, 'no comment may be posted when a secret is detected');

  // Clean up the .proof run dir this test created.
  fs.rmSync(path.join(REPO_ROOT, '.proof', 'pr-123'), { recursive: true, force: true });
});

test('runAgentProof: planted secret in agent summary FAILS before any upload', async () => {
  const { runAgentProof } = await import('./proof-agent.mjs');

  const plantedToken = 'ghp_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB';
  const brief = { title: 'Clean title', description: 'Proves the change works' };

  let uploadCalled = false;

  await withTempBrief(brief, (briefPath) =>
    withEnv(
      {
        BOSS_PROOF_BRIEF: briefPath,
        BOSS_PROOF_UPLOAD: '1',
        BOSS_PROOF_RUN_ID: 'test-run-secret-2',
        BOSS_PROOF_RUN_TOKEN: 'tok67890',
        BOSS_PROOF_R2_BUCKET: 'test-bucket',
        BOSS_PROOF_PUBLIC_BASE_URL: 'https://proof.test.dev',
      },
      async () => {
        await assert.rejects(
          runAgentProof({
            prNumber: '124',
            commit: 'abc1234',
            changedFiles: [],
            dryRun: false,
            deps: {
              spawnPlaywright: ({ localDir }) => {
                fs.mkdirSync(path.join(localDir, 'raw'), { recursive: true });
                fs.writeFileSync(
                  path.join(localDir, 'raw', 'proof-result.json'),
                  // Secret planted in the agent's own summary.
                  JSON.stringify({
                    passed: true,
                    summary: `Found token ${plantedToken} in logs`,
                    evidence: [],
                    steps: 3,
                  }),
                );
                return { status: 0 };
              },
              uploadBundle: () => {
                uploadCalled = true;
              },
              uploadManifest: () => {},
              publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
              collapsePriorProofComments: () => {},
              postComment: () => {},
            },
          }),
          /secret-like content detected/,
        );
      },
    ),
  );

  assert.equal(
    uploadCalled,
    false,
    'uploadBundle must NOT be called when a secret is in the summary',
  );
  fs.rmSync(path.join(REPO_ROOT, '.proof', 'pr-124'), { recursive: true, force: true });
});

test('runAgentProof: clean run reaches uploadBundle (no false-positive gate)', async (t) => {
  const { runAgentProof } = await import('./proof-agent.mjs');

  if (!requireFfmpeg(t, 'ffmpeg is required for clean agent video regression test')) return;

  const brief = { title: 'Clean feature', description: 'Proves the change works' };

  let uploadCalled = false;

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
              const rawDir = path.join(localDir, 'raw');
              fs.mkdirSync(rawDir, { recursive: true });
              fs.writeFileSync(
                path.join(rawDir, 'proof-result.json'),
                JSON.stringify({ passed: true, summary: 'all good', evidence: [], steps: 2 }),
              );
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
              );
              assert.equal(result.status, 0, 'test video fixture must be created');
              return { status: 0 };
            },
            uploadBundle: () => {
              uploadCalled = true;
            },
            uploadManifest: () => {},
            publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
            collapsePriorProofComments: () => {},
            postComment: () => {},
          },
        });
        assert.equal(manifest.agentRunnerStubbed, true);
        assert.equal(manifest.captures[0].title, 'Clean feature');
      },
    ),
  );

  assert.equal(uploadCalled, true, 'a clean run must reach uploadBundle');
  fs.rmSync(path.join(REPO_ROOT, '.proof', 'pr-local'), { recursive: true, force: true });
});

test('runAgentProof: passed agent result without captured video marks proof failed', async () => {
  const { runAgentProof } = await import('./proof-agent.mjs');

  const brief = {
    title: 'Missing video feature',
    description: 'Agent reported done but video was absent',
  };
  const originalExitCode = process.exitCode;
  process.exitCode = undefined;

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
                const rawDir = path.join(localDir, 'raw');
                fs.mkdirSync(rawDir, { recursive: true });
                fs.writeFileSync(
                  path.join(rawDir, 'proof-result.json'),
                  JSON.stringify({
                    passed: true,
                    summary: 'agent claims success',
                    evidence: [],
                    steps: 2,
                  }),
                );
                return { status: 0 };
              },
              uploadBundle: () => {},
              uploadManifest: () => {},
              publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
              collapsePriorProofComments: () => {},
              postComment: () => {},
            },
          });

          assert.equal(manifest.verdict, 'failed');
          assert.equal(manifest.captures[0].status, 'failed');
          assert.match(manifest.captures[0].error, /no video artifact captured/);
          assert.equal(process.exitCode, 1);
        },
      ),
    );
  } finally {
    process.exitCode = originalExitCode;
    fs.rmSync(path.join(REPO_ROOT, '.proof', 'pr-local'), { recursive: true, force: true });
  }
});

test('runAgentProof: successful posted agent run removes local run directory', async (t) => {
  const { runAgentProof } = await import('./proof-agent.mjs');

  if (!requireFfmpeg(t, 'ffmpeg is required for cleanup wiring regression test')) return;

  const brief = { title: 'Cleanup feature', description: 'Successful posted agent proof' };
  const runDir = path.join(
    REPO_ROOT,
    '.proof',
    'pr-789',
    'abc1234',
    'test-run-cleanup',
    'tokcleanup1',
  );

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
              const rawDir = path.join(localDir, 'raw');
              fs.mkdirSync(rawDir, { recursive: true });
              fs.writeFileSync(
                path.join(rawDir, 'proof-result.json'),
                JSON.stringify({ passed: true, summary: 'all good', evidence: [], steps: 2 }),
              );
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
              );
              assert.equal(result.status, 0, 'test video fixture must be created');
              return { status: 0 };
            },
            uploadBundle: () => {},
            uploadManifest: () => {},
            publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
            collapsePriorProofComments: () => {},
            postComment: () => {},
          },
        });

        assert.equal(manifest.verdict, 'passed');
      },
    ),
  );

  assert.equal(fs.existsSync(runDir), false, 'successful posted agent run dir must be removed');
  fs.rmSync(path.join(REPO_ROOT, '.proof', 'pr-789'), { recursive: true, force: true });
});

// ── 8. Fallback frame extraction (agent path) ─────────────────────────────────
// When the agent takes ZERO raw stills but a video exists, extractFallbackFrame
// must be invoked and the resulting capture must contain exactly one still.
// When stills ARE present, the fallback extractor must NOT be called.

test('runAgentProof: no raw stills + video → fallback extractor invoked, capture has 1 still', async () => {
  const { runAgentProof } = await import('./proof-agent.mjs');

  const brief = { title: 'No stills feature', description: 'Agent took no screenshots' };
  let extractorCalled = false;
  let extractorArgs = null;
  const originalExitCode = process.exitCode;
  process.exitCode = undefined;

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
                const rawDir = path.join(localDir, 'raw');
                fs.mkdirSync(rawDir, { recursive: true });
                // Write proof-result.json (passed) but NO \d\d-*.png stills
                fs.writeFileSync(
                  path.join(rawDir, 'proof-result.json'),
                  JSON.stringify({ passed: true, summary: 'all good', evidence: [], steps: 2 }),
                );
                // Write a fake .webm so the video branch is taken
                fs.writeFileSync(path.join(rawDir, 'session.webm'), 'fake-webm-data');
                return { status: 0 };
              },
              // Injected fallback extractor: writes the fallback PNG and records call
              extractFallbackFrame: ({ mp4Path, fallbackPath }) => {
                extractorCalled = true;
                extractorArgs = { mp4Path, fallbackPath };
                // Write the fallback PNG so the stills assembly picks it up
                fs.writeFileSync(fallbackPath, 'fake-png-data');
                return { status: 0 };
              },
              uploadBundle: () => {},
              uploadManifest: () => {},
              publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
              collapsePriorProofComments: () => {},
              postComment: () => {},
            },
          });

          assert.equal(
            extractorCalled,
            true,
            'extractFallbackFrame must be called when no stills exist',
          );
          assert.ok(
            extractorArgs.fallbackPath.endsWith('01-final-frame.png'),
            'fallback file must be named 01-final-frame.png',
          );

          const capture = manifest.captures[0];
          assert.ok(Array.isArray(capture.stills), 'capture must have stills array');
          assert.equal(
            capture.stills.length,
            1,
            'capture must have exactly 1 still (the fallback frame)',
          );
          assert.ok(
            capture.stills[0].fileName.includes('01-final-frame.png'),
            'the still must be the fallback frame',
          );
        },
      ),
    );
  } finally {
    process.exitCode = originalExitCode;
    fs.rmSync(path.join(REPO_ROOT, '.proof', 'pr-local'), { recursive: true, force: true });
  }
});

test('runAgentProof: has raw stills → fallback extractor NOT invoked', async () => {
  const { runAgentProof } = await import('./proof-agent.mjs');

  const brief = { title: 'Has stills feature', description: 'Agent took screenshots' };
  let extractorCalled = false;
  const originalExitCode = process.exitCode;
  process.exitCode = undefined;

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
                const rawDir = path.join(localDir, 'raw');
                fs.mkdirSync(rawDir, { recursive: true });
                fs.writeFileSync(
                  path.join(rawDir, 'proof-result.json'),
                  JSON.stringify({ passed: true, summary: 'ok', evidence: [], steps: 3 }),
                );
                // Write a fake .webm and TWO real stills
                fs.writeFileSync(path.join(rawDir, 'session.webm'), 'fake-webm-data');
                fs.writeFileSync(path.join(rawDir, '01-home.png'), 'fake-png-1');
                fs.writeFileSync(path.join(rawDir, '02-detail.png'), 'fake-png-2');
                return { status: 0 };
              },
              extractFallbackFrame: () => {
                extractorCalled = true;
                return { status: 0 };
              },
              uploadBundle: () => {},
              uploadManifest: () => {},
              publishProofReport: async () => ({ ok: false, reason: 'stubbed' }),
              collapsePriorProofComments: () => {},
              postComment: () => {},
            },
          });

          assert.equal(
            extractorCalled,
            false,
            'extractFallbackFrame must NOT be called when stills exist',
          );

          const capture = manifest.captures[0];
          assert.ok(Array.isArray(capture.stills), 'capture must have stills array');
          assert.equal(
            capture.stills.length,
            2,
            'capture must have exactly 2 stills (the real ones)',
          );
        },
      ),
    );
  } finally {
    process.exitCode = originalExitCode;
    fs.rmSync(path.join(REPO_ROOT, '.proof', 'pr-local'), { recursive: true, force: true });
  }
});
