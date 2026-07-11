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

import { capturesAgentRunnerStubbed, proofRunPaths } from './proof-lib.mjs'
import { generateBriefFromDiff, normalizeScenes, validateBrief } from './proof-brief.mjs'
import { LIVE_AGENT_CHECKS, defaultDoctorLookups, doctorReport } from './proof-doctor.mjs'
import { finalizeAgentProof } from './proof-agent-finalize.mjs'
import { buildPosterArgs } from './proof-poster.mjs'
import {
  applyMinHeightRatio,
  evenCropHeight,
  mapSourceToOutputMs,
  postprocessProofVideo,
  probeDimensions,
} from './proof-video.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const playButtonAsset = fileURLToPath(new URL('./assets/youtube-play-button.png', import.meta.url))

// Haiku is the default: it runs faster and dodges the proof key's sonnet ITPM
// cap (image-heavy web agent runs 429 on sonnet). Override with BOSS_PROOF_MODEL.
// This is WEB-ONLY: the text-only TUI leg defaults to Sonnet and honors its own
// BOSS_PROOF_TUI_MODEL knob (see scripts/proof-tui-agent.mjs). The web leg must
// NOT read BOSS_PROOF_TUI_MODEL — that would let a TUI-scoped override 429 the
// image-heavy web agent.
export const DEFAULT_MODEL = 'claude-haiku-4-5'

// Resolve the model the web capture leg drives under. Deliberately reads only the
// shared BOSS_PROOF_MODEL (not the TUI-scoped knob) so web keeps its Haiku default.
export function resolveWebModel(env = process.env) {
  return env.BOSS_PROOF_MODEL ?? DEFAULT_MODEL
}

// Headroom the outer SIGKILL backstop must leave above the inner agent budget so
// the graceful shutdown (loop break → page.close → video.saveAs → stills write)
// completes before the process is killed. The spec itself reserves +60s
// (test.setTimeout = maxWallClockMs + 60s); this doubles that for video
// finalization + post-processing slack.
export const VIDEO_SAVE_HEADROOM_MS = 120_000
// Mirrors proof-brief DEFAULT_BUDGETS.maxWallClockMs (12 min). Used only when a
// brief carries no usable wall-clock budget.
export const DEFAULT_INNER_BUDGET_MS = 12 * 60 * 1000

/**
 * Resolve the outer process-kill backstop (the runInterruptible SIGKILL timeout).
 * It MUST strictly exceed the inner agent wall-clock budget, or the SIGKILL
 * preempts the graceful shutdown and discards the captured video + stills. An
 * operator-supplied BOSS_PROOF_AGENT_TIMEOUT_MS is honored only when it is
 * already above the floor; otherwise it is clamped up so the two timeouts can
 * never be inverted.
 *
 * @param {{ envValue: string|number|undefined, innerBudgetMs: number|undefined }} args
 * @returns {number}
 */
export function resolveAgentBackstopMs({ envValue, innerBudgetMs }) {
  const inner =
    Number.isFinite(innerBudgetMs) && innerBudgetMs > 0 ? innerBudgetMs : DEFAULT_INNER_BUDGET_MS
  const floor = inner + VIDEO_SAVE_HEADROOM_MS
  const env = Number(envValue) || 0
  return Math.max(env, floor)
}

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
 * The doctor check ids that are relevant to a live-agent scene (BOS-142). A
 * live run is deferred when ANY of these is missing; the deferred set is joined
 * into BOSS_PROOF_LIVE_AGENT_DEFERRED. PROOF_ANTHROPIC_API_KEY is included even
 * though the daemon injects it: a live agent genuinely spends the key, so it is
 * required to run live (never exempted the way the normal preflight exempts it).
 */
const LIVE_AGENT_PREREQ_IDS = new Set([
  ...LIVE_AGENT_CHECKS.map((c) => c.id),
  'PROOF_ANTHROPIC_API_KEY',
])

