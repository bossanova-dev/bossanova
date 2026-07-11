#!/usr/bin/env node

import { execFileSync, spawnSync } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  DEFAULT_TOTAL_PROOF_BUDGET_MS,
  LIVE_AGENT_EXTRA_MS,
  bossE2eBuildCommand,
  browserCaptureCommand,
  buildManifest,
  capturesAgentRunnerStubbed,
  githubCommentCommand,
  listProofCommentsCommand,
  minimizeCommentCommand,
  normalizeRecipe,
  parseProofArgs,
  planSurfaceBudget,
  proofAncestorDirs,
  proofCommentMarker,
  proofRunPaths,
  proofUploadFiles,
  r2UploadCommand,
  renderComment,
  renderDeferredComment,
  resolveCatalogPath,
  selectOutdatedProofCommentIds,
  selectRecipes,
  TUI_REPLAY_EXTRA_MS,
  tuiAgentBridgeBuildCommand,
  validateBrowserRoute,
  validateRecipeId,
} from './proof-lib.mjs'
import {
  BUILTIN_SURFACES,
  captureSurfaceDescriptor,
  classifySurfaces,
  classifyTuiSurface,
  committedScenarioPresent,
  resolveSurfaceRegistry,
  surfaceBudget,
  webUiSurfacePresent,
} from './proof-surfaces.mjs'
import {
  loadPlanEvidence,
  orderSurfaces,
  requiresLiveAgent,
  scopeRequiredProof,
} from './proof-brief.mjs'
import { finishVideo } from './proof-finish-video.mjs'
import { publishProofReport } from './proof-publish-report.mjs'
import { applyMinHeightRatio, evenCropHeight, probeDimensions } from './proof-video.mjs'
import { defaultDoctorLookups, doctorReport, formatDoctorReport } from './proof-doctor.mjs'
import { toStageErrorRecord } from './proof-stage.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
// BOSS_PROOF_CATALOG lets an ad-hoc/experimental recipe set be trialled without
// editing the committed proof/recipes/default.json.
const catalogPath = resolveCatalogPath(repoRoot, process.env.BOSS_PROOF_CATALOG)

// Set once a run has committed to a surface pipeline (prNumber+commit known).
// The outer catch uses it to post an honest pipeline-error note when a stage
// crashes, instead of dying with a bare message and no PR feedback.
let activeRunContext = null
let cachedTuiAgentBridge = null

// Build-input roots that feed BOTH prebuilt TUI-proof binaries: proof-tui-bridge
// (./services/boss/cmd/proof-tui-agent) and boss-e2e (./services/boss/cmd) are
// built from services/boss and import lib/bossalib, and boss-e2e also embeds
// skill payloads from services/boss (see newestSourceMtime). A default ./bin
// binary is only trustworthy while it is newer than every file below; the shared
// set is intentionally broad because over-rebuilding just falls back to the
// pre-BOS-215 in-budget build, while under-rebuilding proves against stale code.
const TUI_PROOF_SOURCE_ROOTS = ['services/boss', 'lib/bossalib']

// Repo-root Go workspace files that ALSO feed the build. The TUI build commands
// run from services/boss, where `go env GOWORK` resolves to the repo-root
// go.work, so a workspace replace/sync change alters the produced binary without
// touching any file under TUI_PROOF_SOURCE_ROOTS. Scan them explicitly (as file
// roots) so a go.work-only change still marks a prebuilt default bin stale.
const TUI_PROOF_SOURCE_FILES = ['go.work', 'go.work.sum']

// Newest mtime (ms) among EVERY regular file AND directory reachable from
// `roots`, or null when nothing is scannable (missing/unreadable paths). Each
// root may be a directory (walked with readdirSync) or an individual file (e.g.
// the go.work workspace files). Skips .git/node_modules. Lets a stale default
// bin be detected without building.
//
// Every file counts, not just .go/go.mod/go.sum: the boss binary embeds skill
// payloads via `//go:embed all:skills` (services/boss/internal/skillinstall/
// embed.go), so a change touching only those non-Go files still alters the built
// binary. An extension allowlist would miss them and reuse a stale bin. Counting
// all files also covers any future go:embed payload for free, and over-counting
// is the safe direction — it just forces the pre-BOS-215 in-budget rebuild.
//
// Directory mtimes count too, which is what makes a pure DELETION or RENAME of a
// source file / embedded payload force a rebuild: unlinking an entry bumps its
// parent directory's mtime even when every surviving file stays older than the
// bin, so a mtime-of-files-only scan would wrongly reuse a bin that still holds
// the deleted code.
export function newestSourceMtime(
  roots,
  { statSync = fs.statSync, readdir = fs.readdirSync } = {},
) {
  let newest = null
  const consider = (p) => {
    try {
      const { mtimeMs } = statSync(p)
      if (newest === null || mtimeMs > newest) newest = mtimeMs
    } catch {
      // missing/unreadable — ignore, it can't make the bin fresher
    }
  }
  const visit = (p) => {
    let entries
    try {
      entries = readdir(p, { withFileTypes: true })
    } catch {
      // Not a readable directory: treat p as an individual file input (e.g. the
      // repo-root go.work / go.work.sum). consider() no-ops if it is missing.
      // Also count the file's PARENT directory mtime, so a DELETION or RENAME of
      // the file is still detected: unlinking it bumps the parent dir's mtime
      // even though the file itself can no longer be stat'd. Without this a PR
      // that removes go.work / go.work.sum could reuse a bin built under the old
      // workspace config — the file-root path skips the directory-walk deletion
      // detection below, so the parent mtime is the only surviving signal.
      consider(p)
      consider(path.dirname(p))
      return
    }
    // Count the directory's own mtime (deletion/rename detection, see above).
    consider(p)
    for (const entry of entries) {
      const full = path.join(p, entry.name)
      if (entry.isDirectory()) {
        if (entry.name === '.git' || entry.name === 'node_modules') continue
        visit(full)
      } else {
        consider(full)
      }
    }
  }
  for (const dir of roots) visit(dir)
  return newest
}

// Freshness gate for a DEFAULT ./bin prebuilt binary: trust it only while it is
// at least as new as its sources. Fails safe toward "fresh" (return true) when
// the bin can't be stat'd or no sources are scannable, so the fileExists-only
// decision — and every existing unit test that fakes fileExists — is preserved.
export function defaultBinFresh(
  binPath,
  { root, statSync = fs.statSync, readdir = fs.readdirSync } = {},
) {
  let binMtime
  try {
    binMtime = statSync(binPath).mtimeMs
  } catch {
    return true
  }
  const inputs = [...TUI_PROOF_SOURCE_ROOTS, ...TUI_PROOF_SOURCE_FILES].map((s) =>
    path.join(root, s),
  )
  const newest = newestSourceMtime(inputs, { statSync, readdir })
  if (newest === null) return true
  return binMtime >= newest
}

/**
 * Resolve prebuilt TUI-proof binaries WITHOUT building (BOS-215). Per binary,
 * prefer an env-var path that exists, then the stable ./bin/ default; a
 * set-but-missing env var falls back to null (build the old way) rather than
 * throwing, so the no-prebuilt path stays byte-identical. An env-var override is
 * an explicit "I built this, trust it" signal and is used on existence alone,
 * but a DEFAULT ./bin binary is only reused while it is newer than its build
 * inputs (`binFresh`, which counts Go sources AND embedded skill payloads):
 * setup-worktree prebuilds these gitignored bins once, so a later edit under
 * services/boss or lib/bossalib in the same worktree — including changes to only
 * embedded files — must force an in-budget rebuild instead of proving against
 * stale executables. The
 * only side effects are the injectable fileExists / binFresh probes. The BOS-223
 * budget rework builds on this seam, so it must stay exported and build-free.
 * @returns {{ bridgeBin: string|null, bossBin: string|null }}
 */
export function resolvePrebuiltTuiBins({
  repoRoot: root = repoRoot,
  env = process.env,
  fileExists = fs.existsSync,
  binFresh = defaultBinFresh,
} = {}) {
  const pick = (envVal, defaultPath) => {
    if (envVal) return fileExists(envVal) ? envVal : null
    if (!fileExists(defaultPath)) return null
    return binFresh(defaultPath, { root }) ? defaultPath : null
  }
  return {
    bridgeBin: pick(env.BOSS_PROOF_TUI_BRIDGE_BIN, path.join(root, 'bin', 'proof-tui-bridge')),
    bossBin: pick(env.BOSS_PROOF_BOSS_BIN, path.join(root, 'bin', 'boss-e2e')),
  }
}

