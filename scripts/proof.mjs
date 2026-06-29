#!/usr/bin/env node

import { execFileSync, spawnSync } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import {
  bossE2eBuildCommand,
  browserCaptureCommand,
  buildManifest,
  githubCommentCommand,
  listProofCommentsCommand,
  minimizeCommentCommand,
  normalizeRecipe,
  parseProofArgs,
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
  terminalRenderCommand,
  tuiAgentBridgeBuildCommand,
  tuiCaptureCommand,
  tuiVideoCaptureCommand,
  validateBrowserRoute,
  validateRecipeId,
} from './proof-lib.mjs';
import { finishVideo } from './proof-finish-video.mjs';
import { publishProofReport } from './proof-publish-report.mjs';
import { applyMinHeightRatio, evenCropHeight, probeDimensions } from './proof-video.mjs';
import { buildTape } from './proof-vhs.mjs';

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
// BOSS_PROOF_CATALOG lets an ad-hoc/experimental recipe set be trialled without
// editing the committed proof/recipes/default.json.
const catalogPath = resolveCatalogPath(repoRoot, process.env.BOSS_PROOF_CATALOG);

function hasVhs() {
  const result = spawnSync('vhs', ['--version'], { stdio: 'ignore' });
  return !result.error && result.status === 0;
}

// Pre-build the e2e boss binary and the fixture daemon ONCE per run, before VHS
// records. The launcher (run-fixture.sh) execs these via BOSS_PROOF_*_BIN, so the
// in-tape boot Sleep only has to cover daemon start + first render — not a 30-60s
// `go build`, which would finish long after VHS has stopped recording.
let cachedTuiBinaries = null;
function buildTuiFixtureBinaries() {
  if (cachedTuiBinaries) {
    return cachedTuiBinaries;
  }
  const binDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-'));
  const bossBin = path.join(binDir, 'boss');
  const daemonBin = path.join(binDir, 'fixture-daemon');
  const bossDir = path.join(repoRoot, 'services', 'boss');
  const buildOpts = { cwd: bossDir, stdio: ['ignore', 'ignore', 'pipe'], encoding: 'utf8' };
  try {
    execFileSync('go', ['build', '-tags', 'e2e', '-o', bossBin, './cmd'], buildOpts);
    execFileSync('go', ['build', '-o', daemonBin, './cmd/proof-fixture-daemon'], buildOpts);
  } catch (error) {
    if (error.stderr) process.stderr.write(error.stderr); // surface the real failure
    fs.rmSync(binDir, { recursive: true, force: true });
    throw error;
  }
  cachedTuiBinaries = { binDir, bossBin, daemonBin };
  return cachedTuiBinaries;
}

function cleanupTuiFixtureBinaries() {
  if (!cachedTuiBinaries) {
    return;
  }
  fs.rmSync(cachedTuiBinaries.binDir, { recursive: true, force: true });
  cachedTuiBinaries = null;
}

let cachedTuiAgentBridge = null;
function buildTuiAgentBridge({ bossBinOverride } = {}) {
  if (cachedTuiAgentBridge) {
    return cachedTuiAgentBridge;
  }
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tui-agent-'));
  const bridgeBin = path.join(dir, 'proof-tui-bridge');
  const bossBin = bossBinOverride || path.join(dir, 'boss-e2e');
  const builds = [{ out: bridgeBin, command: tuiAgentBridgeBuildCommand({ outBin: bridgeBin }) }];
  if (!bossBinOverride) {
    builds.push({ out: bossBin, command: bossE2eBuildCommand({ outBin: bossBin }) });
  }
  try {
    for (const { out, command } of builds) {
      const [bin, args, opts = {}] = command;
      const result = spawnSync(bin, args, {
        cwd: opts.cwd ? path.join(repoRoot, opts.cwd) : repoRoot,
        stdio: ['ignore', 'ignore', 'pipe'],
        encoding: 'utf8',
      });
      if (result.status !== 0 || result.error) {
        if (result.stderr) process.stderr.write(result.stderr);
        fs.rmSync(dir, { recursive: true, force: true });
        const detail = result.error ? `: ${result.error.message}` : '';
        throw new Error(`go build failed for ${out}${detail}`);
      }
    }
  } catch (error) {
    fs.rmSync(dir, { recursive: true, force: true });
    throw error;
  }
  cachedTuiAgentBridge = { dir, bridgeBin, bossBin };
  return cachedTuiAgentBridge;
}

