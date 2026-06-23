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
import { buildTape } from './proof-vhs.mjs';

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const catalogPath = path.join(repoRoot, 'proof', 'recipes', 'default.json');

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
  try {
    execFileSync('go', ['build', '-tags', 'e2e', '-o', bossBin, './cmd'], {
      cwd: bossDir,
      stdio: 'inherit',
    });
    execFileSync('go', ['build', '-o', daemonBin, './cmd/proof-fixture-daemon'], {
      cwd: bossDir,
      stdio: 'inherit',
    });
  } catch (error) {
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

function main() {
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

  const captures = selected.map((recipe) => captureRecipe({ recipe, localDir }));
  const publicBaseUrl = `${publicProofBaseUrl()}/${paths.publicPrefix}`;
  const manifest = buildManifest({ commit, prNumber, runId, publicBaseUrl, captures });
  fs.writeFileSync(path.join(localDir, 'manifest.json'), `${JSON.stringify(manifest, null, 2)}\n`);
  fs.writeFileSync(
    path.join(localDir, 'comment.md'),
    renderComment({ marker: proofCommentMarker(prNumber), manifest }),
  );
  console.log(JSON.stringify(manifest, null, 2));
  if (captures.some((capture) => capture.status === 'failed')) {
    process.exitCode = 1;
    return;
  }
  if (shouldUpload) {
    uploadBundle({ localDir, publicPrefix: paths.publicPrefix, manifest, bucket });
    if (prNumber !== 'local') {
      collapsePriorProofComments({ prNumber });
      runCommand(
        githubCommentCommand({
          prNumber,
          bodyFile: path.relative(repoRoot, path.join(localDir, 'comment.md')),
        }),
      );
    }
  }
}

function captureRecipe({ recipe, localDir }) {
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
        runCommand([
          ...tuiVideoCaptureCommand({ tapePath: path.relative(repoRoot, tapePath) }),
          { env: { BOSS_PROOF_BOSS_BIN: bossBin, BOSS_PROOF_FIXTURE_DAEMON_BIN: daemonBin } },
        ]);
        const stat = fs.statSync(webmPath);
        if (!stat.size) {
          throw new Error('vhs produced an empty webm');
        }
        return {
          recipeId,
          title: recipe.title,
          surface: recipe.surface,
          privacy: recipe.privacy,
          status: 'passed',
          mediaType: 'webm',
          fileName: `${recipeId}/${recipeId}.webm`,
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
      return {
        recipeId,
        title: recipe.title,
        surface: recipe.surface,
        privacy: recipe.privacy,
        status: 'passed',
        mediaType: 'webm',
        fileName: `${recipeId}/${recipeId}.webm`,
        posterFileName: `${recipeId}/${recipeId}.png`,
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
      if (/\.(png|webm|gif|tape)$/i.test(name)) {
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

function uploadBundle({ localDir, publicPrefix, manifest, bucket }) {
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
function collapsePriorProofComments({ prNumber }) {
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

try {
  main();
} catch (error) {
  console.error(error instanceof Error ? error.message : String(error));
  process.exitCode = 1;
} finally {
  cleanupTuiFixtureBinaries();
}