/**
 * Build (or reuse prebuilt) the TUI-proof bridge + boss-e2e binaries. When
 * `bridgeBinOverride` is set the bridge is not built; when `bossBinOverride` is
 * set boss-e2e is not built. With BOTH overrides set no tmp dir is created and
 * nothing is spawned (BOS-215) — the fast prebuilt path. The `spawn` seam is
 * injectable for tests; the no-override fallback is byte-identical to before.
 */
export function buildTuiAgentBridge({
  bossBinOverride,
  bridgeBinOverride,
  spawn = spawnSync,
} = {}) {
  if (cachedTuiAgentBridge) {
    return cachedTuiAgentBridge
  }
  const needBridgeBuild = !bridgeBinOverride
  const needBossBuild = !bossBinOverride
  // Only create a tmp dir when something actually has to be built into it.
  const dir =
    needBridgeBuild || needBossBuild
      ? fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-agent-'))
      : null
  const bridgeBin = bridgeBinOverride || path.join(dir, 'proof-tui-bridge')
  const bossBin = bossBinOverride || path.join(dir, 'boss-e2e')
  const builds = []
  if (needBridgeBuild) {
    builds.push({ out: bridgeBin, command: tuiAgentBridgeBuildCommand({ outBin: bridgeBin }) })
  }
  if (needBossBuild) {
    builds.push({ out: bossBin, command: bossE2eBuildCommand({ outBin: bossBin }) })
  }
  try {
    for (const { out, command } of builds) {
      const [bin, args, opts = {}] = command
      const result = spawn(bin, args, {
        cwd: opts.cwd ? path.join(repoRoot, opts.cwd) : repoRoot,
        stdio: ['ignore', 'ignore', 'pipe'],
        encoding: 'utf8',
      })
      if (result.status !== 0 || result.error) {
        if (result.stderr) process.stderr.write(result.stderr)
        if (dir) fs.rmSync(dir, { recursive: true, force: true })
        const detail = result.error ? `: ${result.error.message}` : ''
        throw new Error(`go build failed for ${out}${detail}`)
      }
    }
  } catch (error) {
    if (dir) fs.rmSync(dir, { recursive: true, force: true })
    throw error
  }
  cachedTuiAgentBridge = { dir, bridgeBin, bossBin }
  return cachedTuiAgentBridge
}

// Test hook: clear the module-level build cache between cases so one test's
// result does not leak into the next.
export function __resetTuiAgentBridgeCache() {
  cachedTuiAgentBridge = null
}

function cleanupTuiAgentBridge() {
  if (!cachedTuiAgentBridge) {
    return
  }
  // A fully-prebuilt run creates no tmp dir (dir === null) — nothing to remove.
  if (cachedTuiAgentBridge.dir) {
    fs.rmSync(cachedTuiAgentBridge.dir, { recursive: true, force: true })
  }
  cachedTuiAgentBridge = null
}

// PR title for the proof comment heading. Falls back to the first recipe's
// title, then a generic label, when gh / PR lookup is unavailable.
function resolveProofTitle({ recipes }) {
  try {
    const info = JSON.parse(
      execFileSync('gh', ['pr', 'view', '--json', 'title'], {
        cwd: repoRoot,
        encoding: 'utf8',
        stdio: ['ignore', 'pipe', 'ignore'],
      }),
    )
    if (info.title) return info.title
  } catch {
    // no gh / no PR — fall through
  }
  return recipes[0]?.title ?? 'Proof of implementation'
}

const PROOF_USAGE = `Usage: node scripts/proof.mjs <command> [options]

Commands:
  plan                 Print the selected recipes for the current diff (read-only)
  run                  Capture, upload, and comment proof on the PR
  scenario validate <file>          Validate a committed proof scenario file
  scenario run <file> [--dry-run] [--pr <n>]
                       Deterministically replay a scenario into a TUI proof (no LLM)

Options:
  --recipe <id>        Capture a specific recipe (repeatable)
  --changed-file <p>   Override changed-file detection (repeatable)
  --dry-run            Capture locally without uploading or commenting
  --pr <n>             (scenario run) PR number when not on a PR branch

Examples:
  node scripts/proof.mjs plan
  node scripts/proof.mjs run --dry-run --recipe tui-home
  node scripts/proof.mjs scenario validate proof/scenarios/demo.scenario.json
  node scripts/proof.mjs scenario run proof/scenarios/demo.scenario.json --dry-run`

