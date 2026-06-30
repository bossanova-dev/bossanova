#!/usr/bin/env node
/**
 * proof-agent.mjs — Agent-mode proof orchestrator.
 *
 * Spawns the Playwright agent-proof project, processes the resulting video and
 * stills, builds a manifest, scans for secrets, and posts a proof comment.
 * Designed to run from proof.mjs via the mode dispatcher.
 */

import { execFileSync, spawn, spawnSync } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { proofRunPaths } from './proof-lib.mjs'
import { generateBriefFromDiff, validateBrief } from './proof-brief.mjs'
import { finalizeAgentProof } from './proof-agent-finalize.mjs'
import { buildPosterArgs } from './proof-poster.mjs'
import {
  applyMinHeightRatio,
  evenCropHeight,
  postprocessProofVideo,
  probeDimensions,
} from './proof-video.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const playButtonAsset = fileURLToPath(new URL('./assets/youtube-play-button.png', import.meta.url))

const DEFAULT_MODEL = 'claude-sonnet-4-6'

/**
 * Run a command with a kill-on-timeout.  Resolves (never rejects) with
 * `{ code, timedOut }`.  `timedOut` is true when the child was sent SIGKILL
 * because `timeoutMs` elapsed before it exited.
 *
 * @param {string} command
 * @param {string[]} args
 * @param {{ cwd?: string, env?: NodeJS.ProcessEnv, timeoutMs: number }} opts
 * @returns {Promise<{ code: number|null, timedOut: boolean }>}
 */
export function runInterruptible(command, args, { cwd, env, timeoutMs }) {
  return new Promise((resolve) => {
    const child = spawn(command, args, { cwd, env, stdio: 'inherit' })
    const timer = setTimeout(() => {
      child.kill('SIGKILL')
    }, timeoutMs)
    child.on('exit', (code, signal) => {
      clearTimeout(timer)
      resolve({ code, timedOut: signal === 'SIGKILL' })
    })
    child.on('error', (err) => {
      clearTimeout(timer)
      console.warn(`[proof-agent] runInterruptible spawn error: ${err.message}`)
      resolve({ code: -1, timedOut: false })
    })
  })
}

/**
 * Default external-effect seams. Overridable via the `deps` argument to
 * runAgentProof so unit tests can drive the orchestrator without spawning
 * Playwright, calling the Anthropic SDK, or hitting R2/GitHub. Production calls
 * pass no `deps` and get the real implementations.
 */
function defaultDeps(timeoutMs) {
  return {
    // Spawns the Playwright agent-proof project.
    // Returns Promise<{ status: number, timedOut: boolean }>.
    spawnPlaywright: async ({ briefPath, localDir, model }) => {
      const result = await runInterruptible(
        'pnpm',
        [
          '--dir',
          'services/web',
          'exec',
          'playwright',
          'test',
          '--project=agent-proof',
          '--reporter=line',
        ],
        {
          cwd: repoRoot,
          env: {
            ...process.env,
            E2E_REAL: '1',
            E2E_AGENT_PROOF: '1',
            BOSS_PROOF_BRIEF: briefPath,
            BOSS_PROOF_OUT: localDir,
            BOSS_PROOF_MODEL: model,
          },
          timeoutMs,
        },
      )
      return { status: result.timedOut ? -1 : (result.code ?? -1), timedOut: result.timedOut }
    },
    // Extracts the last frame of a video as a fallback still. Returns { status }.
    extractFallbackFrame: ({ mp4Path, fallbackPath }) =>
      spawnSync(
        'ffmpeg',
        ['-y', '-loglevel', 'error', '-sseof', '-1', '-i', mp4Path, '-frames:v', '1', fallbackPath],
        {
          stdio: 'inherit',
        },
      ),
  }
}

/**
 * Main agent proof orchestrator.
 * @param {{ prNumber: string, commit: string, changedFiles: string[], dryRun: boolean, fallbackRecipeCaptures?: Function, deps?: object }} opts
 * @returns {Promise<{ manifest: object, commentBody: string }>}
 */