function cleanupTuiAgentBridge() {
  if (!cachedTuiAgentBridge) {
    return;
  }
  fs.rmSync(cachedTuiAgentBridge.dir, { recursive: true, force: true });
  cachedTuiAgentBridge = null;
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
    );
    if (info.title) return info.title;
  } catch {
    // no gh / no PR — fall through
  }
  return recipes[0]?.title ?? 'Proof of implementation';
}

const PROOF_USAGE = `Usage: node scripts/proof.mjs <command> [options]

Commands:
  plan                 Print the selected recipes for the current diff (read-only)
  run                  Capture, upload, and comment proof on the PR

Options:
  --recipe <id>        Capture a specific recipe (repeatable)
  --changed-file <p>   Override changed-file detection (repeatable)
  --dry-run            Capture locally without uploading or commenting

Examples:
  node scripts/proof.mjs plan
  node scripts/proof.mjs run --dry-run --recipe tui-home`;

async function main() {
  const args = parseProofArgs(process.argv.slice(2));
  if (args.command === 'help') {
    console.log(PROOF_USAGE);
    return;
  }
  const catalog = JSON.parse(fs.readFileSync(catalogPath, 'utf8'));
  const changedFiles = args.changedFiles.length > 0 ? args.changedFiles : changedFilesFromGit();
  const selected = selectRecipes(catalog, changedFiles, args.recipes);
  selected.forEach((recipe) => validateRecipeId(recipe.id));

  if (args.command === 'plan') {
    console.log(JSON.stringify({ changedFiles, recipes: selected }, null, 2));
    return;
  }

  if (args.command !== 'run') {
    throw new Error(`unknown proof command: ${args.command}`);
  }

  // ── Docs-only branch: short-circuit before agent/recipe dispatch ───────────
  // A docs/markdown-only PR with no matched proof recipe has no UI surface to
  // capture. Post a neutral docs build check instead of running the web agent.
  // services/docs changes now have Docusaurus recipes, so those continue to the
  // deterministic recipe path below.
  if (shouldPostDocsBuildCheck({ changedFiles, selectedRecipes: selected })) {
    const prNumber = currentPrNumber();
    const commit = execFileSync('git', ['rev-parse', '--short', 'HEAD'], {
      cwd: repoRoot,
      encoding: 'utf8',
    }).trim();
    const marker = proofCommentMarker(prNumber);
    const pageList = changedFiles.map((f) => `- \`${f}\``).join('\n');
    const commentBody =
      renderDeferredComment({
        marker,
        manifest: { commit, prNumber },
        reasonCode: 'docs-build-check',
        recaptureHint: 'pnpm --dir services/docs build',
      }) +
      '\n\n**Changed pages:**\n' +
      pageList;
    const shouldUpload = !args.dryRun && process.env.BOSS_PROOF_UPLOAD !== '0';
    if (shouldUpload && prNumber !== 'local') {
      const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-docs-'));
      const commentPath = path.join(tmpDir, 'comment.md');
      try {
        fs.writeFileSync(commentPath, commentBody);
        collapsePriorProofComments({ prNumber });
        runCommand(
          githubCommentCommand({ prNumber, bodyFile: path.relative(repoRoot, commentPath) }),
        );
      } finally {
        fs.rmSync(tmpDir, { recursive: true, force: true });
      }
    }
    return;
  }

  // ── Mode dispatcher: agent mode is a cheap preflight before booting anything.
  // BOSS_PROOF_MODE=recipe forces the recipe path; =agent forces agent path.
  // Without an explicit mode, explicit --recipe selection stays on the recipe
  // path even when an API key exists.
  if (agentModeAvailable({ explicitRecipeSelection: args.recipes.length > 0 })) {
    const prNumber = currentPrNumber();
    const commit = execFileSync('git', ['rev-parse', '--short', 'HEAD'], {
      cwd: repoRoot,
      encoding: 'utf8',
    }).trim();
    const surface = agentSurface({ catalog, changedFiles });
    if (surface === 'tui') {
      if (!process.env.BOSS_PROOF_TUI_BRIDGE_BIN) {
        const existingBossBin = process.env.BOSS_PROOF_BOSS_BIN;
        const { bridgeBin, bossBin } = buildTuiAgentBridge({ bossBinOverride: existingBossBin });
        const bridgeEnv = tuiAgentBridgeEnv({
          bridgeBin,
          bossBin,
          existingBossBin,
        });
        process.env.BOSS_PROOF_TUI_BRIDGE_BIN = bridgeEnv.BOSS_PROOF_TUI_BRIDGE_BIN;
        process.env.BOSS_PROOF_BOSS_BIN = bridgeEnv.BOSS_PROOF_BOSS_BIN;
      }
      const { runTuiAgentProof } = await import('./proof-tui-agent.mjs');
      return runTuiAgentProof({
        prNumber,
        commit,
        changedFiles,
        dryRun: args.dryRun,
        fallbackRecipeCaptures: recipeFloorCapture({ selected }),
      });
    }
    if (surface === 'web') {
      const { runAgentProof } = await import('./proof-agent.mjs');
      return runAgentProof({
        prNumber,
        commit,
        changedFiles,
        dryRun: args.dryRun,
        fallbackRecipeCaptures: recipeFloorCapture({ selected }),
      });
    }
    // surface === 'recipe': deterministic marketing/docs capture — fall through
    // to the recipe path below instead of running the LLM web agent.
  }

  // Preflight ffmpeg/ffprobe only when a browser-video recipe is selected for a
  // run. Still and TUI-only runs (and the `plan` command above) must not
  // require ffmpeg. Normalize first: a route-only browser recipe defaults to
  // video in captureRecipe, so reading the raw capture here would skip the
  // preflight and fail deep in finishVideo instead of with this clear message.
  if (
    selected.some(
      (recipe) => normalizeRecipe(recipe).capture === 'video' && recipe.surface !== 'tui',
    )
  ) {
    const ffmpegOk = spawnSync('ffmpeg', ['-version'], { stdio: 'ignore' }).status === 0;
    const ffprobeOk = spawnSync('ffprobe', ['-version'], { stdio: 'ignore' }).status === 0;
    if (!ffmpegOk || !ffprobeOk) {
      throw new Error(
        'ffmpeg and ffprobe are required for video proof recipes — install ffmpeg (e.g. brew install ffmpeg)',
      );
    }
  }

  const shouldUpload = !args.dryRun && process.env.BOSS_PROOF_UPLOAD !== '0';
  const bucket = shouldUpload ? requiredProofBucket() : null;
  const prNumber = currentPrNumber();
  const commit = execFileSync('git', ['rev-parse', '--short', 'HEAD'], {
    cwd: repoRoot,
    encoding: 'utf8',
  }).trim();
  const runId = process.env.BOSS_PROOF_RUN_ID || new Date().toISOString().replaceAll(/[:.]/g, '-');
  // Random per-run segment so the public URL isn't guessable from PR + commit.
  const token = process.env.BOSS_PROOF_RUN_TOKEN || randomUUID();
  const paths = proofRunPaths({ prNumber, commit, runId, token });
  const localDir = path.join(repoRoot, paths.localDir);
  fs.mkdirSync(localDir, { recursive: true });

  // Keep the source .webm when we're not uploading (dry-run / BOSS_PROOF_UPLOAD=0)
  // so `proof-postprocess-video.mjs --proof-dir` can re-run the pipeline locally.
  const captures = selected.map((recipe) =>
    captureRecipe({ recipe, localDir, keepWebm: !shouldUpload }),
  );
  const publicBaseUrl = `${publicProofBaseUrl()}/${paths.publicPrefix}`;
  const hasFailure = captures.some((capture) => capture.status === 'failed');
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
  });
  fs.writeFileSync(path.join(localDir, 'manifest.json'), `${JSON.stringify(manifest, null, 2)}\n`);
  console.log(JSON.stringify(manifest, null, 2));

  if (shouldUpload && !hasFailure) {
    uploadBundle({ localDir, publicPrefix: paths.publicPrefix, manifest, bucket });

    // Best-effort report publish — must not throw into the run.
    const repoIdent = currentRepoIdentity();
    if (repoIdent) {
      const branch = currentBranch();
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
      };
      const report = await publishProofReport({ manifest, identity, env: process.env });
      if (report.ok) {
        manifest.reportUrl = report.reportUrl;
        // manifest.json was written + uploaded before reportUrl existed; rewrite
        // the local copy AND re-upload it so both the persisted artifact and the
        // R2-hosted manifest the PR comment links to carry the report link.
        const manifestPath = path.join(localDir, 'manifest.json');
        fs.writeFileSync(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`);
        try {
          runCommand(
            r2UploadCommand({
              bucket,
              key: `${paths.publicPrefix}/manifest.json`,
              file: path.relative(repoRoot, manifestPath),
              contentType: 'application/json',
            }),
          );
        } catch (err) {
          console.warn(`[proof] report manifest refresh skipped: ${err.message}`);
        }
      } else {
        console.warn(`[proof] report publish skipped: ${report.reason}`);
      }
    } else {
      console.warn('[proof] report publish skipped: repo identity unavailable');
    }
  }

  // comment.md is always written (now possibly carrying reportUrl). On failure it has no reportUrl.
  fs.writeFileSync(
    path.join(localDir, 'comment.md'),
    renderComment({ marker: proofCommentMarker(prNumber), manifest }),
  );

  if (hasFailure) {
    process.exitCode = 1;
    return;
  }

  if (shouldUpload && prNumber !== 'local') {
    collapsePriorProofComments({ prNumber });
    runCommand(
      githubCommentCommand({
        prNumber,
        bodyFile: path.relative(repoRoot, path.join(localDir, 'comment.md')),
      }),
    );
    if (shouldCleanupRunDir({ shouldUpload, hasFailure, prNumber, keepWebm: !shouldUpload })) {
      try {
        fs.rmSync(localDir, { recursive: true, force: true });
        // Prune now-empty scaffold dirs up to and including `.proof`. rmdirSync
        // throws on a non-empty dir, which stops the upward walk.
        for (const dir of proofAncestorDirs(localDir)) {
          try {
            fs.rmdirSync(dir);
          } catch {
            break;
          }
        }
      } catch (err) {
        console.warn(`[proof] run-dir cleanup failed (non-fatal): ${err.message}`);
      }
    }
  }
}

function captureRecipe({ recipe: rawRecipe, localDir, keepWebm = false }) {
  const recipe = normalizeRecipe(rawRecipe);
  const recipeId = validateRecipeId(recipe.id);
  const recipeDir = path.join(localDir, recipeId);
  fs.mkdirSync(recipeDir, { recursive: true });
  const recipePath = path.join(recipeDir, 'recipe.json');
  fs.writeFileSync(recipePath, `${JSON.stringify(recipe, null, 2)}\n`);

  // Still/TUI render target; the video branch returns its own <id>.webm plus
  // this PNG as a poster thumbnail.
  const pngPath = path.join(recipeDir, `${recipeId}.png`);
  try {
    if (recipe.surface === 'tui') {
      if (recipe.capture === 'video') {
        if (!hasVhs()) {
          // Degrade gracefully: a missing vhs must NOT fail a proof run that also
          // captured browser/still surfaces. Mark this one capture 'skipped'
          // (the exit-code gate only trips on 'failed'; the comment shows why).
          return {
            recipeId,
            title: recipe.title,
            surface: recipe.surface,
            privacy: recipe.privacy,
            status: 'skipped',
            error: 'vhs not installed — TUI video skipped (install charmbracelet/vhs)',
          };
        }
        const webmPath = path.join(recipeDir, `${recipeId}.webm`);
        const tapePath = path.join(recipeDir, `${recipeId}.tape`);
        const fixture = recipe.fixture ?? 'demo';
        runCommand(
          tuiCaptureCommand({
            recipePath: path.relative(repoRoot, recipePath),
            outputDir: path.relative(repoRoot, recipeDir),
          }),
        );
        runCommand(
          terminalRenderCommand({
            input: path.relative(repoRoot, path.join(recipeDir, 'screen.txt')),
            output: path.relative(repoRoot, pngPath),
            title: recipe.title,
          }),
        );
        const tape = buildTape({
          recipe,
          launcherCmd: `proof/tui/run-fixture.sh ${fixture}`,
          outputPath: webmPath,
        });
        fs.writeFileSync(tapePath, tape);
        const { bossBin, daemonBin } = buildTuiFixtureBinaries();
        // vhs prints a "Host your GIF on vhs.charm.sh: vhs publish <file>.gif" hint we
        // don't want in proof logs. Capture stdout, forward everything except that.
        const [vhsCmd, vhsArgs, vhsOpts = {}] = [
          ...tuiVideoCaptureCommand({ tapePath: path.relative(repoRoot, tapePath) }),
          { env: { BOSS_PROOF_BOSS_BIN: bossBin, BOSS_PROOF_FIXTURE_DAEMON_BIN: daemonBin } },
        ];
        const vhsRes = spawnSync(vhsCmd, vhsArgs, {
          cwd: repoRoot,
          encoding: 'utf8',
          stdio: ['inherit', 'pipe', 'pipe'],
          env: { ...process.env, ...(vhsOpts.env ?? {}) },
        });
        // Filter out VHS hint lines from stdout and stderr before writing
        if (vhsRes.stdout) {
          const filtered = vhsRes.stdout
            .split('\n')
            .filter((l) => !/vhs publish|Host your GIF/i.test(l))
            .join('\n');
          if (filtered) process.stdout.write(filtered);
        }
        if (vhsRes.stderr) {
          const filtered = vhsRes.stderr
            .split('\n')
            .filter((l) => !/vhs publish|Host your GIF/i.test(l))
            .join('\n');
          if (filtered) process.stderr.write(filtered);
        }
        if (vhsRes.status !== 0) throw new Error(`vhs exited ${vhsRes.status}`);
        const stat = fs.statSync(webmPath);
        if (!stat.size) {
          throw new Error('vhs produced an empty webm');
        }

        // Best-effort PR identity for the intro card label (same pattern as browser branch).
        let label = null;
        let cardTitle = recipe.title;
        try {
          const repoName = execFileSync('gh', ['repo', 'view', '--json', 'name', '-q', '.name'], {
            cwd: repoRoot,
            encoding: 'utf8',
            stdio: ['ignore', 'pipe', 'ignore'],
          }).trim();
          try {
            const prInfo = JSON.parse(
              execFileSync('gh', ['pr', 'view', '--json', 'number,title'], {
                cwd: repoRoot,
                encoding: 'utf8',
                stdio: ['ignore', 'pipe', 'ignore'],
              }),
            );
            label = `${repoName}#${prInfo.number}`;
            cardTitle = prInfo.title ?? recipe.title;
          } catch {
            const branch = execFileSync('git', ['rev-parse', '--abbrev-ref', 'HEAD'], {
              cwd: repoRoot,
              encoding: 'utf8',
              stdio: ['ignore', 'pipe', 'ignore'],
            }).trim();
            label = `${repoName} · ${branch}`;
          }
        } catch {
          // No gh or not a repo — skip the intro card.
        }

        finishVideo({
          recipeDir,
          recipeId,
          webmPath,
          pngPath,
          label,
          cardTitle,
          surface: recipe.surface,
          cropHeight: null,
          contentHeight: null,
          timer: true,
          idleSpeedup: true,
          trimLeadingBlank: true,
          keepWebm,
        });

        return {
          recipeId,
          title: recipe.title,
          surface: recipe.surface,
          privacy: recipe.privacy,
          status: 'passed',
          mediaType: 'mp4',
          fileName: `${recipeId}/${recipeId}.mp4`,
          posterFileName: `${recipeId}/${recipeId}.png`,
        };
      }
      // ----- existing still path -----
      runCommand(
        tuiCaptureCommand({
          recipePath: path.relative(repoRoot, recipePath),
          outputDir: path.relative(repoRoot, recipeDir),
        }),
      );
      runCommand(
        terminalRenderCommand({
          input: path.relative(repoRoot, path.join(recipeDir, 'screen.txt')),
          output: path.relative(repoRoot, pngPath),
          title: recipe.title,
        }),
      );
    } else if (recipe.capture === 'video') {
      // Browser video: the runner records a .webm and screenshots a poster .png.
      // The steps carry their own routes (validated by the runner), so there is
      // no single recipe.route to validate here.
      runCommand(
        browserCaptureCommand({
          surface: recipe.surface,
          recipePath: path.relative(repoRoot, recipePath),
          outputDir: path.relative(repoRoot, recipeDir),
        }),
      );
      // Read video-meta.json sidecar (written by the Playwright runner).
      // A missing or invalid sidecar must NOT fail the run (older recipes / fallback).
      let videoMeta = { cropHeight: null, stills: [] };
      try {
        videoMeta = JSON.parse(fs.readFileSync(path.join(recipeDir, 'video-meta.json'), 'utf8'));
      } catch {}

      // ── Post-processing: intro card + condensed mp4 + play-button poster ──

      // Step 2.5: best-effort PR identity for the intro card label.
      let label = null;
      let cardTitle = recipe.title;
      try {
        const repoName = execFileSync('gh', ['repo', 'view', '--json', 'name', '-q', '.name'], {
          cwd: repoRoot,
          encoding: 'utf8',
          stdio: ['ignore', 'pipe', 'ignore'],
        }).trim();
        try {
          const prInfo = JSON.parse(
            execFileSync('gh', ['pr', 'view', '--json', 'number,title'], {
              cwd: repoRoot,
              encoding: 'utf8',
              stdio: ['ignore', 'pipe', 'ignore'],
            }),
          );
          label = `${repoName}#${prInfo.number}`;
          cardTitle = prInfo.title ?? recipe.title;
        } catch {
          const branch = execFileSync('git', ['rev-parse', '--abbrev-ref', 'HEAD'], {
            cwd: repoRoot,
            encoding: 'utf8',
            stdio: ['ignore', 'pipe', 'ignore'],
          }).trim();
          label = `${repoName} · ${branch}`;
        }
      } catch {
        // No gh or not a repo — skip the intro card.
      }

      const webmPath = path.join(recipeDir, `${recipeId}.webm`);
      // pngPath (the recorded poster frame) is declared at the top of captureRecipe.

      // Apply the min-height-ratio cap before using cropHeight anywhere below.
      // A 600px-wide clip must keep at least 300px of height so it stays watchable.
      const dims = probeDimensions(webmPath);
      const contentHeight = videoMeta.cropHeight; // raw content height BEFORE capping
      const cappedCropHeight = dims
        ? applyMinHeightRatio(videoMeta.cropHeight, dims.width, dims.height)
        : evenCropHeight(videoMeta.cropHeight, videoMeta.recordedHeight ?? Infinity);

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
      });

      const prefixedStills = prefixStillFileNames(recipeId, videoMeta.stills);
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
      };
    } else {
      validateBrowserRoute(recipe.route);
      runCommand(
        browserCaptureCommand({
          surface: recipe.surface,
          recipePath: path.relative(repoRoot, recipePath),
          outputDir: path.relative(repoRoot, recipeDir),
        }),
      );
      const auditText = fs.readFileSync(path.join(recipeDir, 'audit.txt'), 'utf8');
      if (!auditText.trim() && recipe.privacy === 'live') {
        throw new Error('live browser capture produced no auditable text');
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
    };
  } catch (error) {
    // Never leave a partial/unvetted artifact behind for a failed capture: no
    // .png/.webm/.gif may survive under .proof. Scan the recipe dir rather than
    // only the stable <id>.* names, so Playwright recordVideo's random-named
    // .webm (written when a video flow fails mid-way) is cleaned up too.
    let leftovers = [];
    try {
      leftovers = fs.readdirSync(recipeDir);
    } catch {
      leftovers = [];
    }
    for (const name of leftovers) {
      if (/\.(png|webm|gif|tape|mp4)$/i.test(name)) {
        fs.rmSync(path.join(recipeDir, name), { force: true });
      }
    }
    return {
      recipeId,
      title: recipe.title,
      surface: recipe.surface,
      privacy: recipe.privacy,
      status: 'failed',
      error: error instanceof Error ? error.message : String(error),
    };
  }
}