async function main() {
  const args = parseProofArgs(process.argv.slice(2))
  if (args.command === 'help') {
    console.log(PROOF_USAGE)
    return
  }
  // BOS-219: deterministic scenario replay CLI. Handled BEFORE the catalog read
  // and the `run`-only guard — it needs neither recipe catalog nor changed-file
  // detection. This is the ONLY new dispatch branch (no change to run defaults).
  if (args.command === 'scenario') {
    await runScenarioCommand(args)
    return
  }
  const catalog = JSON.parse(fs.readFileSync(catalogPath, 'utf8'))
  const surfaceRegistry = resolveSurfaceRegistry(catalog)
  const changedFiles = args.changedFiles.length > 0 ? args.changedFiles : changedFilesFromGit()

  if (args.command === 'doctor') {
    const surface = agentSurface({ catalog, changedFiles })
    const report = doctorReport({ surface, lookups: defaultDoctorLookups({ repoRoot }) })
    if (args.json) console.log(JSON.stringify(report, null, 2))
    else console.log(formatDoctorReport(report))
    if (!report.ok) process.exitCode = 1
    return
  }

  const selected = selectRecipes(catalog, changedFiles, args.recipes)
  selected.forEach((recipe) => validateRecipeId(recipe.id))

  if (args.command === 'plan') {
    // `surface` (single-select) is kept for one release; `surfaces` + `order`
    // are the BOS-139 multi-surface view of the same diff.
    const surface = agentSurface({ catalog, changedFiles })
    const requiredProofBullets = await loadPlanEvidence(changedFiles, (p) =>
      fs.promises.readFile(path.join(repoRoot, p), 'utf8'),
    )
    const surfacePlan = resolveSurfacePlan({
      catalog,
      changedFiles,
      requiredProofBullets,
      briefSurface: briefSurfaceOverride(),
    })
    console.log(
      JSON.stringify(
        {
          changedFiles,
          recipes: selected,
          surface,
          surfaces: surfacePlan.surfaces,
          order: surfacePlan.order,
        },
        null,
        2,
      ),
    )
    return
  }

  if (args.command !== 'run') {
    throw new Error(`unknown proof command: ${args.command}`)
  }

  // ── Docs-only branch: short-circuit before agent/recipe dispatch ───────────
  // A docs/markdown-only PR with no matched proof recipe has no UI surface to
  // capture. Post a neutral docs build check instead of running the web agent.
  // services/docs changes now have Docusaurus recipes, so those continue to the
  // deterministic recipe path below.
  if (shouldPostDocsBuildCheck({ changedFiles, selectedRecipes: selected })) {
    const prNumber = currentPrNumber()
    const commit = execFileSync('git', ['rev-parse', '--short', 'HEAD'], {
      cwd: repoRoot,
      encoding: 'utf8',
    }).trim()
    const marker = proofCommentMarker(prNumber)
    const pageList = changedFiles.map((f) => `- \`${f}\``).join('\n')
    const commentBody =
      renderDeferredComment({
        marker,
        manifest: { commit, prNumber },
        reasonCode: 'docs-build-check',
        recaptureHint: 'pnpm --dir services/docs build',
      }) +
      '\n\n**Changed pages:**\n' +
      pageList
    const shouldUpload = !args.dryRun && process.env.BOSS_PROOF_UPLOAD !== '0'
    if (shouldUpload && prNumber !== 'local') {
      const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-docs-'))
      const commentPath = path.join(tmpDir, 'comment.md')
      try {
        fs.writeFileSync(commentPath, commentBody)
        collapsePriorProofComments({ prNumber })
        runCommand(
          githubCommentCommand({ prNumber, bodyFile: path.relative(repoRoot, commentPath) }),
        )
      } finally {
        fs.rmSync(tmpDir, { recursive: true, force: true })
      }
    }
    return
  }

  // ── Surface dispatcher (BOS-139 multi-surface) ────────────────────────────
  // Classify the diff into a SET of agent surfaces (TUI + web), ordered by
  // required-proof bullets (cheap-first tiebreak). A mixed TUI+web diff runs
  // BOTH agent paths sequentially under one shared budget (runAgentSurfaces).
  // Explicit --recipe runs and marketing/docs-only diffs fall through to the
  // deterministic recipe path below (unchanged).
  const requiredProofBullets = await loadPlanEvidence(changedFiles, (p) =>
    fs.promises.readFile(path.join(repoRoot, p), 'utf8'),
  )
  const plan = resolveSurfacePlan({
    catalog,
    changedFiles,
    requiredProofBullets,
    briefSurface: briefSurfaceOverride(),
  })
  const explicitRecipe = args.recipes.length > 0

  if (!explicitRecipe && plan.order.length > 0) {
    return runAgentSurfaces({ plan, changedFiles, args })
  }
  if (!explicitRecipe && plan.recipes.length === 0) {
    // No agent surface AND no recipe to capture → honest "no UI surface" note
    // (exit 0). (A docs-only no-recipe change was already handled above.)
    const prNumber = currentPrNumber()
    const commit = shortHeadCommit()
    postNoWebUiSurfaceComment({ prNumber, commit, dryRun: args.dryRun })
    return
  }

  // ── Recipe path (explicit --recipe, or marketing/docs recipes) ────────────
  // Hard preflight (BOS-138) against the recipe path's requirements (chromium,
  // ffmpeg, creds) — never a (possibly mismatched) tui/web surface's deps.
  {
    const shouldUpload = !args.dryRun && process.env.BOSS_PROOF_UPLOAD !== '0'
    const preflight = evaluateRunPreflight({ surface: 'recipe', shouldUpload, repoRoot })
    if (preflight) {
      const prNumber = currentPrNumber()
      const commit = shortHeadCommit()
      postEnvUnavailableComment({ prNumber, commit, dryRun: args.dryRun, preflight })
      return
    }
  }

  // Preflight ffmpeg/ffprobe only when a browser-video recipe is selected for a
  // run. Still runs (and the `plan` command above) must not require ffmpeg.
  // Normalize first: a route-only browser recipe defaults to video in
  // captureRecipe, so reading the raw capture here would skip the preflight and
  // fail deep in finishVideo instead of with this clear message.
  if (selected.some((recipe) => normalizeRecipe(recipe).capture === 'video')) {
    const ffmpegOk = spawnSync('ffmpeg', ['-version'], { stdio: 'ignore' }).status === 0
    const ffprobeOk = spawnSync('ffprobe', ['-version'], { stdio: 'ignore' }).status === 0
    if (!ffmpegOk || !ffprobeOk) {
      throw new Error(
        'ffmpeg and ffprobe are required for video proof recipes — install ffmpeg (e.g. brew install ffmpeg)',
      )
    }
  }

  const shouldUpload = !args.dryRun && process.env.BOSS_PROOF_UPLOAD !== '0'
  const bucket = shouldUpload ? requiredProofBucket() : null
  const prNumber = currentPrNumber()
  const commit = execFileSync('git', ['rev-parse', '--short', 'HEAD'], {
    cwd: repoRoot,
    encoding: 'utf8',
  }).trim()
  activeRunContext = { prNumber, commit, dryRun: args.dryRun }
  const runId = process.env.BOSS_PROOF_RUN_ID || new Date().toISOString().replaceAll(/[:.]/g, '-')
  // Random per-run segment so the public URL isn't guessable from PR + commit.
  const token = process.env.BOSS_PROOF_RUN_TOKEN || randomUUID()
  const paths = proofRunPaths({ prNumber, commit, runId, token })
  const localDir = path.join(repoRoot, paths.localDir)
  fs.mkdirSync(localDir, { recursive: true })

  // Keep the source .webm when we're not uploading (dry-run / BOSS_PROOF_UPLOAD=0)
  // so `proof-postprocess-video.mjs --proof-dir` can re-run the pipeline locally.
  const captures = selected.map((recipe) =>
    captureRecipe({ recipe, localDir, registry: surfaceRegistry, keepWebm: !shouldUpload }),
  )
  const publicBaseUrl = `${publicProofBaseUrl()}/${paths.publicPrefix}`
  const hasFailure = captures.some((capture) => capture.status === 'failed')
  const manifest = buildManifest({
    commit,
    prNumber,
    runId,
    publicBaseUrl,
    captures,
    title: resolveProofTitle({ recipes: selected }),
    verdict: hasFailure ? 'failed' : 'passed',
    genAiLive: false,
    brief: { genAi: false },
  })
  fs.writeFileSync(path.join(localDir, 'manifest.json'), `${JSON.stringify(manifest, null, 2)}\n`)
  console.log(JSON.stringify(manifest, null, 2))

  if (shouldUpload && !hasFailure) {
    uploadBundle({ localDir, publicPrefix: paths.publicPrefix, manifest, bucket })

    // Best-effort report publish — must not throw into the run.
    const repoIdent = currentRepoIdentity()
    if (repoIdent) {
      const branch = currentBranch()
      const identity = {
        owner: repoIdent.owner,
        sourceRepo: repoIdent.name,
        prNumber,
        branch,
        // The report directory is `<timestamp>-<runId>`; the default runId is
        // itself a full ISO timestamp, which would double the timestamp in the
        // path. Use the short random run token (already the unguessable segment
        // of the R2 URL) so the directory stays `<timestamp>-<token8>`.
        runId: token.slice(0, 8),
      }
      const report = await publishProofReport({ manifest, identity, env: process.env })
      if (report.ok) {
        manifest.reportUrl = report.reportUrl
        // manifest.json was written + uploaded before reportUrl existed; rewrite
        // the local copy AND re-upload it so both the persisted artifact and the
        // R2-hosted manifest the PR comment links to carry the report link.
        const manifestPath = path.join(localDir, 'manifest.json')
        fs.writeFileSync(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`)
        try {
          runCommand(
            r2UploadCommand({
              bucket,
              key: `${paths.publicPrefix}/manifest.json`,
              file: path.relative(repoRoot, manifestPath),
              contentType: 'application/json',
            }),
          )
        } catch (err) {
          console.warn(`[proof] report manifest refresh skipped: ${err.message}`)
        }
      } else {
        console.warn(`[proof] report publish skipped: ${report.reason}`)
      }
    } else {
      console.warn('[proof] report publish skipped: repo identity unavailable')
    }
  }

  // comment.md is always written (now possibly carrying reportUrl). On failure it has no reportUrl.
  fs.writeFileSync(
    path.join(localDir, 'comment.md'),
    renderComment({ marker: proofCommentMarker(prNumber), manifest }),
  )

  if (hasFailure) {
    process.exitCode = 1
    return
  }

  if (shouldUpload && prNumber !== 'local') {
    collapsePriorProofComments({ prNumber })
    runCommand(
      githubCommentCommand({
        prNumber,
        bodyFile: path.relative(repoRoot, path.join(localDir, 'comment.md')),
      }),
    )
    if (shouldCleanupRunDir({ shouldUpload, hasFailure, prNumber, keepWebm: !shouldUpload })) {
      try {
        fs.rmSync(localDir, { recursive: true, force: true })
        // Prune now-empty scaffold dirs up to and including `.proof`. rmdirSync
        // throws on a non-empty dir, which stops the upward walk.
        for (const dir of proofAncestorDirs(localDir)) {
          try {
            fs.rmdirSync(dir)
          } catch {
            break
          }
        }
      } catch (err) {
        console.warn(`[proof] run-dir cleanup failed (non-fatal): ${err.message}`)
      }
    }
  }
}

/**
 * BOS-219 `scenario` command: deterministic replay of a committed scenario into
 * a real TUI proof with ZERO LLM involvement (no PROOF_ANTHROPIC_API_KEY needed).
 *
 * `validate` loads + shape-validates the scenario, printing OK or the collected
 * pointerful errors (exit 1 on invalid — a CLI tooling failure, NOT a change to
 * the proof-outcome exit-code contract).
 *
 * `run` synthesizes a brief from the scenario (skipping resolveBrief / the SDK
 * via the D4 `brief` seam) and drives the UNCHANGED runTuiAgentProof capture tail
 * through three injections: `brief`, a `loopRunner` adapting `runReplayLoop`, and
 * `evaluateEvidence: makeScenarioEvaluator`. `--dry-run` skips upload/comment and
 * prints local artifacts; a non-dry run on a PR finalizes through the shared
 * pipeline. The bridge factory is injectable (`overrides.bridge`) so tests drive
 * the full CLI path with a fake bridge and never spawn the Go binary.
 *
 * @param {{ sub: string, file: string, dryRun: boolean, pr: string|null }} args
 * @param {{
 *   loadScenario?: Function, runReplayLoop?: Function, synthesizeBrief?: Function,
 *   makeScenarioEvaluator?: Function, runTuiAgentProof?: Function,
 *   bridge?: object, proofDeps?: object, prNumber?: string, commit?: string,
 *   log?: Function, errorLog?: Function, setExitCode?: Function,
 * }} [overrides]
 */
export async function runScenarioCommand(args, overrides = {}) {
  const log = overrides.log ?? console.log
  const errorLog = overrides.errorLog ?? console.error
  const setExitCode = overrides.setExitCode ?? ((n) => (process.exitCode = n))

  const { loadScenario } =
    overrides.loadScenario != null
      ? { loadScenario: overrides.loadScenario }
      : await import('./proof-scenario.mjs')

  if (args.sub === 'validate') {
    try {
      loadScenario(args.file)
      log(`scenario valid: ${args.file}`)
    } catch (err) {
      errorLog(err instanceof Error ? err.message : String(err))
      setExitCode(1)
    }
    return
  }

  // ── run ────────────────────────────────────────────────────────────────────
  const { scenario } = loadScenario(args.file)
  const replay =
    overrides.runReplayLoop != null
      ? {
          runReplayLoop: overrides.runReplayLoop,
          synthesizeBrief: overrides.synthesizeBrief,
          makeScenarioEvaluator: overrides.makeScenarioEvaluator,
        }
      : await import('./proof-tui-replay.mjs')
  const runReplayLoop = overrides.runReplayLoop ?? replay.runReplayLoop
  const synthesizeBrief = overrides.synthesizeBrief ?? replay.synthesizeBrief
  const makeScenarioEvaluator = overrides.makeScenarioEvaluator ?? replay.makeScenarioEvaluator
  const runTuiAgentProof =
    overrides.runTuiAgentProof ?? (await import('./proof-tui-agent.mjs')).runTuiAgentProof

  const basename = path.basename(args.file)
  const brief = synthesizeBrief(scenario, basename)
  const prNumber = overrides.prNumber ?? (args.pr != null ? String(args.pr) : currentPrNumber())
  const commit = overrides.commit ?? shortHeadCommit()

  // Bridge: an injected fake (tests) OR the real prebuilt/spawned Go bridge. When
  // neither is supplied, resolve/build the bridge binaries and export the env so
  // runTuiAgentProof's makeStdioBridge can spawn the real bridge (honoring an
  // existing BOSS_PROOF_TUI_BRIDGE_BIN via resolvePrebuiltTuiBins).
  const proofDeps = { ...(overrides.proofDeps ?? {}) }
  if (overrides.bridge) {
    proofDeps.bridge = overrides.bridge
  } else if (!proofDeps.bridge) {
    const { bridgeBin, bossBin } = resolvePrebuiltTuiBins()
    const built = buildTuiAgentBridge({
      bridgeBinOverride: bridgeBin ?? undefined,
      bossBinOverride: bossBin ?? undefined,
    })
    const env = tuiAgentBridgeEnv({
      bridgeBin: built.bridgeBin,
      bossBin: built.bossBin,
      existingBossBin: process.env.BOSS_PROOF_BOSS_BIN,
    })
    process.env.BOSS_PROOF_TUI_BRIDGE_BIN = env.BOSS_PROOF_TUI_BRIDGE_BIN
    process.env.BOSS_PROOF_BOSS_BIN = env.BOSS_PROOF_BOSS_BIN
  }

  activeRunContext = { prNumber, commit, dryRun: args.dryRun }

  const result = await runTuiAgentProof({
    prNumber,
    commit,
    changedFiles: [],
    dryRun: args.dryRun,
    // The three BOS-219 seams: the pre-resolved brief SKIPS resolveBrief (no SDK
    // call), loopRunner adapts runReplayLoop to runTuiAgentProof's call shape, and
    // evaluateEvidence swaps the gate for the scenario's structured matcher.
    brief,
    loopRunner: ({ bridge, rawDir }) =>
      runReplayLoop({ scenario, bridge, rawDir, fileBasename: basename }),
    evaluateEvidence: makeScenarioEvaluator(scenario),
    deps: proofDeps,
  })

  if (result?.manifest) {
    // Dry-run + full-run both surface the manifest (its captures list the local
    // artifact fileNames: cast-derived mp4/stills + scenes). Honest, no upload on
    // dry-run (shouldUpload:false threads through the shared finalize).
    log(JSON.stringify(result.manifest, null, 2))
  }
  return result
}

function captureRecipe({
  recipe: rawRecipe,
  localDir,
  registry = BUILTIN_SURFACES,
  keepWebm = false,
}) {
  const recipe = normalizeRecipe(rawRecipe)
  const recipeId = validateRecipeId(recipe.id)
  // Resolve the browser-capture descriptor once; browserCaptureCommand reads
  // serviceDir/specRoot/crop/stageEnv from it (surface-name-agnostic core).
  const descriptor = captureSurfaceDescriptor(registry, recipe.surface)
  const recipeDir = path.join(localDir, recipeId)
  fs.mkdirSync(recipeDir, { recursive: true })
  const recipePath = path.join(recipeDir, 'recipe.json')
  fs.writeFileSync(recipePath, `${JSON.stringify(recipe, null, 2)}\n`)

  // Still render target; the video branch returns its own <id>.webm plus this
  // PNG as a poster thumbnail.
  const pngPath = path.join(recipeDir, `${recipeId}.png`)
  try {
    if (recipe.capture === 'video') {
      // Browser video: the runner records a .webm and screenshots a poster .png.
      // The steps carry their own routes (validated by the runner), so there is
      // no single recipe.route to validate here.
      runCommand(
        browserCaptureCommand({
          descriptor,
          surface: recipe.surface,
          recipePath: path.relative(repoRoot, recipePath),
          outputDir: path.relative(repoRoot, recipeDir),
        }),
      )
      // Read video-meta.json sidecar (written by the Playwright runner).
      // A missing or invalid sidecar must NOT fail the run (older recipes / fallback).
      let videoMeta = { cropHeight: null, stills: [] }
      try {
        videoMeta = JSON.parse(fs.readFileSync(path.join(recipeDir, 'video-meta.json'), 'utf8'))
      } catch {}

      // ── Post-processing: intro card + condensed mp4 + play-button poster ──

      // Step 2.5: best-effort PR identity for the intro card label.
      let label = null
      let cardTitle = recipe.title
      try {
        const repoName = execFileSync('gh', ['repo', 'view', '--json', 'name', '-q', '.name'], {
          cwd: repoRoot,
          encoding: 'utf8',
          stdio: ['ignore', 'pipe', 'ignore'],
        }).trim()
        try {
          const prInfo = JSON.parse(
            execFileSync('gh', ['pr', 'view', '--json', 'number,title'], {
              cwd: repoRoot,
              encoding: 'utf8',
              stdio: ['ignore', 'pipe', 'ignore'],
            }),
          )
          label = `${repoName}#${prInfo.number}`
          cardTitle = prInfo.title ?? recipe.title
        } catch {
          const branch = execFileSync('git', ['rev-parse', '--abbrev-ref', 'HEAD'], {
            cwd: repoRoot,
            encoding: 'utf8',
            stdio: ['ignore', 'pipe', 'ignore'],
          }).trim()
          label = `${repoName} · ${branch}`
        }
      } catch {
        // No gh or not a repo — skip the intro card.
      }

      const webmPath = path.join(recipeDir, `${recipeId}.webm`)
      // pngPath (the recorded poster frame) is declared at the top of captureRecipe.

      // Apply the min-height-ratio cap before using cropHeight anywhere below.
      // A 600px-wide clip must keep at least 300px of height so it stays watchable.
      const dims = probeDimensions(webmPath)
      const contentHeight = videoMeta.cropHeight // raw content height BEFORE capping
      const cappedCropHeight = dims
        ? applyMinHeightRatio(videoMeta.cropHeight, dims.width, dims.height)
        : evenCropHeight(videoMeta.cropHeight, videoMeta.recordedHeight ?? Infinity)

      finishVideo({
        recipeDir,
        recipeId,
        webmPath,
        pngPath,
        label,
        cardTitle,
        surface: recipe.surface,
        cropHeight: cappedCropHeight,
        contentHeight,
        timer: true,
        idleSpeedup: true,
        trimLeadingBlank: true,
        keepWebm,
      })

      const prefixedStills = prefixStillFileNames(recipeId, videoMeta.stills)
      return {
        recipeId,
        title: recipe.title,
        surface: recipe.surface,
        privacy: recipe.privacy,
        status: 'passed',
        mediaType: 'mp4',
        fileName: `${recipeId}/${recipeId}.mp4`,
        posterFileName: `${recipeId}/${recipeId}.png`,
        ...(prefixedStills.length > 0 ? { stills: prefixedStills } : {}),
      }
    } else {
      validateBrowserRoute(recipe.route)
      runCommand(
        browserCaptureCommand({
          descriptor,
          surface: recipe.surface,
          recipePath: path.relative(repoRoot, recipePath),
          outputDir: path.relative(repoRoot, recipeDir),
        }),
      )
      const auditText = fs.readFileSync(path.join(recipeDir, 'audit.txt'), 'utf8')
      if (!auditText.trim() && recipe.privacy === 'live') {
        throw new Error('live browser capture produced no auditable text')
      }
    }

    return {
      recipeId,
      title: recipe.title,
      surface: recipe.surface,
      privacy: recipe.privacy,
      status: 'passed',
      mediaType: 'png',
      fileName: `${recipeId}/${recipeId}.png`,
    }
  } catch (error) {
    // Never leave a partial/unvetted artifact behind for a failed capture: no
    // .png/.webm/.gif may survive under .proof. Scan the recipe dir rather than
    // only the stable <id>.* names, so Playwright recordVideo's random-named
    // .webm (written when a video flow fails mid-way) is cleaned up too.
    let leftovers = []
    try {
      leftovers = fs.readdirSync(recipeDir)
    } catch {
      leftovers = []
    }
    for (const name of leftovers) {
      if (/\.(png|webm|gif|tape|mp4)$/i.test(name)) {
        fs.rmSync(path.join(recipeDir, name), { force: true })
      }
    }
    return {
      recipeId,
      title: recipe.title,
      surface: recipe.surface,
      privacy: recipe.privacy,
      status: 'failed',
      error: error instanceof Error ? error.message : String(error),
    }
  }
}