export async function runAgentProof({
  prNumber,
  commit,
  changedFiles,
  dryRun,
  fallbackRecipeCaptures,
  deps,
}) {
  const agentTimeoutMs = Number(process.env.BOSS_PROOF_AGENT_TIMEOUT_MS) || 600000
  const d = { ...defaultDeps(agentTimeoutMs), ...(deps ?? {}) }
  const model = process.env.BOSS_PROOF_MODEL ?? DEFAULT_MODEL
  const shouldUpload = !dryRun && process.env.BOSS_PROOF_UPLOAD !== '0'
  const bucket = shouldUpload ? requiredProofBucket() : null

  const runId = process.env.BOSS_PROOF_RUN_ID || new Date().toISOString().replaceAll(/[:.]/g, '-')
  const token = process.env.BOSS_PROOF_RUN_TOKEN || randomUUID()
  const paths = proofRunPaths({ prNumber, commit, runId, token })
  const localDir = path.join(repoRoot, paths.localDir)
  fs.mkdirSync(localDir, { recursive: true })

  // ── Step 1: Resolve brief ─────────────────────────────────────────────────
  let brief
  const explicitBriefPath = process.env.BOSS_PROOF_BRIEF
  if (explicitBriefPath) {
    const raw = JSON.parse(fs.readFileSync(explicitBriefPath, 'utf8'))
    const result = validateBrief(raw)
    if (result.brief === null) {
      throw new Error(`Invalid BOSS_PROOF_BRIEF: ${result.errors.join(', ')}`)
    }
    brief = result.brief
  } else {
    // Generate brief from the PR diff using Claude
    const diff = gatherDiff()
    const routes = gatherRouteMap()
    const fixtures = gatherFixturesSummary()
    const rawBrief = await generateBriefFromDiff({ diff, changedFiles, routes, fixtures, model })
    const result = validateBrief(rawBrief)
    if (result.brief === null) {
      throw new Error(`Generated brief failed validation: ${result.errors.join(', ')}`)
    }
    brief = result.brief
  }

  // Write brief to run dir so the Playwright spec can read it
  const briefPath = path.join(localDir, 'brief.json')
  fs.writeFileSync(briefPath, `${JSON.stringify(brief, null, 2)}\n`)

  // ── Step 2: Spawn Playwright agent-proof project ──────────────────────────
  const rawDir = path.join(localDir, 'raw')
  fs.mkdirSync(rawDir, { recursive: true })

  let agentPassed = false
  let agentResult = { passed: false, summary: 'agent did not run', evidence: [], steps: 0 }
  let agentTimedOut = false

  try {
    // spawnPlaywright may be async (real path) or sync (test stubs); await handles both.
    const pwResult = await Promise.resolve(d.spawnPlaywright({ briefPath, localDir, model }))
    agentTimedOut = Boolean(pwResult.timedOut)
    if (agentTimedOut) {
      console.warn(
        `[proof-agent] Playwright agent timed out after ${agentTimeoutMs}ms — deferring to recipe floor`,
      )
      agentResult = {
        passed: false,
        summary: `agent timed out after ${agentTimeoutMs}ms`,
        evidence: [],
        steps: 0,
      }
    } else {
      agentPassed = pwResult.status === 0
    }
  } catch (err) {
    console.warn(`[proof-agent] playwright spawn failed: ${err.message}`)
    agentPassed = false
  }

  // Read result JSON (written by proof.spec.ts) — skipped on timeout (process was killed).
  const resultPath = path.join(rawDir, 'proof-result.json')
  if (!agentTimedOut && fs.existsSync(resultPath)) {
    try {
      agentResult = JSON.parse(fs.readFileSync(resultPath, 'utf8'))
    } catch {
      console.warn('[proof-agent] could not parse proof-result.json')
    }
  }

  // ── Step 3: Find the Playwright video and post-process ───────────────────
  // The agent-proof project saves the video under the test output dir.
  // Playwright writes it as <uuid>.webm inside the output dir; we find the
  // first .webm in rawDir.
  const rawWebm = findFirstFile(rawDir, /\.webm$/)
  const captureId = 'agent-proof'
  const captureDir = path.join(localDir, captureId)
  fs.mkdirSync(captureDir, { recursive: true })

  let captureShape
  if (rawWebm && fs.existsSync(rawWebm)) {
    const webmPath = path.join(captureDir, `${captureId}.webm`)
    const mp4Path = path.join(captureDir, `${captureId}.mp4`)
    const timedPath = path.join(captureDir, `${captureId}-timed.mp4`)
    const scratchPath = path.join(captureDir, `${captureId}-timer.raw`)
    const pngPath = path.join(captureDir, `${captureId}.png`)
    const tmpPosterPath = path.join(captureDir, `${captureId}-poster-tmp.png`)

    // Copy webm from raw dir to capture dir
    fs.copyFileSync(rawWebm, webmPath)

    // Extract still stills from raw/*.png
    const rawStills = findRawStills(rawDir)

    // Copy stills to captureDir
    let stills = rawStills.map((stillPath) => {
      const base = path.basename(stillPath)
      const dest = path.join(captureDir, base)
      fs.copyFileSync(stillPath, dest)
      return {
        fileName: `${captureId}/${base}`,
        label: labelFromStillName(base),
      }
    })

    // Post-process: dimensions + crop
    const dims = probeDimensions(webmPath)
    const cropHeight = dims
      ? applyMinHeightRatio(null, dims.width, dims.height)
      : evenCropHeight(null, Infinity)

    // Extract poster frame from the video (first frame)
    if (dims) {
      const ffmpegResult = spawnSync(
        'ffmpeg',
        ['-y', '-loglevel', 'error', '-i', webmPath, '-vframes', '1', '-update', '1', pngPath],
        { stdio: 'inherit' },
      )
      if (ffmpegResult.status !== 0) {
        console.warn('[proof-agent] could not extract poster frame from webm')
      }
    }

    // Post-process video
    const post = postprocessProofVideo({
      webmPath,
      timedPath,
      outPath: mp4Path,
      scratchPath,
      cropHeight,
    })

    if (!post.ok) {
      console.warn(
        `[proof-agent] video post-processing failed (${post.warning}) — falling back to plain mp4`,
      )
      const fallback = spawnSync('ffmpeg', ['-y', '-loglevel', 'error', '-i', webmPath, mp4Path], {
        stdio: 'inherit',
      })
      if (fallback.status !== 0) {
        console.warn('[proof-agent] ffmpeg fallback mp4 conversion also failed — no video artifact')
      }
    }

    // Fallback still: if the agent took no screenshots, extract the final frame
    // from the video so an image-only reviewer always has something to look at.
    // Prefer the post-processed mp4; fall back to the source webm if mp4 is absent.
    const videoSourceForFallback = fs.existsSync(mp4Path) ? mp4Path : webmPath
    if (stills.length === 0 && fs.existsSync(videoSourceForFallback)) {
      const fallbackPath = path.join(captureDir, '01-final-frame.png')
      const result = d.extractFallbackFrame({ mp4Path: videoSourceForFallback, fallbackPath })
      if (result.status === 0 && fs.existsSync(fallbackPath)) {
        stills = [
          {
            fileName: `${captureId}/01-final-frame.png`,
            label: '01 final frame',
          },
        ]
      } else {
        console.warn('[proof-agent] could not extract fallback final frame from video')
      }
    }

    // Composite play-button poster if we have a frame
    if (fs.existsSync(pngPath)) {
      try {
        const posterResult = spawnSync(
          'ffmpeg',
          [
            '-y',
            '-loglevel',
            'error',
            ...buildPosterArgs({
              base: pngPath,
              playButton: playButtonAsset,
              outPath: tmpPosterPath,
              cropHeight,
            }),
          ],
          { stdio: 'inherit' },
        )
        if (posterResult.status === 0 && fs.existsSync(tmpPosterPath)) {
          fs.copyFileSync(tmpPosterPath, pngPath)
        }
      } catch (err) {
        console.warn(`[proof-agent] poster compositing error: ${err.message}`)
      }
    }

    // Clean up temp files
    for (const tmpFile of [timedPath, scratchPath, tmpPosterPath]) {
      fs.rmSync(tmpFile, { force: true })
    }

    const mp4Exists = fs.existsSync(mp4Path)
    const captureStatus = agentPassed && agentResult.passed && mp4Exists ? 'passed' : 'failed'
    const captureError = !agentPassed
      ? agentResult.summary || 'agent proof test failed'
      : !agentResult.passed
        ? agentResult.summary || 'agent run did not pass'
        : !mp4Exists
          ? 'agent passed but no converted video artifact was produced'
          : null
    captureShape = {
      recipeId: captureId,
      title: brief.title,
      surface: 'web',
      privacy: 'fixture',
      status: captureStatus,
      mediaType: 'mp4',
      ...(mp4Exists ? { fileName: `${captureId}/${captureId}.mp4` } : {}),
      ...(fs.existsSync(pngPath) ? { posterFileName: `${captureId}/${captureId}.png` } : {}),
      ...(stills.length > 0 ? { stills } : {}),
      ...(captureError ? { error: captureError } : {}),
    }
  } else {
    // No video artifact — build a no-media failed capture
    captureShape = {
      recipeId: captureId,
      title: brief.title,
      surface: 'web',
      privacy: 'fixture',
      status: 'failed',
      error: agentResult.passed
        ? 'no video artifact captured'
        : agentResult.summary || 'no video artifact captured',
    }
  }

  const hasFailure = !agentPassed || !agentResult.passed || captureShape.status !== 'passed'
  const recipeFloorCaptures = hasFailure
    ? captureFallbackRecipeFloor({ fallbackRecipeCaptures, localDir, keepWebm: !shouldUpload })
    : []

  // ── Steps 4–7: Build manifest, secret scan, upload, render + post comment ─
  const publicBaseUrl = `${publicProofBaseUrl()}/${paths.publicPrefix}`
  return finalizeAgentProof({
    captureShapes: [captureShape, ...recipeFloorCaptures],
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
    // Surface-specific texts for the secret scan (manifest is always scanned
    // inside finalizeAgentProof — these are the extra web-path texts).
    scanTexts: [
      agentResult.summary ?? '',
      ...(agentResult.evidence ?? []),
      captureShape.error ?? '',
    ],
    agentRunnerStubbed: true,
    // Forward the full merged deps so test stubs (uploadBundle, postComment,
    // etc.) injected into runAgentProof still take effect inside finalize.
    deps: d,
  })
}

