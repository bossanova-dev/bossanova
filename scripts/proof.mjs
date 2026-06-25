#!/usr/bin/env node

import { execFileSync, spawnSync } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import {
  browserCaptureCommand,
  buildManifest,
  classifySecretRisk,
  githubCommentCommand,
  introCardCommand,
  listProofCommentsCommand,
  minimizeCommentCommand,
  parseProofArgs,
  proofCommentMarker,
  proofRunPaths,
  proofUploadFiles,
  r2UploadCommand,
  renderComment,
  selectOutdatedProofCommentIds,
  selectRecipes,
  terminalRenderCommand,
  tuiCaptureCommand,
  tuiVideoCaptureCommand,
  validateBrowserRoute,
  validateRecipeId,
} from './proof-lib.mjs';
import { buildPosterArgs } from './proof-poster.mjs';
import { publishProofReport } from './proof-publish-report.mjs';
import {
  applyMinHeightRatio,
  evenCropHeight,
  postprocessProofVideo,
  probeDimensions,
} from './proof-video.mjs';
import { buildTape } from './proof-vhs.mjs';

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const catalogPath = path.join(repoRoot, 'proof', 'recipes', 'default.json');
const playButtonAsset = fileURLToPath(new URL('./assets/youtube-play-button.png', import.meta.url));

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

async function main() {
  const args = parseProofArgs(process.argv.slice(2));
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

  // ── Mode dispatcher: agent mode is a cheap preflight before booting anything.
  // BOSS_PROOF_MODE=recipe forces the recipe path; =agent forces agent path.
  // Without an explicit mode, explicit --recipe selection stays on the recipe
  // path even when an API key exists.
  if (agentModeAvailable({ explicitRecipeSelection: args.recipes.length > 0 })) {
    const { runAgentProof } = await import('./proof-agent.mjs');
    return runAgentProof({
      prNumber: currentPrNumber(),
      commit: execFileSync('git', ['rev-parse', '--short', 'HEAD'], {
        cwd: repoRoot,
        encoding: 'utf8',
      }).trim(),
      changedFiles: args.changedFiles.length > 0 ? args.changedFiles : changedFilesFromGit(),
      dryRun: args.dryRun,
    });
  }

  // Preflight ffmpeg/ffprobe only when a browser-video recipe is selected for a
  // run. Still and TUI-only runs (and the `plan` command above) must not
  // require ffmpeg.
  if (selected.some((recipe) => recipe.capture === 'video' && recipe.surface !== 'tui')) {
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
      } catch (err) {
        console.warn(`[proof] run-dir cleanup failed (non-fatal): ${err.message}`);
      }
    }
  }
}

// Shared video finish: optional intro card, condense/timer per flags, then a
// play-button poster composite. Browser passes timer/idleSpeedup/trimLeadingBlank
// true; TUI passes them false (D3 — short clips don't need timer/idle, and a VHS
// recording has no white flash to trim).
function finishVideo({
  recipeDir,
  recipeId,
  webmPath,
  pngPath,
  label,
  cardTitle,
  surface,
  cropHeight,
  contentHeight,
  timer,
  idleSpeedup,
  trimLeadingBlank,
  keepWebm,
}) {
  const mp4Path = path.join(recipeDir, `${recipeId}.mp4`);
  const timedPath = path.join(recipeDir, `${recipeId}-timed.mp4`);
  const scratchPath = path.join(recipeDir, `${recipeId}-timer.raw`);
  const introPngPath = path.join(recipeDir, `${recipeId}-intro.png`);
  const tmpPosterPath = path.join(recipeDir, `${recipeId}-poster-tmp.png`);
  const dims = probeDimensions(webmPath);

  let resolvedIntroPngPath;
  if (label && dims) {
    const introHeight = evenCropHeight(cropHeight, dims.height) ?? dims.height;
    try {
      runCommand(
        introCardCommand({
          surface,
          out: path.relative(repoRoot, introPngPath),
          width: dims.width,
          height: introHeight,
          label,
          title: cardTitle,
        }),
      );
      resolvedIntroPngPath = introPngPath;
    } catch (err) {
      console.warn(`[proof] intro-card render failed — continuing without it: ${err.message}`);
    }
  }

  const post = postprocessProofVideo({
    webmPath,
    timedPath,
    outPath: mp4Path,
    scratchPath,
    introPngPath: resolvedIntroPngPath,
    cropHeight,
    timer,
    idleSpeedup,
    trimLeadingBlank,
  });
  if (!post.ok) {
    console.warn(`[proof] video post-processing failed (${post.warning}) — plain mp4 fallback`);
    const fb = spawnSync('ffmpeg', ['-y', '-loglevel', 'error', '-i', webmPath, mp4Path], {
      stdio: 'inherit',
    });
    if (fb.status !== 0)
      throw new Error('ffmpeg fallback mp4 conversion failed — no usable video artifact');
  }

  try {
    const pr = spawnSync(
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
          overlayCenterHeight: contentHeight,
        }),
      ],
      { stdio: 'inherit' },
    );
    if (pr.status === 0 && fs.existsSync(tmpPosterPath)) fs.copyFileSync(tmpPosterPath, pngPath);
    else console.warn('[proof] play-button poster compositing failed — using plain poster frame');
  } catch (err) {
    console.warn(`[proof] play-button poster compositing error: ${err.message}`);
  }

  const tmpFiles = [timedPath, scratchPath, tmpPosterPath, introPngPath];
  if (!keepWebm) tmpFiles.unshift(webmPath);
  for (const f of tmpFiles) fs.rmSync(f, { force: true });
  return { mp4Path };
}