/**
 * Posts the honest "no web UI surface" deferred comment for a web-surface change
 * that touches no user-visible app code (BOS-118): no agent run, no media, no
 * recipe floor, exit 0. When uploads are off or there is no real PR, the comment
 * is printed instead of posted. Mirrors the other post*Comment deferral helpers.
 * @param {{ prNumber: string, commit: string, dryRun: boolean }} opts
 */
function postNoWebUiSurfaceComment({ prNumber, commit, dryRun }) {
  const marker = proofCommentMarker(prNumber)
  const commentBody = renderDeferredComment({
    marker,
    manifest: { commit, prNumber, deferred: true },
    reasonCode: 'no-ui-surface',
  })
  const shouldUpload = !dryRun && process.env.BOSS_PROOF_UPLOAD !== '0'
  if (shouldUpload && prNumber !== 'local') {
    const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-web-no-surface-'))
    const commentPath = path.join(tmpDir, 'comment.md')
    try {
      fs.writeFileSync(commentPath, commentBody)
      collapsePriorProofComments({ prNumber })
      runCommand(githubCommentCommand({ prNumber, bodyFile: path.relative(repoRoot, commentPath) }))
    } finally {
      fs.rmSync(tmpDir, { recursive: true, force: true })
    }
  } else {
    console.log(commentBody)
  }
}

