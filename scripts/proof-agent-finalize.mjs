#!/usr/bin/env node
/**
 * proof-agent-finalize.mjs — Surface-neutral finalize machinery for agent proof runs.
 *
 * Implements Steps 4–7 of the agent proof orchestration (build manifest,
 * upload + report, render + post comment, exit-code, cleanup). Shared by
 * proof-agent.mjs (web surface) and any future surface-specific orchestrators.
 */

import { execFileSync, spawnSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import {
  buildManifest,
  githubCommentCommand,
  proofCommentMarker,
  r2UploadCommand,
  renderComment,
  renderDeferredComment,
} from './proof-lib.mjs';
import { publishProofReport } from './proof-publish-report.mjs';
import { collapsePriorProofComments, shouldCleanupRunDir, uploadBundle } from './proof.mjs';

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

function runCommand(commandTuple) {
  const [command, args, options = {}] = commandTuple;
  const result = spawnSync(command, args, {
    cwd: options.cwd ? path.join(repoRoot, options.cwd) : repoRoot,
    stdio: 'inherit',
    env: { ...process.env, ...(options.env ?? {}) },
  });
  if (result.status !== 0) {
    throw new Error(`${command} ${args.join(' ')} exited ${result.status}`);
  }
}

function currentBranch() {
  try {
    return execFileSync('git', ['rev-parse', '--abbrev-ref', 'HEAD'], {
      cwd: repoRoot,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    }).trim();
  } catch {
    return 'unknown';
  }
}

function currentRepoIdentity() {
  try {
    const raw = execFileSync('gh', ['repo', 'view', '--json', 'owner,name'], {
      cwd: repoRoot,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    });
    const parsed = JSON.parse(raw);
    return { owner: parsed.owner.login, name: parsed.name };
  } catch {
    return null;
  }
}

/**
 * Returns a shell hint for re-running the proof capture manually. Includes
 * `--recipe <id>` when a recipe id is available on the first capture.
 * @param {object} manifest
 * @returns {string}
 */
function recaptureHintFor(manifest) {
  const recipeId = manifest.captures?.[0]?.recipeId;
  return recipeId
    ? `node scripts/proof.mjs run --recipe ${recipeId}`
    : 'node scripts/proof.mjs run';
}

/**
 * Default external-effect seams for the finalize step.
 * Overridable via the `deps` argument to finalizeAgentProof so unit tests can
 * drive the orchestrator without hitting R2 or GitHub.
 */
export function defaultFinalizeDeps() {
  return {
    uploadBundle,
    publishProofReport,
    collapsePriorProofComments,
    postComment: ({ prNumber, bodyFile }) =>
      runCommand(githubCommentCommand({ prNumber, bodyFile })),
    uploadManifest: ({ bucket, key, file }) =>
      runCommand(r2UploadCommand({ bucket, key, file, contentType: 'application/json' })),
    currentRepoIdentity,
    currentBranch,
  };
}

/**
 * Surface-neutral finalize step for agent proof runs (Steps 4–7).
 *
 * 1. Builds the manifest.
 * 2. Writes manifest.json and logs it.
 * 3. Uploads the bundle and best-effort-publishes a report (if shouldUpload).
 * 4. Renders and posts the proof comment; sets process.exitCode=1 on failure.
 * 5. Cleans up the local run dir on a successful posted run.
 *
 * @param {{
 *   captureShape?: object,
 *   captureShapes?: object[],
 *   brief: { title: string, genAi?: boolean },
 *   agentResult: { passed: boolean, summary?: string, evidence?: string[], steps: number },
 *   hasFailure: boolean,
 *   prNumber: string,
 *   commit: string,
 *   runId: string,
 *   token: string,
 *   paths: { publicPrefix: string },
 *   localDir: string,
 *   publicBaseUrl: string,
 *   shouldUpload: boolean,
 *   bucket: string|null,
 *   scanTexts?: string[],
 *   agentRunnerStubbed?: boolean,
 *   keepWebm?: boolean,
 *   deps?: object,
 * }} opts
 * @returns {Promise<{ manifest: object, commentBody: string }>}
 */
export async function finalizeAgentProof({
  captureShape,
  captureShapes,
  brief,
  agentResult,
  hasFailure,
  prNumber,
  commit,
  runId,
  token,
  paths,
  localDir,
  publicBaseUrl,
  shouldUpload,
  bucket,
  scanTexts = [],
  agentRunnerStubbed = true,
  keepWebm = false,
  deps,
}) {
  const fd = { ...defaultFinalizeDeps(), ...(deps ?? {}) };

  // ── Step 4: Build manifest ─────────────────────────────────────────────────
  const captures = captureShapes ?? [captureShape];
  const manifest = buildManifest({
    commit,
    prNumber,
    runId,
    publicBaseUrl,
    agentRunnerStubbed,
    captures,
    title: brief.title,
    verdict: hasFailure ? 'failed' : 'passed',
    genAiLive: false,
    agentSummary: agentResult.summary ?? null,
    brief: { genAi: brief.genAi ?? false },
  });

  // A run that did not pass or produced no media is degraded: it defers to a
  // neutral comment (never the ❌ verdict block). Mark it BEFORE persisting and
  // uploading the manifest so the stored artifact carries `deferred: true`.
  const hasMedia = manifest.captures?.some(
    (c) => c?.fileName || c?.posterFileName || (c?.stills?.length ?? 0) > 0,
  );
  const degraded = manifest.verdict !== 'passed' || !hasMedia;
  if (degraded) {
    manifest.deferred = true;
  }

  fs.writeFileSync(path.join(localDir, 'manifest.json'), `${JSON.stringify(manifest, null, 2)}\n`);
  console.log(JSON.stringify(manifest, null, 2));

  // ── Step 6: Upload (if enabled) ───────────────────────────────────────────
  if (shouldUpload) {
    fd.uploadBundle({ localDir, publicPrefix: paths.publicPrefix, manifest, bucket });

    // Best-effort report publish — must not throw into the run.
    const repoIdent = fd.currentRepoIdentity();
    if (repoIdent) {
      const branch = fd.currentBranch();
      const identity = {
        owner: repoIdent.owner,
        sourceRepo: repoIdent.name,
        prNumber,
        branch,
        runId: token.slice(0, 8),
      };
      const report = await fd.publishProofReport({ manifest, identity, env: process.env });
      if (report.ok) {
        manifest.reportUrl = report.reportUrl;
        const manifestPath = path.join(localDir, 'manifest.json');
        fs.writeFileSync(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`);
        try {
          fd.uploadManifest({
            bucket,
            key: `${paths.publicPrefix}/manifest.json`,
            file: path.relative(repoRoot, manifestPath),
          });
        } catch (err) {
          console.warn(`[proof-agent] report manifest refresh skipped: ${err.message}`);
        }
      } else {
        console.warn(`[proof-agent] report publish skipped: ${report.reason}`);
      }
    } else {
      console.warn('[proof-agent] report publish skipped: repo identity unavailable');
    }
  }

  // ── Step 7: Render and post comment ──────────────────────────────────────
  // `degraded` / `manifest.deferred` were determined before the manifest write.
  const marker = proofCommentMarker(prNumber);

  let commentBody;
  if (degraded) {
    commentBody = renderDeferredComment({
      marker,
      manifest,
      reasonCode: manifest.verdict !== 'passed' ? 'agent-incomplete' : 'no-media',
      recaptureHint: recaptureHintFor(manifest),
    });
  } else {
    commentBody = renderComment({ marker, manifest });
  }
  const commentPath = path.join(localDir, 'comment.md');
  fs.writeFileSync(commentPath, commentBody);

  if (hasFailure) {
    process.exitCode = 1;
  }

  if (shouldUpload && prNumber !== 'local') {
    fd.collapsePriorProofComments({ prNumber });
    fd.postComment({
      prNumber,
      bodyFile: path.relative(repoRoot, commentPath),
    });
  }

  if (shouldCleanupRunDir({ shouldUpload, hasFailure, prNumber, keepWebm })) {
    fs.rmSync(localDir, { recursive: true, force: true });
  }

  return { manifest, commentBody };
}