function captureRecipe({ recipe, localDir, keepWebm = false }) {
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
        const screenText = fs.readFileSync(path.join(recipeDir, 'screen.txt'), 'utf8');
        const risk = classifySecretRisk(screenText);
        if (risk.risk === 'high') {
          throw new Error(`secret-like text detected in TUI video screen: ${risk.reason}`);
        }
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
          timer: false,
          idleSpeedup: false,
          trimLeadingBlank: false,
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
      const screenText = fs.readFileSync(path.join(recipeDir, 'screen.txt'), 'utf8');
      const risk = classifySecretRisk(screenText);
      if (risk.risk === 'high') {
        throw new Error(`secret-like text detected in TUI screen: ${risk.reason}`);
      }
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
      const videoAuditText = fs.readFileSync(path.join(recipeDir, 'audit.txt'), 'utf8');
      const videoRisk = classifySecretRisk(videoAuditText);
      if (videoRisk.risk === 'high') {
        throw new Error(`secret-like text detected in browser video capture: ${videoRisk.reason}`);
      }

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
      // The secret scan only sees DOM text (see collectProofAuditText). A
      // canvas-rendered surface (e.g. an xterm terminal) shows pixels the scan
      // cannot inspect, so such recipes must never run against live state.
      if (recipe.canvas && recipe.privacy !== 'fixture') {
        throw new Error(
          'canvas proof recipes must use fixture privacy (DOM text scan cannot inspect canvas pixels)',
        );
      }
      runCommand(
        browserCaptureCommand({
          surface: recipe.surface,
          recipePath: path.relative(repoRoot, recipePath),
          outputDir: path.relative(repoRoot, recipeDir),
        }),
      );
      const auditText = fs.readFileSync(path.join(recipeDir, 'audit.txt'), 'utf8');
      const risk = classifySecretRisk(auditText);
      if (!auditText.trim() && recipe.privacy === 'live') {
        throw new Error('live browser capture produced no auditable text');
      }
      if (risk.risk === 'high') {
        throw new Error(`secret-like text detected in browser capture: ${risk.reason}`);
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
 * Unset                  → agent iff ANTHROPIC_API_KEY is set and this
 *                          invocation did not explicitly select recipes.
 * Exported for unit-testing the dispatcher logic without spawning anything.
 * @param {{ explicitRecipeSelection?: boolean }} [opts]
 */
export function agentModeAvailable({ explicitRecipeSelection = false } = {}) {
  if (process.env.BOSS_PROOF_MODE === 'recipe') return false;
  if (process.env.BOSS_PROOF_MODE === 'agent') return true;
  if (explicitRecipeSelection) return false;
  return Boolean(process.env.ANTHROPIC_API_KEY);
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
 * Prepends the recipeId to each still's fileName, producing recipe-dir-relative
 * paths that match the manifest `captures[].stills[].fileName` convention.
 * @param {string} recipeId
 * @param {Array<{fileName: string, label: string}>|undefined|null} stills
 * @returns {Array<{fileName: string, label: string}>}
 */
export function prefixStillFileNames(recipeId, stills) {
  return (stills ?? []).map((s) => ({ fileName: `${recipeId}/${s.fileName}`, label: s.label }));
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
      cleanupTuiFixtureBinaries();
    }
  })();
}