/** Short HEAD commit sha (deduplicated helper for the dispatch branches). */
function shortHeadCommit() {
  return execFileSync('git', ['rev-parse', '--short', 'HEAD'], {
    cwd: repoRoot,
    encoding: 'utf8',
  }).trim()
}

/**
 * Runs the doctor preflight for a classified surface. Returns null when every
 * surface-required prerequisite is present (proceed with the run), or a
 * `{ reasonCode:'env-unavailable', missing, report }` decision otherwise.
 *
 * The agent-key requirement mirrors the existing agent gates: it is treated as
 * satisfied when BOSS_PROOF_MODE=agent forces agent mode (the key itself is
 * daemon-injected in cron). Pure enough to unit-test with injected lookups.
 *
 * @param {{
 *   surface: 'tui'|'web'|'recipe'|'docs',
 *   shouldUpload: boolean,
 *   repoRoot?: string,
 *   lookups?: object,
 *   env?: NodeJS.ProcessEnv,
 * }} opts
 * @returns {{ reasonCode: 'env-unavailable', missing: string[], report: object } | null}
 */
export function evaluateRunPreflight({
  surface,
  shouldUpload,
  repoRoot: root = repoRoot,
  lookups = defaultDoctorLookups({ repoRoot: root }),
  env = process.env,
}) {
  const report = doctorReport({ surface, shouldUpload, lookups })
  let missing = report.missing
  if (env.BOSS_PROOF_MODE === 'agent') {
    missing = missing.filter((id) => id !== 'PROOF_ANTHROPIC_API_KEY')
  }
  if (missing.length === 0) return null
  return { reasonCode: 'env-unavailable', missing, report }
}

/**
 * Posts the honest env-unavailable deferred note (embedding the doctor report so
 * a human can fix the gap), then exits 0 without starting any pipeline stage.
 * When uploads are off or there is no real PR, the comment is printed instead.
 * @param {{ prNumber: string, commit: string, dryRun: boolean, preflight: object }} opts
 */
function postEnvUnavailableComment({ prNumber, commit, dryRun, preflight }) {
  const marker = proofCommentMarker(prNumber)
  const commentBody = renderDeferredComment({
    marker,
    manifest: { commit, prNumber, deferred: true },
    reasonCode: 'env-unavailable',
    missing: preflight.missing,
    details: '```\n' + formatDoctorReport(preflight.report) + '\n```',
    recaptureHint: 'node scripts/proof.mjs doctor',
  })
  postDeferredComment({ prNumber, dryRun, commentBody, tmpPrefix: 'proof-env-unavailable-' })
}

/**
 * Posts a pipeline-error note (BOS-138 P1c): OUR bug crashed a stage. Names the
 * stage + a stderr tail and exits 1. NEVER claims an environment limitation.
 * @param {{ prNumber: string, commit: string, dryRun: boolean, error: unknown }} opts
 */
function postPipelineErrorComment({ prNumber, commit, dryRun, error }) {
  const record = toStageErrorRecord(error)
  const marker = proofCommentMarker(prNumber)
  const commentBody = renderDeferredComment({
    marker,
    manifest: { commit, prNumber, deferred: true },
    reasonCode: 'pipeline-error',
    stage: record.stage,
    stderrTail: record.stderrTail,
    recaptureHint: 'node scripts/proof.mjs run',
  })
  process.exitCode = 1
  postDeferredComment({ prNumber, dryRun, commentBody, tmpPrefix: 'proof-pipeline-error-' })
}

/** Shared post-or-print for the deferred (env-unavailable / pipeline-error) notes. */
function postDeferredComment({ prNumber, dryRun, commentBody, tmpPrefix }) {
  const shouldUpload = !dryRun && process.env.BOSS_PROOF_UPLOAD !== '0'
  if (shouldUpload && prNumber !== 'local') {
    const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), tmpPrefix))
    const commentPath = path.join(tmpDir, 'comment.md')
    try {
      fs.writeFileSync(commentPath, commentBody)
      collapsePriorProofComments({ prNumber })
      runCommand(githubCommentCommand({ prNumber, bodyFile: path.relative(repoRoot, commentPath) }))
    } catch (err) {
      // Posting is best-effort: the env-unavailable defer often fires precisely
      // because gh/git creds are the missing prerequisite, so `gh pr comment`
      // itself throws. That must NOT turn the honest defer into a hard failure —
      // fall back to printing the note locally and let the caller keep its exit
      // code (env-unavailable stays 0; pipeline-error already set 1 before us).
      console.error(
        `[proof] could not post deferred note to PR (${err.message}); printing locally:`,
      )
      console.log(commentBody)
    } finally {
      fs.rmSync(tmpDir, { recursive: true, force: true })
    }
  } else {
    console.log(commentBody)
  }
}

export function uploadBundle({ localDir, publicPrefix, manifest, bucket }) {
  for (const { file, relative, contentType } of proofUploadFiles({ manifest, localDir })) {
    const key = `${publicPrefix}/${relative}`
    runCommand(
      r2UploadCommand({
        bucket,
        key,
        file: path.relative(repoRoot, file),
        contentType,
      }),
    )
  }
}

// Collapse prior proof comments as "Outdated" before posting the fresh one, so
// the PR shows only the latest run while preserving earlier runs (hidden, not
// deleted). Runs before the new comment is posted, so it is never minimized.
// Best-effort: a lookup/minimize failure must not fail the proof run.
export function collapsePriorProofComments({ prNumber }) {
  const marker = proofCommentMarker(prNumber)
  let commentsJson
  try {
    const [command, args] = listProofCommentsCommand({ prNumber })
    commentsJson = execFileSync(command, args, { cwd: repoRoot, encoding: 'utf8' })
  } catch {
    return // no PR access or gh failure — skip collapsing, still post the comment
  }
  for (const commentId of selectOutdatedProofCommentIds({ commentsJson, marker })) {
    try {
      runCommand(minimizeCommentCommand({ commentId }))
    } catch {
      // ignore a single minimize failure; keep collapsing the rest
    }
  }
}

/**
 * Returns true when agent mode should run for this invocation.
 * BOSS_PROOF_MODE=recipe → always recipe.
 * BOSS_PROOF_MODE=agent  → always agent.
 * Unset                  → agent iff PROOF_ANTHROPIC_API_KEY is set and this
 *                          invocation did not explicitly select recipes.
 * Exported for unit-testing the dispatcher logic without spawning anything.
 * @param {{ explicitRecipeSelection?: boolean }} [opts]
 */