/**
 * Builds the Playwright child env for the agent-proof run. Pure — no fs/env/
 * spawn. Layers the fixed harness keys over `base` (normally `process.env`) and
 * encodes the live-agent decision (BOS-142):
 *   - `liveAgent:true` → set BOSS_PROOF_LIVE_AGENT='1' (run genuinely live);
 *   - else a non-empty `liveAgentDeferred` array → set
 *     BOSS_PROOF_LIVE_AGENT_DEFERRED to its comma-joined ids (and DO NOT set
 *     BOSS_PROOF_LIVE_AGENT) so the driver marks the scene deferred;
 *   - neither → leave both unset (byte-identical to the pre-BOS-142 env).
 * @param {NodeJS.ProcessEnv} base
 * @param {{ briefPath: string, localDir: string, model: string, liveAgent?: boolean, liveAgentDeferred?: string[]|null }} opts
 * @returns {NodeJS.ProcessEnv}
 */
export function buildPlaywrightEnv(
  base,
  { briefPath, localDir, model, liveAgent = false, liveAgentDeferred = null },
) {
  const env = {
    ...base,
    E2E_REAL: '1',
    E2E_AGENT_PROOF: '1',
    BOSS_PROOF_BRIEF: briefPath,
    BOSS_PROOF_OUT: localDir,
    BOSS_PROOF_MODEL: model,
  }
  // Hermetic: the live-agent decision is set EXCLUSIVELY from the args below, so
  // an ambient BOSS_PROOF_LIVE_AGENT / _DEFERRED inherited through `...base`
  // (a stray operator/CI export, a nested run) can never leak a non-live run
  // into live mode and boot the real runner. Clear BOTH first, then set exactly
  // one (or neither) from the decision.
  delete env.BOSS_PROOF_LIVE_AGENT
  delete env.BOSS_PROOF_LIVE_AGENT_DEFERRED
  if (liveAgent) {
    env.BOSS_PROOF_LIVE_AGENT = '1'
  } else if (Array.isArray(liveAgentDeferred) && liveAgentDeferred.length > 0) {
    env.BOSS_PROOF_LIVE_AGENT_DEFERRED = liveAgentDeferred.join(',')
  }
  return env
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
    spawnPlaywright: async ({ briefPath, localDir, model, liveAgent, liveAgentDeferred }) => {
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
          env: buildPlaywrightEnv(process.env, {
            briefPath,
            localDir,
            model,
            liveAgent,
            liveAgentDeferred,
          }),
          timeoutMs,
        },
      )
      return { status: result.timedOut ? -1 : (result.code ?? -1), timedOut: result.timedOut }
    },
    // Live-agent preflight (BOS-142): returns the subset of live-agent-relevant
    // doctor prereqs that are MISSING for a live web run. An empty array means
    // all prereqs are present and the scene may run genuinely live; a non-empty
    // array defers the live scene (its ids ride BOSS_PROOF_LIVE_AGENT_DEFERRED).
    liveAgentPreflight: () => {
      const report = doctorReport({
        surface: 'web',
        liveAgent: true,
        lookups: defaultDoctorLookups({ repoRoot }),
      })
      return report.missing.filter((id) => LIVE_AGENT_PREREQ_IDS.has(id))
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
 *
 * When `runContext` is provided (BOS-139 collect mode), the orchestrator owns
 * run identity and the shared budget: this runner uses the provided
 * runId/token/paths/localDir, writes its brief to `<localDir>/<briefFileName>`
 * (so a shared run dir does not collide with the TUI runner's brief), clamps the
 * brief's inner wall-clock budget to `runContext.maxWallClockMs`, and — when
 * `collect` is set — returns a SurfaceRun instead of self-finalizing (the
 * consolidated finalize consumes N of these). Without `runContext`, behavior is
 * byte-identical to today (compute own identity, write `brief.json`, finalize).
 *
 * @param {{ prNumber: string, commit: string, changedFiles: string[], dryRun: boolean, deps?: object, planRequiredProof?: string[], runContext?: object }} opts
 * @returns {Promise<{ manifest: object, commentBody: string } | object>}
 */
export async function runAgentProof({
  prNumber,
  commit,
  changedFiles,
  dryRun,
  deps,
  planRequiredProof,
  runContext,
}) {
  const startedAt = Date.now()
  const model = resolveWebModel()
  const shouldUpload = !dryRun && process.env.BOSS_PROOF_UPLOAD !== '0'
  const bucket = shouldUpload ? requiredProofBucket() : null

  const runId =
    runContext?.runId ??
    (process.env.BOSS_PROOF_RUN_ID || new Date().toISOString().replaceAll(/[:.]/g, '-'))
  const token = runContext?.token ?? (process.env.BOSS_PROOF_RUN_TOKEN || randomUUID())
  const paths = runContext?.paths ?? proofRunPaths({ prNumber, commit, runId, token })
  const localDir = runContext?.localDir ?? path.join(repoRoot, paths.localDir)
  fs.mkdirSync(localDir, { recursive: true })

  // ── Step 1: Resolve brief ─────────────────────────────────────────────────
  // The Playwright web harness only understands plain-string evidence (it joins
  // expectedEvidence for the goal and audits with text.includes(sub)). The brief
  // prompt keeps matcher objects out via the default surface:'web' framing, and
  // validation is scoped with allowMatchers:false so a stray matcher (LLM slip or
  // authored brief) fails loudly instead of stringifying to `[object Object]`
  // (BOS-222).
  let brief
  const explicitBriefPath = process.env.BOSS_PROOF_BRIEF
  if (explicitBriefPath) {
    const raw = JSON.parse(fs.readFileSync(explicitBriefPath, 'utf8'))
    const result = validateBrief(raw, { allowMatchers: false })
    if (result.brief === null) {
      throw new Error(`Invalid BOSS_PROOF_BRIEF: ${result.errors.join(', ')}`)
    }
    brief = result.brief
  } else {
    // Generate brief from the PR diff using Claude (surface:'web' is the default).
    const diff = gatherDiff()
    const routes = gatherRouteMap()
    const fixtures = gatherFixturesSummary()
    const rawBrief = await generateBriefFromDiff({
      diff,
      changedFiles,
      routes,
      fixtures,
      model,
      planRequiredProof,
    })
    const result = validateBrief(rawBrief, { source: 'generated', allowMatchers: false })
    if (result.brief === null) {
      throw new Error(`Generated brief failed validation: ${result.errors.join(', ')}`)
    }
    brief = result.brief
  }

  // Collect mode: clamp the brief's inner wall-clock budget to the orchestrator's
  // shared-budget grant AFTER the default-merge (validateBrief already merged
  // DEFAULT_BUDGETS). resolveAgentBackstopMs then derives the outer SIGKILL from
  // the clamped value automatically. The clamped brief is written below so the
  // Playwright spec reads the reduced timeout.
  if (runContext?.maxWallClockMs) {
    brief.budgets = {
      ...(brief.budgets ?? {}),
      maxWallClockMs: Math.min(
        brief.budgets?.maxWallClockMs ?? DEFAULT_INNER_BUDGET_MS,
        runContext.maxWallClockMs,
      ),
    }
  }

  // Write brief to run dir so the Playwright spec can read it. In collect mode a
  // per-surface filename avoids colliding with the TUI runner in a shared dir.
  const briefPath = path.join(localDir, runContext?.briefFileName ?? 'brief.json')
  fs.writeFileSync(briefPath, `${JSON.stringify(brief, null, 2)}\n`)

  // Outer SIGKILL backstop, derived from the brief's inner wall-clock budget so
  // it can never preempt the spec's graceful shutdown (which saves the video and
  // stills). Computed here — after the brief is resolved — rather than from a
  // bare default that could sit below the inner budget.
  const agentTimeoutMs = resolveAgentBackstopMs({
    envValue: process.env.BOSS_PROOF_AGENT_TIMEOUT_MS,
    innerBudgetMs: brief.budgets?.maxWallClockMs,
  })
  const d = { ...defaultDeps(agentTimeoutMs), ...(deps ?? {}) }

  // ── Step 2: Spawn Playwright agent-proof project ──────────────────────────
  const rawDir = path.join(localDir, 'raw')
  fs.mkdirSync(rawDir, { recursive: true })

  let agentPassed = false
  let agentResult = { passed: false, summary: 'agent did not run', evidence: [], steps: 0 }
  let agentTimedOut = false

  // Live-agent decision (BOS-142): live MODE is enabled ONLY when the VALIDATED
  // brief actually carries a `liveAgent:true` scene — the exact condition under
  // which proof.spec.ts runs the deterministic live driver (it keys off
  // scene.liveAgent). A generated brief always has the flag stripped
  // (validateBrief source:'generated'), so it stays stubbed. We must NOT boot
  // live mode off a plan `live-agent` bullet alone: a generated brief can never
  // carry the scene to drive, so the spec would fall through to the plain
  // LLM/stub path while the real runner was booted — spending with no live
  // scene and no disclosure. The bullet still widens the shared budget in
  // proof.mjs (a harmless over-grant). Only when requested do we run the
  // preflight; all prereqs present ⇒ genuinely live, otherwise the run proceeds
  // with the deferred ids so the driver marks the scene deferred (honest, non-red).
  const liveAgentRequested =
    Array.isArray(brief.scenes) && brief.scenes.some((s) => s.liveAgent === true)
  let liveAgentOpts = {}
  if (liveAgentRequested) {
    const missing = d.liveAgentPreflight()
    liveAgentOpts = missing.length === 0 ? { liveAgent: true } : { liveAgentDeferred: missing }
  }

  try {
    // spawnPlaywright may be async (real path) or sync (test stubs); await handles both.
    const pwResult = await Promise.resolve(
      d.spawnPlaywright({ briefPath, localDir, model, ...liveAgentOpts }),
    )
    agentTimedOut = Boolean(pwResult.timedOut)
    if (agentTimedOut) {
      console.warn(
        `[proof-agent] Playwright agent timed out after ${agentTimeoutMs}ms — deferring honestly (no recipe floor)`,
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

  // Per-scene pass/fail (BOS-140 P3b/P3c), from the web tracker's DOM-text
  // evidence audit (AgentResult.scenes). A miss on ANY scene forces the
  // capture to 'failed' even when the agent's own done(passed=true) and the
  // whole-run interaction gate both succeeded — parity with the TUI gate.
  const scenes = Array.isArray(agentResult.scenes) ? agentResult.scenes : []
  // A live-deferred scene (BOS-142: prereq missing) carries passed:false but is
  // an HONEST env deferral, NOT a failure — exclude it from the failure set so a
  // passing sibling scene is never dragged red and the capture summary never
  // names a deferral as "failed". This mirrors the spec-side computeLivePassed,
  // which also excludes liveDeferred scenes; keeping the two in lockstep is what
  // lets a mixed (deferred-live + passing-stub) brief finish green.
  const failedScenes = scenes.filter((s) => s.passed === false && !s.liveDeferred)

  // Per-scene agent-runner disclosure (BOS-142). A scene that actually ran
  // against the real Claude plugin (Task 5 driver set `liveAgent === true`) is
  // unstubbed → mark it `agentRunnerStubbed: false`. Every other scene (stub-run
  // OR live-deferred) stays stubbed and we ADD NO field, so an all-stub capture
  // stays byte-identical to the BOS-141 goldens (absent field ⇒ stubbed). The
  // run-level manifest flag is then `capturesAgentRunnerStubbed([captureShape])`.
  for (const scene of scenes) {
    if (scene.liveAgent === true) scene.agentRunnerStubbed = false
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
    const normalizedScenes = normalizeScenes(brief)

    // Copy stills to captureDir
    let stills = rawStills.map((stillPath) => {
      const base = path.basename(stillPath)
      const dest = path.join(captureDir, base)
      fs.copyFileSync(stillPath, dest)
      const sceneId = sceneIdFromStillName(base, normalizedScenes)
      return {
        fileName: `${captureId}/${base}`,
        label: labelFromStillName(base),
        ...(sceneId ? { sceneId } : {}),
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

    // Chapter-timestamp mapping (BOS-140/P3b): map each scene's wall-offset
    // marker (AgentResult.scenes[].atMs, from the web tracker's begin_scene)
    // through the video's trim + retime + intro to the output mp4's clock, so
    // the comment/gallery (Task 7) can link `[m:ss]` into the video. Null-safe:
    // a failed post-process (post.ok === false, plain-mp4 fallback) has no
    // known timeline, so scenes degrade to outputMs: null (unlinked chapters).
    // Double-approximation note: for web, `atMs` is measured from the runner's
    // `t0`, while `post.timeline.trimMs` also strips the pre-`t0` recording
    // pre-roll (context creation predates `t0` by ~1-2s, see Task 5's clock
    // note) — the two approximations compound, so mapped `#t=` links can skew
    // slightly early. Accepted per the plan's risk note; no behavior change.
    for (const scene of scenes) {
      scene.outputMs = mapSourceToOutputMs(post.ok ? post.timeline : null, scene.atMs)
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
    const captureStatus =
      agentPassed && agentResult.passed && mp4Exists && failedScenes.length === 0
        ? 'passed'
        : 'failed'
    const captureError = !agentPassed
      ? agentResult.summary || 'agent proof test failed'
      : !agentResult.passed
        ? agentResult.summary || 'agent run did not pass'
        : !mp4Exists
          ? 'agent passed but no converted video artifact was produced'
          : failedScenes.length > 0
            ? `scene(s) failed: ${failedScenes.map((s) => s.id).join(', ')}`
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
      ...(scenes.length > 0 ? { scenes } : {}),
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
      ...(scenes.length > 0 ? { scenes } : {}),
      error: agentResult.passed
        ? 'no video artifact captured'
        : agentResult.summary || 'no video artifact captured',
    }
  }

  const hasFailure = !agentPassed || !agentResult.passed || captureShape.status !== 'passed'
  // Web is agent-only (BOS-118): a failed or declined run defers honestly with
  // the agent's own capture, never a recipe floor. The agent can also declare,
  // via done({noSurface:true}), that this change has no demonstrable surface —
  // a neutral outcome the finalizer maps to the honest "no UI surface" note.
  const noSurface = agentResult.noSurface === true

  // Surface-specific texts for the secret scan (manifest is always scanned
  // inside finalizeAgentProof — these are the extra web-path texts).
  const scanTexts = [
    agentResult.summary ?? '',
    ...(agentResult.evidence ?? []),
    captureShape.error ?? '',
  ]

  // Collect mode (BOS-139): return a SurfaceRun for the consolidated finalize
  // instead of self-finalizing.
  if (runContext?.collect) {
    return {
      surface: 'web',
      captureShapes: [captureShape],
      brief,
      agentResult,
      hasFailure,
      noSurface,
      scanTexts,
      elapsedMs: Date.now() - startedAt,
      reasonCode: null,
    }
  }

  // ── Steps 4–7: Build manifest, secret scan, upload, render + post comment ─
  const publicBaseUrl = `${publicProofBaseUrl()}/${paths.publicPrefix}`
  return finalizeAgentProof({
    captureShapes: [captureShape],
    brief,
    agentResult,
    hasFailure,
    noSurface,
    prNumber,
    commit,
    runId,
    token,
    paths,
    localDir,
    publicBaseUrl,
    shouldUpload,
    bucket,
    scanTexts,
    // BOS-142: computed, not hardcoded. false only when the web capture is
    // entirely live scenes; any stub/deferred scene keeps it true (honest).
    agentRunnerStubbed: capturesAgentRunnerStubbed([captureShape]),
    // Forward the full merged deps so test stubs (uploadBundle, postComment,
    // etc.) injected into runAgentProof still take effect inside finalize.
    deps: d,
  })
}

// ── Private helpers ──────────────────────────────────────────────────────────

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

// Matches both the legacy `NN-label.png` shape and the BOS-140 P3b per-scene
// `scene-SS-NN-label.png` shape (mirrors proof.spec.ts's STILL_RE).
const STILL_RE = /^(?:scene-\d\d-)?\d\d-.*\.png$/
const SCENE_STILL_PREFIX_RE = /^scene-(\d{2})-/

function findRawStills(rawDir) {
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

/** Derive a still's sceneId from its `scene-SS-` filename prefix by mapping the
 * ordinal onto the brief's normalized scenes. Returns undefined for a legacy
 * `NN-label.png` still (no prefix) so it falls into the gallery's trailing
 * unlabeled group instead of being misattributed to scene 1. */
function sceneIdFromStillName(filename, normalizedScenes) {
  const match = SCENE_STILL_PREFIX_RE.exec(filename)
  if (!match) return undefined
  const ordinal = Number(match[1])
  return normalizedScenes[ordinal - 1]?.id
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
