#!/usr/bin/env node
/**
 * proof-agent-finalize.mjs — Surface-neutral finalize machinery for agent proof runs.
 *
 * Implements Steps 4–7 of the agent proof orchestration (build manifest,
 * upload + report, render + post comment, exit-code, cleanup). Shared by
 * proof-agent.mjs (web surface) and any future surface-specific orchestrators.
 */

import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  buildManifest,
  githubCommentCommand,
  proofCommentMarker,
  r2UploadCommand,
  renderConsolidatedComment,
} from './proof-lib.mjs'
import {
  aggregateExitCode,
  classifySurfaceOutcomes,
  surfaceRunHasMedia,
} from './proof-finalize-outcome.mjs'
import { buildSurfaceSections } from './proof-finalize-render.mjs'
import {
  addLabelCommand,
  ensureLabelCommand,
  judgeProof,
  labelActionForJudge,
  removeLabelCommand,
} from './proof-judge.mjs'
import { publishProofReport } from './proof-publish-report.mjs'
import { scrubSecrets } from './proof-stage.mjs'
import { collapsePriorProofComments, shouldCleanupRunDir, uploadBundle } from './proof.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

function runCommand(commandTuple) {
  const [command, args, options = {}] = commandTuple
  const result = spawnSync(command, args, {
    cwd: options.cwd ? path.join(repoRoot, options.cwd) : repoRoot,
    stdio: 'inherit',
    env: { ...process.env, ...(options.env ?? {}) },
  })
  if (result.status !== 0) {
    throw new Error(`${command} ${args.join(' ')} exited ${result.status}`)
  }
}

/**
 * Best-effort, idempotent application of the `proof-invalid` label (BOS-141
 * D12). Every spawn is individually try/catch'd with a one-line warn — a gh
 * failure (rate limit, missing permission, label already in the desired
 * state) must NEVER throw into finalize. `remove` does not `ensure` first:
 * `gh pr edit --remove-label` on an absent label exits non-zero, which the
 * catch below swallows, so there is nothing to create on the remove path.
 * @param {{ action: 'add'|'remove', prNumber: string }} opts
 */
function applyJudgeLabel({ action, prNumber }) {
  if (action === 'add') {
    try {
      runCommand(ensureLabelCommand())
    } catch (err) {
      console.warn(`[proof-agent] proof-invalid label ensure skipped: ${err.message}`)
    }
    try {
      runCommand(addLabelCommand({ prNumber }))
    } catch (err) {
      console.warn(`[proof-agent] proof-invalid label add skipped: ${err.message}`)
    }
    return
  }
  try {
    runCommand(removeLabelCommand({ prNumber }))
  } catch (err) {
    console.warn(`[proof-agent] proof-invalid label remove skipped: ${err.message}`)
  }
}

function currentBranch() {
  try {
    return execFileSync('git', ['rev-parse', '--abbrev-ref', 'HEAD'], {
      cwd: repoRoot,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    }).trim()
  } catch {
    return 'unknown'
  }
}