export function agentModeAvailable({ explicitRecipeSelection = false } = {}) {
  if (process.env.BOSS_PROOF_MODE === 'recipe') return false
  if (process.env.BOSS_PROOF_MODE === 'agent') return true
  if (explicitRecipeSelection) return false
  return Boolean(process.env.PROOF_ANTHROPIC_API_KEY)
}

/**
 * Whether a usable agent exists to drive the agent-only TUI proof path.
 * BOSS_PROOF_MODE=recipe → never (TUI has no recipe fallback, so this defers).
 * BOSS_PROOF_MODE=agent  → always.
 * Unset                  → usable iff PROOF_ANTHROPIC_API_KEY is set.
 * Unlike agentModeAvailable, this ignores explicit --recipe selection: the TUI
 * branch already excludes explicit-recipe runs upstream.
 * Exported for unit-testing the dispatcher without spawning anything.
 * @returns {boolean}
 */
export function tuiAgentUsable() {
  if (process.env.BOSS_PROOF_MODE === 'recipe') return false
  if (process.env.BOSS_PROOF_MODE === 'agent') return true
  return Boolean(process.env.PROOF_ANTHROPIC_API_KEY)
}

/**
 * Whether the TUI agent can ACTUALLY capture a proof on this invocation — an
 * agent run must be both wanted (`tuiAgentUsable`) AND authenticable. The TUI
 * agent leg builds its Anthropic client from `PROOF_ANTHROPIC_API_KEY`
 * explicitly (proof-tui-agent.mjs), with no implicit `ANTHROPIC_API_KEY`
 * fallback, so `BOSS_PROOF_MODE=agent` WITHOUT that key cannot drive the TUI
 * agent — the agent leg would throw and, with no committed scenario to replay,
 * surface as a `pipeline-error` (exit 1). This predicate is therefore stricter
 * than `tuiAgentUsable()`, which trusts the mode=agent assertion (correct for
 * `driver.usable`, where the key is daemon-injected in cron). The BOS-220
 * scenario-missing gate uses THIS predicate so a scenario-less TUI diff with no
 * real key defers the warn-only `scenario-missing` nudge (exit 0) rather than
 * falling through to a doomed agent leg. Exported for unit-testing the gate.
 * @returns {boolean}
 */
export function tuiAgentCanCapture() {
  return tuiAgentUsable() && Boolean(process.env.PROOF_ANTHROPIC_API_KEY)
}

/**
 * Determines which proof surface to use for a run.
 * Returns 'tui' when any changed file lives under a TUI path prefix
 * (catalog-independent, via classifyTuiSurface — BOS-115); 'recipe' when every
 * matched recipe is a deterministic browser surface (marketing/docs) that needs
 * no LLM exploration — those fall through to the recipe capture path; and 'web'
 * otherwise (mixed, the Vite web app, or no match all use the web brain).
 *
 * BOSS_PROOF_AGENT_SURFACE=tui|web overrides the result. When that is absent,
 * an explicit brief surface can override it. Any other override value is
 * ignored. The TUI path classifier runs AFTER both overrides so they keep their
 * existing precedence.
 *
 * @param {{ catalog: object, changedFiles: string[] }} opts
 * @returns {'tui' | 'web' | 'recipe'}
 */
export function agentSurface({ catalog, changedFiles }) {
  const override = process.env.BOSS_PROOF_AGENT_SURFACE
  if (override === 'tui' || override === 'web') return override
  const briefSurface = briefSurfaceOverride()
  if (briefSurface) return briefSurface
  // TUI is detected by path, not the recipe catalog: a boss/TUI diff matches
  // zero recipes yet must still route to the agentic TUI proof path.
  if (classifyTuiSurface(changedFiles)) return 'tui'
  const matched = selectRecipes(catalog, changedFiles)
  // Marketing and docs are static, deterministic captures of known routes — the
  // LLM web agent (which explores the live Vite app) cannot reach them and would
  // post irrelevant proof. Route them to the recipe path instead.
  if (
    matched.length > 0 &&
    matched.every((r) => r.surface === 'marketing' || r.surface === 'docs')
  ) {
    return 'recipe'
  }
  return 'web'
}

/**
 * Resolves the multi-surface plan for an agent proof run (BOS-139 D5/D13).
 * Replaces the single-select `agentSurface` for dispatch. Pure given `env` +
 * `briefSurface`.
 *
 * Override precedence (preserved from agentSurface): `BOSS_PROOF_AGENT_SURFACE=
 * tui|web` narrows the order to that single surface; else a valid
 * `BOSS_PROOF_BRIEF` surface (passed in as `briefSurface`) narrows likewise;
 * else `classifySurfaces` + `scopeRequiredProof` (forcedSurfaces = surfaces with
 * ≥1 scoped required-proof bullet, the D16 mitigation) + `orderSurfaces`.
 *
 * @param {{ catalog: object, changedFiles: string[], requiredProofBullets?: string[], env?: NodeJS.ProcessEnv, briefSurface?: 'tui'|'web'|null }} opts
 * @returns {{ order: ('tui'|'web')[], surfaces: {tui: boolean, web: boolean}, scoped: {tui:string[], web:string[], unscoped:string[]}, recipes: object[] }}
 */
export function resolveSurfacePlan({
  catalog,
  changedFiles,
  requiredProofBullets = [],
  env = process.env,
  briefSurface = null,
}) {
  const scoped = scopeRequiredProof(requiredProofBullets)
  // Required-proof bullets that name a surface FORCE it into the set (D16): a
  // backend-only diff with a "the /settings page shows…" bullet still proves web.
  const forcedSurfaces = [
    ...(scoped.tui.length ? ['tui'] : []),
    ...(scoped.web.length ? ['web'] : []),
  ]
  const classified = classifySurfaces({ changedFiles, catalog, forcedSurfaces })

  const override = env.BOSS_PROOF_AGENT_SURFACE
  const forced = override === 'tui' || override === 'web' ? override : briefSurface
  const surfaces = forced
    ? { tui: forced === 'tui', web: forced === 'web' }
    : { tui: classified.tui, web: classified.web }

  return {
    order: orderSurfaces({ surfaces, scoped }),
    surfaces,
    scoped,
    recipes: classified.recipes,
  }
}

/** A never-ran surface: carries only its deferral reason for the consolidated comment. */
function syntheticDeferredRun(surface, reasonCode, { missing } = {}) {
  return {
    surface,
    captureShapes: [],
    brief: {},
    agentResult: { passed: false, summary: '', evidence: [], steps: 0 },
    hasFailure: false,
    noSurface: false,
    reasonCode,
    ...(missing ? { missing } : {}),
    elapsedMs: 0,
  }
}

/**
 * Runs the agent surfaces in `plan.order` SEQUENTIALLY under ONE shared budget
 * (BOS-139 D5) and posts ONE consolidated comment. Each surface gets its own
 * per-surface preflight + gate; a miss (env-unavailable / agent-unavailable /
 * no-ui-surface / budget-exceeded) defers THAT surface with a synthetic run and
 * never blocks the others. A surface's runner crash becomes a per-surface
 * pipeline-error so the other surface still finalizes. Exit code is the
 * aggregate of the per-surface outcomes (set inside finalizeAgentProof).
 *
 * @param {{ plan: object, changedFiles: string[], args: object }} opts
 */