// ── Private helpers ──────────────────────────────────────────────────────────

function captureFallbackRecipeFloor({ fallbackRecipeCaptures, localDir, keepWebm }) {
  if (typeof fallbackRecipeCaptures !== 'function') return []
  try {
    return fallbackRecipeCaptures({ localDir, keepWebm }) ?? []
  } catch (err) {
    console.warn(`[proof-agent] recipe floor capture failed: ${err.message}`)
    return []
  }
}

function gatherDiff() {
  const baseRef = process.env.BASE_REF || 'origin/main'
  try {
    return execFileSync('git', ['diff', `${baseRef}...HEAD`], {
      cwd: repoRoot,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    })
  } catch {
    return ''
  }
}

function gatherRouteMap() {
  // Best-effort: read the React Router config from App.tsx.
  const candidates = [
    path.join(repoRoot, 'services/web/src/App.tsx'),
    path.join(repoRoot, 'services/web/src/router.tsx'),
  ]
  for (const candidate of candidates) {
    if (fs.existsSync(candidate)) {
      try {
        return fs.readFileSync(candidate, 'utf8').slice(0, 10_000)
      } catch {
        // ignore
      }
    }
  }
  return '(route map unavailable)'
}

function gatherFixturesSummary() {
  // Best-effort: read a fixture summary file if it exists.
  const candidates = [
    path.join(repoRoot, 'proof/fixtures-summary.md'),
    path.join(repoRoot, 'proof/fixtures.md'),
    path.join(repoRoot, 'services/web/tests/e2e/real/fixtures.md'),
  ]
  for (const candidate of candidates) {
    if (fs.existsSync(candidate)) {
      try {
        return fs.readFileSync(candidate, 'utf8').slice(0, 5_000)
      } catch {
        // ignore
      }
    }
  }
  return '(fixture summary unavailable)'
}

function findFirstFile(dir, pattern) {
  try {
    const entries = fs.readdirSync(dir)
    for (const entry of entries) {
      if (pattern.test(entry)) {
        return path.join(dir, entry)
      }
    }
  } catch {
    // directory missing or unreadable
  }
  return null
}

function findRawStills(rawDir) {
  const STILL_RE = /^\d\d-.*\.png$/
  try {
    return fs
      .readdirSync(rawDir)
      .filter((f) => STILL_RE.test(f))
      .sort()
      .map((f) => path.join(rawDir, f))
  } catch {
    return []
  }
}

function labelFromStillName(filename) {
  // Convert "01-open-home.png" → "01 open home"
  return path.basename(filename, '.png').replaceAll('-', ' ')
}

function requiredProofBucket() {
  const bucket = process.env.BOSS_PROOF_R2_BUCKET
  if (!bucket) {
    throw new Error('BOSS_PROOF_R2_BUCKET is required to upload proof artifacts')
  }
  return bucket
}

function publicProofBaseUrl() {
  return (process.env.BOSS_PROOF_PUBLIC_BASE_URL || 'https://proof.bossanova.dev').replace(
    /\/$/,
    '',
  )
}