function recipeFloorCapture({ selected }) {
  return ({ localDir, keepWebm }) =>
    selected.map((recipe) => captureRecipe({ recipe, localDir, keepWebm }));
}

export function uploadBundle({ localDir, publicPrefix, manifest, bucket }) {
  for (const { file, relative, contentType } of proofUploadFiles({ manifest, localDir })) {
    const key = `${publicPrefix}/${relative}`;
    runCommand(
      r2UploadCommand({
        bucket,
        key,
        file: path.relative(repoRoot, file),
        contentType,
      }),
    );
  }
}

// Collapse prior proof comments as "Outdated" before posting the fresh one, so
// the PR shows only the latest run while preserving earlier runs (hidden, not
// deleted). Runs before the new comment is posted, so it is never minimized.
// Best-effort: a lookup/minimize failure must not fail the proof run.
export function collapsePriorProofComments({ prNumber }) {
  const marker = proofCommentMarker(prNumber);
  let commentsJson;
  try {
    const [command, args] = listProofCommentsCommand({ prNumber });
    commentsJson = execFileSync(command, args, { cwd: repoRoot, encoding: 'utf8' });
  } catch {
    return; // no PR access or gh failure — skip collapsing, still post the comment
  }
  for (const commentId of selectOutdatedProofCommentIds({ commentsJson, marker })) {
    try {
      runCommand(minimizeCommentCommand({ commentId }));
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
  if (process.env.BOSS_PROOF_MODE === 'recipe') return false;
  if (process.env.BOSS_PROOF_MODE === 'agent') return true;
  if (explicitRecipeSelection) return false;
  return Boolean(process.env.PROOF_ANTHROPIC_API_KEY);
}

/**
 * Determines which agent orchestrator surface to use for a proof run.
 * Returns 'tui' when every matched recipe is a TUI recipe; 'recipe' when every
 * matched recipe is a deterministic browser surface (marketing/docs) that needs
 * no LLM exploration — those fall through to the recipe capture path; and 'web'
 * otherwise (mixed, the Vite web app, or no match all use the web brain).
 *
 * BOSS_PROOF_AGENT_SURFACE=tui|web overrides the result. When that is absent,
 * an explicit brief surface can override the catalog-derived result. Any other
 * override value is ignored.
 *
 * @param {{ catalog: object, changedFiles: string[] }} opts
 * @returns {'tui' | 'web' | 'recipe'}
 */
export function agentSurface({ catalog, changedFiles }) {
  const override = process.env.BOSS_PROOF_AGENT_SURFACE;
  if (override === 'tui' || override === 'web') return override;
  const briefSurface = briefSurfaceOverride();
  if (briefSurface) return briefSurface;
  const matched = selectRecipes(catalog, changedFiles);
  if (matched.length > 0 && matched.every((r) => r.surface === 'tui')) return 'tui';
  // Marketing and docs are static, deterministic captures of known routes — the
  // LLM web agent (which explores the live Vite app) cannot reach them and would
  // post irrelevant proof. Route them to the recipe path instead.
  if (
    matched.length > 0 &&
    matched.every((r) => r.surface === 'marketing' || r.surface === 'docs')
  ) {
    return 'recipe';
  }
  return 'web';
}

/**
 * Reads an explicit BOSS_PROOF_BRIEF file and returns its surface when usable.
 * Missing or malformed briefs are ignored so dispatch can fall back normally.
 *
 * @returns {'tui' | 'web' | null}
 */
function briefSurfaceOverride() {
  const briefPath = process.env.BOSS_PROOF_BRIEF;
  if (!briefPath) return null;
  try {
    const surface = JSON.parse(fs.readFileSync(briefPath, 'utf8'))?.surface;
    return surface === 'tui' || surface === 'web' ? surface : null;
  } catch {
    return null;
  }
}

function requiredProofBucket() {
  const bucket = process.env.BOSS_PROOF_R2_BUCKET;
  if (!bucket) {
    throw new Error('BOSS_PROOF_R2_BUCKET is required to upload proof screenshots');
  }
  return bucket;
}

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

function changedFilesFromGit() {
  const baseRef = process.env.BASE_REF || 'origin/main';
  try {
    const output = execFileSync('git', ['diff', '--name-only', `${baseRef}...HEAD`], {
      cwd: repoRoot,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    });
    return output.split(/\r?\n/).filter(Boolean);
  } catch {
    return [];
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

function currentPrNumber() {
  if (process.env.PR_NUMBER) {
    return process.env.PR_NUMBER;
  }
  try {
    return execFileSync('gh', ['pr', 'view', '--json', 'number', '-q', '.number'], {
      cwd: repoRoot,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    }).trim();
  } catch {
    return 'local';
  }
}

function publicProofBaseUrl() {
  return (process.env.BOSS_PROOF_PUBLIC_BASE_URL || 'https://proof.bossanova.dev').replace(
    /\/$/,
    '',
  );
}

/**
 * Clean the local run dir only after a genuine successful post: upload was on,
 * nothing failed, a real PR received the comment, and this wasn't an
 * inspectable keep-webm run. Failures and dry-runs keep the dir for debugging.
 * @param {{shouldUpload:boolean, hasFailure:boolean, prNumber:string, keepWebm:boolean}} o
 */
export function shouldCleanupRunDir({ shouldUpload, hasFailure, prNumber, keepWebm }) {
  return shouldUpload && !hasFailure && prNumber !== 'local' && !keepWebm;
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
  if (!changedFiles?.length) return false;
  const isDoc = (f) =>
    f.startsWith('docs/') ||
    f.startsWith('services/docs/') ||
    /\.(md|mdx)$/.test(f) ||
    f === 'README.md';
  return changedFiles.every(isDoc);
}

/**
 * Docs-only changes with no visual recipe should post a neutral build-check note.
 * If a docs recipe matched (for example services/docs → Docusaurus), capture it.
 *
 * @param {{ changedFiles: string[], selectedRecipes: object[] }} opts
 * @returns {boolean}
 */
export function shouldPostDocsBuildCheck({ changedFiles, selectedRecipes }) {
  return isDocsOnlyChange(changedFiles) && (selectedRecipes?.length ?? 0) === 0;
}

/**
 * Prepends the recipeId to each still's fileName, producing recipe-dir-relative
 * paths that match the manifest `captures[].stills[].fileName` convention.
 * @param {string} recipeId
 * @param {Array<{fileName: string, label: string}>|undefined|null} stills
 * @returns {Array<{fileName: string, label: string}>}
 */
export function prefixStillFileNames(recipeId, stills) {
  return (stills ?? []).map((s) => ({ fileName: `${recipeId}/${s.fileName}`, label: s.label }));
}

export function tuiAgentBridgeEnv({ bridgeBin, bossBin, existingBossBin }) {
  return {
    BOSS_PROOF_TUI_BRIDGE_BIN: bridgeBin,
    BOSS_PROOF_BOSS_BIN: existingBossBin || bossBin,
  };
}

const invokedDirectly =
  process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (invokedDirectly) {
  (async () => {
    try {
      await main();
    } catch (error) {
      console.error(error instanceof Error ? error.message : String(error));
      process.exitCode = 1;
    } finally {
      cleanupTuiAgentBridge();
      cleanupTuiFixtureBinaries();
    }
  })();
}