async function runAgentSurfaces({ plan, changedFiles, args }) {
  const shouldUpload = !args.dryRun && process.env.BOSS_PROOF_UPLOAD !== '0'
  // Resolve the R2 bucket WITHOUT throwing: a missing BOSS_PROOF_R2_BUCKET is a
  // shared upload prerequisite that the per-surface preflight below classifies as
  // `env-unavailable` (an honest exit-0 deferral). Throwing here (the old
  // requiredProofBucket()) would pre-empt that graceful path before any surface
  // is evaluated — crashing to a pipeline-error with no comment. A null bucket
  // reaches finalize only when every surface deferred (nothing to upload), and
  // finalize skips the upload when the bucket is absent.
  const bucket = shouldUpload ? (process.env.BOSS_PROOF_R2_BUCKET ?? null) : null
  const prNumber = currentPrNumber()
  const commit = shortHeadCommit()
  activeRunContext = { prNumber, commit, dryRun: args.dryRun }

  // ONE shared run identity + dir for every surface in this invocation.
  const runId = process.env.BOSS_PROOF_RUN_ID || new Date().toISOString().replaceAll(/[:.]/g, '-')
  const token = process.env.BOSS_PROOF_RUN_TOKEN || randomUUID()
  const paths = proofRunPaths({ prNumber, commit, runId, token })
  const localDir = path.join(repoRoot, paths.localDir)
  fs.mkdirSync(localDir, { recursive: true })
  const publicBaseUrl = `${publicProofBaseUrl()}/${paths.publicPrefix}`

  // BOS-142: a required-proof bullet carrying the `live-agent` marker (D13
  // scoped bullets + unscoped bullets, mirroring the per-surface
  // `planRequiredProof` assembly below) means the web run in THIS invocation
  // will drive a genuine (unstubbed) agent session, which is slower than the
  // stub. When that's the case, grant the shared pool `LIVE_AGENT_EXTRA_MS`
  // more so the live web scene doesn't squeeze a sibling TUI floor (D5). An
  // operator-supplied BOSS_PROOF_TOTAL_BUDGET_MS override remains a HARD
  // ceiling — it is used verbatim, never widened, same as before this ticket.
  const explicitTotalBudgetMs = Number(process.env.BOSS_PROOF_TOTAL_BUDGET_MS) || 0
  const liveAgentInRun =
    plan.order.includes('web') && requiresLiveAgent([...plan.scoped.web, ...plan.scoped.unscoped])
  // BOS-223 (D-E): a TUI run that ships a committed scenario drives BOTH the agent
  // leg AND the scenario-replay fallback under one slice. Extend the shared pool by
  // TUI_REPLAY_EXTRA_MS (env-overridable at THIS call site, mirroring the
  // LIVE_AGENT_EXTRA_MS precedent) so reserving replay headroom never squeezes the
  // agent leg below its floor. An explicit BOSS_PROOF_TOTAL_BUDGET_MS stays a HARD
  // ceiling (used verbatim, never widened).
  const tuiReplayReserveMs = Number(process.env.TUI_REPLAY_EXTRA_MS) || TUI_REPLAY_EXTRA_MS
  const tuiReplayInRun = plan.order.includes('tui') && committedScenarioPresent(changedFiles)
  const totalBudgetMs =
    explicitTotalBudgetMs ||
    DEFAULT_TOTAL_PROOF_BUDGET_MS +
      (liveAgentInRun ? LIVE_AGENT_EXTRA_MS : 0) +
      (tuiReplayInRun ? tuiReplayReserveMs : 0)
  // A required-proof bullet that named web FORCES it into the set even on a
  // path-miss diff (D16) — so the web pre-gate must not then skip it.
  const webForced = plan.scoped.web.length > 0
  let elapsedMs = 0
  const surfaceRuns = []

  // Resolve the bespoke-surface drivers ONCE (BOS-203): the built-in tui/web
  // reference drivers unioned with any repo-local `agent-driver` skill
  // extensions, discovered via the BOS-193 contract. The runners are still
  // dynamically imported here (cycle-break) and handed to the built-ins as deps.
  const { discoverAgentDrivers, driverForSurface } = await import('./proof-agent-drivers.mjs')
  const { runTuiAgentProof, runTuiWithReplayFallback } = await import('./proof-tui-agent.mjs')
  const runAgentProof = (await import('./proof-agent.mjs')).runAgentProof
  // BOS-223 replay seams (dynamic import breaks the proof.mjs ⇄ proof-tui-replay
  // cycle) fed to the built-in TUI driver's agent→replay orchestration.
  const { runReplayLoop, synthesizeBrief, makeScenarioEvaluator } =
    await import('./proof-tui-replay.mjs')
  const { loadScenario } = await import('./proof-scenario.mjs')
  const agentDrivers = await discoverAgentDrivers({
    repoRoot,
    deps: {
      tuiAgentUsable,
      buildTuiAgentBridge,
      resolvePrebuiltTuiBins,
      tuiAgentBridgeEnv,
      agentModeAvailable,
      webUiSurfacePresent,
      evaluateRunPreflight,
      runTuiAgentProof,
      runAgentProof,
      // BOS-223 agent-first TUI dispatch with scenario-replay fallback.
      runTuiWithReplayFallback,
      runReplayLoop,
      synthesizeBrief,
      makeScenarioEvaluator,
      loadScenario,
      tuiReplayReserveMs,
    },
  })

  for (const surface of plan.order) {
    // Scoped bullets are this surface's PRIMARY brief content; unscoped bullets
    // feed every surface (D13).
    const planRequiredProof = [...(plan.scoped[surface] ?? []), ...plan.scoped.unscoped]

    // Registry-driven dispatch (BOS-203): resolve THIS surface's driver and run
    // it through the AgentDriver interface. The per-surface reason-code order is
    // PRESERVED byte-identically — no driver / preflight-missing / not-usable /
    // no-ui-surface / budget-exceeded each defer THIS surface without blocking
    // the others; a run crash becomes a per-surface pipeline-error.
    const driver = driverForSurface(agentDrivers, surface)
    if (!driver) {
      surfaceRuns.push(syntheticDeferredRun(surface, 'agent-unavailable'))
      continue
    }

    // BOS-220 scenario gate (TUI only): a TUI change should commit a
    // proof/scenarios/*.scenario.json demonstrating it. BOS-350: this gate now
    // fires ONLY when the TUI agent cannot ACTUALLY capture the diff
    // (`!tuiAgentCanCapture()` — recipe mode, or no real PROOF_ANTHROPIC_API_KEY,
    // INCLUDING BOSS_PROOF_MODE=agent set without a key). The reasoning is that a
    // scenario-less TUI diff has no leg to run in those states — replay needs a
    // committed scenario and there is no authenticable agent — so
    // `scenario-missing` is the honest, most-actionable authoring nudge (a keyless
    // CI env sees it, not `agent-unavailable`), and the ~4-min agent run + bridge
    // build is skipped. Critically it also avoids a doomed agent leg crashing to a
    // `pipeline-error` (exit 1) under mode=agent-without-key, keeping this change
    // exit-code-neutral. When a real key IS present, control falls through to the
    // driver's agent-only leg (planTuiLegs {agentUsable:T, scenarioPresent:F} ⇒
    // agent only) so the epic's agent-captured video is produced; the author is
    // still nudged non-fatally that a committed scenario is owed (scenarioOwed,
    // rendered by surfaceSectionLines). A committed scenario skips this gate
    // entirely and proceeds normally. Warn-only for now; Epic 4 (BOS-226) flips
    // the keyless case fatal.
    if (surface === 'tui' && !committedScenarioPresent(changedFiles) && !tuiAgentCanCapture()) {
      surfaceRuns.push(syntheticDeferredRun(surface, 'scenario-missing'))
      continue
    }

    // Per-surface hard preflight (BOS-138): a missing prereq defers THIS surface.
    // BOS-223 (D-C): a committed-scenario TUI can replay WITHOUT the agent key, so a
    // keyless env must NOT defer here as env-unavailable — the driver's leg planning
    // routes keyless→replay-only. Relax ONLY PROOF_ANTHROPIC_API_KEY (mirroring
    // evaluateRunPreflight's BOSS_PROOF_MODE=agent filter); every other prereq
    // (agg/ffmpeg/go-toolchain/upload creds) is still needed by the replay capture and
    // stays hard. BOS-350: a TUI surface reaching this gate is either a committed-scenario
    // diff (any key state) or a key-present scenario-less diff (the reordered
    // scenario-missing gate above now only defers the KEYLESS scenario-less case). The
    // key-relax stays scenario-gated: a key-present scenario-less run has the key present,
    // so PROOF_ANTHROPIC_API_KEY is not in `missing` and no relax is needed for it.
    let missing = driver.preflightMissing({ shouldUpload, repoRoot })
    if (surface === 'tui' && committedScenarioPresent(changedFiles)) {
      missing = missing.filter((id) => id !== 'PROOF_ANTHROPIC_API_KEY')
    }
    if (missing.length > 0) {
      surfaceRuns.push(syntheticDeferredRun(surface, 'env-unavailable', { missing }))
      continue
    }

    // Per-surface agent gate. BOS-223 (D-C): a keyless TUI must NOT blanket-defer
    // as `agent-unavailable` here — a keyless TUI diff reaching this gate is
    // guaranteed to have a committed scenario (the reordered BOS-220/BOS-350
    // scenario-missing gate above defers a KEYLESS scenario-less TUI first), so a
    // keyless TUI proceeds to the replay-only leg (the driver's leg planning routes
    // keyless→replay-only). A key-present scenario-less TUI is `usable()` so it does
    // not defer here either — it proceeds to the agent-only leg. The
    // `agent-unavailable` synthetic stays for every non-TUI surface.
    if (!driver.usable(process.env) && surface !== 'tui') {
      surfaceRuns.push(syntheticDeferredRun(surface, 'agent-unavailable'))
      continue
    }

    // BOS-118 pre-gate: skip the ~12-min web run for a change with no visible web
    // surface — UNLESS a required-proof bullet forced web (D16). This is a
    // diff-classification POLICY decision, so it stays in the dispatcher rather
    // than becoming a driver capability.
    if (surface === 'web' && !webForced && !webUiSurfacePresent(changedFiles)) {
      surfaceRuns.push(syntheticDeferredRun(surface, 'no-ui-surface'))
      continue
    }

    // Prebuild BEFORE the budget gate (matches the current tui control flow); the
    // driver's guard makes it idempotent (skips when the bridge bin is set).
    if (driver.prebuild) driver.prebuild({ repoRoot, env: process.env })

    // Shared-budget grant for THIS surface; a run that cannot fit defers.
    const budget = planSurfaceBudget({
      surface,
      elapsedMs,
      totalBudgetMs,
      budget: surfaceBudget(driver.budgetClass),
      liveAgent: surface === 'web' && liveAgentInRun,
    })
    if (!budget.run) {
      surfaceRuns.push(syntheticDeferredRun(surface, 'budget-exceeded'))
      continue
    }

    const runContext = {
      runId,
      token,
      paths,
      localDir,
      briefFileName: `brief-${surface}.json`,
      maxWallClockMs: budget.maxWallClockMs,
      collect: true,
    }
    try {
      const surfaceRun = await driver.run({
        prNumber,
        commit,
        changedFiles,
        dryRun: args.dryRun,
        planRequiredProof,
        runContext,
      })
      elapsedMs += surfaceRun.elapsedMs ?? 0
      surfaceRuns.push(surfaceRun)
    } catch (err) {
      // A crashed surface is OUR bug (pipeline-error): record it as a deferred
      // surface run so the OTHER surface still finalizes.
      const record = toStageErrorRecord(err)
      surfaceRuns.push({
        surface,
        captureShapes: [],
        brief: {},
        agentResult: { passed: false, summary: '', evidence: [], steps: 0 },
        hasFailure: true,
        noSurface: false,
        reasonCode: null,
        pipelineError: {
          stage: record.stage,
          message: err instanceof Error ? err.message : String(err),
          stderrTail: record.stderrTail,
        },
        elapsedMs: 0,
      })
    }
  }

  // Dynamic import breaks the proof.mjs ⇄ proof-agent-finalize.mjs cycle
  // (finalize imports uploadBundle/collapse/… from here).
  const { finalizeAgentProof } = await import('./proof-agent-finalize.mjs')
  return finalizeAgentProof({
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
    // BOS-142: computed across ALL surfaceRuns. false only when every proven
    // scene ran live (all-live web run); any TUI surface, stub scene, deferred
    // scene, or errored surface with no scenes keeps it true (honest).
    agentRunnerStubbed: capturesAgentRunnerStubbed(
      surfaceRuns.flatMap((r) => r.captureShapes ?? []),
    ),
  })
}