function currentRepoIdentity() {
  try {
    const raw = execFileSync('gh', ['repo', 'view', '--json', 'owner,name'], {
      cwd: repoRoot,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    })
    const parsed = JSON.parse(raw)
    return { owner: parsed.owner.login, name: parsed.name }
  } catch {
    return null
  }
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
    // BOS-141 P4a: fresh-context judge, advisory-only (D6). Overridable so
    // tests never hit the network.
    judge: judgeProof,
    // BOS-141 D12 (Task 5): best-effort proof-invalid label. Overridable so
    // tests never shell out to gh.
    applyJudgeLabel,
  }
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
  noSurface = false,
  pipelineError = null,
  surfaceRuns,
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
  const fd = { ...defaultFinalizeDeps(), ...(deps ?? {}) }

  // ── Normalize to surfaceRuns ───────────────────────────────────────────────
  // BOS-139: the consolidated finalize renders N surface runs. When called with
  // the legacy singular params (no surfaceRuns), wrap them as ONE implicit run
  // so there is a single render path. Each run's pipelineError stderr tail is
  // scrubbed here (a crashing stage runs with the proof secrets in its env).
  const rawRuns = surfaceRuns ?? [
    {
      surface: (captureShapes ?? [captureShape]).find((c) => c?.surface)?.surface ?? 'web',
      captureShapes: captureShapes ?? [captureShape],
      brief,
      agentResult,
      hasFailure,
      noSurface,
      reasonCode: null,
      pipelineError,
    },
  ]
  const runs = rawRuns.map((run) => ({
    ...run,
    pipelineError: run.pipelineError
      ? {
          stage: run.pipelineError.stage,
          message: run.pipelineError.message,
          stderrTail: scrubSecrets(run.pipelineError.stderrTail),
          ...(run.pipelineError.elapsedMs !== undefined
            ? { elapsedMs: run.pipelineError.elapsedMs }
            : {}),
        }
      : null,
  }))

  // ── Step 4: Build manifest (flat captures + per-surface summary) ───────────
  const captures = runs.flatMap((r) => r.captureShapes ?? [])
  const perSurface = classifySurfaceOutcomes(runs)
  const anyDeferred = perSurface.some((p) => p.outcome === 'deferred')
  const runsHasFailure = runs.some((r) => r.hasFailure)
  const canUploadArtifacts = !shouldUpload || Boolean(bucket)
  const manifest = buildManifest({
    commit,
    prNumber,
    runId,
    publicBaseUrl: canUploadArtifacts ? publicBaseUrl : null,
    agentRunnerStubbed,
    captures,
    title: (surfaceRuns ? null : brief?.title) ?? runs[0]?.brief?.title,
    verdict: runsHasFailure ? 'failed' : 'passed',
    genAiLive: false,
    // Single-surface keeps its top-level summary; multi-surface carries summaries
    // in the per-surface sections instead.
    agentSummary: runs.length === 1 ? (runs[0].agentResult?.summary ?? null) : null,
    brief: { genAi: (runs.length === 1 ? runs[0].brief?.genAi : false) ?? false },
  })

  // A deferred surface marks the persisted artifact deferred. A recorded stage
  // crash is OUR bug (pipeline-error): tag the manifest with the first one so it
  // carries the failed stage for post-hoc debugging.
  if (anyDeferred) {
    manifest.deferred = true
  }
  const firstPipelineError = runs.find((r) => r.pipelineError)?.pipelineError ?? null
  if (firstPipelineError) {
    manifest.pipelineError = firstPipelineError
  }
  // Per-surface summary block (additive; captures stays the flat list).
  manifest.surfaces = perSurface.map((p, i) => ({
    surface: p.surface,
    outcome: p.outcome,
    reasonCode: p.reasonCode,
    briefTitle: runs[i]?.brief?.title ?? null,
  }))

  // ── Judge (BOS-141 P4a, advisory-only D6) ─────────────────────────────────
  // Runs AFTER outcome classification, BEFORE the manifest write below, so
  // ONE write/upload carries manifest.judge. A no-media run has nothing to
  // grade — skip the API call entirely rather than pay for (and caveat) a
  // judgment with zero evidence; a run with media always gets a fresh-context
  // opinion, even when mechanically deferred (the clamp downgrades it). The
  // judge NEVER affects process.exitCode (Step 7 below, unchanged).
  if (!runs.some((r) => surfaceRunHasMedia(r))) {
    manifest.judge = { unjudged: true, reason: 'not-applicable' }
  } else {
    try {
      manifest.judge = await fd.judge({ surfaceRuns: runs, manifest, localDir })
    } catch (err) {
      manifest.judge = { unjudged: true, reason: err?.message || 'judge-error' }
    }
  }

  fs.writeFileSync(path.join(localDir, 'manifest.json'), `${JSON.stringify(manifest, null, 2)}\n`)
  console.log(JSON.stringify(manifest, null, 2))

  // ── Step 6: Upload (if enabled) ───────────────────────────────────────────
  // Guard on `bucket`: the multi-surface dispatcher resolves the R2 bucket
  // lazily (null when BOSS_PROOF_R2_BUCKET is absent) so a missing bucket defers
  // per-surface as env-unavailable rather than throwing. In that case every
  // surface deferred (no media to upload), so skip the upload and still post the
  // honest consolidated deferral comment below. Legacy single-surface callers
  // always pass a real bucket when shouldUpload, so this never skips a real one.
  if (shouldUpload && bucket) {
    fd.uploadBundle({ localDir, publicPrefix: paths.publicPrefix, manifest, bucket })

    // Best-effort report publish — must not throw into the run.
    const repoIdent = fd.currentRepoIdentity()
    if (repoIdent) {
      const branch = fd.currentBranch()
      const identity = {
        owner: repoIdent.owner,
        sourceRepo: repoIdent.name,
        prNumber,
        branch,
        runId: token.slice(0, 8),
      }
      const report = await fd.publishProofReport({ manifest, identity, env: process.env })
      if (report.ok) {
        manifest.reportUrl = report.reportUrl
        const manifestPath = path.join(localDir, 'manifest.json')
        fs.writeFileSync(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`)
        try {
          fd.uploadManifest({
            bucket,
            key: `${paths.publicPrefix}/manifest.json`,
            file: path.relative(repoRoot, manifestPath),
          })
        } catch (err) {
          console.warn(`[proof-agent] report manifest refresh skipped: ${err.message}`)
        }
      } else {
        console.warn(`[proof-agent] report publish skipped: ${report.reason}`)
      }
    } else {
      console.warn('[proof-agent] report publish skipped: repo identity unavailable')
    }
  }

  // ── Step 7: Render and post ONE consolidated comment ──────────────────────
  const marker = proofCommentMarker(prNumber)
  // The consolidated comment's chapter links (renderSceneChapters) need each
  // capture's uploaded videoUrl, which only the manifest carries (buildManifest
  // maps each input capture to a NEW object with url/videoUrl; the raw
  // captureShapes on the runs do not). manifest.captures is exactly the flat
  // runs.flatMap(r => r.captureShapes) order, 1:1 and order-preserving, so slice
  // it back per run by cumulative count and feed the URL-bearing captures to
  // buildSurfaceSections. Slice-by-count (not surface filter) is exact even when
  // two runs share a surface. Spreading ...run keeps agentResult/surface/brief/
  // pipelineError/missing; only captureShapes is swapped.
  let capIdx = 0
  const enrichedRuns = runs.map((run) => {
    const count = (run.captureShapes ?? []).length
    const enriched = manifest.captures.slice(capIdx, capIdx + count)
    capIdx += count
    return { ...run, captureShapes: enriched }
  })
  const sections = buildSurfaceSections({ runs: enrichedRuns, perSurface })
  const commentBody = renderConsolidatedComment({ marker, manifest, sections })
  const commentPath = path.join(localDir, 'comment.md')
  fs.writeFileSync(commentPath, commentBody)

  // Exit policy (BOS-139): aggregate the per-surface contributions. A no-surface
  // outcome and every neutral deferral keep exit 0; a web agent-incomplete or
  // any pipeline crash signals exit 1; a TUI agent-incomplete stays 0 (Q6).
  if (aggregateExitCode(perSurface) === 1) {
    process.exitCode = 1
  }

  if (shouldUpload && prNumber !== 'local') {
    fd.collapsePriorProofComments({ prNumber })
    fd.postComment({
      prNumber,
      bodyFile: path.relative(repoRoot, commentPath),
    })

    // BOS-141 D12 (Task 5): best-effort, idempotent proof-invalid label.
    // Belt-and-suspenders: labelActionForJudge is pure and can't throw, but
    // fd.applyJudgeLabel is a dep (tests can inject a throwing one) — a
    // throw here must never fail finalize or change the exit code/manifest.
    const labelAction = labelActionForJudge({ judge: manifest.judge, shouldUpload, prNumber })
    if (labelAction) {
      try {
        await fd.applyJudgeLabel({ action: labelAction, prNumber })
      } catch (err) {
        console.warn(`[proof-agent] proof-invalid label step skipped: ${err.message}`)
      }
    }
  }

  if (shouldCleanupRunDir({ shouldUpload, hasFailure: runsHasFailure, prNumber, keepWebm })) {
    fs.rmSync(localDir, { recursive: true, force: true })
  }

  return { manifest, commentBody }
}