/**
 * Reads an explicit BOSS_PROOF_BRIEF file and returns its surface when usable.
 * Missing or malformed briefs are ignored so dispatch can fall back normally.
 *
 * @returns {'tui' | 'web' | null}
 */
function briefSurfaceOverride() {
  const briefPath = process.env.BOSS_PROOF_BRIEF
  if (!briefPath) return null
  try {
    const surface = JSON.parse(fs.readFileSync(briefPath, 'utf8'))?.surface
    return surface === 'tui' || surface === 'web' ? surface : null
  } catch {
    return null
  }
}

function requiredProofBucket() {
  const bucket = process.env.BOSS_PROOF_R2_BUCKET
  if (!bucket) {
    throw new Error('BOSS_PROOF_R2_BUCKET is required to upload proof screenshots')
  }
  return bucket
}

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

function changedFilesFromGit() {
  const baseRef = process.env.BASE_REF || 'origin/main'
  try {
    // --diff-filter=d excludes deletions so a removed committed path never enters
    // changedFiles. The scenario classifiers (committedScenarioPresent /
    // discoverChangedScenarios) filter by path convention only and would otherwise
    // treat a deleted proof/scenarios/*.scenario.json as present — relaxing the
    // keyless env gate and then throwing ENOENT in loadScenario (a pipeline-error
    // out of an authoring/no-scenario case). Fixing it here keeps both classifiers,
    // which consume this same list, in agreement.
    const output = execFileSync(
      'git',
      ['diff', '--name-only', '--diff-filter=d', `${baseRef}...HEAD`],
      {
        cwd: repoRoot,
        encoding: 'utf8',
        stdio: ['ignore', 'pipe', 'ignore'],
      },
    )
    return output.split(/\r?\n/).filter(Boolean)
  } catch {
    return []
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

function currentPrNumber() {
  if (process.env.PR_NUMBER) {
    return process.env.PR_NUMBER
  }
  try {
    return execFileSync('gh', ['pr', 'view', '--json', 'number', '-q', '.number'], {
      cwd: repoRoot,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    }).trim()
  } catch {
    return 'local'
  }
}

function publicProofBaseUrl() {
  return (process.env.BOSS_PROOF_PUBLIC_BASE_URL || 'https://proof.bossanova.dev').replace(
    /\/$/,
    '',
  )
}

/**
 * Clean the local run dir only after a genuine successful post: upload was on,
 * nothing failed, a real PR received the comment, and this wasn't an
 * inspectable keep-webm run. Failures and dry-runs keep the dir for debugging.
 * @param {{shouldUpload:boolean, hasFailure:boolean, prNumber:string, keepWebm:boolean}} o
 */
export function shouldCleanupRunDir({ shouldUpload, hasFailure, prNumber, keepWebm }) {
  return shouldUpload && !hasFailure && prNumber !== 'local' && !keepWebm
}

/**
 * Returns true when every changed file is a docs/markdown-only path that has
 * no UI surface to proof-capture. Empty input returns false (no files → not
 * docs-only). The `services/docs/` subtree, any `docs/` prefix, any `.md`/
 * `.mdx` file, and the top-level `README.md` all count as docs.
 *
 * Note: proof-brief.mjs's `isLowSignalDiffPath` was considered but is not
 * suitable here — it also covers lock files, `.sum`, and config dirs
 * (`.claude/`, `.codex/`) that are not docs-only patterns.
 *
 * @param {string[]} changedFiles
 * @returns {boolean}
 */
export function isDocsOnlyChange(changedFiles) {
  if (!changedFiles?.length) return false
  const isDoc = (f) =>
    f.startsWith('docs/') ||
    f.startsWith('services/docs/') ||
    /\.(md|mdx)$/.test(f) ||
    f === 'README.md'
  return changedFiles.every(isDoc)
}

/**
 * Docs-only changes with no visual recipe should post a neutral build-check note.
 * If a docs recipe matched (for example services/docs → Docusaurus), capture it.
 *
 * @param {{ changedFiles: string[], selectedRecipes: object[] }} opts
 * @returns {boolean}
 */
export function shouldPostDocsBuildCheck({ changedFiles, selectedRecipes }) {
  return isDocsOnlyChange(changedFiles) && (selectedRecipes?.length ?? 0) === 0
}

/**
 * Prepends the recipeId to each still's fileName, producing recipe-dir-relative
 * paths that match the manifest `captures[].stills[].fileName` convention.
 * @param {string} recipeId
 * @param {Array<{fileName: string, label: string}>|undefined|null} stills
 * @returns {Array<{fileName: string, label: string}>}
 */
export function prefixStillFileNames(recipeId, stills) {
  return (stills ?? []).map((s) => ({ fileName: `${recipeId}/${s.fileName}`, label: s.label }))
}

export function tuiAgentBridgeEnv({ bridgeBin, bossBin, existingBossBin }) {
  return {
    BOSS_PROOF_TUI_BRIDGE_BIN: bridgeBin,
    BOSS_PROOF_BOSS_BIN: existingBossBin || bossBin,
  }
}

const invokedDirectly =
  process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
if (invokedDirectly) {
  ;(async () => {
    try {
      await main()
    } catch (error) {
      console.error(error instanceof Error ? error.message : String(error))
      // A crash after a surface pipeline committed (bridge/render/encode/upload)
      // is OUR bug: post an honest pipeline-error note naming the stage + stderr
      // tail, rather than dying silently. toStageErrorRecord tags a non-Stage
      // error at stage `pipeline` so the note still names a stage.
      const ctx = activeRunContext
      if (ctx) {
        try {
          postPipelineErrorComment({ ...ctx, error })
        } catch (postErr) {
          console.error(`[proof] failed to post pipeline-error note: ${postErr.message}`)
        }
      }
      process.exitCode = 1
    } finally {
      cleanupTuiAgentBridge()
    }
  })()
}
